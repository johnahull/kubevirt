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

package admitter

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

// ProvisionerGetter looks up a cluster-scoped ManagedClaimProvisioner by name.
//
// This is deliberately narrower than the generated lister so the validation
// logic stays independent of client-go plumbing and is trivially fakeable.
type ProvisionerGetter interface {
	Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error)
}

// ValidateProvisionerExists rejects VMIs referencing a ManagedClaimProvisioner
// that does not exist.
//
// This is a usability check rather than a correctness one: a typo would
// otherwise leave the launcher pod pending indefinitely with nothing to
// explain why, because no controller serves the misspelled provisioner. The
// provisioner controller independently reports genuine generation failures as
// events, and virt-controller surfaces overall claim readiness through the
// ManagedClaimsReady condition.
//
// Only the VMI create path is wired to this check. VirtualMachine and
// VirtualMachineInstanceReplicaSet validate their templates through a pure
// function with no cluster access, so a bad reference there surfaces when the
// VMI is created.
func ValidateProvisionerExists(
	field *k8sfield.Path,
	spec *v1.VirtualMachineInstanceSpec,
	getter ProvisionerGetter,
) []metav1.StatusCause {
	if getter == nil {
		return nil
	}

	var causes []metav1.StatusCause
	claimsField := field.Child("resourceClaims")

	for i, claim := range spec.ResourceClaims {
		if claim.ManagedClaimProvisionerName == nil {
			continue
		}

		name := *claim.ManagedClaimProvisionerName
		if _, err := getter.Get(name); err != nil {
			// Only a definitive NotFound is a user error. Any other failure is
			// an unhealthy cache, and rejecting on it would block all VMI
			// creation rather than surfacing a real problem.
			if !errors.IsNotFound(err) {
				continue
			}
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueNotFound,
				Message: fmt.Sprintf("ManagedClaimProvisioner %q does not exist", name),
				Field:   claimsField.Index(i).Child("managedClaimProvisionerName").String(),
			})
		}
	}

	return causes
}
