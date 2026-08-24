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

package admitters

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

var _ = Describe("Validating ManagedClaimProvisioner Admitter", func() {
	provisioner := func(deviceTypes ...corev1alpha1.ManagedClaimDeviceType) *corev1alpha1.ManagedClaimProvisioner {
		return &corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "pcie-aligned"},
			Spec: corev1alpha1.ManagedClaimProvisionerSpec{
				Provisioner: "policy.kubevirt.io/aligner",
				DeviceTypes: deviceTypes,
			},
		}
	}

	deviceType := func(name corev1alpha1.ManagedClaimDeviceTypeName, deviceClass string) corev1alpha1.ManagedClaimDeviceType {
		return corev1alpha1.ManagedClaimDeviceType{Name: name, DeviceClassName: deviceClass}
	}

	It("should accept the VEP's pcie-aligned example", func() {
		p := provisioner(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com"),
		)

		Expect(validateManagedClaimProvisioner(p)).To(BeEmpty())
	})

	It("should accept all four device types", func() {
		p := provisioner(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeCPU, "cpu.dra.k8s.io"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeHostDevice, "pci.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeNetwork, "sriov.example.com"),
		)

		Expect(validateManagedClaimProvisioner(p)).To(BeEmpty())
	})

	It("should reject an empty provisioner name", func() {
		p := provisioner(deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"))
		p.Spec.Provisioner = ""

		Expect(validateManagedClaimProvisioner(p)).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: "spec.provisioner is a required field",
			Field:   "spec.provisioner",
		}))
	})

	It("should reject an unknown device type name", func() {
		p := provisioner(deviceType("accelerator", "acc.example.com"))

		Expect(validateManagedClaimProvisioner(p)).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueNotSupported,
			Message: "spec.deviceTypes[0].name must be one of: cpu, gpu, hostDevice, network",
			Field:   "spec.deviceTypes[0].name",
		}))
	})

	It("should reject a duplicate device type name", func() {
		p := provisioner(
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com"),
			deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "other.example.com"),
		)

		Expect(validateManagedClaimProvisioner(p)).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueDuplicate,
			Message: "duplicate deviceTypes name \"gpu\"",
			Field:   "spec.deviceTypes[1].name",
		}))
	})

	It("should reject an empty deviceClassName", func() {
		p := provisioner(deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, ""))

		Expect(validateManagedClaimProvisioner(p)).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: "spec.deviceTypes[0].deviceClassName is a required field",
			Field:   "spec.deviceTypes[0].deviceClassName",
		}))
	})

	It("should reject opaque configuration without a driver", func() {
		dt := deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")
		dt.Opaque = &resourcev1.OpaqueDeviceConfiguration{}

		Expect(validateManagedClaimProvisioner(provisioner(dt))).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: "spec.deviceTypes[0].opaque.driver is a required field",
			Field:   "spec.deviceTypes[0].opaque.driver",
		}))
	})

	It("should accept opaque configuration with a driver", func() {
		dt := deviceType(corev1alpha1.ManagedClaimDeviceTypeGPU, "gpu.example.com")
		dt.Opaque = &resourcev1.OpaqueDeviceConfiguration{Driver: "gpu.example.com"}

		Expect(validateManagedClaimProvisioner(provisioner(dt))).To(BeEmpty())
	})

	It("should reject a provisioner with no device types", func() {
		Expect(validateManagedClaimProvisioner(provisioner())).To(ContainElement(metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: "spec.deviceTypes must contain at least one entry",
			Field:   "spec.deviceTypes",
		}))
	})
})
