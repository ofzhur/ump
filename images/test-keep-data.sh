#!/usr/bin/env bash
# Reinstall machines with their data kept (install/install.sh --keep-data)
# in QEMU and check the result: the UMP rescue image boots with UEFI
# firmware (or BIOS: FIRMWARE=bios, amd64) over disks that hold an old system, the installer shows its
# plan (--plan), refuses a hash that is not the plan's, installs the
# confirmed plan (--confirm), and the disk boots into the new system,
# which is checked over SSH as the administrator. Around every run the
# devices that are kept are read whole (sha256): not a byte of them may
# differ; around every plan and every refusal the whole disks are.
#
# The machines (default: all of them, in this order):
#   loop       no machine's disks: test-keep-loop.sh in the rescue system,
#              the installer's own looks and writes on file systems in
#              loop devices (what a look leaves of an unclean file system;
#              installations that fail half-way, and the next ones)
#   main       an old system installed by install.sh itself (OLD_OS) on a
#              layout with /boot, a volume group (root, /var, /data, /home,
#              swap) and a plain partition (/srv/plain), booted, given
#              marker files, bind mounts, an fstab entry whose device is
#              gone and a boot loader with a firmware entry of its own, as
#              a distribution's installer leaves one, and switched off
#              without a shutdown, as a reset into a rescue system does
#              it; then OS with --keep-data: /data, /srv/plain and swap as
#              they were, /home kept with the administrator's home
#              directory, /var at /var.old, the root and /boot new, the
#              firmware's stale entry harmless
#   zfs        the same machine, now running Ubuntu, gets a ZFS pool on a
#              second disk; reinstalled with --keep-data, the pool is
#              imported at boot (skipped where OS is not Ubuntu or the
#              archive cannot be reached)
#   keep-root  the same machine again, with --keep-root --format
#              /srv/plain and OLD_OS as the new system: the system before
#              is in /old-root-DATE, /srv/plain is empty, /data as ever
#   md-luks    a root on an md RAID1 over two disks, which the old system
#              called /dev/md0, and a second array it called by a name,
#              neither assembled when the installer starts (it assembles
#              them read-only, and makes writable what it writes to); /home
#              on a LUKS volume with a key file on the root, which the
#              installer cannot mount and the new system does (the
#              administrator gets in by the keys outside /home); a
#              partition the fstab named as the kernel did; an encrypted
#              swap of crypttab whose device holds a file system now
#   btrfs      the root in the subvolume @ of a btrfs, which is its default
#              subvolume, /home in @home with the administrator's home
#              directory from before, a snapshot of the root; then once
#              more with --keep-root
#   resume     an installation with --format /var that fails after the
#              root, /boot (named by its label) and /var were formatted
#              (the image is not there), and a second one that finds the
#              old system's fstab where the first left it, and /boot and
#              /var by the UUIDs the first wrote down
#   refusals   an encrypted root, no EFI system partition, two old
#              systems, a root too small, disks in use, a volume to format
#              that is not there, a /boot named as the kernel named it
#              that does not hold the old system's kernels, no
#              confirmation: each refused with status 2, the disks byte for
#              byte as before; and a root that --root names where no old
#              system is: planned, and marked as not known
#   mbr        (FIRMWARE=bios) an old system on a disk with an MBR
#              partition table, started by GRUB in the MBR; the new one
#              likewise
#
# Usage: test-keep-data.sh [arm64|amd64] [MACHINE...]
# Needs the rescue image in images/rescue/out/<arch> and the images of OS
# and OLD_OS in images/base/out. Environment: OS (default ubuntu-26.04),
# OLD_OS (default debian-13), MEM (MB, default 2048), PORT (default
# 2262), TIMEOUT (s, default 600), FIRMWARE (uefi, or bios on amd64: main
# and keep-root, whose old system ump installed with a BIOS boot
# partition, and mbr; the other machines made by hand have an ESP).
set -euo pipefail
ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
case "${1:-}" in arm64|amd64) ARCH="$1"; shift ;; esac
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
RESCUE="$ROOT/images/rescue/out/$ARCH"
OS="${OS:-ubuntu-26.04}"
OLD_OS="${OLD_OS:-debian-13}"
MEM="${MEM:-2048}"
PORT="${PORT:-2262}"
HTTP=$((PORT + 1))
TIMEOUT="${TIMEOUT:-600}"
FIRMWARE="${FIRMWARE:-uefi}"
MAC=52:54:00:12:34:56
machines=("$@")
case "$FIRMWARE" in
uefi) [ ${#machines[@]} -gt 0 ] || machines=(loop main zfs keep-root md-luks btrfs resume refusals) ;;
bios)
	[ "$ARCH" = amd64 ] || { echo "FIRMWARE=bios is for amd64" >&2; exit 2; }
	[ ${#machines[@]} -gt 0 ] || machines=(main keep-root mbr)
	for m in "${machines[@]}"; do case "$m" in main|keep-root|mbr) ;; *) echo "FIRMWARE=bios: main, keep-root and mbr alone" >&2; exit 2 ;; esac; done ;;
*) echo "FIRMWARE is uefi or bios" >&2; exit 2 ;;
esac
WORK="$(mktemp -d /tmp/ump-keep-data-test.XXXXXX)"
qemu_pid="" http_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$http_pid" ] && kill "$http_pid" 2>/dev/null || true
	echo "work directory (serial logs, plans): $WORK"
}
trap cleanup EXIT
fail() { echo "FAILED: $*"; exit 1; }
[ -f "$RESCUE/vmlinuz" ] || { echo "no rescue image in $RESCUE" >&2; exit 2; }
for o in "$OS" "$OLD_OS"; do
	[ -f "$HERE/base/out/$o/$ARCH/manifest.json" ] || { echo "no image at $HERE/base/out/$o/$ARCH; build it first (make os-image OS_LIST=$o OS_ARCH=$ARCH)" >&2; exit 2; }
done

case "$ARCH" in
arm64)
	qemu=qemu-system-aarch64; qmachine=(-M virt -cpu cortex-a72); fw=edk2-aarch64-code.fd; vars=edk2-arm-vars.fd
	[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] && qmachine=(-M virt -accel hvf -cpu host)
	;;
amd64)
	qemu=qemu-system-x86_64; qmachine=(-M q35 -cpu max); fw=edk2-x86_64-code.fd; vars=edk2-i386-vars.fd
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
code="$WORK/code.fd"; cp "$share/$fw" "$code"; [ "$ARCH" = arm64 ] && truncate -s 64M "$code"

# the storage: the images and the installer's files; broken/ has the
# manifests alone, for an installation that fails at the download
for o in "$OS" "$OLD_OS"; do
	mkdir -p "$WORK/storage/os/$o/$ARCH" "$WORK/storage/broken/os/$o/$ARCH"
	cp "$HERE/base/out/$o/$ARCH/manifest.json" "$WORK/storage/os/$o/$ARCH/"
	cp "$HERE/base/out/$o/$ARCH/manifest.json" "$WORK/storage/broken/os/$o/$ARCH/"
	ln -sf "$(cd "$HERE/base/out/$o/$ARCH" && pwd -P)/rootfs.tar.zst" "$WORK/storage/os/$o/$ARCH/rootfs.tar.zst"
done
mkdir -p "$WORK/storage/install" "$WORK/storage/broken/install"
cp "$ROOT/install/motd.sh" "$ROOT/install/bashrc.sh" "$WORK/storage/install/"
cp "$ROOT/install/motd.sh" "$ROOT/install/bashrc.sh" "$WORK/storage/broken/install/"
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/storage" "$HTTP" >/dev/null 2>&1 &
http_pid=$!

ssh-keygen -q -t ed25519 -N '' -C ump-keep-data-test -f "$WORK/key"
ssh_opts=(-i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
	-o ConnectTimeout=5 -o ServerAliveInterval=10)
r() { ssh "${ssh_opts[@]}" -p "$PORT" root@127.0.0.1 "$@"; }      # in the rescue system
u() { ssh "${ssh_opts[@]}" -p "$PORT" ump@127.0.0.1 "$@"; }       # in an installed system, as its administrator

# spec OS HOSTNAME [STORAGE] [LAYOUT]: a specification; the layout is the
# old system's (keep-data applies none)
spec() {
	local layout="${4:-}"
	[ -n "$layout" ] || layout='[{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "slice", "target": [".*"], "size": "rest", "filesystem": "lvm", "vg": "vg0"},
		{"type": "lvm", "vg": "vg0", "name": "root", "size": "rest", "filesystem": "ext4", "mountpoint": "/"}]'
	jq -n --arg key "$(cat "$WORK/key.pub")" --arg storage "http://10.0.2.2:$HTTP${3:+/$3}" --arg os "$1" --arg hostname "$2" --argjson parts "$layout" '{
		os: $os, hostname: $hostname, timezone: "UTC", storage: $storage,
		kernel: {mitigations: false, ipv6: false, extra: ""},
		layout: {name: "test", parts: $parts}, disks: [],
		users: [{name: "ump", sudo: true, groups: [], shell: "/bin/bash", keys: [$key]}],
		userdata: [], firewall: {policy: "accept", allow: []},
		network: {rename: true}, profile: {name: "keep-data-test"}}'
}

# machine NAME SIZE...: a machine of its own: firmware variables and disks
machine() {
	M="$WORK/$1"; shift
	mkdir -p "$M"
	cp "$share/$vars" "$M/vars.fd"; [ "$ARCH" = arm64 ] && truncate -s 64M "$M/vars.fd"
	drives=(); local n=0 s
	for s in "$@"; do
		n=$((n + 1)); truncate -s "$s" "$M/disk$n.img"
		drives+=(-drive "file=$M/disk$n.img,format=raw,if=virtio")
	done
	boots=0
}
# start rescue|disk: the machine in the rescue system (SSH as root), or
# from its disks (SSH as ump)
start() {
	local kernel=() firmware=() user=root deadline
	boots=$((boots + 1))
	[ "$FIRMWARE" = bios ] || firmware=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=$M/vars.fd")
	if [ "$1" = rescue ]; then
		kernel=(-kernel "$RESCUE/vmlinuz" -initrd "$RESCUE/initrd.zst"
			-append "console=ttyS0 console=ttyAMA0 ump.hostname=rescue ump.authorized_keys=$(base64 < "$WORK/key.pub" | tr -d '\n')")
	else
		user=ump
	fi
	"$qemu" "${qmachine[@]}" "${firmware[@]}" -m "$MEM" -smp 2 -nographic \
		"${kernel[@]}" -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device "virtio-net-pci,netdev=n0,mac=$MAC" "${drives[@]}" \
		-serial "file:$M/serial-$boots-$1.log" -monitor none -display none </dev/null >/dev/null 2>"$M/qemu.err" &
	qemu_pid=$!
	deadline=$(( $(date +%s) + TIMEOUT ))
	until ssh "${ssh_opts[@]}" -p "$PORT" "$user@127.0.0.1" true 2>/dev/null; do
		kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited"; cat "$M/qemu.err"; exit 1; }
		[ "$(date +%s)" -le "$deadline" ] || { echo "no SSH as $user within ${TIMEOUT}s ($1)"; tr '\r' '\n' < "$M/serial-$boots-$1.log" | tail -n 40; exit 1; }
		sleep 3
	done
	if [ "$1" = rescue ]; then
		scp -q "${ssh_opts[@]}" -P "$PORT" "$ROOT/install/install.sh" root@127.0.0.1:/tmp/
	else
		# the boot is over, whatever it leaves failed
		u 'systemctl is-system-running --wait >/dev/null 2>&1 || true'
	fi
}
# stop [off]: the machine is gone: switched off as a reset does it, or
# (off) shut down by its system first, the file systems unmounted
stop() {
	local n=0
	if [ "${1:-}" = off ]; then
		u 'sudo -n systemctl poweroff' 2>/dev/null || true
		while kill -0 "$qemu_pid" 2>/dev/null && [ "$n" -lt 120 ]; do sleep 1; n=$((n + 1)); done
	fi
	kill "$qemu_pid" 2>/dev/null || true; wait "$qemu_pid" 2>/dev/null || true; qemu_pid=""
}
put() { scp -q "${ssh_opts[@]}" -P "$PORT" "$1" "root@127.0.0.1:$2"; }
# sums DEVICE...: what the devices hold, byte for byte
sums() { r "vgchange -ay >/dev/null 2>&1; udevadm settle; sha256sum $*"; }

# keep OS HOSTNAME ARGS...: a keep-data installation as ump runs it: the
# plan, a confirmation that is not the plan's, then the plan confirmed.
# Before the plan and after the refusal the disks are the same; the
# plan is in $WORK/plan.json
keep() {
	local os="$1" host="$2" before rc hash; shift 2
	spec "$os" "$host" > "$WORK/spec.json"; put "$WORK/spec.json" /tmp/spec.json
	before="$(sums '/dev/vd?')"
	echo "==> install.sh --keep-data $* --plan ($os)"
	rc=0; r "bash /tmp/install.sh --spec /tmp/spec.json --keep-data $* --plan /tmp/plan.json" > "$WORK/plan.log" 2>&1 || rc=$?
	sed -n '/==> keep-data: the old system/,/==> plan:/p' "$WORK/plan.log"
	[ "$rc" = 0 ] || { tail -n 20 "$WORK/plan.log"; fail "the plan ended with status $rc"; }
	r 'cat /tmp/plan.json' > "$WORK/plan.json"
	hash="$(jq -r .hash "$WORK/plan.json")"
	[ "$(jq -cS 'del(.hash)' "$WORK/plan.json" | sha256sum | cut -d' ' -f1)" = "$hash" ] || fail "the plan's hash is not the sha256 of the plan"
	rc=0; r "bash /tmp/install.sh --spec /tmp/spec.json --keep-data $* --confirm $(printf '%064d' 0)" > "$WORK/refused.log" 2>&1 || rc=$?
	[ "$rc" = 2 ] && grep -q "not the one that was confirmed" "$WORK/refused.log" || { tail -n 5 "$WORK/refused.log"; fail "a confirmation that is not the plan's was not refused with status 2 (status $rc)"; }
	[ "$(sums '/dev/vd?')" = "$before" ] || fail "the plan, or the refused confirmation, changed a disk"
	echo "plan and refused confirmation: the disks are byte for byte as before"
	echo "==> install.sh --keep-data $* --confirm $hash"
	rc=0; r "bash /tmp/install.sh --spec /tmp/spec.json --keep-data $* --confirm $hash" > "$WORK/install.log" 2>&1 || rc=$?
	grep '^==> \(keep-data: writing\|formatting\|the old\|the default\|deleting\|crypttab\|packages\|users: \|ssh: the users\|installed\)\|install.sh' "$WORK/install.log" || true
	[ "$rc" = 0 ] && grep -q "^==> installed: $os" "$WORK/install.log" || { tail -n 30 "$WORK/install.log"; fail "the installation failed (status $rc)"; }
	! grep -q "findmnt --verify finds fault" "$WORK/install.log" || fail "the new system's findmnt finds fault with the new fstab"
	grep -q "the plan is made: stopping here" "$WORK/plan.log" || fail "the plan run does not end as a plan run"
}
# refused WHAT PATTERN ARGS...: install.sh ends with status 2 and says
# PATTERN, and no disk has changed
refused() {
	local what="$1" pattern="$2" before rc=0; shift 2
	before="$(sums '/dev/vd?')"
	r "bash /tmp/install.sh --spec /tmp/spec.json $*" > "$WORK/refused.log" 2>&1 || rc=$?
	[ "$rc" = 2 ] || { tail -n 5 "$WORK/refused.log"; fail "$what: status $rc, not 2"; }
	grep -q "$pattern" "$WORK/refused.log" || { tail -n 5 "$WORK/refused.log"; fail "$what: the refusal does not say \"$pattern\""; }
	[ "$(sums '/dev/vd?')" = "$before" ] || fail "$what: the refused run changed a disk"
	echo "refused, the disks byte for byte as before: $what ($(grep -m1 'refused:\|^install.sh: ' "$WORK/refused.log" | sed 's/^install.sh: //; s/^keep-data: //' | cut -c1-150))"
}
# check [VAR=VALUE...]: the script on stdin runs in the installed system
# as its administrator; ok WHAT TEST says WHAT when the shell's TEST
# holds, and fails the run when not
check() {
	{ cat <<'OK'
ok() { if eval "$2" >/dev/null 2>&1; then echo "$1"; else echo "FAILED: $1"; exit 1; fi; }
failed() { systemctl --failed --no-legend | awk '{print $1}' | paste -sd' ' -; }
OK
	cat; } | u "$* bash -s" || fail "the checks of the new system"
}
# layout: the script on stdin makes an old layout in the rescue system;
# old_root DIR NAME puts there what keep-data reads of an old system
# (os-release, a hostname) and something to lose; wipe empties the disks
layout() {
	{ cat <<'LIB'
set -e
old_root() {
	mkdir -p "$1/etc" "$1/home/alice" "$1/root" "$1/var/lib" "$1/boot" "$1/usr/lib"
	printf 'PRETTY_NAME="Old Linux 1.0"\nID=old\n' > "$1/usr/lib/os-release"; ln -s ../usr/lib/os-release "$1/etc/os-release"
	echo "$2" > "$1/etc/hostname"; dd if=/dev/urandom of="$1/home/alice/data" bs=1M count=3 status=none
}
wipe() {
	swapoff -a || true; umount /mnt 2>/dev/null || true; vgchange -an >/dev/null 2>&1 || true; mdadm --stop --scan >/dev/null 2>&1 || true
	for d in /dev/vd?; do wipefs -af "$d"?* "$d" >/dev/null 2>&1 || true; sgdisk --zap-all "$d" >/dev/null 2>&1 || true; done
	udevadm settle
}
uuid() { blkid -s UUID -o value "$1"; }
LIB
	cat; } | r 'bash -s' || fail "the old layout could not be made"
}

# --- main: LVM, a separate /var, a plain partition ---------------------------

m_main() {
	echo "=== main: an old $OLD_OS with LVM, /var, /home and a plain partition; $OS with --keep-data"
	local kept old_uuid
	machine main 12G 2G
	start rescue
	spec "$OLD_OS" old.example.net "" '[
		{"type": "slice", "target": ["first"], "size": "1M", "filesystem": "bios"},
		{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "slice", "target": ["first"], "size": "1G", "filesystem": "ext4", "mountpoint": "/boot"},
		{"type": "slice", "target": ["first"], "size": "1G", "filesystem": "ext4", "mountpoint": "/srv/plain"},
		{"type": "slice", "target": ["first"], "size": "rest", "filesystem": "lvm", "vg": "vg0"},
		{"type": "lvm", "vg": "vg0", "name": "swap", "size": "512M", "filesystem": "swap"},
		{"type": "lvm", "vg": "vg0", "name": "var", "size": "2G", "filesystem": "ext4", "mountpoint": "/var"},
		{"type": "lvm", "vg": "vg0", "name": "data", "size": "1G", "filesystem": "xfs", "mountpoint": "/data"},
		{"type": "lvm", "vg": "vg0", "name": "home", "size": "512M", "filesystem": "ext4", "mountpoint": "/home"},
		{"type": "lvm", "vg": "vg0", "name": "root", "size": "rest", "filesystem": "ext4", "mountpoint": "/"}]' \
		| jq '.disks = ["/dev/vda"]' > "$WORK/old.json"
	put "$WORK/old.json" /tmp/old.json
	echo "==> install.sh ($OLD_OS, the old system)"
	r 'bash /tmp/install.sh --spec /tmp/old.json' > "$WORK/old.log" 2>&1 || { tail -n 20 "$WORK/old.log"; fail "the old system could not be installed"; }
	stop
	start disk
	echo "==> the old system: markers, bind mounts, an entry whose device is gone, a boot loader with a firmware entry"
	u 'sudo -n bash -s' <<'OLD' || fail "the old system could not be prepared"
set -e
echo data-marker > /data/marker; echo plain-marker > /srv/plain/marker; echo var-marker > /var/lib/marker
echo home-marker > /home/ump/marker; chown ump: /home/ump/marker; echo etc-marker > /etc/marker; echo root-marker > /opt/marker
mkdir -p /data/shared /opt/app /srv/shared /srv/app; echo shared > /data/shared/file; echo app > /opt/app/file
cat >> /etc/fstab <<F
/data/shared  /srv/shared  none  bind  0 0
/opt/app  /srv/app  none  bind  0 0
UUID=0a0a0a0a-0b0b-0c0c-0d0d-0e0e0e0e0e0e  /mnt/gone  ext4  defaults,nofail  0 2
tmpfs  /srv/tmp  tmpfs  size=16M  0 0
F
# as a distribution installs GRUB: a directory of its own on the ESP, and
# an entry in the firmware that leads there, ahead of the disk itself
if [ -d /sys/firmware/efi ]; then
	grub-install --target="$(uname -m | sed 's/aarch64/arm64/')-efi" --efi-directory=/boot/efi --boot-directory=/boot --bootloader-id=debian --recheck >/dev/null 2>&1
	efibootmgr | grep -qi debian || { echo "no firmware entry for the old boot loader"; exit 1; }
fi
sync
OLD
	old_uuid="$(u 'findmnt -n -o UUID /')"
	# a reset, no shutdown: what was written so far is on the disks (a
	# freeze writes the journals out), what follows is in a journal that
	# nobody replays before the installer looks
	u 'sudo -n bash -c "for m in /data /srv/plain /var /home /; do fsfreeze -f \$m && fsfreeze -u \$m; done; cp -a /etc /opt/late; rm -rf /srv/app/x /opt/late/ssh; sync"' || fail "the old system could not be frozen"
	stop

	start rescue
	kept="$(sums /dev/mapper/vg0-data /dev/mapper/vg0-var /dev/mapper/vg0-swap /dev/vda4 /dev/vdb)"
	keep "$OS" new.example.net
	jq -e '.warnings | map(test("not unmounted cleanly")) | any' "$WORK/plan.json" >/dev/null || fail "the plan does not say that the old root was not unmounted cleanly"
	jq -e '[.format[] | "\(.device) \(.mountpoint)"] == ["/dev/mapper/vg0-root /", "/dev/vda3 /boot"]' "$WORK/plan.json" >/dev/null || fail "the plan formats something else than the root and /boot"
	jq -e '([.keep[] | "\(.device) \(.mountpoint)"] | contains(["/dev/vda2 /boot/efi", "/dev/mapper/vg0-data /data", "/dev/vda4 /srv/plain", "/dev/mapper/vg0-swap swap"]))
		and ([.aside[] | "\(.device) \(.mountpoint)"] == ["/dev/mapper/vg0-var /var.old"])
		and ([.keep[] | "\(.device) \(.mountpoint)"] | contains(["/dev/mapper/vg0-home /home"])) and (.warnings | map(test("^/home is kept")) | any)
		and ([.lost[].path] | contains(["/root", "/etc"])) and ([.dropped[].mountpoint] == ["/mnt/gone"]) and (.resumed != true) and (.root.known != false)
		and ([.untouched[].device] | contains(["/dev/vdb"]))' "$WORK/plan.json" >/dev/null || fail "the plan does not keep, set aside, lose and leave what it must"
	if [ "$FIRMWARE" = bios ]; then
		jq -e '(.firmware == "bios") and (.boot.bios_disks == ["/dev/vda"]) and (.boot.esp == "")' "$WORK/plan.json" >/dev/null || fail "the plan does not put GRUB onto the disk of /boot"
	else
		jq -e '.keep[0].note | test("EFI/debian")' "$WORK/plan.json" >/dev/null || fail "the plan does not set the old boot loader on the ESP aside"
	fi
	[ "$(sums /dev/mapper/vg0-data /dev/mapper/vg0-var /dev/mapper/vg0-swap /dev/vda4 /dev/vdb)" = "$kept" ] || fail "a kept device changed during the installation"
	echo "the kept devices (/data, /var, swap, /srv/plain, the second disk) are byte for byte as before"
	[ "$FIRMWARE" = bios ] || r 'mount -o ro /dev/vda2 /mnt && ls -d /mnt/EFI/debian-old-* >/dev/null && [ ! -e /mnt/EFI/debian ] && ls /mnt/EFI/BOOT/BOOT*.EFI >/dev/null; rc=$?; umount /mnt; exit $rc' \
		|| fail "the old boot loader on the ESP was not set aside"
	stop
	start disk
	check "OLD_UUID=$old_uuid" "OS=$OS" <<'CHECK'
. /etc/os-release
ok "the new system: $PRETTY_NAME as $(hostname), started by $([ -d /sys/firmware/efi ] && echo "UEFI, although the firmware has an entry for the old boot loader" || echo BIOS)" '[ "$(hostname)" = new.example.net ]'
ok "os: $OS" 'grep -q "^os=$OS" /etc/ump/install'
ok "the root is new (another file system than the old one's)" '[ "$(findmnt -n -o UUID /)" != "$OLD_UUID" ]'
ok "/data: the old volume (xfs on vg0-data), its marker there" '[ "$(findmnt -n -o SOURCE,FSTYPE /data) $(cat /data/marker)" = "/dev/mapper/vg0-data xfs data-marker" ]'
ok "/srv/plain: the old partition, its marker there" '[ "$(findmnt -n -o SOURCE /srv/plain) $(cat /srv/plain/marker)" = "/dev/vda4 plain-marker" ]'
ok "/var.old: the old /var, its marker there" '[ "$(findmnt -n -o SOURCE /var.old) $(cat /var.old/lib/marker)" = "/dev/mapper/vg0-var var-marker" ]'
ok "/var is the new system's, on the root" '[ "$(findmnt -n -o TARGET -T /var/lib)" = / ] && [ ! -e /var/lib/marker ]'
ok "swap: the old volume" 'grep -q "^$(readlink -f /dev/mapper/vg0-swap) " /proc/swaps'
ok "the old root is gone: no marker in /opt or /etc" '[ ! -e /opt/marker ] && [ ! -e /etc/marker ]'
ok "/home: the old volume, the administrator in the home directory that was there, its marker and the old authorized_keys beside the new" '[ "$(findmnt -n -o SOURCE /home) $(cat ~/marker)" = "/dev/mapper/vg0-home home-marker" ] && ls ~/.ssh/authorized_keys.old-* >/dev/null && [ "$(stat -c %U ~/marker)" = ump ]'
ok "the bind mount of a kept directory is there" '[ "$(cat /srv/shared/file)" = shared ]'
ok "the bind mount whose source went with the root: an empty directory, mounted" '[ -d /opt/app ] && [ -z "$(ls -A /srv/app)" ] && findmnt -n /srv/app'
ok "the tmpfs entry is mounted" '[ "$(findmnt -n -o FSTYPE /srv/tmp)" = tmpfs ]'
ok "the entry whose device is gone is not in the new fstab" '! grep -q /mnt/gone /etc/fstab'
ok "fstab: /data and /srv/plain as the old one had them, /var as /var.old" '[ "$(awk "\$2 == \"/data\" && \$3 == \"xfs\" || \$2 == \"/srv/plain\" && \$3 == \"ext4\" || \$2 == \"/var.old\" && \$3 == \"ext4\"" /etc/fstab | wc -l)" = 3 ] && ! awk "\$2 == \"/var\"" /etc/fstab | grep -q .'
ok "install record: $(grep '^keep_data=\|^kept=\|^aside=' /etc/ump/install | paste -sd' ' -)" 'grep -q "^keep_data=yes" /etc/ump/install'
ok "the plan is in /etc/ump/keep-data.json" 'jq -e ".hash and (.format | length == 2)" /etc/ump/keep-data.json'
ok "nothing of the installation's own is left on the root" '[ ! -e /.ump-keep-data ]'
ok "no unit failed ($(systemctl is-system-running)) $(failed)" '[ -z "$(failed)" ]'
echo second-marker | sudo -n tee /opt/marker-second >/dev/null
CHECK
}

# --- zfs: a pool beside the system, on Ubuntu ----------------------------------

m_zfs() {
	echo "=== zfs: a ZFS pool on the second disk of main; $OS with --keep-data"
	local hostid
	case "$OS" in ubuntu-*) ;; *) echo "skipped: $OS packages no ZFS with its kernel"; stop off; start rescue; return 0 ;; esac
	if ! u 'sudo -n bash -c "apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends zfsutils-linux"' > "$WORK/zfs-apt.log" 2>&1; then
		echo "skipped: zfsutils-linux could not be installed in the running system (no archive?)"; stop off; start rescue; return 0
	fi
	u 'sudo -n bash -s' <<'ZFS' || fail "the pool could not be made"
set -e
modprobe zfs
[ -s /etc/hostid ] || zgenhostid
zpool create -f tank /dev/vdb
zfs create tank/files
echo zfs-marker > /tank/files/marker
sync
ZFS
	hostid="$(u hostid)"
	stop off
	start rescue
	keep "$OS" zfs.example.net
	jq -e '(.packages | index("zfsutils-linux")) and ([.untouched[] | select(.filesystem == "zfs_member")] | length > 0)' "$WORK/plan.json" >/dev/null || fail "the plan does not leave the pool and install ZFS"
	stop
	start disk
	check "HOSTID=$hostid" <<'CHECK'
ok "the new system: $(hostname)" '[ "$(hostname)" = zfs.example.net ]'
ok "the pool is imported: $(sudo -n zpool list -H -o name,health tank | paste -sd' ' -)" '[ "$(sudo -n zpool list -H -o health tank)" = ONLINE ]'
ok "its dataset is mounted, the marker there" '[ "$(cat /tank/files/marker)" = zfs-marker ]'
ok "the host ID is the old system's ($(hostid))" '[ "$(hostid)" = "$HOSTID" ]'
ok "/data and /var.old are still there" '[ "$(cat /data/marker) $(cat /var.old/lib/marker)" = "data-marker var-marker" ]'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
echo second-marker | sudo -n tee /opt/marker-second >/dev/null
CHECK
	stop off
	start rescue
}

# --- keep-root: nothing formatted but /boot and what --format names ------------

m_keep_root() {
	echo "=== keep-root: main once more, $OLD_OS with --keep-root --format /srv/plain"
	local kept root_uuid
	kept="$(sums /dev/mapper/vg0-data /dev/mapper/vg0-var /dev/mapper/vg0-swap)"
	root_uuid="$(r 'blkid -s UUID -o value /dev/mapper/vg0-root')"
	keep "$OLD_OS" third.example.net --keep-root --format /srv/plain
	jq -e '([.format[] | "\(.device) \(.mountpoint)"] == ["/dev/vda3 /boot", "/dev/vda4 /srv/plain"]) and (.lost == [])
		and ([.aside[] | select(.how == "moved") | .mountpoint] == ["/old-root-DATE"])' "$WORK/plan.json" >/dev/null || fail "the plan with --keep-root formats the root, or not what --format names"
	[ "$(sums /dev/mapper/vg0-data /dev/mapper/vg0-var /dev/mapper/vg0-swap)" = "$kept" ] || fail "a kept device changed during the installation"
	echo "the kept devices (/data, /var, swap) are byte for byte as before"
	stop
	start disk
	check "ROOT_UUID=$root_uuid" <<'CHECK'
. /etc/os-release
ok "the new system: $PRETTY_NAME as $(hostname)" '[ "$(hostname)" = third.example.net ]'
ok "the root file system is the old one, not formatted" '[ "$(findmnt -n -o UUID /)" = "$ROOT_UUID" ]'
old="$(ls -d /old-root-* | head -n 1)"
ok "the system before is in $old: its hostname ($(sudo -n cat $old/etc/hostname)), its /opt, its record" 'sudo -n grep -q "\.example\.net" $old/etc/hostname && [ "$(sudo -n cat $old/opt/marker-second)" = second-marker ] && sudo -n grep -q "^keep_data=yes" $old/etc/ump/install'
ok "nothing else of it is at the top" '[ "$(ls -d /old-root-* | wc -l)" = 1 ] && [ ! -e /opt/marker-second ] && [ "$(hostname)" != "$(sudo -n cat $old/etc/hostname)" ]'
ok "/home is the volume it was, the administrator in the same home directory" '[ "$(findmnt -n -o SOURCE /home) $(cat ~/marker)" = "/dev/mapper/vg0-home home-marker" ]'
ok "/srv/plain was formatted: empty, and mounted" '[ -z "$(ls -A /srv/plain | grep -v lost+found)" ] && findmnt -n /srv/plain'
ok "/data and /var.old are still there" '[ "$(cat /data/marker) $(cat /var.old/lib/marker)" = "data-marker var-marker" ]'
ok "install record: $(grep '^keep_root=\|^aside=' /etc/ump/install | paste -sd' ' -)" 'grep -q "^aside=.*:$old\$" /etc/ump/install'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
CHECK
	stop off
}

# --- md-luks: arrays not assembled, /home on LUKS, kernel names -------------------

m_md_luks() {
	echo "=== md-luks: a root on md RAID1 (/dev/md0 to the old system), an array by name, /home on LUKS with a key file, an encrypted swap over a file system; $OLD_OS with --keep-data"
	local kept
	machine md-luks 7G 7G
	start rescue
	layout <<'SETUP'
for d in vda vdb; do sgdisk -n 1:0:+256M -t 1:ef00 -n 2:0:+4G -t 2:fd00 -n 3:0:+512M -t 3:8300 -n 4:0:+256M -t 4:8300 -n 5:0:0 -t 5:fd00 /dev/$d >/dev/null; done
udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.vfat -F 32 /dev/vdb1 >/dev/null
mdadm --create /dev/md0 --level=1 --raid-devices=2 --metadata=1.2 --homehost=old --name=0 --run /dev/vda2 /dev/vdb2 >/dev/null 2>&1
mdadm --create /dev/md/data --level=1 --raid-devices=2 --metadata=1.2 --homehost=old --name=data --run /dev/vda5 /dev/vdb5 >/dev/null 2>&1
udevadm settle
mkfs.ext4 -q /dev/md0; mkfs.ext4 -q /dev/md/data
mount /dev/md/data /mnt; echo array-marker > /mnt/marker; umount /mnt
head -c 64 /dev/urandom > /tmp/luks.key
cryptsetup luksFormat -q --pbkdf pbkdf2 --key-file /tmp/luks.key /dev/vda3
cryptsetup open --key-file /tmp/luks.key /dev/vda3 vault; mkfs.ext4 -q /dev/mapper/vault
# the administrator's home directory on it, without keys: who gets in, gets in by the keys outside /home
mount /dev/mapper/vault /mnt; echo luks-marker > /mnt/marker; mkdir -p /mnt/ump; echo home-marker > /mnt/ump/marker; chown -R 1000:1000 /mnt/ump; umount /mnt; cryptsetup close vault
mkfs.xfs -q /dev/vdb3; mount /dev/vdb3 /mnt; echo xfs-marker > /mnt/marker; umount /mnt
mkfs.ext4 -q -L spare /dev/vda4; mount /dev/vda4 /mnt; echo spare-marker > /mnt/marker; umount /mnt
mount /dev/md0 /mnt
old_root /mnt old-md
mkdir -p /mnt/etc/keys /mnt/etc/mdadm; install -m 400 /tmp/luks.key /mnt/etc/keys/vault.key
{ echo "ARRAY /dev/md0 metadata=1.2 UUID=$(mdadm --detail /dev/md0 | sed -n 's/^ *UUID : //p') name=old:0"
  echo "ARRAY /dev/md/data metadata=1.2 UUID=$(mdadm --detail /dev/md/data | sed -n 's/^ *UUID : //p') name=old:data"; } > /mnt/etc/mdadm/mdadm.conf
cat > /mnt/etc/fstab <<F
/dev/md0  /  ext4  errors=remount-ro  0 1
UUID=$(uuid /dev/vda1)  /boot/efi  vfat  umask=0077  0 1
/dev/mapper/vault  /home  ext4  defaults  0 2
/dev/md/data  /srv/data  ext4  defaults  0 2
/dev/vdb3  /srv/xfs  xfs  noatime  0 2
/dev/mapper/cswap  none  swap  sw  0 0
F
# the encrypted swap of a system from before the spare partition was given a file system
printf 'vault  UUID=%s  /etc/keys/vault.key  luks\ncswap  /dev/vda4  /dev/urandom  swap,cipher=aes-xts-plain64,size=256\n' "$(uuid /dev/vda3)" > /mnt/etc/crypttab
umount /mnt
# as a rescue system finds the arrays of another host that nothing assembled: not at all
mdadm --wait /dev/md0 /dev/md/data >/dev/null 2>&1 || true
mdadm --stop --scan >/dev/null 2>&1; udevadm settle
grep -q '^md' /proc/mdstat && { echo "an array is still assembled"; exit 1; }
echo "no array is assembled: $(grep -c '^md' /proc/mdstat) in /proc/mdstat"
SETUP
	kept="$(r 'sha256sum /dev/vda3 /dev/vdb3 /dev/vda4 /dev/vdb1')"
	keep "$OLD_OS" md.example.net
	jq -e '([.format[] | .mountpoint] == ["/"]) and (.format[0].device | test("^/dev/md")) and (.packages == ["cryptsetup"])
		and ([.keep[] | .mountpoint] | contains(["/boot/efi", "/home", "/srv/data", "/srv/xfs"])) and ([.untouched[].device] | contains(["/dev/vda4", "/dev/vdb1"]))
		and (.volatile == []) and ([.crypttab[].name] == ["vault"]) and (.warnings | map(test("cswap of the old crypttab is dropped: it is made anew at every boot")) | any)
		and (.warnings | map(test("/home is kept and not mounted by the installer")) | any) and (.warnings | map(test("/etc/ssh/authorized_keys/NAME")) | any)
		and (.warnings | map(test("/dev/vdb3 \\(/srv/xfs\\) is named as the old system.s kernel named it")) | any)' "$WORK/plan.json" >/dev/null \
		|| fail "the plan does not format the root's array alone, keep the rest, drop the encrypted swap over the spare partition and put the keys outside /home"
	grep -q "^==> ssh: the users' keys also in /etc/ssh/authorized_keys" "$WORK/install.log" || fail "the keys were not put outside /home"
	[ "$(r 'sha256sum /dev/vda3 /dev/vdb3 /dev/vda4 /dev/vdb1')" = "$kept" ] || fail "a kept device changed during the installation"
	echo "the kept devices (the LUKS container, the xfs partition, the spare one, the second ESP) are byte for byte as before"
	stop
	start disk
	check <<'CHECK'
ok "the new system: $(. /etc/os-release; echo $PRETTY_NAME) as $(hostname)" '[ "$(hostname)" = md.example.net ]'
ok "the root is on the array, which the installer assembled read-only and made writable: $(findmnt -n -o SOURCE /), $(grep -A1 '^md' /proc/mdstat | paste -sd' ' - | tr -s ' ')" 'findmnt -n -o SOURCE / | grep -q "^/dev/md" && [ "$(grep -c "\[UU\]" /proc/mdstat)" = 2 ]'
ok "mdadm.conf names the arrays as the old system did: $(grep '^ARRAY' /etc/mdadm/mdadm.conf | awk '{print $2}' | paste -sd' ' -)" 'grep -q "^ARRAY /dev/md0 " /etc/mdadm/mdadm.conf && grep -q "^ARRAY /dev/md/data " /etc/mdadm/mdadm.conf && ! grep -q "name=old" /etc/mdadm/mdadm.conf'
ok "the array the old fstab called /dev/md/data is mounted by its UUID, its marker there" 'grep -q "^UUID=.* /srv/data  *ext4" /etc/fstab && [ "$(cat /srv/data/marker)" = array-marker ]'
ok "/home is the LUKS volume, opened with its key file, its marker there" '[ "$(findmnt -n -o SOURCE /home) $(cat /home/marker)" = "/dev/mapper/vault luks-marker" ]'
ok "the administrator got in by the key outside /home (no authorized_keys in the home directory that was there)" '[ "$(cat ~/marker)" = home-marker ] && [ ! -e ~/.ssh/authorized_keys ] && [ -s /etc/ssh/authorized_keys/ump ] && grep -q "^AuthorizedKeysFile" /etc/ssh/sshd_config.d/20-ump-keep-data.conf'
ok "the key file is in the new system as it was ($(sudo -n stat -c '%a %U' /etc/keys/vault.key))" '[ "$(sudo -n stat -c "%a %U" /etc/keys/vault.key)" = "400 root" ]'
ok "crypttab is the old one's entry for the LUKS volume alone: no encrypted swap over the spare partition" 'grep -q "^vault  *UUID=.*/etc/keys/vault.key  *luks" /etc/crypttab && ! grep -q cswap /etc/crypttab /etc/fstab'
ok "the spare partition holds its file system and marker after the boot of the new system" 'sudo -n mkdir -p /mnt/spare && sudo -n mount -o ro /dev/disk/by-label/spare /mnt/spare && [ "$(cat /mnt/spare/marker)" = spare-marker ]'
ok "the partition the old fstab called /dev/vdb3 is mounted by its UUID, its marker there" 'grep -q "^UUID=.* /srv/xfs  *xfs  *noatime" /etc/fstab && [ "$(cat /srv/xfs/marker)" = xfs-marker ]'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
CHECK
	stop off
}

# --- btrfs: the root in a subvolume --------------------------------------------

m_btrfs() {
	echo "=== btrfs: the root in the subvolume @, /home in @home, a snapshot of the root; $OLD_OS with --keep-data, then with --keep-root"
	machine btrfs 8G
	start rescue
	layout <<'SETUP'
sgdisk -n 1:0:+256M -t 1:ef00 -n 2:0:0 -t 2:8300 /dev/vda >/dev/null; udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.btrfs -q /dev/vda2
mount /dev/vda2 /mnt
btrfs subvolume create /mnt/@ >/dev/null; btrfs subvolume create /mnt/@home >/dev/null; btrfs subvolume create /mnt/@snapshots >/dev/null
old_root /mnt/@ old-btrfs
cat > /mnt/@/etc/fstab <<F
UUID=$(uuid /dev/vda2)  /  btrfs  subvol=@,noatime  0 0
UUID=$(uuid /dev/vda2)  /home  btrfs  subvol=@home,noatime  0 0
UUID=$(uuid /dev/vda2)  /.snapshots  btrfs  subvol=@snapshots  0 0
UUID=$(uuid /dev/vda1)  /boot/efi  vfat  umask=0077  0 1
F
btrfs subvolume create /mnt/@/var/lib/machines >/dev/null
mkdir -p /mnt/@snapshots/1; btrfs subvolume snapshot /mnt/@ /mnt/@snapshots/1/snapshot >/dev/null
# the root subvolume is the default one too, as some installers leave it
btrfs subvolume set-default "$(btrfs subvolume list /mnt | awk '$NF == "@" {print $2}')" /mnt
# the administrator's home directory from before, under another ID
mkdir -p /mnt/@home/ump/.ssh /mnt/@home/alice; echo home-marker > /mnt/@home/ump/marker; echo "ssh-ed25519 AAAA old@key" > /mnt/@home/ump/.ssh/authorized_keys
chown -R 1005:1005 /mnt/@home/ump; echo alice-marker > /mnt/@home/alice/marker
umount /mnt
SETUP
	keep "$OLD_OS" btrfs.example.net
	jq -e '(.format | length == 1) and (.format[0].how == "subvolume") and (.root.subvolume == "@")
		and ([.keep[] | .mountpoint] | contains(["/home", "/.snapshots", "/boot/efi"])) and (.warnings | map(test("is the default subvolume")) | any)' "$WORK/plan.json" >/dev/null || fail "the plan does not replace the subvolume @ alone, or does not say what becomes of the default subvolume"
	stop
	start disk
	check <<'CHECK'
ok "the new system: $(. /etc/os-release; echo $PRETTY_NAME) as $(hostname)" '[ "$(hostname)" = btrfs.example.net ]'
ok "the root is the subvolume @ ($(findmnt -n -o SOURCE /))" 'findmnt -n -o SOURCE / | grep -q "\[/@\]\$"'
ok "/home is @home, with what it held" 'findmnt -n -o SOURCE /home | grep -q "\[/@home\]\$" && [ "$(cat /home/alice/marker)" = alice-marker ]'
ok "the administrator logged in with the profile's key into the home directory that was there, and owns it" '[ "$(cat ~/marker)" = home-marker ] && [ "$(stat -c %u ~)" = "$(id -u)" ] && [ "$(stat -c %u ~/marker)" = "$(id -u)" ]'
ok "its ID is the one the directory had ($(id -u))" '[ "$(id -u)" = 1005 ]'
ok "the authorized_keys it had is saved beside the new one" 'grep -q "old@key" ~/.ssh/authorized_keys.old-* && ! grep -q "old@key" ~/.ssh/authorized_keys'
ok "the snapshot of the old root is still there, the old root is not" '[ "$(sudo -n cat /.snapshots/1/snapshot/etc/hostname)" = old-btrfs ] && [ ! -e /home/alice/data ] && [ ! -e /root/.old ]'
ok "the subvolumes: $(sudo -n btrfs subvolume list / | sed 's/.* path //' | paste -sd' ' -)" 'l="$(sudo -n btrfs subvolume list / | sed "s/.* path //")"; for v in @ @home @snapshots @snapshots/1/snapshot; do echo "$l" | grep -qx "$v" || exit 1; done; ! echo "$l" | grep -q "^@-old"'
ok "the default subvolume is the new @ ($(sudo -n btrfs subvolume get-default / | sed 's/.* path //'))" 'sudo -n btrfs subvolume get-default / | grep -q " path @\$"'
ok "nothing of the installation's own is left at the top of the file system" '(sudo -n mkdir -p /mnt/top && sudo -n mount -o subvolid=5,ro "$(findmnt -n -o SOURCE / | sed "s/\[.*//")" /mnt/top && [ ! -e /mnt/top/.ump-keep-data ]; rc=$?; sudo -n umount /mnt/top; exit $rc)'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
echo before-keep-root > ~/marker2
CHECK
	stop off
	start rescue
	keep "$OLD_OS" btrfs2.example.net --keep-root
	jq -e '(.format == []) and ([.aside[] | select(.how == "renamed") | "\(.from) \(.mountpoint)"] == ["@ @-old-DATE"])' "$WORK/plan.json" >/dev/null || fail "the plan with --keep-root does not rename the subvolume @"
	stop
	start disk
	check <<'CHECK'
ok "the new system: $(hostname), its root the subvolume @" '[ "$(hostname)" = btrfs2.example.net ] && findmnt -n -o SOURCE / | grep -q "\[/@\]\$"'
old="$(sudo -n btrfs subvolume list / | sed 's/.* path //' | grep '^@-old-[0-9-]*$')"
ok "the root before is the subvolume $old" 'sudo -n mkdir -p /mnt/top && sudo -n mount -o subvolid=5,ro "$(findmnt -n -o SOURCE / | sed "s/\[.*//")" /mnt/top && [ "$(sudo -n cat /mnt/top/$old/etc/hostname)" = btrfs.example.net ]'
ok "/home as ever" '[ "$(cat ~/marker2) $(cat /home/alice/marker)" = "before-keep-root alice-marker" ]'
ok "the default subvolume is the new @, not the one set aside ($(sudo -n btrfs subvolume get-default / | sed 's/.* path //'))" 'sudo -n btrfs subvolume get-default / | grep -q " path @\$"'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
CHECK
	stop off
}

# --- resume: a second attempt after one that failed mid-way --------------------

m_resume() {
	echo "=== resume: an installation with --format /var that fails after the root, /boot and /var were formatted, and the next one"
	local kept hash rc=0
	machine resume 6G
	start rescue
	layout <<'SETUP'
sgdisk -n 1:0:+256M -t 1:ef00 -n 2:0:+512M -t 2:8300 -n 3:0:+3G -t 3:8300 -n 4:0:+256M -t 4:8300 -n 5:0:0 -t 5:8300 /dev/vda >/dev/null; udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.ext4 -q -L boot /dev/vda2; mkfs.ext4 -q /dev/vda3; mkfs.ext4 -q -L keep /dev/vda4; mkfs.ext4 -q /dev/vda5
mount /dev/vda4 /mnt; echo kept-marker > /mnt/marker; umount /mnt
mount /dev/vda5 /mnt; mkdir -p /mnt/lib; echo var-marker > /mnt/lib/marker; umount /mnt
mount /dev/vda3 /mnt; old_root /mnt old-resume
printf 'UUID=%s / ext4 defaults 0 1\nLABEL=boot /boot ext4 defaults 0 2\nUUID=%s /boot/efi vfat umask=0077 0 1\nLABEL=keep /srv/keep ext4 defaults 0 2\nUUID=%s /var ext4 defaults 0 2\n' "$(uuid /dev/vda3)" "$(uuid /dev/vda1)" "$(uuid /dev/vda5)" > /mnt/etc/fstab
umount /mnt
SETUP
	kept="$(sums /dev/vda4)"
	spec "$OLD_OS" resume.example.net broken > "$WORK/spec.json"; put "$WORK/spec.json" /tmp/spec.json
	r 'bash /tmp/install.sh --spec /tmp/spec.json --keep-data --format /var --plan /tmp/plan.json' > "$WORK/plan.log" 2>&1 || { tail "$WORK/plan.log"; fail "the plan"; }
	r 'jq -e "(.resumed != true) and ([.format[] | \"\(.device) \(.mountpoint)\"] == [\"/dev/vda3 /\", \"/dev/vda2 /boot\", \"/dev/vda5 /var\"]) and (.aside == [])" /tmp/plan.json' >/dev/null || fail "the first plan does not format the root, /boot and /var"
	hash="$(r 'jq -r .hash /tmp/plan.json')"
	r "bash /tmp/install.sh --spec /tmp/spec.json --keep-data --format /var --confirm $hash" > "$WORK/install.log" 2>&1 || rc=$?
	[ "$rc" = 1 ] && grep -q "keep-data: writing begins" "$WORK/install.log" || { tail "$WORK/install.log"; fail "the installation without an image did not fail after its first write (status $rc)"; }
	r 'mount -o ro /dev/vda3 /mnt && [ -s /mnt/.ump-keep-data/etc/fstab ] && [ -s /mnt/.ump-keep-data/plan.json ] && [ "$(wc -l < /mnt/.ump-keep-data/formats)" = 2 ] && (cd /mnt/.ump-keep-data && sha256sum -c --status SHA256SUMS); rc=$?; umount /mnt; exit $rc' \
		|| fail "the failed attempt did not leave the old fstab, its plan and the new UUIDs of /boot and /var on the new root"
	echo "the first attempt failed after the root, /boot and /var were formatted (status 1); the old fstab, the plan and the UUIDs it gave are on the new root"
	keep "$OLD_OS" resume.example.net --format /var
	jq -e '([.keep[] | .mountpoint] | contains(["/srv/keep"])) and ([.format[] | "\(.device) \(.mountpoint)"] == ["/dev/vda3 /", "/dev/vda2 /boot", "/dev/vda5 /var"])
		and (.resumed == true) and (.shrunk != true) and (.root.by == "detection") and (.warnings | map(test("interrupted")) | any)' "$WORK/plan.json" >/dev/null \
		|| fail "the second plan does not know the old fstab, with /boot (named by its label) and /var as the first attempt left them, or says that it keeps less"
	[ "$(sums /dev/vda4)" = "$kept" ] || fail "the kept device changed"
	stop
	start disk
	check <<'CHECK'
ok "the new system: $(hostname)" '[ "$(hostname)" = resume.example.net ]'
ok "/srv/keep is mounted as the old fstab had it, its marker there; /boot is the partition it was, with its label" '[ "$(cat /srv/keep/marker)" = kept-marker ] && [ "$(findmnt -n -o SOURCE,LABEL /boot | tr -s " ")" = "/dev/vda2 boot" ]'
ok "/var is the old system's volume, formatted (--format /var): the new system's /var on it, nothing of the old" '[ "$(findmnt -n -o SOURCE /var)" = /dev/vda5 ] && [ ! -e /var/lib/marker ] && [ -d /var/lib/dpkg ] && ! grep -q var.old /etc/fstab'
ok "nothing of the installation's own is left on the root" '[ ! -e /.ump-keep-data ]'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
CHECK
	stop off
}

# --- refusals: nothing is written ----------------------------------------------

m_refusals() {
	echo "=== refusals: what keep-data does not do, refused before anything is written"
	machine refusals 3G 2G
	start rescue
	spec "$OLD_OS" refused.example.net > "$WORK/spec.json"; put "$WORK/spec.json" /tmp/spec.json
	jq '.disks = ["/dev/vda"]' "$WORK/spec.json" > "$WORK/spec-disks.json"; put "$WORK/spec-disks.json" /tmp/spec-disks.json

	layout <<'SETUP'
wipe
sgdisk -n 1:0:+128M -t 1:ef00 -n 2:0:+256M -t 2:8300 -n 3:0:0 -t 3:8300 /dev/vda >/dev/null; udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.ext4 -q /dev/vda2
echo -n passphrase | cryptsetup luksFormat -q --pbkdf pbkdf2 --key-file - /dev/vda3
echo -n passphrase | cryptsetup open --key-file - /dev/vda3 cryptroot; mkfs.ext4 -q /dev/mapper/cryptroot
mount /dev/mapper/cryptroot /mnt; old_root /mnt old-crypt; echo "/dev/mapper/cryptroot / ext4 defaults 0 1" > /mnt/etc/fstab; umount /mnt; cryptsetup close cryptroot
SETUP
	refused "an encrypted root" "encrypted root is not supported" --keep-data --plan /tmp/refused.json
	r 'jq -e ".refused | test(\"encrypted root\")" /tmp/refused.json' >/dev/null || fail "the refusal is not in the plan file"
	refused "an encrypted root, named with --root" "is encrypted (LUKS)" --keep-data --root /dev/vda3 --dry-run

	layout <<'SETUP'
wipe
sgdisk -n 1:0:+256M -t 1:8300 -n 2:0:0 -t 2:8300 /dev/vda >/dev/null; udevadm settle
mkfs.ext4 -q /dev/vda1; mkfs.ext4 -q /dev/vda2
mount /dev/vda2 /mnt; old_root /mnt old-noesp
printf 'UUID=%s / ext4 defaults 0 1\nUUID=%s /boot ext4 defaults 0 2\n' "$(uuid /dev/vda2)" "$(uuid /dev/vda1)" > /mnt/etc/fstab; umount /mnt
SETUP
	refused "no EFI system partition on a machine that boots with UEFI" "no EFI system partition" --keep-data --dry-run

	layout <<'SETUP'
wipe
sgdisk -n 1:0:+128M -t 1:ef00 -n 2:0:+1G -t 2:8300 -n 3:0:0 -t 3:8300 /dev/vda >/dev/null; sgdisk -n 1:0:0 -t 1:8300 /dev/vdb >/dev/null; udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.ext4 -q /dev/vda2; mkfs.ext4 -q /dev/vda3; mkfs.ext4 -q /dev/vdb1
for p in 2 3; do
	mount /dev/vda$p /mnt; old_root /mnt old-$p
	printf 'UUID=%s / ext4 defaults 0 1\nUUID=%s /boot/efi vfat umask=0077 0 1\nUUID=%s /data ext4 defaults 0 2\n' "$(uuid /dev/vda$p)" "$(uuid /dev/vda1)" "$(uuid /dev/vdb1)" > /mnt/etc/fstab; umount /mnt
done
SETUP
	refused "two old systems" "several old systems" --keep-data --plan /tmp/refused.json
	r 'jq -e "(.candidates | length == 2) and (.refused | test(\"--root\"))" /tmp/refused.json' >/dev/null || fail "the refusal does not name the candidates in the plan file"
	refused "two old systems, one named: a root too small for the image" "is too small for" --keep-data --root /dev/vda2 --dry-run
	refused "a volume to format that the old fstab does not have" "the old fstab has no such mountpoint" --keep-data --root /dev/vda3 --format /srv/nothing --dry-run
	refused "the EFI system partition to format" "never formatted" --keep-data --root /dev/vda3 --format /boot/efi --dry-run
	# a root that --root names where no old system is: planned, and marked
	r 'bash /tmp/install.sh --spec /tmp/spec.json --keep-data --root /dev/vdb1 --plan /tmp/unknown.json' > "$WORK/plan.log" 2>&1 || { tail "$WORK/plan.log"; fail "the plan of a root without an old system"; }
	r 'jq -e "(.root.known == false) and (.root.by == \"option\") and (.warnings | map(test(\"holds no old system\")) | any)" /tmp/unknown.json' >/dev/null || fail "a root without an old system is not marked as not known in the plan"
	echo "planned and marked (root.known: false): a root that --root names where no old system is"
	refused "no confirmation" "writes only what was confirmed" --keep-data --root /dev/vda3
	refused "a confirmation that is no hash" "writes only what was confirmed" --keep-data --root /dev/vda3 --confirm yes
	refused "disks named beside keep-data" "keep-data takes no disks" --keep-data --root /dev/vda3 --dry-run --spec /tmp/spec-disks.json
	refused "the options of keep-data without it" "belong to --keep-data" --keep-root --dry-run
	# (mounted read-only: a file system mounted for writing changes its disk by itself)
	r 'mount -o ro,noload /dev/vdb1 /mnt'
	refused "a disk in use" "the disks are in use" --keep-data --root /dev/vda3 --dry-run
	r 'umount /mnt'
	# and what is not refused, for the difference: the plan of the system named
	refused_before="$(sums '/dev/vd?')"
	r 'bash /tmp/install.sh --spec /tmp/spec.json --keep-data --root /dev/vda3 --dry-run' > "$WORK/plan.log" 2>&1 || { tail "$WORK/plan.log"; fail "the plan of the named system"; }
	[ "$(sums '/dev/vd?')" = "$refused_before" ] || fail "the dry run changed a disk"
	grep -q "dry run: stopping here" "$WORK/plan.log" || fail "the dry run does not end as one"
	echo "planned, the disks byte for byte as before: $(grep -m1 'keep-data: the old system' "$WORK/plan.log" | sed 's/^==> //' | cut -c1-110)"

	# a /boot the old fstab names as its kernel did, where that name is a data partition now
	layout <<'SETUP'
wipe
sgdisk -n 1:0:+128M -t 1:ef00 -n 2:0:+400M -t 2:8300 -n 3:0:0 -t 3:8300 /dev/vda >/dev/null; sgdisk -n 1:0:0 -t 1:8300 /dev/vdb >/dev/null; udevadm settle
mkfs.vfat -F 32 /dev/vda1 >/dev/null; mkfs.ext4 -q /dev/vda2; mkfs.ext4 -q /dev/vda3; mkfs.ext4 -q -L archive /dev/vdb1
mount /dev/vdb1 /mnt; echo archive-marker > /mnt/marker; umount /mnt
mount /dev/vda2 /mnt; mkdir -p /mnt/grub; echo kernel > /mnt/vmlinuz-6.1.0-13-old; umount /mnt
mount /dev/vda3 /mnt; old_root /mnt old-names; mkdir -p /mnt/usr/lib/modules/6.1.0-13-old
printf 'UUID=%s / ext4 defaults 0 1\n/dev/vdb1 /boot ext4 defaults 0 2\nUUID=%s /boot/efi vfat umask=0077 0 1\n' "$(uuid /dev/vda3)" "$(uuid /dev/vda1)" > /mnt/etc/fstab; umount /mnt
SETUP
	refused "a /boot named as the kernel named it, where that name is a data partition" "does not hold the old system's kernels" --keep-data --dry-run
	r 'mount /dev/vda3 /mnt && sed -i "s|^/dev/vdb1 /boot|/dev/vda2 /boot|" /mnt/etc/fstab && umount /mnt'
	r 'bash /tmp/install.sh --spec /tmp/spec.json --keep-data --plan /tmp/named.json' > "$WORK/plan.log" 2>&1 || { tail "$WORK/plan.log"; fail "the plan with /boot named as the kernel named it, on the old /boot"; }
	r 'jq -e "([.format[] | \"\(.device) \(.mountpoint)\"] == [\"/dev/vda3 /\", \"/dev/vda2 /boot\"]) and (.warnings | map(test(\"it holds the old system.s kernels \\\\(6.1.0-13-old\\\\)\")) | any)" /tmp/named.json' >/dev/null \
		|| fail "a /boot named as the kernel named it, with the old system's kernels on it, is not taken for the old /boot"
	echo "planned: a /boot named as the kernel named it, proven by the old system's kernels on it"
	stop
}

# --- loop: the installer's looks and writes on file systems in loop devices ------

m_loop() {
	echo "=== loop: test-keep-loop.sh in the rescue system"
	machine loop 1G
	start rescue
	put "$HERE/test-keep-loop.sh" /tmp/test-keep-loop.sh
	r 'bash /tmp/test-keep-loop.sh /tmp/install.sh' > "$WORK/loop.log" 2>&1 || { cat "$WORK/loop.log"; fail "test-keep-loop.sh in the rescue system"; }
	cat "$WORK/loop.log"
	! grep -q "^skipped" "$WORK/loop.log" || fail "the rescue system skipped a part of test-keep-loop.sh: it has every file system the installer looks at"
	stop
}

# --- mbr: BIOS, a disk with an MBR partition table ---------------------------------

m_mbr() {
	echo "=== mbr: an old system on an MBR disk, started by BIOS; $OLD_OS with --keep-data"
	local kept
	machine mbr 6G
	start rescue
	layout <<'SETUP'
parted -s /dev/vda mklabel msdos mkpart primary ext4 1MiB 4GiB mkpart primary ext4 4GiB 100% set 1 boot on; udevadm settle
mkfs.ext4 -q /dev/vda1; mkfs.ext4 -q -L data /dev/vda2
mount /dev/vda2 /mnt; echo data-marker > /mnt/marker; umount /mnt
mount /dev/vda1 /mnt; old_root /mnt old-mbr
printf 'UUID=%s / ext4 errors=remount-ro 0 1\nLABEL=data /data ext4 defaults 0 2\n' "$(uuid /dev/vda1)" > /mnt/etc/fstab; umount /mnt
SETUP
	kept="$(sums /dev/vda2)"
	keep "$OLD_OS" mbr.example.net
	jq -e '(.firmware == "bios") and (.boot.bios_disks == ["/dev/vda"]) and (.boot.bios_optional == []) and ([.format[] | "\(.device) \(.mountpoint)"] == ["/dev/vda1 /"])
		and ([.keep[] | "\(.device) \(.mountpoint)"] == ["/dev/vda2 /data"])' "$WORK/plan.json" >/dev/null || fail "the plan does not format the root, keep /data and put GRUB into the MBR"
	[ "$(sums /dev/vda2)" = "$kept" ] || fail "the kept device changed"
	stop
	start disk
	check <<'CHECK'
ok "the new system: $(hostname), started by BIOS from an MBR disk" '[ "$(hostname)" = mbr.example.net ] && [ ! -d /sys/firmware/efi ]'
ok "/data is the old partition, its marker there" '[ "$(findmnt -n -o SOURCE /data) $(cat /data/marker)" = "/dev/vda2 data-marker" ]'
ok "no unit failed $(failed)" '[ -z "$(failed)" ]'
CHECK
	stop off
}

last=""
for m in "${machines[@]}"; do
	case "$m" in
	loop) m_loop ;;
	mbr) m_mbr ;;
	main) m_main ;;
	zfs) [ "$last" = main ] || fail "zfs follows main"; m_zfs ;;
	keep-root)
		case "$last" in main) stop off; start rescue ;; zfs) ;; *) fail "keep-root follows main or zfs" ;; esac
		m_keep_root ;;
	md-luks) m_md_luks ;;
	btrfs) m_btrfs ;;
	resume) m_resume ;;
	refusals) m_refusals ;;
	*) echo "unknown machine $m (loop, main, zfs, keep-root, md-luks, btrfs, resume, refusals, mbr)" >&2; exit 2 ;;
	esac
	last="$m"
	# main's machine stays, running, for zfs and keep-root
	case "$m" in main|zfs) ;; *) [ -z "$qemu_pid" ] || stop ;; esac
done
[ -z "$qemu_pid" ] || stop
echo "==> ok"
