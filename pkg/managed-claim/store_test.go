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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	"kubevirt.io/kubevirt/pkg/testutils"
)

var _ = Describe("informer-backed ProvisionerStore", func() {
	var store ProvisionerStore

	BeforeEach(func() {
		informer, _ := testutils.NewFakeInformerFor(&corev1alpha1.ManagedClaimProvisioner{})
		Expect(informer.GetStore().Add(&corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-default"},
		})).To(Succeed())
		store = NewInformerProvisionerStore(informer)
	})

	It("returns the provisioner when present", func() {
		got, err := store.Get("gpu-default")
		Expect(err).ToNot(HaveOccurred())
		Expect(got.Name).To(Equal("gpu-default"))
	})

	It("returns an IsNotFound error when absent, which Rule 3 relies on", func() {
		_, err := store.Get("missing")
		Expect(errors.IsNotFound(err)).To(BeTrue())
	})
})
