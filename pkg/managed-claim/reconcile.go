/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package managedclaim

import (
	"context"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/pointer"
)

const (
	// ManagedClaimLabel names the VMI resource claim entry a generated
	// ResourceClaim was built for.
	ManagedClaimLabel = "kubevirt.io/managed-claim"
	// ManagedClaimProvisionerLabel names the ManagedClaimProvisioner that
	// configured the generation.
	ManagedClaimProvisionerLabel = "kubevirt.io/managed-claim-provisioner"
)

// ProvisionerStore looks up a cluster-scoped ManagedClaimProvisioner by name.
//
// Kept narrow so the reconciler does not depend on the generated lister, which
// also makes it trivially fakeable in tests.
type ProvisionerStore interface {
	Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error)
}

// Reconciler converges the ResourceClaims for the managed claim entries a
// single provisioner controller is responsible for.
//
// It handles only entries whose referenced ManagedClaimProvisioner names this
// controller in spec.provisioner, so several provisioner controllers can run
// side by side without coordinating.
type Reconciler struct {
	provisionerName string
	provisioner     ClaimProvisioner
	client          kubernetes.Interface
	store           ProvisionerStore
}

func NewReconciler(
	provisionerName string,
	provisioner ClaimProvisioner,
	client kubernetes.Interface,
	store ProvisionerStore,
) *Reconciler {
	return &Reconciler{
		provisionerName: provisionerName,
		provisioner:     provisioner,
		client:          client,
		store:           store,
	}
}

// Reconcile brings every managed claim entry on the VMI to its desired state.
//
// A failure on one entry does not abandon the others: one mistyped provisioner
// name should not strand every other claim on the VMI. All errors are returned
// together so the caller can retry and report them.
func (r *Reconciler) Reconcile(ctx context.Context, vmi *v1.VirtualMachineInstance) error {
	// Owner-reference garbage collection cleans up anything already created,
	// so there is nothing to do for a VMI on its way out.
	if vmi.DeletionTimestamp != nil {
		return nil
	}

	var errs []error
	for i := range vmi.Spec.ResourceClaims {
		claim := vmi.Spec.ResourceClaims[i]
		if !dra.IsManagedClaim(claim) {
			continue
		}

		if err := r.reconcileClaim(ctx, vmi, &claim); err != nil {
			errs = append(errs, fmt.Errorf(
				"managed claim %q (provisioner %q): %w",
				claim.Name, *claim.ManagedClaimProvisionerName, err))
		}
	}

	return utilerrors.NewAggregate(errs)
}

func (r *Reconciler) reconcileClaim(
	ctx context.Context,
	vmi *v1.VirtualMachineInstance,
	claim *v1.VirtualMachineInstanceResourceClaim,
) error {
	provisioner, err := r.store.Get(*claim.ManagedClaimProvisionerName)
	if err != nil {
		return err
	}

	// Another controller serves this provisioner; leave it alone.
	if provisioner.Spec.Provisioner != r.provisionerName {
		return nil
	}

	devices, err := CollectDevices(vmi, claim.Name)
	if err != nil {
		return err
	}

	spec, err := r.provisioner.GenerateClaim(&ManagedClaimContext{
		VMI:         vmi,
		Claim:       claim,
		Provisioner: provisioner,
		Devices:     devices,
	})
	if err != nil {
		return err
	}

	return r.applyClaim(ctx, vmi, claim, provisioner, spec)
}

func (r *Reconciler) applyClaim(
	ctx context.Context,
	vmi *v1.VirtualMachineInstance,
	claim *v1.VirtualMachineInstanceResourceClaim,
	provisioner *corev1alpha1.ManagedClaimProvisioner,
	spec *resourcev1.ResourceClaimSpec,
) error {
	desired := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dra.ManagedClaimName(vmi.Name, claim.Name),
			Namespace: vmi.Namespace,
			Labels: map[string]string{
				ManagedClaimLabel:            claim.Name,
				ManagedClaimProvisionerLabel: provisioner.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1.VirtualMachineInstanceGroupVersionKind.GroupVersion().String(),
				Kind:       v1.VirtualMachineInstanceGroupVersionKind.Kind,
				Name:       vmi.Name,
				UID:        vmi.UID,
				Controller: pointer.P(true),
			}},
		},
		Spec: *spec,
	}

	claims := r.client.ResourceV1().ResourceClaims(vmi.Namespace)

	existing, err := claims.Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = claims.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	// Only write when something actually differs. Reconciling on every VMI
	// event is normal, and an unconditional update would rewrite the claim
	// each time, churning resourceVersion and re-triggering watchers.
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.Spec = desired.Spec
	updated.Labels = desired.Labels
	updated.OwnerReferences = desired.OwnerReferences

	_, err = claims.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}
