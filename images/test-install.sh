#!/usr/bin/env bash
# Install an operating system with install/install.sh in QEMU and check
# the result: the UMP rescue image boots with UEFI firmware (or BIOS:
# FIRMWARE=bios, amd64), the installer
# runs with a specification for OS against a local storage (the built
# image or manifest from images/base/out, images/nixos/out or
# images/arch/out, and the
# installer's files from install/), then the disk boots and the new
# system is checked over SSH as the administrator: the system, the
# hostname, the interface names, sudo, the firewall, swap, a userdata
# entry's effect, the install record and the message of the day; on
# NixOS also a userdata module winning over a default, and a
# configuration that nixos-rebuild still evaluates. install.sh runs as
# ump runs it, with --spec alone; with an uplink, or RECORD=1, also with
# --record, as a PXE machine's runner does, and the record must hold the
# new system's host keys.
#
# FreeBSD (freebsd-15.1, freebsd-14.5) goes the whole chain of its
# installation, without ump: in the UMP rescue system install.sh
# --dry-run prints the plan and must leave every disk as it was, byte for
# byte; install.sh then stages (the disks wiped, the boot image of the
# UMP FreeBSD rescue system on them with its boot config) and ends with
# "==> staged:"; the firmware starts that system from the disk, and it
# must have its settings from the boot config (the hostname, the key,
# the address) and the plan as install.sh made it; the installer for
# FreeBSD runs there with the same specification, the machine reboots,
# and the new system is checked as the administrator: the system, the
# hostname, the root on the pool and the pool's devices, ext01, sudo,
# pf's rules, the userdata entry's mark, swap, the install record, the
# time zone and Python.
#
# Usage: test-install.sh OS [arm64|amd64]   (OS: debian-13, ubuntu-26.04,
#        nixos-25.11, archlinux, cachyos, freebsd-15.1, ...; the architecture defaults to this machine's)
# Needs the rescue image in images/rescue/out/<arch> and the image (or the
# NixOS manifest) built. Environment: MEM (MB, default 2048), DISK (GB,
# default 12), DISK2 (GB: a second disk, which the layouts span, as a
# Linode's swap disk; default none), PORT (default 2252), TIMEOUT (s),
# UPLINK (static, dhcp or dhcp-dns: the specification's uplink,
# network.uplink, as a PXE machine's has it, checked on the new system;
# dhcp-dns is DHCP with resolvers of its own instead of the lease's;
# default none, the uplink as the rescue system has it), RECORD (1:
# --record without an uplink too), FIRMWARE (uefi, or bios on amd64;
# default uefi), RESCUE (the rescue image's directory; default
# images/rescue/out/<arch>). For FreeBSD also: LAYOUT (zfs, zfs-mirror,
# zfs-raidz of profiles/layouts.yaml; default zfs), DISK3 (GB: a third
# disk), SPARE (GB: one more disk, full of data, which the specification
# leaves out, as --disks does, and which must be as it was at the end;
# every disk has a serial number then), SERIAL (1: serial numbers without
# SPARE too), SECOND (1: a second attempt: after the first staging the
# FreeBSD rescue system is started with a network card it has no driver
# for, so that nobody reaches it, and the next install.sh must show the
# boot log it left on the disk), FREEBSD (what images/freebsd builds,
# laid out as the storage has it: os/<os>/<arch> and
# rescue/<name>/<arch>; default images/freebsd/out) and
# FREEBSD_INSTALLER (default install/freebsd/install.sh). Emulated amd64
# runs FreeBSD with the CPU model qemu64: with newer ones its kernel
# stops while zfs.ko loads. The work directory, with the disks and the
# serial logs, is made under TMPDIR (default /tmp).
set -euo pipefail
OS="${1:?usage: test-install.sh OS [arm64|amd64]}"
ARCH="${2:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
RESCUE="${RESCUE:-$ROOT/images/rescue/out/$ARCH}"
FREEBSD="${FREEBSD:-$HERE/freebsd/out}"
case "$OS" in
freebsd-*) family=freebsd; IMAGE="$FREEBSD/os/$OS/$ARCH" ;;
nixos-*) family=nixos; IMAGE="$HERE/nixos/out/$OS/$ARCH" ;;
archlinux*|cachyos*) family=arch; IMAGE="$HERE/arch/out/$OS/$ARCH" ;;
*) family=image; IMAGE="$HERE/base/out/$OS/$ARCH" ;;
esac
MANIFEST="$IMAGE/manifest.json"
MEM="${MEM:-2048}"
UPLINK="${UPLINK:-}"
RECORD="${RECORD:-0}"; [ -z "$UPLINK" ] || RECORD=1
MAC=52:54:00:12:34:56
DISK="${DISK:-12}"
PORT="${PORT:-2252}"
HTTP=$((PORT + 1))
TIMEOUT="${TIMEOUT:-600}"
FIRMWARE="${FIRMWARE:-uefi}"
LAYOUT="${LAYOUT:-zfs}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-install-test.XXXXXX")"
qemu_pid="" http_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$http_pid" ] && kill "$http_pid" 2>/dev/null || true
	echo "work directory (serial logs): $WORK"
}
trap cleanup EXIT
[ -f "$RESCUE/vmlinuz" ] || { echo "no rescue image in $RESCUE" >&2; exit 2; }
[ -f "$MANIFEST" ] || { echo "no image at $IMAGE; build it first" >&2; exit 2; }

case "$ARCH" in
arm64)
	qemu=qemu-system-aarch64; machine=(-M virt -cpu cortex-a72); fw=edk2-aarch64-code.fd; vars=edk2-arm-vars.fd
	[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] && machine=(-M virt -accel hvf -cpu host)
	;;
amd64)
	qemu=qemu-system-x86_64; machine=(-M q35); fw=edk2-x86_64-code.fd; vars=edk2-i386-vars.fd
	# FreeBSD's kernel stops with a page fault while zfs.ko loads under
	# QEMU's newer CPU models (Haswell, max); with qemu64 it loads
	[ "$family" != freebsd ] || machine=(-M q35 -cpu qemu64)
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
case "$FIRMWARE" in
uefi) ;;
bios) [ "$ARCH" = amd64 ] || { echo "FIRMWARE=bios is for amd64" >&2; exit 2; } ;;
*) echo "FIRMWARE is uefi or bios" >&2; exit 2 ;;
esac
share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
cp "$share/$vars" "$WORK/vars.fd"
[ "$ARCH" = arm64 ] && truncate -s 64M "$WORK/vars.fd"
code="$WORK/code.fd"; cp "$share/$fw" "$code"; [ "$ARCH" = arm64 ] && truncate -s 64M "$code"
firmware=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=$WORK/vars.fd")
[ "$FIRMWARE" = uefi ] || firmware=()

# the storage: the image (or the manifest) and the installer's files
mkdir -p "$WORK/storage/os/$OS/$ARCH" "$WORK/storage/install/nixos"
cp "$IMAGE"/manifest.json "$WORK/storage/os/$OS/$ARCH/"
[ -f "$IMAGE/rootfs.tar.zst" ] && ln -s "$IMAGE/rootfs.tar.zst" "$WORK/storage/os/$OS/$ARCH/rootfs.tar.zst"
cp "$ROOT/install/nixos/ump.nix" "$WORK/storage/install/nixos/ump.nix"
cp "$ROOT/install/motd.sh" "$ROOT/install/bashrc.sh" "$WORK/storage/install/"
if [ "$family" = freebsd ]; then
	# FreeBSD's sets and packages, the rescue image its manifest names, and
	# the installer that runs there
	fb_rescue="$(jq -r '.rescue // empty' "$MANIFEST")"
	fb_installer="${FREEBSD_INSTALLER:-$ROOT/install/freebsd/install.sh}"
	[ -f "$FREEBSD/rescue/$fb_rescue/$ARCH/disk.img.zst" ] || { echo "no FreeBSD rescue image at $FREEBSD/rescue/$fb_rescue/$ARCH; build it first" >&2; exit 2; }
	[ -f "$fb_installer" ] || { echo "no installer for FreeBSD at $fb_installer" >&2; exit 2; }
	for f in base.txz kernel.txz packages.tar; do ln -s "$IMAGE/$f" "$WORK/storage/os/$OS/$ARCH/$f"; done
	mkdir -p "$WORK/storage/rescue/$fb_rescue/$ARCH" "$WORK/storage/install/freebsd"
	cp "$FREEBSD/rescue/$fb_rescue/$ARCH/manifest.json" "$WORK/storage/rescue/$fb_rescue/$ARCH/"
	ln -s "$FREEBSD/rescue/$fb_rescue/$ARCH/disk.img.zst" "$WORK/storage/rescue/$fb_rescue/$ARCH/disk.img.zst"
	cp "$fb_installer" "$WORK/storage/install/freebsd/install.sh"
fi
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/storage" "$HTTP" >/dev/null 2>&1 &
http_pid=$!

ssh-keygen -q -t ed25519 -N '' -C ump-install-test -f "$WORK/key"
pub="$(cat "$WORK/key.pub")"
# a userdata entry of the system's kind that leaves a mark: a NixOS module
# that also overrides a default (the time zone), or a bash script
if [ "$family" = nixos ]; then
	userdata="$(jq -n '[{name: "test", type: "nix", script: "{ pkgs, lib, ... }: {\n  environment.etc.\"ump-userdata-test\".text = \"merged\";\n  environment.systemPackages = [ pkgs.hello ];\n  time.timeZone = \"UTC\";\n}\n"}]')"
elif [ "$family" = freebsd ]; then
	userdata="$(jq -n '[{name: "test", type: "bash", script: "#!/usr/local/bin/bash\nset -e\necho merged > /etc/ump-userdata-test\nsysrc ump_userdata_test=YES\n"}]')"
else
	userdata="$(jq -n '[{name: "test", type: "bash", script: "#!/bin/bash\nset -e\necho merged > /etc/ump-userdata-test\n"}]')"
fi
# the uplink of a PXE machine: QEMU's user network, by DHCP with its
# resolver, or the same address static
case "$UPLINK" in
"") uplink=null ;;
dhcp) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "dhcp"}, ipv6: {mode: "none"}, dns_from_dhcp: true}')" ;;
dhcp-dns) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "dhcp"}, ipv6: {mode: "none"}, dns: ["192.0.2.53"]}')" ;;
static) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "static", address: "10.0.2.15/24", gateway: "10.0.2.2"}, ipv6: {mode: "none"}, dns: ["10.0.2.3"]}')" ;;
*) echo "UPLINK is static, dhcp or dhcp-dns" >&2; exit 2 ;;
esac
jq -n --arg key "$pub" --arg storage "http://10.0.2.2:$HTTP" --arg os "$OS" --argjson userdata "$userdata" --argjson uplink "$uplink" '{
	os: $os, hostname: "test.example.net", timezone: "Europe/Berlin", storage: $storage,
	kernel: {mitigations: false, ipv6: false, extra: ""},
	layout: {name: "lvm-linear", parts: [
		{type: "slice", target: ["first"], size: "1M", filesystem: "bios"},
		{type: "slice", target: ["first"], size: "512M", filesystem: "efi", mountpoint: "/boot/efi"},
		{type: "slice", target: ["first"], size: "1G", filesystem: "ext4", mountpoint: "/boot"},
		{type: "slice", target: [".*"], size: "rest", filesystem: "lvm", vg: "vg0"},
		{type: "lvm", vg: "vg0", name: "swap", size: "512M", filesystem: "swap"},
		{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: "ext4", mountpoint: "/"}]},
	disks: [],
	users: [{name: "ump", sudo: true, groups: ["adm"], shell: "/bin/bash", keys: [$key]}],
	userdata: $userdata,
	firewall: {policy: "drop", allow: [{proto: "icmp"}, {proto: "tcp", port: "22"}]},
	network: {rename: true}, profile: {name: "test"}} | if $uplink then .network.uplink = $uplink else . end' > "$WORK/spec.json"
if [ "$family" = freebsd ]; then
	[ -z "$UPLINK" ] || { echo "UPLINK is not checked on FreeBSD yet" >&2; exit 2; }
	# a published ZFS layout: its parts in profiles/layouts.yaml are YAML
	# flow mappings, one line each, which sed turns into JSON; and with
	# SPARE the disks to use, as --disks names them
	layout="$(awk -v n="  $LAYOUT:" '$0 == n {on = 1; next} on && /^  [a-z]/ {exit} on && /^      - \{/ {sub(/^      - /, ""); print}' "$ROOT/profiles/layouts.yaml" \
		| sed -E 's/([{,] *)([a-z]+): /\1"\2": /g; s/: ([^]["{},][^,}]*)/: "\1"/g; s/\[([a-z]+)\]/["\1"]/g' | jq -s -c --arg name "$LAYOUT" '{name: $name, parts: .}')"
	[ "$(jq '[.parts[] | select(.type == "zfs")] | length' <<< "$layout")" = 1 ] || { echo "LAYOUT=$LAYOUT is no ZFS layout of profiles/layouts.yaml" >&2; exit 2; }
	use='[]'
	if [ -n "${SPARE:-}" ]; then use='["/dev/vda"]'; [ -z "${DISK2:-}" ] || use='["/dev/vda", "/dev/vdb"]'; [ -z "${DISK3:-}" ] || use='["/dev/vda", "/dev/vdb", "/dev/vdc"]'; fi
	jq --argjson layout "$layout" --argjson use "$use" '.layout = $layout | .disks = $use' "$WORK/spec.json" > "$WORK/spec.json.new"
	mv "$WORK/spec.json.new" "$WORK/spec.json"
fi

ssh_opts=(-i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
	-o ConnectTimeout=5 -o ServerAliveInterval=10 -p "$PORT")
wait_ssh() { # wait_ssh USER
	local deadline=$(( $(date +%s) + TIMEOUT ))
	until ssh "${ssh_opts[@]}" "$1@127.0.0.1" true 2>/dev/null; do
		kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited"; tail -n 40 "$WORK/serial.log"; exit 1; }
		[ "$(date +%s)" -gt "$deadline" ] && { echo "no SSH as $1 within ${TIMEOUT}s"; tail -n 40 "$WORK/serial.log"; exit 1; }
		sleep 3
	done
}

# --- FreeBSD: from the UMP rescue system to the installed system ------------

# sums: the SHA-256 of every disk, as the host sees it once the machine
# wrote out what it had (sync)
# (openssl first: it reads a disk of 10 GB in seconds)
sha256() { if command -v openssl >/dev/null; then openssl dgst -sha256 -r; elif command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
sums() {
	local f
	ssh "${ssh_opts[@]}" root@127.0.0.1 sync
	for f in "${disks[@]}"; do sha256 < "$f"; done
}
# dry_run: install.sh --dry-run prints the plan, and no disk differs by a
# byte after it
dry_run() {
	local before
	before="$(sums)"
	ssh "${ssh_opts[@]}" root@127.0.0.1 "bash /tmp/install.sh --spec /tmp/spec.json --dry-run" > "$WORK/dry-run.log" 2>&1 || { echo "the dry run failed"; cat "$WORK/dry-run.log"; exit 1; }
	grep '^==>\|install.sh' "$WORK/dry-run.log" | grep -v '^==> \(users\|userdata\|interfaces\):' || true
	grep -q '^==> dry run: stopping here, no disk was touched' "$WORK/dry-run.log" || { echo "the dry run did not end as one"; exit 1; }
	[ "$(sums)" = "$before" ] || { echo "FAILED: the dry run changed a disk"; exit 1; }
	echo "dry run: every disk as it was, byte for byte (sha256 over ${#disks[@]} whole disk(s))"
}
# stage: install.sh, which wipes the disks and puts the FreeBSD rescue
# system on them
stage() {
	local start; start=$(date +%s)
	ssh "${ssh_opts[@]}" root@127.0.0.1 "bash /tmp/install.sh --spec /tmp/spec.json" 2>&1 | tee "$WORK/install.log" | grep '^==>\|install.sh' || true
	grep -q '^==> staged: ' "$WORK/install.log" || { echo "the staging failed"; tail -n 30 "$WORK/install.log"; exit 1; }
	! grep -q '^==> installed: ' "$WORK/install.log" || { echo "install.sh says installed, not staged"; exit 1; }
	echo "==> staged in $(( $(date +%s) - start ))s"
}
freebsd_chain() {
	local spare_sum="" start chain plan release
	chain=$(date +%s)
	[ -z "${SPARE:-}" ] || spare_sum="$(sha256 < "$WORK/spare.img")"
	echo "==> install.sh --spec /tmp/spec.json --dry-run ($OS, layout $LAYOUT)"
	dry_run
	if [ "${SECOND:-0}" = 1 ]; then
		echo "==> a first attempt: install.sh --spec /tmp/spec.json"
		stage
		stop_qemu
		# (a card in the same place: the firmware remembers where its disk is)
		echo "==> booting the disk with a network card FreeBSD has no driver for (ne2k_pci): its rescue system comes up, and nobody reaches it"
		start_qemu "$WORK/serial-first.log" -netdev user,id=n0 -device "ne2k_pci,netdev=n0,mac=$MAC"
		start=$(date +%s)
		until grep -q -a '^login:' "$WORK/serial-first.log" 2>/dev/null; do
			kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited"; tail -n 40 "$WORK/serial-first.log"; exit 1; }
			[ $(( $(date +%s) - start )) -lt "$TIMEOUT" ] || { echo "the FreeBSD rescue system did not come as far as its login prompt within ${TIMEOUT}s"; tail -n 40 "$WORK/serial-first.log"; exit 1; }
			sleep 2
		done
		echo "    at its login prompt after $(( $(date +%s) - start ))s, without a network; switched off"
		stop_qemu
		echo "==> the second attempt: the rescue image again, and install.sh --dry-run"
		start_rescue
		dry_run
		grep -q '^==> a FreeBSD rescue system was here and left its boot log' "$WORK/dry-run.log" || { echo "FAILED: install.sh did not show the boot log of the first attempt"; exit 1; }
		grep -q '^    UMP FreeBSD rescue system .*, booted ' "$WORK/dry-run.log" || { echo "FAILED: what install.sh showed is not the boot log"; exit 1; }
		echo "boot log of the first attempt, read back from the disk: $(grep -c '^    ' "$WORK/dry-run.log") lines; the first: $(grep -m 1 '^    UMP FreeBSD rescue system' "$WORK/dry-run.log" | sed 's/^ *//')"
	fi
	echo "==> install.sh --spec /tmp/spec.json"
	stage
	plan="$(ssh "${ssh_opts[@]}" root@127.0.0.1 'p="$(lsblk -lnpo NAME,PARTLABEL /dev/vda | awk "\$2 == \"umpboot\" {print \$1}")"; mount -o ro "$p" /mnt && cat /mnt/ump/plan.json && umount /mnt')"
	[ "$(jq -r '.version' <<< "$plan")" = 1 ] || { echo "FAILED: no plan.json on the partition umpboot"; exit 1; }
	echo "plan.json: $(jq -r '"\(.firmware), boot disk \(.boot_disk), \([.disks[] | "\(.name) (\(.size) bytes\(if .serial != "" then ", " + .serial else "" end): \([.parts[] | "\(.index) \(.kind) \(.size)"] | join(", ")))"] | join("; ")); left alone: \([.left[].name] | join(" ") | if . == "" then "none" else . end); pool \(.pool.name), \(.pool.vdev), at \(.pool.mountpoint)"' <<< "$plan")"
	ssh "${ssh_opts[@]}" root@127.0.0.1 sync
	stop_qemu
	echo "==> booting the disk: the UMP FreeBSD rescue system"
	start=$(date +%s)
	start_qemu "$WORK/serial-freebsd.log" "${net[@]}"
	cp /dev/null "$WORK/serial.log"; ln -sf "$WORK/serial-freebsd.log" "$WORK/serial.log"
	wait_ssh root
	release="$(sed -n 's/.*"release": "\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)"
	ssh "${ssh_opts[@]}" root@127.0.0.1 "env RELEASE='$release' PLAN='$(jq -cS . <<< "$plan")' sh -s" <<'RESCUE'
set -e
ok() { what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
ok "rescue system: ump-rescue $(cat /etc/ump-rescue-version), $(uname -sr) $(uname -m)" [ "$(uname -s) $(uname -r)" = "FreeBSD $RELEASE" ]
ok "rescue system: its root in memory ($(mount -p | awk '$2 == "/" {print $1}')), no disk mounted" sh -c 'mount -p | awk "\$2 == \"/\" {print \$1}" | grep -q "^/dev/md" && ! mount -p | grep -Eq "^/dev/(vtbd|ada|da|nda)"'
ok "boot config: hostname $(hostname) from ump/cmdline" [ "$(hostname)" = rescue ]
ok "boot config: $(tr ' ' '\n' < /etc/ump/boot/cmdline | grep -E '^ump\.(mac|ip|dns)=' | paste -sd' ' -), and the address is there ($(ifconfig | awk '$1 == "inet" && $2 != "127.0.0.1" {print $2}'))" sh -c 'a="$(tr " " "\n" < /etc/ump/boot/cmdline | sed -n "s/^ump\.ip=\([^/,]*\).*/\1/p")"; [ -n "$a" ] && ifconfig | grep -q "inet $a "'
ok "boot config: plan.json as install.sh wrote it" [ "$(jq -cS . /etc/ump/boot/plan.json)" = "$PLAN" ]
ok "zfs: the module is loaded" kldstat -q -m zfs
RESCUE
	echo "    up $(( $(date +%s) - start ))s after power-on"
	echo "==> the installer for FreeBSD: install.sh --spec /tmp/ump-install.json$([ "$RECORD" != 1 ] || echo " --record /tmp/ump-record")"
	start=$(date +%s)
	ssh "${ssh_opts[@]}" root@127.0.0.1 "cat > /tmp/ump-install.json && fetch -qo /tmp/ump-install.sh http://10.0.2.2:$HTTP/install/freebsd/install.sh && sh /tmp/ump-install.sh --spec /tmp/ump-install.json$([ "$RECORD" != 1 ] || echo " --record /tmp/ump-record")" \
		< "$WORK/spec.json" 2>&1 | tee "$WORK/install-freebsd.log" | grep '^==>\|install.sh' || true
	grep -q "^==> installed: $OS" "$WORK/install-freebsd.log" || { echo "the installation failed"; tail -n 30 "$WORK/install-freebsd.log"; exit 1; }
	if [ "$RECORD" = 1 ]; then
		mkdir -p "$WORK/record"
		scp "${scp_opts[@]}" "root@127.0.0.1:/tmp/ump-record/*" "$WORK/record/"
	fi
	echo "==> installed in $(( $(date +%s) - start ))s; rebooting"
	start=$(date +%s)
	ssh "${ssh_opts[@]}" root@127.0.0.1 'shutdown -r now' >/dev/null 2>&1 || true
	sleep 5
	wait_ssh ump
	echo "==> the new system answers $(( $(date +%s) - start ))s after the reboot; checking it"
	ssh "${ssh_opts[@]}" ump@127.0.0.1 "env OS=$OS RELEASE='$release' LAYOUT=$LAYOUT NDISKS=$(jq '.disks | length' <<< "$plan") ARCH=$ARCH RECORD_KEY='$(cat "$WORK"/record/ssh_host_ed25519_key.pub 2>/dev/null | awk '{print $2}')' sh -s" <<'CHECK'
set -e
# ok WHAT COMMAND...: say so, or fail the run
ok() { what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
pretty="$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')"
ok "os: $pretty   hostname: $(hostname)   kernel: $(uname -r)" [ "$pretty" = "FreeBSD $RELEASE" ]
ok "hostname: test.example.net" [ "$(hostname)" = test.example.net ]
ok "root: $(mount -p | awk '$2 == "/" {print $1}'), a boot environment ($(sudo -n bectl list -H | awk '{print $1, $2}' | paste -sd, -))" sh -c 'mount -p | awk "\$2 == \"/\" {print \$1}" | grep -qx zroot/ROOT/default'
case "$LAYOUT" in zfs-mirror) vdev=mirror-0 ;; zfs-raidz) vdev=raidz1-0 ;; *) vdev="" ;; esac
ok "pool: $(zpool list -H -o name,size,health zroot | tr '\t' ' ')${vdev:+, $vdev} over $(zpool status zroot | grep -c 'gpt/zfs') disk(s)" sh -c "zpool list -H -o health zroot | grep -qx ONLINE && [ \"\$(zpool status zroot | grep -c gpt/zfs)\" = $NDISKS ] && { [ -z \"$vdev\" ] || zpool status zroot | grep -q \"$vdev\"; }"
ok "interfaces: $(ifconfig -l ether)   address: $(ifconfig ext01 | awk '$1 == "inet" {print $2}')" sh -c 'ifconfig ext01 | grep -q "inet "'
ok "sudo: without a password" sudo -n true
ok "firewall: pf, $(sudo -n pfctl -sr 2>/dev/null | grep -c '^pass in') rules that let in, the rest dropped" sh -c 'sudo -n pfctl -si | grep -q "Status: Enabled" && sudo -n pfctl -sr | grep -q "^block drop in all"'
ok "userdata: the entry left its mark (/etc/ump-userdata-test, and $(grep ump_userdata_test /etc/rc.conf))" sh -c '[ "$(cat /etc/ump-userdata-test)" = merged ] && grep -q "^ump_userdata_test=\"YES\"" /etc/rc.conf'
ok "userdata: ran before the first boot (/var/log/ump-userdata.log)" sudo -n grep -q "=== userdata test" /var/log/ump-userdata.log
ok "swap: $(swapinfo | awk 'NR > 1 && $1 != "Total" {print $1}' | paste -sd' ' -)" sh -c '[ "$(swapinfo | awk "NR > 1" | wc -l)" -gt 0 ]'
ok "install record: $(grep -E '^(image_version|layout|disks|root)=' /etc/ump/install | paste -sd' ' -)" sh -c "grep -q '^os=$OS' /etc/ump/install && grep -q '^layout=$LAYOUT' /etc/ump/install && grep -q '^root=zroot/ROOT/default' /etc/ump/install"
if [ -n "$RECORD_KEY" ]; then
	ok "record (--record): the host key this system offers" grep -qF "$RECORD_KEY" /etc/ssh/ssh_host_ed25519_key.pub
fi
ok "time zone: Europe/Berlin from the profile" [ "$(cat /var/db/zoneinfo)" = Europe/Berlin ]
ok "python: $(/usr/local/bin/python3 --version 2>&1)" /usr/local/bin/python3 -c 'import json'
ok "root: no password, and no login over SSH" sh -c "sudo -n grep -q '^root:\*:' /etc/master.passwd && sudo -n sshd -T | grep -qi '^permitrootlogin no'"
[ "$ARCH" != amd64 ] || echo "firmware: $(sysctl -n machdep.bootmethod)"
CHECK
	echo "==> powering off"
	ssh "${ssh_opts[@]}" ump@127.0.0.1 'sudo -n shutdown -p now' >/dev/null 2>&1 || true
	for _ in $(seq 1 60); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
	if [ -n "$spare_sum" ]; then
		kill "$qemu_pid" 2>/dev/null || true; wait "$qemu_pid" 2>/dev/null || true
		[ "$(sha256 < "$WORK/spare.img")" = "$spare_sum" ] || { echo "FAILED: the disk the specification leaves out was changed"; exit 1; }
		echo "the disk left alone (--disks; ${SPARE}G with data): as it was, byte for byte"
	fi
	echo "==> ok ($(( $(date +%s) - chain ))s from the first dry run)"
	exit 0
}

truncate -s "${DISK}G" "$WORK/disk.img"
disks=("$WORK/disk.img")
if [ -n "${DISK2:-}" ]; then
	truncate -s "${DISK2}G" "$WORK/disk2.img"
	disks+=("$WORK/disk2.img")
fi
serial=0
if [ "$family" = freebsd ]; then
	if [ -n "${DISK3:-}" ]; then
		truncate -s "${DISK3}G" "$WORK/disk3.img"
		disks+=("$WORK/disk3.img")
	fi
	# no disk is empty: a megabyte of data on each, which the dry run must leave
	for f in "${disks[@]}"; do dd if=/dev/urandom of="$f" bs=1048576 count=1 seek=1 conv=notrunc 2>/dev/null; done
	if [ -n "${SPARE:-}" ]; then
		# the disk that is left alone: data at its start, in it and at its end
		truncate -s "${SPARE}G" "$WORK/spare.img"
		for at in 0 $((SPARE * 512)) $((SPARE * 1024 - 8)); do dd if=/dev/urandom of="$WORK/spare.img" bs=1048576 count=8 seek="$at" conv=notrunc 2>/dev/null; done
		disks+=("$WORK/spare.img")
		serial=1
	fi
	[ "${SERIAL:-0}" != 1 ] || serial=1
fi
drives=()
for i in "${!disks[@]}"; do
	if [ "$serial" = 1 ]; then
		drives+=(-drive "file=${disks[$i]},format=raw,if=none,id=disk$i" -device "virtio-blk-pci,drive=disk$i,serial=ump-test-$((i + 1))")
	else
		drives+=(-drive "file=${disks[$i]},format=raw,if=virtio")
	fi
done
net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device "virtio-net-pci,netdev=n0,mac=$MAC")
# start_qemu LOG ARGS...: the machine with its disks; LOG gets its serial console
start_qemu() {
	local log="$1"; shift
	"$qemu" "${machine[@]}" ${firmware[@]+"${firmware[@]}"} -m "$MEM" -smp 2 -nographic "$@" "${drives[@]}" \
		-serial "file:$log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
}
stop_qemu() { kill "$qemu_pid"; wait "$qemu_pid" 2>/dev/null || true; }
# start_rescue: the rescue image, started with kernel and initramfs as
# after a kexec, and the installer with the specification put there
scp_opts=(-q -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT")
start_rescue() {
	start_qemu "$WORK/serial.log" -kernel "$RESCUE/vmlinuz" -initrd "$RESCUE/initrd.zst" \
		-append "console=ttyS0 console=ttyAMA0 ump.hostname=rescue ump.authorized_keys=$(base64 < "$WORK/key.pub" | tr -d '\n')" "${net[@]}"
	wait_ssh root
	ssh "${ssh_opts[@]}" root@127.0.0.1 '[ -d /sys/firmware/efi ] && echo "firmware: UEFI" || echo "firmware: BIOS"'
	scp "${scp_opts[@]}" "$ROOT/install/install.sh" "$WORK/spec.json" root@127.0.0.1:/tmp/
}
echo "==> booting the rescue image ($ARCH, ${MEM}M, $([ "$FIRMWARE" = uefi ] && echo UEFI || echo BIOS))"
start_rescue
[ "$family" != freebsd ] || freebsd_chain
# as ump runs it, or as a PXE machine's runner does (--record)
args="--spec /tmp/spec.json"; [ "$RECORD" = 1 ] && args="$args --record /tmp/ump-record"
echo "==> install.sh $args ($OS${UPLINK:+, uplink $UPLINK})"
start=$(date +%s)
ssh "${ssh_opts[@]}" root@127.0.0.1 "bash /tmp/install.sh $args" 2>&1 | tee "$WORK/install.log" | grep '^==>\|install.sh' || true
grep -q "^==> installed: $OS" "$WORK/install.log" || { echo "the installation failed"; tail -n 30 "$WORK/install.log"; exit 1; }
if [ "$RECORD" = 1 ]; then
	mkdir -p "$WORK/record"
	scp "${scp_opts[@]}" "root@127.0.0.1:/tmp/ump-record/*" "$WORK/record/"
fi
echo "==> installed in $(( $(date +%s) - start ))s; booting the disk"
stop_qemu
start_qemu "$WORK/serial-nixos.log" "${net[@]}"
cp /dev/null "$WORK/serial.log"; ln -sf "$WORK/serial-nixos.log" "$WORK/serial.log"
wait_ssh ump
echo "==> checking the new system"
ssh "${ssh_opts[@]}" ump@127.0.0.1 "FAMILY=$family OS=$OS UPLINK=$UPLINK RECORD_KEY='$(cat "$WORK"/record/ssh_host_ed25519_key.pub 2>/dev/null | awk '{print $2}')' bash -s" <<'CHECK'
set -e
# ok WHAT COMMAND...: say so, or fail the run
ok() { local what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
. /etc/os-release; echo "os: $PRETTY_NAME   hostname: $(hostname -f 2>/dev/null || hostname)   kernel: $(uname -r)"
echo "interfaces: $(ip -br link | awk "\$1 != \"lo\" {print \$1}" | paste -sd" " -)   address: $(ip -br -4 addr show scope global | awk "{print \$3}")"
ok "sudo: without a password" sudo -n true
ok "firewall: input policy drop" bash -c 'sudo -n nft list ruleset | grep -q "policy drop"'
ok "userdata: the entry left its mark (/etc/ump-userdata-test)" [ "$(cat /etc/ump-userdata-test)" = merged ]
ok "swap: $(awk 'NR > 1 {print $1}' /proc/swaps | paste -sd" " -)" [ "$(awk 'NR > 1' /proc/swaps | wc -l)" -gt 0 ]
ok "install record: $(grep "^image_version=" /etc/ump/install)" grep -q "^os=$OS" /etc/ump/install
if [ -n "$RECORD_KEY" ]; then
	ok "record (--record): the host key this system offers" grep -qF "$RECORD_KEY" /etc/ssh/ssh_host_ed25519_key.pub
fi
if [ -n "$UPLINK" ]; then
	ok "uplink: $(grep '^network=\|^dns=' /etc/ump/install | paste -sd' ' -) in the install record" grep -q "^network=${UPLINK%-dns}$" /etc/ump/install
	if [ "$UPLINK" != static ]; then
		ok "uplink: the address by DHCP ($(ip -4 -o addr show scope global | awk '{print $4}'))" bash -c "ip -4 -o addr show scope global | grep -q dynamic"
	else
		ok "uplink: the address static (10.0.2.15/24)" bash -c "ip -4 -o addr show scope global | grep -v dynamic | grep -q 10.0.2.15/24"
	fi
	resolvers="$(resolvectl dns | sed 's/^[^:]*://' | tr ' ' '\n' | sed '/^$/d' | sort -u | paste -sd, -)"
	case "$UPLINK" in
	dhcp) ok "uplink: resolver 10.0.2.3 alone (from the lease)" [ "$resolvers" = 10.0.2.3 ] ;;
	static) ok "uplink: resolver 10.0.2.3 alone (from the specification)" [ "$resolvers" = 10.0.2.3 ] ;;
	dhcp-dns) ok "uplink: resolver 192.0.2.53 alone (from the specification, not the lease's 10.0.2.3; got $resolvers)" [ "$resolvers" = 192.0.2.53 ] ;;
	esac
fi
ok "message of the day: ump-motd shows the banner" bash -c 'ump-motd | grep -q "Unus Mundus Provisioner"'
ump-motd | grep "host  \|uptime" | sed "s/\x1b\[[0-9;]*m//g; s/^ */message of the day: /"
if [ "$FAMILY" = nixos ]; then
	ok "userdata: package hello installed" hello
	ok "userdata: time zone UTC wins over the profile's Europe/Berlin" [ "$(readlink /etc/localtime | sed "s#.*/zoneinfo/##")" = UTC ]
	ok "nixos-rebuild: the configuration evaluates on the machine" sudo -n nixos-rebuild dry-build
else
	ok "time zone: Europe/Berlin from the profile" [ "$(cat /etc/timezone 2>/dev/null)" = Europe/Berlin ]
	ok "userdata: ran before the first boot (/var/log/ump-userdata.log)" sudo -n grep -q "=== userdata test" /var/log/ump-userdata.log
fi
CHECK
# the prompt UMP gives bash on every system
{ sleep 5; printf 'echo "PROMPT[$PS1]"; exit\n'; } | ssh "${ssh_opts[@]}" -tt ump@127.0.0.1 2>/dev/null | tr -d "\r" | grep -q 'PROMPT\[.*\\u@\\h.*\\w.* \$ \]' && echo "shell: UMP's prompt (user@host dir \$)" || echo "shell: NOT UMP's prompt"
# the banner at an interactive login, as Ubuntu shows it
{ sleep 5; printf 'exit\n'; } | ssh "${ssh_opts[@]}" -tt ump@127.0.0.1 2>/dev/null | tr -d "\r" | grep -q "Unus Mundus Provisioner" && echo "message of the day: shown at an interactive SSH login" || echo "message of the day: NOT shown at an interactive SSH login"
echo "==> powering off"
ssh "${ssh_opts[@]}" ump@127.0.0.1 'sudo -n systemctl poweroff' 2>/dev/null || true
echo "==> ok"
