# UMP — Unus Mundus Provisioner
#
#   make              build ./ump for this machine
#   make install      install ump into $(go env GOBIN), else $(go env GOPATH)/bin
#   make check        gofmt, go vet and the tests (what CI runs)
#   make dist         cross-compile release binaries into dist/
#   make help         list the targets
#
# VERSION is taken from `git describe` (tags v*) and compiled into the
# binary; override it with `make VERSION=v1.2.3`. Binaries are static
# (CGO off), stripped and built with -trimpath.

MODULE     := github.com/ofzhur/ump
BINARY     := ump
VERSION    ?= $(shell git describe --tags --match 'v*' --dirty 2>/dev/null || echo dev)
BUILDFLAGS ?= -trimpath
LDFLAGS    := -s -w -X $(MODULE)/internal/cli.Version=$(VERSION)
PLATFORMS  ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

export CGO_ENABLED := 0

.PHONY: all build install test vet fmt fmt-check check dist clean help

all: build

build: ## build ./ump for the current platform
	go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/ump

install: ## install ump into GOBIN (or GOPATH/bin)
	go install $(BUILDFLAGS) -ldflags '$(LDFLAGS)' ./cmd/ump

test: ## run the tests
	go test ./...

vet: ## run go vet
	go vet ./...

fmt: ## format the sources with gofmt
	gofmt -w cmd internal

fmt-check:
	@out="$$(gofmt -l cmd internal)"; \
	if [ -n "$$out" ]; then echo "files need gofmt:"; echo "$$out"; exit 1; fi

check: fmt-check vet test ## gofmt, go vet and the tests

dist: ## cross-compile into dist/ump-<os>-<arch> for $(PLATFORMS)
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/$(BINARY)-$$os-$$arch; \
		if [ "$$os" = windows ]; then out=$$out.exe; fi; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/ump || exit 1; \
	done

clean: ## remove the binary and dist/
	rm -rf $(BINARY) $(BINARY).exe dist

help: ## list the targets
	@awk -F ':.*## ' '/^[a-z-]+:.*## /{printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
