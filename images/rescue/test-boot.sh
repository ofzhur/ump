#!/usr/bin/env bash
# Boot a built rescue image in QEMU and check it over SSH: the setup from
# the kernel command line, sshd with the injected key, the tools the UMP
# agent needs. Needs qemu-system-<arch> and ssh-keygen on the host.
#
# Usage: test-boot.sh [amd64|arm64]   (uses out/<arch>, see build.sh)
# Environment: MEM (MB, default 2048), TIMEOUT, PORT, KEXEC_TEST=0 to skip
# the kexec test, KEXEC_DISK=1 to keep its download on the virtual disk.
set -euo pipefail
ARCH="${1:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out/$ARCH}"
PORT="${PORT:-2222}"
MEM="${MEM:-2048}"
TIMEOUT="${TIMEOUT:-300}"
WORK="$(mktemp -d /tmp/ump-rescue-test.XXXXXX)"
qemu_pid=""
cleanup() {
	[ -n "$qemu_pid" ] && kill "$qemu_pid" 2>/dev/null || true
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
echo "==> ok"
