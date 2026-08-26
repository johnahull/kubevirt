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
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionv1 "k8s.io/api/admission/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"kubevirt.io/api/core"
	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
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

var _ = Describe("Admitting ManagedClaimProvisioner", func() {
	validProvisioner := func() *corev1alpha1.ManagedClaimProvisioner {
		return &corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "pcie-aligned"},
			Spec: corev1alpha1.ManagedClaimProvisionerSpec{
				Provisioner: "policy.kubevirt.io/aligner",
				DeviceTypes: []corev1alpha1.ManagedClaimDeviceType{{
					Name:            corev1alpha1.ManagedClaimDeviceTypeGPU,
					DeviceClassName: "gpu.example.com",
				}},
			},
		}
	}

	kv := &v1.KubeVirt{
		ObjectMeta: metav1.ObjectMeta{Name: "kubevirt", Namespace: "kubevirt"},
		Spec: v1.KubeVirtSpec{
			Configuration: v1.KubeVirtConfiguration{
				DeveloperConfiguration: &v1.DeveloperConfiguration{},
			},
		},
		Status: v1.KubeVirtStatus{
			Phase:               v1.KubeVirtPhaseDeploying,
			DefaultArchitecture: "amd64",
		},
	}
	config, _, kvStore := testutils.NewFakeClusterConfigUsingKV(kv)

	setFeatureGates := func(gates ...string) {
		kvConfig := kv.DeepCopy()
		kvConfig.Spec.Configuration.DeveloperConfiguration.FeatureGates = gates
		testutils.UpdateFakeKubeVirtClusterConfig(kvStore, kvConfig)
	}

	provisionerGVR := metav1.GroupVersionResource{
		Group:    core.GroupName,
		Resource: corev1alpha1.ResourceManagedClaimProvisioners,
	}

	marshal := func(p *corev1alpha1.ManagedClaimProvisioner) []byte {
		raw, err := json.Marshal(p)
		Expect(err).ToNot(HaveOccurred())
		return raw
	}

	admissionReview := func(op admissionv1.Operation, gvr metav1.GroupVersionResource, raw []byte) *admissionv1.AdmissionReview {
		return &admissionv1.AdmissionReview{
			Request: &admissionv1.AdmissionRequest{
				Operation: op,
				Resource:  gvr,
				Object:    runtime.RawExtension{Raw: raw},
			},
		}
	}

	var admitter *ManagedClaimProvisionerAdmitter

	BeforeEach(func() {
		admitter = NewManagedClaimProvisionerAdmitter(config)
		setFeatureGates()
	})

	AfterEach(func() {
		setFeatureGates()
	})

	It("should reject an unexpected group", func() {
		setFeatureGates(featuregate.ManagedDRAClaimsGate)
		gvr := provisionerGVR
		gvr.Group = "wrong.example.com"

		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, gvr, marshal(validProvisioner())))

		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Message).To(ContainSubstring("unexpected group"))
	})

	It("should reject an unexpected resource", func() {
		setFeatureGates(featuregate.ManagedDRAClaimsGate)
		gvr := provisionerGVR
		gvr.Resource = "wrongresources"

		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, gvr, marshal(validProvisioner())))

		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Message).To(ContainSubstring("unexpected resource"))
	})

	It("should reject creation when the ManagedDRAClaims feature gate is disabled", func() {
		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, provisionerGVR, marshal(validProvisioner())))

		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Message).To(Equal("ManagedDRAClaims feature gate is not enabled"))
	})

	It("should allow updates even when the ManagedDRAClaims feature gate is disabled", func() {
		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Update, provisionerGVR, marshal(validProvisioner())))

		Expect(resp.Allowed).To(BeTrue())
	})

	It("should reject a malformed object body", func() {
		setFeatureGates(featuregate.ManagedDRAClaimsGate)

		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, provisionerGVR, []byte("{not json")))

		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result).ToNot(BeNil())
	})

	It("should allow a valid provisioner when the feature gate is enabled", func() {
		setFeatureGates(featuregate.ManagedDRAClaimsGate)

		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, provisionerGVR, marshal(validProvisioner())))

		Expect(resp.Allowed).To(BeTrue())
	})

	It("should reject an invalid provisioner and surface its validation causes", func() {
		setFeatureGates(featuregate.ManagedDRAClaimsGate)
		invalid := validProvisioner()
		invalid.Spec.Provisioner = ""

		resp := admitter.Admit(context.Background(),
			admissionReview(admissionv1.Create, provisionerGVR, marshal(invalid)))

		Expect(resp.Allowed).To(BeFalse())
		Expect(resp.Result.Details.Causes).ToNot(BeEmpty())
		Expect(resp.Result.Message).To(Equal(resp.Result.Details.Causes[0].Message))
	})
})
