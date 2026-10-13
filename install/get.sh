#!/bin/sh
# Install ump, the Unus Mundus Provisioner, from its storage:
#
#   curl -fsSL https://ump.zhurko.de/get.sh | sh
#
# Downloads the binary for this machine (Linux or macOS, amd64 or arm64)
# from bin/latest/ of the storage, checks it against SHA256SUMS and puts
# it into /usr/local/bin (with sudo when needed), else ~/.local/bin.
#
#   UMP_VERSION=v1.2.3    a version from bin/v/ instead of the latest
#   UMP_INSTALL_DIR=DIR   where the binary goes
#   UMP_STORAGE=URL       another storage (see images/rescue/README.md)
#
# Windows: take ump-windows-amd64.exe from bin/latest/ by hand.
set -eu

storage="${UMP_STORAGE:-https://ump.zhurko.de}"
storage="${storage%/}"
version="${UMP_VERSION:-latest}"

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) echo "get.sh: $(uname -s) is not Linux or macOS; the binaries are at $storage/bin/latest/" >&2; exit 2 ;;
esac
case "$(uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) echo "get.sh: no ump binary for $(uname -m)" >&2; exit 2 ;;
esac

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	echo "get.sh: neither curl nor wget found" >&2; exit 2
fi
if command -v sha256sum >/dev/null 2>&1; then
	sum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	sum() { echo skip; }
fi

base="$storage/bin/latest"
[ "$version" = latest ] || base="$storage/bin/v/$version"
name="ump-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> downloading $base/$name"
fetch "$base/$name" "$tmp/ump" || { echo "get.sh: download failed: $base/$name" >&2; exit 1; }
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || { echo "get.sh: download failed: $base/SHA256SUMS" >&2; exit 1; }
want="$(grep " $name\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
have="$(sum "$tmp/ump")"
if [ -z "$want" ]; then
	echo "get.sh: $name is not in SHA256SUMS" >&2; exit 1
elif [ "$have" != skip ] && [ "$have" != "$want" ]; then
	echo "get.sh: checksum mismatch for $name (expected $want, got $have)" >&2; exit 1
fi
chmod 755 "$tmp/ump"

dir="${UMP_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
	dir=/usr/local/bin
	if [ ! -w "$dir" ] && ! command -v sudo >/dev/null 2>&1; then
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir" 2>/dev/null || true
if [ -w "$dir" ]; then
	mv "$tmp/ump" "$dir/ump"
else
	echo "==> installing into $dir with sudo"
	sudo install -m 755 "$tmp/ump" "$dir/ump"
fi
echo "==> installed $("$dir/ump" version) into $dir/ump"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "    $dir is not in your PATH; add it, or run $dir/ump" ;;
esac
echo "    next: ump config isp (an account), then ump server list"
