#!/usr/bin/env bash

set -e

source hack/common.sh

for arch in ${BUILD_ARCH//,/ }; do
    echo "[INFO] Pushing ${IMAGE}:${SHA_TAG}-linux-${arch}"
    ${CONTAINER_ENGINE} push "${IMAGE}:${SHA_TAG}-linux-${arch}"
done
