BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
ENV_FILE ?= .env
CONVEYOR_CONFIG ?= conveyor.yaml
LISTEN_ADDR ?= 127.0.0.1:8080
POLL_GITHUB ?= 60s
TEST_POSTGRES_PORT ?= 5433
TEST_DATABASE_URL ?= postgres://conveyor:conveyor@127.0.0.1:$(TEST_POSTGRES_PORT)/conveyor_test?sslmode=disable
PLAYWRIGHT_ARGS ?=
DEV_COMPOSE := docker compose --env-file $(ENV_FILE) -f compose.dev.yaml

.PHONY: all build ui test test-ui test-ui-evidence compose-check test-integration test-db-up test-db-down vet plugin-check fmt tidy clean db-up db-down run build-run dev

all: build

build: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

ui:
	cd web && npm ci && npm run build

test: compose-check
	CONVEYOR_TEST_DATABASE_URL= go test ./...

test-ui: ui
	cd web && npm run test:e2e -- $(PLAYWRIGHT_ARGS)

test-ui-evidence: ui
	cd web && npm run test:e2e -- tests/task-full.spec.ts --grep "review card renders authorized verification evidence"

compose-check:
	python3 scripts/validate_compose_isolation.py

test-integration: compose-check test-db-up
	@trap '$(MAKE) test-db-down' EXIT; \
		CONVEYOR_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -p=1 ./internal/store/postgres ./internal/dispatch -count=1 -timeout=5m

test-db-up:
	CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose --profile test up -d --wait postgres-test

test-db-down:
	CONVEYOR_TEST_POSTGRES_PORT=$(TEST_POSTGRES_PORT) docker compose --profile test rm -s -f postgres-test

vet:
	go vet ./...

plugin-check:
	python3 scripts/validate_codex_plugin.py

fmt:
	gofmt -l -w .

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
