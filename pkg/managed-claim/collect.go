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

package managedclaim

import (
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
)

// CollectDevices gathers every VMI device declaration that references the named
// managed claim entry.
//
// The VMI spec is the source of truth for what the claim contains: the user
// already said "I have a GPU and a NIC", so the provisioner reads that rather
// than asking the user to restate it as ResourceClaim requests.
func CollectDevices(vmi *v1.VirtualMachineInstance, claimName string) (ManagedClaimDevices, error) {
	devices := ManagedClaimDevices{}

	for _, gpu := range vmi.Spec.Domain.Devices.GPUs {
		if dra.IsGPUDRA(gpu) && gpu.ClaimRequest.ClaimName == claimName {
			devices.GPUs = append(devices.GPUs, gpu)
		}
	}

	for _, hostDevice := range vmi.Spec.Domain.Devices.HostDevices {
		if dra.IsHostDeviceDRA(hostDevice) && hostDevice.ClaimRequest.ClaimName == claimName {
			devices.HostDevices = append(devices.HostDevices, hostDevice)
		}
	}

	interfacesByName := indexInterfacesByName(vmi.Spec.Domain.Devices.Interfaces)
	for _, network := range vmi.Spec.Networks {
		if !vmispec.IsDRANetwork(network) || network.ResourceClaim.ClaimName != claimName {
			continue
		}
		managedNetwork := ManagedClaimNetwork{Network: network}
		if iface, found := interfacesByName[network.Name]; found {
			managedNetwork.Interface = iface
		}
		devices.Networks = append(devices.Networks, managedNetwork)
	}

	cpu, err := collectCPU(vmi, claimName)
	if err != nil {
		return ManagedClaimDevices{}, err
	}
	devices.CPU = cpu

	return devices, nil
}

// collectCPU is the single place that will change when VEP-152 lands.
//
// VEP-300 owns the managed-claim framework; VEP-152 owns the cpu.dra struct
// (CPUDRASource) and the CPU accounting formula
// (cores x sockets x threads + emulatorThreadCPUs + supplementalPoolThreadCount).
// Until v1.CPU carries a DRA field there is nothing on the VMI that can name a
// claim, so no VMI can reach this path. Once it does, read vmi.Spec.Domain.CPU.DRA
// here and derive the count using VEP-152's formula.
func collectCPU(_ *v1.VirtualMachineInstance, _ string) (*CPUDRASource, error) {
	return nil, nil
}

func indexInterfacesByName(interfaces []v1.Interface) map[string]*v1.Interface {
	indexed := make(map[string]*v1.Interface, len(interfaces))
	for i := range interfaces {
		indexed[interfaces[i].Name] = &interfaces[i]
	}
	return indexed
}
