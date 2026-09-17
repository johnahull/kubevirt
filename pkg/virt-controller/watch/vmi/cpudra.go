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
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

// handleCPUDRAClaim ensures the synthesized CPU ResourceClaim (VEP #152)
// exists before the virt-launcher pod is created. The claim is owned by the
// VMI, so it is garbage-collected automatically; there is no delete path
// here.
//
// This must run, and must succeed, before RenderLaunchManifest: the pod
// references the claim by name (services.WithCPUDRA / PodResourceClaim), so
// creating the pod before the claim exists would leave it referencing
// nothing.
func (c *Controller) handleCPUDRAClaim(vmi *virtv1.VirtualMachineInstance) common.SyncError {
	if !dra.UsesCPUDRA(c.clusterConfig, vmi) {
		return nil
	}

	additionalCPUs := services.SupplementalPoolIOThreadCPUs(vmi)
	hostCPUs := services.HostCPUs(vmi, vmi.Annotations, additionalCPUs)

	claim := dra.NewCPUResourceClaim(vmi, hostCPUs)
	claimsClient := c.clientset.ResourceV1().ResourceClaims(vmi.Namespace)
	_, err := claimsClient.Create(context.Background(), claim, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !k8serrors.IsAlreadyExists(err) {
		return common.NewSyncError(
			fmt.Errorf("failed to create CPU DRA ResourceClaim for vmi %s/%s: %v", vmi.Namespace, vmi.Name, err),
			controller.FailedCPUDRAClaimCreateReason,
		)
	}

	// The claim name is derived only from the VMI name (dra.CPUClaimName), so
	// if a VMI was deleted and recreated with the same name/namespace before
	// the old claim's owner-ref GC caught up, Create would return
	// AlreadyExists for a claim that still belongs to the deleted VMI. Adopt
	// only if the existing claim is actually owned by this VMI's UID;
	// otherwise treat it as a real conflict rather than silently wiring the
	// new pod to a claim that GC can delete out from under it.
	existing, getErr := claimsClient.Get(context.Background(), claim.Name, metav1.GetOptions{})
	if getErr != nil {
		return common.NewSyncError(
			fmt.Errorf("failed to get existing CPU DRA ResourceClaim for vmi %s/%s: %v", vmi.Namespace, vmi.Name, getErr),
			controller.FailedCPUDRAClaimCreateReason,
		)
	}
	if !isOwnedBy(existing.OwnerReferences, vmi.UID) {
		return common.NewSyncError(
			fmt.Errorf("CPU DRA ResourceClaim %s/%s already exists and is not owned by vmi %s (uid %s)",
				vmi.Namespace, claim.Name, vmi.Name, vmi.UID),
			controller.FailedCPUDRAClaimCreateReason,
		)
	}

	return nil
}

func isOwnedBy(ownerRefs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range ownerRefs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}
