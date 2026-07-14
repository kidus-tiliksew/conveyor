BIN := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build ui test vet fmt tidy clean

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

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -rf $(BIN)
