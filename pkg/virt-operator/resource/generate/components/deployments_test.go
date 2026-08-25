package components

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"

	operatorutil "kubevirt.io/kubevirt/pkg/virt-operator/util"
)

var _ = Describe("Deployments", func() {
	It("should create Prometheus service that is headless", func() {
		service := NewPrometheusService("mynamespace")
		Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	})

	It("should build the managed-claim controller deployment from the config", func() {
		config := operatorutil.GetTargetConfigFromKV(&v1.KubeVirt{
			ObjectMeta: metav1.ObjectMeta{Namespace: "test-namespace"},
			Spec: v1.KubeVirtSpec{
				ImageRegistry: "reg",
				ImageTag:      "v9.9.9",
			},
		})

		deployment := NewManagedClaimControllerDeployment(config, "kubevirt", "v9.9.9", "controller")

		Expect(deployment.Name).To(Equal(VirtManagedClaimControllerName))
		Expect(deployment.Namespace).To(Equal("test-namespace"))
		Expect(deployment.Spec.Template.Spec.ServiceAccountName).To(Equal(ManagedClaimControllerServiceAccountName))
		Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))

		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Command).To(ContainElement(VirtManagedClaimControllerName))
		Expect(container.Image).To(ContainSubstring(VirtManagedClaimControllerName))
	})
})
