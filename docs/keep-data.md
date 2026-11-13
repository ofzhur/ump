# Keeping the data

`ump server reinstall` erases every disk of a server and builds the
profile's [disk layout](disk-layouts.md) on them. `--keep-data` is the
other way to install: the old system is replaced and its data stays. No
disk is wiped, no partition table, array or volume group is changed and
no layout is applied. The installer finds the old system, reads its
`/etc/fstab`, formats the root (and a separate `/boot`), and mounts
every other volume in the new system where the old one had it. It is
for a server whose operating system is to be renewed or replaced while
its volumes of data, on LVM or not, stay as they are.

```console
$ ump server reinstall db-01 --keep-data --dry-run        # the plan, nothing written
$ ump server reinstall db-01 --keep-data                  # two questions: the rescue system, then the plan
$ ump server reinstall db-01 --keep-data --format /var    # the old /var volume becomes the new /var
$ ump server reinstall db-01 --keep-data --keep-root      # nothing formatted but /boot: the old system moves aside
$ ump server reinstall db-01 --keep-data --root /dev/vg0/root   # the old root, where the installer finds several or none
```

Keeping the data is no backup. The plan says what is lost before
anything is written, and the installer writes only what the plan
names, but a disk that is formatted by mistake stays formatted: have a
copy of what matters.

## How it runs

Everything else of a reinstall is as without `--keep-data`: the
profile, the users and keys, the userdata, the firewall, the private
addresses, the server's lock, the record, the inventory and the
provisioning ([`ump server reinstall`](cli.md#install-ump-server-reinstall)).
What differs is the middle:

1. **The checks**, before the server is touched. `--keep-data` takes no
   `--layout` and no `--disks` (it applies no layout and chooses no
   disks) and no `--unattended` (it asks about its plan); `--root`,
   `--keep-root` and `--format` belong to it; the new system is one of
   the Ubuntu and Debian images. Anything else is exit status 2.
2. **The first question.** `Reinstall hetzner-cloud/234567 (db-01,
   203.0.113.10) with ubuntu-26.04 (...) as db-01? Its data is kept: no
   disk is wiped. The running system goes down and the server boots the
   rescue system, where the installer finds the old system and plans
   what it formats and what it keeps; nothing is written to a disk
   until you confirm that plan.`
3. **The rescue system**, entered as for any reinstall: by a kexec from
   the running system where it lets UMP in, else through the provider's
   rescue system. Never through a provider's own reinstall with a stock
   Linux, the way in for other reinstalls where no rescue system takes
   UMP's key (Vultr, for one): it erases the disks. There the running
   system must let UMP in, and ump says so when it does not (exit
   status 5, nothing changed).
4. **The plan.** The installer runs in the rescue system with `--plan`:
   it looks, reading only, and prints what it will format, keep, set
   aside, leave unused and leave untouched, what the old root holds
   that would be lost, and its warnings, with the hash of the plan. A
   machine it will not do is [refused](#what-is-refused) here.
5. **The second question**, under the plan: `Install ubuntu-26.04 on
   db-01 as the plan above says (plan 8911da13c974)? What it lists under
   FORMAT and LOST is lost.`
6. **The installation.** The installer runs again, with `--confirm` and
   the hash. It finds everything anew, and writes only when the plan it
   makes now has that hash: a disk that changed meanwhile, or another
   option, is a refusal. It formats only devices the confirmed plan
   names, and only while they hold the UUID the plan saw. The line
   `==> keep-data: writing begins` marks the first write; before it,
   nothing was changed.
7. **The reboot**, the check of the new system, the record and the
   provisioning, as always.

A no at the second question, a refusal, and an installer that stops
before it writes leave the disks as they were (exit status 3). The
server is in the rescue system then, and ump says how its old system
starts again: `ump server unrescue NAME --reboot` where the provider's
rescue system is armed, else `ump server reboot NAME --hard` (the
rescue system runs from memory and has no disk mounted). The private
addresses the server was given for the installation go back.

`--dry-run` stops after the plan and asks one question, about the
rescue system: the running system goes down for a dry run too. `--yes`
answers both questions; it takes a plan whose old system the installer
found by itself, or that `--root` names. `--no-reboot` and
`--no-provision` are as without `--keep-data`. `-o json` (and `yaml`)
carries the installer's plan under `keep_data`, in a dry run and after
an installation, as [the installer's README](../install/README.md#keeping-the-data)
describes it; `layout` is `keep-data`.

## What becomes of the volumes

The old system is the file system that holds `/etc/fstab` and
`/etc/os-release` and whose fstab mounts that very file system at `/`.
Every entry of its fstab is one of these:

| In the old fstab | In the plan | What happens |
|---|---|---|
| `/` | FORMAT | made anew, with the file system the profile's layout gives `/` (ext4, xfs or btrfs; else ext4). With `--keep-root`: KEEP and ASIDE, nothing formatted |
| `/boot` | FORMAT | made anew as ext4, the new `/boot` |
| `/boot/efi`, `/efi` (UEFI) | KEEP | the EFI system partition: never formatted, mounted at `/boot/efi`, the new boot loader added |
| `/var`, `/var/lib`, `/var/cache` | ASIDE | kept and mounted at `/var.old`, `/var/lib.old`, `/var/cache.old`: the new system's own is on the root. `--format /var` makes it the new `/var` instead |
| `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc` and below (not `/usr/local`) | UNUSED | left as it is, not mounted: the new system has its own |
| swap on a device | KEEP | used as it is |
| a swap file | dropped | it was on the old root |
| anything else on a device: `/home`, `/srv/...`, `/data`, `/var/lib/docker`, `/usr/local` | KEEP | its line of the old fstab in the new one, with its options, its mountpoint made |
| no device: NFS and CIFS, bind mounts, tmpfs | KEEP | its line as it was. `nfs-common` or `cifs-utils` is installed for a network file system; a bind mount whose source was on the old root gets an empty directory there |
| a device that is not there, or holds another file system than the fstab says | dropped | with a warning in the plan |

Devices the old fstab does not name are UNTOUCHED: other volume groups
and unmounted volumes, other partitions and disks, members of ZFS pools
and LUKS containers nothing opens. The plan lists them, and the new
system does not mount them.

LOST is what the old root holds beyond the system itself, with sizes:
`/etc`, `/root` and every home directory, the directories under
`/var/lib` and the like from 64 MB, whatever else lies at its top from
1 MB. It is a reminder, not a complete list: everything on a formatted
file system is lost.

The new `/etc/fstab` has the root, `/boot`, the EFI system partition
and swap, then the kept entries in their old order. An entry stays
spelled as it was (`UUID=`, `LABEL=`, `/dev/mapper/...`, `/dev/vg0/data`),
but for a kernel name (`/dev/sdb1`, `/dev/md0`), which is written as
`UUID=...`: the new kernel may name the device otherwise.

## The old root

Exactly one old system on the machine is the root. With several (two
systems on one machine), or with none that the installer can identify
(no `/etc/os-release`, an fstab that names another root), it refuses
and says what it found; `--root DEVICE` then says which: a device
(`/dev/sda2`), a volume's path (`/dev/vg0/root`), `UUID=...`, or on
btrfs `DEVICE:SUBVOLUME`. A copy of a root on a data volume, and a
snapshot, name another root in their fstab and are not taken for one.
`--root` on a file system without an old system makes it the root and
mounts no other volume.

By default the root is formatted. `--keep-root` formats nothing but a
separate `/boot` (and what `--format` names): the old system's files
move into
`/old-root-YYYYMMDD-HHMMSS` on the same file system, and the new system
is unpacked beside them. The file system keeps its type and its
options, and must have room: the new system's unpacked size and 1 GiB
free. A formatted root must be twice the unpacked size.

## Formatting more: `--format`

`--format MOUNTPOINT` (repeatable) formats one more volume of the old
fstab and mounts it there again, with the file system it had (ext4,
xfs, btrfs or vfat) and its label. `--format /var` turns the old `/var`
volume into the new system's `/var`. The EFI system partition, an
unused volume (`/usr`), an encrypted one, a network file system and a
mountpoint the old fstab does not have are refused; `--format /` with
`--keep-root` contradicts itself.

## Storage

| Storage | What keep-data does |
|---|---|
| Plain partitions | taken as they are; no partition table is written |
| LVM | volume groups are activated to look; no volume is made, resized or removed. A volume on a thin pool gets `thin-provisioning-tools` in the new system. Volumes the fstab does not name are untouched |
| md RAID | arrays are assembled read-only to look, never created or resynchronised; those that are written to become writable when writing begins. The new system gets an `mdadm.conf` of the arrays that are there, and its initramfs is rebuilt with them. An array is found by the name the old `mdadm.conf` gave it |
| LUKS | containers are not opened. A data volume of the old `/etc/crypttab` is kept with its crypttab line, and `cryptsetup` is installed; a key file that was on the old root is copied to the new one (same path, owner and mode; held in memory meanwhile). A volume that cannot be opened unattended (a passphrase, a key elsewhere, a key file that does not open it) gets `nofail` in crypttab and fstab, and the plan says so. An encrypted root is refused |
| btrfs | a root in a subvolume: the subvolume is deleted, with those inside it, and made anew; the rest of the file system and the other subvolumes' fstab lines stay. With `--keep-root` it is renamed `NAME-old-DATE`. The file system is never made anew while the fstab mounts other subvolumes of it |
| ZFS | pool members are left as they are and pools are not imported in the rescue system. On Ubuntu `zfsutils-linux` is installed, with the old `/etc/hostid` and pool cache, and the pools are imported at boot; on Debian they are left for you, and the plan says so. A ZFS root is refused |

A machine that boots with UEFI keeps its EFI system partition, found in
the fstab or by its partition type: the new boot loader is added, and a
boot loader of the old system on it is renamed `...-old-DATE`, so that
the firmware's entry for it leads nowhere. With BIOS, GRUB goes onto
the disks under `/boot` and onto every other disk that carries GRUB's
boot code; the plan names them under BOOT LOADER.

A kept `/home` (or `/root`) gets the profile's users: a home directory
that is there goes to the user of its name, who gets the user ID it
belongs to when that is free; its `authorized_keys` is replaced, the
old one saved beside it.

## What is refused

The installer refuses, with the reason and before anything is written,
what it cannot do with certainty:

- no old system, or several, or one whose fstab does not mount it at
  `/`, without `--root`;
- an encrypted root, a ZFS root, a root that is read-only;
- a btrfs root it cannot read with certainty: the default subvolume not
  named in the fstab, the top of a file system whose subvolumes other
  entries mount, a file system over several devices at its top;
- a root or `/boot` too small (twice the image's unpacked size for a
  formatted root; 300 MB for `/boot`), too little room beside a kept
  root, an EFI system partition with less than 16 MB free;
- no way to boot: UEFI without an EFI system partition, BIOS with a GPT
  disk that has no BIOS boot partition, BIOS on arm64;
- disks in use in the rescue system;
- an fstab or crypttab field that names several devices here;
- NixOS, Arch Linux and CachyOS as the new system, which ump refuses
  before the server is touched.

## The plan

The plan for a machine with Debian 12 on LVM, a separate `/boot` and
`/var`, an xfs data volume, a plain partition and a tmpfs (the machine
`lvm` of the installer's test, each line after the installer's `==> `):

```
keep-data: the old system Debian GNU/Linux 12 (bookworm) (old.example.net) on /dev/mapper/vg0-root; UEFI firmware; the layout of the profile is not applied, no disk is wiped or partitioned
FORMAT (what is on them is lost)
  /dev/mapper/vg0-root  ext4   /  (10G)
  /dev/vda2             ext4   /boot  (1G)
KEEP (as they are, mounted where they were)
  /dev/vda1             vfat   /boot/efi  (the EFI system partition: not formatted, the new boot loader is added)
  /dev/mapper/vg0-swap  swap   swap
  /dev/mapper/vg0-data  xfs    /data
  /dev/vda4             ext4   /srv/plain
  tmpfs                 tmpfs  /tmp
ASIDE (kept, under another name)
  /dev/mapper/vg0-var   ext4   /var -> /var.old  (mounted)
LOST (on the old root, beyond the system itself)
  /backup  2M
  /etc  6M
  /home/alice  1.1G
  /home/bob  1M
  /opt  245M
  /root  1M
  /usr/local  69M
  /var/lib/apt  176M
  /var/lib/docker  8.5G
plan: 8911da13c974ed8c5d80e49b00519c87f95c8b0017f62b7beb2ef47db69c808b (sha256; --confirm takes it)
```

and the `/etc/fstab` the new system gets from it:

```
UUID=...  /  ext4  defaults,errors=remount-ro  0 1
UUID=...  /boot  ext4  defaults  0 2
UUID=769B-CE7F  /boot/efi  vfat  umask=0077  0 1
/dev/mapper/vg0-swap  none  swap  sw  0 0
/dev/mapper/vg0-var  /var.old  ext4  defaults  0 2
/dev/vg0/data  /data  xfs  defaults,noatime  0 2
LABEL=plain  /srv/plain  ext4  defaults  0 2
tmpfs  /tmp  tmpfs  nosuid,nodev,size=1G  0 0
```

With `--keep-root` the same machine formats `/boot` alone, keeps
`/dev/mapper/vg0-root` as `/` and lists it under ASIDE as `/ ->
/old-root-DATE (moved)`; there is no LOST. UNUSED, UNTOUCHED, BOOT
LOADER (BIOS), PACKAGES and WARNINGS appear when they have something to
say.

## Several servers

`ump server multiinstall` takes `--keep-data` and its flags for every
server named, and the keys `keep-data`, `root`, `keep-root` and
`format` per server in a plan:

```yaml
servers:
  web-01: {}                 # wiped, the profile's layout
  db-01:
    keep-data: true
    format: [/var]
```

The servers that keep their data go first: each into the rescue
system, their plans made side by side, each line with `[NAME]` in
front. One question covers the plans, and only then the installations
start, those of the servers that are wiped too. A no ends the run with
nothing written to any server (exit status 3). A server whose plan the
installer refused is a failed installation (exit status 1 at the end);
the others go on. `pxe.yaml` takes none of these keys: its
installations run unattended.

## After a failure

| Exit status | What happened | The disks |
|---|---|---|
| 2 | a check failed before the server was touched | as they were, the old system runs |
| 3 | a question was answered no, the installer refused the machine or the plan, or it stopped before its first write | as they were; the server is in the rescue system, and ump says how the old system starts again |
| 1 | the installation failed after the installer began to write | the old root is gone, or with `--keep-root` moved aside; the data volumes are untouched |
| 5 | the connection ended, or the new system did not answer after the reboot | ump says whether the installer had begun to write |

After exit status 1 the server stays in the rescue system (`ump ssh NAME
--staging`). The same command again, with the same options, recovers:
before its first write the installer saved the old system's fstab,
crypttab and key files on the new root (`/.ump-keep-data`, removed when
an installation completes), and the next run plans from that. The plan
is new, with a new hash, and ump asks about it again.

## The record

The state records the installation with `layout: keep-data` and, under
`keep_data`, the old root, `keep_root: true` where the old system was
moved aside, and the hash of the plan that was confirmed
([the state document](state.md#the-state-document)); `ump server show`
has it as `Keep-data`, and `ump server list -o wide` says `keep-data`
under LAYOUT. On the server, `/etc/ump/install` has `layout=keep-data`,
`keep_data=yes`, `keep_root=`, `keep_plan=` (the hash), `old_system=`,
`formatted=`, `kept=` and `aside=`, and `/etc/ump/keep-data.json` is the
whole plan.

## Tests

`make keep-plan-test` (part of `make check`) makes the plan, the fstab
and the crypttab for the machines of
[`images/testdata/keep-data`](../images/testdata/keep-data) without a
machine and compares them, and checks the guards of the installer.
`make keep-data-test` reinstalls machines in QEMU and reads the kept
devices whole before and after ([the installer's
README](../install/README.md#keeping-the-data) lists both). The Go
tests play the installer with the plans of those machines: the flags
and refusals, both questions and a no at each, `--dry-run`, `--yes`,
the hash handed back, the installer's exit statuses, `multiinstall`,
the record and the listings.
