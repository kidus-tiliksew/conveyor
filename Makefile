# Local integration tests derive a stable identity from the canonical worktree
# path: POSIX cksum selects port 20000-29999, and the port suffix scopes the
# Compose project. Hash collisions fail at port binding instead of sharing a
# database. CONVEYOR_TEST_POSTGRES_PORT or TEST_POSTGRES_PORT pins both values.
# Run `make test-db-down` in a worktree to remove only that worktree's database.
BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X github.com/kidus-tiliksew/conveyor/internal/releaseinfo.Version=$(VERSION)"
ENV_FILE ?= .env
CONVEYOR_CONFIG ?= conveyor.yaml
LISTEN_ADDR ?= 127.0.0.1:8080
POLL_GITHUB ?= 60s
IMAGE ?= conveyor:local
TEST_WORKTREE_PATH := $(shell cd "$(dir $(abspath $(lastword $(MAKEFILE_LIST))))" && pwd -P)
TEST_POSTGRES_PORT_DERIVED := $(shell printf '%s\n' "$(TEST_WORKTREE_PATH)" | cksum | awk '{print 20000 + ($$1 % 10000)}')
ifeq ($(origin TEST_POSTGRES_PORT),undefined)
TEST_POSTGRES_PORT := $(if $(strip $(CONVEYOR_TEST_POSTGRES_PORT)),$(CONVEYOR_TEST_POSTGRES_PORT),$(TEST_POSTGRES_PORT_DERIVED))
endif
TEST_COMPOSE_PROJECT := conveyor-test-p$(TEST_POSTGRES_PORT)
TEST_DATABASE_URL ?= postgres://conveyor:conveyor@127.0.0.1:$(TEST_POSTGRES_PORT)/conveyor_test?sslmode=disable
export TEST_DATABASE_URL
TEST_COMPOSE_NETWORK_ENV = CONVEYOR_TEST_NETWORK_NAME="$${CONVEYOR_TEST_EXTERNAL_NETWORK:-$(TEST_COMPOSE_PROJECT)_default}" CONVEYOR_TEST_NETWORK_EXTERNAL="$$(if test -n "$${CONVEYOR_TEST_EXTERNAL_NETWORK:-}"; then printf true; else printf false; fi)"
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
RUN_WEB_TESTS = cd web && $(VALIDATION_CHILD_ENV) npm run lint && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- $(PLAYWRIGHT_ARGS)
DEV_COMPOSE := docker compose --env-file $(ENV_FILE) -f compose.dev.yaml

.PHONY: all build image test-image release release-archives test-release web-deps web-typecheck ui dashboard-fresh test test-web test-ui test-ui-evidence compose-check test-integration test-integration-ci test-postgres test-db-identity test-db-up test-db-down vet plugin-check fmt fmt-check tidy clean db-up db-down run build-run dev test-validation-fixtures

all: build

# REQ-7/AC-7.1 (component-verification-strategy): one Make graph shares
# web-deps and ui across the complete ordinary validation session.
.PHONY: validate test-validation test-validation-environment test-worker-environment
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
	$(VALIDATION_CHILD_ENV) go test ./scripts/validation-fixture-sql

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
		-run '^(TestCheckoutCmdSkipsAttachForWorkerAssignment|TestIsolatedChildEnvironmentReplacesLaunchIdentity)$$' -count=1

build: conveyor-cli
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

.PHONY: conveyor-cli browser-runtime vk10-runtime test-vk10
conveyor-cli: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor

# VK-10: each gate builds its own CLI and prepares the browser before fixtures.
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
	docker run --rm --entrypoint /bin/sh "$(IMAGE)" -c 'command -v conveyor >/dev/null && command -v git >/dev/null && command -v gh >/dev/null'

RELEASE_DIR ?= dist
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# REQ-11 / AC-11.1: release archives use the same build-injected identity as
# ordinary builds. VERSION must be the pushed tag when invoked by release.yml.
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

test: compose-check dashboard-fresh test-release test-validation test-validation-environment vk10-runtime
	CONVEYOR_TEST_DATABASE_URL= CONVEYOR_TEST_SINGLESTORE_URL= $(VALIDATION_CHILD_ENV) go test ./...
	$(RUN_WEB_TESTS)

test-web: web-typecheck browser-runtime
	$(RUN_WEB_TESTS)

test-ui: ui
	cd web && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- $(PLAYWRIGHT_ARGS)

test-ui-evidence: ui
	cd web && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) $(VALIDATION_CHILD_ENV) npm run test:e2e -- tests/task-full.spec.ts --grep "review card renders authorized verification evidence"

compose-check:
	$(VALIDATION_CHILD_ENV) python3 scripts/validate_compose_isolation.py

test-integration: compose-check vk10-runtime
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then \
			$(MAKE) _test-integration-postgres; \
		else \
			$(MAKE) test-db-up; \
			trap '$(MAKE) test-db-down' EXIT; \
			state="$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$${CONVEYOR_TASK_ID:-manual-validation}/fixtures/postgres-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$"; \
			$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --backend postgres --url-env TEST_DATABASE_URL --prepared-url-env CONVEYOR_TEST_DATABASE_URL --state "$$state" -- $(MAKE) _test-integration-postgres; \
		fi

test-integration-ci: compose-check vk10-runtime
	@test -n "$(CONVEYOR_TEST_DATABASE_URL)" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) _test-integration-postgres; else \
		state="$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$${CONVEYOR_TASK_ID:-ci-validation}/fixtures/postgres-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --backend postgres --url-env CONVEYOR_TEST_DATABASE_URL --prepared-url-env CONVEYOR_TEST_DATABASE_URL --state "$$state" -- $(MAKE) _test-integration-postgres; \
	fi

.PHONY: _test-integration-postgres
_test-integration-postgres:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "PostgreSQL fixture was not prepared" >&2; exit 1)
	@test -n "$$CONVEYOR_TEST_DATABASE_URL" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	$(VALIDATION_CHILD_ENV) go test -v -p=1 ./cmd/conveyor ./cmd/conveyord ./internal/store/postgres ./internal/dispatch -count=1 -timeout=5m

# Keep the accepted work-order validation command explicit while sharing the
# integration suite's isolated Postgres lifecycle.
test-postgres: test-integration

test-db-identity:
	@printf '%s\t%s\n' '$(TEST_POSTGRES_PORT)' '$(TEST_COMPOSE_PROJECT)'

test-db-up:
	$(TEST_COMPOSE_NETWORK_ENV) CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose -p $(TEST_COMPOSE_PROJECT) --profile test up -d --wait postgres-test

test-db-down:
	$(TEST_COMPOSE_NETWORK_ENV) CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose -p $(TEST_COMPOSE_PROJECT) --profile test down --remove-orphans

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

test-integration-singlestore-ci: vk10-runtime
	@test -n "$$CONVEYOR_TEST_SINGLESTORE_URL" || (echo "CONVEYOR_TEST_SINGLESTORE_URL is required" >&2; exit 1)
	@if test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1; then $(MAKE) _test-integration-singlestore; else \
		state="$${XDG_STATE_HOME:-$$HOME/.local/state}/conveyor/$${CONVEYOR_TASK_ID:-ci-validation}/fixtures/singlestore-$$(date -u +%Y%m%dT%H%M%SZ)-$$$$"; \
		$(VALIDATION_CHILD_ENV) python3 scripts/validation_fixtures.py run --backend singlestore --url-env CONVEYOR_TEST_SINGLESTORE_URL --prepared-url-env CONVEYOR_TEST_SINGLESTORE_URL --state "$$state" -- $(MAKE) _test-integration-singlestore; \
	fi

.PHONY: _test-integration-singlestore
_test-integration-singlestore:
	@test "$${CONVEYOR_FIXTURE_PREPARED:-}" = 1 || (echo "SingleStore fixture was not prepared" >&2; exit 1)
	@test -n "$$CONVEYOR_TEST_SINGLESTORE_URL" || (echo "CONVEYOR_TEST_SINGLESTORE_URL is required" >&2; exit 1)
	$(VALIDATION_CHILD_ENV) go test -v -p=1 ./internal/eventlog/s2log ./internal/store/storetest ./internal/store/singlestore -count=1 -timeout=20m
	$(VALIDATION_CHILD_ENV) go test -v -p=1 ./cmd/conveyor ./cmd/conveyord -run 'TestSingleStoreInitAndUserIntegration|TestConveyordDurableStartupIntegration' -count=1 -timeout=10m

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

.PHONY: test-document-events test-document-event-plans
# Focused iteration; the full local and configured backend gates remain required.
test-document-events:
	go test -v ./internal/store ./internal/httpapi ./internal/store/postgres ./internal/store/singlestore -run 'TestMemoryConformance/Requirements/.*document|TestPostgresConformanceIntegration/Requirements/.*document|TestSingleStoreConformanceIntegration/Requirements/.*document|TestDocumentEvent|TestSystemDesignEventLookupIndexIntegration' -count=1

# Supply the Make-managed disposable database through CONVEYOR_TEST_DATABASE_URL.
test-document-event-plans:
	@test -n "$$CONVEYOR_TEST_DATABASE_URL" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	CONVEYOR_DOCUMENT_EVENT_MEASUREMENT=370000 go test -v ./internal/store/postgres -run '^TestDocumentEventQueryPlansIntegration$$' -count=1 -timeout=15m

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
