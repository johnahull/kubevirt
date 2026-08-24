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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/pointer"
)

type fakeProvisionerGetter struct {
	known map[string]*corev1alpha1.ManagedClaimProvisioner
	err   error
}

func (f *fakeProvisionerGetter) Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error) {
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.known[name]; ok {
		return p, nil
	}
	return nil, errors.NewNotFound(corev1alpha1.Resource(corev1alpha1.ResourceManagedClaimProvisioners), name)
}

var _ = Describe("ManagedClaimProvisioner existence", func() {
	var (
		field  *k8sfield.Path
		getter *fakeProvisionerGetter
	)

	BeforeEach(func() {
		field = k8sfield.NewPath("spec")
		getter = &fakeProvisionerGetter{
			known: map[string]*corev1alpha1.ManagedClaimProvisioner{
				"pcie-aligned": {ObjectMeta: metav1.ObjectMeta{Name: "pcie-aligned"}},
			},
		}
	})

	specWithProvisioner := func(names ...string) *v1.VirtualMachineInstanceSpec {
		spec := &v1.VirtualMachineInstanceSpec{}
		for i, name := range names {
			spec.ResourceClaims = append(spec.ResourceClaims, v1.VirtualMachineInstanceResourceClaim{
				Name:                        fmt.Sprintf("claim%d", i),
				ManagedClaimProvisionerName: pointer.P(name),
			})
		}
		return spec
	}

	It("should accept a claim referencing an existing provisioner", func() {
		Expect(ValidateProvisionerExists(field, specWithProvisioner("pcie-aligned"), getter)).To(BeEmpty())
	})

	It("should reject a claim referencing a missing provisioner", func() {
		Expect(ValidateProvisionerExists(field, specWithProvisioner("does-not-exist"), getter)).To(
			ContainElement(metav1.StatusCause{
				Type:    metav1.CauseTypeFieldValueNotFound,
				Message: "ManagedClaimProvisioner \"does-not-exist\" does not exist",
				Field:   "spec.resourceClaims[0].managedClaimProvisionerName",
			}))
	})

	It("should report each missing provisioner separately", func() {
		causes := ValidateProvisionerExists(field, specWithProvisioner("missing-a", "pcie-aligned", "missing-b"), getter)

		Expect(causes).To(HaveLen(2))
		Expect(causes[0].Field).To(Equal("spec.resourceClaims[0].managedClaimProvisionerName"))
		Expect(causes[1].Field).To(Equal("spec.resourceClaims[2].managedClaimProvisionerName"))
	})

	It("should ignore non-managed claims", func() {
		spec := &v1.VirtualMachineInstanceSpec{
			ResourceClaims: []v1.VirtualMachineInstanceResourceClaim{
				{Name: "direct", ResourceClaimName: pointer.P("some-claim")},
				{Name: "template", ResourceClaimTemplateName: pointer.P("some-template")},
			},
		}

		Expect(ValidateProvisionerExists(field, spec, getter)).To(BeEmpty())
	})

	It("should not reject when the lookup itself fails", func() {
		// A cache read error is an infrastructure problem, not a user error.
		// Failing admission here would block every VMI creation while the
		// informer is unhealthy, so existence is treated as unproven rather
		// than disproven; the provisioner controller reports the real outcome.
		getter.err = fmt.Errorf("informer cache not synced")

		Expect(ValidateProvisionerExists(field, specWithProvisioner("pcie-aligned"), getter)).To(BeEmpty())
	})

	It("should tolerate a nil getter", func() {
		// virt-api may run before the informer is wired; skip rather than panic.
		Expect(ValidateProvisionerExists(field, specWithProvisioner("anything"), nil)).To(BeEmpty())
	})
})
