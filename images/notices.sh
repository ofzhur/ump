#!/usr/bin/env bash
# Write THIRD_PARTY_NOTICES: the license (and NOTICE, PATENTS) of every
# Go module compiled into ump, for any of the platforms it is built for.
# Usage: images/notices.sh [OUTPUT]  (default: stdout)
set -euo pipefail
cd "$(dirname "$0")/.."
out="${1:-/dev/stdout}"
mods="$(for os in linux darwin windows; do GOOS=$os go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}' ./cmd/ump; done | sort -u)"
{
	echo "UMP includes the following third-party Go modules, each under its own license."
	while read -r path version; do
		[ -n "$path" ] || continue
		dir="$(go mod download -json "$path@$version" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",*$/\1/p')"
		echo
		echo "================================================================"
		echo "$path $version"
		echo "================================================================"
		for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/NOTICE* "$dir"/PATENTS*; do
			[ -f "$f" ] || continue
			echo
			echo "--- $(basename "$f")"
			echo
			cat "$f"
		done
	done <<<"$mods"
} >"$out"
