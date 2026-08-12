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
TEST_WORKTREE_PATH := $(shell cd "$(dir $(abspath $(lastword $(MAKEFILE_LIST))))" && pwd -P)
TEST_POSTGRES_PORT_DERIVED := $(shell printf '%s\n' "$(TEST_WORKTREE_PATH)" | cksum | awk '{print 20000 + ($$1 % 10000)}')
ifeq ($(origin TEST_POSTGRES_PORT),undefined)
TEST_POSTGRES_PORT := $(if $(strip $(CONVEYOR_TEST_POSTGRES_PORT)),$(CONVEYOR_TEST_POSTGRES_PORT),$(TEST_POSTGRES_PORT_DERIVED))
endif
TEST_COMPOSE_PROJECT := conveyor-test-p$(TEST_POSTGRES_PORT)
TEST_DATABASE_URL ?= postgres://conveyor:conveyor@127.0.0.1:$(TEST_POSTGRES_PORT)/conveyor_test?sslmode=disable
PLAYWRIGHT_ARGS ?=
PLAYWRIGHT_INSTALL_ARGS ?=
PLAYWRIGHT_WORKERS ?= 2
RUN_WEB_TESTS = cd web && npx playwright install $(PLAYWRIGHT_INSTALL_ARGS) chromium && npm run lint && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) npm run test:e2e -- $(PLAYWRIGHT_ARGS)
DEV_COMPOSE := docker compose --env-file $(ENV_FILE) -f compose.dev.yaml

.PHONY: all build web-deps web-typecheck ui dashboard-fresh test test-web test-ui test-ui-evidence compose-check test-integration test-integration-ci test-postgres test-db-identity test-db-up test-db-down vet plugin-check fmt fmt-check tidy clean db-up db-down run build-run dev

all: build

build: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

web-deps:
	cd web && npm ci

web-typecheck: web-deps
	cd web && npm run typecheck

ui: web-deps
	cd web && npm run build

dashboard-fresh: ui
	git diff --exit-code -- internal/httpapi/dashboard

test: compose-check dashboard-fresh
	CONVEYOR_TEST_DATABASE_URL= go test ./...
	$(RUN_WEB_TESTS)

test-web: web-typecheck
	$(RUN_WEB_TESTS)

test-ui: ui
	cd web && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) npm run test:e2e -- $(PLAYWRIGHT_ARGS)

test-ui-evidence: ui
	cd web && PLAYWRIGHT_WORKERS=$(PLAYWRIGHT_WORKERS) npm run test:e2e -- tests/task-full.spec.ts --grep "review card renders authorized verification evidence"

compose-check:
	python3 scripts/validate_compose_isolation.py

test-integration: compose-check test-db-up
	@trap '$(MAKE) test-db-down' EXIT; \
		CONVEYOR_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -p=1 ./cmd/conveyor ./internal/store/postgres ./internal/dispatch -count=1 -timeout=5m

test-integration-ci: compose-check
	@test -n "$(CONVEYOR_TEST_DATABASE_URL)" || (echo "CONVEYOR_TEST_DATABASE_URL is required" >&2; exit 1)
	CONVEYOR_TEST_DATABASE_URL='$(CONVEYOR_TEST_DATABASE_URL)' go test -p=1 ./cmd/conveyor ./internal/store/postgres ./internal/dispatch -count=1 -timeout=5m

# Keep the accepted work-order validation command explicit while sharing the
# integration suite's isolated Postgres lifecycle.
test-postgres: test-integration

test-db-identity:
	@printf '%s\t%s\n' '$(TEST_POSTGRES_PORT)' '$(TEST_COMPOSE_PROJECT)'

test-db-up:
	CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose -p $(TEST_COMPOSE_PROJECT) --profile test up -d --wait postgres-test

test-db-down:
	CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose -p $(TEST_COMPOSE_PROJECT) --profile test rm -s -f postgres-test

vet:
	go vet ./...

plugin-check:
	python3 scripts/validate_codex_plugin.py

fmt:
	gofmt -l -w .

fmt-check:
	@files="$$(gofmt -l .)"; test -z "$$files" || (echo "$$files"; exit 1)

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
