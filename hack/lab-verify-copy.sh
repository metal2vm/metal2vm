#!/usr/bin/env bash
# Verifies a lab migration's copy: compares the SHA-256 of each source disk PVC
# (of the lab "physical server" VM) with its copied PVC, up to the source size.
#
# Usage: hack/lab-verify-copy.sh <migration> <source-pvc>=<target-pvc> ...   (namespace: $NS)
# Example: hack/lab-verify-copy.sh lab-server01 lab-server01-root=lab-server01-disk-0
set -euo pipefail
NS="${NS:-metal2vm-test}"
MIG="$1"; shift
POD="$MIG-verify"

devices="" volumes="" script="set -e; rc=0; "
i=0
for pair in "$@"; do
  src="${pair%%=*}"; dst="${pair#*=}"
  size="$(kubectl -n "$NS" get p2vmigration "$MIG" -o json |
    python3 -c "import sys,json; d=json.load(sys.stdin); print(next(x['bytesTotal'] for x in d['status']['disks'] if x.get('pvcName')=='$dst'))")"
  devices+="    - {name: s$i, devicePath: /dev/s$i}
    - {name: d$i, devicePath: /dev/d$i}
"
  volumes+="  - {name: s$i, persistentVolumeClaim: {claimName: $src, readOnly: true}}
  - {name: d$i, persistentVolumeClaim: {claimName: $dst, readOnly: true}}
"
  script+="a=\$(head -c $size /dev/s$i | sha256sum | cut -d' ' -f1); b=\$(head -c $size /dev/d$i | sha256sum | cut -d' ' -f1); "
  script+="if [ \$a = \$b ]; then echo \"MATCH    $src -> $dst ($size bytes) \$a\"; else echo \"MISMATCH $src -> $dst: \$a != \$b\"; rc=1; fi; "
  i=$((i+1))
done
script+="exit \$rc"

kubectl -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $POD
  namespace: $NS
  labels: {app.kubernetes.io/part-of: metal2vm}
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: verify
    image: registry.access.redhat.com/ubi9/ubi-minimal
    command: [sh, -c]
    args:
    - |
      $script
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeDevices:
$devices  volumes:
$volumes
EOF
kubectl -n "$NS" wait pod "$POD" --for=jsonpath='{.status.phase}'=Succeeded --timeout=30m ||
  kubectl -n "$NS" wait pod "$POD" --for=jsonpath='{.status.phase}'=Failed --timeout=10s || true
kubectl -n "$NS" logs "$POD"
phase="$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.phase}')"
kubectl -n "$NS" delete pod "$POD" >/dev/null
[ "$phase" = Succeeded ]
