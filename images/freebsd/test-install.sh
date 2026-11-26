#!/usr/bin/env bash
# Install FreeBSD with install/freebsd/install.sh in QEMU and check the
# result. The disks are staged as install.sh of the Linux rescue system
# leaves them: the UMP FreeBSD rescue image at the start of every disk,
# its backup partition table moved to the end of the disk (sgdisk -e -G),
# and the boot config on its partition umpboot (ump/cmdline,
# ump/authorized_keys and, unless PLAN=none, ump/plan.json). The firmware
# starts the rescue system from the disk; the installer is fetched from a
# local storage and run there as ump runs it, with --spec and --record:
#
#   1. what must be refused and write nothing: --dry-run, a plan whose
#      disk is not there, a plan with a disk it cannot tell from one that
#      is left alone, keep_data, a layout of the Linux systems, a user
#      FreeBSD's base has, an unknown time zone, an image of another
#      release, of the other installer, or missing; QEMU counts the
#      writes to every disk, and there must be none meanwhile
#   2. the installation, then the reboot from the disks
#   3. the new system, over SSH as its administrator: the release, the
#      hostname, the pool and its devices, the interface names, the
#      address, the resolvers, sudo, pf's rules and what they let in, the
#      userdata entry's mark, swap, the boot loader on every disk, the
#      install record, the host key of --record, the time zone, Python
#   4. with a mirror or a raidz, the system started again without its
#      first disk
#   5. with AGAIN=1, a second installation over the first, the disks
#      staged again as they are
#
# Usage: test-install.sh [arm64|amd64] [RELEASE]   (default: this machine's
#        architecture, 15.1)
# Needs the rescue image (disk.img.zst) and the system's files
# (manifest.json, base.txz, kernel.txz, packages.tar) built, qemu, jq,
# python3, zstd, and sgdisk and mtools here or a container engine to run
# them in. Environment:
#   RESCUE, IMAGE  their directories (default out/rescue/freebsd-RELEASE/ARCH
#                  and out/os/freebsd-RELEASE/ARCH beside this script)
#   LAYOUT         zfs (default), zfs-mirror, zfs-raidz or zfs-raidz2
#   DISKS, DISK    the number of disks (default: what the layout needs)
#                  and their size in GB (default 10)
#   FIRMWARE       uefi (default) or bios (amd64)
#   MEM            MB (default 1024)
#   NET            static (default), dhcp, or onlink (the address as a /32
#                  and its gateway outside it, as some providers give
#                  it): the boot config's network
#   UPLINK         static, dhcp or dhcp-dns: the specification's uplink
#                  (network.uplink), as a PXE machine has it; default none
#   PLAN           yes (default): plan.json in the boot config; none: the
#                  installer plans itself from the layout
#   SERIALS        1: the disks have serial numbers, and plan.json names
#                  them in another order than the machine (UEFI)
#   LEFT           GB: one more disk of that size, which the plan leaves
#                  alone (with PLAN=none the specification names the
#                  others); QEMU must count no write to it
#   RENAME         no: network.rename is false, the kernel's interface
#                  names stay
#   NICS           2: a second interface, which the kernel finds first, so
#                  that the uplink is its second one (with NET=static)
#   IPV6           static or auto: the profile keeps IPv6, and the
#                  specification's uplink has it (QEMU's user network,
#                  fec0::/64)
#   AGAIN          1: a second installation over the first
#   FAIL           1: first an installation that fails after the disks
#                  were cleared (a userdata entry that fails), then the
#                  installation, in the same rescue system
#   PORT           the first of four host ports (default 2262): SSH, the
#                  storage, and two for the firewall check
#   TIMEOUT        seconds to wait for SSH (default 600)
#   WORK           the work directory (default: a new one under /tmp)
#   CONTAINER, STAGE_IMAGE, CONTAINER_NAME  the container engine, the image
#                  and the container's name the disks are staged in where
#                  sgdisk or mtools is missing here (default docker or
#                  podman, debian:trixie, which then installs them at
#                  every run)
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
RELEASE="${2:-15.1}"
OS="freebsd-$RELEASE"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
RESCUE="${RESCUE:-$HERE/out/rescue/$OS/$ARCH}"
IMAGE="${IMAGE:-$HERE/out/os/$OS/$ARCH}"
LAYOUT="${LAYOUT:-zfs}"
case "$LAYOUT" in
zfs) need=1; vdev=stripe; raid=null ;;
zfs-mirror) need=2; vdev=mirror; raid=1 ;;
zfs-raidz) need=3; vdev=raidz; raid=5 ;;
zfs-raidz2) need=4; vdev=raidz2; raid=6 ;;
*) echo "LAYOUT is zfs, zfs-mirror, zfs-raidz or zfs-raidz2" >&2; exit 2 ;;
esac
DISKS="${DISKS:-$need}"; DISK="${DISK:-10}"
FIRMWARE="${FIRMWARE:-uefi}"
MEM="${MEM:-1024}"
NET="${NET:-static}"
UPLINK="${UPLINK:-}"
PLAN="${PLAN:-yes}"
SERIALS="${SERIALS:-0}"
LEFT="${LEFT:-}"
RENAME="${RENAME:-yes}"
NICS="${NICS:-1}"
IPV6="${IPV6:-}"
AGAIN="${AGAIN:-0}"
FAIL="${FAIL:-0}"
PORT="${PORT:-2262}"
HTTP=$((PORT + 1)); OPEN=$((PORT + 2)); CLOSED=$((PORT + 3))
TIMEOUT="${TIMEOUT:-600}"
MAC=52:54:00:12:34:56; MAC2=52:54:00:12:34:57
CONTAINER="${CONTAINER:-$(command -v docker || command -v podman || true)}"
STAGE_IMAGE="${STAGE_IMAGE:-debian:trixie}"
[ "$DISKS" -ge "$need" ] || { echo "layout $LAYOUT needs $need disks" >&2; exit 2; }
[ "$FIRMWARE" = uefi ] || [ "$ARCH" = amd64 ] || { echo "FIRMWARE=bios is for amd64" >&2; exit 2; }
[ -f "$RESCUE/disk.img.zst" ] || { echo "no rescue image in $RESCUE (RESCUE names its directory)" >&2; exit 2; }
[ -f "$IMAGE/manifest.json" ] || { echo "no system in $IMAGE (IMAGE names its directory)" >&2; exit 2; }
for t in jq python3 ssh-keygen; do command -v "$t" >/dev/null || { echo "test-install.sh: $t is needed" >&2; exit 2; }; done
if [ -n "${WORK:-}" ]; then mkdir -p "$WORK"; else WORK="$(mktemp -d /tmp/ump-freebsd-install-test.XXXXXX)"; fi
WORK="$(cd "$WORK" && pwd)"
# (QEMU's monitor socket, where its path is short enough for one)
QMP="$(mktemp -d /tmp/ump-freebsd-qmp.XXXXXX)"
qemu_pid="" http_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$http_pid" ] && kill "$http_pid" 2>/dev/null || true
	rm -rf "$QMP"
	echo "work directory (serial logs, disks): $WORK"
}
trap cleanup EXIT

host_os="$(uname -s)"
case "$ARCH" in
arm64)
	qemu=qemu-system-aarch64; fw=edk2-aarch64-code.fd; vars=edk2-arm-vars.fd
	if [ "$host_os" = Darwin ] && [ "$(uname -m)" = arm64 ]; then machine=(-M virt -accel hvf -cpu host)
	elif [ "$host_os" = Linux ] && [ "$(uname -m)" = aarch64 ] && [ -w /dev/kvm ]; then machine=(-M virt -accel kvm -cpu host)
	else machine=(-M virt -cpu cortex-a72); fi
	;;
amd64)
	qemu=qemu-system-x86_64; fw=edk2-x86_64-code.fd; vars=edk2-i386-vars.fd
	# emulated, the CPU model is qemu64: with QEMU's newer models the
	# kernel of FreeBSD 15.1 ends in a page fault while zfs.ko loads
	if [ "$host_os" = Linux ] && [ "$(uname -m)" = x86_64 ] && [ -w /dev/kvm ]; then machine=(-M q35 -accel kvm -cpu host)
	else machine=(-M q35 -cpu qemu64); fi
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
command -v "$qemu" >/dev/null || { echo "$qemu not found" >&2; exit 2; }
firmware=()
if [ "$FIRMWARE" = uefi ]; then
	share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
	cp "$share/$fw" "$WORK/code.fd"; cp "$share/$vars" "$WORK/vars.fd"
	[ "$ARCH" != arm64 ] || truncate -s 64M "$WORK/code.fd" "$WORK/vars.fd"
	firmware=(-drive "if=pflash,format=raw,readonly=on,file=$WORK/code.fd" -drive "if=pflash,format=raw,file=$WORK/vars.fd")
fi

# the storage: the system's files and the installer
mkdir -p "$WORK/storage/os/$OS/$ARCH" "$WORK/storage/install/freebsd"
for f in manifest.json base.txz kernel.txz packages.tar; do ln -sf "$IMAGE/$f" "$WORK/storage/os/$OS/$ARCH/$f"; done
cp "$ROOT/install/freebsd/install.sh" "$WORK/storage/install/freebsd/install.sh"
# and two images the installer must refuse: one of another release than
# the rescue system's, one of the installer of the Linux rescue system
mkdir -p "$WORK/storage/os/freebsd-0.0/$ARCH" "$WORK/storage/os/ubuntu-26.04/$ARCH"
jq '.os = "freebsd-0.0" | .release = "0.0-RELEASE"' "$IMAGE/manifest.json" > "$WORK/storage/os/freebsd-0.0/$ARCH/manifest.json"
echo '{"version": "2026.11.26", "os": "ubuntu-26.04", "files": {"rootfs.tar.zst": {"sha256": "0", "size": 0}}}' > "$WORK/storage/os/ubuntu-26.04/$ARCH/manifest.json"
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/storage" "$HTTP" >/dev/null 2>&1 &
http_pid=$!

[ -f "$WORK/key" ] || ssh-keygen -q -t ed25519 -N '' -C ump-freebsd-install-test -f "$WORK/key"
pub="$(cat "$WORK/key.pub")"
ssh_opts=(-i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
	-o ConnectTimeout=5 -o ServerAliveInterval=10 -p "$PORT")
on() { local user="$1"; shift; ssh "${ssh_opts[@]}" "$user@127.0.0.1" "$@"; }
fail() { echo "FAILED: $*"; exit 1; }
# writes: how often the machine has written to each of its disks since
# it was started, as QEMU counts it (DISK:WRITES:FLUSHES ...)
writes() {
	python3 - "$QMP/sock" <<'ASK'
import json, socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
f = s.makefile("rw")
json.loads(f.readline())
def ask(command):
    f.write(json.dumps({"execute": command}) + "\n")
    f.flush()
    while True:
        answer = json.loads(f.readline())
        if "return" in answer or "error" in answer:
            return answer.get("return")
ask("qmp_capabilities")
disks = [d for d in ask("query-blockstats") if (d.get("device") or "").startswith("disk")]
print(" ".join("%s:%d:%d" % (d["device"], d["stats"]["wr_operations"], d["stats"]["flush_operations"]) for d in sorted(disks, key=lambda d: d["device"])))
ASK
}

# the layouts of layouts.yaml, as ump renders them: the pool's name, and
# its raid level where it has one
case "$LAYOUT" in
zfs) boot_target='["first"]' ;;
*) boot_target='[".*"]' ;;
esac
layout="$(jq -n --arg name "$LAYOUT" --argjson raid "$raid" --argjson boot "$boot_target" '{name: $name, parts: [
	{type: "slice", target: $boot, size: "512K", filesystem: "bios"},
	{type: "slice", target: $boot, size: "512M", filesystem: "efi", mountpoint: "/boot/efi"},
	{type: "slice", target: $boot, size: "2G", filesystem: "swap"},
	({type: "zfs", target: [".*"], size: "rest", name: "zroot", mountpoint: "/"} + if $raid == null then {} else {raid: $raid} end)]}')"
# the uplink of a PXE machine: QEMU's user network, by DHCP with its
# resolver, or the same address static
case "$UPLINK" in
"") uplink=null ;;
dhcp) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "dhcp"}, ipv6: {mode: "none"}, dns_from_dhcp: true}')" ;;
dhcp-dns) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "dhcp"}, ipv6: {mode: "none"}, dns: ["192.0.2.53"]}')" ;;
static) uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "static", address: "10.0.2.15/24", gateway: "10.0.2.2"}, ipv6: {mode: "none"}, dns: ["10.0.2.3"]}')" ;;
*) echo "UPLINK is static, dhcp or dhcp-dns" >&2; exit 2 ;;
esac
# IPv6 in QEMU's user network: fec0::/64, the router fe80::2
case "$IPV6" in
"") ;;
static|auto)
	[ "$uplink" != null ] || uplink="$(jq -n --arg mac "$MAC" '{mac: $mac, ipv4: {mode: "static", address: "10.0.2.15/24", gateway: "10.0.2.2"}}')"
	uplink="$(jq --arg mode "$IPV6" '.ipv6 = (if $mode == "static" then {mode: "static", address: "fec0::15/64", gateway: "fe80::2"} else {mode: "auto"} end)' <<< "$uplink")" ;;
*) echo "IPV6 is static or auto" >&2; exit 2 ;;
esac
IFNAME=ext01; [ "$RENAME" = yes ] || IFNAME="vtnet$((NICS - 1))"
# a second interface in front of the uplink, with a network of its own
# that nothing configures
nics=()
[ "$NICS" = 1 ] || nics=(-netdev "user,id=n1,restrict=on" -device "virtio-net-pci,netdev=n1,mac=$MAC2")
# spec HOSTNAME: the specification, as ump renders it: an administrator
# with sudo and a group, a userdata entry that leaves a mark, a firewall
# with a range and a rule with a source besides ICMP and SSH
spec() {
	jq -n --arg key "$pub" --arg storage "http://10.0.2.2:$HTTP" --arg os "$OS" --arg hostname "$1" --argjson layout "$layout" --argjson uplink "$uplink" \
		--argjson disks "$spec_disks" --argjson ipv6 "$([ -n "$IPV6" ] && echo true || echo false)" --argjson rename "$([ "$RENAME" = yes ] && echo true || echo false)" '{
		os: $os, hostname: $hostname, timezone: "Europe/Berlin", storage: $storage,
		kernel: {mitigations: false, ipv6: $ipv6, extra: "kern.msgbufsize=131072"},
		layout: $layout, disks: $disks,
		users: [{name: "ump", sudo: true, groups: ["operator", "deploy"], shell: "/bin/bash", keys: [$key]}],
		userdata: [{name: "test", type: "bash", script: "#!/usr/local/bin/bash\nset -e\necho merged > /etc/ump-userdata-test\nsysrc -q ump_userdata_test=yes\necho \"$UMP_USERDATA for $UMP_ADMIN on $UMP_HOSTNAME ($UMP_OS, $UMP_PROFILE)\"\n"}],
		firewall: {policy: "drop", allow: [{proto: "icmp"}, {proto: "tcp", port: "22"}, {proto: "tcp", port: "8000-8100"}, {proto: "tcp", port: "9100", from: "10.0.0.0/8"}]},
		network: {rename: $rename, private_networks: [{network: "default", cidr: "10.0.0.0/24", address: "10.0.0.7"}]},
		profile: {name: "test"}} | if $uplink then .network.uplink = $uplink else . end'
}
# the disks, and the plan the Linux rescue system would have made for
# them: its names, the sizes, the serial numbers where the disks have
# any (then in another order than the machine has them: the plan's first
# disk is the machine's last)
size=$((DISK * 1024 * 1024 * 1024))
drives=(); plan_disks='[]'
for i in $(seq 1 "$DISKS"); do
	[ -f "$WORK/disk$i.raw" ] || truncate -s "${DISK}G" "$WORK/disk$i.raw"
	serial=""; [ "$SERIALS" != 1 ] || serial="UMPTEST$i"
	drives+=(-drive "file=$WORK/disk$i.raw,format=raw,if=none,id=disk$i" -device "virtio-blk-pci,drive=disk$i${serial:+,serial=$serial}")
	plan_disks="$(jq -c --argjson size "$size" --arg serial "$serial" '. + [{size: $size, serial: $serial, wwn: ""}]' <<< "$plan_disks")"
done
[ "$SERIALS" != 1 ] || plan_disks="$(jq -c reverse <<< "$plan_disks")"
# a disk the plan leaves alone: not staged, with something on it; without
# a plan.json the specification names the disks to use, as FreeBSD does
plan_left='[]'; spec_disks='[]'
if [ -n "$LEFT" ]; then
	[ -f "$WORK/left.raw" ] || { truncate -s "${LEFT}G" "$WORK/left.raw"; dd if=/dev/urandom of="$WORK/left.raw" bs=1048576 seek=1 count=4 conv=notrunc 2>/dev/null; }
	serial=""; [ "$SERIALS" != 1 ] || serial="UMPTESTLEFT"
	drives+=(-drive "file=$WORK/left.raw,format=raw,if=none,id=diskleft" -device "virtio-blk-pci,drive=diskleft${serial:+,serial=$serial}")
	plan_left="$(jq -nc --argjson size $((LEFT * 1024 * 1024 * 1024)) --arg serial "$serial" '[{name: "/dev/vdz", size: $size, serial: $serial}]')"
	[ "$PLAN" = yes ] || spec_disks="$(jq -nc --argjson n "$DISKS" '[range($n) | "/dev/vtbd\(.)"]')"
fi
plan="$(jq -n --arg firmware "$FIRMWARE" --arg vdev "$vdev" --arg layout "$LAYOUT" --argjson disks "$plan_disks" --argjson left "$plan_left" '
	def all: [{index: 1, kind: "bios", size: "512K"}, {index: 2, kind: "efi", size: "512M"}, {index: 3, kind: "swap", size: "2G"}, {index: 4, kind: "zfs", size: "rest"}];
	{version: 1, firmware: $firmware, boot_disk: "/dev/vda",
	 disks: [$disks | to_entries[] | .value + {name: "/dev/vd\([.key + 97] | implode)", parts: (if .key == 0 or $layout != "zfs" then all else [{index: 1, kind: "zfs", size: "rest"}] end)}],
	 left: $left, pool: {name: "zroot", vdev: $vdev, mountpoint: "/"}}')"

# stage: the disks as install.sh of the Linux rescue system leaves them
stage() {
	local words="ump.mac=$MAC ump.dns=10.0.2.3 ump.hostname=rescue ump.time=$(date +%s)" script i
	[ "$NET" != static ] || words="$words ump.ip=10.0.2.15/24,10.0.2.2"
	[ "$NET" != onlink ] || words="$words ump.ip=10.0.2.15/32,10.0.2.2"
	mkdir -p "$WORK/ump"; rm -f "$WORK/ump/plan.json"
	echo "$words" > "$WORK/ump/cmdline"
	cp "$WORK/key.pub" "$WORK/ump/authorized_keys"
	[ "$PLAN" != yes ] || echo "$plan" > "$WORK/ump/plan.json"
	script='set -e
	for d in disk*.raw; do
		zstd -dc /rescue/disk.img.zst | dd of="$d" bs=1M conv=notrunc status=none
		sgdisk -e -G "$d" >/dev/null 2>&1
		start="$(sgdisk -i 1 "$d" 2>/dev/null | awk "/^First sector:/ {print \$3}")"
		mmd -i "$d@@$((start * 512))" ::/ump 2>/dev/null || true
		mcopy -o -i "$d@@$((start * 512))" ump/* ::/ump/
	done'
	if command -v sgdisk >/dev/null && command -v mcopy >/dev/null && command -v zstd >/dev/null; then
		(cd "$WORK" && ln -sfn "$RESCUE" rescue && bash -c "${script//\/rescue\//rescue/}")
	else
		[ -n "$CONTAINER" ] || { echo "test-install.sh: sgdisk and mtools are needed, or docker or podman to run them in" >&2; exit 2; }
		"$CONTAINER" run --rm --name "${CONTAINER_NAME:-ump-freebsd-install-test-$PORT}" -v "$WORK:/work" -v "$RESCUE:/rescue:ro" -w /work "$STAGE_IMAGE" bash -c "
			command -v sgdisk >/dev/null && command -v mcopy >/dev/null && command -v zstd >/dev/null || { apt-get update -qq && apt-get install -y -qq gdisk mtools zstd; } >/dev/null 2>&1
			$script"
	fi
}
boot() { # boot LOG [without the first disk]
	local use=("${drives[@]}")
	[ -z "${2:-}" ] || use=("${drives[@]:4}")
	"$qemu" "${machine[@]}" ${firmware[@]+"${firmware[@]}"} -m "$MEM" -smp 2 -nographic ${nics[@]+"${nics[@]}"} \
		-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22,hostfwd=tcp:127.0.0.1:$OPEN-:8080,hostfwd=tcp:127.0.0.1:$CLOSED-:9999" \
		-device "virtio-net-pci,netdev=n0,mac=$MAC" "${use[@]}" \
		-qmp "unix:$QMP/sock,server=on,wait=off" \
		-serial "file:$WORK/$1" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
	serial="$WORK/$1"
}
halt() { # halt USER COMMAND: the machine switched off
	on "$1" "$2" >/dev/null 2>&1 || true
	for _ in $(seq 1 60); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
	kill "$qemu_pid" 2>/dev/null || true; wait "$qemu_pid" 2>/dev/null || true; qemu_pid=""
}
wait_ssh() { # wait_ssh USER [COMMAND]: until the command succeeds there
	local start deadline; start=$(date +%s); deadline=$((start + TIMEOUT))
	until on "$1" "${2:-true}" >/dev/null 2>&1; do
		kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited"; cat "$WORK/qemu.err"; tail -n 40 "$serial"; exit 1; }
		[ "$(date +%s)" -le "$deadline" ] || { echo "no SSH as $1 within ${TIMEOUT}s"; tail -n 40 "$serial"; exit 1; }
		sleep 2
	done
	echo $(( $(date +%s) - start ))
}
# refused WHAT WORDS ENVIRONMENT SPEC: the installer must refuse (exit
# status 2), say WORDS and that nothing was written
refused() {
	local out rc=0
	out="$(on root "cat > /tmp/ump-refused.json && env $3 sh /tmp/ump-install.sh --spec /tmp/ump-refused.json" <<< "$4" 2>&1)" || rc=$?
	[ "$rc" = 2 ] || fail "$1: exit status $rc, not 2: $out"
	grep -qF "install.sh: nothing was written to the disks" <<< "$out" || fail "$1: no word that nothing was written: $out"
	grep -qF "$2" <<< "$out" || fail "$1: not refused for that: $out"
	echo "refused: $1 ($(grep '^install.sh: ' <<< "$out" | head -n 1 | cut -c 13-140))"
}
# install HOSTNAME: the rescue system from the staged disks, what it
# must refuse, the installation, the reboot into the new system
install() {
	local name="$1" secs before start out rc
	stage
	echo "==> booting the FreeBSD rescue system from the staged disk(s) ($ARCH, $FIRMWARE, ${MEM}M, $DISKS disk(s) of ${DISK}G$([ "$SERIALS" != 1 ] || echo ", with serial numbers"))"
	boot "serial-rescue-$name.log"
	secs="$(wait_ssh root 'test -f /etc/ump-rescue-version')" || { echo "$secs"; exit 1; }
	echo "rescue system: up after ${secs}s, ump-rescue $(on root 'cat /etc/ump-rescue-version; uname -sr; echo "root on $(mount -p | awk "\$2 == \"/\" {print \$1}")"' | paste -sd' ' -)"
	on root 'mount -p' | grep -qv '^/dev/md\|^devfs\|^tmpfs' && fail "a disk is mounted in the rescue system"
	spec "$name" > "$WORK/spec.json"
	on root "cat > /tmp/ump-install.json && fetch -qo /tmp/ump-install.sh http://10.0.2.2:$HTTP/install/freebsd/install.sh" < "$WORK/spec.json"

	# what writes nothing: QEMU's count of the writes to every disk,
	# before and after
	before="$(writes)"
	on root 'sh /tmp/ump-install.sh --spec /tmp/ump-install.json --dry-run' > "$WORK/dry-run-$name.log" 2>&1 || fail "the dry run: $(cat "$WORK/dry-run-$name.log")"
	grep '^==> ' "$WORK/dry-run-$name.log" | sed 's/^/    /'
	grep -q '^==> dry run: stopping here, no disk was touched' "$WORK/dry-run-$name.log" || fail "the dry run did not stop"
	if [ "$PLAN" = yes ]; then grep -q '^==> plan: the plan of the Linux rescue system' "$WORK/dry-run-$name.log" || fail "the plan is not plan.json's"
	else grep -q '^==> plan: no plan.json' "$WORK/dry-run-$name.log" || fail "the plan is not the installer's own"; fi
	on root 'mkdir -p /tmp/bc-gone /tmp/bc-alike /tmp/bc-none; cp /etc/ump/boot/cmdline /tmp/bc-gone/; cp /etc/ump/boot/cmdline /tmp/bc-alike/; cp /etc/ump/boot/cmdline /tmp/bc-none/'
	jq '.disks += [{name: "/dev/nvme7n1", size: 512110190592, serial: "S4EWNX0N123456", wwn: "", parts: .disks[0].parts}]' <<< "$plan" | on root 'cat > /tmp/bc-gone/plan.json'
	refused "a plan with a disk that is not here" "/dev/nvme7n1 (512110190592 bytes, serial S4EWNX0N123456) is not here" "UMP_BOOT_CONFIG=/tmp/bc-gone" "$(cat "$WORK/spec.json")"
	if [ "$DISKS" -ge 2 ]; then
		jq --argjson size "$size" '.disks = [.disks[0] | .serial = ""] | .left = [{name: "/dev/vdz", size: $size, serial: ""}] | .pool.vdev = "stripe"' <<< "$plan" | on root 'cat > /tmp/bc-alike/plan.json'
		refused "a plan with a disk like one it leaves alone" "cannot be told from" "UMP_BOOT_CONFIG=/tmp/bc-alike" "$(cat "$WORK/spec.json")"
	fi
	refused "keep_data" "keep-data" "" "$(jq '.keep_data = {}' "$WORK/spec.json")"
	refused "a layout of the Linux systems, no plan.json" "is for the Linux systems" "UMP_BOOT_CONFIG=/tmp/bc-none" "$(jq '.layout = {name: "lvm-linear", parts: [
		{type: "slice", target: ["first"], size: "512M", filesystem: "efi", mountpoint: "/boot/efi"},
		{type: "slice", target: [".*"], size: "rest", filesystem: "lvm", vg: "vg0"},
		{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: "ext4", mountpoint: "/"}]}' "$WORK/spec.json")"
	refused "a user FreeBSD's base has" "system account of FreeBSD" "" "$(jq '.users += [{name: "daemon", keys: []}]' "$WORK/spec.json")"
	refused "an unknown time zone" "unknown time zone" "" "$(jq '.timezone = "Europe/Atlantis"' "$WORK/spec.json")"
	refused "an image of another release than the rescue system's" "the pool is made by the release that will import it" "" "$(jq '.os = "freebsd-0.0"' "$WORK/spec.json")"
	refused "an image of the other installer" "is not freebsd" "" "$(jq '.os = "ubuntu-26.04"' "$WORK/spec.json")"
	refused "an image the storage does not have" "cannot fetch" "" "$(jq '.os = "freebsd-9.9"' "$WORK/spec.json")"
	[ -n "$before" ] && [ "$(writes)" = "$before" ] || fail "the dry run or a refusal wrote to a disk (writes and flushes before: $before; after: $(writes))"
	echo "the dry run and the refusals wrote nothing: no write to a disk meanwhile, as QEMU counts them ($before)"
	[ -z "$LEFT" ] || grep -q '^==> disks .*; left alone: vtbd' "$WORK/dry-run-$name.log" || fail "the plan does not leave the last disk alone"

	if [ "$FAIL" = 1 ]; then
		# a failure after the first write: status 1, the reason, the machine
		# left in the rescue system with nothing of the disks in use
		rc=0
		out="$(jq '.userdata += [{name: "broken", type: "bash", script: "echo no luck today; exit 7\n"}]' "$WORK/spec.json" \
			| on root 'cat > /tmp/ump-failing.json && sh /tmp/ump-install.sh --spec /tmp/ump-failing.json' 2>&1)" || rc=$?
		[ "$rc" = 1 ] || fail "an installation whose userdata fails: exit status $rc, not 1: $(tail -n 5 <<< "$out")"
		grep -qF "no luck today" <<< "$out" && grep -qF "install.sh: userdata broken failed" <<< "$out" || fail "the failed installation does not say why: $(tail -n 5 <<< "$out")"
		grep -qF "install.sh: the installation is not complete" <<< "$out" || fail "the failed installation does not say what it leaves"
		grep -qF "nothing was written" <<< "$out" && fail "the failed installation claims that nothing was written"
		on root '[ -z "$(zpool list -H -o name 2>/dev/null)" ] && ! mount -p | grep -q "[[:space:]]/mnt"' || fail "the failed installation left a pool imported or something mounted"
		echo "failed as it must: an installation whose userdata fails (exit status 1, the reason, the pool exported; the rescue system stays)"
	fi
	echo "==> install.sh --spec /tmp/ump-install.json --record /tmp/ump-record ($OS, layout $LAYOUT${UPLINK:+, uplink $UPLINK}, $([ "$PLAN" = yes ] && echo "plan.json" || echo "no plan.json"), boot config $NET)"
	start=$(date +%s)
	on root 'sh /tmp/ump-install.sh --spec /tmp/ump-install.json --record /tmp/ump-record' 2>&1 | tee "$WORK/install-$name.log" | grep '^==>\|install.sh' || true
	grep -q "^==> installed: $OS" "$WORK/install-$name.log" || { echo "the installation failed"; tail -n 30 "$WORK/install-$name.log"; exit 1; }
	rm -rf "$WORK/record"; mkdir -p "$WORK/record"
	scp -q -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT" "root@127.0.0.1:/tmp/ump-record/*" "$WORK/record/"
	if [ -n "$LEFT" ]; then
		grep -q 'diskleft:0:0' <<< "$(writes)" || fail "the installer wrote to the disk the plan leaves alone ($(writes))"
		echo "the disk left alone: no write to it since the machine was started, as QEMU counts them"
	fi
	echo "==> installed in $(( $(date +%s) - start ))s; rebooting into the new system"
	start=$(date +%s)
	on root 'shutdown -r now' >/dev/null 2>&1 || true
	sleep 5
	secs="$(wait_ssh ump)" || { echo "$secs"; exit 1; }
	echo "new system: SSH as ump $(( $(date +%s) - start ))s after the reboot"
}
# check HOSTNAME [degraded]: the new system, as its administrator
check() {
	on ump "HOST=$1 OS=$OS RELEASE=$RELEASE LAYOUT=$LAYOUT VDEV=$vdev DISKS=$DISKS NET=$NET UPLINK=$UPLINK IPV6=$IPV6 IF=$IFNAME NICS=$NICS MAC=$MAC MAC2=$MAC2 ARCH=$ARCH FIRMWARE=$FIRMWARE SERIALS=$SERIALS DEGRADED=${2:-} RECORD_KEY='$(awk '{print $2}' "$WORK/record/ssh_host_ed25519_key.pub")' sh -s" <<'CHECK'
set -e
# ok WHAT COMMAND...: say so, or fail the run
ok() { what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
echo "os: $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')   hostname: $(hostname)   kernel: $(uname -r)   firmware: $(sysctl -n machdep.bootmethod 2>/dev/null || echo UEFI)   memory: $(( $(sysctl -n hw.realmem) / 1048576 )) MB"
[ "$ARCH" != amd64 ] || ok "firmware: the system was started by $(echo "$FIRMWARE" | tr a-z A-Z)" sh -c '[ "$(sysctl -n machdep.bootmethod | tr A-Z a-z)" = "$FIRMWARE" ]'
ok "system: FreeBSD $RELEASE-RELEASE" sh -c 'grep -q "^PRETTY_NAME=\"FreeBSD $RELEASE-RELEASE\"" /etc/os-release && [ "$(freebsd-version -u)" = "$RELEASE-RELEASE" ]'
ok "hostname: $HOST, and it resolves here" sh -c '[ "$(hostname)" = "$HOST" ] && ping -c 1 -t 2 "$HOST"'
ok "root: $(mount -p | awk '$2 == "/" {print $1, $3}'), boot environment $(bectl list -H | awk '{print $1, $2}')" sh -c 'mount -p | grep -q "^zroot/ROOT/default[[:space:]]*/[[:space:]]" && bectl list -H | grep -q "^default[[:space:]]*NR"'
ok "datasets: /home, /tmp, /var/log, /var/tmp and the others, mounted" sh -c 'for d in home tmp var/audit var/crash var/log var/mail var/tmp; do mount -p | grep -q "^zroot/$d[[:space:]]" || exit 1; done; [ "$(zfs get -H -o value canmount zroot/usr)" = off ] && [ "$(zfs get -H -o value compression zroot)" = lz4 ]'
if [ -z "$DEGRADED" ]; then
	ok "pool: $(zpool list -H -o name,size,health | tr '\t' ' '), $VDEV over $(zpool status zroot | grep -c 'gpt/zfs') device(s)" sh -c '[ "$(zpool list -H -o health zroot)" = ONLINE ] && [ "$(zpool status zroot | grep -c "gpt/zfs")" = "$DISKS" ]'
	case "$VDEV" in
	stripe) ok "pool: no mirror or raidz" sh -c '! zpool status zroot | grep -q "mirror\|raidz"' ;;
	raidz) ok "pool: raidz1-0" sh -c 'zpool status zroot | grep -q "raidz1-0"' ;;
	*) ok "pool: $VDEV-0" sh -c 'zpool status zroot | grep -q "$VDEV-0"' ;;
	esac
else
	ok "pool: $(zpool list -H -o name,health | tr '\t' ' ') without its first disk, $(zpool status zroot | grep -c 'gpt/zfs.*ONLINE') of $DISKS devices" sh -c '[ "$(zpool list -H -o health zroot)" = DEGRADED ] && [ "$(zpool status zroot | grep -c "gpt/zfs.*ONLINE")" = "$((DISKS - 1))" ]'
fi
if [ "$IF" = ext01 ]; then
	ok "interface names: ext01 is the uplink, bound to its MAC ($(ifconfig -l ether))" sh -c 'ifconfig ext01 ether | grep -q "$MAC" && [ -x /usr/local/etc/rc.d/ump_ifnames ]'
else
	ok "interface names: the kernel's ($(ifconfig -l ether)), no ump_ifnames" sh -c 'ifconfig "$IF" ether | grep -q "$MAC" && ! grep -q ump_ifnames /etc/rc.conf && [ ! -e /usr/local/etc/rc.d/ump_ifnames ]'
fi
names="$IF=$MAC"
if [ "$NICS" = 2 ]; then
	other=int01; [ "$IF" = ext01 ] || other=vtnet0
	names="$other=$MAC2 $IF=$MAC"
	ok "interface names: $other is the other interface, the kernel's first, up without an address" sh -c 'ifconfig "$1" ether | grep -q "$MAC2" && ifconfig "$1" | grep -q "<UP" && ! ifconfig "$1" | grep -q "inet " && grep -q "^ifconfig_$1=\"up\"" /etc/rc.conf' _ "$other"
fi
case "$IPV6" in
"") ok "IPv6: no address on $IF" sh -c '! ifconfig "$IF" | grep -q "inet6"' ;;
static) ok "IPv6: $(ifconfig "$IF" inet6 | awk '$1 == "inet6" && $2 !~ /^fe80/ {print $2 "/" $4}') static, the router fe80::2 answers through it (fec0::2)" sh -c 'ifconfig "$IF" inet6 | grep -q "inet6 fec0::15 prefixlen 64" && netstat -rn -f inet6 | grep -q "^default[[:space:]]*fe80::2%$IF" && ping -6 -c 1 -t 5 fec0::2' ;;
auto) ok "IPv6: $(ifconfig "$IF" inet6 | awk '$1 == "inet6" && $2 !~ /^fe80/ {print $2 "/" $4}') from router advertisements, the router answers (fec0::2)" sh -c 'ifconfig "$IF" inet6 | grep -q "inet6 fec0:.*autoconf" && ping -6 -c 1 -t 5 fec0::2' ;;
esac
[ -z "$IPV6" ] || ok "IPv6: $(grep '^ipv6=\|^network6=' /etc/ump/install | paste -sd' ' -) in the install record" sh -c 'grep -q "^ipv6=on$" /etc/ump/install && grep -q "^network6=$IPV6$" /etc/ump/install'
mode="$NET"; [ -z "$UPLINK" ] || mode="${UPLINK%-dns}"
[ -z "$IPV6" ] || [ -n "$UPLINK" ] || mode=static
if [ "$mode" = onlink ]; then
	ok "address: static, 10.0.2.15/32, the gateway 10.0.2.2 on-link and the default route through it" sh -c 'grep -q "^ifconfig_$IF=\"inet 10.0.2.15/32\"" /etc/rc.conf && grep -q "^static_routes=\"umpgw umpdefault\"" /etc/rc.conf && ifconfig "$IF" inet | grep -q "inet 10.0.2.15 netmask 0xffffffff" && route -n get default | grep -q "gateway: 10.0.2.2" && route -n get 10.0.2.2 | grep -q "interface: $IF"'
elif [ "$mode" = dhcp ]; then
	ok "address: by DHCP ($(ifconfig -f inet:cidr "$IF" inet | awk '$1 == "inet" {print $2}'), dhclient on $IF)" sh -c 'grep -q "^ifconfig_$IF=\"DHCP\"" /etc/rc.conf && pgrep -f "dhclient: $IF" && ifconfig "$IF" inet | grep -q "inet 10.0.2.15"'
else
	ok "address: static, 10.0.2.15/24, the default route through 10.0.2.2" sh -c 'grep -q "^ifconfig_$IF=\"inet 10.0.2.15/24\"" /etc/rc.conf && ! pgrep -q dhclient && ifconfig "$IF" inet | grep -q "inet 10.0.2.15" && route -n get default | grep -q "gateway: 10.0.2.2"'
fi
resolvers="$(awk '/^nameserver/ {print $2}' /etc/resolv.conf | paste -sd, -)"
resolvers_of="$UPLINK"; [ -z "$IPV6" ] || [ -n "$UPLINK" ] || resolvers_of=public
case "$resolvers_of" in
""|public) ok "resolvers: the public ones ($resolvers), whatever a lease says" sh -c '[ "$1" = 1.1.1.1,8.8.8.8 ] && grep -q "^resolvconf=NO" /etc/resolvconf.conf' _ "$resolvers" ;;
dhcp) ok "resolvers: 10.0.2.3 alone, from the lease" sh -c '[ "$1" = 10.0.2.3 ] && [ ! -e /etc/resolvconf.conf ]' _ "$resolvers" ;;
static) ok "resolvers: 10.0.2.3 alone, from the specification" [ "$resolvers" = 10.0.2.3 ] ;;
dhcp-dns) ok "resolvers: 192.0.2.53 alone, from the specification, not the lease's 10.0.2.3" [ "$resolvers" = 192.0.2.53 ] ;;
esac
[ -z "$UPLINK" ] || ok "uplink: $(grep '^network=\|^dns=' /etc/ump/install | paste -sd' ' -) in the install record" grep -q "^network=${UPLINK%-dns}$" /etc/ump/install
ok "user: $(id -un) ($(id -Gn | tr ' ' ',')), shell $SHELL" sh -c 'id -Gn | grep -q wheel && id -Gn | grep -q operator && id -Gn | grep -q deploy && [ "$SHELL" = /usr/local/bin/bash ]'
ok "sudo: without a password" sudo -n true
ok "root: no password, no login over SSH; sshd takes no passwords and asks no DNS" sh -c 'sudo -n grep -q "^root:\*:" /etc/master.passwd && sudo -n sshd -T | grep -qi "^permitrootlogin no" && sudo -n sshd -T | grep -qi "^passwordauthentication no" && sudo -n sshd -T | grep -qi "^kbdinteractiveauthentication no" && sudo -n sshd -T | grep -qi "^usedns no"'
ok "firewall: pf enabled, block drop in all" sh -c 'sudo -n pfctl -si | grep -q "Status: Enabled" && sudo -n pfctl -sr | grep -q "^block drop in all"'
ok "firewall: ICMP, ICMPv6 and SSH pass in" sh -c 'r="$(sudo -n pfctl -sr)"; echo "$r" | grep -q "^pass in inet proto icmp all" && echo "$r" | grep -q "^pass in inet6 proto ipv6-icmp all" && echo "$r" | grep -q "^pass in proto tcp from any to any port = ssh"'
ok "firewall: the specification's rules (tcp/8000-8100; tcp/9100 from 10.0.0.0/8)" sh -c 'r="$(sudo -n pfctl -sr)"; echo "$r" | grep -q "port 8000:8100" && echo "$r" | grep -q "from 10.0.0.0/8 to any port = "'
ok "userdata: the entry left its mark (/etc/ump-userdata-test, rc.conf), its output in /var/log/ump-userdata.log" sh -c '[ "$(cat /etc/ump-userdata-test)" = merged ] && [ "$(sysrc -n ump_userdata_test)" = yes ] && sudo -n grep -q "^test for ump on $HOST ($OS, test)" /var/log/ump-userdata.log'
boots=1; [ "$LAYOUT" = zfs ] || boots="$DISKS"
if [ -z "$DEGRADED" ]; then
	ok "swap: $(swapinfo | awk 'NR > 1 && $1 != "Total" {print $1}' | paste -sd' ' -)" sh -c '[ "$(swapinfo | grep -c "^/dev/gpt/swap")" = "$1" ]' _ "$boots"
	ok "ESP: $(mount -p | awk '$2 == "/boot/efi" {print $1}') at /boot/efi" sh -c 'mount -p | grep -q "^/dev/gpt/efi[0-9]*[[:space:]]*/boot/efi[[:space:]]*msdosfs"'
fi
efi=BOOTAA64.EFI; [ "$ARCH" != amd64 ] || efi=BOOTX64.EFI
n=0
for p in /dev/gpt/efi*; do
	[ -e "$p" ] || continue
	if mount -p | grep -q "^$p[[:space:]]"; then
		ok "boot loader: FreeBSD's loader.efi as EFI/BOOT/$efi on ${p#/dev/}" sudo -n cmp /boot/efi/EFI/BOOT/"$efi" /boot/loader.efi
	else
		sudo -n mkdir -p /tmp/esp; sudo -n mount -t msdosfs -o ro "$p" /tmp/esp
		ok "boot loader: FreeBSD's loader.efi as EFI/BOOT/$efi on ${p#/dev/}" sudo -n cmp /tmp/esp/EFI/BOOT/"$efi" /boot/loader.efi
		sudo -n umount /tmp/esp
	fi
	n=$((n + 1))
done
want="$boots"; [ -z "$DEGRADED" ] || want=$((boots - 1))
ok "boot loader: on $n disk(s)" [ "$n" = "$want" ]
if [ "$ARCH" = amd64 ]; then
	for p in /dev/gpt/boot*; do
		ok "boot loader: gptzfsboot in ${p#/dev/}" sh -c 'sudo -n dd if="$1" bs=64k count=1 2>/dev/null | cmp -s -n 65536 - /boot/gptzfsboot' _ "$p"
	done
fi
ok "install record: $(grep '^image_version=\|^root=\|^disks=\|^interfaces=\|^layout=' /etc/ump/install | paste -sd' ' -)" sh -c 'grep -q "^os=$OS$" /etc/ump/install && grep -q "^image_version=$RELEASE-RELEASE$" /etc/ump/install && grep -q "^root=zroot/ROOT/default$" /etc/ump/install && grep -q "^layout=$LAYOUT$" /etc/ump/install && grep -q "^interfaces=$1$" /etc/ump/install && grep -q "^uplink=$IF$" /etc/ump/install && grep -q "^admin=ump$" /etc/ump/install && grep -q "^firewall=drop$" /etc/ump/install && grep -q "^userdata=test$" /etc/ump/install && grep -q "^private=default:10.0.0.7$" /etc/ump/install && grep -q "^private_address=10.0.0.7$" /etc/ump/install' _ "$names"
ok "install record: the specification beside it, root's alone" sh -c '[ "$(stat -f %Lp /etc/ump/spec.json)" = 600 ] && sudo -n grep -q "\"hostname\": \"$HOST\"" /etc/ump/spec.json'
if [ "$SERIALS" = 1 ] && [ -z "$DEGRADED" ]; then
	last="vtbd$((DISKS - 1))"
	ok "plan: its first disk is the one with its serial number, $last ($(grep '^disks=\|^boot_disk=' /etc/ump/install | paste -sd' ' -))" sh -c 'grep -q "^boot_disk=/dev/$1$" /etc/ump/install && gpart show -l "$1" | grep -q "efi0"' _ "$last"
fi
ok "record (--record): the host key this system offers" grep -qF "$RECORD_KEY" /etc/ssh/ssh_host_ed25519_key.pub
ok "time zone: Europe/Berlin ($(date +%Z))" sh -c '[ "$(cat /var/db/zoneinfo)" = Europe/Berlin ] && cmp -s /etc/localtime /usr/share/zoneinfo/Europe/Berlin'
ok "loader: the profile's tunable (kern.msgbufsize=$(sysctl -n kern.msgbufsize))$([ "$ARCH" != amd64 ] || echo ", page table isolation off (vm.pmap.pti=$(sysctl -n vm.pmap.pti))")" sh -c '[ "$(sysctl -n kern.msgbufsize)" = 131072 ] && { [ "$ARCH" != amd64 ] || [ "$(sysctl -n vm.pmap.pti)" = 0 ]; }'
ok "packages: $(pkg query '%n' | wc -l | tr -d ' ') ($(pkg query -e '%a = 0' '%n' | paste -sd' ' -)); $(/usr/local/bin/python3 --version 2>&1) for Ansible" sh -c '/usr/local/bin/python3 -c "import json" && pkg info -e sudo && pkg info -e bash'
ok "services: sshd, ntpd and pf enabled, sendmail not" sh -c 's="$(service -e)"; echo "$s" | grep -q "/sshd$" && echo "$s" | grep -q "/ntpd$" && echo "$s" | grep -q "/pf$" && ! echo "$s" | grep -q sendmail'
echo "disk: $(zfs list -H -o used,avail zroot | tr '\t' '/') used/free; memory free $(( $(sysctl -n vm.stats.vm.v_free_count) * $(sysctl -n hw.pagesize) / 1048576 )) MB"
CHECK
	# what pf lets in: a port of the allowed range answers, another does not
	on ump 'sudo -n pkill -f "nc -l" 2>/dev/null; (echo open | nc -l 8080 >/dev/null 2>&1 &) ; (echo closed | nc -l 9999 >/dev/null 2>&1 &) ; sleep 1' </dev/null
	python3 - "$OPEN" "$CLOSED" <<'PROBE' || fail "pf does not let in what it should, or lets in more"
import socket, sys
def answer(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=5) as c:
            c.settimeout(5)
            return c.recv(16).decode().strip()
    except OSError:
        return ""
allowed, other = answer(int(sys.argv[1])), answer(int(sys.argv[2]))
if allowed != "open" or other != "":
    print("FAILED: firewall: tcp/8080 answers %r, tcp/9999 answers %r" % (allowed, other))
    sys.exit(1)
print("firewall: tcp/8080 (in the allowed range) answers from outside, tcp/9999 does not")
PROBE
}

install test.example.net
echo "==> checking the new system"
check test.example.net
echo "==> powering off"
halt ump 'sudo -n shutdown -p now'
if [ "$vdev" != stripe ]; then
	echo "==> the new system without its first disk"
	boot serial-degraded.log without
	secs="$(wait_ssh ump)" || { echo "$secs"; exit 1; }
	echo "new system: SSH as ump after ${secs}s, from $((DISKS - 1)) of $DISKS disks"
	check test.example.net degraded
	halt ump 'sudo -n shutdown -p now'
fi
if [ "$AGAIN" = 1 ]; then
	echo "==> a second installation over the first"
	install again.example.net
	echo "==> checking the new system"
	check again.example.net
	echo "==> powering off"
	halt ump 'sudo -n shutdown -p now'
fi
echo "==> ok"
