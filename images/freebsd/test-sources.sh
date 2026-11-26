#!/usr/bin/env bash
# Check that what a build left of FreeBSD goes out with its source: the
# two directories of a release and an architecture, as build.sh wrote
# them and upload.sh publishes them.
#
#   os/       src.txz is the release's, by the checksum SHA256SUMS pins,
#             and the manifest names it. Every package of packages.tar
#             is asked for its licenses (its own manifest) and
#             licenses.txt decides it; one it does not decide fails. A
#             package that goes out with its source has NAME-VERSION/ in
#             packages-source.tar: its port (ports/ORIGIN/Makefile), the
#             ports tree's COPYRIGHT, a distinfo, and under distfiles/
#             every file a distinfo there names, with that size and
#             SHA-256, and no other file; the
#             distribution files the package itself names (catalog.mk,
#             beside its license) are among them. The manifest says of
#             every package what was found, and has the size and the
#             SHA-256 of packages-source.tar.
#   rescue/   the manifest names the release's source, with the pinned
#             checksum and its place in the storage, and the packages
#             whose files are in the image with their licenses, which
#             licenses.txt decides; one that goes out with its source
#             has it in packages-source.tar beside the image, checked
#             as above.
#
# build.sh ends with this check. It runs nothing of FreeBSD and needs no
# machine: bash, jq, tar, zstd, and sha256sum or shasum.
#
# Usage: test-sources.sh [amd64|arm64] [RELEASE]   (default: this machine's architecture, 15.1)
# Environment: OUT (default: ./out, as build.sh has it).
set -euo pipefail
export LC_ALL=C
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
RELEASE="${2:-15.1}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out}"
OS="$OUT/os/freebsd-$RELEASE/$ARCH"
RESCUE="$OUT/rescue/freebsd-$RELEASE/$ARCH"
case "$ARCH" in
amd64) target=amd64/amd64 ;;
arm64) target=arm64/aarch64 ;;
*) echo "test-sources.sh: usage: test-sources.sh [amd64|arm64] [RELEASE]" >&2; exit 2 ;;
esac
for c in jq tar zstd; do
	command -v "$c" >/dev/null || { echo "test-sources.sh: $c is needed" >&2; exit 2; }
done
if command -v sha256sum >/dev/null; then sum() { sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null; then sum() { shasum -a 256 | cut -d' ' -f1; }
else echo "test-sources.sh: sha256sum or shasum is needed" >&2; exit 2; fi
[ -f "$OS/manifest.json" ] && [ -f "$RESCUE/manifest.json" ] || { echo "no image in $OS and $RESCUE; run build.sh $ARCH $RELEASE first" >&2; exit 2; }
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-freebsd-sources.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAILED: $*"; exit 1; }

# what build.sh decides with, and what it reads a distinfo with (their
# die speaks on stderr: they run where the output is taken)
die() { echo "FAILED: $*" >&2; exit 1; }
section="$(awk '/^# --- the sources: /{on = 1; next} on && /^# --- /{exit} on' "$HERE/build.sh")"
[ -n "$section" ] || { echo "test-sources.sh: no sources section in build.sh" >&2; exit 2; }
eval "$section"
LICENSES="$(license_list "$HERE/licenses.txt")" || fail "licenses.txt is not what build.sh reads"

size() { wc -c < "$1" | tr -d ' '; }
mb() { awk -v n="$1" 'BEGIN {printf "%.1f", n / 1048576}'; }
# in_manifest DIRECTORY FILE: the manifest has the file's size and SHA-256
in_manifest() {
	[ "$(jq -r --arg f "$2" '.files[$f] | "\(.sha256) \(.size)"' "$1/manifest.json")" = "$(sum < "$1/$2") $(size "$1/$2")" ] \
		|| fail "$1/manifest.json does not have the size and the SHA-256 of $2 as it is there"
}

# source_of TAR NAME-VERSION ORIGIN [DISTFILE...]: the archive has the
# package's port and every file its distinfo names, as the distinfo has
# it, and nothing else among them; the named distribution files, those
# the package says it was built from, are among them
source_of() {
	local tar="$1" nv="$2" origin="$3" members distinfos d files path want bytes named="" n=0 total=0 f
	shift 3
	members="$(tar -tf "$tar")"
	grep -qxF "$nv/ports/$origin/Makefile" <<< "$members" || fail "$tar has no $nv/ports/$origin/Makefile: the port $nv was built from is not there"
	grep -qxF "$nv/ports/COPYRIGHT" <<< "$members" || fail "$tar has no $nv/ports/COPYRIGHT, the notice of the ports tree the port is taken from"
	distinfos="$(awk -v p="$nv/ports/" 'index($0, p) == 1 && split(substr($0, length(p) + 1), a, "/") == 3 && a[3] == "distinfo"' <<< "$members")"
	[ -n "$distinfos" ] || fail "$tar has no distinfo under $nv/ports: nothing says which files the source of $nv is"
	while IFS= read -r d; do
		files="$(tar -xOf "$tar" "$d" | distinfo_files)" || fail "$d of $tar is not a distinfo"
		while IFS=$'\t' read -r path want bytes; do
			[ -n "$path" ] || continue
			grep -qxF "$nv/distfiles/$path" <<< "$members" || fail "$tar lacks $nv/distfiles/$path, which $d names"
			tar -xOf "$tar" "$nv/distfiles/$path" > "$WORK/distfile" || fail "$nv/distfiles/$path cannot be read from $tar"
			[ "$(size "$WORK/distfile")" = "$bytes" ] || fail "$nv/distfiles/$path of $tar has $(size "$WORK/distfile") bytes, and $d says $bytes"
			[ "$(sum < "$WORK/distfile")" = "$want" ] || fail "$nv/distfiles/$path of $tar has the SHA-256 $(sum < "$WORK/distfile"), and $d says $want"
			named+="$nv/distfiles/$path"$'\n'
			n=$((n + 1)); total=$((total + bytes))
		done <<< "$files"
	done <<< "$distinfos"
	[ "$n" -gt 0 ] || fail "the distinfo of $nv in $tar names no file"
	while IFS= read -r f; do
		grep -qxF "$f" <<< "$named" || fail "$tar has $f, which no distinfo of $nv names"
	done < <(awk -v p="$nv/distfiles/" 'index($0, p) == 1 && $0 !~ /\/$/' <<< "$members")
	for f in "$@"; do
		awk -F/ '{print $NF}' <<< "$named" | grep -qxF "$f" || fail "$nv was built from $f (its catalog.mk), which no distinfo of $nv in $tar names"
	done
	echo "$nv: ports/$origin, and under distfiles/ what its distinfo names: $n $([ "$n" = 1 ] && echo file || echo files), $(mb "$total") MB${1:+, with $* that the package itself names}"
}

# sources DIRECTORY PACKAGES: packages-source.tar of a directory for the
# decided packages (a JSON array), which the manifest must describe as
# they were decided; PACKAGES_TAR, when set, has the packages themselves
sources() {
	local dir="$1" packages="$2" tar="$1/packages-source.tar" nv origin distfiles
	[ "$(jq -c '[.packages[] | {name, version, origin, licenses, source}]' "$dir/manifest.json")" = "$(jq -c 'map({name, version, origin, licenses: (.licenses // []), source})' <<< "$packages")" ] \
		|| fail "$dir/manifest.json does not say of its packages what they and licenses.txt say: origin, licenses, and whether the source goes out with them"
	if [ "$(jq '[.[] | select(.source)] | length' <<< "$packages")" = 0 ]; then
		[ ! -e "$tar" ] && [ "$(jq '.files | has("packages-source.tar")' "$dir/manifest.json")" = false ] || fail "$dir has a packages-source.tar, or its manifest names one, and no package there goes out with its source"
		echo "no package there goes out with its source: no packages-source.tar"
		return 0
	fi
	[ -f "$tar" ] || fail "$tar is not there: it must have the source of $(jq -r '[.[] | select(.source) | .name] | join(", ")' <<< "$packages")"
	in_manifest "$dir" packages-source.tar
	grep -qxF README <<< "$(tar -tf "$tar")" || fail "$tar has no README"
	while IFS=$'\t' read -r nv origin; do
		distfiles=""
		if [ -n "${PACKAGES_TAR:-}" ]; then
			distfiles="$(tar -xOf "$PACKAGES_TAR" "$nv.pkg" | zstd -dc | tar -xOf - "/usr/local/share/licenses/$nv/catalog.mk" 2>/dev/null | sed -n 's/^_LICENSE_DISTFILES[^=]*=//p' | tr -s ' \t' '\n\n' | sort -u | paste -sd' ' -)" || true
		fi
		# (the names it has, each an argument)
		source_of "$tar" "$nv" "$origin" $distfiles
	done < <(jq -r '.[] | select(.source) | ["\(.name)-\(.version)", .origin] | @tsv' <<< "$packages")
	echo "packages-source.tar: $(mb "$(size "$tar")") MB, as the manifest says"
}

want="$(awk -v p="$target/$RELEASE-RELEASE/src.txz" '$2 == p {print $1}' "$HERE/SHA256SUMS")"
[ -n "$want" ] || fail "SHA256SUMS has no line for $target/$RELEASE-RELEASE/src.txz"

echo "==> $OS"
[ -f "$OS/src.txz" ] || fail "$OS has no src.txz, the source of base.txz and kernel.txz"
[ "$(sum < "$OS/src.txz")" = "$want" ] || fail "$OS/src.txz has the SHA-256 $(sum < "$OS/src.txz"), and SHA256SUMS says $want"
in_manifest "$OS" src.txz
echo "src.txz: $(mb "$(size "$OS/src.txz")") MB, the release's by the checksum of SHA256SUMS, as the manifest says"
[ -f "$OS/packages.tar" ] || fail "$OS has no packages.tar"
packages="$(tar -tf "$OS/packages.tar" | while IFS= read -r p; do
	tar -xOf "$OS/packages.tar" "$p" | zstd -dc | tar -xOf - +COMPACT_MANIFEST | jq -c '{name, version, origin, licenses}'
done | jq -s -c 'sort_by(.name)' | classify "$LICENSES")" || fail "a package of $OS/packages.tar is not decided"
[ "$(jq length <<< "$packages")" -gt 0 ] || fail "$OS/packages.tar has no package"
echo "packages.tar: $(jq length <<< "$packages") packages, each decided by licenses.txt; with their source: $(jq -r '[.[] | select(.source) | "\(.name) (\((.licenses // []) | join(", ")))"] | join(", ") | if . == "" then "none" else . end' <<< "$packages")"
PACKAGES_TAR="$OS/packages.tar" sources "$OS" "$packages"

echo "==> $RESCUE"
[ "$(jq -r '.source | "\(.path) \(.sha256)"' "$RESCUE/manifest.json")" = "os/freebsd-$RELEASE/$ARCH/src.txz $want" ] \
	|| fail "$RESCUE/manifest.json does not name the source of its FreeBSD (source: os/freebsd-$RELEASE/$ARCH/src.txz with the SHA-256 of SHA256SUMS)"
[ "$(jq -r .source.size "$RESCUE/manifest.json")" = "$(size "$OS/src.txz")" ] || fail "$RESCUE/manifest.json has another size of src.txz than $OS/src.txz has"
echo "the manifest names the source of its FreeBSD: os/freebsd-$RELEASE/$ARCH/src.txz, the release's by the checksum of SHA256SUMS"
packages="$(jq -c '[(.packages // [])[] | {name, version, origin, licenses}]' "$RESCUE/manifest.json" | classify "$LICENSES")" || fail "a package of $RESCUE/manifest.json is not decided"
[ "$(jq -c '[.[].name]' <<< "$packages")" = "$(awk '{print $1}' "$RESCUE/packages.txt" | jq -R . | jq -s -c .)" ] || fail "$RESCUE/manifest.json does not describe the packages of $RESCUE/packages.txt"
echo "the packages in the image: $(jq -r '[.[] | "\(.name) (\((.licenses // []) | join(", ")))"] | join(", ")' <<< "$packages"), each decided by licenses.txt"
sources "$RESCUE" "$packages"
echo "==> ok"
