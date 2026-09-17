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

package dra

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/dra/metadata"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("Resources DRA", func() {
	newVMI := func(dedicated bool, pageSize string) *v1.VirtualMachineInstance {
		vmi := &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vmi-with-dra",
				Namespace: "default",
				UID:       "1234-5678",
			},
			Spec: v1.VirtualMachineInstanceSpec{
				Domain: v1.DomainSpec{
					CPU: &v1.CPU{
						DedicatedCPUPlacement: dedicated,
					},
				},
			},
		}
		if pageSize != "" {
			vmi.Spec.Domain.Memory = &v1.Memory{
				Hugepages: &v1.Hugepages{PageSize: pageSize},
			}
		}
		return vmi
	}

	newConfig := func(gates ...string) *virtconfig.ClusterConfig {
		kv := &v1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{Name: "kubevirt", Namespace: "kubevirt"},
		}
		if len(gates) > 0 {
			kv.Spec.Configuration.DeveloperConfiguration = &v1.DeveloperConfiguration{
				FeatureGates: gates,
			}
		}
		config, _, _ := testutils.NewFakeClusterConfigUsingKV(kv)
		return config
	}

	DescribeTable("UsesCPUDRA",
		func(gateEnabled, dedicated, expected bool) {
			var config *virtconfig.ClusterConfig
			if gateEnabled {
				config = newConfig("CPUsWithDRA")
			} else {
				config = newConfig()
			}
			vmi := newVMI(dedicated, "")
			Expect(UsesCPUDRA(config, vmi)).To(Equal(expected))
		},
		Entry("gate disabled, dedicatedCpuPlacement unset", false, false, false),
		Entry("gate disabled, dedicatedCpuPlacement set", false, true, false),
		Entry("gate enabled, dedicatedCpuPlacement unset", true, false, false),
		Entry("gate enabled, dedicatedCpuPlacement set", true, true, true),
	)

	DescribeTable("UsesMemoryDRA",
		func(gateEnabled bool, pageSize string, expected bool) {
			var config *virtconfig.ClusterConfig
			if gateEnabled {
				config = newConfig("MemoryWithDRA")
			} else {
				config = newConfig()
			}
			vmi := newVMI(false, pageSize)
			Expect(UsesMemoryDRA(config, vmi)).To(Equal(expected))
		},
		Entry("gate disabled, hugepages unset", false, "", false),
		Entry("gate disabled, hugepages set", false, "1Gi", false),
		Entry("gate enabled, hugepages unset", true, "", false),
		Entry("gate enabled, hugepages set", true, "1Gi", true),
	)

	DescribeTable("HugepageDeviceClass",
		func(pageSize, expectedClass string, expectErr bool) {
			class, err := HugepageDeviceClass(pageSize)
			if expectErr {
				Expect(err).To(HaveOccurred())
				return
			}
			Expect(err).ToNot(HaveOccurred())
			Expect(class).To(Equal(expectedClass))
		},
		Entry("1Gi maps to dra.hugepages-1g", "1Gi", "dra.hugepages-1g", false),
		Entry("2Mi maps to dra.hugepages-2m", "2Mi", "dra.hugepages-2m", false),
		Entry("unknown size errors", "4Ki", "", true),
	)

	It("derives the claim name from the VMI name", func() {
		vmi := newVMI(true, "")
		Expect(ResourcesClaimName(vmi)).To(Equal("vmi-with-dra-dra"))
	})

	Context("NewResourcesClaim", func() {
		It("builds a CPU-only claim shape identical in intent to the pre-rework CPU-only claim: single request, no constraint", func() {
			vmi := newVMI(true, "")
			claim := NewResourcesClaim(vmi, 10, nil, "")

			Expect(claim.Name).To(Equal("vmi-with-dra-dra"))
			Expect(claim.Namespace).To(Equal("default"))

			Expect(claim.OwnerReferences).To(HaveLen(1))
			Expect(claim.OwnerReferences[0].Name).To(Equal(vmi.Name))
			Expect(claim.OwnerReferences[0].UID).To(Equal(vmi.UID))
			Expect(claim.OwnerReferences[0].Kind).To(Equal("VirtualMachineInstance"))
			Expect(claim.OwnerReferences[0].Controller).ToNot(BeNil())
			Expect(*claim.OwnerReferences[0].Controller).To(BeTrue())

			Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
			request := claim.Spec.Devices.Requests[0]
			Expect(request.Name).To(Equal(CPURequestName))
			Expect(request.Exactly).ToNot(BeNil())
			Expect(request.Exactly.DeviceClassName).To(Equal(CPUDeviceClassName))
			Expect(request.Exactly.Capacity).ToNot(BeNil())

			quantity, ok := request.Exactly.Capacity.Requests[CPUCapacityName]
			Expect(ok).To(BeTrue())
			Expect(quantity.Value()).To(Equal(int64(10)))

			Expect(claim.Spec.Devices.Constraints).To(BeEmpty())
		})

		It("builds a combined CPU+memory claim with a NUMA-alignment constraint when both are requested", func() {
			vmi := newVMI(true, "1Gi")
			memSize := resource.MustParse("4Gi")
			claim := NewResourcesClaim(vmi, 8, &memSize, "dra.hugepages-1g")

			Expect(claim.Spec.Devices.Requests).To(HaveLen(2))

			cpuReq := claim.Spec.Devices.Requests[0]
			Expect(cpuReq.Name).To(Equal(CPURequestName))
			Expect(cpuReq.Exactly.DeviceClassName).To(Equal(CPUDeviceClassName))

			memReq := claim.Spec.Devices.Requests[1]
			Expect(memReq.Name).To(Equal(MemoryRequestName))
			Expect(memReq.Exactly.DeviceClassName).To(Equal("dra.hugepages-1g"))
			quantity, ok := memReq.Exactly.Capacity.Requests[MemoryCapacityName]
			Expect(ok).To(BeTrue())
			Expect(quantity.Value()).To(Equal(memSize.Value()))

			Expect(claim.Spec.Devices.Constraints).To(HaveLen(1))
			constraint := claim.Spec.Devices.Constraints[0]
			Expect(constraint.MatchAttribute).ToNot(BeNil())
			Expect(string(*constraint.MatchAttribute)).To(Equal(string(metadata.NUMANodeAttribute)))
			Expect(constraint.Requests).To(ConsistOf(CPURequestName, MemoryRequestName))
		})

		It("omits the mem request and constraint when memorySize is nil", func() {
			vmi := newVMI(true, "")
			claim := NewResourcesClaim(vmi, 8, nil, "")

			Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
			Expect(claim.Spec.Devices.Constraints).To(BeEmpty())
		})
	})
})
