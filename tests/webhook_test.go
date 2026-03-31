package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	alternativeLauncherLabel      = "kubevirt-aie-webhook/alternative-launcher"
	alternativeLauncherAnnotation = "kubevirt.io/alternative-launcher-image"
	nodeAffinityLabel             = "kubevirt-aie-webhook/node-affinity"
	nodeLabel                     = "kubevirt-aie-webhook/node"
	iommufdResourceName           = "devices.kubevirt.io/iommufd"
	pollInterval                  = 2 * time.Second
	pollTimeout                   = 5 * time.Minute
)

func newGuestlessVMI(name, namespace string, labels map[string]string) *kubevirtv1.VirtualMachineInstance {
	terminationGracePeriod := int64(0)
	return &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			TerminationGracePeriodSeconds: &terminationGracePeriod,
			Domain: kubevirtv1.DomainSpec{
				Resources: kubevirtv1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}
}

func waitForVirtLauncherPod(namespace, vmiName string) *corev1.Pod {
	var pod *corev1.Pod
	Eventually(func(g Gomega) {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(context.Background(), podList,
			client.InNamespace(namespace),
			client.MatchingLabels{
				"kubevirt.io":        "virt-launcher",
				"vm.kubevirt.io/name": vmiName,
			},
		)).To(Succeed())
		g.Expect(podList.Items).NotTo(BeEmpty(), "no virt-launcher pod found for VMI %s", vmiName)
		pod = &podList.Items[0]
	}, pollTimeout, pollInterval).Should(Succeed())
	return pod
}

var _ = Describe("Webhook functional tests", func() {
	Context("when a VMI has the alternative-launcher label", func() {
		It("should mutate the virt-launcher pod image and inject iommufd resource", func() {
			vmi := newGuestlessVMI("test-mutated", testNamespace, map[string]string{
				alternativeLauncherLabel: "true",
			})
			Expect(k8sClient.Create(context.Background(), vmi)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), vmi)
			})

			pod := waitForVirtLauncherPod(testNamespace, vmi.Name)
			Expect(pod.Spec.Containers[0].Image).To(Equal(develAltLauncherImage),
				"expected compute container image to be the devel_alt launcher image")
			Expect(pod.Annotations).To(HaveKey(alternativeLauncherAnnotation),
				"expected alternative-launcher-image annotation to be set")

			By("verifying iommufd resource limit was injected")
			limits := pod.Spec.Containers[0].Resources.Limits
			Expect(limits).NotTo(BeNil(), "expected resource limits to be set")
			qty, ok := limits[corev1.ResourceName(iommufdResourceName)]
			Expect(ok).To(BeTrue(), "expected iommufd resource limit to be present")
			Expect(qty.String()).To(Equal("1"), "expected iommufd resource limit to be 1")

			By("verifying the VMI reaches Running")
			Eventually(func(g Gomega) {
				updatedVMI := &kubevirtv1.VirtualMachineInstance{}
				g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(vmi), updatedVMI)).To(Succeed())
				g.Expect(updatedVMI.Status.Phase).To(Equal(kubevirtv1.Running),
					"expected VMI to reach Running phase")
			}, pollTimeout, pollInterval).Should(Succeed())
		})
	})

	Context("when a VMI does not have the alternative-launcher label", func() {
		It("should not mutate the virt-launcher pod image", func() {
			vmi := newGuestlessVMI("test-not-mutated", testNamespace, nil)
			Expect(k8sClient.Create(context.Background(), vmi)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), vmi)
			})

			pod := waitForVirtLauncherPod(testNamespace, vmi.Name)
			Expect(pod.Spec.Containers[0].Image).NotTo(ContainSubstring("devel_alt"),
				"expected compute container image to not contain devel_alt")
			Expect(pod.Annotations).NotTo(HaveKey(alternativeLauncherAnnotation),
				"expected alternative-launcher-image annotation to not be set")

			By("verifying iommufd resource limit was not injected")
			limits := pod.Spec.Containers[0].Resources.Limits
			if limits != nil {
				_, ok := limits[corev1.ResourceName(iommufdResourceName)]
				Expect(ok).To(BeFalse(), "expected iommufd resource limit to not be present")
			}
		})
	})

	Context("when a VMI matches a rule with a nodeSelector", RequiresTwoSchedulableNodes, func() {
		It("should inject node affinity and schedule to labeled node", func() {
			vmi := newGuestlessVMI("test-node-affinity", testNamespace, map[string]string{
				nodeAffinityLabel: "true",
			})
			Expect(k8sClient.Create(context.Background(), vmi)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), vmi)
			})

			pod := waitForVirtLauncherPod(testNamespace, vmi.Name)

			By("verifying the launcher image was mutated")
			Expect(pod.Spec.Containers[0].Image).To(Equal(develAltLauncherImage),
				"expected compute container image to be the devel_alt launcher image")
			Expect(pod.Annotations).To(HaveKey(alternativeLauncherAnnotation),
				"expected alternative-launcher-image annotation to be set")

			By("verifying node affinity was injected")
			Expect(pod.Spec.Affinity).NotTo(BeNil(), "expected pod affinity to be set")
			Expect(pod.Spec.Affinity.NodeAffinity).NotTo(BeNil(), "expected node affinity to be set")
			required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			Expect(required).NotTo(BeNil(), "expected requiredDuringSchedulingIgnoredDuringExecution to be set")

			found := false
			for _, term := range required.NodeSelectorTerms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == nodeLabel &&
						expr.Operator == corev1.NodeSelectorOpIn &&
						len(expr.Values) == 1 && expr.Values[0] == "true" {
						found = true
					}
				}
			}
			Expect(found).To(BeTrue(),
				"expected a node affinity term with %s In [true]", nodeLabel)

			By("verifying the pod was scheduled to a node with the correct label")
			Eventually(func(g Gomega) {
				updatedPod := &corev1.Pod{}
				g.Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(pod), updatedPod)).To(Succeed())
				g.Expect(updatedPod.Spec.NodeName).NotTo(BeEmpty(), "expected pod to be scheduled")

				node := &corev1.Node{}
				g.Expect(k8sClient.Get(context.Background(), client.ObjectKey{Name: updatedPod.Spec.NodeName}, node)).To(Succeed())
				g.Expect(node.Labels).To(HaveKeyWithValue(nodeLabel, "true"),
					"expected pod to be scheduled on a node with label %s=true", nodeLabel)
			}, pollTimeout, pollInterval).Should(Succeed())
		})
	})

	Context("when a VMI matches a rule without a nodeSelector", func() {
		It("should not inject node affinity for the node label", func() {
			vmi := newGuestlessVMI("test-no-node-affinity", testNamespace, map[string]string{
				alternativeLauncherLabel: "true",
			})
			Expect(k8sClient.Create(context.Background(), vmi)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), vmi)
			})

			pod := waitForVirtLauncherPod(testNamespace, vmi.Name)

			By("verifying the launcher image was mutated")
			Expect(pod.Spec.Containers[0].Image).To(Equal(develAltLauncherImage),
				"expected compute container image to be the devel_alt launcher image")

			By("verifying no node affinity for the node label exists")
			if pod.Spec.Affinity != nil &&
				pod.Spec.Affinity.NodeAffinity != nil &&
				pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
					for _, expr := range term.MatchExpressions {
						Expect(expr.Key).NotTo(Equal(nodeLabel),
							"expected no node affinity term with key %s", nodeLabel)
					}
				}
			}
		})
	})
})
