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
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

// deviceTypeOrder fixes the order requests are emitted in, so a given VMI
// always produces a byte-identical ResourceClaim. Without this the claim would
// churn on every reconcile as map iteration reordered the requests.
var deviceTypeOrder = []corev1alpha1.ManagedClaimDeviceTypeName{
	corev1alpha1.ManagedClaimDeviceTypeCPU,
	corev1alpha1.ManagedClaimDeviceTypeGPU,
	corev1alpha1.ManagedClaimDeviceTypeHostDevice,
	corev1alpha1.ManagedClaimDeviceTypeNetwork,
}

// requestName pairs a generated request with the device type it came from, so
// BuildConfigs can group requests without re-deriving the mapping.
type typedRequestNames map[corev1alpha1.ManagedClaimDeviceTypeName][]string

// BuildRequests implements steps 1 and 2 of the VEP's claim generation
// algorithm: collect one DeviceRequest per device, resolving each device's
// DeviceClass from the provisioner by which VMI field the device came from.
//
// Constraints are deliberately not built here. They are the provisioner's
// policy (step 4), which is what distinguishes the built-in topology aligner
// from a third-party provisioner.
func BuildRequests(
	devices ManagedClaimDevices,
	provisioner *corev1alpha1.ManagedClaimProvisioner,
) ([]resourcev1.DeviceRequest, error) {
	if devices.IsEmpty() {
		return nil, fmt.Errorf("no devices reference this claim")
	}

	deviceClasses := deviceClassesByType(provisioner)
	names := requestNamesByType(devices)

	var requests []resourcev1.DeviceRequest
	seen := sets.New[string]()

	for _, deviceType := range deviceTypeOrder {
		requestNames := names[deviceType]
		if len(requestNames) == 0 {
			continue
		}

		deviceClass, configured := deviceClasses[deviceType]
		if !configured || deviceClass == "" {
			return nil, fmt.Errorf(
				"no deviceClassName configured for device type %q in ManagedClaimProvisioner %q",
				deviceType, provisioner.Name)
		}

		for _, name := range requestNames {
			if seen.Has(name) {
				return nil, fmt.Errorf("duplicate request name %q", name)
			}
			seen.Insert(name)

			requests = append(requests, resourcev1.DeviceRequest{
				Name: name,
				Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: deviceClass,
					Count:           requestCount(deviceType),
					AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
				},
			})
		}
	}

	return requests, nil
}

// BuildConfigs implements step 3: for each configured device type carrying
// opaque driver configuration, emit a DeviceClaimConfiguration naming every
// request generated for that type.
func BuildConfigs(
	devices ManagedClaimDevices,
	provisioner *corev1alpha1.ManagedClaimProvisioner,
) []resourcev1.DeviceClaimConfiguration {
	names := requestNamesByType(devices)
	opaqueByType := map[corev1alpha1.ManagedClaimDeviceTypeName]*resourcev1.OpaqueDeviceConfiguration{}
	for _, deviceType := range provisioner.Spec.DeviceTypes {
		if deviceType.Opaque != nil {
			opaqueByType[deviceType.Name] = deviceType.Opaque
		}
	}

	var configs []resourcev1.DeviceClaimConfiguration
	for _, deviceType := range deviceTypeOrder {
		opaque, hasOpaque := opaqueByType[deviceType]
		requestNames := names[deviceType]
		if !hasOpaque || len(requestNames) == 0 {
			continue
		}

		configs = append(configs, resourcev1.DeviceClaimConfiguration{
			Requests: requestNames,
			DeviceConfiguration: resourcev1.DeviceConfiguration{
				Opaque: opaque,
			},
		})
	}

	return configs
}

// RequestNamesFor returns the generated request names for one device type,
// letting a provisioner build constraints over a subset of requests.
func RequestNamesFor(
	devices ManagedClaimDevices,
	deviceTypes ...corev1alpha1.ManagedClaimDeviceTypeName,
) []string {
	names := requestNamesByType(devices)

	var selected []string
	for _, deviceType := range deviceTypeOrder {
		for _, wanted := range deviceTypes {
			if deviceType == wanted {
				selected = append(selected, names[deviceType]...)
				break
			}
		}
	}
	return selected
}

func requestNamesByType(devices ManagedClaimDevices) typedRequestNames {
	names := typedRequestNames{}

	if devices.CPU != nil {
		names[corev1alpha1.ManagedClaimDeviceTypeCPU] = []string{devices.CPU.RequestName}
	}
	for _, gpu := range devices.GPUs {
		names[corev1alpha1.ManagedClaimDeviceTypeGPU] = append(
			names[corev1alpha1.ManagedClaimDeviceTypeGPU], gpu.ClaimRequest.RequestName)
	}
	for _, hostDevice := range devices.HostDevices {
		names[corev1alpha1.ManagedClaimDeviceTypeHostDevice] = append(
			names[corev1alpha1.ManagedClaimDeviceTypeHostDevice], hostDevice.ClaimRequest.RequestName)
	}
	for _, network := range devices.Networks {
		names[corev1alpha1.ManagedClaimDeviceTypeNetwork] = append(
			names[corev1alpha1.ManagedClaimDeviceTypeNetwork], network.Network.ResourceClaim.RequestName)
	}

	return names
}

func deviceClassesByType(
	provisioner *corev1alpha1.ManagedClaimProvisioner,
) map[corev1alpha1.ManagedClaimDeviceTypeName]string {
	deviceClasses := map[corev1alpha1.ManagedClaimDeviceTypeName]string{}
	for _, deviceType := range provisioner.Spec.DeviceTypes {
		deviceClasses[deviceType.Name] = deviceType.DeviceClassName
	}
	return deviceClasses
}

// requestCount is 1 for every passthrough device: KubeVirt supports exactly one
// device per request. CPUs are not an exception to the count: once VEP-152
// lands they use DRA consumable capacity (grouped mode), where count stays 1
// (one grouped device) and VEP-152's accounting formula feeds
// Capacity.Requests rather than the count.
func requestCount(_ corev1alpha1.ManagedClaimDeviceTypeName) int64 {
	return 1
}
