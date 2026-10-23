#!/usr/bin/env bash
# Build the UMP rescue image: a Debian system that runs entirely from RAM
# (a squashfs inside a small initramfs, with a writable layer in RAM on
# top), with a generic kernel, the firmware of server NICs and
# storage controllers, and the tools the UMP agent needs. The provider's
# rescue system (or any running Linux) downloads it and kexecs into it;
# see README.md and kexec-into-rescue.sh.
#
# Usage: build.sh [amd64|arm64]
#
# Runs as root on Debian trixie (natively or in a privileged container,
# see the Makefile's rescue-image target). The other architecture is
# built from the same container: mmdebstrap bootstraps it through
# qemu-user-static and kexec is cross-compiled. Environment: SUITE,
# MIRROR, VERSION (default: today's date), OUT (default: ./out/<arch>),
# BUILD_KEXEC=0 to skip the static kexec binary.
set -euo pipefail

ARCH="${1:-$(dpkg --print-architecture)}"
SUITE="${SUITE:-trixie}"
MIRROR="${MIRROR:-http://deb.debian.org/debian}"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
BUILD_KEXEC="${BUILD_KEXEC:-1}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/$ARCH}"
WORK="$(mktemp -d /tmp/ump-rescue.XXXXXX)"
ROOTFS="$WORK/rootfs"
trap 'rm -rf "$WORK"' EXIT

case "$ARCH" in
	amd64|arm64) ;;
	*) echo "build.sh: unsupported architecture $ARCH (amd64 or arm64)" >&2; exit 2 ;;
esac
if [ "$(id -u)" != 0 ]; then
	echo "build.sh: run as root, for example with 'make rescue-image' in a privileged container" >&2
	exit 2
fi

log() { printf '\n==> %s\n' "$*"; }

log "build dependencies"
export DEBIAN_FRONTEND=noninteractive
HOST_ARCH="$(dpkg --print-architecture)"
deps=(mmdebstrap cpio zstd xz-utils squashfs-tools ca-certificates)
if [ "$ARCH" != "$HOST_ARCH" ]; then
	deps+=(qemu-user-static binfmt-support arch-test)
fi
if [ "$BUILD_KEXEC" = 1 ]; then
	deps+=(build-essential xz-utils curl)
	if [ "$ARCH" = "$HOST_ARCH" ]; then
		deps+=(zlib1g-dev liblzma-dev)
	else
		deps+=("crossbuild-essential-$ARCH" "zlib1g-dev:$ARCH" "liblzma-dev:$ARCH")
		dpkg --add-architecture "$ARCH"
	fi
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

packages="$(grep -vE '^[[:space:]]*(#|$)' "$HERE/packages.txt" | sed 's/[[:space:]]//g' | paste -sd, -),linux-image-$ARCH"

log "bootstrapping Debian $SUITE for $ARCH"
mmdebstrap \
	--arch="$ARCH" \
	--variant=minbase \
	--mode=root \
	--components=main,contrib,non-free-firmware \
	--aptopt='Apt::Install-Recommends "false"' \
	--dpkgopt='path-exclude=/usr/share/man/*' \
	--dpkgopt='path-exclude=/usr/share/doc/*' \
	--dpkgopt='path-include=/usr/share/doc/*/copyright' \
	--dpkgopt='path-exclude=/usr/share/info/*' \
	--dpkgopt='path-exclude=/usr/share/lintian/*' \
	--dpkgopt='path-exclude=/usr/share/locale/*' \
	--dpkgopt='path-include=/usr/share/locale/locale.alias' \
	--include="$packages" \
	--customize-hook="sync-in $HERE/overlay /" \
	--customize-hook="copy-in $HERE/finish.sh /tmp" \
	--customize-hook="chroot \"\$1\" env UMP_VERSION=$VERSION UMP_ARCH=$ARCH sh /tmp/finish.sh && rm -f \"\$1/tmp/finish.sh\"" \
	"$SUITE" "$ROOTFS" "$MIRROR"

log "assembling the image"
mkdir -p "$OUT"
vmlinuz="$(ls "$ROOTFS"/boot/vmlinuz-* | head -n 1)"
kver="$(basename "$vmlinuz")"; kver="${kver#vmlinuz-}"
cp "$vmlinuz" "$OUT/vmlinuz"
if [ "$ARCH" = arm64 ]; then
	# Debian ships the arm64 kernel as a raw Image (37 MB); kexec_load,
	# QEMU and firmware loaders all take it gzip-compressed, which is a
	# quarter of the size to download and to hold in RAM.
	gzip -9 -n -f "$OUT/vmlinuz" && mv "$OUT/vmlinuz.gz" "$OUT/vmlinuz"
fi
# build-host traces: emulation caches (Rosetta on an Apple Silicon host)
# and the build container's resolv.conf that resolved's postinst saved
rm -rf "$ROOTFS/root/.cache" "$ROOTFS/etc/.resolv.conf.systemd-resolved.bak"
rm -f "$ROOTFS"/boot/vmlinuz-* "$ROOTFS"/boot/initrd.img-* "$ROOTFS"/boot/System.map-* "$ROOTFS"/boot/config-*
[ -e "$ROOTFS/dev/console" ] || mknod -m 600 "$ROOTFS/dev/console" c 5 1
# read the package list from the host side: a foreign-architecture root
# cannot be chrooted into without /proc (the emulation needs it)
dpkg-query --admindir="$ROOTFS/var/lib/dpkg" -W -f='${Package} ${Version}\n' > "$OUT/packages.txt"

# The system is a squashfs inside a small initramfs: the kernel unpacks
# only the initramfs (busybox, three modules, /init and the squashfs
# file), and /init mounts the squashfs with a writable layer in RAM on
# top and starts systemd there. The system so costs its compressed size
# in memory instead of its unpacked size (see README.md, Memory).
log "squashing the system"
INITRD="$WORK/initrd"
mkdir -p "$INITRD/bin" "$INITRD/dev" "$INITRD/lib/modules"
mksquashfs "$ROOTFS" "$INITRD/rootfs.squashfs" -comp zstd -Xcompression-level 19 -b 1M -noappend -quiet -no-progress
cp "$ROOTFS/usr/bin/busybox" "$INITRD/bin/busybox"
for m in loop squashfs overlay; do
	ko="$(find "$ROOTFS/usr/lib/modules/$kver" -name "$m.ko*" | head -n 1)"
	case "$ko" in
	"") ;; # built into the kernel
	*.xz) xz -dc "$ko" > "$INITRD/lib/modules/$m.ko" ;;
	*.zst) zstd -dcq "$ko" > "$INITRD/lib/modules/$m.ko" ;;
	*.gz) gzip -dc "$ko" > "$INITRD/lib/modules/$m.ko" ;;
	*) cp "$ko" "$INITRD/lib/modules/$m.ko" ;;
	esac
done
install -m 0755 "$HERE/init" "$INITRD/init"
mknod -m 600 "$INITRD/dev/console" c 5 1
# the squashfs is compressed already; zstd at a low level only keeps the
# name honest and costs nothing
(cd "$INITRD" && find . -print0 | cpio --null -o -H newc --quiet) | zstd -T0 -3 -q > "$OUT/initrd.zst"

if [ "$BUILD_KEXEC" = 1 ]; then
	log "building a static kexec"
	if OUT="$OUT" "$HERE/build-kexec.sh" "$ARCH"; then :; else
		echo "build.sh: static kexec could not be built; the image is usable with the provider's kexec-tools" >&2
	fi
fi

log "manifest"
cd "$OUT"
files=(vmlinuz initrd.zst)
[ -f kexec ] && files+=(kexec)
sha256sum "${files[@]}" > SHA256SUMS
{
	printf '{\n'
	printf '  "version": "%s",\n  "arch": "%s",\n  "suite": "%s",\n  "kernel": "%s",\n' "$VERSION" "$ARCH" "$SUITE" "$kver"
	printf '  "built": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf '  "cmdline": "console=tty0 console=ttyS0,115200n8",\n'
	# the smallest MemTotal the image boots with: the kernel holds the
	# compressed initramfs while it unpacks it (twice its size), plus the
	# kernel itself and room for systemd and the tools (see README.md)
	initrd_mb=$(( ($(stat -c %s initrd.zst) + 1048575) / 1048576 ))
	kernel_mb=$(( ($(stat -c %s vmlinuz) + 1048575) / 1048576 ))
	printf '  "memory_mb": %s,\n' "$(( 2 * initrd_mb + 4 * kernel_mb + 160 ))"
	printf '  "files": {\n'
	n=0
	for f in "${files[@]}"; do
		n=$((n + 1))
		sep=","; [ "$n" = "${#files[@]}" ] && sep=""
		printf '    "%s": {"sha256": "%s", "size": %s}%s\n' "$f" "$(sha256sum "$f" | cut -d' ' -f1)" "$(stat -c %s "$f")" "$sep"
	done
	printf '  }\n}\n'
} > manifest.json
ls -la "$OUT"
log "done: $OUT (Debian $SUITE, kernel $kver, version $VERSION)"
