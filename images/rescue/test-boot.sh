#!/usr/bin/env bash
# Boot a built rescue image in QEMU and check it over SSH: the setup from
# the kernel command line, sshd with the injected key, the tools the UMP
# agent needs; then the entry by kexec, and a network boot with a boot
# config in front of the image against a stand-in PXE server. Needs
# qemu-system-<arch> and ssh-keygen on the host, python3 for the kexec
# test, python3 and jq for the network boot.
#
# Usage: test-boot.sh [amd64|arm64]   (uses out/<arch>, see build.sh)
# Environment: MEM (MB, default 2048), TIMEOUT, PORT, KEXEC_TEST=0 to skip
# the kexec test, KEXEC_DISK=1 to keep its download on the virtual disk,
# PXE_TEST=0 to skip the network boot.
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/$ARCH}"
PORT="${PORT:-2222}"
MEM="${MEM:-2048}"
TIMEOUT="${TIMEOUT:-300}"
WORK="$(mktemp -d /tmp/ump-rescue-test.XXXXXX)"
qemu_pid="" pxe_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$pxe_pid" ] && kill "$pxe_pid" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

[ -f "$OUT/vmlinuz" ] && [ -f "$OUT/initrd.zst" ] || { echo "no image in $OUT; run build.sh $ARCH first" >&2; exit 2; }
ssh-keygen -q -t ed25519 -N '' -C ump-rescue-test -f "$WORK/key"
keys_b64="$(base64 < "$WORK/key.pub" | tr -d '\n')"
host_os="$(uname -s)"

case "$ARCH" in
amd64)
	qemu=qemu-system-x86_64
	console=ttyS0
	machine=(-M q35)
	[ "$host_os" = Linux ] && [ "$(uname -m)" = x86_64 ] && [ -w /dev/kvm ] && machine+=(-accel kvm -cpu host)
	;;
arm64)
	qemu=qemu-system-aarch64
	console=ttyAMA0
	machine=(-M virt)
	if [ "$host_os" = Darwin ] && [ "$(uname -m)" = arm64 ]; then machine+=(-accel hvf -cpu host)
	elif [ "$host_os" = Linux ] && [ "$(uname -m)" = aarch64 ] && [ -w /dev/kvm ]; then machine+=(-accel kvm -cpu host)
	else machine+=(-cpu cortex-a72); fi
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
command -v "$qemu" >/dev/null || { echo "$qemu not found" >&2; exit 2; }

cmdline="console=$console ump.hostname=ump-rescue-test ump.authorized_keys=$keys_b64"
truncate -s 1G "$WORK/disk.img"
echo "==> booting $OUT in $qemu (${MEM}M, ssh on 127.0.0.1:$PORT); serial log: $WORK/serial.log"
"$qemu" "${machine[@]}" -m "$MEM" -smp 2 -nographic \
	-kernel "$OUT/vmlinuz" -initrd "$OUT/initrd.zst" -append "$cmdline" \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device virtio-net-pci,netdev=n0 \
	-drive "file=$WORK/disk.img,format=raw,if=virtio" \
	-serial "file:$WORK/serial.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
qemu_pid=$!

# keepalives: a kexec replaces the kernel without closing TCP connections,
# and the client must notice instead of waiting for the TCP timeout
ssh_opts=(-i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
	-o ConnectTimeout=5 -o ServerAliveInterval=3 -o ServerAliveCountMax=3 -p "$PORT" root@127.0.0.1)
deadline=$(( $(date +%s) + TIMEOUT ))
until ssh "${ssh_opts[@]}" true 2>/dev/null; do
	if ! kill -0 "$qemu_pid" 2>/dev/null; then echo "qemu exited:"; cat "$WORK/qemu.err"; tail -n 30 "$WORK/serial.log"; exit 1; fi
	if [ "$(date +%s)" -gt "$deadline" ]; then echo "no SSH within ${TIMEOUT}s; last serial output:"; tail -n 40 "$WORK/serial.log"; exit 1; fi
	sleep 3
done
echo "==> SSH is up; checking the rescue system"
ssh "${ssh_opts[@]}" 'set -e
echo "hostname: $(hostname)   kernel: $(uname -r)   ump-rescue: $(cat /etc/ump-rescue-version)"
echo "root fs: $(findmnt -n -o FSTYPE /)   memory: $(free -m | awk "/Mem:/ {print \$2}") MB   mounted disks: $(lsblk -n -o MOUNTPOINTS /dev/vda | grep -c . || true)"
missing=""
# what the operator wants, and every command install/install.sh relies on
for t in sgdisk parted mdadm lvm cryptsetup wipefs mkfs.ext4 mkfs.xfs mkfs.vfat mkfs.btrfs nvme smartctl unsquashfs zstd tar curl rsync kexec efibootmgr ipmitool jq dmidecode lshw ip ssh-keygen \
	lsblk partprobe udevadm pvcreate vgcreate lvcreate pvscan vgchange swapoff mount umount mountpoint blkid chroot dd sha256sum mktemp nproc base64 findmnt resolvectl; do
	command -v "$t" >/dev/null || missing="$missing $t"
done
[ -z "$missing" ] && echo "tools: all present" || { echo "missing tools:$missing"; exit 1; }
# the drivers of server NICs and storage load (without their hardware, a
# driver loads and binds nothing); QEMU only has virtio, so this is what
# catches a module left without its dependencies (igb without dca)
broken=""
for m in igb igc e1000e ixgbe i40e ice bnxt_en tg3 bnx2 bnx2x r8169 mlx4_en mlx5_core qede be2net sfc atlantic ena \
	nvme ahci megaraid_sas mpt3sas aacraid hpsa smartpqi raid1 raid10 raid456; do
	modprobe "$m" 2>/dev/null || broken="$broken $m"
done
[ -z "$broken" ] && echo "drivers: all load" || { echo "drivers that do not load:$broken"; exit 1; }
# the installer checks the firewall of a new system with its nft -c,
# which needs nf_tables in this kernel
modprobe nf_tables && modprobe nft_ct && echo "nf_tables: loads (for nft -c of the new system)" || { echo "nf_tables does not load"; exit 1; }
echo "network: $(ip -br -4 addr show scope global | awk "{print \$1, \$3}" | paste -sd" " -)"
echo "sshd: $(systemctl is-active ssh)   setup: $(systemctl is-active ump-rescue-setup)   resolved: $(systemctl is-active systemd-resolved)"
systemctl --failed --no-legend | sed "s/^/failed unit: /"
echo "firmware dirs: $(ls /lib/firmware | wc -l)   modules: $(find /lib/modules -name "*.ko*" | wc -l)"
'
# The real entry path: inside the guest, kexec-into-rescue.sh fetches the
# image from this host, verifies it, carries the network configuration
# over and kexecs into it; afterwards the static configuration must be
# in effect and the same key must still open the door.
KEXEC_TEST="${KEXEC_TEST:-1}"
if [ "$KEXEC_TEST" = 1 ] && command -v python3 >/dev/null; then
	http_port=$(( PORT + 1 ))
	python3 -m http.server --bind 127.0.0.1 --directory "$(dirname "$OUT")" "$http_port" >/dev/null 2>&1 &
	http_pid=$!
	trap 'kill "$http_pid" 2>/dev/null; cleanup' EXIT
	boot_id="$(ssh "${ssh_opts[@]}" cat /proc/sys/kernel/random/boot_id)"
	echo "==> kexec test: downloading the image from the host and kexecing into it"
	# KEXEC_DISK=1 keeps the download on the virtual disk, as on a machine
	# that jumps from a system installed on disk (not from a rescue system
	# in RAM, which holds the download in memory too)
	if [ "${KEXEC_DISK:-0}" = 1 ]; then
		ssh "${ssh_opts[@]}" 'mkfs.ext4 -q -F /dev/vda && mkdir -p /var/tmp && mount /dev/vda /var/tmp' || { echo "could not put /var/tmp on the disk"; exit 1; }
	fi
	scp -q -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT" "$HERE/kexec-into-rescue.sh" root@127.0.0.1:/tmp/
	ssh "${ssh_opts[@]}" "UMP_RESCUE_URL=http://10.0.2.2:$http_port /tmp/kexec-into-rescue.sh -a $ARCH -H ump-rescue-kexec --no-exec" || { echo "kexec-into-rescue.sh failed"; exit 1; }
	# the command that replaces the kernel is detached from the session
	ssh "${ssh_opts[@]}" "nohup sh -c 'sleep 1; kexec -e' >/dev/null 2>&1 </dev/null &" || true
	sleep 5
	deadline=$(( $(date +%s) + TIMEOUT ))
	until [ "$(ssh "${ssh_opts[@]}" cat /proc/sys/kernel/random/boot_id 2>/dev/null)" != "$boot_id" ] && ssh "${ssh_opts[@]}" true 2>/dev/null; do
		if ! kill -0 "$qemu_pid" 2>/dev/null; then echo "qemu exited during kexec:"; tail -n 30 "$WORK/serial.log"; exit 1; fi
		if [ "$(date +%s)" -gt "$deadline" ]; then echo "no new boot within ${TIMEOUT}s; last serial output:"; tail -n 40 "$WORK/serial.log"; exit 1; fi
		sleep 3
	done
	echo "==> back after kexec; checking the carried-over configuration"
	ssh "${ssh_opts[@]}" 'set -e
echo "hostname: $(hostname)   boot: $(cat /proc/sys/kernel/random/boot_id | cut -c1-8)"
grep -q "^Address=10.0.2.15/24" /etc/systemd/network/10-ump-static.network && echo "static network file: present"
echo "address: $(ip -br -4 addr show scope global | awk "{print \$1, \$3}")   gateway: $(ip -4 route show default | awk "{print \$3}")"
echo "dns: $(resolvectl dns 2>/dev/null | awk "NF > 2 {print \$NF}" | paste -sd, -)"
systemctl --failed --no-legend | sed "s/^/failed unit: /"
'
fi

echo "==> powering off"
ssh "${ssh_opts[@]}" 'systemctl poweroff' 2>/dev/null || true
for _ in $(seq 1 20); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
kill "$qemu_pid" 2>/dev/null || true
qemu_pid=""

# A network boot as a PXE server makes one: nothing of UMP on the kernel
# command line, a boot config in a cpio in front of the image (hostname,
# boot NIC, the lease's resolvers, the clock, the server, root's keys,
# the token), two NICs of which the first is the boot's, and a clock
# years behind. A stand-in for the server (Python) takes the hello, the
# logs and the reports, and hands out one specification per request:
# 1 pins a disk the machine lacks (refused), 2 is not there (claimed
# by another boot), 3 takes blank disks when the disk has a file system
# (refused), 4 wants the other firmware (refused), 5 takes blank disks
# with an installer that fails, 6 the disks seen (the hello's
# guard_disks) with one that succeeds, after which the machine reboots,
# and 7 is not there.
# The installers are stand-ins too, served as the storage's.
PXE_TEST="${PXE_TEST:-1}"
if [ "$PXE_TEST" = 1 ] && command -v python3 >/dev/null && command -v jq >/dev/null; then
	pxe="$WORK/pxe"
	pxe_port=$(( PORT + 2 ))
	server="http://10.0.2.2:$pxe_port"
	boot_mac=52:54:00:12:34:56 other_mac=52:54:00:12:34:57
	token="$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
	mkdir -p "$pxe/s-fail/install" "$pxe/s-ok/install"
	for r in fail ok; do
		cat > "$pxe/s-$r/install/install.sh" <<STUB
#!/bin/bash
# a stand-in for install.sh: says what it got, records, and $([ $r = ok ] && echo succeeds || echo fails)
while [ \$# -gt 0 ]; do case "\$1" in --spec) spec=\$2; shift 2 ;; --record) record=\$2; shift 2 ;; *) shift ;; esac; done
echo "==> stand-in installer: disks \$(jq -c .disks "\$spec")"
seq -f 'output line %.0f of a long installation' 1 40000
mkdir -p "\$record" && printf 'os=stand-in\nnetwork=dhcp\n' > "\$record/install" && cp /etc/ssh/ssh_host_ed25519_key.pub "\$record/"
exit $([ $r = ok ] && echo 0 || echo 1)
STUB
	done
	jq -n --arg s "$server/s-fail" '{spec: {os: "stand-in", storage: $s}, disks: {rule: "pinned", names: ["virtio-ump-missing"]}}' > "$pxe/spec1.json"
	jq -n --arg s "$server/s-fail" '{spec: {os: "stand-in", storage: $s}, disks: {rule: "blank"}}' > "$pxe/spec3.json"
	cp "$pxe/spec3.json" "$pxe/spec5.json"
	cat > "$WORK/pxe-server.py" <<'PY'
# A stand-in PXE server: the API the rescue system's ump-pxe talks to.
import http.server, os, sys
port, root, token = int(sys.argv[1]), sys.argv[2], sys.argv[3]
specs = 0
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def reply(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def authorized(self):
        return self.headers.get("Authorization") == "Bearer " + token
    def do_GET(self):
        global specs
        if self.path == "/api/v1/spec":
            if not self.authorized():
                return self.reply(401)
            specs += 1
            f = os.path.join(root, "spec%d.json" % specs)
            return self.reply(200, open(f, "rb").read()) if os.path.exists(f) else self.reply(409)
        f = os.path.join(root, self.path.lstrip("/"))
        if ".." in self.path or not os.path.isfile(f):
            return self.reply(404)
        self.reply(200, open(f, "rb").read())
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if not self.authorized():
            return self.reply(401)
        path, _, query = self.path.partition("?")
        if path == "/api/v1/log":
            offset = int(query.partition("offset=")[2] or 0)
            log = os.path.join(root, "log")
            open(log, "ab").close()
            if offset > os.path.getsize(log):
                return self.reply(416)
            # a part sent again changes nothing, and the log never shrinks
            with open(log, "r+b") as f:
                f.seek(offset)
                f.write(body)
            return self.reply(200)
        if path not in ("/api/v1/hello", "/api/v1/report"):
            return self.reply(404)
        name = path.rsplit("/", 1)[1]
        n = len([x for x in os.listdir(root) if x.startswith(name + ".")]) + 1
        open(os.path.join(root, "%s.%d.json" % (name, n)), "wb").write(body)
        self.reply(200)
http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
	python3 "$WORK/pxe-server.py" "$pxe_port" "$pxe" "$token" >/dev/null 2>&1 &
	pxe_pid=$!
	# the boot config: an uncompressed newc cpio of /ump, owned by root,
	# padded to 512 bytes, in front of initrd.zst
	python3 - "$WORK/boot.cpio" "ump.hostname=ump-rescue-pxe ump.boot-mac=$boot_mac ump.dns=dhcp ump.time=$(date +%s) ump.pxe=$server ump.pxe.boot=test-boot ump.pxe.mode=install" "$WORK/key.pub" "$token" <<'PY'
import sys, time
out, cmdline, keys, token = sys.argv[1], sys.argv[2], open(sys.argv[3], "rb").read(), sys.argv[4]
data = bytearray()
def add(ino, name, body, mode):
    n = name.encode() + b"\0"
    data.extend(b"070701" + b"".join(b"%08X" % v for v in (ino, mode, 0, 0, 1, int(time.time()), len(body), 0, 0, 0, 0, len(n), 0)) + n)
    data.extend(b"\0" * (-len(data) % 4) + body)
    data.extend(b"\0" * (-len(data) % 4))
add(1, "ump", b"", 0o40700)
add(2, "ump/cmdline", cmdline.encode() + b"\n", 0o100644)
add(3, "ump/authorized_keys", keys, 0o100600)
add(4, "ump/token", token.encode() + b"\n", 0o100600)
add(5, "TRAILER!!!", b"", 0)
data.extend(b"\0" * (-len(data) % 512))
open(out, "wb").write(data)
PY
	cat "$WORK/boot.cpio" "$OUT/initrd.zst" > "$WORK/initrd-pxe"
	rm -f "$WORK/disk.img" && truncate -s 1G "$WORK/disk.img"
	echo "==> network boot: a boot config in front of the image, two NICs, the clock in 2020; stand-in PXE server on 127.0.0.1:$pxe_port"
	"$qemu" "${machine[@]}" -m "$MEM" -smp 2 -nographic -rtc base=2020-01-01T00:00:00 \
		-kernel "$OUT/vmlinuz" -initrd "$WORK/initrd-pxe" -append "console=$console" \
		-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device "virtio-net-pci,netdev=n0,mac=$boot_mac" \
		-netdev user,id=n1 -device "virtio-net-pci,netdev=n1,mac=$other_mac" \
		-drive "file=$WORK/disk.img,format=raw,if=none,id=d0" -device virtio-blk-pci,drive=d0,serial=ump-test-disk \
		-serial "file:$WORK/serial-pxe.log" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
	# wait_for FILE WHAT: until the stand-in server has FILE
	wait_for() {
		local deadline=$(( $(date +%s) + TIMEOUT ))
		until [ -f "$pxe/$1" ]; do
			kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited:"; tail -n 30 "$WORK/serial-pxe.log"; exit 1; }
			[ "$(date +%s)" -le "$deadline" ] || { echo "no $2 within ${TIMEOUT}s; last serial output:"; tail -n 40 "$WORK/serial-pxe.log"; exit 1; }
			sleep 2
		done
	}
	check() { # check WHAT COMMAND...: say so, or fail
		local what="$1"; shift
		if "$@" >/dev/null; then echo "$what"; else echo "FAILED: $what"; exit 1; fi
	}
	wait_for hello.1.json "hello"
	check "hello: taken, with the token (boot $(jq -r .boot "$pxe/hello.1.json"), $(jq -r .firmware "$pxe/hello.1.json"), $(jq -r '.memory_mb' "$pxe/hello.1.json") MB, $(jq '.host_keys | length' "$pxe/hello.1.json") host keys, guard_disks $(jq -r '.guard_disks | map("\(.path) \(.id)") | join(", ")' "$pxe/hello.1.json"))" \
		jq -e --arg m "$boot_mac" '.boot == "test-boot" and .boot_mac == $m and (.host_keys | length) > 0 and .memory_mb > 0
			and ([.interfaces[] | select(.mac == $m) | .addresses[]] == ["10.0.2.15/24"]) and ([.disks[] | select(.serial == "ump-test-disk")] | length) == 1
			and .guard_disks == [{path: "/dev/vda", id: "serial:ump-test-disk"}]' "$pxe/hello.1.json"
	# the firmware this boot has, for the specifications that name one
	fw="$(jq -r .firmware "$pxe/hello.1.json")"; other_fw=uefi; [ "$fw" = uefi ] && other_fw=bios
	jq --arg f "$other_fw" '.firmware = $f' "$pxe/spec3.json" > "$pxe/spec4.json"
	# a seen arm takes the IDs of the hello's guard_disks, verbatim
	jq -n --arg s "$server/s-ok" --arg f "$fw" --argjson ids "$(jq '[.guard_disks[].id]' "$pxe/hello.1.json")" \
		'{spec: {os: "stand-in", storage: $s}, firmware: $f, disks: {rule: "seen", ids: $ids}}' > "$pxe/spec6.json"
	wait_for report.1.json "report of the first installation"
	check "spec 1, a pinned disk the machine lacks: refused ($(jq -r .reason "$pxe/report.1.json"))" \
		jq -e '.exit == 2 and (.reason | test("virtio-ump-missing"))' "$pxe/report.1.json"
	until ssh "${ssh_opts[@]}" true 2>/dev/null; do kill -0 "$qemu_pid" 2>/dev/null || exit 1; sleep 3; done
	boot_id="$(ssh "${ssh_opts[@]}" cat /proc/sys/kernel/random/boot_id)"
	ssh "${ssh_opts[@]}" "BOOT_MAC=$boot_mac OTHER_MAC=$other_mac bash -s" <<'CHECK'
ok() { local what="$1"; shift; if "$@" >/dev/null; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
nic() { ip -o link | awk -v m="$1" '$0 ~ m {sub(":", "", $2); print $2}'; }
boot="$(nic "$BOOT_MAC")" other="$(nic "$OTHER_MAC")"
ok "hostname: $(hostname), from the boot config (root's key too)" [ "$(hostname)" = ump-rescue-pxe ]
ok "boot config: /etc/ump/boot (0700), its token 0600" [ "$(stat -c %a /etc/ump/boot)-$(stat -c %a /etc/ump/boot/token)" = 700-600 ]
ok "boot NIC $boot: 10.0.2.15/24 by DHCP (20-ump-boot.network)" [ "$(ip -4 -o addr show dev "$boot" scope global | awk '{print $4}') $(networkctl status "$boot" --no-pager 2>/dev/null | sed -n 's/^ *Network File: //p')" = "10.0.2.15/24 /etc/systemd/network/20-ump-boot.network" ]
ok "other NIC $other: no address" [ -z "$(ip -4 -o addr show dev "$other" scope global)" ]
ok "resolvers: from the lease (10.0.2.3), no public ones" [ "$(resolvectl dns "$boot" | awk '{print $NF}')$(cat /etc/systemd/resolved.conf.d/ump.conf 2>/dev/null)" = 10.0.2.3 ]
ok "clock: set from ump.time ($(date -u +%FT%TZ))" journalctl -b -u ump-rescue-setup --no-pager -q --grep 'set from ump.time'
ok "refusal: in the message of the day" grep -q 'refused: the disk virtio-ump-missing' /etc/motd
ok "disk: untouched" [ -z "$(lsblk -nr -o NAME /dev/vda | tail -n +2)$(wipefs --noheadings /dev/vda)" ]
CHECK
	out="$(ssh "${ssh_opts[@]}" ump-pxe install 2>&1)" && rc=0 || rc=$?
	check "spec 2, not there: claimed by another boot, nothing written (exit $rc)" grep -q "claimed by another boot" <<< "$out"
	ssh "${ssh_opts[@]}" mkfs.ext4 -q -F /dev/vda
	ssh "${ssh_opts[@]}" ump-pxe install >/dev/null 2>&1 || true
	check "spec 3, blank disks, the disk has ext4: refused ($(jq -r .reason "$pxe/report.2.json"))" \
		jq -e '.exit == 2 and (.reason | test("/dev/vda is not blank \\(ext4\\)"))' "$pxe/report.2.json"
	ssh "${ssh_opts[@]}" wipefs -q -a /dev/vda
	ssh "${ssh_opts[@]}" ump-pxe install >/dev/null 2>&1 || true
	check "spec 4, the other firmware: refused ($(jq -r .reason "$pxe/report.3.json"))" \
		jq -e --arg f "$other_fw" '.exit == 2 and (.reason | test("armed for " + $f))' "$pxe/report.3.json"
	ssh "${ssh_opts[@]}" ump-pxe install >/dev/null 2>&1 || true
	check "spec 5, blank disks, an installer that fails: reported (exit $(jq .exit "$pxe/report.4.json"), $(wc -l < "$pxe/log" | tr -d ' ') lines of log), no reboot" \
		jq -e '.exit == 1 and (.record | test("os=stand-in")) and (.host_keys | length) == 1 and (.log_tail | test("output line 40000 "))' "$pxe/report.4.json"
	check "the log: the installer's whole output ($(wc -c < "$pxe/log" | tr -d ' ') bytes, in chunks of at most 1 MB), with the disks guarded" \
		grep -q '^==> stand-in installer: disks \["/dev/vda"\]' "$pxe/log"
	[ "$(grep -c '^output line [0-9]* of a long installation$' "$pxe/log")" = 40000 ] || { echo "FAILED: the log is not whole"; exit 1; }
	[ "$(ssh "${ssh_opts[@]}" cat /proc/sys/kernel/random/boot_id)" = "$boot_id" ] || { echo "FAILED: the machine rebooted after a failed installation"; exit 1; }
	ssh "${ssh_opts[@]}" ump-pxe install >/dev/null 2>&1 || true
	check "spec 6, the disks seen, an installer that succeeds: reported (exit $(jq .exit "$pxe/report.5.json"))" jq -e '.exit == 0' "$pxe/report.5.json"
	wait_for hello.2.json "hello after the reboot"
	check "rebooted after the acknowledgement, and the next boot said hello again" test -f "$pxe/hello.2.json"
	until ssh "${ssh_opts[@]}" true 2>/dev/null; do kill -0 "$qemu_pid" 2>/dev/null || exit 1; sleep 3; done
	[ "$(ssh "${ssh_opts[@]}" cat /proc/sys/kernel/random/boot_id)" != "$boot_id" ] || { echo "FAILED: no new boot"; exit 1; }
	ssh "${ssh_opts[@]}" 'systemctl --failed --no-legend | grep -v ump-pxe-install | sed "s/^/failed unit: /"; true'
	echo "==> powering off"
	ssh "${ssh_opts[@]}" 'systemctl poweroff' 2>/dev/null || true
	for _ in $(seq 1 20); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
fi
echo "==> ok"
