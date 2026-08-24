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
	"fmt"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"kubevirt.io/api/core"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	webhookutils "kubevirt.io/kubevirt/pkg/util/webhooks"
	validating_webhooks "kubevirt.io/kubevirt/pkg/util/webhooks/validating-webhooks"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

// validManagedClaimDeviceTypes are the VMI device declarations a
// ManagedClaimProvisioner can configure, in the order used for error messages.
var validManagedClaimDeviceTypes = []corev1alpha1.ManagedClaimDeviceTypeName{
	corev1alpha1.ManagedClaimDeviceTypeCPU,
	corev1alpha1.ManagedClaimDeviceTypeGPU,
	corev1alpha1.ManagedClaimDeviceTypeHostDevice,
	corev1alpha1.ManagedClaimDeviceTypeNetwork,
}

type ManagedClaimProvisionerAdmitter struct {
	Config *virtconfig.ClusterConfig
}

func NewManagedClaimProvisionerAdmitter(config *virtconfig.ClusterConfig) *ManagedClaimProvisionerAdmitter {
	return &ManagedClaimProvisionerAdmitter{Config: config}
}

func (admitter *ManagedClaimProvisionerAdmitter) Admit(_ context.Context, ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	if ar.Request.Resource.Group != core.GroupName {
		return webhookutils.ToAdmissionResponseError(
			fmt.Errorf("unexpected group: %s, expected: %s", ar.Request.Resource.Group, core.GroupName))
	}
	if ar.Request.Resource.Resource != corev1alpha1.ResourceManagedClaimProvisioners {
		return webhookutils.ToAdmissionResponseError(
			fmt.Errorf("unexpected resource: %s, expected: %s",
				ar.Request.Resource.Resource, corev1alpha1.ResourceManagedClaimProvisioners))
	}

	if ar.Request.Operation == admissionv1.Create && !admitter.Config.ManagedDRAClaimsEnabled() {
		return webhookutils.ToAdmissionResponseError(fmt.Errorf("ManagedDRAClaims feature gate is not enabled"))
	}

	provisioner := &corev1alpha1.ManagedClaimProvisioner{}
	if err := json.Unmarshal(ar.Request.Object.Raw, provisioner); err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	causes := validateManagedClaimProvisioner(provisioner)
	resp := validating_webhooks.NewPassingAdmissionResponse()
	if len(causes) > 0 {
		resp.Allowed = false
		resp.Result = &metav1.Status{
			Message: causes[0].Message,
			Reason:  metav1.StatusReasonInvalid,
			Details: &metav1.StatusDetails{
				Causes: causes,
			},
		}
	}
	return resp
}

// validateManagedClaimProvisioner enforces the DeviceClass mapping rules that
// the provisioner controller relies on when generating a ResourceClaim. A
// mapping that is missing or ambiguous here surfaces much later as a failed
// claim generation on an already-created VMI, so it is worth rejecting at the
// source.
func validateManagedClaimProvisioner(provisioner *corev1alpha1.ManagedClaimProvisioner) []metav1.StatusCause {
	var causes []metav1.StatusCause
	specPath := field.NewPath("spec")

	if provisioner.Spec.Provisioner == "" {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: fmt.Sprintf("%s is a required field", specPath.Child("provisioner")),
			Field:   specPath.Child("provisioner").String(),
		})
	}

	if len(provisioner.Spec.DeviceTypes) == 0 {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: fmt.Sprintf("%s must contain at least one entry", specPath.Child("deviceTypes")),
			Field:   specPath.Child("deviceTypes").String(),
		})
	}

	seen := sets.New[corev1alpha1.ManagedClaimDeviceTypeName]()
	for i, deviceType := range provisioner.Spec.DeviceTypes {
		typePath := specPath.Child("deviceTypes").Index(i)
		causes = append(causes, validateManagedClaimDeviceType(deviceType, typePath, seen)...)
		seen.Insert(deviceType.Name)
	}

	return causes
}

func validateManagedClaimDeviceType(
	deviceType corev1alpha1.ManagedClaimDeviceType,
	typePath *field.Path,
	seen sets.Set[corev1alpha1.ManagedClaimDeviceTypeName],
) []metav1.StatusCause {
	var causes []metav1.StatusCause

	switch {
	case seen.Has(deviceType.Name):
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueDuplicate,
			Message: fmt.Sprintf("duplicate deviceTypes name %q", deviceType.Name),
			Field:   typePath.Child("name").String(),
		})
	case !isValidManagedClaimDeviceType(deviceType.Name):
		causes = append(causes, metav1.StatusCause{
			Type: metav1.CauseTypeFieldValueNotSupported,
			Message: fmt.Sprintf("%s must be one of: %s",
				typePath.Child("name"), joinManagedClaimDeviceTypes()),
			Field: typePath.Child("name").String(),
		})
	}

	if deviceType.DeviceClassName == "" {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: fmt.Sprintf("%s is a required field", typePath.Child("deviceClassName")),
			Field:   typePath.Child("deviceClassName").String(),
		})
	}

	if deviceType.Opaque != nil && deviceType.Opaque.Driver == "" {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueRequired,
			Message: fmt.Sprintf("%s is a required field", typePath.Child("opaque", "driver")),
			Field:   typePath.Child("opaque", "driver").String(),
		})
	}

	return causes
}

func isValidManagedClaimDeviceType(name corev1alpha1.ManagedClaimDeviceTypeName) bool {
	for _, valid := range validManagedClaimDeviceTypes {
		if name == valid {
			return true
		}
	}
	return false
}

func joinManagedClaimDeviceTypes() string {
	names := make([]string, len(validManagedClaimDeviceTypes))
	for i, name := range validManagedClaimDeviceTypes {
		names[i] = string(name)
	}
	return strings.Join(names, ", ")
}
