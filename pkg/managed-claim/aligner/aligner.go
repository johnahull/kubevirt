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

// Package aligner implements KubeVirt's built-in managed claim provisioner,
// policy.kubevirt.io/aligner.
//
// Its constraint policy is implementation behaviour, not API: nothing in
// ManagedClaimProvisioner lets a user or admin tune it. A cluster wanting
// different behaviour runs a different provisioner controller.
package aligner

import (
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	managedclaim "kubevirt.io/kubevirt/pkg/managed-claim"
)

const (
	// ProvisionerName is the spec.provisioner value this controller serves.
	ProvisionerName = "policy.kubevirt.io/aligner"

	// PCIeRootAttribute groups devices behind the same PCIe root complex,
	// the tightest useful locality for passthrough devices.
	PCIeRootAttribute = "resource.kubernetes.io/pcieRoot"

	// NUMANodeAttribute groups devices on the same NUMA node. Standardised by
	// KEP-6072.
	NUMANodeAttribute = "resource.kubernetes.io/numaNode"
)

// Provisioner aligns a claim's devices on shared host topology.
type Provisioner struct{}

var _ managedclaim.ClaimProvisioner = &Provisioner{}

// GenerateClaim builds the ResourceClaim spec for one managed claim entry.
//
// Requests and opaque configuration come from the shared framework; the
// constraints below are this provisioner's own policy.
func (p *Provisioner) GenerateClaim(ctx *managedclaim.ManagedClaimContext) (*resourcev1.ResourceClaimSpec, error) {
	requests, err := managedclaim.BuildRequests(ctx.Devices, ctx.Provisioner)
	if err != nil {
		return nil, err
	}

	return &resourcev1.ResourceClaimSpec{
		Devices: resourcev1.DeviceClaim{
			Requests:    requests,
			Config:      managedclaim.BuildConfigs(ctx.Devices, ctx.Provisioner),
			Constraints: buildConstraints(ctx.Devices),
		},
	}, nil
}

// buildConstraints implements step 4 of the VEP algorithm.
//
// Passthrough devices (GPUs, host devices, NICs) align on PCIe root, which is
// the boundary that actually determines peer-to-peer bandwidth.
//
// NUMA alignment is deferred until a memory DRA driver is available. Without
// co-located memory the constraint would misrepresent system guarantees.
func buildConstraints(devices managedclaim.ManagedClaimDevices) []resourcev1.DeviceConstraint {
	var constraints []resourcev1.DeviceConstraint

	passthrough := managedclaim.RequestNamesFor(devices,
		corev1alpha1.ManagedClaimDeviceTypeGPU,
		corev1alpha1.ManagedClaimDeviceTypeHostDevice,
		corev1alpha1.ManagedClaimDeviceTypeNetwork,
	)
	// A single device has nothing to align against, so constraining it would
	// only narrow the scheduler's choices for no benefit.
	if len(passthrough) > 1 {
		constraints = append(constraints, resourcev1.DeviceConstraint{
			MatchAttribute: ptr.To(resourcev1.FullyQualifiedName(PCIeRootAttribute)),
			Requests:       passthrough,
		})
	}

	return constraints
}
