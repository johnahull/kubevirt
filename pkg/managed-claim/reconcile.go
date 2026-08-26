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

	"kubevirt.io/kubevirt/pkg/controller"
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
	// ManagedClaimVMILabel names the VMI a generated ResourceClaim belongs to,
	// so claims can be selected by VMI without walking owner references.
	ManagedClaimVMILabel = "kubevirt.io/managed-claim-vmi"
	// ManagedClaimFinalizer protects a generated ResourceClaim from being
	// removed out from under a running VMI. Owner-reference garbage collection
	// still deletes the claim, but only once this controller has released the
	// finalizer during VMI teardown.
	ManagedClaimFinalizer = "kubevirt.io/managed-claim-protection"
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
	// A VMI on its way out must have the finalizer released from its managed
	// claims, otherwise owner-reference garbage collection can never remove
	// them and VMI deletion wedges forever.
	if vmi.DeletionTimestamp != nil {
		return r.releaseClaims(ctx, vmi)
	}

	return forEachManagedClaim(vmi, func(claim *v1.VirtualMachineInstanceResourceClaim) error {
		return r.reconcileClaim(ctx, vmi, claim)
	})
}

// forEachManagedClaim runs fn against every managed claim entry on the VMI,
// skipping entries this feature does not own. A failure on one entry does not
// abandon the others: one mistyped provisioner name should not strand every
// other claim on the VMI. All errors are wrapped with the offending entry and
// its provisioner and returned together so the caller can retry and report
// them.
func forEachManagedClaim(
	vmi *v1.VirtualMachineInstance,
	fn func(claim *v1.VirtualMachineInstanceResourceClaim) error,
) error {
	var errs []error
	for i := range vmi.Spec.ResourceClaims {
		claim := &vmi.Spec.ResourceClaims[i]
		if !dra.IsManagedClaim(*claim) {
			continue
		}

		if err := fn(claim); err != nil {
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

// releaseClaims removes this controller's finalizer from the managed claims of
// a VMI that is being deleted, so owner-reference garbage collection can
// complete. As during normal reconciliation, one failure does not abandon the
// other claims.
func (r *Reconciler) releaseClaims(ctx context.Context, vmi *v1.VirtualMachineInstance) error {
	return forEachManagedClaim(vmi, func(claim *v1.VirtualMachineInstanceResourceClaim) error {
		return r.releaseClaim(ctx, vmi, claim)
	})
}

func (r *Reconciler) releaseClaim(
	ctx context.Context,
	vmi *v1.VirtualMachineInstance,
	claim *v1.VirtualMachineInstanceResourceClaim,
) error {
	provisioner, err := r.store.Get(*claim.ManagedClaimProvisionerName)
	if errors.IsNotFound(err) {
		// The provisioner is gone, so ownership cannot be confirmed; leave the
		// finalizer for whichever controller does serve this claim.
		return nil
	}
	if err != nil {
		return err
	}

	// Another controller serves this provisioner and owns its finalizer.
	if provisioner.Spec.Provisioner != r.provisionerName {
		return nil
	}

	name := dra.ManagedClaimName(vmi.Name, claim.Name)
	existing, err := r.client.ResourceV1().ResourceClaims(vmi.Namespace).
		Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	return r.removeFinalizer(ctx, vmi.Namespace, existing)
}

func (r *Reconciler) removeFinalizer(
	ctx context.Context,
	namespace string,
	existing *resourcev1.ResourceClaim,
) error {
	if !controller.HasFinalizer(existing, ManagedClaimFinalizer) {
		return nil
	}

	updated := existing.DeepCopy()
	controller.RemoveFinalizer(updated, ManagedClaimFinalizer)

	_, err := r.client.ResourceV1().ResourceClaims(namespace).
		Update(ctx, updated, metav1.UpdateOptions{})
	return err
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
			Name:       dra.ManagedClaimName(vmi.Name, claim.Name),
			Namespace:  vmi.Namespace,
			Finalizers: []string{ManagedClaimFinalizer},
			Labels: map[string]string{
				ManagedClaimLabel:            claim.Name,
				ManagedClaimProvisionerLabel: provisioner.Name,
				ManagedClaimVMILabel:         vmi.Name,
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

	// The claim was deleted externally while the VMI is still alive: the
	// finalizer has held it in a terminating state. Release the finalizer so
	// garbage collection can finish removing it; a later reconcile recreates it
	// through the not-found path above once it is gone.
	if existing.DeletionTimestamp != nil {
		return r.removeFinalizer(ctx, vmi.Namespace, existing)
	}

	// Only write when something actually differs. Reconciling on every VMI
	// event is normal, and an unconditional update would rewrite the claim
	// each time, churning resourceVersion and re-triggering watchers.
	needsFinalizer := !controller.HasFinalizer(existing, ManagedClaimFinalizer)
	if !needsFinalizer &&
		equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.Spec = desired.Spec
	updated.Labels = desired.Labels
	updated.OwnerReferences = desired.OwnerReferences
	if needsFinalizer {
		updated.Finalizers = append(updated.Finalizers, ManagedClaimFinalizer)
	}

	_, err = claims.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}
