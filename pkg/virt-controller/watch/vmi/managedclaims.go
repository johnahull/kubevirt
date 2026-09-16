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
	"fmt"
	"strings"

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
	managedProvisioners := make(map[string]string)
	for i := range vmi.Spec.ResourceClaims {
		claim := vmi.Spec.ResourceClaims[i]
		if dra.IsManagedClaim(claim) {
			managedEntries = append(managedEntries, claim.Name)
			managedProvisioners[claim.Name] = *claim.ManagedClaimProvisionerName
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

	var missing []string
	var unallocated []string
	for _, entryName := range managedEntries {
		resourceClaimName := dra.ManagedClaimName(vmi.Name, entryName)
		claim, found := claimsByName[resourceClaimName]
		if !found {
			missing = append(missing, fmt.Sprintf("%s (provisioner %s)", resourceClaimName, managedProvisioners[entryName]))
			continue
		}
		if claim.Status.Allocation == nil {
			unallocated = append(unallocated, claim.Name)
		}
	}
	ready := len(missing) == 0 && len(unallocated) == 0

	condition := v1.VirtualMachineInstanceCondition{
		Type:    v1.VirtualMachineInstanceManagedClaimsReady,
		Status:  k8sv1.ConditionTrue,
		Reason:  v1.VirtualMachineInstanceReasonAllManagedClaimsReady,
		Message: "All of the VMI's managed ResourceClaims are created and allocated",
	}
	if !ready {
		condition.Status = k8sv1.ConditionFalse
		condition.Reason = v1.VirtualMachineInstanceReasonNotAllManagedClaimsReady
		var details []string
		if len(missing) > 0 {
			details = append(details, fmt.Sprintf("not yet created: %s", strings.Join(missing, ", ")))
		}
		if len(unallocated) > 0 {
			details = append(details, fmt.Sprintf("not yet allocated: %s", strings.Join(unallocated, ", ")))
		}
		condition.Message = "Managed ResourceClaims " + strings.Join(details, "; ")
	}

	controller.NewVirtualMachineInstanceConditionManager().UpdateCondition(vmi, &condition)
}
