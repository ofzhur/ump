#!/usr/bin/env bash
# UMP installer. Runs as root in the UMP rescue system, where no disk is in
# use, and puts an operating system on the machine's disks the way a
# specification says (ump install renders it from a profile): wipes the
# disks, partitions them, builds md arrays and volume groups, then puts the
# system there the way its family wants it, and leaves the machine ready to
# reboot into it. See README.md.
#
#   image (Ubuntu)  unpacks the image and configures what depends on the
#                   machine (hostname, network, users and keys, userdata,
#                   firewall, boot loader)
#   nixos           writes the profile, what this script found on the
#                   machine and the firewall ruleset next to UMP's NixOS
#                   module (install/nixos/ump.nix on the storage), with the
#                   profile's userdata as NixOS modules, and runs
#                   nixos-install from the image's channel
#
# Usage: install.sh --spec FILE [--dry-run] [--force]
#        install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--disks /dev/sda,/dev/sdb]
#                   [--keys FILE] [--ipv6] [--mitigations] [--timezone ZONE] [--dry-run] [--force]
#
#   --spec FILE      the specification (JSON): os, hostname, timezone, storage,
#                    kernel, layout, disks, users, userdata, firewall, network
#                    (rename: false keeps the kernel's interface names;
#                    private: the server's address in its private network,
#                    recorded in /etc/ump/install, configured nowhere yet)
#   without --spec   the lvm-linear layout with root alone; the flags are:
#   --storage URL    base URL of the storage, which serves os/<os>/<arch>/
#                    manifest.json and rootfs.tar.zst (or UMP_STORAGE)
#   --hostname NAME  hostname of the new system
#   --os NAME        the image to install (default ubuntu-26.04)
#   --disks LIST     the disks to use, comma-separated (default: every disk; USB
#                    devices are left out)
#   --keys FILE      authorized_keys for root (default: /root/.ssh/authorized_keys)
#   --ipv6           keep IPv6 on; by default the kernel gets ipv6.disable=1
#   --mitigations    keep the CPU vulnerability mitigations; by default mitigations=off
#   --timezone ZONE  default UTC
#   --force          run outside the UMP rescue system too (it erases every disk
#                    of the machine it runs on)
#   --dry-run        print the plan and stop before touching a disk
set -Eeuo pipefail

spec_file=""
storage="${UMP_STORAGE:-}"
hostname=""
os="ubuntu-26.04"
disks_arg=""
keys="/root/.ssh/authorized_keys"
ipv6=0
mitigations=0
timezone="UTC"
force=0
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	--spec) spec_file="$2"; shift 2 ;;
	--storage) storage="$2"; shift 2 ;;
	--hostname) hostname="$2"; shift 2 ;;
	--os) os="$2"; shift 2 ;;
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
[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 2; }
case "$(uname -m)" in
x86_64) arch=amd64 ;;
aarch64) arch=arm64 ;;
*) echo "install.sh: unsupported machine $(uname -m)" >&2; exit 2 ;;
esac
if [ ! -f /etc/ump-rescue-version ] && [ "$force" = 0 ]; then
	echo "install.sh: this is not the UMP rescue system. The installer erases every disk of the machine it runs on; run it from the rescue system (ump install), or pass --force if that is what you want." >&2
	exit 2
fi
# Every command this script relies on, checked up front: the rescue system
# is minimal, and a missing tool must fail here, not halfway through the
# disks. mdadm is needed for md layouts and to stop old arrays.
for t in jq lsblk sgdisk wipefs partprobe udevadm blockdev pvcreate vgcreate lvcreate pvscan vgchange vgs swapoff mount umount mountpoint \
	mkfs.ext4 mkfs.vfat mkswap blkid zstd tar curl chroot dd sha256sum mktemp nproc awk sed grep sort head tail cut tr seq date \
	ln chmod chown mkdir rm cat basename ip sync sleep touch wc uname id tee; do
	command -v "$t" >/dev/null || { echo "install.sh: $t is missing in this rescue system; the UMP rescue image has it (images/rescue/packages.txt)" >&2; exit 2; }
done
command -v mdadm >/dev/null || echo "install.sh: warning: mdadm is missing; old md arrays cannot be stopped and md parts cannot be made" >&2

log() { printf '==> %s\n' "$*"; }
die() { dying=1; echo "install.sh: $*" >&2; exit 1; }
# a command that fails under set -e names its line, so that a failure is
# never silent (die says why on its own); set -E carries this into
# functions, and only the main shell speaks, not a command substitution
# whose status the caller looks at
trap 'rc=$?; [ "$rc" = 0 ] || [ -n "${dying:-}" ] || [ "$BASHPID" != "$$" ] || echo "install.sh: failed at line $LINENO (exit status $rc)" >&2' ERR
warn() { echo "install.sh: warning: $*" >&2; }
TARGET=/mnt/target

# --- the specification -------------------------------------------------

if [ -n "$spec_file" ]; then
	spec="$(cat "$spec_file")" || die "cannot read $spec_file"
	jq -e . >/dev/null <<< "$spec" || die "$spec_file is not valid JSON"
else
	[ -n "$storage" ] || { echo "install.sh: --storage URL is required" >&2; exit 2; }
	[ -n "$hostname" ] || { echo "install.sh: --hostname NAME is required" >&2; exit 2; }
	[ -f "$keys" ] || die "$keys does not exist; the new system would have no way in"
	spec="$(jq -n --arg os "$os" --arg hostname "$hostname" --arg timezone "$timezone" --arg storage "${storage%/}" \
		--argjson mitigations "$([ "$mitigations" = 1 ] && echo true || echo false)" --argjson ipv6 "$([ "$ipv6" = 1 ] && echo true || echo false)" \
		--arg disks "$disks_arg" --rawfile keys "$keys" '{
		os: $os, hostname: $hostname, timezone: $timezone, storage: $storage,
		kernel: {mitigations: $mitigations, ipv6: $ipv6, extra: ""},
		layout: {name: "lvm-linear", parts: [
			{type: "slice", target: ["first"], size: "1M", filesystem: "bios"},
			{type: "slice", target: ["first"], size: "512M", filesystem: "efi", mountpoint: "/boot/efi"},
			{type: "slice", target: ["first"], size: "1G", filesystem: "ext4", mountpoint: "/boot"},
			{type: "slice", target: [".*"], size: "rest", filesystem: "lvm", vg: "vg0"},
			{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: "ext4", mountpoint: "/"}]},
		disks: ($disks | if . == "" then [] else split(",") end),
		users: [{name: "root", shell: "/bin/bash", groups: [], sudo: false, keys: ($keys | split("\n") | map(select(test("^[a-z]"))))}],
		userdata: [], firewall: {policy: "accept", allow: []},
		profile: {name: "command line"}}')"
fi
s() { jq -r "$1 // empty" <<< "$spec"; }           # a string, empty when absent
sb() { jq -r "$1 // false" <<< "$spec"; }           # a boolean
os="$(s .os)"; hostname="$(s .hostname)"; timezone="$(s .timezone)"; storage="$(s .storage)"
[ -n "$os" ] && [ -n "$hostname" ] && [ -n "$storage" ] || die "the specification needs os, hostname and storage"
[ -n "$timezone" ] || timezone=UTC
storage="${storage%/}"
mitigations="$(sb .kernel.mitigations)"; ipv6="$(sb .kernel.ipv6)"; kernel_extra="$(s .kernel.extra)"
layout_name="$(s .layout.name)"; [ -n "$layout_name" ] || layout_name="inline"
jq -e '.layout.parts | length > 0' >/dev/null <<< "$spec" || die "the layout has no parts"
jq -e '.users | length > 0' >/dev/null <<< "$spec" || die "the specification names no users"
profile_name="$(s .profile.name)"
private_network="$(s .network.private.network)"; private_address="$(s .network.private.address)"; private_cidr="$(s .network.private.cidr)"
image_url="$storage/os/$os/$arch"
efi=0; [ -d /sys/firmware/efi ] && efi=1

# kho=off: the kernel's kexec handover (on by default in Ubuntu's kernel)
# reserves a scratch area of up to half the memory of a small machine for
# live updates of hypervisors, and that reservation starved the kexec
# into the rescue system on a 1 GB machine
kernel_cmdline="kho=off"
[ "$mitigations" = true ] || kernel_cmdline="$kernel_cmdline mitigations=off"
[ "$ipv6" = true ] || kernel_cmdline="$kernel_cmdline ipv6.disable=1"
[ -n "$kernel_extra" ] && kernel_cmdline="$kernel_cmdline $kernel_extra"

# --- the disks ---------------------------------------------------------

# whole block devices that are not removable media, RAM or loop devices,
# and not on USB (an install medium or virtual media)
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
wanted="$(jq -r '.disks // [] | .[]' <<< "$spec")"
if [ -n "$wanted" ]; then
	for d in $wanted; do
		[ -b "$d" ] || die "$d is not a block device"
		[ "$(lsblk -dnr -o TYPE "$d")" = disk ] || die "$d is not a whole disk"
		disks+=("$d")
	done
else
	# NVMe first, then the rest by name: the first one is "first" in layouts
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) disks+=("$d") ;; esac; done
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) ;; *) disks+=("$d") ;; esac; done
fi
[ ${#disks[@]} -gt 0 ] || die "no disk found"
# "first" in layouts: the first disk of at least 4 GiB, so that a small
# helper disk (a Linode's swap disk, a provider's configuration disk)
# never takes the boot partitions, whatever name the kernel gave it this
# boot; the first disk when none is that big
boot_disk="${disks[0]}"
for d in "${disks[@]}"; do
	if [ "$(blockdev --getsize64 "$d")" -ge $((4 * 1024 * 1024 * 1024)) ]; then boot_disk="$d"; break; fi
done
describe_disk() {
	local size rota tran model
	size="$(lsblk -dnr -o SIZE "$1")"; rota="$(lsblk -dnr -o ROTA "$1")"
	tran="$(lsblk -dnr -o TRAN "$1")"; model="$(lsblk -dn -o MODEL "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
	printf '%s %s%s%s' "$size" "$([ "$rota" = 1 ] && echo HDD || echo SSD)" "${tran:+ $tran}" "${model:+ $model}"
}
disk_bytes() { blockdev --getsize64 "$1"; }

# --- the plan: every partition, array and volume, before anything is touched ---

# partitions: one entry per planned partition
P_DISK=(); P_INDEX=(); P_SIZE=(); P_FS=(); P_MOUNT=(); P_LABEL=(); P_VG=(); P_MD=(); P_DEV=()
# md arrays
M_LEVEL=(); M_FS=(); M_MOUNT=(); M_LABEL=(); M_VG=(); M_DEV=()
# logical volumes
L_VG=(); L_NAME=(); L_SIZE=(); L_RAID=(); L_FS=(); L_MOUNT=(); L_LABEL=(); L_DEV=()
declare -A VG_PVS=()        # volume group -> physical volume devices
declare -A VG_NPV=()        # volume group -> number of physical volumes
declare -A DISK_REST=()     # disk -> 1 when a partition takes the rest

match_disks() { # match_disks TARGETS_JSON -> the matching disks, in order, once each
	local t d out=()
	while IFS= read -r t; do
		[ -n "$t" ] || continue
		for d in "${disks[@]}"; do
			if [ "$t" = first ]; then [ "$d" = "$boot_disk" ] || continue
			else [[ "$d" =~ $t ]] || continue; fi
			case " ${out[*]:-} " in *" $d "*) ;; *) out+=("$d") ;; esac
		done
	done < <(jq -r '.[]' <<< "$1")
	printf '%s\n' "${out[@]:-}"
}
next_index() { # next_index DISK: the next free partition number below 100
	local i max=0
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$1" ] || continue
		[ "${P_INDEX[$i]}" -lt 100 ] && [ "${P_INDEX[$i]}" -gt "$max" ] && max="${P_INDEX[$i]}"
	done
	echo $((max + 1))
}
plan_partition() { # plan_partition DISK INDEX SIZE FS MOUNT LABEL VG MD
	local i
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$1" ] && [ "${P_INDEX[$i]}" = "$2" ] && die "partition $2 of $1 is planned twice"
	done
	case "$3" in rest|all|100%)
		[ -z "${DISK_REST[$1]:-}" ] || die "two parts take the rest of $1"
		DISK_REST[$1]=1 ;;
	esac
	P_DISK+=("$1"); P_INDEX+=("$2"); P_SIZE+=("$3"); P_FS+=("$4"); P_MOUNT+=("$5"); P_LABEL+=("$6"); P_VG+=("$7"); P_MD+=("$8"); P_DEV+=("")
}
min_members() { # min_members LEVEL KIND(md|lvm)
	case "$2:$1" in
	md:0|md:1|md:10) echo 2 ;; md:4|md:5) echo 3 ;; md:6) echo 4 ;;
	lvm:0|lvm:1) echo 2 ;; lvm:5) echo 3 ;; lvm:6) echo 5 ;; lvm:10) echo 4 ;;
	*) echo 1 ;;
	esac
}

nparts="$(jq '.layout.parts | length' <<< "$spec")"
for ((n = 0; n < nparts; n++)); do
	part="$(jq -c ".layout.parts[$n]" <<< "$spec")"
	f() { jq -r "$1 // empty" <<< "$part"; }
	type="$(f .type)"; size="$(f .size)"; fs="$(f .filesystem)"; mount="$(f .mountpoint)"; label="$(f .label)"; vg="$(f .vg)"
	raid="$(jq -r '.raid // "none"' <<< "$part")"
	at="layout $layout_name, part $((n + 1)) ($type)"
	case "$type" in
	slice|md)
		mapfile -t targets < <(match_disks "$(jq -c '.target // []' <<< "$part")")
		[ -n "${targets[0]:-}" ] || die "$at: no disk matches $(jq -c '.target' <<< "$part") (disks: ${disks[*]})"
		if [ "$type" = slice ]; then
			index="$(jq -r '.index // 0' <<< "$part")"
			for d in "${targets[@]}"; do
				i="$index"; [ "$i" -gt 0 ] || i="$(next_index "$d")"
				plan_partition "$d" "$i" "$size" "$fs" "$mount" "$label" "$vg" ""
			done
		else
			need="$(min_members "$raid" md)"
			[ ${#targets[@]} -ge "$need" ] || die "$at: raid $raid needs $need disks, ${#targets[@]} match"
			j=${#M_LEVEL[@]}
			M_LEVEL+=("$raid"); M_FS+=("$fs"); M_MOUNT+=("$mount"); M_LABEL+=("$label"); M_VG+=("$vg"); M_DEV+=("/dev/md$j")
			for d in "${targets[@]}"; do plan_partition "$d" "$(next_index "$d")" "$size" "md" "" "" "" "$j"; done
		fi
		;;
	lvm)
		name="$(f .name)"
		L_VG+=("$vg"); L_NAME+=("$name"); L_SIZE+=("$size"); L_RAID+=("$raid"); L_FS+=("$fs"); L_MOUNT+=("$mount"); L_LABEL+=("$label"); L_DEV+=("/dev/$vg/$name")
		;;
	*) die "$at: unknown part type" ;;
	esac
done
# the volume groups: how many physical volumes each will have, for the
# raid levels that need a minimum
for i in "${!P_DISK[@]}"; do [ "${P_FS[$i]}" = lvm ] && VG_NPV[${P_VG[$i]}]=$(( ${VG_NPV[${P_VG[$i]}]:-0} + 1 )); done
for j in "${!M_LEVEL[@]}"; do [ "${M_FS[$j]}" = lvm ] && VG_NPV[${M_VG[$j]}]=$(( ${VG_NPV[${M_VG[$j]}]:-0} + 1 )); done
for k in "${!L_VG[@]}"; do
	[ -n "${VG_NPV[${L_VG[$k]}]:-}" ] || die "volume group ${L_VG[$k]} of volume ${L_NAME[$k]} gets no physical volume on this machine"
	need="$(min_members "${L_RAID[$k]}" lvm)"
	if [ "${L_RAID[$k]}" = 0 ] && [ "${VG_NPV[${L_VG[$k]}]}" -lt 2 ]; then
		L_RAID[$k]="none"   # striping over one physical volume is linear
	elif [ "${VG_NPV[${L_VG[$k]}]}" -lt "$need" ]; then
		die "volume ${L_NAME[$k]}: raid ${L_RAID[$k]} needs $need physical volumes in ${L_VG[$k]}, this machine gives ${VG_NPV[${L_VG[$k]}]}"
	fi
done
# what the boot loader needs
has_bios=0; has_efi=0; root_dev=""
for i in "${!P_DISK[@]}"; do
	[ "${P_FS[$i]}" = bios ] && has_bios=1
	[ "${P_FS[$i]}" = efi ] && has_efi=1
done
[ "$has_efi" = 1 ] || [ "$efi" = 0 ] || die "this machine boots with UEFI and the layout has no efi part"
[ "$has_bios" = 1 ] || [ "$efi" = 1 ] || [ "$arch" != amd64 ] || die "this machine boots with BIOS and the layout has no bios part"
jq -e '[.layout.parts[] | select(.mountpoint == "/")] | length == 1' >/dev/null <<< "$spec" || die "the layout must mount exactly one part at /"

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

# chroot_resolv FILE: the resolver for a chroot, the rescue system's own
# (its systemd-resolved falls back to TCP where a firewall at the provider
# lets no UDP answers in), else the public resolvers
chroot_resolv() {
	if grep -q '^nameserver' /etc/resolv.conf 2>/dev/null; then
		grep -E '^(nameserver|options|search) ' /etc/resolv.conf > "$1"
	else
		printf 'nameserver %s\n' $dns > "$1"
	fi
}

# the interfaces: every wired one, by MAC. The uplink and those with a
# public address are external (ext01, ext02, ...), the rest internal
# (int01, ...), unless the profile keeps the kernel's names.
rename="$(jq -r 'if .network.rename == false then "no" else "yes" end' <<< "$spec")"
IF_MAC=(); IF_NAME=(); IF_KIND=(); IF_OLD=()
next_ext=1; next_int=1
private='^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|169\.254\.)'
for d in $(ls /sys/class/net | sort -V); do
	[ "$d" = lo ] && continue
	[ -e "/sys/class/net/$d/device" ] || continue         # a device, not a bond, vlan or bridge
	[ "$(cat "/sys/class/net/$d/type" 2>/dev/null)" = 1 ] || continue   # ethernet
	[ -d "/sys/class/net/$d/wireless" ] && continue
	m="$(cat "/sys/class/net/$d/address" 2>/dev/null || true)"
	case "$m" in ""|00:00:00:00:00:00) continue ;; esac
	kind=int
	if [ "$m" = "$mac" ]; then
		kind=ext
	elif ip -o -4 addr show dev "$d" scope global 2>/dev/null | awk '{print $4}' | grep -qvE "$private"; then
		kind=ext
	elif ip -o -6 addr show dev "$d" scope global 2>/dev/null | awk '{print $4}' | grep -qviE '^f[cd]'; then
		kind=ext
	fi
	IF_MAC+=("$m"); IF_KIND+=("$kind"); IF_OLD+=("$d")
	if [ "$rename" = yes ]; then
		if [ "$kind" = ext ]; then IF_NAME+=("$(printf 'ext%02d' "$next_ext")"); next_ext=$((next_ext + 1))
		else IF_NAME+=("$(printf 'int%02d' "$next_int")"); next_int=$((next_int + 1)); fi
	else
		IF_NAME+=("$d")
	fi
done
uplink=""
for i in "${!IF_MAC[@]}"; do [ "${IF_MAC[$i]}" = "$mac" ] && uplink="${IF_NAME[$i]}"; done
if [ -z "$uplink" ]; then
	# the uplink's MAC was given, but no such interface is up here: keep it
	uplink=uplink; [ "$rename" = yes ] && uplink=ext01
	IF_MAC=("$mac" "${IF_MAC[@]}"); IF_KIND=(ext "${IF_KIND[@]}"); IF_OLD=("?" "${IF_OLD[@]}"); IF_NAME=("$uplink" "${IF_NAME[@]}")
fi

# --- the plan, printed ---------------------------------------------------

log "machine: $arch, $([ "$efi" = 1 ] && echo UEFI || echo BIOS) firmware, $(nproc) CPUs, $(awk '/^MemTotal/ {printf "%d", $2 / 1024}' /proc/meminfo) MB RAM"
log "image: $os for $arch from $image_url (profile ${profile_name:-?}, layout $layout_name)"
d=""; for x in "${disks[@]}"; do d="${d:+$d, }$x ($(describe_disk "$x"))"; done
log "disks (${#disks[@]}): $d"
for x in "${disks[@]}"; do
	line=""
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$x" ] || continue
		what="${P_FS[$i]}"
		[ "${P_FS[$i]}" = lvm ] && what="lvm (${P_VG[$i]})"
		[ "${P_FS[$i]}" = md ] && what="md${P_MD[$i]} member"
		line="${line:+$line, }${P_INDEX[$i]}: ${P_SIZE[$i]} $what${P_MOUNT[$i]:+ at ${P_MOUNT[$i]}}"
	done
	log "  $x: ${line:-untouched}"
done
for j in "${!M_LEVEL[@]}"; do
	what="${M_FS[$j]}"; [ "${M_FS[$j]}" = lvm ] && what="lvm (${M_VG[$j]})"
	log "  md$j: raid ${M_LEVEL[$j]} over $(for i in "${!P_DISK[@]}"; do [ "${P_MD[$i]}" = "$j" ] && printf '%s ' "${P_DISK[$i]}"; done), $what${M_MOUNT[$j]:+ at ${M_MOUNT[$j]}}"
done
for k in "${!L_VG[@]}"; do
	r="linear"; [ "${L_RAID[$k]}" != none ] && r="raid ${L_RAID[$k]}"
	log "  ${L_VG[$k]}/${L_NAME[$k]}: ${L_SIZE[$k]} $r ${L_FS[$k]}${L_MOUNT[$k]:+ at ${L_MOUNT[$k]}}"
done
log "network: $uplink ($mac) $ip4${ip6:+ $ip6}; IPv6 $([ "$ipv6" = true ] && echo on || echo off); DNS $dns; hostname $hostname; time zone $timezone"
ifs=""
for i in "${!IF_MAC[@]}"; do
	was=""
	if [ "${IF_OLD[$i]}" != "${IF_NAME[$i]}" ]; then was=", now ${IF_OLD[$i]}"; fi
	ifs="${ifs:+$ifs, }${IF_NAME[$i]} (${IF_KIND[$i]}, ${IF_MAC[$i]}$was)"
done
log "interfaces: $ifs$([ "$rename" = yes ] || printf '; kernel names kept')"
if [ -n "$private_address" ]; then
	log "private network: $private_network, address $private_address (of $private_cidr; recorded, no interface configured)"
fi
log "users: $(jq -r '[.users[] | "\(.name) (\(.keys | length) key(s)\(if .sudo then ", sudo" else "" end))"] | join(", ")' <<< "$spec")"
log "userdata: $(jq -r '.userdata | if length == 0 then "none" else map(.name) | join(" ") end' <<< "$spec"); firewall: $(jq -r '.firewall | if .policy == "drop" then "drop, allow " + ([.allow[] | if .proto == "icmp" then "icmp" else "\(.proto)/\(.port)\(if .from then " from " + .from else "" end)" end] | join(", ")) else "none" end' <<< "$spec")"
log "kernel command line: $kernel_cmdline"
if [ "$dry" = 1 ]; then
	log "dry run: stopping here, no disk was touched"
	exit 0
fi

# --- the image: the manifest first, so that a missing image costs no disk ---

log "fetching the manifest"
manifest="$(curl -fsSL "$image_url/manifest.json")" || die "cannot fetch $image_url/manifest.json; the storage must serve the image (see images/base/README.md)"
family="$(printf '%s' "$manifest" | jq -r '.family // "image"')"
image_version="$(printf '%s' "$manifest" | jq -r '.version // empty')"
image_kernel="$(printf '%s' "$manifest" | jq -r '.kernel // empty')"
# the size of the installed system: the root file system must hold twice it
unpacked_mb="$(printf '%s' "$manifest" | jq -r '.unpacked_mb // empty')"
# a manifest may say why there is nothing for this machine (Arch Linux on
# arm64)
unsupported="$(printf '%s' "$manifest" | jq -r '.unsupported // empty')"
[ -z "$unsupported" ] || die "$os on $arch: $unsupported"
case "$family" in
image)
	want_sum="$(printf '%s' "$manifest" | jq -r '.files["rootfs.tar.zst"].sha256 // empty')"
	[ -n "$want_sum" ] || die "the manifest names no rootfs.tar.zst checksum"
	log "image $os version ${image_version:-?}, kernel ${image_kernel:-?}, ${unpacked_mb:-?} MB unpacked"
	;;
nixos)
	for k in channel nix.url nix.sha256 state_version; do
		[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the NixOS manifest names no $k"
	done
	log "image $os: NixOS from the channel $(printf '%s' "$manifest" | jq -r .channel), installed with nixos-install"
	;;
arch)
	for k in bootstrap.url bootstrap.sha256 mirror; do
		[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the Arch Linux manifest names no $k"
	done
	[ "$(printf '%s' "$manifest" | jq -r '.packages | length')" -gt 0 ] || die "the Arch Linux manifest names no packages"
	if [ -n "$(printf '%s' "$manifest" | jq -r '.cachyos // empty')" ]; then
		for k in cachyos.mirror cachyos.keyring.url cachyos.keyring.sha256; do
			[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the CachyOS manifest names no $k"
		done
		log "image $os: CachyOS from $(printf '%s' "$manifest" | jq -r .cachyos.mirror) over Arch Linux from $(printf '%s' "$manifest" | jq -r .mirror) (bootstrap $(printf '%s' "$manifest" | jq -r .bootstrap.release)), installed with pacstrap"
		log "CachyOS is experimental in UMP: not for production unless you know what you are doing (docs/oses.md#cachyos)"
	else
		log "image $os: Arch Linux from $(printf '%s' "$manifest" | jq -r .mirror) (bootstrap $(printf '%s' "$manifest" | jq -r .bootstrap.release)), installed with pacstrap"
	fi
	;;
*) die "the manifest's family $family is not one this installer knows (image, nixos, arch)" ;;
esac
# the userdata must be of the kind the system takes
want_ud=bash; [ "$family" = nixos ] && want_ud=nix
bad_ud="$(jq -r --arg t "$want_ud" '[.userdata[] | select((.type // "bash") != $t) | .name] | join(", ")' <<< "$spec")"
[ -z "$bad_ud" ] || die "userdata $bad_ud is not of the type $os takes ($want_ud)"

# --- the disks ---------------------------------------------------------

# kill_chrooted: ends what still runs in the new system or a bootstrap
# (a gpg-agent that pacman-key started there, a daemon a userdata
# script left): it keeps the root file system busy, and an attempt
# after a failed one could not release the disks. Every process whose
# root is not the rescue system's: nothing of the rescue system runs
# chrooted, and once its file system is unmounted lazily, the root of a
# process left behind no longer reads as a path under $TARGET.
kill_chrooted() {
	local p r found=""
	for p in /proc/[0-9]*; do
		r="$(readlink "$p/root" 2>/dev/null)" || continue
		[ "$r" = / ] || { kill -9 "${p#/proc/}" 2>/dev/null && found=1; }
	done
	[ -z "$found" ] || sleep 1
	return 0
}

log "releasing the disks"
kill_chrooted
swapoff -a 2>/dev/null || true
# a swap file of an earlier attempt whose file system was unmounted under
# it cannot be found by its name any more, and keeps its disk busy
stale_swap="$(awk 'NR > 1 && $2 == "file" {print $1}' /proc/swaps)"
[ -z "$stale_swap" ] || die "a swap file of an earlier attempt is still active ($stale_swap) and keeps a disk busy; reboot the rescue system (ump server rescue with a reboot) and run the installation again"
while read -r src tgt; do
	case "$src" in /dev/*) ;; *) continue ;; esac
	for x in "${disks[@]}"; do case "$src" in "$x"*|/dev/mapper/*|/dev/md*|/dev/dm-*) umount -l "$tgt" 2>/dev/null || true; break ;; esac; done
done < <(awk '{print $1, $2}' /proc/mounts | sort -k2 -r)
# whatever an earlier attempt left on $TARGET
for _ in 1 2 3 4 5; do mountpoint -q "$TARGET" && umount -R -l "$TARGET" 2>/dev/null || break; done
vgchange -an >/dev/null 2>&1 || true
# a mapping LVM no longer knows (its metadata wiped by an earlier
# attempt) still holds its disk: removed when it sits on one of ours
for dm in /sys/block/dm-*; do
	[ -d "$dm/slaves" ] || continue
	for sl in "$dm"/slaves/*; do
		for x in "${disks[@]}"; do
			case "/dev/${sl##*/}" in "$x"*) dmsetup remove "$(cat "$dm/dm/name")" 2>/dev/null || true; continue 3 ;; esac
		done
	done
done
command -v mdadm >/dev/null && mdadm --stop --scan >/dev/null 2>&1 || true

log "wiping the disks"
for x in "${disks[@]}"; do
	for p in $(lsblk -nr -o PATH "$x" | tail -n +2); do wipefs -af "$p" >/dev/null 2>&1 || true; done
	sgdisk --zap-all "$x" >/dev/null 2>&1 || true
	wipefs -af "$x" >/dev/null
	dd if=/dev/zero of="$x" bs=1M count=2 conv=notrunc status=none 2>/dev/null || true
done
partprobe >/dev/null 2>&1 || true
udevadm settle
pvscan --cache >/dev/null 2>&1 || true

log "partitioning"
sg_size() { # sg_size DISK SIZE -> the end argument for sgdisk
	case "$2" in
	rest|all|100%) echo 0 ;;
	*%) echo "+$(( $(disk_bytes "$1") / 100 * ${2%\%} / 1024 / 1024 ))M" ;;
	*) echo "+$2" ;;
	esac
}
type_code() { case "$1" in bios) echo ef02 ;; efi) echo ef00 ;; swap) echo 8200 ;; lvm) echo 8e00 ;; md) echo fd00 ;; *) echo 8300 ;; esac; }
for x in "${disks[@]}"; do
	args=()
	# every partition in index order, the one that takes the rest last
	for pass in fixed rest; do
		for i in $(for i in "${!P_DISK[@]}"; do [ "${P_DISK[$i]}" = "$x" ] && echo "${P_INDEX[$i]} $i"; done | sort -n | cut -d' ' -f2); do
			case "${P_SIZE[$i]}" in rest|all|100%) [ "$pass" = rest ] || continue ;; *) [ "$pass" = fixed ] || continue ;; esac
			name="${P_LABEL[$i]:-${P_FS[$i]}}"
			args+=(-n "${P_INDEX[$i]}:0:$(sg_size "$x" "${P_SIZE[$i]}")" -t "${P_INDEX[$i]}:$(type_code "${P_FS[$i]}")" -c "${P_INDEX[$i]}:$name")
		done
	done
	[ ${#args[@]} -gt 0 ] || continue
	sgdisk "${args[@]}" "$x" >/dev/null || die "partitioning $x failed"
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
for i in "${!P_DISK[@]}"; do P_DEV[$i]="$(partdev "${P_DISK[$i]}" "${P_INDEX[$i]}")"; done

if [ ${#M_LEVEL[@]} -gt 0 ]; then
	log "md arrays"
	for j in "${!M_LEVEL[@]}"; do
		members=(); for i in "${!P_DISK[@]}"; do [ "${P_MD[$i]}" = "$j" ] && members+=("${P_DEV[$i]}"); done
		mdadm --create "${M_DEV[$j]}" --level="${M_LEVEL[$j]}" --raid-devices="${#members[@]}" --metadata=1.2 --force --run "${members[@]}" >/dev/null 2>&1 || die "mdadm --create ${M_DEV[$j]} failed"
	done
	udevadm settle
fi

format() { # format DEV FS LABEL
	local l=()
	case "$2" in
	efi|vfat) [ -n "$3" ] && l=(-n "$3"); mkfs.vfat -F 32 "${l[@]}" "$1" >/dev/null ;;
	# without orphan_file and metadata_csum_seed, which a current mkfs.ext4
	# turns on and the GRUB of older releases (Ubuntu 22.04) cannot read
	ext4) [ -n "$3" ] && l=(-L "$3"); mkfs.ext4 -q -F -O ^orphan_file,^metadata_csum_seed "${l[@]}" "$1" ;;
	xfs) [ -n "$3" ] && l=(-L "$3"); command -v mkfs.xfs >/dev/null || die "mkfs.xfs is missing"; mkfs.xfs -f -q "${l[@]}" "$1" ;;
	btrfs) [ -n "$3" ] && l=(-L "$3"); command -v mkfs.btrfs >/dev/null || die "mkfs.btrfs is missing"; mkfs.btrfs -f -q "${l[@]}" "$1" >/dev/null ;;
	swap) [ -n "$3" ] && l=(-L "$3"); mkswap -f "${l[@]}" "$1" >/dev/null ;;
	lvm) pvcreate -ff -y "$1" >/dev/null ;;
	bios|none|md) ;;
	*) die "unknown file system $2" ;;
	esac
}
add_pv() { VG_PVS[$1]="${VG_PVS[$1]:-}${VG_PVS[$1]:+ }$2"; }

log "file systems"
for i in "${!P_DISK[@]}"; do
	format "${P_DEV[$i]}" "${P_FS[$i]}" "${P_LABEL[$i]}"
	[ "${P_FS[$i]}" = lvm ] && add_pv "${P_VG[$i]}" "${P_DEV[$i]}"
done
for j in "${!M_LEVEL[@]}"; do
	format "${M_DEV[$j]}" "${M_FS[$j]}" "${M_LABEL[$j]}"
	[ "${M_FS[$j]}" = lvm ] && add_pv "${M_VG[$j]}" "${M_DEV[$j]}"
done
if [ ${#VG_PVS[@]} -gt 0 ]; then
	log "volume groups: $(for vg in "${!VG_PVS[@]}"; do printf '%s (%s) ' "$vg" "${VG_PVS[$vg]}"; done)"
	for vg in "${!VG_PVS[@]}"; do vgcreate -y "$vg" ${VG_PVS[$vg]} >/dev/null || die "vgcreate $vg failed"; done
	for k in "${!L_VG[@]}"; do
		npv="$(vgs --noheadings -o pv_count "${L_VG[$k]}" | tr -d ' ')"
		sz=()
		case "${L_SIZE[$k]}" in
		rest|all|100%) sz=(-l 100%FREE) ;;
		*%) sz=(-l "${L_SIZE[$k]}VG") ;;
		*) sz=(-L "${L_SIZE[$k]}") ;;
		esac
		kind=()
		case "${L_RAID[$k]}" in
		0) kind=(--type striped -i "$npv") ;;
		1) kind=(--type raid1 -m 1) ;;
		5) kind=(--type raid5 -i $((npv - 1))) ;;
		6) kind=(--type raid6 -i $((npv - 2))) ;;
		10) kind=(--type raid10 -i $((npv / 2)) -m 1) ;;
		esac
		lvcreate -y "${sz[@]}" "${kind[@]}" -n "${L_NAME[$k]}" "${L_VG[$k]}" >/dev/null || die "lvcreate ${L_VG[$k]}/${L_NAME[$k]} failed"
	done
	udevadm settle
	for k in "${!L_VG[@]}"; do format "${L_DEV[$k]}" "${L_FS[$k]}" "${L_LABEL[$k]}"; done
fi

# --- mounting: root first, then the rest by depth -------------------------

MOUNTS=()   # "mountpoint dev fs"
SWAPS=()
efi_dev=""
collect() { # collect DEV FS MOUNT
	case "$2" in
	swap) SWAPS+=("$1") ;;
	efi) [ -n "$efi_dev" ] || { efi_dev="$1"; MOUNTS+=("$3 $1 vfat"); } ;;
	bios|lvm|md|none) ;;
	*) [ -n "$3" ] && MOUNTS+=("$3 $1 $2") ;;
	esac
}
for i in "${!P_DISK[@]}"; do collect "${P_DEV[$i]}" "${P_FS[$i]}" "${P_MOUNT[$i]}"; done
for j in "${!M_LEVEL[@]}"; do collect "${M_DEV[$j]}" "${M_FS[$j]}" "${M_MOUNT[$j]}"; done
for k in "${!L_VG[@]}"; do collect "${L_DEV[$k]}" "${L_FS[$k]}" "${L_MOUNT[$k]}"; done
mapfile -t MOUNTS < <(printf '%s\n' "${MOUNTS[@]}" | awk '{print length($1), $0}' | sort -n | cut -d' ' -f2-)
root_dev=""
for m in "${MOUNTS[@]}"; do set -- $m; [ "$1" = / ] && root_dev="$2"; done
[ -n "$root_dev" ] || die "nothing is mounted at /"
root_mb="$(lsblk -dnr -o SIZE -b "$root_dev" | awk '{printf "%d", $1 / 1024 / 1024}')"
if [ -n "$unpacked_mb" ] && [ "$root_mb" -lt $((unpacked_mb * 2)) ]; then
	die "the root file system ($root_mb MB) is too small for the image ($unpacked_mb MB unpacked)"
fi

log "mounting the new system at $TARGET"
mkdir -p "$TARGET"
# the directory above $TARGET a private mount of its own, before the new
# system is mounted: nothing mounted there is copied into the mount
# namespaces of the rescue system's services (resolved, udevd, networkd).
# A copy of a mount made private later (the Arch Linux bootstrap's) would
# never see it unmounted, and keep the disks busy for the next attempt.
# Not $TARGET itself: nixos-generate-config would take that mount for a
# second root file system.
target_parent="$(dirname "$TARGET")"
if ! mountpoint -q "$target_parent"; then
	mount --bind "$target_parent" "$target_parent" && mount --make-private "$target_parent" || die "cannot make $target_parent a private mount"
fi
for m in "${MOUNTS[@]}"; do
	set -- $m
	mkdir -p "$TARGET$1"
	mount -t "$3" "$2" "$TARGET$1" || die "mounting $2 at $1 failed"
done
cleanup() {
	set +e
	kill_chrooted
	# a swap file on the new root keeps it busy: off before it is unmounted
	[ -n "${target_swap:-}" ] && swapoff "$target_swap" 2>/dev/null && rm -f "$target_swap"
	# what is mounted into the Arch Linux bootstrap
	[ -n "${arch_boot:-}" ] && arch_umount
	mountpoint -q /nix && umount -l /nix 2>/dev/null
	for m in "$TARGET/dev/pts" "$TARGET/run" "$TARGET/sys" "$TARGET/proc" "$TARGET/dev"; do mountpoint -q "$m" && umount -l "$m" 2>/dev/null; done
	for ((n = ${#MOUNTS[@]} - 1; n >= 0; n--)); do set -- ${MOUNTS[$n]}; mountpoint -q "$TARGET$1" && umount -l "$TARGET$1" 2>/dev/null; done
}
trap cleanup EXIT

# the administrator: the first sudo user other than root, else root, as
# ump picks it; ump logs in as it
admin="$(jq -r '([.users[] | select(.sudo == true and .name != "root")][0].name) // ([.users[] | select(.name == "root")][0].name) // .users[0].name' <<< "$spec")"

# the firewall ruleset, the same for every system: input policy, and the
# same list for new connections forwarded in from the external interfaces
nft_ruleset() {
	ext=""; for i in "${!IF_MAC[@]}"; do [ "${IF_KIND[$i]}" = ext ] && ext="${ext:+$ext, }\"${IF_NAME[$i]}\""; done
	# the allowed entries, for the input chain (dport) and for the forward
	# chain, where the destination port is the container's after the DNAT
	# of a published port, so the original one is matched; ICMPv6 is
	# protocol 58, by number because an older nft resolves the name
	# through /etc/protocols only
	allow() { # allow input|forward
		if [ "$1" = input ]; then
			jq -r '.firewall.allow[] | if .proto == "icmp" then "    ip protocol icmp accept\n    meta l4proto 58 accept" else (if .from then (if (.from | test(":")) then "    ip6 saddr " + .from + " " else "    ip saddr " + .from + " " end) else "    " end) + .proto + " dport " + .port + " accept" end' <<< "$spec"
		else
			jq -r '.firewall.allow[] | if .proto == "icmp" then "    ip protocol icmp accept\n    meta l4proto 58 accept" else (if .from then (if (.from | test(":")) then "    ip6 saddr " + .from + " " else "    ip saddr " + .from + " " end) else "    " end) + "meta l4proto " + .proto + " ct original proto-dst " + .port + " accept" end' <<< "$spec"
		fi
	}
	echo "#!/usr/sbin/nft -f"
	echo "# written by UMP from the profile's firewall section; nft -f after editing"
	echo "flush ruleset"
	echo "table inet filter {"
	echo "  chain input {"
	echo "    type filter hook input priority 0; policy drop;"
	echo "    iif lo accept"
	echo "    ct state established,related accept"
	echo "    ct state invalid drop"
	allow input
	echo "  }"
	echo "  # Forwarded traffic (containers, virtual machines) passes, except"
	echo "  # new connections coming in on the external interfaces: those are"
	echo "  # held to the same list, by the port they were addressed to. A"
	echo "  # port a container publishes (docker run -p) is reachable only"
	echo "  # when it is allowed here, whatever the container engine adds."
	echo "  chain forward {"
	echo "    type filter hook forward priority 0; policy accept;"
	echo "    ct state established,related accept"
	echo "    ct state invalid drop"
	echo "    iifname { ${ext:-\"ext*\"} } ct state new jump forward-in"
	echo "  }"
	echo "  chain forward-in {"
	allow forward
	echo "    drop"
	echo "  }"
	echo "  chain output {"
	echo "    type filter hook output priority 0; policy accept;"
	echo "  }"
	echo "}"
}

# --- Arch Linux: pacstrap from the mirrors, out of the monthly bootstrap ---

# Arch is a rolling release: there is no image to unpack, but the official
# bootstrap tarball (pinned by the manifest, from archive.archlinux.org)
# runs pacstrap, which installs the manifest's packages as the mirror has
# them today; pacman checks every package's signature against the Arch
# keyring. The bootstrap lives on the new disk, not in the rescue system's
# memory, and is removed afterwards.
arch_umount() {
	local m
	kill_chrooted
	for m in mnt dev sys proc; do
		mountpoint -q "$arch_boot/$m" && umount -R -l "$arch_boot/$m" 2>/dev/null
	done
	return 0
}

# CachyOS: Arch Linux with CachyOS's repositories in front of Arch's,
# whose packages are built for newer CPUs (x86-64-v3: AVX2 and more) and
# win over Arch's of the same name, its own pacman and kernel among them.
# The keyring package comes pinned by its checksum, as the bootstrap
# does; it makes pacman trust the keys that sign CachyOS's packages.
cachyos_level=""

# cachyos_conf FILE Server|Include LEVEL: CachyOS's repositories in front
# of the first repository of the pacman.conf FILE: those of LEVEL
# (x86-64-v3, x86-64-v4; none for the plain x86_64 ones alone), with the
# manifest's mirror (Server, for the bootstrap's stock pacman, which
# knows no $arch_v3) or CachyOS's mirror lists (Include, for the new
# system, whose pacman is CachyOS's)
cachyos_conf() {
	local file="$1" how="$2" level="$3" cmirror blocks="" r v=""
	cmirror="$(jq -r .cachyos.mirror <<< "$manifest")"
	case "$level" in x86-64-v3) v=v3 ;; x86-64-v4) v=v4 ;; esac
	if [ -n "$v" ]; then
		for r in "cachyos-$v" "cachyos-core-$v" "cachyos-extra-$v"; do
			if [ "$how" = Server ]; then
				blocks="$blocks[$r]\nServer = $cmirror/x86_64_$v/\$repo\n\n"
			else
				blocks="$blocks[$r]\nInclude = /etc/pacman.d/cachyos-$v-mirrorlist\n\n"
			fi
		done
	fi
	if [ "$how" = Server ]; then
		blocks="$blocks[cachyos]\nServer = $cmirror/x86_64/\$repo\n\n"
	else
		blocks="$blocks[cachyos]\nInclude = /etc/pacman.d/cachyos-mirrorlist\n\n"
	fi
	awk -v b="$blocks" '!done && /^\[/ && $0 != "[options]" {printf "%s", b; done = 1} {print}' "$file" > "$file.ump" \
		&& mv "$file.ump" "$file" || die "cannot add CachyOS's repositories to $file"
	grep -q '^\[cachyos\]' "$file" || die "cannot add CachyOS's repositories to $file"
}

cachyos_repos() {
	local url sum have l levels archs=x86_64
	url="$(jq -r .cachyos.keyring.url <<< "$manifest")"
	sum="$(jq -r .cachyos.keyring.sha256 <<< "$manifest")"
	# the newest of the manifest's CPU levels this machine runs, as the
	# bootstrap's glibc tells it
	levels="$(in_boot /usr/lib/ld-linux-x86-64.so.2 --help 2>/dev/null || true)"
	for l in $(jq -r '.cachyos.levels // [] | .[]' <<< "$manifest"); do
		if grep -q "$l (supported, searched)" <<< "$levels"; then
			cachyos_level="$l"
			break
		fi
	done
	if [ -n "$cachyos_level" ]; then
		log "CachyOS: this CPU runs $cachyos_level; its repositories first"
		archs="x86_64 x86_64_${cachyos_level#x86-64-}"
	else
		cachyos_level=x86-64
		log "CachyOS: this CPU runs none of $(jq -r '.cachyos.levels // [] | join(", ")' <<< "$manifest"); the plain x86_64 repository"
	fi
	log "CachyOS keyring"
	curl -fsSL -o "$arch_boot/root/cachyos-keyring.pkg.tar.zst" "$url" || die "cannot download $url"
	have="$(sha256sum "$arch_boot/root/cachyos-keyring.pkg.tar.zst" | cut -d' ' -f1)"
	[ "$have" = "$sum" ] || die "checksum mismatch for $url: manifest $sum, download $have"
	{ in_boot pacman -U --noconfirm /root/cachyos-keyring.pkg.tar.zst && in_boot pacman-key --populate cachyos; } >> "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "the CachyOS keyring could not be set up"; }
	rm -f "$arch_boot/root/cachyos-keyring.pkg.tar.zst"
	cachyos_conf "$arch_boot/etc/pacman.conf" Server "$cachyos_level"
	sed -i "s/^#*Architecture = .*/Architecture = $archs/" "$arch_boot/etc/pacman.conf"
}

arch_pacstrap() {
	local url sum mirror release have pkgs plog
	url="$(jq -r .bootstrap.url <<< "$manifest")"
	sum="$(jq -r .bootstrap.sha256 <<< "$manifest")"
	release="$(jq -r .bootstrap.release <<< "$manifest")"
	mirror="$(jq -r .mirror <<< "$manifest")"
	mapfile -t pkgs < <(jq -r '.packages[]' <<< "$manifest")

	arch_boot="$TARGET/.ump-arch-bootstrap"
	log "downloading the Arch Linux bootstrap $release"
	mkdir -p "$arch_boot"
	have="$(mktemp)"
	curl -fsSL "$url" | tee >(sha256sum | cut -d' ' -f1 > "$have") | zstd -dc | tar --warning=no-unknown-keyword -xpf - -C "$arch_boot" --strip-components=1 \
		|| die "cannot download or unpack $url"
	for _ in $(seq 1 100); do [ -s "$have" ] && break; sleep 0.1; done
	[ "$(cat "$have")" = "$sum" ] || die "checksum mismatch for $url: manifest $sum, download $(cat "$have")"
	rm -f "$have"

	printf 'Server = %s/$repo/os/$arch\n' "$mirror" > "$arch_boot/etc/pacman.d/mirrorlist"
	# parallel downloads; and no free space check, which needs the
	# bootstrap's / to be a mount point (the installer checked the root's
	# size already; the new system keeps Arch's pacman.conf with the check)
	sed -i -e 's/^#ParallelDownloads.*/ParallelDownloads = 5/' -e 's/^CheckSpace/#CheckSpace/' "$arch_boot/etc/pacman.conf"
	rm -f "$arch_boot/etc/resolv.conf"
	chroot_resolv "$arch_boot/etc/resolv.conf"
	# the new system with everything mounted in it (/boot, /boot/efi) as
	# the bootstrap's /mnt, first, while the bootstrap has no mounts of its
	# own yet; private, so that nothing mounted later shows up twice. /sys
	# and /dev as arch-chroot binds them: slaves, so that unmounting them
	# never reaches the rescue system's own.
	mkdir -p "$arch_boot/mnt"
	mount --rbind "$TARGET" "$arch_boot/mnt"
	mount --make-rprivate "$arch_boot/mnt"
	mount -t proc proc "$arch_boot/proc"
	mount --rbind /sys "$arch_boot/sys"
	mount --make-rslave "$arch_boot/sys"
	mount --rbind /dev "$arch_boot/dev"
	mount --make-rslave "$arch_boot/dev"
	in_boot() { chroot "$arch_boot" /usr/bin/env -i HOME=/root LANG=C.UTF-8 PATH=/usr/local/sbin:/usr/local/bin:/usr/bin "$@"; }

	log "pacman keyring"
	plog="$TARGET/.ump-pacstrap.log"
	{ in_boot pacman-key --init && in_boot pacman-key --populate archlinux; } > "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "the pacman keyring could not be set up"; }
	[ -z "$(jq -r '.cachyos // empty' <<< "$manifest")" ] || cachyos_repos
	# the newest keys first: the bootstrap's may predate who signs today
	in_boot pacman -Sy --noconfirm archlinux-keyring >> "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "cannot update archlinux-keyring from $mirror"; }

	log "pacstrap: ${#pkgs[@]} packages from $mirror (this takes a few minutes)"
	# without -K: the new system takes the bootstrap's keyring, made for
	# this installation a moment ago, instead of making and filling another
	in_boot pacstrap /mnt "${pkgs[@]}" >> "$plog" 2>&1 \
		|| { tail -n 40 "$plog" >&2; die "pacstrap failed"; }
	# the new system's pacman.conf (CachyOS's pacman's, which knows only
	# Arch's repositories) gets CachyOS's in front, from their mirror lists
	if [ -n "$cachyos_level" ]; then
		cachyos_conf "$TARGET/etc/pacman.conf" Include "$cachyos_level"
		sed -i 's/^#*Architecture = .*/Architecture = auto/' "$TARGET/etc/pacman.conf"
	fi

	arch_umount
	# the new system was mounted in there: nothing may be left mounted
	# before the bootstrap is deleted
	if awk -v d="$arch_boot/" 'index($2, d) == 1 {found = 1} END {exit !found}' /proc/mounts; then
		die "something is still mounted under $arch_boot; not deleting it (the new system is mounted in there)"
	fi
	rm -rf "$arch_boot"
	arch_boot=""
	mkdir -p "$TARGET/var/log"
	mv "$plog" "$TARGET/var/log/ump-pacstrap.log"
	image_version="rolling, packages of $(date -u +%Y-%m-%d)${cachyos_level:+, CachyOS repositories $cachyos_level}"
	image_kernel="$(ls "$TARGET/usr/lib/modules" | head -n 1)"
}

# --- an image (Ubuntu, Debian), or Arch Linux: then configured in place ----

install_image() {
if [ "$family" = arch ]; then
	arch_pacstrap
else
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
fi
sync

# --- the machine's own configuration ------------------------------------

log "configuring $hostname"
[ -f "$TARGET/usr/share/zoneinfo/$timezone" ] || die "unknown time zone $timezone"
echo "$hostname" > "$TARGET/etc/hostname"
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n127.0.1.1\t%s\n' "$hostname" > "$TARGET/etc/hosts"
ln -sf "/usr/share/zoneinfo/$timezone" "$TARGET/etc/localtime"
echo "$timezone" > "$TARGET/etc/timezone"

{
	echo "# /etc/fstab, written by UMP (layout $layout_name over ${disks[*]})"
	for m in "${MOUNTS[@]}"; do
		set -- $m
		uuid="$(blkid -s UUID -o value "$2")"
		case "$1:$3" in
		/boot/efi:*) printf 'UUID=%s  %s  vfat  umask=0077  0 1\n' "$uuid" "$1" ;;
		/:ext4) printf 'UUID=%s  /  ext4  defaults,errors=remount-ro  0 1\n' "$uuid" ;;
		/:*) printf 'UUID=%s  /  %s  defaults  0 1\n' "$uuid" "$3" ;;
		*) printf 'UUID=%s  %s  %s  defaults  0 2\n' "$uuid" "$1" "$3" ;;
		esac
	done
	for sw in "${SWAPS[@]:-}"; do [ -n "$sw" ] && printf 'UUID=%s  none  swap  sw  0 0\n' "$(blkid -s UUID -o value "$sw")"; done
} > "$TARGET/etc/fstab"

if [ "$family" = arch ]; then
# Arch: systemd-networkd itself, the same configuration as netplan writes
# on Ubuntu and Debian: names by MAC (.link), the uplink static
nd="$TARGET/etc/systemd/network"
mkdir -p "$nd"
for i in "${!IF_MAC[@]}"; do
	if [ "$rename" = yes ]; then
		printf '# written by UMP at installation: the uniform name of this interface\n[Match]\nMACAddress=%s\n\n[Link]\nName=%s\n' \
			"${IF_MAC[$i]}" "${IF_NAME[$i]}" > "$nd/10-ump-${IF_NAME[$i]}.link"
	fi
	[ "${IF_NAME[$i]}" = "$uplink" ] && continue
	printf '# written by UMP at installation: up, without addresses\n[Match]\nMACAddress=%s\n\n[Link]\nRequiredForOnline=no\n\n[Network]\nLinkLocalAddressing=no\n' \
		"${IF_MAC[$i]}" > "$nd/20-ump-${IF_NAME[$i]}.network"
done
{
	echo "# written by UMP at installation; networkctl reload after editing"
	echo "[Match]"
	echo "MACAddress=$mac"
	echo
	echo "[Network]"
	if [ -z "$ip4$ip6" ]; then
		echo "DHCP=ipv4"
	else
		[ -n "$ip4" ] && echo "Address=${ip4%%,*}"
		[ "$ipv6" = true ] && [ -n "$ip6" ] && echo "Address=${ip6%%,*}"
	fi
	for d in $dns; do echo "DNS=$d"; done
	if [ "$ipv6" = true ]; then echo "IPv6AcceptRA=no"; else echo "LinkLocalAddressing=no"; echo "IPv6AcceptRA=no"; fi
	case "$ip4" in *,*) printf '\n[Route]\nGateway=%s\nGatewayOnLink=yes\n' "${ip4#*,}" ;; esac
	if [ "$ipv6" = true ]; then case "$ip6" in *,*) printf '\n[Route]\nGateway=%s\nGatewayOnLink=yes\n' "${ip6#*,}" ;; esac; fi
} > "$nd/20-ump-$uplink.network"
ln -sf ../run/systemd/resolve/stub-resolv.conf "$TARGET/etc/resolv.conf"
echo "LANG=C.UTF-8" > "$TARGET/etc/locale.conf"
echo "KEYMAP=us" > "$TARGET/etc/vconsole.conf"
else
mkdir -p "$TARGET/etc/netplan"
{
	echo "# written by UMP at installation; netplan try / netplan apply after editing"
	echo "network:"
	echo "  version: 2"
	echo "  renderer: networkd"
	echo "  ethernets:"
	for i in "${!IF_MAC[@]}"; do
		[ "${IF_NAME[$i]}" = "$uplink" ] && continue
		echo "    ${IF_NAME[$i]}:"
		echo "      match:"
		echo "        macaddress: \"${IF_MAC[$i]}\""
		[ "$rename" = yes ] && echo "      set-name: ${IF_NAME[$i]}"
		echo "      dhcp4: false"
		echo "      optional: true"
		[ "$ipv6" = true ] || echo "      link-local: []"
	done
	echo "    $uplink:"
	echo "      match:"
	echo "        macaddress: \"$mac\""
	[ "$rename" = yes ] && echo "      set-name: $uplink"
	if [ -z "$ip4$ip6" ]; then
		echo "      dhcp4: true"
	else
		echo "      dhcp4: false"
		echo "      addresses:"
		[ -n "$ip4" ] && echo "        - \"${ip4%%,*}\""
		[ "$ipv6" = true ] && [ -n "$ip6" ] && echo "        - \"${ip6%%,*}\""
		echo "      routes:"
		case "$ip4" in *,*) printf '        - to: default\n          via: "%s"\n          on-link: true\n' "${ip4#*,}" ;; esac
		if [ "$ipv6" = true ]; then case "$ip6" in *,*) printf '        - to: "::/0"\n          via: "%s"\n          on-link: true\n' "${ip6#*,}" ;; esac; fi
	fi
	if [ "$ipv6" = true ]; then echo "      accept-ra: false"; else echo "      link-local: []"; fi
	echo "      nameservers:"
	echo "        addresses: [$(echo "$dns" | sed 's/ /, /g')]"
} > "$TARGET/etc/netplan/10-ump.yaml"
chmod 600 "$TARGET/etc/netplan/10-ump.yaml"
fi

if [ "$family" = arch ]; then
	# Arch's GRUB reads /etc/default/grub alone (no grub.d)
	[ -f "$TARGET/etc/default/grub" ] || die "the new system has no /etc/default/grub"
	sed -i "/^GRUB_CMDLINE_LINUX_DEFAULT=/c\\
# UMP: the kernel command line of this machine: mitigations=off and\\
# ipv6.disable=1 unless the profile turns them on, kho=off always (see\\
# docs/oses.md). Change it here, run grub-mkconfig -o /boot/grub/grub.cfg\\
# and reboot.\\
GRUB_CMDLINE_LINUX_DEFAULT=\"$kernel_cmdline\"" "$TARGET/etc/default/grub"
else
mkdir -p "$TARGET/etc/default/grub.d"
cat > "$TARGET/etc/default/grub.d/ump-cmdline.cfg" <<GRUB
# UMP: the kernel command line of this machine. By default the CPU
# vulnerability mitigations are off (mitigations=off) and IPv6 is off
# (ipv6.disable=1): both cost performance, and most servers need neither.
# To turn one back on, remove it from the line below and run update-grub;
# for IPv6 also add the address and route to /etc/netplan/10-ump.yaml
# (netplan apply). Then reboot. kho=off keeps the kernel's kexec handover
# from reserving a large part of the memory for live updates, which no
# server needs and which stops ump from re-entering its rescue system.
GRUB_CMDLINE_LINUX_DEFAULT="$kernel_cmdline"
GRUB
fi

: > "$TARGET/etc/machine-id"
rm -f "$TARGET/var/lib/dbus/machine-id"

mount --bind /dev "$TARGET/dev"
mount -t devpts devpts "$TARGET/dev/pts" 2>/dev/null || true
mount -t proc proc "$TARGET/proc"
mount -t sysfs sysfs "$TARGET/sys"
mount --bind /run "$TARGET/run"
in_target() { chroot "$TARGET" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"; }
run_in_target() { # run_in_target WHAT COMMAND...: the output is shown only when it fails
	local what="$1" out; shift
	if ! out="$(in_target "$@" 2>&1)"; then printf '%s\n' "$out" >&2; die "$what failed"; fi
}
in_target ssh-keygen -A >/dev/null

# the shell setup and the message of the day, the same on every system:
# from the storage, else the image's own stay
if curl -fsSL -o "$TARGET/etc/skel/.bashrc.ump" "$storage/install/bashrc.sh"; then
	mv "$TARGET/etc/skel/.bashrc.ump" "$TARGET/etc/skel/.bashrc"
	cp "$TARGET/etc/skel/.bashrc" "$TARGET/root/.bashrc"
else
	rm -f "$TARGET/etc/skel/.bashrc.ump"
	warn "cannot fetch $storage/install/bashrc.sh; the image's shell setup stays"
fi
if [ "$family" = arch ]; then
	# Arch has no update-motd.d: the login shell shows it, as on NixOS
	mkdir -p "$TARGET/usr/local/bin" "$TARGET/etc/profile.d"
	if curl -fsSL -o "$TARGET/usr/local/bin/ump-motd" "$storage/install/motd.sh"; then
		chmod 755 "$TARGET/usr/local/bin/ump-motd"
		cat > "$TARGET/etc/profile.d/ump-motd.sh" <<'MOTD'
# the message of the day of UMP, once per SSH login
if [ -n "${SSH_CONNECTION:-}" ] && [ -t 1 ] && [ -z "${UMP_MOTD_SHOWN:-}" ]; then
	export UMP_MOTD_SHOWN=1
	/usr/local/bin/ump-motd
fi
MOTD
	else
		warn "cannot fetch $storage/install/motd.sh; no message of the day"
	fi
else
mkdir -p "$TARGET/etc/update-motd.d"
if curl -fsSL -o "$TARGET/etc/update-motd.d/00-ump" "$storage/install/motd.sh"; then
	chmod 755 "$TARGET/etc/update-motd.d/00-ump"
	ln -sf /etc/update-motd.d/00-ump "$TARGET/usr/local/bin/ump-motd"
else
	warn "cannot fetch $storage/install/motd.sh; the image's message of the day stays"
fi
# Ubuntu's news for the message of the day comes from 50-motd-news, which
# UMP's images leave out; its timer would start the missing script every
# few hours and leave the system degraded (images up to 2026-10-19 still
# have the timer on)
if [ ! -e "$TARGET/etc/update-motd.d/50-motd-news" ] && { [ -e "$TARGET/usr/lib/systemd/system/motd-news.timer" ] || [ -e "$TARGET/lib/systemd/system/motd-news.timer" ]; }; then
	rm -f "$TARGET/etc/systemd/system/timers.target.wants/motd-news.timer"
	ln -sf /dev/null "$TARGET/etc/systemd/system/motd-news.timer"
fi
fi

# --- users --------------------------------------------------------------

log "users"
# the group of the administrators: sudo on Ubuntu and Debian, wheel on Arch
sudo_group=sudo; [ "$family" = arch ] && sudo_group=wheel
nusers="$(jq '.users | length' <<< "$spec")"
for ((n = 0; n < nusers; n++)); do
	u="$(jq -c ".users[$n]" <<< "$spec")"
	name="$(jq -r .name <<< "$u")"; shell="$(jq -r '.shell // "/bin/bash"' <<< "$u")"; sudo="$(jq -r '.sudo // false' <<< "$u")"
	if [ "$name" = root ]; then
		home=/root
	else
		if ! in_target getent passwd "$name" >/dev/null 2>&1; then
			run_in_target "useradd $name" useradd -m -s "$shell" "$name"
		else
			# an account of the image (www-data, backup, daemon) has no login
			# shell: nobody would get in after the reboot
			uid="$(in_target getent passwd "$name" | cut -d: -f3)"
			if [ "$uid" -lt 1000 ] || [ "$uid" -ge 65534 ]; then
				die "user $name is a system account of the image; choose another name"
			fi
		fi
		home="$(in_target getent passwd "$name" | cut -d: -f6)"
		for g in $(jq -r '.groups // [] | .[]' <<< "$u"); do
			in_target getent group "$g" >/dev/null 2>&1 || run_in_target "groupadd $g" groupadd "$g"
			run_in_target "usermod $name" usermod -aG "$g" "$name"
		done
		if [ "$sudo" = true ]; then
			run_in_target "usermod $name" usermod -aG "$sudo_group" "$name"
			mkdir -p "$TARGET/etc/sudoers.d"
			printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$name" > "$TARGET/etc/sudoers.d/90-ump-$name"
			chmod 440 "$TARGET/etc/sudoers.d/90-ump-$name"
		fi
	fi
	mkdir -p -m 700 "$TARGET$home/.ssh"
	jq -r '.keys[]' <<< "$u" | awk '!seen[$0]++' > "$TARGET$home/.ssh/authorized_keys"
	chmod 600 "$TARGET$home/.ssh/authorized_keys"
	[ "$name" = root ] || in_target chown -R "$name:$name" "$home/.ssh"
done
# root over SSH only when the profile lists it with keys (the image says
# no)
mkdir -p "$TARGET/etc/ssh/sshd_config.d"
rm -f "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
if jq -e '[.users[] | select(.name == "root")] | length > 0' >/dev/null <<< "$spec"; then
	printf '# UMP: the profile lists root with keys.\nPermitRootLogin prohibit-password\n' > "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
	log "ssh: root logs in with keys; administrator $admin"
else
	log "ssh: root cannot log in; administrator $admin (sudo)"
fi

# --- nftables, userdata -------------------------------------------------

# the network for the chroot: the resolver of the rescue system serves it,
# and no service starts there
net_on() {
	resolv_was="$(readlink "$TARGET/etc/resolv.conf" 2>/dev/null || true)"
	rm -f "$TARGET/etc/resolv.conf"
	chroot_resolv "$TARGET/etc/resolv.conf"
	printf '#!/bin/sh\nexit 101\n' > "$TARGET/usr/sbin/policy-rc.d"; chmod 755 "$TARGET/usr/sbin/policy-rc.d"
}
net_off() {
	in_target apt-get clean >/dev/null 2>&1 || true
	rm -f "$TARGET/usr/sbin/policy-rc.d" "$TARGET/etc/resolv.conf"
	[ -n "$resolv_was" ] && ln -s "$resolv_was" "$TARGET/etc/resolv.conf"
	return 0
}

if jq -e '.firewall.policy == "drop"' >/dev/null <<< "$spec" && [ ! -x "$TARGET/usr/sbin/nft" ]; then
	log "packages: nftables (the firewall needs it)"
	net_on
	run_in_target "apt-get update" apt-get update -q
	run_in_target "apt-get install" apt-get install -y -q --no-install-recommends nftables
	net_off
fi

# the userdata: each entry as a script under /etc/ump/userdata in the new
# system, run there in order as root, with network; a failing one fails
# the installation
n_userdata=$(jq -r '.userdata | length' <<< "$spec")
if [ "$n_userdata" -gt 0 ]; then
	mkdir -p "$TARGET/etc/ump/userdata"
	net_on
	for ((i = 0; i < n_userdata; i++)); do
		ud_name="$(jq -r ".userdata[$i].name" <<< "$spec")"
		ud_type="$(jq -r ".userdata[$i].type // \"bash\"" <<< "$spec")"
		[ "$ud_type" = bash ] || die "userdata $ud_name: type $ud_type is not supported by this installer (bash only)"
		ud_file="/etc/ump/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.sh"
		jq -r ".userdata[$i].script" <<< "$spec" > "$TARGET$ud_file"
		chmod 700 "$TARGET$ud_file"
		log "userdata: $ud_name ($ud_file; its output in /var/log/ump-userdata.log)"
		if ! ud_out="$(in_target env UMP_USERDATA="$ud_name" UMP_HOSTNAME="$hostname" UMP_OS="$os" UMP_PROFILE="$profile_name" UMP_ADMIN="$admin" bash "$ud_file" 2>&1)"; then
			printf '%s\n' "$ud_out" >&2; die "userdata $ud_name failed"
		fi
		printf '=== userdata %s (%s) at %s ===\n%s\n' "$ud_name" "$ud_file" "$(date -u +%FT%TZ)" "$ud_out" >> "$TARGET/var/log/ump-userdata.log"
	done
	net_off
fi

# --- firewall -----------------------------------------------------------

if jq -e '.firewall.policy == "drop"' >/dev/null <<< "$spec"; then
	log "firewall: input drop, allowing $(jq -r '[.firewall.allow[] | if .proto == "icmp" then "icmp" else "\(.proto)/\(.port)" end] | join(", ")' <<< "$spec"); the same for connections forwarded in from outside (containers' published ports)"
	nft_ruleset > "$TARGET/etc/nftables.conf"
	chmod 755 "$TARGET/etc/nftables.conf"
	if ! out="$(in_target nft -c -f /etc/nftables.conf 2>&1)"; then
		printf '%s\n' "$out" >&2
		# a syntax error is the file's fault; anything else is the rescue
		# kernel's (the check needs its nf_tables)
		case "$out" in *"Error:"*) die "the firewall rules in /etc/nftables.conf are wrong (see above)" ;; esac
		warn "could not check /etc/nftables.conf here (see above); the new system loads it at boot"
	fi
	run_in_target "enabling nftables" systemctl enable nftables.service
fi

# --- the boot loader ---------------------------------------------------

if [ "$family" = arch ]; then
	# What UMP's Ubuntu and Debian images bring from their build
	# (images/base: overlay/ and finish.sh), done here: Arch's packages
	# set nothing up and enable nothing.
	mkdir -p "$TARGET/etc/ssh/sshd_config.d" "$TARGET/etc/systemd/resolved.conf.d" "$TARGET/etc/tuned"
	cat > "$TARGET/etc/ssh/sshd_config.d/50-ump.conf" <<'SSHD'
# UMP: no passwords over SSH, and no root login. The installer writes
# 10-ump-root.conf (which wins, sshd keeps the first value it reads) with
# "PermitRootLogin prohibit-password" when the profile lists root with
# keys. The administrator is the profile's sudo user, ump by default.
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
SSHD
	grep -q '^Include /etc/ssh/sshd_config.d/\*\.conf' "$TARGET/etc/ssh/sshd_config" \
		|| sed -i '1i Include /etc/ssh/sshd_config.d/*.conf' "$TARGET/etc/ssh/sshd_config"
	printf '# UMP: public resolvers, independent of the provider; the .network\n# file sets the same per interface. Replace both to use your own.\n[Resolve]\nDNS=%s\n' "$dns" \
		> "$TARGET/etc/systemd/resolved.conf.d/ump.conf"
	echo throughput-performance > "$TARGET/etc/tuned/active_profile"
	echo manual > "$TARGET/etc/tuned/profile_mode"
	# the boot menu: two seconds, no other systems searched
	sed -i -e 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=2/' -e '/^#*GRUB_DISABLE_OS_PROBER=/d' "$TARGET/etc/default/grub"
	echo 'GRUB_DISABLE_OS_PROBER=true' >> "$TARGET/etc/default/grub"
	in_target passwd -l root >/dev/null 2>&1 || true
	# network, resolver, time, SSH, tuned; the activity history of sar and
	# atop, where the packages have their units
	run_in_target "enabling the services" systemctl enable systemd-networkd.service systemd-resolved.service sshd.service systemd-timesyncd.service tuned.service
	for u in sysstat.service sysstat-collect.timer sysstat-summary.timer atop.service atop-rotate.timer; do
		[ -f "$TARGET/usr/lib/systemd/system/$u" ] || continue
		in_target systemctl enable "$u" >/dev/null 2>&1 || warn "could not enable $u"
	done
fi

log "installing the boot loader"
if [ ${#M_LEVEL[@]} -gt 0 ]; then
	mdadm_conf=/etc/mdadm/mdadm.conf; [ "$family" = arch ] && mdadm_conf=/etc/mdadm.conf
	mkdir -p "$TARGET$(dirname "$mdadm_conf")"
	mdadm --detail --scan >> "$TARGET$mdadm_conf"
fi
need_initramfs=0
[ ${#M_LEVEL[@]} -gt 0 ] && need_initramfs=1
for k in "${!L_VG[@]}"; do [ "${L_RAID[$k]}" != none ] && need_initramfs=1; done
if [ "$family" = arch ]; then
	# Arch's initramfs knows neither LVM nor md without its hooks, and the
	# root is always on a volume group
	hooks="lvm2"
	[ ${#M_LEVEL[@]} -gt 0 ] && hooks="mdadm_udev lvm2"
	sed -i -E "/^HOOKS=/ s/ filesystems/ $hooks filesystems/" "$TARGET/etc/mkinitcpio.conf"
	grep -q "^HOOKS=.* lvm2 filesystems" "$TARGET/etc/mkinitcpio.conf" || die "cannot add the lvm2 hook to /etc/mkinitcpio.conf"
	log "initramfs (mkinitcpio, hooks $hooks)"
	run_in_target "mkinitcpio" mkinitcpio -P
elif [ "$need_initramfs" = 1 ]; then
	log "initramfs (md or raid volumes)"
	run_in_target "update-initramfs" update-initramfs -u -k all
fi
case "$arch" in
amd64)
	for x in "${disks[@]}"; do
		for i in "${!P_DISK[@]}"; do
			[ "${P_DISK[$i]}" = "$x" ] && [ "${P_FS[$i]}" = bios ] || continue
			run_in_target "grub-install (BIOS, $x)" grub-install --target=i386-pc --recheck "$x"
			break
		done
	done
	[ "$has_efi" = 1 ] && run_in_target "grub-install (EFI)" grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
arm64)
	[ "$has_efi" = 1 ] && run_in_target "grub-install (EFI)" grub-install --target=arm64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
esac
if [ "$family" = arch ]; then
	run_in_target "grub-mkconfig" grub-mkconfig -o /boot/grub/grub.cfg
else
	run_in_target "update-grub" update-grub
fi
grep -q 'vmlinuz' "$TARGET/boot/grub/grub.cfg" || die "grub.cfg names no kernel"
}

# --- NixOS: UMP's module and the profile, installed with nixos-install ----

# a swap file on the new root while Nix evaluates and builds: evaluating
# a NixOS configuration takes about a gigabyte, which a small machine's
# rescue system (in RAM) does not have
swap_on_target() {
	local avail need=2048
	avail=$(( $(awk '/^MemAvailable:/ {print $2}' /proc/meminfo) / 1024 ))
	[ "$avail" -ge "$need" ] && return 0
	target_swap="$TARGET/.ump-install-swap"
	log "memory: $avail MB free; a swap file of $need MB on the new root while NixOS is built"
	dd if=/dev/zero of="$target_swap" bs=1M count="$need" status=none && chmod 600 "$target_swap" && mkswap "$target_swap" >/dev/null && swapon "$target_swap" \
		|| { rm -f "$target_swap"; target_swap=""; warn "no swap file; building may run out of memory"; }
}

install_nixos() {
	local channel nix_url nix_sum work nixdir nixbin p chan tools log_file ud_n i ud_name

	channel="$(jq -r .channel <<< "$manifest")"
	nix_url="$(jq -r .nix.url <<< "$manifest")"
	nix_sum="$(jq -r .nix.sha256 <<< "$manifest")"
	target_swap=""

	# Nix, from its release tarball, with the store on the new system's
	# disk from the first byte: /nix here is the new /nix
	log "downloading Nix from $nix_url"
	mkdir -p "$TARGET/nix" /nix
	mount --bind "$TARGET/nix" /nix
	work="$(mktemp -d)"
	curl -fsSL -o "$work/nix.tar.xz" "$nix_url" || die "cannot download $nix_url"
	[ "$(sha256sum "$work/nix.tar.xz" | cut -d' ' -f1)" = "$nix_sum" ] || die "checksum mismatch for $nix_url"
	tar -xJf "$work/nix.tar.xz" -C "$work"
	nixdir="$(ls -d "$work"/nix-*/ | head -n 1)"
	mkdir -p /nix/store
	cp -RPp "$nixdir"/store/* /nix/store/
	for p in "$nixdir"/store/*; do [ -x "$p/bin/nix-build" ] && nixbin="/nix/store/$(basename "$p")/bin"; done
	[ -n "${nixbin:-}" ] || die "no nix-build in $nix_url"
	# single-user Nix as root: no build users, no sandbox (the rescue system
	# has neither), and everything comes from the binary cache anyway
	export HOME=/root USER=root PATH="$nixbin:$PATH" NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
	export NIX_CONFIG=$'build-users-group =\nsandbox = false\nexperimental-features = nix-command\nfallback = true'
	"$nixbin/nix-store" --load-db < "$nixdir/.reginfo"
	rm -rf "$work"

	log "channel: $channel"
	nix-channel --add "$channel" nixos >/dev/null
	nix-channel --update nixos >/dev/null 2>&1 || die "nix-channel --update failed for $channel"
	chan=/nix/var/nix/profiles/per-user/root/channels/nixos
	[ -d "$chan" ] || chan="$(nix-instantiate --eval --expr '<nixos>' 2>/dev/null)"
	nixos_rev="$(cat "$chan/.git-revision" 2>/dev/null || true)"
	export NIX_PATH="nixpkgs=$chan:nixos=$chan:nixos-config=$TARGET/etc/nixos/configuration.nix"
	tools="$(nix-build '<nixpkgs>' -A nixos-install-tools --no-out-link 2>/dev/null)" || die "cannot get nixos-install from $channel"
	export PATH="$tools/bin:$PATH"

	log "configuration: /etc/nixos (hardware-configuration.nix, ump/, userdata/)"
	"$tools/bin/nixos-generate-config" --root "$TARGET" >/dev/null 2>&1 || die "nixos-generate-config failed"
	# only now: the hardware configuration must not take the swap file
	swap_on_target
	mkdir -p "$TARGET/etc/nixos/ump" "$TARGET/etc/nixos/userdata"
	curl -fsSL -o "$TARGET/etc/nixos/ump/default.nix" "$storage/install/nixos/ump.nix" || die "cannot fetch $storage/install/nixos/ump.nix"
	curl -fsSL -o "$TARGET/etc/nixos/ump/motd.sh" "$storage/install/motd.sh" || die "cannot fetch $storage/install/motd.sh"
	curl -fsSL -o "$TARGET/etc/nixos/ump/bashrc.sh" "$storage/install/bashrc.sh" || die "cannot fetch $storage/install/bashrc.sh"
	printf '%s\n' "$spec" | jq 'del(.userdata)' > "$TARGET/etc/nixos/ump/spec.json"
	nft_ruleset > "$TARGET/etc/nixos/ump/nftables.conf"
	{
		printf '%s\n' "${IF_MAC[@]}" | paste -d'\t' - <(printf '%s\n' "${IF_NAME[@]}") <(printf '%s\n' "${IF_KIND[@]}") \
			| jq -R 'split("\t") | {mac: .[0], name: .[1], kind: .[2]}' | jq -s .
	} > "$TARGET/etc/nixos/ump/interfaces.json"
	# (true at the end: the last disk need not have a bios part)
	bios_disks="$(for x in "${disks[@]}"; do for i in "${!P_DISK[@]}"; do [ "${P_DISK[$i]}" = "$x" ] && [ "${P_FS[$i]}" = bios ] && { echo "$x"; break; }; done; done; true)"
	lvm_raid=false; for k in "${!L_VG[@]}"; do [ "${L_RAID[$k]}" != none ] && lvm_raid=true; done
	jq -n --arg os "$os" --arg state_version "$(jq -r .state_version <<< "$manifest")" \
		--slurpfile interfaces "$TARGET/etc/nixos/ump/interfaces.json" --arg uplink_mac "$mac" \
		--arg ip4 "${ip4%%,*}" --arg gw4 "$(case "$ip4" in *,*) echo "${ip4#*,}" ;; esac)" \
		--arg ip6 "${ip6%%,*}" --arg gw6 "$(case "$ip6" in *,*) echo "${ip6#*,}" ;; esac)" \
		--arg dns "$dns" --arg cmdline "$kernel_cmdline" --argjson rename "$([ "$rename" = yes ] && echo true || echo false)" \
		--argjson efi "$([ "$has_efi" = 1 ] && echo true || echo false)" --arg bios "$bios_disks" \
		--argjson md "$([ ${#M_LEVEL[@]} -gt 0 ] && echo true || echo false)" --arg mdadm_conf "$( [ ${#M_LEVEL[@]} -gt 0 ] && mdadm --detail --scan || true)" \
		--arg swaps "$(for sw in "${SWAPS[@]:-}"; do [ -n "$sw" ] && blkid -s UUID -o value "$sw"; done)" \
		--argjson lvm_raid "$lvm_raid" '{
		os: $os, state_version: $state_version, rename: $rename, interfaces: $interfaces[0], uplink_mac: $uplink_mac,
		ip4: $ip4, gw4: $gw4, ip6: $ip6, gw6: $gw6, dns: ($dns | split(" ")),
		kernel_params: ($cmdline | split(" ") | map(select(. != ""))),
		efi: $efi, bios_disks: ($bios | split("\n") | map(select(. != ""))),
		md: $md, mdadm_conf: $mdadm_conf, lvm_raid: $lvm_raid,
		swaps: ($swaps | split("\n") | map(select(. != "")))}' > "$TARGET/etc/nixos/ump/facts.json"
	rm -f "$TARGET/etc/nixos/ump/interfaces.json"

	ud_n=$(jq -r '.userdata | length' <<< "$spec")
	for ((i = 0; i < ud_n; i++)); do
		ud_name="$(jq -r ".userdata[$i].name" <<< "$spec")"
		jq -r ".userdata[$i].script" <<< "$spec" > "$TARGET/etc/nixos/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.nix"
		log "userdata: $ud_name (/etc/nixos/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.nix)"
	done
	cat > "$TARGET/etc/nixos/configuration.nix" <<'NIX'
# Written by ump server reinstall. The machine is ./ump (what UMP set up
# from the profile: users and keys, network, firewall, boot loader) plus
# the userdata modules in ./userdata, in the order of their names; NixOS
# merges them, and the userdata wins over UMP's defaults. Change the
# machine here or in a userdata module, then nixos-rebuild switch.
{ lib, ... }:
{
  imports = [ ./hardware-configuration.nix ./ump ]
    ++ map (f: ./userdata + "/${f}") (lib.attrNames (lib.filterAttrs (n: t: t == "regular" && lib.hasSuffix ".nix" n) (builtins.readDir ./userdata)));
}
NIX

	log "nixos-install (downloads from the binary cache; this takes a few minutes)"
	log_file="$TARGET/var/log/ump-nixos-install.log"
	mkdir -p "$(dirname "$log_file")"
	# once more on a failure: a transient one (a download, a bus error seen
	# once on an OVHcloud VPS) passes the second time, and nixos-install
	# carries on with what the store already has
	if ! "$tools/bin/nixos-install" --root "$TARGET" --no-root-passwd > "$log_file" 2>&1; then
		tail -n 3 "$log_file" >&2
		warn "nixos-install failed (above); trying once more"
		if ! "$tools/bin/nixos-install" --root "$TARGET" --no-root-passwd >> "$log_file" 2>&1; then
			tail -n 40 "$log_file" >&2
			die "nixos-install failed twice (the whole output is in /var/log/ump-nixos-install.log of the new system)"
		fi
	fi
	image_version="$(jq -r .version <<< "$manifest")${nixos_rev:+ (nixpkgs ${nixos_rev:0:12})}"
	image_kernel="$(basename "$(dirname "$(readlink -f "$TARGET/nix/var/nix/profiles/system/kernel")")" | sed 's/^[a-z0-9]*-linux-//')"
	[ -n "$target_swap" ] && { swapoff "$target_swap" 2>/dev/null; rm -f "$target_swap"; }
	umount /nix 2>/dev/null || umount -l /nix
}

case "$family" in
image|arch) install_image ;;
nixos) install_nixos ;;
esac

mkdir -p "$TARGET/etc/ump"
printf '%s\n' "$spec" | jq . > "$TARGET/etc/ump/spec.json"
chmod 600 "$TARGET/etc/ump/spec.json"
cat > "$TARGET/etc/ump/install" <<META
os=$os
image_version=$image_version
image_kernel=$image_kernel
installed=$(date -u +%Y-%m-%dT%H:%M:%SZ)
profile=$profile_name
layout=$layout_name
disks=${disks[*]}
boot_disk=$boot_disk
root=$root_dev
hostname=$hostname
admin=$admin
interfaces=$(for i in "${!IF_MAC[@]}"; do printf '%s=%s ' "${IF_NAME[$i]}" "${IF_MAC[$i]}"; done | sed 's/ $//')
uplink=$uplink
ipv6=$([ "$ipv6" = true ] && echo on || echo off)
mitigations=$([ "$mitigations" = true ] && echo on || echo off)
firewall=$(jq -r '.firewall.policy' <<< "$spec")
userdata=$(jq -r '.userdata | map(.name) | join(",")' <<< "$spec")
private_network=$private_network
private_address=$private_address
META
chmod 644 "$TARGET/etc/ump/install"

sync
cleanup
trap - EXIT
vgchange -an >/dev/null 2>&1 || true
log "installed: $os ${image_version:-?} (kernel ${image_kernel:-?}) as $hostname, root on $root_dev ($root_mb MB; layout $layout_name over ${#disks[@]} disk(s)); reboot to start it"
