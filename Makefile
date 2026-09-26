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

.DEFAULT_GOAL := help

.PHONY: build
build: ## compile for the host platform into ./mrsh
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: install
install: ## build and install into GOBIN
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" .

.PHONY: run
run: build ## build then run (use ARGS="-g web run -c uptime")
	./$(BINARY) $(ARGS)

.PHONY: test
test: ## run unit tests with race detector + coverage
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

.PHONY: cover
cover: test ## open the HTML coverage report
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## golangci-lint (install if missing)
	@command -v golangci-lint >/dev/null || { echo "install golangci-lint: https://golangci-lint.run"; exit 1; }
	golangci-lint run ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy + verify
	go mod tidy && go mod verify

.PHONY: check-tools
check-tools: ## verify optional runtime deps present
	@command -v ssh >/dev/null && echo "ssh: ok" || echo "ssh: not found (only needed if using ssh-agent auth)"
	@echo "note: v1 needs no external binaries; 'sops' becomes relevant only if config encryption is enabled later"

.PHONY: release-build
release-build: ## cross-compile all platforms into ./dist
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o dist/$(BINARY)_$${os}_$${arch}$$ext . ; \
	done

IMAGE ?= mrsh:dev
.PHONY: docker
docker: ## build the container image for the host platform (IMAGE=mrsh:dev)
	docker build -t $(IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

.PHONY: snapshot
snapshot: ## local goreleaser build without publishing (skips SBOMs without syft)
	goreleaser release --snapshot --clean $(if $(shell command -v syft),,--skip=sbom)

.PHONY: clean
clean: ## remove binaries, dist/, coverage and test output, and temp files
	rm -rf $(BINARY) $(BINARY).exe dist coverage.* *.out *.test *.coverprofile profile.cov .*.tmp-*

.PHONY: help
help: ## list targets
	@printf 'Usage: make \033[36m<target>\033[0m [VAR=value]\n\n'
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
