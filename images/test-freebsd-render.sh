#!/bin/sh
# Check what install/freebsd/install.sh plans and writes, without a
# machine: the functions of its renderers section run for plans, disks,
# uplinks and firewalls of this script's own, and what they give is
# compared with testdata/freebsd-render/CASE:
#
#   plan-*     a plan.json of the Linux rescue system and the disks of a
#              FreeBSD machine: which disk is which (serial number, size,
#              order), the plan as it is printed, the commands that
#              partition the disks and make the pool, /etc/fstab and the
#              record's disks; the refusals (a disk that is not there, one
#              that cannot be told from a disk the plan leaves alone)
#   layout-*   the same without a plan.json: the published ZFS layouts
#              planned on the FreeBSD side, with first and .*
#   net-*      /etc/rc.conf, /etc/resolv.conf and /etc/resolvconf.conf for
#              the uplinks of images/test-network.sh: what the rescue
#              system found, and a specification's network.uplink
#   pf-*       /etc/pf.conf for firewalls
#   loader-*   /boot/loader.conf
#   record-*   /etc/ump/install
#   files      the files that are the same on every machine
#
# Usage: test-freebsd-render.sh [CASE...]   (default: every case)
# Needs sh (dash, bash or FreeBSD's) and jq. VERBOSE=1 names every case;
# UPDATE=1 writes the expected files instead of comparing (review the
# diff).
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
EXPECTED="$HERE/testdata/freebsd-render"
command -v jq >/dev/null || { echo "test-freebsd-render.sh: jq is needed" >&2; exit 2; }
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-freebsd-render-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# what the section calls of the rest of install.sh
die() { echo "install.sh: $*"; exit 1; }
warn() { echo "install.sh: warning: $*"; }
section="$(awk '/^# --- the renderers: /{on = 1; next} on && /^# --- /{exit} on' "$ROOT/install/freebsd/install.sh")"
[ -n "$section" ] || { echo "test-freebsd-render.sh: no renderers section in install/freebsd/install.sh" >&2; exit 2; }
eval "$section"

# --- the machines and the specifications --------------------------------

# machine NAME: the disks of a FreeBSD machine, as install.sh lists them
# (NAME SECTOR SIZE SERIAL), in the order the kernel happens to give
machine() {
	case "$1" in
	virtio1) echo "vtbd0 512 10737418240" ;;
	virtio2) printf '%s\n' "vtbd1 512 10737418240" "vtbd0 512 10737418240" ;;
	virtio3) printf '%s\n' "vtbd2 512 10737418240" "vtbd0 512 10737418240" "vtbd1 512 10737418240" ;;
	# a Linode: the system's disk and its small second one
	linode) printf '%s\n' "da0 512 26306674688 QM00001" "da1 512 536870912 QM00002" ;;
	# the small disk first
	small-first) printf '%s\n' "vtbd0 512 536870912" "vtbd1 512 26306674688" ;;
	nvme1) echo "nda0 512 512110190592 S4EWNX0N123456" ;;
	# two NVMe disks and a hard disk, which the kernel lists first
	nvme2-hdd) printf '%s\n' "ada0 512 4000787030016 ZC1A2B3C" "nda1 512 512110190592 S4EWNX0N123457" "nda0 512 512110190592 S4EWNX0N123456" ;;
	# the same behind a controller that shows no serial numbers
	nvme2-hdd-anonymous) printf '%s\n' "da0 512 4000787030016" "da1 512 512110190592" "da2 512 512110190592" ;;
	sata3) printf '%s\n' "ada0 512 4000787030016 ZC1A0001" "ada1 512 4000787030016 ZC1A0002" "ada2 512 4000787030016 ZC1A0003" ;;
	sata12) for i in 0 1 2 3 4 5 6 7 8 9 10 11; do echo "da$i 4096 8001563222016 WD-$i"; done ;;
	tiny) echo "vtbd0 512 3221225472" ;;
	none) ;;
	esac | machine_disks
}

# plan_json FIRMWARE VDEV: a plan.json as install.sh of the Linux rescue
# system writes it, from lines "NAME SIZE SERIAL PARTS" on stdin; PARTS
# is all (bios, efi, swap and the pool's partition), zfs (the pool's
# alone), boot (bios, efi and swap alone) or left (a disk the plan
# leaves alone); a serial number - is none. The first disk is the boot
# disk.
plan_json() {
	jq -Rn --arg firmware "$1" --arg vdev "$2" '
	def parts: {
		"all": [{index: 1, kind: "bios", size: "512K"}, {index: 2, kind: "efi", size: "512M"}, {index: 3, kind: "swap", size: "2G"}, {index: 4, kind: "zfs", size: "rest"}],
		"zfs": [{index: 1, kind: "zfs", size: "rest"}],
		"boot": [{index: 1, kind: "bios", size: "512K"}, {index: 2, kind: "efi", size: "512M"}, {index: 3, kind: "swap", size: "2G"}],
		"noefi": [{index: 1, kind: "bios", size: "512K"}, {index: 2, kind: "swap", size: "2G"}, {index: 3, kind: "zfs", size: "rest"}],
		"nobios": [{index: 1, kind: "efi", size: "512M"}, {index: 2, kind: "swap", size: "2G"}, {index: 3, kind: "zfs", size: "rest"}],
		"percent": [{index: 1, kind: "bios", size: "1M"}, {index: 2, kind: "efi", size: "1G"}, {index: 3, kind: "swap", size: "10%"}, {index: 5, kind: "zfs", size: "100%"}]
	}[.];
	[inputs | select(length > 0) | split(" ") | {name: .[0], size: (.[1] | tonumber), serial: (if .[2] == "-" then "" else .[2] end), wwn: "", what: .[3]}] as $d
	| {version: 1, firmware: $firmware, boot_disk: $d[0].name,
		disks: [$d[] | select(.what != "left") | {name, size, serial, wwn, parts: (.what | parts)}],
		left: [$d[] | select(.what == "left") | {name, size, serial}],
		pool: {name: "zroot", vdev: $vdev, mountpoint: "/"}}'
}

# layout NAME: a layout of layouts.yaml as the specification has it (a
# zfs part with an empty filesystem, as ump renders it)
layout() {
	case "$1" in
	zfs) echo '{"name": "zfs", "parts": [
		{"type": "slice", "target": ["first"], "size": "512K", "filesystem": "bios"},
		{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "slice", "target": ["first"], "size": "2G", "filesystem": "swap"},
		{"type": "zfs", "target": [".*"], "size": "rest", "filesystem": "", "mountpoint": "/", "name": "zroot"}]}' ;;
	zfs-mirror|zfs-raidz|zfs-raidz2|zfs-raid10)
		# (the raid level of each; 10 is none a pool has)
		case "$1" in zfs-mirror) raid=1 ;; zfs-raidz) raid=5 ;; zfs-raidz2) raid=6 ;; *) raid=10 ;; esac
		echo '{"name": "'"$1"'", "parts": [
		{"type": "slice", "target": [".*"], "size": "512K", "filesystem": "bios"},
		{"type": "slice", "target": [".*"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "slice", "target": [".*"], "size": "2G", "filesystem": "swap"},
		{"type": "zfs", "target": [".*"], "size": "rest", "filesystem": "", "mountpoint": "/", "name": "zroot", "raid": '"$raid"'}]}' ;;
	lvm-linear) echo '{"name": "lvm-linear", "parts": [
		{"type": "slice", "target": ["first"], "size": "1M", "filesystem": "bios"},
		{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "slice", "target": ["first"], "size": "1G", "filesystem": "ext4", "mountpoint": "/boot"},
		{"type": "slice", "target": [".*"], "size": "rest", "filesystem": "lvm", "vg": "vg0"},
		{"type": "lvm", "vg": "vg0", "name": "root", "size": "rest", "filesystem": "ext4", "mountpoint": "/"}]}' ;;
	nvme-only) echo '{"name": "nvme-only", "parts": [
		{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "zfs", "target": ["nvme.*"], "size": "rest", "mountpoint": "/"}]}' ;;
	data-pool) echo '{"name": "data-pool", "parts": [
		{"type": "slice", "target": ["first"], "size": "512M", "filesystem": "efi", "mountpoint": "/boot/efi"},
		{"type": "zfs", "target": ["first"], "size": "20G", "mountpoint": "/"},
		{"type": "zfs", "target": [".*"], "size": "rest", "name": "tank", "mountpoint": "/data"}]}' ;;
	esac
}

# the values every case starts from
defaults() {
	arch=amd64; firmware=uefi; os=freebsd-15.1; hostname=web-01.example.net; timezone=UTC
	layout_name=zfs; profile_name=freebsd-15.1; mitigations=false; ipv6=false; kernel_extra=""
	firewall=drop; admin=ump; privates='[]'; rename=yes; uplink_spec=""
	spec='{"firewall": {"policy": "drop", "allow": [{"proto": "icmp"}, {"proto": "tcp", "port": "22"}]}, "userdata": []}'
	# two wired interfaces; the uplink is the second, and its MAC the one
	# the rescue system was given
	mac=0c:c4:7a:aa:bb:11
	ifaces="$(printf '%s\n' "igb0 0c:c4:7a:aa:bb:10 int" "igb1 0c:c4:7a:aa:bb:11 ext" | name_interfaces)"
	uplink=ext01
	ip4=""; ip6=""; net_from_machine
}

# --- the cases ---------------------------------------------------------------

# show_plan PLAN: the plan and what follows from it, or why it is refused
show_plan() {
	local refused
	refused="$(printf '%s' "$1" | jq -r '(.refused // [])[]')"
	[ -z "$refused" ] || die "$refused"
	echo "==> the plan"
	plan_lines "$1"
	refused="$(plan_check "$1" "${unpacked:-0}")"
	[ -z "$refused" ] || die "$refused"
	[ "$brief" = 0 ] || return 0
	echo "==> the disks"
	disk_commands "$1"
	echo "==> the pool"
	pool_commands "$1"
	echo "==> /etc/fstab"
	fstab_text "$1"
	echo "==> /etc/ump/install (the disks)"
	record_text "$1" 15.1-RELEASE 15.1-RELEASE 2026-11-26T14:46:59Z | grep -E '^(layout|disks|boot_disk|root)='
}
# plan_case NAME FIRMWARE ARCH MACHINE VDEV DISK...: a plan.json (each
# DISK "NAME SIZE SERIAL PARTS") against the disks of MACHINE
plan_case() {
	local name="$1" disks plan
	defaults; firmware="$2"; arch="$3"; disks="$(machine "$4")"; layout_name="$name"
	shift 4
	plan="$(vdev="$1"; shift; printf '%s\n' "$@" | plan_json "$firmware" "$vdev")"
	[ -z "${plan_edit:-}" ] || plan="$(printf '%s' "$plan" | jq -c "$plan_edit")"
	show_plan "$(plan_from_file "$plan" "$disks")"
}
# twelve disks behind one controller, which Linux and FreeBSD count alike
plan_twelve() {
	local name="$1" n=0 l
	set -- "$name" uefi amd64 sata12 raidz2
	for l in a b c d e f g h i j k l; do set -- "$@" "/dev/sd$l 8001563222016 WD-$n all"; n=$((n + 1)); done
	brief=1; plan_case "$@"
}
# layout_case NAME FIRMWARE ARCH MACHINE LAYOUT [DISKS_JSON]: no
# plan.json; the layout planned on the FreeBSD side
layout_case() {
	defaults; firmware="$2"; arch="$3"; layout_name="$5"
	spec="$(layout "$5" | jq -c --argjson disks "${6:-[]}" '{layout: ., disks: $disks}')"
	show_plan "$(plan_from_layout "$(machine "$4")")"
}
# net_case NAME RENAME IPV6 IP4 IP6: the uplink as the rescue system has
# it; uplink_case NAME RENAME IPV6 UPLINK: the specification's (JSON)
show_net() {
	echo "==> /etc/rc.conf"
	rc_conf
	echo "==> /etc/resolv.conf"
	resolv_conf
	echo "==> /etc/resolvconf.conf"
	resolvconf_conf
	[ -z "$uplink_spec" ] || { echo "==> /etc/ump/install (the network)"; net_record; }
	echo "==> the plan"
	net_plan
}
net_names() {
	rename="$1"
	ifaces="$(printf '%s\n' "igb0 0c:c4:7a:aa:bb:10" "igb1 0c:c4:7a:aa:bb:11" | awk -v mac="$mac" '{print $1, $2, ($2 == mac ? "ext" : "int")}' | name_interfaces)"
	uplink="$(printf '%s\n' "$ifaces" | awk -v mac="$mac" '$2 == mac {print $1}')"
}
net_case() { defaults; ipv6="$3"; net_names "$2"; ip4="$4"; ip6="$5"; net_from_machine; show_net; }
uplink_case() { defaults; ipv6="$3"; ip4=""; ip6=""; uplink_spec="$4"; net_from_spec "$4"; net_names "$2"; show_net; }
# pf_case NAME FIREWALL_JSON: /etc/pf.conf, and the plan's word for it
pf_case() {
	defaults; spec="$(jq -nc --argjson firewall "$2" '{firewall: $firewall}')"
	firewall="$(printf '%s' "$spec" | jq -r .firewall.policy)"
	echo "==> the plan"
	firewall_plan
	if [ "$firewall" = drop ]; then echo "==> /etc/pf.conf"; pf_conf; fi
	echo "==> /etc/rc.conf"
	rc_conf | grep '^pf' || echo "(nothing of pf)"
}
# loader_case NAME ARCH MITIGATIONS EXTRA
loader_case() {
	defaults; arch="$2"; mitigations="$3"; kernel_extra="$4"
	echo "==> /boot/loader.conf"
	loader_conf
	echo "==> the plan"
	echo "loader tunables: $(loader_tunables | paste -sd ' ' -)"
	echo "left out: $(loader_ignored | paste -sd ' ' -)"
}
record_case() {
	local plan
	defaults
	spec='{"userdata": [{"name": "freebsd"}, {"name": "web"}]}'
	privates='[{"network": "default", "cidr": "10.0.0.0/24", "address": "10.0.0.7"}, {"network": "db", "cidr": "10.0.1.0/24", "address": "10.0.1.7"}]'
	layout_name=zfs-mirror; ip4="203.0.113.10/26,203.0.113.1"; net_from_machine
	[ -z "${2:-}" ] || { uplink_spec="$2"; net_from_spec "$2"; }
	plan="$(printf '%s\n' "/dev/nvme0n1 512110190592 S4EWNX0N123456 all" "/dev/nvme1n1 512110190592 S4EWNX0N123457 all" | plan_json uefi mirror)"
	record_text "$(plan_from_file "$plan" "$(machine nvme2-hdd)")" 15.1-RELEASE 15.1-RELEASE 2026-11-26T14:46:59Z
}
files_case() {
	echo "==> /etc/ssh/sshd_config.d/50-ump.conf"
	sshd_conf
	echo "==> /usr/local/etc/rc.d/ump_ifnames"
	ifnames_script
	ifnames_script > "$WORK/ump_ifnames"
	sh -n "$WORK/ump_ifnames"
	echo "==> /etc/hosts"
	defaults; hosts_text
	hostname=web-01; hosts_text
}

G10=10737418240; NVME=512110190592; HDD=4000787030016
unpacked=0; plan_edit=""; brief=0
cases() {
	# a plan.json and the machine's disks
	run plan-one-disk plan_case uefi arm64 virtio1 stripe "/dev/vda $G10 - all"
	run plan-one-disk-bios plan_case bios amd64 virtio1 stripe "/dev/vda $G10 - all"
	run plan-mirror plan_case uefi amd64 virtio2 mirror "/dev/vda $G10 - all" "/dev/vdb $G10 - all"
	run plan-raidz plan_case bios amd64 sata3 raidz "/dev/sda $HDD ZC1A0001 all" "/dev/sdb $HDD ZC1A0002 all" "/dev/sdc $HDD ZC1A0003 all"
	run plan-raidz-no-serials plan_case uefi arm64 virtio3 raidz "/dev/vda $G10 - all" "/dev/vdb $G10 - all" "/dev/vdc $G10 - all"
	run plan-stripe-two-disks plan_case uefi amd64 linode stripe "/dev/sda 26306674688 QM00001 all" "/dev/sdb 536870912 QM00002 zfs"
	run plan-nvme-mirror-hdd-left plan_case uefi amd64 nvme2-hdd mirror "/dev/nvme0n1 $NVME S4EWNX0N123456 all" "/dev/nvme1n1 $NVME S4EWNX0N123457 all" "/dev/sda $HDD ZC1A2B3C left"
	run plan-serials-in-another-order plan_case uefi amd64 nvme2-hdd stripe "/dev/nvme0n1 $NVME S4EWNX0N123457 all" "/dev/nvme1n1 $NVME S4EWNX0N123456 zfs" "/dev/sda $HDD ZC1A2B3C left"
	run plan-no-serials-here plan_case uefi amd64 nvme2-hdd-anonymous mirror "/dev/nvme0n1 $NVME S4EWNX0N123456 all" "/dev/nvme1n1 $NVME S4EWNX0N123457 all" "/dev/sda $HDD ZC1A2B3C left"
	run plan-disk-outside-the-plan plan_case uefi amd64 nvme2-hdd mirror "/dev/nvme0n1 $NVME S4EWNX0N123456 all" "/dev/nvme1n1 $NVME S4EWNX0N123457 all"
	run plan-raidz2-twelve plan_twelve
	run plan-percent plan_case uefi amd64 nvme1 stripe "/dev/nvme0n1 $NVME S4EWNX0N123456 percent"
	# refused
	run plan-refused-disk-not-here plan_case uefi amd64 nvme1 mirror "/dev/nvme0n1 $NVME S4EWNX0N123456 all" "/dev/nvme1n1 $NVME S4EWNX0N123457 all"
	run plan-refused-loop-device plan_case uefi arm64 virtio1 stripe "/dev/loop0 2147483648 - all"
	run plan-refused-left-alike plan_case uefi arm64 virtio2 stripe "/dev/vda $G10 - all" "/dev/vdb $G10 - left"
	run plan-refused-left-alike-not-seen plan_case uefi arm64 virtio1 stripe "/dev/sda $G10 SERIAL-A all" "/dev/sdb $G10 SERIAL-B left"
	run plan-refused-outside-alike plan_case uefi arm64 virtio3 mirror "/dev/vda $G10 - all" "/dev/vdb $G10 - all"
	run plan-refused-mirror-one-disk plan_case uefi arm64 virtio1 mirror "/dev/vda $G10 - all"
	run plan-refused-raidz2-three plan_case uefi amd64 sata3 raidz2 "/dev/sda $HDD ZC1A0001 all" "/dev/sdb $HDD ZC1A0002 all" "/dev/sdc $HDD ZC1A0003 all"
	run plan-refused-no-efi plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - noefi"
	run plan-refused-no-bios plan_case bios amd64 virtio1 stripe "/dev/vda $G10 - nobios"
	run plan-refused-no-pool plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - boot"
	unpacked=747
	run plan-refused-too-small plan_case uefi amd64 tiny stripe "/dev/vda 3221225472 - all"
	unpacked=0
	plan_edit='.version = 2'
	run plan-refused-version plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - all"
	plan_edit='.pool.vdev = "raid5"'
	run plan-refused-vdev plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - all"
	plan_edit='.disks[0].parts[2] = {index: 3, kind: "ext4", size: "20G"}'
	run plan-refused-kind plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - all"
	plan_edit='del(.disks[0].size)'
	run plan-refused-no-size plan_case uefi amd64 virtio1 stripe "/dev/vda $G10 - all"
	plan_edit=""
	# without a plan.json: the layout planned here
	run layout-zfs layout_case uefi arm64 virtio1 zfs
	run layout-zfs-two-disks layout_case uefi amd64 linode zfs
	run layout-zfs-small-disk-first layout_case bios amd64 small-first zfs
	run layout-zfs-mirror layout_case uefi amd64 virtio2 zfs-mirror
	run layout-zfs-mirror-nvme-first layout_case uefi amd64 nvme2-hdd zfs-mirror '["nda0", "/dev/nda1"]'
	run layout-zfs-raidz layout_case bios amd64 virtio3 zfs-raidz
	run layout-zfs-every-disk layout_case uefi amd64 nvme2-hdd zfs
	run layout-refused-mirror-one-disk layout_case uefi arm64 virtio1 zfs-mirror
	run layout-refused-raidz2-three layout_case uefi amd64 virtio3 zfs-raidz2
	run layout-refused-raid layout_case uefi amd64 virtio3 zfs-raid10
	run layout-refused-lvm layout_case uefi amd64 virtio1 lvm-linear
	run layout-refused-target layout_case uefi amd64 nvme1 nvme-only
	run layout-refused-second-pool layout_case uefi amd64 nvme1 data-pool
	run layout-refused-linux-disk-names layout_case uefi amd64 nvme2-hdd zfs-mirror '["/dev/nvme0n1", "/dev/nvme1n1"]'
	run layout-refused-no-disk layout_case uefi amd64 none zfs
	# the uplink as the rescue system has it
	run net-static net_case yes false "203.0.113.10/26,203.0.113.1" "2001:db8::2/64,fe80::1"
	run net-static-ipv6 net_case yes true "203.0.113.10/26,203.0.113.1" "2001:db8::2/64,fe80::1"
	run net-static-v4 net_case yes false "203.0.113.10/26,203.0.113.1" ""
	run net-static-nogateway net_case yes true "203.0.113.10/26" "2001:db8::2/64"
	run net-static-on-link net_case yes false "203.0.113.10/32,172.31.1.1" ""
	run net-static-ipv6-gateway-global net_case yes true "203.0.113.10/32,172.31.1.1" "2001:db8:1::2/128,2001:db8:ff::1"
	run net-dhcp net_case yes false "" ""
	run net-dhcp-ipv6 net_case yes true "" ""
	run net-kernel-names net_case no false "203.0.113.10/26,203.0.113.1" ""
	run net-kernel-names-dhcp net_case no true "" ""
	# the uplink of the specification
	run net-uplink-static uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24", "gateway": "192.0.2.1"}, "ipv6": {"mode": "none"}, "dns": ["192.0.2.53", "192.0.2.54"]}'
	run net-uplink-static-public-dns uplink_case no false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24"}, "ipv6": {"mode": "none"}}'
	run net-uplink-static-ipv6 uplink_case yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24", "gateway": "192.0.2.1"}, "ipv6": {"mode": "static", "address": "2001:db8::17/64", "gateway": "fe80::1"}, "dns": ["192.0.2.53", "2001:db8::53"]}'
	run net-uplink-dhcp uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}, "dns_from_dhcp": true}'
	run net-uplink-dhcp-dns uplink_case yes false '{"mac": "0C-C4-7A-AA-BB-11", "ipv4": {"mode": "dhcp"}, "dns": ["192.0.2.53"]}'
	run net-uplink-dhcp-dns-kernel-names uplink_case no true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns": ["192.0.2.53", "2001:db8::53"]}'
	run net-uplink-dhcp-public-dns uplink_case no false '{"ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}}'
	run net-uplink-dhcp-auto uplink_case yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	run net-uplink-dhcp-auto-ipv6-off uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	run net-uplink-none-static6 uplink_case yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "static", "address": "2001:db8::17/64", "gateway": "fe80::1"}, "dns": ["2001:db8::53"]}'
	run net-uplink-none-auto uplink_case yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "auto"}, "dns_from_dhcp": true}'
	run net-uplink-other-interface uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:10", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}, "dns_from_dhcp": true}'
	run net-refused-none uplink_case yes true '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "none"}}'
	run net-refused-none-ipv6-off uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "none"}, "ipv6": {"mode": "auto"}}'
	run net-refused-mode uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "auto"}}'
	run net-refused-prefix uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17"}}'
	run net-refused-lease uplink_case yes false '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "static", "address": "192.0.2.17/24"}, "dns_from_dhcp": true}'
	run net-refused-mac uplink_case yes false '{"mac": "0c:c4:7a:aa:bb", "ipv4": {"mode": "dhcp"}}'
	# the firewalls of firewalls.yaml, the one of docs/firewalls.md, sources
	run pf-default pf_case '{"name": "default", "policy": "drop", "allow": [{"proto": "icmp"}, {"proto": "tcp", "port": "22"}]}'
	run pf-web pf_case '{"name": "web", "policy": "drop", "allow": [{"proto": "icmp"}, {"proto": "tcp", "port": "22"}, {"proto": "tcp", "port": "80"}, {"proto": "tcp", "port": "443"}]}'
	run pf-open pf_case '{"name": "open", "policy": "accept", "allow": []}'
	run pf-ranges-and-sources pf_case '{"policy": "drop", "allow": [{"proto": "icmp"}, {"proto": "tcp", "port": "22"}, {"proto": "tcp", "port": "8000-8100"}, {"proto": "tcp", "port": "9100", "from": "10.0.0.0/8"}, {"proto": "udp", "port": "53", "from": "2001:db8::/32"}, {"proto": "udp", "port": "60000-61000"}]}'
	run pf-icmp-from pf_case '{"policy": "drop", "allow": [{"proto": "icmp", "from": "192.0.2.0/24"}, {"proto": "icmp", "from": "2001:db8::/32"}, {"proto": "tcp", "port": "22", "from": "192.0.2.7"}]}'
	run pf-nothing-allowed pf_case '{"policy": "drop"}'
	# /boot/loader.conf
	run loader-amd64 loader_case amd64 false ""
	run loader-amd64-mitigations loader_case amd64 true ""
	run loader-arm64 loader_case arm64 false ""
	run loader-extra loader_case amd64 true 'vfs.zfs.arc.max=4294967296 hw.vga.textmode="1" quiet kern.maxfiles=200000'
	# /etc/ump/install
	run record record_case
	run record-uplink record_case '{"mac": "0c:c4:7a:aa:bb:11", "ipv4": {"mode": "dhcp"}, "ipv6": {"mode": "none"}, "dns": ["192.0.2.53"]}'
	run files files_case
}

failed=0 n=0
only=" $* "
run() { # run NAME FUNCTION ARGS...: one case, unless the command line names others
	local name="$1" function="$2" out rc
	if [ "$only" != "  " ]; then case "$only" in *" $name "*) ;; *) return 0 ;; esac; fi
	n=$((n + 1))
	out="$WORK/$name.out"
	shift 2
	# errexit as in install.sh: a command that fails there fails here
	set +e
	( set -e; "$function" "$name" "$@" ) > "$out" 2>&1
	rc=$?
	set -e
	[ "$rc" = 0 ] || echo "(exit status $rc)" >> "$out"
	if [ "${UPDATE:-0}" = 1 ]; then
		cp "$out" "$EXPECTED/$name"
		echo "wrote   $name"
	elif [ ! -f "$EXPECTED/$name" ]; then
		echo "MISSING $name (UPDATE=1 writes it)"; failed=$((failed + 1))
	elif diff -u "$EXPECTED/$name" "$out" > "$WORK/$name.diff"; then
		[ "${VERBOSE:-0}" = 0 ] || echo "ok      $name"
	else
		echo "FAIL    $name"; sed 's/^/        /' "$WORK/$name.diff"; failed=$((failed + 1))
	fi
}
[ "${UPDATE:-0}" != 1 ] || mkdir -p "$EXPECTED"
cases
[ "$n" -gt 0 ] || { echo "test-freebsd-render.sh: no such case: $*" >&2; exit 2; }
if [ "$failed" -gt 0 ]; then echo "$failed of $n cases failed"; exit 1; fi
echo "test-freebsd-render.sh: all $n cases as expected"
