#!/usr/bin/env bash
# Builds metal2vm images on an OpenShift cluster (BinaryBuild, docker strategy,
# pushed to the internal registry) and deploys the operator from config/default.
# For development clusters only; releases use the OLM bundle / Helm chart.
#
# Needs: oc (or bin/oc), kubectl, podman (for the kustomize toolbox), and a
# logged-in cluster-admin. Usage: hack/openshift-dev-deploy.sh [operator|transfer|boot|plugin|deploy|all]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS="${NS:-metal2vm-system}"
OC="${OC:-$(command -v oc || echo "$ROOT/bin/oc")}"
STEP="${1:-all}"
cd "$ROOT"

ensure_build() { # name dockerfile
  if ! "$OC" -n "$NS" get bc "$1" >/dev/null 2>&1; then
    "$OC" -n "$NS" new-build --binary --strategy=docker --name="$1" \
      --labels=app.kubernetes.io/part-of=metal2vm >/dev/null
  fi
  "$OC" -n "$NS" patch bc "$1" --type=merge \
    -p "{\"spec\":{\"strategy\":{\"dockerStrategy\":{\"dockerfilePath\":\"$2\"}}}}" >/dev/null
}

build() { # name dockerfile srcdir paths...
  local name=$1 dockerfile=$2 src=$3; shift 3
  ensure_build "$name" "$dockerfile"
  # Stage the build context; oc creates a portable archive from it (macOS tar
  # adds metadata the builder cannot extract).
  local ctx
  ctx="$(mktemp -d -t metal2vm-ctx)"
  (cd "$src" && cp -R "$@" "$ctx/")
  echo ">> building $name on the cluster ($(du -sh "$ctx" | cut -f1) context)"
  "$OC" -n "$NS" start-build "$name" --from-dir="$ctx" --follow --wait | tail -n 3
  rm -rf "$ctx"
}

image_ref() { # imagestream name -> internal pull spec by digest
  local digest
  digest="$("$OC" -n "$NS" get istag "$1:latest" -o jsonpath='{.image.metadata.name}')"
  echo "image-registry.openshift-image-registry.svc:5000/$NS/$1@$digest"
}

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS"
# Transfer pods run in users' namespaces and pull the (open source) images
# built into this namespace's ImageStreams.
"$OC" -n "$NS" policy add-role-to-group system:image-puller system:serviceaccounts >/dev/null

case "$STEP" in operator|all)
  VERSION="${VERSION:-dev-$(date -u +%Y%m%d%H%M%S)}"
  ensure_build metal2vm-operator Dockerfile
  # start-build --build-arg is ignored for binary builds; set it on the BuildConfig.
  "$OC" -n "$NS" patch bc metal2vm-operator --type=merge -p \
    "{\"spec\":{\"strategy\":{\"dockerStrategy\":{\"buildArgs\":[{\"name\":\"VERSION\",\"value\":\"$VERSION\"}]}}}}" >/dev/null
  build metal2vm-operator Dockerfile . Dockerfile .dockerignore go.mod go.sum LICENSE NOTICE api cmd internal ;;
esac
case "$STEP" in transfer|all)
  build metal2vm-transfer images/transfer/Containerfile . images go.mod go.sum LICENSE NOTICE api cmd internal ;;
esac
case "$STEP" in boot|all)
  build metal2vm-boot-artifacts Containerfile images/boot-artifacts Containerfile ;;
esac
case "$STEP" in plugin|all)
  ctx="$(mktemp -d -t metal2vm-plugin)"
  (cd console-plugin && tar --exclude=node_modules --exclude=dist -cf - .) | tar -xf - -C "$ctx"
  ensure_build metal2vm-console-plugin Dockerfile
  echo ">> building metal2vm-console-plugin on the cluster"
  "$OC" -n "$NS" start-build metal2vm-console-plugin --from-dir="$ctx" --follow --wait | tail -n 1
  rm -rf "$ctx"
  ;;
esac
case "$STEP" in deploy|all)
  op="$(image_ref metal2vm-operator)"
  bootimg="$(image_ref metal2vm-boot-artifacts)"
  xferimg="$(image_ref metal2vm-transfer)"
  pimg="$(image_ref metal2vm-console-plugin)"
  echo ">> deploying operator $op"
  work="$(mktemp -d "$ROOT/.deploy-XXXX")"
  trap 'rm -rf "$work"' EXIT
  # config/openshift = operator + console plugin; the release image names are
  # replaced with the images built on the cluster.
  cat > "$work/kustomization.yaml" <<EOF
resources:
- ../config/openshift
images:
- name: ghcr.io/metal2vm/metal2vm-operator
  newName: ${op%@*}
  digest: ${op#*@}
- name: ghcr.io/metal2vm/metal2vm-boot-artifacts
  newName: ${bootimg%@*}
  digest: ${bootimg#*@}
- name: ghcr.io/metal2vm/metal2vm-console-plugin
  newName: ${pimg%@*}
  digest: ${pimg#*@}
patches:
- target:
    kind: Deployment
    name: .*controller-manager
  patch: |
    - op: replace
      path: /spec/template/spec/containers/0/env/1/value
      value: $xferimg
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --enable-console-plugin
EOF
  hack/toolbox.sh bash -c "make kustomize >/dev/null && bin/kustomize build $(basename "$work")" | kubectl apply --server-side -f -
  kubectl -n "$NS" rollout status deploy/metal2vm-controller-manager --timeout=300s
  kubectl -n "$NS" rollout status deploy/metal2vm-console-plugin --timeout=180s ;;
esac
