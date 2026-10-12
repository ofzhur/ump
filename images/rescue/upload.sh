#!/usr/bin/env bash
# Upload a built rescue image to an S3-compatible store (or any S3-compatible
# store). The files go to <prefix>/<arch>/ (what kexec-into-rescue.sh and
# ump fetch) and to <prefix>/v/<version>/<arch>/ (kept for rollbacks);
# manifest.json is uploaded last, so a reader never sees a manifest that
# points to files which are not there yet.
#
# Usage: upload.sh [amd64|arm64]        (uploads out/<arch>, see build.sh)
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

ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/$ARCH}"
PREFIX="${STORE_PREFIX:-rescue}"
DRY_RUN="${DRY_RUN:-0}"

[ -f "$OUT/manifest.json" ] || { echo "upload.sh: no image in $OUT; run build.sh $ARCH (or make rescue-image) first" >&2; exit 2; }
for v in STORE_ACCESS_KEY_ID STORE_SECRET_ACCESS_KEY STORE_BUCKET; do
	[ -n "${!v:-}" ] || { echo "upload.sh: $v is not set (see the environment section of images/rescue/README.md)" >&2; exit 2; }
done
endpoint="${STORE_ENDPOINT:-}"
if [ -z "$endpoint" ]; then
	[ -n "${STORE_ACCOUNT_ID:-}" ] || { echo "upload.sh: set STORE_ACCOUNT_ID or STORE_ENDPOINT" >&2; exit 2; }
	endpoint="https://${STORE_ACCOUNT_ID}.storage.example.net"
fi
version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$OUT/manifest.json" | head -n 1)"
[ -n "$version" ] || { echo "upload.sh: no version in $OUT/manifest.json" >&2; exit 2; }

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
	*.txt|SHA256SUMS) echo text/plain ;;
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

files=(vmlinuz initrd.zst SHA256SUMS packages.txt)
[ -f "$OUT/kexec" ] && files+=(kexec)
echo "==> uploading the $ARCH image, version $version, to $endpoint bucket $STORE_BUCKET"
for dest in "$PREFIX/v/$version/$ARCH" "$PREFIX/$ARCH"; do
	for f in "${files[@]}"; do
		put "$OUT/$f" "$dest/$f"
	done
	put "$OUT/manifest.json" "$dest/manifest.json"
done
echo "==> done: <public base URL>/$PREFIX/$ARCH/manifest.json is the current $ARCH image"
