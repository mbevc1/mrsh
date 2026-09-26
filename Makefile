# ---- metadata (overridable) ----
BINARY      := mrsh
PKG         := github.com/mbevc1/mrsh
CMD_PKG     := $(PKG)/cmd
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILT_BY    ?= $(shell whoami)

LDFLAGS := -s -w \
  -X $(CMD_PKG).version=$(VERSION) \
  -X $(CMD_PKG).commit=$(COMMIT) \
  -X $(CMD_PKG).date=$(DATE) \
  -X $(CMD_PKG).builtBy=$(BUILT_BY)

# cross-compile matrix
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.DEFAULT_GOAL := build

## build: compile for the host platform into ./$(BINARY)
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## install: build and install into GOBIN
.PHONY: install
install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" .

## run: build then run (use ARGS="-g web run -c uptime")
.PHONY: run
run: build
	./$(BINARY) $(ARGS)

## test: run unit tests with race detector + coverage
.PHONY: test
test:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

## cover: open the HTML coverage report
.PHONY: cover
cover: test
	go tool cover -html=coverage.out

## lint: golangci-lint (install if missing)
.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null || { echo "install golangci-lint: https://golangci-lint.run"; exit 1; }
	golangci-lint run ./...

## vet: go vet
.PHONY: vet
vet:
	go vet ./...

## tidy: go mod tidy + verify
.PHONY: tidy
tidy:
	go mod tidy && go mod verify

## check-tools: verify optional runtime deps present
.PHONY: check-tools
check-tools:
	@command -v ssh >/dev/null && echo "ssh: ok" || echo "ssh: not found (only needed if using ssh-agent auth)"
	@echo "note: v1 needs no external binaries; 'sops' becomes relevant only if config encryption is enabled later"

## release-build: cross-compile all platforms into ./dist
.PHONY: release-build
release-build:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o dist/$(BINARY)_$${os}_$${arch}$$ext . ; \
	done

## snapshot: local goreleaser build without publishing (skips SBOMs without syft)
.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean $(if $(shell command -v syft),,--skip=sbom)

## clean: remove build artifacts
.PHONY: clean
clean:
	rm -rf $(BINARY) dist coverage.out

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
