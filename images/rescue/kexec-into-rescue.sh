#!/usr/bin/env bash
# Boot the UMP rescue image on the machine this runs on: download kernel,
# initramfs (and a static kexec) from a base URL, verify the checksums
# from manifest.json, carry the current network configuration, hostname
# and root's authorized keys over on the kernel command line, and kexec.
#
# Run as root in a provider's rescue system, or in any Linux that is
# about to be reinstalled. This is what `ump install` will do itself;
# until then it is the manual way.
#
# Usage: kexec-into-rescue.sh -u https://example.net/ump/rescue [-a amd64|arm64]
#            [-k AUTHORIZED_KEYS_FILE] [-H HOSTNAME] [-i IFACE] [-c EXTRA_CMDLINE]
#            [--no-exec] [--dry-run]
set -euo pipefail

url="${UMP_RESCUE_URL:-}"
arch=""
keys="/root/.ssh/authorized_keys"
host=""
iface=""
extra=""
exec_it=1
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	-u|--url) url="$2"; shift 2 ;;
	-a|--arch) arch="$2"; shift 2 ;;
	-k|--keys) keys="$2"; shift 2 ;;
	-H|--hostname) host="$2"; shift 2 ;;
	-i|--iface) iface="$2"; shift 2 ;;
	-c|--cmdline) extra="$2"; shift 2 ;;
	--no-exec) exec_it=0; shift ;;
	--dry-run) dry=1; shift ;;
	-h|--help) sed -n '2,16p' "$0"; exit 0 ;;
	*) echo "unknown argument $1" >&2; exit 2 ;;
	esac
done
[ -n "$url" ] || { echo "kexec-into-rescue.sh: the base URL is required (-u or UMP_RESCUE_URL)" >&2; exit 2; }
if [ -z "$arch" ]; then
	case "$(uname -m)" in
	x86_64) arch=amd64 ;;
	aarch64) arch=arm64 ;;
	*) echo "unsupported machine $(uname -m)" >&2; exit 2 ;;
	esac
fi
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }

dir="/tmp/ump-rescue-$arch"
mkdir -p "$dir"
fetch() {
	if command -v curl >/dev/null; then curl -fsSL -o "$2" "$1"; else wget -q -O "$2" "$1"; fi
}
echo "==> downloading the rescue image for $arch from $url"
fetch "$url/$arch/manifest.json" "$dir/manifest.json"
for f in vmlinuz initrd.zst; do fetch "$url/$arch/$f" "$dir/$f"; done
fetch "$url/$arch/kexec" "$dir/kexec" 2>/dev/null && chmod +x "$dir/kexec" || rm -f "$dir/kexec"

echo "==> verifying checksums"
sum() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
for f in vmlinuz initrd.zst $([ -f "$dir/kexec" ] && echo kexec); do
	want="$(sed -n "s/.*\"$f\": {\"sha256\": \"\([0-9a-f]*\)\".*/\1/p" "$dir/manifest.json" | head -n 1)"
	have="$(sum "$dir/$f")"
	if [ -z "$want" ] || [ "$want" != "$have" ]; then
		echo "checksum mismatch for $f: manifest $want, file $have" >&2
		exit 1
	fi
done

echo "==> reading the network configuration"
if [ -z "$iface" ]; then
	iface="$(ip -o route show default 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") print $(i + 1)}' | head -n 1)"
fi
[ -n "$iface" ] || { echo "no default route; give the interface with -i" >&2; exit 1; }
mac="$(cat "/sys/class/net/$iface/address")"
first() { awk "$1" | head -n 1; }
ip4="$( { ip -o -4 addr show dev "$iface" scope global 2>/dev/null || true; } | first '{print $4}')"
gw4="$( { ip -o -4 route show default dev "$iface" 2>/dev/null || true; } | first '{print $3}')"
ip6="$( { ip -o -6 addr show dev "$iface" scope global 2>/dev/null || true; } | first '$0 !~ /deprecated|tentative|temporary/ {print $4}')"
gw6="$( { ip -o -6 route show default dev "$iface" 2>/dev/null || true; } | first '{print $3}')"
dns="$( { awk '/^nameserver/ {print $2}' /etc/resolv.conf 2>/dev/null || true; } | grep -v '^127\.' | paste -sd, - || true)"
if [ -z "$dns" ] && command -v resolvectl >/dev/null; then
	dns="$(resolvectl dns "$iface" 2>/dev/null | sed 's/^[^:]*: *//' | tr ' ' ',' | sed 's/,*$//')"
fi
[ -n "$dns" ] || dns="1.1.1.1,9.9.9.9"
[ -n "$host" ] || host="$(hostname -s 2>/dev/null || echo ump-rescue)"

# The running kernel knows which consoles this machine has (the provider's
# rescue system was booted with the right ones); keep them, so that the
# rescue image talks to the same serial console.
consoles="$(tr ' ' '\n' < /proc/cmdline | grep '^console=' | paste -sd' ' - || true)"
[ -n "$consoles" ] || consoles="console=tty0 console=ttyS0,115200n8"

cmdline="$consoles ump.hostname=$host ump.mac=$mac"
[ -n "$ip4" ] && cmdline="$cmdline ump.ip=$ip4${gw4:+,$gw4}"
[ -n "$ip6" ] && cmdline="$cmdline ump.ip6=$ip6${gw6:+,$gw6}"
cmdline="$cmdline ump.dns=$dns"
if [ -f "$keys" ]; then
	cmdline="$cmdline ump.authorized_keys=$(base64 < "$keys" | tr -d '\n')"
else
	echo "warning: $keys does not exist; the rescue system will only be reachable on the console" >&2
fi
[ -n "$extra" ] && cmdline="$cmdline $extra"

echo "==> interface $iface ($mac): $ip4 via ${gw4:-?}${ip6:+, $ip6 via ${gw6:-?}}; DNS $dns; hostname $host"
if [ "$dry" = 1 ]; then
	echo "kernel:  $dir/vmlinuz"
	echo "initrd:  $dir/initrd.zst"
	echo "cmdline: $cmdline"
	exit 0
fi

kexec_bin="$dir/kexec"
[ -x "$kexec_bin" ] || kexec_bin="$(command -v kexec || true)"
[ -n "$kexec_bin" ] || { echo "no kexec available: install kexec-tools or publish the static kexec with the image" >&2; exit 1; }
echo "==> loading the rescue image"
"$kexec_bin" -l "$dir/vmlinuz" --initrd="$dir/initrd.zst" --command-line="$cmdline"
if [ "$exec_it" = 0 ]; then
	echo "loaded; run '$kexec_bin -e' to boot it"
	exit 0
fi
echo "==> booting into the UMP rescue system; reconnect as root@${ip4%%/*}"
sync
"$kexec_bin" -e
