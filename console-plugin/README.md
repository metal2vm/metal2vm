# metal2vm console plugin

An OpenShift console dynamic plugin (React, PatternFly 6) that adds metal2vm's
pages under **Virtualization**:
- **Physical servers**: list, add a server, server details with boot ISO and iPXE URL.
- **Migrate a server**: a five-step wizard (server, disks, network, VM, review).
- **Physical migrations**: list and details with progress, *Cut over*, *Roll back* and *Archive*.

The plugin only reads and writes metal2vm's custom resources. Everything it does
can also be done with `oc`. It started from the
[console plugin template](https://github.com/openshift/console-plugin-template)
(Apache-2.0, see [LICENSE](LICENSE)).

| Path | Contents |
|---|---|
| `src/components/sources/` | Physical servers pages |
| `src/components/wizard/` | Migration wizard |
| `src/components/migrations/` | Migration list and details |
| `src/utils/migration.ts` | Wizard state → `P2VMigration` mapping and UI rules (unit-tested) |
| `src/models/` | Types and models for the CRDs |
| `console-extensions.json` | Routes and navigation items |
| `locales/` | Generated i18n strings (`yarn i18n`) |

## Build, test, lint

Node runs in a pinned container, so only podman (or docker) is needed. From the repository root:

```sh
make plugin-test plugin-lint      # Jest tests, eslint/stylelint
make plugin-build                 # production build into console-plugin/dist
```

## Run against a cluster

```sh
cd console-plugin
yarn install && yarn start        # terminal 1: plugin dev server on port 9001 (Node 22)
yarn run start-console            # terminal 2: local OpenShift console on port 9000 (needs `oc login`)
```

Then open <http://localhost:9000/p2v/migrations>. On Apple silicon, the console
image is amd64 and runs under emulation.

## Deployment

The plugin image is built from [Dockerfile](Dockerfile): nginx serving `dist/`.
It is installed with the operator in all three install methods (OLM, Helm,
YAML; see [docs/install.md](../docs/install.md)). The operator registers the
`ConsolePlugin`. On a development cluster, `hack/openshift-dev-deploy.sh plugin
deploy` builds and deploys it.
