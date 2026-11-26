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
#   out/os/freebsd-<release>/<arch>/       base.txz, kernel.txz, packages.tar, src.txz, packages-source.tar,
#                                          manifest.json, SHA256SUMS, packages.txt
#
# as the storage has them under rescue/ and os/. What FreeBSD made goes
# out with its source: src.txz is the release's own, and
# packages-source.tar has, for every package whose license asks for it
# (licenses.txt decides), the port it was built from and the files that
# port's distinfo names; the same beside the rescue image for a package
# of that kind in it. The sets and packages a build before left there
# are taken again when their checksums still hold, and so are the
# distribution files and the ports' directories in out/cache/.
# Environment: VERSION (default: today's date), OUT (default: ./out),
# MIRROR (the releases, default https://download.freebsd.org/releases),
# PKG_MIRROR (default https://pkg.FreeBSD.org), PKG_BRANCH (default
# quarterly), DISTCACHE (FreeBSD's distribution files, default
# http://distcache.FreeBSD.org/ports-distfiles: the address the ports
# tree has for it, without TLS; every file is checked against its
# distinfo), PORTS_GIT (the ports tree, default
# https://git.FreeBSD.org/ports.git).
set -euo pipefail

ARCH="${1:-}"
RELEASE="${2:-15.1}"
VERSION="${VERSION:-$(date -u +%Y.%m.%d)}"
MIRROR="${MIRROR:-https://download.freebsd.org/releases}"
PKG_MIRROR="${PKG_MIRROR:-https://pkg.FreeBSD.org}"
PKG_BRANCH="${PKG_BRANCH:-quarterly}"
DISTCACHE="${DISTCACHE:-http://distcache.FreeBSD.org/ports-distfiles}"
PORTS_GIT="${PORTS_GIT:-https://git.FreeBSD.org/ports.git}"
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
CACHE="$OUT/cache"
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

# --- the sources: which packages go out with their source ------------------
#
# What licenses.txt decides, and what a port's distinfo names. These
# functions need jq and die alone: test-sources.sh runs them too, on
# what a build left.

# license_list FILE: licenses.txt as JSON, {licenses: {NAME: "source" or
# "-"}, packages: {NAME: {class, reason}}}
license_list() {
	jq -R -s -c '
		reduce (split("\n") | to_entries[]
			| {line: (.key + 1), words: (.value | sub("#.*$"; "") | [scan("[^ \t]+")])}
			| select(.words | length > 0)) as $l
			({licenses: {}, packages: {}};
			if $l.words[0] == "package" and ($l.words | length) > 3 and ($l.words[2] | . == "source" or . == "-") and .packages[$l.words[1]] == null then
				.packages[$l.words[1]] = {class: $l.words[2], reason: ($l.words[3:] | join(" "))}
			elif ($l.words | length) == 2 and ($l.words[1] | . == "source" or . == "-") and .licenses[$l.words[0]] == null then
				.licenses[$l.words[0]] = $l.words[1]
			else
				error("line \($l.line) is neither \"NAME source\" nor \"NAME -\" nor \"package NAME source|- REASON\", or names what a line before it names")
			end)
		| [.licenses | to_entries[] | select(.value == "-" and (.key | test("^(AGPL|GPL|LGPL|GMGPL|MPL|CDDL|EPL|EUPL)"))) | .key] as $refused
		| if ($refused | length) > 0 then error("\($refused | join(", ")): a license of that family asks for the source") else . end
	' "$1"
}

# classify LIST: the packages on stdin (a JSON array; name, version and
# licenses of each) with source, whether the package goes out with its
# source, and reason, why it does when its own licenses do not ask for
# it. A package the list does not decide stops everything: one under a
# license without a line, one that names no license and has no line.
classify() {
	local packages undecided
	packages="$(cat)"
	undecided="$(jq -r --argjson list "$1" '.[]
		| ((.licenses // []) - ($list.licenses | keys)) as $unknown
		| if ($unknown | length) > 0 then
			"\(.name) \(.version) is under \($unknown | join(" and ")), which licenses.txt has no line for: read the license (the package has it under /usr/local/share/licenses), then add \"NAME source\" or \"NAME -\" there"
		  elif ((.licenses // []) | length) == 0 and $list.packages[.name] == null then
			"\(.name) \(.version) names no license, and licenses.txt has no line for the package: see what it holds, then add \"package \(.name) source REASON\" or \"package \(.name) - REASON\" there"
		  else empty end' <<< "$packages")"
	[ -z "$undecided" ] || die "$undecided"
	jq -c --argjson list "$1" 'map(
		([(.licenses // [])[] | select($list.licenses[.] == "source")] | length > 0) as $asks
		| ($list.packages[.name] // {}) as $line
		| . + {source: ($asks or $line.class == "source")}
		| if ($asks | not) and $line.class == "source" then . + {reason: $line.reason} else . end)' <<< "$packages"
}

# distinfo_files: the files a port's distinfo (on stdin) names, one per
# line: the path below the distribution files' directory, the SHA-256,
# the size
distinfo_files() {
	jq -R -s -r '
		[split("\n")[] | select(test("^[ \t]*$") | not)
			| capture("^(?<kind>SHA256|SIZE) \\((?<file>[^ )]+)\\) = (?<value>[0-9a-f]+)$")
			  // capture("^(?<kind>TIMESTAMP) = (?<value>[0-9]+)$")
			  // error("a line of another kind than TIMESTAMP, SHA256 and SIZE: \(.)")] as $lines
		| ($lines | map(select(.kind == "SIZE") | {key: .file, value: .value}) | from_entries) as $size
		| $lines[] | select(.kind == "SHA256")
		| if (.file | test("^[A-Za-z0-9._+~,=@-]+(/[A-Za-z0-9._+~,=@-]+)*$") | not) or (.file | test("(^|/)\\.\\.?(/|$)")) then error("a file name with characters this script does not take: \(.file)")
		  elif (.value | test("^[0-9a-f]{64}$") | not) then error("\(.file): its SHA256 is not one")
		  elif ($size[.file] // "" | test("^[0-9]+$") | not) then error("\(.file): no SIZE")
		  else [.file, .value, $size[.file]] | @tsv end'
}

# --- the build ----------------------------------------------------------------

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

LICENSES="$(license_list "$HERE/licenses.txt")" || die "licenses.txt is not what this script reads (jq says where)"

log "the distribution sets of FreeBSD $RELEASE for $ARCH, and the release's source"
mkdir -p "$RESCUE_OUT" "$OS_OUT" "$ROOT" "$BOOTFS/boot" "$WORK/kernel" "$WORK/packages"
for f in base.txz kernel.txz src.txz; do
	path="$target/$RELEASE-RELEASE/$f"
	want="$(awk -v p="$path" '$2 == p {print $1}' "$HERE/SHA256SUMS")"
	[ -n "$want" ] || die "SHA256SUMS has no line for $path; add the release's, from $MIRROR/$target/$RELEASE-RELEASE/MANIFEST"
	if [ -f "$OS_OUT/$f" ] && [ "$(sha256 "$OS_OUT/$f")" = "$want" ]; then
		echo "$f: as a build before left it"
		continue
	fi
	# the source is one file for every architecture of a release
	other=""
	if [ "$f" = src.txz ]; then
		for o in "$OUT/os/freebsd-$RELEASE"/*/src.txz; do
			if [ -f "$o" ] && [ "$o" != "$OS_OUT/$f" ] && [ "$(sha256 "$o")" = "$want" ]; then other="$o"; break; fi
		done
	fi
	if [ -n "$other" ]; then
		echo "$f: as the build for $(basename "$(dirname "$other")") left it"
		ln -f "$other" "$OS_OUT/$f" 2>/dev/null || { cp "$other" "$OS_OUT/$f.part" && mv "$OS_OUT/$f.part" "$OS_OUT/$f"; }
	else
		echo "$f: from $MIRROR/$path"
		get -o "$OS_OUT/$f.part" "$MIRROR/$path"
		have="$(sha256 "$OS_OUT/$f.part")"
		[ "$have" = "$want" ] || { rm -f "$OS_OUT/$f.part"; die "$MIRROR/$path has the SHA-256 $have, and SHA256SUMS says $want"; }
		mv "$OS_OUT/$f.part" "$OS_OUT/$f"
	fi
done
src_sum="$(awk -v p="$target/$RELEASE-RELEASE/src.txz" '$2 == p {print $1}' "$HERE/SHA256SUMS")"

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
jq -c '{name, version, path: (.repopath // .path), sum, size: .pkgsize, flatsize, deps: ((.deps // {}) | keys),
	origin, licenses: (.licenses // []), commit: (.annotations.ports_top_git_hash // "")}' "$cat/packagesite.yaml" > "$WORK/index"

# closure NAME...: the named packages and what they depend on, as an
# array of the catalogue's entries with wanted (it was named), and with
# source: whether licenses.txt sends the package out with its source
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
	' --args "$@" < "$WORK/index" | classify "$LICENSES"
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

# ports_path COMMIT PATH: a directory or a file of FreeBSD's ports tree as
# it was at a commit, as a tar in the cache ($CACHE/ports/COMMIT/PATH.tar,
# PATH in its members' names); in_tree says whether the tree has the
# path there. The repository is asked for that commit alone, without
# its history and without the files (a partial clone: the directories,
# 9 MB), then for the files of the path; what arrives is named by its
# hash, from the commit down. git fetches nothing by itself here
# (GIT_NO_LAZY_FETCH): a partial clone that is asked for an object it
# lacks goes for it, and for a commit without a limit to its history.
ports_path() {
	local commit="$1" path="$2" tar="$CACHE/ports/$1/$2.tar" repo="$WORK/ports.git" dir="$WORK/ports-path" entry mode oid name
	export GIT_NO_LAZY_FETCH=1 GIT_TERMINAL_PROMPT=0
	in_tree=1
	if [ -s "$tar" ] && tar -tf "$tar" >/dev/null 2>&1; then return 0; fi
	if ! command -v git >/dev/null; then
		echo "git, for the ports tree"
		# (the package lists are there when the build dependencies were installed)
		apt-get install -y -q --no-install-recommends git > "$WORK/apt.log" 2>&1 \
			|| { apt-get update -q >/dev/null && apt-get install -y -q --no-install-recommends git > "$WORK/apt.log" 2>&1; } \
			|| { tail -n 20 "$WORK/apt.log" >&2; die "git could not be installed"; }
	fi
	if [ ! -d "$repo" ]; then
		git init -q --bare "$repo"
		git -C "$repo" remote add origin "$PORTS_GIT"
		git -C "$repo" config remote.origin.promisor true
		git -C "$repo" config remote.origin.partialclonefilter blob:none
	fi
	# (a commit this build fetched has its mark: git is not asked for one it may lack)
	if [ ! -e "$repo/ump-fetched-$commit" ]; then
		echo "the ports tree at $commit: from $PORTS_GIT"
		git -C "$repo" fetch -q --depth=1 --filter=blob:none origin "$commit" || die "cannot fetch the commit $commit of $PORTS_GIT"
		: > "$repo/ump-fetched-$commit"
	fi
	entry="$(git -C "$repo" ls-tree "$commit" -- "$path")" || die "cannot read the ports tree at $commit"
	if [ -z "$entry" ]; then in_tree=0; return 0; fi
	# the files of the path in one request, then each as git has it:
	# nothing is converted on the way, as an archive of git's would be
	git -C "$repo" ls-tree -r --format='%(objectname)' "$commit" -- "$path" \
		| git -C "$repo" -c fetch.negotiationAlgorithm=noop fetch -q origin --no-tags --no-write-fetch-head --recurse-submodules=no --filter=blob:none --stdin \
		|| die "cannot fetch $path at $commit from $PORTS_GIT"
	rm -rf "$dir"
	while IFS= read -r -d '' entry; do
		name="${entry#*$'\t'}"
		read -r mode _ oid <<< "${entry%%$'\t'*}"
		case "$name" in
		*$'\n'*|../*|*/../*|*/..) die "$path at $commit of the ports tree has a file name this script does not take" ;;
		esac
		mkdir -p "$dir/$(dirname "$name")"
		case "$mode" in
		100644|100755)
			git -C "$repo" cat-file blob "$oid" > "$dir/$name" || die "cannot read $name at $commit"
			[ "$mode" = 100644 ] || chmod 0755 "$dir/$name"
			;;
		120000) ln -s "$(git -C "$repo" cat-file blob "$oid")" "$dir/$name" ;;
		*) die "$name at $commit of the ports tree is neither a file nor a symbolic link (mode $mode)" ;;
		esac
	done < <(git -C "$repo" ls-tree -r -z "$commit" -- "$path")
	[ -e "$dir/$path" ] || [ -L "$dir/$path" ] || die "cannot read $path at $commit"
	mkdir -p "$(dirname "$tar")"
	tar -cf "$tar.part" -C "$dir" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 "$path"
	mv "$tar.part" "$tar"
}

# port_references CATEGORY PATH: the other places of the ports tree that
# the makefiles of a port's directory take files from, one per line: a
# port that shares another's Makefile or distinfo names it relative to
# itself (${.CURDIR}/../NAME) or to the tree (${PORTSDIR}/CATEGORY/NAME).
# The framework (Mk, Templates) is not the port's. A place named in
# another way stops the build: the source would lack it.
port_references() {
	local category="$1" f files=() text left
	if [ -d "$2" ]; then
		for f in "$2"/Makefile* "$2"/*.mk; do
			if [ -f "$f" ] && [ ! -L "$f" ]; then files+=("$f"); fi
		done
	elif [ -f "$2" ] && [ ! -L "$2" ]; then
		files=("$2")
	fi
	[ ${#files[@]} -gt 0 ] || return 0
	text="$(sed -E -e 's/#.*//' \
		-e 's,\$\{\.CURDIR\}/\.\./\.\.,@TREE@,g; s,\$\{\.CURDIR:H:H\},@TREE@,g; s,\$\{PORTSDIR\},@TREE@,g' \
		-e 's,\$\{\.CURDIR\}/\.\.,@CATEGORY@,g; s,\$\{\.CURDIR:H\},@CATEGORY@,g' "${files[@]}")"
	grep -oE '@TREE@/[a-z0-9_-]+/[A-Za-z0-9._+-]+' <<< "$text" | sed 's,^@TREE@/,,' || true
	grep -oE '@CATEGORY@/[A-Za-z0-9._+-]+' <<< "$text" | sed "s,^@CATEGORY@/,$category/," || true
	left="$(sed -E -e 's,@TREE@/(Mk|Templates|Tools|Keywords)(/|$|[^A-Za-z0-9._+-]),,g' \
		-e 's,@TREE@/[a-z0-9_-]+/[A-Za-z0-9._+-]+,,g; s,@CATEGORY@/[A-Za-z0-9._+-]+,,g' <<< "$text" | grep -E '@TREE@|@CATEGORY@' || true)"
	[ -z "$left" ] || die "a makefile of $2 names a place of the ports tree in a way this script does not follow: $(head -n 1 <<< "$left" | sed 's/^[[:space:]]*//')"
}

# distfile PATH SHA256 SIZE: a distribution file in the cache
# ($CACHE/distfiles/PATH), from FreeBSD's when the cache has none that
# the distinfo's size and checksum hold for
distfile() {
	local file="$CACHE/distfiles/$1"
	if [ -f "$file" ] && [ "$(bytes "$file")" = "$3" ] && [ "$(sha256 "$file")" = "$2" ]; then return 0; fi
	echo "$1: from $DISTCACHE/$1"
	mkdir -p "$(dirname "$file")"
	get -o "$file.part" "$DISTCACHE/$1" || { rm -f "$file.part"; die "cannot fetch $DISTCACHE/$1, which a distinfo names"; }
	if [ "$(bytes "$file.part")" != "$3" ] || [ "$(sha256 "$file.part")" != "$2" ]; then
		rm -f "$file.part"
		die "$DISTCACHE/$1 is not what the distinfo says ($3 bytes with the SHA-256 $2)"
	fi
	mv "$file.part" "$file"
}

# package_sources LIST DIRECTORY WHERE: packages-source.tar in the
# directory, with the source of every package of a closure that goes out
# with it; no such file when there is none. Per package NAME-VERSION/:
# ports/ has the port's directory as the ports tree had it at the commit
# the package names, with the places its makefiles take files from and
# the tree's COPYRIGHT, and distfiles/ every file the distinfo of those
# names, checked against it.
# WHERE is the README's sentence on where the packages are.
package_sources() {
	local list="$1" out="$2" stage="$WORK/source" name version origin commit nv licenses path files file sum size count todo seen
	rm -rf "$stage"
	rm -f "$out/packages-source.tar"
	[ "$(jq '[.[] | select(.source)] | length' <<< "$list")" -gt 0 ] || return 0
	mkdir -p "$stage"
	while IFS=$'\t' read -r name version origin commit; do
		nv="$name-$version"
		[[ "$nv" =~ ^[A-Za-z0-9][A-Za-z0-9._+,-]*$ ]] || die "$nv is not a name this script takes for a directory"
		[[ "$origin" =~ ^[a-z0-9_-]+/[A-Za-z0-9._+-]+$ ]] || die "$nv: the catalogue's origin ($origin) is no port's directory"
		[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || die "$nv: the catalogue names no commit of the ports tree (ports_top_git_hash), so the port it was built from cannot be told"
		mkdir -p "$stage/$nv/ports" "$stage/$nv/distfiles"
		todo=("$origin") seen=" "
		while [ ${#todo[@]} -gt 0 ]; do
			path="${todo[0]}"
			todo=("${todo[@]:1}")
			case "$seen" in *" $path "*) continue ;; esac
			seen+="$path "
			ports_path "$commit" "$path"
			if [ "$in_tree" = 0 ]; then
				[ "$path" != "$origin" ] || die "$nv: the ports tree has no $origin at the commit $commit"
				echo "$nv: $path, which a makefile names, is not in the ports tree at that commit"
				continue
			fi
			tar -xf "$CACHE/ports/$commit/$path.tar" -C "$stage/$nv/ports"
			echo "$nv: $path of the ports tree at ${commit:0:12}"
			files="$(port_references "${origin%%/*}" "$stage/$nv/ports/$path")"
			[ -z "$files" ] || mapfile -t -O "${#todo[@]}" todo <<< "$files"
		done
		# the ports tree's own notice goes with what is taken out of it
		ports_path "$commit" COPYRIGHT
		[ "$in_tree" = 1 ] || die "$nv: the ports tree has no COPYRIGHT at the commit $commit"
		tar -xf "$CACHE/ports/$commit/COPYRIGHT.tar" -C "$stage/$nv/ports"
		count=0
		while IFS= read -r file; do
			files="$(distinfo_files < "$file")" || die "$nv: ${file#"$stage/$nv/"} is not a distinfo this script reads (jq says why)"
			while IFS=$'\t' read -r path sum size; do
				[ -n "$path" ] || continue
				distfile "$path" "$sum" "$size"
				install -D -m 0644 "$CACHE/distfiles/$path" "$stage/$nv/distfiles/$path"
				count=$((count + 1))
			done <<< "$files"
		done < <(find "$stage/$nv/ports" -mindepth 3 -maxdepth 3 -type f -name distinfo | sort)
		[ "$count" -gt 0 ] || die "$nv: no distinfo of $origin names a file, so its source cannot be taken from FreeBSD's distribution files"
		echo "$nv: what its distinfo names, $count $([ "$count" = 1 ] && echo file || echo files), $(du -sk "$stage/$nv/distfiles" | awk '{printf "%.1f", $1 / 1024}') MB"
	done < <(jq -r '.[] | select(.source) | [.name, .version, .origin, .commit] | @tsv' <<< "$list")
	{
		cat <<-EOF
		The source of the packages of FreeBSD $RELEASE for $ARCH that go out with
		their source. $3

		The packages are FreeBSD's own, as its repository had them:
		  $repo
		Nothing in them was changed, and nothing here was built again. A
		package is here when its license asks whoever passes it on for the
		source, or when it is linked with the library of such a package.

		Per package, NAME-VERSION/ has

		  ports/      the port the package was built from: its directory of
		              FreeBSD's ports tree at the commit the package names,
		              the directories its makefiles take files from, and
		              the tree's COPYRIGHT
		  distfiles/  every file a distinfo there names, the release of the
		              program's authors and their patches, each with the
		              size and the SHA-256 of that distinfo

		The ports tree is $PORTS_GIT; the framework
		that builds a port (Mk/) is the tree's at the same commit. The
		distribution files are as FreeBSD keeps them:
		  $DISTCACHE
		The options a package was built with are in its own manifest (pkg
		info -f, or +COMPACT_MANIFEST in the package).

		EOF
		while IFS=$'\t' read -r nv licenses origin commit; do
			printf '%s\n  licenses: %s\n  port: %s at the commit %s\n' "$nv" "$licenses" "$origin" "$commit"
			path="$(cd "$stage/$nv/ports" && find . -mindepth 2 -maxdepth 2 ! -path "./$origin" | sed 's,^\./,,' | sort | paste -sd' ' - | sed 's/ /, /g')"
			[ -z "$path" ] || printf '  with: %s\n' "$path"
		done < <(jq -r '.[] | select(.source) | ["\(.name)-\(.version)", "\(.licenses | join(", "))\(if .reason then "; here because \(.reason)" else "" end)", .origin, .commit] | @tsv' <<< "$list")
	} > "$stage/README"
	tar -cf "$out/packages-source.tar.part" -C "$stage" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
		README $(jq -r '[.[] | select(.source) | "\(.name)-\(.version)"] | sort | .[]' <<< "$list")
	mv "$out/packages-source.tar.part" "$out/packages-source.tar"
}

log "the packages of the installed system, with what they depend on"
mapfile -t wanted < <(grep -vE '^[[:space:]]*(#|$)' "$HERE/packages.txt" | sed 's/[[:space:]]//g')
os_packages="$(closure "${wanted[@]}")"
# the installer adds the packages with pkg-static and reads its
# specification, JSON, with jq, which the base lacks
rescue_packages="$(closure pkg jq)"
[ ! -f "$OS_OUT/packages.tar" ] || tar -xf "$OS_OUT/packages.tar" -C "$WORK/packages" 2>/dev/null || true
fetch_packages "$os_packages"

log "the source of the packages whose licenses ask for it: $(jq -r '[.[] | select(.source) | .name] | join(" ")' <<< "$os_packages")"
package_sources "$os_packages" "$OS_OUT" "They are in packages.tar beside this file."

log "pkg-static and jq for the memory root"
fetch_packages "$rescue_packages"
package_sources "$rescue_packages" "$RESCUE_OUT" "Files of them are in disk.img.zst beside this file, the UMP FreeBSD rescue image."
# from_package NAME MEMBER...: files of a package, a tar of its files
# under their absolute paths after its manifests, into the memory root
from_package() {
	local file
	file="$(jq -r --arg n "$1" '.[] | select(.name == $n) | .file' <<< "$rescue_packages")"
	shift
	tar --zstd -xpf "$WORK/packages/$file" -C "$ROOT" --no-same-owner --warning=no-unknown-keyword --wildcards "$@" 2> "$WORK/tar.err" \
		|| { cat "$WORK/tar.err" >&2; die "cannot take $* from $file"; }
}
# (each with its license, which a package has in /usr/local/share/licenses)
from_package pkg /usr/local/sbin/pkg-static '/usr/local/share/licenses/pkg-*/*'
from_package jq /usr/local/bin/jq '/usr/local/lib/libjq.so*' '/usr/local/share/licenses/jq-*/*'
from_package oniguruma '/usr/local/lib/libonig.so*' '/usr/local/share/licenses/oniguruma-*/*'
ls -l "$ROOT/usr/local/sbin/pkg-static" "$ROOT/usr/local/bin/jq" "$ROOT"/usr/local/lib/lib*.so.[0-9]

log "the notices, and where the source is"
# FreeBSD's notices stay with what is left of it, and the packages'
# licenses with their files: rescue-exclude.txt must not take them
for f in COPYRIGHT usr/share/doc/legal usr/share/doc/llvm/LICENSE.TXT; do
	[ -s "$ROOT/$f" ] || die "the memory root lacks /$f, a notice of FreeBSD's base.txz: rescue-exclude.txt takes it, or the release has it elsewhere"
done
while read -r p; do
	[ -n "$(ls "$ROOT/usr/local/share/licenses/$p" 2>/dev/null)" ] || die "the memory root has files of the package $p and not its license (/usr/local/share/licenses/$p)"
done < <(jq -r '.[] | "\(.name)-\(.version)"' <<< "$rescue_packages")
cat > "$ROOT/etc/ump-rescue-source" <<EOF
The UMP FreeBSD rescue system is FreeBSD $RELEASE-RELEASE for $ARCH: the
kernel of the release and a part of its base system, as FreeBSD
published them, and files of FreeBSD's packages
$(jq -r '[.[] | "\(.name) \(.version)"] | join(", ")' <<< "$rescue_packages").
They stay under their own licenses: FreeBSD's notices are in /COPYRIGHT
and /usr/share/doc, the packages' in /usr/local/share/licenses.

The source of the release is its src.txz, with the SHA-256
$src_sum.
The storage this image came from has it beside the release's sets, as
os/freebsd-$RELEASE/$ARCH/src.txz, and FreeBSD as
$MIRROR/$target/$RELEASE-RELEASE/src.txz
EOF
jq -e 'all(.[]; .source | not)' <<< "$rescue_packages" >/dev/null || cat >> "$ROOT/etc/ump-rescue-source" <<EOF

The source of the packages $(jq -r '[.[] | select(.source) | .name] | join(", ")' <<< "$rescue_packages") is beside this image in the
storage, in rescue/freebsd-$RELEASE/$ARCH/packages-source.tar
EOF
cat "$ROOT/etc/ump-rescue-source"

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

# what a manifest says of a package beside its name and version: the
# port it was built from, its licenses, and whether its source goes out
# with it
described='origin, licenses, source'
src="$(jq -n --arg path "os/freebsd-$RELEASE/$ARCH/src.txz" --arg sum "$src_sum" --argjson size "$(bytes "$OS_OUT/src.txz")" '{path: $path, sha256: $sum, size: $size}')"

cd "$RESCUE_OUT"
jq -r '.[] | "\(.name) \(.version)"' <<< "$rescue_packages" > packages.txt
rescue_files=(disk.img.zst)
[ ! -f packages-source.tar ] || rescue_files+=(packages-source.tar)
sha256sum "${rescue_files[@]}" > SHA256SUMS
# memory_mb, the smallest machine the image is for: the memory root as
# the kernel holds it, twice the kernel, and 288 MB for the system, an
# installer and ZFS's cache (see README.md, Memory)
kernel_mb="$(mib "$BOOTFS/boot/kernel/kernel")"
jq -n --arg version "$VERSION" --arg release "$RELEASE" --arg arch "$ARCH" --arg built "$built" \
	--argjson memory "$(( mfsroot_mb + 2 * kernel_mb + 288 ))" --argjson disk "$disk_mb" \
	--argjson files "$(file_entries . "${rescue_files[@]}")" --argjson source "$src" \
	--argjson packages "$(jq -c "map({name, version, $described})" <<< "$rescue_packages")" '{
	version: $version, system: "freebsd", release: $release, arch: $arch,
	kernel: ($release + "-RELEASE GENERIC"), built: $built,
	features: ["boot-config"],
	memory_mb: $memory, disk_mb: $disk,
	files: $files, source: $source, packages: $packages}' > manifest.json

cd "$OS_OUT"
# the packages as they are (they are compressed), under stable names
tar -cf packages.tar -C "$WORK/packages" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
	$(jq -r '.[].file' <<< "$os_packages")
jq -r '.[] | "\(.name) \(.version)"' <<< "$os_packages" > packages.txt
os_files=(base.txz kernel.txz packages.tar src.txz)
[ ! -f packages-source.tar ] || os_files+=(packages-source.tar)
sha256sum "${os_files[@]}" > SHA256SUMS
# the size of the installed system: the sets unpacked and the packages added
unpacked=$(( $(xz -l --robot base.txz | awk '$1 == "totals" {print $5}') + $(xz -l --robot kernel.txz | awk '$1 == "totals" {print $5}') + $(jq '[.[].flatsize] | add' <<< "$os_packages") ))
packages="$(jq -c '.[]' <<< "$os_packages" | while read -r p; do
	jq -c --arg s "$(sha256 "$WORK/packages/$(jq -r .file <<< "$p")")" "{name, version, file, sha256: \$s, size, wanted, $described}" <<< "$p"
done | jq -s .)"
jq -n --arg version "$VERSION" --arg os "freebsd-$RELEASE" --arg arch "$ARCH" --arg release "$RELEASE-RELEASE" --arg built "$built" \
	--argjson unpacked "$(( (unpacked + 1048575) / 1048576 ))" --argjson packages "$packages" \
	--argjson files "$(file_entries . "${os_files[@]}")" '{
	version: $version, os: $os, family: "freebsd", arch: $arch, release: $release,
	kernel: ($release + " GENERIC"), rescue: $os,
	unpacked_mb: $unpacked, built: $built,
	files: $files, packages: $packages}' > manifest.json

ls -l "$RESCUE_OUT" "$OS_OUT"

log "the sources, checked as test-sources.sh checks them"
OUT="$OUT" "$HERE/test-sources.sh" "$ARCH" "$RELEASE"

log "done: FreeBSD $RELEASE for $ARCH, version $VERSION; the rescue image needs $(jq .memory_mb "$RESCUE_OUT/manifest.json") MB of memory"
