#!/usr/bin/env bash
# Publish what ump fetches onto machines to an S3-compatible store (or any
# S3-compatible store): the rescue images and their entry script, the
# installer, the operating system images, the profiles, and the ump
# binaries with the script that installs them. The layout below the
# prefix is the one the storage must serve (see images/rescue/README.md):
#
#   get.sh                                 curl -fsSL <storage>/get.sh | sh
#   bin/latest/ump-<os>-<arch>, SHA256SUMS, VERSION
#   bin/v/<version>/...                    (kept; UMP_VERSION= of get.sh)
#   rescue/kexec-into-rescue.sh
#   rescue/<arch>/manifest.json, vmlinuz, initrd.zst, kexec, ...
#   rescue/v/<version>/<arch>/...          (kept for rollbacks)
#   install/install.sh
#   profiles/default.yaml, profiles/layouts.yaml, profiles/firewalls.yaml
#   os/index.json                          the image names (for the profile wizard)
#   os/<os>/<arch>/manifest.json, rootfs.tar.zst, ...
#   os/<os>/v/<version>/<arch>/...         (kept for rollbacks)
#
# Per image, manifest.json is uploaded last, so a reader never sees a
# manifest that points to files which are not there yet.
#
# Usage: upload.sh [rescue|os|index|bin|scripts|profiles]...  (default: everything that is built;
#        index alone rewrites os/index.json from what the bucket holds)
#
# Environment:
#   STORE_ACCOUNT_ID         account ID (makes the endpoint
#                         https://<id>.storage.example.net), or
#   STORE_ENDPOINT           the S3 endpoint URL directly
#   STORE_ACCESS_KEY_ID      an API token with object read and write
#   STORE_SECRET_ACCESS_KEY
#   STORE_BUCKET             the bucket
#   STORE_PREFIX             key prefix inside the bucket (default: none, the
#                         bucket root is the storage's base URL)
#   DRY_RUN=1             print what would be uploaded
#
# Needs the AWS CLI (aws) or rclone.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${STORE_PREFIX:-}"
PREFIX="${PREFIX%/}"
DRY_RUN="${DRY_RUN:-0}"

what=("$@")
[ ${#what[@]} -gt 0 ] || what=(rescue os bin scripts profiles)
for v in STORE_ACCESS_KEY_ID STORE_SECRET_ACCESS_KEY STORE_BUCKET; do
	[ -n "${!v:-}" ] || { echo "upload.sh: $v is not set (see the environment section of images/rescue/README.md)" >&2; exit 2; }
done
endpoint="${STORE_ENDPOINT:-}"
if [ -z "$endpoint" ]; then
	[ -n "${STORE_ACCOUNT_ID:-}" ] || { echo "upload.sh: set STORE_ACCOUNT_ID or STORE_ENDPOINT" >&2; exit 2; }
	endpoint="https://${STORE_ACCOUNT_ID}.storage.example.net"
fi

if command -v aws >/dev/null; then
	tool=aws
elif command -v rclone >/dev/null; then
	tool=rclone
else
	echo "upload.sh: neither aws nor rclone found; install one (brew install awscli, or rclone)" >&2
	exit 2
fi

content_type() {
	case "$1" in
	*.json) echo application/json ;;
	*.txt|*.sh|SHA256SUMS|VERSION) echo text/plain ;;
	*.yaml|*.yml) echo application/yaml ;;
	*) echo application/octet-stream ;;
	esac
}

s3() { # s3 ARGS...: the tool with the credentials
	case "$tool" in
	aws)
		AWS_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}" \
			aws s3 --only-show-errors --endpoint-url "$endpoint" "$@"
		;;
	rclone)
		RCLONE_CONFIG_UPLOAD_TYPE=s3 RCLONE_CONFIG_UPLOAD_PROVIDER=Other RCLONE_CONFIG_UPLOAD_ENDPOINT="$endpoint" \
			RCLONE_CONFIG_UPLOAD_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" RCLONE_CONFIG_UPLOAD_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" \
			rclone "$@"
		;;
	esac
}

put() { # put FILE KEY
	local file="$1" key="$2" type
	type="$(content_type "$(basename "$file")")"
	if [ "$DRY_RUN" = 1 ]; then
		echo "would upload $file -> s3://$STORE_BUCKET/$key ($type)"
		return 0
	fi
	case "$tool" in
	aws) s3 cp --content-type "$type" "$file" "s3://$STORE_BUCKET/$key" ;;
	rclone) s3 copyto --s3-no-check-bucket "$file" "upload:$STORE_BUCKET/$key" ;;
	esac
	echo "uploaded s3://$STORE_BUCKET/$key"
}

copy() { # copy KEY NEWKEY: within the bucket, on the server; large files go up once
	if [ "$DRY_RUN" = 1 ]; then
		echo "would copy s3://$STORE_BUCKET/$1 -> s3://$STORE_BUCKET/$2"
		return 0
	fi
	case "$tool" in
	# R2 implements neither object tagging nor the tagging directive that
	# "aws s3 cp" sends for a copy; the low-level call sends only what it
	# is told, and the server copies the metadata (content type) itself
	aws)
		AWS_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}" \
			aws s3api copy-object --endpoint-url "$endpoint" --bucket "$STORE_BUCKET" --copy-source "$STORE_BUCKET/$1" --key "$2" --metadata-directive COPY >/dev/null
		;;
	rclone) s3 copyto "upload:$STORE_BUCKET/$1" "upload:$STORE_BUCKET/$2" ;;
	esac
	echo "copied   s3://$STORE_BUCKET/$2"
}

key() { printf '%s' "${PREFIX:+$PREFIX/}$1"; }

# publish_image DIR KIND NAME: the files of a built image directory to
# <kind>/<name>/<arch>/ and <kind>/<name>/v/<version>/<arch>/ (the rescue
# image has no name level: rescue/<arch>/)
publish_image() {
	local dir="$1" kind="$2" name="$3" arch version base files f dest
	arch="$(basename "$dir")"
	version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$dir/manifest.json" | head -n 1)"
	[ -n "$version" ] || { echo "upload.sh: no version in $dir/manifest.json" >&2; exit 2; }
	base="$kind${name:+/$name}"
	files=()
	for f in vmlinuz initrd.zst kexec rootfs.tar.zst SHA256SUMS packages.txt; do [ -f "$dir/$f" ] && files+=("$f"); done
	echo "==> uploading $base $arch, version $version, to $endpoint bucket $STORE_BUCKET"
	# the versioned copy goes up, the current one is a copy of it on the server
	for f in "${files[@]}"; do put "$dir/$f" "$(key "$base/v/$version/$arch/$f")"; done
	put "$dir/manifest.json" "$(key "$base/v/$version/$arch/manifest.json")"
	for f in "${files[@]}"; do copy "$(key "$base/v/$version/$arch/$f")" "$(key "$base/$arch/$f")"; done
	copy "$(key "$base/v/$version/$arch/manifest.json")" "$(key "$base/$arch/manifest.json")"
}

# publish_os_index writes os/index.json: the names of the images the
# storage holds (the ump config os wizard offers them)
publish_os_index() {
	local names f
	case "$tool" in
	aws)
		names="$(AWS_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}" \
			aws s3 ls --endpoint-url "$endpoint" "s3://$STORE_BUCKET/$(key os/)" | sed -n 's|.*PRE \(.*\)/$|\1|p' | grep -v '^v$' | sort)"
		;;
	rclone) names="$(s3 lsd "upload:$STORE_BUCKET/$(key os/)" | awk '{print $NF}' | grep -v '^v$' | sort)" ;;
	esac
	f="$(mktemp)"
	printf '{"images": [%s]}\n' "$(printf '%s\n' $names | sed 's/.*/"&"/' | paste -sd, - | sed 's/,/, /g')" > "$f"
	put "$f" "$(key os/index.json)"
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
		for d in "$HERE"/ubuntu/out/*/*/; do
			[ -f "$d/manifest.json" ] || continue
			name="$(sed -n 's/.*"os": "\([^"]*\)".*/\1/p' "$d/manifest.json" | head -n 1)"
			[ -n "$name" ] || { echo "upload.sh: no os name in $d/manifest.json" >&2; exit 2; }
			publish_image "${d%/}" os "$name"; built=1
		done
		[ "$built" = 1 ] && publish_os_index
		;;
	bin)
		dist="$HERE/../dist"
		[ -f "$dist/VERSION" ] || { [ "${#what[@]}" -gt 1 ] && continue; echo "upload.sh: no dist/VERSION; run make dist first" >&2; exit 2; }
		version="$(cat "$dist/VERSION")"
		case "$version" in
		dev|*-dirty) [ "${FORCE:-0}" = 1 ] || { echo "upload.sh: the binaries are $version (no tag, or uncommitted changes); tag a release (git tag v1.2.3) or FORCE=1" >&2; exit 2; } ;;
		esac
		echo "==> uploading the binaries, version $version"
		for f in "$dist"/ump-*; do put "$f" "$(key "bin/v/$version/$(basename "$f")")"; done
		put "$dist/SHA256SUMS" "$(key "bin/v/$version/SHA256SUMS")"
		put "$dist/VERSION" "$(key "bin/v/$version/VERSION")"
		for f in "$dist"/ump-* "$dist/SHA256SUMS" "$dist/VERSION"; do copy "$(key "bin/v/$version/$(basename "$f")")" "$(key "bin/latest/$(basename "$f")")"; done
		built=1
		;;
	scripts)
		echo "==> uploading the scripts"
		put "$HERE/rescue/kexec-into-rescue.sh" "$(key rescue/kexec-into-rescue.sh)"
		put "$HERE/../install/install.sh" "$(key install/install.sh)"
		put "$HERE/../install/get.sh" "$(key get.sh)"
		built=1
		;;
	index)
		echo "==> uploading os/index.json"
		publish_os_index; built=1
		;;
	profiles)
		echo "==> uploading the profiles"
		for f in "$HERE"/../profiles/*.yaml; do put "$f" "$(key "profiles/$(basename "$f")")"; done
		built=1
		;;
	*) echo "upload.sh: unknown item $w (rescue, os, index, bin, scripts, profiles)" >&2; exit 2 ;;
	esac
done
[ "$built" = 1 ] || { echo "upload.sh: nothing to upload; build first (make rescue-image, make os-image, make dist)" >&2; exit 2; }
echo "==> done: the bucket's public base URL${PREFIX:+/$PREFIX} is the storage (ump's storage setting)"
