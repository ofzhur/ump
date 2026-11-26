#!/usr/bin/env bash
# Boot a built FreeBSD rescue image in QEMU and check it over SSH. The
# machine starts from a disk staged as README.md says (Starting it): the
# image at the start of the disk, its backup partition table moved to
# the end (sgdisk -e -G), and a boot config on its FAT partition. Four
# boots per firmware, each with another boot config:
#
#   static    hostname, key, a static IPv4 address with /32 and an
#             on-link gateway, a static IPv6 address, resolvers and the
#             clock (the machine's is in 2020), a plan.json for an
#             installer; then the system itself: the root in memory and
#             no disk mounted, sshd's settings, every tool an installer
#             calls, pf, the zones and the CA certificates, a GPT, a
#             FAT32 file system and a ZFS pool made on a second disk, the
#             drivers of server NICs and storage controllers, no program
#             without its libraries; after the shutdown, boot.log on the
#             partition
#   dhcp      two NICs, DHCP on the one of ump.boot-mac alone with the
#             lease's resolver, the key from ump.authorized_keys, a
#             value that is no address ignored, and under UEFI the
#             hostname from the kernel environment
#   memory    nothing but the key: DHCP on every interface, the public
#             resolvers whatever the lease says, with as much memory as
#             the manifest asks for
#   silent    a NIC that gets no lease: nobody can log in, and boot.log
#             on the partition says how far the boot came
#
# Needs qemu-system-<arch> and ssh-keygen on the host, and for the disk
# zstd, sgdisk and mtools, on the host or in a Debian container (Docker
# or Podman) that this script starts when the host lacks one of them.
#
# Usage: test-boot.sh [amd64|arm64] [RELEASE]   (uses out/rescue/freebsd-<release>/<arch>, see build.sh; default 15.1)
# Environment: FIRMWARE (uefi or bios; default: uefi, then bios on
# amd64), MEM (MB, default 1024), TIMEOUT (s, default 600), PORT
# (default 2272), CONTAINER (docker or podman), CONTAINER_NAME, KEEP=1
# to leave the work directory with the serial logs.
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
RELEASE="${2:-15.1}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/rescue/freebsd-$RELEASE/$ARCH}"
PORT="${PORT:-2272}"
MEM="${MEM:-1024}"
TIMEOUT="${TIMEOUT:-600}"
MAC=52:54:00:12:34:56 OTHER_MAC=52:54:00:12:34:57
WORK="$(mktemp -d /tmp/ump-freebsd-test.XXXXXX)"
qemu_pid="" container="" name="${CONTAINER_NAME:-ump-freebsd-test-$$}"
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
	[ -n "$container" ] && "$container" rm -f "$name" >/dev/null 2>&1 || true
	if [ "${KEEP:-0}" = 1 ]; then rm -f "$WORK"/*.img "$WORK"/*.fd; echo "work directory (serial logs): $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT
fail() { echo "FAILED: $*"; exit 1; }

[ -f "$OUT/disk.img.zst" ] && [ -f "$OUT/manifest.json" ] || { echo "no image in $OUT; run build.sh $ARCH $RELEASE first" >&2; exit 2; }
manifest() { sed -n "s/.*\"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}\$/\1/p" "$OUT/manifest.json" | head -n 1; }
version="$(manifest version)"
memory_mb="$(manifest memory_mb)"
[ -n "$version" ] && [ -n "$memory_mb" ] || { echo "no version or memory_mb in $OUT/manifest.json" >&2; exit 2; }
host_os="$(uname -s)"

# the drivers every image must have, and those of amd64 alone (FreeBSD
# builds them for no other architecture)
drivers="if_em if_igb if_igc if_ix if_ice if_bnxt if_bge if_bce if_re if_cxgbe if_oce mlx4en mlx5en if_ena if_gve if_vmx if_vtnet hv_netvsc \
nvme ahci mps mpr mpi3mr mrsas mfi mpt isp pvscsi virtio_blk virtio_scsi hv_storvsc"
case "$ARCH" in
amd64)
	drivers="$drivers if_ixl if_bxe if_qlnxe if_axp if_enic sfxge aacraid smartpqi ciss isci arcmsr hptiop ips"
	qemu=qemu-system-x86_64; code=edk2-x86_64-code.fd; vars=edk2-i386-vars.fd
	machine=(-M q35)
	if [ "$host_os" = Linux ] && [ "$(uname -m)" = x86_64 ] && [ -w /dev/kvm ]; then machine+=(-accel kvm -cpu host)
	# emulated: with QEMU's models of newer CPUs (Haswell, max) the kernel
	# faults while zfs.ko loads; with qemu64 it loads
	else machine+=(-cpu qemu64); fi
	firmwares="${FIRMWARE:-uefi bios}"
	;;
arm64)
	qemu=qemu-system-aarch64; code=edk2-aarch64-code.fd; vars=edk2-arm-vars.fd
	machine=(-M virt)
	if [ "$host_os" = Darwin ] && [ "$(uname -m)" = arm64 ]; then machine+=(-accel hvf -cpu host)
	elif [ "$host_os" = Linux ] && [ "$(uname -m)" = aarch64 ] && [ -w /dev/kvm ]; then machine+=(-accel kvm -cpu host)
	else machine+=(-cpu cortex-a72); fi
	firmwares="${FIRMWARE:-uefi}"
	[ "$firmwares" = uefi ] || { echo "arm64 starts with UEFI alone" >&2; exit 2; }
	;;
*) echo "unsupported architecture $ARCH" >&2; exit 2 ;;
esac
command -v "$qemu" >/dev/null || { echo "$qemu not found" >&2; exit 2; }
share="$(dirname "$(dirname "$(command -v "$qemu")")")/share/qemu"
if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then echo "port $PORT is in use; set PORT" >&2; exit 2; fi

# The disk is written with the tools the UMP Linux rescue system has for
# it, its boot config with mtools, since nothing is mounted here.
# tools COMMAND runs a command in the work directory, where the image is
# image.zst, on the host or in the container.
ln "$OUT/disk.img.zst" "$WORK/image.zst" 2>/dev/null || cp "$OUT/disk.img.zst" "$WORK/image.zst"
if command -v zstd >/dev/null && command -v sgdisk >/dev/null && command -v mcopy >/dev/null; then
	tools() { (cd "$WORK" && bash -c "$1"); }
else
	container="${CONTAINER:-$(command -v docker || command -v podman || true)}"
	[ -n "$container" ] || { echo "zstd, sgdisk and mtools are needed for the disk: install them, or docker or podman" >&2; exit 2; }
	echo "==> a Debian container ($name) for zstd, sgdisk and mtools"
	"$container" run -d --name "$name" -v "$WORK:/work" -w /work debian:trixie sleep infinity >/dev/null
	"$container" exec -e DEBIAN_FRONTEND=noninteractive "$name" sh -c 'apt-get update -q && apt-get install -y -q --no-install-recommends zstd gdisk mtools' >"$WORK/apt.log" 2>&1 \
		|| { tail -n 20 "$WORK/apt.log"; exit 2; }
	tools() { "$container" exec "$name" bash -c "$1"; }
fi

ssh-keygen -q -t ed25519 -N '' -C ump-freebsd-test -f "$WORK/key"
ssh_opts=(-i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
	-o ConnectTimeout=5 -o ServerAliveInterval=10 -p "$PORT" root@127.0.0.1)

# stage FILE...: the disk with the image, and a boot config of these
# files of $WORK/config (cmdline, authorized_keys, plan.json, ...); a
# loader.env among them goes where loader.efi reads it
fat='disk.img@@$(( $(sgdisk -i 1 disk.img 2>/dev/null | awk "/^First sector:/ {print \$3}") * 512 ))'
stage() {
	local f files=""
	rm -f "$WORK/disk.img"
	truncate -s 4G "$WORK/disk.img"
	tools 'set -o pipefail; zstd -dcq image.zst | dd of=disk.img bs=1048576 conv=notrunc 2>/dev/null && sgdisk -e -G disk.img >/dev/null 2>&1' || fail "the image could not be written to the disk"
	for f in "$@"; do [ "$f" = loader.env ] || files="$files config/$f"; done
	[ -z "$files" ] || tools "mcopy -o -i $fat $files ::/ump/" || fail "the boot config could not be written"
	case " $* " in
	*" loader.env "*) tools "mmd -i $fat ::/EFI/freebsd && mcopy -o -i $fat config/loader.env ::/EFI/freebsd/loader.env" || fail "loader.env could not be written" ;;
	esac
}
# boot_log: boot.log of the disk's partition, into $WORK/boot.log. The
# partition is read from a copy made here: a container may still see the
# disk's file as it was before the machine wrote to it.
boot_log() {
	local first sectors
	read -r first sectors <<< "$(tools 'sgdisk -i 1 disk.img 2>/dev/null' | awk '/^First sector:/ {f = $3} /^Partition size:/ {n = $3} END {print f, n}')"
	rm -f "$WORK/boot.log" "$WORK/fat.img"
	dd if="$WORK/disk.img" of="$WORK/fat.img" bs=512 skip="$first" count="$sectors" 2>/dev/null
	tools 'mcopy -n -i fat.img ::/ump/boot.log boot.log' 2>/dev/null || fail "no ump/boot.log on the disk's partition"
	cp "$WORK/boot.log" "${serial%.log}-boot.log"
}

# boot NAME FIRMWARE MEMORY [QEMU ARGUMENT]...: start the machine from the
# disk; its serial console goes to $serial
boot() {
	local fw="$2" mem="$3" firmware=()
	serial="$WORK/serial-$fw-$1.log"
	shift 3
	if [ "$fw" = uefi ]; then
		# variables of its own for every boot: nothing remembers a boot entry
		cp "$share/$code" "$WORK/code.fd"
		cp "$share/$vars" "$WORK/vars.fd"
		[ "$ARCH" != arm64 ] || truncate -s 64M "$WORK/code.fd" "$WORK/vars.fd"
		firmware=(-drive "if=pflash,format=raw,readonly=on,file=$WORK/code.fd" -drive "if=pflash,format=raw,file=$WORK/vars.fd")
	fi
	"$qemu" "${machine[@]}" ${firmware[@]+"${firmware[@]}"} -m "$mem" -smp 2 -nographic \
		-drive "file=$WORK/disk.img,format=raw,if=virtio" "$@" \
		-serial "file:$serial" -monitor none -display none </dev/null >/dev/null 2>"$WORK/qemu.err" &
	qemu_pid=$!
	started=$(date +%s)
}
# wait_for WHAT COMMAND...: until the command succeeds; the seconds since the start in $took
wait_for() {
	local what="$1" deadline=$(( started + TIMEOUT ))
	shift
	until "$@" 2>/dev/null; do
		kill -0 "$qemu_pid" 2>/dev/null || { echo "qemu exited:"; cat "$WORK/qemu.err"; tail -n 30 "$serial"; exit 1; }
		[ "$(date +%s)" -le "$deadline" ] || { echo "no $what within ${TIMEOUT}s; last serial output:"; tail -n 40 "$serial"; exit 1; }
		sleep 2
	done
	took=$(( $(date +%s) - started ))
}
# stop [kill]: shut the machine down over SSH; with kill, stop it as it is
stop() {
	[ "${1:-}" = kill ] || ssh "${ssh_opts[@]}" 'shutdown -p now' >/dev/null 2>&1 || true
	[ "${1:-}" = kill ] || for _ in $(seq 1 30); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
	kill "$qemu_pid" 2>/dev/null || true
	wait "$qemu_pid" 2>/dev/null || true
	qemu_pid=""
}
net=(-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device "virtio-net-pci,netdev=n0,mac=$MAC")
# guest "NAME=VALUE ..." CHECKS: run checks on the machine, as root over SSH
guest() { ssh "${ssh_opts[@]}" "$1 sh -s" <<< "$prelude$2"; }
prelude='ok() { what="$1"; shift; if "$@" >/dev/null 2>&1; then echo "$what"; else echo "FAILED: $what"; exit 1; fi; }
nic() { for i in $(ifconfig -l ether); do ifconfig "$i" ether | grep -q "ether $1" && echo "$i"; done; }
addr() { ifconfig -f inet:cidr "$1" inet | awk "\$1 == \"inet\" {print \$2}"; }
resolvers() { awk "/^nameserver/ {print \$2}" /etc/resolv.conf | paste -sd, -; }
memory() { echo "physical $(( $(sysctl -n hw.physmem) / 1048576 )) MB, free $(( $(sysctl -n vm.stats.vm.v_free_count) * $(sysctl -n hw.pagesize) / 1048576 )) MB"; }
'

# The checks of each boot, run on the machine.
IFS= read -r -d '' check_static <<'EOF' || true
ok "system: ump-rescue $(cat /etc/ump-rescue-version), $(uname -srm)" [ "$(cat /etc/ump-rescue-version) $(uname -sr)" = "$VERSION FreeBSD $RELEASE-RELEASE" ]
ok "hostname: $(hostname), from the boot config" [ "$(hostname)" = ump-freebsd-test ]
ok "boot config: /etc/ump/boot (0700) with authorized_keys, cmdline and plan.json (0600), without boot.log" \
	[ "$(stat -f %Lp /etc/ump/boot) $(cd /etc/ump/boot && stat -f %N:%Lp * | paste -sd' ' -)" = "700 authorized_keys:600 cmdline:600 plan.json:600" ]
ok "root: $(mount -p | awk '$2 == "/" {print $1 " (" $3 ")"}'), in memory; memory: $(memory)" \
	[ "$(mount -p | awk '$2 == "/" {print $1}')" = /dev/md0 ]
ok "mounted: $(mount -p | awk '{print $1}' | sort | paste -sd' ' -); no disk" [ "$(mount -p | awk '{print $1}' | sort | paste -sd' ' -)" = "/dev/md0 devfs tmpfs" ]
n="$(nic "$MAC")"
ok "interface $n ($MAC): $(addr "$n"), static, no DHCP client" [ "$(addr "$n")$(pgrep dhclient)" = 10.0.2.15/32 ]
ok "default route: $(route -n get default | awk '/gateway:/ {print $2}') on $n, on-link; the gateway answers" \
	sh -c "[ \"\$(route -n get default | awk '/gateway:|interface:/ {print \$2}' | paste -sd' ' -)\" = '10.0.2.2 $n' ] && ping -c 1 -t 5 10.0.2.2"
ok "IPv6: fec0::15/64 on $n, default route $(route -6 -n get default | awk '/gateway:/ {print $2}'); the host answers" \
	sh -c "ifconfig -f inet6:cidr $n inet6 | grep -q 'inet6 fec0::15/64' && [ \"\$(route -6 -n get default | awk '/gateway:/ {print \$2}')\" = 'fe80::2%$n' ] && ping -6 -c 1 -t 5 fec0::2"
ok "resolvers: $(resolvers), from ump.dns" [ "$(resolvers)" = 10.0.2.3 ]
off=$(( $(date +%s) - NOW ))
ok "clock: $(date -u +%Y-%m-%dT%H:%M:%SZ), set from ump.time ($(grep 'the clock' /var/log/ump-rescue-setup.log))" \
	sh -c "[ ${off#-} -lt 600 ] && grep -q 'set from ump.time' /var/log/ump-rescue-setup.log"
ok "sshd: root with keys only, no name looked up at a login" [ "$(sshd -T | grep -E '^(permitrootlogin|passwordauthentication|kbdinteractiveauthentication|usedns) ' | sort | paste -sd' ' -)" \
	= "kbdinteractiveauthentication no passwordauthentication no permitrootlogin without-password usedns no" ]
missing=""
for t in gpart zpool zfs zdb newfs_msdos fetch tar pw pkg-static jq pfctl chroot sysrc service mount umount swapon swapoff dd sha256 diskinfo camcontrol nvmecontrol \
	geom glabel mdconfig kldload kldstat kenv sysctl ifconfig route dhclient mktemp awk sed grep install cp ln ssh-keygen sshd bectl freebsd-version tzsetup \
	date hostname pciconf dmesg tcpdump ping drill openssl b64decode xz gzip ldd top vi; do
	command -v "$t" >/dev/null || missing="$missing $t"
done
ok "tools: all present$missing" [ -z "$missing" ]
ok "jq $(jq --version | sed 's/^jq-//') and pkg-static $(pkg-static -v) run; tar knows its locale" \
	sh -c 'echo "{\"a\": [1, 2]}" | jq -e ".a[1] == 2" && pkg-static -v && [ -z "$(tar -cf /dev/null /etc/fstab 2>&1 | grep -v "Removing leading")" ]'
ok "zfs: loaded at boot (zpool $(zpool version | sed -n '1s/^zfs-//p'))" kldstat -q -m zfs
# what an installer needs beside the tools: pf to check a ruleset with,
# FAT for the EFI system partitions, the zones, and the certificates
# fetch trusts, as OpenSSL finds them by itself
ok "pf: pf.ko loads, pfctl takes a ruleset; msdosfs.ko is there" \
	sh -c 'kldload -n pf && echo "block in all" | pfctl -nf - && [ -f /boot/kernel/pf.ko ] && [ -f /boot/kernel/msdosfs.ko ] && kldstat -q -m msdosfs'
ok "time zones: $(find /usr/share/zoneinfo -type f | wc -l | tr -d ' ') files; CA certificates: $(ls /usr/share/certs/trusted | wc -l | tr -d ' '), which openssl finds" \
	sh -c '[ -f /usr/share/zoneinfo/Europe/Berlin ] && openssl verify "$(ls /usr/share/certs/trusted/*.pem | head -n 1)"'
# what an installer does to a disk, on the second one
for d in vtbd0 vtbd1; do gpart show -l "$d" 2>/dev/null | grep -q umpboot || disk="$d"; done
ok "disk $disk: a GPT, a FAT32 file system, a pool with a file system, exported again" \
	sh -c "gpart create -s gpt $disk && gpart add -t efi -s 64m -l umptestefi $disk && gpart add -t freebsd-zfs -l umptestzfs $disk \
	&& newfs_msdos -F 32 -c 1 /dev/gpt/umptestefi && zpool create -f -o altroot=/mnt -O compression=lz4 -O mountpoint=none umptest /dev/gpt/umptestzfs \
	&& zfs create -o mountpoint=/ umptest/root && cp /etc/fstab /mnt/ && zpool export umptest && [ ! -e /mnt/fstab ]"
# the drivers of server NICs and storage controllers: each is in the
# kernel or a module that loads (QEMU only has virtio, and a driver loads
# without its hardware), which is what catches a module left out, or left
# without one it depends on
broken=""
for m in $DRIVERS; do
	kldload -n "$m" 2>/dev/null || kldstat -v | awk '{print $2}' | grep -q "/${m#if_}\$" || broken="$broken $m"
done
ok "drivers: $(echo $DRIVERS | wc -w | tr -d ' ') of server NICs and storage controllers, in the kernel or loaded$broken" [ -z "$broken" ]
unlinked="$(for f in /bin/* /sbin/* /usr/bin/* /usr/sbin/* /usr/libexec/* /usr/local/bin/* /usr/local/sbin/* /lib/*.so.* /usr/lib/*.so.* /usr/local/lib/*.so.*; do
	[ -f "$f" ] && [ ! -L "$f" ] && ldd "$f" 2>/dev/null | grep -q 'not found' && printf ' %s' "$f"; done)"
ok "libraries: no program or library lacks one$unlinked" [ -z "$unlinked" ]
ok "services: sshd runs, nothing mails or schedules" sh -c 'service sshd status && ! pgrep -q sendmail && ! pgrep -q cron'
echo "rc.conf.local: $(paste -sd' ' /etc/rc.conf.local)"
EOF
IFS= read -r -d '' check_dhcp <<'EOF' || true
n="$(nic "$MAC")" o="$(nic "$OTHER_MAC")"
ok "a value with characters that do not belong in it: $(grep 'is not what' /var/log/ump-rescue-setup.log)" \
	sh -c "grep -q '^ump.ip=10.0.2.99/24,.* is not what it should be; ignored' /var/log/ump-rescue-setup.log && ! grep -q -e 10.0.2.99 -e '(id)' /etc/rc.conf.local"
ok "boot NIC $n ($MAC): $(addr "$n") by DHCP" sh -c "[ '$(addr "$n")' = 10.0.2.15/24 ] && pgrep -q -f 'dhclient: $n'"
ok "other NIC $o ($OTHER_MAC): up, no address, no DHCP client" sh -c "[ -z '$(addr "$o")' ] && ifconfig $o | grep -q 'flags=.*<UP' && ! pgrep -q -f 'dhclient: $o'"
ok "resolvers: $(resolvers), the lease's$([ "$WANT_HOST" = ump-rescue ] || echo ": ump.dns=dhcp of cmdline wins over the kernel environment's")" [ "$(resolvers)" = 10.0.2.3 ]
ok "hostname: $(hostname)$([ "$WANT_HOST" = ump-rescue ] || echo ", from the kernel environment (loader.env)")" [ "$(hostname)" = "$WANT_HOST" ]
EOF
IFS= read -r -d '' check_memory <<'EOF' || true
n="$(nic "$MAC")"
ok "interface $n: $(addr "$n") by DHCP (every Ethernet interface)" sh -c "[ '$(addr "$n")' = 10.0.2.15/24 ] && grep -q '^ifconfig_DEFAULT=\"SYNCDHCP\"' /etc/rc.conf.local"
ok "resolvers: $(resolvers), not the lease's" [ "$(resolvers)" = 1.1.1.1,8.8.8.8 ]
ok "hostname: $(hostname)" [ "$(hostname)" = ump-rescue ]
ok "memory: $(memory) with zfs loaded; / has $(df -m / | awk 'NR == 2 {print $4}') MB free" \
	sh -c 'kldstat -q -m zfs && [ "$(mount -p | awk "\$2 == \"/\" {print \$1}")" = /dev/md0 ]'
EOF

mkdir "$WORK/config"
cp "$WORK/key.pub" "$WORK/config/authorized_keys"
echo "a boot log of an earlier boot" > "$WORK/config/boot.log"
echo '{"version": 1}' > "$WORK/config/plan.json"

for fw in $firmwares; do
case "$fw" in uefi|bios) ;; *) echo "FIRMWARE is uefi or bios" >&2; exit 2 ;; esac

echo "==> static: $OUT in $qemu ($fw, ${MEM}M, ssh on 127.0.0.1:$PORT), the clock in 2020"
echo "ump.hostname=ump-freebsd-test ump.mac=$MAC ump.ip=10.0.2.15/32,10.0.2.2 ump.ip6=fec0::15/64,fe80::2 ump.dns=10.0.2.3 ump.time=$(date +%s)" > "$WORK/config/cmdline"
stage cmdline authorized_keys plan.json boot.log
rm -f "$WORK/disk2.img"
truncate -s 1G "$WORK/disk2.img"
boot static "$fw" "$MEM" "${net[@]}" -drive "file=$WORK/disk2.img,format=raw,if=virtio" -rtc base=2020-01-01T00:00:00
wait_for SSH ssh "${ssh_opts[@]}" true
echo "SSH as root with the boot config's key ${took}s after power-on"
guest "VERSION=$version RELEASE=$RELEASE MAC=$MAC NOW=$(date +%s) DRIVERS='$drivers'" "$check_static"
stop
boot_log
grep -q "^UMP FreeBSD rescue system $version, FreeBSD $RELEASE-RELEASE .*ump-freebsd-test" "$WORK/boot.log" && grep -q "ether $MAC" "$WORK/boot.log" \
	&& grep -q '^== kernel messages' "$WORK/boot.log" && grep -q 'inet 10.0.2.15' "$WORK/boot.log" || fail "boot.log is not this boot's"
echo "boot.log: on the partition, $(wc -c < "$WORK/boot.log" | tr -d ' ') bytes, this boot's ($(grep -c . "$WORK/boot.log") lines: setup, interfaces, routes, resolvers, devices, disks, kernel messages)"

echo "==> dhcp: two NICs, ump.boot-mac names the second$([ "$fw" != uefi ] || echo "; the hostname in loader.env")"
echo "ump.boot-mac=$MAC ump.dns=dhcp ump.ip=10.0.2.99/24,\$(id) ump.authorized_keys=$(base64 < "$WORK/key.pub" | tr -d '\n')" > "$WORK/config/cmdline"
echo "ump.hostname=ump-freebsd-kenv ump.dns=192.0.2.53" > "$WORK/config/loader.env"
if [ "$fw" = uefi ]; then stage cmdline loader.env; else stage cmdline; fi
boot dhcp "$fw" "$MEM" -netdev user,id=n1 -device "virtio-net-pci,netdev=n1,mac=$OTHER_MAC" "${net[@]}"
wait_for SSH ssh "${ssh_opts[@]}" true
echo "SSH as root with the key of ump.authorized_keys ${took}s after power-on"
want_host=ump-rescue; [ "$fw" != uefi ] || want_host=ump-freebsd-kenv
guest "MAC=$MAC OTHER_MAC=$OTHER_MAC WANT_HOST=$want_host" "$check_dhcp"
stop

echo "==> memory: ${memory_mb}M, as the manifest asks for; a boot config with the key alone"
stage authorized_keys
boot memory "$fw" "$memory_mb" "${net[@]}"
wait_for SSH ssh "${ssh_opts[@]}" true
echo "SSH as root ${took}s after power-on"
guest "MAC=$MAC" "$check_memory"
stop

echo "==> silent: a NIC without a DHCP server, nobody logs in"
echo "ump.hostname=ump-freebsd-silent ump.boot-mac=$MAC" > "$WORK/config/cmdline"
stage cmdline authorized_keys
boot silent "$fw" "$MEM" -netdev hubport,id=n0,hubid=0 -device "virtio-net-pci,netdev=n0,mac=$MAC"
wait_for "login prompt on the serial console" grep -q 'login:' "$serial"
stop kill
boot_log
grep -q "^UMP FreeBSD rescue system $version, .*ump-freebsd-silent" "$WORK/boot.log" && grep -q "ether $MAC" "$WORK/boot.log" || fail "boot.log is not this boot's"
! grep -q 'inet 10\.' "$WORK/boot.log" || fail "boot.log shows an address where there was no lease"
echo "boot.log: written ${took}s after power-on at the latest, without a lease: $(grep -A 3 '^== setup' "$WORK/boot.log" | sed -n '2p'); $(awk '/^== interfaces/ {on = 1} on && /status:/ {print "link " $2; exit}' "$WORK/boot.log")"
done
echo "==> ok"
