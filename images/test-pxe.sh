#!/usr/bin/env bash
# Boot a machine over the network in QEMU against ump pxe serve, in
# chain mode on QEMU's user network: its DHCP server stands for the
# site's and hands out http://10.0.2.2:PORT/boot.ipxe as the boot file,
# which iPXE fetches itself. iPXE is the distribution's EFI binary,
# started by the UEFI firmware (FIRMWARE=uefi), or QEMU's own iPXE ROM of
# the NIC, started by SeaBIOS (FIRMWARE=bios, amd64). In a home of its
# own, with ump built from this tree, it checks:
#  - ump server rescue --via provider arms a rescue boot of the machine,
#    waits for the PXE server to take it and says to power-cycle the
#    machine (QEMU starts then); it waits for the rescue system's hello
#    and logs in at the address the rescue system reported, through the
#    site's jump host (the rescue system itself, through QEMU's port
#    forward), with the host keys it reported pinned; ssh -W through
#    the jump host, as ump probes an installed system, gets the
#    greeting and ends;
#  - the rescue system has the boot config the server put in front of
#    the image (it booted by its MAC): its hostname, root's keys (UMP's,
#    staging.keys and its own rescue-keys, not another machine's), DHCP
#    on the NIC it booted from alone, with the lease's resolvers;
#  - ump server list has the machines at their configured addresses
#    alone, never at one learned at boot;
#  - the arm is consumed: the next boot gets the disk (iPXE exits);
#  - a MAC without a machine gets the disk too, and ump pxe status
#    --unknown lists it;
#  - with TFTP=1, ump's TFTP server hands the iPXE binary to the rescue
#    system, at this host's address on its network (QEMU answers TFTP to
#    10.0.2.2 itself).
#
# Usage: test-pxe.sh [arm64|amd64]   (default: this machine's)
# Needs qemu-system-<arch> with its firmware, go, python3, jq, curl, ssh
# and ssh-keygen, and the rescue image built for the architecture
# (images/rescue/out/<arch>, make rescue-image) with the feature
# boot-config. Environment: FIRMWARE (uefi, or bios on amd64; default
# uefi), IPXE_DIR (the distribution's iPXE binaries; default its ipxe
# package's directory, else Debian's package, downloaded and checked),
# MEM (MB, default 2048), PORT (default 2262, and the three after it),
# TIMEOUT (s, default 600; amd64 without KVM needs more), TFTP (1:
# ump pxe serve --tftp :69 too, which needs root on Linux).
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
FIRMWARE="${FIRMWARE:-uefi}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
RESCUE="$ROOT/images/rescue/out"
MEM="${MEM:-2048}"
PORT="${PORT:-2262}"
TIMEOUT="${TIMEOUT:-600}"
TFTP="${TFTP:-0}"
http_port=$((PORT + 1)) pxe_port=$((PORT + 2))
mac=52:54:00:12:34:56 other_mac=52:54:00:12:34:57 unknown_mac=52:54:00:12:34:ab
# Debian's ipxe, for machines without the distribution's: pinned and checked
IPXE_DEB="https://snapshot.debian.org/archive/debian/20251101T000000Z/pool/main/i/ipxe/ipxe_1.21.1%2Bgit20250501.dad20602%2Bdfsg-1_all.deb"
IPXE_DEB_SHA256=eeaca79dbde0fb86c102bf1d362c6f6a67ed0ddab38b3dd12e1b332339f5dd99
WORK="$(mktemp -d /tmp/ump-pxe-test.XXXXXX)"
qemu_pid="" serve_pid="" http_pid="" rescue_pid="" ok=0
cleanup() {
	for p in "$qemu_pid" "$rescue_pid" "$serve_pid" "$http_pid"; do
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
# until DESCRIPTION COMMAND...: wait for COMMAND to succeed, while QEMU
# (when it runs) and the server run
until_ok() {
	local what="$1" deadline=$(( $(date +%s) + TIMEOUT )); shift
	until "$@" >/dev/null 2>&1; do
		[ -z "$serve_pid" ] || kill -0 "$serve_pid" 2>/dev/null || { echo "ump pxe serve exited:"; tail -n 20 "$WORK/serve.log"; fail "$what"; }
		[ -z "$qemu_pid" ] || kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited:"; cat "$WORK/qemu.err"; tail -n 30 "$WORK/serial.log"; fail "$what"; }
		if [ "$(date +%s)" -gt "$deadline" ]; then
			echo "no $what within ${TIMEOUT}s; last lines of the server and the serial console:"
			tail -n 15 "$WORK/serve.log"; [ -f "$WORK/serial.log" ] && tail -n 30 "$WORK/serial.log" | tr -d '\033'
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
for t in "$qemu" go python3 jq curl ssh ssh-keygen; do
	command -v "$t" >/dev/null || { echo "$t not found" >&2; exit 2; }
done
manifest="$RESCUE/$ARCH/manifest.json"
[ -f "$manifest" ] || { echo "no rescue image in $RESCUE/$ARCH; make rescue-image RESCUE_ARCH=$ARCH" >&2; exit 2; }
jq -e '.features // [] | index("boot-config")' "$manifest" >/dev/null ||
	{ echo "the rescue image in $RESCUE/$ARCH has no boot-config; build it again (make rescue-image RESCUE_ARCH=$ARCH)" >&2; exit 2; }

echo "==> ump from this tree, a home of its own in $WORK"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/ump" ./cmd/ump)
home="$WORK/home"
mkdir -p "$home/.config/ump" "$WORK/bin"
# every SSH of the test, ump's and the jump host's, with the test's keys
# and known hosts alone: nothing of the user's ~/.ssh, which ssh reads
# whatever HOME says
cat > "$WORK/ssh_config" <<EOF
Host *
	IdentityFile $WORK/key
	IdentityFile $home/.config/ump/staging_ed25519
	IdentitiesOnly yes
	UserKnownHostsFile $WORK/known_hosts
	StrictHostKeyChecking no
	BatchMode yes
	LogLevel ERROR
	ConnectTimeout 5
	ServerAliveInterval 3
EOF
printf '#!/bin/sh\nexec %s -F %s "$@"\n' "$(command -v ssh)" "$WORK/ssh_config" > "$WORK/bin/ssh"
chmod +x "$WORK/bin/ssh"
# ump in the test's home and nothing of this environment; started in the
# background as "${ump[@]}" ... &, so that $! is ump's process
ump=(env -i HOME="$home" PATH="$WORK/bin:$PATH" TERM=dumb UMP_NO_UPDATE_CHECK=1 UMP_WAIT="${TIMEOUT}s" "$WORK/ump")
ump() { "${ump[@]}" "$@"; }
gssh() { ssh -F "$WORK/ssh_config" -p "$PORT" root@127.0.0.1 "$@"; }
gone() { ! kill -0 "$1" 2>/dev/null; }
# serving: the PXE server of site lab runs and serves the site.json there
serving() { ump pxe status -o json | jq -e '.servers[] | select(.site == "lab" and .running and .current)'; }
# answered ANSWER: pxe-test's last network boot got ANSWER, for no arm
answered() { ump pxe status pxe-test -o json | jq -e --arg a "$1" '.machines[0] | .answer == $a and (.arm // "") == ""'; }
# asked: ump server rescue asks to power-cycle the machine
asked() {
	grep -q '^Power-cycle pxe-test now' "$WORK/rescue.log" && return
	gone "$rescue_pid" && { cat "$WORK/rescue.log"; fail "ump server rescue ended before it armed the boot"; }
	return 1
}
# listed MAC: ump pxe status --unknown lists the MAC
listed() { ump pxe status --unknown -o json | jq -e --arg m "$1" '.unknown[] | select(.mac == $m)'; }
# greets ADDR: SSH at ADDR greets through the site's jump host, and the
# connection ends within 15 s, as ump's probe of an installed system
# behind a jump host has it (ssh -W, stdin at its end)
greets() {
	"$WORK/bin/ssh" -o BatchMode=yes -o ConnectTimeout=10 -p "$PORT" -W "$1:22" root@127.0.0.1 </dev/null >"$WORK/greeting" 2>/dev/null &
	local pid=$! i
	for i in $(seq 15); do gone "$pid" && break; sleep 1; done
	if ! gone "$pid"; then kill "$pid" 2>/dev/null; return 1; fi
	wait "$pid"; grep -q '^SSH-' "$WORK/greeting"
}
# the operator's key (staging.keys: every rescue system's, and the jump
# host's), the machine's own rescue key, and another machine's
for k in key key-machine key-other; do ssh-keygen -q -t ed25519 -N '' -C "$k" -f "$WORK/$k"; done

# the iPXE binary: the firmware's NIC ROM (QEMU's own) for BIOS, the
# distribution's EFI binary for UEFI
ipxe="" ipxe_dir=""
if [ "$FIRMWARE" = uefi ] || [ "$TFTP" = 1 ]; then
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
# boot MAC [NETDEV-OPTIONS]: power on a machine with that MAC on the
# boot NIC and a second NIC on a network of its own; the firmware and
# iPXE take the boot file from QEMU's DHCP server
boot() {
	local nic=(-netdev "user,id=n0,bootfile=http://10.0.2.2:$pxe_port/boot.ipxe${2:+,$2}")
	if [ "$FIRMWARE" = uefi ]; then
		nic+=(-device "virtio-net-pci,netdev=n0,mac=$1,romfile=" -kernel "$ipxe_dir/$ipxe")
	else
		nic+=(-device "virtio-net-pci,netdev=n0,mac=$1,bootindex=1")
	fi
	: > "$WORK/serial.log"
	"$qemu" "${machine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic "${nic[@]}" \
		-netdev user,id=n1 -device "virtio-net-pci,netdev=n1,mac=$other_mac,romfile=" \
		-serial "file:$WORK/serial.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
}
power_off() {
	kill "$qemu_pid" 2>/dev/null || true
	wait "$qemu_pid" 2>/dev/null || true
	qemu_pid=""
}

echo "==> the rescue image $(jq -r .version "$manifest") for $ARCH, served on 127.0.0.1:$http_port"
python3 -m http.server --bind 127.0.0.1 --directory "$RESCUE" "$http_port" >/dev/null 2>&1 &
http_pid=$!
cat > "$home/.config/ump/config.yaml" <<EOF
staging:
  image: http://127.0.0.1:$http_port
  keys: [$WORK/key.pub]
EOF
# the site's jump host is the rescue system itself, through QEMU's port
# forward: what ump reaches at 10.0.2.15 is the machine on QEMU's
# network, as on a private network behind a jump host
cat > "$home/.config/ump/pxe.yaml" <<EOF
# the machines of the test, on QEMU's user network
unknown: local
sites:
  lab:
    description: QEMU's user network
    jump: root@127.0.0.1:$PORT
machines:
  pxe-test:
    site: lab
    mac: $mac
    arch: $ARCH
    address: 10.0.2.15
    rescue-keys: [$WORK/key-machine.pub]
  pxe-other:
    site: lab
    mac: 52:54:00:12:34:99
    arch: $ARCH
    rescue-keys: [$WORK/key-other.pub]
EOF
ump config pxe apply
check "site.json of site lab: 2 machines, pxe-test with the keys of UMP, staging.keys and its rescue-keys" \
	jq -e '(.machines | length) == 2 and (.machines[] | select(.name == "pxe-test") | .keys | length) == 3' "$home/.config/ump/.pxe/lab/site.json"

serve=(pxe serve --site lab --http "127.0.0.1:$pxe_port" --url "http://10.0.2.2:$pxe_port")
[ "$TFTP" = 1 ] && serve+=(--tftp :69 --ipxe-dir "$ipxe_dir")
echo "==> ump ${serve[*]} --check"
ump "${serve[@]}" --check
echo "==> ump ${serve[*]} --poll 2s (log: $WORK/serve.log)"
"${ump[@]}" -v "${serve[@]}" --poll 2s >"$WORK/serve.log" 2>&1 &
serve_pid=$!
until_ok "status of the PXE server" serving

echo "==> ump server rescue pxe/pxe-test --via provider --yes (log: $WORK/rescue.log)"
"${ump[@]}" server rescue pxe/pxe-test --via provider --yes >"$WORK/rescue.log" 2>&1 &
rescue_pid=$!
until_ok "request to power-cycle pxe-test" asked
grep -q 'PXE server .* of site lab is ready to boot it' "$WORK/rescue.log" || { cat "$WORK/rescue.log"; fail "the PXE server took the arm"; }
arm="$(jq -r .id "$home/.config/ump/.pxe/lab/arm/pxe-test.json")"
echo "armed: $arm; the PXE server is ready"

echo "==> power on: $ARCH $FIRMWARE, $([ "$FIRMWARE" = uefi ] && echo "iPXE $ipxe from $ipxe_dir" || echo "QEMU's iPXE ROM"), MAC $mac (serial log: $WORK/serial.log)"
boot "$mac" "hostfwd=tcp:127.0.0.1:$PORT-:22"
until_ok "end of ump server rescue" gone "$rescue_pid"
wait "$rescue_pid" && rc=0 || rc=$?
rescue_pid=""
[ "$rc" = 0 ] || { cat "$WORK/rescue.log"; fail "ump server rescue: exit status $rc"; }
check "ump server rescue: the rescue system reported in, at 10.0.2.15, reached through the jump host with its host keys pinned" \
	grep -q 'The rescue system of pxe-test is up at 10.0.2.15 .* its host keys are pinned' "$WORK/rescue.log"
outcome="$home/.config/ump/.pxe/lab/outcome/pxe-test.json"
check "the arm consumed: its outcome says booted" jq -e --arg a "$arm" '.arm == $a and .phase == "booted"' "$outcome"
boot_id="$(jq -r .boot "$outcome")"
st="$(ump pxe status pxe-test -o json)"
check "ump pxe status: answer rescue, the hello of arm ${arm:0:8}, boot ${boot_id:0:8}: $(jq -r '.machines[0].hello | "\(.arch) \(.firmware), \(.memory_mb) MB, \(.interfaces | length) NICs"' <<<"$st")" \
	jq -e --arg a "$arm" --arg b "$boot_id" --arg arch "$ARCH" --arg fw "$FIRMWARE" --arg m "$mac" \
	'.machines[0] | .answer == "rescue" and .hello.arm == $a and .hello.boot == $b and .hello.arch == $arch and .hello.firmware == $fw and .hello.boot_mac == $m' <<<"$st"

expect_console="console=ttyS0,115200n8 console=tty0"; [ "$ARCH" = arm64 ] && expect_console=""
gssh "MAC=$mac OTHER=$other_mac URL=http://10.0.2.2:$pxe_port BOOT=$boot_id CONSOLE='$expect_console' \
	UMP_KEY=$(cut -d' ' -f2 "$home/.config/ump/staging_ed25519.pub") OPERATOR=$(cut -d' ' -f2 "$WORK/key.pub") \
	OWN=$(cut -d' ' -f2 "$WORK/key-machine.pub") FOREIGN=$(cut -d' ' -f2 "$WORK/key-other.pub") bash -s" <<'CHECK'
ok() { local what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
nic() { ip -o link | awk -v m="$1" '$0 ~ m {sub(":", "", $2); print $2}'; }
words=" $(cat /etc/ump/boot/cmdline) "
has() { case "$words" in *" $1 "*) return 0 ;; esac; return 1; }
config() { for w in ump.hostname=pxe-test "ump.boot-mac=$MAC" ump.dns=dhcp "ump.pxe=$URL" "ump.pxe.boot=$BOOT"; do has "$w" || return 1; done; ! has ump.pxe.mode=install; }
kernel() { ! grep -q 'ump\.' /proc/cmdline && [ "$(grep -o 'console=[^ ]*' /proc/cmdline | paste -sd' ' -)" = "$CONSOLE" ]; }
keys() { for k in "$UMP_KEY" "$OPERATOR" "$OWN"; do grep -qF "$k" /root/.ssh/authorized_keys || return 1; done; ! grep -qF "$FOREIGN" /root/.ssh/authorized_keys; }
boot="$(nic "$MAC")" other="$(nic "$OTHER")"
ok "hostname: $(hostname), from the boot config" [ "$(hostname)" = pxe-test ]
ok "boot config: ump.hostname, ump.boot-mac, ump.dns=dhcp, ump.pxe and ump.pxe.boot of this boot, no installation" config
ok "boot config: /etc/ump/boot (0700), its token 0600" [ "$(stat -c %a /etc/ump/boot)-$(stat -c %a /etc/ump/boot/token)" = 700-600 ]
ok "kernel command line: nothing of ump, the consoles ${CONSOLE:-of the firmware (none given)}" kernel
ok "root's keys: UMP's, staging.keys and the machine's rescue-keys, not another machine's" keys
ok "boot NIC $boot: 10.0.2.15/24 by DHCP (20-ump-boot.network)" [ "$(ip -4 -o addr show dev "$boot" scope global | awk '{print $4}') $(networkctl status "$boot" --no-pager 2>/dev/null | sed -n 's/^ *Network File: //p')" = "10.0.2.15/24 /etc/systemd/network/20-ump-boot.network" ]
ok "other NIC $other: no address" [ -z "$(ip -4 -o addr show dev "$other" scope global)" ]
ok "resolvers: the lease's (10.0.2.3), no public ones" [ "$(resolvectl dns "$boot" | awk '{print $NF}')$(cat /etc/systemd/resolved.conf.d/ump.conf 2>/dev/null)" = 10.0.2.3 ]
CHECK
if [ "$TFTP" = 1 ]; then
	# QEMU answers TFTP to 10.0.2.2 itself: the rescue system asks this
	# host at its address on its own network, through QEMU's NAT
	if [ "$(uname -s)" = Darwin ]; then
		host_ip="$(ipconfig getifaddr "$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')" 2>/dev/null || true)"
	else
		host_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')"
	fi
	if [ -z "$host_ip" ]; then
		echo "TFTP: not checked, this host has no address on a network"
	else
		want="$( (command -v sha256sum >/dev/null && sha256sum || shasum -a 256) < "$ipxe_dir/$ipxe" | cut -d' ' -f1)"
		check "TFTP: ump's server hands the rescue system $ipxe (at $host_ip, through QEMU's NAT)" \
			test "$(gssh "curl -fsS tftp://$host_ip/$ipxe | sha256sum | cut -d' ' -f1")" = "$want"
	fi
fi
check "the jump host's forward to SSH at 10.0.2.15 (ssh -W, ump's probe after a reinstall): it greets, and ends" greets 10.0.2.15
check "ump server list: pxe-test at its configured address, pxe-other (none configured) without one" \
	jq -e 'map({(.name): (.ipv4 // "")}) | add == {"pxe-test": "10.0.2.15", "pxe-other": ""}' <<<"$(ump server list -o json)"
ump server list

echo "==> reboot: the arm is consumed, so the disk"
gssh 'systemctl reboot' 2>/dev/null || true
until_ok "network boot of pxe-test after the reboot answered with the disk" grep -q "pxe-test: local\$" "$WORK/serve.log"
[ "$FIRMWARE" = uefi ] && check "console: iPXE says the disk, and exits" grep -q 'ump: pxe-test: local' "$WORK/serial.log"
until_ok "status of the boot after the reboot" answered local
echo "ump pxe status: answer local, the rescue boot of arm ${arm:0:8} done"
power_off

echo "==> a MAC without a machine: $unknown_mac"
boot "$unknown_mac"
until_ok "network boot of $unknown_mac" grep -q "$unknown_mac .*: no machine of pxe.yaml, the disk" "$WORK/serve.log"
[ "$FIRMWARE" = uefi ] && check "console: iPXE says it is no machine of the site, and exits" grep -q "ump: $unknown_mac is no machine of site lab" "$WORK/serial.log"
until_ok "unknown MAC in the status" listed "$unknown_mac"
echo "ump pxe status --unknown: $unknown_mac listed"
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
