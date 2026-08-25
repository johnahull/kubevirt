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

package vmi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/pointer"
)

var _ = Describe("Managed claim conditions", func() {
	const (
		vmiName   = "testvmi"
		namespace = "default"
	)

	condMgr := controller.NewVirtualMachineInstanceConditionManager()

	newVMI := func(claims ...v1.VirtualMachineInstanceResourceClaim) *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: vmiName, Namespace: namespace},
			Spec:       v1.VirtualMachineInstanceSpec{ResourceClaims: claims},
		}
	}

	managedEntry := func(name string) v1.VirtualMachineInstanceResourceClaim {
		return v1.VirtualMachineInstanceResourceClaim{
			Name:                        name,
			ManagedClaimProvisionerName: pointer.P("pcie-aligned"),
		}
	}

	directEntry := func(name string) v1.VirtualMachineInstanceResourceClaim {
		return v1.VirtualMachineInstanceResourceClaim{
			Name:              name,
			ResourceClaimName: pointer.P(name + "-claim"),
		}
	}

	claimFor := func(entryName string, allocated bool) *resourcev1.ResourceClaim {
		rc := &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dra.ManagedClaimName(vmiName, entryName),
				Namespace: namespace,
			},
		}
		if allocated {
			rc.Status.Allocation = &resourcev1.AllocationResult{}
		}
		return rc
	}

	It("adds no condition when the VMI has no managed claim entries", func() {
		vmi := newVMI(directEntry("data"))

		aggregateManagedClaimsConditions(vmi, nil)

		Expect(condMgr.HasCondition(vmi, v1.VirtualMachineInstanceManagedClaimsReady)).To(BeFalse())
	})

	It("reports not ready when a managed claim has no ResourceClaim yet", func() {
		vmi := newVMI(managedEntry("gpu"))

		aggregateManagedClaimsConditions(vmi, nil)

		Expect(condMgr.HasConditionWithStatusAndReason(
			vmi,
			v1.VirtualMachineInstanceManagedClaimsReady,
			k8sv1.ConditionFalse,
			v1.VirtualMachineInstanceReasonNotAllManagedClaimsReady,
		)).To(BeTrue())
	})

	It("reports not ready when the ResourceClaim exists but is unallocated", func() {
		vmi := newVMI(managedEntry("gpu"))

		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{claimFor("gpu", false)})

		Expect(condMgr.HasConditionWithStatus(
			vmi,
			v1.VirtualMachineInstanceManagedClaimsReady,
			k8sv1.ConditionFalse,
		)).To(BeTrue())
	})

	It("reports ready when every managed ResourceClaim is allocated", func() {
		vmi := newVMI(managedEntry("gpu"), managedEntry("nic"))

		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{
			claimFor("gpu", true),
			claimFor("nic", true),
		})

		Expect(condMgr.HasConditionWithStatusAndReason(
			vmi,
			v1.VirtualMachineInstanceManagedClaimsReady,
			k8sv1.ConditionTrue,
			v1.VirtualMachineInstanceReasonAllManagedClaimsReady,
		)).To(BeTrue())
	})

	It("reports not ready when one of several managed claims is still unallocated", func() {
		vmi := newVMI(managedEntry("gpu"), managedEntry("nic"))

		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{
			claimFor("gpu", true),
			claimFor("nic", false),
		})

		Expect(condMgr.HasConditionWithStatus(
			vmi,
			v1.VirtualMachineInstanceManagedClaimsReady,
			k8sv1.ConditionFalse,
		)).To(BeTrue())
	})

	It("ignores direct and template claim entries", func() {
		vmi := newVMI(directEntry("data"))

		aggregateManagedClaimsConditions(vmi, nil)

		Expect(condMgr.HasCondition(vmi, v1.VirtualMachineInstanceManagedClaimsReady)).To(BeFalse())
	})

	It("flips the condition in place across re-runs without duplicating it", func() {
		vmi := newVMI(managedEntry("gpu"))

		countReadyConditions := func() int {
			count := 0
			for _, c := range vmi.Status.Conditions {
				if c.Type == v1.VirtualMachineInstanceManagedClaimsReady {
					count++
				}
			}
			return count
		}

		// Not allocated yet: False.
		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{claimFor("gpu", false)})
		Expect(condMgr.HasConditionWithStatus(
			vmi, v1.VirtualMachineInstanceManagedClaimsReady, k8sv1.ConditionFalse,
		)).To(BeTrue())
		Expect(countReadyConditions()).To(Equal(1))

		// Allocated: flips to True in place.
		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{claimFor("gpu", true)})
		Expect(condMgr.HasConditionWithStatus(
			vmi, v1.VirtualMachineInstanceManagedClaimsReady, k8sv1.ConditionTrue,
		)).To(BeTrue())
		Expect(countReadyConditions()).To(Equal(1))

		// De-allocated again: flips back to False, still a single condition.
		aggregateManagedClaimsConditions(vmi, []*resourcev1.ResourceClaim{claimFor("gpu", false)})
		Expect(condMgr.HasConditionWithStatus(
			vmi, v1.VirtualMachineInstanceManagedClaimsReady, k8sv1.ConditionFalse,
		)).To(BeTrue())
		Expect(countReadyConditions()).To(Equal(1))
	})
})
