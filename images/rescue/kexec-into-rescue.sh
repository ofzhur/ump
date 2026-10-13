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
#            [-k AUTHORIZED_KEYS_FILE] [-K PUBLIC_KEY]... [-H HOSTNAME] [-i IFACE]
#            [-d DNS,DNS] [-c EXTRA_CMDLINE] [--no-exec] [--dry-run]
set -euo pipefail

url="${UMP_RESCUE_URL:-}"
arch=""
keys="/root/.ssh/authorized_keys"
extra_keys=()
host=""
iface=""
dns="1.1.1.1,8.8.8.8"
extra=""
exec_it=1
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	-u|--url) url="$2"; shift 2 ;;
	-a|--arch) arch="$2"; shift 2 ;;
	-k|--keys) keys="$2"; shift 2 ;;
	-K|--key) extra_keys+=("$2"); shift 2 ;;
	-H|--hostname) host="$2"; shift 2 ;;
	-i|--iface) iface="$2"; shift 2 ;;
	-d|--dns) dns="$2"; shift 2 ;;
	-c|--cmdline) extra="$2"; shift 2 ;;
	--no-exec) exec_it=0; shift ;;
	--dry-run) dry=1; shift ;;
	-h|--help) sed -n '2,/^set -euo/{/^set -euo/!p;}' "$0"; exit 0 ;;
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

# /var/tmp, not /tmp: on systems with /tmp in RAM (Ubuntu since 25.04) the
# download would hold 100 MB of memory that the kexec needs
dir="/var/tmp/ump-rescue-$arch"
mkdir -p "$dir"
fetch() { # fetch URL FILE: download, and say what went wrong when it did not
	if command -v curl >/dev/null; then
		curl -fsSL -o "$2" "$1" && return 0
		status="$(curl -sL -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || true)"
		echo "download failed: $1 (HTTP ${status:-?})" >&2
		if curl -sIL "$1" 2>/dev/null | grep -qiE '^(content-type: text/html)'; then
			echo "the storage answered with a web page instead of the file: a bot protection or a login page in front of it must let scripted clients through (see the README)" >&2
		fi
	else
		wget -q -O "$2" "$1" && return 0
		echo "download failed: $1" >&2
	fi
	return 1
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
[ -n "$host" ] || host="$(hostname -s 2>/dev/null || echo ump-rescue)"
# the resolvers are the public ones, not the provider's, unless -d says
# otherwise; they go on the command line so that every image version
# gets them, not only those with the same default built in

# The running kernel knows which consoles this machine has (the provider's
# rescue system was booted with the right ones); keep them, so that the
# rescue image talks to the same serial console.
consoles="$(tr ' ' '\n' < /proc/cmdline | grep '^console=' | paste -sd' ' - || true)"
[ -n "$consoles" ] || consoles="console=tty0 console=ttyS0,115200n8"

cmdline="$consoles ump.hostname=$host ump.mac=$mac"
[ -n "$ip4" ] && cmdline="$cmdline ump.ip=$ip4${gw4:+,$gw4}"
[ -n "$ip6" ] && cmdline="$cmdline ump.ip6=$ip6${gw6:+,$gw6}"
cmdline="$cmdline ump.dns=$dns"
# root's keys of the current system (the provider's rescue system has UMP's
# there) plus the -K keys, without comments, blanks and duplicates
keylist="$( { [ -f "$keys" ] && cat "$keys"; printf '%s\n' ${extra_keys[@]+"${extra_keys[@]}"}; } 2>/dev/null | grep -v '^[[:space:]]*#' | grep '[^[:space:]]' | awk '!seen[$0]++' || true)"
if [ -n "$keylist" ]; then
	cmdline="$cmdline ump.authorized_keys=$(printf '%s\n' "$keylist" | base64 | tr -d '\n')"
else
	echo "warning: no authorized keys ($keys does not exist and no -K given); the rescue system will only be reachable on the console" >&2
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
# the published static kexec, unless it cannot run here (a noexec mount)
[ -x "$kexec_bin" ] && "$kexec_bin" --version >/dev/null 2>&1 || kexec_bin="$(command -v kexec || true)"
[ -n "$kexec_bin" ] || { echo "no kexec available: install kexec-tools or publish the static kexec with the image" >&2; exit 1; }
echo "==> loading the rescue image"
# kexec_file_load (-s) lets the kernel read the files itself, which is one
# copy less in memory than the classic kexec_load; that matters on a
# rescue system that runs from RAM and holds the download too. Older
# kernels and some firmware setups only take the classic call.
if ! err="$("$kexec_bin" -s -l "$dir/vmlinuz" --initrd="$dir/initrd.zst" --command-line="$cmdline" 2>&1)"; then
	echo "kexec_file_load not usable (${err:-no details}); using kexec_load" >&2
	"$kexec_bin" -l "$dir/vmlinuz" --initrd="$dir/initrd.zst" --command-line="$cmdline"
fi
# the kernel holds its own copy now; the downloads only cost memory
rm -f "$dir/vmlinuz" "$dir/initrd.zst"
if [ "$exec_it" = 0 ]; then
	echo "loaded; run '$kexec_bin -e' to boot it"
	exit 0
fi
# The jump does not shut the running system down. On a system that runs
# from disk the file systems are made consistent first, the way the
# kernel's own emergency path does it (sync, then an emergency read-only
# remount of every block-device file system, which commits the journals
# and needs no cooperation from the processes that keep writing); a rescue
# system in RAM has no such file systems and is left as it is.
quiesce() {
	sync
	[ -w /proc/sysrq-trigger ] || return 0
	echo s > /proc/sysrq-trigger
	echo u > /proc/sysrq-trigger
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		awk '$1 ~ /^\/dev\// && $4 ~ /^rw/ {found = 1} END {exit !found}' /proc/mounts 2>/dev/null || return 0
		sleep 1
	done
}
echo "==> booting into the UMP rescue system; reconnect as root@${ip4%%/*}"
quiesce
"$kexec_bin" -e
