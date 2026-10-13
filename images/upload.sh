#!/usr/bin/env bash
# Publish what ump fetches onto machines to an S3-compatible store (or any
# S3-compatible store): the rescue images and their entry script, the
# installer, and the operating system images. The layout below the
# prefix is the one the storage must serve (see images/rescue/README.md):
#
#   rescue/kexec-into-rescue.sh
#   rescue/<arch>/manifest.json, vmlinuz, initrd.zst, kexec, ...
#   rescue/v/<version>/<arch>/...          (kept for rollbacks)
#   install/install.sh
#   profiles/default.yaml, profiles/layouts.yaml
#   os/<os>/<arch>/manifest.json, rootfs.tar.zst, ...
#   os/<os>/v/<version>/<arch>/...         (kept for rollbacks)
#
# Per image, manifest.json is uploaded last, so a reader never sees a
# manifest that points to files which are not there yet.
#
# Usage: upload.sh [rescue|os|scripts|profiles]...     (default: everything that is built)
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
[ ${#what[@]} -gt 0 ] || what=(rescue os scripts profiles)
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
	*.txt|*.sh|SHA256SUMS) echo text/plain ;;
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
		for d in "$HERE"/ubuntu/out/*/; do
			[ -f "$d/manifest.json" ] || continue
			name="$(sed -n 's/.*"os": "\([^"]*\)".*/\1/p' "$d/manifest.json" | head -n 1)"
			[ -n "$name" ] || { echo "upload.sh: no os name in $d/manifest.json" >&2; exit 2; }
			publish_image "${d%/}" os "$name"; built=1
		done
		;;
	scripts)
		echo "==> uploading the scripts"
		put "$HERE/rescue/kexec-into-rescue.sh" "$(key rescue/kexec-into-rescue.sh)"
		put "$HERE/../install/install.sh" "$(key install/install.sh)"
		built=1
		;;
	profiles)
		echo "==> uploading the profiles"
		for f in "$HERE"/../profiles/*.yaml; do put "$f" "$(key "profiles/$(basename "$f")")"; done
		built=1
		;;
	*) echo "upload.sh: unknown item $w (rescue, os, scripts, profiles)" >&2; exit 2 ;;
	esac
done
[ "$built" = 1 ] || { echo "upload.sh: nothing to upload; build an image first (make rescue-image, make os-image)" >&2; exit 2; }
echo "==> done: the bucket's public base URL${PREFIX:+/$PREFIX} is the storage (ump's storage setting)"
