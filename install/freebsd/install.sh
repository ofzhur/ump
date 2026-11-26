#!/bin/sh
# UMP installer for FreeBSD. Runs as root in the UMP FreeBSD rescue system,
# whose root is in memory and where no disk is in use, and puts FreeBSD on
# the machine's disks the way a specification says, with its root on ZFS:
# it partitions the disks, makes the pool and its datasets, unpacks the
# release's distribution sets, configures the machine, installs the boot
# loader on every disk, adds the packages, the users, the userdata and
# the firewall, and leaves the machine ready to reboot. The counterpart
# of install/install.sh, with FreeBSD's own tools (gpart, zpool, zfs,
# tar, pkg, pw, pf) and the same specification. See ../README.md.
#
# Usage: install.sh --spec FILE [--record DIR] [--dry-run] [--force]
#
#   --spec FILE      the specification (JSON), as ump renders it: os,
#                    hostname, timezone, storage, kernel (mitigations: false
#                    turns page table isolation off on amd64; ipv6: false
#                    gives no interface an IPv6 address; extra: loader
#                    tunables, NAME=VALUE), layout, disks, users, userdata
#                    (bash), firewall (written as /etc/pf.conf), network
#                    (rename: false keeps the kernel's interface names;
#                    private_networks, recorded in /etc/ump/install; uplink:
#                    the uplink's interface by MAC, its IPv4 and IPv6 and
#                    its resolvers, where the provider knows them, else what
#                    the rescue system has), profile
#   --record DIR     copy /etc/ump/install and the SSH host keys (public) of
#                    the new system into DIR at the end
#   --dry-run        print the plan and stop before touching a disk
#   --force          run outside the UMP FreeBSD rescue system too (it erases
#                    every disk of its plan), and install a release other
#                    than the rescue system's own
#
# The disks and their partitions are those of the plan the Linux rescue
# system made (plan.json of the boot config, /etc/ump/boot): its disks
# are found again here by serial number, else by size, and a disk that
# is not here, or cannot be told from one the plan leaves alone, refuses
# the installation. Without a plan.json the installer plans itself from
# the specification's layout, with first and .* as the only targets;
# disks of the specification then names FreeBSD's disks (da0). UMP_BOOT_CONFIG
# names another directory than /etc/ump/boot (cmdline, plan.json).
#
# Exit status: 0 done, the machine is ready to reboot; 2 refused (usage,
# the environment, the specification, the layout, the disks not those of
# the plan, the image), with the line "install.sh: nothing was written to
# the disks"; 1 a failed step after the first disk was cleared, the
# reason on stderr, the machine left in the rescue system.
set -eu
# the new system's files as a root shell over SSH makes them, whatever
# umask the caller has
umask 022

log() { printf '==> %s\n' "$*"; }
warn() { echo "install.sh: warning: $*" >&2; }
# (a failure before the first write is a refusal: finish says so and
# makes the status 2)
die() { said=1; echo "install.sh: $*" >&2; exit 1; }
wrote=0; said=0; step="starting"; pool_made=""
TARGET=/mnt
ESP_MOUNT=/tmp/ump-esp
cleanup() {
	umount -f "$TARGET/dev" 2>/dev/null || true
	umount -f "$ESP_MOUNT" 2>/dev/null || true
	[ -z "$pool_made" ] || zpool export -f "$pool_made" 2>/dev/null || true
	rm -f /tmp/ump-sum.$$ /tmp/ump-sum.$$.fifo
}
finish() {
	rc=$?
	trap - EXIT
	[ "$rc" != 0 ] || return 0
	[ "$said" = 1 ] || echo "install.sh: failed while $step (exit status $rc)" >&2
	if [ "$wrote" = 0 ]; then
		echo "install.sh: nothing was written to the disks" >&2
		exit 2
	fi
	cleanup
	echo "install.sh: the installation is not complete; the machine stays in the rescue system, and a reboot finds no system on its disks" >&2
	exit 1
}
trap finish EXIT
trap 'exit 1' INT TERM HUP

spec_file=""; record=""; dry=0; force=0
while [ $# -gt 0 ]; do
	case "$1" in
	--spec) [ $# -ge 2 ] || die "--spec needs a file"; spec_file="$2"; shift 2 ;;
	--record) [ $# -ge 2 ] || die "--record needs a directory"; record="$2"; shift 2 ;;
	--dry-run) dry=1; shift ;;
	--force) force=1; shift ;;
	-h|--help) sed -n '2,/^set -eu/{/^set -eu/!p;}' "$0"; exit 0 ;;
	*) die "unknown argument $1" ;;
	esac
done
[ "$(uname -s)" = FreeBSD ] || die "this is the installer for FreeBSD: it runs in the UMP FreeBSD rescue system (install/install.sh is the one for the Linux rescue system)"
[ "$(id -u)" = 0 ] || die "run as root"
case "$(uname -m)" in
amd64) arch=amd64; efi_name=BOOTX64.EFI ;;
arm64) arch=arm64; efi_name=BOOTAA64.EFI ;;
*) die "unsupported machine $(uname -m)" ;;
esac
if [ ! -f /etc/ump-rescue-version ] && [ "$force" = 0 ]; then
	die "this is not the UMP FreeBSD rescue system. The installer erases every disk of its plan; run it from the rescue system (ump server reinstall), or pass --force if that is what you want."
fi
# Every command this script relies on, checked up front: a missing tool
# must fail here, not halfway through the disks.
for t in jq gpart zpool zfs zdb newfs_msdos fetch tar sha256 pw pkg-static diskinfo camcontrol sysctl ifconfig route kenv kldload pfctl ssh-keygen \
	chroot mount umount swapoff mkfifo tee dd awk sed grep sort tr cut paste head date cat cp mv rm mkdir chmod chown sleep; do
	command -v "$t" >/dev/null || die "$t is missing in this rescue system; the UMP FreeBSD rescue image has it"
done

# --- the specification -------------------------------------------------

[ -n "$spec_file" ] || die "--spec FILE is required"
spec="$(cat "$spec_file")" || die "cannot read $spec_file"
printf '%s' "$spec" | jq -e . >/dev/null 2>&1 || die "$spec_file is not valid JSON"
s() { printf '%s' "$spec" | jq -r "$1 // empty"; }      # a string, empty when absent
sb() { printf '%s' "$spec" | jq -r "$1 // false"; }      # a boolean
os="$(s .os)"; hostname="$(s .hostname)"; timezone="$(s .timezone)"; storage="$(s .storage)"
[ -n "$os" ] && [ -n "$hostname" ] && [ -n "$storage" ] || die "the specification needs os, hostname and storage"
[ -n "$timezone" ] || timezone=UTC
storage="${storage%/}"
mitigations="$(sb .kernel.mitigations)"; ipv6="$(sb .kernel.ipv6)"; kernel_extra="$(s .kernel.extra)"
layout_name="$(s .layout.name)"; [ -n "$layout_name" ] || layout_name="inline"
profile_name="$(s .profile.name)"
if printf '%s' "$spec" | jq -e '.keep_data != null and .keep_data != false' >/dev/null; then
	die "keep-data replaces a Linux system on its own volumes (Ubuntu, Debian); FreeBSD is installed onto disks the plan erases"
fi
printf '%s' "$spec" | jq -e '(.users // []) | length > 0' >/dev/null || die "the specification names no users"
bad_ud="$(printf '%s' "$spec" | jq -r '[(.userdata // [])[] | select((.type // "bash") != "bash") | "\(.name) (\(.type))"] | join(", ")')"
[ -z "$bad_ud" ] || die "userdata $bad_ud is not of the type FreeBSD takes (bash)"
firewall="$(printf '%s' "$spec" | jq -r '.firewall.policy // "accept"')"
# the administrator: the first sudo user other than root, else root, as
# ump picks it; ump logs in as it
admin="$(printf '%s' "$spec" | jq -r '([.users[] | select(.sudo == true and .name != "root")][0].name) // ([.users[] | select(.name == "root")][0].name) // .users[0].name')"
# the private networks, in order; a specification from before several
# networks has the one, private
privates="$(printf '%s' "$spec" | jq -c '.network.private_networks // [.network.private | select(. != null)]')"
image_url="$storage/os/$os/$arch"
# the firmware this system was started by; arm64 has UEFI alone
firmware=uefi
if [ "$arch" = amd64 ] && [ "$(sysctl -n machdep.bootmethod 2>/dev/null || true)" != UEFI ]; then firmware=bios; fi

# --- the renderers: the plan, and the files of the new system ----------

# Functions only, which images/test-freebsd-render.sh runs without a
# machine, under any sh with jq. What they read of the rest:
#
#   spec         the specification
#   arch, firmware, hostname, timezone, os, layout_name, profile_name,
#   mitigations, ipv6, kernel_extra, firewall, admin, privates
#   mac, uplink  the uplink: its MAC address and its name
#   ifaces       the interfaces, a line each: NAME MAC KIND WAS (KIND ext
#                or int; WAS the name the kernel gave it)
#   rename       yes when the interfaces get UMP's names
#   net4, ip4    static, dhcp or none; ADDRESS/PREFIX[,GATEWAY] when static
#   net6, ip6    static, auto (router advertisements) or none; likewise
#   dns, dns_source  the resolvers, empty when they come from DHCP;
#                public (1.1.1.1 and 8.8.8.8, ump's default), spec or dhcp
#   lease_dns    no when the uplink takes its address from a lease but
#                not the lease's resolvers
#   uplink_spec  the specification's uplink, empty without one
#
# The disks of the machine are JSON, [{name, size, serial, sector}], as
# machine_disks gives them; a plan is JSON too, as plan_from_file and
# plan_from_layout give it:
#
#   {source: "plan" | "layout", firmware, boot_disk, pool: {name, vdev, mountpoint},
#    disks: [{name, size, serial, sector, plan, by, parts: [{index, kind, size}]}],
#    left: [{name, size, serial, plan}]}
#
# name is FreeBSD's (nda0), plan the name the Linux rescue system has for
# the disk (/dev/nvme0n1) and by how it was found here (serial number,
# size, order); kind is bios, efi, swap or zfs; left are the disks the
# installation leaves alone. A plan that cannot be made is {refused: [...]}.

# what the jq programs share: norm trims a string; order puts NVMe disks
# first and the rest by name, numbers as numbers (da2 before da10), which
# is where first looks; gib prints bytes; rest says that a size takes
# what is left of a disk; bytes is a size in bytes on a disk of that
# many, and gsize the same for gpart; part is a part as the plan keeps it
# (a bios part is FreeBSD's freebsd-boot partition, which pmbr loads
# whole: 512K whatever the layout says)
jq_lib='
def norm: (. // "") | tostring | gsub("^\\s+|\\s+$"; "");
def natural: . as $s | (($s | capture("^(?<p>.*[^0-9])(?<n>[0-9]+)$")) // {p: $s, n: "0"}) | [.p, (.n | tonumber)];
def order: sort_by([(if (.name | test("^(nda|nvd)[0-9]+$")) then 0 else 1 end)] + (.name | natural));
def gib: (. / 1073741824 * 10 | round / 10 | tostring) + " GiB";
def rest: . == "rest" or . == "all" or . == "100%";
def bytes($disk):
	if type != "string" then null
	elif test("^[0-9]+K$") then (.[:-1] | tonumber) * 1024
	elif test("^[0-9]+M$") then (.[:-1] | tonumber) * 1048576
	elif test("^[0-9]+G$") then (.[:-1] | tonumber) * 1073741824
	elif test("^[0-9]+T$") then (.[:-1] | tonumber) * 1099511627776
	elif test("^[0-9]+%$") then ((($disk / 100 | floor) * (.[:-1] | tonumber)) / 1048576 | floor) * 1048576
	else null end;
def gsize($disk):
	if test("^[0-9]+[KMGT]$") then ascii_downcase
	else ((bytes($disk) // 0) / 1048576 | floor | tostring) + "m" end;
def part: {index: .index, kind: .kind, size: (if .kind == "bios" then "512K" else .size end)};
def members: [.disks | to_entries[] | select(any(.value.parts[]; .kind == "zfs"))];
def describe: "\(.name) (\(.size) bytes\(if (.serial | norm) != "" then ", serial " + (.serial | norm) else "" end))";
'

# machine_disks: the disks of this machine from lines "NAME SECTOR SIZE
# SERIAL" on stdin, in the order of the layouts
machine_disks() {
	jq -Rnc "$jq_lib"'[inputs | select(length > 0) | split(" ") | {name: .[0], sector: (.[1] | tonumber), size: (.[2] | tonumber), serial: (.[3:] | join(" ") | norm)}] | order'
}

# plan_from_file PLAN DISKS: the plan of the Linux rescue system
# (plan.json) with its disks found among this machine's: by serial
# number where both systems read one and the size agrees (the disks the
# plan leaves alone too, so that they are known), else by size when one
# disk of that size is left, else in order among the disks of that size.
# Refused: a disk of the plan that is not here; and one found by size or
# order while a disk outside the plan has that size and no serial number
# says which is which.
plan_from_file() {
	printf '%s' "$1" | jq -c --argjson have "$2" "$jq_lib"'
	def wrong:
		if type != "object" then "it is no object"
		elif .version != 1 then "its version is \(.version | tojson), and this installer reads version 1"
		elif (.disks | type) != "array" or (.disks | length) == 0 then "it names no disks"
		elif any(.disks[]; (.name | type) != "string" or (.size | type) != "number" or (.parts | type) != "array") then "a disk in it lacks its name, its size in bytes or its parts"
		elif any(.disks[].parts[]; (.index | type) != "number" or (.kind | type) != "string" or (.size | type) != "string") then "a part in it lacks its index, kind or size"
		elif any((.left // [])[]; (.name | type) != "string" or (.size | type) != "number") then "a disk it leaves alone lacks its name or its size in bytes"
		else null end;
	. as $plan
	| wrong as $wrong
	| if $wrong then {refused: ["plan.json cannot be read: \($wrong)"]} else
	($have | order) as $all
	# by serial number: the disks of the plan, and those it leaves alone
	| reduce (($plan.disks | to_entries | map(. + {mine: true})) + (($plan.left // []) | to_entries | map(. + {mine: false})))[] as $e
		({free: $all, got: {}, left: [], missing: []};
		($e.value.serial | norm) as $serial
		| (if $serial == "" then null else [.free[] | select((.serial | norm) == $serial and .size == $e.value.size)][0] end) as $m
		| if $m == null then (if $e.mine then . else .missing += [$e.value] end)
		  else .free -= [$m]
			| if $e.mine then .got[$e.key | tostring] = $m + {by: "serial number"} else .left += [$m + {plan: $e.value.name}] end
		  end)
	# by size, in order
	| reduce ($plan.disks | to_entries[]) as $e (. + {gone: []};
		if .got[$e.key | tostring] then .
		else [.free[] | select(.size == $e.value.size)] as $same
			| if ($same | length) == 0 then .gone += [$e.value]
			  else .free -= [$same[0]]
				| .got[$e.key | tostring] = $same[0] + {by: (if ($same | length) == 1 then "size" else "order" end)}
			  end
		end)
	| . as $st
	| [$st.gone[] | "\(describe) is not here"] as $gone
	| [$plan.disks | to_entries[] | . as $e | $st.got[$e.key | tostring] | select(. != null and .by != "serial number")
		| ([$st.free[] | select(.size == $e.value.size) | .name] | if length > 0 then . else [$st.missing[] | select(.size == $e.value.size) | .name] end) as $others
		| select(($others | length) > 0)
		| "\($e.value | describe) cannot be told from \($others | join(", ")), which the plan leaves alone: the same size, and no serial number says which is which"] as $unsure
	| if ($gone + $unsure | length) > 0 then
		{refused: ["the disks are not those of the plan: " + ($gone + $unsure | join("; ")) + " (this machine has " + ($all | map(describe) | join(", ")) + ")"]}
	  else
		[$plan.disks | to_entries[] | . as $e | $st.got[$e.key | tostring]
			| {name, size, serial: (.serial | norm), sector: (.sector // 512), plan: $e.value.name, by, parts: ($e.value.parts | map(part) | sort_by(.index))}] as $disks
		| {source: "plan", firmware: ($plan.firmware // null),
			boot_disk: (([$disks[] | select(.plan == $plan.boot_disk)][0] // [$disks[] | select(any(.parts[]; .kind == "efi" or .kind == "bios"))][0] // $disks[0]).name),
			pool: {name: ($plan.pool.name // "zroot"), vdev: ($plan.pool.vdev // "stripe"), mountpoint: ($plan.pool.mountpoint // "/")},
			disks: $disks,
			left: ([$st.left[] | {name, size, serial: (.serial | norm), plan}] + [$st.free[] | {name, size, serial: (.serial | norm), plan: null}] | order)}
	  end
	end'
}

# plan_from_layout DISKS: without a plan.json, the specification's
# layout planned here: first is the first disk of at least 4 GiB (the
# first when none is that big), .* every disk; disks of the
# specification, when it names any, are FreeBSD's names
plan_from_layout() {
	printf '%s' "$spec" | jq -c --argjson have "$1" --arg firmware "$firmware" "$jq_lib"'
	(.layout.name // "inline") as $layout
	| (.layout.parts // []) as $parts
	| ($have | order) as $all
	| [(.disks // [])[] | ltrimstr("/dev/")] as $want
	| [$want[] | . as $n | select(any($all[]; .name == $n) | not)] as $unknown
	| if ($unknown | length) > 0 then
		{refused: ["\($unknown | map("/dev/" + .) | join(", ")) of the specification is no disk of this machine (it has \($all | map(.name) | join(", "))); without a plan.json its disks are named as FreeBSD names them"]}
	  elif ($all | length) == 0 then {refused: ["no disk found"]}
	  elif ($parts | length) == 0 then {refused: ["the layout has no parts"]}
	  else
		(if ($want | length) > 0 then [$want[] | . as $n | $all[] | select(.name == $n)] else $all end) as $disks
		| ([$disks[] | select(.size >= 4294967296)][0] // $disks[0]) as $first
		| reduce ($parts | to_entries[]) as $e ({disks: [$disks[] | . + {parts: []}], pool: null, refused: []};
			$e.value as $p
			| "layout \($layout), part \($e.key + 1) (\($p.type))" as $at
			| (if $p.type == "zfs" and (($p.filesystem // "zfs") == "zfs") then "zfs"
			   elif $p.type == "slice" and ($p.filesystem == "efi" or $p.filesystem == "bios" or $p.filesystem == "swap") then $p.filesystem
			   else null end) as $kind
			| [($p.target // [])[] | select(. != "first" and . != ".*")] as $other
			| ([($p.target // [])[] | if . == "first" then $first.name elif . == ".*" then $disks[].name else empty end]
				| reduce .[] as $n ([]; if index($n) then . else . + [$n] end)) as $names
			| if $kind == null then
				.refused += ["\($at): FreeBSD takes efi, bios and swap slices and one zfs part, its root pool; \($p.type) with \($p.filesystem // "no file system") is for the Linux systems (the ZFS layouts are zfs, zfs-mirror and zfs-raidz)"]
			  elif ($other | length) > 0 then
				.refused += ["\($at): target \($other | join(", ")): without a plan.json the installer plans with first and .* alone"]
			  elif ($names | length) == 0 then
				.refused += ["\($at): no target"]
			  elif $kind == "zfs" and .pool != null then
				.refused += ["\($at): a second zfs part; this version makes the root pool and no other"]
			  else
				(if $kind == "zfs" then .pool = {name: ($p.pool // "zroot"), vdev: ($p.vdev // "stripe"), mountpoint: ($p.mountpoint // "")} else . end)
				| .disks |= map(if (.name as $n | $names | index($n)) then
					.parts += [{index: (if ($p.index // 0) > 0 then $p.index else ([.parts[].index | select(. < 100)] | max // 0) + 1 end), kind: $kind, size: $p.size} | part]
				  else . end)
			  end)
		| if (.refused | length) > 0 then {refused: .refused[:1]}
		  else {source: "layout", firmware: $firmware, boot_disk: $first.name,
			pool: (.pool // {name: "zroot", vdev: "stripe", mountpoint: "/"}),
			disks: [.disks[] | {name, size, serial: (.serial | norm), sector: (.sector // 512), plan: null, by: null, parts: (.parts | sort_by(.index))}],
			left: [$all[] | . as $d | select(any($disks[]; .name == $d.name) | not) | {name, size, serial: (.serial | norm), plan: null}]}
		  end
	  end'
}

# plan_check PLAN [UNPACKED_MB]: what stops a plan before a disk is
# touched, a line each; with the size of the system, also a pool too
# small for it (twice the system, as the root of every installation)
plan_check() {
	printf '%s' "$1" | jq -r --arg firmware "$firmware" --arg arch "$arch" --argjson unpacked "${2:-0}" "$jq_lib"'
	def need: {"stripe": 1, "mirror": 2, "raidz": 3, "raidz1": 3, "raidz2": 4, "raidz3": 5};
	def parity: {"raidz": 1, "raidz1": 1, "raidz2": 2, "raidz3": 3};
	def fixed: . as $d | [.parts[] | select(.size | rest | not) | .size | bytes($d.size) // 0] | add // 0;
	. as $plan
	| (members | length) as $n
	| (.disks[] | . as $d | .parts[]
		| select(.kind != "bios" and .kind != "efi" and .kind != "swap" and .kind != "zfs")
		| "\($d.name), partition \(.index): FreeBSD takes bios, efi, swap and zfs parts, not \(.kind)"),
	  (.disks[] | . as $d | .parts[]
		| select((.size | rest | not) and (.size | bytes($d.size)) == null)
		| "\($d.name), partition \(.index): size \(.size | tojson) is not like 512K, 512M, 2G, 20% or rest"),
	  (.disks[] | select((.parts | map(.index) | unique | length) != (.parts | length)) | "a partition of \(.name) is planned twice"),
	  (.disks[] | select(any(.parts[]; .index < 1 or .index > 128)) | "a partition of \(.name) has a number that is not between 1 and 128"),
	  (.disks[] | select([.parts[] | select(.size | rest)] | length > 1) | "two parts take the rest of \(.name)"),
	  (.disks[] | . as $d | ("bios", "efi", "swap", "zfs") as $k | select([$d.parts[] | select(.kind == $k)] | length > 1) | "\($d.name) has two \($k) parts; one of a kind on a disk"),
	  (.disks[] | select(fixed + 67108864 > .size) | "the parts of \(.name) do not fit its \(.size | gib)"),
	  (.disks[] | . as $d | .parts[] | select(.kind == "efi" and (.size | rest | not) and ((.size | bytes($d.size)) // 0) < 34603008)
		| "\($d.name), partition \(.index): an efi part of \(.size) is too small for FAT32 (33M at least)"),
	  (if (.pool.name | test("^[a-z][a-z0-9_-]*$") | not) or (.pool.name | test("^(mirror|raidz|draid|spare|log|cache|special|dedup)")) then "the pool name \(.pool.name | tojson) is not one ZFS takes (letters, digits, - and _, and none of its own words)" else empty end),
	  (if .pool.mountpoint != "/" then "the zfs part is mounted at \(.pool.mountpoint | tojson): this version makes the root pool (mountpoint /) and no other" else empty end),
	  (if need[.pool.vdev] == null then "vdev \(.pool.vdev | tojson) is not stripe, mirror, raidz, raidz2 or raidz3"
	   elif $n == 0 then "the plan has no zfs part: FreeBSD is installed with its root on ZFS (the layouts zfs, zfs-mirror and zfs-raidz)"
	   elif $n < need[.pool.vdev] then "\(.pool.vdev) needs \(need[.pool.vdev]) disks, and the plan gives it \($n)"
	   else empty end),
	  (if $firmware == "uefi" and (any(.disks[].parts[]; .kind == "efi") | not) then "this machine boots with UEFI and the plan has no efi part" else empty end),
	  (if $firmware == "bios" and $arch == "amd64" and (any(.disks[].parts[]; .kind == "bios") | not) then "this machine boots with BIOS and the plan has no bios part" else empty end),
	  (if $unpacked > 0 and $n > 0 and need[.pool.vdev] != null then
		[members[].value | . as $d | (.parts[] | select(.kind == "zfs") | .size) | if rest then $d.size - ($d | fixed) - 2097152 else bytes($d.size) // 0 end] as $sizes
		| (if .pool.vdev == "stripe" then ($sizes | add) elif .pool.vdev == "mirror" then ($sizes | min) else ($sizes | min) * ($n - parity[.pool.vdev]) end) as $room
		| if $room < $unpacked * 2 * 1048576 then "the pool would hold \($room / 1048576 | floor) MB, and the system needs \($unpacked * 2) MB (twice its \($unpacked) MB)" else empty end
	   else empty end)'
}

# plan_lines PLAN: the disks, their partitions and the pool, as the plan
# shows them
plan_lines() {
	printf '%s' "$1" | jq -r "$jq_lib"'
	"disks (\(.disks | length)): "
		+ ([.disks[] | "\(.name) (\(.size | gib)\(if .serial != "" then ", " + .serial else "" end)\(if .plan then ", \(.plan) in the plan, found by \(.by)" else "" end))"] | join(", "))
		+ (if (.left | length) > 0 then "; left alone: " + ([.left[] | "\(.name) (\(.size | gib)\(if .serial != "" then ", " + .serial else "" end)\(if .plan then ", \(.plan) in the plan" else "" end))"] | join(", ")) else "" end),
	(.pool.name as $pool | .disks[]
		| "  \(.name): " + (if (.parts | length) == 0 then "cleared, no partition"
			else ([.parts[] | "\(.index): \(.size) \(.kind)\(if .kind == "zfs" then " (" + $pool + ")" else "" end)"] | join(", ")) end)),
	"  \(.pool.name): \(.pool.vdev) over \(members | map(.value.name) | join(" ")), at \(.pool.mountpoint); the system in \(.pool.name)/ROOT/default, a boot environment"'
}

# disk_commands PLAN: what partitions the disks, a command a line, in
# order: every disk cleared (clear_disk: its table destroyed, its first
# and last MiB zeroed), a new GPT, the partitions with their labels
# (boot0, efi0, swap0, zfs0, ... by the disk's place in the plan), the
# one that takes the rest last, and the first and last MiB of each
# zeroed (zero_ends), where a file system or pool from before would be
# found again
disk_commands() {
	printf '%s' "$1" | jq -r "$jq_lib"'
	def ptype: {"bios": "freebsd-boot", "efi": "efi", "swap": "freebsd-swap", "zfs": "freebsd-zfs"}[.];
	def plabel: {"bios": "boot", "efi": "efi", "swap": "swap", "zfs": "zfs"}[.];
	.disks | to_entries[] | .key as $k | .value as $d
	| "clear_disk \($d.name)",
	  (if ($d.parts | length) > 0 then "gpart create -s gpt \($d.name)" else empty end),
	  ($d.parts | (map(select(.size | rest | not)) + map(select(.size | rest)))[]
		| "gpart add -a \(if .kind == "bios" then "4k" else "1m" end) -t \(.kind | ptype) -i \(.index) -l \(.kind | plabel)\($k)\(if (.size | rest) then "" else " -s " + (.size | gsize($d.size)) end) \($d.name)"),
	  ($d.parts[] | "zero_ends \($d.name)p\(.index)")'
}

# pool_commands PLAN: the pool over the zfs partitions, by their GPT
# labels, and FreeBSD's own arrangement of datasets (bsdinstall's,
# without usr/ports and usr/src): the system in ROOT/default, a boot
# environment that bectl and freebsd-update find; /usr and /var
# themselves are in it, their datasets only hold the ones below
pool_commands() {
	printf '%s' "$1" | jq -r --arg target "${TARGET:-/mnt}" "$jq_lib"'
	.pool.name as $p
	| "zpool create -f -o altroot=\($target) -o ashift=12 -O compression=lz4 -O atime=off -O mountpoint=none \($p)\(if .pool.vdev == "stripe" then "" elif .pool.vdev == "raidz1" then " raidz" else " " + .pool.vdev end) \(members | map("/dev/gpt/zfs\(.key)") | join(" "))",
	  "zfs create -o mountpoint=none \($p)/ROOT",
	  "zfs create -o mountpoint=/ -o canmount=noauto \($p)/ROOT/default",
	  "zfs mount \($p)/ROOT/default",
	  "zfs create -o mountpoint=/home \($p)/home",
	  "zfs create -o mountpoint=/tmp -o setuid=off \($p)/tmp",
	  "zfs create -o mountpoint=/usr -o canmount=off \($p)/usr",
	  "zfs create -o mountpoint=/var -o canmount=off \($p)/var",
	  "zfs create -o exec=off -o setuid=off \($p)/var/audit",
	  "zfs create -o exec=off -o setuid=off \($p)/var/crash",
	  "zfs create -o exec=off -o setuid=off \($p)/var/log",
	  "zfs create -o atime=on \($p)/var/mail",
	  "zfs create -o setuid=off \($p)/var/tmp",
	  "zpool set bootfs=\($p)/ROOT/default \($p)"'
}

# plan_labels PLAN: the GPT labels the plan makes, a line each
plan_labels() {
	printf '%s' "$1" | jq -r '.disks | to_entries[] | .key as $k | .value.parts[] | {"bios": "boot", "efi": "efi", "swap": "swap", "zfs": "zfs"}[.kind] + ($k | tostring)'
}

# fstab_text PLAN: /etc/fstab: the datasets mount themselves, so only the
# EFI system partition and swap are here. Every disk with an efi part has
# the boot loader; the first is mounted, with failok and without a check
# at boot, so that a mirror that lost that disk still starts.
fstab_text() {
	printf '%s' "$1" | jq -r --arg layout "$layout_name" '
	"# /etc/fstab, written by UMP at installation (layout \($layout) over \(.disks | map(.name) | join(" "))).",
	"# The datasets of \(.pool.name) mount themselves (zfs_enable in /etc/rc.conf).",
	([.disks | to_entries[] | select(any(.value.parts[]; .kind == "efi"))][0]
		| if . then "# failok and no check at boot: the system starts without this disk too", "/dev/gpt/efi\(.key)\t/boot/efi\tmsdosfs\trw,failok\t0\t0" else empty end),
	(.disks | to_entries[] | select(any(.value.parts[]; .kind == "swap")) | "/dev/gpt/swap\(.key)\tnone\tswap\tsw\t0\t0")'
}

# loader_tunables: what the profile's kernel section gives the loader,
# NAME=VALUE a line: page table isolation off unless the mitigations
# stay (amd64; it is the one that costs), then the words of kernel.extra
# that are tunables; loader_ignored: its other words (those of a Linux
# kernel's command line), which FreeBSD's loader has no use for
loader_tunables() {
	local w
	if [ "$arch" = amd64 ] && [ "$mitigations" != true ]; then echo "vm.pmap.pti=0"; fi
	for w in $kernel_extra; do case "$w" in ?*=*) echo "$w" ;; esac; done
}
loader_ignored() {
	local w
	for w in $kernel_extra; do case "$w" in ?*=*) ;; *) echo "$w" ;; esac; done
}

# loader_conf: /boot/loader.conf
loader_conf() {
	local line value
	echo "# /boot/loader.conf, written by UMP at installation."
	echo 'zfs_load="YES"'
	echo 'autoboot_delay="2"'
	echo "# the pool's disks under their GPT labels alone"
	echo 'kern.geom.label.disk_ident.enable="0"'
	echo 'kern.geom.label.gptid.enable="0"'
	echo "# the kernel's messages on the serial port and the screen"
	echo 'boot_multicons="YES"'
	[ "$arch" != amd64 ] || echo 'console="comconsole,vidconsole"'
	loader_tunables | while IFS= read -r line; do
		value="${line#*=}"; value="${value#\"}"; value="${value%\"}"
		case "${line%%=*}" in
		vm.pmap.pti) echo "# UMP: page table isolation is off (the profile's kernel.mitigations is false); remove the line to turn it on" ;;
		esac
		echo "${line%%=*}=\"$value\""
	done
}

# net_from_spec JSON: the uplink of the specification (network.uplink,
# which the provider gives where it knows the machine's network, as for
# a PXE machine); without a mac there, mac stays as it is
net_from_spec() {
	local m gw
	m="$(printf '%s' "$1" | jq -r '.mac // empty' | tr 'A-F-' 'a-f:')"
	if [ -n "$m" ]; then
		printf '%s\n' "$m" | grep -Eq '^([0-9a-f]{2}:){5}[0-9a-f]{2}$' || die "network.uplink.mac: $m is not a MAC address"
		mac="$m"
	fi
	net4="$(printf '%s' "$1" | jq -r '.ipv4.mode // "" | if . == "" then "none" else . end')"
	net6="$(printf '%s' "$1" | jq -r '.ipv6.mode // "" | if . == "" then "none" else . end')"
	ip4=""; ip6=""
	case "$net4" in
	static)
		ip4="$(printf '%s' "$1" | jq -r '.ipv4.address // empty')"; gw="$(printf '%s' "$1" | jq -r '.ipv4.gateway // empty')"
		case "$ip4" in */*) ;; *) die "network.uplink.ipv4: a static address is ADDRESS/PREFIX, not \"$ip4\"" ;; esac
		ip4="$ip4${gw:+,$gw}" ;;
	dhcp|none) ;;
	*) die "network.uplink.ipv4: mode $net4 is not static, dhcp or none" ;;
	esac
	case "$net6" in
	static)
		ip6="$(printf '%s' "$1" | jq -r '.ipv6.address // empty')"; gw="$(printf '%s' "$1" | jq -r '.ipv6.gateway // empty')"
		case "$ip6" in */*) ;; *) die "network.uplink.ipv6: a static address is ADDRESS/PREFIX, not \"$ip6\"" ;; esac
		ip6="$ip6${gw:+,$gw}" ;;
	auto|none) ;;
	*) die "network.uplink.ipv6: mode $net6 is not static, auto or none" ;;
	esac
	if [ "$net6" != none ] && [ "$ipv6" != true ]; then
		warn "network.uplink.ipv6 is $net6, and the profile's IPv6 is off (kernel.ipv6): no IPv6"
		net6=none; ip6=""
	fi
	[ "$net4$net6" != nonenone ] || die "network.uplink has neither IPv4 nor IPv6; the new system would be unreachable"
	dns="$(printf '%s' "$1" | jq -r '.dns // [] | join(" ")')"; dns_source=spec
	if [ "$(printf '%s' "$1" | jq -r '.dns_from_dhcp // false')" = true ]; then
		[ "$net4" = dhcp ] || [ "$net6" = auto ] || die "network.uplink.dns_from_dhcp: no lease to take resolvers from (ipv4 $net4, ipv6 $net6)"
		dns=""; dns_source=dhcp
	elif [ -z "$dns" ]; then
		dns="1.1.1.1 8.8.8.8"; dns_source=public
	fi
	# resolvers of its own: those alone, not the lease's besides
	lease_dns=yes
	if [ "$net4" = dhcp ] && [ "$dns_source" = spec ]; then lease_dns=no; fi
}

# net_from_machine: the uplink as the rescue system has it (ip4, ip6), as
# ump configures it on every system: static with the addresses found,
# DHCP without any, and the public resolvers; IPv6 only when the profile
# keeps it
net_from_machine() {
	net4=none; net6=none
	[ -n "$ip4$ip6" ] || net4=dhcp
	[ -z "$ip4" ] || net4=static
	if [ "$ipv6" = true ] && [ -n "$ip6" ]; then net6=static; else ip6=""; fi
	[ "$net4$net6" != nonenone ] || net4=dhcp
	dns="1.1.1.1 8.8.8.8"; dns_source=public; lease_dns=yes
}

# name_interfaces: the interfaces from lines "WAS MAC KIND" on stdin, as
# ifaces has them: the uplink and those with a public address ext01,
# ..., the rest int01, ..., unless the profile keeps the kernel's names
name_interfaces() {
	awk -v rename="$rename" 'NF >= 3 {
		if (rename != "yes") name = $1
		else if ($3 == "ext") name = sprintf("ext%02d", ++ext)
		else name = sprintf("int%02d", ++int_)
		print name, $2, $3, $1
	}'
}

# in_network ADDRESS/PREFIX GATEWAY: the gateway is in the address's
# network (IPv4)
in_network() {
	echo "$1 $2" | awk '{
		split($1, p, "/"); split(p[1], a, "."); split($2, g, ".")
		x = ((a[1] * 256 + a[2]) * 256 + a[3]) * 256 + a[4]; y = ((g[1] * 256 + g[2]) * 256 + g[3]) * 256 + g[4]
		d = 2 ^ (32 - p[2]); exit !(int(x / d) == int(y / d))
	}'
}

# rc_conf: /etc/rc.conf: the hostname, the services, the firewall, and
# the network: every wired interface under its name, the uplink
# configured, the others up without addresses. A gateway outside the
# address's network (a /32, as some providers give) is reached on-link:
# a route to it through the interface, then the default route.
rc_conf() {
	local name m kind was a g
	echo "# /etc/rc.conf, written by UMP at installation (profile ${profile_name:-?}); sysrc"
	echo "# changes it, /etc/defaults/rc.conf has what is not set here."
	echo "hostname=\"$hostname\""
	echo 'zfs_enable="YES"'
	echo 'sshd_enable="YES"'
	echo 'ntpd_enable="YES"'
	echo 'ntpd_sync_on_start="YES"'
	echo 'sendmail_enable="NONE"'
	echo 'syslogd_flags="-ss"'
	if [ "$firewall" = drop ]; then
		echo "# the firewall: /etc/pf.conf"
		echo 'pf_enable="YES"'
		echo 'pflog_enable="YES"'
	fi
	if [ "$rename" = yes ]; then
		echo "# the interfaces by MAC address (/usr/local/etc/rc.d/ump_ifnames renames"
		echo "# them before the network starts): ext01, ... the uplink and those with a"
		echo "# public address, int01, ... the rest"
		echo 'ump_ifnames_enable="YES"'
		echo "ump_ifnames=\"$(printf '%s\n' "$ifaces" | awk 'NF >= 2 {printf "%s%s=%s", sep, $1, $2; sep = " "}')\""
	fi
	printf '%s\n' "$ifaces" | while read -r name m kind was; do
		[ -n "$name" ] || continue
		[ "$name" = "$uplink" ] || echo "ifconfig_$name=\"up\""
	done
	case "$net4" in
	dhcp) echo "ifconfig_$uplink=\"DHCP\"" ;;
	static)
		a="${ip4%%,*}"
		echo "ifconfig_$uplink=\"inet $a\""
		case "$ip4" in
		*,*)
			g="${ip4#*,}"
			if in_network "$a" "$g"; then
				echo "defaultrouter=\"$g\""
			else
				echo "# the gateway is outside the address's network: on-link"
				echo 'static_routes="umpgw umpdefault"'
				echo "route_umpgw=\"-host $g -interface $uplink\""
				echo "route_umpdefault=\"default $g\""
			fi ;;
		esac ;;
	*) echo "ifconfig_$uplink=\"up\"" ;;
	esac
	case "$net6" in
	auto)
		echo "ifconfig_${uplink}_ipv6=\"inet6 accept_rtadv\""
		echo 'rtsold_enable="YES"' ;;
	static)
		a="${ip6%%,*}"
		echo "ifconfig_${uplink}_ipv6=\"inet6 ${a%/*} prefixlen ${a##*/}\""
		case "$ip6" in
		*,*)
			g="${ip6#*,}"
			case "$g" in
			fe80:*|FE80:*) echo "ipv6_defaultrouter=\"$g%$uplink\"" ;;
			*)
				echo 'ipv6_static_routes="umpgw umpdefault"'
				echo "ipv6_route_umpgw=\"-host $g -interface $uplink\""
				echo "ipv6_route_umpdefault=\"default $g\"" ;;
			esac ;;
		esac ;;
	esac
}

# resolv_conf: /etc/resolv.conf, the uplink's resolvers; nothing when
# they come from the lease
resolv_conf() {
	local d
	[ -n "$dns" ] || return 0
	case "$dns_source" in
	public) echo "# written by UMP at installation: public resolvers, independent of the provider" ;;
	*) echo "# written by UMP at installation: the resolvers of the specification (network.uplink.dns)" ;;
	esac
	for d in $dns; do echo "nameserver $d"; done
}

# resolvconf_conf: /etc/resolvconf.conf: the resolvers above stay, whatever
# a lease or a router advertisement says; nothing when they come from the
# lease
resolvconf_conf() {
	[ "$dns_source" != dhcp ] || return 0
	echo "# written by UMP at installation: /etc/resolv.conf is as the installation"
	echo "# wrote it, and neither a DHCP lease nor a router advertisement replaces"
	echo "# it. Remove this file to take the lease's resolvers."
	echo "resolvconf=NO"
}

# hosts_text: /etc/hosts: FreeBSD's own two lines, and the hostname on
# the loopback address (the 127.0.1.1 of the other systems answers
# nothing on FreeBSD)
hosts_text() {
	local short="${hostname%%.*}"
	[ "$short" != "$hostname" ] || short=""
	printf '# /etc/hosts, written by UMP at installation.\n'
	printf '::1\t\tlocalhost\n127.0.0.1\tlocalhost\n'
	printf '127.0.0.1\t%s%s\n' "$hostname" "${short:+ $short}"
}

# net_plan: the uplink, as the plan shows it
net_plan() {
	local addresses="$ip4${ip6:+ $ip6}"
	[ -n "$addresses" ] || addresses=DHCP
	if [ -n "$uplink_spec" ]; then
		addresses="IPv4 $net4${ip4:+ $ip4}$([ "$net4" != dhcp ] || echo " (client ID: MAC)"), IPv6 $net6${ip6:+ $ip6} (the specification's uplink)"
	fi
	echo "$uplink ($mac) $addresses; IPv6 $([ "$ipv6" = true ] && echo on || echo off); DNS ${dns:-from DHCP}$([ "$lease_dns" = yes ] || echo ", not the lease's")"
}

# net_record: the lines of /etc/ump/install that say how the
# specification's uplink is configured: network (IPv4) and network6 are
# static, dhcp, auto or none, dns the resolvers or dhcp
net_record() {
	printf 'network=%s\nnetwork6=%s\ndns=%s\n' "$net4" "$net6" "$([ "$dns_source" = dhcp ] && echo dhcp || echo "$dns" | tr ' ' ',')"
}

# pf_conf: /etc/pf.conf for a firewall with policy drop, the counterpart
# of the nftables ruleset of the other systems: in, only what is allowed;
# pf keeps state, so the answers to the server's own connections pass.
# There is no counterpart to the forward chain: block in all holds for
# every packet that comes in on any interface but lo0, whatever it is
# for.
pf_conf() {
	echo "# /etc/pf.conf, written by UMP from the profile's firewall section. After"
	echo "# editing, pfctl -nf /etc/pf.conf checks it and service pf reload loads it."
	echo "set skip on lo0"
	echo "set block-policy drop"
	echo "# in: only what is allowed below; out: everything, and the answers come"
	echo "# back in with the states"
	echo "block in all"
	echo "pass out all"
	printf '%s' "$spec" | jq -r '(.firewall.allow // [])[]
		| (.from // "") as $from
		| (if $from == "" then "" elif ($from | test(":")) then "inet6 " else "inet " end) as $af
		| (if $from == "" then "" else " from " + $from end) as $src
		| if .proto == "icmp" then
			(if $af != "inet6 " then "pass in inet proto icmp\($src)" else empty end),
			(if $af != "inet " then "pass in inet6 proto icmp6\($src)" else empty end)
		  else "pass in \($af)proto \(.proto)\($src) to port \(.port | tostring | sub("-"; ":"))" end'
}

# firewall_plan: the firewall, as the plan shows it
firewall_plan() {
	printf '%s' "$spec" | jq -r '.firewall | if .policy == "drop" then "drop, allow " + ([(.allow // [])[] | (if .proto == "icmp" then "icmp" else "\(.proto)/\(.port)" end) + (if .from then " from " + .from else "" end)] | if length == 0 then "nothing" else join(", ") end) else "none" end'
}

# sshd_conf: /etc/ssh/sshd_config.d/50-ump.conf, the file of the other
# systems (FreeBSD's sshd would let PAM ask for a password), and no
# lookup of the client in the DNS, which is FreeBSD's default
sshd_conf() {
	cat <<'EOF'
# UMP: no passwords over SSH, and no root login. The installer writes
# 10-ump-root.conf (which wins, sshd keeps the first value it reads) with
# "PermitRootLogin prohibit-password" when the profile lists root with
# keys. The administrator is the profile's sudo user, ump by default.
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
# FreeBSD's sshd looks every client up in the DNS unless told not to: a
# resolver that does not answer would hold every login for half a minute.
UseDNS no
EOF
}

# ifnames_script: /usr/local/etc/rc.d/ump_ifnames. FreeBSD renames an
# interface by its driver's name (ifconfig_em0_name), which is the
# kernel's order and not the MAC address; this does it by MAC, before
# the network starts.
ifnames_script() {
	cat <<'EOF'
#!/bin/sh
#
# UMP: uniform interface names, bound to the MAC addresses. ump_ifnames
# in /etc/rc.conf lists them, NAME=MAC each, as /etc/ump/install does;
# every interface with one of the addresses is renamed before the
# network starts, so that ifconfig_NAME configures it. sysctl names
# (dev.em.0) keep the driver's.
#
# PROVIDE: ump_ifnames
# REQUIRE: FILESYSTEMS
# BEFORE: netif
# KEYWORD: nojail

. /etc/rc.subr

name="ump_ifnames"
desc="Name the network interfaces by their MAC addresses"
rcvar="ump_ifnames_enable"
start_cmd="ump_ifnames_start"
stop_cmd=":"

ump_ifnames_start()
{
	local pair want mac i m
	for pair in ${ump_ifnames}; do
		want="${pair%%=*}"; mac="${pair#*=}"
		for i in $(ifconfig -l ether); do
			m="$(ifconfig "$i" ether 2>/dev/null | awk '$1 == "ether" {print $2}')"
			[ "$m" = "$mac" ] || continue
			[ "$i" = "$want" ] || ifconfig "$i" name "$want" >/dev/null
			break
		done
	done
}

load_rc_config $name
: ${ump_ifnames_enable:="NO"}
: ${ump_ifnames:=""}
run_rc_command "$1"
EOF
}

# record_text PLAN VERSION KERNEL DATE: /etc/ump/install, the keys of
# every system with FreeBSD's values
record_text() {
	local plan="$1"
	cat <<EOF
os=$os
image_version=$2
image_kernel=$3
installed=$4
profile=$profile_name
layout=$layout_name
disks=$(printf '%s' "$plan" | jq -r '.disks | map("/dev/" + .name) | join(" ")')
boot_disk=/dev/$(printf '%s' "$plan" | jq -r .boot_disk)
root=$(printf '%s' "$plan" | jq -r .pool.name)/ROOT/default
hostname=$hostname
admin=$admin
interfaces=$(printf '%s\n' "$ifaces" | awk 'NF >= 2 {printf "%s%s=%s", sep, $1, $2; sep = " "}')
uplink=$uplink
ipv6=$([ "$ipv6" = true ] && echo on || echo off)
mitigations=$([ "$mitigations" = true ] && echo on || echo off)
firewall=$firewall
userdata=$(printf '%s' "$spec" | jq -r '(.userdata // []) | map(.name) | join(",")')
private=$(printf '%s' "$privates" | jq -r 'map("\(.network):\(.address)") | join(",")')
private_network=$(printf '%s' "$privates" | jq -r '.[0].network // empty')
private_address=$(printf '%s' "$privates" | jq -r '.[0].address // empty')
EOF
	[ -z "$uplink_spec" ] || net_record
}

# --- the machine ----------------------------------------------------------

step="looking at the machine"
boot_config="${UMP_BOOT_CONFIG:-/etc/ump/boot}"
# the disks: every one but memory disks, CDs, floppies and those on USB
# (an install medium or virtual media)
usb_disks="$(camcontrol devlist -v 2>/dev/null | awk '
	/^scbus/ {usb = ($0 ~ /umass/)}
	usb && /\(/ {gsub(/[()]/, " "); n = split($0, a, /[ ,]+/); for (i = 1; i <= n; i++) if (a[i] ~ /^da[0-9]+$/) print a[i]}')"
have="$(for d in $(sysctl -n kern.disks); do
	case "$d" in md*|cd*|fd*) continue ;; esac
	case " $(echo $usb_disks) " in *" $d "*) continue ;; esac
	info="$(diskinfo "$d" 2>/dev/null)" || continue
	set -- $info
	[ "${3:-0}" -gt 0 ] || continue
	echo "$d $2 $3 $(diskinfo -s "$d" 2>/dev/null || true)"
done | machine_disks)"

# the network: the uplink of the specification where it has one (a PXE
# machine's), else what the rescue system was given in its boot config
# (the Linux rescue system writes there what it had itself), else what
# is configured
uplink_spec="$(printf '%s' "$spec" | jq -c '.network.uplink // empty')"
cmdline="$(cat "$boot_config/cmdline" 2>/dev/null | tr '\r\n' '  ' || true)"
param() {
	local w
	for w in $cmdline; do case "$w" in "$1="*) printf '%s' "${w#*=}"; return 0 ;; esac; done
	kenv -q "$1" 2>/dev/null || true
}
ether() { ifconfig "$1" ether 2>/dev/null | awk '$1 == "ether" {print $2; exit}'; }
mac="$(param ump.mac | tr 'A-F-' 'a-f:')"
ip4="$(param ump.ip)"
ip6="$(param ump.ip6)"
route_if="$(route -n get -inet default 2>/dev/null | awk '$1 == "interface:" {print $2}')"
[ -n "$route_if" ] || route_if="$(route -n get -inet6 default 2>/dev/null | awk '$1 == "interface:" {print $2}')"
if [ -z "$mac" ] && [ -n "$route_if" ]; then
	mac="$(ether "$route_if")"
	if [ -z "$ip4" ]; then
		a="$(ifconfig -f inet:cidr "$route_if" inet 2>/dev/null | awk '$1 == "inet" {print $2; exit}')"
		g="$(route -n get -inet default 2>/dev/null | awk '$1 == "gateway:" {print $2}')"
		[ -z "$a" ] || ip4="$a${g:+,$g}"
	fi
	if [ -z "$ip6" ]; then
		a="$(ifconfig -f inet6:cidr "$route_if" inet6 2>/dev/null | awk '$1 == "inet6" && $2 !~ /^fe80/ && $0 !~ /tentative|deprecated|temporary/ {print $2; exit}')"
		g="$(route -n get -inet6 default 2>/dev/null | awk '$1 == "gateway:" {sub(/%.*/, "", $2); print $2}')"
		[ -z "$a" ] || ip6="$a${g:+,$g}"
	fi
fi
if [ -n "$uplink_spec" ]; then
	net_from_spec "$uplink_spec"
else
	net_from_machine
fi

# the interfaces: every wired one, by MAC, without those FreeBSD makes
# itself (vlan, bridge, ...). The uplink and those with a public address
# are external (ext01, ext02, ...), the rest internal (int01, ...),
# unless the profile keeps the kernel's names.
rename="$(printf '%s' "$spec" | jq -r 'if .network.rename == false then "no" else "yes" end')"
private_v4='^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|169\.254\.|127\.)'
wired() {
	local i m
	for i in $(ifconfig -l ether); do
		case "$i" in vlan*|bridge*|lagg*|tap*|vmnet*|epair*|wlan*|vxlan*|gif*|gre*|tun*|wg*|lo[0-9]*|ipsec*|enc*|pflog*|pfsync*|usbus*) continue ;; esac
		m="$(ether "$i")"
		case "$m" in ""|00:00:00:00:00:00) continue ;; esac
		echo "$i $m"
	done
}
wired_now="$(wired)"
if [ -n "$mac" ] && ! printf '%s\n' "$wired_now" | grep -q " $mac\$"; then
	[ -z "$uplink_spec" ] || die "the uplink of the specification, $mac, is no interface of this machine (it has $(printf '%s\n' "$wired_now" | awk 'NF {printf "%s%s (%s)", sep, $1, $2; sep = ", "; n++} END {if (!n) printf "none"}'))"
	# the MAC of the boot config is no interface here: the one this
	# rescue system reaches the network through is the uplink
	[ -z "$route_if" ] || { warn "the uplink of the boot config, $mac, is no interface of this machine; taking $route_if, which has the default route"; mac="$(ether "$route_if")"; }
fi
[ -n "$mac" ] && printf '%s\n' "$wired_now" | grep -q " $mac\$" || die "no network interface with a default route; the new system would be unreachable"
ifaces="$(printf '%s\n' "$wired_now" | while read -r i m; do
	[ -n "$i" ] || continue
	kind=int
	if [ "$m" = "$mac" ]; then kind=ext
	elif ifconfig "$i" inet 2>/dev/null | awk '$1 == "inet" {print $2}' | grep -qvE "$private_v4"; then kind=ext
	elif ifconfig "$i" inet6 2>/dev/null | awk '$1 == "inet6" && $2 !~ /^fe80/ {print $2}' | grep -qviE '^f[cd]'; then kind=ext
	fi
	echo "$i $m $kind"
done | name_interfaces)"
uplink="$(printf '%s\n' "$ifaces" | awk -v mac="$mac" '$2 == mac {print $1; exit}')"

# --- the plan: the disks of plan.json found again, or the layout planned here ---

step="planning"
if [ -f "$boot_config/plan.json" ]; then
	plan_text="$(cat "$boot_config/plan.json")"
	printf '%s' "$plan_text" | jq -e . >/dev/null 2>&1 || die "$boot_config/plan.json is not valid JSON"
	plan="$(plan_from_file "$plan_text" "$have")"
	plan_origin="the plan of the Linux rescue system ($boot_config/plan.json)"
else
	plan="$(plan_from_layout "$have")"
	plan_origin="no plan.json in $boot_config: planned here from the layout"
fi
refused="$(printf '%s' "$plan" | jq -r '(.refused // []) | join("; ")')"
[ -z "$refused" ] || die "$refused"
refused="$(plan_check "$plan" | paste -sd ';' - | sed 's/;/; /g')"
[ -z "$refused" ] || die "$refused"
plan_firmware="$(printf '%s' "$plan" | jq -r '.firmware // empty')"
[ -z "$plan_firmware" ] || [ "$plan_firmware" = "$firmware" ] || warn "the plan was made for $plan_firmware firmware, and this system was started by $firmware; the boot loader goes onto every disk that has the partition for either"
disks="$(printf '%s' "$plan" | jq -r '.disks | map(.name) | join(" ")')"
ndisks="$(printf '%s' "$plan" | jq -r '.disks | length')"
pool="$(printf '%s' "$plan" | jq -r .pool.name)"
# What a disk left alone must not hold. A member of a pool with the new
# pool's name: the kernel takes its root pool by name, and started the
# old one in its place when this was tried. And a GPT label the plan
# makes: it would be found under /dev/gpt in the new partition's place.
for d in $(printf '%s' "$plan" | jq -r '.left[].name'); do
	for p in "$d" $(gpart show -p "$d" 2>/dev/null | awk -v d="$d" 'NR > 1 && index($3, d) == 1 {print $3}'); do
		[ "$(zdb -l "/dev/$p" 2>/dev/null | awk '/^ *name:/ {print $2; exit}')" != "'$pool'" ] || die "$p, on a disk the plan leaves alone, belongs to a pool named $pool, and so would the new system's: FreeBSD takes its root pool by name and may start the old one. Put the disk into the installation, or give the layout's zfs part another pool name (pool: NAME)"
	done
	theirs="$(gpart show -l "$d" 2>/dev/null | awk 'NR > 1 && $3 ~ /^[0-9]+$/ {print $4}')"
	for l in $(plan_labels "$plan"); do
		case " $(echo $theirs) " in *" $l "*) die "$d, which the plan leaves alone, has a partition with the GPT label $l, and so would the new system: FreeBSD finds one of the two under /dev/gpt/$l. Give that partition another label (gpart modify -i N -l NAME $d), or put the disk into the installation" ;; esac
	done
done
# what can be known of the users and the time zone here, where the
# base system is the one that will be installed
for u in $(printf '%s' "$spec" | jq -r '.users[].name'); do
	[ "$u" != root ] || continue
	uid="$(pw usershow "$u" 2>/dev/null | cut -d: -f3 || true)"
	[ -z "$uid" ] || { [ "$uid" -ge 1000 ] && [ "$uid" -lt 65534 ]; } || die "user $u is a system account of FreeBSD; choose another name"
done
[ ! -d /usr/share/zoneinfo ] || [ -f "/usr/share/zoneinfo/$timezone" ] || die "unknown time zone $timezone"

# --- the plan, printed -----------------------------------------------------

log "machine: $arch, $(echo "$firmware" | tr a-z A-Z) firmware, $(sysctl -n hw.ncpu) CPUs, $(( $(sysctl -n hw.realmem 2>/dev/null || sysctl -n hw.physmem) / 1048576 )) MB RAM"
log "image: $os for $arch from $image_url (profile ${profile_name:-?}, layout $layout_name)"
plan_lines "$plan" | while IFS= read -r line; do log "$line"; done
log "plan: $plan_origin"
log "network: $(net_plan); hostname $hostname; time zone $timezone"
log "interfaces: $(printf '%s\n' "$ifaces" | awk '{printf "%s%s (%s, %s%s)", sep, $1, $3, $2, ($4 != $1 ? ", now " $4 : ""); sep = ", "}')$([ "$rename" = yes ] || printf '; kernel names kept')"
if [ "$(printf '%s' "$privates" | jq length)" -gt 0 ]; then
	log "$(printf '%s' "$privates" | jq -r '(if length > 1 then "private networks: " else "private network: " end) + (map("\(.address) in \(.network) (\(.cidr))") | join(", "))'); recorded, no interface configured"
fi
log "users: $(printf '%s' "$spec" | jq -r '[.users[] | "\(.name) (\(.keys | length) key(s)\(if .sudo then ", sudo" else "" end))"] | join(", ")')"
log "userdata: $(printf '%s' "$spec" | jq -r '(.userdata // []) | if length == 0 then "none" else map(.name) | join(" ") end'); firewall: $(firewall_plan)"
tunables="$(loader_tunables | paste -sd ' ' -)"
log "loader tunables: ${tunables:-none}"
ignored="$(loader_ignored | paste -sd ' ' -)"
[ -z "$ignored" ] || warn "kernel.extra: $ignored: FreeBSD's loader takes tunables (NAME=VALUE); left out"
if [ "$dry" = 1 ]; then
	log "dry run: stopping here, no disk was touched"
	exit 0
fi

# --- the image: the manifest first, so that a missing image costs no disk ---

step="fetching the manifest"
log "fetching the manifest"
manifest=""
for try in 1 2 3; do
	if manifest="$(fetch -T 30 -qo - "$image_url/manifest.json" 2>/dev/null)" && [ -n "$manifest" ]; then break; fi
	manifest=""; [ "$try" = 3 ] || sleep 5
done
[ -n "$manifest" ] || die "cannot fetch $image_url/manifest.json; the storage must serve the image (the release's sets and the packages)"
printf '%s' "$manifest" | jq -e . >/dev/null 2>&1 || die "$image_url/manifest.json is not valid JSON"
m() { printf '%s' "$manifest" | jq -r "$1 // empty"; }
family="$(m .family)"
[ "$family" = freebsd ] || die "the manifest's family ${family:-image} is not freebsd: $os is for the installer of the Linux rescue system (install/install.sh)"
[ -z "$(m .unsupported)" ] || die "$os on $arch: $(m .unsupported)"
for f in base.txz kernel.txz packages.tar; do
	[ -n "$(m ".files[\"$f\"].sha256")" ] || die "the manifest names no $f checksum"
done
release="$(m .release)"
[ -n "$(m '.packages[] | select(.name == "pkg") | .file')" ] || die "the manifest's packages lack pkg itself"
# the pool is made by the release that will import it: one made by a
# newer ZFS may have features the installed system cannot read
here="$(uname -r | sed 's/-.*//')"
if [ "${release%%-*}" != "$here" ]; then
	[ "$force" = 1 ] || die "this rescue system is FreeBSD $here, and $os is ${release:-of no release the manifest names}: the pool is made by the release that will import it. The image's own rescue system is $(m .rescue)."
	warn "this rescue system is FreeBSD $here, and $os is $release; going on (--force)"
fi
log "image $os version $(m .version): FreeBSD $release; packages $(m '[.packages[] | select(.wanted) | .name] | join(" ")') ($(m '.packages | length') with what they depend on)"
refused="$(plan_check "$plan" "$(m '.unpacked_mb | numbers')" | paste -sd ';' - | sed 's/;/; /g')"
[ -z "$refused" ] || die "$refused"
# a shell is the base system's or a package's (/bin/bash is the
# package's, /usr/local/bin/bash)
for sh in $(printf '%s' "$spec" | jq -r '.users[] | select(.name != "root") | .shell // "/bin/bash"' | sort -u); do
	[ -x "$sh" ] || [ -n "$(m ".packages[] | select(.name == \"${sh##*/}\") | .name")" ] || die "no shell $sh for the users: FreeBSD's base has none, and the image's packages have no ${sh##*/}"
done
if [ "$(printf '%s' "$spec" | jq '(.userdata // []) | length')" -gt 0 ]; then
	[ -n "$(m '.packages[] | select(.name == "bash") | .name')" ] || die "the userdata is run by bash, and the image's packages have none"
fi

# --- the disks -------------------------------------------------------------

# zero_ends PROVIDER: zeroes its first and last MiB (all of one no larger
# than two), where a partition table, a pool or a file system from
# before would be found again
zero_ends() {
	local info sector size whole rest
	info="$(diskinfo "$1")"
	set -- "$1" $info
	sector="$3"; size="$4"
	whole=$((size / 1048576)); rest=$(( (size % 1048576) / sector ))
	if [ "$whole" -gt 2 ]; then
		dd if=/dev/zero of="/dev/$1" bs=1048576 count=1 2>/dev/null
		dd if=/dev/zero of="/dev/$1" bs=1048576 oseek=$((whole - 1)) count=1 2>/dev/null
	elif [ "$whole" -gt 0 ]; then
		dd if=/dev/zero of="/dev/$1" bs=1048576 count="$whole" 2>/dev/null
	fi
	# what is past the last whole MiB
	[ "$rest" = 0 ] || dd if=/dev/zero of="/dev/$1" bs="$sector" oseek=$((whole * 1048576 / sector)) count="$rest" 2>/dev/null
}
# clear_disk DISK: its partition table destroyed, whatever kind it is
clear_disk() {
	gpart destroy -F "$1" >/dev/null 2>&1 || true
	zero_ends "$1"
}
# run_commands: the commands on stdin, a line each, as disk_commands and
# pool_commands give them
run_commands() {
	local line
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		$line </dev/null >/dev/null || die "failed: $line"
	done
}

step="releasing the disks"
log "releasing the disks"
# what an earlier attempt in this rescue system left
umount -f "$TARGET/dev" 2>/dev/null || true
umount -f "$ESP_MOUNT" 2>/dev/null || true
for p in $(zpool list -H -o name 2>/dev/null || true); do zpool export -f "$p" || die "cannot export the pool $p, which holds a disk"; done
swapoff -a >/dev/null 2>&1 || true

step="partitioning"
log "partitioning"
wrote=1
commands="$(disk_commands "$plan")"
set -f
run_commands <<EOF
$commands
EOF
set +f
for d in $disks; do gpart show -l "$d" 2>/dev/null | sed '/^$/d; s/^/    /' || true; done

# --- the pool and its datasets -------------------------------------------

step="making the pool"
log "pool $pool ($(printf '%s' "$plan" | jq -r .pool.vdev))"
# on a small machine the ARC must leave room for the memory root and the
# installer: a quarter of the memory, where it would take all it finds
physmem_mb=$(( $(sysctl -n hw.physmem) / 1048576 ))
if [ "$physmem_mb" -lt 4096 ]; then
	arc=$(( (physmem_mb / 4 > 64 ? physmem_mb / 4 : 64) * 1048576 ))
	sysctl -q vfs.zfs.arc.max="$arc" >/dev/null 2>&1 || sysctl -q vfs.zfs.arc_max="$arc" >/dev/null 2>&1 || true
fi
for l in $(plan_labels "$plan"); do
	n=0; until [ -e "/dev/gpt/$l" ]; do n=$((n + 1)); [ "$n" -lt 20 ] || die "the partition with the GPT label $l did not appear"; sleep 0.5; done
done
commands="$(pool_commands "$plan")"
pool_made="$pool"
set -f
run_commands <<EOF
$commands
EOF
set +f
chmod 1777 "$TARGET/tmp" "$TARGET/var/tmp"

# --- the sets: straight from the download onto the pool --------------------

# unpack FILE DIR: nothing of it is kept in the rescue system's memory;
# the SHA-256 is taken on the way and compared with the manifest's
unpack() {
	local want got
	want="$(m ".files[\"$1\"].sha256")"
	rm -f /tmp/ump-sum.$$ /tmp/ump-sum.$$.fifo
	mkfifo /tmp/ump-sum.$$.fifo
	sha256 -q < /tmp/ump-sum.$$.fifo > /tmp/ump-sum.$$ &
	# (a download that fails anywhere in the pipe fails it)
	( set -o pipefail; fetch -T 60 -qo - "$image_url/$1" | tee /tmp/ump-sum.$$.fifo | tar -xpf - -C "$2" ) || die "cannot download and unpack $image_url/$1"
	wait
	got="$(cat /tmp/ump-sum.$$)"
	rm -f /tmp/ump-sum.$$ /tmp/ump-sum.$$.fifo
	[ "$got" = "$want" ] || die "checksum mismatch for $1: the manifest says $want, the download has $got"
}
for f in base.txz kernel.txz; do
	step="unpacking $f"
	log "downloading and unpacking $f"
	unpack "$f" "$TARGET"
done

# --- the machine's own configuration -----------------------------------------

step="configuring the system"
log "configuring $hostname"
[ -f "$TARGET/usr/share/zoneinfo/$timezone" ] || die "unknown time zone $timezone"
cp "$TARGET/usr/share/zoneinfo/$timezone" "$TARGET/etc/localtime"
echo "$timezone" > "$TARGET/var/db/zoneinfo"
fstab_text "$plan" > "$TARGET/etc/fstab"
mkdir -p "$TARGET/boot/efi"
loader_conf > "$TARGET/boot/loader.conf"
rc_conf > "$TARGET/etc/rc.conf"
hosts_text > "$TARGET/etc/hosts"
rm -f "$TARGET/etc/resolv.conf" "$TARGET/etc/resolvconf.conf"
[ -z "$dns" ] || resolv_conf > "$TARGET/etc/resolv.conf"
[ "$dns_source" = dhcp ] || resolvconf_conf > "$TARGET/etc/resolvconf.conf"
if [ "$rename" = yes ]; then
	mkdir -p "$TARGET/usr/local/etc/rc.d"
	ifnames_script > "$TARGET/usr/local/etc/rc.d/ump_ifnames"
	chmod 555 "$TARGET/usr/local/etc/rc.d/ump_ifnames"
fi
# sshd: keys alone, and root only when the profile lists it
mkdir -p "$TARGET/etc/ssh/sshd_config.d"
if ! grep -q '^Include /etc/ssh/sshd_config.d/\*\.conf' "$TARGET/etc/ssh/sshd_config"; then
	{ echo 'Include /etc/ssh/sshd_config.d/*.conf'; cat "$TARGET/etc/ssh/sshd_config"; } > "$TARGET/etc/ssh/sshd_config.ump"
	mv "$TARGET/etc/ssh/sshd_config.ump" "$TARGET/etc/ssh/sshd_config"
fi
sshd_conf > "$TARGET/etc/ssh/sshd_config.d/50-ump.conf"
ssh-keygen -A -f "$TARGET" >/dev/null
# what the first boot seeds its random numbers from, as bsdinstall leaves it
for f in /entropy /boot/entropy; do
	( umask 077; dd if=/dev/random of="$TARGET$f" bs=4096 count=1 2>/dev/null )
done

# --- the boot loader: on every disk that has the partition for it ---------------

step="installing the boot loader"
log "installing the boot loader"
[ -f "$TARGET/boot/loader.efi" ] || die "the new system has no /boot/loader.efi"
mkdir -p "$ESP_MOUNT"
for l in $(plan_labels "$plan"); do
	case "$l" in efi*) ;; *) continue ;; esac
	newfs_msdos -F 32 -c 1 -L EFISYS "/dev/gpt/$l" >/dev/null 2>&1 || die "newfs_msdos on /dev/gpt/$l failed"
	mount -t msdosfs "/dev/gpt/$l" "$ESP_MOUNT" || die "cannot mount /dev/gpt/$l"
	mkdir -p "$ESP_MOUNT/EFI/BOOT"
	# the removable path, which every firmware tries without a boot entry;
	# none is written, so the firmware's boot order stays as it is
	cp "$TARGET/boot/loader.efi" "$ESP_MOUNT/EFI/BOOT/$efi_name"
	umount "$ESP_MOUNT"
done
if [ "$arch" = amd64 ]; then
	printf '%s' "$plan" | jq -r '.disks[] | .name as $d | .parts[] | select(.kind == "bios") | "\($d) \(.index)"' | while read -r d i; do
		gpart bootcode -b "$TARGET/boot/pmbr" -p "$TARGET/boot/gptzfsboot" -i "$i" "$d" >/dev/null || die "gpart bootcode on $d failed"
	done
fi

# --- packages: from the storage, added without FreeBSD's repository ---------------

step="adding the packages"
log "packages: $(m '[.packages[] | select(.wanted) | .name] | join(" ")') (from the storage)"
mount -t devfs devfs "$TARGET/dev"
packages=/var/tmp/ump-packages
mkdir -p "$TARGET$packages"
unpack packages.tar "$TARGET$packages"
pkg_log="$TARGET/var/log/ump-pkg.log"
: > "$pkg_log"
export ASSUME_ALWAYS_YES=yes
# pkg itself by the rescue system's pkg-static, then the wanted ones by
# the new system's pkg, which finds what they depend on beside them
pkg_file() { m ".packages[] | select(.name == \"$1\") | .file"; }
pkg-static -c "$TARGET" add "$packages/$(pkg_file pkg)" >> "$pkg_log" 2>&1 || { tail -n 20 "$pkg_log" >&2; die "pkg add pkg failed"; }
for p in $(m '.packages[] | select(.wanted and .name != "pkg") | .name'); do
	chroot "$TARGET" /usr/local/sbin/pkg add "$packages/$(pkg_file "$p")" >> "$pkg_log" 2>&1 || { tail -n 20 "$pkg_log" >&2; die "pkg add $p failed"; }
done
rm -rf "$TARGET$packages"

# --- users --------------------------------------------------------------------

step="making the users"
log "users"
# no password opens root: the base system's root has an empty one
pw -R "$TARGET" usermod root -h -
nusers="$(printf '%s' "$spec" | jq '.users | length')"
n=0
while [ "$n" -lt "$nusers" ]; do
	u="$(printf '%s' "$spec" | jq -c ".users[$n]")"
	name="$(printf '%s' "$u" | jq -r .name)"; shell="$(printf '%s' "$u" | jq -r '.shell // "/bin/bash"')"; sudo="$(printf '%s' "$u" | jq -r '.sudo // false')"
	if [ "$name" = root ]; then
		home=/root
	else
		# a shell of the other systems is the package's here
		if [ ! -x "$TARGET$shell" ]; then
			[ -x "$TARGET/usr/local/bin/${shell##*/}" ] || die "user $name: the new system has no shell $shell"
			shell="/usr/local/bin/${shell##*/}"
		fi
		if entry="$(pw -R "$TARGET" usershow "$name" 2>/dev/null)"; then
			uid="$(printf '%s' "$entry" | cut -d: -f3)"
			[ "$uid" -ge 1000 ] && [ "$uid" -lt 65534 ] || die "user $name is a system account of FreeBSD; choose another name"
		else
			pw -R "$TARGET" useradd "$name" -m -s "$shell" -c "$name" -h - || die "pw useradd $name failed"
		fi
		home="$(pw -R "$TARGET" usershow "$name" | cut -d: -f9)"
		for g in $(printf '%s' "$u" | jq -r '.groups // [] | .[]'); do
			pw -R "$TARGET" groupshow "$g" >/dev/null 2>&1 || pw -R "$TARGET" groupadd "$g" || die "pw groupadd $g failed"
			pw -R "$TARGET" groupmod "$g" -m "$name" || die "pw groupmod $g failed"
		done
		if [ "$sudo" = true ]; then
			pw -R "$TARGET" groupmod wheel -m "$name" || die "pw groupmod wheel failed"
			mkdir -p "$TARGET/usr/local/etc/sudoers.d"
			printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$name" > "$TARGET/usr/local/etc/sudoers.d/90-ump-$name"
			chmod 440 "$TARGET/usr/local/etc/sudoers.d/90-ump-$name"
		fi
	fi
	mkdir -p -m 700 "$TARGET$home/.ssh"
	printf '%s' "$u" | jq -r '.keys[]' | awk '!seen[$0]++' > "$TARGET$home/.ssh/authorized_keys"
	chmod 600 "$TARGET$home/.ssh/authorized_keys"
	[ "$name" = root ] || chroot "$TARGET" chown -R "$name:$name" "$home/.ssh"
	n=$((n + 1))
done
# root over SSH only when the profile lists it with keys
rm -f "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
if printf '%s' "$spec" | jq -e '[.users[] | select(.name == "root")] | length > 0' >/dev/null; then
	printf '# UMP: the profile lists root with keys.\nPermitRootLogin prohibit-password\n' > "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
	log "ssh: root logs in with keys; administrator $admin"
else
	log "ssh: root cannot log in; administrator $admin (sudo)"
fi

# --- userdata: scripts, run in the new system before its first boot ---------------

nud="$(printf '%s' "$spec" | jq '(.userdata // []) | length')"
if [ "$nud" -gt 0 ]; then
	step="running the userdata"
	mkdir -p "$TARGET/etc/ump/userdata"
	# the network for the chroot: the resolver of the rescue system
	# serves it; nothing a package brings is started there
	[ ! -f "$TARGET/etc/resolv.conf" ] || mv "$TARGET/etc/resolv.conf" "$TARGET/etc/resolv.conf.ump"
	cp /etc/resolv.conf "$TARGET/etc/resolv.conf"
	n=0
	while [ "$n" -lt "$nud" ]; do
		ud_name="$(printf '%s' "$spec" | jq -r ".userdata[$n].name")"
		ud_file="/etc/ump/userdata/$(printf '%02d' "$((n + 1))")-$ud_name.sh"
		printf '%s' "$spec" | jq -r ".userdata[$n].script" > "$TARGET$ud_file"
		chmod 700 "$TARGET$ud_file"
		log "userdata: $ud_name ($ud_file; its output in /var/log/ump-userdata.log)"
		if ! ud_out="$(chroot "$TARGET" /usr/bin/env PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin ASSUME_ALWAYS_YES=yes \
			UMP_USERDATA="$ud_name" UMP_HOSTNAME="$hostname" UMP_OS="$os" UMP_PROFILE="$profile_name" UMP_ADMIN="$admin" /usr/local/bin/bash "$ud_file" 2>&1)"; then
			printf '%s\n' "$ud_out" >&2; die "userdata $ud_name failed"
		fi
		printf '=== userdata %s (%s) at %s ===\n%s\n' "$ud_name" "$ud_file" "$(date -u +%FT%TZ)" "$ud_out" >> "$TARGET/var/log/ump-userdata.log"
		n=$((n + 1))
	done
	rm -f "$TARGET/etc/resolv.conf"
	[ ! -f "$TARGET/etc/resolv.conf.ump" ] || mv "$TARGET/etc/resolv.conf.ump" "$TARGET/etc/resolv.conf"
fi

# --- firewall: pf -----------------------------------------------------------------

if [ "$firewall" = drop ]; then
	step="writing the firewall"
	log "firewall: pf, in $(firewall_plan)"
	pf_conf > "$TARGET/etc/pf.conf"
	chmod 644 "$TARGET/etc/pf.conf"
	kldload -n pf 2>/dev/null || true
	if ! out="$(pfctl -nf "$TARGET/etc/pf.conf" 2>&1)"; then
		printf '%s\n' "$out" >&2
		die "the firewall rules in /etc/pf.conf are wrong (see above)"
	fi
fi

# --- the record ---------------------------------------------------------------------

step="writing the record"
mkdir -p "$TARGET/etc/ump"
printf '%s\n' "$spec" | jq . > "$TARGET/etc/ump/spec.json"
chmod 600 "$TARGET/etc/ump/spec.json"
image_version="$(chroot "$TARGET" freebsd-version -u 2>/dev/null || echo "$release")"
image_kernel="$(chroot "$TARGET" freebsd-version -k 2>/dev/null || echo "$release")"
record_text "$plan" "$image_version" "$image_kernel" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$TARGET/etc/ump/install"
chmod 644 "$TARGET/etc/ump/install"
# the record for the caller (--record): the installation and the host
# keys of the new system, read before the pool is exported
if [ -n "$record" ]; then
	mkdir -p "$record"
	rm -f "$record/install" "$record"/ssh_host_*_key.pub
	cp "$TARGET/etc/ump/install" "$record/install"
	cp "$TARGET"/etc/ssh/ssh_host_*_key.pub "$record/"
	log "record: $record"
fi

step="exporting the pool"
umount "$TARGET/dev" 2>/dev/null || umount -f "$TARGET/dev"
used="$(zfs list -H -o used "$pool")"
if ! zpool export "$pool" 2>/dev/null; then
	# something userdata started still runs in the new system
	warn "the pool $pool is busy; exporting it by force"
	zpool export -f "$pool" || die "cannot export the pool $pool"
fi
pool_made=""
log "installed: $os $image_version as $hostname, root on $pool/ROOT/default ($used used; layout $layout_name over $ndisks disk(s)); reboot to start it"
