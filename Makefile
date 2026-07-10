BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build shim test vet fmt tidy image clean

all: build

build:
	go build $(LDFLAGS) -o $(BIN)/conveyor ./cmd/conveyor
	go build $(LDFLAGS) -o $(BIN)/conveyord ./cmd/conveyord

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

clean:
	rm -rf $(BIN) images/base/conveyor-shim
