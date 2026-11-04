# Releasing metal2vm

A release is cut by pushing a tag `vX.Y.Z`. The
[release workflow](../.github/workflows/release.yml) then does the following:

1. Checks that the tag matches `VERSION` in the Makefile.
2. Builds and pushes the four images to `ghcr.io/metal2vm` (`linux/amd64`).
3. Rebuilds the OLM bundle and catalog. It fails if the committed `bundle/` or
   `catalog/` differ, then pushes both images.
4. Packages the Helm chart and pushes it to `oci://ghcr.io/metal2vm/charts`.
5. Creates a GitHub pre-release with `install.yaml` and the chart archive attached.

## Steps

```sh
hack/toolbox.sh make set-version VERSION=0.2.0   # updates image tags, CSV, chart; regenerates bundle/catalog/install.yaml
git commit -am "Release v0.2.0"
git tag v0.2.0
git push origin main v0.2.0
```

`set-version` changes these:
- `VERSION` in the Makefile;
- the image tags in `config/manager`, `config/console-plugin` and the CSV base;
- `dist/chart/Chart.yaml` and `values.yaml`.

It then runs `make catalog build-installer`. Check `git diff` before committing.

## First release only

GitHub creates new container packages as **private**. After the first release
workflow has run, set each package to **public**: under the organization's
**Packages**, open each package, then **Package settings → Change visibility**.

The packages to change:
- `metal2vm-operator`, `metal2vm-transfer`, `metal2vm-boot-artifacts`,
  `metal2vm-console-plugin`;
- `metal2vm-operator-bundle`, `metal2vm-operator-catalog`;
- `charts/metal2vm`.

Clusters can't pull the images until this is done.

## Checks on every push

The [Packaging workflow](../.github/workflows/packaging.yml) checks two things:
- **Generated files are current:** `make catalog build-installer` produces no
  diff, and the bundle and catalog validate.
- **The chart works:** it installs into a Kind cluster, and the operator
  becomes ready.
