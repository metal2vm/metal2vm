# Third-party components

metal2vm's own code is Apache-2.0. The components below are either Go/JS
library dependencies (tracked in `go.mod` / `console-plugin/yarn.lock`) or
programs that metal2vm's container images ship and run as **separate
processes**. No GPL/LGPL code is linked into metal2vm binaries.

Every entry records the upstream, license, version and where the corresponding
source can be obtained, as required by the GPL/LGPL. Published images:
`metal2vm-operator`, `metal2vm-transfer`, `metal2vm-boot-artifacts`,
`metal2vm-console-plugin`, plus the OLM bundle (manifests only) and catalog.

## Programs shipped in container images

| Component | Image | License | Version | Source |
|---|---|---|---|---|
| virt-v2v / virt-v2v-in-place | transfer | GPL-2.0-or-later | CentOS Stream 10 package | https://github.com/libguestfs/virt-v2v (SRPM: https://gitlab.com/redhat/centos-stream/rpms/virt-v2v) |
| libguestfs (incl. guestfish) | transfer | LGPL-2.1-or-later (guestfish: GPL-2.0-or-later) | CentOS Stream 10 package | https://github.com/libguestfs/libguestfs (SRPM: https://gitlab.com/redhat/centos-stream/rpms/libguestfs) |
| libnbd (nbdcopy) | transfer | LGPL-2.1-or-later | CentOS Stream 10 package | https://gitlab.com/nbdkit/libnbd (SRPM: https://gitlab.com/redhat/centos-stream/rpms/libnbd) |
| CentOS Stream 10 (base image) | transfer | Various (see /usr/share/licenses) | stream10 | https://gitlab.com/redhat/centos-stream/rpms |
| nbdkit + nbdkit-basic-plugins (live-tools rootfs) | boot-artifacts | BSD-3-Clause | Fedora 44 package | https://gitlab.com/nbdkit/nbdkit (SRPM: https://src.fedoraproject.org/rpms/nbdkit) |
| Fedora 44 minimal rootfs (live-tools) | boot-artifacts | Various (see /usr/share/licenses in the rootfs) | 44 | https://src.fedoraproject.org |
| virtio-win drivers | transfer | Apache-2.0 AND BSD-3-Clause AND GPL-2.0-only AND GPL-2.0-or-later (per the package; see its license files) | 0.1.302-1 (pinned by SHA-256) | https://github.com/virtio-win/kvm-guest-drivers-windows |
| Fedora CoreOS live ISO (unmodified) | boot-artifacts | Various (Fedora; see the ISO's license files) | 44.20261023.3.1 | https://fedoraproject.org/coreos/ (sources: https://builds.coreos.fedoraproject.org) |
| UBI 9 micro (base image) | boot-artifacts | UBI EULA (freely redistributable) | 9 | https://catalog.redhat.com/software/base-images |
| distroless static (base image) | operator | Apache-2.0 (plus Debian package licenses) | nonroot | https://github.com/GoogleContainerTools/distroless |
| UBI 9 nginx 1.20 (base image) | console-plugin | UBI EULA (freely redistributable); nginx: BSD-2-Clause | 9 | https://catalog.redhat.com/software/containers/ubi9/nginx-120 |
| opm (base image of the catalog) | catalog | Apache-2.0 | v1.74.0 | https://github.com/operator-framework/operator-registry |

## Go libraries linked into metal2vm binaries

In use: controller-runtime, k8s.io/*, openshift/api, kubevirt.io/api, chisel (agent and transfer), gofish (Redfish).

| Library | License |
|---|---|
| github.com/jpillora/chisel | MIT |
| github.com/stmcginnis/gofish | BSD-3-Clause |
| github.com/openshift/api | Apache-2.0 |
| kubevirt.io/api, CDI API | Apache-2.0 |
| sigs.k8s.io/controller-runtime, k8s.io/* | Apache-2.0 |
