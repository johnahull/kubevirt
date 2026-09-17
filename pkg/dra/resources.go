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

// Package dra additions for VEP #152 (CPU DRA) and memory DRA support.
//
// This file is intentionally kept separate from utils.go, which is owned by
// VEP #115 (PCIe/NUMA topology) work. Keeping CPU/memory-DRA synthesis in its
// own file avoids merge conflicts between the feature branches.
package dra

import (
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/dra/metadata"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	// CPUDeviceClassName is the DeviceClass a CPU DRA driver (e.g. dra-driver-cpu)
	// is expected to publish grouped-mode CPU capacity under.
	CPUDeviceClassName = "dra.cpu"

	// CPUCapacityName is the qualified capacity name dra-driver-cpu publishes
	// grouped/consumable CPU capacity under.
	CPUCapacityName = resourcev1.QualifiedName("dra.cpu/cpu")

	// CPURequestName is the name of the CPU DeviceRequest in the synthesized
	// claim.
	CPURequestName = "cpu"

	// MemoryRequestName is the name of the memory/hugepages DeviceRequest in
	// the synthesized claim.
	MemoryRequestName = "mem"

	// MemoryCapacityName is the qualified capacity name dra-driver-memory
	// publishes consumable memory/hugepages capacity under. Unlike CPU's,
	// this key carries no "dra.memory/" prefix (verified against
	// dra-driver-memory's pkg/types/types.go ResourceIdent.CapacityName).
	MemoryCapacityName = resourcev1.QualifiedName("size")

	// PodClaimName is the shared name used for the synthesized claim in
	// pod.spec.resourceClaims[] and containers[].resources.claims[]. Both
	// the cpu and mem requests are referenced under this one local name with
	// different Request values.
	PodClaimName = "vmi-dra"

	// resourcesClaimNameSuffix is appended to the VMI name to derive the
	// synthesized ResourceClaim's name.
	resourcesClaimNameSuffix = "-dra"
)

// UsesCPUDRA returns true when virt-controller should synthesize a CPU
// DeviceRequest for this VMI instead of relying on kubelet CPU Manager.
func UsesCPUDRA(config *virtconfig.ClusterConfig, vmi *v1.VirtualMachineInstance) bool {
	return config.CPUsWithDRAEnabled() && vmi.IsCPUDedicated()
}

// UsesMemoryDRA returns true when virt-controller should synthesize a
// memory/hugepages DeviceRequest for this VMI instead of relying on
// kubelet's hugepages allocation. Reuses the existing
// vmi.Spec.Domain.Memory.Hugepages field as the trigger, matching VEP #152's
// "no new VMI API" principle.
func UsesMemoryDRA(config *virtconfig.ClusterConfig, vmi *v1.VirtualMachineInstance) bool {
	return config.MemoryWithDRAEnabled() &&
		vmi.Spec.Domain.Memory != nil &&
		vmi.Spec.Domain.Memory.Hugepages != nil
}

// ResourcesClaimName returns the name of the synthesized CPU/memory
// ResourceClaim for a VMI.
func ResourcesClaimName(vmi *v1.VirtualMachineInstance) string {
	return vmi.Name + resourcesClaimNameSuffix
}

// HugepageDeviceClass maps a VMI's hugepage page size (as used in
// vmi.Spec.Domain.Memory.Hugepages.PageSize, e.g. "1Gi"/"2Mi") to the
// DeviceClass name dra-driver-memory publishes for it. Verified against the
// driver's install manifests (hack/ci/install.tmpl.yaml in dra-driver-memory):
// the DeviceClass names are lowercase abbreviated forms ("1g"/"2m"), not the
// "1Gi"/"2Mi" quantity-suffix form used in the CEL selector or in KubeVirt's
// own API.
func HugepageDeviceClass(pageSize string) (string, error) {
	switch pageSize {
	case "1Gi":
		return "dra.hugepages-1g", nil
	case "2Mi":
		return "dra.hugepages-2m", nil
	default:
		return "", fmt.Errorf("no known DRA memory DeviceClass for hugepage size %q", pageSize)
	}
}

// NewResourcesClaim builds the KubeVirt-owned ResourceClaim for a VMI's
// exclusive CPUs and/or DRA-backed hugepages.
//
// hostCPUs sizes the cpu request and is always included. memorySize and
// hugepageDeviceClass are optional (nil / "" to omit the mem request); when
// both cpu and mem requests are present, a DeviceConstraint ties them to the
// same host NUMA node via the standard numaNode attribute both dra-driver-cpu
// and dra-driver-memory publish, so DRA's scheduler guarantees alignment
// instead of leaving it to chance.
func NewResourcesClaim(
	vmi *v1.VirtualMachineInstance,
	hostCPUs int64,
	memorySize *resource.Quantity,
	hugepageDeviceClass string,
) *resourcev1.ResourceClaim {
	requests := []resourcev1.DeviceRequest{
		{
			Name: CPURequestName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: CPUDeviceClassName,
				Capacity: &resourcev1.CapacityRequirements{
					Requests: map[resourcev1.QualifiedName]resource.Quantity{
						CPUCapacityName: *resource.NewQuantity(hostCPUs, resource.DecimalSI),
					},
				},
			},
		},
	}

	var constraints []resourcev1.DeviceConstraint
	if memorySize != nil && hugepageDeviceClass != "" {
		requests = append(requests, resourcev1.DeviceRequest{
			Name: MemoryRequestName,
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: hugepageDeviceClass,
				Capacity: &resourcev1.CapacityRequirements{
					Requests: map[resourcev1.QualifiedName]resource.Quantity{
						MemoryCapacityName: *memorySize,
					},
				},
			},
		})
		constraints = append(constraints, resourcev1.DeviceConstraint{
			MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(metadata.NUMANodeAttribute)),
			Requests:       []string{CPURequestName, MemoryRequestName},
		})
	}

	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResourcesClaimName(vmi),
			Namespace: vmi.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmi, v1.VirtualMachineInstanceGroupVersionKind),
			},
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests:    requests,
				Constraints: constraints,
			},
		},
	}
}
