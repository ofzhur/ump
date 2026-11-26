#!/usr/bin/env bash
# The keep-data functions of install/install.sh on real file systems: where
# test-keep-plan.sh replaces every kd_fact_ function by a reader of its
# cases, this test runs them, and kd_write, on file systems in loop
# devices, as root on a Linux that can make them: a privileged container
# (make keep-loop-test), or the rescue system (test-keep-data.sh runs it
# there as its first machine). It proves what the plan test cannot:
#
#   looks    the functions that look at the old system (kd_fact_fs,
#            kd_fact_root, kd_fact_usage, kd_fact_esp, kd_fact_boot) and a
#            whole kd_build over them leave file systems that were not
#            unmounted cleanly (ext4, xfs, btrfs: a copy taken while
#            mounted, as a reset leaves a disk) byte for byte as they were
#   root     an installation that fails while the root is formatted, before
#            anything of the old system is saved on it: the next plan reads
#            the old fstab from memory; one that fails later: the next plan
#            reads it from the root, and finds /boot, formatted meanwhile,
#            by the UUID that was written down before its mkfs; what was
#            saved, damaged: refused
#   move     --keep-root with an entry that cannot be moved aside: refused
#            by the plan when it is immutable; a move that fails half-way
#            is carried on by the same command, into the same directory
#   btrfs    the root in a subvolume that is the default subvolume, the
#            installation failing between its deletion and the new one:
#            the next plan knows the old system, and the new subvolume is
#            the default again; with --keep-root, the rename
#   guard    a plan that would format the file system the root subvolume is
#            on is refused by the plan, and kd_write refuses one handed to
#            it, with nothing written
#
# Usage: test-keep-loop.sh [INSTALL.SH]
# As root on Linux with jq, e2fsprogs, xfsprogs, btrfs-progs and dosfstools
# it runs there; anywhere else it runs itself in a privileged container
# (CONTAINER: docker or podman; CONTAINER_NAME: its name; debian:13, whose
# packages it installs).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL="${1:-$HERE/../install/install.sh}"
[ -f "$INSTALL" ] || { echo "test-keep-loop.sh: no $INSTALL" >&2; exit 2; }
if [ "$(uname -s)" != Linux ] || [ "$(id -u)" != 0 ]; then
	c="${CONTAINER:-$(command -v docker || command -v podman || true)}"
	[ -n "$c" ] || { echo "test-keep-loop.sh: run it as root on Linux, or with docker or podman (it makes loop devices)" >&2; exit 2; }
	exec "$c" run --rm --privileged --name "${CONTAINER_NAME:-ump-keep-loop-test}" -v /lib/modules:/lib/modules:ro -v "$(cd "$(dirname "$INSTALL")" && pwd)":/install:ro -v "$HERE":/images:ro debian:13 bash -c '
		export DEBIAN_FRONTEND=noninteractive
		{ apt-get update -q && apt-get install -y -q --no-install-recommends jq e2fsprogs xfsprogs btrfs-progs dosfstools kmod util-linux; } >/tmp/apt.log 2>&1 || { tail -n 20 /tmp/apt.log; exit 2; }
		exec bash /images/test-keep-loop.sh /install/install.sh'
fi
for t in jq losetup mkfs.ext4 mkfs.vfat lsblk blkid sha256sum; do
	command -v "$t" >/dev/null || { echo "test-keep-loop.sh: $t is missing" >&2; exit 2; }
done
W="$(mktemp -d /tmp/ump-keep-loop-test.XXXXXX)"
LOOPS=()
finish() {
	set +e
	for m in /run/ump-keep-data/look /run/ump-keep-data/top "$W/mnt"; do mountpoint -q "$m" && umount "$m"; done
	for l in "${LOOPS[@]:-}"; do [ -n "$l" ] && losetup -d "$l" 2>/dev/null; done
	rm -rf "$W" /run/ump-keep-data
}
trap finish EXIT
fail() { echo "FAILED: $*"; exit 1; }
ok() { echo "ok      $*"; }
modprobe -q btrfs 2>/dev/null || true; modprobe -q xfs 2>/dev/null || true; modprobe -q loop 2>/dev/null || true
# (a container has the device nodes it was started with, and no udev to make more)
for i in $(seq 0 31); do [ -e "/dev/loop$i" ] || mknod "/dev/loop$i" b 7 "$i" 2>/dev/null || true; done

# the keep-data section of the installer and format(), as the installer has them
keep=1; kd_wrote=0; dying=""; plan_file=""; confirm=""
log() { printf '==> %s\n' "$*" >> "$W/log"; }
warn() { echo "install.sh: warning: $*" >> "$W/log"; }
die() { echo "install.sh: $*" >> "$W/log"; exit 1; }
eval "$(awk '/^# --- keep-data: /{on = 1; next} on && /^# --- /{exit} on' "$INSTALL")"
eval "$(awk '/^format\(\) \{/{on = 1} on {print} on && /^}/{exit}' "$INSTALL")"
[ "$(type -t kd_write)" = function ] && [ "$(type -t format)" = function ] || fail "no keep-data section or format() in $INSTALL"
rm -rf "$KD_DIR"; mkdir -p "$KD_DIR" "$W/mnt"; : > "$W/log"
# (a container has no udev to wait for)
command -v udevadm >/dev/null || udevadm() { :; }

# img VARIABLE NAME MB: a loop device over a new image, into VARIABLE
img() { truncate -s "${3}M" "$W/$2.img"; LOOP="$(losetup --show -f "$W/$2.img")"; LOOPS+=("$LOOP"); printf -v "$1" '%s' "$LOOP"; }
unloop() { losetup -d "$1"; local i; for i in "${!LOOPS[@]}"; do [ "${LOOPS[$i]}" != "$1" ] || unset "LOOPS[$i]"; done; }
uuid() { blkid -s UUID -o value "$1"; }
sums() { sync; sha256sum "$W"/*.img | sha256sum | cut -d' ' -f1; }
# old_root DIR: what an old system has at its root
old_root() {
	mkdir -p "$1/etc" "$1/home/alice" "$1/root" "$1/srv/www" "$1/var/lib" "$1/usr/lib/modules/6.1.0-13-test" "$1/opt/app" "$1/bin"
	printf 'PRETTY_NAME="Old Linux 1.0"\n' > "$1/usr/lib/os-release"; ln -s ../usr/lib/os-release "$1/etc/os-release"
	echo old-loop > "$1/etc/hostname"; echo alice > "$1/home/alice/data"; echo site > "$1/srv/www/index.html"
}
# the machine: the loop devices as lsblk shows them, under names the plan
# takes for disks' (a loop device is none of a machine's); the first is the
# EFI system partition; what is on them as blkid finds it (lsblk asks
# udev, which a container has not). Every other fact is the installer's own
DISKS=(); ESP=""
kd_fact_lsblk() {
	local j d
	j="$(lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,PARTUUID,PARTLABEL,PARTTYPE,PARTN,PTTYPE,MOUNTPOINTS,RO,RM,TRAN,START "${DISKS[@]}" \
		| jq -c --arg esp "$ESP" '.blockdevices |= map(.name = "disk" + (.name | sub("^loop"; "")) | .type = "part" | if .path == $esp then .parttype = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" else . end)')"
	for d in "${DISKS[@]}"; do
		j="$(jq -c --arg d "$d" --arg t "$(blkid -p -s TYPE -o value "$d" 2>/dev/null || true)" --arg u "$(blkid -p -s UUID -o value "$d" 2>/dev/null || true)" --arg l "$(blkid -p -s LABEL -o value "$d" 2>/dev/null || true)" \
			'def n: if . == "" then null else . end; .blockdevices |= map(if .path == $d then .fstype = ($t | n) | .uuid = ($u | n) | .label = ($l | n) else . end)' <<< "$j")"
	done
	printf '%s\n' "$j"
}
efi=1; arch=arm64; os=test-os; unpacked_mb=0; keep_root_arg=""; keep_root=0; keep_format=()
spec='{"layout": {"parts": [{"mountpoint": "/", "filesystem": "ext4"}]}}'
# plan: kd_build's plan into kd_plan; its status
plan() {
	local rc=0
	( set -e; kd_build ) > "$W/plan.json" || rc=$?
	kd_cleanup
	kd_plan="$(cat "$W/plan.json")"
	return "$rc"
}
# write [FUNCTIONS]: kd_write for kd_plan in a shell of its own, as the
# installer runs it and ends (with FUNCTIONS defined first: what makes it fail)
write() {
	local rc=0
	( set -e; eval "${1:-}"; trap kd_cleanup EXIT; kd_write ) >> "$W/log" 2>&1 || rc=$?
	return "$rc"
}
has() { jq -e "$@" >/dev/null <<< "$kd_plan"; }

# --- looks: reading changes nothing --------------------------------------------

echo "=== looks: file systems that were not unmounted cleanly, read and left as they were"
for fs in ext4 xfs btrfs; do
	command -v "mkfs.$fs" >/dev/null || { echo "skipped $fs: no mkfs.$fs"; continue; }
	img d "live-$fs" 400
	case "$fs" in ext4) mkfs.ext4 -q -F "$d" ;; xfs) mkfs.xfs -q -f "$d" ;; btrfs) mkfs.btrfs -q -f "$d" >/dev/null ;; esac
	mount "$d" "$W/mnt" 2>/dev/null || { echo "skipped $fs: this kernel does not mount it"; unloop "$d"; rm -f "$W/live-$fs.img"; continue; }
	old_root "$W/mnt"
	printf 'UUID=%s / %s defaults 0 1\n' "$(uuid "$d")" "$fs" > "$W/mnt/etc/fstab"
	sync
	# more, which is in the journal or log alone when the copy is taken
	for i in $(seq 1 200); do echo "$i" > "$W/mnt/home/alice/f$i"; done
	[ "$fs" != btrfs ] || for i in $(seq 1 20); do echo "$i" > "$W/mnt/home/alice/g$i"; sync -d "$W/mnt/home/alice/g$i"; done
	[ "$fs" = btrfs ] || sync -f "$W/mnt/home/alice/f1"
	cp --sparse=always "$W/live-$fs.img" "$W/crash-$fs.img"
	umount "$W/mnt"; unloop "$d"; rm -f "$W/live-$fs.img"
	c="$(losetup --show -f "$W/crash-$fs.img")"; LOOPS+=("$c")
	before="$(sha256sum < "$W/crash-$fs.img")"
	if [ "$fs" = xfs ] && ! kd_look "$c" xfs; then
		# (a kernel that does not take norecovery on a read-only mount: the look fails, and says so)
		kd_unlook
		[ "$(kd_fact_fs "$c" xfs | jq -r '.error // ""')" != "" ] || fail "xfs: a look that fails is not reported"
		sync; [ "$(sha256sum < "$W/crash-$fs.img")" = "$before" ] || fail "xfs: a failed look changed the device"
		echo "skipped xfs beyond the failed look, which changed nothing and is reported: this kernel does not mount it read-only without its log"
		unloop "$c"; rm -f "$W/crash-$fs.img"; continue
	fi
	kd_unlook
	f="$(kd_fact_fs "$c" "$fs")"; r="$(kd_fact_root "$c" "" "$fs")"; u="$(kd_fact_usage "$c" "" "$fs" 1)"
	sync; after="$(sha256sum < "$W/crash-$fs.img")"
	[ "$before" = "$after" ] || fail "$fs: the looks changed the device"
	[ "$(blockdev --getro "$c")" = 0 ] || fail "$fs: the device is left read-only"
	jq -e '.dirty == true' >/dev/null <<< "$f" || fail "$fs: not seen as not unmounted cleanly ($f)"
	# (what a log holds alone may not be seen: then the plan says where to look, and nothing is taken for the root)
	if jq -e '(.roots | length) == 1' >/dev/null <<< "$f"; then
		jq -e '(.fstab | test(" / ")) and (.modules == ["6.1.0-13-test"]) and .resume == false' >/dev/null <<< "$r" || fail "$fs: the old root is not read ($r)"
		jq -e '.dirty == true and (.usage | map(.path) | index("/home/alice") != null)' >/dev/null <<< "$u" || fail "$fs: the old root's usage is not read ($u)"
		ok "$fs: the old system found (dirty), fstab, kernels and usage read; the device byte for byte as before, writable again"
	else
		ok "$fs: no old system seen without its log (dirty: reported); the device byte for byte as before"
	fi
	unloop "$c"; [ "$fs" = ext4 ] || rm -f "$W/crash-$fs.img"
done

# --- a machine: ESP, /boot, a root that was not unmounted cleanly, data ------------

echo "=== root: a machine of loop devices; the plan from the installer's own looks"
[ -f "$W/crash-ext4.img" ] || fail "no ext4 root"
mv "$W/crash-ext4.img" "$W/root.img"
ROOTDEV="$(losetup --show -f "$W/root.img")"; LOOPS+=("$ROOTDEV")
img ESP esp 64; img BOOT boot 320; img DATA data 100; img OTHER other 64
mkfs.vfat -F 32 "$ESP" >/dev/null; mkfs.ext4 -q -F -L bootfs "$BOOT"; mkfs.ext4 -q -F -L data "$DATA"; mkfs.ext4 -q -F -L other "$OTHER"
boot_was="$(uuid "$BOOT")"; root_was="$(uuid "$ROOTDEV")"
mount "$ESP" "$W/mnt"; mkdir -p "$W/mnt/EFI/old" "$W/mnt/EFI/BOOT"; echo "search --fs-uuid $root_was" > "$W/mnt/EFI/old/grub.cfg"; echo loader > "$W/mnt/EFI/BOOT/BOOTAA64.EFI"; umount "$W/mnt"
mount "$BOOT" "$W/mnt"; mkdir -p "$W/mnt/grub"; echo kernel > "$W/mnt/vmlinuz-6.1.0-13-test"; umount "$W/mnt"
mount "$DATA" "$W/mnt"; echo data-marker > "$W/mnt/marker"; umount "$W/mnt"
mount "$OTHER" "$W/mnt"; echo other-marker > "$W/mnt/marker"; umount "$W/mnt"
# (the fstab of the root, written without mounting it for writing: the journal stays as it is)
debugfs -w -R "rm /etc/fstab" "$ROOTDEV" >/dev/null 2>&1
printf 'UUID=%s / ext4 defaults 0 1\n%s /boot ext4 defaults 0 2\nUUID=%s /boot/efi vfat umask=0077 0 1\nLABEL=data /data ext4 defaults 0 2\n' "$root_was" "$BOOT" "$(uuid "$ESP")" > "$W/fstab"
debugfs -w -R "write $W/fstab /etc/fstab" "$ROOTDEV" >/dev/null 2>&1
DISKS=("$ESP" "$BOOT" "$ROOTDEV" "$DATA" "$OTHER")
before="$(sums)"
plan || fail "the plan (status $?): $(jq -r '.refused // ""' "$W/plan.json"); the machine: $(kd_fact_lsblk | jq -c '[.blockdevices[] | {path, fstype, uuid, label}]'); the old fstab: $(debugfs -R "cat /etc/fstab" "$ROOTDEV" 2>/dev/null | paste -sd'|' -)"
[ "$(sums)" = "$before" ] || fail "the plan changed a device"
has --arg r "$ROOTDEV" --arg b "$BOOT" --arg d "$DATA" --arg o "$OTHER" '(.root.device == $r) and ([.format[].device] == [$r, $b]) and ([.keep[] | select(.mountpoint == "/data") | .device] == [$d])
	and ([.untouched[].device] == [$o]) and (.warnings | map(test("not unmounted cleanly")) | any) and (.warnings | map(test("holds the old system.s kernels")) | any)
	and (.keep[0].note | test("EFI/old")) and (.warnings | map(test("EFI/BOOT")) | any)' \
	|| fail "the plan is not the one of this machine: $(kd_text)"
ok "the plan: the root (not unmounted cleanly) and /boot (named as the kernel named it, proven by its kernels) formatted, /data kept, the old loader on the ESP found; every device byte for byte as before"

echo "=== root: an installation that fails while the root is formatted, and the next ones"
# 1: the root is formatted, and nothing of the old system is on it yet
kd_wrote=0
write 'kd_marker_write() { die "the test stops the installation here"; }' && fail "the installation that was to fail did not"
[ -e "$KD_DIR/unsaved" ] && [ -f "$KD_DIR/old/etc/fstab" ] || fail "nothing of the old system is left in memory after a failure with nothing saved on the root"
[ "$(uuid "$ROOTDEV")" != "$root_was" ] || fail "the root was not formatted"
plan || fail "the second plan (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
has --arg b "$BOOT" '.resumed == true and .shrunk != true and (.candidates[0].ram == true) and ([.format[].device] | index($b) != null) and ([.keep[].mountpoint] | index("/data") != null) and (.warnings | map(test("in memory")) | any)' \
	|| fail "the plan after a failure with nothing saved on the root does not read the old fstab from memory: $(kd_text)"
ok "failed with the root formatted and nothing saved on it: the next plan reads the old fstab from memory (resumed, nothing missing)"
# 2: the same command: everything formatted, the old system's files on the new root
write || fail "the second attempt: $(tail -n 3 "$W/log")"
[ ! -e "$KD_DIR/unsaved" ] || fail "the copy in memory is still called the only one"
mount -o ro "$ROOTDEV" "$W/mnt"
kd_marker_ok "$W/mnt/$KD_MARK" || fail "what was saved on the root does not read back"
grep -q "^$boot_was " "$W/mnt/$KD_MARK/formats" && grep -q "^UUID=$boot_was /boot" "$W/mnt/$KD_MARK/etc/fstab" || fail "the saved fstab does not name /boot by its UUID, or its mkfs was not written down: $(cat "$W/mnt/$KD_MARK/etc/fstab" "$W/mnt/$KD_MARK/formats")"
boot_new="$(awk -v o="$boot_was" '$1 == o {print $2}' "$W/mnt/$KD_MARK/formats")"
umount "$W/mnt"
[ "$(uuid "$BOOT")" = "$boot_new" ] || fail "/boot does not have the UUID that was written down before its mkfs"
[ "$(blkid -s LABEL -o value "$BOOT")" = bootfs ] || fail "/boot lost its label"
mount -o ro "$DATA" "$W/mnt"; [ "$(cat "$W/mnt/marker")" = data-marker ] || fail "/data changed"; umount "$W/mnt"
ok "the second attempt formats the root and /boot (its label kept, its new UUID written down first) and saves the old system's files on the root"
# 3: and fails after that: the third plan reads them there; /boot is found by the UUID written down
plan || fail "the third plan (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
has --arg b "$BOOT" '.resumed == true and .shrunk != true and (.candidates[0].ram != true) and ([.format[] | select(.mountpoint == "/boot") | .device] == [$b]) and ([.keep[].mountpoint] | index("/data") != null)' \
	|| fail "the plan after a failure does not find /boot and /data again: $(kd_text)"
write || fail "the third attempt: $(tail -n 3 "$W/log")"
plan || fail "the fourth plan"
has --arg b "$BOOT" '.resumed == true and .shrunk != true and ([.format[] | select(.mountpoint == "/boot") | .device] == [$b])' || fail "/boot, made anew twice, is not found by the plan after: $(kd_text)"
ok "the plans after: the old fstab from the root, /boot found by the UUID written down before each mkfs, nothing missing"
# 4: what was saved, damaged
mount "$ROOTDEV" "$W/mnt"; echo x >> "$W/mnt/$KD_MARK/etc/fstab"; umount "$W/mnt"
if plan; then fail "a damaged record of the old system is taken"; fi
jq -r .refused "$W/plan.json" | grep -q "damaged" || fail "the refusal does not say that what was saved is damaged: $(jq -r .refused "$W/plan.json")"
ok "what was saved of the old system, damaged: refused ($(jq -r .refused "$W/plan.json" | cut -c1-80)...)"
mount -o ro "$OTHER" "$W/mnt"; [ "$(cat "$W/mnt/marker")" = other-marker ] || fail "a device outside the plan changed"; umount "$W/mnt"

# --- --keep-root: the move aside ---------------------------------------------------

echo "=== move: --keep-root, the old system moved aside in pieces"
for l in "$ROOTDEV" "$BOOT" "$DATA" "$OTHER"; do unloop "$l"; done; rm -f "$W/root.img" "$W/boot.img" "$W/data.img" "$W/other.img"; rm -rf "$KD_DIR/old" "$KD_DIR/unsaved"
img ROOTDEV root 300; mkfs.ext4 -q -F "$ROOTDEV"; mount "$ROOTDEV" "$W/mnt"; old_root "$W/mnt"
printf 'UUID=%s / ext4 defaults 0 1\nUUID=%s /boot/efi vfat umask=0077 0 1\n' "$(uuid "$ROOTDEV")" "$(uuid "$ESP")" > "$W/mnt/etc/fstab"
if chattr +i "$W/mnt/srv" 2>/dev/null; then immutable=1; else immutable=0; fi
umount "$W/mnt"
DISKS=("$ESP" "$ROOTDEV"); keep_root=1
if [ "$immutable" = 1 ]; then
	if plan; then fail "an immutable entry at the top of the old root is not refused with --keep-root"; fi
	jq -r .refused "$W/plan.json" | grep -q "/srv at the top of the old root cannot be renamed" || fail "the refusal does not name the immutable entry: $(jq -r .refused "$W/plan.json")"
	ok "an entry that cannot be renamed (chattr +i /srv): refused by the plan, nothing written"
	mount "$ROOTDEV" "$W/mnt"; chattr -i "$W/mnt/srv"; umount "$W/mnt"
else
	echo "skipped: chattr +i is not possible here"
fi
plan || fail "the plan with --keep-root (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
kd_wrote=0
write 'mv() { case "$1" in */srv) return 1 ;; esac; command mv "$@"; }' && fail "the move that was to fail did not"
mount -o ro "$ROOTDEV" "$W/mnt"
kd_marker_ok "$W/mnt/$KD_MARK" && [ "$(kd_state "$W/mnt/$KD_MARK" root)" = old ] || fail "nothing was saved before the move began"
aside="$(kd_state "$W/mnt/$KD_MARK" aside)"; [ -d "$W/mnt/$aside/etc" ] && [ -d "$W/mnt/srv" ] || fail "the move did not stop half-way: $(ls -A "$W/mnt")"
umount "$W/mnt"
plan || fail "the plan after the interrupted move (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
has '.resumed == true and .shrunk != true and .options.keep_root == true' || fail "the plan after the interrupted move is not a second attempt: $(kd_text)"
write || fail "the second attempt: $(tail -n 3 "$W/log")"
mount -o ro "$ROOTDEV" "$W/mnt"
[ "$(ls -A "$W/mnt" | grep -vx "lost+found\|$KD_MARK" | paste -sd' ' -)" = "$aside" ] || fail "the top of the root is not the one directory of the old system: $(ls -A "$W/mnt" | paste -sd' ' -)"
[ "$(cat "$W/mnt/$aside/srv/www/index.html" "$W/mnt/$aside/home/alice/data" "$W/mnt/$aside/etc/hostname" | paste -sd' ' -)" = "site alice old-loop" ] || fail "the old system is not whole in $aside"
[ "$(kd_state "$W/mnt/$KD_MARK" root)" = aside ] || fail "the state does not say that the old system is aside"
umount "$W/mnt"
ok "a move that failed half-way: saved first, found again by the plan, carried on into the same directory ($aside), the old system whole there"
# the installation fails later: what it unpacked is not the old system
mount "$ROOTDEV" "$W/mnt"; mkdir -p "$W/mnt/etc" "$W/mnt/usr"; echo new > "$W/mnt/etc/hostname"; umount "$W/mnt"
plan || fail "the plan after the failed installation"
write || fail "the third attempt: $(tail -n 3 "$W/log")"
mount -o ro "$ROOTDEV" "$W/mnt"
[ "$(cat "$W/mnt/$aside/etc/hostname")" = old-loop ] && [ "$(cat "$W/mnt"/failed-install-*/etc/hostname)" = new ] && [ ! -e "$W/mnt/etc" ] || fail "what the failed attempt unpacked is not set aside apart from the old system: $(ls -A "$W/mnt" | paste -sd' ' -)"
umount "$W/mnt"
ok "an attempt after the old system was aside: what the failed one unpacked goes into /failed-install-DATE, the old system stays in $aside"
keep_root=0

# --- btrfs: the root in a subvolume ----------------------------------------------

echo "=== btrfs: the root in the default subvolume, the installation failing between its deletion and the new one"
if command -v mkfs.btrfs >/dev/null && grep -qw btrfs /proc/filesystems; then
	unloop "$ROOTDEV"; rm -f "$W/root.img"; rm -rf "$KD_DIR/old" "$KD_DIR/unsaved"
	img ROOTDEV root 600; mkfs.btrfs -q -f "$ROOTDEV" >/dev/null; mount "$ROOTDEV" "$W/mnt"
	for s in @ @home @snapshots; do btrfs subvolume create "$W/mnt/$s" >/dev/null; done
	old_root "$W/mnt/@"; echo home-marker > "$W/mnt/@home/marker"; echo precious > "$W/mnt/@snapshots/kept"
	printf 'UUID=%s / btrfs subvol=@ 0 0\nUUID=%s /home btrfs subvol=@home 0 0\nUUID=%s /boot/efi vfat umask=0077 0 1\n' "$(uuid "$ROOTDEV")" "$(uuid "$ROOTDEV")" "$(uuid "$ESP")" > "$W/mnt/@/etc/fstab"
	btrfs subvolume create "$W/mnt/@/var/lib/machines" >/dev/null
	btrfs subvolume set-default "$(btrfs subvolume list "$W/mnt" | awk '$NF == "@" {print $2}')" "$W/mnt" >/dev/null
	umount "$W/mnt"
	DISKS=("$ESP" "$ROOTDEV")
	# the guard first: --format of a subvolume of the root's file system, the root's own fstab line leading nowhere
	keep_root_arg="$ROOTDEV:@"; keep_format=(/home)
	if plan; then fail "--format of a subvolume of the root's btrfs is planned"; fi
	jq -r .refused "$W/plan.json" | grep -q "the root's own file system" || fail "the refusal: $(jq -r .refused "$W/plan.json")"
	keep_root_arg=""; keep_format=()
	plan || fail "the plan (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
	has '(.format[0].how == "subvolume") and (.warnings | map(test("default subvolume")) | any)' || fail "the plan does not say what becomes of the default subvolume: $(kd_text)"
	# a plan that formats the root's file system beside the subvolume: kd_write refuses it whole
	good="$kd_plan"; before="$(sums)"
	kd_plan="$(jq -c --arg u "$(uuid "$ROOTDEV")" '.format += [{device: .root.device, filesystem: "btrfs", mountpoint: "/home", how: "mkfs", was: "btrfs", uuid: $u, label: ""}]' <<< "$good")"
	kd_wrote=0
	write && fail "a plan that formats the root's file system beside its subvolume was written"
	[ "$(sums)" = "$before" ] || fail "the refused plan changed a device"
	ok "guard: --format of a subvolume of the root's btrfs is refused by the plan, and kd_write refuses such a plan with nothing written"
	kd_plan="$good"; kd_wrote=0
	write 'btrfs() { case "$1 $2" in "subvolume create") return 1 ;; esac; command btrfs "$@"; }' && fail "the installation that was to fail did not"
	mount -o ro,subvolid=5 "$ROOTDEV" "$W/mnt"
	[ ! -e "$W/mnt/@" ] && kd_marker_ok "$W/mnt/$KD_MARK" && [ "$(kd_state "$W/mnt/$KD_MARK" subvolume)" = @ ] || fail "after the deletion, nothing of the old system is saved at the top: $(ls -A "$W/mnt")"
	umount "$W/mnt"
	plan || fail "the plan after the deletion (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
	has '.resumed == true and .shrunk != true and .root.subvolume == "@" and ([.keep[].mountpoint] | index("/home") != null)' || fail "the plan after the deletion does not know the old system: $(kd_text)"
	write || fail "the second attempt: $(tail -n 3 "$W/log")"
	mount -o ro,subvolid=5 "$ROOTDEV" "$W/mnt"
	[ -d "$W/mnt/@" ] && [ "$(cat "$W/mnt/@home/marker" "$W/mnt/@snapshots/kept" | paste -sd' ' -)" = "home-marker precious" ] || fail "the subvolumes after: $(btrfs subvolume list "$W/mnt")"
	btrfs subvolume get-default "$W/mnt" | grep -q ' path @$' || fail "the default subvolume is not the new @: $(btrfs subvolume get-default "$W/mnt")"
	umount "$W/mnt"
	ok "the default subvolume deleted (the top the default meanwhile), the failure after it found again by the plan, the new @ made and the default again; @home and @snapshots as they were"
	# the installation is complete; the system that is there now, once more with --keep-root: renamed, and a second attempt
	TARGET="$W/none"; kd_done
	mount -o subvolid=5 "$ROOTDEV" "$W/mnt"
	[ ! -e "$W/mnt/$KD_MARK" ] || fail "what was saved for a second attempt is left when the installation is complete"
	old_root "$W/mnt/@"
	printf 'UUID=%s / btrfs subvol=@ 0 0\nUUID=%s /home btrfs subvol=@home 0 0\nUUID=%s /boot/efi vfat umask=0077 0 1\n' "$(uuid "$ROOTDEV")" "$(uuid "$ROOTDEV")" "$(uuid "$ESP")" > "$W/mnt/@/etc/fstab"
	umount "$W/mnt"
	keep_root=1
	plan || fail "the plan with --keep-root (status $?): $(jq -r '.refused // ""' "$W/plan.json")"
	write || fail "--keep-root: $(tail -n 3 "$W/log")"
	plan || fail "the plan after --keep-root"
	write || fail "--keep-root, a second attempt: $(tail -n 3 "$W/log")"
	mount -o ro,subvolid=5 "$ROOTDEV" "$W/mnt"
	l="$(btrfs subvolume list "$W/mnt" | sed 's/.* path //' | paste -sd' ' -)"
	echo "$l" | grep -q '@-old-[0-9-]* ' && echo "$l" | grep -q '@-failed-[0-9-]*' && [ -d "$W/mnt/@" ] || fail "--keep-root twice: the subvolumes are $l"
	umount "$W/mnt"
	ok "--keep-root: the root subvolume renamed @-old-DATE; a second attempt renames what the first made @-failed-DATE and leaves @-old-DATE"
	keep_root=0
else
	echo "skipped: no btrfs here"
fi
echo "==> ok"
