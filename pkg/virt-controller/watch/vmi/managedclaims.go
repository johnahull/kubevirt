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
	k8sv1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/dra"
)

// aggregateManagedClaimsConditions mirrors the state of the managed
// ResourceClaims generated for the VMI into a single ManagedClaimsReady
// condition.
//
// It follows the same single-writer discipline as aggregateDataVolumesConditions:
// the provisioner controllers own the ResourceClaims and never touch VMI status,
// while virt-controller reflects that owned state onto the VMI as its sole
// writer. The condition is diagnostic; it does not gate pod creation.
func aggregateManagedClaimsConditions(vmi *v1.VirtualMachineInstance, claims []*resourcev1.ResourceClaim) {
	var managedEntries []string
	for i := range vmi.Spec.ResourceClaims {
		claim := vmi.Spec.ResourceClaims[i]
		if dra.IsManagedClaim(claim) {
			managedEntries = append(managedEntries, claim.Name)
		}
	}
	// A VMI spec is immutable after admission, so managed entries are never
	// removed from a VMI that once had them; there is no stale condition to
	// clear here. This mirrors aggregateDataVolumesConditions.
	if len(managedEntries) == 0 {
		return
	}

	claimsByName := make(map[string]*resourcev1.ResourceClaim, len(claims))
	for _, claim := range claims {
		claimsByName[claim.Name] = claim
	}

	ready := true
	for _, entryName := range managedEntries {
		claim, found := claimsByName[dra.ManagedClaimName(vmi.Name, entryName)]
		if !found || claim.Status.Allocation == nil {
			ready = false
			break
		}
	}

	condition := v1.VirtualMachineInstanceCondition{
		Type:    v1.VirtualMachineInstanceManagedClaimsReady,
		Status:  k8sv1.ConditionTrue,
		Reason:  v1.VirtualMachineInstanceReasonAllManagedClaimsReady,
		Message: "All of the VMI's managed ResourceClaims are created and allocated",
	}
	if !ready {
		condition.Status = k8sv1.ConditionFalse
		condition.Reason = v1.VirtualMachineInstanceReasonNotAllManagedClaimsReady
		condition.Message = "Not all of the VMI's managed ResourceClaims are created and allocated"
	}

	controller.NewVirtualMachineInstanceConditionManager().UpdateCondition(vmi, &condition)
}
