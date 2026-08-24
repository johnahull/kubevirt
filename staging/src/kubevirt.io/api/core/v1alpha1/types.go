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

package v1alpha1

import (
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedClaimDeviceTypeName identifies which VMI device declaration a
// ManagedClaimDeviceType configures.
type ManagedClaimDeviceTypeName string

const (
	// ManagedClaimDeviceTypeCPU maps to vmi.spec.domain.cpu.dra.
	ManagedClaimDeviceTypeCPU ManagedClaimDeviceTypeName = "cpu"
	// ManagedClaimDeviceTypeGPU maps to vmi.spec.domain.devices.gpus[].
	ManagedClaimDeviceTypeGPU ManagedClaimDeviceTypeName = "gpu"
	// ManagedClaimDeviceTypeHostDevice maps to vmi.spec.domain.devices.hostDevices[].
	ManagedClaimDeviceTypeHostDevice ManagedClaimDeviceTypeName = "hostDevice"
	// ManagedClaimDeviceTypeNetwork maps to vmi.spec.networks[].resourceClaim.
	ManagedClaimDeviceTypeNetwork ManagedClaimDeviceTypeName = "network"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
// +genclient
// +genclient:nonNamespaced

// ManagedClaimProvisioner encodes the DeviceClass mappings and the name of the
// provisioner controller used to generate a ResourceClaim for a VMI.
//
// Admins create ManagedClaimProvisioner objects; users reference one by name
// from vmi.spec.resourceClaims[].managedClaimProvisionerName and declare their
// devices as usual. The referenced provisioner controller receives every device
// declaration for the managed claim and assembles the ResourceClaim.
//
// This resource is in alpha and requires the ManagedDRAClaims feature gate.
type ManagedClaimProvisioner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec defines the provisioner controller and its DeviceClass mappings.
	Spec ManagedClaimProvisionerSpec `json:"spec"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ManagedClaimProvisionerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// +listType=atomic
	Items []ManagedClaimProvisioner `json:"items"`
}

type ManagedClaimProvisionerSpec struct {
	// Provisioner identifies the controller responsible for claim generation.
	// KubeVirt ships policy.kubevirt.io/aligner, which applies PCIe-root and
	// NUMA topology alignment. Third-party controllers use their own name.
	//
	// A managed claim is only reconciled once a controller serving this
	// provisioner name is running; until then the launcher pod stays pending.
	Provisioner string `json:"provisioner"`

	// DeviceTypes maps VMI device declarations to DeviceClass names and
	// optional driver-specific configuration.
	//
	// Each entry's name must be unique and one of cpu, gpu, hostDevice, or
	// network. Every device type referenced by a device in the managed claim
	// must have an entry here.
	// +listType=map
	// +listMapKey=name
	DeviceTypes []ManagedClaimDeviceType `json:"deviceTypes"`
}

type ManagedClaimDeviceType struct {
	// Name is the VMI device declaration this entry configures: one of
	// cpu, gpu, hostDevice, or network.
	Name ManagedClaimDeviceTypeName `json:"name"`

	// DeviceClassName is the DeviceClass used for every request generated for
	// this device type.
	DeviceClassName string `json:"deviceClassName"`

	// Opaque is driver-specific configuration. When set, the provisioner
	// renders a DeviceClaimConfiguration into the generated
	// ResourceClaim.spec.devices.config, with requests set to every generated
	// request for this device type.
	// +optional
	Opaque *resourcev1.OpaqueDeviceConfiguration `json:"opaque,omitempty"`
}
