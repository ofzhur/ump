#!/usr/bin/env bash
# Publish the built rescue images and the entry script to an S3-compatible store
# (or any S3-compatible store): everything ump and the manual path fetch
# from the image's base URL. Per architecture, the files go to
# <prefix>/<arch>/ (the current image) and to <prefix>/v/<version>/<arch>/
# (kept for rollbacks); manifest.json is uploaded last, so a reader never
# sees a manifest that points to files which are not there yet.
# kexec-into-rescue.sh goes to <prefix>/kexec-into-rescue.sh.
#
# Usage: upload.sh [amd64|arm64]...     (default: every architecture built in out/)
#
# Environment:
#   STORE_ACCOUNT_ID         account ID (makes the endpoint
#                         https://<id>.storage.example.net), or
#   STORE_ENDPOINT           the S3 endpoint URL directly
#   STORE_ACCESS_KEY_ID      an API token with object read and write
#   STORE_SECRET_ACCESS_KEY
#   STORE_BUCKET             the bucket
#   STORE_PREFIX             key prefix inside the bucket (default: rescue)
#   DRY_RUN=1             print what would be uploaded
#
# Needs the AWS CLI (aws) or rclone.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out}"
PREFIX="${STORE_PREFIX:-rescue}"
DRY_RUN="${DRY_RUN:-0}"

arches=("$@")
if [ ${#arches[@]} -eq 0 ]; then
	for d in "$OUT"/*/; do
		[ -f "$d/manifest.json" ] && arches+=("$(basename "$d")")
	done
fi
[ ${#arches[@]} -gt 0 ] || { echo "upload.sh: no image built in $OUT; run make rescue-image RESCUE_ARCH=amd64|arm64 first" >&2; exit 2; }
for arch in "${arches[@]}"; do
	[ -f "$OUT/$arch/manifest.json" ] || { echo "upload.sh: no image in $OUT/$arch; run make rescue-image RESCUE_ARCH=$arch first" >&2; exit 2; }
done
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
	*) echo application/octet-stream ;;
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
	aws)
		AWS_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}" \
			aws s3 cp --only-show-errors --endpoint-url "$endpoint" --content-type "$type" "$file" "s3://$STORE_BUCKET/$key"
		;;
	rclone)
		RCLONE_CONFIG_UPLOAD_TYPE=s3 RCLONE_CONFIG_UPLOAD_PROVIDER=Other RCLONE_CONFIG_UPLOAD_ENDPOINT="$endpoint" \
			RCLONE_CONFIG_UPLOAD_ACCESS_KEY_ID="$STORE_ACCESS_KEY_ID" RCLONE_CONFIG_UPLOAD_SECRET_ACCESS_KEY="$STORE_SECRET_ACCESS_KEY" \
			rclone copyto --s3-no-check-bucket "$file" "upload:$STORE_BUCKET/$key"
		;;
	esac
	echo "uploaded s3://$STORE_BUCKET/$key"
}

for arch in "${arches[@]}"; do
	dir="$OUT/$arch"
	version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$dir/manifest.json" | head -n 1)"
	[ -n "$version" ] || { echo "upload.sh: no version in $dir/manifest.json" >&2; exit 2; }
	files=(vmlinuz initrd.zst SHA256SUMS packages.txt)
	[ -f "$dir/kexec" ] && files+=(kexec)
	echo "==> uploading the $arch image, version $version, to $endpoint bucket $STORE_BUCKET"
	for dest in "$PREFIX/v/$version/$arch" "$PREFIX/$arch"; do
		for f in "${files[@]}"; do
			put "$dir/$f" "$dest/$f"
		done
		put "$dir/manifest.json" "$dest/manifest.json"
	done
done
echo "==> uploading the entry script"
put "$HERE/kexec-into-rescue.sh" "$PREFIX/kexec-into-rescue.sh"
echo "==> done: <public base URL>/$PREFIX is the base URL (staging.image); the current images are ${arches[*]}"
