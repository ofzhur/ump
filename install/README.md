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
the specification is not applied. The steps 2, 4 and 5 above do not
run; what runs instead:

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
   containers are not opened, and ZFS pools not imported.
2. **Reads its `/etc/fstab`**, `/etc/crypttab`, `/etc/mdadm/mdadm.conf`
   and `/etc/hostid`, finds the device of every entry (`UUID=`,
   `LABEL=`, `PARTUUID=`, `PARTLABEL=`, `/dev/mapper/...`, `/dev/VG/LV`,
   kernel names, an array by the name the old `mdadm.conf` gave it) and
   decides:

   | Entry of the old fstab | What becomes of it |
   |---|---|
   | `/` | formatted, with the file system the layout of the specification gives `/` (ext4, xfs or btrfs; else ext4). In a btrfs subvolume: the subvolume is deleted, with those inside it, and made anew; the rest of the file system stays. With `--keep-root` nothing is formatted: the old system moves into `/old-root-DATE` on the same file system (a subvolume is renamed `NAME-old-DATE`), and the new one is unpacked beside it |
   | `/boot` | formatted (ext4), the new `/boot`; an EFI system partition mounted there is the ESP |
   | `/boot/efi`, `/efi` | the ESP: not formatted, mounted at `/boot/efi`, the new boot loader added; a boot loader of the old system on it (a directory under `EFI` with a file that names the old root or `/boot`) is renamed `...-old-DATE`, so that the firmware's entry for it leads nowhere. Not in the fstab: the ESP is found by its partition type. Under BIOS it stays an entry like any other |
   | `/usr` (and `/bin`, `/lib`, `/etc/...`) | left as it is, not mounted by the new system |
   | `/var`, `/var/lib`, `/var/cache` | kept, mounted at `/var.old` (`/var/lib.old`, ...): the new system's own is on the root. `--format /var` formats it and makes it the new `/var` |
   | swap | used as it is; a swap file went with the root |
   | anything else: data volumes, `/home`, `/srv/...`, network file systems, bind mounts, tmpfs | kept: the same line in the new fstab, the mountpoint made. `--format MOUNTPOINT` formats a local one (ext4, xfs, btrfs, vfat: the type it had, its label kept) and mounts it there again |
   | an entry whose device is not there, or holds another file system than the fstab says | dropped, with a warning |

   Devices the old fstab does not name (other volume groups, unmounted
   volumes, ZFS and LUKS members, other disks) are not touched and
   listed as such.
3. **Prints the plan** (FORMAT, KEEP, ASIDE, UNUSED, UNTOUCHED, LOST:
   what the old root holds beyond the system itself, with sizes;
   WARNINGS) and its hash. `--dry-run` stops here; `--plan FILE` also
   writes the plan as JSON. A machine it will not do is refused here,
   with status 2 and the reason: no old system or several, an encrypted
   root (LUKS data volumes are kept), a ZFS root, a btrfs root it cannot
   read with certainty (the default subvolume unnamed in the fstab, the
   top of a file system other entries mount subvolumes of, several
   devices), a root or `/boot` too small, no ESP on a UEFI machine, a GPT
   without a BIOS boot partition on a BIOS one, disks in use, an fstab
   field that names several devices, NixOS and Arch Linux as the new
   system. When unsure, it refuses.
4. **Writes only with `--confirm HASH`**, the hash of the plan that was
   shown: the installer finds everything again, and when the plan it
   makes now has another hash (a disk changed, another option), it
   refuses. `format()` makes anew only a device the confirmed plan
   names, and only while it holds the UUID the plan saw; the functions
   that wipe and partition end at once in this mode.
5. **Installs** as above (steps 6 to 12), with these differences: the
   new `/etc/fstab` is the root, `/boot`, the ESP, swap, then the kept
   entries in their old order with their old options (a device named as
   the kernel did, `/dev/sdb1`, by its UUID); `/etc/crypttab` is the
   old one's entries whose devices are there, with the key files that
   were on the old root copied over (path, owner and mode kept; held in
   memory meanwhile), and `nofail` in crypttab and fstab for a volume
   that cannot be opened unattended (a passphrase, a key elsewhere, a
   key file that does not open it); `cryptsetup` is installed for them,
   `thin-provisioning-tools`, `nfs-common` and `cifs-utils` where kept
   volumes need them, on Ubuntu `zfsutils-linux` when ZFS members are
   there (with the old `/etc/hostid` and pool cache, so that the pools
   import at boot; elsewhere they are left for the operator);
   `mdadm.conf` names the arrays that are there and the initramfs is
   rebuilt; a kept `/home` (or `/root`) is mounted for the users'
   section, so that the profile's users and keys are on it: a home
   directory that is there goes to the user of its name, who gets the
   ID it belongs to when that is free (its files change owner
   otherwise), the old `authorized_keys` saved beside the new; a user
   without one gets an ID that no other home directory there belongs
   to; under
   BIOS GRUB goes onto every disk under `/boot` and every other disk
   that carries GRUB's boot code. `/etc/ump/install` gets `keep_data=`,
   `keep_root=`, `keep_plan=` (the hash), `old_system=`, `formatted=`,
   `kept=` and `aside=` (`DEVICE:MOUNTPOINT` each), and the plan is
   `/etc/ump/keep-data.json` (with `--record DIR` also `DIR/keep-data.json`).

Exit status with `--keep-data`: 0 installed; 2 nothing was written (a
refusal, or anything that failed before the first write; the line `==>
keep-data: writing begins` marks it); 1 a failure after that: the old
root is gone or moved, the data volumes are as they were. A second run
then finds what the first saved of the old system's fstab on the new
root (`/.ump-keep-data`, removed when an installation completes) and
plans from it.

In the specification:

```json
"keep_data": {"root": "/dev/vg0/root", "keep_root": false, "format": ["/var"], "confirm": "HASH"}
```

all four optional (`"keep_data": {}` is `--keep-data` alone); the flags
add to it. `disks` must be empty. The plan as JSON (`--plan FILE`,
`/etc/ump/keep-data.json`):

| Key | What |
|---|---|
| `version`, `os`, `firmware`, `arch`, `options` | 1; the system to install; `uefi` or `bios`; the options as given (`root`, `keep_root`, `format`) |
| `old` | the old system: `system` (its `PRETTY_NAME`), `hostname`, `fstab_sha256` |
| `root` | `device`, `subvolume`, `filesystem` (the new one), `was`, `size_mb`, `by` (`detection` or `option`), `free_mb` where the file system stays |
| `candidates` | every old system found: `device`, `subvolume`, `system`, `hostname`, `consistent` (its fstab mounts it at `/`), `resume` |
| `boot` | `esp`, `boot` (a separate `/boot`), `bios_disks`, `disk`, `disks` |
| `format` | what is made anew: `device`, `filesystem`, `mountpoint`, `how` (`mkfs`, or `subvolume` with `subvolume`), `was`, `uuid`, `size_mb` |
| `keep` | `device` (null for an entry without one), `filesystem`, `mountpoint`, `note` |
| `aside` | `device`, `filesystem`, `from`, `mountpoint` (with `DATE` for the time of the installation), `how` (`mounted`, `moved`, `renamed`) |
| `unused`, `untouched` | `device`, `filesystem`, `mountpoint`; `device`, `filesystem`, `label`, `size_mb`, `note` |
| `dropped`, `lost` | `spec`, `mountpoint`, `reason`; `path`, `mb` |
| `packages`, `warnings` | package names; sentences |
| `entries` | the old fstab, entry by entry, with `device` and `action` (`root`, `boot`, `esp`, `swap`, `keep`, `text`, `aside`, `format`, `unused`, `drop`); passwords in options masked |
| `hash` | sha256 of the plan without `hash`, as `jq -cS` prints it |

A refusal with `--plan FILE` writes `{"version": 1, "refused": "why"}`,
with `candidates` when `--root` would decide.

[`images/test-keep-plan.sh`](../images/test-keep-plan.sh) (`make
keep-plan-test`, part of `make check`) makes the plan, the fstab and
the crypttab for some fifty machines without a machine
([`images/testdata/keep-data`](../images/testdata/keep-data): `lsblk`'s
output, the old root's files) and compares, and checks the guards:
`format()` with devices in and out of the plan, the whole-disk functions
in keep mode, and that no command that erases stands outside them.
[`images/test-keep-data.sh`](../images/test-keep-data.sh) (`make
keep-data-test`) reinstalls machines in QEMU: an old system with LVM, a
separate `/var` and a plain partition, switched off without a shutdown,
then `--keep-root` and
`--format`, a ZFS pool on Ubuntu, a root on md RAID1 with a LUKS volume
and its key file, btrfs subvolumes, a second attempt after a failed
one, and the refusals; around every run the kept devices are read whole
and must not differ by a byte, around every plan and refusal the whole
disks. `FIRMWARE=bios` (amd64) runs the first of them on a machine that
boots with BIOS.

Not in this version: an encrypted or ZFS root, NixOS, Arch Linux and
CachyOS as the new system, a second ESP (the one found gets the boot
loader; the others stay as they are, and the plan says so), opening
LUKS volumes during the installation (a kept `/home` inside one does
not get the profile's users).

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
