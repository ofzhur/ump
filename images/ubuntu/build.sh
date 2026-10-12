#!/usr/bin/env bash
# Build the UMP Ubuntu base image: a complete Ubuntu LTS server system as a
# root file system archive, with the generic kernel, its firmware, LVM,
# netplan, SSH and the tools a server needs, and the UMP defaults from
# overlay/ and finish.sh. install.sh unpacks it onto a machine's disks and
# does what depends on the machine; see README.md.
#
# Usage: build.sh [amd64|arm64]
#
# Runs as root on Debian trixie (natively or in a privileged container,
# see the Makefile's os-image target). The other architecture is built
# from the same container through qemu-user-static. Environment: SUITE
# (default: resolute, Ubuntu 26.04), OS (the name under which the image is
# published, default: ubuntu-26.04), MIRROR, VERSION (default: today's
# date), OUT (default: ./out/<arch>).
set -euo pipefail

ARCH="${1:-$(dpkg --print-architecture)}"
SUITE="${SUITE:-resolute}"
OS="${OS:-ubuntu-26.04}"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/$ARCH}"
WORK="$(mktemp -d /tmp/ump-ubuntu.XXXXXX)"
ROOTFS="$WORK/rootfs"
trap 'rm -rf "$WORK"' EXIT

case "$ARCH" in
	amd64) MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"; boot="grub-efi-amd64-bin,grub-pc-bin" ;;
	arm64) MIRROR="${MIRROR:-http://ports.ubuntu.com/ubuntu-ports}"; boot="grub-efi-arm64-bin" ;;
	*) echo "build.sh: unsupported architecture $ARCH (amd64 or arm64)" >&2; exit 2 ;;
esac
if [ "$(id -u)" != 0 ]; then
	echo "build.sh: run as root, for example with 'make os-image' in a privileged container" >&2
	exit 2
fi

log() { printf '\n==> %s\n' "$*"; }

log "build dependencies"
export DEBIAN_FRONTEND=noninteractive
HOST_ARCH="$(dpkg --print-architecture)"
deps=(mmdebstrap zstd ca-certificates ubuntu-keyring)
if [ "$ARCH" != "$HOST_ARCH" ]; then
	deps+=(qemu-user-static binfmt-support arch-test)
fi
missing=()
for p in "${deps[@]}"; do dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p"); done
if [ ${#missing[@]} -gt 0 ]; then
	apt-get update -q
	apt-get install -y -q --no-install-recommends "${missing[@]}"
fi
if [ "$ARCH" != "$HOST_ARCH" ] && ! arch-test "$ARCH" >/dev/null 2>&1; then
	echo "build.sh: this system cannot run $ARCH binaries (binfmt_misc with qemu-user-static is needed)" >&2
	exit 1
fi

packages="$(grep -vE '^[[:space:]]*(#|$)' "$HERE/packages.txt" | sed 's/[[:space:]]//g' | paste -sd, -),linux-image-generic,$boot"

log "bootstrapping Ubuntu $SUITE for $ARCH from $MIRROR"
mmdebstrap \
	--arch="$ARCH" \
	--variant=minbase \
	--mode=root \
	--keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg \
	--components=main,universe \
	--aptopt='Apt::Install-Recommends "false"' \
	--dpkgopt='path-exclude=/usr/share/man/*' \
	--dpkgopt='path-exclude=/usr/share/doc/*' \
	--dpkgopt='path-include=/usr/share/doc/*/copyright' \
	--dpkgopt='path-exclude=/usr/share/info/*' \
	--dpkgopt='path-exclude=/usr/share/lintian/*' \
	--include="$packages" \
	--customize-hook="sync-in $HERE/overlay /" \
	--customize-hook="copy-in $HERE/finish.sh /tmp" \
	--customize-hook="chroot \"\$1\" env UMP_VERSION=$VERSION UMP_ARCH=$ARCH UMP_SUITE=$SUITE sh /tmp/finish.sh && rm -f \"\$1/tmp/finish.sh\"" \
	"$SUITE" "$ROOTFS" "$MIRROR"

log "assembling the image"
mkdir -p "$OUT"
kver="$(ls "$ROOTFS"/boot/vmlinuz-* | sed 's|.*/vmlinuz-||' | sort -V | tail -n 1)"
# read the package list from the host side: a foreign-architecture root
# cannot be chrooted into without /proc (the emulation needs it)
dpkg-query --admindir="$ROOTFS/var/lib/dpkg" -W -f='${Package} ${Version}\n' > "$OUT/packages.txt"
# capabilities (ping, ...) and ACLs travel as extended attributes
tar --numeric-owner --xattrs --xattrs-include='*' --acls -C "$ROOTFS" -cf - . | zstd -T0 -15 -q > "$OUT/rootfs.tar.zst"
size="$(du -sm "$ROOTFS" | cut -f1)"

log "manifest"
cd "$OUT"
sha256sum rootfs.tar.zst > SHA256SUMS
{
	printf '{\n'
	printf '  "version": "%s",\n  "os": "%s",\n  "suite": "%s",\n  "arch": "%s",\n  "kernel": "%s",\n' "$VERSION" "$OS" "$SUITE" "$ARCH" "$kver"
	printf '  "built": "%s",\n  "unpacked_mb": %s,\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$size"
	printf '  "files": {\n'
	printf '    "rootfs.tar.zst": {"sha256": "%s", "size": %s}\n' "$(sha256sum rootfs.tar.zst | cut -d' ' -f1)" "$(stat -c %s rootfs.tar.zst)"
	printf '  }\n}\n'
} > manifest.json
ls -la "$OUT"
log "done: $OUT ($OS, $SUITE, kernel $kver, version $VERSION, $size MB unpacked)"
