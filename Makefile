# Release version and images. `make set-version VERSION=x.y.z` updates the
# defaults in config/, the CSV base and the Helm chart.
VERSION ?= 0.1.0
IMAGE_TAG_BASE ?= ghcr.io/metal2vm
IMG ?= $(IMAGE_TAG_BASE)/metal2vm-operator:v$(VERSION)
TRANSFER_IMG ?= $(IMAGE_TAG_BASE)/metal2vm-transfer:v$(VERSION)
BOOT_ARTIFACTS_IMG ?= $(IMAGE_TAG_BASE)/metal2vm-boot-artifacts:v$(VERSION)
PLUGIN_IMG ?= $(IMAGE_TAG_BASE)/metal2vm-console-plugin:v$(VERSION)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)/metal2vm-operator-bundle:v$(VERSION)
CATALOG_IMG ?= $(IMAGE_TAG_BASE)/metal2vm-operator-catalog:v$(VERSION)
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= metal2vm-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name metal2vm-builder
	$(CONTAINER_TOOL) buildx use metal2vm-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm metal2vm-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate dist/install.yaml (operator + console plugin, for OpenShift).
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/openshift > dist/install.yaml

##@ Console plugin

# The plugin builds in a pinned Node container (hack/node-toolbox.sh); set
# PLUGIN_RUN= to use a local Node/yarn instead.
PLUGIN_RUN ?= hack/node-toolbox.sh
PLUGIN_YARN = node .yarn/releases/yarn-4.14.1.cjs
ifeq ($(PLUGIN_RUN),)
PLUGIN_EXEC = cd console-plugin && sh -c
else
PLUGIN_EXEC = $(PLUGIN_RUN) sh -c
endif

.PHONY: plugin-install
plugin-install: ## Install console plugin dependencies.
	$(PLUGIN_EXEC) '$(PLUGIN_YARN) install --immutable'

.PHONY: plugin-test
plugin-test: plugin-install ## Run console plugin unit tests.
	$(PLUGIN_EXEC) '$(PLUGIN_YARN) test'

.PHONY: plugin-lint
plugin-lint: plugin-install ## Lint the console plugin.
	$(PLUGIN_EXEC) '$(PLUGIN_YARN) lint'

.PHONY: plugin-build
plugin-build: plugin-install ## Production build of the console plugin.
	$(PLUGIN_EXEC) '$(PLUGIN_YARN) build'

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	$(if $(BOOT_IMG),cd config/manager && "$(KUSTOMIZE)" edit set image boot-artifacts=$(BOOT_IMG))
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Packaging (OLM bundle and catalog, Helm chart, release)

OPERATOR_SDK_VERSION ?= v1.42.3
OPM_VERSION ?= v1.74.0
HELM_VERSION ?= v4.3.1
KUBEBUILDER_VERSION ?= v4.16.0
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
OPM ?= $(LOCALBIN)/opm
HELM ?= $(LOCALBIN)/helm
KUBEBUILDER ?= $(LOCALBIN)/kubebuilder
TOOLS_OS = $(shell go env GOOS)
TOOLS_ARCH = $(shell go env GOARCH)

# download-tool downloads $2 to $1 (versioned by $3) unless present.
define download-tool
@[ -f "$(1)-$(3)" ] || { set -e; echo "Downloading $(2)"; curl -sSLf -o "$(1)-$(3)" "$(2)"; chmod +x "$(1)-$(3)"; }; \
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

.PHONY: operator-sdk opm helm kubebuilder
operator-sdk: $(LOCALBIN) ## Download operator-sdk locally if necessary.
	$(call download-tool,$(OPERATOR_SDK),https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(TOOLS_OS)_$(TOOLS_ARCH),$(OPERATOR_SDK_VERSION))
opm: $(LOCALBIN) ## Download opm locally if necessary.
	$(call download-tool,$(OPM),https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION)/$(TOOLS_OS)-$(TOOLS_ARCH)-opm,$(OPM_VERSION))
helm: $(LOCALBIN) ## Download helm locally if necessary.
	@[ -f "$(HELM)-$(HELM_VERSION)" ] || { set -e; echo "Downloading helm $(HELM_VERSION)"; \
	  curl -sSLf https://get.helm.sh/helm-$(HELM_VERSION)-$(TOOLS_OS)-$(TOOLS_ARCH).tar.gz | tar -xzO $(TOOLS_OS)-$(TOOLS_ARCH)/helm > "$(HELM)-$(HELM_VERSION)"; \
	  chmod +x "$(HELM)-$(HELM_VERSION)"; }; ln -sf "$$(realpath "$(HELM)-$(HELM_VERSION)")" "$(HELM)"
kubebuilder: $(LOCALBIN) ## Download kubebuilder locally if necessary.
	$(call go-install-tool,$(KUBEBUILDER),sigs.k8s.io/kubebuilder/v4,$(KUBEBUILDER_VERSION))

BUNDLE_CHANNELS ?= --channels=alpha
BUNDLE_DEFAULT_CHANNEL ?= --default-channel=alpha
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate the OLM bundle (bundle/) and validate it.
	$(OPERATOR_SDK) generate kustomize manifests -q --interactive=false --package metal2vm
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle -q --overwrite --package metal2vm \
		--version $(VERSION) $(BUNDLE_METADATA_OPTS)
	go run ./hack/csv-related-images bundle/manifests/metal2vm.clusterserviceversion.yaml
	@# createdAt changes on every run; dropping it keeps bundle/ and catalog/ reproducible.
	sed -i '/^    createdAt: /d' bundle/manifests/metal2vm.clusterserviceversion.yaml
	@# OLM reads bundle files as another user: they must be world-readable.
	chmod -R a+rX bundle
	$(OPERATOR_SDK) bundle validate ./bundle --select-optional suite=operatorframework

.PHONY: catalog
catalog: bundle opm ## Generate the file-based OLM catalog (catalog/) for BUNDLE_IMG.
	rm -rf catalog && mkdir -p catalog/metal2vm
	$(OPM) init metal2vm --default-channel=alpha --icon=docs/images/metal2vm-icon.svg --output yaml \
		> catalog/metal2vm/catalog.yaml
	$(OPM) render ./bundle --output yaml | sed 's#image: ""#image: $(BUNDLE_IMG)#' >> catalog/metal2vm/catalog.yaml
	printf -- '---\nschema: olm.channel\npackage: metal2vm\nname: alpha\nentries:\n- name: metal2vm.v$(VERSION)\n  skipRange: "<$(VERSION)"\n' \
		>> catalog/metal2vm/catalog.yaml
	$(OPM) validate catalog
	@[ -f catalog.Dockerfile ] || $(OPM) generate dockerfile catalog --base-image=quay.io/operator-framework/opm:$(OPM_VERSION)

.PHONY: bundle-build catalog-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .
catalog-build: ## Build the catalog image.
	$(CONTAINER_TOOL) build -f catalog.Dockerfile -t $(CATALOG_IMG) .

##@ Helm Deployment

## Helm binary to use for deploying the chart
HELM ?= helm
## Namespace to deploy the Helm release
HELM_NAMESPACE ?= metal2vm-system
## Name of the Helm release
HELM_RELEASE ?= metal2vm
## Path to the Helm chart directory
HELM_CHART_DIR ?= dist/chart
## Additional arguments to pass to helm commands
HELM_EXTRA_ARGS ?=

.PHONY: install-helm
install-helm: helm ## Download the pinned Helm version (bin/helm).

.PHONY: helm-deploy
helm-deploy: install-helm ## Deploy manager to the K8s cluster via Helm. Specify an image with IMG.
	IMG="$(IMG)"; $(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--namespace $(HELM_NAMESPACE) \
		--create-namespace \
		--set manager.image.repository=$${IMG%:*} \
		--set manager.image.tag=$${IMG##*:} \
		--wait \
		--timeout 5m \
		$(HELM_EXTRA_ARGS)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm release from the K8s cluster.
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-status
helm-status: ## Show Helm release status.
	$(HELM) status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-history
helm-history: ## Show Helm release history.
	$(HELM) history $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-rollback
helm-rollback: ## Rollback to previous Helm release.
	$(HELM) rollback $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: set-version
set-version: ## Set the release version everywhere and regenerate: make set-version VERSION=x.y.z
	@old=$$(sed -n 's/^VERSION ?= //p' Makefile); [ -n "$$old" ] && [ "$$old" != "$(VERSION)" ] || { echo "VERSION must differ from $$old"; exit 1; }; \
	sed -i "s/^VERSION ?= .*/VERSION ?= $(VERSION)/" Makefile; \
	for f in config/manager/kustomization.yaml config/manager/manager.yaml config/console-plugin/kustomization.yaml \
	  config/manifests/bases/metal2vm.clusterserviceversion.yaml dist/chart/values.yaml; do \
	  sed -i "s/:v$$old/:v$(VERSION)/g; s/newTag: v$$old/newTag: v$(VERSION)/; s/tag: \"v$$old\"/tag: \"v$(VERSION)\"/" $$f; done; \
	sed -i "s/^version: .*/version: $(VERSION)/; s/^appVersion: .*/appVersion: \"v$(VERSION)\"/" dist/chart/Chart.yaml
	$(MAKE) catalog build-installer VERSION=$(VERSION)
