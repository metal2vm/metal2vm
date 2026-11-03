#!/usr/bin/env bash
# Runs a command in the console-plugin directory inside a pinned Node container.
# Usage: hack/node-toolbox.sh yarn test
set -euo pipefail
ENGINE="${CONTAINER_TOOL:-$(command -v podman || command -v docker)}"
IMAGE="${NODE_TOOLBOX_IMAGE:-docker.io/library/node:22}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
exec "$ENGINE" run --rm -i $([ -t 0 ] && echo -t) \
  -v "$ROOT/console-plugin":/src:Z -v metal2vm-yarncache:/root/.yarn/berry \
  -w /src -e PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 "$IMAGE" "$@"
