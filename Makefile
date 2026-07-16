BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
ENV_FILE ?= .env
CONVEYOR_CONFIG ?= conveyor.yaml
LISTEN_ADDR ?= 127.0.0.1:8080
POLL_GITHUB ?= 60s

.PHONY: all build ui test vet plugin-check fmt tidy clean db-up db-down run build-run dev

all: build

build: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

ui:
	cd web && npm ci && npm run build

test:
	go test ./...

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
	docker compose --env-file $(ENV_FILE) up -d --wait postgres

db-down:
	docker compose --env-file $(ENV_FILE) down

run:
	CONVEYOR_ENV_FILE=$(abspath $(ENV_FILE)) $(BIN)/conveyord -config $(CONVEYOR_CONFIG) -addr $(LISTEN_ADDR) -poll-github $(POLL_GITHUB)

build-run: build
	$(MAKE) run

dev: db-up
	$(MAKE) build-run
