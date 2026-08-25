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
 * Copyright the KubeVirt Authors.
 */

package rbac

import (
	"fmt"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
)

var _ = Describe("Managed-claim controller SA cluster role and role bindings", func() {
	const expectedNamespace = "kubevirt"
	Context("GetAllManagedClaimController", func() {
		allObjects := GetAllManagedClaimController(expectedNamespace)

		It("should not be nil", func() {
			Expect(allObjects).ToNot(BeNil())
		})

		DescribeTable("cluster role should contain rule to", func(apiGroup, resource string, verbs ...string) {
			clusterRole, ok := getObject(allObjects, reflect.TypeOf(&rbacv1.ClusterRole{}), ManagedClaimControllerServiceAccountName).(*rbacv1.ClusterRole)
			Expect(ok).To(BeTrue())
			Expect(clusterRole).ToNot(BeNil())
			expectExactRuleExists(clusterRole.Rules, apiGroup, resource, verbs...)
		},
			Entry(fmt.Sprintf("get/list/watch/create/update %s/%s", "resource.k8s.io", "resourceclaims"), "resource.k8s.io", "resourceclaims", "get", "list", "watch", "create", "update"),
			Entry(fmt.Sprintf("get/list/watch %s/%s", GroupName, apiVMInstances), GroupName, apiVMInstances, "get", "list", "watch"),
			Entry(fmt.Sprintf("get/list/watch %s/%s", GroupName, "managedclaimprovisioners"), GroupName, "managedclaimprovisioners", "get", "list", "watch"),
			Entry(fmt.Sprintf("update/create/patch %s/%s", "", "events"), "", "events", "update", "create", "patch"),
		)

		It("should not grant access to virtualmachineinstances/status", func() {
			clusterRole, ok := getObject(allObjects, reflect.TypeOf(&rbacv1.ClusterRole{}), ManagedClaimControllerServiceAccountName).(*rbacv1.ClusterRole)
			Expect(ok).To(BeTrue())
			for _, rule := range clusterRole.Rules {
				for _, resource := range rule.Resources {
					Expect(resource).ToNot(Equal("virtualmachineinstances/status"))
				}
			}
		})

		DescribeTable("role should contain rule to", func(apiGroup, resource string, verbs ...string) {
			role, ok := getObject(allObjects, reflect.TypeOf(&rbacv1.Role{}), ManagedClaimControllerServiceAccountName).(*rbacv1.Role)
			Expect(ok).To(BeTrue())
			Expect(role).ToNot(BeNil())
			expectExactRuleExists(role.Rules, apiGroup, resource, verbs...)
		},
			Entry(fmt.Sprintf("leases %s/%s", "coordination.k8s.io", "leases"), "coordination.k8s.io", "leases", "get", "list", "watch", "delete", "update", "create", "patch"),
		)
	})
})
