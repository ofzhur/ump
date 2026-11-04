#!/usr/bin/env bash
# Check the network configuration install/install.sh writes, without a
# machine: the functions of its network section (net_from_spec,
# net_from_machine and the renderers) run for machines of this script's
# own, and what they write for netplan (Ubuntu, Debian), systemd-networkd
# (Arch), systemd-resolved, UMP's NixOS module (facts.json) and the
# install record is compared with testdata/network/CASE. The cases
# without an uplink in the specification are what every provider gets,
# and their files are the ones ump has always written, byte for byte;
# the others are a specification's uplink (network.uplink, a PXE
# machine's) with static, dhcp and none, and those it refuses. The plan
# line comes along.
#
# Usage: test-network.sh [CASE...]   (default: every case)
# Needs bash 4 or later and jq. VERBOSE=1 names every case; UPDATE=1 writes the
# expected files instead of comparing (review the diff).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
EXPECTED="$HERE/testdata/network"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-network-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
# the section is install.sh's, which runs under the rescue system's bash
# 5; bash 3.2 (macOS's /bin/bash) misreads it
[ "${BASH_VERSINFO[0]}" -ge 4 ] || { echo "test-network.sh: bash 4 or later is needed (this is $BASH_VERSION)" >&2; exit 2; }
command -v jq >/dev/null || { echo "test-network.sh: jq is needed" >&2; exit 2; }

# what the section calls of the rest of install.sh
die() { echo "install.sh: $*"; exit 1; }
warn() { echo "install.sh: warning: $*"; }
section="$(awk '/^# --- the network: /{on = 1; next} on && /^# --- /{exit} on' "$ROOT/install/install.sh")"
[ -n "$section" ] || { echo "test-network.sh: no network section in install/install.sh" >&2; exit 2; }
eval "$section"

# the machines: two wired interfaces; the uplink is the second, and its
# MAC the one the rescue system found
machine() { # machine RENAME IPV6
	rename=$1; ipv6=$2
	IF_MAC=(0c:c4:7a:aa:bb:10 0c:c4:7a:aa:bb:11); IF_KIND=(int ext)
	if [ "$rename" = yes ]; then IF_NAME=(int01 ext01); uplink=ext01; else IF_NAME=(eno1 eno2); uplink=eno2; fi
	mac=0c:c4:7a:aa:bb:11
}
# render CASE: the files of the case under $WORK/CASE, and their dump
render() {
	local root="$WORK/$1" f
	mkdir -p "$root/etc/netplan" "$root/etc/systemd/network" "$root/etc/systemd/resolved.conf.d" "$root/etc/nixos/ump" "$root/etc/ump"
	if [ -n "$uplink_spec" ]; then net_from_spec "$uplink_spec"; else net_from_machine; fi
	netplan_yaml > "$root/etc/netplan/10-ump.yaml"
	networkd_files "$root/etc/systemd/network"
	resolved_conf > "$root/etc/systemd/resolved.conf.d/ump.conf"
	nixos_net_facts > "$root/etc/nixos/ump/facts.network.json"
	[ -z "$uplink_spec" ] || net_record > "$root/etc/ump/install.network"
	net_plan > "$root/plan"
	(cd "$root" && find . -type f | LC_ALL=C sort | while read -r f; do echo "==> ${f#./}"; cat "$f"; done)
}
cases() {
	legacy static yes false "203.0.113.10/26,203.0.113.1" "2001:db8::2/64,fe80::1"
	legacy static-ipv6 yes true "203.0.113.10/26,203.0.113.1" "2001:db8::2/64,fe80::1"
	legacy static-v4 yes false "203.0.113.10/26,203.0.113.1" ""
	legacy static-nogateway yes true "203.0.113.10/26" "2001:db8::2/64"
	legacy dhcp yes false "" ""
	legacy dhcp-ipv6 yes true "" ""
	legacy kernel-names no false "203.0.113.10/26,203.0.113.1" ""
	legacy kernel-names-dhcp no true "" ""
	spec uplink-static yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24", "gateway": "192.0.2.1"}, "ipv6": {"mode": "none"}, "dns": ["192.0.2.53", "192.0.2.54"]}'
	spec uplink-static-public-dns no false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24"}, "ipv6": {"mode": "none"}}'
	spec uplink-static-ipv6 yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24", "gateway": "192.0.2.1"}, "ipv6": {"mode": "static", "address": "2001:db8::17/64", "gateway": "fe80::1"}, "dns": ["192.0.2.53", "2001:db8::53"]}'
	spec uplink-dhcp yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}, "dns_from_dhcp": true}'
	spec uplink-dhcp-dns yes false '{"mac": "0C-C4-7A-AA-BB-11", "ipv4": {"mode": "dhcp"}, "dns": ["192.0.2.53"]}'
	spec uplink-dhcp-dns-kernel-names no true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns": ["192.0.2.53", "2001:db8::53"]}'
	spec uplink-dhcp-public-dns no false '{"ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}}'
	spec uplink-dhcp-auto yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	spec uplink-dhcp-auto-kernel-off yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	spec uplink-none-static6 yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "static", "address": "2001:db8::17/64", "gateway": "fe80::1"}, "dns": ["2001:db8::53"]}'
	spec uplink-none-auto yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	spec refused-none yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "none"}}'
	spec refused-none-kernel-off yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "auto"}}'
	spec refused-mode yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "auto"}}'
	spec refused-prefix yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17"}}'
	spec refused-lease yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24"}, "dns_from_dhcp": true}'
	spec refused-mac yes false '{"mac": "0c:c4:7a:aa:bb", "ipv4": {"mode": "dhcp"}}'
}

failed=0 n=0
run() { # run NAME ARGS...: one case, unless the command line names others
	local name="$2"
	if [ ${#only[@]} -gt 0 ] && ! printf '%s\n' "${only[@]}" | grep -qxF "$name"; then return 0; fi
	n=$((n + 1))
	local out="$WORK/$name.out" rc
	# errexit as in install.sh: a command that fails there fails here
	set +e
	( set -e; "$@" ) > "$out" 2>&1
	rc=$?
	set -e
	[ "$rc" = 0 ] || echo "(exit status $rc)" >> "$out"
	if [ "${UPDATE:-0}" = 1 ]; then
		cp "$out" "$EXPECTED/$name"
		echo "wrote   $name"
	elif [ ! -f "$EXPECTED/$name" ]; then
		echo "MISSING $name (UPDATE=1 writes it)"; failed=$((failed + 1))
	elif diff -u "$EXPECTED/$name" "$out" > "$WORK/$name.diff"; then
		[ "${VERBOSE:-0}" = 0 ] || echo "ok      $name"
	else
		echo "FAIL    $name"; sed 's/^/        /' "$WORK/$name.diff"; failed=$((failed + 1))
	fi
}
# legacy NAME RENAME IPV6 IP4 IP6: what the rescue system found, no
# uplink in the specification; spec NAME RENAME IPV6 UPLINK: the
# specification's uplink (JSON)
legacy() { run legacy_case "$@"; }
spec() { run spec_case "$@"; }
legacy_case() { machine "$2" "$3"; ip4=$4; ip6=$5; uplink_spec=""; render "$1"; }
spec_case() { machine "$2" "$3"; ip4=""; ip6=""; uplink_spec=$4; render "$1"; }
only=("$@")
[ "${UPDATE:-0}" = 1 ] && mkdir -p "$EXPECTED"
cases
[ "$n" -gt 0 ] || { echo "test-network.sh: no such case: $*" >&2; exit 2; }
if [ "$failed" -gt 0 ]; then echo "$failed of $n cases failed"; exit 1; fi
echo "test-network.sh: all $n cases as expected"
