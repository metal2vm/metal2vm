# metal2vm on disconnected clusters

metal2vm needs no internet access at runtime:

| Needed during a migration | Where it comes from |
|---|---|
| Live environment (Fedora CoreOS ISO, PXE kernel/initramfs/rootfs) | Inside the `boot-artifacts` image; served by the operator's gateway |
| Agent binary, Ignition config | Built into the operator / generated per server |
| Live tools (nbdkit etc.) | Inside the `boot-artifacts` image |
| virt-v2v, libguestfs, virtio-win drivers | Inside the `transfer` image (virtio-win pinned by SHA-256) |
| Console plugin | Inside the `console-plugin` image |

So a disconnected install only means mirroring four images, plus the catalog for
an OperatorHub install.

## Images

| Image | Used by |
|---|---|
| `ghcr.io/metal2vm/metal2vm-operator:vX.Y.Z` | Operator Deployment |
| `ghcr.io/metal2vm/metal2vm-transfer:vX.Y.Z` | Transfer and conversion pods, in the migration's namespace |
| `ghcr.io/metal2vm/metal2vm-boot-artifacts:vX.Y.Z` | Init container of the operator (copies the ISO and PXE files) |
| `ghcr.io/metal2vm/metal2vm-console-plugin:vX.Y.Z` | Console plugin Deployment |
| `ghcr.io/metal2vm/metal2vm-operator-bundle:vX.Y.Z` | OLM only |
| `ghcr.io/metal2vm/metal2vm-operator-catalog:vX.Y.Z` | OLM only (CatalogSource) |

All four runtime images are listed as `relatedImages` in the bundle, so
`oc-mirror` finds them from the catalog alone.

## OperatorHub (OLM) install

1. **On a connected machine,** mirror to disk with
   [`oc-mirror/imageset-olm.yaml`](oc-mirror/imageset-olm.yaml):
   ```sh
   oc mirror -c imageset-olm.yaml file:///data/metal2vm-mirror --v2
   ```
2. **Carry the archive across.** On the disconnected side, push it to your mirror registry:
   ```sh
   oc mirror -c imageset-olm.yaml --from file:///data/metal2vm-mirror \
     docker://mirror.example.com:8443 --v2
   ```
3. **Apply the cluster resources** oc-mirror wrote under
   `working-dir/cluster-resources/`: the CatalogSource, and the
   ImageDigestMirrorSet/ImageTagMirrorSet that redirect `ghcr.io/metal2vm` to
   your mirror.
4. **Install from OperatorHub** as usual; see [install.md](install.md#operatorhub-olm).
   The image references stay unchanged; the mirror sets redirect the pulls.

## Helm or YAML install

1. Mirror the images with
   [`oc-mirror/imageset-images.yaml`](oc-mirror/imageset-images.yaml), using the
   same two `oc mirror` steps. Then apply the generated mirror sets.
2. Copy the chart or the YAML file across:
   - chart: `helm pull oci://ghcr.io/metal2vm/charts/metal2vm --version X.Y.Z`;
   - YAML: `install.yaml` from the release.
3. Install as in [install.md](install.md#helm). No value changes are needed when
   the mirror sets are applied. Without mirror sets, point every image value at
   your registry (`manager.image.*`, `bootArtifacts.image.*`,
   `consolePlugin.image.*`, `manager.envOverrides.RELATED_IMAGE_TRANSFER`).

## Network

Inside the data center, the flows are the same as for connected clusters; see
[DESIGN.md §3.9](DESIGN.md#39-required-network-flows-included-in-the-docs). In
short, the physical server needs HTTPS (443) to the cluster's ingress. The
operator needs HTTPS to the BMCs, and optionally SSH/WinRM to the running OS.

Transfer pods pull the transfer image in the migration's namespace. Make sure
those namespaces can pull from your mirror registry (the global pull secret
covers this by default).
