#!/usr/bin/env bash
# UMP installer. Runs as root in the UMP rescue system, where no disk is in
# use, and puts an operating system on the machine's disks from a UMP base
# image: wipes the disks, partitions them, builds the volume group, unpacks
# the image, configures what depends on the machine (hostname, network,
# keys, boot loader) and leaves the machine ready to reboot into the new
# system. ump install runs it; it can be run by hand just the same.
#
# Usage: install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--arch amd64|arm64]
#            [--layout lvm-linear] [--disks /dev/sda,/dev/sdb] [--keys FILE]
#            [--ipv6] [--mitigations] [--timezone ZONE] [--force] [--dry-run]
#
#   --storage URL    base URL of the storage, which serves
#                    os/<os>/<arch>/manifest.json and rootfs.tar.zst (or UMP_STORAGE)
#   --hostname NAME  hostname of the new system
#   --os NAME        the image to install (default ubuntu-26.04)
#   --arch ARCH      default: the machine's
#   --layout NAME    disk layout; lvm-linear (the default and so far the only one):
#                    the first disk gets the boot partitions, every disk becomes a
#                    physical volume of one volume group, root is one linear logical
#                    volume over all of them, ext4
#   --disks LIST     the disks to use, comma-separated (default: every disk; USB
#                    devices are left out)
#   --keys FILE      authorized_keys for root (default: /root/.ssh/authorized_keys,
#                    which is where ump rescue put its key and yours)
#   --ipv6           keep IPv6 on; by default the kernel gets ipv6.disable=1
#   --mitigations    keep the CPU vulnerability mitigations; by default mitigations=off
#   --timezone ZONE  default UTC
#   --force          run outside the UMP rescue system too (it erases every disk
#                    of the machine it runs on)
#   --dry-run        print what would be done and stop before touching a disk
set -euo pipefail

storage="${UMP_STORAGE:-}"
hostname=""
os="ubuntu-26.04"
arch=""
layout="lvm-linear"
disks_arg=""
keys="/root/.ssh/authorized_keys"
ipv6=0
mitigations=0
timezone="UTC"
force=0
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	--storage) storage="$2"; shift 2 ;;
	--hostname) hostname="$2"; shift 2 ;;
	--os) os="$2"; shift 2 ;;
	--arch) arch="$2"; shift 2 ;;
	--layout) layout="$2"; shift 2 ;;
	--disks) disks_arg="$2"; shift 2 ;;
	--keys) keys="$2"; shift 2 ;;
	--ipv6) ipv6=1; shift ;;
	--mitigations) mitigations=1; shift ;;
	--timezone) timezone="$2"; shift 2 ;;
	--force) force=1; shift ;;
	--dry-run) dry=1; shift ;;
	-h|--help) sed -n '2,/^set -euo/{/^set -euo/!p;}' "$0"; exit 0 ;;
	*) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
	esac
done
storage="${storage%/}"
[ -n "$storage" ] || { echo "install.sh: --storage URL is required" >&2; exit 2; }
[ -n "$hostname" ] || { echo "install.sh: --hostname NAME is required" >&2; exit 2; }
[ "$layout" = lvm-linear ] || { echo "install.sh: unknown layout $layout (known: lvm-linear)" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 2; }
case "$(uname -m)" in
x86_64) machine_arch=amd64 ;;
aarch64) machine_arch=arm64 ;;
*) echo "install.sh: unsupported machine $(uname -m)" >&2; exit 2 ;;
esac
[ -n "$arch" ] || arch="$machine_arch"
[ "$arch" = "$machine_arch" ] || { echo "install.sh: this machine is $machine_arch; a $arch image cannot be configured here" >&2; exit 2; }
if [ ! -f /etc/ump-rescue-version ] && [ "$force" = 0 ]; then
	echo "install.sh: this is not the UMP rescue system. The installer erases every disk of the machine it runs on; run it from the rescue system (ump install), or pass --force if that is what you want." >&2
	exit 2
fi
for t in sgdisk wipefs partprobe udevadm pvcreate vgcreate lvcreate mkfs.ext4 mkfs.vfat blkid zstd tar curl chroot lsblk; do
	command -v "$t" >/dev/null || { echo "install.sh: $t is missing" >&2; exit 2; }
done

log() { printf '==> %s\n' "$*"; }
die() { echo "install.sh: $*" >&2; exit 1; }
TARGET=/mnt/target
VG=vg0
image_url="$storage/os/$os/$arch"

# --- the machine -------------------------------------------------------

efi=0; [ -d /sys/firmware/efi ] && efi=1

# the disks: whole block devices that are not removable media, not RAM or
# loop devices, and not on USB (an install medium or virtual media)
all_disks=()
# (raw lsblk output drops empty columns, so the one that can be empty is last)
while IFS= read -r line; do
	set -- $line
	path="$1"; type="$2"; ro="$3"; tran="${4:-}"
	[ "$type" = disk ] || continue
	[ "$ro" = 0 ] || continue
	case "$(basename "$path")" in loop*|ram*|zram*|sr*|fd*|nbd*|md*|dm-*) continue ;; esac
	[ "$tran" = usb ] && continue
	all_disks+=("$path")
done < <(lsblk -dnr -o PATH,TYPE,RO,TRAN | sort)
disks=()
if [ -n "$disks_arg" ]; then
	IFS=, read -ra wanted <<< "$disks_arg"
	for d in "${wanted[@]}"; do
		[ -b "$d" ] || die "$d is not a block device"
		[ "$(lsblk -dnr -o TYPE "$d")" = disk ] || die "$d is not a whole disk"
		disks+=("$d")
	done
else
	# NVMe first, then the rest by name: the first one carries the boot partitions
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) disks+=("$d") ;; esac; done
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) ;; *) disks+=("$d") ;; esac; done
fi
[ ${#disks[@]} -gt 0 ] || die "no disk found"
boot_disk="${disks[0]}"
describe_disk() {
	local size rota tran model
	size="$(lsblk -dnr -o SIZE "$1")"; rota="$(lsblk -dnr -o ROTA "$1")"
	tran="$(lsblk -dnr -o TRAN "$1")"; model="$(lsblk -dn -o MODEL "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
	printf '%s %s%s%s' "$size" "$([ "$rota" = 1 ] && echo HDD || echo SSD)" "${tran:+ $tran}" "${model:+ $model}"
}

# the network: what the rescue system was given on its kernel command line
# (ump rescue carries the running system's over), else what is configured
cmdline="$(cat /proc/cmdline)"
param() { for w in $cmdline; do case "$w" in "$1="*) printf '%s' "${w#*=}"; return 0 ;; esac; done; return 1; }
mac="$(param ump.mac || true)"
ip4="$(param ump.ip || true)"
ip6="$(param ump.ip6 || true)"
if [ -z "$mac" ]; then
	iface="$(ip -o route show default 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") print $(i + 1)}' | head -n 1)"
	[ -n "$iface" ] && mac="$(cat "/sys/class/net/$iface/address")"
	if [ -n "$iface" ] && [ -z "$ip4" ]; then
		a="$(ip -o -4 addr show dev "$iface" scope global 2>/dev/null | awk '{print $4}' | head -n 1)"
		g="$(ip -o -4 route show default dev "$iface" 2>/dev/null | awk '{print $3}' | head -n 1)"
		[ -n "$a" ] && ip4="$a${g:+,$g}"
	fi
	if [ -n "$iface" ] && [ -z "$ip6" ]; then
		a="$(ip -o -6 addr show dev "$iface" scope global 2>/dev/null | awk '$0 !~ /deprecated|tentative|temporary/ {print $4}' | head -n 1)"
		g="$(ip -o -6 route show default dev "$iface" 2>/dev/null | awk '{print $3}' | head -n 1)"
		[ -n "$a" ] && ip6="$a${g:+,$g}"
	fi
fi
[ -n "$mac" ] || die "no network interface with a default route; the new system would be unreachable"
dns="1.1.1.1 8.8.8.8"

[ -f "$keys" ] || die "$keys does not exist; the new system would have no way in"
nkeys="$(grep -c '^[a-z]' "$keys" || true)"
[ "$nkeys" -gt 0 ] || die "$keys holds no key"

kernel_cmdline=""
[ "$mitigations" = 1 ] || kernel_cmdline="mitigations=off"
[ "$ipv6" = 1 ] || kernel_cmdline="${kernel_cmdline:+$kernel_cmdline }ipv6.disable=1"

# --- the plan ----------------------------------------------------------

log "machine: $arch, $([ "$efi" = 1 ] && echo UEFI || echo BIOS) firmware, $(nproc) CPUs, $(awk '/^MemTotal/ {printf "%d", $2 / 1024}' /proc/meminfo) MB RAM"
log "image: $os for $arch from $image_url"
d=""; for x in "${disks[@]}"; do d="${d:+$d, }$x ($(describe_disk "$x"))"; done
log "disks (${#disks[@]}): $d"
log "layout $layout: $boot_disk gets 1 MiB BIOS boot, 512 MiB EFI (vfat), 1 GiB /boot (ext4) and the rest as LVM; every other disk is LVM whole; $VG/root is one linear volume over all of it, ext4"
log "network: $mac $ip4${ip6:+ $ip6}; IPv6 $([ "$ipv6" = 1 ] && echo on || echo off); DNS $dns; hostname $hostname"
log "root keys: $nkeys from $keys; kernel command line: ${kernel_cmdline:-(defaults)}; mitigations $([ "$mitigations" = 1 ] && echo on || echo off)"
if [ "$dry" = 1 ]; then
	log "dry run: stopping here, no disk was touched"
	exit 0
fi

# --- the image: the manifest first, so that a missing image costs no disk ---

log "fetching the manifest"
manifest="$(curl -fsSL "$image_url/manifest.json")" || die "cannot fetch $image_url/manifest.json; the storage must serve the image (see images/ubuntu/README.md)"
want_sum="$(printf '%s' "$manifest" | sed -n 's/.*"rootfs.tar.zst": {"sha256": "\([0-9a-f]*\)".*/\1/p' | head -n 1)"
image_version="$(printf '%s' "$manifest" | sed -n 's/.*"version": "\([^"]*\)".*/\1/p' | head -n 1)"
image_kernel="$(printf '%s' "$manifest" | sed -n 's/.*"kernel": "\([^"]*\)".*/\1/p' | head -n 1)"
unpacked_mb="$(printf '%s' "$manifest" | sed -n 's/.*"unpacked_mb": \([0-9]*\).*/\1/p' | head -n 1)"
[ -n "$want_sum" ] || die "the manifest names no rootfs.tar.zst checksum"
log "image $os version ${image_version:-?}, kernel ${image_kernel:-?}, ${unpacked_mb:-?} MB unpacked"

# --- the disks ---------------------------------------------------------

log "releasing the disks"
swapoff -a 2>/dev/null || true
# unmount in depth-first order whatever is mounted from the disks, LVM or md
while read -r src tgt; do
	case "$src" in /dev/*) ;; *) continue ;; esac
	for x in "${disks[@]}"; do case "$src" in "$x"*|/dev/mapper/*|/dev/md*|/dev/dm-*|/dev/$VG/*) umount -l "$tgt" 2>/dev/null || true; break ;; esac; done
done < <(awk '{print $1, $2}' /proc/mounts | sort -k2 -r)
vgchange -an >/dev/null 2>&1 || true
mdadm --stop --scan >/dev/null 2>&1 || true

log "wiping the disks"
for x in "${disks[@]}"; do
	# signatures inside the old partitions first (LVM, md, file systems),
	# so that nothing is found again at the same offsets after the new
	# partitioning; then the partition tables and signatures of the disk
	for p in $(lsblk -nr -o PATH "$x" | tail -n +2); do wipefs -af "$p" >/dev/null 2>&1 || true; done
	sgdisk --zap-all "$x" >/dev/null 2>&1 || true
	wipefs -af "$x" >/dev/null
	dd if=/dev/zero of="$x" bs=1M count=2 conv=notrunc status=none 2>/dev/null || true
done
partprobe >/dev/null 2>&1 || true
udevadm settle
pvscan --cache >/dev/null 2>&1 || true

log "partitioning"
sgdisk -n 1:0:+1M -t 1:ef02 -c 1:bios -n 2:0:+512M -t 2:ef00 -c 2:esp -n 3:0:+1G -t 3:8300 -c 3:boot -n 4:0:0 -t 4:8e00 -c 4:lvm "$boot_disk" >/dev/null
for x in "${disks[@]:1}"; do
	sgdisk -n 1:0:0 -t 1:8e00 -c 1:lvm "$x" >/dev/null
done
partprobe >/dev/null 2>&1 || true
udevadm settle
partdev() { # partdev DISK N
	local p
	p="$(lsblk -nr -o PATH,PARTN "$1" 2>/dev/null | awk -v n="$2" '$2 == n {print $1}' | head -n 1)"
	if [ -z "$p" ]; then case "$1" in *[0-9]) p="${1}p$2" ;; *) p="$1$2" ;; esac; fi
	[ -b "$p" ] || die "partition $2 of $1 did not appear"
	printf '%s' "$p"
}
esp="$(partdev "$boot_disk" 2)"
bootp="$(partdev "$boot_disk" 3)"
pvs=("$(partdev "$boot_disk" 4)")
for x in "${disks[@]:1}"; do pvs+=("$(partdev "$x" 1)"); done

log "file systems and the volume group ($VG over ${#pvs[@]} physical volume(s))"
mkfs.vfat -F 32 -n ESP "$esp" >/dev/null
mkfs.ext4 -q -F -L boot "$bootp"
for p in "${pvs[@]}"; do pvcreate -ff -y "$p" >/dev/null; done
vgcreate -y "$VG" "${pvs[@]}" >/dev/null
lvcreate -y -l 100%FREE -n root "$VG" >/dev/null
udevadm settle
mkfs.ext4 -q -F -L root "/dev/$VG/root"
root_mb="$(lsblk -dnr -o SIZE -b "/dev/$VG/root" | awk '{printf "%d", $1 / 1024 / 1024}')"
if [ -n "$unpacked_mb" ] && [ "$root_mb" -lt $((unpacked_mb * 2)) ]; then
	die "the root volume ($root_mb MB) is too small for the image ($unpacked_mb MB unpacked)"
fi

log "mounting the new system at $TARGET"
mkdir -p "$TARGET"
mount "/dev/$VG/root" "$TARGET"
mkdir -p "$TARGET/boot"
mount "$bootp" "$TARGET/boot"
mkdir -p "$TARGET/boot/efi"
mount "$esp" "$TARGET/boot/efi"
cleanup() {
	set +e
	for m in "$TARGET/sys/firmware/efi/efivars" "$TARGET/run" "$TARGET/sys" "$TARGET/proc" "$TARGET/dev" "$TARGET/boot/efi" "$TARGET/boot" "$TARGET"; do
		mountpoint -q "$m" && umount -l "$m" 2>/dev/null
	done
}
trap cleanup EXIT

# --- the image ---------------------------------------------------------

log "downloading and unpacking $image_url/rootfs.tar.zst"
sumfile="$(mktemp)"
rm -f "$sumfile"
curl -fsSL "$image_url/rootfs.tar.zst" \
	| tee >(sha256sum | cut -d' ' -f1 > "$sumfile") \
	| zstd -dc \
	| tar --xattrs --xattrs-include='*' --acls --numeric-owner -xpf - -C "$TARGET"
for _ in $(seq 1 100); do [ -s "$sumfile" ] && break; sleep 0.1; done
have_sum="$(cat "$sumfile" 2>/dev/null || true)"
rm -f "$sumfile"
[ "$have_sum" = "$want_sum" ] || die "checksum mismatch for rootfs.tar.zst: manifest $want_sum, download ${have_sum:-?}; nothing was configured"
sync

# --- the machine's own configuration ------------------------------------

log "configuring $hostname"
[ -f "$TARGET/usr/share/zoneinfo/$timezone" ] || die "unknown time zone $timezone"
echo "$hostname" > "$TARGET/etc/hostname"
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n127.0.1.1\t%s\n' "$hostname" > "$TARGET/etc/hosts"
ln -sf "/usr/share/zoneinfo/$timezone" "$TARGET/etc/localtime"
echo "$timezone" > "$TARGET/etc/timezone"

root_uuid="$(blkid -s UUID -o value "/dev/$VG/root")"
boot_uuid="$(blkid -s UUID -o value "$bootp")"
esp_uuid="$(blkid -s UUID -o value "$esp")"
cat > "$TARGET/etc/fstab" <<FSTAB
# /etc/fstab, written by UMP; root is $VG/root over ${disks[*]}
UUID=$root_uuid  /          ext4  defaults,errors=remount-ro  0 1
UUID=$boot_uuid  /boot      ext4  defaults                    0 2
UUID=$esp_uuid   /boot/efi  vfat  umask=0077                  0 1
FSTAB

mkdir -p "$TARGET/etc/netplan"
{
	echo "# written by UMP at installation; netplan try / netplan apply after editing"
	echo "network:"
	echo "  version: 2"
	echo "  renderer: networkd"
	echo "  ethernets:"
	echo "    uplink:"
	echo "      match:"
	echo "        macaddress: \"$mac\""
	if [ -z "$ip4$ip6" ]; then
		echo "      dhcp4: true"
	else
		echo "      dhcp4: false"
		echo "      addresses:"
		[ -n "$ip4" ] && echo "        - \"${ip4%%,*}\""
		[ "$ipv6" = 1 ] && [ -n "$ip6" ] && echo "        - \"${ip6%%,*}\""
		echo "      routes:"
		case "$ip4" in *,*) printf '        - to: default\n          via: "%s"\n          on-link: true\n' "${ip4#*,}" ;; esac
		if [ "$ipv6" = 1 ]; then case "$ip6" in *,*) printf '        - to: "::/0"\n          via: "%s"\n          on-link: true\n' "${ip6#*,}" ;; esac; fi
	fi
	if [ "$ipv6" = 1 ]; then echo "      accept-ra: false"; else echo "      link-local: []"; fi
	echo "      nameservers:"
	echo "        addresses: [$(echo "$dns" | sed 's/ /, /g')]"
} > "$TARGET/etc/netplan/10-ump.yaml"
chmod 600 "$TARGET/etc/netplan/10-ump.yaml"

mkdir -p -m 700 "$TARGET/root/.ssh"
grep '^[a-z]' "$keys" | awk '!seen[$0]++' > "$TARGET/root/.ssh/authorized_keys"
chmod 600 "$TARGET/root/.ssh/authorized_keys"

mkdir -p "$TARGET/etc/default/grub.d"
cat > "$TARGET/etc/default/grub.d/ump-cmdline.cfg" <<GRUB
# UMP: the kernel command line of this machine. By default the CPU
# vulnerability mitigations are off (mitigations=off) and IPv6 is off
# (ipv6.disable=1): both cost performance, and most servers need neither.
# To turn one back on, remove it from the line below and run update-grub;
# for IPv6 also add the address and route to /etc/netplan/10-ump.yaml
# (netplan apply). Then reboot.
GRUB_CMDLINE_LINUX_DEFAULT="$kernel_cmdline"
GRUB

: > "$TARGET/etc/machine-id"
rm -f "$TARGET/var/lib/dbus/machine-id"

# --- the boot loader ---------------------------------------------------

log "installing the boot loader on $boot_disk"
mount --bind /dev "$TARGET/dev"
mount -t proc proc "$TARGET/proc"
mount -t sysfs sysfs "$TARGET/sys"
mount --bind /run "$TARGET/run"
in_target() { chroot "$TARGET" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"; }
in_target ssh-keygen -A >/dev/null
# The EFI loader goes to the removable path (EFI/BOOT/BOOT*.EFI), which
# every firmware tries without a boot entry, and no boot entry is written:
# the firmware's boot order stays as the provider set it, with its network
# boot (which is how rescue systems get in) ahead of the disk. On amd64 the
# BIOS loader goes into the BIOS boot partition as well, so the disk boots
# either way.
run_in_target() { # run_in_target WHAT COMMAND...: the output is shown only when it fails
	local what="$1" out; shift
	if ! out="$(in_target "$@" 2>&1)"; then printf '%s\n' "$out" >&2; die "$what failed"; fi
}
case "$arch" in
amd64)
	run_in_target "grub-install (BIOS)" grub-install --target=i386-pc --recheck "$boot_disk"
	run_in_target "grub-install (EFI)" grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
arm64)
	run_in_target "grub-install (EFI)" grub-install --target=arm64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
esac
run_in_target "update-grub" update-grub
grep -q 'vmlinuz' "$TARGET/boot/grub/grub.cfg" || die "grub.cfg names no kernel"

mkdir -p "$TARGET/etc/ump"
cat > "$TARGET/etc/ump/install" <<META
os=$os
image_version=$image_version
image_kernel=$image_kernel
installed=$(date -u +%Y-%m-%dT%H:%M:%SZ)
layout=$layout
disks=${disks[*]}
boot_disk=$boot_disk
vg=$VG
hostname=$hostname
ipv6=$([ "$ipv6" = 1 ] && echo on || echo off)
mitigations=$([ "$mitigations" = 1 ] && echo on || echo off)
META

sync
cleanup
trap - EXIT
vgchange -an "$VG" >/dev/null 2>&1 || true
log "installed: $os ${image_version:-?} (kernel ${image_kernel:-?}) as $hostname on $VG/root ($root_mb MB over ${#disks[@]} disk(s)); reboot to start it"
