package webhook

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt-aie-webhook/pkg/config"
)

const (
	annotationKey  = "kubevirt.io/alternative-launcher-image"
	iommufdResource = "devices.kubevirt.io/iommufd"
)

// VirtLauncherMutator mutates virt-launcher pods to use alternative launcher images
// based on VMI device and label selectors.
type VirtLauncherMutator struct {
	Client  client.Reader
	Store   *config.ConfigStore
	Decoder admission.Decoder
}

// Handle processes admission requests for virt-launcher pod creation.
func (m *VirtLauncherMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("pod", req.Name, "namespace", req.Namespace)

	pod, err := decodePod(m.Decoder, req)
	if err != nil {
		logger.Error(err, "failed to decode pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	cfg := m.Store.Get()
	if cfg == nil {
		logger.V(1).Info("no launcher config loaded, allowing pod")
		return admission.Allowed("no config loaded")
	}

	ownerRef := findVMIOwnerRef(pod.OwnerReferences)
	if ownerRef == nil {
		logger.V(1).Info("pod has no VMI owner reference, allowing")
		return admission.Allowed("no VMI owner reference")
	}

	var vmi kubevirtv1.VirtualMachineInstance
	if err := m.Client.Get(ctx, types.NamespacedName{
		Name:      ownerRef.Name,
		Namespace: req.Namespace,
	}, &vmi); err != nil {
		logger.Error(err, "failed to fetch VMI", "vmi", ownerRef.Name)
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("failed to fetch VMI %s: %w", ownerRef.Name, err))
	}

	rule := matchRules(cfg.Rules, &vmi)
	if rule == nil {
		logger.V(1).Info("no matching rule found, allowing pod")
		return admission.Allowed("no matching rule")
	}

	logger.Info("matched rule, mutating launcher image",
		"rule", rule.Name, "image", rule.Image, "vmi", vmi.Name)

	patches := []jsonpatch.JsonPatchOperation{
		{
			Operation: "replace",
			Path:      "/spec/containers/0/image",
			Value:     rule.Image,
		},
		{
			Operation: "add",
			Path:      "/metadata/annotations/" + escapeJSONPointer(annotationKey),
			Value:     rule.Image,
		},
	}

	patches = append(patches, nodeAffinityPatches(pod, rule.NodeSelector)...)
	patches = append(patches, iommufdResourcePatches(pod)...)
	patches = append(patches, vfioMemoryOverheadPatches(pod, &vmi)...)

	return admission.Patched("launcher image replaced", patches...)
}

// decodePod extracts the pod from the admission request.
func decodePod(decoder admission.Decoder, req admission.Request) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := decoder.Decode(req, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// findVMIOwnerRef returns the first ownerReference that points to a VirtualMachineInstance.
func findVMIOwnerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Kind == "VirtualMachineInstance" && refs[i].APIVersion == "kubevirt.io/v1" {
			return &refs[i]
		}
	}
	return nil
}

// matchRules evaluates rules in order and returns the first matching rule.
func matchRules(rules []config.Rule, vmi *kubevirtv1.VirtualMachineInstance) *config.Rule {
	for i := range rules {
		if matchesRule(&rules[i], vmi) {
			return &rules[i]
		}
	}
	return nil
}

// matchesRule checks if a VMI matches a single rule.
// DeviceNames and VMLabels are OR'd: either matching is sufficient.
func matchesRule(rule *config.Rule, vmi *kubevirtv1.VirtualMachineInstance) bool {
	if matchesDeviceNames(rule.Selector.DeviceNames, vmi) {
		return true
	}
	if matchesVMLabels(rule.Selector.VMLabels, vmi) {
		return true
	}
	return false
}

// matchesDeviceNames checks if any GPU or HostDevice DeviceName matches the selector.
func matchesDeviceNames(deviceNames []string, vmi *kubevirtv1.VirtualMachineInstance) bool {
	if len(deviceNames) == 0 {
		return false
	}
	nameSet := make(map[string]struct{}, len(deviceNames))
	for _, n := range deviceNames {
		nameSet[n] = struct{}{}
	}
	for _, gpu := range vmi.Spec.Domain.Devices.GPUs {
		if _, ok := nameSet[gpu.DeviceName]; ok {
			return true
		}
	}
	for _, hd := range vmi.Spec.Domain.Devices.HostDevices {
		if _, ok := nameSet[hd.DeviceName]; ok {
			return true
		}
	}
	return false
}

// matchesVMLabels checks if all matchLabels are present on the VMI.
func matchesVMLabels(vmLabels *config.VMLabels, vmi *kubevirtv1.VirtualMachineInstance) bool {
	if vmLabels == nil || len(vmLabels.MatchLabels) == 0 {
		return false
	}
	vmiLabels := vmi.GetLabels()
	if vmiLabels == nil {
		return false
	}
	for k, v := range vmLabels.MatchLabels {
		if vmiLabels[k] != v {
			return false
		}
	}
	return true
}

// nodeAffinityPatches builds JSON patch operations to inject a required node
// affinity term based on the rule's NodeSelector. It correctly merges with any
// existing affinity already present on the pod.
func nodeAffinityPatches(pod *corev1.Pod, nodeSelector *config.NodeSelector) []jsonpatch.JsonPatchOperation {
	if nodeSelector == nil || len(nodeSelector.MatchLabels) == 0 {
		return nil
	}

	keys := make([]string, 0, len(nodeSelector.MatchLabels))
	for k := range nodeSelector.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	expressions := make([]corev1.NodeSelectorRequirement, 0, len(keys))
	for _, k := range keys {
		expressions = append(expressions, corev1.NodeSelectorRequirement{
			Key:      k,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{nodeSelector.MatchLabels[k]},
		})
	}
	term := corev1.NodeSelectorTerm{
		MatchExpressions: expressions,
	}

	if pod.Spec.Affinity == nil {
		return []jsonpatch.JsonPatchOperation{{
			Operation: "add",
			Path:      "/spec/affinity",
			Value: corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{term},
					},
				},
			},
		}}
	}

	if pod.Spec.Affinity.NodeAffinity == nil {
		return []jsonpatch.JsonPatchOperation{{
			Operation: "add",
			Path:      "/spec/affinity/nodeAffinity",
			Value: corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{term},
				},
			},
		}}
	}

	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return []jsonpatch.JsonPatchOperation{{
			Operation: "add",
			Path:      "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution",
			Value: corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{term},
			},
		}}
	}

	return []jsonpatch.JsonPatchOperation{{
		Operation: "add",
		Path:      "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/-",
		Value:     term,
	}}
}

// iommufdResourcePatches builds JSON patch operations to inject the
// devices.kubevirt.io/iommufd resource limit on the compute container.
// It returns nil if the compute container is not found or the resource
// is already defined.
func iommufdResourcePatches(pod *corev1.Pod) []jsonpatch.JsonPatchOperation {
	idx := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == "compute"
	})
	if idx == -1 {
		return nil
	}

	container := pod.Spec.Containers[idx]

	if _, exists := container.Resources.Limits[corev1.ResourceName(iommufdResource)]; exists {
		return nil
	}

	prefix := fmt.Sprintf("/spec/containers/%d", idx)
	qty := resource.MustParse("1")

	if container.Resources.Limits == nil && container.Resources.Requests == nil {
		return []jsonpatch.JsonPatchOperation{{
			Operation: "add",
			Path:      prefix + "/resources",
			Value: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceName(iommufdResource): qty,
				},
			},
		}}
	}

	if container.Resources.Limits == nil {
		return []jsonpatch.JsonPatchOperation{{
			Operation: "add",
			Path:      prefix + "/resources/limits",
			Value: corev1.ResourceList{
				corev1.ResourceName(iommufdResource): qty,
			},
		}}
	}

	return []jsonpatch.JsonPatchOperation{{
		Operation: "add",
		Path:      prefix + "/resources/limits/" + escapeJSONPointer(iommufdResource),
		Value:     qty,
	}}
}

// vfioMemoryOverheadPatches builds JSON patch operations to increase the compute
// container's memory requests and limits to account for libvirt's per-device
// memlock requirement. With a vIOMMU, libvirt locks the full guest memory per
// VFIO device (qemuDomainGetMemLockLimitBytes). This adds (N-1) * guest_memory
// for N VFIO devices, since the first device's memory is already covered.
func vfioMemoryOverheadPatches(pod *corev1.Pod, vmi *kubevirtv1.VirtualMachineInstance) []jsonpatch.JsonPatchOperation {
	numDevices := len(vmi.Spec.Domain.Devices.GPUs) + len(vmi.Spec.Domain.Devices.HostDevices)
	if numDevices <= 1 {
		return nil
	}

	var guestMemory *resource.Quantity
	if vmi.Spec.Domain.Memory != nil && vmi.Spec.Domain.Memory.Guest != nil {
		guestMemory = vmi.Spec.Domain.Memory.Guest
	} else if req := vmi.Spec.Domain.Resources.Requests.Memory(); req != nil && !req.IsZero() {
		guestMemory = req
	}
	if guestMemory == nil || guestMemory.IsZero() {
		return nil
	}

	additionalBytes := guestMemory.Value() * int64(numDevices-1)

	idx := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool {
		return c.Name == "compute"
	})
	if idx == -1 {
		return nil
	}

	container := pod.Spec.Containers[idx]
	prefix := fmt.Sprintf("/spec/containers/%d", idx)
	var patches []jsonpatch.JsonPatchOperation

	if req, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
		newReq := req.DeepCopy()
		newReq.Set(newReq.Value() + additionalBytes)
		patches = append(patches, jsonpatch.JsonPatchOperation{
			Operation: "replace",
			Path:      prefix + "/resources/requests/memory",
			Value:     newReq.String(),
		})
	}

	if lim, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
		newLim := lim.DeepCopy()
		newLim.Set(newLim.Value() + additionalBytes)
		patches = append(patches, jsonpatch.JsonPatchOperation{
			Operation: "replace",
			Path:      prefix + "/resources/limits/memory",
			Value:     newLim.String(),
		})
	}

	return patches
}

// escapeJSONPointer escapes special characters in JSON Pointer tokens per RFC 6901.
func escapeJSONPointer(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '~':
			result = append(result, '~', '0')
		case '/':
			result = append(result, '~', '1')
		default:
			result = append(result, s[i])
		}
	}
	return string(result)
}
