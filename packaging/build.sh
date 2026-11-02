#!/usr/bin/env bash
# Build the packages of ump, a deb and an rpm per architecture, into
# OUT (make packages; default dist/pkg/) with SHA256SUMS and VERSION:
# ump_0.8.0_amd64.deb, ump-0.8.0-1.x86_64.rpm, and arm64 (aarch64). Each
# format gets ump built with Packaging set to it, so that its update hint
# names the package; nfpm (packaging/nfpm.yaml) is built into a temporary
# directory with go install, so nothing needs installing.
#
# Environment: VERSION (vX.Y.Z, as make has it), ARCHS (default: amd64
# arm64), LDFLAGS (make's), NFPM_VERSION, OUT, FORCE=1 (a -dirty version).
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:?VERSION=vX.Y.Z}"
ARCHS="${ARCHS:-amd64 arm64}"
OUT="${OUT:-dist/pkg}"
NFPM_VERSION="${NFPM_VERSION:?NFPM_VERSION=vX.Y.Z}"
LDFLAGS="${LDFLAGS:--s -w -X github.com/ofzhur/ump/internal/cli.Version=$VERSION}"

# the packages' version: v0.8.0 is 0.8.0; a build after a tag
# (v0.8.0-3-gabc1234) sorts after it, as 0.8.0+3.gabc1234
case "$VERSION" in
dev|v*-dirty) [ "${FORCE:-0}" = 1 ] || { echo "build.sh: ump is $VERSION (no tag, or uncommitted changes); tag a release (git tag v1.2.3), give VERSION=v1.2.3, or FORCE=1" >&2; exit 2; } ;;
esac
case "$VERSION" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "build.sh: $VERSION is no version a package can carry (vX.Y.Z)" >&2; exit 2 ;;
esac
pkgversion="$(sed -E 's/^v//; s/-dirty$/+dirty/; s/-([0-9]+)-g([0-9a-f]+)(\+dirty)?$/+\1.g\2\3/; s/\+([^+]*)\+dirty$/+\1.dirty/' <<<"$VERSION")"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "  nfpm $NFPM_VERSION"
GOBIN="$work" CGO_ENABLED=0 go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$NFPM_VERSION"

# the completions, from ump for this machine, in a home of its own
mkdir -p "$work/completions" "$work/home"
go build -trimpath -o "$work/ump" ./cmd/ump
for shell in bash zsh fish; do
	f=ump.$shell; [ "$shell" = zsh ] && f=_ump
	env -i HOME="$work/home" PATH="$PATH" "$work/ump" completion "$shell" >"$work/completions/$f"
done

rm -rf "$OUT" && mkdir -p "$OUT"
for arch in $ARCHS; do
	for format in deb rpm; do
		bin="$work/ump-$arch-$format"
		GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
			-ldflags "$LDFLAGS -X github.com/ofzhur/ump/internal/cli.Packaging=$format" -o "$bin" ./cmd/ump
		ARCH="$arch" VERSION="$pkgversion" BINARY="$bin" COMPLETIONS="$work/completions" \
			"$work/nfpm" package --config packaging/nfpm.yaml --packager "$format" --target "$OUT/" | sed 's/^/  /'
	done
done
(cd "$OUT" && (command -v sha256sum >/dev/null && sha256sum *.deb *.rpm || shasum -a 256 *.deb *.rpm) > SHA256SUMS)
echo "$VERSION" > "$OUT/VERSION"
echo "  $OUT/SHA256SUMS $OUT/VERSION ($VERSION)"
