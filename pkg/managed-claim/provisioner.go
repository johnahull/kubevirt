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

// Package managedclaim provides the framework a provisioner controller uses to
// turn a VMI's device declarations into a generated ResourceClaim.
//
// KubeVirt ships one provisioner, policy.kubevirt.io/aligner, in the aligner
// subpackage. Third parties implement ClaimProvisioner and run their own
// controller; nothing here is registered in virt-controller.
package managedclaim

import (
	resourcev1 "k8s.io/api/resource/v1"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

// ClaimProvisioner turns one managed claim entry into a ResourceClaim spec.
type ClaimProvisioner interface {
	GenerateClaim(ctx *ManagedClaimContext) (*resourcev1.ResourceClaimSpec, error)
}

// ManagedClaimContext is everything a provisioner needs to generate one claim.
type ManagedClaimContext struct {
	VMI         *v1.VirtualMachineInstance
	Claim       *v1.VirtualMachineInstanceResourceClaim
	Provisioner *corev1alpha1.ManagedClaimProvisioner
	Devices     ManagedClaimDevices
}

// ManagedClaimDevices holds every VMI declaration that references the managed
// claim entry. It is deliberately not narrowed to a single request: a
// provisioner chooses its own request grouping and constraint layout without
// that grouping appearing in either the VMI or the ManagedClaimProvisioner API.
type ManagedClaimDevices struct {
	GPUs        []v1.GPU
	HostDevices []v1.HostDevice
	Networks    []ManagedClaimNetwork
	// CPU is always nil until VEP-152 adds CPUDRASource to the core API.
	CPU *CPUDRASource
}

// ManagedClaimNetwork pairs a DRA-backed network with its interface, since a
// provisioner may need both to decide how to shape the request.
type ManagedClaimNetwork struct {
	Network   v1.Network
	Interface *v1.Interface
}

// CPUDRASource is a placeholder for the type VEP-152 will add to the core API
// as v1.CPU.DRA. It exists so the cpu device type can be threaded through
// configuration and validation now; collection returns an error rather than
// populating it. See collectCPU.
type CPUDRASource struct {
	ClaimName   string
	RequestName string
}

// IsEmpty reports whether no device references the managed claim.
func (d ManagedClaimDevices) IsEmpty() bool {
	return len(d.GPUs) == 0 && len(d.HostDevices) == 0 && len(d.Networks) == 0 && d.CPU == nil
}
