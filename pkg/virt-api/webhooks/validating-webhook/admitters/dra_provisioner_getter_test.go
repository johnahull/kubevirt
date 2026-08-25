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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
)

var _ = Describe("ManagedClaimProvisioner getter", func() {
	newInformer := func() cache.SharedIndexInformer {
		return cache.NewSharedIndexInformer(
			&cache.ListWatch{}, &corev1alpha1.ManagedClaimProvisioner{}, 0, cache.Indexers{})
	}

	It("returns a provisioner present in the cache", func() {
		informer := newInformer()
		getter := NewManagedClaimProvisionerGetter(informer)
		Expect(informer.GetStore().Add(&corev1alpha1.ManagedClaimProvisioner{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-default"},
		})).To(Succeed())

		got, err := getter.Get("gpu-default")

		Expect(err).ToNot(HaveOccurred())
		Expect(got.Name).To(Equal("gpu-default"))
	})

	It("returns a NotFound error when the provisioner is absent", func() {
		// The create webhook turns a definitive NotFound into a user-facing
		// rejection, so an absent provisioner must read as NotFound.
		getter := NewManagedClaimProvisionerGetter(newInformer())

		_, err := getter.Get("missing")

		Expect(errors.IsNotFound(err)).To(BeTrue())
	})

	It("surfaces a cache error rather than reporting the provisioner missing", func() {
		// A store failure is not a definitive NotFound; the caller tolerates it
		// instead of rejecting VMI creation, so it must not look like NotFound.
		getter := &managedClaimProvisionerGetter{store: &fakeProvisionerStore{err: fmt.Errorf("cache is broken")}}

		_, err := getter.Get("gpu-default")

		Expect(err).To(MatchError(ContainSubstring("cache is broken")))
		Expect(errors.IsNotFound(err)).To(BeFalse())
	})

	It("rejects an unexpected object type in the cache", func() {
		getter := &managedClaimProvisionerGetter{store: &fakeProvisionerStore{obj: &metav1.Status{}, exists: true}}

		_, err := getter.Get("gpu-default")

		Expect(err).To(HaveOccurred())
		Expect(errors.IsNotFound(err)).To(BeFalse())
	})
})

// fakeProvisionerStore drives the getter's error and unexpected-type branches,
// which a real informer cache cannot be coaxed into producing.
type fakeProvisionerStore struct {
	cache.Store
	obj    interface{}
	exists bool
	err    error
}

func (f *fakeProvisionerStore) GetByKey(string) (interface{}, bool, error) {
	return f.obj, f.exists, f.err
}
