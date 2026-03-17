#!/usr/bin/env bash

set -e

source hack/common.sh

# Collect arch-specific image references
MANIFEST_IMAGES=""
for arch in ${BUILD_ARCH//,/ }; do
    MANIFEST_IMAGES="${MANIFEST_IMAGES} ${IMAGE}:${SHA_TAG}-linux-${arch}"
done

# Create and push the SHA-tagged manifest
echo "[INFO] Creating manifest ${IMAGE}:${SHA_TAG}"
${CONTAINER_ENGINE} manifest rm "${IMAGE}:${SHA_TAG}" 2>/dev/null || true
${CONTAINER_ENGINE} manifest create "${IMAGE}:${SHA_TAG}" ${MANIFEST_IMAGES}
echo "[INFO] Pushing manifest ${IMAGE}:${SHA_TAG}"
${CONTAINER_ENGINE} manifest push "${IMAGE}:${SHA_TAG}"

# If DOCKER_TAG is set, also create and push a manifest with that tag
if [ -n "${DOCKER_TAG}" ]; then
    echo "[INFO] Creating manifest ${IMAGE}:${DOCKER_TAG}"
    ${CONTAINER_ENGINE} manifest rm "${IMAGE}:${DOCKER_TAG}" 2>/dev/null || true
    ${CONTAINER_ENGINE} manifest create "${IMAGE}:${DOCKER_TAG}" ${MANIFEST_IMAGES}
    echo "[INFO] Pushing manifest ${IMAGE}:${DOCKER_TAG}"
    ${CONTAINER_ENGINE} manifest push "${IMAGE}:${DOCKER_TAG}"
fi
