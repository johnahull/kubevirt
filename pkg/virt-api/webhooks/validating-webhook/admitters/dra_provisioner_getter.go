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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"

	corev1alpha1 "kubevirt.io/api/core/v1alpha1"

	draadmitter "kubevirt.io/kubevirt/pkg/dra/admitter"
)

// managedClaimProvisionerGetter adapts an informer's local cache to the
// draadmitter.ProvisionerGetter interface. It lives here, in the client-go
// aware webhook layer, rather than in pkg/dra/admitter, which deliberately
// keeps its validation logic free of cache plumbing.
type managedClaimProvisionerGetter struct {
	store cache.Store
}

// NewManagedClaimProvisionerGetter returns a ProvisionerGetter that reads
// ManagedClaimProvisioners from the given informer's local cache. The
// resource is cluster scoped, so its cache key is the bare name.
func NewManagedClaimProvisionerGetter(informer cache.SharedIndexInformer) draadmitter.ProvisionerGetter {
	return &managedClaimProvisionerGetter{store: informer.GetStore()}
}

func (g *managedClaimProvisionerGetter) Get(name string) (*corev1alpha1.ManagedClaimProvisioner, error) {
	obj, exists, err := g.store.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(
			corev1alpha1.Resource(corev1alpha1.ResourceManagedClaimProvisioners), name)
	}

	provisioner, ok := obj.(*corev1alpha1.ManagedClaimProvisioner)
	if !ok {
		return nil, fmt.Errorf("unexpected object type %T in ManagedClaimProvisioner cache", obj)
	}
	return provisioner, nil
}
