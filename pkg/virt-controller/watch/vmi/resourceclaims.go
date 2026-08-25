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

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
)

// addResourceClaim handles the addition of a ResourceClaim, enqueuing the VMI
// that owns it.
func (c *Controller) addResourceClaim(obj interface{}) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}
	c.enqueueVMIForResourceClaim(claim)
}

// updateResourceClaim handles updates to a ResourceClaim, enqueuing the VMI
// that owns it. Provisioner controllers write allocation results here, so the
// VMI's ManagedClaimsReady condition needs to be re-evaluated.
func (c *Controller) updateResourceClaim(old, cur interface{}) {
	curClaim, ok := cur.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}
	oldClaim, ok := old.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}
	if curClaim.ResourceVersion == oldClaim.ResourceVersion {
		// Periodic resync will send update events for all known ResourceClaims.
		// Two different versions of the same claim always have different RVs.
		return
	}
	c.enqueueVMIForResourceClaim(curClaim)
}

// deleteResourceClaim handles the deletion of a ResourceClaim, enqueuing the VMI
// that owns it.
func (c *Controller) deleteResourceClaim(obj interface{}) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	// When a delete is dropped, the relist will notice a claim in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			log.Log.Reason(fmt.Errorf(tombstoneGetObjectErrFmt, obj)).Error(deleteNotifFailed)
			return
		}
		claim, ok = tombstone.Obj.(*resourcev1.ResourceClaim)
		if !ok {
			log.Log.Reason(fmt.Errorf("tombstone contained object that is not a ResourceClaim %#v", obj)).Error(deleteNotifFailed)
			return
		}
	}
	c.enqueueVMIForResourceClaim(claim)
}

// enqueueVMIForResourceClaim enqueues the VMI that controller-owns the given
// ResourceClaim, if any. Claims not owned by a VMI are ignored.
func (c *Controller) enqueueVMIForResourceClaim(claim *resourcev1.ResourceClaim) {
	vmi := c.resolveVMIOwner(claim)
	if vmi == nil {
		return
	}
	log.Log.V(4).Object(claim).Infof("ResourceClaim event for vmi %s", vmi.Name)
	c.enqueueVirtualMachine(vmi)
}

// listManagedResourceClaimsForVMI returns the ResourceClaims in the VMI's
// namespace that are controller-owned by the VMI. The provisioner controller
// only ever creates managed claims owned by their VMI, so ownership is the
// filter; aggregateManagedClaimsConditions matches them to spec entries by name.
func (c *Controller) listManagedResourceClaimsForVMI(vmi *virtv1.VirtualMachineInstance) []*resourcev1.ResourceClaim {
	objs, err := c.resourceClaimIndexer.ByIndex(cache.NamespaceIndex, vmi.Namespace)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("Failed to list ResourceClaims for vmi")
		return nil
	}

	var claims []*resourcev1.ResourceClaim
	for _, obj := range objs {
		claim, ok := obj.(*resourcev1.ResourceClaim)
		if !ok {
			continue
		}
		if ref := metav1.GetControllerOf(claim); ref == nil || ref.UID != vmi.UID {
			continue
		}
		claims = append(claims, claim)
	}
	return claims
}

// resolveVMIOwner returns the VMI that controller-owns the ResourceClaim, or nil
// when the claim has no VMI controller owner or the VMI is not in the cache.
func (c *Controller) resolveVMIOwner(claim *resourcev1.ResourceClaim) *virtv1.VirtualMachineInstance {
	ref := metav1.GetControllerOf(claim)
	if ref == nil || ref.Kind != virtv1.VirtualMachineInstanceGroupVersionKind.Kind {
		return nil
	}
	obj, exists, err := c.vmiIndexer.GetByKey(controller.NamespacedKey(claim.Namespace, ref.Name))
	if err != nil || !exists {
		return nil
	}
	vmi, ok := obj.(*virtv1.VirtualMachineInstance)
	if !ok || vmi.UID != ref.UID {
		return nil
	}
	return vmi
}
