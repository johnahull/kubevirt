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

package aligner

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
	"kubevirt.io/client-go/testutils"

	managedclaim "kubevirt.io/kubevirt/pkg/managed-claim"
)

func TestAligner(t *testing.T) {
	testutils.KubeVirtTestSuiteSetup(t)
}

func claimRequest(claimName, requestName string) *v1.ClaimRequest {
	return &v1.ClaimRequest{ClaimName: claimName, RequestName: requestName}
}

func deviceType(name corev1alpha1.ManagedClaimDeviceTypeName, deviceClass string) corev1alpha1.ManagedClaimDeviceType {
	return corev1alpha1.ManagedClaimDeviceType{Name: name, DeviceClassName: deviceClass}
}

func contextFor(
	vmi *v1.VirtualMachineInstance,
	claimName string,
	deviceTypes ...corev1alpha1.ManagedClaimDeviceType,
) *managedclaim.ManagedClaimContext {
	devices, err := managedclaim.CollectDevices(vmi, claimName)
	Expect(err).ToNot(HaveOccurred())

	return &managedclaim.ManagedClaimContext{
		VMI:   vmi,
		Claim: &v1.VirtualMachineInstanceResourceClaim{Name: claimName},
		Provisioner: &corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "pcie-aligned"},
			Spec: corev1alpha1.ManagedClaimProvisionerSpec{
				Provisioner: ProvisionerName,
				DeviceTypes: deviceTypes,
			},
		},
		Devices: devices,
	}
}

var _ = Describe("Topology aligner", func() {
	Describe("VEP worked examples", func() {
		It("should match the single-GPU example", func() {
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-vm", Namespace: "default"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{Devices: v1.Devices{
						GPUs: []v1.GPU{{Name: "gpu0", ClaimRequest: claimRequest("my-gpu", "gpu")}},
					}},
				},
			}

			spec, err := (&Provisioner{}).GenerateClaim(contextFor(vmi, "my-gpu",
				deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")))

			Expect(err).ToNot(HaveOccurred())
			Expect(spec.Devices.Requests).To(Equal([]resourcev1.DeviceRequest{{
				Name: "gpu",
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu.example.com",
					Count:           1,
					AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
				},
			}}))
			// A single device has nothing to align against.
			Expect(spec.Devices.Constraints).To(BeEmpty())
		})

		It("should match the GPU + NIC PCIe-root example", func() {
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-nic-vm", Namespace: "default"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{Devices: v1.Devices{
						GPUs:       []v1.GPU{{Name: "gpu0", ClaimRequest: claimRequest("aligned-devices", "gpu")}},
						Interfaces: []v1.Interface{{Name: "rdma-nic"}},
					}},
					Networks: []v1.Network{{
						Name:          "rdma-nic",
						NetworkSource: v1.NetworkSource{ResourceClaim: claimRequest("aligned-devices", "nic")},
					}},
				},
			}

			spec, err := (&Provisioner{}).GenerateClaim(contextFor(vmi, "aligned-devices",
				deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
				deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com")))

			Expect(err).ToNot(HaveOccurred())
			Expect(spec.Devices.Requests).To(HaveLen(2))
			Expect(spec.Devices.Constraints).To(Equal([]resourcev1.DeviceConstraint{{
				MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(PCIeRootAttribute)),
				Requests:       []string{"gpu", "nic"},
			}}))
		})

		It("should match the multi-GPU + NIC full-topology example", func() {
			// The VEP's third example, minus its CPU request: cpu stays inert
			// until VEP-152 lands. The PCIe constraint covers gpu0, gpu1 and
			// nic; the NUMA constraint covers everything and carries no
			// explicit request list.
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "full-topology-vm", Namespace: "default"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{Devices: v1.Devices{
						GPUs: []v1.GPU{
							{Name: "gpu0", ClaimRequest: claimRequest("all-devices", "gpu0")},
							{Name: "gpu1", ClaimRequest: claimRequest("all-devices", "gpu1")},
						},
						Interfaces: []v1.Interface{{Name: "rdma-nic"}},
					}},
					Networks: []v1.Network{{
						Name:          "rdma-nic",
						NetworkSource: v1.NetworkSource{ResourceClaim: claimRequest("all-devices", "nic")},
					}},
				},
			}

			spec, err := (&Provisioner{}).GenerateClaim(contextFor(vmi, "all-devices",
				deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
				deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com")))

			Expect(err).ToNot(HaveOccurred())
			Expect(spec.Devices.Requests).To(HaveLen(3))
			Expect(spec.Devices.Constraints).To(Equal([]resourcev1.DeviceConstraint{{
				MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(PCIeRootAttribute)),
				Requests:       []string{"gpu0", "gpu1", "nic"},
			}}))
		})
	})

	Describe("NUMA alignment", func() {
		It("should add an unscoped NUMA constraint once CPUs join the claim", func() {
			// CPUs are affine to multiple PCIe roots, so they align on NUMA
			// rather than PCIe root. The NUMA constraint is emitted without a
			// request list so it covers every request in the claim.
			ctx := contextFor(&v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{Devices: v1.Devices{
						GPUs: []v1.GPU{{Name: "gpu0", ClaimRequest: claimRequest("all", "gpu0")}},
					}},
				},
			}, "all",
				deviceType(corev1alpha1.ManagedClaimDeviceTypeCPU, "cpu.dra.k8s.io"),
				deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"))
			ctx.Devices.CPU = &managedclaim.CPUDRASource{ClaimName: "all", RequestName: "cpus"}

			spec, err := (&Provisioner{}).GenerateClaim(ctx)

			Expect(err).ToNot(HaveOccurred())
			Expect(spec.Devices.Constraints).To(ContainElement(resourcev1.DeviceConstraint{
				MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(NUMANodeAttribute)),
			}))
		})
	})

	Describe("errors", func() {
		It("should fail when a device type has no DeviceClass mapping", func() {
			vmi := &v1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
				Spec: v1.VirtualMachineInstanceSpec{
					Domain: v1.DomainSpec{Devices: v1.Devices{
						GPUs: []v1.GPU{{Name: "gpu0", ClaimRequest: claimRequest("c", "gpu")}},
					}},
				},
			}

			_, err := (&Provisioner{}).GenerateClaim(contextFor(vmi, "c",
				deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com")))

			Expect(err).To(MatchError(ContainSubstring("no deviceClassName configured")))
		})
	})
})
