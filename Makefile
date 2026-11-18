# Local integration tests run against a PostgreSQL container owned by one
# managed validation invocation (component-validation-tooling, "Invocation
# inventory" and "Owned teardown"). Each invocation gets its own Compose
# project conveyor-test-<invocation-id>, a free loopback port, and a durable
# inventory under $XDG_STATE_HOME/conveyor/<task>/invocations/. Teardown and
# `make test-db-down INVOCATION=<dir>` remove only that inventory's sealed
# container and network by ID. CONVEYOR_TEST_POSTGRES_PORT or
# TEST_POSTGRES_PORT pins the port; an occupied pin fails instead of attaching.
# The container budget defaults to CONVEYOR_TEST_POSTGRES_MEMORY=2g with a
# CONVEYOR_TEST_POSTGRES_TMPFS_SIZE=1g data tmpfs; overrides must stay
# finite and keep the tmpfs smaller than the memory limit. Large disposable
# outputs use CONVEYOR_VALIDATION_TMP_ROOT (default: the task cache), which
# must be disk-backed unless CONVEYOR_VALIDATION_ALLOW_RAM_TMP=1.
BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/kidus-tiliksew/conveyor/internal/releaseinfo.Version=$(VERSION)"
ENV_FILE ?= .env
CONVEYOR_CONFIG ?= conveyor.yaml
LISTEN_ADDR ?= 127.0.0.1:8080
POLL_GITHUB ?= 60s
IMAGE ?= conveyor:local
# Durable invocation inventories and fixture state are grouped by task. The
# validation child boundary removes CONVEYOR_TASK_ID, so pass it explicitly.
VALIDATION_TASK ?= $(if $(strip $(CONVEYOR_TASK_ID)),$(CONVEYOR_TASK_ID),manual-validation)
CI_VALIDATION_TASK ?= $(if $(strip $(CONVEYOR_TASK_ID)),$(CONVEYOR_TASK_ID),ci-validation)
PLAYWRIGHT_ARGS ?=
PLAYWRIGHT_INSTALL_ARGS ?=
PLAYWRIGHT_WORKERS ?= 2
# Repository validation children must not inherit the launching worker's claim,
# assignment, handoff, or checkout-root metadata. Keep this denylist exact so
# backend URLs, tool paths, task caches, HOME, PATH, and other validation inputs
# continue to reach the child process.
VALIDATION_WORKER_ENV_VARS = \
	CONVEYOR_ADDR \
	CONVEYOR_API_TOKEN \
	CONVEYOR_CLIENT_TOKEN \
	CONVEYOR_CURRENT_ATTEMPT_ID \
	CONVEYOR_PREVIOUS_ATTEMPT_ID \
	CONVEYOR_PREVIOUS_ATTEMPT_REASON \
	CONVEYOR_PREDECESSOR \
	CONVEYOR_PREVIOUS_WORK_ORDER_ID \
	CONVEYOR_SESSION_ID \
	CONVEYOR_TASK_BASE_BRANCH \
	CONVEYOR_TASK_BRANCH \
	CONVEYOR_TASK_ID \
	CONVEYOR_TASK_REPO \
	CONVEYOR_TASK_REPO_URL \
	CONVEYOR_WORKSPACE \
	CONVEYOR_WORKTREE_ROOT \
	CONVEYOR_WORK_ORDER_ID \
	CONVEYOR_WRITER_GENERATION \
	CONVEYOR_WRITER_PATH
VALIDATION_CHILD_ENV = env $(foreach name,$(VALIDATION_WORKER_ENV_VARS),-u $(name))
RUN_WEB_LINT = cd web && $(VALIDATION_CHILD_ENV) npm run lint
RUN_PLAYWRIGHT = cd web && CONVEYOR_VALIDATION_TASK=$(VALIDATION_TASK) PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e --
RUN_WEB_TESTS = $(RUN_WEB_LINT) && CONVEYOR_VALIDATION_TASK=$(VALIDATION_TASK) PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- $(PLAYWRIGHT_ARGS)
# The memory (go test ./...) and PostgreSQL ReviewSummary stress cases run
# 500 rounds wherever they run; the SingleStore pull-request shards leave
# CONVEYOR_REVIEW_SUMMARY_STRESS_ROUNDS unset and run 50, and the nightly soak
# workflow runs SingleStore at 500 (component-testing-strategy,
# Configuration). go test ./... runs without -v, so the memory run echoes its
# count instead of logging it.
REVIEW_SUMMARY_STRESS_ROUNDS_FULL = 500
RUN_GO_UNIT_TESTS = echo "CONVEYOR_REVIEW_SUMMARY_STRESS_ROUNDS=$(REVIEW_SUMMARY_STRESS_ROUNDS_FULL): the memory ReviewSummary stress cases run $(REVIEW_SUMMARY_STRESS_ROUNDS_FULL) rounds" && CONVEYOR_REVIEW_SUMMARY_STRESS_ROUNDS=$(REVIEW_SUMMARY_STRESS_ROUNDS_FULL) CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./...
# Integration shards run go test through this wrapper, which fails a package
# that selected no test (component-verification-strategy, "CI shards").
# GO_INTEGRATION_FLAGS follows each shard's own flags, so a later -run wins.
GO_TEST_REQUIRE_TESTS = $(VALIDATION_CHILD_ENV) bash scripts/go-test-require-tests.sh
GO_INTEGRATION_FLAGS ?=
DEV_COMPOSE := docker compose --env-file $(ENV_FILE) -f compose.dev.yaml

.PHONY: all build image test-image release release-archives test-release web-deps web-typecheck ui dashboard-fresh test test-web test-ui test-ui-evidence compose-check test-integration test-integration-ci test-postgres test-db-identity test-db-up test-db-down vet plugin-check fmt fmt-check tidy clean db-up db-down run build-run dev test-validation-fixtures

all: build

# component-validation-tooling ("The ordinary validation session"): one Make
# graph shares web-deps and ui across the complete ordinary validation session.
.PHONY: validate test-validation test-validation-docker test-validation-environment test-worker-environment
validate: build vet fmt-check test

# Go vet and the installer compile embedded dashboard files. During the
# composite session they must wait for ui even under make -j. Standalone
# targets retain their existing prerequisite contracts.
ifneq ($(filter validate,$(MAKECMDGOALS)),)
vet test-release: ui
endif

test-validation:
	PYTHONDONTWRITEBYTECODE=1 $(VALIDATION_CHILD_ENV) python3 -m unittest discover -s scripts -p 'test_validation_evidence.py'
	PYTHONDONTWRITEBYTECODE=1 $(VALIDATION_CHILD_ENV) python3 -m unittest discover -s scripts -p 'test_validation_fixtures.py'
	PYTHONDONTWRITEBYTECODE=1 $(VALIDATION_CHILD_ENV) python3 -m unittest discover -s scripts -p 'test_validation_resources.py'
	$(VALIDATION_CHILD_ENV) go test ./scripts/validation-fixture-sql

# Real Docker and PostgreSQL lifecycle fixtures for owned containers. Missing
# Docker fails this target as missing evidence; it is never a skip.
test-validation-docker:
	cd scripts && CONVEYOR_VALIDATION_DOCKER=1 PYTHONDONTWRITEBYTECODE=1 $(VALIDATION_CHILD_ENV) python3 -m unittest -v test_validation_resources.DockerLifecycleTests

test-validation-fixtures:
	PYTHONDONTWRITEBYTECODE=1 $(VALIDATION_CHILD_ENV) python3 -m unittest discover -s scripts -p 'test_validation_fixtures.py'
	$(VALIDATION_CHILD_ENV) go test ./scripts/validation-fixture-sql

test-validation-environment:
	@env $(foreach name,$(VALIDATION_WORKER_ENV_VARS),$(name)=polluted) \
		CONVEYOR_TEST_DATABASE_URL=backend-marker GOCACHE=cache-marker \
		$(VALIDATION_CHILD_ENV) sh -eu -c 'for name in $(VALIDATION_WORKER_ENV_VARS); do \
			if env | grep -q "^$${name}="; then echo "validation child inherited $$name" >&2; exit 1; fi; \
		done; test "$$CONVEYOR_TEST_DATABASE_URL" = backend-marker; test "$$GOCACHE" = cache-marker'

test-worker-environment:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./cmd/conveyor \
		-run '^(TestCheckoutTakesWriterLockWithoutLauncherEnvironment|TestIsolatedChildEnvironmentReplacesLaunchIdentity)$$' -count=1

build: conveyor-cli
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

.PHONY: conveyor-cli browser-runtime vk10-runtime test-vk10
conveyor-cli: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor

# Repository-local verification lifecycle scenario (component-verification-service):
# each gate builds its own CLI and prepares the browser before fixtures.
export CONVEYOR_VK10_CLI := $(abspath $(BIN)/conveyor)
export CONVEYOR_VK10_SOURCE := $(CURDIR)
vk10-runtime: conveyor-cli browser-runtime

browser-runtime: web-deps
	cd web && npx playwright install $(PLAYWRIGHT_INSTALL_ARGS) chromium

test-vk10: vk10-runtime
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test -v ./internal/verification -run '^TestVK10Scenario$$' -count=1
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./cmd/conveyor ./internal/dispatch ./internal/store -run 'TestKitRunner|TestKitVerifyOrdinary|TestVerificationDispatchReviewBinding|TestPolicyHandoffVerificationBinding|TestMemoryConformance/WorkOrders/VerifyPolicy' -count=1

image:
	docker build --build-arg VERSION="$(VERSION)" --tag "$(IMAGE)" .

test-image:
	@version="$$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$(IMAGE)")"; \
		test -n "$$version"; \
		test "$$(docker run --rm "$(IMAGE)" version)" = "conveyord $$version"
	docker run --rm --entrypoint /bin/sh "$(IMAGE)" -c 'command -v conveyor >/dev/null && command -v git >/dev/null && ! command -v gh >/dev/null'

RELEASE_DIR ?= dist
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# req-deployment-and-releases REQ-3/AC-3.1: release archives use the same
# build-injected identity as ordinary builds. VERSION must be the pushed tag when invoked by release.yml.
release: release-archives

release-archives:
	@test -n "$(VERSION)" || (echo "VERSION is required" >&2; exit 1)
	@set -eu; \
		case "$(VERSION)" in *[!A-Za-z0-9._+-]*) echo "VERSION contains unsafe filename characters: $(VERSION)" >&2; exit 1;; esac; \
		if test -e "$(RELEASE_DIR)" && test -n "$$(ls -A "$(RELEASE_DIR)")"; then echo "RELEASE_DIR must be empty: $(RELEASE_DIR)" >&2; exit 1; fi; \
		mkdir -p "$(RELEASE_DIR)"; \
		for target in $(RELEASE_TARGETS); do \
			goos=$${target%/*}; goarch=$${target#*/}; \
			name="conveyor_$(VERSION)_$${goos}_$${goarch}"; \
			stage="$(RELEASE_DIR)/$$name"; \
			test ! -e "$$stage" || (echo "release staging path already exists: $$stage" >&2; exit 1); \
			mkdir "$$stage"; \
			CGO_ENABLED=0 GOOS="$$goos" GOARCH="$$goarch" go build $(LDFLAGS) -o "$$stage/conveyor" ./cmd/conveyor; \
			CGO_ENABLED=0 GOOS="$$goos" GOARCH="$$goarch" go build $(LDFLAGS) -o "$$stage/conveyord" ./cmd/conveyord; \
			cp LICENSE "$$stage/LICENSE"; \
			tar -C "$(RELEASE_DIR)" -czf "$(RELEASE_DIR)/$$name.tar.gz" "$$name"; \
			rm "$$stage/conveyor" "$$stage/conveyord" "$$stage/LICENSE"; rmdir "$$stage"; \
		done; \
		cd "$(RELEASE_DIR)"; \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum *.tar.gz > checksums.txt; else shasum -a 256 *.tar.gz > checksums.txt; fi

test-release:
	$(VALIDATION_CHILD_ENV) sh scripts/test-install.sh

web-deps:
	cd web && npm ci

web-typecheck: web-deps
	cd web && npm run typecheck

ui: web-deps
	cd web && npm run build

dashboard-fresh: ui
	git diff --exit-code -- internal/httpapi/dashboard

TEST_PREREQUISITES = compose-check dashboard-fresh test-release test-validation test-validation-environment vk10-runtime
test: $(TEST_PREREQUISITES)
	$(RUN_GO_UNIT_TESTS)
	$(RUN_WEB_TESTS)

# CI splits make test into test-go and PLAYWRIGHT_SHARD_COUNT Playwright
# shards. Together they run the same prerequisites, Go tests, Biome lint, and
# Playwright suite; make test stays the local aggregate.
PLAYWRIGHT_SHARD_COUNT = 4
PLAYWRIGHT_SHARD ?=
.PHONY: test-go test-playwright-shard
test-go: $(TEST_PREREQUISITES)
	$(RUN_GO_UNIT_TESTS)
	$(RUN_WEB_LINT)

test-playwright-shard: browser-runtime
	@case " $(shell seq 1 $(PLAYWRIGHT_SHARD_COUNT)) " in *" $(PLAYWRIGHT_SHARD) "*) ;; *) echo "PLAYWRIGHT_SHARD must be one of 1..$(PLAYWRIGHT_SHARD_COUNT), got '$(PLAYWRIGHT_SHARD)'" >&2; exit 1;; esac
	$(RUN_PLAYWRIGHT) --shard=$(PLAYWRIGHT_SHARD)/$(PLAYWRIGHT_SHARD_COUNT) $(PLAYWRIGHT_ARGS)

test-web: web-typecheck browser-runtime
	$(RUN_WEB_TESTS)

test-ui: ui
	cd web && CONVEYOR_VALIDATION_TASK=$(VALIDATION_TASK) PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- $(PLAYWRIGHT_ARGS)

test-ui-evidence: ui
	cd web && CONVEYOR_VALIDATION_TASK=$(VALIDATION_TASK) PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- tests/task-full.spec.ts --grep "review card renders authorized verification evidence"

compose-check:
	$(VALIDATION_CHILD_ENV) python3 scripts/validate_compose_isolation.py

# The PostgreSQL gate is the union of its shards. CI runs one shard per job by
# setting POSTGRES_INTEGRATION_SHARD; an empty value runs every shard in one
# go test invocation. Only shards that run the VK-10 browser scenario
# (internal/dispatch) build the CLI and install Chromium.
POSTGRES_INTEGRATION_SHARDS = store cli daemon
POSTGRES_INTEGRATION_PACKAGES_store = ./internal/store/postgres
POSTGRES_INTEGRATION_PACKAGES_cli = ./cmd/conveyor
POSTGRES_INTEGRATION_PACKAGES_daemon = ./cmd/conveyord ./internal/dispatch
POSTGRES_INTEGRATION_RUNTIME_daemon = vk10-runtime
POSTGRES_INTEGRATION_SHARD ?=
postgres_integration_selected = $(or $(POSTGRES_INTEGRATION_SHARD),$(POSTGRES_INTEGRATION_SHARDS))
POSTGRES_INTEGRATION_PACKAGES = $(foreach shard,$(postgres_integration_selected),$(POSTGRES_INTEGRATION_PACKAGES_$(shard)))
POSTGRES_INTEGRATION_RUNTIME = $(sort $(foreach shard,$(postgres_integration_selected),$(POSTGRES_INTEGRATION_RUNTIME_$(shard))))

.PHONY: _postgres-integration-shard-check _postgres-integration-url-check
_postgres-integration-shard-check:
	@for shard in $(postgres_integration_selected); do case " $(POSTGRES_INTEGRATION_SHARDS) " in *" $$shard "*) ;; *) echo "unknown POSTGRES_INTEGRATION_SHARD $$shard; expected one of: $(POSTGRES_INTEGRATION_SHARDS)" >&2; exit 1;; esac; done

_postgres-integration-url-check:
	@test -n "$(CONVEYOR_TEST_DATABASE_URL)" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)

test-integration: compose-check test-validation-docker vk10-runtime
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then \
			$(MAKE) _test-integration-postgres; \
		else \
			state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(VALIDATION_TASK)/fixtures/postgres-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
			$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(VALIDATION_TASK)" --managed-postgres --backend postgres --url-env TEST_DATABASE_URL --prepared-url-env CONVEYOR_TEST_DATABASE_URL --state "$$state" -- $(MAKE) _test-integration-postgres; \
		fi

# The URL and shard checks run before the browser runtime is built.
test-integration-ci: _postgres-integration-shard-check _postgres-integration-url-check compose-check $(POSTGRES_INTEGRATION_RUNTIME)
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) _test-integration-postgres; else \
		state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(CI_VALIDATION_TASK)/fixtures/postgres-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(CI_VALIDATION_TASK)" --backend postgres --url-env CONVEYOR_TEST_DATABASE_URL --prepared-url-env CONVEYOR_TEST_DATABASE_URL --state "$$state" -- $(MAKE) _test-integration-postgres; \
	fi

.PHONY: _test-integration-postgres
_test-integration-postgres:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "PostgreSQL fixture was not prepared" >&2; exit 1)
	@test -n "$$CONVEYOR_TEST_DATABASE_URL" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	CONVEYOR_REVIEW_SUMMARY_STRESS_ROUNDS=$(REVIEW_SUMMARY_STRESS_ROUNDS_FULL) $(GO_TEST_REQUIRE_TESTS) -v -p=1 $(POSTGRES_INTEGRATION_PACKAGES) -count=1 -timeout=10m $(GO_INTEGRATION_FLAGS)

# Keep the accepted work-order validation command explicit while sharing the
# integration suite's isolated Postgres lifecycle.
test-postgres: test-integration

# Without INVOCATION, prints the port and project a new invocation would use.
test-db-identity:
	@$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py identity $(if $(INVOCATION),--invocation "$(INVOCATION)")

# Starts a detached managed invocation and prints its INVOCATION reference.
test-db-up:
	$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py up --task "$(VALIDATION_TASK)"

# Removes only the sealed resources of the named invocation after identity checks.
test-db-down:
	@test -n "$(INVOCATION)" || (echo "INVOCATION=<invocation directory printed by make test-db-up> is required" >&2; exit 1)
	$(VALIDATION_CHILD_ENV) python3 scripts/validation_resources.py recover --invocation "$(INVOCATION)"

vet:
	$(VALIDATION_CHILD_ENV) go vet ./...

plugin-check:
	$(VALIDATION_CHILD_ENV) python3 scripts/validate_codex_plugin.py

fmt:
	gofmt -l -w .

fmt-check:
	@files="$$($(VALIDATION_CHILD_ENV) gofmt -l .)"; test -z "$$files" || (echo "$$files"; exit 1)

tidy:
	go mod tidy

clean:
	rm -rf $(BIN)

db-up:
	$(DEV_COMPOSE) up -d --wait postgres

db-down:
	$(DEV_COMPOSE) down

run:
	CONVEYOR_ENV_FILE=$(abspath $(ENV_FILE)) $(BIN)/conveyord -config $(CONVEYOR_CONFIG) -addr $(LISTEN_ADDR) -poll-github $(POLL_GITHUB)

build-run: build
	$(MAKE) run

dev: db-up
	$(MAKE) build-run

.PHONY: test-integration-singlestore-ci test-singlestore-unit

# The SingleStore gate is the union of its shards, each one go test command
# built from the shard's packages, -run and -skip patterns, and timeout.
# The two conformance shards select complementary subtests of
# TestSingleStoreConformanceIntegration through one shared pattern. The
# proofs shard runs the heavy artifact-hot-read, hot-read and activity proofs,
# and the migrations shard the DDL-heavy migration and layout tests and the
# VK-10 browser scenario, each by full test name; the packages shard skips the
# conformance test and both lists, so together they run exactly the three
# packages. The commands shard keeps the command and daemon cases. CI runs one
# shard per job through SINGLESTORE_INTEGRATION_SHARD; an empty value runs
# every shard in order. Only the migrations shard, which runs the VK-10
# browser scenario, builds the CLI and installs Chromium. RunAll registers
# every suite in each conformance shard, so its registration audit runs in
# both. make singlestore-shard-union-check proves the union.
SINGLESTORE_INTEGRATION_SHARDS = conformance-1 conformance-2 packages proofs migrations commands
SINGLESTORE_CONFORMANCE_TEST = ^TestSingleStoreConformanceIntegration$$
SINGLESTORE_CONFORMANCE_FIRST = ^(Verification|Requirements|NextAction|Lineage|AutomaticRecovery|ListPages|ActivityMarkerBounds|SystemDesignDrift)$$
SINGLESTORE_PROOF_TESTS = ^(TestActivityValidationSeesRowOnlyWritesAndLateCommitsIntegration|TestArtifactHotReadLayoutsIntegration|TestArtifactHotReadRowstoreLayoutsIntegration|TestHotReadActualMixedOriginAndInterruptedBootstrapIntegration|TestHotReadEgressMeasurementIntegration|TestHotReadEventLookupsIntegration|TestHotReadIndexKeyKindMatchesEngine|TestHotReadLayoutDatabaseIntegration|TestHotReadLayoutDiscovery|TestHotReadLayoutFreshDatabaseIntegration|TestHotReadLayoutIncompatibleDatabaseIntegration|TestHotReadLayoutMixedProvenanceIntegration|TestHotReadLayoutOldOperatorEventsIntegration|TestHotReadLookupNamesDigestOnlyWhenKeyIsReady|TestHotReadOperatorScriptEngineVariantsIntegration|TestHotReadPrepareScriptIntegration|TestHotReadResumeWrongExistingObjectFailsBeforeDDLIntegration|TestHotReadRowstoreArtifactLinksStoreReadinessIntegration|TestHotReadTaskLookupsIntegration)$$
SINGLESTORE_MIGRATION_TESTS = ^(TestActivityValidationTransfersFixedSizeIntegration|TestExistingColumnstoreEventLogPreservedIntegration|TestHistoryMigrationLayoutsIntegration|TestFeatureRetirementPartialDDLRetryIntegration|TestVK10SingleStoreScenarioIntegration)$$
SINGLESTORE_INTEGRATION_PACKAGES_conformance-1 = ./internal/store/singlestore
SINGLESTORE_INTEGRATION_RUN_conformance-1 = $(SINGLESTORE_CONFORMANCE_TEST)/$(SINGLESTORE_CONFORMANCE_FIRST)
SINGLESTORE_INTEGRATION_TIMEOUT_conformance-1 = 20m
SINGLESTORE_INTEGRATION_PACKAGES_conformance-2 = ./internal/store/singlestore
SINGLESTORE_INTEGRATION_RUN_conformance-2 = $(SINGLESTORE_CONFORMANCE_TEST)
SINGLESTORE_INTEGRATION_SKIP_conformance-2 = $(SINGLESTORE_CONFORMANCE_TEST)/$(SINGLESTORE_CONFORMANCE_FIRST)
SINGLESTORE_INTEGRATION_TIMEOUT_conformance-2 = 20m
SINGLESTORE_INTEGRATION_PACKAGES_packages = ./internal/eventlog/s2log ./internal/store/storetest ./internal/store/singlestore
SINGLESTORE_INTEGRATION_SKIP_packages = $(SINGLESTORE_CONFORMANCE_TEST)|$(SINGLESTORE_PROOF_TESTS)|$(SINGLESTORE_MIGRATION_TESTS)
SINGLESTORE_INTEGRATION_TIMEOUT_packages = 20m
SINGLESTORE_INTEGRATION_PACKAGES_proofs = ./internal/store/singlestore
SINGLESTORE_INTEGRATION_RUN_proofs = $(SINGLESTORE_PROOF_TESTS)
SINGLESTORE_INTEGRATION_TIMEOUT_proofs = 20m
SINGLESTORE_INTEGRATION_PACKAGES_migrations = ./internal/store/singlestore
SINGLESTORE_INTEGRATION_RUN_migrations = $(SINGLESTORE_MIGRATION_TESTS)
SINGLESTORE_INTEGRATION_TIMEOUT_migrations = 20m
SINGLESTORE_INTEGRATION_RUNTIME_migrations = vk10-runtime
SINGLESTORE_INTEGRATION_PACKAGES_commands = ./cmd/conveyor ./cmd/conveyord
SINGLESTORE_INTEGRATION_RUN_commands = TestSingleStoreInitAndUserIntegration|TestConveyordDurableStartupIntegration|TestConveyordIdleRuntimeIntegration
SINGLESTORE_INTEGRATION_TIMEOUT_commands = 10m
# The three packages the gate runs whole; the commands shard adds its cases
# from the command packages.
SINGLESTORE_INTEGRATION_WHOLE_PACKAGES = ./internal/eventlog/s2log ./internal/store/storetest ./internal/store/singlestore
# The command cases the gate requires, as package:test; the union check fails
# unless exactly one shard runs each.
SINGLESTORE_INTEGRATION_REQUIRED_CASES = ./cmd/conveyor:TestSingleStoreInitAndUserIntegration ./cmd/conveyord:TestConveyordDurableStartupIntegration ./cmd/conveyord:TestConveyordIdleRuntimeIntegration
singlestore_integration_args = $(SINGLESTORE_INTEGRATION_PACKAGES_$(1))$(if $(SINGLESTORE_INTEGRATION_RUN_$(1)), -run '$(SINGLESTORE_INTEGRATION_RUN_$(1))')$(if $(SINGLESTORE_INTEGRATION_SKIP_$(1)), -skip '$(SINGLESTORE_INTEGRATION_SKIP_$(1))') -timeout=$(SINGLESTORE_INTEGRATION_TIMEOUT_$(1))
SINGLESTORE_INTEGRATION_SHARD ?=
singlestore_integration_selected = $(or $(SINGLESTORE_INTEGRATION_SHARD),$(SINGLESTORE_INTEGRATION_SHARDS))
SINGLESTORE_INTEGRATION_RUNTIME = $(sort $(foreach shard,$(singlestore_integration_selected),$(SINGLESTORE_INTEGRATION_RUNTIME_$(shard))))

.PHONY: _singlestore-integration-shard-check _singlestore-integration-url-check _test-singlestore-shard
_singlestore-integration-shard-check:
	@for shard in $(singlestore_integration_selected); do case " $(SINGLESTORE_INTEGRATION_SHARDS) " in *" $$shard "*) ;; *) echo "unknown SINGLESTORE_INTEGRATION_SHARD $$shard; expected one of: $(SINGLESTORE_INTEGRATION_SHARDS)" >&2; exit 1;; esac; done

_singlestore-integration-url-check:
	@test -n "$$CONVEYOR_TEST_SINGLESTORE_URL" || (echo "CONVEYOR_TEST_SINGLESTORE_URL is required" >&2; exit 1)

# The URL and shard checks run before the browser runtime is built.
test-integration-singlestore-ci: _singlestore-integration-shard-check _singlestore-integration-url-check $(SINGLESTORE_INTEGRATION_RUNTIME)
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) _test-integration-singlestore; else \
		state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(CI_VALIDATION_TASK)/fixtures/singlestore-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(CI_VALIDATION_TASK)" --backend singlestore --url-env CONVEYOR_TEST_SINGLESTORE_URL --prepared-url-env CONVEYOR_TEST_SINGLESTORE_URL --state "$$state" -- $(MAKE) _test-integration-singlestore; \
	fi

.PHONY: _test-integration-singlestore
_test-integration-singlestore:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "SingleStore fixture was not prepared" >&2; exit 1)
	@test -n "$$CONVEYOR_TEST_SINGLESTORE_URL" || (echo "CONVEYOR_TEST_SINGLESTORE_URL is required" >&2; exit 1)
	@for shard in $(singlestore_integration_selected); do $(MAKE) --no-print-directory _test-singlestore-shard SINGLESTORE_INTEGRATION_SHARD=$$shard || exit 1; done

_test-singlestore-shard:
	@test -n "$(SINGLESTORE_INTEGRATION_PACKAGES_$(SINGLESTORE_INTEGRATION_SHARD))" || (echo "unknown SINGLESTORE_INTEGRATION_SHARD '$(SINGLESTORE_INTEGRATION_SHARD)'; expected one of: $(SINGLESTORE_INTEGRATION_SHARDS)" >&2; exit 1)
	$(GO_TEST_REQUIRE_TESTS) -v -p=1 $(call singlestore_integration_args,$(SINGLESTORE_INTEGRATION_SHARD)) -count=1 $(GO_INTEGRATION_FLAGS)

# Prints the CI shard matrices as JSON for the workflow's plan job, so every
# shard Make defines runs in its own CI job.
.PHONY: ci-shard-matrix
ci-shard-matrix:
	@json() { printf '['; sep=; for item in "$$@"; do printf '%s"%s"' "$$sep" "$$item"; sep=,; done; printf ']'; }; \
		echo "playwright=$$(json $(shell seq 1 $(PLAYWRIGHT_SHARD_COUNT)))"; \
		echo "postgres=$$(json $(POSTGRES_INTEGRATION_SHARDS))"; \
		echo "singlestore=$$(json $(SINGLESTORE_INTEGRATION_SHARDS))"

# Proves that the SingleStore shards together run the unsharded gate: every
# test of the whole packages, and each top-level conformance suite, runs in
# exactly one shard, and no test runs in two. go test -list names each
# package's top-level tests without a database, and the conformance suites
# are the keys of storetest's suiteMethods, which RunAll registers each
# exactly once (its registration audit fails otherwise); -list cannot see
# subtests. The check applies each shard's -run and -skip to every name as go
# test does: patterns split at top-level | into alternatives and at / into
# levels, -run matches a name whose levels match, and -skip removes a name
# whose levels match every skip level. Every name a pattern spells must exist,
# so a misspelt name cannot silently move a test. The whole packages and the
# required command cases' packages are always listed, so a package dropped
# from every shard still fails as running in no shard, and each required
# command case must run in exactly one shard. The Plan CI shards job runs it
# on every CI run.
define singlestore_shard_union_awk
function split_top(s, sep, out,    n, i, c, cs, cp, cur) {
	n = 0; cs = 0; cp = 0; cur = ""
	for (i = 1; i <= length(s); i++) {
		c = substr(s, i, 1)
		if (c == "\\") { cur = cur c substr(s, i + 1, 1); i++; continue }
		if (c == "[") cs++
		else if (c == "]") { cs--; if (cs < 0) cs = 0 }
		else if (c == "(" && cs == 0) cp++
		else if (c == ")" && cs == 0) cp--
		else if (c == sep && cs == 0 && cp == 0) { out[++n] = cur; cur = ""; continue }
		cur = cur c
	}
	out[++n] = cur
	return n
}
# 0: no match; 1: every pattern level matches; 2: a partial match, the name
# has fewer levels than the pattern.
function match_pattern(pattern, path,    alts, na, a, elems, ne, parts, np, i, ok) {
	np = split(path, parts, "/")
	na = split_top(pattern, "|", alts)
	for (a = 1; a <= na; a++) {
		ne = split_top(alts[a], "/", elems)
		ok = 1
		for (i = 1; i <= np && i <= ne; i++) if (parts[i] !~ elems[i]) { ok = 0; break }
		if (ok) return np < ne ? 2 : 1
	}
	return 0
}
function selected(s, path) {
	if (run[s] != "" && match_pattern(run[s], path) == 0) return 0
	if (skip[s] != "" && match_pattern(skip[s], path) == 1) return 0
	return 1
}
function add(pkg, name,    k) {
	known[pkg, name] = 1
	if (pkg == conf_pkg && match_pattern(conf_test, name) == 1) {
		conformance++
		for (k = 1; k <= nsuite; k++) { known[pkg, suite[k]] = 1; entries++; epkg[entries] = pkg; epath[entries] = name "/" suite[k] }
		return
	}
	entries++; epkg[entries] = pkg; epath[entries] = name
}
function fail(message) { print "singlestore-shard-union-check: " message > "/dev/stderr"; failed = 1 }
BEGIN { FS = "\t" }
FILENAME == ARGV[1] { if ($1 != "") suite[++nsuite] = $1; next }
FILENAME == ARGV[2] { shard[++nshard] = $1; pkgs[$1] = " " $2 " "; run[$1] = $3; skip[$1] = $4; next }
/^(ok|FAIL|\?)[ \t]/ {
	split($0, f, /[ \t]+/)
	pkg = "./" substr(f[2], length(module) + 2)
	for (i = 1; i <= npending; i++) add(pkg, pending[i])
	npending = 0
	next
}
/^(Test|Example|Fuzz)/ { pending[++npending] = $0 }
END {
	if (nsuite == 0) fail("found no conformance suite in storetest suiteMethods")
	if (conformance != 1) fail("found the conformance test " conformance " times, want once")
	for (e = 1; e <= entries; e++) {
		for (s = 1; s <= nshard; s++) {
			if (index(pkgs[shard[s]], " " epkg[e] " ") && selected(shard[s], epath[e])) { count[e]++; where[e] = where[e] " " shard[s]; per[shard[s]]++ }
		}
		if (count[e] > 1) fail(epkg[e] " " epath[e] " runs in" where[e])
		else if (count[e] == 0 && (index(" " whole " ", " " epkg[e] " ") || index(" " required " ", " " epkg[e] ":" epath[e] " "))) fail(epkg[e] " " epath[e] " runs in no shard")
		else if (count[e] == 1) total++
	}
	nreq = split(required, req, " ")
	for (r = 1; r <= nreq; r++) {
		split(req[r], rp, ":")
		if (!((rp[1], rp[2]) in known)) fail("required command case " req[r] " is no test in its package")
	}
	for (s = 1; s <= nshard; s++) {
		patterns = run[shard[s]] " " skip[shard[s]]
		gsub(/[^A-Za-z0-9_]+/, " ", patterns)
		n = split(patterns, words, " ")
		for (w = 1; w <= n; w++) {
			found = 0
			for (key in known) { split(key, kp, SUBSEP); if (kp[2] == words[w] && index(pkgs[shard[s]], " " kp[1] " ")) { found = 1; break } }
			if (!found) fail("shard " shard[s] " names " words[w] ", which is no test or suite in its packages")
		}
		printf "%s: %d tests and conformance suites\n", shard[s], per[shard[s]]
	}
	if (failed) exit 1
	printf "singlestore-shard-union-check: %d tests and conformance suites each run in exactly one of %d shards\n", total, nshard
}
endef

.PHONY: singlestore-shard-union-check
singlestore-shard-union-check: export SINGLESTORE_SHARD_UNION_AWK = $(value singlestore_shard_union_awk)
singlestore-shard-union-check:
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		sed -n '/^var suiteMethods = /,/^}/p' internal/store/storetest/coverage.go | grep -oE '^[[:space:]]+"[A-Za-z0-9]+":' | tr -d ' \t":' > "$$tmp/suites"; \
		printf '%s\t%s\t%s\t%s\n' $(foreach shard,$(SINGLESTORE_INTEGRATION_SHARDS),'$(shard)' '$(SINGLESTORE_INTEGRATION_PACKAGES_$(shard))' '$(SINGLESTORE_INTEGRATION_RUN_$(shard))' '$(SINGLESTORE_INTEGRATION_SKIP_$(shard))') > "$$tmp/shards"; \
		if ! $(VALIDATION_CHILD_ENV) go test -vet=off -list '.*' $(sort $(SINGLESTORE_INTEGRATION_WHOLE_PACKAGES) $(foreach case,$(SINGLESTORE_INTEGRATION_REQUIRED_CASES),$(firstword $(subst :, ,$(case)))) $(foreach shard,$(SINGLESTORE_INTEGRATION_SHARDS),$(SINGLESTORE_INTEGRATION_PACKAGES_$(shard)))) > "$$tmp/list" 2>&1; then cat "$$tmp/list" >&2; exit 1; fi; \
		awk -v module="$$(go list -m)" -v conf_pkg='$(SINGLESTORE_INTEGRATION_PACKAGES_conformance-1)' -v conf_test='$(SINGLESTORE_CONFORMANCE_TEST)' -v whole='$(SINGLESTORE_INTEGRATION_WHOLE_PACKAGES)' -v required='$(SINGLESTORE_INTEGRATION_REQUIRED_CASES)' "$$SINGLESTORE_SHARD_UNION_AWK" "$$tmp/suites" "$$tmp/shards" "$$tmp/list"

test-singlestore-unit:
	CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./internal/eventlog/s2log ./internal/store/singlestore ./internal/store/backend ./internal/store/storetest ./internal/store ./internal/config ./cmd/conveyor ./cmd/conveyord

.PHONY: smoke-singlestore
smoke-singlestore:
	@test -n "$$CONVEYOR_DATABASE_URL" || (echo "CONVEYOR_DATABASE_URL must name a fresh SingleStore database" >&2; exit 1)
	@test -n "$(SMOKE_OUTPUT)" || (echo "SMOKE_OUTPUT must name a new artifact directory" >&2; exit 1)
	python3 scripts/smoke-singlestore-admission.py --bin-dir "$(BIN)" --output "$(SMOKE_OUTPUT)"

# Focused onboarding checks before the repository-wide delivery gates.
.PHONY: test-repository-install
test-repository-install:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/config ./internal/gitx ./internal/httpapi ./internal/store ./internal/store/storetest ./internal/store/postgres ./internal/store/singlestore -run 'TestRepository|TestMemoryConformance|TestBackendCoverage|TestEmbeddedMigrationVersionsAreUnique'

# GitHub App storage, browser-session handshake, minting, and resolver checks.
.PHONY: test-github-apps
test-github-apps:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/trigger/github ./internal/httpapi ./internal/dispatch ./internal/workorder ./internal/redact ./internal/store ./internal/store/storetest ./internal/store/postgres ./internal/store/singlestore ./cmd/conveyord -run 'TestApp|TestGitHubApp|TestWorkspaceGitHubApp|TestWorkspaceForge|TestMemoryConformance|TestBackendCoverage|TestEmbeddedMigrationVersionsAreUnique'

.PHONY: test-task-start-over
test-task-start-over:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/core ./internal/taskops ./internal/store ./internal/store/storetest ./internal/store/postgres ./internal/store/singlestore ./internal/httpapi -run 'TestTaskStartOver|TestStartOver|TestMemoryConformance|TestBackendCoverage|TestEmbeddedMigrationVersionsAreUnique'

.PHONY: test-document-events test-document-event-plans test-document-event-plans-singlestore _test-document-event-plans-singlestore
# Focused iteration; the full local and configured backend gates remain required.
test-document-events:
	go test -v ./internal/store ./internal/httpapi ./internal/store/postgres ./internal/store/singlestore -run 'TestMemoryConformance/Requirements/.*document|TestPostgresConformanceIntegration/Requirements/.*document|TestSingleStoreConformanceIntegration/Requirements/.*document|TestDocumentEvent|TestSystemDesignEventLookupIndexIntegration' -count=1

# Supply the Make-managed disposable database through CONVEYOR_TEST_DATABASE_URL.
test-document-event-plans:
	@test -n "$$CONVEYOR_TEST_DATABASE_URL" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	CONVEYOR_DOCUMENT_EVENT_MEASUREMENT=370000 go test -v ./internal/store/postgres -run '^TestDocumentEventQueryPlansIntegration$$' -count=1 -timeout=15m

# Supply a SingleStore server whose configured database ends in _test through
# CONVEYOR_TEST_SINGLESTORE_URL. The fixture lifecycle creates and removes one
# owned database. DOCUMENT_EVENT_MEASUREMENT=0 checks results and plans only.
DOCUMENT_EVENT_MEASUREMENT ?= 200000
test-document-event-plans-singlestore: _singlestore-integration-url-check
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) --no-print-directory _test-document-event-plans-singlestore; else \
		state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(CI_VALIDATION_TASK)/fixtures/singlestore-document-plans-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(CI_VALIDATION_TASK)" --backend singlestore --url-env CONVEYOR_TEST_SINGLESTORE_URL --prepared-url-env CONVEYOR_TEST_SINGLESTORE_URL --state "$$state" -- $(MAKE) --no-print-directory _test-document-event-plans-singlestore; \
	fi

_test-document-event-plans-singlestore:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "SingleStore fixture was not prepared" >&2; exit 1)
	CONVEYOR_DOCUMENT_EVENT_MEASUREMENT=$(DOCUMENT_EVENT_MEASUREMENT) $(GO_TEST_REQUIRE_TESTS) -v ./internal/store/singlestore -run '^TestDocumentEventQueryPlansIntegration$$' -count=1 -timeout=45m

# Focused hot-read egress measurements: warmup, then five interleaved
# baseline/candidate trials per registered case, recorded from the store's
# driver read statistics. Standard output carries only the machine-readable
# READ_EGRESS_MEASUREMENT lines; capture it into the task validation evidence
# directory. A missing URL, fixture failure, skipped or unselected test, or
# missing measurement fails the target. These are focused measurements, not a
# substitute for the CI gates. Registered cases are listed in
# internal/store/{postgres,singlestore}/read_egress_measurement_integration_test.go.
.PHONY: measure-hot-read-egress-singlestore measure-hot-read-egress-postgres _measure-hot-read-egress-singlestore _measure-hot-read-egress-postgres _hot-read-egress-run
measure-hot-read-egress-singlestore: _singlestore-integration-url-check
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) --no-print-directory _measure-hot-read-egress-singlestore; else \
		state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(CI_VALIDATION_TASK)/fixtures/singlestore-hot-read-egress-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(CI_VALIDATION_TASK)" --backend singlestore --url-env CONVEYOR_TEST_SINGLESTORE_URL --prepared-url-env CONVEYOR_TEST_SINGLESTORE_URL --state "$$state" -- $(MAKE) --no-print-directory _measure-hot-read-egress-singlestore; \
	fi

_measure-hot-read-egress-singlestore:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "SingleStore fixture was not prepared" >&2; exit 1)
	@$(MAKE) --no-print-directory _hot-read-egress-run HOT_READ_EGRESS_PKG=singlestore

measure-hot-read-egress-postgres:
	@test -n "$$CONVEYOR_TEST_DATABASE_URL" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) --no-print-directory _measure-hot-read-egress-postgres; else \
		state="$${CONVEYOR_FIXTURE_STATE:-$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$(CI_VALIDATION_TASK)/fixtures/postgres-hot-read-egress-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$}"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --task "$(CI_VALIDATION_TASK)" --backend postgres --url-env CONVEYOR_TEST_DATABASE_URL --prepared-url-env CONVEYOR_TEST_DATABASE_URL --state "$$state" -- $(MAKE) --no-print-directory _measure-hot-read-egress-postgres; \
	fi

_measure-hot-read-egress-postgres:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "PostgreSQL fixture was not prepared" >&2; exit 1)
	@$(MAKE) --no-print-directory _hot-read-egress-run HOT_READ_EGRESS_PKG=postgres

# Runs the focused measurement test, prints only its measurement lines to
# standard output and the test log to standard error, and fails when the test
# fails, is skipped, or emits no measurement.
# Override with ./internal/httpapi to measure memo routes on the same owned
# backend fixture; CONVEYOR_READ_EGRESS_MEMO_BACKEND selects that backend.
HOT_READ_EGRESS_PACKAGE ?= ./internal/store/$(HOT_READ_EGRESS_PKG)
_hot-read-egress-run:
	@test -n "$(HOT_READ_EGRESS_PKG)" || (echo "HOT_READ_EGRESS_PKG is required" >&2; exit 1)
	@out=$$(mktemp "$${TMPDIR:-/tmp}/hot-read-egress.XXXXXX") && trap 'rm -f "$$out"' EXIT && \
	if ! CONVEYOR_READ_EGRESS_MEASUREMENT=1 $(GO_TEST_REQUIRE_TESTS) -v $(HOT_READ_EGRESS_PACKAGE) -run '^TestHotReadEgressMeasurementIntegration$$' -count=1 -timeout=45m >"$$out" 2>&1; then \
		cat "$$out" >&2; echo "hot-read egress measurement test failed" >&2; exit 1; \
	fi && \
	if ! grep -q '^READ_EGRESS_MEASUREMENT ' "$$out" || grep -q -- '--- SKIP' "$$out"; then \
		cat "$$out" >&2; echo "no hot-read egress measurement was recorded (test skipped or unselected)" >&2; exit 1; \
	fi && \
	grep -v '^READ_EGRESS_MEASUREMENT ' "$$out" >&2 && grep '^READ_EGRESS_MEASUREMENT ' "$$out"

# Focused context-entry and corpus regressions; full validation remains make test.
.PHONY: test-context-eligibility
test-context-eligibility:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/store ./internal/store/storetest ./internal/store/postgres ./internal/store/singlestore ./internal/corpus ./internal/planning ./internal/httpapi

.PHONY: test-artifact-media
test-artifact-media:
	go test ./internal/core -run TestArtifactMedia -count=1

.PHONY: test-artifacts
test-artifacts:
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= go test ./internal/core ./internal/store ./internal/store/storetest ./internal/store/postgres ./internal/store/singlestore ./internal/httpapi ./internal/inprocess ./cmd/conveyor -run 'Artifact|Attachment|Image|Conformance' -count=1
