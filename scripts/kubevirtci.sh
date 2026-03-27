#!/bin/bash

set -ex

export KUBEVIRT_MEMORY_SIZE="${KUBEVIRT_MEMORY_SIZE:-16G}"
export KUBEVIRT_REPO="${KUBEVIRT_REPO:-https://github.com/kubevirt/kubevirt.git}"
export KUBEVIRT_BRANCH="${KUBEVIRT_BRANCH:-release-1.8}"
export NAMESPACE="${NAMESPACE:-kubevirt}"
export IOMMUFD_DEVICE_PLUGIN_TAG="${IOMMUFD_DEVICE_PLUGIN_TAG:-v0.0.1}"

_base_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
_kubevirt_dir="${_base_dir}/_kubevirt"
_cluster_up_dir="${_kubevirt_dir}/kubevirtci/cluster-up"
_kubectl="${_cluster_up_dir}/kubectl.sh"
_cli="${_cluster_up_dir}/cli.sh"
_action=$1
shift

function determine_cri_bin() {
    "${_base_dir}/hack/container-engine.sh"
}

function kubevirtci::install() {
    if [[ ! -d "${_kubevirt_dir}" ]]; then
        git clone --depth 1 --branch "${KUBEVIRT_BRANCH}" "${KUBEVIRT_REPO}" "${_kubevirt_dir}"
    fi
}

function kubevirtci::up() {
    make cluster-up -C "${_kubevirt_dir}"
    make cluster-sync -C "${_kubevirt_dir}"

    echo "waiting for kubevirt to become ready, this can take a few minutes..."
    ${_kubectl} -n "${NAMESPACE}" wait kv kubevirt --for condition=Available --timeout=15m
}

function kubevirtci::down() {
    make cluster-down -C "${_kubevirt_dir}"
}

function kubevirtci::sync() {
    local cri
    cri=$(determine_cri_bin)
    if [[ -z "${cri}" ]]; then
        echo >&2 "no working container runtime found. Neither docker nor podman seems to work."
        exit 1
    fi

    local docker_tag="${DOCKER_TAG:-devel}"
    local docker_prefix="${DOCKER_PREFIX:-quay.io/kubevirt}"
    local image_name="${IMAGE_NAME:-kubevirt-aie-webhook}"
    local img="${docker_prefix}/${image_name}:${docker_tag}"

    echo "Building container image ${img}..."
    ${cri} build -t "${img}" "${_base_dir}"

    echo "Loading image into cluster..."
    local registry_port
    registry_port=$(${_cli} ports registry 2>/dev/null || true)
    if [[ -n "${registry_port}" ]]; then
        local registry="localhost:${registry_port}"
        local registry_img="${registry}/${image_name}:${docker_tag}"
        ${cri} tag "${img}" "${registry_img}"
        ${cri} push "${registry_img}" --tls-verify=false 2>/dev/null || \
            ${cri} push "${registry_img}"
        img="registry:5000/${image_name}:${docker_tag}"
    fi

    echo "Creating devel_alt virt-launcher image..."
    local virt_api_image
    virt_api_image=$(KUBECONFIG=$(kubevirtci::kubeconfig) ${_kubectl} get deployment virt-api \
        -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
    local image_prefix="${virt_api_image%/*}"
    local virt_launcher_devel="${image_prefix}/virt-launcher:devel"
    local virt_launcher_devel_alt="${image_prefix}/virt-launcher:devel_alt"

    if [[ -n "${registry_port}" ]]; then
        local local_devel="localhost:${registry_port}/kubevirt/virt-launcher:devel"
        local local_devel_alt="localhost:${registry_port}/kubevirt/virt-launcher:devel_alt"
        ${cri} pull "${local_devel}" --tls-verify=false 2>/dev/null || \
            ${cri} pull "${local_devel}"
        ${cri} tag "${local_devel}" "${local_devel_alt}"
        ${cri} push "${local_devel_alt}" --tls-verify=false 2>/dev/null || \
            ${cri} push "${local_devel_alt}"
    fi

    echo "Generating self-signed TLS certificates..."
    local cert_dir
    cert_dir=$(mktemp -d /tmp/kubevirt-aie-webhook-certs.XXXXXX)
    local service_name="kubevirt-aie-webhook"
    local san="DNS:${service_name}.${NAMESPACE}.svc,DNS:${service_name}.${NAMESPACE}.svc.cluster.local"

    openssl genrsa -out "${cert_dir}/ca.key" 2048
    openssl req -x509 -new -nodes -key "${cert_dir}/ca.key" \
        -subj "/CN=${service_name}-ca" -days 3650 \
        -out "${cert_dir}/ca.crt"

    openssl genrsa -out "${cert_dir}/tls.key" 2048
    openssl req -new -key "${cert_dir}/tls.key" \
        -subj "/CN=${service_name}.${NAMESPACE}.svc" \
        -out "${cert_dir}/tls.csr"
    openssl x509 -req -in "${cert_dir}/tls.csr" \
        -CA "${cert_dir}/ca.crt" -CAkey "${cert_dir}/ca.key" -CAcreateserial \
        -days 3650 -extfile <(echo "subjectAltName=${san}") \
        -out "${cert_dir}/tls.crt"

    echo "Creating TLS secret in cluster..."
    local kubeconfig
    kubeconfig=$(kubevirtci::kubeconfig)

    KUBECONFIG="${kubeconfig}" ${_kubectl} create secret tls "${service_name}-tls" \
        --cert="${cert_dir}/tls.crt" \
        --key="${cert_dir}/tls.key" \
        --namespace "${NAMESPACE}" \
        --dry-run=client -o yaml | KUBECONFIG="${kubeconfig}" ${_kubectl} apply -f -

    echo "Configuring webhook rule for functional tests..."
    local values_file
    values_file=$(mktemp /tmp/kubevirt-aie-webhook-values.XXXXXX.yaml)
    cat > "${values_file}" <<EOF
certManager:
  enabled: false
launcherConfig:
  rules:
  - name: "functest-devel-alt"
    image: "${virt_launcher_devel_alt}"
    selector:
      vmLabels:
        matchLabels:
          kubevirt-aie-webhook/alternative-launcher: "true"
  - name: "functest-node-affinity"
    image: "${virt_launcher_devel_alt}"
    selector:
      vmLabels:
        matchLabels:
          kubevirt-aie-webhook/node-affinity: "true"
    nodeSelector:
      matchLabels:
        kubevirt-aie-webhook/node: "true"
EOF

    echo "Deploying webhook to cluster..."
    KUBECONFIG="${kubeconfig}" helm upgrade --install kubevirt-aie-webhook \
        "${_base_dir}/deploy/helm/kubevirt-aie-webhook" \
        --namespace "${NAMESPACE}" \
        --create-namespace \
        --set image.repository="${img%:*}" \
        --set image.tag="${docker_tag}" \
        --set image.pullPolicy="${IMAGE_PULL_POLICY:-Always}" \
        --set namespace="${NAMESPACE}" \
        -f "${values_file}" \
        --wait

    echo "Patching webhook configuration with CA bundle..."
    local ca_bundle
    ca_bundle=$(base64 -w 0 < "${cert_dir}/ca.crt")
    KUBECONFIG="${kubeconfig}" ${_kubectl} patch mutatingwebhookconfiguration "${service_name}" \
        --type=json -p "[{\"op\":\"add\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${ca_bundle}\"}]"

    echo "Deploying iommufd-device-plugin DaemonSet..."
    curl -sL "https://raw.githubusercontent.com/kubevirt/iommufd-device-plugin/main/deploy/daemonset.yaml" | \
        sed "s|iommufd-device-plugin:latest|iommufd-device-plugin:${IOMMUFD_DEVICE_PLUGIN_TAG}|" | \
        KUBECONFIG="${kubeconfig}" ${_kubectl} apply -f -
    KUBECONFIG="${kubeconfig}" ${_kubectl} rollout status daemonset/iommufd-device-plugin \
        -n kube-system --timeout=2m

    echo "Labeling first worker node for node affinity tests..."
    local first_worker
    first_worker=$(KUBECONFIG="${kubeconfig}" ${_kubectl} get nodes \
        -l node-role.kubernetes.io/worker \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "${first_worker}" ]]; then
        KUBECONFIG="${kubeconfig}" ${_kubectl} label node "${first_worker}" \
            kubevirt-aie-webhook/node=true --overwrite
    else
        echo "WARNING: no worker node found, skipping node label"
    fi

    rm -f "${values_file}"
    rm -rf "${cert_dir}"

    echo "Webhook synced to cluster."
}

function kubevirtci::kubeconfig() {
    "${_cluster_up_dir}/kubeconfig.sh"
}

function kubevirtci::kubectl() {
    ${_kubectl} "$@"
}

kubevirtci::install

case ${_action} in
    "up")
        kubevirtci::up
        ;;
    "down")
        kubevirtci::down
        ;;
    "sync")
        kubevirtci::sync
        ;;
    "kubeconfig")
        kubevirtci::kubeconfig
        ;;
    "kubectl")
        kubevirtci::kubectl "$@"
        ;;
    *)
        echo "Unknown command '${_action}'. Known commands: up, down, sync, kubeconfig, kubectl"
        exit 1
        ;;
esac
