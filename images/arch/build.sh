#!/usr/bin/env bash
# Write the manifest of the Arch Linux "image": Arch is a rolling release,
# so it is not unpacked from an image that would be out of date in weeks,
# but installed from the mirrors when the server is installed, as NixOS
# is. The manifest names the monthly bootstrap tarball the installer
# starts from (from archive.archlinux.org, where its URL and checksum
# never change), the mirror the packages come from, and the packages
# (packages.txt, each checked against the Arch package database here).
#
# CachyOS is Arch Linux with CachyOS's repositories in front of Arch's:
# its manifest is the same, plus those repositories (the mirror and the
# keyring package, pinned by its checksum, which the installer adds to
# the bootstrap) and its packages (cachyos-packages.txt, checked against
# CachyOS's repository; its kernel instead of Arch's linux). The
# installer takes the repositories built for the newest of the manifest's
# CPU levels (cachyos.levels) the machine runs, else the plain x86_64
# ones.
#
# Usage: build.sh [YYYY.MM.01]   (default: the newest bootstrap release)
# Writes out/<os>/amd64/manifest.json, and an arm64 manifest that says
# why there is none (both are x86_64 only), for archlinux and cachyos;
# publish with images/upload.sh os. Environment: VERSION (default:
# today's date), MIRROR (default: https://geo.mirror.pkgbuild.com),
# CACHYOS_MIRROR (default: https://mirror.cachyos.org/repo; not their CDN,
# cdn77.cachyos.org, which answers a "+" in a file name, as Arch's pacman
# asks for glibc-2.44+r50..., with 404), OSES (default: "archlinux
# cachyos").
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
MIRROR="${MIRROR:-https://geo.mirror.pkgbuild.com}"
CACHYOS_MIRROR="${CACHYOS_MIRROR:-https://mirror.cachyos.org/repo}"
OSES="${OSES:-archlinux cachyos}"
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

# names lists the package names of packages.txt-style file $1
names() { grep -v -e '^#' -e '^[[:space:]]*$' "$1"; }

packages=()
missing=()
while read -r p; do
	found="$(get "https://archlinux.org/packages/search/json/?name=$p" | jq -r '[.results[] | select(.arch == "x86_64" or .arch == "any")] | length')"
	if [ "${found:-0}" -gt 0 ]; then packages+=("$p"); else missing+=("$p"); fi
done < <(names "$HERE/packages.txt")
[ ${#missing[@]} -eq 0 ] || { echo "build.sh: not in the Arch package database: ${missing[*]}" >&2; exit 1; }

# unsupported OS NAME: the arm64 manifest, which says why there is none
unsupported() {
	local out="$HERE/out/$1/arm64"
	mkdir -p "$out"
	jq -n --arg version "$VERSION" --arg os "$1" --arg why "$2" '{version: $version, os: $os, family: "arch", arch: "arm64", unsupported: $why}' > "$out/manifest.json"
	echo "==> $out/manifest.json (unsupported)"
}

for os in $OSES; do
case "$os" in
archlinux)
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
	unsupported archlinux "Arch Linux is built for x86_64 only (Arch Linux ARM is a separate project); install another OS on this machine"
	;;
cachyos)
	# CachyOS's repository database: the package names, and the keyring
	# package's file name and checksum
	db="$(mktemp -d)"
	{ get -o "$db/cachyos.db" "$CACHYOS_MIRROR/x86_64/cachyos/cachyos.db" && tar -xf "$db/cachyos.db" -C "$db" && rm "$db/cachyos.db"; } \
		|| { echo "build.sh: cannot read $CACHYOS_MIRROR/x86_64/cachyos/cachyos.db" >&2; exit 1; }
	field() { awk -v f="%$2%" 'p {print; exit} $0 == f {p = 1}' "$1/desc"; }
	cpackages=()
	for p in "${packages[@]}"; do [ "$p" = linux ] || cpackages+=("$p"); done
	missing=()
	while read -r p; do
		found=""
		for d in "$db"/*/; do [ "$(field "$d" NAME)" = "$p" ] && found=1 && break; done
		if [ -n "$found" ]; then cpackages+=("$p"); else missing+=("$p"); fi
	done < <(names "$HERE/cachyos-packages.txt")
	[ ${#missing[@]} -eq 0 ] || { echo "build.sh: not in CachyOS's repository: ${missing[*]}" >&2; exit 1; }
	keyring=""
	for d in "$db"/cachyos-keyring-*/; do keyring="$d"; done
	[ -n "$keyring" ] || { echo "build.sh: no cachyos-keyring in CachyOS's repository" >&2; exit 1; }
	kfile="$(field "$keyring" FILENAME)" ksum="$(field "$keyring" SHA256SUM)"
	rm -rf "$db"
	get -I -o /dev/null "$CACHYOS_MIRROR/x86_64/cachyos/$kfile" || { echo "build.sh: no $CACHYOS_MIRROR/x86_64/cachyos/$kfile" >&2; exit 1; }
	for r in x86_64_v3/cachyos-v3 x86_64_v3/cachyos-core-v3 x86_64_v3/cachyos-extra-v3; do
		get -I -o /dev/null "$CACHYOS_MIRROR/$r/${r#*/}.db" || { echo "build.sh: $CACHYOS_MIRROR serves no ${r#*/}" >&2; exit 1; }
	done
	out="$HERE/out/cachyos/amd64"
	mkdir -p "$out"
	# levels: x86-64-v3 alone. v4 (AVX-512) gains little over v3 on most
	# server work, and a virtual machine moved to a host without AVX-512
	# would no longer start its programs.
	jq -n --arg version "$VERSION" --arg release "$release" --arg mirror "$MIRROR" \
		--arg url "$ARCHIVE/$release/$file" --arg sum "$sum" --arg built "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		--arg cmirror "$CACHYOS_MIRROR" --arg kurl "$CACHYOS_MIRROR/x86_64/cachyos/$kfile" --arg ksum "$ksum" \
		--args '{
		version: $version, os: "cachyos", family: "arch", arch: "amd64",
		kernel: "CachyOS'"'"'s (linux-cachyos-server)", unpacked_mb: 3072, built: $built,
		mirror: $mirror,
		bootstrap: {release: $release, url: $url, sha256: $sum},
		cachyos: {mirror: $cmirror, keyring: {url: $kurl, sha256: $ksum}, levels: ["x86-64-v3"]},
		packages: $ARGS.positional}' "${cpackages[@]}" > "$out/manifest.json"
	echo "==> $out/manifest.json (bootstrap $release, ${#cpackages[@]} packages, CachyOS from $CACHYOS_MIRROR)"
	unsupported cachyos "CachyOS is built for x86_64 only; install another OS on this machine"
	;;
*) echo "build.sh: unknown OS $os (archlinux, cachyos)" >&2; exit 2 ;;
esac
done
