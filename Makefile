# Makefile for Accretion with comprehensive caching
.PHONY: all bootstrap build build-all build-mcp cache-clean cache-info charts-check check-k8s ci clean \
	credential-plaintext-check deploy-auth deploy-collector deploy-status deps-precommit deps-update deps-verify destroy dev \
	deploy-collector-preflight dev-cleanup dev-setup docker-build docker-build-cached docker-build-multiplatform env-file existing-secret-check fmt \
	go-tools gofmt-check helm-deps helm-lint help init-project install-hooks install-mcp kind-down kind-up \
	lint lint-new lint-new-selfcheck preflight preflight-gate rbac-check rbac-secrets-check redis-check release render-check require-env secrets secrets-check \
	secrets-functional-check secrets-identity-check secrets-keys-check secrets-portability-check secrets-pulumi-check secrets-stale-check \
	secrets-strength-check security setup-namespace size sizing-check test test-bench test-coverage \
	test-functional tilt-down tilt-full tilt-server-only tilt-up tiltfile-check trace-ingest-check traces-check up \
	values-check values-matrix-check vet

# Configuration
# Tool versions are pinned so local checks and CI report the same findings, and
# so neither silently executes whatever upstream published this morning.
# golangci-lint is additionally not optional at v2: v1 cannot lint a Go 1.25
# module at all.
AIR_VERSION ?= v1.67.4
BINARY_NAME := accretion-api
# The shape of a scaled-out collector, and of one that additionally splits trace
# ingest out with a floor of two replicas and a PDB.
#
# #123 deleted charts/accretion-collector/values-{dev,test,staging,prod}.yaml,
# which is where the gates below used to reach a scaled configuration. Scaling
# any collector past one replica is what brings the controller, Redis and the
# Redis queue mode back (see the chart's own values.yaml), so that one --set is
# the whole of it.
#
# The log and metric splits are part of the scaled shape, not an extra. Without
# them this renders 25 objects; the deleted staging and prod files rendered 41,
# and the sixteen-object difference is the split log and metric collectors with
# their ServiceAccounts, roles, bindings, Services, autoscalers and budgets.
# Since lane 3's central file sets both splits true, a fixture named for a
# scaled-out collector that does not split is not the shape a scaled-out
# collector has -- it would leave those sixteen objects ungated. With the splits
# it renders 35.
#
# Declared once here rather than repeated per gate. NOT the only site, though:
# scripts/check-values-matrix.sh spells the same scaled shape out again, because
# it is a standalone bash-and-helm script by design and cannot read a Makefile
# variable. Two places until the API half of #123 points both at
# deploy/pulumi.yaml and deletes the flags from each.
#
# Carries a Redis password because scale-out is the shape that deploys Redis,
# and the chart refuses to render one without a password now that the default is
# empty. The value is a render-time placeholder, not a credential: no gate
# asserts anything about it, and the two that care about the password's handling
# (redis-check, credential-plaintext-check) pass their own and override this.
COLLECTOR_SCALED := --set dataCollector.replicaCount=2 \
	--set dataCollector.kinds.log.split=true \
	--set dataCollector.kinds.metric.split=true \
	--set-string redis.auth.password=render-only-not-a-credential
COLLECTOR_REDUNDANT := $(COLLECTOR_SCALED) --set dataCollector.kinds.trace.split=true \
	--set dataCollector.kinds.trace.replicaCount=2 \
	--set dataCollector.kinds.trace.podDisruptionBudget.enabled=true \
	--set dataCollector.kinds.trace.podDisruptionBudget.minAvailable=1
# Extra values for `make deploy-collector`. Empty by default: since #123 the
# chart ships no per-environment files, so an operator deploying anything other
# than the defaults passes their own -- COLLECTOR_VALUES="-f my-prod.yaml".
COLLECTOR_VALUES ?=
# A render-time database password for the gates, exactly parallel to the Redis
# one in COLLECTOR_SCALED above and added for the same reason (#134): the three
# charts that talk to Postgres used to ship `postgres` in their own values files
# and now ship nothing, so they refuse to render with no password and no
# existingSecret. Every gate that templates one of them for a reason unrelated
# to credentials -- RBAC, sizing, trace scoping, PDBs -- has to supply one.
#
# It is a literal for the renderer, not a credential: nothing deploys it, and
# the gates that DO assert something about the password's handling
# (credential-plaintext-check, existing-secret-check) pass their own sentinel
# and override this. Named so that a grep for it in a cluster finds nothing and
# a reader who sees it in a diff does not go looking for where it is stored.
DB_PASSWORD_RENDER := --set-string config.database.password=render-only-not-a-credential
# The database password SOURCE for the real `make deploy-collector` path (#144),
# as opposed to the gate stub above. deploy-collector and its preflight point the
# chart at the operator-provisioned Secret -- exactly as deploy/tilt.yaml and
# deploy/pulumi.yaml do -- rather than a --set-string literal that `helm get
# values` would expose afterwards. `make secrets` builds accretion-api-secrets
# with the db-password key both the api and collector charts read; the collector
# chart's own existingSecretKey default is "password", so the key is named
# explicitly here for the same reason deploy/tilt.yaml names it.
COLLECTOR_DB_SECRET := --set config.database.existingSecret=accretion-api-secrets --set-string config.database.existingSecretKey=db-password
# Database TLS for the collector deploy path.
#
# charts/accretion-collector/values-{staging,prod}.yaml carried
# `sslmode: require` until #123 deleted them, and the chart's own default is
# `disable`. Deleting the files therefore did not merely remove the CHECK on
# staging and production database encryption, it removed the SETTING: for one
# commit `make deploy-auth STACK=prod` brought up an API on verify-full beside a
# collector talking to the same Postgres in the clear. The Makefile comment on
# values-check has recorded since #99 that a regeneration of those files once
# downgraded staging from require to disable and nothing caught it; deleting
# them reproduced that downgrade and removed the guard in the same change.
#
# So the setting lives here now, on the path that lost it, and it is derived
# from the stack rather than defaulted to one answer. Encrypted is the default
# and cleartext has to be NAMED: local clusters run a Postgres with no TLS, so
# dev and test are the documented exceptions rather than an assumption.
# Override explicitly for anything else -- DB_SSLMODE=verify-full.
#
# `?=` means the ENVIRONMENT can set this, and make treats an exported variable
# as already assigned. That is deliberate -- an operator may need verify-full --
# but it also means `export DB_SSLMODE=disable` in a shell, or a line added to
# the tracked .envrc that direnv loads on every cd, would otherwise downgrade
# every deploy with nothing at deploy time saying so. So the value is VALIDATED
# rather than trusted, by deploy-collector-preflight below, and echoed when it
# deploys. Overriding stays possible; overriding into cleartext does not.
#
# This covers the `make deploy-collector` path. The Pulumi path is closed
# separately and independently, and NOT in deploy/pulumi.yaml -- that file sets
# no sslmode at all, so looking there and finding none proves nothing. The mode
# is a per-stack Pulumi config value, `dbSslMode` in pulumi/pkg/config, which
# defaults to DefaultDatabaseSSLMode and is validated against the
# encryptedSSLModes allowlist -- the same three values this file enforces --
# before helm.go injects it into both charts. It did once deploy the collector
# in the clear; that predated this lane and was never this lane's to fix, and it
# does not now.
DB_SSLMODE ?= $(if $(filter dev test,$(STACK)),disable,require)
DOCKER_IMAGE := accretion-api
DOCKER_TAG ?= latest
FUNCTIONAL_PORT ?= 18080
# Seconds test-functional waits for /api/health behind the port-forward. A
# variable so secrets-functional-check can drive the target to its "not reachable"
# exit in one second instead of twenty — that exit is what proves the credential
# resolution above it SUCCEEDED.
FUNCTIONAL_WAIT ?= 20
GOIMPORTS_VERSION ?= v0.49.0
GOLANGCI_VERSION ?= v2.13.2
GOSEC_VERSION ?= v2.29.0
# The values file that declares the machine callers' `apiKeys`. A variable, not a
# constant, because that file is moving: the per-environment file today, a
# central per-caller file once #119/#120 land.
IDENTITIES_FILE ?= charts/accretion-api/values-dev.yaml
NAMESPACE ?= accretion-core
RELEASE_NAME ?= accretion-api
STACK ?= dev
# Derived from git tags; see scripts/version.sh. Override to pin a build.
VERSION ?= $(shell bash scripts/version.sh)
ZERO_DIGEST := 0000000000000000000000000000000000000000000000000000000000000000

# Build configuration
CGO_ENABLED := 0
GOARCH := amd64
GOOS := linux

# Go flags
#
# -race lives in RACE, not GOFLAGS, because the race detector requires cgo while
# the shipped binaries are built CGO_ENABLED=0 for a static image. Putting it in
# GOFLAGS applied it to those builds too, which fails outright with "-race
# requires cgo". It belongs on every `go run` and `go test` — where the code
# actually executes and a data race can be observed — and nowhere else.
# Version stamp for the builds that do not use the full static BUILD_FLAGS.
LDFLAGS_VERSION ?= -ldflags=-X=main.version=$(VERSION)
BUILD_FLAGS ?= -a -installsuffix cgo -ldflags='-w -s -extldflags "-static" -X main.version=$(VERSION)'
GOFLAGS ?=
RACE ?= -race
TEST_FLAGS ?= -v $(RACE)

# Source and cache directories
BUILD_DIR := ./build
CACHE_DIR := ./.cache
GOCACHE := $(shell go env GOCACHE)
GOMODCACHE := $(shell go env GOMODCACHE)

# BuildKit configuration for Docker
export BUILDKIT_PROGRESS=plain
export DOCKER_BUILDKIT=1

# Colors
BLUE := \033[34m
GREEN := \033[32m
NC := \033[0m
RED := \033[31m
YELLOW := \033[33m

# Default target
all: ci docker-build-cached ## Build everything

# One command from a fresh clone to a working checkout. Ordered so each step can
# assume the previous one: the env file first (nothing runs without it), then the
# Go tools, then the hooks that use them, then a verification pass.
bootstrap: env-file go-tools install-hooks preflight ## Set up a fresh clone end to end
	@printf "%b\n" "$(GREEN)Ready. Fill in your secrets in .env, then: direnv allow$(NC)"

build: deps-verify ## Build binary with caching
	@echo "$(BLUE)Building $(BINARY_NAME) with caching...$(NC)"
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
	go build $(GOFLAGS) $(BUILD_FLAGS) \
		-o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/accretion-api
	@echo "$(GREEN)Build completed: $(BUILD_DIR)/$(BINARY_NAME)$(NC)"

build-all: deps-verify ## Build for multiple platforms
	@echo "$(BLUE)Building for multiple platforms...$(NC)"
	mkdir -p $(BUILD_DIR)
	@for os in linux darwin windows; do \
		for arch in amd64 arm64; do \
			if [ "$$os" = "windows" ]; then ext=".exe"; else ext=""; fi; \
			echo "Building $$os/$$arch..."; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build $(GOFLAGS) $(BUILD_FLAGS) \
				-o $(BUILD_DIR)/$(BINARY_NAME)-$$os-$$arch$$ext ./cmd/accretion-api; \
		done; \
	done
	@echo "$(GREEN)Multi-platform build completed$(NC)"

build-mcp: ## Build the accretion-mcp server binary
	@echo "$(BLUE)Building MCP binaries...$(NC)"
	mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS_VERSION) -o $(BUILD_DIR)/accretion-mcp ./cmd/accretion-mcp
	@echo "$(GREEN)Build completed: $(BUILD_DIR)/accretion-mcp$(NC)"

cache-clean: ## Clean all caches
	@echo "$(YELLOW)Cleaning caches...$(NC)"
	go clean -cache -modcache -testcache
	docker builder prune -f
	rm -rf $(BUILD_DIR) $(CACHE_DIR)
	@echo "$(GREEN)Caches cleaned$(NC)"

cache-info: ## Show cache information
	@echo "$(BLUE)Cache Information:$(NC)"
	@echo "GOCACHE: $(GOCACHE)"
	@echo "GOMODCACHE: $(GOMODCACHE)"
	@echo "Build cache size: $$(du -sh $(GOCACHE) 2>/dev/null | cut -f1 || echo 'N/A')"
	@echo "Module cache size: $$(du -sh $(GOMODCACHE) 2>/dev/null | cut -f1 || echo 'N/A')"

# The one target the pipeline's charts job calls, and the one `make ci` calls.
# It used to enumerate three of these in the workflow and a different set in
# `make ci`, which is how profiles-check spent a sprint in `make ci` while CI
# never ran it and values-minimal.yaml was rendered by nothing (#102). Add a
# chart gate here and it runs in both places or neither.
charts-check: helm-lint values-check values-matrix-check rbac-check rbac-secrets-check redis-check sizing-check trace-ingest-check traces-check secrets-check render-check credential-plaintext-check existing-secret-check ## Run every chart gate CI runs
	@printf "%b\n" "$(GREEN)Every chart gate passed$(NC)"

check-k8s: ## Check if Kubernetes is available and configured
	@echo "$(BLUE)Checking Kubernetes cluster status...$(NC)"
	@if kubectl cluster-info >/dev/null 2>&1; then \
		echo "$(GREEN)✓ Kubernetes cluster is running$(NC)"; \
		kubectl cluster-info | head -1; \
		echo "$(BLUE)Current context: $$(kubectl config current-context)$(NC)"; \
		echo "$(BLUE)Available nodes:$(NC)"; \
		kubectl get nodes -o wide; \
	else \
		echo "$(RED)✗ No Kubernetes cluster detected$(NC)"; \
		exit 1; \
	fi

# CI verifies dependencies rather than updating them: the old chain began with
# deps-update, whose `go get ./...` and `go mod tidy` rewrite the very files the
# run is supposed to be checking, making the result non-reproducible.
#
# CI enforces lint-new, not lint: golangci-lint's v2 upgrade (forced by Go 1.25)
# brought ~ten versions of new checks whose pre-existing findings are deliberately
# grandfathered via --new-from-rev. `lint` stays a target for anyone auditing that
# backlog, but `make ci` runs lint-new so it passes exactly when GitHub does (#137).
ci: deps-verify lint-new lint-new-selfcheck vet security charts-check tiltfile-check test build ## Run full CI pipeline
	@echo "$(GREEN)CI pipeline completed successfully$(NC)"

clean: ## Clean build artifacts
	@echo "$(BLUE)Cleaning build artifacts...$(NC)"
	rm -rf $(BUILD_DIR)
	rm -rf $(CACHE_DIR)
# Sweep any stray binaries left at the repo root by a manual `go build`: without
# -o, `go build ./cmd/accretion-mcp` writes the binary into the current directory.
# All Makefile targets write to $(BUILD_DIR) instead.
	rm -f $(BINARY_NAME) accretion-mcp
	go clean -testcache
	@echo "$(GREEN)Build artifacts cleaned$(NC)"

deploy-auth: deploy-collector ## Deploy accretion-api server with authentication ENABLED (production)
	@echo "$(GREEN)Deploying with authentication enabled$(NC)"
	helm upgrade --install $(RELEASE_NAME) ./charts/accretion-api \
		--namespace $(NAMESPACE) \
		--values charts/accretion-api/values-$(STACK).yaml
	@echo "$(GREEN)Deployed $(RELEASE_NAME) (auth enabled)$(NC)"

# Split out of deploy-collector so the refusal can be PROVEN without deploying
# anything: values-check drives this target directly with good and bad values.
# A validation that only runs on the way to `helm upgrade` cannot be tested by a
# gate, and an untested refusal is a comment.
deploy-collector-preflight: ## Validate the collector deploy inputs without deploying
# Validates the mode that will actually BE USED, which is not always DB_SSLMODE,
# in TWO stages: a loose one asking whether it is a PostgreSQL sslmode at all,
# which catches a typo like `requrie` on any stack, and a tight allowlist outside
# dev and test. Both run against the EFFECTIVE mode rather than the variable --
# an earlier version applied the loose stage to DB_SSLMODE only, so a typo in an
# operator values file sailed through on a dev stack, which is the same "checked
# the variable instead of what gets used" shape as the ceiling below.
#
# The guard was added to raise a floor and it was also imposing a ceiling: helm
# gives --set precedence over -f regardless of order, so an operator passing
# COLLECTOR_VALUES with verify-full had it silently downgraded to require by the
# target's own flag. One variable that could be quietly weakened from outside
# and quietly capped a caller from inside.
#
# So there is exactly one source for the value in each case, and the check reads
# whichever one applies. With no operator file the target owns it and DB_SSLMODE
# is checked directly. With an operator file THEIRS is authoritative -- the
# target passes no --set-string at all (see deploy-collector) -- so the file is
# rendered and the resulting mode is checked. That also catches a
# COLLECTOR_VALUES that does not render, before anything reaches a cluster.
#
# No DB_PASSWORD_RENDER stub is passed here (#144). helm ranks --set above -f
# whatever the order, so a stub would override an operator file's own
# config.database.password and validate a render the deploy will not produce --
# the same "--set-string ONLY when the operator supplied no file" rule the deploy
# body obeys. The operator file is authoritative: it supplies its own password
# source (a literal or, as deploy/tilt.yaml does, an existingSecret), and a file
# that supplies none fails the chart's guard here, before the cluster.
#
# The allowlist is deliberately the same three values values-check enforces on
# the rendered output: before this the gate refused `prefer` while the deploy
# path passed it straight through, so the two disagreed about what counts as
# encrypted. dev and test are the documented exceptions because a local Postgres
# has no TLS.
	@set -e; \
	if [ -n "$(COLLECTOR_VALUES)" ]; then \
		out=$$(helm template preflight ./charts/accretion-collector $(COLLECTOR_VALUES) 2>&1) || { \
			printf "%b\n" "$(RED)COLLECTOR_VALUES does not render, so this deploy would fail after touching the cluster:$(NC)" >&2; \
			printf "%s\n" "$$out" >&2; exit 1; }; \
		mode=$$(printf "%s\n" "$$out" | grep -m1 "sslmode:" | sed "s/.*sslmode: *//;s/\"//g"); \
		src="COLLECTOR_VALUES"; \
	else \
		mode="$(DB_SSLMODE)"; src="DB_SSLMODE"; \
	fi; \
	if [ -z "$$mode" ]; then \
		printf "%b\n" "$(RED)no sslmode could be determined from $$src — refusing rather than deploying an unknown database TLS posture$(NC)" >&2; exit 1; \
	fi; \
	case "$$mode" in \
		disable|allow|prefer|require|verify-ca|verify-full) ;; \
		*) printf "%b\n" "$(RED)$$src gives sslmode=$$mode, which is not a PostgreSQL sslmode at all.$(NC)" >&2; \
		   printf "%b\n" "Valid values: disable allow prefer require verify-ca verify-full" >&2; \
		   exit 1 ;; \
	esac; \
	if [ "$(STACK)" != "dev" ] && [ "$(STACK)" != "test" ]; then \
		case "$$mode" in \
			require|verify-ca|verify-full) ;; \
			*) printf "%b\n" "$(RED)$$src gives sslmode=$$mode, which would send STACK=$(STACK) database traffic unencrypted.$(NC)" >&2; \
			   printf "%b\n" "$(YELLOW)Only require, verify-ca and verify-full are accepted outside dev and test — the same list values-check enforces on the rendered output. DB_SSLMODE can be set from the environment, so check for an exported value or a line in .envrc.$(NC)" >&2; \
			   exit 1 ;; \
		esac; \
	fi; \
	printf "%b\n" "  database sslmode=$$mode (from $$src)"

deploy-collector: deploy-collector-preflight ## Deploy accretion-collector (includes PostgreSQL + Redis)
# STACK no longer selects anything here. It still picks the API's values file in
# deploy-auth, but #123 retired the collector's per-environment files, so
# without this guard `make deploy-auth STACK=prod` installed bare chart defaults
# for half of what it deploys and said nothing: three workloads instead of
# seven, no autoscalers, no budgets, no Redis and no controller. A variable that
# silently stops meaning anything for one half of a command is worse than one
# that never applied, so refuse rather than install the wrong thing quietly.
	@if [ -z "$(COLLECTOR_VALUES)" ] && [ "$(STACK)" != "dev" ] && [ "$(STACK)" != "test" ]; then \
		printf "%b\n" "$(RED)STACK=$(STACK) no longer selects a values file for the collector.$(NC)" >&2; \
		printf "%b\n" "$(YELLOW)#123 retired charts/accretion-collector/values-<env>.yaml. Installing now would deploy bare chart defaults — no autoscalers, no PodDisruptionBudgets, no Redis, no controller — under the name $(STACK).$(NC)" >&2; \
		printf "%b\n" "Supply that environment yourself:  make deploy-collector STACK=$(STACK) COLLECTOR_VALUES=\"-f path/to/$(STACK).yaml\"" >&2; \
		exit 1; \
	fi
	@echo "$(BLUE)Deploying accretion-collector (PostgreSQL + Redis + workers)...$(NC)"
# --set-string ONLY when the operator supplied no file. helm ranks --set above
# -f whatever the order, so passing both would override a value the operator
# chose -- silently, and downward. Exactly one source owns the mode in each case
# and deploy-collector-preflight validates whichever one that is.
	helm upgrade --install accretion-collector ./charts/accretion-collector \
		--namespace $(NAMESPACE) \
		$(if $(COLLECTOR_VALUES),$(COLLECTOR_VALUES),$(COLLECTOR_DB_SECRET) --set-string config.database.sslmode=$(DB_SSLMODE))
	@echo "$(GREEN)Deployed accretion-collector$(NC)"

deploy-status: ## Show deployment status of all components
	@echo "$(BLUE)Deployment Status:$(NC)"
	@echo ""
	@echo "$(YELLOW)Accretion API Server:$(NC)"
	@kubectl get deployment $(RELEASE_NAME) -n $(NAMESPACE) 2>/dev/null || echo "  Not deployed"
	@echo ""
	@echo "$(YELLOW)Accretion-Collector:$(NC)"
	@kubectl get deployment -l app.kubernetes.io/name=accretion-collector -n $(NAMESPACE) 2>/dev/null || echo "  Not deployed"

deps-precommit: go-tools ## Install tools used for pre-commit hooks (alias for go-tools)
	@true


deps-update: ## Update, vendor, and verify dependencies
	@echo "$(BLUE)Updating dependencies...$(NC)"
	go get ./...
	go mod tidy
	go mod vendor
	go mod verify
	@echo "$(GREEN)Dependencies updated$(NC)"
# Checks that `go mod vendor` is a no-op, by hashing the tree either side of it.
# Comparing against git instead would report any uncommitted work as a stale
# vendor tree, which is wrong on a developer's machine and only accidentally
# right on a clean CI checkout.
deps-verify: ## Verify go.mod/go.sum/vendor are consistent — never mutates
	@printf "%b\n" "$(BLUE)Verifying dependencies...$(NC)"
	go mod verify
	@printf "%b\n" "$(BLUE)Checking vendor/ matches go.mod...$(NC)"
	@TMPDIR_DV=$$(mktemp -d) bash -c 'set -eo pipefail; \
		trap "rm -rf $$TMPDIR_DV" EXIT; \
		snap() { find vendor -type f | LC_ALL=C sort | xargs shasum 2>/dev/null; shasum go.mod go.sum; }; \
		snap > "$$TMPDIR_DV/before.txt"; \
		go mod vendor; \
		snap > "$$TMPDIR_DV/after.txt"; \
		if ! diff -q "$$TMPDIR_DV/before.txt" "$$TMPDIR_DV/after.txt" >/dev/null; then \
			printf "%b\n" "$(RED)vendor/ is stale — run go mod vendor and commit the result$(NC)"; \
			printf "%b\n" "$(YELLOW)what changed:$(NC)"; \
			diff "$$TMPDIR_DV/before.txt" "$$TMPDIR_DV/after.txt" | head -40; \
			exit 1; \
		fi'
	@printf "%b\n" "$(GREEN)Dependencies consistent$(NC)"

# Sequential on purpose: tilt-down also kills the tilt process for this
# directory, and kind-down deletes the cluster it was talking to. Two $(MAKE)
# lines, like up, guarantee the order; prerequisites would not under -j. It
# leaves .env and .tls/ alone -- a rebuild regenerates the CA.
destroy: ## Tear down Tilt and the Kind cluster
	@$(MAKE) tilt-down
	@$(MAKE) kind-down

dev: require-env preflight-gate ## Run in development mode with auto-reload
	@echo "$(BLUE)Starting development server with auto-reload...$(NC)"
	@if command -v air >/dev/null 2>&1; then \
		air; \
	else \
		echo "$(YELLOW)air not found, install with: go install github.com/air-verse/air@$(AIR_VERSION)$(NC)"; \
		echo "$(BLUE)Running without auto-reload...$(NC)"; \
		go run $(GOFLAGS) $(RACE) ./cmd/accretion-api server; \
	fi

dev-cleanup: ## Clean up development environment
	@echo "$(BLUE)Cleaning up development environment...$(NC)"
	helm uninstall accretion-api-dev -n development || true
	helm uninstall accretion-api-dev-collector -n development || true
	@echo "$(YELLOW)Killing any existing port-forwards...$(NC)"
	@pkill -f "kubectl.*port-forward.*5432" || true
	@pkill -f "kubectl.*port-forward.*11434" || true
	@echo "$(GREEN)Development environment cleaned up$(NC)"

dev-setup: init-project ## Install development tools (alias for init-project)
	@true

docker-build: ## Build Docker image without cache
	@echo "$(BLUE)Building Docker image (no cache)...$(NC)"
	docker build --no-cache --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	@echo "$(GREEN)Docker image built: $(DOCKER_IMAGE):$(DOCKER_TAG)$(NC)"

docker-build-cached: ## Build Docker image with maximum caching
	@echo "$(BLUE)Building Docker image with maximum caching...$(NC)"
	docker build \
		--cache-from $(DOCKER_IMAGE):$(DOCKER_TAG) \
		--cache-from $(DOCKER_IMAGE):latest \
		--build-arg BUILDKIT_INLINE_CACHE=1 \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	@echo "$(GREEN)Docker image built with caching: $(DOCKER_IMAGE):$(DOCKER_TAG)$(NC)"

docker-build-multiplatform: ## Build multi-platform Docker image
	@echo "$(BLUE)Building multi-platform Docker image...$(NC)"
	docker buildx create --use --name multiplatform || true
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--cache-from type=local,src=/tmp/.buildx-cache \
		--cache-to type=local,dest=/tmp/.buildx-cache \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) \
		--push .

# Never overwrites: .env holds real secrets and is gitignored, so clobbering it
# destroys credentials with nothing to restore from.
env-file: ## Create .env from env.example if it does not exist
	@if [ -f .env ]; then \
		printf "%b\n" "$(YELLOW)⚠ .env already exists — leaving it untouched$(NC)"; \
	else \
		cp env.example .env; \
		printf "%b\n" "$(GREEN)✓ Created .env from env.example$(NC)"; \
		printf "%b\n" "$(YELLOW)  Fill in the keys for the providers you use, then: direnv allow$(NC)"; \
	fi

fmt: ## Format code
	@echo "$(BLUE)Formatting code...$(NC)"
	go fmt ./...
	@echo "$(GREEN)Code formatted$(NC)"

go-tools: ## Install all Go tools (goimports, golangci-lint, gosec, air)
	@echo "$(BLUE)Installing Go tools...$(NC)"
	go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)
	go install github.com/air-verse/air@$(AIR_VERSION)
	@echo "$(GREEN)Go tools installed$(NC)"

# gofmt needs no external tool, so unlike golangci-lint above it cannot skip
# itself when something is not installed — which is how an unformatted file sat
# on main unnoticed. Three trees are excluded because none of them is ours to
# reformat: vendor/ (vendored deps), .gopath/ (the project-local GOPATH module
# cache set by .envrc), and pulumi/ (a separate module, linted by its own hook).
gofmt-check: ## Verify every Go file outside vendor/ is gofmt-clean
	@printf "%b\n" "$(BLUE)Checking gofmt...$(NC)"
	@bash -c 'bad=$$(gofmt -l . 2>/dev/null | grep -vE "^(vendor|\.gopath|pulumi)/" || true); \
		if [ -n "$$bad" ]; then \
			printf "%b\n" "$(RED)These files are not gofmt-clean:$(NC)"; \
			printf "  %s\n" $$bad; \
			printf "%b\n" "$(YELLOW)Fix with:  gofmt -w .$(NC)"; \
			exit 1; \
		fi; \
		printf "%b\n" "$(GREEN)all Go files outside vendor/ are gofmt-clean$(NC)"'

helm-deps: ## Build Helm dependencies with caching
	@echo "$(BLUE)Building Helm dependencies...$(NC)"
	cd charts/accretion-api && \
	if [ ! -f Chart.lock ] || [ charts/accretion-api/Chart.yaml -nt charts/accretion-api/Chart.lock ]; then \
		helm dependency build; \
	else \
		echo "$(GREEN)Helm dependencies are up to date$(NC)"; \
	fi

# Lint every chart. accretion-db needs its Bitnami dependency built first
# (`helm dependency build ./charts/accretion-db`); CI does that in the job.
helm-lint: ## helm lint every chart
	@printf "%b\n" "$(BLUE)Linting charts...$(NC)"
	@bash -c 'set -eo pipefail; \
		for c in charts/*/; do \
			[ -f "$$c/Chart.yaml" ] || continue; \
			printf "%b\n" "  == $$c"; \
			helm lint "$$c"; \
		done'

help: ## Show this help message
	@echo "$(BLUE)Accretion API Build System with Caching$(NC)"
	@echo "=================================="
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "$(YELLOW)%-20s$(NC) %s\n", $$1, $$2}'

init-project: go-tools install-hooks ## Initialize project: install tools and configure hooks
	@echo "$(GREEN)Project initialized. You're ready to go.$(NC)"

# Both stages, always. Installing only the pre-commit stage leaves the release
# tag check inert with nothing to report it, which is how it sat unnoticed.
install-hooks: ## Install the git hooks (pre-commit and pre-push stages)
	@if command -v pre-commit >/dev/null 2>&1; then \
		pre-commit install --hook-type pre-commit --hook-type pre-push; \
	else \
		printf "%b\n" "$(RED)✗ pre-commit not found — the git hooks cannot be installed.$(NC)"; \
		printf "%b\n" "$(YELLOW)  Linux:  pipx install pre-commit   (or apt/dnf install pre-commit)$(NC)"; \
		printf "%b\n" "$(YELLOW)  macOS:  brew install pre-commit$(NC)"; \
		exit 1; \
	fi

install-mcp: ## Install MCP server binaries to GOPATH/bin
	@echo "$(BLUE)Installing MCP binaries...$(NC)"
	go install $(LDFLAGS_VERSION) ./cmd/accretion-mcp
	@echo "$(GREEN)Installed to $$(go env GOPATH)/bin: accretion-mcp$(NC)"

kind-down: ## Delete Kind cluster and stop local registry
	@echo "$(BLUE)Deleting Kind cluster and registry...$(NC)"
	kind delete cluster --name accretion-dev || true
	docker rm -f kind-registry || true
	@echo "$(GREEN)Kind cluster and registry removed$(NC)"

kind-up: ## Create Kind cluster with local image registry
	@echo "$(BLUE)Creating Kind cluster with local registry...$(NC)"
	bash scripts/kind-with-registry.sh
	@echo "$(GREEN)Kind cluster ready$(NC)"

lint: gofmt-check ## Run linter with caching
	@echo "$(BLUE)Running linter...$(NC)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
	else \
		echo "$(YELLOW)golangci-lint not found, install it with: make go-tools$(NC)"; \
	fi

# The `cd "$(pwd -P)"` is load-bearing. golangci-lint's --new-from-rev filter
# keeps a finding only if its path, made relative to the working directory,
# matches a path in `git diff --relative`. It resolves symlinks in the working
# directory (pkg/fsutils/fsutils.go, Getwd) but takes the finding's own path
# from `go list`, which uses $PWD as typed. Run from a directory reached
# through a symlink — macOS /tmp is one, and the sprint tooling puts its
# worktrees there — the relative path comes out as ../../private/tmp/... and
# matches nothing, so EVERY finding is dropped and the run reports "0 issues".
# That is a false pass, not a clean tree (#98). Entering the physical path
# first makes the two views agree. scripts/check-lint-new-symlink.sh proves
# this keeps working; `make lint-new-selfcheck` runs it.
lint-new: gofmt-check ## Lint only code changed relative to origin/main (what CI enforces)
	@printf "%b\n" "$(BLUE)Linting changes against origin/main...$(NC)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd "$$(pwd -P)" && golangci-lint run --timeout=5m --new-from-rev=origin/main; \
	else \
		printf "%b\n" "$(RED)golangci-lint not found. Install it: make go-tools$(NC)"; \
		exit 1; \
	fi

lint-new-selfcheck: ## Prove lint-new reports findings from a symlinked directory (regression test for #98)
	@bash scripts/check-lint-new-symlink.sh

# Preflight gates every target that starts the app. SKIP_PREFLIGHT=1 bypasses it
# for the case where you know a dependency is down and want to start anyway.
preflight: ## Verify required tools and configured endpoints are healthy
	@bash scripts/preflight.sh $(PREFLIGHT_FLAGS)

preflight-gate:
	@if [ -n "$(SKIP_PREFLIGHT)" ]; then \
		printf "%b\n" "$(YELLOW)⚠ preflight skipped (SKIP_PREFLIGHT set)$(NC)"; \
	else \
		bash scripts/preflight.sh $(PREFLIGHT_FLAGS); \
	fi

# Rendered-RBAC assertions for the collector chart (#100, #103, #96). The
# DEFAULT install is rendered alongside every env file because that is the
# case that was missed: sprint-10's fix was verified against prod while the
# shipped default put pods/log on the OTLP listener's ServiceAccount. A render
# with the configmap kind disabled covers the other direction of the secrets
# grant, and a render with trace alone in the shared process covers the one
# legitimate unsplit shape. The script asserts the RENDER on its own; the
# _validation.tpl guard refuses VALUES, independently. The proofs at the end
# show both have teeth: the guard's message on the escape renders, and the
# script's failure on renders with the grant stripped or the trace role
# widened.
rbac-check: ## Verify no trace-receiving pod holds pods/log, and pods watch lands only where the controller runs
	@printf "%b\n" "$(BLUE)Checking rendered collector RBAC (default install + every env)...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) > $$d/default.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) > $$d/scaled.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) --set statusCollector.kinds.configmap.enabled=false > $$d/no-configmap-kind.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) --set dataCollector.kinds.trace.split=false \
			--set dataCollector.kinds.log.split=true --set dataCollector.kinds.metric.split=true \
			--set dataCollector.kinds.event.enabled=false > $$d/trace-alone.yaml; \
		bash scripts/check-collector-rbac.sh $$d/default.yaml $$d/scaled.yaml \
			$$d/no-configmap-kind.yaml $$d/trace-alone.yaml; \
		sed "s/resources: \[\"configmaps\"\]/resources: [\"secrets\"]/" $$d/default.yaml > $$d/secrets-added.yaml; \
		if ! grep -q "resources: \[\"secrets\"\]" $$d/secrets-added.yaml; then \
			printf "%b\n" "$(RED)the no-secrets proof did not inject a secrets rule -- the proof itself is inert$(NC)"; exit 1; \
		fi; \
		proof=$$(bash scripts/check-collector-rbac.sh $$d/secrets-added.yaml 2>&1 || true); \
		case "$$proof" in \
			*"grants a verb on secrets"*) ;; \
			*) printf "%b\n" "$(RED)a role granting secrets was not rejected for granting secrets -- the no-secrets assertion is inert$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  no-secrets assertion proven to fire (a role granting secrets is rejected by name)"; \
		guard="the trace collector shares the data-collector process with"; \
		for escape in "--set dataCollector.kinds.trace.split=false" "--set dataCollector.kinds.trace.split=false --set dataCollector.kinds.log.split=true"; do \
			msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $$escape 2>&1 >/dev/null || true); \
			if printf "%s" "$$msg" | grep -q "$$guard"; then \
				printf "%b\n" "  ok  guard proven to fire on [$$escape]"; \
			else \
				printf "%b\n" "$(RED)[$$escape] rendered, or failed without the _validation.tpl message -- the guard is inert or masked: $$msg$(NC)"; exit 1; \
			fi; \
		done; \
		awk "/^kind: ClusterRole\$$/{c=1} c&&/^  name: .*-trace-collector\$$/{t=1} c&&t&&/resources: \[\"namespaces\"\]/{sub(/namespaces/,\"events\")} {print} /^---/{c=0;t=0}" $$d/default.yaml > $$d/trace-widened.yaml; \
		grep -q "resources: \[\"events\"\]" $$d/trace-widened.yaml || { printf "%b\n" "$(RED)could not widen the trace role for the proof$(NC)"; exit 1; }; \
		if bash scripts/check-collector-rbac.sh $$d/trace-widened.yaml >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)a trace-collector role granting events passed -- the minimal-identity assertion is inert$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  minimal-identity assertion proven to fire (an events grant on the trace role is rejected)"'
	@printf "%b\n" "$(GREEN)Collector RBAC is isolated in the default install and every env$(NC)"

# Redis renders only under scale-out, so staging and prod are the renders that
# carry it; the default render is asserted to carry none, which is what makes
# "no Redis rendered" a checked fact rather than a vacuous pass elsewhere.
# maxmemory must sit below the container limit or it is inert: the kubelet
# OOM-kills before Redis ever refuses a write (#105).
#
# Single source of truth (lead ruling on #111): before this ticket,
# config.redis.password (server argv) and redis.auth.password (the Secret
# clients read) were two independent values that happened to default to the
# same literal, which hid the split -- overriding one alone left the server
# authenticating a different password than its clients configure. redis.yaml
# no longer reads config.redis.password at all; both the redis-secret's
# `password` key (clients) and its `redis.conf` key (the server, mounted as a
# file per #111) are built from redis.auth.password only. The check below
# renders with the two values deliberately diverged and asserts config.redis.
# password renders nowhere and the two Secret keys agree; a synthetic
# divergence injected into the render afterward proves the assertion is not
# vacuous.
#
# Input guard on redis.auth.password (scope extension, not a ticket in this
# series -- closes a live finding at its source instead of chasing another
# encoder). templates/secrets.yaml now refuses to render an empty password
# (requirepass "" is no password at all: Redis authenticates anyone) or one
# containing anything outside printable ASCII. Narrower rules were tried and
# both leaked: a control-character-only rule missed U+00A0, which %q also
# escapes and redis.conf also drops silently -- same shape as the toJson
# bug, a different character. Printable ASCII is the range confirmed to
# round-trip on both sides (only " and \ need escaping there), closed
# permanently at the input instead of enumerating a fourth exception list.
# Proven non-vacuous three ways below, and an ordinary password (including
# one with `&`, which must NOT be rejected) is asserted to still render.
redis-check: ## Verify the standalone Redis caps its memory below its limit and evicts nothing
	@printf "%b\n" "$(BLUE)Checking rendered Redis memory cap...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)"; exit 1; }; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) > $$d/default.yaml; \
		! grep -q "image: redis:" $$d/default.yaml || fail "the default render deploys Redis; nothing is scaled out there"; \
		printf "%b\n" "  ok  default render deploys no Redis"; \
		for e in scaled; do \
			helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) > $$d/$$e.yaml; \
			grep -q "image: redis:" $$d/$$e.yaml || fail "$$e renders no Redis -- this check is vacuous"; \
			max=$$(grep -A1 -- "- \"--maxmemory\"" $$d/$$e.yaml | tail -1 | sed "s/.*- \"//;s/\"//"); \
			pol=$$(grep -A1 -- "- \"--maxmemory-policy\"" $$d/$$e.yaml | tail -1 | sed "s/.*- \"//;s/\"//"); \
			lim=$$(awk "/image: redis:/{r=1} r&&/limits:/{l=1} r&&l&&/memory:/{gsub(/\"/,\"\",\$$2); print \$$2; exit}" $$d/$$e.yaml); \
			[ -n "$$max" ] || fail "$$e: Redis container has no --maxmemory"; \
			[ "$$pol" = "noeviction" ] || fail "$$e: --maxmemory-policy is [$$pol], not noeviction; other policies evict the controller state"; \
			sav=$$(grep -A1 -- "- \"--save\"" $$d/$$e.yaml | tail -1 | sed "s/^ *- //"); \
			[ "$$sav" = "\"\"" ] || fail "$$e: Redis --save is [$$sav], not \"\"; \
the image default snapshots fork against the dataset and a failed BGSAVE stalls every write"; \
			case "$$max" in *mb) ;; *) fail "$$e: redis.maxmemory [$$max] must use the binary mb suffix so it compares against the Mi limit";; esac; \
			case "$$lim" in *Mi) ;; *) fail "$$e: Redis memory limit [$$lim] is not in Mi; update this check with the unit";; esac; \
			[ "$${max%mb}" -lt "$${lim%Mi}" ] || fail "$$e: redis.maxmemory $$max is not below the $$lim container limit, so the kubelet OOM-kills before Redis refuses a write"; \
			printf "%b\n" "  ok  $$e renders Redis with --maxmemory $$max ($$pol), snapshots off, under a $$lim limit"; \
		done; \
		! grep -q "kind: PersistentVolumeClaim" $$d/scaled.yaml || fail "the scaled render carries a PVC for Redis; its state is reconciled and must stay ephemeral"; \
		printf "%b\n" "  ok  the scaled render carries no PersistentVolumeClaim"; \
		check_single_source() { \
			block=$$(sed -n "/-redis-secret$$/,/^---/p" "$$1"); \
			pw_b64=$$(printf "%s\n" "$$block" | grep "  password:" | head -1 | sed "s/.*password: *\"//;s/\"$$//"); \
			conf_b64=$$(printf "%s\n" "$$block" | grep "  redis.conf:" | head -1 | sed "s/.*redis.conf: *\"//;s/\"$$//"); \
			pw=$$(printf "%s" "$$pw_b64" | base64 -d 2>/dev/null); \
			conf=$$(printf "%s" "$$conf_b64" | base64 -d 2>/dev/null); \
			case "$$conf" in \
				*"$$2"*) ;; \
				*) echo "redis-secret redis.conf key is [$$conf], carrying a different value than the redis-secret password key [$$pw] -- server and clients must authenticate from the same value"; return 1 ;; \
			esac; \
			[ "$$pw" = "$$2" ] || { echo "redis-secret password key is [$$pw], expected [$$2]"; return 1; }; \
			return 0; \
		}; \
		src="SOURCE-$$$$"; decoy="DECOY-$$$$"; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) \
			--set-string redis.auth.password=$$src --set-string config.redis.password=$$decoy > $$d/split.yaml; \
		read found tainted <<< "$$(awk -v decoy=$$decoy "/^---/{kind=\"\";name=\"\";next} /^kind: /{kind=\$$2} /^  name: /{if(name==\"\")name=\$$2} kind==\"Deployment\"&&name==\"t-redis-master\"{found=1;if(index(\$$0,decoy)>0)tainted=1} END{print (found?1:0), (tainted?1:0)}" $$d/split.yaml)"; \
		[ "$$found" = "1" ] || fail "the -redis-master Deployment did not render -- this check is vacuous"; \
		[ "$$tainted" = "0" ] || fail "config.redis.password ($$decoy) rendered into the redis-master Deployment even though the server must read redis.auth.password only (lead ruling on #111)"; \
		msg=$$(check_single_source "$$d/split.yaml" "$$src") || fail "$$msg"; \
		printf "%b\n" "  ok  redis-secret password and redis.conf keys agree, both sourced from redis.auth.password; the redis-master Deployment does not read config.redis.password"; \
		conf_b64=$$(sed -n "/-redis-secret$$/,/^---/p" $$d/split.yaml | grep "  redis.conf:" | head -1 | sed "s/.*redis.conf: *\"//;s/\"$$//"); \
		diverged_b64=$$(printf "requirepass %s\n" "$$decoy" | base64); \
		sed "s|redis.conf: \"$$conf_b64\"|redis.conf: \"$$diverged_b64\"|" $$d/split.yaml > $$d/split-diverged.yaml; \
		grep -q "$$diverged_b64" $$d/split-diverged.yaml || fail "the divergence injection did not land -- the single-source-of-truth proof is inert"; \
		if msg=$$(check_single_source "$$d/split-diverged.yaml" "$$src"); then \
			fail "a redis.conf key diverged from redis.auth.password was not rejected -- the single-source-of-truth assertion is inert"; \
		fi; \
		printf "%b\n" "  ok  single-source-of-truth assertion proven to fire ($$msg)"; \
		msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set-string redis.auth.password= 2>&1 >/dev/null || true); \
		case "$$msg" in \
			*"must be set"*) ;; \
			*) fail "an empty redis.auth.password was not rejected -- an unauthenticated Redis is reachable and the guard is inert: $$msg" ;; \
		esac; \
		printf "%b\n" "  ok  empty redis.auth.password (an unauthenticated Redis) is rejected by name"; \
		ctrlpw=$$(printf "a\x0bb"); \
		msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set-string redis.auth.password="$$ctrlpw" 2>&1 >/dev/null || true); \
		case "$$msg" in \
			*"printable ASCII"*) ;; \
			*) fail "a redis.auth.password containing a control character (0x0B) was not rejected -- the guard is inert: $$msg" ;; \
		esac; \
		printf "%b\n" "  ok  redis.auth.password containing a control character (0x0B) is rejected by name"; \
		nbsppw=$$(printf "a\xc2\xa0b"); \
		msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set-string redis.auth.password="$$nbsppw" 2>&1 >/dev/null || true); \
		case "$$msg" in \
			*"printable ASCII"*) ;; \
			*) fail "a redis.auth.password containing U+00A0 (non-breaking space) was not rejected -- a control-character-only rule would miss exactly this, and the guard is inert: $$msg" ;; \
		esac; \
		printf "%b\n" "  ok  redis.auth.password containing U+00A0 is rejected by name"; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set-string redis.auth.password="an-ordinary-password-&-with-punctuation" >/dev/null \
			|| fail "an ordinary printable-ASCII redis.auth.password (including &) was rejected -- the input guard is over-broad"; \
		printf "%b\n" "  ok  an ordinary printable-ASCII redis.auth.password, including one containing &, still renders"'
	@printf "%b\n" "$(GREEN)Redis is capped below its limit, evicts nothing, snapshots nothing, and persists nothing$(NC)"

# The status collector's memory scales with the objects its informers cache
# (#96), so its Go heap gets a soft cap (GOMEMLIMIT) below the container
# limit: under pressure the runtime collects harder instead of overshooting
# into an OOMKill. This asserts the cap is rendered and numerically below the
# limit on the default install and every env file, so an env that overrides one
# side of the pair is caught.
sizing-check: ## Verify the status collector renders a GOMEMLIMIT below its memory limit
	@printf "%b\n" "$(BLUE)Checking status collector heap cap against its limit...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)"; exit 1; }; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) > $$d/default.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) > $$d/scaled.yaml; \
		for e in default scaled; do \
			cap=$$(awk "/^kind: Deployment\$$/{k=1} k&&/^  name: .*-status-collector\$$/{t=1} t&&/name: GOMEMLIMIT/{g=1;next} g&&/value:/{gsub(/\"/,\"\",\$$2); print \$$2; exit} /^---/{k=0;t=0;g=0}" $$d/$$e.yaml); \
			lim=$$(awk "/^kind: Deployment\$$/{k=1} k&&/^  name: .*-status-collector\$$/{t=1} t&&/limits:/{l=1} t&&l&&/memory:/{gsub(/\"/,\"\",\$$2); print \$$2; exit} /^---/{k=0;t=0;l=0}" $$d/$$e.yaml); \
			[ -n "$$cap" ] || fail "$$e: status collector renders no GOMEMLIMIT"; \
			[ -n "$$lim" ] || fail "$$e: status collector renders no memory limit -- this check is vacuous"; \
			case "$$cap" in *MiB) ;; *) fail "$$e: GOMEMLIMIT [$$cap] must use the MiB suffix so it compares against the limit";; esac; \
			case "$$lim" in *Mi) limmi=$${lim%Mi};; *Gi) limmi=$$(( $${lim%Gi} * 1024 ));; *) fail "$$e: status memory limit [$$lim] is not in Mi or Gi; update this check with the unit";; esac; \
			[ "$${cap%MiB}" -lt "$$limmi" ] || fail "$$e: GOMEMLIMIT $$cap is not below the $$lim container limit, so the cap cannot act before the kubelet does"; \
			printf "%b\n" "  ok  $$e status collector: GOMEMLIMIT $$cap under a $$lim limit"; \
		done'
	@printf "%b\n" "$(GREEN)Status collector heap cap sits below its limit$(NC)"

# Renders the paths that `helm lint` and the other gates never reach. The
# Discord chart defaults to enabled=false, so linting it exercises zero
# templates and a broken enabled=true path merges green (#106); both halves of
# the opt-in contract are asserted, the default renders nothing and the
# enabled render carries the bot's Deployment. The render's exit status is
# checked before anything is counted: a failed helm prints nothing, and
# "nothing" is what the opt-in default is supposed to look like. The otel-collector minimal profile is a
# documented install path that no other gate renders (#102). `helm template`
# executes the charts' fail-closed guards; `helm lint` does not. "Assert the
# render, not the lint."
render-check: ## Verify opt-in chart paths render (Discord enabled=true, otel values-minimal) and defaults stay opt-in
	@printf "%b\n" "$(BLUE)Rendering opt-in chart paths...$(NC)"
	@bash -c 'set -eo pipefail; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)"; exit 1; }; \
		out=$$(helm template t ./charts/accretion-discord) || fail "accretion-discord failed to render with default values"; \
		n=$$(printf "%s\n" "$$out" | grep -c "^kind:" || true); \
		[ "$$n" -eq 0 ] || fail "accretion-discord renders $$n objects with default values; it is meant to be opt-in (#94)"; \
		printf "%b\n" "  ok  accretion-discord renders nothing by default (render exit checked first)"; \
		out=$$(helm template t ./charts/accretion-discord --set enabled=true --set secret.botToken=x) || fail "accretion-discord failed to render with enabled=true -- the opt-in path is broken"; \
		n=$$(printf "%s\n" "$$out" | grep -c "^kind:" || true); \
		printf "%s\n" "$$out" | grep -q "^kind: Deployment$$" || fail "accretion-discord enabled=true renders no Deployment; the bot itself is missing from an otherwise non-empty render"; \
		[ "$$n" -ge 4 ] || fail "accretion-discord enabled=true renders $$n objects; the bot needs at least its Deployment, Secret, ServiceAccount and ConfigMap"; \
		printf "%b\n" "  ok  accretion-discord enabled=true renders $$n objects including its Deployment"; \
		helm template t ./charts/otel-collector --namespace obs -f charts/otel-collector/values-minimal.yaml >/dev/null \
			|| fail "charts/otel-collector/values-minimal.yaml does not render"; \
		printf "%b\n" "  ok  otel-collector values-minimal.yaml renders"'
	@printf "%b\n" "$(GREEN)Opt-in chart paths render$(NC)"

# Detector for #109 (closes the class that came back after #16/#17/#18): a
# credential rendered in cleartext into a ConfigMap, an initContainer command,
# or a container's args is readable by anyone with get configmaps/get pods --
# far below the get secrets bar that reading the Secret itself requires. Every
# chart and every env values file is rendered once with a single shared
# SENTINEL value standing in for every known password/token field, and
# scripts/check-credential-plaintext.sh asserts that value appears in none of
# those three places. A helm failure aborts the whole check (via `render`'s
# own exit 1) instead of being read as zero occurrences -- render-check had
# exactly that bug (#106). --selfcheck proves the scanner itself has teeth
# against hand-built fixtures, independent of which of this series' fixes
# have landed in this checkout.
credential-plaintext-check: ## Verify no credential renders into a ConfigMap, a command, or args, in any chart or env
	@printf "%b\n" "$(BLUE)Checking rendered charts for plaintext credentials...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		sentinel="CREDENTIAL-SENTINEL-$$$$-DO-NOT-LEAK"; \
		zero=$(ZERO_DIGEST); \
		apikeys="apiKeys=[{\"name\":\"ci\",\"hash\":\"$$zero\",\"permissions\":[\"read:memory\"]}]"; \
		render() { \
			out="$$1"; shift; \
			if ! helm template t "$$@" > "$$d/$$out.yaml" 2>"$$d/$$out.err"; then \
				printf "%b\n" "$(RED)helm template failed for $$out [$$*]:$(NC)"; cat "$$d/$$out.err"; exit 1; \
			fi; \
		}; \
		render api-values ./charts/accretion-api -f charts/accretion-api/values.yaml \
			--set-string config.database.password=$$sentinel --set-json "$$apikeys"; \
		render api-example ./charts/accretion-api -f charts/accretion-api/values-example.yaml \
			--set-string config.database.password=$$sentinel; \
		render api-dev ./charts/accretion-api -f charts/accretion-api/values-dev.yaml \
			--set-string config.database.password=$$sentinel --set-json "$$apikeys"; \
		render api-staging ./charts/accretion-api -f charts/accretion-api/values-staging.yaml \
			--set-string config.database.password=$$sentinel --set-json "$$apikeys"; \
		render api-prod ./charts/accretion-api -f charts/accretion-api/values-prod.yaml \
			--set-string config.database.password=$$sentinel --set-json "$$apikeys"; \
		render collector-default ./charts/accretion-collector \
			--set-string config.database.password=$$sentinel --set-string redis.auth.password=$$sentinel; \
		render collector-scaled ./charts/accretion-collector $(COLLECTOR_SCALED) \
			--set-string config.database.password=$$sentinel --set-string redis.auth.password=$$sentinel; \
		render db ./charts/accretion-db -f charts/accretion-db/values.yaml \
			--set-string config.database.password=$$sentinel --set-string postgresql.auth.postgresPassword=$$sentinel; \
		render discord-enabled ./charts/accretion-discord --set enabled=true --set-string secret.botToken=$$sentinel; \
		render otel-values ./charts/otel-collector -f charts/otel-collector/values.yaml; \
		render otel-minimal ./charts/otel-collector --namespace obs -f charts/otel-collector/values-minimal.yaml; \
		bash scripts/check-credential-plaintext.sh "$$sentinel" $$d/*.yaml; \
		bash scripts/check-credential-plaintext.sh --selfcheck "$$sentinel"'
	@printf "%b\n" "$(GREEN)No chart renders a credential into a ConfigMap, a command, or args$(NC)"

# Detector for #113: with config.database.existingSecret / redis.auth.existingSecret
# set, the collector chart must never receive, render, or interpolate the real
# credential anywhere -- not even into a Secret it owns. credential-plaintext-check
# above explicitly does not cover Secret data (by design: that is where a credential
# is meant to live), so this is a separate, narrower check aimed at exactly the
# object that one skips. Proven non-vacuous three ways: (1) the DEFAULT
# (existingSecret unset) render is SCALED OUT, because the chart-owned redis-secret
# only exists where Redis does -- templates/secrets.yaml gates it on the same
# condition as the Redis server itself, so an unscaled render carries no
# redis-secret to have eyes on and this proof would pass over an object that was
# never there. The render is decoded and its chart-owned postgresql-secret and
# redis-secret are confirmed to actually carry the sentinel, so the detector below is
# proven to have eyes on real Secret content rather than passing on an empty check;
# (2) the SAME sentinel, passed again with existingSecret set, is grepped across the
# ENTIRE render and found nowhere -- no chart-owned Secret for that credential renders
# at all, and every consumer (the wait-for-postgres/wait-for-redis initContainers, the
# controller, data-collector and status-collector processes, and the Redis pod's own
# volume mount) is confirmed to reference the operator-supplied Secret name and key
# instead; (3) the render-time guards on an existingSecret with a blanked-out key name
# (templates/secrets.yaml) are proven to fire by name -- also under scale-out,
# since those guards live in the block that only renders where Redis does -- so a chart that silently
# dropped that assertion would be caught here instead of at pod rollout.
existing-secret-check: ## Verify existingSecret is honored: the collector chart never sees, renders, or leaks the real credential
	@printf "%b\n" "$(BLUE)Checking existingSecret wiring for the collector chart...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)"; exit 1; }; \
		sentinel="EXISTINGSECRET-SENTINEL-$$$$-DO-NOT-LEAK"; \
		helm template t ./charts/accretion-collector $(COLLECTOR_SCALED) \
			--set-string config.database.password=$$sentinel --set-string redis.auth.password=$$sentinel \
			> $$d/owned.yaml; \
		grep -q -- "-postgresql-secret$$" $$d/owned.yaml || fail "the chart-owned render does not render a postgresql-secret -- this check is vacuous"; \
		grep -q -- "-redis-secret$$" $$d/owned.yaml || fail "the chart-owned render does not render a redis-secret -- this check is vacuous"; \
		pgpw=$$(sed -n "/-postgresql-secret$$/,/^---/p" $$d/owned.yaml | grep "  password:" | head -1 | sed "s/.*password: *\"//;s/\"$$//" | base64 -d); \
		rdpw=$$(sed -n "/-redis-secret$$/,/^---/p" $$d/owned.yaml | grep "  password:" | head -1 | sed "s/.*password: *\"//;s/\"$$//" | base64 -d); \
		[ "$$pgpw" = "$$sentinel" ] || fail "decoded postgresql-secret password [$$pgpw] does not match the sentinel it was given -- the detector below cannot be trusted"; \
		[ "$$rdpw" = "$$sentinel" ] || fail "decoded redis-secret password [$$rdpw] does not match the sentinel it was given -- the detector below cannot be trusted"; \
		printf "%b\n" "  ok  default render puts the real value in the chart-owned Secrets (detector proven live)"; \
		helm template t ./charts/accretion-collector \
			--set-string config.database.password=$$sentinel --set-string redis.auth.password=$$sentinel \
			--set config.database.existingSecret=ext-postgres-creds --set config.database.existingSecretKey=db-pw \
			--set redis.auth.existingSecret=ext-redis-creds --set redis.auth.existingSecretPasswordKey=pw \
			--set redis.auth.existingSecretConfKey=conf.txt \
			--set dataCollector.replicaCount=3 \
			> $$d/external.yaml; \
		! grep -q "$$sentinel" $$d/external.yaml || fail "the real credential value rendered somewhere with existingSecret set -- the chart must never see it"; \
		! grep -qE -- "-postgresql-secret$$|-redis-secret$$" $$d/external.yaml || fail "a chart-owned Secret still rendered its name with existingSecret set"; \
		grep -q "name: ext-postgres-creds" $$d/external.yaml || fail "no container references the operator-supplied database Secret by name"; \
		grep -q "key: db-pw" $$d/external.yaml || fail "no container references the operator-supplied database Secret key"; \
		grep -q "name: ext-redis-creds" $$d/external.yaml || fail "no container references the operator-supplied Redis Secret by name"; \
		grep -q "key: pw" $$d/external.yaml || fail "no container references the operator-supplied Redis password key"; \
		grep -q "key: conf.txt" $$d/external.yaml || fail "the Redis pod does not mount the operator-supplied redis.conf key"; \
		printf "%b\n" "  ok  existingSecret set: no chart-owned Secret renders, the real value appears nowhere, and every consumer references the operator Secret by name and key"; \
		msg=$$(helm template t ./charts/accretion-collector --set config.database.existingSecret=x --set config.database.existingSecretKey= 2>&1 >/dev/null || true); \
		case "$$msg" in *"existingSecretKey is empty"*) ;; *) fail "an existingSecret with a blanked-out existingSecretKey was not rejected -- the assert-the-key-name guard is inert: $$msg";; esac; \
		printf "%b\n" "  ok  config.database.existingSecret with an empty existingSecretKey is rejected by name"; \
		msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set redis.auth.existingSecret=x --set redis.auth.existingSecretPasswordKey= 2>&1 >/dev/null || true); \
		case "$$msg" in *"existingSecretPasswordKey is empty"*) ;; *) fail "an existingSecret with a blanked-out existingSecretPasswordKey was not rejected: $$msg";; esac; \
		printf "%b\n" "  ok  redis.auth.existingSecret with an empty existingSecretPasswordKey is rejected by name"; \
		msg=$$(helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED) --set redis.auth.existingSecret=x --set redis.auth.existingSecretConfKey= 2>&1 >/dev/null || true); \
		case "$$msg" in *"existingSecretConfKey is empty"*) ;; *) fail "an existingSecret with a blanked-out existingSecretConfKey was not rejected: $$msg";; esac; \
		printf "%b\n" "  ok  redis.auth.existingSecret with an empty existingSecretConfKey is rejected by name"'
	@printf "%b\n" "$(GREEN)existingSecret is honored: the collector chart never sees, renders, or leaks the credential$(NC)"

# Detector for #114, broader than rbac-check's collector-only assertion (#108
# removed the only rule granting verbs on secrets and inverted
# check-collector-rbac.sh to fail if one reappears, but only for the collector
# chart). accretion-api, accretion-collector, accretion-discord and
# otel-collector are each rendered in every environment, and
# scripts/check-secrets-grant.sh asserts no Role or ClusterRole in any of them
# grants any verb on secrets, reporting how many it inspected so an empty
# render cannot pass vacuously. The one documented exception (accretion-mcp
# reading its own Secret by name) is allowed only as `get` plus an explicit,
# NON-EMPTY resourceNames -- never a blanket rule -- and is named when it
# fires. Proven non-vacuous against four shapes: a hardcoded template's
# single-line flow style; Helm's `toYaml` on a values-driven list
# (accretion-discord's rbac.rules), which renders in block style instead; a
# cluster-admin-shaped wildcard (apiGroups/resources/verbs all "*", which
# Helm's toYaml single-quotes); and a get-on-secrets rule with an EMPTY
# resourceNames list, which is a blanket grant, not the documented exception
# (cycle-1 review, both found live and fixed in scripts/check-secrets-grant.sh).
rbac-secrets-check: ## Verify no ServiceAccount in any chart holds a verb on secrets
	@printf "%b\n" "$(BLUE)Checking no chart grants a Role/ClusterRole verb on secrets...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		zero=$(ZERO_DIGEST); \
		apikeys="apiKeys=[{\"name\":\"ci\",\"hash\":\"$$zero\",\"permissions\":[\"read:memory\"]}]"; \
		render() { \
			out="$$1"; shift; \
			if ! helm template t "$$@" > "$$d/$$out.yaml" 2>"$$d/$$out.err"; then \
				printf "%b\n" "$(RED)helm template failed for $$out [$$*]:$(NC)"; cat "$$d/$$out.err"; exit 1; \
			fi; \
		}; \
		render api-values ./charts/accretion-api -f charts/accretion-api/values.yaml \
			--set-string config.database.password=x --set-json "$$apikeys"; \
		render api-dev ./charts/accretion-api -f charts/accretion-api/values-dev.yaml \
			--set-string config.database.password=x --set-json "$$apikeys"; \
		render api-staging ./charts/accretion-api -f charts/accretion-api/values-staging.yaml \
			--set-string config.database.password=x --set-json "$$apikeys"; \
		render api-prod ./charts/accretion-api -f charts/accretion-api/values-prod.yaml \
			--set-string config.database.password=x --set-json "$$apikeys"; \
		render api-example ./charts/accretion-api -f charts/accretion-api/values-example.yaml \
			--set-string config.database.password=x; \
		render collector-default ./charts/accretion-collector $(DB_PASSWORD_RENDER); \
		render collector-scaled ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_SCALED); \
		render discord-enabled ./charts/accretion-discord --set enabled=true --set-string secret.botToken=x --set rbac.create=true \
			--set-json "rbac.rules=[{\"apiGroups\":[\"\"],\"resources\":[\"pods\"],\"verbs\":[\"get\",\"list\"]}]"; \
		render otel-values ./charts/otel-collector -f charts/otel-collector/values.yaml; \
		render otel-minimal ./charts/otel-collector --namespace obs -f charts/otel-collector/values-minimal.yaml; \
		bash scripts/check-secrets-grant.sh $$d/*.yaml; \
		awk "!done && /^- apiGroups: \[\"\"\]\$$/ { c=1 } c && /^  resources: \[\"namespaces\"\]\$$/ { sub(/namespaces/,\"secrets\"); c=0; done=1 } { print }" \
			$$d/collector-scaled.yaml > $$d/tainted-flow.yaml; \
		grep -q "resources: \[\"secrets\"\]" $$d/tainted-flow.yaml || { printf "%b\n" "$(RED)the flow-style proof did not inject a secrets rule -- the proof itself is inert$(NC)"; exit 1; }; \
		proof=$$(bash scripts/check-secrets-grant.sh $$d/tainted-flow.yaml 2>&1 || true); \
		case "$$proof" in \
			*"grants a verb on secrets"*) ;; \
			*) printf "%b\n" "$(RED)a ClusterRole granting secrets (flow-style render) was not rejected -- the assertion is inert: $$proof$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  flow-style secrets grant proven to fire (rejected by name)"; \
		awk "!done && /^    - pods\$$/ { sub(/pods/,\"secrets\"); done=1 } { print }" $$d/discord-enabled.yaml > $$d/tainted-block.yaml; \
		grep -q "^    - secrets$$" $$d/tainted-block.yaml || { printf "%b\n" "$(RED)the block-style proof did not inject a secrets rule -- the proof itself is inert$(NC)"; exit 1; }; \
		proof=$$(bash scripts/check-secrets-grant.sh $$d/tainted-block.yaml 2>&1 || true); \
		case "$$proof" in \
			*"grants a verb on secrets"*) ;; \
			*) printf "%b\n" "$(RED)a ClusterRole granting secrets (block-style render, e.g. toYaml on values-driven rules) was not rejected -- the assertion is inert: $$proof$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  block-style secrets grant proven to fire (rejected by name)"; \
		printf "%s\n" "apiVersion: rbac.authorization.k8s.io/v1" "kind: ClusterRole" "metadata:" "  name: proof-wildcard" "rules:" "- apiGroups: [\"*\"]" "  resources: [\"*\"]" "  verbs: [\"*\"]" > $$d/proof-wildcard.yaml; \
		proof=$$(bash scripts/check-secrets-grant.sh $$d/proof-wildcard.yaml 2>&1 || true); \
		case "$$proof" in \
			*"grants a verb on secrets"*) ;; \
			*) printf "%b\n" "$(RED)a cluster-admin-shaped wildcard rule (apiGroups/resources/verbs all *) was not rejected -- the wildcard assertion is inert: $$proof$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  wildcard cluster-admin rule proven to fire (rejected by name)"; \
		printf "%s\n" "apiVersion: rbac.authorization.k8s.io/v1" "kind: ClusterRole" "metadata:" "  name: proof-empty-resourcenames" "rules:" "- apiGroups: [\"\"]" "  resources: [\"secrets\"]" "  resourceNames: []" "  verbs: [\"get\"]" > $$d/proof-empty-rn.yaml; \
		proof=$$(bash scripts/check-secrets-grant.sh $$d/proof-empty-rn.yaml 2>&1 || true); \
		case "$$proof" in \
			*"no resourceNames"*) ;; \
			*) printf "%b\n" "$(RED)get on secrets with an EMPTY resourceNames list was not rejected as a blanket grant -- the resourceNames-count assertion is inert: $$proof$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  get+empty-resourceNames on secrets proven to fire as a blanket grant, not the documented exception"'
	@printf "%b\n" "$(GREEN)No Role or ClusterRole in any chart grants a verb on secrets$(NC)"

release: clean ci docker-build-cached ## Prepare release
	@echo "$(GREEN)Release preparation completed$(NC)"

require-env: ## Verify a local .env (secrets) exists — prerequisite for run/tilt targets
	@if [ ! -f .env ]; then \
		echo "$(RED)✗ No .env found — this target needs your local secrets.$(NC)"; \
		echo "$(YELLOW)Set it up:$(NC)"; \
		echo "  1) cp env.example .env"; \
		echo "  2) fill in ACCRETION_LLM_OPENAI_API_KEY, ACCRETION_LLM_ANTHROPIC_API_KEY, DISCORD_BOT_TOKEN, ..."; \
		echo "  3) direnv allow   (loads .envrc, which pulls in .env)"; \
		exit 1; \
	fi

secrets: require-env ## Create/refresh Secrets in $(NAMESPACE) from .env (accretion-api, accretion-mcp, accretion-discord; accretion-db in its own namespace when that namespace exists)
	@echo "$(BLUE)Verifying API-key identities against .env...$(NC)"
	@bash scripts/check-api-key-hashes.sh $(IDENTITIES_FILE)
	@echo "$(BLUE)Syncing Secrets into $(NAMESPACE)...$(NC)"
# make-secret.sh also publishes accretion-db-secrets into the database's own
# namespace (default accretion-db), but only when it exists -- a cloud install on
# Cloud SQL has no such namespace, so the script skips it by name rather than
# half-applying and failing (#146). accretion-mcp-secrets and accretion-discord-secrets
# are each skipped loudly when .env sets none of their keys.
	bash scripts/make-secret.sh --namespace $(NAMESPACE)
	@echo "$(GREEN)Secrets synced (accretion-api-secrets, plus accretion-mcp/-discord and accretion-db when applicable)$(NC)"

# The failure this guards against is silent: every mismatched key just leaves an
# env var unset, and the API 401s. Compare the keys the script emits against the
# keys the chart's rendered secretKeyRefs actually ask for. (toYaml renders map
# keys alphabetically, so the `key:` line precedes its `name:` line.)
secrets-check: secrets-functional-check secrets-identity-check secrets-keys-check secrets-pulumi-check secrets-portability-check secrets-stale-check secrets-strength-check ## Run every make-secret.sh guard

# test-functional used to default an unresolvable credential to the empty
# string, so a missing key produced 401s that named nothing and a silently
# SKIPPED TestAPIKeyScoping. This is the gate that keeps the default from coming
# back, and it needs no cluster: it drives the real target both ways under
# `env -i` and reads which of two mutually exclusive messages comes out.
#
# The positive control is the point. A run with no keys must stop AT the
# credential check; a run WITH keys must get past it and die further on, at the
# port-forward health wait. Asserting only the first would pass just as well if
# the target were broken outright and never reached anything, which is how a
# check comes to prove nothing. Both runs assert the other run's message is
# absent, so neither can be satisfied by a target that always fails the same way.
#
# KUBECONFIG=/dev/null so the positive control cannot reach a real cluster and
# start running the actual suite on a developer's machine, and FUNCTIONAL_WAIT=1
# so it reaches its failure in a second rather than twenty. FUNCTIONAL_WAIT is a
# command-line override rather than an exported variable precisely so `env -i`
# cannot strip it.
#
# HOME is passed for output hygiene, not for the run. Each recursive make
# re-parses this file, and GOCACHE/GOMODCACHE above are $(shell go env ...),
# which prints "error loading config: $HOME is not defined" twice per parse
# without it -- errors in the log of a PASSING gate, which is the same class
# this sprint keeps finding: something that reads as a signal and is not,
# training the next reader to skip the place a real signal would appear. It
# costs no hermeticity: the credentials are passed explicitly on each line
# below, KUBECONFIG still points at nothing, and --print-identity-var never
# opens an env file.
#
# It hangs off secrets-check, not `make ci`, and the name says which family it
# belongs to. `make ci` is not what CI runs: .github/workflows/ci.yml enumerates
# its own steps and `make ci` appears there only inside a comment, so a gate
# reachable only from that target is inert exactly where a regression would
# arrive from someone else. That is #102 verbatim -- profiles-check spent a
# sprint in `make ci` while CI never ran it -- and the charts-check comment
# above already draws the conclusion: hang a gate off an aggregate the workflow
# calls, and it runs in both places or neither. secrets-check is that aggregate
# here, reached by charts-check, which ci.yml's charts job runs directly.
#
# secrets-check is also the honest family rather than merely a convenient one:
# every guard under it asserts that the .env -> credential path holds across a
# boundary make-secret.sh cannot see on its own. secrets-keys-check checks the
# chart's secretKeyRefs against the keys the script emits; secrets-pulumi-check
# checks pulumi's Secret against the same; this one checks the Makefile consumer
# that turns an identity into the plaintext it sends.
secrets-functional-check: ## Verify test-functional refuses to run without a real API key rather than defaulting to an empty one
	@printf "%b\n" "$(BLUE)Checking test-functional's credential resolution...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		sentinel=functional-key-DO-NOT-PRINT-$$RANDOM; \
		ids="accretion-mcp ci"; n=0; \
		for id in $$ids; do \
			v=$$(bash scripts/make-secret.sh --print-identity-var $$id); \
			case "$$v" in \
				ACCRETION_API_KEY_*) ;; \
				*) printf "%b\n" "$(RED)identity $$id resolves to \"$$v\", which is not an API-key variable$(NC)"; exit 1;; \
			esac; \
			printf "%b\n" "  ok  $$id -> $$v"; n=$$((n+1)); \
		done; \
		[ "$$n" -eq 2 ] || { printf "%b\n" "$(RED)resolved $$n identities, expected 2 — the loop did not run$(NC)"; exit 1; }; \
		if bash scripts/make-secret.sh --print-identity-var not-an-identity >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)an unknown identity name resolved instead of failing, so a renamed identity would yield an empty variable name$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  an unknown identity is an error, not an empty variable name"; \
		env -i PATH="$$PATH" HOME="$$HOME" KUBECONFIG=/dev/null $(MAKE) test-functional FUNCTIONAL_WAIT=1 >$$d/none.out 2>$$d/none.err || true; \
		grep -q "no API key for the functional suite" $$d/none.err || { \
			printf "%b\n" "$(RED)test-functional with no credential in the environment did not refuse to run$(NC)"; cat $$d/none.err; exit 1; }; \
		grep -q "ACCRETION_API_KEY_MCP" $$d/none.err || { \
			printf "%b\n" "$(RED)the refusal does not name the variable to set$(NC)"; cat $$d/none.err; exit 1; }; \
		if grep -q "waiting for /api/health" $$d/none.out $$d/none.err; then \
			printf "%b\n" "$(RED)test-functional went on to port-forward with no credential — the check does not gate the run$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  no credential: refuses by name, before the port-forward"; \
		bad=0; \
		for shape in "$$sentinel with a space" "<accretion-core:api-key-accretion-mcp>" "op://vault/accretion/$$sentinel"; do \
			env -i PATH="$$PATH" HOME="$$HOME" KUBECONFIG=/dev/null \
				ACCRETION_API_KEY_MCP="$$shape" ACCRETION_API_KEY_CI="$$shape" \
				$(MAKE) test-functional FUNCTIONAL_WAIT=1 >$$d/shape.out 2>$$d/shape.err || true; \
			grep -q "does not hold an API key" $$d/shape.err || { \
				printf "%b\n" "$(RED)a value that cannot be a key was accepted as one — the suite would 401 instead of naming it$(NC)"; cat $$d/shape.err; exit 1; }; \
			grep -q "ACCRETION_API_KEY_MCP" $$d/shape.err || { \
				printf "%b\n" "$(RED)the shape refusal does not name the variable holding the bad value$(NC)"; cat $$d/shape.err; exit 1; }; \
			if grep -q "waiting for /api/health" $$d/shape.out $$d/shape.err; then \
				printf "%b\n" "$(RED)test-functional went on to port-forward with a placeholder credential$(NC)"; exit 1; \
			fi; \
			if grep -q "$$sentinel" $$d/shape.out $$d/shape.err; then \
				printf "%b\n" "$(RED)the shape refusal printed the VALUE it rejected$(NC)"; exit 1; \
			fi; \
			bad=$$((bad+1)); \
		done; \
		[ "$$bad" -eq 3 ] || { printf "%b\n" "$(RED)checked $$bad unusable shapes, expected 3 — the loop did not run$(NC)"; exit 1; }; \
		printf "%b\n" "  ok  $$bad unusable shapes (whitespace, k8s ref, secret-manager URI) refused by name"; \
		env -i PATH="$$PATH" HOME="$$HOME" KUBECONFIG=/dev/null \
			ACCRETION_API_KEY_MCP=$$sentinel ACCRETION_API_KEY_CI=$$sentinel \
			$(MAKE) test-functional FUNCTIONAL_WAIT=1 >$$d/keys.out 2>$$d/keys.err || true; \
		if grep -q "no API key for the functional suite\|no read-only API key\|does not hold an API key" $$d/keys.out $$d/keys.err; then \
			printf "%b\n" "$(RED)test-functional refused even with both identity variables set — either the resolution never reads them, or the shape check rejects a usable key$(NC)"; cat $$d/keys.err; exit 1; \
		fi; \
		grep -q "waiting for /api/health" $$d/keys.out || { \
			printf "%b\n" "$(RED)test-functional did not reach the port-forward with both variables set, so the run above proves nothing about the credential check$(NC)"; cat $$d/keys.out $$d/keys.err; exit 1; }; \
		if grep -q "$$sentinel" $$d/none.out $$d/none.err $$d/keys.out $$d/keys.err; then \
			printf "%b\n" "$(RED)test-functional printed an API key VALUE$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  both identity variables set: resolves and proceeds, printing no key value"'
	@printf "%b\n" "$(GREEN)test-functional resolves its credentials from .env and refuses to run without them$(NC)"

# #122 split one input into two that can now disagree: the plaintext each client
# sends lives in .env, the SHA-256 digest the server compares against lives in
# the chart's apiKeys list. When they diverge the only symptom is a 401 on every
# request from that caller, with nothing in any log, manifest or render saying
# why. scripts/make-secret.sh documented excluding API keys for exactly this
# reason, so the split is paid for with a gate rather than a comment.
#
# CI runs the SELFCHECK, not the real comparison: .env is gitignored, so a real
# run would pass or fail depending on whether the machine happens to hold a
# credential -- non-deterministic, and the "skip when absent" shape this
# repository has been bitten by before. The real comparison runs in `make
# secrets`, where the real data is, before either Secret is created.
secrets-identity-check: ## Verify each identity's hash is the digest of its plaintext in .env
	@bash scripts/check-api-key-hashes.sh --selfcheck

# BOTH NEIGHBOURS of the `name:` line are scanned, not just the one above it
# (#134). The env entries generated from the ConfigMap come out of `toYaml`,
# which sorts map keys, so `key:` precedes `name:` there -- but the database
# password's secretKeyRef is written by hand in templates/deployment.yaml with
# `name:` first, and a -B1-only scan simply does not see it. Reading both sides
# is what lets db-password be compared like every other key rather than being
# the one key the gate is structurally blind to. Within a secretKeyRef block the
# only lines adjacent to `name:` are `key:`, `name:` and `optional:`, so looking
# both ways adds no false positives TODAY -- and `refs` does not trust that it
# will stay true. It counts the `name: <secret>` lines and requires the
# extraction to yield exactly that many keys, so a future hand-written block
# with an `optional:` sitting between `name:` and `key:` fails the gate rather
# than vanishing from it. A key only the chart reads, missed here, is a silent
# pass and a pod that fails at rollout -- the very failure this gate exists to
# prevent, arriving through the gate itself.
#
# TWO Secrets since #135. accretion-discord-secrets was previously beyond this
# gate's reach for a real reason: the script emitted only one of its four keys,
# and the other three arrived as plaintext `--set` flags out of the Tiltfile, so
# there was no script-side list to diff against. Now that the script owns all
# four, the same comparison applies -- exact in both directions, because a key
# the bot reads and the script does not emit fails the pod at rollout, while a
# key the script emits and nothing reads is dead weight that outlives whatever
# removed its reader. The bot is rendered from the REAL deploy/tilt.yaml
# subtree, read through `scripts/check-values-matrix.sh --print-subtree`, so the
# values under test are the values that deploy. That mode exists because this
# recipe first grew its own copy of that script's values_subtree_file -- same
# synthetic chart, same toJson template, same sed -- and dropped the per-key
# caching and the `local`-expansion note the original had learned. Two readers
# of "one chart's subtree out of the central file" is the drift these gates
# exist to prevent, so there is one, and that script's selfcheck already covers
# it. The count assertions (4 for discord, 2 for accretion-db) are the other
# vacuity guard: a chart that stopped reading a key would otherwise agree with a
# script that stopped emitting it.
#
# THREE Secrets are checked the same way through the shared refs() helper:
# accretion-api-secrets, accretion-discord-secrets, and accretion-db-secrets --
# the last folded in here (was a separate secrets-db-keys-check) so there is one
# reader of the subtree render and one refs() with its want/got vacuity assertion,
# not a cloned, weaker copy. accretion-db-secrets is rendered from the same
# deploy/tilt.yaml accretion-db subtree the Tiltfile passes, so the chart's
# existingSecretKey, Tilt's copy of it, and the Bitnami subchart's
# adminPasswordKey all flow through the one render and a rename on any shows up.
secrets-keys-check: ## Verify make-secret.sh emits exactly the Secret keys the charts read (accretion-api, -discord, -db)
	@echo "$(BLUE)Diffing script Secret keys against the charts' secretKeyRefs...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)" >&2; exit 1; }; \
		refs() { \
			{ grep -B1 "name: $$2" "$$1"; grep -A1 "name: $$2" "$$1"; } \
				| grep -oE "key: [a-z0-9-]+" | awk "{print \$$2}" > $$d/raw; \
			want=$$(grep -c "name: $$2" "$$1" || true); \
			got=$$(wc -l < $$d/raw | tr -d " "); \
			[ "$$got" -eq "$$want" ] || fail "$$1 references $$2 $$want times but only $$got of those refs yielded a key. The extraction reads the line either side of name:, so a secretKeyRef whose key: sits further away (an optional: between them, say) is INVISIBLE to it -- and a key only the chart reads, missed here, is a silent pass and a pod that fails at rollout."; \
			sort -u $$d/raw; \
		}; \
		bash scripts/make-secret.sh --print-keys | sort -u > $$d/script; \
		helm template $(RELEASE_NAME) ./charts/accretion-api -f charts/accretion-api/values-dev.yaml \
			--set-json "apiKeys=[{\"name\":\"ci\",\"hash\":\"$(ZERO_DIGEST)\",\"permissions\":[\"read:memory\"]}]" \
			> $$d/render.yaml; \
		refs $$d/render.yaml accretion-api-secrets > $$d/chart; \
		if ! diff -u $$d/chart $$d/script; then \
			fail "Secret key mismatch: < chart expects, > script emits"; \
		fi; \
		grep -oE "Secret key: [^[:space:]]+" env.example | awk "{print \$$3}" | sort -u > $$d/envexample; \
		if ! diff -u $$d/script $$d/envexample; then \
			fail "env.example annotations drifted: < script emits, > env.example documents"; \
		fi; \
		printf "%b\n" "  ok  accretion-api-secrets: $$(wc -l < $$d/script | tr -d " ") keys match the chart, and env.example documents them"; \
		bash scripts/make-secret.sh --print-discord-keys | sort -u > $$d/dscript; \
		bash scripts/check-values-matrix.sh --print-subtree deploy/tilt.yaml accretion-discord > $$d/dvalues.json; \
		helm template accretion-discord ./charts/accretion-discord -f $$d/dvalues.json > $$d/discord.yaml; \
		refs $$d/discord.yaml accretion-discord-secrets > $$d/dchart; \
		if ! diff -u $$d/dchart $$d/dscript; then \
			fail "accretion-discord-secrets key mismatch: < the bot reads, > the script emits"; \
		fi; \
		n=$$(wc -l < $$d/dchart | tr -d " "); \
		[ "$$n" -eq 4 ] || fail "the bot reads $$n keys from accretion-discord-secrets, expected 4 (its own API key, the Discord token and the two model keys). A render that reads fewer is not proof of agreement, it is a chart that stopped asking."; \
		printf "%b\n" "  ok  accretion-discord-secrets: the bot reads exactly the 4 keys the script emits"; \
		bash scripts/make-secret.sh --print-db-keys | sort -u > $$d/dbscript; \
		bash scripts/check-values-matrix.sh --print-subtree deploy/tilt.yaml accretion-db > $$d/dbvalues.json; \
		helm template t ./charts/accretion-db -f $$d/dbvalues.json > $$d/db.yaml; \
		refs $$d/db.yaml accretion-db-secrets > $$d/dbchart; \
		if ! diff -u $$d/dbchart $$d/dbscript; then \
			fail "accretion-db-secrets key mismatch: < the chart, Tilt and the subchart read via secretKeyRef, > the script emits. A rename on either side (config.database.existingSecretKey, deploy/tilt.yaml, the Bitnami auth.secretKeys.adminPasswordKey, or DB_PASSWORD_KEY / DB_ADMIN_PASSWORD_KEY) fails the schema job and the StatefulSet at rollout with nothing pointing back here."; \
		fi; \
		n=$$(wc -l < $$d/dbchart | tr -d " "); \
		[ "$$n" -eq 2 ] || fail "accretion-db-secrets is read with $$n distinct keys in the render, expected 2 (postgres-password for the Postgres StatefulSet via the Bitnami subchart, db-password for the schema job and the partition cronjob). A render that reads fewer is a chart that stopped asking, not agreement."; \
		printf "%b\n" "  ok  accretion-db-secrets: the chart, Tilt and the subchart read exactly the 2 keys the script emits (via deploy/tilt.yaml existingSecretKey and the subchart adminPasswordKey)"; \
		printf "%b\n" "$(GREEN)Secret keys match the charts, and env.example documents them$(NC)"'

# The declare -A regression (#54): macOS ships bash 3.2 as /bin/bash and Tilt
# invokes the script as `bash scripts/make-secret.sh`, bypassing the shebang.
# --print-keys exits before the code that broke, so this drives the full path
# via the missing-required branch and asserts the script's own error surfaces
# rather than a bash syntax error. env -i so a developer's exported keys cannot
# satisfy the check being made to fail.
secrets-portability-check: ## Verify make-secret.sh runs under /bin/bash 3.2 (macOS, Tilt)
	@echo "$(BLUE)Running make-secret.sh under /bin/bash...$(NC)"
	@bash -c 'out=$$(env -i PATH="$$PATH" /bin/bash scripts/make-secret.sh --env-file /dev/null --dry-run 2>&1 || true); \
		case "$$out" in \
			*"is missing values the chart requires"*) ;; \
			*) printf "%b\n" "$(RED)make-secret.sh is not portable to /bin/bash:$(NC)"; printf "%s\n" "$$out"; exit 1 ;; \
		esac; \
		printf "%b\n" "$(GREEN)make-secret.sh runs under /bin/bash $$(/bin/bash --version | head -1 | grep -oE "version [0-9.]+")$(NC)"'

# The FOURTH copy of the .env -> Secret-key mapping lives in pulumi (#55), and
# until now no gate covered it. SUBSET, not equality: pulumi provisions keys
# that make-secret.sh has no business emitting, so pulumi-only keys are fine and
# only a key the script emits but pulumi lacks is drift. The extracted list is
# asserted non-empty, so renaming the Go literal cannot make this pass by
# matching nothing against nothing.
#
# TWO extractions, not one, since #134 put db-password on both sides. secrets.go
# writes most keys as quoted literals but names the database one through the
# `config.DBPasswordKey` constant -- deliberately, because the charts' own
# existingSecretKey values are built from the same constant and a literal here
# could drift from them. A literal-only scan therefore reported pulumi as
# missing a key it has provisioned all along. Identifier-keyed entries are
# resolved through their string constant in pulumi/pkg/config, and an identifier
# that resolves to NOTHING is a hard failure rather than a silently dropped row:
# dropping it is exactly how this gate would go back to reporting a false
# missing key, or worse, stop noticing a real one.
# BOTH Secrets since #135. accretion-discord-secrets was explicitly out of this
# gate's reach, and values.go said so in two parts: the script emitted none of
# those keys, and the extraction above matches literal `"key": pulumi.String()`
# pairs, which building a Secret from a []string does not produce. The first
# half stopped being true when the script took ownership of the bot token and
# the model keys, so the list is read from DiscordSecretKeys() directly -- its
# string literals, plus the api-key-<identity> entry rebuilt the way
# DiscordAPIKeySecretKey() builds it. That last bit is the one duplication here
# and it is named: the "api-key-" prefix lives in that function too, and the two
# must move together.
secrets-pulumi-check: ## Verify pulumi provisions every Secret key make-secret.sh emits
	@echo "$(BLUE)Diffing script Secret keys against pulumi's Secret...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		bash scripts/make-secret.sh --print-keys | sort -u > $$d/script; \
		sed -nE "s/.*\"([a-z0-9-]+)\":[[:space:]]*pulumi\.String.*/\1/p" \
			pulumi/pkg/kubernetes/secrets.go > $$d/pulumi.raw; \
		sed -nE "s/.*config\.([A-Za-z0-9_]+):[[:space:]]*pulumi\.String.*/\1/p" \
			pulumi/pkg/kubernetes/secrets.go | sort -u > $$d/pulumi.idents; \
		while read -r ident; do \
			[ -n "$$ident" ] || continue; \
			v=$$(sed -nE "s/^[[:space:]]*$$ident[[:space:]]*=[[:space:]]*\"([a-z0-9-]+)\".*/\1/p" pulumi/pkg/config/*.go | head -1); \
			if [ -z "$$v" ]; then \
				printf "%b\n" "$(RED)pulumi/pkg/kubernetes/secrets.go provisions config.$$ident, but no string constant of that name is declared in pulumi/pkg/config — this key cannot be compared, and dropping it would make the gate report a missing key that is not missing (or miss one that is)$(NC)"; exit 1; \
			fi; \
			printf "%s\n" "$$v" >> $$d/pulumi.raw; \
		done < $$d/pulumi.idents; \
		sort -u $$d/pulumi.raw > $$d/pulumi; \
		if [ ! -s $$d/pulumi ]; then \
			printf "%b\n" "$(RED)no Secret keys found in pulumi/pkg/kubernetes/secrets.go — this check would pass vacuously$(NC)"; exit 1; \
		fi; \
		comm -23 $$d/script $$d/pulumi > $$d/only; \
		if [ -s $$d/only ]; then \
			printf "%b\n" "$(RED)pulumi/pkg/kubernetes/secrets.go is missing Secret keys make-secret.sh emits:$(NC)"; \
			sed "s/^/  /" $$d/only; exit 1; \
		fi; \
		printf "%b\n" "  ok  accretion-api-secrets: pulumi provisions all $$(wc -l < $$d/script | tr -d " ") script Secret keys"; \
		bash scripts/make-secret.sh --print-discord-keys | sort -u > $$d/dscript; \
		awk "/^func DiscordSecretKeys\(\)/,/^}/" pulumi/pkg/config/values.go > $$d/dk.go; \
		sed -nE "s/^[[:space:]]*\"([a-z0-9-]+)\",?[[:space:]]*\$$/\1/p" $$d/dk.go > $$d/dpulumi.raw; \
		if grep -q "DiscordAPIKeySecretKey()" $$d/dk.go; then \
			id=$$(sed -nE "s/^[[:space:]]*DiscordIdentity[[:space:]]*=[[:space:]]*\"([a-z0-9-]+)\".*/\1/p" pulumi/pkg/config/values.go | head -1); \
			if [ -z "$$id" ]; then \
				printf "%b\n" "$(RED)DiscordSecretKeys() calls DiscordAPIKeySecretKey(), but no DiscordIdentity string constant is declared in pulumi/pkg/config/values.go, so that key cannot be resolved and would be silently dropped from this comparison$(NC)"; exit 1; \
			fi; \
			printf "api-key-%s\n" "$$id" >> $$d/dpulumi.raw; \
		fi; \
		sort -u $$d/dpulumi.raw > $$d/dpulumi; \
		if [ ! -s $$d/dpulumi ]; then \
			printf "%b\n" "$(RED)no Secret keys found in DiscordSecretKeys() — this half of the check would pass vacuously$(NC)"; exit 1; \
		fi; \
		comm -23 $$d/dscript $$d/dpulumi > $$d/donly; \
		if [ -s $$d/donly ]; then \
			printf "%b\n" "$(RED)pulumi/pkg/config/values.go DiscordSecretKeys() is missing keys make-secret.sh emits into accretion-discord-secrets:$(NC)"; \
			sed "s/^/  /" $$d/donly; exit 1; \
		fi; \
		printf "%b\n" "  ok  accretion-discord-secrets: pulumi provisions all $$(wc -l < $$d/dscript | tr -d " ") script Secret keys"; \
		printf "%b\n" "$(GREEN)pulumi provisions every Secret key make-secret.sh emits$(NC)"'

# #56: .env is gitignored, so no diff, review, or acceptance gate can see a
# developer's stale bare OPENAI_API_KEY / ANTHROPIC_API_KEY — a runtime warning
# is the only thing that catches it. Driven both ways so it cannot pass
# vacuously: the same env file with and without the stale line. Asserts the
# warning names the replacement, goes to stderr, leaves the exit code at 0, and
# never prints the value (a sentinel value must appear in neither stream).
#
# The value assertion is made TWICE, raw and base64-decoded, because --dry-run
# prints a Secret manifest and every `data:` value in one is encoded. A plaintext
# grep of that output cannot see a leaked credential at all -- it would be
# looking for a string that is, by construction, never there -- so the claim
# "never prints the value" read far more generally than what was tested. The
# decode closes that: a stale name whose value reached the Secret is now caught
# in the form it would actually appear in.
#
# The decode extraction is scoped to the `data:` block (`/^data:$/,/^[^ ]/`)
# rather than every two-space-indented `key: value` line (#153). The broad form
# also matched the `metadata:` block's `name:`/`namespace:` lines, whose values
# (e.g. `accretion-api-secrets`) are not base64. GNU `base64 -d` (Linux/CI)
# rejects non-base64 input with a non-zero exit; under `set -eo pipefail` that
# killed this gate with a messageless `make: *** Error 1`. BSD `base64` (macOS)
# tolerates the junk and returns 0, so it passed locally and failed only in CI.
#
# The fixture carries ACCRETION_DATABASE_PASSWORD since #134 for the same reason
# values-check's renders carry one: db-password joined the script's REQUIRED
# list, and that check runs before the warning this gate is about, so a fixture
# without it would exit 1 on a missing key and never reach the code under test.
secrets-stale-check: ## Verify make-secret.sh warns about bare pre-rename LLM key names (#56)
	@echo "$(BLUE)Checking the stale bare-env-name warning...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		sentinel=sk-DO-NOT-PRINT-$$RANDOM; \
		printf "OAUTH_CLIENT_ID=id\nOAUTH_CLIENT_SECRET=s\nACCRETION_AUTH_ALLOWED_DOMAINS=example.com\nACCRETION_DATABASE_PASSWORD=fixture-not-a-credential\nSESSION_KEY=%s\n" "$$(openssl rand -hex 32)" > $$d/clean; \
		cp $$d/clean $$d/stale; \
		printf "OPENAI_API_KEY=%s\nANTHROPIC_API_KEY=%s\n" "$$sentinel" "$$sentinel" >> $$d/stale; \
		env -i PATH="$$PATH" /bin/bash scripts/make-secret.sh --env-file $$d/stale --dry-run >$$d/out 2>$$d/err; \
		for v in OPENAI_API_KEY ANTHROPIC_API_KEY; do \
			grep -q "assigns $$v, which nothing reads any more" $$d/err || { \
				printf "%b\n" "$(RED)no stale-name warning for $$v on stderr$(NC)"; cat $$d/err; exit 1; }; \
			grep -q "Rename it to ACCRETION_LLM_$$v" $$d/err || { \
				printf "%b\n" "$(RED)stale-name warning for $$v does not name its replacement$(NC)"; exit 1; }; \
		done; \
		if grep -q "$$sentinel" $$d/out $$d/err; then \
			printf "%b\n" "$(RED)stale-name warning leaked the variable VALUE$(NC)"; exit 1; \
		fi; \
		sed -nE "/^data:\$$/,/^[^ ]/{s/^  [a-zA-Z0-9_.-]+: (.*)\$$/\1/p;}" $$d/out | tr -d "\"" \
			| while read -r b64; do printf "%s" "$$b64" | base64 -d 2>/dev/null; printf "\n"; done > $$d/decoded; \
		[ -s $$d/decoded ] || { printf "%b\n" "$(RED)the base64-decode assertion decoded NOTHING: either base64 -d is unavailable or the sed pattern stopped matching the Secret data lines (a kubectl output-format change, or a key name outside [a-zA-Z0-9_.-]). The grep below would then find nothing and report success while testing nothing.$(NC)"; exit 1; }; \
		if grep -q "$$sentinel" $$d/decoded; then \
			printf "%b\n" "$(RED)a stale bare name reached the Secret: the sentinel is absent from the raw output but present once the data values are base64-DECODED. The plaintext grep above cannot see that, which is exactly the shape a leak would take in a manifest whose values are all encoded.$(NC)"; exit 1; \
		fi; \
		if grep -q "warning:" $$d/out; then \
			printf "%b\n" "$(RED)warning went to stdout, which is the manifest$(NC)"; exit 1; \
		fi; \
		grep -q "^  session-key:" $$d/out || { printf "%b\n" "$(RED)a stale bare name changed the outcome of the run$(NC)"; exit 1; }; \
		env -i PATH="$$PATH" /bin/bash scripts/make-secret.sh --env-file $$d/clean --dry-run >/dev/null 2>$$d/cleanerr; \
		if grep -q "nothing reads any more" $$d/cleanerr; then \
			printf "%b\n" "$(RED)warning fires on an env file with no stale names — this check is vacuous$(NC)"; cat $$d/cleanerr; exit 1; \
		fi; \
		printf "%b\n" "$(GREEN)stale bare LLM key names warn to stderr, name the replacement, and do not fail the run$(NC)"'

# #64: a session key of the wrong strength must fail here, not as a crashloop.
# Driven both ways so the guard cannot pass vacuously: a 32-hex-char key (16
# bytes — the exact shape that caused the outage) must be rejected, and a real
# 64-hex-char key must reach a rendered manifest. env -i again so a developer's
# exported SESSION_KEY cannot satisfy the case being made to fail.
#
# The non-ASCII case is the locale trap: bracket RANGES like [0-9a-fA-F] are
# matched by the locale's collation order, under which accented Latin letters
# collate inside a-f. A ranged version of this guard accepts such a key and the
# server then rejects it — the crashloop #64 exists to prevent. It must therefore
# run under a UTF-8 locale: under LC_ALL=C the buggy version passes and the test
# proves nothing, exactly like testing with `bash` instead of `/bin/bash`.
# The fixtures carry ACCRETION_DATABASE_PASSWORD since #134, in the printf
# FORMAT string rather than in $$w: db-password joined the script's REQUIRED
# list and that check runs before the session-key guard, so a fixture without it
# exits on a missing key and never reaches the code under test. $$w is a `%s`
# argument, and an escape inside an argument is never interpreted -- putting the
# line there writes a literal backslash-n into the env file, which parses as one
# unusable variable name and leaves the whitelist unset too.
secrets-strength-check: ## Verify make-secret.sh rejects a weak or non-hex session key (#64)
	@echo "$(BLUE)Checking the session-key strength guard...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		w="ACCRETION_AUTH_ALLOWED_DOMAINS=example.com"; \
		printf "OAUTH_CLIENT_ID=id\nOAUTH_CLIENT_SECRET=s\nACCRETION_DATABASE_PASSWORD=fixture-not-a-credential\n%s\nSESSION_KEY=%s\n" "$$w" "$$(openssl rand -hex 16)" > $$d/weak; \
		printf "OAUTH_CLIENT_ID=id\nOAUTH_CLIENT_SECRET=s\nACCRETION_DATABASE_PASSWORD=fixture-not-a-credential\n%s\nSESSION_KEY=%s\n" "$$w" "$$(openssl rand -hex 32)" > $$d/good; \
		if out=$$(env -i PATH="$$PATH" /bin/bash scripts/make-secret.sh --env-file $$d/weak --dry-run 2>&1); then \
			printf "%b\n" "$(RED)session-key guard is vacuous: a 16-byte key was accepted$(NC)"; exit 1; \
		fi; \
		case "$$out" in \
			*"32 hexadecimal characters, decoding to 16 bytes"*) ;; \
			*) printf "%b\n" "$(RED)session-key guard did not report both counts:$(NC)"; printf "%s\n" "$$out"; exit 1 ;; \
		esac; \
		case "$$out" in *"openssl rand -hex 32"*) ;; \
			*) printf "%b\n" "$(RED)session-key guard did not name the remediation$(NC)"; exit 1 ;; \
		esac; \
		env -i PATH="$$PATH" /bin/bash scripts/make-secret.sh --env-file $$d/good --dry-run \
			| grep -q "^  session-key:" || { \
			printf "%b\n" "$(RED)session-key guard rejects a valid 64-hex-char key$(NC)"; exit 1; }; \
		printf "OAUTH_CLIENT_ID=id\nOAUTH_CLIENT_SECRET=s\nACCRETION_DATABASE_PASSWORD=fixture-not-a-credential\n%s\nSESSION_KEY=%s\n" "$$w" "ÁBCDEF0000000000000000000000000000000000000000000000000000000000" > $$d/accented; \
		if env -i PATH="$$PATH" LC_ALL=en_US.UTF-8 /bin/bash scripts/make-secret.sh --env-file $$d/accented --dry-run >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)session-key guard accepts non-ASCII as hex: the hex class must be enumerated, not a locale-collated range$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "$(GREEN)session-key guard rejects a 16-byte key and non-ASCII under UTF-8, and accepts a 32-byte one$(NC)"'

security: ## Run security checks
	@echo "$(BLUE)Running security checks...$(NC)"
	@if command -v gosec >/dev/null 2>&1; then \
		gosec -exclude-dir=vendor -exclude-dir=.gopath -severity medium ./...; \
	else \
		printf "%b\n" "$(RED)gosec not found — the security check cannot run.$(NC)"; \
		printf "%b\n" "$(YELLOW)Install it: make go-tools$(NC)"; \
		exit 1; \
	fi

setup-namespace: ## Create development namespace if it doesn't exist
	@echo "$(BLUE)Setting up development namespace...$(NC)"
	kubectl create namespace development --dry-run=client -o yaml | kubectl apply -f -
	@echo "$(GREEN)Development namespace ready$(NC)"

size: build ## Show binary size
	@echo "$(BLUE)Binary size analysis:$(NC)"
	@ls -lh $(BUILD_DIR)/$(BINARY_NAME)
	@echo "Compressed size: $$(gzip -c $(BUILD_DIR)/$(BINARY_NAME) | wc -c | numfmt --to=iec)"

test: deps-verify ## Run tests with caching
	@echo "$(BLUE)Running tests with caching...$(NC)"
	mkdir -p $(CACHE_DIR)/test
	go test $(GOFLAGS) $(TEST_FLAGS) ./... -coverprofile=$(CACHE_DIR)/test/coverage.out
	@echo "$(GREEN)Tests completed$(NC)"

test-bench: deps-verify ## Run benchmark tests
	@echo "$(BLUE)Running benchmarks...$(NC)"
	go test $(GOFLAGS) $(TEST_FLAGS) -bench=. -benchmem ./...

test-coverage: test ## Generate test coverage report
	@echo "$(BLUE)Generating coverage report...$(NC)"
	go tool cover -html=$(CACHE_DIR)/test/coverage.out -o $(CACHE_DIR)/test/coverage.html
	@echo "$(GREEN)Coverage report: $(CACHE_DIR)/test/coverage.html$(NC)"

# Functional tests run against the LIVE accretion-api in the kind cluster. They
# port-forward the Service, wait for /api/health, run the `functional`-tagged
# suite, and always tear the forward down. Requires the environment to be up
# (see `make up`).
#
# TLS is on by default, so the scheme and the CA are derived from the cluster
# rather than hardcoded: the CA is read out of the chart-issued accretion-api-tls
# Secret, exactly as the keys below are read out of the client-keys Secret, so
# neither can drift from what the server is actually serving. A deployment with
# no such Secret is treated as cleartext, which keeps this target working against
# an instance that terminates TLS elsewhere. Nothing here skips verification.
#
# Auth is enforced, so the suite needs real keys: one scoped read:memory AND
# chat:query (the accretion-mcp identity, which the whole suite runs under) and
# one scoped read:memory only (ci), which drives the scoping test — a 403 on
# /api/chat.
#
# They come from .env, via the same IDENTITIES table in scripts/make-secret.sh
# that derives every Secret, and NOT from the chart's client-keys Secret. That
# Secret is published only for an identity declared with a plaintext `key`, and
# #122 made every identity hash-only, so it is rendered on no path any more:
# `helm template` of values-dev.yaml with hash-only identities produces zero
# copies of it, and a plaintext identity produces one. The read that used to
# work therefore fails, and `$${VAR:-$$(kubectl get ...)}` turns that failure
# into an EMPTY key rather than an error. This is the same defect #119 fixed for
# the Discord bot, but quieter: the bot's pod would not start, whereas an empty
# key here 401s every request with nothing naming the cause — and worse, the
# suite reads an empty ACCRETION_API_KEY_READONLY as "skip TestAPIKeyScoping"
# (test/functional/api_test.go), so a run that proved nothing reports a pass.
#
# The variable NAMES are asked of make-secret.sh rather than written out here,
# because a renamed identity must not leave this target reading a variable
# nobody sets. Resolution is fail-closed and happens FIRST, before the
# port-forward: there is no default, and the error names the variable to set.
# Override ACCRETION_API_URL / ACCRETION_API_KEY / ACCRETION_API_KEY_READONLY to
# hit a different instance.
#
# Present-but-unusable is refused too, not only absent. A value carrying
# whitespace, `://` or `<` cannot be a key — it is a secret-manager reference
# ("<ns:key>", "op://vault/item"), and .env files here do currently hold that
# shape. Unrefused it produces `got 401, want 200` from whichever test runs
# first, which names nothing — the failure this target was rewritten to stop
# giving.
#
# This is a DENYLIST OF THREE SHAPES, not placeholder detection, and the
# difference is measured rather than hedged. Of six placeholder forms tried, it
# refuses two ("<ns:key>", "op://vault/item") and accepts four:
# REPLACE_WITH_SECRET_MANAGER_REF, k8s:accretion-core:api-key (single colons, no
# `://`), your-key-here and changeme. The first of those is this repository's
# OWN canonical placeholder — pulumi/pkg/kubernetes/secrets.go writes it into 15
# Secret fields — so the placeholder most likely to be pasted in is the one not
# caught. Say only what the refusal matches, therefore, and never that
# placeholders are covered.
#
# Widening it is deliberately NOT done here. An allowlist
# (^[A-Za-z0-9+/=_.-]{16,}$) is strictly better and still cannot catch
# REPLACE_WITH_SECRET_MANAGER_REF, which is alphanumeric-plus-underscore and 31
# characters — shape-indistinguishable from a real key. The only discriminator
# is a naming convention, which is a different kind of rule and deserves its own
# review rather than a one-line tightening here. Filed.
#
# The value is REFUSED rather than trimmed. An earlier version of this comment
# justified that by claiming trimming would send a key whose digest is not the
# one .env implies. That is FALSE, and it was tested rather than argued: both
# parsers on the path already strip surrounding whitespace, so .env implies the
# TRIMMED digest and trimming here would have MATCHED the server rather than
# diverging from it. `ACCRETION_API_KEY_MCP=abc ` yields the digest of "abc"
# from `scripts/make-secret.sh --print-identities` (load_env_file strips around
# every value before it is hashed), and direnv exports it as "abc", length 3.
# Do not reinstate the digest argument; it does not hold.
#
# Three reasons that do hold, and they are why the refusal stays:
#   - A developer whose editor leaves a trailing space never reaches this check
#     at all, because both parsers strip it first. The case this refusal was
#     feared to bite does not arise on the .env path.
#   - What DOES reach it is an explicit `export VAR="abc "` in a shell, which
#     bypasses both parsers. There the value really is not what any digest
#     covers, and a loud stop beats papering over a quoting mistake.
#   - Whitespace INSIDE a key must be refused rather than trimmed regardless, so
#     one rule is simpler than a rule plus an exception.
test-functional: ## Run functional tests against the running environment (kind $(NAMESPACE))
	@echo "$(BLUE)Running functional tests against $(NAMESPACE)/accretion-api...$(NC)"
	@bash -c 'set -e; \
		refuse_shape() { \
			printf "%b\n" "$(RED)$$1 does not hold an API key$(NC)" >&2; \
			printf "%s\n" "  Its value contains whitespace, :// or <, none of which can appear in a key. Most likely" >&2; \
			printf "%s\n" "  an unresolved secret-manager reference — resolve it before .env, or paste the key itself." >&2; \
			printf "%s\n" "  The server compares a SHA-256 of the EXACT bytes, so this would 401 every request." >&2; \
			exit 1; \
		}; \
		key_var=$$(bash scripts/make-secret.sh --print-identity-var accretion-mcp); \
		ro_var=$$(bash scripts/make-secret.sh --print-identity-var ci); \
		API_KEY=$${ACCRETION_API_KEY:-$${!key_var:-}}; \
		API_KEY_RO=$${ACCRETION_API_KEY_READONLY:-$${!ro_var:-}}; \
		case "$$API_KEY" in *[[:space:]]*|*://*|*"<"*) refuse_shape "$$key_var";; esac; \
		case "$$API_KEY_RO" in *[[:space:]]*|*://*|*"<"*) refuse_shape "$$ro_var";; esac; \
		if [ -z "$$API_KEY" ]; then \
			printf "%b\n" "$(RED)no API key for the functional suite$(NC)" >&2; \
			printf "%s\n" "  Set $$key_var in .env — the accretion-mcp identity plaintext, scoped read:memory + chat:query." >&2; \
			printf "%s\n" "  It is read from the ENVIRONMENT, which .env reaches through direnv: run direnv allow after editing." >&2; \
			printf "%s\n" "  See env.example; scripts/make-secret.sh --print-identities emits the hash half to configure." >&2; \
			printf "%s\n" "  Or export ACCRETION_API_KEY to run against another instance." >&2; \
			exit 1; \
		fi; \
		if [ -z "$$API_KEY_RO" ]; then \
			printf "%b\n" "$(RED)no read-only API key for the functional suite$(NC)" >&2; \
			printf "%s\n" "  Set $$ro_var in .env — the ci identity plaintext, scoped read:memory only." >&2; \
			printf "%s\n" "  It is read from the ENVIRONMENT, which .env reaches through direnv: run direnv allow after editing." >&2; \
			printf "%s\n" "  Without it TestAPIKeyScoping SKIPS, and the suite reports a pass having never" >&2; \
			printf "%s\n" "  proven a narrow key is denied /api/chat. Or export ACCRETION_API_KEY_READONLY." >&2; \
			exit 1; \
		fi; \
		CA=$$(mktemp); trap "rm -f $$CA" EXIT; \
		if kubectl -n $(NAMESPACE) get secret accretion-api-tls >/dev/null 2>&1; then \
			kubectl -n $(NAMESPACE) get secret accretion-api-tls -o jsonpath="{.data.ca\.crt}" | base64 -d > $$CA; \
			SCHEME=https; CURL_CA="--cacert $$CA"; CA_ENV=$$CA; \
		else \
			SCHEME=http; CURL_CA=""; CA_ENV=""; \
		fi; \
		kubectl -n $(NAMESPACE) port-forward svc/accretion-api $(FUNCTIONAL_PORT):80 >/tmp/accretion-func-pf.log 2>&1 & \
		PF=$$!; trap "kill $$PF 2>/dev/null || true; rm -f $$CA" EXIT; \
		echo "  waiting for /api/health via port-forward ($$SCHEME)..."; \
		for i in $$(seq 1 $(FUNCTIONAL_WAIT)); do \
			curl -sf $$CURL_CA -o /dev/null $$SCHEME://localhost:$(FUNCTIONAL_PORT)/api/health && break; \
			if [ $$i -eq $(FUNCTIONAL_WAIT) ]; then echo "$(RED)api not reachable on :$(FUNCTIONAL_PORT) — is the environment up? try make up$(NC)"; exit 1; fi; \
			sleep 1; \
		done; \
		ACCRETION_API_URL=$$SCHEME://localhost:$(FUNCTIONAL_PORT) \
			ACCRETION_API_CA_FILE=$$CA_ENV \
			ACCRETION_API_KEY=$$API_KEY \
			ACCRETION_API_KEY_READONLY=$$API_KEY_RO \
			go test $(RACE) -tags functional -count=1 -v ./test/functional/...; \
	'
	@echo "$(GREEN)Functional tests completed$(NC)"

tilt-down: ## Stop Tilt and clean up
	@echo "$(BLUE)Stopping Tilt...$(NC)"
	tilt down
# `tilt down` deletes the Kubernetes objects but leaves the `tilt up` process
# running, still watching files and still reconciling. A surviving process is
# not harmless: one outlived its worktree here, lost its build context, and
# began garbage-collecting the cluster it still believed it owned.
#
# Matched by working directory, not by name, so a Tilt running for a different
# project or another worktree of this repo is left alone. Paths are resolved with
# `pwd -P` because macOS reports /tmp as /private/tmp and the two would not
# compare equal. cwd comes from /proc on Linux and lsof on macOS.
	@bash -c 'set -uo pipefail; \
	  proj=$$(cd "$(CURDIR)" && pwd -P); killed=0; \
	  for pid in $$(pgrep -f "tilt up" 2>/dev/null); do \
	    if [ -r "/proc/$$pid/cwd" ]; then \
	      cwd=$$(readlink "/proc/$$pid/cwd" 2>/dev/null); \
	    else \
	      cwd=$$(lsof -a -d cwd -p "$$pid" -Fn 2>/dev/null | sed -n "s/^n//p" | head -1); \
	    fi; \
	    [ -n "$$cwd" ] || continue; \
	    cwd=$$(cd "$$cwd" 2>/dev/null && pwd -P) || continue; \
	    [ "$$cwd" = "$$proj" ] || continue; \
	    kill "$$pid" 2>/dev/null || true; killed=$$((killed+1)); \
	    for i in 1 2 3 4 5 6 7 8 9 10; do \
	      kill -0 "$$pid" 2>/dev/null || break; sleep 1; \
	    done; \
	    if kill -0 "$$pid" 2>/dev/null; then \
	      printf "%b\\n" "$(YELLOW)  tilt pid $$pid ignored SIGTERM; sending SIGKILL$(NC)"; \
	      kill -9 "$$pid" 2>/dev/null || true; \
	    fi; \
	  done; \
	  if [ "$$killed" -eq 0 ]; then \
	    printf "%b\\n" "$(GREEN)No tilt process running for $$proj$(NC)"; \
	  else \
	    printf "%b\\n" "$(GREEN)Stopped $$killed tilt process(es) for $$proj$(NC)"; \
	  fi'

# tilt-full and tilt-server-only pass the setting on the command line rather
# than writing tilt_config.json: that file was git-tracked and rewritten here, so
# one server-only run left the next `make tilt-up` server-only too (#101).
tilt-full: require-env preflight-gate ## Start Tilt with all components (server + collector)
	@echo "$(BLUE)Starting Tilt (full stack)...$(NC)"
	tilt up -- --enable_collector=true

tilt-server-only: require-env preflight-gate ## Start Tilt with only accretion-api server (no collector)
	@echo "$(BLUE)Starting Tilt (server only)...$(NC)"
	tilt up -- --enable_collector=false


# install-mcp is a prerequisite because accretion-mcp is the one component that
# lives on the developer's machine while the server it talks to is redeployed
# underneath it, so it goes stale silently and nothing cluster-side can notice.
# Observed: a binary five days older than the TLS work read the URL and key from
# the Secret exactly as before, dialled correctly, then verified against the
# system pool because it had never heard of ACCRETION_API_CA_SECRET. It failed as
# `x509: certificate is not trusted`, which points at the certificate rather than
# at the binary's age -- every cluster-side artifact was correct.
#
# It hangs off tilt-up rather than up because tilt-up is what redeploys the
# server; `make tilt-up` on an existing cluster is the common path and would
# otherwise skip it. Measured at 1.7s on a warm cache, and a no-op when unchanged.
tilt-up: require-env preflight-gate install-mcp ## Start Tilt for local development (components per an optional local tilt_config.json)
	@echo "$(BLUE)Starting Tilt for local development...$(NC)"
	@echo "$(YELLOW)Components come from the Tiltfile defaults, overridden by a local tilt_config.json if present$(NC)"
	tilt up

# Evaluates the Tiltfile for both collector settings. `tilt alpha tiltfile-result`
# exits 5 on an evaluation error, which is what server-only hit in #101 while
# the pre-commit hook ran a `tilt validate` command that does not exist and
# passed on its own fallback echo. No CI runner has tilt, so this is the only
# gate the Tiltfile has; it runs from pre-commit whenever the Tiltfile changes.
tiltfile-check: ## Verify the Tiltfile evaluates with the collector on and off
	@bash scripts/check-tiltfile.sh

# OTLP trace ingest is the one push-based path; the pull-based collectors catch
# up after a gap and it does not (#104). staging and prod must render it as its
# own Deployment with a floor of two replicas and a PDB, and no PDB anywhere
# may sit at minAvailable >= its workload's floor, which blocks drains for
# good. The last step proves that second assertion has teeth: enabling the
# shared pod's PDB at one replica -- the tempting one-line "fix" -- must fail.
trace-ingest-check: ## Verify a scaled-out collector keeps redundant trace ingest and no PDB can block a drain
	@printf "%b\n" "$(BLUE)Checking rendered trace ingest and PodDisruptionBudgets...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) > $$d/default.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) $(COLLECTOR_REDUNDANT) > $$d/redundant.yaml; \
		bash scripts/check-trace-ingest.sh $$d/default.yaml; \
		bash scripts/check-trace-ingest.sh --redundant $$d/redundant.yaml; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) --set dataCollector.podDisruptionBudget.enabled=true > $$d/blocking.yaml; \
		if bash scripts/check-trace-ingest.sh $$d/blocking.yaml >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)a PDB with minAvailable 1 over a one-replica Deployment passed -- the drain check is inert$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  drain check proven to fire (PDB minAvailable 1 over one replica is rejected)"'
	@printf "%b\n" "$(GREEN)Trace ingest is redundant where it must be, and no PDB can block a drain$(NC)"

traces-check: ## Verify the trace pipeline is scoped by pod identity, not by what a sender claims
	@printf "%b\n" "$(BLUE)Checking rendered trace scoping...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		fail() { printf "%b\n" "$(RED)$$1$(NC)"; exit 1; }; \
		helm template t ./charts/otel-collector --namespace obs > $$d/otel.yaml; \
		helm template t ./charts/otel-collector --namespace obs \
			--set accretionTraceWorker.namespace=accretion-probe > $$d/otel-probe.yaml; \
		order=$$(awk "/^        traces\/accretion:/{f=1} f&&/^          processors:/{p=1;next} p&&/^          - /{gsub(/^          - /,\"\");printf \"%s \",\$$0;next} p{exit}" $$d/otel.yaml); \
		[ "$$order" = "memory_limiter transform/strip-claimed-identity k8sattributes/accretion filter/accretion tail_sampling/accretion batch " ] \
			|| fail "traces/accretion processors are [$$order]; required order is memory_limiter, transform/strip-claimed-identity, k8sattributes/accretion, filter/accretion, tail_sampling/accretion, batch"; \
		printf "%b\n" "  ok  traces/accretion processors: $$order"; \
		grep -A1 "transform/strip-claimed-identity:" $$d/otel.yaml | grep -q "error_mode: ignore" \
			|| fail "transform/strip-claimed-identity is missing or malformed"; \
		for a in k8s.namespace.name k8s.pod.name; do \
			grep -q "delete_key(resource.attributes, \"$$a\")" $$d/otel.yaml \
				|| fail "strip-claimed-identity does not delete $$a, which k8sattributes stamps — a sender could pre-set it and keep its own value"; \
		done; \
		printf "%b\n" "  ok  strip-claimed-identity deletes every attribute k8sattributes stamps"; \
		grep -q "resources: \[pods\]" $$d/otel.yaml \
			|| fail "collector ClusterRole is not scoped to pods alone"; \
		grep -q "verbs: \[list, watch\]" $$d/otel.yaml \
			|| fail "collector ClusterRole verbs are wider than the informer needs (list, watch)"; \
		printf "%b\n" "  ok  collector ClusterRole is pods:[list,watch] and nothing more"; \
		grep -q "k8s.namespace.name\"\] == nil" $$d/otel.yaml \
			|| fail "filter/accretion lost its fail-closed nil condition: an unattributable span would be KEPT"; \
		printf "%b\n" "  ok  filter/accretion drops unattributable spans (== nil)"; \
		grep -q "k8s.namespace.name\"\] == \"accretion-probe\"" $$d/otel-probe.yaml \
			|| fail "filter/accretion namespace is hardcoded, not read from .Values.accretionTraceWorker.namespace"; \
		printf "%b\n" "  ok  filter/accretion namespace follows .Values.accretionTraceWorker.namespace"; \
		grep -q "^ *num_traces:" $$d/otel.yaml || fail "tail_sampling has no num_traces cap"; \
		grep -q "^ *memory_limiter:" $$d/otel.yaml || fail "no memory_limiter processor is defined"; \
		printf "%b\n" "  ok  tail_sampling buffer capped and memory_limiter defined"; \
		helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) --namespace accretion-core > $$d/coll.yaml; \
		grep -A2 "^      traces:" $$d/coll.yaml | grep -q -- "- \"\*\"" \
			|| fail "collector config does not scope traces with namespaces.traces.include=[*]"; \
		grep -q "self_namespace: \"accretion-core\"" $$d/coll.yaml \
			|| fail "self_namespace is not the release namespace; accretion could ingest its own traces"; \
		printf "%b\n" "  ok  trace scope is namespaces.traces.include=[*] minus self_namespace"; \
		test -z "$$(grep -A2 \"trace_worker:\" $$d/coll.yaml | sed -n 3p | tr -d \"[:space:]\")" \
			|| fail "the legacy trace_worker.namespaces list is populated again — one policy, one place"; \
		printf "%b\n" "  ok  legacy trace_worker.namespaces left empty (no duplicated list)"'
	@printf "%b\n" "$(GREEN)Trace scoping verified$(NC)"

up: ## Create the Kind cluster, then start Tilt for local development
	@$(MAKE) kind-up
	@$(MAKE) tilt-up

# B1 was a checked-in values file shipping a working bearer token. This is the
# mechanical version of that review: render EVERY checked-in values file with
# the chart's own forbidPlaintextAPIKeys guard on, so a plaintext `key` can no
# longer reach any of them -- including values-dev.yaml, which since #122 holds
# no plaintext either and is now rendered as a file that must PASS.
#
# That is why the vacuity proof is a purpose-built fixture. It used to render
# values-dev.yaml and require the guard to reject it, which worked only while
# that file carried checked-in plaintext keys. With the plaintext moved to .env
# the same assertion would still "pass" -- the render fails on the empty-apiKeys
# guard instead -- and would then be proving nothing about the plaintext guard
# at all. The fixture below carries a plaintext `key` and nothing else, and the
# failure must NAME forbidPlaintextAPIKeys, so an unrelated failure cannot stand
# in for the guard firing.
#
# Which is why the renders here pass $(DB_PASSWORD_RENDER) since #134. The
# database-password guard is unconditional now and validate-security.yaml runs
# it BEFORE the identity guards, so a render with no password fails with the
# database message -- and the "rejected, but not by forbidPlaintextAPIKeys"
# branch would then fire on every run, turning this gate permanently red while
# saying nothing at all about plaintext keys. Supplying a password is what keeps
# each assertion about the guard it names.
# Every case above passes $$guard explicitly, though, which proves the guard
# WORKS but not that it is ON by default — flip values.yaml's own
# forbidPlaintextAPIKeys back to false and every check above still passes,
# because none of them render without $$guard. The block right after this one
# renders with NO --set override at all, so it is the one assertion that goes
# red if the shipped default ever regresses to permissive.
# values-example.yaml is rendered WITHOUT $$stub: --set-json replaces a list
# rather than merging, so the stub would overwrite the very apiKeys entry under
# test — which is how a plaintext key in that file once passed this check. It
# declares its own identity and renders standalone. The staging/prod loop passes
# $$guard explicitly rather than relying on the files setting it, so deleting
# security.forbidPlaintextAPIKeys from one of them cannot silently pass.
# The sslmode half covers BOTH charts, but it reaches them differently, and the
# difference is the point. The API's answer still lives in its per-environment
# values file, so that half renders the file. The collector's used to live in
# values-{staging,prod}.yaml and #123 deleted them, so that half asks
# `make deploy-collector STACK=<e>` what sslmode it would actually pass and
# renders with THAT.
#
# Reading the flag out of the deploy target rather than writing `require` into
# this check is what keeps the assertion honest. A literal here would prove only
# that helm passes a flag through -- worse than an absent check, because it
# looks like coverage -- and it would go green while the deploy path silently
# reverted to the chart's `disable` default. Extracting it means deleting the
# setting from deploy-collector makes this fail by name, which is exactly how
# the gap that cycle-1 review caught would have been caught at the time.
#
# NOTE for the API half of #123: this cannot simply be repointed at
# deploy/pulumi.yaml, because that file sets no collector sslmode either. The
# setting has to be ADDED there, not moved.
#
# The sslmode half exists because a regeneration of the collector env files
# silently downgraded staging from "require" to "disable". Nothing caught it:
# the change was described as a key rename, and no gate read the rendered
# result. Renaming keys across a values file is exactly when an unrelated value
# can be lost, so this asserts the rendered output rather than trusting review.
# The sslmode check reads EVERY sslmode: occurrence in each render, not the
# first. There is exactly one today, but this gate exists because one unrelated
# value went unnoticed, and a second occurrence (a DSN in a Secret, a sidecar)
# is exactly what would go unnoticed next (#99). The synthetic file at the end
# proves the loop reads past a compliant first match.
values-check: ## Verify checked-in values ship no plaintext API keys and no unencrypted staging/prod DB traffic
	@printf "%b\n" "$(BLUE)Checking checked-in values files for plaintext API keys...$(NC)"
	@bash -c 'set -eo pipefail; \
		guard="--set security.forbidPlaintextAPIKeys=true"; \
		stub="--set-json apiKeys=[{\"name\":\"ci\",\"hash\":\"$(ZERO_DIGEST)\",\"permissions\":[\"read:memory\"]}]"; \
		helm template t ./charts/accretion-api -f charts/accretion-api/values.yaml $$guard $$stub $(DB_PASSWORD_RENDER) >/dev/null; \
		printf "%b\n" "  ok  charts/accretion-api/values.yaml"; \
		helm template t ./charts/accretion-api -f charts/accretion-api/values-example.yaml $$guard >/dev/null; \
		printf "%b\n" "  ok  charts/accretion-api/values-example.yaml (own apiKeys)"; \
		for f in charts/accretion-api/values-staging.yaml charts/accretion-api/values-prod.yaml; do \
			helm template t ./charts/accretion-api -f $$f $$guard $$stub \
				--set-string config.database.password=x >/dev/null; \
			printf "%b\n" "  ok  $$f"; \
		done; \
		helm template t ./charts/accretion-api -f charts/accretion-api/values-dev.yaml $$guard $$stub >/dev/null; \
		printf "%b\n" "  ok  charts/accretion-api/values-dev.yaml"; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		printf "%s\n" "apiKeys:" "  - name: probe" "    key: \"plaintext-probe\"" "    permissions: [\"read:memory\"]" > $$d/plaintext.yaml; \
		if out=$$(helm template t ./charts/accretion-api -f $$d/plaintext.yaml $$guard $(DB_PASSWORD_RENDER) 2>&1); then \
			printf "%b\n" "$(RED)a values file carrying a plaintext key rendered under the guard — this check is vacuous$(NC)"; exit 1; \
		fi; \
		case "$$out" in \
			*forbidPlaintextAPIKeys*) ;; \
			*) printf "%b\n" "$(RED)the plaintext fixture was rejected, but not by forbidPlaintextAPIKeys — an unrelated failure is masking whether the guard fired: $$out$(NC)"; exit 1;; \
		esac; \
		printf "%b\n" "  ok  guard proven to fire (a purpose-built plaintext fixture is rejected BY NAME)"'
	@printf "%b\n" "$(BLUE)Checking that forbidPlaintextAPIKeys defaults to true with no override...$(NC)"
	@bash -c 'set -eo pipefail; \
		plaintext="--set-json apiKeys=[{\"name\":\"ci\",\"key\":\"plaintext-probe\",\"permissions\":[\"read:memory\"]}]"; \
		hashOnly="--set-json apiKeys=[{\"name\":\"ci\",\"hash\":\"$(ZERO_DIGEST)\",\"permissions\":[\"read:memory\"]}]"; \
		if out=$$(helm template t ./charts/accretion-api $$plaintext $(DB_PASSWORD_RENDER) 2>&1); then \
			printf "%b\n" "$(RED)a plaintext apiKeys entry rendered with NO --set override — values.yaml no longer defaults forbidPlaintextAPIKeys to true$(NC)"; exit 1; \
		fi; \
		if ! printf "%s" "$$out" | grep -q "forbidPlaintextAPIKeys"; then \
			printf "%b\n" "$(RED)the plaintext probe was rejected, but not by forbidPlaintextAPIKeys — an unrelated failure is masking whether the default guard even fired: $$out$(NC)"; exit 1; \
		fi; \
		helm template t ./charts/accretion-api $$hashOnly $(DB_PASSWORD_RENDER) >/dev/null; \
		printf "%b\n" "  ok  values.yaml default (no override at all) rejects a plaintext key by name, and still renders a hash-only one"'
	@printf "%b\n" "$(BLUE)Checking staging/prod database TLS...$(NC)"
	@bash -c 'set -eo pipefail; \
		d=$$(mktemp -d); trap "rm -rf $$d" EXIT; \
		check_sslmodes() { \
			local modes; modes=$$(grep "sslmode:" "$$1" | sed "s/.*sslmode: *//;s/\"//g"); \
			test -n "$$modes" || { printf "%b\n" "$(RED)no sslmode rendered in $$1 — this check is vacuous$(NC)"; return 1; }; \
			for mode in $$modes; do \
				case "$$mode" in \
					require|verify-ca|verify-full) ;; \
					*) printf "%b\n" "$(RED)$$1 renders sslmode=$$mode; $$2 must not send database traffic unencrypted$(NC)"; return 1;; \
				esac; \
			done; \
		}; \
		stub="--set-json apiKeys=[{\"name\":\"ci\",\"hash\":\"$(ZERO_DIGEST)\",\"permissions\":[\"read:memory\"]}]"; \
		for e in staging prod; do \
			helm template t ./charts/accretion-api -f charts/accretion-api/values-$$e.yaml $$stub \
				--set-string config.database.password=x > $$d/api-$$e.yaml; \
			check_sslmodes "$$d/api-$$e.yaml" "$$e" || exit 1; \
			mode=$$($(MAKE) -n deploy-collector STACK=$$e 2>/dev/null | sed -n "s/.*set-string config.database.sslmode=\([a-z-]*\).*/\1/p" | head -1); \
			[ -n "$$mode" ] || { printf "%b\n" "$(RED)make deploy-collector STACK=$$e passes no sslmode at all — the setting the per-environment files used to carry is gone again$(NC)"; exit 1; }; \
			helm template t ./charts/accretion-collector $(DB_PASSWORD_RENDER) --set-string config.database.sslmode=$$mode > $$d/coll-$$e.yaml; \
			check_sslmodes "$$d/coll-$$e.yaml" "$$e" || exit 1; \
			printf "%b\n" "  ok  $$e renders an encrypted sslmode in both charts (collector via the $$mode that deploy-collector passes)"; \
		done; \
		for bad in disable prefer requrie; do \
			if $(MAKE) deploy-collector-preflight STACK=prod DB_SSLMODE=$$bad >/dev/null 2>&1; then \
				printf "%b\n" "$(RED)deploy-collector accepted DB_SSLMODE=$$bad for STACK=prod. The gate below refuses that mode in the rendered output, so the deploy path and the gate disagree about what counts as encrypted — and DB_SSLMODE can be set from the environment, so this is reachable without editing anything.$(NC)"; exit 1; \
			fi; \
		done; \
		$(MAKE) deploy-collector-preflight STACK=prod >/dev/null 2>&1 \
			|| { printf "%b\n" "$(RED)deploy-collector refuses its own default for STACK=prod — the refusal above is firing on everything and proves nothing$(NC)"; exit 1; }; \
		$(MAKE) deploy-collector-preflight STACK=dev DB_SSLMODE=disable >/dev/null 2>&1 \
			|| { printf "%b\n" "$(RED)deploy-collector refuses cleartext for STACK=dev, where a local Postgres has no TLS$(NC)"; exit 1; }; \
		printf "%b\n" "  ok  deploy-collector refuses an unencrypted or unknown DB_SSLMODE outside dev/test, and accepts its own default"; \
		printf "%s\n" "config:" "  database:" "    sslmode: \"verify-full\"" "    existingSecret: accretion-api-secrets" "    existingSecretKey: db-password" > $$d/op-strong.yaml; \
		printf "%s\n" "config:" "  database:" "    sslmode: \"disable\"" "    existingSecret: accretion-api-secrets" "    existingSecretKey: db-password" > $$d/op-weak.yaml; \
		if $(MAKE) -n deploy-collector STACK=prod COLLECTOR_VALUES="-f $$d/op-strong.yaml" 2>/dev/null | grep -q "set-string config.database.sslmode"; then \
			printf "%b\n" "$(RED)deploy-collector passes its own --set-string alongside an operator values file. helm ranks --set above -f whatever the order, so an operator who asked for verify-full would be silently downgraded to the target default — a guard meant to raise a floor imposing a ceiling instead.$(NC)"; exit 1; \
		fi; \
		$(MAKE) deploy-collector-preflight STACK=prod COLLECTOR_VALUES="-f $$d/op-strong.yaml" >/dev/null 2>&1 \
			|| { printf "%b\n" "$(RED)deploy-collector refuses an operator file asking for verify-full, which is STRONGER than its own default$(NC)"; exit 1; }; \
		if $(MAKE) deploy-collector-preflight STACK=prod COLLECTOR_VALUES="-f $$d/op-weak.yaml" >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)deploy-collector accepted an operator file asking for cleartext on STACK=prod — with a values file supplied the target passes no --set-string, so this file IS the deployed mode and nothing else would catch it$(NC)"; exit 1; \
		fi; \
		printf "%s\n" "config:" "  database:" "    sslmode: \"requrie\"" "    existingSecret: accretion-api-secrets" "    existingSecretKey: db-password" > $$d/op-typo.yaml; \
		if $(MAKE) deploy-collector-preflight STACK=dev COLLECTOR_VALUES="-f $$d/op-typo.yaml" >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)deploy-collector accepted sslmode=requrie from an operator values file. The loose stage — is this a PostgreSQL sslmode at all — is only reaching DB_SSLMODE, not the mode that actually gets used, so a typo reaches the cluster on any stack the tight allowlist does not cover.$(NC)"; exit 1; \
		fi; \
		$(MAKE) deploy-collector-preflight STACK=dev DB_SSLMODE=prefer >/dev/null 2>&1 \
			|| { printf "%b\n" "$(RED)deploy-collector refuses sslmode=prefer on dev, so the loose and tight stages have collapsed into one list — dev must still accept any real mode$(NC)"; exit 1; }; \
		printf "%b\n" "  ok  an operator values file owns the mode: verify-full survives uncapped, cleartext refused, a typo refused on every stack, and dev still accepts any real mode"; \
		printf "sslmode: require\nsslmode: disable\n" > $$d/second-weak.yaml; \
		if check_sslmodes $$d/second-weak.yaml synthetic >/dev/null 2>&1; then \
			printf "%b\n" "$(RED)a weak sslmode after a compliant one passed — the check reads only the first occurrence again$(NC)"; exit 1; \
		fi; \
		printf "%b\n" "  ok  sslmode check proven to read every occurrence (a second, weak one is rejected)"'
	@printf "%b\n" "$(GREEN)No plaintext API keys, and staging/prod database traffic is encrypted$(NC)"

# Detector for #117: nothing verified that the value sets our own callers build
# actually produce a valid release. Each caller assembles the full set
# independently -- `make deploy-auth STACK=x` points helm at a per-environment
# values file, the Tiltfile passes inline --set flags, Pulumi builds a
# pulumi.Map in Go -- and no gate rendered any of them, so two deployment paths
# were broken at once with nothing red. Renders every first-party chart under
# every caller's value set and fails on any non-zero helm exit; the exit status
# is checked before anything is counted, so a failed render can never be read as
# zero findings the way render-check once was (#106). --selfcheck proves the
# engine has teeth against hand-built fixtures, independent of which of this
# series' fixes have landed in this checkout. The selfcheck runs FIRST: while a
# caller is still broken the matrix exits non-zero, and a selfcheck placed after
# it would never run on exactly the days it matters most.
values-matrix-check: ## Verify every caller's value set renders against every first-party chart
	@bash scripts/check-values-matrix.sh --selfcheck
	@bash scripts/check-values-matrix.sh

vet: ## Run go vet
	@echo "$(BLUE)Running go vet...$(NC)"
	go vet ./...
	@echo "$(GREEN)Vet completed$(NC)"

# Make .cache directory
$(CACHE_DIR):
	mkdir -p $(CACHE_DIR)
