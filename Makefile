# UMP — Unus Mundus Provisioner
#
#   make              build ./ump for this machine
#   make install      install ump into $(go env GOBIN), else $(go env GOPATH)/bin
#   make check        gofmt, go vet and the tests (what CI runs)
#   make dist         cross-compile release binaries into dist/
#   make release      check, dist, and publish the binaries and get.sh to the storage
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

# the images are built for both architectures; RESCUE_ARCH=amd64 or
# OS_ARCH=arm64 limits a target to one, and the QEMU test takes one
IMAGE_ARCHS ?= amd64 arm64
RESCUE_ARCH ?= $(IMAGE_ARCHS)
OS_ARCH     ?= $(IMAGE_ARCHS)
# the Ubuntu releases to build: OS_LIST="ubuntu-24.04 ubuntu-22.04" for others
OS_LIST     ?= ubuntu-26.04
TEST_ARCH   ?= $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
# the images carry the day they were built as their version
IMAGE_VERSION ?= $(shell date -u +%Y.%m.%d)
CONTAINER  ?= $(shell command -v docker || command -v podman)

.PHONY: all build install test vet fmt fmt-check check scripts-check dist release clean help images rescue-image rescue-test os-image upload

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
	@for f in images/*.sh images/rescue/*.sh images/ubuntu/*.sh install/install.sh; do bash -n "$$f" || exit 1; done
	@sh -n images/rescue/finish.sh images/rescue/overlay/usr/local/sbin/ump-rescue-setup images/ubuntu/finish.sh install/get.sh

check: fmt-check vet test scripts-check ## gofmt, go vet, the tests and the script syntax checks

dist: ## cross-compile into dist/ump-<os>-<arch> for $(PLATFORMS), with SHA256SUMS and VERSION
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/$(BINARY)-$$os-$$arch; \
		if [ "$$os" = windows ]; then out=$$out.exe; fi; \
		echo "  $$out"; \
		GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/ump || exit 1; \
	done
	@cd dist && (command -v sha256sum >/dev/null && sha256sum $(BINARY)-* || shasum -a 256 $(BINARY)-*) > SHA256SUMS
	@echo '$(VERSION)' > dist/VERSION
	@echo "  dist/SHA256SUMS dist/VERSION ($(VERSION))"

release: check dist ## check, dist, and publish the binaries (bin/) and get.sh to the storage (STORE_* from the environment)
	images/upload.sh bin scripts

clean: ## remove the binary and dist/
	rm -rf $(BINARY) $(BINARY).exe dist

images: rescue-image os-image ## build every image for every architecture

rescue-image: ## build the rescue image for RESCUE_ARCH (default: amd64 and arm64) in a privileged Debian container
	@for arch in $(RESCUE_ARCH); do \
		$(CONTAINER) run --rm --privileged \
			-v "$(CURDIR)/images/rescue:/work" -w /work -e VERSION=$(IMAGE_VERSION) debian:trixie ./build.sh $$arch || exit 1; \
	done

os-image: ## build the Ubuntu base image(s) of OS_LIST (default: ubuntu-26.04) for OS_ARCH (default: amd64 and arm64) in a privileged Debian container
	@for os in $(OS_LIST); do for arch in $(OS_ARCH); do \
		$(CONTAINER) run --rm --privileged \
			-v "$(CURDIR)/images/ubuntu:/work" -w /work -e VERSION=$(IMAGE_VERSION) debian:trixie ./build.sh $$arch $$os || exit 1; \
	done; done

rescue-test: ## boot the built rescue image for TEST_ARCH (default: this machine's) in QEMU and check it over SSH
	images/rescue/test-boot.sh $(TEST_ARCH)

upload: ## publish everything built (images, binaries) and the scripts and profiles to R2 (STORE_* from the environment)
	images/upload.sh

images/rescue/out/%/manifest.json:
	$(MAKE) rescue-image RESCUE_ARCH=$*

help: ## list the targets
	@awk -F ':.*## ' '/^[a-z-]+:.*## /{printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
