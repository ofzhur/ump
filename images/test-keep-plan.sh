#!/usr/bin/env bash
# Check the keep-data plan of install/install.sh (--keep-data), without a
# machine: the functions of its keep-data section run on the machines of
# testdata/keep-data, one directory each, and what they make of it is
# compared with the directory's expected: the plan as people read it,
# the new /etc/fstab and /etc/crypttab, and the plan as JSON with its
# hash; or the refusal. Then the guards, on the installer itself: format()
# makes anew only what the confirmed plan names, as the plan saw it, each
# device once; the whole-disk steps end at once in keep mode; what
# kd_write runs for a plan, command by command, in a directory that
# stands for the root's file system (guard_write: nothing it runs reaches
# outside); and where the script has its commands that erase, delete,
# move and overwrite (guard_commands). What the facts functions
# themselves do to a disk is not seen here, where each of them is
# replaced: test-keep-loop.sh runs them on file systems.
#
# A case is what the kd_fact_ functions of the section read on a real
# machine:
#   lsblk.json           lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,
#                        PARTUUID,PARTLABEL,PARTTYPE,PARTN,PTTYPE,MOUNTPOINTS,RO,RM,TRAN,START
#                        (fields that are null, false or empty left out)
#   case                 the machine and the options, as shell: efi (1, 0), arch,
#                        os, unpacked_mb, root_fs (the layout's), keep_root_arg
#                        (--root), keep_root (1: --keep-root), keep_format
#                        (--format, an array)
#   fs/dev/NAME/         the files of the file system on /dev/NAME that matter
#                        (etc/fstab, etc/os-release, etc/crypttab, key files,
#                        etc/hostid, lib/modules/VERSION/, ...); of a btrfs its
#                        top; .ump-keep-data/ there is what an interrupted
#                        installation saved (etc/fstab, plan.json, state,
#                        formats; with a file broken: damaged), with
#   fs/dev/NAME.subvolumes   its subvolumes, "ID TOP PATH" per line, and
#   fs/dev/NAME.default      its default subvolume, when that is not the top
#   fs/dev/NAME.dirty        (empty) it was not unmounted cleanly
#   fs/dev/NAME.error        why it cannot be looked at
#   fs/dev/NAME.usage.json   kd_fact_usage's answer for it, as the old root
#   fs/dev/NAME.esp.json     kd_fact_esp's, as the EFI system partition
#   fs/dev/NAME.boot.json    kd_fact_boot's, as /boot
#   ram/                 what a failed installation left in memory (as
#                        .ump-keep-data; its state names the root device)
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
WORK="$(cd "$WORK" && pwd -P)"
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
# (its own directory is under /run; here, one of the test's)
KD_DIR="$WORK/kd"; KD_LOOK="$KD_DIR/look"

# Whatever could change a disk is a function here that only writes its
# name down: a plan made, or a guard passed, with one of them called fails.
CALLS="$WORK/calls"; PLAN_FILE="$WORK/plan.json"
for c in mount umount mkfs.ext4 mkfs.xfs mkfs.btrfs mkfs.vfat mkswap wipefs sgdisk mdadm dd pvcreate vgcreate lvcreate vgchange lvchange \
	vgremove lvremove pvremove btrfs cryptsetup partprobe blockdev dmsetup swapoff swapon rm mv cp mkdir chmod chown tune2fs e2fsck \
	grub-install lsattr xfs_logprint dumpe2fs; do
	eval "$c() { echo \"$c \$*\" >> \"\$CALLS\"; }"
done

# --- the facts of a case ---------------------------------------------------

kd_fact_lsblk() { cat "$CASE/lsblk.json"; }
kd_fact_realpath() {
	local t=""
	[ ! -f "$CASE/links" ] || t="$(awk -v p="$1" '$1 == p {print $2}' "$CASE/links")"
	printf '%s\n' "${t:-$1}"
}
kd_fact_has_uuid() { jq -e --arg u "$1" '[.. | objects | select(((.uuid // "") | ascii_downcase) == ($u | ascii_downcase))] | length > 0' "$CASE/lsblk.json" >/dev/null; }
# saved DIR: what an interrupted installation saved there is whole
saved() { [ -f "$1/etc/fstab" ] && [ -f "$1/plan.json" ] && [ -f "$1/state" ] && [ ! -e "$1/broken" ]; }
# saved_json DIR RAM: as kd_saved_json of the section
saved_json() {
	local b="$1" keyfiles='[]' k hostid="" zc=false
	rf() { if [ -f "$b/$1" ]; then cat "$b/$1"; fi; }
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		[ ! -f "$b$k" ] || keyfiles="$(jq -c --arg p "$k" '. + [{path: $p, mode: "600"}]' <<< "$keyfiles")"
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $3}' | sort -u)
	[ ! -f "$b/etc/hostid" ] || hostid="$(base64 < "$b/etc/hostid" | tr -d '\n')"
	[ ! -f "$b/etc/zfs/zpool.cache" ] || zc=true
	jq -c --arg fstab "$(rf etc/fstab | kd_journal "$b/formats")" --arg crypttab "$(rf etc/crypttab)" --arg mdadm_conf "$(rf etc/mdadm/mdadm.conf; rf etc/mdadm.conf)" \
		--arg hostid "$hostid" --argjson zpool_cache "$zc" --argjson keyfiles "$keyfiles" --argjson ram "$2" \
		--arg root "$(kd_state "$b" root)" --arg aside "$(kd_state "$b" aside)" \
		'{fstab: $fstab, crypttab: $crypttab, mdadm_conf: $mdadm_conf, hostid: $hostid, zpool_cache: $zpool_cache, keyfiles: $keyfiles, opens: {}, modules: [],
		  resume: true, ram: $ram, first: ., saved: {root: $root, aside: $aside}}' "$b/plan.json"
}
kd_fact_fs() {
	local tree="$CASE/fs$1" subs='[]' def="" roots='[]' p base b resume system host mark="" msub="" broken=false dirty=false
	[ ! -e "$tree.dirty" ] || dirty=true
	if [ -f "$tree.error" ]; then jq -n -c --arg e "$(cat "$tree.error")" --argjson dirty "$dirty" '{error: $e, dirty: $dirty}'; return 0; fi
	[ -d "$tree" ] || { jq -n -c --argjson dirty "$dirty" '{roots: [], subvolumes: [], default: "", dirty: $dirty, broken: false}'; return 0; }
	if [ -f "$tree.subvolumes" ]; then
		subs="$(jq -R -s -c 'split("\n") | map(select(. != "") | split(" ") | {id: (.[0] | tonumber), top: (.[1] | tonumber), path: (.[2:] | join(" "))})' < "$tree.subvolumes")"
	fi
	[ ! -f "$tree.default" ] || def="$(cat "$tree.default")"
	if [ -e "$tree/$KD_MARK" ]; then
		if saved "$tree/$KD_MARK"; then mark="$tree/$KD_MARK"; msub="$(kd_state "$mark" subvolume)"; else broken=true; fi
	fi
	while IFS= read -r p; do
		[ "$broken" = false ] || break
		p="${p#=}"; base="$tree${p:+/$p}"
		if [ -n "$mark" ] && [ "$p" = "$msub" ]; then
			resume=true; b="$mark"
		elif [ -f "$base/etc/fstab" ] && [ -f "$base/etc/os-release" ]; then
			resume=false; b="$base"
		else
			continue
		fi
		system="$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$b/etc/os-release" 2>/dev/null | head -n 1)"
		host=""; [ ! -f "$b/etc/hostname" ] || host="$(head -n 1 "$b/etc/hostname")"
		roots="$(jq -c --arg s "$p" --arg system "$system" --arg host "$host" --argjson resume "$resume" '. + [{subvolume: $s, system: $system, hostname: $host, resume: $resume}]' <<< "$roots")"
	done < <(echo "="; jq -r '.[] | "=" + .path' <<< "$subs"; if [ -n "$msub" ] && ! jq -e --arg s "$msub" 'any(.path == $s)' >/dev/null <<< "$subs"; then echo "=$msub"; fi)
	printf '%s\n' "$roots" "$subs" | jq -c -s --arg default "$def" --argjson dirty "$dirty" --argjson broken "$broken" '{roots: .[0], subvolumes: .[1], default: $default, dirty: $dirty, broken: $broken}'
}
kd_fact_root() {
	local tree="$CASE/fs$1" b keyfiles='[]' opens='{}' modules k n hostid="" zc=false
	if saved "$tree/$KD_MARK" && [ "$(kd_state "$tree/$KD_MARK" subvolume)" = "$2" ]; then saved_json "$tree/$KD_MARK" false; return 0; fi
	b="$tree${2:+/$2}"
	[ -d "$b" ] || { echo '{}'; return 0; }
	rf() { if [ -f "$b/$1" ]; then cat "$b/$1"; fi; }
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		[ ! -f "$b$k" ] || keyfiles="$(jq -c --arg p "$k" '. + [{path: $p, mode: "600"}]' <<< "$keyfiles")"
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $3}' | sort -u)
	# a key file that says "wrong" is one that does not open its container
	while read -r n k; do
		if [ -f "$b$k" ] && grep -q '^wrong' "$b$k"; then opens="$(jq -c --arg n "$n" '.[$n] = false' <<< "$opens")"; fi
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $1, $3}')
	modules="$(for k in "$b"/lib/modules/*/ "$b"/usr/lib/modules/*/; do [ -d "$k" ] && basename "$k"; done | sort -u | jq -R -s -c 'split("\n") | map(select(. != ""))')"
	[ ! -f "$b/etc/hostid" ] || hostid="$(base64 < "$b/etc/hostid" | tr -d '\n')"
	[ ! -f "$b/etc/zfs/zpool.cache" ] || zc=true
	jq -n -c --arg fstab "$(rf etc/fstab)" --arg crypttab "$(rf etc/crypttab)" --arg mdadm_conf "$(rf etc/mdadm/mdadm.conf; rf etc/mdadm.conf)" \
		--arg hostid "$hostid" --argjson zpool_cache "$zc" --argjson keyfiles "$keyfiles" --argjson opens "$opens" --argjson modules "$modules" \
		'{fstab: $fstab, crypttab: $crypttab, mdadm_conf: $mdadm_conf, hostid: $hostid, zpool_cache: $zpool_cache, keyfiles: $keyfiles, opens: $opens, modules: $modules, resume: false}'
}
kd_fact_ram() {
	local old="$CASE/ram" system=""
	saved "$old" || return 0
	system="$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$old/etc/os-release" 2>/dev/null | head -n 1)"
	jq -n -c --arg d "$(kd_state "$old" device)" --arg s "$(kd_state "$old" subvolume)" --arg system "$system" --arg host "$(head -n 1 "$old/etc/hostname" 2>/dev/null || true)" \
		'{device: $d, subvolume: $s, system: $system, hostname: $host}'
}
kd_fact_root_ram() { saved_json "$CASE/ram" true; }
kd_fact_usage() {
	if [ -f "$CASE/fs$1.usage.json" ]; then jq -c --argjson sizes "$4" 'if $sizes == 0 then .usage = [] else . end' "$CASE/fs$1.usage.json"
	else echo '{"size_mb": 0, "used_mb": 0, "free_mb": 100000, "tree_mb": 0, "features": "", "usage": []}'; fi
}
kd_fact_esp() {
	if [ -f "$CASE/fs$1.esp.json" ]; then jq -c . "$CASE/fs$1.esp.json"; else echo '{"free_mb": 190, "loaders": []}'; fi
}
kd_fact_boot() {
	if [ -f "$CASE/fs$1.boot.json" ]; then jq -c . "$CASE/fs$1.boot.json"; else echo '{"kernels": [], "loaders": []}'; fi
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

# make_plan: the plan of $CASE into kd_plan; its status is kd_build's
make_plan() {
	local rc
	efi=1; arch=arm64; os=ubuntu-26.04; unpacked_mb=1614; root_fs=ext4; keep_root_arg=""; keep_root=0; keep_format=()
	. "$CASE/case"
	spec="$(jq -n -c --arg fs "$root_fs" '{layout: {parts: [{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: $fs, mountpoint: "/"}]}}')"
	: > "$CALLS"
	set +e; ( set -e; kd_build ) > "$PLAN_FILE"; rc=$?; set -e
	[ ! -s "$CALLS" ] || { echo "the plan called what could change a disk:"; cat "$CALLS"; return 1; }
	kd_plan="$(cat "$PLAN_FILE")"
	return "$rc"
}

# plan_case: the plan of $CASE, as the installer prints, writes and keeps it
plan_case() {
	local rc=0
	make_plan || rc=$?
	case "$rc" in
	0)
		kd_text
		echo "--- /etc/fstab"
		kd_fstab
		if [ -n "$(kd_crypttab)" ]; then echo "--- /etc/crypttab"; kd_crypttab; fi
		echo "--- the plan (--plan)"
		kd_public
		[ "$(jq -cS 'del(.private) | del(.hash)' <<< "$kd_plan" | sha256sum | cut -d' ' -f1)" = "$(jq -r .hash <<< "$kd_plan")" ] || { echo "the hash is not the plan's"; return 1; }
		# what the plan's own rules must hold, whatever the machine
		jq -e '([.format[] | select(.how == "mkfs") | .device] | (length == (unique | length)))
			and ([.format[] | select(.how == "mkfs" and .mountpoint != "/") | .device] | index($p.boot.esp) == null)
			and (.keep | all(.device == null or (. as $k | $p.format | all(.device != $k.device or .how == "subvolume"))))' --argjson p "$kd_plan" <<< "$kd_plan" >/dev/null \
			|| { echo "the plan formats a device twice, the EFI system partition, or what it keeps"; return 1; }
		;;
	9)
		echo "refused: $(jq -r .refused <<< "$kd_plan")"
		echo "--- the plan (--plan)"
		kd_public
		;;
	*) echo "kd_build failed (status $rc)"; cat "$PLAN_FILE"; return 1 ;;
	esac
	[ ! -s "$CALLS" ] || { echo "the plan called what could change a disk:"; cat "$CALLS"; return 1; }
}

failed=0 n=0
JOBS="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)"
NAMES=(); EXPECTED=()
run() { # run NAME EXPECTED COMMAND...: the command's output for NAME, side by side with the others'
	local name="$1"; NAMES+=("$1"); EXPECTED+=("$2"); shift 2
	while [ "$(jobs -rp | wc -l)" -ge "$JOBS" ]; do wait -n; done
	(
		CALLS="$WORK/$name.calls"; PLAN_FILE="$WORK/$name.plan.json"
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
# plan saw, once, and nothing else
guard_format() {
	eval "$(fn format)"
	[ "$(type -t format)" = function ] || { echo "no format() in install.sh"; return 1; }
	keep=1; KD_FORMAT=("/dev/mapper/vg0-root 1111" "/dev/vda2 2222")
	kd_fact_uuid() { case "$1" in /dev/mapper/vg0-root) echo 1111 ;; /dev/vda2) echo changed ;; /dev/mapper/vg0-data) echo 3333 ;; esac; }
	try() { # try WHAT WANT COMMAND...: the command in a shell of its own, and what it called
		local what="$1" want="$2" rc; shift 2
		: > "$CALLS"
		set +e; ( set -e; "$@" ) > "$CALLS.out" 2>&1; rc=$?; set -e
		echo "$what: $([ "$rc" = 0 ] && echo done || echo "refused ($(head -n 1 "$CALLS.out"))"); called: $(called)"
		if [ "$want" = ok ]; then [ "$rc" = 0 ] || { echo "  NOT AS IT MUST BE"; return 1; }
		elif [ "$rc" = 0 ] || [ -s "$CALLS" ]; then echo "  NOT AS IT MUST BE"; return 1; fi
	}
	twice() { format "$1" ext4 "" && format "$1" ext4 ""; }
	try "the plan's root" ok format /dev/mapper/vg0-root ext4 ""
	try "the plan's root, with the label it had and a UUID known beforehand" ok format /dev/mapper/vg0-root ext4 root 9999
	try "a device the plan does not name" no format /dev/mapper/vg0-data ext4 ""
	try "a device the plan does not name, by another name of the plan's" no format /dev/vda ext4 ""
	try "the plan's /boot, which changed since the plan" no format /dev/vda2 ext4 ""
	try "the plan's root as a physical volume" no format /dev/mapper/vg0-root lvm ""
	try "the plan's root as swap" no format /dev/mapper/vg0-root swap ""
	: > "$CALLS"
	set +e; ( set -e; twice /dev/mapper/vg0-root ) > "$CALLS.out" 2>&1; rc=$?; set -e
	echo "the plan's root a second time: $([ "$rc" = 0 ] && echo done || echo "refused ($(head -n 1 "$CALLS.out"))"); called: $(called)"
	[ "$rc" != 0 ] && [ "$(wc -l < "$CALLS" | tr -d ' ')" = 1 ] || { echo "  NOT AS IT MUST BE"; return 1; }
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
		set +e; ( set -e; "$f" ) > "$CALLS.out" 2>&1; rc=$?; set -e
		echo "$f in keep mode: $([ "$rc" = 0 ] && echo ran || echo "refused ($(head -n 1 "$CALLS.out"))"); called: $(called)"
		[ "$rc" != 0 ] && [ ! -s "$CALLS" ] || { echo "  NOT AS IT MUST BE"; return 1; }
	done
}

# guard_write: what kd_write does for the plan of a case, in a directory
# that stands for the root's file system (the case's files of it): the
# commands that format, delete, rename and move, as it calls them, and
# what the directory holds after. A command that moves or removes acts
# there, and nowhere else: outside the test's directory it is refused,
# and so is a plan that kd_check_write must stop
guard_write() {
	local c rc root sub box
	eval "$(fn format)"
	date() { echo 20260102-030405; }
	kd_new_uuid() { local n; n=$(($(cat "$box.n" 2>/dev/null || echo 0) + 1)); echo "$n" > "$box.n"; echo "new-uuid-$n"; }
	mountpoint() { return 1; }
	sync() { :; }; udevadm() { :; }; blockdev() { :; }
	kd_fact_uuid() { jq -r --arg d "$1" '[.. | objects | select(.path == $d) | .uuid // ""][0] // ""' "$CASE/lsblk.json"; }
	# the commands that act on files do, inside the test's directory
	inside() { local a; for a in "$@"; do case "$a" in -*|[!/]*) ;; "$box"/*) ;; *) return 1 ;; esac; done; }
	for c in mv rm cp mkdir chmod; do
		eval "$c() { if inside \"\$@\"; then case $c in mv|rm) echo \"$c \$*\" | sed \"s|\$box|BOX|g\" >> \"\$CALLS\" ;; esac; command $c \"\$@\"; else echo \"$c \$* (OUTSIDE: refused)\" >> \"\$CALLS\"; return 1; fi; }"
	done
	for c in mount umount mdadm; do
		eval "$c() { echo \"$c \$*\" | sed \"s|\$box|BOX|g\" >> \"\$CALLS\"; }"
	done
	# a file system made anew on the root's device holds nothing
	for c in mkfs.ext4 mkfs.xfs mkfs.btrfs mkfs.vfat; do
		eval "$c() { echo \"$c \$*\" >> \"\$CALLS\"; local a; for a in \"\$@\"; do :; done; if [ \"\$a\" = \"\$root\" ]; then command find \"\$box/top\" -mindepth 1 -delete; fi; }"
	done
	# btrfs: the subvolumes of the case, and what is asked of them
	btrfs() {
		case "$1 $2" in
		"subvolume list") [ ! -f "$CASE/fs$root.subvolumes" ] || awk '{id = $1; top = $2; $1 = ""; $2 = ""; sub(/^ +/, ""); print "ID " id " gen 7 top level " top " path " $0}' "$CASE/fs$root.subvolumes" ;;
		"subvolume sync") ;;
		"subvolume create") echo "btrfs $*" | sed "s|$box|BOX|g" >> "$CALLS"; command mkdir -p "$3" ;;
		"subvolume delete") echo "btrfs $*" | sed "s|$box|BOX|g" >> "$CALLS"; case "$3" in "$box"/top/*) command rm -rf "$3" ;; *) return 1 ;; esac ;;
		*) echo "btrfs $*" | sed "s|$box|BOX|g" >> "$CALLS" ;;
		esac
	}
	for c in lvm lvm-format lvm-keep-root btrfs-subvolumes btrfs-subvolumes-keep-root btrfs-default-named md-home-reiserfs resume resume-keep-root resume-move resume-ram resume-btrfs resume-btrfs-keep-root; do
		CASE="$CASES/$c"
		[ -f "$CASE/case" ] || { echo "no case $c"; return 1; }
		echo "=== $c"
		make_plan || { echo "no plan (status $?)"; return 1; }
		root="$(jq -r .root.device <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"
		box="$WORK/write/$c"; KD_DIR="$box"; KD_LOOK="$box/top"
		command mkdir -p "$box/top"
		[ ! -d "$CASE/fs$root" ] || command cp -R "$CASE/fs$root/." "$box/top/"
		# (what is in memory of a failed installation is where the installer has it)
		if [ -d "$CASE/ram" ]; then command cp -R "$CASE/ram" "$box/old"; (cd "$box/old" && sha256sum etc/fstab plan.json > SHA256SUMS); : > "$box/unsaved"; fi
		if [ -d "$box/top/$KD_MARK" ]; then (cd "$box/top/$KD_MARK" && sha256sum etc/fstab plan.json > SHA256SUMS); fi
		kd_wrote=0; keep=1; : > "$CALLS"
		set +e; ( set -e; kd_write ) > "$CALLS.out" 2>&1; rc=$?; set -e
		sed "s|$box|BOX|g" "$CALLS.out"
		echo "kd_write: status $rc; it ran:"
		sed 's/^/  /' "$CALLS"
		echo "the root's file system after: $(cd "$box/top" && find . -mindepth 1 -maxdepth 1 | sed 's|^\./||' | LC_ALL=C sort | paste -sd' ' -)"
		if [ -d "$box/top/$KD_MARK" ]; then
			echo "saved for a second attempt: state $(LC_ALL=C sort "$box/top/$KD_MARK/state" | paste -sd' ' -)$([ ! -f "$box/top/$KD_MARK/formats" ] || echo "; formats $(paste -sd',' "$box/top/$KD_MARK/formats")"); in memory: $(ls "$box/old" >/dev/null 2>&1 && echo "a copy, $([ -e "$box/unsaved" ] && echo "the only one" || echo "and on the root")" || echo nothing)"
			sed 's/^/  fstab: /' "$box/top/$KD_MARK/etc/fstab"
		fi
		[ "$rc" = 0 ] || return 1
		! grep -q OUTSIDE "$CALLS" || { echo "  NOT AS IT MUST BE: outside the root's file system"; return 1; }
	done
	# a plan that formats what the root is on beside its row: stopped before the first write
	CASE="$CASES/btrfs-subvolumes"; make_plan
	kd_plan="$(jq -c '.format += [{device: .root.device, filesystem: "btrfs", mountpoint: "/home", how: "mkfs", was: "btrfs", uuid: "x", label: ""}]' <<< "$kd_plan")"
	box="$WORK/write/twice"; KD_DIR="$box"; KD_LOOK="$box/top"; command mkdir -p "$box/top"; kd_wrote=0; : > "$CALLS"
	set +e; ( set -e; kd_write ) > "$CALLS.out" 2>&1; rc=$?; set -e
	echo "=== a plan with the root's own file system among what it formats: $([ "$rc" = 0 ] && echo ran || echo "refused ($(head -n 1 "$CALLS.out"))"); called: $(called)"
	[ "$rc" != 0 ] && [ ! -s "$CALLS" ] || { echo "  NOT AS IT MUST BE"; return 1; }
}

# guard_commands: the commands that erase, delete, move and overwrite, and
# where the script has them: those of whole disks in the whole-disk steps
# and format() alone (and the swap file NixOS is built with, on the new
# root); those keep-data works with (subvolumes deleted and renamed,
# arrays made writable, the old system moved) in kd_write alone; and a
# count of every such command by function, so that a new one shows
guard_commands() {
	local rc=0
	: > "$WORK/counts"
	awk -v counts="$WORK/counts" '
		here != "" {if ($0 == here) here = ""; next}
		match($0, /<<-?'"'"'?[A-Z]+'"'"'?/) {here = substr($0, RSTART, RLENGTH); gsub(/[<'"'"'-]/, "", here)}
		/^[a-z_]+\(\) \{/ && !/\}[ \t]*(#.*)?$/ {if (depth++ == 0) {name = $1; sub(/\(\).*/, "", name)}}
		$0 == "}" {if (--depth == 0) name = ""}
		/^[ \t]*#/ {next}
		/^for t in |^	mkfs\.ext4 mkfs\.vfat mkswap/ {next}
		{ where = (name == "" ? "the main script" : name); line = $0; sub(/[ \t]+#.*$/, "", line) }
		/wipefs|sgdisk|mdadm --create|mdadm --stop|zero-superblock|pvcreate|vgcreate|lvcreate|vgremove|lvremove|pvremove|mkfs\.|mkswap|of="\$x"|blkdiscard|luksFormat|zpool (create|destroy|labelclear)/ {
			if (name != "wipe_disks" && name != "make_layout" && name != "format" && name != "swap_on_target") { print "line " NR " (" where "): " $0; bad = 1 }
			else seen[name]++
		}
		/btrfs subvolume (delete|create|set-default|snapshot)|mdadm --readwrite/ {
			if (name != "kd_write") { print "line " NR " (" where "): " $0; bad = 1 }
		}
		/blockdev --set(ro|rw)/ { if (name != "kd_look" && name != "kd_unlook" && name != "kd_cleanup") { print "line " NR " (" where "): " $0; bad = 1 } }
		/mdadm --assemble/ { if (name != "kd_detect" || $0 !~ /--readonly/) { print "line " NR " (" where "): " $0; bad = 1 } }
		function count(what) { n[where ": " what]++ }
		/btrfs subvolume delete/ {count("btrfs subvolume delete")}
		/btrfs subvolume create/ {count("btrfs subvolume create")}
		/btrfs subvolume set-default/ {count("btrfs subvolume set-default")}
		/mdadm --readwrite/ {count("mdadm --readwrite")}
		/(^|[ \t;&|({])mv / {count("mv")}
		/rm -rf/ {count("rm -rf")}
		/chown -R/ {count("chown -R")}
		/sed -i/ {count("sed -i")}
		/grub-install /  && !/run_in_target "grub-install|warn "grub-install/ {count("grub-install")}
		/run_in_target "grub-install/ {count("grub-install")}
		/blockdev --set(ro|rw)/ {count("blockdev --setro, --setrw")}
		END {
			for (f in seen) k++
			print "the commands that erase are in " k " functions (wipe_disks, make_layout, format, swap_on_target)"
			for (x in n) print "  " x ": " n[x] > counts
			exit bad
		}
	' "$INSTALL" || rc=$?
	echo "the commands that delete, move and overwrite, by function:"
	LC_ALL=C sort "$WORK/counts"
	[ "$rc" = 0 ] || return 1
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
for g in guard_format guard_whole_disks guard_write guard_commands; do
	wanted "$g" || continue
	run "$g" "$CASES/$g" "$g"
done
wait
for i in "${!NAMES[@]}"; do compare "${NAMES[$i]}" "${EXPECTED[$i]}"; done
[ "$n" -gt 0 ] || { echo "test-keep-plan.sh: no such case: $*" >&2; exit 2; }
if [ "$failed" -gt 0 ]; then echo "$failed of $n cases failed"; exit 1; fi
echo "test-keep-plan.sh: all $n cases as expected"
