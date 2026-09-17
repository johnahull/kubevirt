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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("CPU DRA", func() {
	newVMI := func(dedicated bool) *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vmi-with-cpu-dra",
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
	}

	newConfig := func(gateEnabled bool) *virtconfig.ClusterConfig {
		kv := &v1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{Name: "kubevirt", Namespace: "kubevirt"},
		}
		if gateEnabled {
			kv.Spec.Configuration.DeveloperConfiguration = &v1.DeveloperConfiguration{
				FeatureGates: []string{"CPUsWithDRA"},
			}
		}
		config, _, _ := testutils.NewFakeClusterConfigUsingKV(kv)
		return config
	}

	DescribeTable("UsesCPUDRA",
		func(gateEnabled, dedicated, expected bool) {
			config := newConfig(gateEnabled)
			vmi := newVMI(dedicated)
			Expect(UsesCPUDRA(config, vmi)).To(Equal(expected))
		},
		Entry("gate disabled, dedicatedCpuPlacement unset", false, false, false),
		Entry("gate disabled, dedicatedCpuPlacement set", false, true, false),
		Entry("gate enabled, dedicatedCpuPlacement unset", true, false, false),
		Entry("gate enabled, dedicatedCpuPlacement set", true, true, true),
	)

	It("derives the claim name from the VMI name", func() {
		vmi := newVMI(true)
		Expect(CPUClaimName(vmi)).To(Equal("vmi-with-cpu-dra-cpu"))
	})

	It("builds a grouped-mode ResourceClaim owned by the VMI", func() {
		vmi := newVMI(true)
		claim := NewCPUResourceClaim(vmi, 10)

		Expect(claim.Name).To(Equal("vmi-with-cpu-dra-cpu"))
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
	})
})
