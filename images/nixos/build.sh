#!/usr/bin/env bash
# Write the manifest of a NixOS "image": NixOS is not unpacked from an
# image but installed by nixos-install from its channel, so the manifest
# is all there is. It names the release's channel (nixos-25.11 follows the
# stable release: security and bug fixes, no new major versions) and the
# Nix release that install/install.sh downloads into the rescue system,
# with its checksum, as the official Nix installer names them.
#
# Usage: build.sh [amd64|arm64] [release]   (default: amd64 and arm64, 25.11)
# Writes out/nixos-<release>/<arch>/manifest.json; publish with
# images/upload.sh os. Environment: VERSION (default: today's date).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ARCHES="${1:-amd64 arm64}"
RELEASE="${2:-25.11}"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
CHANNEL="https://channels.nixos.org/nixos-$RELEASE"

curl -fsSIL -o /dev/null "$CHANNEL" || { echo "build.sh: no channel at $CHANNEL" >&2; exit 1; }
installer="$(curl -fsSL https://nixos.org/nix/install)"
nix_version="$(printf '%s\n' "$installer" | sed -n 's#.*/nix-\([0-9][0-9.]*\)-x86_64-linux\.tar\.xz.*#\1#p' | head -n 1)"
[ -n "$nix_version" ] || { echo "build.sh: cannot tell the Nix version from https://nixos.org/nix/install" >&2; exit 1; }

for arch in $ARCHES; do
	case "$arch" in
	amd64) system=x86_64-linux ;;
	arm64) system=aarch64-linux ;;
	*) echo "build.sh: unsupported architecture $arch" >&2; exit 2 ;;
	esac
	# the installer lists hash= and path= per system, the hash first
	sum="$(printf '%s\n' "$installer" | awk -v f="nix-$nix_version-$system.tar.xz" '/hash=/ {h = $0; sub(/.*hash=/, "", h)} index($0, f) && /path=/ {print h; exit}')"
	[ -n "$sum" ] || { echo "build.sh: no checksum for $system in the Nix installer" >&2; exit 1; }
	out="$HERE/out/nixos-$RELEASE/$arch"
	mkdir -p "$out"
	cat > "$out/manifest.json" <<JSON
{
  "version": "$VERSION",
  "os": "nixos-$RELEASE",
  "family": "nixos",
  "arch": "$arch",
  "release": "$RELEASE",
  "state_version": "$RELEASE",
  "channel": "$CHANNEL",
  "kernel": "the channel's",
  "unpacked_mb": 3072,
  "built": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "nix": {
    "version": "$nix_version",
    "url": "https://releases.nixos.org/nix/nix-$nix_version/nix-$nix_version-$system.tar.xz",
    "sha256": "$sum"
  }
}
JSON
	echo "==> $out/manifest.json (NixOS $RELEASE from $CHANNEL, Nix $nix_version)"
done
