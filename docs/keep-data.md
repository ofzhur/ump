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

## Before

The running system goes down once the first question is answered, for a
dry run too, and what is on its root is lost unless `--keep-root` is
given. In the old system, first:

- **Stop the services that write to the volumes that are kept**
  (databases above all). The way down is not always a shutdown:

  | The way into the rescue system | The running system |
  |---|---|
  | direct, the default where it lets UMP in as root or a sudo user | ends with a kexec, no shutdown: its file systems are synced and remounted read-only, which commits their journals; its services are not stopped, and what one of them had not written is gone |
  | `--via provider`, where it lets UMP in | is shut down over SSH (`systemctl halt`, as root or with sudo) and waited for, five minutes at most; then the provider resets the server. The clean way |
  | the provider's rescue system, where it does not let UMP in, the shutdown fails or takes longer | is reset as it runs, like a power cut, and a line says so. The file systems were not unmounted: the new system replays the journals of the kept ones when it mounts them |
  | a PXE machine's network boot | with `--via provider` it reboots over SSH (`systemctl reboot`) where it lets UMP in; else you power-cycle it |

- **Copy what you need from the root** to a kept volume or elsewhere:
  `/etc` (with `passwd` and `group`, for [the owners of
  files](#users-and-owners), and `fstab`), crontabs, `/root`, home
  directories on the root, a database under `/var/lib`. LOST in the plan
  names the larger ones, not all. Or give `--keep-root`, which keeps
  it all in `/old-root-DATE`.
- **Name the volumes in `/etc/fstab` by `UUID=` or `LABEL=`** (`blkid`
  shows them), not as the kernel names them (`/dev/sdb1`): the rescue
  system's kernel may name them otherwise, and the installer formats
  nothing on the word of such a name ([below](#kernel-names)).
- **An xfs root wants a clean shutdown.** After a reset its log holds
  what a read-only look cannot see, the old system is not found, and
  the refusal says so. Then mount it once in the rescue system, which
  replays the log (`ump ssh NAME --staging`, `mount /dev/vg0/root /mnt;
  umount /mnt`), and run the command again. An ext4 or btrfs root is
  read as the disk has it, with a warning.

The storage must serve this version's installer and a current rescue
image: an older `install/install.sh` does not know `--keep-data` and
stops (nothing written, exit status 3), and a rescue image from before
2026-11-13 activates no thin pools. The published storage has both; one
of your own gets them with `make upload`
([your own storage](../images/rescue/README.md#your-own-storage)).

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
   disk is wiped. The running system is shut down (not always cleanly:
   services that write to the volumes that are kept are best stopped
   first) and the server boots the rescue system, where the installer
   finds the old system and plans what it formats and what it keeps;
   nothing is written to a disk until you confirm that plan.`
3. **The rescue system**, entered by one of the ways [above](#before).
   Never through a provider's own reinstall with a stock Linux, the way
   in for other reinstalls where no rescue system takes UMP's key
   (Vultr, for one): it erases the disks. There the running system
   must let UMP in, and ump says so when it does not (exit status 5,
   nothing changed).
4. **The plan.** The installer runs in the rescue system with `--plan`:
   it looks, reading only, and prints what it will format, keep, set
   aside, leave unused and leave untouched, what the old root holds
   that would be lost, and its warnings, with the hash of the plan. A
   machine it will not do is [refused](#what-is-refused) here. Its last
   line, `==> the plan is made: stopping here, no disk was touched;
   --confirm with its hash installs it`, is for ump, which hands the
   hash back after the next question.
5. **The second question**, under the plan: `Install ubuntu-26.04 on
   db-01 as the plan above says (plan 85baa9654455)? What it lists under
   FORMAT and LOST is lost.`
6. **The installation.** The installer runs again, with `--confirm` and
   the hash. It finds everything anew, and writes only when the plan it
   makes now has that hash: a disk that changed meanwhile, another
   option, or another image on the storage is a refusal. It formats
   only devices the confirmed plan names, each once, and only while
   they hold the UUID the plan saw. The line `==> keep-data: writing
   begins` marks the first write; before it, nothing was changed.
7. **The reboot**, the check of the new system, the record and the
   provisioning, as always.

`--dry-run` stops after the plan and asks one question (`Bring ... into
the rescue system and show the keep-data plan for ...? (dry run: the
running system is shut down as for the installation, not always
cleanly; nothing is written to a disk)`); the server then stays in the
rescue system, where the same command without `--dry-run` goes on, or
[the old system starts again](#a-no-and-the-way-back). `--yes` answers
both questions, the second only for a plan whose old system the
installer found by itself, or that `--root` names and the installer
knows as one; a plan for a person to read (`root.known` false: no old
system where `--root` points; `shrunk`: less kept than before a
[failure](#after-a-failure)) ends it with exit status 3, nothing
written. `--no-reboot` and `--no-provision` are as without
`--keep-data`. `-o json` (and `yaml`) carries the installer's plan
under `keep_data`, in a dry run and after an installation, as [the
installer's README](../install/README.md#keeping-the-data) describes
it; `layout` is `keep-data`.

## What becomes of the volumes

The old system is the file system that holds `/etc/fstab` and
`/etc/os-release` and whose fstab mounts that very file system at `/`.
Every entry of its fstab is one of these:

| In the old fstab | In the plan | What happens |
|---|---|---|
| `/` | FORMAT | made anew, with the file system the profile's layout gives `/` (ext4, xfs or btrfs; else ext4), its label kept. With `--keep-root`: KEEP and ASIDE, nothing formatted |
| `/boot` | FORMAT | made anew as ext4, its label kept. Under 300 MB: UNUSED, and `/boot` is on the root ([below](#booting)) |
| `/boot/efi`, `/efi`, a vfat `/boot` (UEFI) | KEEP | the EFI system partition: never formatted, mounted at `/boot/efi`, the new boot loader added |
| `/var`, `/var/lib`, `/var/cache` | ASIDE | kept and mounted at `/var.old`, `/var/lib.old`, `/var/cache.old`: the new system's own is on the root. `--format /var` makes it the new `/var` instead |
| `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc` and below (not `/usr/local`) | UNUSED | left as it is, not mounted: the new system has its own |
| swap on a device | KEEP | used as it is |
| a swap file | dropped | it was on the old root |
| anything else on a device: `/home`, `/srv/...`, `/data`, `/var/lib/docker`, `/usr/local` | KEEP | its line of the old fstab in the new one, with its options, its mountpoint made. With `nofail` added: a file system the new kernel may not know (reiserfs), a volume on a USB disk |
| NFS and CIFS, tmpfs | KEEP | its line as it was; `nfs-common` or `cifs-utils` is installed |
| a bind mount | KEEP | its line as it was. A source that was on the old root or a formatted volume gets an empty directory; one on a volume that is set aside is named under `/var.old` |
| a network file system or bind mount at `/var`, `/usr`, `/etc`, ... | KEEP, UNUSED | as a volume there: mounted at `/var.old`, or not at all |
| a loop mount of a file, an overlay, whose files were on the old root | dropped | with a warning |
| a ZFS dataset, a type the installer does not know | KEEP | with `nofail` |
| a device that is not there, or holds another file system than the fstab says | dropped | with a warning |

Devices the old fstab does not name are UNTOUCHED: other volume groups
and unmounted volumes, other partitions and disks, members of ZFS pools
and LUKS containers nothing opens. The plan lists them, and the new
system does not mount them.

LOST is what the old root holds beyond the system itself, with sizes:
`/etc`, `/root` and every home directory, the directories under `/var`,
`/usr/local`, `/mnt` and `/media` from 64 MB, whatever else lies at its
top from 1 MB. It is a reminder, not a complete list (a small database
under `/var/lib` is not in it): everything on a formatted file system
is lost.

The new `/etc/fstab` has the root, `/boot`, the EFI system partition
and swap, then the kept entries in their old order. An entry stays
spelled as it was (`UUID=`, `LABEL=`, `/dev/mapper/...`, `/dev/vg0/data`),
but for a kernel name and an array's name (`/dev/sdb1`, `/dev/md0`,
`/dev/md/data`), which are written as `UUID=...`. The new system's
`findmnt --verify` reads it, and what it finds wrong is a warning.

### Kernel names

`/dev/sdb1` in the old fstab is whatever the rescue system's kernel
calls `sdb1`, which may be another disk. So:

- a kept entry named that way has a warning that says what the name is
  here (file system, label, size): read it;
- `/boot` named that way is formatted only when it holds a kernel
  (`vmlinuz-VERSION`) the old root has the modules of; else the plan is
  refused;
- `--format` of such an entry is refused;
- when the root's own kernel name does not lead to the root (`--root`
  named it), the names are not this machine's: every other entry named
  that way is dropped, and a `/boot` among them refuses the plan.

An array the old `mdadm.conf` names is known by its UUID and is no such
name. The remedy is `UUID=` or `LABEL=` in the old `/etc/fstab`.

## The old root

Exactly one old system on the machine is the root. With several (two
systems on one machine), or with none that the installer can identify
(no `/etc/os-release`, an fstab that names another root), it refuses
and says what it found; `--root DEVICE` then says which: a device
(`/dev/sda2`), a volume's path (`/dev/vg0/root`), `UUID=...`, or on
btrfs `DEVICE:SUBVOLUME`. A copy of a root on a data volume, and a
snapshot, name another root in their fstab and are not taken for one.
`--root` on a file system without an old system makes it the root and
mounts no other volume: the plan warns, has `root.known` false, and is
not for `--yes`.

By default the root is formatted, and must be twice the image's
unpacked size. `--keep-root` formats nothing but a separate `/boot`
(and what `--format` names): the old system's files move into
`/old-root-YYYYMMDD-HHMMSS` on the same file system, whence it cannot
be started again, and the new system is unpacked beside them. The file
system keeps its type and its options, and must have room: the new
system's unpacked size and 1 GiB free.

What was on the old root is, afterwards:

| Installed | Where |
|---|---|
| by default | nowhere: the root was formatted |
| with `--keep-root` | `/old-root-DATE` (a btrfs root subvolume: the subvolume `NAME-old-DATE`); remove it when you have what you need |
| a separate `/var` | mounted at `/var.old` (the old `/var/lib/postgresql` is `/var.old/lib/postgresql`); likewise `/var/lib.old`, `/var/cache.old` |
| the old boot loader | `EFI/NAME-old-DATE` on the EFI system partition |
| the old `authorized_keys` on a kept `/home` | `authorized_keys.old-DATE` beside the new one |

## Formatting more: `--format`

`--format MOUNTPOINT` (repeatable) formats one more volume of the old
fstab and mounts it there again, with the file system it had (ext4,
xfs, btrfs or vfat; ext4 for ext2 and ext3) and its label; a `subvol=`
leaves its line. `--format /var` turns the old `/var` volume into the
new system's `/var`. Refused: the file system the root, `/boot` or the
EFI system partition is on, an unused volume (`/usr`), an encrypted
one, one mounted twice, read-only, on a USB disk or named by a kernel
name, a network file system, a mountpoint the old fstab does not have;
`--format /` with `--keep-root` contradicts itself.

## Storage

| Storage | What keep-data does |
|---|---|
| Plain partitions | taken as they are; no partition table is written |
| LVM | volume groups are activated to look; no volume is made, resized or removed. A volume on a thin pool gets `thin-provisioning-tools` in the new system. Volumes the fstab does not name are untouched |
| md RAID | arrays are assembled read-only to look, never created or resynchronised; only those that are written to (under the root, `/boot`, the EFI system partition, a formatted volume, a kept `/home`) become writable when writing begins. The new system's `mdadm.conf` names the arrays that are there by their UUIDs, under the names the old one gave them, and its initramfs is rebuilt |
| LUKS | containers are not opened. A data volume of the old `/etc/crypttab` is kept with its crypttab line, and `cryptsetup` is installed; a key file that was on the old root is copied to the new one (same path, owner and mode; held in memory meanwhile). A volume that cannot be opened unattended (a passphrase, a key elsewhere, a key file that does not open it) gets `nofail` in crypttab and fstab, and the plan says so; so does the line of a file system that is not found now and may be inside one. An encrypted root is refused |
| crypttab's `swap` and `tmp` | a volume that systemd makes anew at every boot (`swap`, `tmp`, a key from `/dev/urandom`) would overwrite whatever its device holds then. It is dropped, with the fstab entry on it, when the device holds a file system now, is on a USB disk, or has only a kernel name; else kept (a kernel name becomes its `PARTUUID`) and listed under MADE ANEW AT EVERY BOOT with its options. A small file system in front that names it (`offset=`, `LABEL=`) is kept as it is |
| btrfs | a root in a subvolume: the subvolume is deleted, with those inside it, and made anew; the rest of the file system and the other subvolumes' fstab lines stay. With `--keep-root` it is renamed `NAME-old-DATE`. A root subvolume that is the default subvolume: the new one becomes the default. A root at the top of the file system is made anew only when the file system holds no subvolumes but the system's own (`var/lib/machines`, `var/lib/portables`, Docker's and Podman's layers, which go with it and are named in WARNINGS); any other is data, and the plan is refused: `--keep-root` keeps the file system |
| ZFS | pool members are left as they are and pools are not imported in the rescue system. On Ubuntu `zfsutils-linux` is installed, with the old `/etc/hostid` and pool cache, and the pools are imported at boot; on Debian they are left for you, and the plan says so. A ZFS root is refused |

## Booting

A machine that boots with UEFI keeps its EFI system partition, found in
the fstab or by its partition type: the new boot loader is added, and a
boot loader of the old system on it is renamed `...-old-DATE`, so that
the firmware's entry for it leads nowhere. With BIOS, GRUB goes onto
the disks under `/boot`, and is tried on every other disk that carries
GRUB's boot code and has room for it (a failure there is a warning);
never onto a USB disk. The plan names them under BOOT LOADER.

A separate `/boot` smaller than 300 MB is too small for the new
kernels. It is left as it is, unused, and `/boot` is on the root where
GRUB reads that: ext4 on a partition, a linear logical volume or an md
array. Else the plan is refused.

## Users and owners

A kept `/home` (or `/root`) gets the profile's users: a home directory
that is there goes to the user of its name, who gets the user ID it
belongs to when that is free (else its files change owner); its
`authorized_keys` is replaced, the old one saved beside it. A `/home`
the installer cannot mount (encrypted, of the network, a ZFS dataset, a
file system it does not know) would hide the home directories it makes
on the root: the users' keys are then also in
`/etc/ssh/authorized_keys/NAME`, which the new system's sshd reads
besides, and the plan says so.

Everything else on a kept volume keeps its numeric owner and group, and
the new system gives its accounts IDs of its own: the files of
`postgres` may belong to another account, or to none. Compare with the
old `/etc/passwd` and `/etc/group` (your copy, or `/old-root-DATE/etc`)
before a service starts on such a volume, and make the account with
its old ID first (`useradd -u`), or change the owner of the files
(`chown -R`).

## What is refused

The installer refuses, with the reason and before anything is written,
what it cannot do with certainty (exit status 3; the server is in the
rescue system, and [the old system starts again](#a-no-and-the-way-back)
by a reboot):

| Refused | What to do |
|---|---|
| no old system, or several, or one whose fstab does not mount it at `/` | `--root DEVICE`; ump names the candidates |
| a file system that was not unmounted cleanly and shows no system (xfs), or cannot be looked at | mount it once in the rescue system and run again ([before](#before)) |
| an encrypted root, a ZFS root, a root on a USB disk | not in this version |
| a root, `/boot` or EFI system partition that is read-only | take the write protection off |
| a root too small (twice the image's unpacked size) | grow it in the rescue system (`lvextend -r`), which keep-data does not do |
| `--keep-root` without room (the unpacked size and 1 GiB), or with an entry at the top that cannot be renamed (`chattr +i`) | free or grow the root; `chattr -ia`; or install without `--keep-root` |
| a btrfs root it cannot read with certainty: the default subvolume not named in the fstab, the top of a file system whose subvolumes other entries mount, `/boot` in a subvolume of it, several devices at its top; data subvolumes under a root at the top | `--keep-root` for the last two; else not in this version |
| `/boot` twice in the fstab, mounted elsewhere too, encrypted, on a USB disk | not a layout it reads |
| `/boot` under 300 MB where GRUB cannot read `/boot` on the root | a larger `/boot`, made in the rescue system; or install without `--keep-data` |
| `/boot` by a kernel name, without the old system's kernels on it | `UUID=` or `LABEL=` in the old fstab ([kernel names](#kernel-names)) |
| UEFI without an EFI system partition, or with one on LVM or LUKS, unreadable, or with less than 16 MB free | free room on it; else the machine needs another partitioning: install without `--keep-data` |
| BIOS with a GPT disk that has no BIOS boot partition, or an MBR disk whose first partition starts before sector 2048; BIOS on arm64 | install without `--keep-data` |
| disks in use in the rescue system | `umount`, `swapoff`, or reboot the rescue system |
| an fstab or crypttab field that names several devices here (a snapshot with its origin's UUID) | remove the copy, or name the device another way |
| the old fstab mounts the root's device elsewhere too | not a layout it reads |
| `--format` of [what it does not format](#formatting-more---format) | another option |
| what an installation that failed saved, damaged | `--root DEVICE` ([after a failure](#after-a-failure)) |
| NixOS, Arch Linux and CachyOS as the new system | refused by ump before the server is touched (exit status 2) |

A file to change in the old system (`/etc/fstab`) is changed there
before the run, or from the rescue system: `ump ssh NAME --staging`,
`mount DEVICE /mnt`, edit `/mnt/etc/fstab`, `umount /mnt`.

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
plan: 85baa9654455323d5f402b58bf8b05e5e2918b5f575036e93298ddef77abd7d0 (sha256; --confirm takes it)
```

and the `/etc/fstab` the new system gets from it (the two file systems
made anew have new UUIDs):

```
# /etc/fstab, written by UMP (keep-data: the root made anew, the old system's other volumes as they were)
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
/old-root-DATE (moved)`; there is no LOST. A FORMAT row names the label
of what it formats (`, label data`), and the first line says `(--root)`
and `, after an interrupted installation` where that is so. MADE ANEW
AT EVERY BOOT, UNUSED, UNTOUCHED, BOOT LOADER (BIOS), PACKAGES and
WARNINGS appear when they have something to say. The hash is over the
whole plan, the image's version and checksum included.

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
front. One question covers the plans (`--yes` answers it where it would
take each of them), and only then the installations start, those of
the servers that are wiped too. A no ends the run with nothing written
to any server (exit status 3). A server whose plan the installer
refused is a failed installation (exit status 1 at the end); the
others go on. `pxe.yaml` takes none of these keys: its installations
run unattended.

## A no, and the way back

A no at the second question, a refusal, a plan `--yes` does not take,
an installer that stops before it writes, and a dry run leave the
disks as they were. The server is in the rescue system then, which
runs from memory and has no disk mounted, and ump's last line says how
its old system starts again:

| Where | The way back |
|---|---|
| the provider's rescue system is still the boot option | `ump server unrescue NAME --reboot` |
| it is not (after the direct way; providers that arm it for one boot) | `ump server reboot NAME --hard` |
| the provider did not say | both commands are named, with which is for which |
| no power control (a manual server, PXE) | `ump ssh NAME --staging -- systemctl reboot`, or the run's own `ssh` line with `systemctl reboot` where the rescue system has an address of its own |

The old system boots as after the way down it had: after a reset, with
its journals to replay. The private addresses the server was given for
the installation go back. Instead of going back, the same command can
be run again from the rescue system, with other options.

## After a failure

| Exit status | What happened | The disks |
|---|---|---|
| 2 | a check failed before the server was touched | as they were, the old system runs |
| 3 | a question was answered no, the installer refused the machine or the plan or stopped before its first write, or `--yes` did not take the plan | as they were; the server is in the rescue system, [the way back](#a-no-and-the-way-back) in the last line |
| 1, `Nothing was written` in the message | the installer could not be fetched, the plan run failed, the plan could not be read back or was not the plan shown, or the installation failed before `writing begins` | as they were; the same |
| 1, `after the installer began to write` | the installation failed after `==> keep-data: writing begins` | the old root is gone, or with `--keep-root` moved aside; the volumes the plan keeps hold their data |
| 5 | the connection ended, or the new system did not answer after the reboot | the message says whether the installer had begun to write |

After a failure past the first write the server stays in the rescue
system (`ump ssh NAME --staging`), and the same command again recovers:

- Before its first step that destroys, the installer saved what a
  second attempt needs at the top of the root's file system, in
  `/.ump-keep-data` (mode 700): the old fstab, crypttab with its key
  files, `mdadm.conf`, the host ID, the plan that was confirmed, how
  far it came, and checksums. It is removed when an installation
  completes.
- The next run plans from that. Its first line says `after an
  interrupted installation` (`resumed` in the plan), the hash is new,
  and ump asks again; `--yes` takes it.
- A plan that keeps less than the first did (a volume is not found now)
  says so in WARNINGS (`shrunk`): look before you confirm. `--yes` does
  not take it.
- With `--keep-root`, a move that stopped half-way carries on into the
  same `/old-root-DATE`. Where the old system was aside already, what
  the failed attempt unpacked goes into `/failed-install-DATE` (a
  subvolume: `NAME-failed-DATE`), which can be removed. Where the root
  was replaced already, `--keep-root` has nothing left to keep and is
  refused: run without it.

Its limits:

- While the root itself is formatted, a matter of seconds, the saved
  files are in the rescue system's memory alone (`/run/ump-keep-data`).
  A failure then is picked up by the next run in the same rescue
  system; a reboot of the rescue system in between loses them.
- Saved files that are damaged are refused.
- In both cases `--root DEVICE` installs onto the root without the old
  system's other volumes (`root.known` false): they stay as they are,
  and their fstab lines are yours to write, from your copy of the old
  `/etc/fstab`.
- What made the installation fail (a storage that does not answer, a
  full EFI system partition) fails it again until it is mended.

## The record

The state records the installation with `layout: keep-data` and
nothing else of it, in a field every ump reads ([the state
document](state.md#the-state-document)); `ump server show` and `ump
server list -o wide` say `keep-data` as the layout, and no layout can
have that name. The old root and the plan are on the server:
`/etc/ump/install` has `layout=keep-data`, `keep_data=yes`,
`keep_root=`, `keep_plan=` (the hash), `old_system=`, `formatted=`,
`kept=` and `aside=`, and `/etc/ump/keep-data.json` is the whole plan,
as the run's `-o json` carried it.

## Tests

`make keep-plan-test` (part of `make check`) makes the plan, the fstab
and the crypttab for the machines of
[`images/testdata/keep-data`](../images/testdata/keep-data) without a
machine and compares them, and checks the guards of the installer.
`make keep-loop-test` runs the installer's looks and writes on file
systems in loop devices: a look leaves an unclean file system byte for
byte, and installations made to fail half-way are picked up. `make
keep-data-test` reinstalls machines in QEMU and reads the kept devices
whole before and after ([the installer's
README](../install/README.md#keeping-the-data) lists all three). The Go
tests play the installer with the plans of those machines: the flags
and refusals, both questions and a no at each, `--dry-run`, `--yes`,
the hash handed back, the installer's exit statuses, the ways down,
`multiinstall`, the record and the listings.

| | Installed and booted in QEMU | Planned from fixtures alone |
|---|---|---|
| Storage | LVM with `/var`, `/home`, an xfs data volume and a plain partition; a root on md RAID1; LUKS `/home` with a key file; crypttab's swap over a file system (dropped); a volume named by a kernel name; a btrfs root subvolume that is the default one; a ZFS pool on Ubuntu | thin pools; NFS and CIFS; crypttab's swap and `tmp` that are kept; an xfs or btrfs root made anew; a root at the top of a btrfs; a `/boot` too small |
| Options | `--keep-root`, `--format`, a second attempt after a failure, the refusals | `--root` among several systems |
| Firmware | UEFI (arm64); BIOS with GPT and with MBR (amd64) | md under BIOS; GRUB on several disks |

The machines are QEMU's; no run was on real hardware yet.
