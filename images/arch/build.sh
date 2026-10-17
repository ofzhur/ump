#!/usr/bin/env bash
# Write the manifest of the Arch Linux "image": Arch is a rolling release,
# so it is not unpacked from an image that would be out of date in weeks,
# but installed from the mirrors when the server is installed, as NixOS
# is. The manifest names the monthly bootstrap tarball the installer
# starts from (from archive.archlinux.org, where its URL and checksum
# never change), the mirror the packages come from, and the packages
# (packages.txt, each checked against the Arch package database here).
#
# Usage: build.sh [YYYY.MM.01]   (default: the newest bootstrap release)
# Writes out/archlinux/amd64/manifest.json, and an arm64 manifest that
# says why there is none (Arch Linux is x86_64 only); publish with
# images/upload.sh os. Environment: VERSION (default: today's date),
# MIRROR (default: https://geo.mirror.pkgbuild.com).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
MIRROR="${MIRROR:-https://geo.mirror.pkgbuild.com}"
ARCHIVE=https://archive.archlinux.org/iso
# archlinux.org resets a connection now and then, more so under many queries
get() { curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 "$@"; }

release="${1:-$(get "$ARCHIVE/" | grep -o 'href="20[0-9][0-9]\.[0-9][0-9]\.01/"' | tail -n 1 | sed 's/href="//; s#/"##')}"
[ -n "$release" ] || { echo "build.sh: cannot tell the newest release from $ARCHIVE" >&2; exit 1; }
file="archlinux-bootstrap-$release-x86_64.tar.zst"
sum="$(get "$ARCHIVE/$release/sha256sums.txt" | awk -v f="$file" '$2 == f {print $1}')"
[ -n "$sum" ] || { echo "build.sh: no checksum for $file in $ARCHIVE/$release/sha256sums.txt" >&2; exit 1; }
get -I -o /dev/null "$ARCHIVE/$release/$file" || { echo "build.sh: no $ARCHIVE/$release/$file" >&2; exit 1; }
get -I -o /dev/null "$MIRROR/core/os/x86_64/core.db" || { echo "build.sh: $MIRROR serves no core repository" >&2; exit 1; }

packages=()
missing=()
while read -r p; do
	case "$p" in ''|'#'*) continue ;; esac
	found="$(get "https://archlinux.org/packages/search/json/?name=$p" | jq -r '[.results[] | select(.arch == "x86_64" or .arch == "any")] | length')"
	if [ "${found:-0}" -gt 0 ]; then packages+=("$p"); else missing+=("$p"); fi
done < "$HERE/packages.txt"
[ ${#missing[@]} -eq 0 ] || { echo "build.sh: not in the Arch package database: ${missing[*]}" >&2; exit 1; }

out="$HERE/out/archlinux/amd64"
mkdir -p "$out"
jq -n --arg version "$VERSION" --arg release "$release" --arg mirror "$MIRROR" \
	--arg url "$ARCHIVE/$release/$file" --arg sum "$sum" --arg built "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	--args '{
	version: $version, os: "archlinux", family: "arch", arch: "amd64",
	kernel: "the mirror'"'"'s (linux)", unpacked_mb: 3072, built: $built,
	mirror: $mirror,
	bootstrap: {release: $release, url: $url, sha256: $sum},
	packages: $ARGS.positional}' "${packages[@]}" > "$out/manifest.json"
echo "==> $out/manifest.json (bootstrap $release, ${#packages[@]} packages from $MIRROR)"

out="$HERE/out/archlinux/arm64"
mkdir -p "$out"
jq -n --arg version "$VERSION" '{version: $version, os: "archlinux", family: "arch", arch: "arm64",
	unsupported: "Arch Linux is built for x86_64 only (Arch Linux ARM is a separate project); install another OS on this machine"}' > "$out/manifest.json"
echo "==> $out/manifest.json (unsupported)"
