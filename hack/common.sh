#!/usr/bin/env bash

source hack/container-engine.sh

DOCKER_PREFIX=${DOCKER_PREFIX:-quay.io/kubevirt}
IMAGE_NAME=${IMAGE_NAME:-kubevirt-aie-webhook}
BUILD_ARCH=${BUILD_ARCH:-amd64,arm64}

SHA_TAG="$(date +%Y%m%d)-$(git rev-parse --short HEAD)"
IMAGE="${DOCKER_PREFIX}/${IMAGE_NAME}"
