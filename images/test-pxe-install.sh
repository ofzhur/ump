#!/usr/bin/env bash
# An unattended installation in QEMU, end to end: a machine with a blank
# virtio disk boots over the network against ump pxe serve, in chain
# mode on QEMU's user network as test-pxe.sh boots it, and installs OS
# by itself. In a home of its own, with ump built from this tree and a
# storage of its own (the rescue image, the OS image, the installer of
# install/), which the machine reaches at http://10.0.2.2:PORT and ump
# on this host at the same URL, through that server as its HTTP proxy,
# it checks:
#  - ump server reinstall pxe/pxe-test --unattended renders the
#    specification, arms it for blank disks alone (no network boot
#    reported the disks yet) and asks to power-cycle the machine (QEMU
#    starts then);
#  - the rescue system claims the specification, install.sh installs OS
#    on the disk, and its output reaches ump as it comes; the report
#    makes the outcome installed and the machine reboots; its next
#    network boot is answered with the disk, and the firmware boots it;
#  - ump checks the new system at the machine's configured address
#    through the site's jump host (the new system itself, through QEMU's
#    port forward), as its administrator with UMP's key and the host
#    keys the installation reported pinned, and only then records it and
#    removes the arm and its outcome; ump pxe status --log has the output;
#  - the new system, over ump ssh through the jump host: the hostname,
#    sudo, the install record, the userdata's mark, the firewall, and the
#    uplink of pxe.yaml (DHCP with the address QEMU hands out as the
#    reservation, or UPLINK=static);
#  - a second ump server reinstall --unattended reboots the new system
#    over SSH, and the rescue system's disk guard refuses the
#    installation, as the disk is no longer blank: the outcome is failed,
#    ump records nothing and exits with 1, and the machine stays in the
#    rescue system; powered on again, it boots its disk, the first
#    installation untouched.
#
# Usage: test-pxe-install.sh [arm64|amd64]   (default: this machine's)
# Needs what test-pxe.sh needs, the rescue image with the feature
# pxe-install, and the base image of OS for the architecture
# (images/base/out/OS/<arch>, make os-image). Environment: OS (an
# Ubuntu or Debian, default ubuntu-26.04), UPLINK (dhcp or static, the
# installed system's network in pxe.yaml; default dhcp), FIRMWARE,
# IPXE_DIR and MEM as for test-pxe.sh, DISK (GB, default 8), PORT
# (default 2272, and the two after it), TIMEOUT (s, default 900, for each
# step; amd64 without KVM needs more).
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
FIRMWARE="${FIRMWARE:-uefi}"
OS="${OS:-ubuntu-26.04}"
UPLINK="${UPLINK:-dhcp}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
RESCUE="$ROOT/images/rescue/out"
IMAGE="$HERE/base/out/$OS/$ARCH"
MEM="${MEM:-2048}"
DISK="${DISK:-8}"
PORT="${PORT:-2272}"
TIMEOUT="${TIMEOUT:-900}"
storage_port=$((PORT + 1)) pxe_port=$((PORT + 2))
mac=52:54:00:12:34:56
# Debian's ipxe, for machines without the distribution's: pinned and checked
IPXE_DEB="https://snapshot.debian.org/archive/debian/20251101T000000Z/pool/main/i/ipxe/ipxe_1.21.1%2Bgit20250501.dad20602%2Bdfsg-1_all.deb"
IPXE_DEB_SHA256=eeaca79dbde0fb86c102bf1d362c6f6a67ed0ddab38b3dd12e1b332339f5dd99
WORK="$(mktemp -d /tmp/ump-pxe-install-test.XXXXXX)"
qemu_pid="" serve_pid="" storage_pid="" reinstall_pid="" rlog="" ok=0
cleanup() {
	for p in "$qemu_pid" "$reinstall_pid" "$serve_pid" "$storage_pid"; do
		[ -n "$p" ] && kill "$p" 2>/dev/null || true
	done
	if [ "$ok" = 1 ]; then rm -rf "$WORK"; else echo "work directory (logs): $WORK"; fi
}
trap cleanup EXIT

fail() { echo "FAILED: $*"; exit 1; }
check() { # check WHAT COMMAND...: say so, or fail
	local what="$1"; shift
	if "$@" >/dev/null 2>&1; then echo "$what"; else fail "$what"; fi
}
# until_ok DESCRIPTION COMMAND...: wait for COMMAND to succeed, while QEMU
# (when it runs) and the server run
until_ok() {
	local what="$1" deadline=$(( $(date +%s) + TIMEOUT )); shift
	until "$@" >/dev/null 2>&1; do
		[ -z "$serve_pid" ] || kill -0 "$serve_pid" 2>/dev/null || { echo "ump pxe serve exited:"; tail -n 20 "$WORK/serve.log"; fail "$what"; }
		[ -z "$qemu_pid" ] || kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited:"; cat "$WORK/qemu.err"; tail -n 30 "$WORK/serial.log"; fail "$what"; }
		if [ "$(date +%s)" -gt "$deadline" ]; then
			echo "no $what within ${TIMEOUT}s; last lines of the server, of ump and of the serial console:"
			tail -n 15 "$WORK/serve.log"; [ -z "$rlog" ] || tail -n 15 "$rlog"
			[ -f "$WORK/serial.log" ] && tail -n 30 "$WORK/serial.log" | tr -d '\033'
			fail "$what"
		fi
		sleep 2
	done
}

case "$ARCH-$FIRMWARE" in
arm64-uefi)
	qemu=qemu-system-aarch64 fw=edk2-aarch64-code.fd vars=edk2-arm-vars.fd
	machine=(-M virt)
	if [ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ]; then machine+=(-accel hvf -cpu host)
	elif [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = aarch64 ] && [ -w /dev/kvm ]; then machine+=(-accel kvm -cpu host)
	else machine+=(-cpu cortex-a72); fi
	ipxe_names=(ipxe-arm64.efi arm64-efi/ipxe.efi arm64-efi/snponly.efi)
	;;
amd64-uefi|amd64-bios)
	qemu=qemu-system-x86_64 fw=edk2-x86_64-code.fd vars=edk2-i386-vars.fd
	machine=(-M q35)
	[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] && [ -w /dev/kvm ] && machine+=(-accel kvm -cpu host)
	ipxe_names=(ipxe-amd64.efi ipxe.efi ipxe-x86_64.efi snponly.efi ipxe-snponly-x86_64.efi)
	;;
*) echo "unsupported: $ARCH with FIRMWARE=$FIRMWARE (arm64 uefi, amd64 uefi or bios)" >&2; exit 2 ;;
esac
case "$UPLINK" in
dhcp|static) ;;
*) echo "UPLINK is dhcp or static" >&2; exit 2 ;;
esac
for t in "$qemu" go python3 jq curl ssh ssh-keygen; do
	command -v "$t" >/dev/null || { echo "$t not found" >&2; exit 2; }
done
manifest="$RESCUE/$ARCH/manifest.json"
[ -f "$manifest" ] || { echo "no rescue image in $RESCUE/$ARCH; make rescue-image RESCUE_ARCH=$ARCH" >&2; exit 2; }
jq -e '.features // [] | index("pxe-install")' "$manifest" >/dev/null ||
	{ echo "the rescue image in $RESCUE/$ARCH has no pxe-install; build it again (make rescue-image RESCUE_ARCH=$ARCH)" >&2; exit 2; }
[ -f "$IMAGE/rootfs.tar.zst" ] || { echo "no image of $OS in $IMAGE; make os-image OS_LIST=$OS OS_ARCH=$ARCH" >&2; exit 2; }

echo "==> ump from this tree, a home of its own in $WORK"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/ump" ./cmd/ump)
home="$WORK/home"
mkdir -p "$home/.config/ump/profiles" "$WORK/bin"
# every SSH of the test, ump's and the jump host's, with the test's keys
# alone: nothing of the user's ~/.ssh, which ssh reads whatever HOME
# says. The jump host is the machine itself, whose host key changes with
# every system it boots, so the known hosts are none; ump pins its own.
cat > "$WORK/ssh_config" <<EOF
Host *
	IdentityFile $WORK/key
	IdentityFile $home/.config/ump/staging_ed25519
	IdentitiesOnly yes
	UserKnownHostsFile /dev/null
	StrictHostKeyChecking no
	BatchMode yes
	LogLevel ERROR
	ConnectTimeout 5
	ServerAliveInterval 3
EOF
printf '#!/bin/sh\nexec %s -F %s "$@"\n' "$(command -v ssh)" "$WORK/ssh_config" > "$WORK/bin/ssh"
chmod +x "$WORK/bin/ssh"
# ump in the test's home and nothing of this environment; started in the
# background as "${ump[@]}" ... &, so that $! is ump's process. The
# storage server is its HTTP proxy: the storage's URL is the machine's
# view of this host, 10.0.2.2, which ump reaches through it.
ump=(env -i HOME="$home" PATH="$WORK/bin:$PATH" TERM=dumb UMP_NO_UPDATE_CHECK=1 UMP_WAIT="${TIMEOUT}s"
	HTTP_PROXY="http://127.0.0.1:$storage_port" "$WORK/ump")
ump() { "${ump[@]}" "$@"; }
# gssh: root in the rescue system, through QEMU's port forward
gssh() { ssh -F "$WORK/ssh_config" -p "$PORT" root@127.0.0.1 "$@"; }
gone() { ! kill -0 "$1" 2>/dev/null; }
# serving: the PXE server of site lab runs and serves the site.json there
serving() { ump pxe status -o json | jq -e '.servers[] | select(.site == "lab" and .running and .current)'; }
# logged PATTERN: the server logged a line that matches since the mark
# (the length of its log when the step began); ump server reinstall,
# when it runs, has not ended without it
mark=0
logged() {
	tail -c +$((mark + 1)) "$WORK/serve.log" | grep -q -- "$1" && return
	[ -z "$reinstall_pid" ] || ! gone "$reinstall_pid" || { cat "$rlog"; fail "ump server reinstall ended before the server logged: $1"; }
	return 1
}
mark() { mark="$(wc -c < "$WORK/serve.log" | tr -d ' ')"; }
# said PATTERN: ump server reinstall said it (its stderr is $rlog), or
# still runs
said() {
	grep -q -- "$1" "$rlog" && return
	gone "$reinstall_pid" && { cat "$rlog"; fail "ump server reinstall ended before it said: $1"; }
	return 1
}
# the operator's key (staging.keys, and the new system's administrator's)
ssh-keygen -q -t ed25519 -N '' -C key -f "$WORK/key"

# the iPXE binary: the firmware's NIC ROM (QEMU's own) for BIOS, the
# distribution's EFI binary for UEFI
ipxe="" ipxe_dir=""
if [ "$FIRMWARE" = uefi ]; then
	for d in ${IPXE_DIR:-/usr/lib/ipxe /usr/share/ipxe}; do
		for n in "${ipxe_names[@]}"; do
			[ -f "$d/$n" ] && { ipxe_dir="$d" ipxe="$n"; break 2; }
		done
	done
	if [ -z "$ipxe" ]; then
		[ -z "${IPXE_DIR:-}" ] || { echo "no iPXE EFI binary for $ARCH in $IPXE_DIR (${ipxe_names[*]})" >&2; exit 2; }
		echo "==> Debian's ipxe package, for its iPXE binaries"
		mkdir -p "$WORK/ipxe-deb" && curl -fsSL -o "$WORK/ipxe-deb/ipxe.deb" "$IPXE_DEB"
		sum="$( (command -v sha256sum >/dev/null && sha256sum || shasum -a 256) < "$WORK/ipxe-deb/ipxe.deb" | cut -d' ' -f1)"
		[ "$sum" = "$IPXE_DEB_SHA256" ] || { echo "Debian's ipxe package has the sha256 $sum, not $IPXE_DEB_SHA256" >&2; exit 2; }
		(cd "$WORK/ipxe-deb" && { ar x ipxe.deb data.tar.xz 2>/dev/null || tar -xf ipxe.deb data.tar.xz; } && tar -xJf data.tar.xz ./usr/lib/ipxe)
		ipxe_dir="$WORK/ipxe-deb/usr/lib/ipxe"
		for n in "${ipxe_names[@]}"; do [ -f "$ipxe_dir/$n" ] && { ipxe="$n"; break; }; done
		[ -n "$ipxe" ] || { echo "no iPXE binary for $ARCH in Debian's package" >&2; exit 2; }
	fi
fi

share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
firmware=()
if [ "$FIRMWARE" = uefi ]; then
	cp "$share/$fw" "$WORK/code.fd" && cp "$share/$vars" "$WORK/vars.fd"
	[ "$ARCH" = arm64 ] && truncate -s 64M "$WORK/code.fd" "$WORK/vars.fd"
	firmware=(-drive "if=pflash,format=raw,readonly=on,file=$WORK/code.fd" -drive "if=pflash,format=raw,file=$WORK/vars.fd")
fi
truncate -s "${DISK}G" "$WORK/disk.img"
# boot: power on the machine, its disk and its NIC, whose firmware and
# iPXE take the boot file from QEMU's DHCP server, with SSH forwarded to
# the machine; the network boot comes first, the disk after it
boot() {
	local nic=(-netdev "user,id=n0,bootfile=http://10.0.2.2:$pxe_port/boot.ipxe,hostfwd=tcp:127.0.0.1:$PORT-:22")
	local disk=(-drive "if=none,id=d0,format=raw,file=$WORK/disk.img")
	if [ "$FIRMWARE" = uefi ]; then
		nic+=(-device "virtio-net-pci,netdev=n0,mac=$mac,romfile=" -kernel "$ipxe_dir/$ipxe")
		disk+=(-device virtio-blk-pci,drive=d0)
	else
		nic+=(-device "virtio-net-pci,netdev=n0,mac=$mac,bootindex=1")
		disk+=(-device virtio-blk-pci,drive=d0,bootindex=2)
	fi
	: > "$WORK/serial.log"
	"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic "${nic[@]}" "${disk[@]}" \
		-serial "file:$WORK/serial.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
}
power_off() {
	kill "$qemu_pid" 2>/dev/null || true
	wait "$qemu_pid" 2>/dev/null || true
	qemu_pid=""
}

# the storage: the rescue image, the OS image and the installer; a
# request through it as a proxy names the whole URL, whose path is the
# file
mkdir -p "$WORK/storage/os/$OS/$ARCH"
ln -s "$RESCUE" "$WORK/storage/rescue"
ln -s "$ROOT/install" "$WORK/storage/install"
cp "$IMAGE/manifest.json" "$WORK/storage/os/$OS/$ARCH/"
ln -s "$IMAGE/rootfs.tar.zst" "$WORK/storage/os/$OS/$ARCH/rootfs.tar.zst"
cat > "$WORK/storage.py" <<'PY'
import functools, http.server, sys, urllib.parse

class Storage(http.server.SimpleHTTPRequestHandler):
    def translate_path(self, path):
        return super().translate_path(urllib.parse.urlsplit(path).path)

    def log_message(self, *args):
        pass

http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[2])), functools.partial(Storage, directory=sys.argv[1])).serve_forever()
PY
echo "==> the storage: rescue image $(jq -r .version "$manifest"), $OS $(jq -r .version "$IMAGE/manifest.json") for $ARCH, install/ of this tree, on 127.0.0.1:$storage_port (10.0.2.2 for the machine)"
python3 -I "$WORK/storage.py" "$WORK/storage" "$storage_port" >/dev/null 2>&1 &
storage_pid=$!

cat > "$home/.config/ump/config.yaml" <<EOF
storage: http://10.0.2.2:$storage_port
staging:
  image: http://127.0.0.1:$storage_port/rescue
  keys: [$WORK/key.pub]
EOF
# the published profiles' named layouts and firewalls, and a profile of
# the test: a blank disk, no private network, no packages to fetch
cp "$ROOT"/profiles/*.yaml "$home/.config/ump/profiles/"
cat > "$home/.config/ump/profiles/pxe-test.yaml" <<EOF
name: pxe-test
description: the machine of test-pxe-install.sh
os: $OS
timezone: Europe/Berlin
kernel: {mitigations: false, ipv6: false, extra: ""}
disks: lvm-linear
users:
  - {name: ump, sudo: true, groups: [adm], shell: /bin/bash, keys: [ump, $WORK/key.pub]}
userdata:
  - name: test
    script: |
      #!/bin/bash
      set -e
      echo merged > /etc/ump-userdata-test
firewall: default
network: {rename: true, private: none}
EOF
# the site's jump host is the machine itself, through QEMU's port
# forward, as its administrator: what ump reaches at 10.0.2.15 is the
# new system on QEMU's network, as on a private network behind a jump
# host
if [ "$UPLINK" = static ]; then
	network="network: {ipv4: {address: 10.0.2.15/24, gateway: 10.0.2.2}, dns: [10.0.2.3]}"
else
	network="address: 10.0.2.15"
fi
cat > "$home/.config/ump/pxe.yaml" <<EOF
# the machine of the test, on QEMU's user network
unknown: local
sites:
  lab:
    description: QEMU's user network
    jump: ump@127.0.0.1:$PORT
machines:
  pxe-test:
    site: lab
    mac: $mac
    arch: $ARCH
    firmware: $FIRMWARE
    $network
EOF
ump config pxe apply

serve=(pxe serve --site lab --http "127.0.0.1:$pxe_port" --url "http://10.0.2.2:$pxe_port")
echo "==> ump ${serve[*]} --check"
ump "${serve[@]}" --check
echo "==> ump ${serve[*]} --poll 2s (log: $WORK/serve.log)"
"${ump[@]}" -v "${serve[@]}" --poll 2s >"$WORK/serve.log" 2>&1 &
serve_pid=$!
until_ok "status of the PXE server" serving

reinstall=(server reinstall pxe/pxe-test --unattended --profile pxe-test --no-provision --yes)
rlog="$WORK/reinstall.log"
echo "==> ump ${reinstall[*]} -o json (log: $rlog)"
mark
"${ump[@]}" "${reinstall[@]}" -o json >"$WORK/reinstall.json" 2>"$rlog" &
reinstall_pid=$!
until_ok "request to power-cycle pxe-test" said '^Power-cycle pxe-test now'
check "ump server reinstall: blank disks alone, as no network boot of pxe-test reported its disks" \
	grep -q '^The installation on pxe-test may erase blank disks alone (no network boot of pxe-test reported its disks)' "$rlog"
check "ump server reinstall: armed, and the PXE server is ready to boot it" \
	grep -q 'PXE server .* of site lab is ready to boot it' "$rlog"
check "ump server reinstall: no system to reboot over SSH (the machine is off)" \
	grep -q '^Checking root access to 10.0.2.15 over SSH, for the reboot ... no' "$rlog"
arm_file="$home/.config/ump/.pxe/lab/arm/pxe-test.json"
outcome_file="$home/.config/ump/.pxe/lab/outcome/pxe-test.json"
arm="$(jq -r .id "$arm_file")"
check "the arm ${arm:0:8}: an installation of $OS as pxe-test, for $FIRMWARE, blank disks" \
	jq -e --arg os "$OS" --arg fw "$FIRMWARE" '.mode == "install" and .spec.os == $os and .spec.hostname == "pxe-test" and .firmware == $fw and .disks.rule == "blank"' "$arm_file"

echo "==> power on: $ARCH $FIRMWARE, $([ "$FIRMWARE" = uefi ] && echo "iPXE $ipxe from $ipxe_dir" || echo "QEMU's iPXE ROM"), MAC $mac, a blank ${DISK} GB disk (serial log: $WORK/serial.log)"
boot
until_ok "claim of the installation" logged "pxe-test: the installation (arm ${arm:0:8}) claimed by boot"
boot_id="$(jq -r .boot "$outcome_file")"
echo "the rescue system of boot ${boot_id:0:8} claimed the installation"
until_ok "report of the installation" logged "pxe-test: installed (arm ${arm:0:8}); it boots its disk from now on"
echo "reported: installed"
mark
until_ok "network boot of pxe-test after the installation answered with the disk" logged 'pxe-test: local$'
[ "$FIRMWARE" = uefi ] && check "console: iPXE says the disk, and exits" grep -q 'ump: pxe-test: local' "$WORK/serial.log"
until_ok "end of ump server reinstall" gone "$reinstall_pid"
wait "$reinstall_pid" && rc=0 || rc=$?
reinstall_pid=""
[ "$rc" = 0 ] || { cat "$rlog"; fail "ump server reinstall: exit status $rc"; }
check "ump server reinstall: it followed the boot, the claim and the installer's output" \
	grep -q "^PXE server .* claimed the installation of pxe-test for its boot $boot_id" "$rlog"
check "ump server reinstall: the installer said it installed $OS" grep -q "^==> installed: $OS" "$rlog"
check "ump server reinstall: the new system up at 10.0.2.15, through the jump host, as ump: $(sed -n 's/^pxe-test is up: \([^,]*\),.*/\1/p' "$rlog")" \
	grep -q '^pxe-test is up: .*, hostname pxe-test, root on .*; log in as ump' "$rlog"
check "ump server reinstall: the result says unattended, rebooted, administrator ump" \
	jq -e '.via == "unattended" and .rebooted and .admin == "ump" and .system.name != ""' "$WORK/reinstall.json"
check "recorded: the arm and its outcome are gone" test ! -e "$arm_file" -a ! -e "$outcome_file"
installed() { ump server list -o json | jq -r '.[] | select(.name == "pxe-test") | "\(.os) \(.installed_at)"'; }
first="$(installed)"
check "recorded: ump server list has pxe-test with $OS, installed at ${first#* }" test "${first% *}" = "$OS"
check "ump config pxe list: pxe-test is local again" \
	jq -e '.[] | select(.name == "pxe-test") | .status == "local"' <<<"$(ump config pxe list -o json)"
ump pxe status --log pxe-test >"$WORK/install.log" 2>/dev/null
check "ump pxe status --log: the installer's output, $(wc -l < "$WORK/install.log" | tr -d ' ') lines" grep -q "^==> installed: $OS" "$WORK/install.log"

echo "==> the new system, by ump ssh through the jump host"
ump ssh pxe-test -- "OS=$OS UPLINK=$UPLINK bash -s" <<'CHECK'
ok() { local what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
. /etc/os-release
ok "system: $PRETTY_NAME, kernel $(uname -r)" grep -q "^os=$OS$" /etc/ump/install
ok "hostname: $(hostname)" [ "$(hostname)" = pxe-test ]
ok "sudo: without a password" sudo -n true
ok "userdata: the entry left its mark (/etc/ump-userdata-test)" [ "$(cat /etc/ump-userdata-test)" = merged ]
ok "firewall: input policy drop" bash -c 'sudo -n nft list ruleset | grep -q "policy drop"'
ok "time zone: Europe/Berlin from the profile" [ "$(cat /etc/timezone 2>/dev/null)" = Europe/Berlin ]
ok "uplink: $(grep '^network=' /etc/ump/install) in the install record" grep -q "^network=$UPLINK$" /etc/ump/install
if [ "$UPLINK" = dhcp ]; then
	ok "uplink: 10.0.2.15/24 by DHCP" bash -c "ip -4 -o addr show scope global | grep dynamic | grep -q ' 10.0.2.15/24 '"
else
	ok "uplink: 10.0.2.15/24 static" bash -c "ip -4 -o addr show scope global | grep -v dynamic | grep -q ' 10.0.2.15/24 '"
fi
ok "resolver: 10.0.2.3 alone" [ "$(resolvectl dns | sed 's/^[^:]*://' | tr ' ' '\n' | sed '/^$/d' | sort -u | paste -sd, -)" = 10.0.2.3 ]
CHECK
first_record="$(ump ssh pxe-test -- cat /etc/ump/install)"

rlog="$WORK/refused.log"
echo "==> a second unattended installation, for blank disks: the disk guard refuses it (log: $rlog)"
mark
"${ump[@]}" "${reinstall[@]}" >/dev/null 2>"$rlog" &
reinstall_pid=$!
until_ok "check of root access to the new system" said '^Checking root access to 10.0.2.15 over SSH, for the reboot ... [a-z]'
check "ump server reinstall: blank disks alone, as the disk seen at the last boot has no WWN or serial number" \
	grep -q '^The installation on pxe-test may erase blank disks alone (/dev/vda, seen at its last network boot, has neither a WWN nor a serial number)' "$rlog"
check "ump server reinstall: the new system lets UMP in, and is rebooted over SSH" \
	grep -q '^Checking root access to 10.0.2.15 over SSH, for the reboot ... yes; rebooting it as ump' "$rlog"
arm="$(jq -r .id "$arm_file")"
check "the arm ${arm:0:8}: blank disks" jq -e '.disks.rule == "blank"' "$arm_file"
until_ok "claim of the second installation" logged "pxe-test: the installation (arm ${arm:0:8}) claimed by boot"
until_ok "refusal of the second installation" logged "pxe-test: the installation (arm ${arm:0:8}) failed with exit status 2: /dev/vda has partitions"
until_ok "end of ump server reinstall" gone "$reinstall_pid"
wait "$reinstall_pid" && rc=0 || rc=$?
reinstall_pid=""
[ "$rc" = 1 ] || { cat "$rlog"; fail "ump server reinstall: exit status $rc, not 1"; }
check "ump server reinstall: refused, nothing written, nothing recorded, exit status 1" \
	grep -q 'the installation of pxe-test was refused (exit status 2: /dev/vda has partitions, .*); nothing was written, nothing is recorded' "$rlog"
check "console: the rescue system says why, and stays" grep -q 'the unattended installation is refused: /dev/vda has partitions, .*; nothing was written, the machine stays in the rescue system' "$WORK/serial.log"
check "the machine is in the rescue system still, with the reason for whoever logs in" gssh 'test -f /etc/ump-rescue-version && grep -q "the unattended installation is refused" /etc/motd'
check "the outcome: failed, with the arm" jq -e --arg a "$arm" '.arm == $a and .phase == "failed" and .exit == 2' "$outcome_file"
check "ump config pxe list: pxe-test failed" \
	jq -e '.[] | select(.name == "pxe-test") | .status == "failed"' <<<"$(ump config pxe list -o json)"
check "nothing recorded: ump server list has the first installation" test "$(installed)" = "$first"

echo "==> power-cycle: the outcome is written, so the disk"
power_off
mark
boot
until_ok "network boot of pxe-test after the refusal answered with the disk" logged 'pxe-test: local$'
until_ok "the first installation over ump ssh" ump ssh pxe-test -- true
check "the first installation, untouched: /etc/ump/install as it was" test "$(ump ssh pxe-test -- cat /etc/ump/install)" = "$first_record"
ump ssh pxe-test -- 'sudo -n systemctl poweroff' 2>/dev/null || true
power_off
ump pxe status

echo "==> stopping the server"
kill -TERM "$serve_pid"
wait "$serve_pid" && rc=0 || rc=$?
serve_pid=""
[ "$rc" = 0 ] || { tail -n 20 "$WORK/serve.log"; fail "ump pxe serve: exit status $rc after SIGTERM"; }
check "ump pxe serve: stopped" grep -q 'pxe: lab: stopped$' "$WORK/serve.log"
ok=1
echo "==> ok"
