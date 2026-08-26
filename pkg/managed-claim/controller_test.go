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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = Describe("Controller", func() {
	var (
		ctrl         *Controller
		mockQueue    *testutils.MockWorkQueue[string]
		recorder     *record.FakeRecorder
		client       *fake.Clientset
		store        *stubProvisionerStore
		vmi          *v1.VirtualMachineInstance
		vmiNamespace = "default"
		derivedName  = "gpu-vm-my-gpu"
	)

	newVMI := func() *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
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
	}

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
		reconciler := NewReconciler(testProvisionerName, &stubProvisioner{}, client, store)

		vmiInformer, _ := testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
		provisionerInformer, _ := testutils.NewFakeInformerFor(&corev1alpha1.ManagedClaimProvisioner{})
		claimInformer, _ := testutils.NewFakeInformerFor(&resourcev1.ResourceClaim{})

		recorder = record.NewFakeRecorder(10)

		var err error
		ctrl, err = NewController(reconciler, recorder, vmiInformer, provisionerInformer, claimInformer)
		Expect(err).ToNot(HaveOccurred())

		mockQueue = testutils.NewMockWorkQueue(ctrl.Queue)
		ctrl.Queue = mockQueue

		vmi = newVMI()
	})

	getClaim := func() (*resourcev1.ResourceClaim, error) {
		return client.ResourceV1().ResourceClaims(vmiNamespace).
			Get(context.Background(), derivedName, metav1.GetOptions{})
	}

	Context("Execute", func() {
		It("should reconcile a queued VMI and create its claim", func() {
			Expect(ctrl.vmiInformer.GetStore().Add(vmi)).To(Succeed())
			ctrl.Queue.Add(vmiNamespace + "/gpu-vm")

			Expect(ctrl.Execute()).To(BeTrue())

			_, err := getClaim()
			Expect(err).ToNot(HaveOccurred())
			Expect(mockQueue.GetRateLimitedEnqueueCount()).To(BeZero(),
				"a successful reconcile must not re-enqueue")
		})

		It("should be a no-op when the VMI is gone from the cache", func() {
			ctrl.Queue.Add(vmiNamespace + "/gpu-vm")

			Expect(ctrl.Execute()).To(BeTrue())

			_, err := getClaim()
			Expect(err).To(HaveOccurred())
			Expect(mockQueue.GetRateLimitedEnqueueCount()).To(BeZero())
		})

		It("should record a warning event and re-enqueue when reconcile fails", func() {
			vmi.Spec.ResourceClaims[0].ManagedClaimProvisionerName = pointer.P("does-not-exist")
			Expect(ctrl.vmiInformer.GetStore().Add(vmi)).To(Succeed())
			ctrl.Queue.Add(vmiNamespace + "/gpu-vm")

			Expect(ctrl.Execute()).To(BeTrue())

			Expect(mockQueue.GetRateLimitedEnqueueCount()).To(Equal(1),
				"a failed reconcile must be retried")
			var event string
			Eventually(recorder.Events).Should(Receive(&event))
			Expect(event).To(ContainSubstring("Warning"))
			// The reason is a user-facing contract: admins select these events
			// by reason, so pin it rather than only the free-text message.
			Expect(event).To(ContainSubstring(FailedProvisioningReason))
			Expect(event).To(ContainSubstring("does-not-exist"))
		})

		It("should stop when the queue shuts down", func() {
			ctrl.Queue.ShutDown()
			Expect(ctrl.Execute()).To(BeFalse())
		})
	})

	Context("enqueueing", func() {
		It("should enqueue a VMI on its own event", func() {
			ctrl.enqueueVMI(vmi)

			Expect(ctrl.Queue.Len()).To(Equal(1))
			key, _ := ctrl.Queue.Get()
			Expect(key).To(Equal(vmiNamespace + "/gpu-vm"))
		})

		It("should enqueue the owning VMI on a ResourceClaim event", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      derivedName,
					Namespace: vmiNamespace,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: v1.VirtualMachineInstanceGroupVersionKind.GroupVersion().String(),
						Kind:       v1.VirtualMachineInstanceGroupVersionKind.Kind,
						Name:       "gpu-vm",
						UID:        "vmi-uid",
						Controller: pointer.P(true),
					}},
				},
			}

			ctrl.enqueueResourceClaim(claim)

			Expect(ctrl.Queue.Len()).To(Equal(1))
			key, _ := ctrl.Queue.Get()
			Expect(key).To(Equal(vmiNamespace + "/gpu-vm"))
		})

		It("should ignore a ResourceClaim not owned by a VMI", func() {
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: vmiNamespace},
			}

			ctrl.enqueueResourceClaim(claim)

			Expect(ctrl.Queue.Len()).To(BeZero())
		})

		It("should enqueue only VMIs referencing a changed provisioner", func() {
			Expect(ctrl.vmiInformer.GetStore().Add(vmi)).To(Succeed())
			other := newVMI()
			other.Name = "other-vm"
			other.Spec.ResourceClaims[0].ManagedClaimProvisionerName = pointer.P("some-other")
			Expect(ctrl.vmiInformer.GetStore().Add(other)).To(Succeed())

			ctrl.enqueueProvisioner(&corev1alpha1.ManagedClaimProvisioner{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-default"},
			})

			Expect(ctrl.Queue.Len()).To(Equal(1))
			key, _ := ctrl.Queue.Get()
			Expect(key).To(Equal(vmiNamespace + "/gpu-vm"))
		})
	})

	It("reports readiness once the caches have synced", func() {
		Expect(ctrl.hasSynced).ToNot(BeNil())
	})
})
