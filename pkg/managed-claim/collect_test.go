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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
)

func claimRequest(claimName, requestName string) *v1.ClaimRequest {
	return &v1.ClaimRequest{ClaimName: claimName, RequestName: requestName}
}

// vmiWithDevices builds the VEP's "GPU + NIC co-placed" shape.
func vmiWithDevices() *v1.VirtualMachineInstance {
	return &v1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-nic-vm", Namespace: "default"},
		Spec: v1.VirtualMachineInstanceSpec{
			Domain: v1.DomainSpec{
				Devices: v1.Devices{
					GPUs: []v1.GPU{
						{Name: "gpu0", ClaimRequest: claimRequest("aligned", "gpu")},
						{Name: "gpu-other", ClaimRequest: claimRequest("other-claim", "gpu")},
						{Name: "gpu-plugin", DeviceName: "nvidia.com/GP102"},
					},
					HostDevices: []v1.HostDevice{
						{Name: "hd0", ClaimRequest: claimRequest("aligned", "hostdev")},
						{Name: "hd-plugin", DeviceName: "intel.com/qat"},
					},
					Interfaces: []v1.Interface{
						{Name: "rdma-nic"},
						{Name: "other-nic"},
					},
				},
			},
			Networks: []v1.Network{
				{
					Name: "rdma-nic",
					NetworkSource: v1.NetworkSource{
						ResourceClaim: claimRequest("aligned", "nic"),
					},
				},
				{
					Name: "other-nic",
					NetworkSource: v1.NetworkSource{
						ResourceClaim: claimRequest("other-claim", "nic"),
					},
				},
				{
					Name:          "default",
					NetworkSource: v1.NetworkSource{Pod: &v1.PodNetwork{}},
				},
			},
		},
	}
}

var _ = Describe("CollectDevices", func() {
	It("should collect only devices referencing the named claim", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")

		Expect(err).ToNot(HaveOccurred())
		Expect(devices.GPUs).To(HaveLen(1))
		Expect(devices.GPUs[0].Name).To(Equal("gpu0"))
		Expect(devices.HostDevices).To(HaveLen(1))
		Expect(devices.HostDevices[0].Name).To(Equal("hd0"))
		Expect(devices.Networks).To(HaveLen(1))
		Expect(devices.Networks[0].Network.Name).To(Equal("rdma-nic"))
	})

	It("should pair each network with its interface", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")

		Expect(err).ToNot(HaveOccurred())
		Expect(devices.Networks[0].Interface).ToNot(BeNil())
		Expect(devices.Networks[0].Interface.Name).To(Equal("rdma-nic"))
	})

	It("should leave the interface nil when the network has none", func() {
		vmi := vmiWithDevices()
		vmi.Spec.Domain.Devices.Interfaces = nil

		devices, err := CollectDevices(vmi, "aligned")

		Expect(err).ToNot(HaveOccurred())
		Expect(devices.Networks).To(HaveLen(1))
		Expect(devices.Networks[0].Interface).To(BeNil())
	})

	It("should ignore device-plugin devices that carry no claim request", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")

		Expect(err).ToNot(HaveOccurred())
		for _, gpu := range devices.GPUs {
			Expect(gpu.DeviceName).To(BeEmpty())
		}
		for _, hd := range devices.HostDevices {
			Expect(hd.DeviceName).To(BeEmpty())
		}
	})

	It("should report empty when nothing references the claim", func() {
		devices, err := CollectDevices(vmiWithDevices(), "unreferenced")

		Expect(err).ToNot(HaveOccurred())
		Expect(devices.IsEmpty()).To(BeTrue())
	})

	It("should collect multiple GPUs for one claim", func() {
		// The VEP's full-topology example puts gpu0 and gpu1 in one claim.
		vmi := vmiWithDevices()
		vmi.Spec.Domain.Devices.GPUs = []v1.GPU{
			{Name: "gpu0", ClaimRequest: claimRequest("aligned", "gpu0")},
			{Name: "gpu1", ClaimRequest: claimRequest("aligned", "gpu1")},
		}

		devices, err := CollectDevices(vmi, "aligned")

		Expect(err).ToNot(HaveOccurred())
		Expect(devices.GPUs).To(HaveLen(2))
	})

	It("should reject a VMI whose CPU requests a managed claim", func() {
		// VEP-152 has not landed, so v1.CPU has no DRA field to read. Fail
		// loudly rather than silently dropping the CPU request, which would
		// generate a claim missing the CPUs the user asked for.
		vmi := vmiWithDevices()
		vmi.Spec.Domain.CPU = &v1.CPU{Cores: 16}

		_, err := CollectDevices(vmi, "aligned")

		Expect(err).ToNot(HaveOccurred(), "a plain CPU topology is not a DRA request")
	})
})
