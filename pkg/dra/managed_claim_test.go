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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/validation"
)

var _ = Describe("ManagedClaimName", func() {
	// The generated name is a wire contract between two independently-rolled
	// binaries: virt-controller renders it into the launcher pod, and the
	// provisioner controller names the ResourceClaim it creates. They must
	// agree, so this behaviour is frozen once shipped.

	DescribeTable("should join the VMI and claim names",
		func(vmiName, claimName, expected string) {
			Expect(ManagedClaimName(vmiName, claimName)).To(Equal(expected))
		},
		Entry("short names", "gpu-vm", "my-gpu", "gpu-vm-my-gpu"),
		Entry("VEP example: GPU + NIC", "gpu-nic-vm", "aligned-devices", "gpu-nic-vm-aligned-devices"),
		Entry("VEP example: full topology", "full-topology-vm", "all-devices", "full-topology-vm-all-devices"),
	)

	It("should leave a name at exactly the DNS subdomain limit untouched", func() {
		// vmiName + "-" + claimName == 253 characters exactly.
		claimName := "claim"
		vmiName := strings.Repeat("a", maxManagedClaimNameLen-len(claimName)-1)

		name := ManagedClaimName(vmiName, claimName)

		Expect(name).To(HaveLen(maxManagedClaimNameLen))
		Expect(name).To(Equal(vmiName + "-" + claimName))
	})

	It("should truncate and append a hash when one character over the limit", func() {
		claimName := "claim"
		vmiName := strings.Repeat("a", maxManagedClaimNameLen-len(claimName))
		untruncated := vmiName + "-" + claimName
		Expect(untruncated).To(HaveLen(maxManagedClaimNameLen + 1))

		name := ManagedClaimName(vmiName, claimName)

		Expect(name).To(HaveLen(maxManagedClaimNameLen))
		Expect(name).NotTo(Equal(untruncated))
		Expect(name).To(HavePrefix(untruncated[:maxManagedClaimNameLen-managedClaimHashLen-1]))
	})

	It("should distinguish names that differ only past the truncation point", func() {
		// The whole reason for hashing rather than plain truncation: two VMIs
		// whose names share a long prefix must not collide onto one claim.
		prefix := strings.Repeat("b", maxManagedClaimNameLen)

		first := ManagedClaimName(prefix+"-one", "claim")
		second := ManagedClaimName(prefix+"-two", "claim")

		Expect(first).To(HaveLen(maxManagedClaimNameLen))
		Expect(second).To(HaveLen(maxManagedClaimNameLen))
		Expect(first).NotTo(Equal(second))
	})

	It("should be deterministic across calls", func() {
		vmiName := strings.Repeat("c", 300)

		Expect(ManagedClaimName(vmiName, "claim")).To(Equal(ManagedClaimName(vmiName, "claim")))
	})

	DescribeTable("should always produce a valid DNS subdomain",
		func(vmiName, claimName string) {
			Expect(validation.NameIsDNSSubdomain(ManagedClaimName(vmiName, claimName), false)).To(BeEmpty())
		},
		Entry("short names", "gpu-vm", "my-gpu"),
		Entry("at the limit", strings.Repeat("a", maxManagedClaimNameLen-6), "claim"),
		Entry("well over the limit", strings.Repeat("a", 400), "claim"),
		Entry("long claim name", "vm", strings.Repeat("z", 400)),
	)
})
