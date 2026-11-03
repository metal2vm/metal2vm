#!/usr/bin/env bash
# Creates a KubeVirt VM that plays a "physical server" for end-to-end tests:
# UEFI + Secure Boot + vTPM, a RHEL 9 root disk (virtio) and a blank data disk
# (SCSI), booted from the metal2vm live ISO of a P2VSource.
#
# Usage: hack/lab-source-vm.sh [create|delete] [name]   (namespace: $NS, default metal2vm-test)
set -euo pipefail

ACTION="${1:-create}"
NAME="${2:-lab-server01}"
NS="${NS:-metal2vm-test}"
OP_NS="${OP_NS:-metal2vm-system}"
OS_IMAGE="${OS_IMAGE:-rhel9}"

if [ "$ACTION" = delete ]; then
  kubectl -n "$NS" delete vm "$NAME" --ignore-not-found
  kubectl -n "$NS" delete p2vsource "$NAME" --ignore-not-found
  exit 0
fi

kubectl get ns "$NS" >/dev/null 2>&1 || {
  kubectl create ns "$NS"
  kubectl label ns "$NS" app.kubernetes.io/part-of=metal2vm
}

kubectl apply -f - <<EOF
apiVersion: migration.metal2vm.io/v1alpha1
kind: P2VSource
metadata:
  name: $NAME
  namespace: $NS
spec: {}
EOF

echo ">> waiting for boot media"
kubectl -n "$NS" wait p2vsource "$NAME" --for=condition=BootMediaReady --timeout=120s
ISO_URL="$(kubectl -n "$NS" get secret "$NAME-boot" -o jsonpath='{.data.isoURL}' | base64 -d)"

# CDI downloads the ISO and must trust the gateway CA.
kubectl -n "$NS" create configmap metal2vm-gateway-ca --dry-run=client -o yaml \
  --from-literal=ca.pem="$(kubectl -n "$OP_NS" get secret metal2vm-gateway-ca -o jsonpath='{.data.ca\.crt}' | base64 -d)" |
  kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $NAME
  namespace: $NS
  labels:
    app.kubernetes.io/part-of: metal2vm
    metal2vm.io/role: lab-physical-server
spec:
  runStrategy: Always
  dataVolumeTemplates:
  - metadata:
      name: $NAME-live-iso
    spec:
      source:
        http:
          url: "$ISO_URL"
          certConfigMap: metal2vm-gateway-ca
      storage:
        resources:
          requests:
            storage: 2Gi
  - metadata:
      name: $NAME-root
    spec:
      sourceRef:
        kind: DataSource
        name: $OS_IMAGE
        namespace: openshift-virtualization-os-images
      storage:
        resources:
          requests:
            storage: 30Gi
  - metadata:
      name: $NAME-data
    spec:
      source:
        blank: {}
      storage:
        resources:
          requests:
            storage: 5Gi
  template:
    metadata:
      labels:
        metal2vm.io/role: lab-physical-server
    spec:
      domain:
        cpu:
          sockets: 1
          cores: 2
          threads: 1
        memory:
          guest: 4Gi
        firmware:
          bootloader:
            efi:
              secureBoot: true
        features:
          smm:
            enabled: true
        devices:
          tpm: {}
          disks:
          - name: live
            cdrom:
              bus: sata
            bootOrder: 1
          - name: root
            serial: M2V-ROOT-01
            disk:
              bus: virtio
            bootOrder: 2
          - name: data
            serial: M2V-DATA-01
            disk:
              bus: scsi
          interfaces:
          - name: default
            masquerade: {}
      networks:
      - name: default
        pod: {}
      volumes:
      - name: live
        dataVolume:
          name: $NAME-live-iso
      - name: root
        dataVolume:
          name: $NAME-root
      - name: data
        dataVolume:
          name: $NAME-data
EOF
echo ">> VM $NS/$NAME created; it boots once the ISO import and root disk clone finish."
echo "   Watch: kubectl -n $NS get dv,vmi; kubectl -n $NS get p2vsource $NAME -w"
