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

// Package dra additions for VEP #152: Support CPUs with DRA.
//
// This file is intentionally kept separate from utils.go, which is owned by
// VEP #115 (PCIe/NUMA topology) work. Keeping CPU-DRA synthesis in its own
// file avoids merge conflicts between the two feature branches.
package dra

import (
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	// CPUDeviceClassName is the DeviceClass a CPU DRA driver (e.g. dra-driver-cpu)
	// is expected to publish grouped-mode CPU capacity under.
	CPUDeviceClassName = "dra.cpu"

	// CPUCapacityName is the qualified capacity name dra-driver-cpu publishes
	// grouped/consumable CPU capacity under.
	CPUCapacityName = resourcev1.QualifiedName("dra.cpu/cpu")

	// CPUPodClaimName is the name used for this claim in
	// pod.spec.resourceClaims[] and containers[].resources.claims[].
	CPUPodClaimName = "cpu-dra"

	// CPURequestName is the name of the single DeviceRequest in the
	// synthesized claim.
	CPURequestName = "cpu"

	// cpuClaimNameSuffix is appended to the VMI name to derive the
	// synthesized ResourceClaim's name.
	cpuClaimNameSuffix = "-cpu"
)

// UsesCPUDRA returns true when virt-controller should synthesize a CPU
// ResourceClaim for this VMI instead of relying on kubelet CPU Manager.
func UsesCPUDRA(config *virtconfig.ClusterConfig, vmi *v1.VirtualMachineInstance) bool {
	return config.CPUsWithDRAEnabled() && vmi.IsCPUDedicated()
}

// CPUClaimName returns the name of the synthesized CPU ResourceClaim for a VMI.
func CPUClaimName(vmi *v1.VirtualMachineInstance) string {
	return vmi.Name + cpuClaimNameSuffix
}

// NewCPUResourceClaim builds the KubeVirt-owned ResourceClaim for hostCPUs
// exclusive CPUs, using the grouped-mode / consumable-capacity request shape
// from VEP #152.
func NewCPUResourceClaim(vmi *v1.VirtualMachineInstance, hostCPUs int64) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CPUClaimName(vmi),
			Namespace: vmi.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmi, v1.VirtualMachineInstanceGroupVersionKind),
			},
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
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
				},
			},
		},
	}
}
