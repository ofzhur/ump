#!/usr/bin/env bash
# Check what install/install.sh plans for FreeBSD, without a machine: its
# disk selection, its plan of a layout with a zfs part and the functions
# of its FreeBSD section run on the machines of testdata/freebsd, one
# directory each, and what they make of it is compared with the
# directory's expected: the plan as people read it, what the manifests
# of the system and of its rescue image add, and the boot config the
# FreeBSD rescue system would be started with: ump/cmdline,
# ump/authorized_keys and ump/plan.json, by which the installer for
# FreeBSD finds the disks again; or the refusal, with its exit status (2:
# nothing was written). Then the staging itself, on the installer's own
# functions: what make_layout runs for FreeBSD, command by command, and
# that a checksum which does not match stops it (guard_stage).
#
# A case is what the installer reads on a real machine:
#   lsblk.json    lsblk -J -b -d -o PATH,TYPE,RO,TRAN,SIZE,ROTA,MODEL,SERIAL,WWN,LOG-SEC
#                 (fields that are null left out)
#   case          the machine, the rescue system that runs on it and the
#                 specification, as shell, where they differ from the
#                 defaults (defaults, below): efi (1, 0), arch, memory
#                 (MB), layout (a name of profiles/layouts.yaml), wanted
#                 (--disks, space-separated), os, family, mitigations,
#                 kernel_extra; mac, ip4 and ip6, the uplink as the
#                 installer found it; words, what the rescue system was
#                 started with; resolvers, hostname, keys (how many root
#                 has); record (--record); and of the storage:
#                 unpacked_mb, rescue (the name the system's manifest
#                 gives), rescue_system, rescue_features,
#                 rescue_memory_mb, rescue_disk_mb, rescue_there (0: the
#                 storage has no such image)
#   layout.json   a layout of its own instead of a published one
#                 ({"name": ..., "parts": [...]}, as a specification has it)
#   was/dev/NAME  what an earlier attempt left on /dev/NAME: the path of
#                 its partition umpboot, then the boot log found there
#   holds/dev/NAME.json  what /dev/NAME and its partitions hold, where it
#                 matters (a disk that is left alone): fstype, label and
#                 partlabel of each, a JSON array
#
# Usage: test-freebsd-plan.sh [CASE...]   (default: every case, and the guard)
# Needs bash 4 or later and jq. VERBOSE=1 names every case; UPDATE=1 writes
# the expected files instead of comparing (review the diff).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
CASES="$HERE/testdata/freebsd"
INSTALL="$ROOT/install/install.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-freebsd-plan-test.XXXXXX")"
WORK="$(cd "$WORK" && pwd -P)"
trap 'command rm -rf "$WORK"' EXIT
[ "${BASH_VERSINFO[0]}" -ge 4 ] || { echo "test-freebsd-plan.sh: bash 4 or later is needed (this is $BASH_VERSION)" >&2; exit 2; }
command -v jq >/dev/null || { echo "test-freebsd-plan.sh: jq is needed" >&2; exit 2; }
command -v sha256sum >/dev/null || sha256sum() { shasum -a 256 "$@"; }

# fn NAME: a function of install.sh that starts in the first column
# (with the functions that are defined inside it; a here-document's lines
# are no code)
fn() {
	awk -v f="$1" '
		!on && ($0 == f "() {" || index($0, f "() { ") == 1) {on = 1}
		on {print}
		here != "" {if ($0 == here) here = ""; next}
		match($0, /<<-?'"'"'?[A-Z]+'"'"'?/) {here = substr($0, RSTART, RLENGTH); gsub(/[<'"'"'-]/, "", here)}
		on && /^[a-z_]+\(\) \{/ && !/\}[ \t]*(#.*)?$/ {depth++}
		on && $0 == "}" {if (--depth == 0) exit}' "$INSTALL"
}
# section START: a section of install.sh, from the line that starts with
# "# --- START" to the next section
section() { awk -v s="# --- $1" 'index($0, s) == 1 && !on {on = 1; next} on && /^# --- /{exit} on' "$INSTALL"; }

# what the sections call of the rest of install.sh: its own die, which
# says whether anything was written
log() { printf '==> %s\n' "$*"; }
warn() { echo "install.sh: warning: $*"; }
eval "$(fn die)"
[ "$(type -t die)" = function ] || { echo "test-freebsd-plan.sh: no die() in install/install.sh" >&2; exit 2; }
FB="$(section "FreeBSD: ")"
DISKS="$(section "the disks ")"
PLAN="$(section "the plan: ")"
[ -n "$FB" ] && [ -n "$DISKS" ] && [ -n "$PLAN" ] || { echo "test-freebsd-plan.sh: install/install.sh lacks its FreeBSD, disks or plan section" >&2; exit 2; }
# the line that says whether a layout is FreeBSD's
FBLINE="$(awk '/^fb=0; fb_wrote=0$/ {print; getline; print}' "$INSTALL")"
[ -n "$FBLINE" ] || { echo "test-freebsd-plan.sh: install/install.sh does not set fb where it did" >&2; exit 2; }
eval "$FB"
eval "$(fn print_layout)"
eval "$(fn fetch_manifest)"

# Whatever could change a disk is a function here that only writes its
# name down: a plan made with one of them called fails.
CALLS="$WORK/calls"
for c in mount umount mkfs.ext4 mkfs.xfs mkfs.btrfs mkfs.vfat mkswap wipefs sgdisk mdadm dd pvcreate vgcreate lvcreate vgchange partprobe dmsetup swapoff zstd; do
	eval "$c() { echo \"$c \$*\" >> \"\$CALLS\"; }"
done

# --- the facts of a case ---------------------------------------------------

# lsblk and blockdev answer from the case's lsblk.json what the installer
# asks them about whole disks
lsblk() {
	local cols="" dev=""
	while [ $# -gt 0 ]; do
		case "$1" in
		-o) cols="$2"; shift 2 ;;
		-*) shift ;;
		*) dev="$1"; shift ;;
		esac
	done
	jq -r --arg cols "$cols" --arg dev "$dev" '
		def human: if . >= 1099511627776 then [. / 1099511627776, "T"] elif . >= 1073741824 then [. / 1073741824, "G"] elif . >= 1048576 then [. / 1048576, "M"] else [. / 1024, "K"] end
			| ((.[0] * 10 | round) / 10 | tostring) + .[1];
		.blockdevices[] | select($dev == "" or .path == $dev) | . as $d
		| [$cols | split(",")[] | ascii_downcase
		   | if . == "size" then ($d.size | human) elif . == "ro" or . == "rota" then (if $d[.] then "1" else "0" end) else ($d[.] // "" | tostring) end]
		| join(" ")' "$CASE/lsblk.json"
}
blockdev() {
	case "$1" in
	--getsize64) jq -r --arg d "$2" '.blockdevices[] | select(.path == $d) | .size' "$CASE/lsblk.json" ;;
	--getss) jq -r --arg d "$2" '.blockdevices[] | select(.path == $d) | ."log-sec" // 512' "$CASE/lsblk.json" ;;
	*) echo "blockdev $*" >> "$CALLS" ;;
	esac
}
is_block() { jq -e --arg d "$1" 'any(.blockdevices[]; .path == $d)' "$CASE/lsblk.json" >/dev/null; }
fb_fact_memory() { echo "$memory"; }
fb_fact_words() { echo "$words"; }
fb_fact_hostname() { echo "$hostname"; }
fb_fact_resolvers() { echo "$resolvers"; }
fb_fact_keys() { local i; for ((i = 1; i <= keys; i++)); do echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTe$i key-$i@test"; done; }
fb_fact_time() { echo 1795708800; }
fb_fact_was() { if [ -f "$CASE/was$1" ]; then cat "$CASE/was$1"; fi; }
fb_fact_holds() { if [ -f "$CASE/holds$1.json" ]; then jq -c . "$CASE/holds$1.json"; else echo '[]'; fi; }
# the storage of a case: the system's manifest, and that of the rescue
# image it names
curl() {
	local url="${!#}"
	case "$url" in
	"$storage/os/$os/$arch/manifest.json")
		jq -n --arg os "$os" --arg family "$family" --arg arch "$arch" --arg rescue "$rescue" --argjson unpacked "$unpacked_mb" '
			{version: "2026.11.26", os: $os, arch: $arch, unpacked_mb: $unpacked}
			+ (if $family == "freebsd" then {family: $family, release: "15.1-RELEASE", kernel: "15.1-RELEASE GENERIC", files: {"base.txz": {sha256: "5b7a", size: 1}, "kernel.txz": {sha256: "05f7", size: 1}, "packages.tar": {sha256: "9463", size: 1}}}
			   else {kernel: "7.0.0-14-generic", files: {"rootfs.tar.zst": {sha256: "6f1e", size: 1}}} end)
			+ (if $rescue != "" then {rescue: $rescue} else {} end)' ;;
	"$storage/rescue/$rescue/$arch/manifest.json")
		[ "$rescue_there" = 1 ] || { echo "curl: (22) The requested URL returned error: 404" >&2; return 22; }
		jq -n --arg system "$rescue_system" --arg arch "$arch" --arg features "$rescue_features" --argjson memory "$rescue_memory_mb" --argjson disk "$rescue_disk_mb" '
			{version: "2026.11.26", release: "15.1", arch: $arch, kernel: "15.1-RELEASE GENERIC", features: ($features | split(" ")), memory_mb: $memory, disk_mb: $disk,
			 files: {"disk.img.zst": {sha256: "99b4666494b93270957c6705cecdeb0a6396bd5e810e72ca30ca2803ea41cf6c", size: 95791290}}}
			+ (if $system != "" then {system: $system} else {} end)' ;;
	*) echo "curl: (22) The requested URL returned error: 404" >&2; return 22 ;;
	esac
}

# layout_json NAME: a layout of profiles/layouts.yaml as a specification
# carries it. The parts there are YAML flow mappings, one line each,
# which sed turns into JSON.
layout_json() {
	awk -v n="  $1:" '$0 == n {on = 1; next} on && /^  [a-z]/ {exit} on && /^      - \{/ {sub(/^      - /, ""); print}' "$ROOT/profiles/layouts.yaml" \
		| sed -E 's/([{,] *)([a-z]+): /\1"\2": /g; s/: ([^]["{},][^,}]*)/: "\1"/g; s/\[([a-z]+)\]/["\1"]/g' \
		| jq -s -c --arg name "$1" '{name: $name, parts: map(with_entries(if .key == "index" or .key == "raid" then .value |= tonumber else . end))} | if (.parts | length) == 0 then error("no such layout") else . end'
}

# --- the cases ---------------------------------------------------------------

# defaults: a dedicated server with UEFI firmware, in the UMP rescue
# system as ump server rescue starts it, and the storage as it is
# published
defaults() {
	efi=1; arch=amd64; memory=64224; layout=zfs; wanted=""; os=freebsd-15.1; family=freebsd; mitigations=false; kernel_extra=""
	mac=0c:c4:7a:aa:bb:11; ip4=203.0.113.10/26,203.0.113.1; ip6=""
	words="console=ttyS0,115200n8 console=tty0 ump.hostname=web-01 ump.mac=0c:c4:7a:aa:bb:11 ump.ip=203.0.113.10/26,203.0.113.1 ump.dns=1.1.1.1,8.8.8.8 ump.authorized_keys=c3NoLWVkMjU1MTkK"
	resolvers="1.1.1.1,8.8.8.8"; hostname=web-01; keys=2; record=""
	unpacked_mb=747; rescue=freebsd-15.1; rescue_system=freebsd; rescue_features="boot-config"; rescue_memory_mb=619; rescue_disk_mb=194; rescue_there=1
}

# plan_case: what the installer makes of $CASE, as it prints and writes it
plan_case() {
	local l
	defaults
	. "$CASE/case"
	if [ -f "$CASE/layout.json" ]; then l="$(jq -c . "$CASE/layout.json")"; else l="$(layout_json "$layout")"; fi
	storage=https://ump.example.net/dl; image_url="$storage/os/$os/$arch"; keep=0; dry=0
	spec="$(jq -n -c --arg os "$os" --argjson layout "$l" --arg wanted "$wanted" '{os: $os, layout: $layout, disks: ($wanted | split(" ") | map(select(. != ""))), userdata: []}')"
	layout_name="$(jq -r '.layout.name // "inline"' <<< "$spec")"
	eval "$FBLINE"
	: > "$CALLS"
	eval "$DISKS"
	eval "$PLAN"
	rescue_mac="$mac"; rescue_ip4="$ip4"; rescue_ip6="$ip6"
	print_layout
	if [ "$fb" = 1 ]; then log "loader tunables: $(fb_tunables)"; fi
	fb_was
	fetch_manifest
	echo "--- ump/cmdline"
	fb_cmdline
	echo "--- ump/authorized_keys"
	fb_fact_keys
	echo "--- ump/plan.json"
	fb_plan
	echo "--- the boot image goes to"
	fb_image_disks
	[ ! -s "$CALLS" ] || { echo "the plan called what could change a disk:"; cat "$CALLS"; return 1; }
	# what the plan's own rules must hold, whatever the machine: every disk
	# once, no partition number, kind or rest twice on a disk, the pool on
	# a disk at least, and the image on disks that get partitions alone
	fb_plan | jq -e --arg image "$(fb_image_disks | paste -sd' ' -)" '
		([.disks[].name] + [.left[].name] | length == (unique | length))
		and (.disks | all(.parts | ([.[].index] | length == (unique | length)) and ([.[].kind] | length == (unique | length)) and ([.[] | select(.size == "rest")] | length <= 1)))
		and ([.disks[] | select(any(.parts[]; .kind == "zfs"))] | length >= 1)
		and (.version == 1) and (.pool.mountpoint == "/") and ([.disks[].name] | index($ARGS.named.boot) != null)
		and ([.disks[] | select(.parts | length > 0) | .name] as $d | $image | split(" ") | length > 0 and all(. as $x | $d | index($x) != null))' --arg boot "$boot_disk" >/dev/null \
		|| { echo "the plan names a disk twice, a partition number or kind twice on a disk, no pool, or the image goes to a disk without partitions"; return 1; }
}

failed=0 n=0
JOBS="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)"
NAMES=(); EXPECTED=()
run() { # run NAME EXPECTED COMMAND...: the command's output for NAME, side by side with the others'
	local name="$1"; NAMES+=("$1"); EXPECTED+=("$2"); shift 2
	while [ "$(jobs -rp | wc -l)" -ge "$JOBS" ]; do sleep 0.1; done
	(
		CALLS="$WORK/$name.calls"
		set +e
		( set -e; "$@" ) > "$WORK/$name.out" 2>&1
		rc=$?
		[ "$rc" = 0 ] || echo "(exit status $rc)" >> "$WORK/$name.out"
	) &
}
compare() { # compare NAME EXPECTED: its output with the case's expected
	local name="$1" expected="$2" out="$WORK/$1.out"
	n=$((n + 1))
	if [ "${UPDATE:-0}" = 1 ]; then
		command cp "$out" "$expected"
		echo "wrote   $name"
	elif [ ! -f "$expected" ]; then
		echo "MISSING $name (UPDATE=1 writes it)"; failed=$((failed + 1))
	elif diff -u "$expected" "$out" > "$WORK/$name.diff"; then
		[ "${VERBOSE:-0}" = 0 ] || echo "ok      $name"
	else
		echo "FAIL    $name"; sed 's/^/        /' "$WORK/$name.diff"; failed=$((failed + 1))
	fi
}

# --- the guard ---------------------------------------------------------------

# guard_stage: what make_layout does for FreeBSD, on the machine of a
# case: the commands that write, as it calls them, and what it puts on
# the partition umpboot of each disk; then the same with a download whose
# checksum is another, which must stop it; and that with --keep-data it
# ends at once
guard_stage() {
	local rc box c
	CASE="$CASES/mirror-two-nvme"
	[ -f "$CASE/case" ] || { echo "no case mirror-two-nvme"; return 1; }
	( set -e; plan_case ) > /dev/null || { echo "no plan"; return 1; }
	defaults; . "$CASE/case"
	storage=https://ump.example.net/dl; keep=0; family=freebsd
	spec="$(jq -n -c --argjson layout "$(layout_json "$layout")" '{layout: $layout, disks: []}')"; layout_name="$layout"
	eval "$FBLINE"; eval "$DISKS"; eval "$PLAN"
	rescue_mac="$mac"; rescue_ip4="$ip4"; rescue_ip6="$ip6"
	fb_rescue_url="$storage/rescue/$rescue/$arch"
	eval "$(fn make_layout)"
	[ "$(type -t make_layout)" = function ] || { echo "no make_layout() in install.sh"; return 1; }
	box="$WORK/stage"; command mkdir -p "$box"
	mktemp() { if [ "${1:-}" = -d ]; then command mkdir -p "$box/conf"; echo "$box/conf"; else echo "$box/sum"; fi; }
	shown() { sed "s|$box|BOX|g"; }
	# the download, and the tools between it and the disk (dd is done when
	# the download is)
	curl() { echo "curl $*" | shown >> "$CALLS"; printf 'the image'; }
	zstd() { cat; }
	dd() { cat > /dev/null; echo "dd $*" >> "$CALLS"; }
	lsblk() { case "$*" in *PARTLABEL*) echo "${!#}p1 umpboot" ;; esac; }
	udevadm() { :; }; sync() { :; }
	for c in sgdisk partprobe mount; do eval "$c() { echo \"$c \$*\" | shown >> \"\$CALLS\"; }"; done
	# what is on the partition when it is unmounted
	umount() {
		echo "umount $*" | shown >> "$CALLS"
		(cd "$1" && find . -type f | LC_ALL=C sort | while read -r c; do echo "  on it: ${c#./} ($(wc -c < "$c" | tr -d ' ') bytes)"; done) >> "$CALLS"
		command rm -rf "$1/ump"
	}
	try() { # try WHAT: make_layout in a shell of its own, and what it called
		: > "$CALLS"
		set +e; ( set -e; make_layout ) > "$CALLS.out" 2>&1; rc=$?; set -e
		echo "=== $1: status $rc"
		shown < "$CALLS.out"
		echo "it ran:"
		sed 's/^/  /' "$CALLS"
		echo "left behind: $(cd "$box" && find . -mindepth 1 | LC_ALL=C sort | paste -sd' ' -)"
		command rm -rf "$box"; command mkdir -p "$box"
	}
	fb_wrote=1
	fb_image_sum="$(printf 'the image' | sha256sum | cut -d' ' -f1)"
	try "the boot image and the boot config, on every disk of the mirror"
	[ "$rc" = 0 ] || { echo "  NOT AS IT MUST BE"; return 1; }
	fb_image_sum=99b4666494b93270957c6705cecdeb0a6396bd5e810e72ca30ca2803ea41cf6c
	try "a download with another checksum than the manifest's"
	[ "$rc" = 1 ] && ! grep -q '^sgdisk\|^mount' "$CALLS" || { echo "  NOT AS IT MUST BE"; return 1; }
	keep=1; kd_wrote=0
	try "with --keep-data"
	[ "$rc" = 2 ] && [ ! -s "$CALLS" ] || { echo "  NOT AS IT MUST BE"; return 1; }
}

only=("$@")
wanted_case() { [ ${#only[@]} -eq 0 ] || printf '%s\n' "${only[@]}" | grep -qxF "$1"; }
[ "${UPDATE:-0}" != 1 ] || command mkdir -p "$CASES"
for CASE in "$CASES"/*/; do
	CASE="${CASE%/}"
	[ -f "$CASE/case" ] || continue
	wanted_case "$(basename "$CASE")" || continue
	run "$(basename "$CASE")" "$CASE/expected" plan_case
done
if wanted_case guard_stage; then run guard_stage "$CASES/guard_stage" guard_stage; fi
wait
for i in "${!NAMES[@]}"; do compare "${NAMES[$i]}" "${EXPECTED[$i]}"; done
[ "$n" -gt 0 ] || { echo "test-freebsd-plan.sh: no such case: $*" >&2; exit 2; }
if [ "$failed" -gt 0 ]; then echo "$failed of $n cases failed"; exit 1; fi
echo "test-freebsd-plan.sh: all $n cases as expected"
