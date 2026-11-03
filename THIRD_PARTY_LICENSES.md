# Third-party components

metal2vm's own code is Apache-2.0. The components below are either Go/JS
library dependencies (tracked in `go.mod` / `console-plugin/yarn.lock`) or
programs that metal2vm's container images ship and run as **separate
processes**. No GPL/LGPL code is linked into metal2vm binaries.

This file is completed as each image is built. The boot-artifacts image is final for M1; the transfer image follows in M2. Every entry
records the upstream, license, version and where the corresponding source can
be obtained, as required by the GPL/LGPL.

## Programs shipped in container images

| Component | Image | License | Version | Source |
|---|---|---|---|---|
| virt-v2v / virt-v2v-in-place | transfer | GPL-2.0-or-later | _tbd (M2)_ | https://github.com/libguestfs/virt-v2v |
| libguestfs | transfer | LGPL-2.1-or-later | _tbd (M2)_ | https://github.com/libguestfs/libguestfs |
| libnbd (nbdcopy) | transfer | LGPL-2.1-or-later | CentOS Stream 10 package | https://gitlab.com/nbdkit/libnbd (SRPM: https://gitlab.com/redhat/centos-stream/rpms/libnbd) |
| CentOS Stream 10 (base image) | transfer | Various (see /usr/share/licenses) | stream10 | https://gitlab.com/redhat/centos-stream/rpms |
| nbdkit + nbdkit-basic-plugins (live-tools rootfs) | boot-artifacts | BSD-3-Clause | Fedora 44 package | https://gitlab.com/nbdkit/nbdkit (SRPM: https://src.fedoraproject.org/rpms/nbdkit) |
| Fedora 44 minimal rootfs (live-tools) | boot-artifacts | Various (see /usr/share/licenses in the rootfs) | 44 | https://src.fedoraproject.org |
| virtio-win drivers (>= 1.9.40 only) | transfer | Apache-2.0 AND BSD-3-Clause AND GPL-2.0-only AND GPL-2.0-or-later | _tbd (M2)_ | https://github.com/virtio-win/kvm-guest-drivers-windows |
| Fedora CoreOS live ISO (unmodified) | boot-artifacts | Various (Fedora; see the ISO's license files) | 44.20261023.3.1 | https://fedoraproject.org/coreos/ (sources: https://builds.coreos.fedoraproject.org) |
| UBI 9 micro (base image) | boot-artifacts | UBI EULA (freely redistributable) | 9 | https://catalog.redhat.com/software/base-images |

## Go libraries linked into metal2vm binaries

In use: controller-runtime, k8s.io/*, openshift/api, chisel (agent and transfer). Planned: gofish (M4).

| Library | License |
|---|---|
| github.com/jpillora/chisel | MIT |
| github.com/stmcginnis/gofish | BSD-3-Clause |
| github.com/openshift/api | Apache-2.0 |
| kubevirt.io/api, CDI API | Apache-2.0 |
| sigs.k8s.io/controller-runtime, k8s.io/* | Apache-2.0 |
