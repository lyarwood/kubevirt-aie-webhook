# kubevirt-aie-webhook

A Kubernetes mutating admission webhook that intercepts virt-launcher Pod
creation and conditionally replaces the compute container image with an
alternative launcher image. This enables using specialised virt-launcher builds
(e.g. GPU-optimised) for VMIs that match device or label selectors.

## How It Works

1. A `MutatingWebhookConfiguration` with `objectSelector: {matchLabels:
   {kubevirt.io: virt-launcher}}` ensures only virt-launcher pod CREATE events
   reach the webhook.
2. The handler extracts the VMI name from the pod's `ownerReferences`
   (`Kind=VirtualMachineInstance`, `APIVersion=kubevirt.io/v1`).
3. It fetches the full VMI object via the Kubernetes API.
4. It evaluates the VMI against ordered rules from the
   `kubevirt-aie-launcher-config` ConfigMap (first match wins).
5. On match, a JSON patch replaces `spec.containers[0].image` and adds the
   `kubevirt.io/alternative-launcher-image` annotation.

### Rule Matching

Rules are evaluated in order. Within a rule, `deviceNames` and `vmLabels` are
OR'd -- either matching is sufficient.

- **deviceNames**: Matches if any GPU (`vmi.Spec.Domain.Devices.GPUs[].DeviceName`)
  or HostDevice (`vmi.Spec.Domain.Devices.HostDevices[].DeviceName`) appears in
  the list.
- **vmLabels.matchLabels**: Matches if all specified key-value pairs are present
  on the VMI's labels.

## Configuration

The webhook reads its rules from a ConfigMap named
`kubevirt-aie-launcher-config` in its namespace. Changes are hot-reloaded
without restart.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubevirt-aie-launcher-config
data:
  config.yaml: |
    rules:
    - name: "nvidia-gpu-launcher"
      image: "registry.example.com/aie-virt-launcher:v1.8.0"
      selector:
        deviceNames:
        - "nvidia.com/A100"
        - "nvidia.com/H100"
    - name: "labeled-vms"
      image: "registry.example.com/aie-virt-launcher:v1.8.0"
      selector:
        vmLabels:
          matchLabels:
            aie.kubevirt.io/launcher: "true"
```

## Prerequisites

- Go 1.24+
- A Kubernetes cluster with KubeVirt installed
- [cert-manager](https://cert-manager.io/) (for TLS certificate provisioning)
- Helm 3 (for deployment)

## Building

```sh
# Build the binary
make build

# Run tests
make test

# Run go vet
make lint

# Build the container image
make docker-build IMG=quay.io/kubevirt/kubevirt-aie-webhook:latest

# Push the container image
make docker-push IMG=quay.io/kubevirt/kubevirt-aie-webhook:latest
```

## Deployment

Deploy with Helm:

```sh
helm install kubevirt-aie-webhook deploy/helm/kubevirt-aie-webhook \
  --namespace kubevirt \
  --set image.repository=quay.io/kubevirt/kubevirt-aie-webhook \
  --set image.tag=latest
```

Configure launcher rules via `values.yaml`:

```yaml
launcherConfig:
  rules:
  - name: "nvidia-gpu-launcher"
    image: "registry.example.com/aie-virt-launcher:v1.8.0"
    selector:
      deviceNames:
      - "nvidia.com/A100"
      - "nvidia.com/H100"
```

Render templates without installing:

```sh
make helm-template
```

### Helm Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Number of webhook pod replicas |
| `image.repository` | `quay.io/kubevirt/kubevirt-aie-webhook` | Container image repository |
| `image.tag` | `latest` | Container image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `namespace` | `kubevirt` | Namespace for all resources |
| `resources.limits.cpu` | `200m` | CPU limit |
| `resources.limits.memory` | `128Mi` | Memory limit |
| `resources.requests.cpu` | `100m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `certManager.enabled` | `true` | Use cert-manager for TLS certificates |
| `launcherConfig.rules` | `[]` | Launcher image selection rules |

## Development Cluster

The project uses [kubevirtci](https://github.com/kubevirt/kubevirtci) via
[kubevirt](https://github.com/kubevirt/kubevirt) to provide a local development
cluster with KubeVirt pre-deployed. This follows the same pattern as
[common-instancetypes](https://github.com/kubevirt/common-instancetypes).

```sh
# Bring up a cluster with KubeVirt deployed
make cluster-up

# Build the webhook image and deploy it into the cluster
make cluster-sync

# Tear down the cluster
make cluster-down
```

On first run, `cluster-up` clones kubevirt (`release-1.8` branch) into `_kubevirt/`,
runs its `make cluster-up` to provision the kubevirtci cluster, then runs
`make cluster-sync` to build and deploy KubeVirt from source.

The branch and repo can be overridden:

```sh
KUBEVIRT_BRANCH=release-1.8 KUBEVIRT_REPO=https://github.com/myorg/kubevirt.git make cluster-up
```

To access the cluster directly:

```sh
# Get the kubeconfig path
scripts/kubevirtci.sh kubeconfig

# Run kubectl commands
scripts/kubevirtci.sh kubectl get pods -n kubevirt
```

## Project Layout

```
main.go                          # Manager setup, ConfigMap watcher, webhook registration
pkg/
  config/
    config.go                    # LauncherConfig types + ConfigStore (thread-safe)
    config_test.go               # Ginkgo tests for config parsing and concurrency
  webhook/
    handler.go                   # VirtLauncherMutator admission handler
    handler_test.go              # Ginkgo tests for admission scenarios
scripts/
  kubevirtci.sh                  # Cluster lifecycle: up, down, sync, kubeconfig, kubectl
deploy/
  helm/
    kubevirt-aie-webhook/        # Helm chart
Dockerfile                       # Multi-stage build (distroless runtime)
Makefile                         # Build, test, lint, docker, helm, cluster targets
```

## Endpoints

| Port | Path | Description |
|------|------|-------------|
| 9443 | `/mutate-pods` | Mutating webhook admission endpoint |
| 8080 | `/metrics` | Prometheus metrics |
| 8081 | `/healthz` | Liveness probe |
| 8081 | `/readyz` | Readiness probe |

## Related

- [kubevirt-aie](https://github.com/kubevirt/kubevirt-aie) -- the KubeVirt fork
  that consumes the `kubevirt.io/alternative-launcher-image` annotation
