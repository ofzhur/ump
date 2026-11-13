#!/usr/bin/env bash
# Check the keep-data plan of install/install.sh (--keep-data), without a
# machine: the functions of its keep-data section run on the machines of
# testdata/keep-data, one directory each, and what they make of it is
# compared with the directory's expected: the plan as people read it,
# the new /etc/fstab and /etc/crypttab, and the plan as JSON with its
# hash; or the refusal. Then the guards, on the installer itself: format()
# makes anew only what the confirmed plan names, as the plan saw it; the
# whole-disk steps end at once in keep mode; and no command that erases
# stands anywhere else in the script.
#
# A case is what the kd_fact_ functions of the section read on a real
# machine:
#   lsblk.json           lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,
#                        PARTUUID,PARTLABEL,PARTTYPE,PARTN,PTTYPE,MOUNTPOINTS,RO,RM,TRAN
#                        (fields that are null, false or empty left out)
#   case                 the machine and the options, as shell: efi (1, 0), arch,
#                        os, unpacked_mb, root_fs (the layout's), keep_root_arg
#                        (--root), keep_root (1: --keep-root), keep_format
#                        (--format, an array)
#   fs/dev/NAME/         the files of the file system on /dev/NAME that matter
#                        (etc/fstab, etc/os-release, etc/crypttab, key files,
#                        etc/hostid, ...); of a btrfs its top, with
#   fs/dev/NAME.subvolumes   its subvolumes, "ID TOP PATH" per line, and
#   fs/dev/NAME.default      its default subvolume, when that is not the top
#   fs/dev/NAME.usage.json   kd_fact_usage's answer for it, as the old root
#   fs/dev/NAME.esp.json     kd_fact_esp's, as the EFI system partition
#   links                "PATH DEVICE" per line: where a path under /dev leads
#   mbr                  the disks with GRUB in their first sector, one per line
#   lvs                  kd_fact_lvs's lines: PATH|SEGMENT TYPE|ACTIVE|UUID
#
# Usage: test-keep-plan.sh [CASE...]   (default: every case, and the guards)
# Needs bash 4 or later and jq. VERBOSE=1 names every case; UPDATE=1 writes
# the expected files instead of comparing (review the diff).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
CASES="$HERE/testdata/keep-data"
INSTALL="$ROOT/install/install.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ump-keep-plan-test.XXXXXX")"
trap 'command rm -rf "$WORK"' EXIT
[ "${BASH_VERSINFO[0]}" -ge 4 ] || { echo "test-keep-plan.sh: bash 4 or later is needed (this is $BASH_VERSION)" >&2; exit 2; }
command -v jq >/dev/null || { echo "test-keep-plan.sh: jq is needed" >&2; exit 2; }
command -v sha256sum >/dev/null || sha256sum() { shasum -a 256 "$@"; }

# what the section calls of the rest of install.sh
die() { echo "install.sh: $*"; exit 1; }
warn() { echo "install.sh: warning: $*"; }
log() { printf '==> %s\n' "$*"; }
section="$(awk '/^# --- keep-data: /{on = 1; next} on && /^# --- /{exit} on' "$INSTALL")"
[ -n "$section" ] || { echo "test-keep-plan.sh: no keep-data section in install/install.sh" >&2; exit 2; }
eval "$section"

# Whatever could change a disk is a function here that only writes its
# name down: a plan made, or a guard passed, with one of them called fails.
CALLS="$WORK/calls"
for c in mount umount mkfs.ext4 mkfs.xfs mkfs.btrfs mkfs.vfat mkswap wipefs sgdisk mdadm dd pvcreate vgcreate lvcreate vgchange lvchange \
	vgremove lvremove pvremove btrfs cryptsetup partprobe blockdev dmsetup swapoff swapon rm mv cp mkdir chmod chown tune2fs e2fsck; do
	eval "$c() { echo \"$c \$*\" >> \"\$CALLS\"; }"
done

# --- the facts of a case ---------------------------------------------------

kd_fact_lsblk() { cat "$CASE/lsblk.json"; }
kd_fact_realpath() {
	local t=""
	[ ! -f "$CASE/links" ] || t="$(awk -v p="$1" '$1 == p {print $2}' "$CASE/links")"
	printf '%s\n' "${t:-$1}"
}
kd_fact_fs() {
	local tree="$CASE/fs$1" subs='[]' def="" roots='[]' p base b resume system host
	[ -d "$tree" ] || { echo '{"roots": [], "subvolumes": [], "default": ""}'; return 0; }
	if [ -f "$tree.subvolumes" ]; then
		subs="$(jq -R -s -c 'split("\n") | map(select(. != "") | split(" ") | {id: (.[0] | tonumber), top: (.[1] | tonumber), path: (.[2:] | join(" "))})' < "$tree.subvolumes")"
	fi
	[ ! -f "$tree.default" ] || def="$(cat "$tree.default")"
	while IFS= read -r p; do
		p="${p#=}"; base="$tree${p:+/$p}"
		if [ -f "$base/.ump-keep-data/etc/fstab" ]; then
			resume=true; b="$base/.ump-keep-data"
		elif [ -f "$base/etc/fstab" ] && [ -f "$base/etc/os-release" ]; then
			resume=false; b="$base"
		else
			continue
		fi
		system="$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$b/etc/os-release" 2>/dev/null | head -n 1)"
		host=""; [ ! -f "$b/etc/hostname" ] || host="$(head -n 1 "$b/etc/hostname")"
		roots="$(jq -c --arg s "$p" --arg system "$system" --arg host "$host" --argjson resume "$resume" '. + [{subvolume: $s, system: $system, hostname: $host, resume: $resume}]' <<< "$roots")"
	done < <(echo "="; jq -r '.[] | "=" + .path' <<< "$subs")
	printf '%s\n' "$roots" "$subs" | jq -c -s --arg default "$def" '{roots: .[0], subvolumes: .[1], default: $default}'
}
kd_fact_root() {
	local b="$CASE/fs$1${2:+/$2}" keyfiles='[]' opens='{}' k n hostid="" zc=false resume=false
	if [ -f "$b/.ump-keep-data/etc/fstab" ]; then b="$b/.ump-keep-data"; resume=true; fi
	rf() { if [ -f "$b/$1" ]; then cat "$b/$1"; fi; }
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		[ ! -f "$b$k" ] || keyfiles="$(jq -c --arg p "$k" '. + [{path: $p, mode: "600"}]' <<< "$keyfiles")"
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $3}' | sort -u)
	# a key file that says "wrong" is one that does not open its container
	while read -r n k; do
		if [ -f "$b$k" ] && grep -q '^wrong' "$b$k"; then opens="$(jq -c --arg n "$n" '.[$n] = false' <<< "$opens")"; fi
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $1, $3}')
	[ ! -f "$b/etc/hostid" ] || hostid="$(base64 < "$b/etc/hostid" | tr -d '\n')"
	[ ! -f "$b/etc/zfs/zpool.cache" ] || zc=true
	jq -n -c --arg fstab "$(rf etc/fstab)" --arg crypttab "$(rf etc/crypttab)" --arg mdadm_conf "$(rf etc/mdadm/mdadm.conf; rf etc/mdadm.conf)" \
		--arg hostid "$hostid" --argjson zpool_cache "$zc" --argjson keyfiles "$keyfiles" --argjson opens "$opens" --argjson resume "$resume" \
		'{fstab: $fstab, crypttab: $crypttab, mdadm_conf: $mdadm_conf, hostid: $hostid, zpool_cache: $zpool_cache, keyfiles: $keyfiles, opens: $opens, resume: $resume}'
}
kd_fact_usage() {
	if [ -f "$CASE/fs$1.usage.json" ]; then jq -c --argjson sizes "$4" 'if $sizes == 0 then .usage = [] else . end' "$CASE/fs$1.usage.json"
	else echo '{"size_mb": 0, "used_mb": 0, "free_mb": 100000, "tree_mb": 0, "features": "", "usage": []}'; fi
}
kd_fact_esp() {
	if [ -f "$CASE/fs$1.esp.json" ]; then jq -c . "$CASE/fs$1.esp.json"; else echo '{"free_mb": 190, "loaders": []}'; fi
}
kd_fact_mbr() { if [ -f "$CASE/mbr" ] && grep -qxF "$1" "$CASE/mbr"; then echo grub; fi; }
kd_fact_lvs() { if [ -f "$CASE/lvs" ]; then cat "$CASE/lvs"; fi; }
# what a device holds after the installation: a file system made anew has
# a new UUID, the others the one lsblk saw
kd_fact_uuid() {
	if jq -e --arg d "$1" '[.format[] | select(.how == "mkfs" and .device == $d)] | length > 0' >/dev/null <<< "$kd_plan"; then echo "NEW-${1##*/}"
	else jq -r --arg d "$1" '[.. | objects | select(.path == $d) | .uuid // ""][0] // ""' "$CASE/lsblk.json"; fi
}

# --- the cases ---------------------------------------------------------------

# plan_case: the plan of $CASE, as the installer prints, writes and keeps it
plan_case() {
	local rc
	efi=1; arch=arm64; os=ubuntu-26.04; unpacked_mb=1614; root_fs=ext4; keep_root_arg=""; keep_root=0; keep_format=()
	. "$CASE/case"
	spec="$(jq -n -c --arg fs "$root_fs" '{layout: {parts: [{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: $fs, mountpoint: "/"}]}}')"
	: > "$CALLS"
	set +e; ( set -e; kd_build ) > "$WORK/plan.json"; rc=$?; set -e
	[ ! -s "$CALLS" ] || { echo "the plan called what could change a disk:"; cat "$CALLS"; return 1; }
	kd_plan="$(cat "$WORK/plan.json")"
	case "$rc" in
	0)
		kd_text
		echo "--- /etc/fstab"
		kd_fstab
		if [ -n "$(kd_crypttab)" ]; then echo "--- /etc/crypttab"; kd_crypttab; fi
		echo "--- the plan (--plan)"
		kd_public
		[ "$(jq -cS 'del(.private) | del(.hash)' <<< "$kd_plan" | sha256sum | cut -d' ' -f1)" = "$(jq -r .hash <<< "$kd_plan")" ] || { echo "the hash is not the plan's"; return 1; }
		;;
	9)
		echo "refused: $(jq -r .refused <<< "$kd_plan")"
		echo "--- the plan (--plan)"
		kd_public
		;;
	*) echo "kd_build failed (status $rc)"; cat "$WORK/plan.json"; return 1 ;;
	esac
	[ ! -s "$CALLS" ] || { echo "the plan called what could change a disk:"; cat "$CALLS"; return 1; }
}

failed=0 n=0
run() { # run NAME COMMAND...: compare the command's output with the case's expected
	local name="$1" expected="$2"; shift 2
	n=$((n + 1))
	local out="$WORK/$name.out" rc
	set +e
	( set -e; "$@" ) > "$out" 2>&1
	rc=$?
	set -e
	[ "$rc" = 0 ] || echo "(exit status $rc)" >> "$out"
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

# --- the guards ----------------------------------------------------------------

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
called() { local c; c="$(paste -sd';' "$CALLS" | sed 's/;/; /g')"; echo "${c:-nothing}"; }

# guard_format: format() in keep mode: the plan's device with the UUID the
# plan saw, and nothing else
guard_format() {
	eval "$(fn format)"
	[ "$(type -t format)" = function ] || { echo "no format() in install.sh"; return 1; }
	keep=1; KD_FORMAT=("/dev/mapper/vg0-root 1111" "/dev/vda2 2222")
	kd_fact_uuid() { case "$1" in /dev/mapper/vg0-root) echo 1111 ;; /dev/vda2) echo changed ;; /dev/mapper/vg0-data) echo 3333 ;; esac; }
	try() { # try WHAT WANT COMMAND...: the command in a shell of its own, and what it called
		local what="$1" want="$2" rc; shift 2
		: > "$CALLS"
		set +e; ( set -e; "$@" ) > "$WORK/guard.out" 2>&1; rc=$?; set -e
		echo "$what: $([ "$rc" = 0 ] && echo done || echo "refused ($(head -n 1 "$WORK/guard.out"))"); called: $(called)"
		if [ "$want" = ok ]; then [ "$rc" = 0 ] || { echo "  NOT AS IT MUST BE"; return 1; }
		elif [ "$rc" = 0 ] || [ -s "$CALLS" ]; then echo "  NOT AS IT MUST BE"; return 1; fi
	}
	try "the plan's root" ok format /dev/mapper/vg0-root ext4 ""
	try "a device the plan does not name" no format /dev/mapper/vg0-data ext4 ""
	try "a device the plan does not name, by another name of the plan's" no format /dev/vda ext4 ""
	try "the plan's /boot, which changed since the plan" no format /dev/vda2 ext4 ""
	try "the plan's root as a physical volume" no format /dev/mapper/vg0-root lvm ""
	try "the plan's root as swap" no format /dev/mapper/vg0-root swap ""
	KD_FORMAT=()
	try "anything, with a plan that formats nothing" no format /dev/mapper/vg0-root ext4 ""
	keep=0
	try "without --keep-data, as ever" ok format /dev/mapper/vg0-data xfs data
}

# guard_whole_disks: the steps that wipe and partition end at once in keep mode
guard_whole_disks() {
	local f rc
	for f in wipe_disks make_layout; do
		eval "$(fn "$f")"
		[ "$(type -t "$f")" = function ] || { echo "no $f() in install.sh"; return 1; }
		keep=1; kd_wrote=0; disks=(/dev/vda /dev/vdb); P_DISK=(/dev/vda); P_INDEX=(1); P_SIZE=(rest); P_FS=(ext4); P_MOUNT=(/); P_LABEL=(""); P_VG=(""); P_MD=(""); P_DEV=(/dev/vda1)
		M_LEVEL=(); L_VG=(); declare -A VG_PVS=()
		kill_chrooted() { echo kill_chrooted >> "$CALLS"; }
		: > "$CALLS"
		set +e; ( set -e; "$f" ) > "$WORK/guard.out" 2>&1; rc=$?; set -e
		echo "$f in keep mode: $([ "$rc" = 0 ] && echo ran || echo "refused ($(head -n 1 "$WORK/guard.out"))"); called: $(called)"
		[ "$rc" != 0 ] && [ ! -s "$CALLS" ] || { echo "  NOT AS IT MUST BE"; return 1; }
	done
}

# guard_commands: the commands that erase, and where the script has them:
# in the whole-disk steps and format() alone (and the swap file NixOS is
# built with, on the new root)
guard_commands() {
	awk '
		here != "" {if ($0 == here) here = ""; next}
		match($0, /<<-?'"'"'?[A-Z]+'"'"'?/) {here = substr($0, RSTART, RLENGTH); gsub(/[<'"'"'-]/, "", here)}
		/^[a-z_]+\(\) \{/ && !/\}[ \t]*(#.*)?$/ {if (depth++ == 0) {name = $1; sub(/\(\).*/, "", name)}}
		$0 == "}" {if (--depth == 0) name = ""}
		/^[ \t]*#/ {next}
		/^for t in |^	mkfs\.ext4 mkfs\.vfat mkswap/ {next}
		/wipefs|sgdisk|mdadm --create|mdadm --stop|zero-superblock|pvcreate|vgcreate|lvcreate|vgremove|lvremove|pvremove|mkfs\.|mkswap|of="\$x"|blkdiscard|luksFormat|zpool (create|destroy|labelclear)/ {
			if (name != "wipe_disks" && name != "make_layout" && name != "format" && name != "swap_on_target") { print "line " NR " (" (name == "" ? "the main script" : name) "): " $0; bad = 1 }
			else seen[name]++
		}
		END { for (f in seen) n++; print "the commands that erase are in " n " functions (wipe_disks, make_layout, format, swap_on_target)"; exit bad }
	' "$INSTALL"
	# and the two steps are called in one place, the branch without --keep-data
	grep -nE '^[[:space:]]*(wipe_disks|make_layout)$' "$INSTALL" | sed 's/^[0-9]*://' | paste -sd' ' -
	awk '/^if \[ "\$keep" = 1 \]; then$/ {getline a; getline b; getline c; getline d; getline e; if (a ~ /kd_write/ && b == "else" && c ~ /wipe_disks/ && d ~ /make_layout/ && e == "fi") found = 1} END {if (found) print "called where keep is 0 alone"; else {print "the call of the whole-disk steps is not where it was"; exit 1}}' "$INSTALL"
}

only=("$@")
wanted() { [ ${#only[@]} -eq 0 ] || printf '%s\n' "${only[@]}" | grep -qxF "$1"; }
[ "${UPDATE:-0}" != 1 ] || command mkdir -p "$CASES"
for CASE in "$CASES"/*/; do
	CASE="${CASE%/}"
	[ -f "$CASE/case" ] || continue
	wanted "$(basename "$CASE")" || continue
	run "$(basename "$CASE")" "$CASE/expected" plan_case
done
for g in guard_format guard_whole_disks guard_commands; do
	wanted "$g" || continue
	run "$g" "$CASES/$g" "$g"
done
[ "$n" -gt 0 ] || { echo "test-keep-plan.sh: no such case: $*" >&2; exit 2; }
if [ "$failed" -gt 0 ]; then echo "$failed of $n cases failed"; exit 1; fi
echo "test-keep-plan.sh: all $n cases as expected"
