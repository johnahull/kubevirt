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

	resourcev1 "k8s.io/api/resource/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	virtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

// handleDRAResourcesClaim ensures the synthesized CPU/memory ResourceClaim
// (VEP #152 + memory DRA) exists before the virt-launcher pod is created.
// The claim is owned by the VMI, so it is garbage-collected automatically;
// there is no delete path here.
//
// This must run, and must succeed, before RenderLaunchManifest: the pod
// references the claim by name (services.WithDRAResources / PodResourceClaim),
// so creating the pod before the claim exists would leave it referencing
// nothing.
func (c *Controller) handleDRAResourcesClaim(vmi *virtv1.VirtualMachineInstance) common.SyncError {
	usesCPUDRA := dra.UsesCPUDRA(c.clusterConfig, vmi)
	usesMemoryDRA := dra.UsesMemoryDRA(c.clusterConfig, vmi)
	if !usesCPUDRA && !usesMemoryDRA {
		return nil
	}
	if _, ok := dra.ManualClaimName(vmi); ok {
		if err := c.setUsesDRAResourcesAnnotation(vmi); err != nil {
			return common.NewSyncError(
				fmt.Errorf("failed to annotate vmi %s/%s as using DRA resources: %v", vmi.Namespace, vmi.Name, err),
				controller.FailedDRAResourcesClaimCreateReason,
			)
		}
		return nil
	}

	var hostCPUs int64
	if usesCPUDRA {
		additionalCPUs := services.SupplementalPoolIOThreadCPUs(vmi)
		hostCPUs = services.HostCPUs(vmi, vmi.Annotations, additionalCPUs)
	}

	var memorySize *resource.Quantity
	var hugepageDeviceClass string
	if usesMemoryDRA {
		size := services.HugepagesMemorySize(vmi)
		memorySize = &size
		class, err := dra.HugepageDeviceClass(vmi.Spec.Domain.Memory.Hugepages.PageSize)
		if err != nil {
			return common.NewSyncError(
				fmt.Errorf("failed to resolve DRA memory DeviceClass for vmi %s/%s: %v", vmi.Namespace, vmi.Name, err),
				controller.FailedDRAResourcesClaimCreateReason,
			)
		}
		hugepageDeviceClass = class
	}

	claim := dra.NewResourcesClaim(vmi, usesCPUDRA, hostCPUs, memorySize, hugepageDeviceClass)
	if syncErr := c.ensureResourcesClaim(vmi, claim); syncErr != nil {
		return syncErr
	}

	// virt-launcher has no cluster-config/feature-gate access, so it cannot
	// recompute UsesCPUDRA/UsesMemoryDRA itself. Record that a claim exists
	// as a VMI annotation, which virt-launcher already receives as part of
	// the VMI object handed to it; converter.go reads it to decide whether
	// to build DRA-derived guest NUMA topology (see UsesDRAResourcesAnnotation).
	if err := c.setUsesDRAResourcesAnnotation(vmi); err != nil {
		return common.NewSyncError(
			fmt.Errorf("failed to annotate vmi %s/%s as using DRA resources: %v", vmi.Namespace, vmi.Name, err),
			controller.FailedDRAResourcesClaimCreateReason,
		)
	}

	return nil
}

// ensureResourcesClaim creates claim if it does not already exist. If a claim
// with that name already exists, it is adopted only if it is owned by vmi's
// UID; otherwise it is treated as a real conflict.
func (c *Controller) ensureResourcesClaim(vmi *virtv1.VirtualMachineInstance, claim *resourcev1.ResourceClaim) common.SyncError {
	claimsClient := c.clientset.ResourceV1().ResourceClaims(vmi.Namespace)
	_, err := claimsClient.Create(context.Background(), claim, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !k8serrors.IsAlreadyExists(err) {
		return common.NewSyncError(
			fmt.Errorf("failed to create DRA ResourceClaim for vmi %s/%s: %v", vmi.Namespace, vmi.Name, err),
			controller.FailedDRAResourcesClaimCreateReason,
		)
	}

	// The claim name is derived only from the VMI name (dra.ResourcesClaimName),
	// so if a VMI was deleted and recreated with the same name/namespace
	// before the old claim's owner-ref GC caught up, Create would return
	// AlreadyExists for a claim that still belongs to the deleted VMI. Adopt
	// only if the existing claim is actually owned by this VMI's UID;
	// otherwise treat it as a real conflict rather than silently wiring the
	// new pod to a claim that GC can delete out from under it.
	existing, getErr := claimsClient.Get(context.Background(), claim.Name, metav1.GetOptions{})
	if getErr != nil {
		return common.NewSyncError(
			fmt.Errorf("failed to get existing DRA ResourceClaim for vmi %s/%s: %v", vmi.Namespace, vmi.Name, getErr),
			controller.FailedDRAResourcesClaimCreateReason,
		)
	}
	if !isOwnedBy(existing.OwnerReferences, vmi.UID) {
		return common.NewSyncError(
			fmt.Errorf("DRA ResourceClaim %s/%s already exists and is not owned by vmi %s (uid %s)",
				vmi.Namespace, claim.Name, vmi.Name, vmi.UID),
			controller.FailedDRAResourcesClaimCreateReason,
		)
	}

	return nil
}

// setUsesDRAResourcesAnnotation patches UsesDRAResourcesAnnotation onto vmi
// in the API, so virt-handler/virt-launcher observe it. No-op if already set.
func (c *Controller) setUsesDRAResourcesAnnotation(vmi *virtv1.VirtualMachineInstance) error {
	if _, ok := vmi.Annotations[virtv1.UsesDRAResourcesAnnotation]; ok {
		return nil
	}

	var patchSet *patch.PatchSet
	if vmi.Annotations == nil {
		patchSet = patch.New(patch.WithAdd("/metadata/annotations", map[string]string{virtv1.UsesDRAResourcesAnnotation: "true"}))
	} else {
		patchSet = patch.New(patch.WithAdd(
			fmt.Sprintf("/metadata/annotations/%s", patch.EscapeJSONPointer(virtv1.UsesDRAResourcesAnnotation)),
			"true",
		))
	}
	patchBytes, err := patchSet.GeneratePayload()
	if err != nil {
		return err
	}

	_, err = c.clientset.VirtualMachineInstance(vmi.Namespace).Patch(
		context.Background(), vmi.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{},
	)
	return err
}

func isOwnedBy(ownerRefs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range ownerRefs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}
