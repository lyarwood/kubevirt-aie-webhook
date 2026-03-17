#!/bin/bash

_detect_container_engine() {
    if [ -n "${KUBEVIRT_CRI}" ]; then
        echo "${KUBEVIRT_CRI}"
    elif podman ps >/dev/null 2>&1; then
        echo podman
    elif docker ps >/dev/null 2>&1; then
        echo docker
    fi
}

# When sourced, export CONTAINER_ENGINE. When executed directly, print to stdout.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    _detect_container_engine
else
    export CONTAINER_ENGINE=${CONTAINER_ENGINE:-$(_detect_container_engine)}
fi
