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
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	"kubevirt.io/kubevirt/pkg/dra"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

var _ = Describe("handleDRAResourcesClaim", func() {
	var (
		kubeClient    *fake.Clientset
		virtClientset *kubevirtfake.Clientset
		ctrl          *Controller
		config        *virtconfig.ClusterConfig
	)

	newVMI := func(name string, uid types.UID, opts ...libvmi.Option) *virtv1.VirtualMachineInstance {
		allOpts := append([]libvmi.Option{
			libvmi.WithNamespace("default"),
			libvmi.WithCPUCount(4, 0, 0),
			libvmi.WithDedicatedCPUPlacement(),
		}, opts...)
		vmi := libvmi.New(allOpts...)
		vmi.Name = name
		vmi.UID = uid
		return vmi
	}

	// newNonDedicatedCPUVMI builds a VMI without dedicatedCpuPlacement, for
	// exercising the memory-DRA-only path (usesCPUDRA == false).
	newNonDedicatedCPUVMI := func(name string, uid types.UID, opts ...libvmi.Option) *virtv1.VirtualMachineInstance {
		allOpts := append([]libvmi.Option{libvmi.WithNamespace("default")}, opts...)
		vmi := libvmi.New(allOpts...)
		vmi.Name = name
		vmi.UID = uid
		return vmi
	}

	// addVMI registers vmi with the fake VirtualMachineInstance clientset so
	// setUsesDRAResourcesAnnotation's Patch call has an object to patch.
	addVMI := func(vmi *virtv1.VirtualMachineInstance) {
		_, err := virtClientset.KubevirtV1().VirtualMachineInstances(vmi.Namespace).Create(context.Background(), vmi, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
	}

	setUpController := func(gates ...string) {
		kv := &virtv1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{Name: "kubevirt", Namespace: "kubevirt"},
		}
		if len(gates) > 0 {
			kv.Spec.Configuration.DeveloperConfiguration = &virtv1.DeveloperConfiguration{
				FeatureGates: gates,
			}
		}
		config, _, _ = testutils.NewFakeClusterConfigUsingKV(kv)

		kubeClient = fake.NewSimpleClientset()
		virtClientset = kubevirtfake.NewSimpleClientset()
		virtClient := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
		virtClient.EXPECT().ResourceV1().Return(kubeClient.ResourceV1()).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance("default").Return(virtClientset.KubevirtV1().VirtualMachineInstances("default")).AnyTimes()

		ctrl = &Controller{
			clientset:     virtClient,
			clusterConfig: config,
		}
	}

	It("does nothing when no DRA gate is enabled", func() {
		setUpController()
		vmi := newVMI("testvmi", "uid-1")

		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())

		claims, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claims.Items).To(BeEmpty())
	})

	It("creates a CPU-only claim owned by the VMI when only CPUsWithDRA is enabled", func() {
		setUpController("CPUsWithDRA")
		vmi := newVMI("testvmi", "uid-1")
		addVMI(vmi)

		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())

		claim, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.Background(), dra.ResourcesClaimName(vmi), metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claim.OwnerReferences).To(HaveLen(1))
		Expect(claim.OwnerReferences[0].UID).To(Equal(vmi.UID))
		Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
		Expect(claim.Spec.Devices.Constraints).To(BeEmpty())
	})

	It("creates a combined CPU+memory claim with a NUMA constraint when both gates are enabled", func() {
		setUpController("CPUsWithDRA", "MemoryWithDRA")
		vmi := newVMI("testvmi", "uid-1", libvmi.WithHugepages("1Gi"))
		addVMI(vmi)

		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())

		claim, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.Background(), dra.ResourcesClaimName(vmi), metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(2))
		Expect(claim.Spec.Devices.Constraints).To(HaveLen(1))
	})

	It("creates a memory-only claim with no cpu request when only MemoryWithDRA is enabled", func() {
		setUpController("MemoryWithDRA")
		vmi := newNonDedicatedCPUVMI("testvmi", "uid-1", libvmi.WithHugepages("1Gi"))
		addVMI(vmi)

		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())

		claim, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).Get(context.Background(), dra.ResourcesClaimName(vmi), metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claim.Spec.Devices.Requests).To(HaveLen(1))
		Expect(claim.Spec.Devices.Requests[0].Name).To(Equal(dra.MemoryRequestName))
		Expect(claim.Spec.Devices.Constraints).To(BeEmpty())
	})

	It("fails when the hugepage size has no known DRA memory DeviceClass", func() {
		setUpController("CPUsWithDRA", "MemoryWithDRA")
		vmi := newVMI("testvmi", "uid-1", libvmi.WithHugepages("4Ki"))

		Expect(ctrl.handleDRAResourcesClaim(vmi)).ToNot(BeNil())
	})

	It("is idempotent when the claim already exists and is owned by the same VMI", func() {
		setUpController("CPUsWithDRA")
		vmi := newVMI("testvmi", "uid-1")
		addVMI(vmi)

		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())
		Expect(ctrl.handleDRAResourcesClaim(vmi)).To(BeNil())

		claims, err := kubeClient.ResourceV1().ResourceClaims(vmi.Namespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(claims.Items).To(HaveLen(1))
	})

	It("fails when a claim with the same name exists but is owned by a different VMI", func() {
		setUpController("CPUsWithDRA")
		staleOwner := newVMI("testvmi", "stale-uid")
		staleClaim := dra.NewResourcesClaim(staleOwner, true, 4, nil, "")
		_, err := kubeClient.ResourceV1().ResourceClaims(staleOwner.Namespace).Create(context.Background(), staleClaim, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		newVMI := newVMI("testvmi", "new-uid")
		syncErr := ctrl.handleDRAResourcesClaim(newVMI)
		Expect(syncErr).ToNot(BeNil())
	})
})
