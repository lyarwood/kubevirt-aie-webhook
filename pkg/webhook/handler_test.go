package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt-aie-webhook/pkg/config"
	webhookpkg "kubevirt.io/kubevirt-aie-webhook/pkg/webhook"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(kubevirtv1.AddToScheme(s))
	return s
}

func newAdmissionRequest(pod *corev1.Pod) admission.Request {
	raw, err := json.Marshal(pod)
	Expect(err).ToNot(HaveOccurred())
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Object:    runtime.RawExtension{Raw: raw},
			Operation: admissionv1.Create,
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		},
	}
}

func newStoreWithRules(rules ...config.Rule) *config.ConfigStore {
	store := config.NewConfigStore()
	cfg := config.LauncherConfig{Rules: rules}
	data, err := json.Marshal(cfg)
	Expect(err).ToNot(HaveOccurred())
	Expect(store.Update(data)).To(Succeed())
	return store
}

func newVirtLauncherPod(name, namespace, vmiName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"kubevirt.io": "virt-launcher",
			},
			Annotations: map[string]string{},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "kubevirt.io/v1",
					Kind:       "VirtualMachineInstance",
					Name:       vmiName,
				},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "compute",
					Image: "registry.kubevirt.io/virt-launcher:v1.0.0",
				},
			},
		},
	}
}

func newMutator(scheme *runtime.Scheme, k8sClient client.Client, store *config.ConfigStore) *webhookpkg.VirtLauncherMutator {
	return &webhookpkg.VirtLauncherMutator{
		Client:  k8sClient,
		Store:   store,
		Decoder: admission.NewDecoder(scheme),
	}
}

var _ = Describe("VirtLauncherMutator", func() {
	var (
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		scheme = newScheme()
	})

	Context("when the pod is not a virt-launcher", func() {
		It("should allow the pod without mutation", func() {
			store := newStoreWithRules(config.Rule{
				Name:  "catch-all",
				Image: "alt-image:v1",
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "random-pod",
					Namespace: "default",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx:latest"},
					},
				},
			}

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeEmpty())
		})
	})

	Context("when the pod has no VMI owner reference", func() {
		It("should allow the pod without mutation", func() {
			store := newStoreWithRules(config.Rule{
				Name:  "test",
				Image: "alt-image:v1",
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "virt-launcher-test",
					Namespace: "default",
					Labels:    map[string]string{"kubevirt.io": "virt-launcher"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "compute", Image: "original:v1"},
					},
				},
			}

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeEmpty())
		})
	})

	Context("when a GPU device name matches", func() {
		It("should mutate the container image and add the annotation", func() {
			altImage := "registry.example.com/aie-launcher:v1"
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-abcde", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).ToNot(BeEmpty())
			expectImagePatch(resp, altImage)
			expectAnnotationPatch(resp, altImage)
			expectIommufdResourcePatch(resp)
		})
	})

	Context("when a HostDevice device name matches", func() {
		It("should mutate the container image", func() {
			altImage := "registry.example.com/aie-launcher:v2"
			store := newStoreWithRules(config.Rule{
				Name:  "hostdev-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"intel.com/vfio-device"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							HostDevices: []kubevirtv1.HostDevice{
								{Name: "hd0", DeviceName: "intel.com/vfio-device"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-12345", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)
		})
	})

	Context("when VMI labels match", func() {
		It("should mutate the container image", func() {
			altImage := "registry.example.com/aie-launcher:v3"
			store := newStoreWithRules(config.Rule{
				Name:  "label-rule",
				Image: altImage,
				Selector: config.Selector{
					VMLabels: &config.VMLabels{
						MatchLabels: map[string]string{
							"aie.kubevirt.io/launcher": "true",
						},
					},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
					Labels: map[string]string{
						"aie.kubevirt.io/launcher": "true",
						"other-label":             "value",
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-xyz", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)
		})
	})

	Context("when a rule has a nodeSelector", func() {
		It("should inject node affinity when pod has no affinity", func() {
			altImage := "registry.example.com/aie-launcher:v4"
			store := newStoreWithRules(config.Rule{
				Name:  "label-with-affinity",
				Image: altImage,
				Selector: config.Selector{
					VMLabels: &config.VMLabels{
						MatchLabels: map[string]string{
							"aie.kubevirt.io/launcher": "true",
						},
					},
				},
				NodeSelector: &config.NodeSelector{
					MatchLabels: map[string]string{
						"aie.kubevirt.io/node": "true",
					},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
					Labels: map[string]string{
						"aie.kubevirt.io/launcher": "true",
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-affinity", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)
			expectNodeAffinityPatch(resp, "aie.kubevirt.io/node", "true")
		})

		It("should append to existing node affinity terms", func() {
			altImage := "registry.example.com/aie-launcher:v4"
			store := newStoreWithRules(config.Rule{
				Name:  "label-with-affinity",
				Image: altImage,
				Selector: config.Selector{
					VMLabels: &config.VMLabels{
						MatchLabels: map[string]string{
							"aie.kubevirt.io/launcher": "true",
						},
					},
				},
				NodeSelector: &config.NodeSelector{
					MatchLabels: map[string]string{
						"aie.kubevirt.io/node": "true",
					},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
					Labels: map[string]string{
						"aie.kubevirt.io/launcher": "true",
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-existing", "default", "test-vmi")
			pod.Spec.Affinity = &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "existing-key",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"existing-value"},
									},
								},
							},
						},
					},
				},
			}

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)
			expectNodeAffinityPatch(resp, "aie.kubevirt.io/node", "true")
		})
	})

	Context("when a rule has no nodeSelector", func() {
		It("should not inject node affinity", func() {
			altImage := "registry.example.com/aie-launcher:v5"
			store := newStoreWithRules(config.Rule{
				Name:  "no-affinity-rule",
				Image: altImage,
				Selector: config.Selector{
					VMLabels: &config.VMLabels{
						MatchLabels: map[string]string{
							"aie.kubevirt.io/launcher": "true",
						},
					},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
					Labels: map[string]string{
						"aie.kubevirt.io/launcher": "true",
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-noaff", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)
			expectNoAffinityPatch(resp)
		})
	})

	Context("when no rule matches", func() {
		It("should allow the pod without mutation", func() {
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: "alt-image:v1",
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-abc", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeEmpty())
		})
	})

	Context("when no config is loaded", func() {
		It("should allow the pod without mutation", func() {
			store := config.NewConfigStore()

			pod := newVirtLauncherPod("virt-launcher-test-vmi-abc", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeEmpty())
		})
	})

	Context("when multiple rules match", func() {
		It("should apply the first matching rule", func() {
			firstImage := "first-image:v1"
			secondImage := "second-image:v2"
			store := newStoreWithRules(
				config.Rule{
					Name:  "first-rule",
					Image: firstImage,
					Selector: config.Selector{
						DeviceNames: []string{"nvidia.com/A100"},
					},
				},
				config.Rule{
					Name:  "second-rule",
					Image: secondImage,
					Selector: config.Selector{
						VMLabels: &config.VMLabels{
							MatchLabels: map[string]string{"aie.kubevirt.io/launcher": "true"},
						},
					},
				},
			)

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
					Labels: map[string]string{
						"aie.kubevirt.io/launcher": "true",
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-first", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, firstImage)
		})
	})

	Context("when the admission request is invalid", func() {
		It("should return a bad request error", func() {
			store := newStoreWithRules()
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			mutator := newMutator(scheme, k8sClient, store)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:    "test-uid",
					Object: runtime.RawExtension{Raw: []byte("not-json")},
				},
			}

			resp := mutator.Handle(context.Background(), req)
			Expect(resp.Allowed).To(BeFalse())
			Expect(resp.Result.Code).To(Equal(int32(http.StatusBadRequest)))
		})
	})

	Context("iommufd resource injection", func() {
		It("should inject iommufd resource limit when container has no resources", func() {
			altImage := "registry.example.com/aie-launcher:v1"
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-nores", "default", "test-vmi")
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectIommufdResourcePatch(resp)

			// Verify the patch adds the full resources structure
			for _, p := range resp.Patches {
				if p.Path == "/spec/containers/0/resources" {
					Expect(p.Operation).To(Equal("add"))
					return
				}
			}
			Fail("expected patch at /spec/containers/0/resources")
		})

		It("should skip iommufd resource injection when the resource is already defined", func() {
			altImage := "registry.example.com/aie-launcher:v1"
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-existing-iommufd", "default", "test-vmi")
			pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceName("devices.kubevirt.io/iommufd"): resource.MustParse("1"),
				},
			}
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectImagePatch(resp, altImage)

			// Verify no iommufd resource patch was generated
			for _, p := range resp.Patches {
				Expect(p.Path).ToNot(ContainSubstring("iommufd"),
					"expected no iommufd patch but found one at %s", p.Path)
			}
		})

		It("should find the compute container by name when it is not the first container", func() {
			altImage := "registry.example.com/aie-launcher:v1"
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-reorder", "default", "test-vmi")
			// Prepend a sidecar container so compute is at index 1
			pod.Spec.Containers = append([]corev1.Container{
				{Name: "sidecar", Image: "sidecar:v1"},
			}, pod.Spec.Containers...)
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())

			// Verify the iommufd patch targets container index 1
			for _, p := range resp.Patches {
				if p.Path == "/spec/containers/1/resources" ||
					p.Path == "/spec/containers/1/resources/limits" ||
					p.Path == "/spec/containers/1/resources/limits/devices.kubevirt.io~1iommufd" {
					return
				}
			}
			Fail("expected iommufd resource patch at /spec/containers/1/...")
		})

		It("should inject iommufd resource limit when container already has resource limits", func() {
			altImage := "registry.example.com/aie-launcher:v1"
			store := newStoreWithRules(config.Rule{
				Name:  "gpu-rule",
				Image: altImage,
				Selector: config.Selector{
					DeviceNames: []string{"nvidia.com/A100"},
				},
			})

			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "default",
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							GPUs: []kubevirtv1.GPU{
								{Name: "gpu0", DeviceName: "nvidia.com/A100"},
							},
						},
					},
				},
			}

			pod := newVirtLauncherPod("virt-launcher-test-vmi-withres", "default", "test-vmi")
			pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vmi).Build()
			mutator := newMutator(scheme, k8sClient, store)

			resp := mutator.Handle(context.Background(), newAdmissionRequest(pod))
			Expect(resp.Allowed).To(BeTrue())
			expectIommufdResourcePatch(resp)

			// Verify the patch adds only the specific resource key
			for _, p := range resp.Patches {
				if p.Path == "/spec/containers/0/resources/limits/devices.kubevirt.io~1iommufd" {
					Expect(p.Operation).To(Equal("add"))
					return
				}
			}
			Fail("expected patch at /spec/containers/0/resources/limits/devices.kubevirt.io~1iommufd")
		})
	})
})

func expectImagePatch(resp admission.Response, expectedImage string) {
	ExpectWithOffset(1, resp.Patches).ToNot(BeEmpty())
	found := false
	for _, p := range resp.Patches {
		if p.Path == "/spec/containers/0/image" {
			ExpectWithOffset(1, p.Value).To(Equal(expectedImage))
			found = true
			break
		}
	}
	ExpectWithOffset(1, found).To(BeTrue(), "expected patch at /spec/containers/0/image")
}

func expectAnnotationPatch(resp admission.Response, expectedImage string) {
	ExpectWithOffset(1, resp.Patches).ToNot(BeEmpty())
	found := false
	for _, p := range resp.Patches {
		if p.Path == "/metadata/annotations/kubevirt.io~1alternative-launcher-image" {
			ExpectWithOffset(1, p.Value).To(Equal(expectedImage))
			found = true
			break
		}
	}
	ExpectWithOffset(1, found).To(BeTrue(), "expected patch at annotation path")
}

func expectNodeAffinityPatch(resp admission.Response, expectedKey, expectedValue string) {
	ExpectWithOffset(1, resp.Patches).ToNot(BeEmpty())
	for _, p := range resp.Patches {
		if p.Path == "/spec/affinity" ||
			p.Path == "/spec/affinity/nodeAffinity" ||
			p.Path == "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution" ||
			p.Path == "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/-" {
			return
		}
	}
	ExpectWithOffset(1, false).To(BeTrue(), "expected a node affinity patch")
}

func expectIommufdResourcePatch(resp admission.Response) {
	ExpectWithOffset(1, resp.Patches).ToNot(BeEmpty())
	found := false
	for _, p := range resp.Patches {
		if p.Path == "/spec/containers/0/resources" ||
			p.Path == "/spec/containers/0/resources/limits" ||
			p.Path == "/spec/containers/0/resources/limits/devices.kubevirt.io~1iommufd" {
			found = true
			break
		}
	}
	ExpectWithOffset(1, found).To(BeTrue(), "expected an iommufd resource patch")
}

func expectNoAffinityPatch(resp admission.Response) {
	for _, p := range resp.Patches {
		ExpectWithOffset(1, p.Path).ToNot(HavePrefix("/spec/affinity"),
			"expected no affinity patch but found one at %s", p.Path)
	}
}
