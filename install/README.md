# The UMP installer

[`install.sh`](install.sh) puts an operating system on a machine's disks
from a [UMP base image](../images/base/README.md), or NixOS with
`nixos-install` (see [NixOS](../docs/oses.md#nixos)), the way a specification
says. It runs as root in the [UMP rescue
system](../images/rescue/README.md), where no disk is in use, and
refuses to run anywhere else unless told to with `--force`, because it
erases every disk of the machine it runs on. `ump server reinstall` renders the
specification from a [profile](../docs/profiles.md), fetches the
installer from the storage and runs it there with the specification on
its stdin; it can be run by hand just the same, with a specification
file or with the flags alone (which give the `lvm-linear` layout with
root and the keys of a file). With `--keep-data` it erases no disk: it
replaces the old system and leaves its data, see [Keeping the
data](#keeping-the-data).

## What it does

1. **Looks at the machine:** architecture, firmware (UEFI or BIOS), the
   disks (whole devices that are not removable media, RAM or loop
   devices, and not on USB; NVMe first, so `first` in a layout is an
   NVMe disk when there is one), the network of the rescue system (the
   address, gateway and MAC that `ump server rescue` carried over on the kernel
   command line, or what is configured), unless the specification gives
   the uplink (`network.uplink`, below).
2. **Plans the layout** against the disks: every partition with its
   number and size per disk, every md array, every volume group and
   volume. A target that matches no disk, two parts taking the rest of
   one disk, an md or raid level with too few members, a UEFI machine
   without an `efi` part or a BIOS machine without a `bios` part stop it
   here. Prints the plan, with the users, userdata and firewall;
   `--dry-run` stops here.
3. **Fetches the manifest** of the image, so that a missing image costs
   no disk.
4. **Releases and wipes the disks:** swap off, everything mounted from
   them unmounted, volume groups deactivated, md arrays stopped, then the
   signatures inside the old partitions, the partition tables and the
   signatures of the disks themselves.
5. **Builds the layout:** GPT partitions (the one that takes the rest of
   a disk last), md arrays (`mdadm --create`, metadata 1.2), file systems
   (ext4 without the `orphan_file` and `metadata_csum_seed` features,
   which the GRUB of Ubuntu 22.04 cannot read; xfs, btrfs, vfat, swap),
   physical volumes, volume groups and
   logical volumes (striped, mirrored, raid5, raid6, raid10 or linear;
   striping over a single physical volume becomes linear). Mirrored and
   parity volumes synchronise in the background after the first boot.
   `/boot` stays a plain partition on purpose: the boot loader never has
   to read LVM or md.
6. **Mounts** root, then the other mountpoints by depth, and **unpacks
   the image** straight from the download into them, checking the
   archive's SHA-256 against the manifest. The manifest's `family` decides
   what follows: steps 7 to 11 are for an image (Ubuntu); for `nixos` the
   installer writes the profile, what it found on the machine and the
   firewall ruleset next to UMP's NixOS module ([`nixos/ump.nix`](nixos/ump.nix)),
   with the userdata as NixOS modules, and runs `nixos-install` from the
   manifest's channel instead (see [NixOS](../docs/oses.md#how-the-installation-runs)).
7. **Configures the machine:** hostname, `/etc/fstab` by UUID (swap
   included), a netplan file with every wired interface matched by MAC
   and, unless the profile says `network: {rename: false}`, renamed
   uniformly (`ext01`, ... for the uplink and the interfaces with a
   public address, `int01`, ... for the rest), the uplink with the
   static address and on-link gateway, or DHCP when nothing is known,
   or as `network.uplink` says, the others up without an address,
   IPv6 only when the profile keeps it, resolvers 1.1.1.1 and 8.8.8.8
   unless `network.uplink` names others or those of the lease,
   the kernel command line (`mitigations=off ipv6.disable=1` unless the
   profile keeps them, the profile's extra, and `kho=off`: the kernel's
   kexec handover would reserve up to half the memory of a small machine
   for live updates, and stop ump from re-entering its rescue system),
   time zone, a fresh machine id and SSH host keys.
8. **Users:** every user is created with its shell, groups (created
   when missing) and keys, and `sudo: true` makes it a member of `sudo`
   with a `NOPASSWD` drop-in, since no passwords are set. Root logs in
   over SSH only when the profile lists it (`PermitRootLogin
   prohibit-password` in `/etc/ssh/sshd_config.d/10-ump-root.conf`);
   otherwise the image's `PermitRootLogin no` stands, and the
   administrator (the first sudo user, `ump` in the default profile) is
   the way in, for ump as well.
9. **Userdata:** `nftables` is installed from the Ubuntu archive when
   the firewall needs it and the image lacks it; then every entry of
   the profile's `userdata` is written to `/etc/ump/userdata/NN-NAME.sh`
   in the new system and run there in order, as root in a chroot, with
   the rescue system's resolver and no service started (`policy-rc.d`).
   The output goes to `/var/log/ump-userdata.log` of the new system; a
   failing script stops the installation with its output shown.
10. **Firewall:** with `policy: drop`, `/etc/nftables.conf` is written
    and `nftables.service` enabled: input drop, with the loopback
    interface, established and related connections and the allowed
    entries accepted; output accept; forward accept, except that a new
    connection coming in on an external interface (`ext*`) is held to
    the same allowed entries, matched on the port it was addressed to
    (`ct original proto-dst`, since the port a container publishes is
    rewritten by DNAT before the forward hook). So containers and
    virtual machines reach out freely, and a port Docker publishes with
    `-p` is reachable from outside only when the profile allows it,
    whatever rules the container engine adds (their chains and UMP's
    are evaluated both; a drop in either is final).
11. **Installs the boot loader:** GRUB on the EFI removable path
    (`EFI/BOOT/BOOT*.EFI`), which every firmware tries without a boot
    entry, and without writing one, so the firmware's boot order stays as
    the provider set it, with its network boot (which is how rescue
    systems get in) ahead of the disk; on amd64 also the BIOS loader into
    the BIOS boot partition of every disk that has one, so the disk boots
    either way. With md arrays or raid volumes, `mdadm.conf` is written
    and the initramfs rebuilt first. Then `update-grub`.
12. Writes `/etc/ump/install` (what was installed, when, on which disks,
    which profile and layout, the administrator, the interfaces with
    their MAC addresses, the private networks and addresses the
    specification gives, `private=` (`default:10.0.0.1,db:10.0.1.1`)
    and, of the first network, `private_network=` and
    `private_address=`, which no interface carries yet; with
    `network.uplink` also `network=` and `network6=`, how the uplink
    gets its addresses, and `dns=`, its resolvers or `dhcp`) and
    `/etc/ump/spec.json` (the specification); with `--record DIR` it
    copies `/etc/ump/install` and the new system's SSH host keys (the
    public ones; on NixOS made now rather than at the first boot) into
    DIR, for whoever runs it unattended (the [rescue system's
    installation runner](../images/rescue/README.md#booting-over-the-network))
    to report. Then it unmounts everything and
    reports; `ump server reinstall` then reboots the machine and waits for the
    new system.

## Usage

```
install.sh --spec FILE [--record DIR] [--dry-run] [--force]
install.sh --spec FILE --keep-data [--root DEVICE] [--keep-root] [--format MOUNTPOINT]...
           (--dry-run | --plan FILE | --confirm HASH) [--record DIR]
install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--disks /dev/sda,/dev/sdb]
           [--keys FILE] [--ipv6] [--mitigations] [--timezone ZONE] [--record DIR] [--dry-run] [--force]
```

The specification is JSON with the fields of a resolved profile (`os`,
`hostname`, `timezone`, `storage`, `kernel`, `layout` with its parts,
`disks`, `users` with key lines, `userdata` with scripts, `firewall`,
`network` with `rename` and, where the server has them,
`private_networks`: its addresses in its [private
networks](../docs/networks.md), in order, each as `network`, `cidr` and
`address`, and `private`, the first, which a specification of ump
v0.5.0 has alone, and `uplink` where the provider knows the machine's
network, below);
`ump server reinstall` writes it, and `/etc/ump/spec.json` of an installed system
is one to start from. Without `--spec`, the flags give the `lvm-linear`
layout with root alone (which then logs in over SSH, since it is the
only user): `--storage` is the base URL that serves
`os/<os>/<arch>/manifest.json` and `rootfs.tar.zst` (see [the storage
layout](../images/rescue/README.md#your-own-storage)), `--keys` defaults
to `/root/.ssh/authorized_keys`, which is where `ump server rescue` put UMP's
key and yours. By hand, from the rescue system:

```console
# curl -fsSL https://ump.zhurko.de/dl/install/install.sh -o /tmp/install.sh
# bash /tmp/install.sh --storage https://ump.zhurko.de/dl --hostname web-01 --dry-run
# bash /tmp/install.sh --storage https://ump.zhurko.de/dl --hostname web-01 && systemctl reboot
```

Exit status 0 means the machine is ready to reboot; 2 is a usage or
environment problem (not the rescue system, a missing tool), 1 a failed
step, with the reason on stderr. A failed installation leaves the
machine in the rescue system, with whatever was mounted unmounted again.

## Keeping the data

`--keep-data` (or `keep_data` in the specification) is a second way to
install: the old system is replaced, its data stays. No disk is wiped,
no partition table, array or volume group is touched, and the layout of
the specification is not applied. `ump server reinstall --keep-data`
runs it twice, for the plan and for the installation, and asks in
between ([keeping the data](../docs/keep-data.md)). The steps 2, 4 and
5 above do not run; what runs instead:

1. **Finds the old system**, reading only: arrays are assembled
   read-only and volume groups activated, every ext4, xfs and btrfs file
   system (on btrfs every subvolume) is mounted read-only without its
   journal and looked at, its device read-only meanwhile, so that a file
   system a reset left unclean is read as the disk has it and nothing
   its driver would note reaches the disk (the plan says when the old
   root is such). An old system is one that holds `/etc/fstab`
   and `/etc/os-release` and whose fstab mounts that very file system
   (and subvolume) at `/`: a copy of a root on a data volume, or a
   snapshot, names another root and is none. Exactly one is the root;
   with several, or none, `--root DEVICE` says which (a device, a
   volume's path, `UUID=...`, `DEVICE:SUBVOLUME` on btrfs). LUKS
   containers are not opened, ZFS pools not imported, and USB and
   removable disks not searched. What a journal alone holds is not
   seen: an xfs root that was not unmounted cleanly shows no `/etc`
   without its log, and the refusal then names the file system and says
   how its log is replayed (mounted once in the rescue system: a write
   that is the operator's to make). A file system that cannot be looked
   at is named too.
2. **Reads its `/etc/fstab`**, `/etc/crypttab`, `/etc/mdadm/mdadm.conf`
   and `/etc/hostid`, finds the device of every entry (`UUID=`,
   `LABEL=`, `PARTUUID=`, `PARTLABEL=`, `/dev/disk/by-...`,
   `/dev/mapper/...`, `/dev/VG/LV`, kernel names, an array by the name
   the old `mdadm.conf` gave it) and decides:

   | Entry of the old fstab | What becomes of it |
   |---|---|
   | `/` | formatted, with the file system the layout of the specification gives `/` (ext4, xfs or btrfs; else ext4). In a btrfs subvolume: the subvolume is deleted, with those inside it, and made anew; the rest of the file system stays. With `--keep-root` the root is not formatted: the old system moves into `/old-root-DATE` on the same file system (a subvolume is renamed `NAME-old-DATE`), whence it cannot be started again, and the new one is unpacked beside it |
   | `/boot` | formatted (ext4, its label kept), the new `/boot`; an EFI system partition mounted there is the ESP. One smaller than 300 MB is left as it is, unused, and `/boot` is on the root, where GRUB reads that (a partition, a linear volume or an md array, with an ext4 file system); else refused |
   | `/boot/efi`, `/efi` | the ESP: not formatted, mounted at `/boot/efi`, the new boot loader added; a boot loader of the old system on it (a directory under `EFI` with a file that names the old root or `/boot`) is renamed `...-old-DATE`, so that the firmware's entry for it leads nowhere. Not in the fstab: the ESP is found by its partition type. Under BIOS it stays an entry like any other |
   | `/usr` (and `/bin`, `/sbin`, `/lib*`, `/etc/...`) | left as it is, not mounted by the new system; a bind mount or network file system there likewise |
   | `/var`, `/var/lib`, `/var/cache` | kept, mounted at `/var.old` (`/var/lib.old`, ...): the new system's own is on the root; a bind mount or network file system there likewise. `--format /var` formats it and makes it the new `/var` |
   | swap | used as it is; a swap file went with the root |
   | anything else on a device: data volumes, `/home`, `/srv/...` | kept: the same line in the new fstab, the mountpoint made. `--format MOUNTPOINT` formats a local one (ext4, xfs, btrfs, vfat: the type it had, ext4 for ext2 and ext3, its label kept, no `subvol=` in its new line) and mounts it there again. With `nofail`: a file system the new system may not mount (reiserfs, whatever is not ext2/3/4, xfs, btrfs, vfat, exfat, f2fs, iso9660, udf, squashfs), and one on a USB or removable disk |
   | without a device: network file systems, tmpfs and the kernel's own | kept as they are. A bind mount whose source went with the old root or a formatted volume gets an empty directory there; one whose source is on a volume that is set aside names it under its new place. A loop mount of a file, or an overlay over directories, that went with the root is dropped. A ZFS dataset, and a type the installer does not know, get `nofail` |
   | an entry whose device is not there, or holds another file system than the fstab says | dropped, with a warning |

   **A device named as the old kernel named it** (`/dev/sdb1`,
   `/dev/nvme0n1p3`, `/dev/md0` without an entry in the old
   `mdadm.conf`, `/dev/dm-3`) may be another device here. Such an entry
   is never formatted unproven: `/boot` is the old one when it holds a
   `vmlinuz-VERSION` for a `VERSION` the old root has modules of, else
   the plan is refused; `--format` of such an entry is refused; an ESP
   must be one by its partition type. A kept one is named by its UUID
   in the new fstab and has a warning that says what the name is here
   (file system, label, size). When the root's own kernel name does not
   lead to the root (`--root` named it), the names are known to be
   another machine's: every other entry named so is dropped, a `/boot`
   among them refuses the plan. The remedy the refusals name: `UUID=`
   or `LABEL=` in the old `/etc/fstab`.

   **crypttab**: an entry whose device is there is carried over (one
   named as the kernel named it by its UUID, or its `PARTUUID`). Not
   those that systemd makes anew at every boot (`swap`, `tmp`, a key
   from `/dev/urandom`) when the device holds a file system now, is on
   a USB disk, or can only be named as the kernel names it: they are
   dropped with a warning, and the fstab entries on them too. The
   documented setup with a small file system in front (`offset=`, named
   by its `LABEL=` or `UUID=`) is kept. Those that are kept have a
   section of their own in the plan, MADE ANEW AT EVERY BOOT, with
   their options. An entry that is no LUKS container (and not `plain`)
   is dropped.

   Devices the old fstab does not name (other volume groups, unmounted
   volumes, ZFS and LUKS members, other disks) are not touched and
   listed as such.
3. **Prints the plan** (FORMAT with sizes and labels, KEEP, MADE ANEW
   AT EVERY BOOT, ASIDE, UNUSED, UNTOUCHED, LOST: what the old root
   holds beyond the system itself, with sizes; WARNINGS) and its hash.
   `--dry-run` stops here; `--plan FILE` also writes the plan as JSON
   (its last line then: `==> the plan is made: stopping here, no disk
   was touched; --confirm with its hash installs it`).
   A machine it will not do is refused here, with status 2, the reason
   and what to do about it: no old system or several, an encrypted root
   (LUKS data volumes are kept), a ZFS root, a root on a USB or
   removable disk, a read-only root, `/boot` or ESP, a btrfs root it
   cannot read with certainty (the default subvolume unnamed in the
   fstab, the top of a file system other entries mount subvolumes of,
   `/boot` in a subvolume of it, several devices), a btrfs root at the
   top that holds subvolumes other than the system's own
   (`var/lib/machines`, `var/lib/portables`, the layers of Docker and
   Podman: those go with it and are named in WARNINGS; any other is
   data, and `--keep-root` keeps the file system), a root too small
   (twice the image; `lvextend -r` in the rescue system) or, with
   `--keep-root`, too full or with an immutable entry at its top, a
   `/boot` mounted twice, elsewhere too or encrypted, no ESP on a UEFI
   machine or one that is no plain partition, under BIOS a GPT without
   a BIOS boot partition or an MBR disk whose first partition starts
   before sector 2048 (GRUB has no room; the disks that only carry an
   old GRUB are left alone with a warning instead), `--format` of the
   file system the root, `/boot` or the ESP is on, of a volume mounted
   twice, read-only, on a USB disk or named as the kernel named it,
   disks in use, an fstab or crypttab field that names several devices,
   NixOS and Arch Linux as the new system. When unsure, it refuses.
4. **Writes only with `--confirm HASH`**, the hash of the plan that was
   shown: the installer finds everything again, and when the plan it
   makes now has another hash (a disk changed, another option, another
   image on the storage: its version and checksum are in the plan), it
   refuses. `format()` makes anew only a device the confirmed plan
   names, once, and only while it holds the UUID the plan saw; the
   functions that wipe and partition end at once in this mode; a plan
   that would format what the root or the ESP is on is refused once
   more before the first write. Arrays the installer assembled
   read-only are made writable when it writes to them: those under the
   root, `/boot`, the ESP, a formatted volume and a kept `/home`.

   Before the first step that destroys, what a second attempt needs of
   the old system is saved at the top of the root's file system, in
   `/.ump-keep-data`: its fstab (devices named as the kernel named
   them, and those that are formatted, by their UUIDs), crypttab with
   its key files (mode 700, the root's alone), `mdadm.conf`, the host
   ID, the plan that was confirmed, a state (how far the installation
   came) and checksums; written under another name, synced, then
   renamed. Where the root itself is formatted, these are in the rescue
   system's memory (`/run/ump-keep-data`) until the new file system has
   them, and an installation that fails in between is picked up from
   there by the next run in the same rescue system. Every other mkfs is
   written down first, with the UUID the new file system gets
   (`formats`), so that the saved fstab's entry is found again whether
   the mkfs ran or not.
5. **Installs** as above (steps 6 to 12), with these differences: the
   new `/etc/fstab` is the root, `/boot`, the ESP, swap, then the kept
   entries in their old order with their old options (a device named as
   the kernel did, `/dev/sdb1`, by its UUID); `/etc/crypttab` is the
   old one's entries that are carried over, with the key files that
   were on the old root copied over (path, owner and mode kept), and
   `nofail` in crypttab and fstab for a volume that cannot be opened
   unattended (a passphrase, a key elsewhere, a key file that does not
   open it); `cryptsetup` is installed for them,
   `thin-provisioning-tools`, `nfs-common` and `cifs-utils` where kept
   volumes need them, on Ubuntu `zfsutils-linux` when ZFS members are
   there (with the old `/etc/hostid` and pool cache, so that the pools
   import at boot; elsewhere they are left for the operator);
   `mdadm.conf` names the arrays that are there, each by its UUID and
   under the name the old `mdadm.conf` gave it, and the initramfs is
   rebuilt; a kept `/home` (or `/root`) is mounted for the users'
   section, so that the profile's users and keys are on it: a home
   directory that is there goes to the user of its name, who gets the
   ID it belongs to when that is free (its files change owner
   otherwise), the old `authorized_keys` saved beside the new; a user
   without one gets an ID that no other home directory there belongs
   to. A `/home` or `/root` the installer cannot mount (encrypted, of
   the network, a ZFS dataset or pool, a file system it does not know)
   would cover the home directories it makes on the root: the users'
   keys are then also in `/etc/ssh/authorized_keys/NAME`, which sshd
   reads besides (`sshd_config.d/20-ump-keep-data.conf`), and the plan
   says so. The new system's `findmnt --verify` reads the new fstab,
   and what it finds wrong is a warning. Under BIOS GRUB goes onto
   every disk under `/boot`, and is tried on every other disk that
   carries GRUB's boot code and has room for it (not USB or removable
   ones), where a failure is a warning. `/etc/ump/install` gets
   `keep_data=`, `keep_root=`, `keep_plan=` (the hash), `old_system=`,
   `formatted=`, `kept=` and `aside=` (`DEVICE:MOUNTPOINT` each), and
   the plan is `/etc/ump/keep-data.json` (with `--record DIR` also
   `DIR/keep-data.json`). `/.ump-keep-data` is removed.

Exit status with `--keep-data`: 0 installed; 2 nothing was written (a
refusal, or anything that failed before the first write; the line `==>
keep-data: writing begins` marks it); 1 a failure after that: the old
root is gone or moved, the volumes the plan keeps hold their data. A
second run then plans from what the first saved (`resumed` in the
plan): the old fstab, with `/boot` and the other formatted volumes
found by the UUIDs written down. With `--keep-root` a move that failed
half-way is carried on into the same `/old-root-DATE`; when the old
system was aside already, what the failed attempt unpacked goes into
`/failed-install-DATE` (a subvolume: `NAME-failed-DATE`), which can be
removed. A second plan that keeps less than the first did says so
(`shrunk`, with a warning that names what is missing), and one whose
saved files are damaged is refused: `--root DEVICE` then installs onto
the root without the old system's other volumes (`root.known` is
false).

In the specification:

```json
"keep_data": {"root": "/dev/vg0/root", "keep_root": false, "format": ["/var"], "confirm": "HASH"}
```

all four optional (`"keep_data": {}` is `--keep-data` alone); the flags
add to it. `disks` must be empty. The plan as JSON (`--plan FILE`,
`/etc/ump/keep-data.json`):

| Key | What |
|---|---|
| `version`, `os`, `image`, `firmware`, `arch`, `options` | 1; the system to install; its image as the manifest names it (`version`, `sha256` of the root file system's archive); `uefi` or `bios`; the options as given (`root`, `keep_root`, `format`) |
| `resumed`, `shrunk` | `true` (absent otherwise): a second attempt after an interrupted one, planned from what that saved; and one that keeps less than the plan the first was confirmed with |
| `old` | the old system: `system` (its `PRETTY_NAME`), `hostname`, `fstab_sha256` |
| `root` | `device`, `subvolume`, `filesystem` (the new one), `was`, `uuid`, `size_mb`, `by` (`detection` or `option`), `known` (`false`, absent otherwise: `--root` names a device with no old system on it), `free_mb` where the file system stays |
| `candidates` | every old system found: `device`, `subvolume`, `system`, `hostname`, `consistent` (its fstab mounts it at `/`), `resume`, `ram` (`true`: read from the rescue system's memory) |
| `boot` | `esp`, `boot` (a separate `/boot`), `bios_disks`, `bios_optional` (those of them that only carry GRUB now), `disk`, `disks` |
| `format` | what is made anew: `device`, `filesystem`, `mountpoint`, `how` (`mkfs`, or `subvolume` with `subvolume`), `was`, `uuid`, `label` (what it has now), `size_mb` |
| `keep` | `device` (null for an entry without one, then `spec`), `filesystem`, `mountpoint`, `uuid`, `note` |
| `volatile` | crypttab's volumes that are made anew at every boot: `device`, `filesystem`, `mountpoint`, `name`, `source`, `options`, `how` (`swap`, `tmp`, `a random key`) |
| `aside` | `device`, `filesystem`, `from`, `mountpoint` (with `DATE` for the time of the installation), `uuid`, `how` (`mounted`, `moved`, `renamed`) |
| `unused`, `untouched` | `device` (or `spec`), `filesystem`, `mountpoint`, `uuid`, `note`; `device`, `filesystem`, `label`, `size_mb`, `note` |
| `dropped`, `lost` | `spec`, `mountpoint`, `reason`; `path`, `mb` |
| `crypttab` | the new crypttab: `name`, `device`, `source`, `keyfile`, `options` |
| `packages`, `warnings` | package names; sentences |
| `entries` | the old fstab, entry by entry, with `device` and `action` (`root`, `boot`, `esp`, `swap`, `keep`, `text`, `aside`, `format`, `unused`, `drop`); passwords in options masked |
| `hash` | sha256 of the plan without `hash`, as `jq -cS` prints it |

A refusal with `--plan FILE` writes `{"version": 1, "refused": "why"}`,
with `candidates` when `--root` would decide.

[`images/test-keep-plan.sh`](../images/test-keep-plan.sh) (`make
keep-plan-test`, part of `make check`) makes the plan, the fstab and
the crypttab for the machines of
[`images/testdata/keep-data`](../images/testdata/keep-data) (`lsblk`'s
output, the old root's files) without a machine and compares, and
checks the guards: `format()` with devices in and out of the plan, the
whole-disk functions in keep mode, what `kd_write` runs for a plan,
command by command, in a directory that stands for the root's file
system (fresh, and after an interrupted attempt), and where the script
has its commands that erase, delete, move and overwrite. There every
function that looks at a disk is replaced by a reader of the case;
[`images/test-keep-loop.sh`](../images/test-keep-loop.sh) (`make
keep-loop-test`: as root on Linux, else in a privileged container)
runs them, and `kd_write`, on file systems in loop devices: looks at
ext4, xfs and btrfs that were not unmounted cleanly leave them byte for
byte, and installations made to fail while the root is formatted,
during the move aside of `--keep-root` and between the deletion of a
btrfs root subvolume and the new one are picked up by the next plan.
[`images/test-keep-data.sh`](../images/test-keep-data.sh) (`make
keep-data-test`) runs that test in the rescue system and reinstalls
machines in QEMU: an old system with LVM, a separate `/var`, a `/home`
volume and a plain partition, switched off without a shutdown, then
`--keep-root` and `--format`, a ZFS pool on Ubuntu, a root on md RAID1
with arrays that nothing assembled, `/home` on a LUKS volume and an
encrypted swap over a file system, btrfs subvolumes with the root's the
default one, a second attempt with `--format /var` after a failed one,
and the refusals; around every run the kept devices are read whole and
must not differ by a byte, around every plan and refusal the whole
disks. `FIRMWARE=bios` (amd64) runs the first machine, `--keep-root`
and a machine with an MBR partition table started by BIOS.

Not in this version: an encrypted or ZFS root, NixOS, Arch Linux and
CachyOS as the new system, a second ESP (the one found gets the boot
loader; the others stay as they are, and the plan says so), opening
LUKS volumes during the installation (a kept `/home` inside one does
not get the profile's users: their keys are outside it), replaying the
log of a file system that was not unmounted cleanly.

## The uplink of the specification

A machine whose provider knows its network (a PXE machine, from
`pxe.yaml`) gets `network.uplink` in its specification, and the
installer configures that instead of what the rescue system has:

```json
"uplink": {
  "mac": "0c:c4:7a:aa:bb:11",
  "ipv4": {"mode": "static", "address": "192.0.2.17/24", "gateway": "192.0.2.1"},
  "ipv6": {"mode": "none"},
  "dns": ["192.0.2.53", "192.0.2.54"]
}
```

| Field | Meaning |
|---|---|
| `mac` | the uplink (`ext01`); without it, the interface the rescue system reaches the network through. An interface the machine lacks stops the installation |
| `ipv4` | `static` with `address` (with its prefix length) and `gateway` (on-link; none: no default route), `dhcp` (with the MAC as the client identifier, as iPXE and the rescue system send it), or `none` |
| `ipv6` | `static` likewise, `auto` (router advertisements, an EUI-64 address), or `none`; only when the profile keeps IPv6 |
| `dns` | the resolvers, in netplan, networkd or NixOS and in resolved's configuration; without it 1.1.1.1 and 8.8.8.8. With `ipv4` `dhcp` these alone: the lease's are ignored (netplan `dhcp4-overrides`, networkd `UseDNS=no`) |
| `dns_from_dhcp` | `true`: the resolvers of the lease (or of router advertisements), none written |

Without `network.uplink` the installer writes what it always has,
byte for byte; [`images/test-network.sh`](../images/test-network.sh)
(`make network-test`, part of `make check`) checks that and every
uplink, for every renderer, without a machine, and `UPLINK=dhcp` or
`UPLINK=static` of [`images/test-install.sh`](../images/test-install.sh)
on a machine in QEMU. An installer from before `network.uplink`
ignores it.

## Turning IPv6 or the mitigations back on

The defaults trade the CPU vulnerability mitigations and IPv6 for speed.
On an installed system, edit `/etc/default/grub.d/ump-cmdline.cfg`
(remove `mitigations=off` or `ipv6.disable=1`), run `update-grub`, for
IPv6 add the address and the default route to `/etc/netplan/10-ump.yaml`
(`netplan apply` or `netplan try`), and reboot. `ump server reinstall
--mitigations --ipv6` installs with both on, as does `kernel: {mitigations:
true, ipv6: true}` in the [profile](../docs/profiles.md).
