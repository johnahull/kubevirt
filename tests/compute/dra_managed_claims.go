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

package compute

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/libvmi"
	managedclaim "kubevirt.io/kubevirt/pkg/managed-claim"
	"kubevirt.io/kubevirt/pkg/managed-claim/aligner"
	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"

	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/flags"
	"kubevirt.io/kubevirt/tests/framework/checks"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libkubevirt/config"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/testsuite"
)

// These tests exercise the managed-claim controller's ResourceClaim generation,
// which happens the moment a VMI is created and is entirely independent of DRA
// allocation or pod scheduling. That makes the generated claim's metadata
// (name, labels, owner reference, finalizer) assertable with no DRA driver and
// no accelerator hardware, so this spec is CI-viable and deliberately carries
// no hardware decorator.
//
// Non-goal: this does not assert ManagedClaimsReady=True or that any device is
// allocated. Those require a real DRA driver and hardware and are covered by
// manual validation. The VMI pod is expected to stay Pending here.
var _ = Describe("[sig-compute]Managed DRA Claims", Serial, decorators.SigCompute, func() {
	const (
		managedProvisionerName = "vep300-aligner-hostdev"
		managedClaimEntryName  = "managed-hostdev"
		managedHostDevName     = "hostdev0"
		managedRequestName     = "req0"
		managedDeviceClass     = "hostdev.example.com"
	)

	// ensureFeatureGate enables a gate only when the cluster does not already
	// have it on, and schedules the matching disable only in that case. This
	// keeps cleanup state-restoring: a gate the test found on (for example on a
	// DRA cluster where ManagedDRAClaims is already enabled, or a default-on
	// Beta gate like HostDevicesWithDRA) is left on, and a gate the test turned
	// on is turned back off. An unconditional DisableFeatureGate would instead
	// force the gate off for the rest of the suite.
	ensureFeatureGate := func(gate string) {
		if checks.HasFeature(gate) {
			return
		}
		config.EnableFeatureGate(gate)
		DeferCleanup(config.DisableFeatureGate, gate)
	}

	BeforeEach(func() {
		By("enabling the ManagedDRAClaims and HostDevicesWithDRA feature gates")
		// ManagedDRAClaims (Alpha) is what makes virt-operator deploy the
		// managed-claim controller, so it must be enabled before the readiness
		// check below. HostDevicesWithDRA (Beta) admits the hostDevice claim
		// request on the VMI.
		ensureFeatureGate(featuregate.ManagedDRAClaimsGate)
		ensureFeatureGate(featuregate.HostDevicesWithDRAGate)

		By("waiting for the virt-managed-claim-controller deployment to be Ready")
		// Deployed by virt-operator only once ManagedDRAClaims is enabled
		// above, so this check is meaningful only after the gate is on. Kept
		// explicit so "feature not deployed" is distinguishable in CI triage
		// from "feature deployed but broken".
		Eventually(func(g Gomega) {
			deployment, err := kubevirt.Client().AppsV1().
				Deployments(flags.KubeVirtInstallNamespace).
				Get(context.Background(), components.VirtManagedClaimControllerName, metav1.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, timeout, pollingInterval).Should(Succeed(),
			"the %s deployment must be Ready before managed claims can be generated",
			components.VirtManagedClaimControllerName)
	})

	It("should generate a ResourceClaim from a VMI managed-claim entry", func() {
		namespace := testsuite.GetTestNamespace(nil)

		By("creating a cluster-scoped ManagedClaimProvisioner")
		provisioner := &corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: managedProvisionerName},
			Spec: corev1alpha1.ManagedClaimProvisionerSpec{
				Provisioner: aligner.ProvisionerName,
				DeviceTypes: []corev1alpha1.ManagedClaimDeviceType{{
					Name:            corev1alpha1.ManagedClaimDeviceTypeHostDevice,
					DeviceClassName: managedDeviceClass,
				}},
			},
		}
		_, err := kubevirt.Client().GeneratedKubeVirtClient().
			KubevirtV1alpha1().ManagedClaimProvisioners().
			Create(context.Background(), provisioner, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		// Registered before the VMI cleanup so it runs last (DeferCleanup is
		// LIFO): the VMI must be gone, and its finalizer released, before the
		// provisioner is removed. Deleting the provisioner first would send
		// releaseClaim down its provisioner-not-found path, leaking the
		// generated ResourceClaim across runs.
		DeferCleanup(func() {
			err := kubevirt.Client().GeneratedKubeVirtClient().
				KubevirtV1alpha1().ManagedClaimProvisioners().
				Delete(context.Background(), managedProvisionerName, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		By("creating a VMI whose hostDevice references the managed claim")
		provisionerRef := managedProvisionerName
		vmi := libvmifact.NewAlpine(
			libvmi.WithResourceClaim(v1.VirtualMachineInstanceResourceClaim{
				Name:                        managedClaimEntryName,
				ManagedClaimProvisionerName: &provisionerRef,
			}),
			libvmi.WithHostDevice(v1.HostDevice{
				Name: managedHostDevName,
				ClaimRequest: &v1.ClaimRequest{
					ClaimName:   managedClaimEntryName,
					RequestName: managedRequestName,
				},
			}),
		)
		createdVMI, err := kubevirt.Client().VirtualMachineInstance(namespace).
			Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		// Registered after the provisioner cleanup so it runs first: delete the
		// VMI and wait for it to be gone, which lets the controller release the
		// finalizer and owner-reference GC remove the generated claim.
		DeferCleanup(func() {
			err := kubevirt.Client().VirtualMachineInstance(namespace).
				Delete(context.Background(), createdVMI.Name, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				Expect(err).ToNot(HaveOccurred())
			}
			Eventually(func() bool {
				_, err := kubevirt.Client().VirtualMachineInstance(namespace).
					Get(context.Background(), createdVMI.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, timeout, pollingInterval).Should(BeTrue(),
				"the VMI must be fully deleted so its managed claim is released")
		})

		By("waiting for the generated ResourceClaim and asserting its metadata")
		claimName := dra.ManagedClaimName(createdVMI.Name, managedClaimEntryName)
		Eventually(func(g Gomega) {
			claim, err := kubevirt.Client().ResourceV1().ResourceClaims(namespace).
				Get(context.Background(), claimName, metav1.GetOptions{})
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(claim.Labels).To(HaveKeyWithValue(managedclaim.ManagedClaimLabel, managedClaimEntryName))
			g.Expect(claim.Labels).To(HaveKeyWithValue(managedclaim.ManagedClaimProvisionerLabel, managedProvisionerName))
			g.Expect(claim.Labels).To(HaveKeyWithValue(managedclaim.ManagedClaimVMILabel, createdVMI.Name))

			g.Expect(claim.Finalizers).To(ContainElement(managedclaim.ManagedClaimFinalizer))

			g.Expect(claim.OwnerReferences).To(ContainElement(SatisfyAll(
				HaveField("Kind", v1.VirtualMachineInstanceGroupVersionKind.Kind),
				HaveField("Name", createdVMI.Name),
				HaveField("UID", createdVMI.UID),
			)))
		}, timeout, pollingInterval).Should(Succeed())
	})
})
