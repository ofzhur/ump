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

RESCUE_ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
CONTAINER  ?= $(shell command -v docker || command -v podman)

.PHONY: all build install test vet fmt fmt-check check scripts-check dist clean help rescue-image rescue-test rescue-upload

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

scripts-check: ## syntax-check the shell scripts of the rescue image
	@for f in images/rescue/*.sh; do bash -n "$$f" || exit 1; done
	@sh -n images/rescue/finish.sh images/rescue/overlay/usr/local/sbin/ump-rescue-setup

check: fmt-check vet test scripts-check ## gofmt, go vet, the tests and the script syntax checks

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

rescue-image: ## build the rescue image for RESCUE_ARCH (amd64|arm64) in a privileged Debian container
	$(CONTAINER) run --rm --privileged \
		-v "$(CURDIR)/images/rescue:/work" -w /work -e VERSION=$(VERSION) debian:trixie ./build.sh $(RESCUE_ARCH)

rescue-test: ## boot the built rescue image for RESCUE_ARCH in QEMU and check it over SSH
	images/rescue/test-boot.sh $(RESCUE_ARCH)

rescue-upload: images/rescue/out/$(RESCUE_ARCH)/manifest.json ## upload the rescue image for RESCUE_ARCH to R2 (builds it first if missing; STORE_* from the environment)
	images/rescue/upload.sh $(RESCUE_ARCH)

images/rescue/out/%/manifest.json:
	$(MAKE) rescue-image RESCUE_ARCH=$*

help: ## list the targets
	@awk -F ':.*## ' '/^[a-z-]+:.*## /{printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
