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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

func deviceType(name corev1alpha1.ManagedClaimDeviceTypeName, deviceClass string) corev1alpha1.ManagedClaimDeviceType {
	return corev1alpha1.ManagedClaimDeviceType{Name: name, DeviceClassName: deviceClass}
}

func provisionerWith(deviceTypes ...corev1alpha1.ManagedClaimDeviceType) *corev1alpha1.ManagedClaimProvisioner {
	return &corev1alpha1.ManagedClaimProvisioner{
		ObjectMeta: metav1.ObjectMeta{Name: "pcie-aligned"},
		Spec: corev1alpha1.ManagedClaimProvisionerSpec{
			Provisioner: "policy.kubevirt.io/aligner",
			DeviceTypes: deviceTypes,
		},
	}
}

func exactRequest(name, deviceClass string, count int64) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{
		Name: name,
		Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: deviceClass,
			Count:           count,
			AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
		},
	}
}

var _ = Describe("BuildRequests", func() {
	It("should build one request per device, in cpu/gpu/hostDevice/network order", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")
		Expect(err).ToNot(HaveOccurred())

		provisioner := provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeHostDevice, "pci.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com"),
		)

		requests, err := BuildRequests(devices, provisioner)

		Expect(err).ToNot(HaveOccurred())
		Expect(requests).To(Equal([]resourcev1.DeviceRequest{
			exactRequest("gpu", "gpu.example.com", 1),
			exactRequest("hostdev", "pci.example.com", 1),
			exactRequest("nic", "sriov.example.com", 1),
		}))
	})

	It("should match the VEP single-GPU example", func() {
		vmi := vmiWithDevices()
		vmi.Spec.Domain.Devices.HostDevices = nil
		vmi.Spec.Networks = nil
		devices, err := CollectDevices(vmi, "aligned")
		Expect(err).ToNot(HaveOccurred())

		requests, err := BuildRequests(devices, provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")))

		Expect(err).ToNot(HaveOccurred())
		Expect(requests).To(Equal([]resourcev1.DeviceRequest{
			exactRequest("gpu", "gpu.example.com", 1),
		}))
	})

	It("should reject a device type with no DeviceClass mapping", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")
		Expect(err).ToNot(HaveOccurred())

		// gpu and hostDevice are mapped; network is not.
		_, err = BuildRequests(devices, provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeHostDevice, "pci.example.com"),
		))

		Expect(err).To(MatchError(ContainSubstring("no deviceClassName configured for device type \"network\"")))
	})

	It("should reject duplicate request names", func() {
		vmi := vmiWithDevices()
		vmi.Spec.Domain.Devices.GPUs = []v1.GPU{
			{Name: "gpu0", ClaimRequest: claimRequest("aligned", "gpu")},
			{Name: "gpu1", ClaimRequest: claimRequest("aligned", "gpu")},
		}
		devices, err := CollectDevices(vmi, "aligned")
		Expect(err).ToNot(HaveOccurred())

		_, err = BuildRequests(devices, provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")))

		Expect(err).To(MatchError(ContainSubstring("duplicate request name \"gpu\"")))
	})

	It("should reject a claim no device references", func() {
		_, err := BuildRequests(ManagedClaimDevices{}, provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")))

		Expect(err).To(MatchError(ContainSubstring("no devices reference this claim")))
	})
})

var _ = Describe("BuildConfigs", func() {
	opaque := func(driver string) *resourcev1.OpaqueDeviceConfiguration {
		return &resourcev1.OpaqueDeviceConfiguration{
			Driver:     driver,
			Parameters: runtime.RawExtension{Raw: []byte(`{"kind":"GPUConfig"}`)},
		}
	}

	It("should render one config per device type that has opaque parameters", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")
		Expect(err).ToNot(HaveOccurred())

		gpuType := deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")
		gpuType.Opaque = opaque("gpu.example.com")
		provisioner := provisionerWith(
			gpuType,
			deviceType(corev1alpha1.ManagedClaimDeviceTypeHostDevice, "pci.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com"),
		)

		configs := BuildConfigs(devices, provisioner)

		Expect(configs).To(Equal([]resourcev1.DeviceClaimConfiguration{{
			Requests: []string{"gpu"},
			DeviceConfiguration: resourcev1.DeviceConfiguration{
				Opaque: opaque("gpu.example.com"),
			},
		}}))
	})

	It("should list every request of that device type", func() {
		vmi := vmiWithDevices()
		vmi.Spec.Domain.Devices.GPUs = []v1.GPU{
			{Name: "gpu0", ClaimRequest: claimRequest("aligned", "gpu0")},
			{Name: "gpu1", ClaimRequest: claimRequest("aligned", "gpu1")},
		}
		devices, err := CollectDevices(vmi, "aligned")
		Expect(err).ToNot(HaveOccurred())

		gpuType := deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")
		gpuType.Opaque = opaque("gpu.example.com")

		configs := BuildConfigs(devices, provisionerWith(gpuType))

		Expect(configs).To(HaveLen(1))
		Expect(configs[0].Requests).To(Equal([]string{"gpu0", "gpu1"}))
	})

	It("should render nothing when no device type has opaque parameters", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")
		Expect(err).ToNot(HaveOccurred())

		Expect(BuildConfigs(devices, provisionerWith(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")))).To(BeEmpty())
	})

	It("should render one config per opaque device type in deviceTypeOrder", func() {
		devices, err := CollectDevices(vmiWithDevices(), "aligned")
		Expect(err).ToNot(HaveOccurred())

		gpuType := deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")
		gpuType.Opaque = opaque("gpu.example.com")
		netType := deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com")
		netType.Opaque = opaque("sriov.example.com")

		// Declared out of order on purpose: BuildConfigs must order the output by
		// deviceTypeOrder (GPU before Network), not by the provisioner's order,
		// so the generated claim is byte-stable across reconciles.
		configs := BuildConfigs(devices, provisionerWith(
			netType,
			deviceType(corev1alpha1.ManagedClaimDeviceTypeHostDevice, "pci.example.com"),
			gpuType,
		))

		Expect(configs).To(Equal([]resourcev1.DeviceClaimConfiguration{
			{
				Requests:            []string{"gpu"},
				DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: opaque("gpu.example.com")},
			},
			{
				Requests:            []string{"nic"},
				DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: opaque("sriov.example.com")},
			},
		}))
	})
})
