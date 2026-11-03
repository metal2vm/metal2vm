# metal2vm — Physical-to-Virtual migration for OpenShift Virtualization

Status: **v0.3 approved. Decisions: custom agent, bundled virtio-win, neutral name, standalone now / Forklift-ready (option C)**  ·  License: Apache-2.0  ·  Name: **metal2vm** (domain is a placeholder)

## 1. Goals

A click-ops admin should be able to migrate a physical server to an OpenShift
Virtualization VM from the OpenShift web console. They should not need a
command line, conversion know-how, or help from a storage or network
specialist on the day of the migration.

### Requirements (from intake)

| Topic | Decision |
|---|---|
| Source OS | RHEL family, Ubuntu/Debian/SUSE, Windows Server |
| Downtime | Per server: **cold** (phase 1) or **warm** (phase 2) |
| Target | OCP **4.22+**, OpenShift Virtualization, ODF or any CSI that supports OCP-V |
| Source hardware | Local disks / HW RAID, SAN (FC/iSCSI, multipath), UEFI + Secure Boot, legacy BIOS |
| Source access | Redfish BMC virtual media, PXE, physical USB/console, OS-level only (SSH/WinRM) |
| Network | Routable LAN, firewalled segments, **disconnected** clusters |
| Scale | < 50 servers, one at a time (no wave planner needed) |
| SAN volumes | Admin chooses per disk: **copy** / **re-attach LUN** / **skip** |
| VM network identity | Admin chooses per VM: **keep IP/MAC** (bridged VLAN) or **new IP** |
| Cutover | Source is **shut down automatically**. Rollback = power the source back on |
| RBAC | Normal OpenShift RBAC on namespaces, with the cluster's OAuth/SSO |
| Stack | Go operator (kubebuilder) + OpenShift console dynamic plugin (React/PatternFly) |
| Delivery | OLM bundle (works with oc-mirror), Helm chart, public GitHub + container registry |
| First build | Cold path end-to-end |
| Forklift/MTV | **Standalone now, Forklift-ready (option C)**, see §10 |

### Non-goals (for now)
- Wave planning, scheduling, or bulk migration.
- Non-x86 sources, and OSes outside the list above.
- Migrating applications or changing what's inside the guest beyond what conversion needs.

## 2. Reuse existing open source; build only the glue

| Function | Existing OSS (used as a separate program or library) | License |
|---|---|---|
| Guest conversion (virtio drivers, bootloader, initramfs, fstab, Windows registry) | **virt-v2v / virt-v2v-in-place**, libguestfs | GPL/LGPL — runs as its own binary in its own container image |
| Windows drivers | **virtio-win** (bundled, ≥ 1.9.40) | Apache-2.0 / BSD-3 / GPL-2.0 (see §9) |
| Disk data copy | **nbdcopy** (libnbd) | LGPL — separate binary |
| Live boot environment | **Fedora CoreOS live ISO / PXE** + **Ignition**. The Ignition config is embedded the way `coreos-installer iso ignition embed` does it, implemented natively in Go and checked against coreos-installer | Fedora licenses |
| NBD server in the live environment | **nbdkit** (go-nbd was dropped: no release since 2023) | BSD-3-Clause |
| Reverse tunnel through firewalls (HTTPS/WebSocket over port 443) | **chisel** (Go library) | MIT |
| BMC control (virtual media, boot override, power) | **gofish** (Redfish Go library) | BSD-3 |
| Disk provisioning | **CDI** DataVolumes (blank) / PVCs | Apache-2.0 |
| VM runtime | **KubeVirt** / OpenShift Virtualization | Apache-2.0 |
| Operator framework | **kubebuilder / controller-runtime**, OLM | Apache-2.0 |
| Console UI | **openshift/console-plugin-template**, PatternFly | Apache-2.0 / MIT |

Glue we write ourselves, all Apache-2.0:
1. **p2v-operator**: CRDs and controllers that run the whole workflow.
2. **p2v-agent**: a static Go binary that runs in the live environment. It discovers inventory, exports disks read-only over NBD, and opens the tunnel.
3. **p2v-transfer**: per-migration pod logic that accepts the tunnel, runs nbdcopy into PVCs, then runs virt-v2v-in-place.
4. **p2v-console-plugin**: the click-ops wizard and dashboards.
5. Packaging: OLM bundle, Helm chart, oc-mirror ImageSetConfiguration, docs.

### Why not ship virt-p2v as-is?
virt-p2v is the classic tool. Its model is that the source opens an **inbound
SSH** connection to a conversion server, and that it has its own GUI. That
model works badly here for three reasons:
- **Firewalls:** with our design, the source only needs **outbound 443** to the cluster ingress.
- **Click-ops:** the source needs no console interaction. Pairing is automatic through Ignition-embedded tokens.
- **One data plane for cold and warm:** the same agent, tunnel, NBD export, and nbdcopy pipeline will be reused by the phase-2 warm agent running on the live OS.

We still use virt-v2v for the hard part, guest conversion, exactly as
Forklift/MTV does for warm migration with `virt-v2v-in-place`.

## 3. Architecture (cold path)

```
 Physical server (booted into the FCOS live ISO, running in RAM, source disks are only read)
 ┌──────────────────────────────────────────┐
 │ p2v-agent                                 │
 │  • inventory (CPU, RAM, disks, NICs, IP,  │
 │    firmware, Secure Boot, OS hints)       │
 │  • NBD export of each disk, read-only     │
 │  • chisel client ── wss:443 ─────────────┼──┐
 └──────────────────────────────────────────┘  │  outbound only
          ▲ Redfish (virtual media, power)      │
          │                                     ▼
 ┌────────┴──────────────── OpenShift cluster ─────────────────────────────┐
 │ p2v-operator ── serves Ignition / ISO / iPXE (Route)                     │
 │   │ reconciles P2VSource, P2VMigration                                   │
 │   ├─► transfer pod (per migration, in the target namespace)             │
 │   │     chisel server (Route, TLS passthrough) ◄──── tunnel             │
 │   │     nbdcopy nbd://… → /dev/<pvc> (block PVCs from CDI)              │
 │   │     virt-v2v-in-place (uses /dev/kvm from the KubeVirt device plugin)│
 │   └─► KubeVirt VirtualMachine (firmware/CPU/RAM/NIC/disks mapped)       │
 │ console plugin: wizard, progress, test boot, cutover, rollback          │
 └──────────────────────────────────────────────────────────────────────────┘
```

### 3.1 Workflow as the admin sees it
1. **Add source.** Enter a name and choose how to reach the server:
   - *Redfish BMC*: URL plus a credentials Secret.
   - *PXE*: the boot NIC's MAC address.
   - *Manual*: download a per-server ISO for USB or virtual media.
   - *Optional pre-check over SSH or WinRM while the server is still running.* This reads inventory and flags blockers (BitLocker, dynamic disks, unsupported OS) **before** any downtime is scheduled.
2. **Plan.** Choose the target namespace and VM name, and confirm the pre-filled CPU, RAM, and firmware. Then:
   - For **each disk**, choose copy (and a StorageClass), re-attach the existing LUN, or skip.
   - For **each NIC**, choose the pod network (new IP) or a NetworkAttachmentDefinition/localnet UDN, and whether to keep the MAC and keep the static IP.
3. **Pre-flight.** Automatic checks:
   - storage quota and capacity
   - the NAD exists
   - `/dev/kvm` is schedulable
   - the target namespace is writable by the user
   - re-attached LUNs are visible to the worker nodes
   - the virtio-win image is present when the guest is Windows
4. **Start migration** (downtime begins):
   - The operator boots the server into the live environment, through Redfish virtual media or PXE, or the admin boots the USB.
   - The agent phones home.
   - The inventory is re-checked against the plan.
   - Disks are copied, with progress per disk (bytes, MB/s, ETA).
5. **Convert.** virt-v2v-in-place runs against the copied PVCs. Its log streams to the UI.
6. **Test boot (optional).** The VM starts with **no production NIC**, so it cannot cause an IP conflict. The admin checks it in the embedded VNC/serial console.
7. **Cutover.** The operator:
   - powers off the source (Redfish, or a `poweroff` command to the agent)
   - ejects the virtual media
   - attaches the production NICs
   - starts the VM
8. **Rollback** (available until the admin clicks "Complete"). The operator:
   - stops the VM
   - sets a one-time boot from disk
   - powers the source back on

   Source disks are never written to, so rollback is always safe.

### 3.2 Booting the live environment
- **Image**: the stock Fedora CoreOS live ISO and PXE artifacts, shipped inside a `p2v-boot-artifacts` container image so they can be mirrored with oc-mirror for disconnected clusters. Fedora's shim is Microsoft-signed, so it **boots with Secure Boot**.
- **Per-source customization**: the gateway writes an Ignition config into the ISO's reserved embed area **while streaming it**. No per-source ISO is stored, and HTTP Range requests work, which BMC virtual media needs. The output is verified with `coreos-installer iso ignition show` and the config with `ignition-validate`. The config contains:
  - the gateway URL
  - a one-time token (expires; scoped to one migration)
  - the cluster CA
  - instructions to fetch the p2v-agent binary from the operator's HTTP endpoint
- **Redfish**: gofish calls `InsertMedia(ISO URL)`, sets a one-time boot to CD, then resets the server. Fallbacks for BMCs that cannot fetch over HTTPS are documented (HTTP-only URL option).
- **PXE**: the operator serves an iPXE script per MAC address plus the kernel, initramfs, and rootfs. The docs cover DHCP chainloading.
- **Manual**: a "Download ISO" button in the UI.

### 3.3 Data copy
- The agent exports each selected disk **read-only** over NBD (go-nbd). The NBD ports are reverse-forwarded over chisel (a WebSocket over the Route on 443, which works through firewalls and HTTP proxies). Optionally, the tunnel can go to a direct LoadBalancer/NodePort for throughput.
- The transfer pod runs `nbdcopy --destination-is-zero` into blank **block-mode** PVCs created through CDI. Zero blocks are skipped on write, which keeps thin storage thin.
- Sizes: PVC size = source disk size, rounded up to the StorageClass alignment.

### 3.4 Conversion
- The operator generates libvirt XML that describes the copied disks, then runs `virt-v2v-in-place -i libvirtxml`. Conversion:
  - installs the virtio drivers and the QEMU guest agent where possible
  - rebuilds the initramfs
  - fixes the bootloader and fstab
  - for Windows: injects virtio-win and sets up firstboot scripts
- **Network identity glue**, our own firstboot script. Source NIC config (MAC → IP/prefix/GW/DNS) is captured in the inventory.
  - Linux: the script rewrites NetworkManager keyfiles, ifcfg, or netplan config to bind by the new interface or MAC. Interface names change with PCI topology, so this is required.
  - Windows: uses virt-v2v's `--mac …:ip:…` support to re-apply static IPs to the new virtio NIC.

### 3.5 VM builder
Maps the inventory and plan to a KubeVirt `VirtualMachine`:

| VM setting | How it is set |
|---|---|
| Firmware | BIOS, or EFI (+ `secureBoot`, SMM) |
| TPM | persistent vTPM when the source had a TPM (Windows 11/2025) |
| CPU and memory | sockets/cores and memory as planned |
| Machine type | q35 |
| Disk bus | virtio, with a SATA fallback option |
| NICs | masquerade on the pod network, or bridge on a NAD with an optional fixed MAC |
| Windows | Hyper-V enlightenments |
| Labels | OS labels for the console |

### 3.6 SAN LUN re-attach
- For disks marked *re-attach*, the operator creates a **static block PV**: FC by WWN, or iSCSI by IQN/LUN. It binds that PV to a PVC in the target namespace and attaches it to the VM. No data is copied.
- The UI shows the required zoning/masking change, which the storage team must present to the worker nodes, and pre-flight verifies it.
- Risks are flagged: multipath software inside the guest (for example PowerPath) and boot-from-SAN, where the boot disk must still be converted in place.

### 3.7 Custom resources (namespaced, `metal2vm.io/v1alpha1`, the domain is a placeholder)
- **P2VSource**: access methods (BMC ref, PXE MAC, SSH/WinRM secret ref). `status.inventory` holds the discovered hardware and OS.
- **P2VMigration**: `sourceRef`, `mode: cold|warm`, `bootMethod`, `vm {name, cpu, memory, firmware}`, `disks[] {id, action: copy|attach|skip, storageClass}`, `nics[] {mac, network, keepMac, keepIp}`, `testBoot`, `autoShutdownSource`. `status` holds the phase, per-disk progress, conditions, and log references.
  - Phases: `Pending → Preflight → Booting → Connected → Copying → Converting → ReadyForTest → Testing → CuttingOver → Completed | RolledBack | Failed`.
- **P2VConfig** (cluster-scoped singleton): gateway exposure, image overrides (mirror registry), virtio-win image, default StorageClass.

### 3.8 Security and RBAC
- CRDs are namespaced. The VM, PVCs, and transfer pod are always created in the **same namespace** as the P2VMigration, so OpenShift RBAC limits each admin to their own projects.
- Aggregated ClusterRoles (`p2v-admin`, `p2v-view`) are added to the built-in `admin`/`edit`/`view` roles.
- Agent tokens are single-use, expire, and are scoped to one migration. The tunnel uses TLS with the cluster CA pinned.
- BMC, SSH, and WinRM credentials stay in Secrets in the user's namespace.
- The console plugin calls the API with the logged-in user's token, so there is no privilege escalation.

### 3.8.1 As built in M1
- The gateway runs inside the operator pod on port 9444, behind Service `metal2vm-gateway` (443) and a passthrough Route. TLS uses an operator-managed CA (`metal2vm-gateway-ca`, never rotated automatically) and a serving certificate renewed 30 days before expiry.
- Each P2VSource gets a Secret `<source>-boot` holding:
  - `bootKey`, which is part of the ISO URL
  - `token`, the agent's bearer token
  - `expiresAt` and `isoURL`

  Secrets are looked up through labels carrying SHA-256 hashes. Credentials rotate only after they expire. The gateway extends the expiry while the agent keeps sending heartbeats, so copies lasting several days keep working.
- The live environment masks lvm2-monitor, mdmonitor, multipathd and zincati. The agent sets BLKROSET on every disk and partition before it reads anything.

### 3.8.2 As built in M2: copying disks (disconnected-safe)
- **Disk export:** nbdkit isn't part of Fedora CoreOS, so the `boot-artifacts` image also carries a minimal Fedora 44 root filesystem with nbdkit (about 12 MB). The agent downloads it **from the gateway** and runs one read-only nbdkit per disk, listening only on loopback, with `podman run --rootfs`. The tar is unpacked through `os.Root`, so entries can't escape the target directory.
- **Tunnel:** each migration gets a transfer pod, Service, passthrough Route `m2v-<hash>.<apps domain>`, and a serving certificate from the gateway CA. The agent's chisel client connects out on port 443 and reverse-forwards one port per disk. The chisel **authfile only allows `R:127.0.0.1:<port>`** for the planned ports, so a leaked token can't open tunnels into the cluster network.
- **Copy:** `nbdcopy --flush` per disk into PVCs. The volume mode and access mode come from CDI's StorageProfile (RWX block preferred, then RWO block, then filesystem with `disk.img` plus 6% overhead). A copy is retried up to 3 times, since nbdcopy can't resume. Readiness means an NBD greeting, not a TCP connect, because chisel ports accept connections even when the far end is down. Progress is polled from the pod's `/status`, and the final result comes from its termination message.
- **Phases:**
  - **Preflight** reports every blocker at once, then retries every 15 seconds.
  - **Booting** waits for the live agent to connect.
  - **Connected** re-checks that the planned disks are present on the booted server.
  - **Copying**, then **Converting** (M3).
- **Disconnected clusters:**
  - Physical servers contact **only the gateway and their tunnel Route** (both port 443 on the ingress VIP).
  - The live environment never pulls from a registry.
  - The cluster pulls three images by digest: operator, transfer, boot-artifacts. The transfer image is passed as `RELATED_IMAGE_TRANSFER`, so OLM bundles and oc-mirror pick it up, and `P2VConfig.spec.images` can override it.
  - Only the image builds need internet access (Fedora and CentOS repos).

### 3.9 Required network flows (included in the docs)
| From | To | Port | Purpose |
|---|---|---|---|
| Source (live env) | Cluster ingress VIP | 443 | Ignition, agent binary, live tools, per-migration tunnel (`m2v-*.<apps domain>`) |
| Source (PXE) | Cluster ingress VIP | 80/443 | iPXE, kernel, initrd, rootfs |
| BMC | Cluster ingress VIP | 443 (or 80) | Virtual-media ISO fetch |
| Operator pod | BMC | 443 | Redfish |
| Operator pod | Source OS | 22 / 5986 | Optional pre-check (SSH/WinRM) |

## 4. Warm path (phase 2, outline)
- A **temporary agent** is installed on the running OS (pushed over SSH/WinRM from the UI, removed after migration). It reuses the same tunnel, NBD, and nbdcopy pipeline.
- **Linux**: snapshots plus changed-block tracking with **blksnap** (Veeam's open-source GPL kernel module, loaded only temporarily). Fallback is an LVM/dm snapshot with a hash-based delta rescan.
- **Windows**: a Go Windows service that takes **VSS** snapshots. Delta detection uses block hashing, because no OSS CBT driver exists.
- **Flow**: full sync → repeated incremental syncs → cutover (stop services, final sync, power off source) → virt-v2v-in-place → start VM.

## 5. Repository layout
```
api/v1alpha1/            CRD types
cmd/main.go (operator), cmd/{agent,transfer}/
internal/controller/     reconcilers
internal/bmc/            Redfish (gofish)
internal/boot/           Ignition generation, ISO embed, iPXE
internal/inventory/      inventory model + preflight rules
internal/vmbuilder/      KubeVirt VM spec
internal/netmap/         firstboot network re-mapping
console-plugin/          TS/React/PatternFly dynamic plugin
images/                  Containerfiles: operator, agent, transfer (virt-v2v), boot-artifacts
bundle/  charts/metal2vm/  hack/  docs/
LICENSE  NOTICE  THIRD_PARTY_LICENSES.md
```

## 6. Testing
- Unit tests and envtest for the controllers, VM builder, and preflight rules.
- **e2e without hardware**: a KubeVirt VM booted from the generated live ISO plays the "physical server", with a RHEL/Ubuntu/Windows disk attached. This tests the whole pipeline in kind+KubeVirt+CDI or on the lab cluster.
- Redfish is tested against **sushy-tools** (a Redfish emulator from OpenStack, Apache-2.0).
- Lab validation on OCP 4.22 when the cluster is available.

## 7. Milestones (cold path)
1. **M0**: scaffolding, CRDs, license files, CI (lint, test, image builds).
2. **M1**: agent (inventory, read-only NBD, tunnel), Ignition and ISO generation, manual ISO boot.
3. **M2**: transfer pod: block PVCs, nbdcopy, progress reporting.
4. **M3**: virt-v2v-in-place conversion, VM builder, test boot, cutover, rollback.
5. **M4**: Redfish virtual media and power, PXE/iPXE.
6. **M5**: console plugin wizard and dashboards.
7. **M6**: SAN re-attach, network-identity firstboot, SSH/WinRM pre-check.
8. **M7**: OLM bundle, Helm chart, oc-mirror config, disconnected-install docs.
9. **Phase 2**: warm agents for Linux and Windows.

## 8. Known risks and pre-flight blockers
- **BitLocker / LUKS**: conversion needs the volume unlocked, either suspended beforehand or keys supplied to virt-v2v (`--key`). Detected in the pre-check.
- **Windows dynamic disks, Storage Spaces, in-guest multipath software**: flagged as warnings.
- **UEFI boot entries are lost** with the source NVRAM: the guest relies on the `\EFI\BOOT\BOOTX64.EFI` fallback. virt-v2v and the test boot validate this.
- **Live-env driver coverage** for old RAID controllers (for example cciss): the Fedora kernel covers megaraid_sas, smartpqi, hpsa, mpt3sas, and others. Very old hardware falls back to an older live kernel or a manual path.
- **Windows activation and licensing** after a hardware change: covered in the docs, not by the tool.
- **Throughput through the router**: offer a direct LoadBalancer option (MetalLB) for large disks.

## 9. Licensing and IP (needs confirmation)
- All glue code is Apache-2.0. GPL/LGPL tools (virt-v2v, libguestfs, nbdcopy) run as **separate processes in separate images**, with no linking. Images built from Fedora/CentOS Stream packages ship source pointers in `THIRD_PARTY_LICENSES.md`.
- **virtio-win**: **bundled** in the transfer image (decision 2026-11-02). Build it from a pinned virtio-win RPM, version 1.9.40 or later.
  - Current packages declare `Apache-2.0 AND BSD-3-Clause AND GPL-2.0-only AND GPL-2.0-or-later`. Older RHEL-era RPMs also listed "Red Hat Proprietary", so **never use a pre-1.9.40 / CentOS 8 build**.
  - Ship the RPM's license files. Add a source pointer (the kvm-guest-drivers-windows commit) to `THIRD_PARTY_LICENSES.md` to meet the GPL-2.0 source offer.
  - `P2VConfig.virtioWinImage` can still override the bundled drivers.
- **Fedora CoreOS**: redistributed unmodified, and customized only through Ignition, which is the supported mechanism. Fedora trademark guidelines still need a check.
- **Trademarks**: the project uses a **neutral name** (decision 2026-11-02). "for OpenShift Virtualization" appears only as a description, and API groups and image names must not contain "openshift" or "ocp".

## 10. Relationship to Forklift / MTV (option C)

As of 2026-11-02, Forklift/MTV has no physical source provider and no public
roadmap item for one. Forklift's providers are openshift, vsphere, ovirt,
openstack, ova, hyperv, nutanix and azure.

**Decision:** ship metal2vm standalone, but keep a clean boundary so that a
Forklift `physical` provider adapter can be added later, upstream or as a fork,
without rewriting anything.

Design rules that follow from this:
1. **Provider-shaped inventory API.** p2v-operator serves a read-only REST
   inventory (`/providers/physical/{ns}/{source}/vms|disks|networks`) that
   mirrors the shape of the Forklift inventory service. A future Forklift
   adapter's `Client`/`Builder` can then use it like any other provider.
2. **Separate data plane.** The agent, tunnel and nbdcopy pipeline expose a
   stable contract: "give me NBD URLs for disks X, Y". A Forklift adapter can
   then drive the same transfer, then run virt-v2v-in-place the way Forklift's
   warm vSphere path already does.
3. **Reuse Forklift vocabulary** wherever it fits: NetworkMap/StorageMap-like
   per-NIC and per-disk mapping fields, plan/migration phase names, hooks. This
   keeps a later mapping into Forklift CRs mechanical.
4. **No Forklift runtime dependency** in phase 1: metal2vm installs and runs
   on clusters without MTV.
5. **Later:** propose an upstream enhancement on kubev2v/forklift. This is
   public and needs the owner's go-ahead.
