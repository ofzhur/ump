#!/usr/bin/env bash
# Install an operating system with install/install.sh in QEMU and check
# the result: the UMP rescue image boots with UEFI firmware, the installer
# runs with a specification for OS against a local storage (the built
# image or manifest from images/base/out, images/nixos/out or
# images/arch/out, and the
# installer's files from install/), then the disk boots and the new
# system is checked over SSH as the administrator: the system, the
# hostname, the interface names, sudo, the firewall, swap, a userdata
# entry's effect, the install record and the message of the day; on
# NixOS also a userdata module winning over a default, and a
# configuration that nixos-rebuild still evaluates.
#
# Usage: test-install.sh OS [arm64|amd64]   (OS: debian-13, ubuntu-26.04,
#        nixos-25.11, archlinux, cachyos, ...; the architecture defaults to this machine's)
# Needs the rescue image in images/rescue/out/<arch> and the image (or the
# NixOS manifest) built. Environment: MEM (MB, default 2048), DISK (GB,
# default 12), DISK2 (GB: a second disk, which the layouts span, as a
# Linode's swap disk; default none), PORT (default 2252), TIMEOUT (s).
set -euo pipefail
OS="${1:?usage: test-install.sh OS [arm64|amd64]}"
ARCH="${2:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
RESCUE="$ROOT/images/rescue/out/$ARCH"
case "$OS" in
nixos-*) family=nixos; IMAGE="$HERE/nixos/out/$OS/$ARCH" ;;
archlinux*|cachyos*) family=arch; IMAGE="$HERE/arch/out/$OS/$ARCH" ;;
*) family=image; IMAGE="$HERE/base/out/$OS/$ARCH" ;;
esac
MANIFEST="$IMAGE/manifest.json"
MEM="${MEM:-2048}"
DISK="${DISK:-12}"
PORT="${PORT:-2252}"
HTTP=$((PORT + 1))
TIMEOUT="${TIMEOUT:-600}"
WORK="$(mktemp -d /tmp/ump-install-test.XXXXXX)"
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
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
cp "$share/$vars" "$WORK/vars.fd"
[ "$ARCH" = arm64 ] && truncate -s 64M "$WORK/vars.fd"
code="$WORK/code.fd"; cp "$share/$fw" "$code"; [ "$ARCH" = arm64 ] && truncate -s 64M "$code"
firmware=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=$WORK/vars.fd")

# the storage: the image (or the manifest) and the installer's files
mkdir -p "$WORK/storage/os/$OS/$ARCH" "$WORK/storage/install/nixos"
cp "$IMAGE"/manifest.json "$WORK/storage/os/$OS/$ARCH/"
[ -f "$IMAGE/rootfs.tar.zst" ] && ln -s "$IMAGE/rootfs.tar.zst" "$WORK/storage/os/$OS/$ARCH/rootfs.tar.zst"
cp "$ROOT/install/nixos/ump.nix" "$WORK/storage/install/nixos/ump.nix"
cp "$ROOT/install/motd.sh" "$ROOT/install/bashrc.sh" "$WORK/storage/install/"
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/storage" "$HTTP" >/dev/null 2>&1 &
http_pid=$!

ssh-keygen -q -t ed25519 -N '' -C ump-install-test -f "$WORK/key"
pub="$(cat "$WORK/key.pub")"
# a userdata entry of the system's kind that leaves a mark: a NixOS module
# that also overrides a default (the time zone), or a bash script
if [ "$family" = nixos ]; then
	userdata="$(jq -n '[{name: "test", type: "nix", script: "{ pkgs, lib, ... }: {\n  environment.etc.\"ump-userdata-test\".text = \"merged\";\n  environment.systemPackages = [ pkgs.hello ];\n  time.timeZone = \"UTC\";\n}\n"}]')"
else
	userdata="$(jq -n '[{name: "test", type: "bash", script: "#!/bin/bash\nset -e\necho merged > /etc/ump-userdata-test\n"}]')"
fi
jq -n --arg key "$pub" --arg storage "http://10.0.2.2:$HTTP" --arg os "$OS" --argjson userdata "$userdata" '{
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
drives=(-drive "file=$WORK/disk.img,format=raw,if=virtio")
if [ -n "${DISK2:-}" ]; then
	truncate -s "${DISK2}G" "$WORK/disk2.img"
	drives+=(-drive "file=$WORK/disk2.img,format=raw,if=virtio")
fi
net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device virtio-net-pci,netdev=n0)
echo "==> booting the rescue image ($ARCH, ${MEM}M, UEFI)"
"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic \
	-kernel "$RESCUE/vmlinuz" -initrd "$RESCUE/initrd.zst" \
	-append "console=ttyS0 console=ttyAMA0 ump.hostname=rescue ump.authorized_keys=$(base64 < "$WORK/key.pub" | tr -d '\n')" \
	"${net[@]}" "${drives[@]}" \
	-serial "file:$WORK/serial.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
qemu_pid=$!
wait_ssh root
ssh "${ssh_opts[@]}" root@127.0.0.1 '[ -d /sys/firmware/efi ] && echo "firmware: UEFI" || echo "firmware: BIOS"'
scp -q -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT" \
	"$ROOT/install/install.sh" "$WORK/spec.json" root@127.0.0.1:/tmp/
echo "==> install.sh --spec ($OS)"
start=$(date +%s)
ssh "${ssh_opts[@]}" root@127.0.0.1 'bash /tmp/install.sh --spec /tmp/spec.json' 2>&1 | tee "$WORK/install.log" | grep '^==>\|install.sh' || true
grep -q "^==> installed: $OS" "$WORK/install.log" || { echo "the installation failed"; tail -n 30 "$WORK/install.log"; exit 1; }
echo "==> installed in $(( $(date +%s) - start ))s; booting the disk"
kill "$qemu_pid"; wait "$qemu_pid" 2>/dev/null || true
"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic \
	"${net[@]}" "${drives[@]}" \
	-serial "file:$WORK/serial-nixos.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
qemu_pid=$!
cp /dev/null "$WORK/serial.log"; ln -sf "$WORK/serial-nixos.log" "$WORK/serial.log"
wait_ssh ump
echo "==> checking the new system"
ssh "${ssh_opts[@]}" ump@127.0.0.1 "FAMILY=$family OS=$OS bash -s" <<'CHECK'
set -e
. /etc/os-release; echo "os: $PRETTY_NAME   hostname: $(hostname -f 2>/dev/null || hostname)   kernel: $(uname -r)"
echo "interfaces: $(ip -br link | awk "\$1 != \"lo\" {print \$1}" | paste -sd" " -)   address: $(ip -br -4 addr show scope global | awk "{print \$3}")"
sudo -n true && echo "sudo: without a password"
sudo -n nft list ruleset | grep -q "policy drop" && echo "firewall: input policy drop"
[ "$(cat /etc/ump-userdata-test)" = merged ] && echo "userdata: the entry left its mark (/etc/ump-userdata-test)"
[ "$(awk 'NR > 1' /proc/swaps | wc -l)" -gt 0 ] && echo "swap: $(awk 'NR > 1 {print $1}' /proc/swaps | paste -sd" " -)"
grep -q "^os=$OS" /etc/ump/install && echo "install record: $(grep "^image_version=" /etc/ump/install)"
ump-motd | grep -q "Unus Mundus Provisioner" && echo "message of the day: ump-motd shows the banner"
ump-motd | grep "host  \|uptime" | sed "s/\x1b\[[0-9;]*m//g; s/^ */message of the day: /"
if [ "$FAMILY" = nixos ]; then
	hello >/dev/null && echo "userdata: package hello installed"
	[ "$(readlink /etc/localtime | sed "s#.*/zoneinfo/##")" = UTC ] && echo "userdata: time zone UTC wins over the profile's Europe/Berlin"
	sudo -n nixos-rebuild dry-build >/dev/null 2>&1 && echo "nixos-rebuild: the configuration evaluates on the machine"
else
	[ "$(cat /etc/timezone 2>/dev/null)" = Europe/Berlin ] && echo "time zone: Europe/Berlin from the profile"
	sudo -n grep -q "=== userdata test" /var/log/ump-userdata.log && echo "userdata: ran before the first boot (/var/log/ump-userdata.log)"
fi
CHECK
# the prompt UMP gives bash on every system
{ sleep 5; printf 'echo "PROMPT[$PS1]"; exit\n'; } | ssh "${ssh_opts[@]}" -tt ump@127.0.0.1 2>/dev/null | tr -d "\r" | grep -q 'PROMPT\[.*\\u@\\h.*\\w.* \$ \]' && echo "shell: UMP's prompt (user@host dir \$)" || echo "shell: NOT UMP's prompt"
# the banner at an interactive login, as Ubuntu shows it
{ sleep 5; printf 'exit\n'; } | ssh "${ssh_opts[@]}" -tt ump@127.0.0.1 2>/dev/null | tr -d "\r" | grep -q "Unus Mundus Provisioner" && echo "message of the day: shown at an interactive SSH login" || echo "message of the day: NOT shown at an interactive SSH login"
echo "==> powering off"
ssh "${ssh_opts[@]}" ump@127.0.0.1 'sudo -n systemctl poweroff' 2>/dev/null || true
echo "==> ok"
