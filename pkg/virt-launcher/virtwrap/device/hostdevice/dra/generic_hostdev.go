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

	drautil "kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/dra/metadata"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device"
)

const (
	failedCreateGenericHostDevicesFmt = "failed to create dra generic host-devices: %v"
	DRAHostDeviceAliasPrefix          = "dra-hostdevice-"
)

// CreateDRAHostDevices creates host devices for HostDevices allocated via DRA.
func CreateDRAHostDevices(vmi *v1.VirtualMachineInstance, basePath string) ([]api.HostDevice, error) {
	var hostDevices []api.HostDevice
	if !hasHostDevicesWithDRA(vmi) {
		return hostDevices, nil
	}

	for _, hd := range vmi.Spec.Domain.Devices.HostDevices {
		if !drautil.IsHostDeviceDRA(hd) {
			continue
		}

		devs, err := createHostDevicesForHostDevice(hd, basePath, vmi.Spec)
		if err != nil {
			return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
		}
		hostDevices = append(hostDevices, devs...)
	}

	if err := validateCreationOfDRAHostDevices(vmi.Spec.Domain.Devices.HostDevices, hostDevices); err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	return hostDevices, nil
}

func createHostDevicesForHostDevice(hd v1.HostDevice, basePath string, vmiSpecs v1.VirtualMachineInstanceSpec) ([]api.HostDevice, error) {
	if hd.ClaimRequest == nil || hd.ClaimRequest.ClaimName == "" || hd.ClaimRequest.RequestName == "" {
		return nil, fmt.Errorf("HostDevice %s has incomplete ClaimRequest", hd.Name)
	}

	claimName := hd.ClaimRequest.ClaimName
	requestName := hd.ClaimRequest.RequestName
	resourceClaims := vmiSpecs.ResourceClaims

	devices, err := drautil.ResolveDevices(basePath, resourceClaims, claimName, requestName)
	if err != nil {
		return nil, fmt.Errorf("HostDevice %s: %w", hd.Name, err)
	}

	var hostDevices []api.HostDevice
	for i, dev := range devices {
		suffix := hd.Name
		if len(devices) > 1 {
			suffix = fmt.Sprintf("%s-%d", hd.Name, i)
		}

		if attr, ok := dev.Attributes[metadata.MDevUUIDAttribute]; ok && attr.StringValue != nil && *attr.StringValue != "" {
			model := "vfio-pci"
			if vmiSpecs.Architecture == "s390x" {
				model = "vfio-ap"
			}
			hostDevices = append(hostDevices, api.HostDevice{
				Alias: api.NewUserDefinedAlias(DRAHostDeviceAliasPrefix + suffix),
				Source: api.HostDeviceSource{
					Address: &api.Address{UUID: *attr.StringValue},
				},
				Type:  api.HostDeviceMDev,
				Mode:  "subsystem",
				Model: model,
			})
			continue
		}

		if attr, ok := dev.Attributes[metadata.PCIBusIDAttribute]; ok && attr.StringValue != nil && *attr.StringValue != "" {
			hostAddr, addrErr := device.NewPciAddressField(*attr.StringValue)
			if addrErr != nil {
				return nil, fmt.Errorf("failed to create PCI device for %s: %v", suffix, addrErr)
			}
			hostDevices = append(hostDevices, api.HostDevice{
				Alias:   api.NewUserDefinedAlias(DRAHostDeviceAliasPrefix + suffix),
				Source:  api.HostDeviceSource{Address: hostAddr},
				Type:    api.HostDevicePCI,
				Managed: "no",
			})
			continue
		}

		return nil, fmt.Errorf("HostDevice %s device %d has no mdevUUID or pciBusID in metadata", hd.Name, i)
	}

	return hostDevices, nil
}

func validateCreationOfDRAHostDevices(genericHostDevices []v1.HostDevice, hostDevices []api.HostDevice) error {
	var hostDevsWithDRA []v1.HostDevice
	for _, hd := range genericHostDevices {
		if drautil.IsHostDeviceDRA(hd) {
			hostDevsWithDRA = append(hostDevsWithDRA, hd)
		}
	}

	if len(hostDevsWithDRA) > 0 && len(hostDevices) < len(hostDevsWithDRA) {
		return fmt.Errorf("fewer devices created (%d) than DRA HostDevice entries (%d)", len(hostDevices), len(hostDevsWithDRA))
	}
	return nil
}

func hasHostDevicesWithDRA(vmi *v1.VirtualMachineInstance) bool {
	for _, hd := range vmi.Spec.Domain.Devices.HostDevices {
		if drautil.IsHostDeviceDRA(hd) {
			return true
		}
	}
	return false
}
