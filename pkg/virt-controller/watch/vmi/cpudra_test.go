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

package vmi

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	virtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("handleCPUDRAClaim", func() {
	var (
		kubeClient *fake.Clientset
		ctrl       *Controller
		config     *virtconfig.ClusterConfig
	)

	newDedicatedCPUVMI := func(name string, uid types.UID) *virtv1.VirtualMachineInstance {
		vmi := libvmi.New(
			libvmi.WithNamespace("default"),
			libvmi.WithCPUCount(4, 0, 0),
			libvmi.WithDedicatedCPUPlacement(),
		)
		vmi.Name = name
		vmi.UID = uid
		return vmi
	}

	setUpController := func(gateEnabled bool) {
		kv := &virtv1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{Name: "kubevirt", Namespace: "kubevirt"},
		}
		if gateEnabled {
			kv.Spec.Configuration.DeveloperConfiguration = &virtv1.DeveloperConfiguration{
				FeatureGates: []string{"CPUsWithDRA"},
			}
		}
		config, _, _ = testutils.NewFakeClusterConfigUsingKV(kv)

		kubeClient = fake.NewSimpleClientset()
		virtClient := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
		virtClient.EXPECT().ResourceV1().Return(kubeClient.ResourceV1()).AnyTimes()

		ctrl = &Controller{
			clientset:     virtClient,
			clusterConfig: config,
		}
	}

	It("does nothing when the feature gate is disabled", func() {
		setUpController(false)
		vmi := newDedicatedCPUVMI("testvmi", "uid-1")

		Expect(ctrl.handleCPUDRAClaim(vmi)).To(BeNil())

		claims, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claims.Items).To(BeEmpty())
	})

	It("creates a claim owned by the VMI when the feature gate is enabled", func() {
		setUpController(true)
		vmi := newDedicatedCPUVMI("testvmi", "uid-1")

		Expect(ctrl.handleCPUDRAClaim(vmi)).To(BeNil())

		claim, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.Background(), dra.CPUClaimName(vmi), metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claim.OwnerReferences).To(HaveLen(1))
		Expect(claim.OwnerReferences[0].UID).To(Equal(vmi.UID))
	})

	It("is idempotent when the claim already exists and is owned by the same VMI", func() {
		setUpController(true)
		vmi := newDedicatedCPUVMI("testvmi", "uid-1")

		Expect(ctrl.handleCPUDRAClaim(vmi)).To(BeNil())
		Expect(ctrl.handleCPUDRAClaim(vmi)).To(BeNil())

		claims, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claims.Items).To(HaveLen(1))
	})

	It("fails when a claim with the same name exists but is owned by a different VMI", func() {
		setUpController(true)
		staleOwner := newDedicatedCPUVMI("testvmi", "stale-uid")
		staleClaim := dra.NewCPUResourceClaim(staleOwner, 4)
		_, err := kubeClient.ResourceV1().ResourceClaims(staleOwner.Namespace).Create(context.Background(), staleClaim, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		newVMI := newDedicatedCPUVMI("testvmi", "new-uid")
		syncErr := ctrl.handleCPUDRAClaim(newVMI)
		Expect(syncErr).ToNot(BeNil())
	})
})
