#!/usr/bin/env bash
# Creates a KubeVirt VM that plays a "physical server" for end-to-end tests.
#
# Like a real server, it first runs its own OS (RHEL via cloud-init, or Windows
# via sysprep) so the OS is configured and "lived in". Then it is rebooted into
# the metal2vm live ISO, as an admin would do before a migration. Devices are
# physical-like: SATA disks and an Intel e1000e NIC, so conversion has to
# install virtio drivers for real.
#
# Usage: hack/lab-source-vm.sh create <name> [rhel9|win2k22]
#        hack/lab-source-vm.sh delete <name>
#        hack/lab-source-vm.sh boot-disk <name>   # boot the server's own OS again (rollback)
#        hack/lab-source-vm.sh boot-live <name>   # reboot into the metal2vm live ISO
# Namespace: $NS (default metal2vm-test). Credentials: Secret <name>-credentials.
set -euo pipefail

ACTION="${1:?action}"
NAME="${2:?name}"
OS="${3:-rhel9}"
NS="${NS:-metal2vm-test}"
OP_NS="${OP_NS:-metal2vm-system}"

vm_patch() { kubectl -n "$NS" patch vm "$NAME" --type=json -p "$1" >/dev/null; }
# kubectl wait fails if the object does not exist yet (VMIs appear once disks are ready).
wait_exists() { for _ in $(seq 1 360); do kubectl -n "$NS" get "$1" >/dev/null 2>&1 && return 0; sleep 5; done; return 1; }

case "$ACTION" in
delete)
  kubectl -n "$NS" delete vm "$NAME" --ignore-not-found
  kubectl -n "$NS" delete p2vsource "$NAME" --ignore-not-found
  kubectl -n "$NS" delete secret "$NAME-credentials" --ignore-not-found
  kubectl -n "$NS" delete configmap "$NAME-sysprep" --ignore-not-found
  exit 0 ;;
boot-disk|boot-live)
  first=root; [ "$ACTION" = boot-live ] && first=live
  kubectl -n "$NS" get vm "$NAME" -o json | FIRST="$first" python3 -c '
import sys, json, os
vm = json.load(sys.stdin)
ops = []
first = os.environ["FIRST"]
for i, d in enumerate(vm["spec"]["template"]["spec"]["domain"]["devices"]["disks"]):
    order = 1 if d["name"] == first else (2 if d["name"] in ("live", "root") else None)
    if order: ops.append({"op": "replace", "path": f"/spec/template/spec/domain/devices/disks/{i}/bootOrder", "value": order})
print(json.dumps(ops))' > /tmp/m2v-bootdisk-$NAME.json
  vm_patch "$(cat /tmp/m2v-bootdisk-$NAME.json)"
  virtctl_bin="$(command -v virtctl || echo bin/virtctl)"
  "$virtctl_bin" -n "$NS" start "$NAME" 2>/dev/null || "$virtctl_bin" -n "$NS" restart "$NAME"
  echo ">> $NAME boots from its $first disk"
  exit 0 ;;
create) ;;
*) echo "unknown action $ACTION" >&2; exit 2 ;;
esac

case "$OS" in
rhel9) PREF=rhel.9; MEM=4Gi; ROOT=30Gi ;;
win2k22) PREF=windows.2k22; MEM=6Gi; ROOT=30Gi ;;
*) echo "unsupported OS $OS" >&2; exit 2 ;;
esac

kubectl get ns "$NS" >/dev/null 2>&1 || {
  kubectl create ns "$NS"
  kubectl label ns "$NS" app.kubernetes.io/part-of=metal2vm
}

# Lab-only credentials, random per server.
if ! kubectl -n "$NS" get secret "$NAME-credentials" >/dev/null 2>&1; then
  PASSWORD="M2v-$(python3 -c 'import secrets; print(secrets.token_urlsafe(12).replace("-", "x").replace("_", "y"))')!"
  USERNAME=$([ "$OS" = win2k22 ] && echo Administrator || echo cloud-user)
  kubectl -n "$NS" create secret generic "$NAME-credentials" \
    --from-literal=username="$USERNAME" --from-literal=password="$PASSWORD" >/dev/null
fi
PASSWORD="$(kubectl -n "$NS" get secret "$NAME-credentials" -o jsonpath='{.data.password}' | base64 -d)"

kubectl apply -f - <<EOF >/dev/null
apiVersion: migration.metal2vm.io/v1alpha1
kind: P2VSource
metadata:
  name: $NAME
  namespace: $NS
spec: {}
EOF
kubectl -n "$NS" wait p2vsource "$NAME" --for=condition=BootMediaReady --timeout=120s >/dev/null
ISO_URL="$(kubectl -n "$NS" get secret "$NAME-boot" -o jsonpath='{.data.isoURL}' | base64 -d)"
kubectl -n "$NS" create configmap metal2vm-gateway-ca --dry-run=client -o yaml \
  --from-literal=ca.pem="$(kubectl -n "$OP_NS" get secret metal2vm-gateway-ca -o jsonpath='{.data.ca\.crt}' | base64 -d)" |
  kubectl apply -f - >/dev/null

# First-boot configuration: sysprep answer file (Windows) or cloud-init (RHEL).
if [ "$OS" = win2k22 ]; then
  kubectl -n "$NS" create configmap "$NAME-sysprep" --dry-run=client -o yaml --from-file=autounattend.xml=/dev/stdin <<XML | kubectl apply -f - >/dev/null
<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE><HideEULAPage>true</HideEULAPage><SkipMachineOOBE>true</SkipMachineOOBE><SkipUserOOBE>true</SkipUserOOBE><ProtectYourPC>3</ProtectYourPC></OOBE>
      <UserAccounts><AdministratorPassword><Value>$PASSWORD</Value><PlainText>true</PlainText></AdministratorPassword></UserAccounts>
      <ComputerName>$NAME</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
</unattend>
XML
fi

NAME="$NAME" NS="$NS" OS="$OS" PREF="$PREF" MEM="$MEM" ROOT="$ROOT" ISO_URL="$ISO_URL" PASSWORD="$PASSWORD" python3 - <<'PY' | kubectl apply -f - >/dev/null
import json, os
e = os.environ
name, os_ = e["NAME"], e["OS"]
disks = [
    {"name": "root", "serial": "M2V-ROOT-01", "disk": {"bus": "sata"}, "bootOrder": 1},
    {"name": "data", "serial": "M2V-DATA-01", "disk": {"bus": "sata"}},
    {"name": "live", "cdrom": {"bus": "sata"}, "bootOrder": 2},
]
volumes = [
    {"name": "root", "dataVolume": {"name": f"{name}-root"}},
    {"name": "data", "dataVolume": {"name": f"{name}-data"}},
    {"name": "live", "dataVolume": {"name": f"{name}-live-iso"}},
]
if os_ == "win2k22":
    disks.append({"name": "firstboot", "cdrom": {"bus": "sata"}})
    volumes.append({"name": "firstboot", "sysprep": {"configMap": {"name": f"{name}-sysprep"}}})
else:
    user_data = "\n".join([
        "#cloud-config", f"hostname: {name}", "user: cloud-user",
        f"password: '{e['PASSWORD']}'", "chpasswd: {expire: false}", "ssh_pwauth: true", ""])
    disks.append({"name": "firstboot", "disk": {"bus": "virtio"}})
    volumes.append({"name": "firstboot", "cloudInitNoCloud": {"userData": user_data}})
vm = {
    "apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
    "metadata": {"name": name, "namespace": e["NS"],
                 "labels": {"app.kubernetes.io/part-of": "metal2vm", "metal2vm.io/role": "lab-physical-server"}},
    "spec": {
        "runStrategy": "Always",
        "preference": {"kind": "VirtualMachineClusterPreference", "name": e["PREF"]},
        "dataVolumeTemplates": [
            {"metadata": {"name": f"{name}-live-iso"}, "spec": {
                "source": {"http": {"url": e["ISO_URL"], "certConfigMap": "metal2vm-gateway-ca"}},
                "storage": {"resources": {"requests": {"storage": "2Gi"}}}}},
            {"metadata": {"name": f"{name}-root"}, "spec": {
                "sourceRef": {"kind": "DataSource", "name": os_, "namespace": "openshift-virtualization-os-images"},
                "storage": {"resources": {"requests": {"storage": e["ROOT"]}}}}},
            {"metadata": {"name": f"{name}-data"}, "spec": {
                "source": {"blank": {}}, "storage": {"resources": {"requests": {"storage": "5Gi"}}}}},
        ],
        "template": {
            "metadata": {"labels": {"metal2vm.io/role": "lab-physical-server"}},
            "spec": {
                "domain": {
                    "cpu": {"sockets": 1, "cores": 2, "threads": 1},
                    "memory": {"guest": e["MEM"]},
                    "firmware": {"bootloader": {"efi": {"secureBoot": True}}},
                    "features": {"smm": {"enabled": True}},
                    "devices": {"tpm": {}, "disks": disks,
                                "interfaces": [{"name": "default", "model": "e1000e", "masquerade": {}}]},
                },
                "networks": [{"name": "default", "pod": {}}],
                "volumes": volumes,
            },
        },
    },
}
print(json.dumps(vm))
PY
echo ">> $NS/$NAME ($OS) is booting its own OS for first-boot configuration"
wait_exists "vmi/$NAME"
if [ "$OS" = win2k22 ]; then
  # Physical-like Windows images have no guest agent: allow unattended setup
  # time instead (specialize + OOBE + first logon screen).
  kubectl -n "$NS" wait vmi "$NAME" --for=jsonpath='{.status.phase}'=Running --timeout=30m >/dev/null
  kubectl -n "$NS" wait vmi "$NAME" --for=condition=AgentConnected --timeout=8m >/dev/null 2>&1 ||
    echo ">> no guest agent in this image (as on a physical server); assuming setup finished"
else
  kubectl -n "$NS" wait vmi "$NAME" --for=condition=AgentConnected --timeout=30m >/dev/null
fi
# The agent can connect before first-boot configuration has brought the network up.
for _ in $(seq 1 120); do
  [ -n "$(kubectl -n "$NS" get vmi "$NAME" -o jsonpath='{.status.interfaces[0].ipAddress}')" ] && break
  sleep 5
done
echo ">> guest agent connected: $(kubectl -n "$NS" get vmi "$NAME" -o jsonpath='{.status.guestOSInfo.prettyName}'), IP $(kubectl -n "$NS" get vmi "$NAME" -o jsonpath='{.status.interfaces[0].ipAddress}')"

# Reboot into the live ISO without the first-boot disk, like an admin would.
virtctl_bin="$(command -v virtctl || echo bin/virtctl)"
"$virtctl_bin" -n "$NS" stop "$NAME" >/dev/null
kubectl -n "$NS" wait vmi "$NAME" --for=delete --timeout=10m >/dev/null 2>&1 || true
kubectl -n "$NS" get vm "$NAME" -o json | python3 -c '
import sys, json
vm = json.load(sys.stdin)
spec = vm["spec"]["template"]["spec"]
disks = [d for d in spec["domain"]["devices"]["disks"] if d["name"] != "firstboot"]
for d in disks:
    if d["name"] == "live": d["bootOrder"] = 1
    elif d["name"] == "root": d["bootOrder"] = 2
vols = [v for v in spec["volumes"] if v["name"] != "firstboot"]
print(json.dumps([
  {"op": "replace", "path": "/spec/template/spec/domain/devices/disks", "value": disks},
  {"op": "replace", "path": "/spec/template/spec/volumes", "value": vols},
  {"op": "replace", "path": "/spec/runStrategy", "value": "RerunOnFailure"}]))' > /tmp/m2v-tolive-$NAME.json
# Switching runStrategy from Halted to RerunOnFailure usually starts the VM,
# but not if a graceful shutdown completed afterwards; start it explicitly.
vm_patch "$(cat /tmp/m2v-tolive-$NAME.json)"
"$virtctl_bin" -n "$NS" start "$NAME" >/dev/null 2>&1 || true
echo ">> $NAME is booting the metal2vm live ISO (runStrategy RerunOnFailure: stays off after poweroff)"
kubectl -n "$NS" wait p2vsource "$NAME" --for=jsonpath='{.status.inventory.collectedBy}'=LiveAgent --timeout=15m >/dev/null
kubectl -n "$NS" wait p2vsource "$NAME" --for=jsonpath='{.status.agent.connected}'=true --timeout=5m >/dev/null
echo ">> live agent connected; inventory:"
kubectl -n "$NS" get p2vsource "$NAME" -o jsonpath='{range .status.inventory.disks[*]}   disk {.id} {.transport} {.sizeBytes}{"\n"}{end}{range .status.inventory.nics[*]}   nic {.name} {.mac}{"\n"}{end}'
