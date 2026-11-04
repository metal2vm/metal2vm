# Installing metal2vm

metal2vm runs on **OpenShift 4.22+ with OpenShift Virtualization**. It needs a
storage class that provides block-mode PVCs for VMs (ODF or any CSI driver
supported by OpenShift Virtualization).

There are three ways to install it. All of them install the same things:
- the operator, in `metal2vm-system`;
- the console plugin;
- the three CRDs (`P2VSource`, `P2VMigration`, `P2VConfig`).

| Method | Best for | Upgrades |
|---|---|---|
| [OperatorHub (OLM)](#operatorhub-olm) | Click-ops admins, disconnected clusters | OLM, from the `alpha` channel |
| [Helm](#helm) | GitOps, `helm` users | `helm upgrade` |
| [YAML](#yaml) | A quick try with `oc` only | Re-apply a newer file |

> [!NOTE]
> Releases are published to `ghcr.io/metal2vm` (images, bundle, catalog, chart)
> and listed on the [releases page](https://github.com/metal2vm/metal2vm/releases).
> Images are built for `linux/amd64`.

## What the operator sets up by itself

- **`P2VConfig` named `cluster`**: the cluster-wide settings, with defaults.
- **SCC `metal2vm-conversion`**: restricted-v2 plus CRI-O's stock
  `unshare` seccomp profile. virt-v2v's network helper (passt) needs this in the
  conversion pods. Forklift/MTV uses the same approach.
- **`ConsolePlugin` `metal2vm-console-plugin`**: registers the console plugin.
  Switching it on in the console is a separate step, described per method below.

The SCC and the `ConsolePlugin` are owned by the ClusterRole
`metal2vm-conversion-scc`, which the installer manages. Uninstalling metal2vm
therefore removes them.

The operator's own permissions are listed in `config/rbac/role.yaml`. It may
create SCCs and ConsolePlugins. It may only update or delete the two objects
above, by name.

## OperatorHub (OLM)

1. Add the metal2vm catalog. In a disconnected cluster, use the mirrored catalog instead; see
   [disconnected.md](disconnected.md).
   ```yaml
   apiVersion: operators.coreos.com/v1alpha1
   kind: CatalogSource
   metadata:
     name: metal2vm
     namespace: openshift-marketplace
   spec:
     sourceType: grpc
     image: ghcr.io/metal2vm/metal2vm-operator-catalog:v0.1.0
     displayName: metal2vm
     publisher: metal2vm project
   ```
2. In the console, go to **Ecosystem → Software Catalog** (OperatorHub). Search
   for **metal2vm** and click **Install**. Keep the suggested namespace
   `metal2vm-system`, and set **Console plugin** to **Enable**.
3. Wait until the operator shows **Succeeded**. Then use
   **Virtualization → Physical servers**.

The same install with `oc`:

```sh
oc create namespace metal2vm-system
oc apply -f - <<'EOF'
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata: {name: metal2vm, namespace: metal2vm-system}
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata: {name: metal2vm, namespace: metal2vm-system}
spec:
  channel: alpha
  name: metal2vm
  source: metal2vm
  sourceNamespace: openshift-marketplace
EOF
# Enable the console plugin (what the "Console plugin: Enable" switch does):
oc patch consoles.operator.openshift.io cluster --type=json \
  -p '[{"op":"add","path":"/spec/plugins/-","value":"metal2vm-console-plugin"}]'
```

**Uninstall:**
1. In the console, choose **Uninstall Operator**, or delete the Subscription and the CSV.
2. Remove the cluster-scoped roles from the bundle. OLM does not delete those
   itself. Removing them also removes the SCC and the `ConsolePlugin`, which
   they own:
   ```sh
   oc delete clusterrole -l olm.owner=metal2vm.v0.1.0
   ```

OLM keeps the CRDs and your `P2VSource`/`P2VMigration` objects. Delete the CRDs
only if you want to remove those objects too. Migrated VMs and their disks are
never deleted by metal2vm.

## Helm

```sh
helm install metal2vm oci://ghcr.io/metal2vm/charts/metal2vm --version 0.1.0 \
  --namespace metal2vm-system --create-namespace
```

Useful values. The full list is in [`dist/chart/values.yaml`](../dist/chart/values.yaml).

| Value | Default | Meaning |
|---|---|---|
| `consolePlugin.enabled` | `true` | Deploy the console plugin and register it |
| `consolePlugin.enableInConsole` | `true` | Also switch it on in the console |
| `manager.image.repository`, `.tag` | `ghcr.io/metal2vm/metal2vm-operator`, release | Operator image |
| `manager.envOverrides.RELATED_IMAGE_TRANSFER` | release transfer image | Transfer/conversion pod image |
| `bootArtifacts.image.repository`, `.tag` | `ghcr.io/metal2vm/metal2vm-boot-artifacts`, release | Live ISO and PXE files |
| `consolePlugin.image.repository`, `.tag` | `ghcr.io/metal2vm/metal2vm-console-plugin`, release | Console plugin |
| `crd.keep` | `true` | Keep the CRDs (and your objects) on `helm uninstall` |

**Uninstall:** `helm uninstall metal2vm -n metal2vm-system`. The CRDs stay
(`crd.keep`), and so do migrated VMs.

## YAML

Every release has an `install.yaml` asset. It contains the operator, the console
plugin and the CRDs.

```sh
oc apply --server-side -f https://github.com/metal2vm/metal2vm/releases/download/v0.1.0/install.yaml
oc patch consoles.operator.openshift.io cluster --type=json \
  -p '[{"op":"add","path":"/spec/plugins/-","value":"metal2vm-console-plugin"}]'
```

The file is generated with `make build-installer` (from `config/openshift`).

## Development install

This builds all images on the cluster itself (OpenShift BinaryBuilds into the
internal registry) and deploys `config/openshift`. You need `oc`/`kubectl`
logged in as cluster-admin, and podman.

```sh
hack/openshift-dev-deploy.sh all          # or one step: operator|transfer|boot|plugin|deploy
```

The dev deployment also enables the console plugin
(`--enable-console-plugin`).

## After installing

1. **Check the gateway:** `oc get p2vconfig cluster` should show `Available=True`.
   Physical servers reach it on port 443 of the cluster's ingress.
2. **Check the network flows** from your servers and BMCs; see
   [DESIGN.md §3.9](DESIGN.md#39-required-network-flows-included-in-the-docs).
3. In the console, go to **Virtualization → Physical servers → Add physical
   server**.

**On plain Kubernetes** (no OpenShift), the operator starts and reports what is
missing. The gateway needs an OpenShift Route; `LoadBalancer`/`NodePort`
exposure is not implemented yet.
