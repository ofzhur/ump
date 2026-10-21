#!/usr/bin/env bash
# Publish what ump fetches onto machines to the storage: the rescue
# images and their entry script, the installer, the operating system
# images, the profiles, and the ump binaries with the script that
# installs them. The storage is any place that then serves plain GET
# requests for the layout below (see images/rescue/README.md); this
# script writes it to one of three kinds of target:
#
#   UPLOAD_TARGET=s3://bucket[/prefix]          an S3 bucket, or an S3-compatible
#                                               store with S3_ENDPOINT=https://...
#                                               (AWS CLI or rclone; the usual
#                                               AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
#                                               or an AWS profile for the credentials)
#   UPLOAD_TARGET=scp://[user@]host/abs/path    a directory on a web server, over SSH
#                                               (rsync when both sides have it, else scp;
#                                               UPLOAD_SSH_OPTIONS="-i key -p 2222" for ssh)
#   UPLOAD_TARGET=file:///abs/path              a local directory (a mounted web root)
#
# The layout written below the target:
#
#   get.sh                                 curl -fsSL <storage>/get.sh | sh
#   bin/latest/ump-<os>-<arch>, SHA256SUMS, VERSION
#   bin/v/<version>/...                    (kept; UMP_VERSION= of get.sh)
#   rescue/kexec-into-rescue.sh
#   rescue/<arch>/manifest.json, vmlinuz, initrd.zst, kexec, ...
#   rescue/v/<version>/<arch>/...          (kept for rollbacks)
#   install/install.sh, install/nixos/ump.nix, install/motd.sh, install/bashrc.sh
#   profiles/ubuntu-26.04.yaml, ... , profiles/layouts.yaml, profiles/firewalls.yaml, profiles/users.yaml, profiles/userdata.yaml
#   os/index.json                          the image names (for the profile wizard)
#   os/<os>/<arch>/manifest.json, rootfs.tar.zst, ...
#   os/<os>/v/<version>/<arch>/...         (kept for rollbacks)
#
# Large files go up once, under v/, and are copied to the current path on
# the target side. Per image, manifest.json is uploaded last, so a reader
# never sees a manifest that points to files which are not there yet.
#
# Usage: upload.sh [rescue|os|index|bin|scripts|profiles]...  (os: OS_LIST="a b" limits it to those images)
#  (default: everything that is built;
#        index alone rewrites os/index.json from what the target holds)
#
# Environment: UPLOAD_TARGET (required), S3_ENDPOINT (S3-compatible stores),
# AWS_* (credentials, region), UPLOAD_SSH_OPTIONS (scp targets), DRY_RUN=1
# (print what would be done).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DRY_RUN="${DRY_RUN:-0}"
TARGET="${UPLOAD_TARGET:-}"

what=("$@")
[ ${#what[@]} -gt 0 ] || what=(rescue os bin scripts profiles)
[ -n "$TARGET" ] || { echo "upload.sh: set UPLOAD_TARGET to s3://bucket[/prefix], scp://[user@]host/path or file:///path (see the comment at the top)" >&2; exit 2; }

# --- the target kinds -----------------------------------------------------
#
# Each kind implements: put FILE KEY, copy KEY NEWKEY (target side), and
# list_dirs PREFIX (the directory names below a prefix, one per line).

case "$TARGET" in
s3://*)
	kind=s3
	rest="${TARGET#s3://}"
	bucket="${rest%%/*}"
	prefix=""; [[ "$rest" == */* ]] && prefix="${rest#*/}"
	prefix="${prefix%/}"
	endpoint="${S3_ENDPOINT:-}"
	if command -v aws >/dev/null; then
		tool=aws
	elif command -v rclone >/dev/null; then
		tool=rclone
	else
		echo "upload.sh: neither aws nor rclone found; install one (brew install awscli, or rclone)" >&2; exit 2
	fi
	;;
scp://*)
	kind=scp
	rest="${TARGET#scp://}"
	host="${rest%%/*}"
	dir="/${rest#*/}"
	[ "$dir" != "/$rest" ] || { echo "upload.sh: scp://[user@]host/abs/path needs a path" >&2; exit 2; }
	;;
file://*)
	kind=file
	dir="${TARGET#file://}"
	[ -d "$dir" ] || mkdir -p "$dir"
	;;
/*)
	kind=file; dir="$TARGET"; [ -d "$dir" ] || mkdir -p "$dir"
	;;
*)
	echo "upload.sh: unknown target $TARGET (s3://, scp:// or file://)" >&2; exit 2
	;;
esac

content_type() {
	case "$1" in
	*.json) echo application/json ;;
	*.txt|*.sh|SHA256SUMS|VERSION|THIRD_PARTY_NOTICES) echo text/plain ;;
	*.yaml|*.yml) echo application/yaml ;;
	*) echo application/octet-stream ;;
	esac
}

key() { printf '%s' "${prefix:+$prefix/}$1"; }

aws_s3() { aws s3 --only-show-errors ${endpoint:+--endpoint-url "$endpoint"} "$@"; }
aws_api() { aws s3api ${endpoint:+--endpoint-url "$endpoint"} "$@"; }
rc() { # rclone with the S3 remote from the environment
	RCLONE_CONFIG_UPLOAD_TYPE=s3 RCLONE_CONFIG_UPLOAD_PROVIDER="${RCLONE_S3_PROVIDER:-Other}" ${endpoint:+RCLONE_CONFIG_UPLOAD_ENDPOINT="$endpoint"} \
		RCLONE_CONFIG_UPLOAD_ENV_AUTH=true rclone "$@"
}
ssh_target() { ssh -o BatchMode=yes ${UPLOAD_SSH_OPTIONS:-} "$host" "$@"; }

put() { # put FILE KEY
	local file="$1" k="$2" type
	type="$(content_type "$(basename "$file")")"
	if [ "$DRY_RUN" = 1 ]; then echo "would upload $file -> $TARGET/$k ($type)"; return 0; fi
	case "$kind" in
	s3)
		case "$tool" in
		aws) aws_s3 cp --content-type "$type" "$file" "s3://$bucket/$(key "$k")" ;;
		rclone) rc copyto --s3-no-check-bucket "$file" "upload:$bucket/$(key "$k")" ;;
		esac
		;;
	scp)
		ssh_target mkdir -p "$(dirname "$dir/$k")"
		if command -v rsync >/dev/null && ssh_target command -v rsync >/dev/null 2>&1; then
			rsync -q --partial -e "ssh -o BatchMode=yes ${UPLOAD_SSH_OPTIONS:-}" "$file" "$host:$dir/$k"
		else
			scp -q -o BatchMode=yes ${UPLOAD_SSH_OPTIONS:-} "$file" "$host:$dir/$k"
		fi
		;;
	file)
		mkdir -p "$(dirname "$dir/$k")"
		cp "$file" "$dir/$k.tmp" && mv "$dir/$k.tmp" "$dir/$k"
		;;
	esac
	echo "uploaded $k"
}

copy() { # copy KEY NEWKEY: on the target side, so that large files go up once
	if [ "$DRY_RUN" = 1 ]; then echo "would copy $1 -> $2"; return 0; fi
	case "$kind" in
	s3)
		case "$tool" in
		# the low-level call sends only what it is told: some stores reject
		# the tagging directive that "aws s3 cp" adds to a copy
		aws) aws_api copy-object --bucket "$bucket" --copy-source "$bucket/$(key "$1")" --key "$(key "$2")" --metadata-directive COPY >/dev/null ;;
		rclone) rc copyto "upload:$bucket/$(key "$1")" "upload:$bucket/$(key "$2")" ;;
		esac
		;;
	scp) ssh_target "mkdir -p '$(dirname "$dir/$2")' && cp -p '$dir/$1' '$dir/$2.tmp' && mv '$dir/$2.tmp' '$dir/$2'" ;;
	file) mkdir -p "$(dirname "$dir/$2")" && cp -p "$dir/$1" "$dir/$2.tmp" && mv "$dir/$2.tmp" "$dir/$2" ;;
	esac
	echo "copied   $2"
}

list_dirs() { # list_dirs PREFIX: the directory names right below it
	case "$kind" in
	s3)
		case "$tool" in
		aws) aws s3 ${endpoint:+--endpoint-url "$endpoint"} ls "s3://$bucket/$(key "$1")/" | sed -n 's|.*PRE \(.*\)/$|\1|p' ;;
		rclone) rc lsd "upload:$bucket/$(key "$1")" | awk '{print $NF}' ;;
		esac
		;;
	scp) ssh_target "ls -1 '$dir/$1' 2>/dev/null" ;;
	file) ls -1 "$dir/$1" 2>/dev/null ;;
	esac
}

# --- what is published ----------------------------------------------------

# publish_image DIR KIND NAME: the files of a built image directory to
# <kind>/<name>/<arch>/ and <kind>/<name>/v/<version>/<arch>/ (the rescue
# image has no name level: rescue/<arch>/)
publish_image() {
	local d="$1" k="$2" name="$3" arch version base files f
	arch="$(basename "$d")"
	version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$d/manifest.json" | head -n 1)"
	[ -n "$version" ] || { echo "upload.sh: no version in $d/manifest.json" >&2; exit 2; }
	base="$k${name:+/$name}"
	files=()
	for f in vmlinuz initrd.zst kexec rootfs.tar.zst SHA256SUMS packages.txt; do [ -f "$d/$f" ] && files+=("$f"); done
	echo "==> uploading $base $arch, version $version, to $TARGET"
	for f in "${files[@]}"; do put "$d/$f" "$base/v/$version/$arch/$f"; done
	put "$d/manifest.json" "$base/v/$version/$arch/manifest.json"
	for f in "${files[@]}"; do copy "$base/v/$version/$arch/$f" "$base/$arch/$f"; done
	copy "$base/v/$version/$arch/manifest.json" "$base/$arch/manifest.json"
}

# publish_os_index writes os/index.json: the names of the images the
# storage holds (the ump config os wizard offers them)
publish_os_index() {
	local names f
	names="$(list_dirs os | grep -v '^v$' | grep -v '\.json$' | sort || true)"
	f="$(mktemp)"
	printf '{"images": [%s]}\n' "$(printf '%s\n' $names | sed 's/.*/"&"/' | paste -sd, - | sed 's/,/, /g')" > "$f"
	put "$f" os/index.json
	rm -f "$f"
}

built=0
for w in "${what[@]}"; do
	case "$w" in
	rescue)
		for d in "$HERE"/rescue/out/*/; do
			[ -f "$d/manifest.json" ] || continue
			publish_image "${d%/}" rescue ""; built=1
		done
		;;
	os)
		for d in "$HERE"/base/out/*/*/ "$HERE"/nixos/out/*/*/ "$HERE"/arch/out/*/*/; do
			[ -f "$d/manifest.json" ] || continue
			name="$(sed -n 's/.*"os": "\([^"]*\)".*/\1/p' "$d/manifest.json" | head -n 1)"
			[ -n "$name" ] || { echo "upload.sh: no os name in $d/manifest.json" >&2; exit 2; }
			# OS_LIST="debian-13 debian-12" publishes those only
			case " ${OS_LIST:-$name} " in *" $name "*) ;; *) continue ;; esac
			publish_image "${d%/}" os "$name"; built=1
		done
		[ "$built" = 1 ] && publish_os_index
		;;
	index)
		echo "==> uploading os/index.json"
		publish_os_index; built=1
		;;
	bin)
		dist="$HERE/../dist"
		[ -f "$dist/VERSION" ] || { [ "${#what[@]}" -gt 1 ] && continue; echo "upload.sh: no dist/VERSION; run make dist first" >&2; exit 2; }
		version="$(cat "$dist/VERSION")"
		case "$version" in
		dev|*-dirty) [ "${FORCE:-0}" = 1 ] || { echo "upload.sh: the binaries are $version (no tag, or uncommitted changes); tag a release (git tag v1.2.3) or FORCE=1" >&2; exit 2; } ;;
		esac
		echo "==> uploading the binaries, version $version, to $TARGET"
		for f in "$dist"/ump-*; do put "$f" "bin/v/$version/$(basename "$f")"; done
		put "$dist/SHA256SUMS" "bin/v/$version/SHA256SUMS"
		put "$dist/VERSION" "bin/v/$version/VERSION"
		put "$dist/THIRD_PARTY_NOTICES" "bin/v/$version/THIRD_PARTY_NOTICES"
		for f in "$dist"/ump-* "$dist/SHA256SUMS" "$dist/VERSION" "$dist/THIRD_PARTY_NOTICES"; do copy "bin/v/$version/$(basename "$f")" "bin/latest/$(basename "$f")"; done
		built=1
		;;
	scripts)
		echo "==> uploading the scripts to $TARGET"
		put "$HERE/rescue/kexec-into-rescue.sh" rescue/kexec-into-rescue.sh
		put "$HERE/../install/install.sh" install/install.sh
		put "$HERE/../install/nixos/ump.nix" install/nixos/ump.nix
		put "$HERE/../install/motd.sh" install/motd.sh
		put "$HERE/../install/bashrc.sh" install/bashrc.sh
		put "$HERE/../install/get.sh" get.sh
		built=1
		;;
	profiles)
		echo "==> uploading the profiles to $TARGET"
		for f in "$HERE"/../profiles/*.yaml; do put "$f" "profiles/$(basename "$f")"; done
		built=1
		;;
	*) echo "upload.sh: unknown item $w (rescue, os, index, bin, scripts, profiles)" >&2; exit 2 ;;
	esac
done
[ "$built" = 1 ] || { echo "upload.sh: nothing to upload; build first (make rescue-image, make os-image, make dist)" >&2; exit 2; }
echo "==> done: the URL that serves $TARGET is the storage (ump's storage setting)"
