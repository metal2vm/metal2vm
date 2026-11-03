#!/usr/bin/env bash
# Deploys the LAB Redfish emulator for KubeVirt VMs (test/lab/kubevirt-redfish)
# into $NS and prints the BMC address/credentials to use in P2VSource.spec.bmc.
# Usage: hack/lab-redfish.sh [deploy|delete]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS="${NS:-metal2vm-test}"; BUILD_NS="${BUILD_NS:-metal2vm-system}"
OC="${OC:-$(command -v oc || echo "$ROOT/bin/oc")}"
cd "$ROOT"

if [ "${1:-deploy}" = delete ]; then
  kubectl -n "$NS" delete deploy,svc,sa,role,rolebinding -l app=lab-redfish --ignore-not-found
  kubectl -n "$NS" delete secret lab-redfish-credentials --ignore-not-found
  exit 0
fi

if ! "$OC" -n "$BUILD_NS" get bc lab-redfish >/dev/null 2>&1; then
  "$OC" -n "$BUILD_NS" new-build --binary --strategy=docker --name=lab-redfish >/dev/null
fi
"$OC" -n "$BUILD_NS" patch bc lab-redfish --type=merge \
  -p '{"spec":{"strategy":{"dockerStrategy":{"dockerfilePath":"test/lab/kubevirt-redfish/Containerfile"}}}}' >/dev/null
ctx="$(mktemp -d -t lab-redfish)"; trap 'rm -rf "$ctx"' EXIT
mkdir -p "$ctx/test/lab"
cp -R go.mod go.sum api internal "$ctx/"; cp -R test/lab/kubevirt-redfish "$ctx/test/lab/"
"$OC" -n "$BUILD_NS" start-build lab-redfish --from-dir="$ctx" --follow --wait | tail -n 1
IMG="$("$OC" -n "$BUILD_NS" get istag lab-redfish:latest -o jsonpath='{.image.dockerImageReference}')"

if ! kubectl -n "$NS" get secret lab-redfish-credentials >/dev/null 2>&1; then
  kubectl -n "$NS" create secret generic lab-redfish-credentials --from-literal=username=admin \
    --from-literal=password="$(python3 -c 'import secrets; print(secrets.token_urlsafe(18))')" >/dev/null
  kubectl -n "$NS" label secret lab-redfish-credentials app=lab-redfish >/dev/null
fi
kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: lab-redfish, namespace: $NS, labels: {app: lab-redfish}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: lab-redfish, namespace: $NS, labels: {app: lab-redfish}}
rules:
- apiGroups: [kubevirt.io]
  resources: [virtualmachines, virtualmachineinstances]
  verbs: [get, list, watch, patch]
- apiGroups: [subresources.kubevirt.io]
  resources: [virtualmachines/start, virtualmachines/stop, virtualmachines/restart]
  verbs: [update]
- apiGroups: [cdi.kubevirt.io]
  resources: [datavolumes]
  verbs: [get, list, create, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: lab-redfish, namespace: $NS, labels: {app: lab-redfish}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: lab-redfish}
subjects: [{kind: ServiceAccount, name: lab-redfish, namespace: $NS}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: lab-redfish, namespace: $NS, labels: {app: lab-redfish}}
spec:
  replicas: 1
  selector: {matchLabels: {app: lab-redfish}}
  template:
    metadata: {labels: {app: lab-redfish}}
    spec:
      serviceAccountName: lab-redfish
      securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: redfish
        image: $IMG
        args: [--namespace=$NS]
        env:
        - {name: REDFISH_USERNAME, valueFrom: {secretKeyRef: {name: lab-redfish-credentials, key: username}}}
        - {name: REDFISH_PASSWORD, valueFrom: {secretKeyRef: {name: lab-redfish-credentials, key: password}}}
        ports: [{containerPort: 8000, name: redfish}]
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: lab-redfish, namespace: $NS, labels: {app: lab-redfish}}
spec:
  selector: {app: lab-redfish}
  ports: [{name: redfish, port: 8000, targetPort: redfish}]
YAML
kubectl -n "$NS" rollout status deploy/lab-redfish --timeout=180s >/dev/null
echo ">> BMC address: http://lab-redfish.$NS.svc:8000  credentials Secret: lab-redfish-credentials"
echo ">> systems: VMs labelled metal2vm.io/role=lab-physical-server (systemID = VM name)"
