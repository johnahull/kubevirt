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

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("ResourceClaims", func() {
	DescribeTable("should convert VMI resourceClaims to Pod resourceClaims",
		func(resourceClaims []v1.VirtualMachineInstanceResourceClaim, expected []k8sv1.PodResourceClaim) {
			Expect(ToPodResourceClaims("test-vmi", resourceClaims)).To(Equal(expected))
		},
		Entry("nil resourceClaims",
			nil,
			nil,
		),
		Entry("empty resourceClaims",
			[]v1.VirtualMachineInstanceResourceClaim{},
			nil,
		),
		Entry("managed claim renders the deterministic claim name",
			// The provisioner controller creates the ResourceClaim out of band
			// under this same derived name, so the pod references it directly.
			[]v1.VirtualMachineInstanceResourceClaim{
				{
					Name:                        "aligned-devices",
					ManagedClaimProvisionerName: ptr.To("pcie-aligned"),
				},
			},
			[]k8sv1.PodResourceClaim{
				{
					Name:              "aligned-devices",
					ResourceClaimName: ptr.To("test-vmi-aligned-devices"),
				},
			},
		),
		Entry("managed, direct, and template claims side by side",
			[]v1.VirtualMachineInstanceResourceClaim{
				{
					Name:                        "managed-claim",
					ManagedClaimProvisionerName: ptr.To("pcie-aligned"),
				},
				{
					Name:              "direct-claim",
					ResourceClaimName: ptr.To("resource-claim"),
				},
				{
					Name:                      "template-claim",
					ResourceClaimTemplateName: ptr.To("resource-claim-template"),
				},
			},
			[]k8sv1.PodResourceClaim{
				{
					Name:              "managed-claim",
					ResourceClaimName: ptr.To("test-vmi-managed-claim"),
				},
				{
					Name:              "direct-claim",
					ResourceClaimName: ptr.To("resource-claim"),
				},
				{
					Name:                      "template-claim",
					ResourceClaimTemplateName: ptr.To("resource-claim-template"),
				},
			},
		),
		Entry("direct and template resourceClaims",
			[]v1.VirtualMachineInstanceResourceClaim{
				{
					Name:              "direct-claim",
					ResourceClaimName: ptr.To("resource-claim"),
				},
				{
					Name:                      "template-claim",
					ResourceClaimTemplateName: ptr.To("resource-claim-template"),
				},
			},
			[]k8sv1.PodResourceClaim{
				{
					Name:              "direct-claim",
					ResourceClaimName: ptr.To("resource-claim"),
				},
				{
					Name:                      "template-claim",
					ResourceClaimTemplateName: ptr.To("resource-claim-template"),
				},
			},
		),
	)
})
