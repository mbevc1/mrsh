BINARY    := mrsh
MODULE    := github.com/mbevc1/mrsh
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
             -X main.version=$(VERSION) \
             -X main.commit=$(COMMIT) \
             -X main.date=$(DATE) \
             -X main.builtBy=make

GOFLAGS   := -trimpath
BUILD_DIR := dist

.PHONY: all build clean test vet lint fmt coverage install uninstall release help

all: build

## build: compile the binary for the current platform
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

## install: install binary to GOPATH/bin
install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" .

## uninstall: remove installed binary
uninstall:
	rm -f $(shell go env GOPATH)/bin/$(BINARY)

## test: run all tests with race detector
test:
	go test -race ./...

## coverage: run tests and output HTML coverage report
coverage:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## vet: run go vet
vet:
	go vet ./...

## fmt: format source code
fmt:
	gofmt -w -s .

## lint: run golangci-lint (must be installed separately)
lint:
	golangci-lint run ./...

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

## release: build release binaries for all platforms into dist/
release: clean
	mkdir -p $(BUILD_DIR)
	GOOS=linux   GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_linux_amd64   .
	GOOS=linux   GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_linux_arm64   .
	GOOS=darwin  GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_darwin_amd64  .
	GOOS=darwin  GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_darwin_arm64  .
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_windows_amd64.exe .
	GOOS=freebsd GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)_freebsd_amd64 .
	@echo "Binaries written to $(BUILD_DIR)/"

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf $(BUILD_DIR) coverage.out coverage.html

## help: print this help message
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
