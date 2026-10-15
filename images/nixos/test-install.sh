#!/usr/bin/env bash
# Install NixOS with install/install.sh in QEMU and check the result: the
# UMP rescue image boots with UEFI firmware, the installer runs with a
# NixOS specification against a local storage (the manifest from out/ and
# install/nixos/ump.nix), then the disk boots and the new system is checked
# over SSH as the administrator: NixOS, the hostname, the interface names,
# sudo, the firewall, a userdata module's effect, and a configuration that
# nixos-rebuild still evaluates. Downloads from cache.nixos.org.
#
# Usage: test-install.sh [arm64|amd64]   (default: this machine's)
# Needs the rescue image in images/rescue/out/<arch> (images/rescue/build.sh)
# and the manifest in images/nixos/out (build.sh). Environment: MEM (MB,
# default 2048), DISK (GB, default 12), PORT (default 2252), TIMEOUT (s).
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
RESCUE="$ROOT/images/rescue/out/$ARCH"
MANIFEST="$HERE/out/nixos-25.11/$ARCH/manifest.json"
MEM="${MEM:-2048}"
DISK="${DISK:-12}"
PORT="${PORT:-2252}"
HTTP=$((PORT + 1))
TIMEOUT="${TIMEOUT:-600}"
WORK="$(mktemp -d /tmp/ump-nixos-test.XXXXXX)"
qemu_pid="" http_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$http_pid" ] && kill "$http_pid" 2>/dev/null || true
	echo "work directory (serial logs): $WORK"
}
trap cleanup EXIT
[ -f "$RESCUE/vmlinuz" ] || { echo "no rescue image in $RESCUE" >&2; exit 2; }
[ -f "$MANIFEST" ] || { echo "no manifest at $MANIFEST; run build.sh" >&2; exit 2; }

case "$ARCH" in
arm64)
	qemu=qemu-system-aarch64; machine=(-M virt -cpu cortex-a72); fw=edk2-aarch64-code.fd; vars=edk2-arm-vars.fd
	[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] && machine=(-M virt -accel hvf -cpu host)
	;;
amd64)
	qemu=qemu-system-x86_64; machine=(-M q35); fw=edk2-x86_64-code.fd; vars=edk2-i386-vars.fd
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
cp "$share/$vars" "$WORK/vars.fd"
[ "$ARCH" = arm64 ] && truncate -s 64M "$WORK/vars.fd"
code="$WORK/code.fd"; cp "$share/$fw" "$code"; [ "$ARCH" = arm64 ] && truncate -s 64M "$code"
firmware=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=$WORK/vars.fd")

# the storage: the manifest and UMP's NixOS module
mkdir -p "$WORK/storage/os/nixos-25.11/$ARCH" "$WORK/storage/install/nixos"
cp "$MANIFEST" "$WORK/storage/os/nixos-25.11/$ARCH/manifest.json"
cp "$ROOT/install/nixos/ump.nix" "$WORK/storage/install/nixos/ump.nix"
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/storage" "$HTTP" >/dev/null 2>&1 &
http_pid=$!

ssh-keygen -q -t ed25519 -N '' -C ump-nixos-test -f "$WORK/key"
pub="$(cat "$WORK/key.pub")"
jq -n --arg key "$pub" --arg storage "http://10.0.2.2:$HTTP" '{
	os: "nixos-25.11", hostname: "nixos-test.example.net", timezone: "Europe/Berlin", storage: $storage,
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
	userdata: [{name: "test", type: "nix", script: "{ pkgs, lib, ... }: {\n  environment.etc.\"ump-userdata-test\".text = \"merged\";\n  environment.systemPackages = [ pkgs.hello ];\n  time.timeZone = \"UTC\";\n}\n"}],
	firewall: {policy: "drop", allow: [{proto: "icmp"}, {proto: "tcp", port: "22"}]},
	provision: {type: "none"}, network: {rename: true}, profile: {name: "test"}}' > "$WORK/spec.json"

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

truncate -s "${DISK}G" "$WORK/disk.img"
net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device virtio-net-pci,netdev=n0)
echo "==> booting the rescue image ($ARCH, ${MEM}M, UEFI)"
"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic \
	-kernel "$RESCUE/vmlinuz" -initrd "$RESCUE/initrd.zst" \
	-append "console=ttyS0 console=ttyAMA0 ump.hostname=rescue ump.authorized_keys=$(base64 < "$WORK/key.pub" | tr -d '\n')" \
	"${net[@]}" -drive "file=$WORK/disk.img,format=raw,if=virtio" \
	-serial "file:$WORK/serial.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
qemu_pid=$!
wait_ssh root
ssh "${ssh_opts[@]}" root@127.0.0.1 '[ -d /sys/firmware/efi ] && echo "firmware: UEFI" || echo "firmware: BIOS"'
scp -q -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT" \
	"$ROOT/install/install.sh" "$WORK/spec.json" root@127.0.0.1:/tmp/
echo "==> install.sh --spec (NixOS 25.11; downloads from cache.nixos.org)"
start=$(date +%s)
ssh "${ssh_opts[@]}" root@127.0.0.1 'bash /tmp/install.sh --spec /tmp/spec.json' 2>&1 | tee "$WORK/install.log" | grep '^==>\|install.sh' || true
grep -q '^==> installed: nixos-25.11' "$WORK/install.log" || { echo "the installation failed"; tail -n 30 "$WORK/install.log"; exit 1; }
echo "==> installed in $(( $(date +%s) - start ))s; booting the disk"
kill "$qemu_pid"; wait "$qemu_pid" 2>/dev/null || true
"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic \
	"${net[@]}" -drive "file=$WORK/disk.img,format=raw,if=virtio" \
	-serial "file:$WORK/serial-nixos.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
qemu_pid=$!
cp /dev/null "$WORK/serial.log"; ln -sf "$WORK/serial-nixos.log" "$WORK/serial.log"
wait_ssh ump
echo "==> checking the new system"
ssh "${ssh_opts[@]}" ump@127.0.0.1 'set -e
. /etc/os-release; echo "os: $PRETTY_NAME   hostname: $(hostname -f 2>/dev/null || hostname)   kernel: $(uname -r)"
echo "interfaces: $(ip -br link | awk "\$1 != \"lo\" {print \$1}" | paste -sd" " -)   address: $(ip -br -4 addr show scope global | awk "{print \$3}")"
sudo -n true && echo "sudo: without a password"
sudo -n nft list ruleset | grep -q "policy drop" && echo "firewall: input policy drop"
[ "$(cat /etc/ump-userdata-test)" = merged ] && echo "userdata: module merged (/etc/ump-userdata-test)"
hello >/dev/null && echo "userdata: package hello installed"
[ "$(readlink /etc/localtime | sed "s#.*/zoneinfo/##")" = UTC ] && echo "userdata: time zone UTC wins over the profile'"'"'s Europe/Berlin"
swapon --show=NAME --noheadings | grep -q . && echo "swap: $(swapon --show=NAME --noheadings | paste -sd" " -)"
grep -q "^os=nixos-25.11" /etc/ump/install && echo "install record: $(grep "^image_version=" /etc/ump/install)"
sudo -n nixos-rebuild dry-build >/dev/null 2>&1 && echo "nixos-rebuild: the configuration evaluates on the machine"
'
echo "==> powering off"
ssh "${ssh_opts[@]}" ump@127.0.0.1 'sudo -n systemctl poweroff' 2>/dev/null || true
echo "==> ok"
