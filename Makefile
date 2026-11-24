# Image URL to use all building/pushing image targets
IMG ?= jamalshahverdiev/pgpilot-operator:latest

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
	@$(MAKE) --no-print-directory sync-chart-crds

# The Helm chart ships its own copy of the CRDs. It is never edited by hand:
# this target replaces it with the generated ones so the two cannot drift.
CHART_CRD_DIR ?= charts/pgpilot-operator/files/crds

.PHONY: sync-chart-crds
sync-chart-crds: ## Copy the generated CRDs into the Helm chart.
	@mkdir -p "$(CHART_CRD_DIR)"
	@rm -f "$(CHART_CRD_DIR)"/*.yaml
	cp config/crd/bases/*.yaml "$(CHART_CRD_DIR)/"

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-cover
test-cover: test ## Run tests and display coverage summary.
	go tool cover -func cover.out

.PHONY: test-integration
test-integration: ## Run live integration test against current cluster (requires operator deployed).
	hack/integration-test.sh

# The e2e tests assume Kind is pre-installed. They build the manager image,
# load it into a dedicated Kind cluster and deploy the operator there.
#
# The suite, and every kubectl / make call it spawns, talks to the cluster
# through E2E_KUBECONFIG only. Your own kubeconfig and its current context are
# never read or changed, so the install / deploy / uninstall steps of the
# suite cannot reach another cluster.
#
# cert-manager is not needed by anything the suite tests and is not installed.
# Run with CERT_MANAGER_INSTALL_SKIP=false to have the suite install it.
KIND_CLUSTER ?= pgpilot-operator-test-e2e
E2E_KUBECONFIG ?= $(LOCALBIN)/e2e-kubeconfig
CERT_MANAGER_INSTALL_SKIP ?= true

# e2e-delete-cluster deletes the Kind cluster. The kubeconfig file is removed
# only when it is the one these targets keep under LOCALBIN: E2E_KUBECONFIG can
# be pointed anywhere, and `kind delete cluster` already takes its own entries
# out of the file it is given.
define e2e-delete-cluster
{ $(KIND) delete cluster --name $(KIND_CLUSTER) --kubeconfig "$(E2E_KUBECONFIG)" && \
  case "$(E2E_KUBECONFIG)" in "$(LOCALBIN)"/*) rm -f -- "$(E2E_KUBECONFIG)" ;; esac; }
endef

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@mkdir -p "$$(dirname "$(E2E_KUBECONFIG)")"
	@clusters="$$($(KIND) get clusters)"; \
	if printf '%s\n' "$$clusters" | grep -qxF -- "$(KIND_CLUSTER)"; then \
		echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation."; \
	else \
		echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
		$(KIND) create cluster --name $(KIND_CLUSTER) --kubeconfig "$(E2E_KUBECONFIG)"; \
	fi
	@$(KIND) export kubeconfig --name $(KIND_CLUSTER) --kubeconfig "$(E2E_KUBECONFIG)"

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@rc=0; \
	KUBECONFIG="$(E2E_KUBECONFIG)" CERT_MANAGER_INSTALL_SKIP=$(CERT_MANAGER_INSTALL_SKIP) \
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
		go test -tags=e2e ./test/e2e/ -v -ginkgo.v || rc=$$?; \
	$(e2e-delete-cluster) || true; \
	exit $$rc

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(e2e-delete-cluster)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: govulncheck
govulncheck: govulncheck-bin ## Report known vulnerabilities reachable from the operator's code.
	# The scan has to run the `go` of go.mod's toolchain and read its standard
	# library. When the installed Go is older, that toolchain is only reached
	# through an automatic switch, which govulncheck does not follow for the
	# standard library; a GOROOT exported by a version manager pins the old
	# one as well. Put the right toolchain first on PATH instead.
	@goroot="$$(env -u GOROOT GOTOOLCHAIN="$(GO_MOD_TOOLCHAIN)" go env GOROOT)"; \
	env -u GOROOT PATH="$$goroot/bin:$$PATH" "$(GOVULNCHECK)" ./...

# HELM_VALIDATE_SETS lists the value combinations helm-validate renders, one
# per word; "-" stands for the chart defaults.
#
# CRDs are skipped by kubeconform: the published schemas do not cover
# CustomResourceDefinition itself. They are checked elsewhere - the API server
# of the envtest suite installs them, and test/chart proves the chart renders
# exactly those files.
HELM ?= helm
HELM_CHART ?= charts/pgpilot-operator
HELM_KUBE_VERSION ?= 1.31.0
HELM_VALIDATE_SETS ?= - \
	installCRDs=false \
	rbac.create=false \
	leaderElection.enabled=false \
	metrics.enabled=false \
	metrics.secure=false \
	serviceAccount.create=false,serviceAccount.name=existing

.PHONY: helm-validate
helm-validate: kubeconform ## Render the Helm chart with its toggles and validate the objects against the Kubernetes API schemas.
	@command -v "$(HELM)" >/dev/null 2>&1 || { echo "helm is not installed"; exit 1; }
	@for set in $(HELM_VALIDATE_SETS); do \
		args=""; [ "$$set" = "-" ] || args="--set $$set"; \
		echo "==> helm template $${args:-(default values)}"; \
		rendered="$$("$(HELM)" template pgpilot-operator "$(HELM_CHART)" \
			--namespace pgpilot-system --kube-version $(HELM_KUBE_VERSION) $$args)" || exit 1; \
		printf '%s\n' "$$rendered" | "$(KUBECONFORM)" -strict -summary \
			-kubernetes-version $(HELM_KUBE_VERSION) -skip CustomResourceDefinition - || exit 1; \
	done

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
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name pgpilot-operator-builder
	$(CONTAINER_TOOL) buildx use pgpilot-operator-builder
	# A failed build or push must fail the target; the builder and the
	# generated Dockerfile are cleaned up either way.
	@rc=0; \
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross . || rc=$$?; \
	$(CONTAINER_TOOL) buildx rm pgpilot-operator-builder || true; \
	rm -f Dockerfile.cross; \
	exit $$rc

# render-default writes the manifests of config/default, with the manager image
# set to IMG, to the file given as argument. The image is set in a throwaway
# copy of config/, so the tracked config/manager/kustomization.yaml is never
# rewritten by a build or a deploy.
define render-default
work="$$(mktemp -d)" || exit 1; rc=0; \
{ cp -R config "$$work/config" && \
  ( cd "$$work/config/manager" && "$(KUSTOMIZE)" edit set image controller=${IMG} ) && \
  "$(KUSTOMIZE)" build "$$work/config/default" > "$$work/rendered.yaml" && \
  cp "$$work/rendered.yaml" $(1); } || rc=$$?; \
[ -z "$$work" ] || rm -r -- "$$work"; \
[ $$rc -eq 0 ] || exit $$rc
endef

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	@mkdir -p dist
	@$(call render-default,dist/install.yaml)
	@echo "wrote dist/install.yaml for ${IMG}"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

# In the targets below the kustomize output is captured before kubectl runs: a
# failing build has to fail the target, not be skipped or applied half-rendered.
# The explicit `|| exit 1` keeps that true for a make without .SHELLFLAGS
# support (the 3.81 that macOS ships), where recipes do not run with -e.
.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$("$(KUSTOMIZE)" build config/crd)" || exit 1; \
	printf '%s\n' "$$out" | "$(KUBECTL)" apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$("$(KUSTOMIZE)" build config/crd)" || exit 1; \
	printf '%s\n' "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	@rendered="$$(mktemp)" || exit 1; rc=0; \
	( $(call render-default,"$$rendered") ) && "$(KUBECTL)" apply -f "$$rendered" || rc=$$?; \
	rm -f -- "$$rendered"; \
	exit $$rc

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$("$(KUSTOMIZE)" build config/default)" || exit 1; \
	printf '%s\n' "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

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
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.23.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
KUBECONFORM ?= $(LOCALBIN)/kubeconform
KUBECONFORM_VERSION ?= v0.8.0
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
GOVULNCHECK_VERSION ?= v1.10.0

.PHONY: kubeconform
kubeconform: $(KUBECONFORM) ## Download kubeconform locally if necessary.
$(KUBECONFORM): $(LOCALBIN)
	$(call go-install-tool,$(KUBECONFORM),github.com/yannh/kubeconform/cmd/kubeconform,$(KUBECONFORM_VERSION))

.PHONY: govulncheck-bin
govulncheck-bin: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

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

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
#
# Tools are built with at least the toolchain of go.mod, whatever `go` is
# installed locally: a linter or govulncheck built with an older Go cannot
# read code (or a standard library) written for a newer one.
GO_MOD_TOOLCHAIN := go$(shell sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' go.mod)+auto

define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOTOOLCHAIN="$(GO_MOD_TOOLCHAIN)" GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
