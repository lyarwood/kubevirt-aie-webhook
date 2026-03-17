#!/usr/bin/env bash

set -e

source hack/common.sh

for arch in ${BUILD_ARCH//,/ }; do
    echo "[INFO] Building for linux/${arch}"
    ${CONTAINER_ENGINE} build --platform "linux/${arch}" \
        --build-arg TARGETOS=linux --build-arg TARGETARCH="${arch}" \
        -t "${IMAGE}:${SHA_TAG}-linux-${arch}" .
done
