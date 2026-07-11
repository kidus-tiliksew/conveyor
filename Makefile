BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
CODEX_BASE_VERSION ?= 0.142.0
CODEX_BUMP_VERSION ?= 0.143.0
CODEX_BUMP_IMAGE ?= conveyor-base:codex-$(CODEX_BUMP_VERSION)

.PHONY: all build ui shim test vet fmt tidy image resume-experiment-image resume-experiment clean

all: build

build: ui
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord
	go build $(LDFLAGS) -o $(BIN)/conveyor-runner ./cmd/conveyor-runner
	go build -o $(BIN)/conveyor-resume-experiment ./cmd/conveyor-resume-experiment

ui:
	cd web && npm ci && npm run build

# The shim ships inside the (linux) base image regardless of host OS,
# and must stay a dependency-free static binary (spec §17.0).
shim:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BIN)/linux-arm64/conveyor-shim ./cmd/conveyor-shim
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BIN)/linux-amd64/conveyor-shim ./cmd/conveyor-shim

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

image: shim
	cp $(BIN)/linux-$(shell docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')/conveyor-shim images/base/conveyor-shim
	docker build -t conveyor-base:dev images/base
	rm -f images/base/conveyor-shim

# Build the adjacent minor used only by the spec §20.2 compatibility probe.
# Production stays pinned to the adapter-validated version above.
resume-experiment-image: shim
	cp $(BIN)/linux-$(shell docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')/conveyor-shim images/base/conveyor-shim
	docker build --build-arg CODEX_VERSION=$(CODEX_BUMP_VERSION) -t $(CODEX_BUMP_IMAGE) images/base
	rm -f images/base/conveyor-shim

resume-experiment: image resume-experiment-image
	go run ./cmd/conveyor-resume-experiment \
		-base-version $(CODEX_BASE_VERSION) \
		-bump-version $(CODEX_BUMP_VERSION) \
		-bump-image $(CODEX_BUMP_IMAGE)

clean:
	rm -rf $(BIN) images/base/conveyor-shim
