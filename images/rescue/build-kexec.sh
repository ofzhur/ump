#!/usr/bin/env bash
# Build a static kexec binary, so that a provider rescue system without
# kexec-tools can still kexec into the UMP rescue image. Output: $OUT/kexec.
# Another architecture than the build host's is cross-compiled
# (crossbuild-essential-<arch> and the multiarch zlib/lzma dev packages).
set -euo pipefail
ARCH="${1:-$(dpkg --print-architecture)}"
KEXEC_VERSION="${KEXEC_VERSION:-2.0.31}"
OUT="${OUT:-$(cd "$(dirname "$0")" && pwd)/out/$ARCH}"
WORK="$(mktemp -d /tmp/ump-kexec.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"
cd "$WORK"
url="https://www.kernel.org/pub/linux/utils/kernel/kexec/kexec-tools-$KEXEC_VERSION.tar.xz"
curl -fsSL -o kexec-tools.tar.xz "$url"
tar -xf kexec-tools.tar.xz
cd "kexec-tools-$KEXEC_VERSION"
configure_opts=()
strip_bin=strip
if [ "$ARCH" != "$(dpkg --print-architecture)" ]; then
	case "$ARCH" in
	amd64) triplet=x86_64-linux-gnu ;;
	arm64) triplet=aarch64-linux-gnu ;;
	*) echo "build-kexec.sh: no cross toolchain known for $ARCH" >&2; exit 2 ;;
	esac
	configure_opts=(--host="$triplet" CC="$triplet-gcc")
	strip_bin="$triplet-strip"
fi
./configure --quiet "${configure_opts[@]}" LDFLAGS="-static" CFLAGS="-O2" >/dev/null
make -s -j"$(nproc)"
"$strip_bin" build/sbin/kexec
cp build/sbin/kexec "$OUT/kexec"
echo "kexec-tools $KEXEC_VERSION, static, for $ARCH: $OUT/kexec"
