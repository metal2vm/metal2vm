#!/usr/bin/env bash
# Runs a command inside the pinned Go toolchain container, so contributors only
# need podman (or docker). Usage: hack/toolbox.sh make test
set -euo pipefail
ENGINE="${CONTAINER_TOOL:-$(command -v podman || command -v docker)}"
IMAGE="${TOOLBOX_IMAGE:-docker.io/library/golang:1.27}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec "$ENGINE" run --rm -i $([ -t 0 ] && echo -t) \
  -v "$ROOT":/src:Z -v metal2vm-gomod:/go/pkg/mod -v metal2vm-gocache:/root/.cache \
  -w /src -e GOFLAGS -e GOOS -e GOARCH "$IMAGE" "$@"
