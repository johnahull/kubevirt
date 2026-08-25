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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/pointer"
)

const testProvisionerName = "policy.kubevirt.io/aligner"

// stubProvisioner returns a fixed spec, or an error, so reconciliation
// behaviour can be tested without depending on the aligner's policy.
type stubProvisioner struct {
	spec *resourcev1.ResourceClaimSpec
	err  error
}

func (s *stubProvisioner) GenerateClaim(*ManagedClaimContext) (*resourcev1.ResourceClaimSpec, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.spec != nil {
		return s.spec, nil
	}
	return &resourcev1.ResourceClaimSpec{
		Devices: resourcev1.DeviceClaim{
			Requests: []resourcev1.DeviceRequest{{
				Name: "gpu",
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu.example.com",
					Count:           1,
					AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
				},
			}},
		},
	}, nil
}

type stubProvisionerStore struct {
	known map[string]*corev1alpha1.ManagedClaimProvisioner
}

func (s *stubProvisionerStore) Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error) {
	if p, ok := s.known[name]; ok {
		return p, nil
	}
	return nil, errors.NewNotFound(corev1alpha1.Resource(corev1alpha1.ResourceManagedClaimProvisioners), name)
}

var _ = Describe("Reconciler", func() {
	var (
		client       *fake.Clientset
		store        *stubProvisionerStore
		provisioner  *stubProvisioner
		reconciler   *Reconciler
		vmi          *v1.VirtualMachineInstance
		derivedName  = "gpu-vm-my-gpu"
		vmiNamespace = "default"
	)

	BeforeEach(func() {
		client = fake.NewSimpleClientset()
		store = &stubProvisionerStore{
			known: map[string]*corev1alpha1.ManagedClaimProvisioner{
				"gpu-default": {
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-default"},
					Spec: corev1alpha1.ManagedClaimProvisionerSpec{
						Provisioner: testProvisionerName,
						DeviceTypes: []corev1alpha1.ManagedClaimDeviceType{{
							Name:            corev1alpha1.ManagedClaimDeviceTypeGPU,
							DeviceClassName: "gpu.example.com",
						}},
					},
				},
			},
		}
		provisioner = &stubProvisioner{}
		reconciler = NewReconciler(testProvisionerName, provisioner, client, store)

		vmi = &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gpu-vm",
				Namespace: vmiNamespace,
				UID:       "vmi-uid",
			},
			Spec: v1.VirtualMachineInstanceSpec{
				ResourceClaims: []v1.VirtualMachineInstanceResourceClaim{{
					Name:                        "my-gpu",
					ManagedClaimProvisionerName: pointer.P("gpu-default"),
				}},
				Domain: v1.DomainSpec{Devices: v1.Devices{
					GPUs: []v1.GPU{{Name: "gpu0", ClaimRequest: claimRequest("my-gpu", "gpu")}},
				}},
			},
		}
	})

	getClaim := func() (*resourcev1.ResourceClaim, error) {
		return client.ResourceV1().ResourceClaims(vmiNamespace).
			Get(context.Background(), derivedName, metav1.GetOptions{})
	}

	Context("creating", func() {
		It("should create the ResourceClaim under the derived name", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
		})

		It("should own the claim through the VMI so it is garbage collected", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.OwnerReferences).To(HaveLen(1))
			Expect(claim.OwnerReferences[0].Name).To(Equal("gpu-vm"))
			Expect(claim.OwnerReferences[0].UID).To(Equal(vmi.UID))
			Expect(claim.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))
		})

		It("should label the claim with its entry and provisioner", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Labels).To(HaveKeyWithValue(ManagedClaimLabel, "my-gpu"))
			Expect(claim.Labels).To(HaveKeyWithValue(ManagedClaimProvisionerLabel, "gpu-default"))
		})

		It("should label the claim with the VMI name", func() {
			// The VMI name is carried on the owner reference already; a label
			// makes claims selectable by VMI without walking owner references.
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Labels).To(HaveKeyWithValue(ManagedClaimVMILabel, "gpu-vm"))
		})

		It("should protect the created claim with a finalizer", func() {
			// An external delete must not tear the claim out from under a
			// running VMI; the finalizer keeps the object present until this
			// controller releases it during VMI teardown.
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Finalizers).To(ContainElement(ManagedClaimFinalizer))
		})

		It("should skip entries that are not managed", func() {
			vmi.Spec.ResourceClaims = []v1.VirtualMachineInstanceResourceClaim{
				{Name: "direct", ResourceClaimName: pointer.P("some-claim")},
			}

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claims, err := client.ResourceV1().ResourceClaims(vmiNamespace).
				List(context.Background(), metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(claims.Items).To(BeEmpty())
		})

		It("should skip entries served by a different provisioner", func() {
			store.known["gpu-default"].Spec.Provisioner = "vendor.example.com/other"

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			_, err := getClaim()
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should not create claims for a VMI being deleted", func() {
			now := metav1.Now()
			vmi.DeletionTimestamp = &now

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			_, err := getClaim()
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("converging", func() {
		It("should be idempotent", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claims, err := client.ResourceV1().ResourceClaims(vmiNamespace).
				List(context.Background(), metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(claims.Items).To(HaveLen(1))
		})

		It("should not issue an update when the claim already matches", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())
			client.ClearActions()

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			for _, action := range client.Actions() {
				Expect(action.GetVerb()).ToNot(Equal("update"),
					"an unchanged claim must not be rewritten on every reconcile")
			}
		})

		It("should converge a claim that drifted", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			claim.Spec.Devices.Requests[0].Exactly.DeviceClassName = "tampered.example.com"
			_, err = client.ResourceV1().ResourceClaims(vmiNamespace).
				Update(context.Background(), claim, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err = getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Spec.Devices.Requests[0].Exactly.DeviceClassName).To(Equal("gpu.example.com"))
		})

		It("should restore its finalizer if an existing claim is missing it", func() {
			// A claim created before this feature, or one whose finalizer was
			// stripped, must be protected again on the next reconcile.
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			claim.Finalizers = nil
			_, err = client.ResourceV1().ResourceClaims(vmiNamespace).
				Update(context.Background(), claim, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err = getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Finalizers).To(ContainElement(ManagedClaimFinalizer))
		})

		It("should re-create a claim deleted out of band", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())
			Expect(client.ResourceV1().ResourceClaims(vmiNamespace).
				Delete(context.Background(), derivedName, metav1.DeleteOptions{})).To(Succeed())

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			_, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("releasing", func() {
		It("should release its finalizer from managed claims when the VMI is being deleted", func() {
			// Without releasing the finalizer, owner-reference GC could never
			// remove the claim and VMI deletion would wedge forever.
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			now := metav1.Now()
			vmi.DeletionTimestamp = &now
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Finalizers).ToNot(ContainElement(ManagedClaimFinalizer))
		})

		It("should release its finalizer from a claim deleted externally while the VMI runs", func() {
			// An external delete leaves the claim terminating (the finalizer
			// holds it in the API server). The controller must release the
			// finalizer so GC finishes; a later reconcile recreates it.
			now := metav1.Now()
			terminating := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:              derivedName,
					Namespace:         vmiNamespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{ManagedClaimFinalizer},
				},
			}
			Expect(client.Tracker().Add(terminating)).To(Succeed())

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Finalizers).ToNot(ContainElement(ManagedClaimFinalizer))
		})

		It("should not touch claims served by a different provisioner during teardown", func() {
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())
			store.known["gpu-default"].Spec.Provisioner = "vendor.example.com/other"

			now := metav1.Now()
			vmi.DeletionTimestamp = &now
			Expect(reconciler.Reconcile(context.Background(), vmi)).To(Succeed())

			claim, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(claim.Finalizers).To(ContainElement(ManagedClaimFinalizer))
		})
	})

	Context("failures", func() {
		It("should report the claim entry and provisioner when generation fails", func() {
			provisioner.err = fmt.Errorf("no deviceClassName configured for device type \"gpu\"")

			err := reconciler.Reconcile(context.Background(), vmi)

			Expect(err).To(MatchError(ContainSubstring("my-gpu")))
			Expect(err).To(MatchError(ContainSubstring("gpu-default")))
			Expect(err).To(MatchError(ContainSubstring("no deviceClassName configured")))
		})

		It("should report a missing provisioner rather than silently skipping", func() {
			vmi.Spec.ResourceClaims[0].ManagedClaimProvisionerName = pointer.P("does-not-exist")

			err := reconciler.Reconcile(context.Background(), vmi)

			Expect(err).To(MatchError(ContainSubstring("does-not-exist")))
		})

		It("should keep reconciling the remaining claims after one fails", func() {
			// Two managed entries; the first has no provisioner. The second
			// must still be created, otherwise one typo strands every other
			// claim on the VMI.
			vmi.Spec.ResourceClaims = append([]v1.VirtualMachineInstanceResourceClaim{{
				Name:                        "broken",
				ManagedClaimProvisionerName: pointer.P("does-not-exist"),
			}}, vmi.Spec.ResourceClaims...)

			err := reconciler.Reconcile(context.Background(), vmi)

			Expect(err).To(HaveOccurred())
			_, getErr := getClaim()
			Expect(getErr).ToNot(HaveOccurred())
		})

		It("should surface an API error from the create", func() {
			client.PrependReactor("create", "resourceclaims",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("apiserver unavailable")
				})

			Expect(reconciler.Reconcile(context.Background(), vmi)).To(
				MatchError(ContainSubstring("apiserver unavailable")))
		})
	})
})
