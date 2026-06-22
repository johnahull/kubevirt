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

package dra

import (
	"fmt"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	drautil "kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/dra/metadata"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device"
)

const (
	failedCreateGPUHostDeviceFmt = "failed to create DRA GPU host-devices: %v"
	AliasPrefix                  = "dra-gpu-"
	DefaultDisplayOn             = true
)

// CreateDRAGPUHostDevices creates host devices for GPUs allocated via DRA.
func CreateDRAGPUHostDevices(vmi *v1.VirtualMachineInstance, basePath string) ([]api.HostDevice, error) {
	var hostDevices []api.HostDevice
	if !hasGPUsWithDRA(vmi) {
		log.Log.V(3).Infof("No DRA GPU devices found for vmi %s/%s", vmi.GetNamespace(), vmi.GetName())
		return hostDevices, nil
	}

	for _, gpu := range vmi.Spec.Domain.Devices.GPUs {
		if !drautil.IsGPUDRA(gpu) {
			continue
		}

		devs, err := createHostDevicesForGPU(gpu, basePath, vmi.Spec.ResourceClaims)
		if err != nil {
			return nil, fmt.Errorf(failedCreateGPUHostDeviceFmt, err)
		}
		hostDevices = append(hostDevices, devs...)
	}

	if err := validateCreationOfDRAGPUDevices(vmi.Spec.Domain.Devices.GPUs, hostDevices); err != nil {
		return nil, fmt.Errorf(failedCreateGPUHostDeviceFmt, err)
	}

	// Set default display on first vGPU if not explicitly set
	if DefaultDisplayOn && !isVgpuDisplaySet(vmi.Spec.Domain.Devices.GPUs) {
		for i := range hostDevices {
			if hostDevices[i].Type == api.HostDeviceMDev {
				hostDevices[i].Display = "on"
				hostDevices[i].RamFB = "on"
				break
			}
		}
	}

	return hostDevices, nil
}

func createHostDevicesForGPU(gpu v1.GPU, basePath string, resourceClaims []v1.VirtualMachineInstanceResourceClaim) ([]api.HostDevice, error) {
	if gpu.ClaimRequest == nil || gpu.ClaimRequest.ClaimName == "" || gpu.ClaimRequest.RequestName == "" {
		return nil, fmt.Errorf("GPU %s has incomplete ClaimRequest", gpu.Name)
	}

	claimName := gpu.ClaimRequest.ClaimName
	requestName := gpu.ClaimRequest.RequestName

	devices, err := drautil.ResolveDevices(basePath, resourceClaims, claimName, requestName)
	if err != nil {
		return nil, fmt.Errorf("GPU %s: %w", gpu.Name, err)
	}

	var hostDevices []api.HostDevice
	for i, dev := range devices {
		suffix := gpu.Name
		if len(devices) > 1 {
			suffix = fmt.Sprintf("%s-%d", gpu.Name, i)
		}

		if attr, ok := dev.Attributes[metadata.MDevUUIDAttribute]; ok && attr.StringValue != nil && *attr.StringValue != "" {
			log.Log.V(2).Infof("Adding DRA MDEV GPU device for %s", suffix)
			hostDevice := api.HostDevice{
				Alias: api.NewUserDefinedAlias(AliasPrefix + suffix),
				Source: api.HostDeviceSource{
					Address: &api.Address{UUID: *attr.StringValue},
				},
				Type:  api.HostDeviceMDev,
				Mode:  "subsystem",
				Model: "vfio-pci",
			}
			if gpu.VirtualGPUOptions != nil && gpu.VirtualGPUOptions.Display != nil {
				displayEnabled := gpu.VirtualGPUOptions.Display.Enabled
				if displayEnabled == nil || *displayEnabled {
					hostDevice.Display = "on"
					if gpu.VirtualGPUOptions.Display.RamFB == nil || *gpu.VirtualGPUOptions.Display.RamFB.Enabled {
						hostDevice.RamFB = "on"
					}
				}
			}
			hostDevices = append(hostDevices, hostDevice)
			continue
		}

		if attr, ok := dev.Attributes[metadata.PCIBusIDAttribute]; ok && attr.StringValue != nil && *attr.StringValue != "" {
			log.Log.V(2).Infof("Adding DRA PCI GPU device for %s", suffix)
			hostAddr, addrErr := device.NewPciAddressField(*attr.StringValue)
			if addrErr != nil {
				return nil, fmt.Errorf("failed to create PCI device for %s: %v", suffix, addrErr)
			}
			hostDevices = append(hostDevices, api.HostDevice{
				Alias:   api.NewUserDefinedAlias(AliasPrefix + suffix),
				Source:  api.HostDeviceSource{Address: hostAddr},
				Type:    api.HostDevicePCI,
				Managed: "no",
			})
			continue
		}

		return nil, fmt.Errorf("GPU %s device %d has no mdevUUID or pciBusID in metadata", gpu.Name, i)
	}

	return hostDevices, nil
}

func hasGPUsWithDRA(vmi *v1.VirtualMachineInstance) bool {
	for _, gpu := range vmi.Spec.Domain.Devices.GPUs {
		if drautil.IsGPUDRA(gpu) {
			return true
		}
	}
	return false
}

func isVgpuDisplaySet(gpuSpecs []v1.GPU) bool {
	for _, gpu := range gpuSpecs {
		if gpu.VirtualGPUOptions != nil && gpu.VirtualGPUOptions.Display != nil {
			return true
		}
	}
	return false
}

func validateCreationOfDRAGPUDevices(gpus []v1.GPU, hostDevices []api.HostDevice) error {
	gpusWithDRA := []v1.GPU{}
	for _, gpu := range gpus {
		if drautil.IsGPUDRA(gpu) {
			gpusWithDRA = append(gpusWithDRA, gpu)
		}
	}
	if len(gpusWithDRA) > 0 && len(gpusWithDRA) != len(hostDevices) {
		return fmt.Errorf(
			"the number of DRA GPU/s do not match the number of devices:\nGPU: %v\nDevice: %v", gpusWithDRA, hostDevices,
		)
	}
	return nil
}
