# AGENTS.md

## Project Overview

kubevirt-aie-webhook is a standalone Kubernetes mutating admission webhook
written in Go. It intercepts virt-launcher Pod creation and conditionally
replaces the compute container image with an alternative launcher image based on
rules defined in a ConfigMap. It is a companion component to the
[kubevirt-aie](https://github.com/kubevirt/kubevirt-aie) project.

## Architecture

The project is a single Go binary built with
[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). It
runs two components inside one manager:

1. **ConfigMap reconciler** (`main.go`): Watches the
   `kubevirt-aie-launcher-config` ConfigMap and hot-reloads rules into a
   thread-safe `ConfigStore`.
2. **Admission webhook** (`pkg/webhook/handler.go`): Registered at
   `/mutate-pods`, handles virt-launcher pod CREATE events. Decodes the pod,
   finds the VMI owner reference, fetches the VMI, evaluates rules
   (first-match-wins), and returns a JSON patch.

### Key Packages

- `pkg/config` -- Configuration types (`LauncherConfig`, `Rule`, `Selector`,
  `VMLabels`) and the thread-safe `ConfigStore` with `Get()`/`Update()` methods.
- `pkg/webhook` -- The `VirtLauncherMutator` admission handler. Its only public
  method is `Handle()`. Internal helpers (`findVMIOwnerRef`, `matchRules`,
  `matchesDeviceNames`, `matchesVMLabels`, `escapeJSONPointer`) are unexported.
- `main.go` -- Wires everything together: scheme registration, manager creation,
  ConfigMap controller, webhook registration, health probes.

### External Dependencies

- `kubevirt.io/api` -- KubeVirt API types (VirtualMachineInstance, GPU,
  HostDevice).
- `sigs.k8s.io/controller-runtime` -- Manager, webhook server, fake client for
  tests.
- `gomodules.xyz/jsonpatch/v2` -- JSON Patch operations for admission responses.

## Development

### Build and Test

```sh
make build        # go build
make test         # go test ./... -v -count=1
make lint         # go vet ./...
make docker-build # multi-stage Docker build
```

### Running Tests

Tests use Ginkgo/Gomega and live in external `_test` packages (black-box
testing). They only exercise the public API:

- `pkg/config/config_test.go` -- `ConfigStore.Get()`, `ConfigStore.Update()`:
  YAML parsing, malformed input, config replacement, concurrent access.
- `pkg/webhook/handler_test.go` -- `VirtLauncherMutator.Handle()`: Uses
  controller-runtime's fake client to test all admission scenarios.

Run a specific package's tests:
```sh
go test ./pkg/config/... -v
go test ./pkg/webhook/... -v
```

### Code Conventions

- Go 1.24, module path `kubevirt.io/kubevirt-aie-webhook`.
- Standard controller-runtime patterns for the manager, reconciler, and webhook
  registration.
- No code generation or custom CRDs. Configuration is a plain ConfigMap with
  YAML content.
- The webhook handler does not modify the pod object in memory. It returns a
  JSON Patch via `admission.Patched()`.
- The compute container is always `containers[0]` in virt-launcher pods (a
  KubeVirt invariant).

### Configuration Format

Rules are defined in YAML inside the `kubevirt-aie-launcher-config` ConfigMap
under the `config.yaml` key:

```yaml
rules:
- name: "rule-name"
  image: "registry/image:tag"
  selector:
    deviceNames:       # OR'd with vmLabels
    - "vendor/device"
    vmLabels:
      matchLabels:
        key: "value"   # all must match (AND'd)
```

### Helm Chart

The Helm chart lives under `deploy/helm/kubevirt-aie-webhook/`. It deploys:
Deployment, Service, ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap,
MutatingWebhookConfiguration, and cert-manager Certificate + Issuer.

To render templates locally:
```sh
helm template kubevirt-aie-webhook deploy/helm/kubevirt-aie-webhook
```

### kubevirtci Integration

The project follows the
[common-instancetypes](https://github.com/kubevirt/common-instancetypes)
pattern for kubevirtci integration. A single `scripts/kubevirtci.sh` script
handles all cluster lifecycle operations as subcommands (`up`, `down`, `sync`,
`kubeconfig`, `kubectl`).

`cluster-up` clones [kubevirt](https://github.com/kubevirt/kubevirt)
(`release-1.8` branch by default) into `_kubevirt/` and delegates to its
`make cluster-up && make cluster-sync` to provision the kubevirtci cluster and
deploy KubeVirt from source.

`cluster-sync` builds the webhook container image, pushes it to the kubevirtci
cluster's registry, and deploys via `helm upgrade --install`.

The `_kubevirt/` directory is gitignored and can be removed with `make clean`.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NAMESPACE` | `kubevirt` | Namespace where the ConfigMap is watched |
| `KUBEVIRT_REPO` | `https://github.com/kubevirt/kubevirt.git` | KubeVirt git repository URL |
| `KUBEVIRT_BRANCH` | `release-1.8` | KubeVirt branch to clone |
| `KUBEVIRT_MEMORY_SIZE` | `16G` | Memory for kubevirtci cluster VMs |
| `DOCKER_TAG` | `devel` | Tag for the webhook container image |
| `DOCKER_PREFIX` | `quay.io/kubevirt` | Registry prefix for the webhook image |
| `IMAGE_PULL_POLICY` | `Always` | Image pull policy for cluster-sync |

### Ports

| Port | Purpose |
|------|---------|
| 9443 | Webhook HTTPS (TLS via cert-manager) |
| 8080 | Prometheus metrics |
| 8081 | Health/readiness probes |
