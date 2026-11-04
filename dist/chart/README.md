# metal2vm Helm chart

Installs metal2vm on OpenShift: the operator, the console plugin and the CRDs.

```sh
helm install metal2vm oci://ghcr.io/metal2vm/charts/metal2vm --version 0.1.0 \
  --namespace metal2vm-system --create-namespace
```

The main values are documented in [docs/install.md](../../docs/install.md#helm);
all values are in [values.yaml](values.yaml). For a disconnected install, see
[docs/disconnected.md](../../docs/disconnected.md).

## Maintaining the chart

The chart was generated with `kubebuilder edit --plugins=helm/v2-alpha` from
`dist/install.yaml`, then customized by hand:
- `templates/console-plugin/console-plugin.yaml` (written by hand, fixed names);
- `templates/rbac/conversion-scc.yaml` (fixed name, always a ClusterRole);
- in `templates/manager/manager.yaml`: the boot-artifacts image value and the
  console-plugin flags;
- in `values.yaml`: `bootArtifacts` and `consolePlugin`.

If you regenerate the chart with `--force`, back up these files first and
restore the customizations afterwards.
