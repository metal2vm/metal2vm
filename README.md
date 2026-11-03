# metal2vm

Migrate physical servers to virtual machines on OpenShift Virtualization /
KubeVirt, from the OpenShift web console.

> **Status: early development (milestone M1).** A physical server can be
> booted into the metal2vm live environment from a per-server ISO, and its
> hardware inventory appears in its `P2VSource`. Nothing migrates yet. See
> [docs/DESIGN.md](docs/DESIGN.md) for the design and milestones.

## How it will work (cold migration)

1. Register a physical server (`P2VSource`) with its Redfish BMC, PXE MAC, or
   nothing at all (manual USB boot).
2. Plan the migration (`P2VMigration`): target namespace, CPU and memory, a
   copy, attach-LUN or skip action per disk, and a network mapping per NIC.
3. metal2vm boots the server into a Fedora CoreOS live environment. A small
   agent there exposes the disks read-only over an outbound HTTPS tunnel.
4. Disks are copied into PVCs with `nbdcopy` and converted with
   `virt-v2v-in-place` (virtio drivers, bootloader, network config).
5. The VM is test-booted without production networking. At cutover the source
   is powered off and the VM goes live. Until the migration is archived,
   rollback powers the source back on.

## Repository layout

| Path | Contents |
|---|---|
| `api/v1alpha1/` | CRDs: `P2VSource`, `P2VMigration`, `P2VConfig` (`migration.metal2vm.io`) |
| `cmd/main.go`, `internal/controller/` | Operator (kubebuilder / controller-runtime) |
| `console-plugin/` | OpenShift console dynamic plugin (React, PatternFly) |
| `config/` | Kustomize manifests (CRDs, RBAC, manager) |
| `docs/` | Design and user documentation |
| `hack/` | Developer scripts |

## Development

The only thing you need on your machine is **podman** (or docker). The Go and
Node toolchains run in pinned containers:

```sh
hack/toolbox.sh make test             # generate, vet, unit + envtest (Go 1.27)
hack/toolbox.sh make build            # build the operator binary
make plugin-test plugin-lint          # console plugin tests and lint (Node 22)
make plugin-build                     # console plugin production build
```

If Go is installed locally, the usual `make test` / `make build` work as well.

### Trying it on an OpenShift cluster

```sh
hack/openshift-dev-deploy.sh          # build images on the cluster and deploy the operator
hack/lab-source-vm.sh create          # a KubeVirt VM that plays a physical server
kubectl -n metal2vm-test get p2vsource lab-server01 -o yaml   # inventory appears under status
```

For a real server, create a `P2VSource`, then boot the server from the ISO
URL in the Secret `<name>-boot` (key `isoURL`). Use USB or the BMC's virtual
media.

## Requirements (target)

- OpenShift 4.22+ with OpenShift Virtualization, or Kubernetes with KubeVirt + CDI
- Any CSI storage class that supports block-mode PVCs for VMs (e.g. ODF)

## License

Apache License 2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).

metal2vm is an independent project and is not affiliated with or endorsed by
Red Hat or the CNCF. "OpenShift" is a trademark of Red Hat, Inc.
