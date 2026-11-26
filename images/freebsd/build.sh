#!/usr/bin/env bash
# Build the UMP FreeBSD rescue image and the files of a FreeBSD release
# for the storage. FreeBSD is installed by its own rescue system: the
# release's GENERIC kernel with a root in memory made from base.txz,
# which the UMP Linux rescue system writes to the start of a disk as a
# small boot image, with a boot config beside it; the firmware starts it
# from there (see README.md). FreeBSD itself is not unpacked from an image
# UMP builds: the installer takes the release's distribution sets and
# FreeBSD's packages, both from the storage, pinned by a manifest.
#
# Usage: build.sh amd64|arm64 [RELEASE]   (RELEASE: 15.1, the default, or 14.5)
#
# Runs as root on Debian trixie, natively or in a container without
# privileges (see the Makefile's freebsd-image target): nothing is
# mounted, and nothing of FreeBSD runs, so either architecture is built
# on any host. The sets are unpacked with tar, the file systems made with
# makefs and mkfs.vfat, the partition table with sgdisk. Writes
#
#   out/rescue/freebsd-<release>/<arch>/   disk.img.zst, manifest.json, SHA256SUMS, packages.txt
#   out/os/freebsd-<release>/<arch>/       base.txz, kernel.txz, packages.tar, manifest.json, SHA256SUMS, packages.txt
#
# as the storage has them under rescue/ and os/. The sets and packages a
# build before left there are taken again when their checksums still
# hold. Environment: VERSION (default: today's date), OUT (default:
# ./out), MIRROR (the releases, default
# https://download.freebsd.org/releases), PKG_MIRROR (default
# https://pkg.FreeBSD.org), PKG_BRANCH (default quarterly).
set -euo pipefail

ARCH="${1:-}"
RELEASE="${2:-15.1}"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
MIRROR="${MIRROR:-https://download.freebsd.org/releases}"
PKG_MIRROR="${PKG_MIRROR:-https://pkg.FreeBSD.org}"
PKG_BRANCH="${PKG_BRANCH:-quarterly}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out}"

die() { echo "build.sh: $*" >&2; exit 1; }
log() { printf '\n==> %s\n' "$*"; }

# FreeBSD's names of the architecture: of the release's directory, of the
# package repository, of the loader on the removable path
case "$ARCH" in
amd64) target=amd64/amd64; abi=amd64; efi=BOOTX64.EFI ;;
arm64) target=arm64/aarch64; abi=aarch64; efi=BOOTAA64.EFI ;;
*) echo "build.sh: usage: build.sh amd64|arm64 [RELEASE]" >&2; exit 2 ;;
esac
case "$RELEASE" in
[1-9][0-9].[0-9]|[1-9][0-9].[0-9][0-9]) ;;
*) echo "build.sh: $RELEASE is not a release (15.1, 14.5)" >&2; exit 2 ;;
esac
if [ "$(id -u)" != 0 ]; then
	echo "build.sh: run as root (the owners of the unpacked files), for example with 'make freebsd-image' in a container" >&2
	exit 2
fi
RESCUE_OUT="$OUT/rescue/freebsd-$RELEASE/$ARCH"
OS_OUT="$OUT/os/freebsd-$RELEASE/$ARCH"
# in the container's own file system, not in a bind mount, where the
# host would decide the owners and whether names differ by case
WORK="$(mktemp -d /tmp/ump-freebsd.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
ROOT="$WORK/root"
BOOTFS="$WORK/bootfs"

sha256() { sha256sum "$1" | cut -d' ' -f1; }
bytes() { stat -c %s "$1"; }
mib() { echo $(( ($(bytes "$1") + 1048575) / 1048576 )); }
get() { curl -fsSL --retry 3 --retry-delay 2 "$@"; }

log "build dependencies"
export DEBIAN_FRONTEND=noninteractive
missing=()
for p in makefs mtools dosfstools gdisk xz-utils zstd jq curl ca-certificates openssl; do
	dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p")
done
if [ ${#missing[@]} -gt 0 ]; then
	apt-get update -q
	apt-get install -y -q --no-install-recommends "${missing[@]}"
fi

log "the distribution sets of FreeBSD $RELEASE for $ARCH"
mkdir -p "$RESCUE_OUT" "$OS_OUT" "$ROOT" "$BOOTFS/boot" "$WORK/kernel" "$WORK/packages"
for f in base.txz kernel.txz; do
	path="$target/$RELEASE-RELEASE/$f"
	want="$(awk -v p="$path" '$2 == p {print $1}' "$HERE/SHA256SUMS")"
	[ -n "$want" ] || die "SHA256SUMS has no line for $path; add the release's, from $MIRROR/$target/$RELEASE-RELEASE/MANIFEST"
	if [ -f "$OS_OUT/$f" ] && [ "$(sha256 "$OS_OUT/$f")" = "$want" ]; then
		echo "$f: as a build before left it"
	else
		echo "$f: from $MIRROR/$path"
		get -o "$OS_OUT/$f.part" "$MIRROR/$path"
		have="$(sha256 "$OS_OUT/$f.part")"
		[ "$have" = "$want" ] || { rm -f "$OS_OUT/$f.part"; die "$MIRROR/$path has the SHA-256 $have, and SHA256SUMS says $want"; }
		mv "$OS_OUT/$f.part" "$OS_OUT/$f"
	fi
done

# untar SET DIRECTORY: a set without the exclusions, with the owners it names
grep -vE '^[[:space:]]*(#|$)' "$HERE/rescue-exclude.txt" > "$WORK/exclude"
untar() {
	if ! tar -xJpf "$OS_OUT/$1" -C "$2" --numeric-owner --warning=no-unknown-keyword --anchored --exclude-from="$WORK/exclude" 2> "$WORK/tar.err"; then
		cat "$WORK/tar.err" >&2
		grep -q 'Cannot hard link' "$WORK/tar.err" && die "rescue-exclude.txt excludes a file and keeps a hard link to it; exclude the link too"
		die "cannot unpack $1"
	fi
}

log "the memory root: base.txz without what a rescue system in memory has no use for"
untar base.txz "$ROOT"
# base.txz's /boot is the loader's: the boot partition gets what the boot
# needs of it, and the memory root keeps the directories
mv "$ROOT/boot" "$WORK/boot"
mkdir -p "$ROOT/boot/kernel" "$ROOT/boot/modules" "$ROOT/boot/firmware" "$ROOT/boot/zfs" "$ROOT/boot/efi"
du -sm "$ROOT" | awk '{print "base, trimmed: " $1 " MB"}'

log "kernel.txz: the kernel for the boot partition, the modules for the memory root"
untar kernel.txz "$WORK/kernel"
mkdir -p "$BOOTFS/boot/kernel"
mv "$WORK/kernel/boot/kernel/kernel" "$BOOTFS/boot/kernel/kernel"
# linker.hints is the release's: it names the modules left out too, and
# devmatch gets a refusal when it asks for one of them
mv "$WORK/kernel/boot/kernel"/* "$ROOT/boot/kernel/"
du -sm "$ROOT/boot/kernel" | awk '{print "modules: " $1 " MB"}'

repo="$PKG_MIRROR/FreeBSD:${RELEASE%%.*}:$abi/$PKG_BRANCH"
log "the package catalogue of $repo"
cat="$WORK/catalogue"
mkdir "$cat"
get -o "$WORK/packagesite.pkg" "$repo/packagesite.pkg"
tar --zstd -xf "$WORK/packagesite.pkg" -C "$cat"
# pkg's signature by fingerprints: the catalogue brings the public key,
# whose SHA-256 the release trusts (base.txz has the fingerprints), and
# the signature, RSA over the SHA-256 of the catalogue's SHA-256 in hex
[ -s "$cat/packagesite.yaml.sig" ] && [ -s "$cat/packagesite.yaml.pub" ] || die "the catalogue of $repo is not signed"
fingerprint="$(sha256 "$cat/packagesite.yaml.pub")"
grep -rqsF "\"$fingerprint\"" "$ROOT/usr/share/keys/pkg/trusted" \
	|| die "the catalogue of $repo is signed by a key FreeBSD $RELEASE does not trust (fingerprint $fingerprint, not in /usr/share/keys/pkg/trusted of base.txz)"
! grep -rqsF "\"$fingerprint\"" "$ROOT/usr/share/keys/pkg/revoked" \
	|| die "the catalogue of $repo is signed by a key FreeBSD $RELEASE has revoked (fingerprint $fingerprint)"
printf '%s' "$(sha256 "$cat/packagesite.yaml")" \
	| openssl dgst -sha256 -verify "$cat/packagesite.yaml.pub" -signature "$cat/packagesite.yaml.sig" >/dev/null \
	|| die "the signature of the catalogue of $repo does not verify"
echo "signed by the key $fingerprint, which FreeBSD $RELEASE trusts"
# one line per package: what the steps below need of it
jq -c '{name, version, path: (.repopath // .path), sum, size: .pkgsize, flatsize, deps: ((.deps // {}) | keys)}' "$cat/packagesite.yaml" > "$WORK/index"

# closure NAME...: the named packages and what they depend on, as an
# array of the catalogue's entries with wanted (it was named)
closure() {
	jq -c -s '
		(map({key: .name, value: .}) | from_entries) as $all
		| $ARGS.positional as $wanted
		| {seen: [], todo: $wanted}
		| until(.todo == [];
			.todo[0] as $n
			| if .seen | index($n) then .todo |= .[1:]
			  else ($all[$n] // error("\($n) is not in the catalogue")) as $p
				| .seen += [$n] | .todo = .todo[1:] + $p.deps
			  end)
		| .seen | sort
		| map(. as $n | $all[$n] + {wanted: ($wanted | index($n) != null), file: "\($n)-\($all[$n].version).pkg"})
	' --args "$@" < "$WORK/index"
}

# pkg_sum FILE: the file's checksum as the catalogue has it: "2$" and
# its BLAKE2b-512 in pkg's base32 (z-base-32 with the low bits first,
# pkg_checksum_encode_base32 of libpkg)
pkg_sum() {
	local hex b32=ybndrfg8ejkmcpqxot1uwisza345h769 o="" i x remain=-1 byte
	hex="$(b2sum "$1" | cut -d' ' -f1)"
	for ((i = 0; i < ${#hex} / 2; i++)); do
		byte=$((16#${hex:$((2 * i)):2}))
		case $((i % 5)) in
		0) x=$byte; remain=$((byte >> 5)); o+="${b32:$((x & 31)):1}" ;;
		1) x=$((remain | byte << 3)); o+="${b32:$((x & 31)):1}${b32:$((x >> 5 & 31)):1}"; remain=$((x >> 10)) ;;
		2) x=$((remain | byte << 1)); o+="${b32:$((x & 31)):1}"; remain=$((x >> 5)) ;;
		3) x=$((remain | byte << 4)); o+="${b32:$((x & 31)):1}${b32:$((x >> 5 & 31)):1}"; remain=$((x >> 10 & 3)) ;;
		4) x=$((remain | byte << 2)); o+="${b32:$((x & 31)):1}${b32:$((x >> 5 & 31)):1}"; remain=-1 ;;
		esac
	done
	[ "$remain" -lt 0 ] || o+="${b32:$remain:1}"
	printf '2$%s' "$o"
}

# fetch_packages LIST: every package of a closure into $WORK/packages as
# NAME-VERSION.pkg (the name pkg add looks for beside a package that
# depends on it), checked against the catalogue's checksum
fetch_packages() {
	local file path sum
	while IFS=$'\t' read -r file path sum; do
		case "$sum" in
		'2$'*) ;;
		*) die "the catalogue's checksum of $file is of a kind this script does not compute ($sum)" ;;
		esac
		if [ -f "$WORK/packages/$file" ] && [ "$(pkg_sum "$WORK/packages/$file")" = "$sum" ]; then
			echo "$file: as a build before left it"
			continue
		fi
		echo "$file: from $repo/$path"
		get -o "$WORK/packages/$file" "$repo/$path"
		[ "$(pkg_sum "$WORK/packages/$file")" = "$sum" ] || die "$repo/$path is not what the catalogue says (its checksum $sum, the file's $(pkg_sum "$WORK/packages/$file"))"
	done < <(jq -r '.[] | [.file, .path, .sum] | @tsv' <<< "$1")
}

log "the packages of the installed system, with what they depend on"
mapfile -t wanted < <(grep -vE '^[[:space:]]*(#|$)' "$HERE/packages.txt" | sed 's/[[:space:]]//g')
os_packages="$(closure "${wanted[@]}")"
[ ! -f "$OS_OUT/packages.tar" ] || tar -xf "$OS_OUT/packages.tar" -C "$WORK/packages" 2>/dev/null || true
fetch_packages "$os_packages"

log "pkg-static and jq for the memory root"
# the installer adds the packages with pkg-static and reads its
# specification, JSON, with jq, which the base lacks
rescue_packages="$(closure pkg jq)"
fetch_packages "$rescue_packages"
# from_package NAME MEMBER...: files of a package, a tar of its files
# under their absolute paths after its manifests, into the memory root
from_package() {
	local file
	file="$(jq -r --arg n "$1" '.[] | select(.name == $n) | .file' <<< "$rescue_packages")"
	shift
	tar --zstd -xpf "$WORK/packages/$file" -C "$ROOT" --no-same-owner --warning=no-unknown-keyword --wildcards "$@" 2> "$WORK/tar.err" \
		|| { cat "$WORK/tar.err" >&2; die "cannot take $* from $file"; }
}
from_package pkg /usr/local/sbin/pkg-static
from_package jq /usr/local/bin/jq '/usr/local/lib/libjq.so*'
from_package oniguruma '/usr/local/lib/libonig.so*'
ls -l "$ROOT/usr/local/sbin/pkg-static" "$ROOT/usr/local/bin/jq" "$ROOT"/usr/local/lib/lib*.so.[0-9]

log "configuring the rescue system"
echo "$VERSION" > "$ROOT/etc/ump-rescue-version"
# the overlay, whoever owns it here: root's there, the scripts executable
(cd "$HERE/overlay" && find . -type f) | while read -r f; do
	mode=0644; [ ! -x "$HERE/overlay/$f" ] || mode=0555
	install -D -o 0 -g 0 -m "$mode" "$HERE/overlay/$f" "$ROOT/$f"
done

log "the memory root: UFS1"
# UFS1, because FreeBSD's loader refuses the UFS2 of this makefs (its
# superblock is at 8 KiB); 32 MiB free for what the running system writes
makefs -t ffs -B le -o version=1,bsize=32768,fsize=4096,optimization=space,minfree=0 -b 32m -f 4000 "$WORK/mfsroot" "$ROOT" > "$WORK/makefs.log" \
	|| { cat "$WORK/makefs.log" >&2; die "makefs could not make the memory root"; }
mfsroot_mb="$(mib "$WORK/mfsroot")"
# the loader unpacks it while it loads
gzip -9 -n -c "$WORK/mfsroot" > "$BOOTFS/boot/mfsroot.gz"
echo "$mfsroot_mb MB, $(mib "$BOOTFS/boot/mfsroot.gz") MB as mfsroot.gz"

log "the boot partition: the loader's files, the kernel, the memory root"
mkdir -p "$BOOTFS/boot/defaults" "$BOOTFS/boot/lua" "$BOOTFS/boot/loader.conf.d"
cp "$WORK/boot/defaults/loader.conf" "$BOOTFS/boot/defaults/loader.conf"
cp "$WORK/boot"/lua/*.lua "$BOOTFS/boot/lua/"
cp "$HERE/loader.conf" "$BOOTFS/boot/loader.conf"
if [ "$ARCH" = amd64 ]; then
	# the loader of a BIOS boot, which gptboot starts, and the kernel's
	# hints; each loader takes the consoles it has (vidconsole with BIOS,
	# efi with UEFI) and says that it lacks the other
	cp "$WORK/boot/loader" "$WORK/boot/device.hints" "$BOOTFS/boot/"
	printf 'console="comconsole,vidconsole,efi"\ncomconsole_speed="115200"\n' >> "$BOOTFS/boot/loader.conf"
else
	printf 'console="efi"\n' >> "$BOOTFS/boot/loader.conf"
fi
# (4 MiB free: without any, makefs runs out of room for the large files' indirect blocks)
makefs -t ffs -B le -o version=1,bsize=32768,fsize=4096,optimization=space,minfree=0 -b 4m "$WORK/bootfs.img" "$BOOTFS" > "$WORK/makefs.log" \
	|| { cat "$WORK/makefs.log" >&2; die "makefs could not make the boot partition"; }
bootfs_mb="$(mib "$WORK/bootfs.img")"

# The disk image: 1 MiB, the EFI system partition, on amd64 the BIOS boot
# code, the boot partition, and 1 MiB for the backup table.
esp_mb=64
disk_mb=$(( 1 + esp_mb + 1 + bootfs_mb + 1 ))
IMG="$WORK/disk.img"
truncate -s "${disk_mb}M" "$IMG"
sg=(-a 2048 -n "1:2048:+${esp_mb}M" -t 1:ef00 -c 1:umpboot)
[ "$ARCH" != amd64 ] || sg+=(-a 8 -n 2:0:+512K -t 2:a501 -c 2:umpbootcode -a 2048)
sg+=(-n "3:0:+${bootfs_mb}M" -t 3:a503 -c 3:umpbootfs)
sgdisk "${sg[@]}" "$IMG" >/dev/null
start() { sgdisk -i "$1" "$IMG" | awk '/^First sector:/ {print $3}'; }

log "the EFI system partition: FAT32, the loader on the removable path, the boot config's directory"
truncate -s "${esp_mb}M" "$WORK/esp.img"
mkfs.vfat -F 32 -n UMPBOOT -h "$(start 1)" --invariant "$WORK/esp.img" >/dev/null
mmd -i "$WORK/esp.img" ::/EFI ::/EFI/BOOT ::/ump
mcopy -i "$WORK/esp.img" "$WORK/boot/loader.efi" "::/EFI/BOOT/$efi"

log "the disk image"
dd if="$WORK/esp.img" of="$IMG" bs=512 seek="$(start 1)" conv=notrunc status=none
dd if="$WORK/bootfs.img" of="$IMG" bs=512 seek="$(start 3)" conv=notrunc status=none
if [ "$ARCH" = amd64 ]; then
	# a BIOS boot: pmbr, the boot code of the protective MBR (its first 440
	# bytes; the partition entry is sgdisk's), loads gptboot from the
	# freebsd-boot partition, which starts /boot/loader of the boot partition
	dd if="$WORK/boot/pmbr" of="$IMG" bs=1 count=440 conv=notrunc status=none
	dd if="$WORK/boot/gptboot" of="$IMG" bs=512 seek="$(start 2)" conv=notrunc status=none
fi
sgdisk -p "$IMG" | sed -n '/^Number/,$p'
zstd -T0 -19 -q -f "$IMG" -o "$RESCUE_OUT/disk.img.zst"

log "the manifests"
# file_entries DIRECTORY FILE...: the files object of a manifest
file_entries() {
	local d="$1" f; shift
	for f in "$@"; do jq -n --arg f "$f" --arg s "$(sha256 "$d/$f")" --argjson n "$(bytes "$d/$f")" '{($f): {sha256: $s, size: $n}}'; done | jq -s add
}
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

cd "$RESCUE_OUT"
jq -r '.[] | "\(.name) \(.version)"' <<< "$rescue_packages" > packages.txt
sha256sum disk.img.zst > SHA256SUMS
# memory_mb, the smallest machine the image is for: the memory root as
# the kernel holds it, twice the kernel, and 288 MB for the system, an
# installer and ZFS's cache (see README.md, Memory)
kernel_mb="$(mib "$BOOTFS/boot/kernel/kernel")"
jq -n --arg version "$VERSION" --arg release "$RELEASE" --arg arch "$ARCH" --arg built "$built" \
	--argjson memory "$(( mfsroot_mb + 2 * kernel_mb + 288 ))" --argjson disk "$disk_mb" \
	--argjson files "$(file_entries . disk.img.zst)" '{
	version: $version, system: "freebsd", release: $release, arch: $arch,
	kernel: ($release + "-RELEASE GENERIC"), built: $built,
	features: ["boot-config"],
	memory_mb: $memory, disk_mb: $disk,
	files: $files}' > manifest.json

cd "$OS_OUT"
# the packages as they are (they are compressed), under stable names
tar -cf packages.tar -C "$WORK/packages" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
	$(jq -r '.[].file' <<< "$os_packages")
jq -r '.[] | "\(.name) \(.version)"' <<< "$os_packages" > packages.txt
sha256sum base.txz kernel.txz packages.tar > SHA256SUMS
# the size of the installed system: the sets unpacked and the packages added
unpacked=$(( $(xz -l --robot base.txz | awk '$1 == "totals" {print $5}') + $(xz -l --robot kernel.txz | awk '$1 == "totals" {print $5}') + $(jq '[.[].flatsize] | add' <<< "$os_packages") ))
packages="$(jq -c '.[]' <<< "$os_packages" | while read -r p; do
	jq -c --arg s "$(sha256 "$WORK/packages/$(jq -r .file <<< "$p")")" '{name, version, file, sha256: $s, size, wanted}' <<< "$p"
done | jq -s .)"
jq -n --arg version "$VERSION" --arg os "freebsd-$RELEASE" --arg arch "$ARCH" --arg release "$RELEASE-RELEASE" --arg built "$built" \
	--argjson unpacked "$(( (unpacked + 1048575) / 1048576 ))" --argjson packages "$packages" \
	--argjson files "$(file_entries . base.txz kernel.txz packages.tar)" '{
	version: $version, os: $os, family: "freebsd", arch: $arch, release: $release,
	kernel: ($release + " GENERIC"), rescue: $os,
	unpacked_mb: $unpacked, built: $built,
	files: $files, packages: $packages}' > manifest.json

ls -l "$RESCUE_OUT" "$OS_OUT"
log "done: FreeBSD $RELEASE for $ARCH, version $VERSION; the rescue image needs $(jq .memory_mb "$RESCUE_OUT/manifest.json") MB of memory"
