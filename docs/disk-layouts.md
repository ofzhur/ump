# Disk layouts

A disk layout says how the disks of a server are partitioned and what
goes on them: partitions, md RAID arrays, LVM volume groups and volumes,
file systems and mount points, or, for FreeBSD, a ZFS pool. It is
written once, independent of a particular machine, and applied to
whatever disks a server has: `first` and regular expressions on the
device path pick the disks, `rest` takes whatever space there is.

```console
$ ump config disk-layout list
NAME          PARTS                                STATE   DESCRIPTION
lvm-example   9 (slice,slice,slice,slice,slice,lvm,lvm,lvm,lvm)   ok      two volume groups, NVMe for the system (mirrored root, striped home and swap) and SATA for data
lvm-linear    5 (slice,slice,slice,slice,lvm)      ok      every disk in one volume group, root as one linear volume over all of them; the first disk carries the boot partitions
lvm-raid1     6 (slice,slice,slice,slice,lvm,lvm)  ok      the NVMe disks in one volume group, root mirrored (raid1), swap striped; boot partitions on every disk
lvm-striped   6 (slice,slice,slice,slice,lvm,lvm)  ok      the NVMe disks in one volume group, root and swap striped over all of them; boot partitions on every disk
zfs           4 (slice,slice,slice,zfs)            ok      FreeBSD, every disk in one ZFS pool, striped; the first disk carries the boot partitions and swap
zfs-mirror    4 (slice,slice,slice,zfs)            ok      FreeBSD, every disk in one mirrored ZFS pool; boot partitions and swap on every disk
zfs-raidz     4 (slice,slice,slice,zfs)            ok      FreeBSD, every disk in one raidz ZFS pool (one disk may fail); boot partitions and swap on every disk
```

The `lvm-*` layouts are for the Linux systems, the `zfs*` ones for
FreeBSD: [which system takes which](#which-system-takes-which-layout).

## Where layouts live

Named layouts are the entries of `layouts.yaml` in the profiles
directory, `~/.config/ump/profiles/layouts.yaml`; the first use downloads
the published [`layouts.yaml`](../profiles/layouts.yaml) there (see
[profiles](profiles.md#where-profiles-live)). A [profile](profiles.md)
names one in its `disks` field, or carries a layout of its own under
`disks:` with the same shape (`description` and `parts`). `ump server
reinstall --layout NAME` picks a named one for one installation.

```yaml
layouts:
  lvm-linear:
    description: every disk in one volume group, root as one linear volume over all of them; the first disk carries the boot partitions
    parts:
      - {type: slice, target: [first], size: 1M, filesystem: bios}
      - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
      - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
      - {type: slice, target: [".*"], size: rest, filesystem: lvm, vg: vg0}
      - {type: lvm, vg: vg0, name: root, size: rest, filesystem: ext4, mountpoint: /}
```

## The disks

The installer sees every whole block device of the machine that is not
removable media, a RAM or loop device, or attached over USB (an install
medium, virtual media), and orders them NVMe first, then the rest by
name. **Every one of them is erased**, whether a part targets it or not,
unless `ump server reinstall --disks /dev/sda,/dev/sdb` names the disks
to use; the others are then left alone. `first` is the first disk in
that order of at least 4 GiB, so an NVMe disk when there is one, and
never a small helper disk such as a Linode's 512 MB swap disk, whatever
name the kernel gave it on this boot (the first disk when none is that
big); it carries the boot partitions in the published layouts and
`/boot` in all of them.

A layout is applied in the rescue system, where no disk is in use, and
planned in full before anything is touched: `ump server reinstall
--dry-run` prints the plan, with every partition, array and volume per
disk and its size. A ZFS layout is planned there too, with the same
disks under the same names (`nvme.*` and `sd.*` are Linux's device
paths, also in a layout for FreeBSD), and made by the installer for
FreeBSD, which finds each disk of the plan again by its serial number
and size ([ZFS: FreeBSD's layouts](#zfs-freebsds-layouts)).

## Without a layout: keeping the data

`ump server reinstall --keep-data` applies no layout and erases no
disk: the installer finds the old system, formats its root (and a
separate `/boot`) and mounts every other volume of the old `/etc/fstab`
where it was, LVM, md, LUKS, btrfs subvolumes and ZFS pools included.
The profile's layout then says one thing only, the file system of the
new root (the one it gives `/`: ext4, xfs or btrfs), and `--layout` and
`--disks` are refused. Such an installation is recorded and listed with
the layout `keep-data`, a name that is taken: `ump config disk-layout
create keep-data` and a layout of that name in `layouts.yaml` are
refused. [Keeping the data](keep-data.md) has the rules.

## Parts

A layout is a list of **parts**, applied in order. There are four kinds,
three for the Linux systems and the fourth for FreeBSD:

- a **slice** makes one partition on every disk its target matches, with
  a file system and a mount point, or as a physical volume of a volume
  group;
- an **md** part makes a partition of the given size on every matching
  disk and one md RAID array over them, with a file system or as a
  physical volume;
- an **lvm** part makes a logical volume in a volume group that an
  earlier slice or md part created;
- a **zfs** part makes a partition on every matching disk and one ZFS
  pool over them, the root of a FreeBSD system. It has fields of its
  own: [ZFS: FreeBSD's layouts](#zfs-freebsds-layouts).

| Field | slice | md | lvm |
|---|---|---|---|
| `target` | the disks: `first` (the first disk of at least 4 GiB), or a regular expression on the device path (`nvme.*`, `sd.*`, `.*`); a list, each disk once | same | |
| `index` | the partition number, 1–128 (default: the next free one). The published layouts put the boot partitions at 127 and 128 on every disk, so the data partitions start at 1 | | |
| `size` | `1M`, `512M`, `1G`, `8G` (also `K`, `T`), `20%` of the disk, or `rest` | the member partition on each disk, same forms | `8G`, `20%` of the volume group, or `rest` |
| `raid` | | the md level: 0, 1, 4, 5, 6, 10 | 0 (striped over the physical volumes), 1 (mirrored), 5, 6, 10; absent: linear |
| `filesystem` | `ext4`, `xfs`, `btrfs`, `vfat`, `swap`, `efi`, `bios`, `lvm` (a physical volume of `vg`), `none` | same, except `efi` and `bios` | `ext4`, `xfs`, `btrfs`, `swap` |
| `mountpoint` | where it is mounted; `efi` needs `/boot/efi`; none for `swap`, `bios`, `lvm`, `none` | same | same |
| `vg` | the volume group this physical volume joins (`filesystem: lvm`) | same | the volume group of the volume |
| `name` | | | the volume's name (`vg0/root`) |
| `label` | a file system label | same | same |

Sizes are binary units. At most one part takes the `rest` of a disk, and
it is made last so that the fixed sizes come first. md arrays get
metadata 1.2; a striped (`raid: 0`) volume over a single physical volume
becomes linear. Mirrored and parity volumes synchronise in the background
after the first boot.

### Rules

What makes a layout valid, checked by `ump config disk-layout edit` and
before an installation:

- exactly one part is mounted at `/`;
- when root is on an md array or a logical volume, `/boot` is a slice of
  its own: the boot loader reads plain partitions only, never LVM or md;
- an `efi` part has `mountpoint: /boot/efi`; `efi` and `bios` are
  slices, never on md, and never a logical volume;
- a `slice` or `md` part has a target; an `md` part has a raid level;
- a part with `filesystem: lvm` names its `vg`; an `lvm` part names a
  `vg` that an earlier part created, and a `name`;
- no mount point is used twice.

What the installer checks against the machine, before any disk is
touched: a target matches at least one disk (a layout on `nvme.*` stops
on a machine without NVMe); an md level, an lvm raid level or a pool's
`vdev` has enough members; only one part takes the rest of a disk; a
UEFI machine has an `efi` part and a BIOS machine on amd64 a `bios`
part. GRUB goes on the
EFI removable path (so the firmware's boot order, with the provider's
network boot first, stays as it is) and, on amd64, into the BIOS boot
partition of every disk that has one, so every such disk boots either
way.

## ZFS: FreeBSD's layouts

FreeBSD is installed with its root on ZFS, and a layout for it has a
**zfs** part: one partition (`freebsd-zfs`) on every disk its target
matches, and one pool over them.

| Field | Meaning |
|---|---|
| `type: zfs` | the part |
| `target` | the disks, as for a slice |
| `size` | the pool's partition on each disk, in a slice's forms; `rest` in the published layouts |
| `pool` | the pool's name; `zroot` when absent |
| `vdev` | how the disks are joined: `stripe` (the default: every disk a device of its own, no redundancy), `mirror`, `raidz`, `raidz2` or `raidz3` (one device over all the disks, which survives the loss of one, two or three of them) |
| `mountpoint` | `/`: the pool is the root |
| `filesystem` | absent, or `zfs` |

What makes such a layout valid, beside the [rules](#rules) above:

- it has one zfs part, mounted at `/`: the root pool is the only pool
  made;
- beside it there are only slices with `filesystem` `efi` (at
  `/boot/efi`), `bios` and `swap`: FreeBSD takes no md part, no LVM and
  none of ext4, xfs, btrfs and vfat, and its loader reads the pool, so
  there is no `/boot`;
- an `efi` part of a fixed size has 33 MiB at least, for its FAT32;
- a zfs part has no `index`, `label`, `vg`, `name` or `raid`, and `pool`
  and `vdev` belong to a zfs part alone;
- a pool's name starts with a small letter and goes on with small
  letters, digits, `-` and `_`; it does not begin with one of ZFS's own
  words (`mirror`, `raidz`, `draid`, `spare`, `log`, `cache`,
  `special`, `dedup`).

Against the machine, before any disk is touched: `mirror` needs 2
disks, `raidz` 3, `raidz2` 4 and `raidz3` 5; a UEFI machine an `efi`
part, a BIOS machine a `bios` part; a disk gets one part of a kind (one
`efi`, one `swap`), and its parts fit on it; the pool must hold twice
the installed system; and the first disk (`first`) has 512-byte logical
sectors, since the FreeBSD rescue system the installation goes through
starts from it (a disk with 4096-byte logical sectors there is refused
in this version). A disk that `--disks` leaves alone must not hold a
member of a pool with the new pool's name (FreeBSD takes its root pool
by name, and would start the old one), nor a partition with a GPT
label the installation makes (`boot0`, `efi0`, `swap0`, `zfs0`, and so
on for the second disk): put such a disk into the installation, or
give the pool another name.

A `bios` slice is FreeBSD's `freebsd-boot` partition here and 512 KiB
whatever the layout says: its boot code is loaded whole, and FreeBSD
wants the partition under 545 KiB. The published layouts say `512K`.
The pool is made with 4 KiB blocks (`ashift=12`) and lz4 compression,
its members named by GPT labels (`zfs0`, `zfs1`, ...); the root is the
boot environment `POOL/ROOT/default`, with FreeBSD's usual datasets
beside it (`home`, `tmp`, `var/log`, ...). Swap is a plain partition on
every disk that has the slice, not mirrored; every `efi` partition gets
FreeBSD's loader, so a mirror starts from whichever disk is left.

## Which system takes which layout

| System | Layouts |
|---|---|
| Ubuntu, Debian, NixOS, Arch Linux, CachyOS | those of slices, md and lvm parts: `lvm-linear`, `lvm-striped`, `lvm-raid1`, `lvm-example`, and yours |
| FreeBSD | those with a zfs part: `zfs`, `zfs-mirror`, `zfs-raidz`, and yours |

The other way round is refused before the server is touched, by name:
`--layout zfs-mirror` for Ubuntu ("layout zfs-mirror is a ZFS layout,
which only FreeBSD takes"), `--layout lvm-raid1` for FreeBSD ("layout
lvm-raid1 has md, LVM or Linux file systems, which freebsd-15.1 does
not take"), and `--os freebsd-15.1` over a profile whose layout is one
of the others: take the FreeBSD profile instead.

## The published layouts

The four `lvm-*` layouts put a 1 MiB BIOS boot partition, a 512 MiB EFI
system partition and a 1 GiB ext4 `/boot` on plain partitions; they
differ in where the boot partitions go and how the rest is used. The
three `zfs*` layouts, FreeBSD's, follow them.

### `lvm-linear`

The default. Every disk becomes a physical volume of `vg0`, and root is
one linear volume over all of it; the boot partitions sit on the first
disk only. Fits any machine with one or more disks of any kind; the
simplest way to use all the space, with no redundancy and no swap.

```yaml
parts:
  - {type: slice, target: [first], size: 1M, filesystem: bios}
  - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
  - {type: slice, target: [".*"], size: rest, filesystem: lvm, vg: vg0}
  - {type: lvm, vg: vg0, name: root, size: rest, filesystem: ext4, mountpoint: /}
```

### `lvm-striped`

The NVMe disks in one volume group `data`; 8 GiB of swap and root striped
over all of them. Boot partitions (127 and 128) on every disk, `/boot` on
the first. For machines with several NVMe disks where speed matters more
than redundancy; needs at least one NVMe disk.

```yaml
parts:
  - {type: slice, target: [".*"], index: 127, size: 1M, filesystem: bios}
  - {type: slice, target: [".*"], index: 128, size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
  - {type: slice, target: ["nvme.*"], size: rest, filesystem: lvm, vg: data}
  - {type: lvm, vg: data, name: swap, size: 8G, raid: 0, filesystem: swap}
  - {type: lvm, vg: data, name: root, size: rest, raid: 0, filesystem: ext4, mountpoint: /}
```

### `lvm-raid1`

As `lvm-striped`, but root is mirrored (LVM raid1) while swap stays
striped. The usual choice for a dedicated server with two NVMe disks:
root survives the loss of one disk, and the boot loader is on both.

```yaml
parts:
  - {type: slice, target: [".*"], index: 127, size: 1M, filesystem: bios}
  - {type: slice, target: [".*"], index: 128, size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
  - {type: slice, target: ["nvme.*"], size: rest, filesystem: lvm, vg: data}
  - {type: lvm, vg: data, name: swap, size: 8G, raid: 0, filesystem: swap}
  - {type: lvm, vg: data, name: root, size: rest, raid: 1, filesystem: ext4, mountpoint: /}
```

### `lvm-example`

Two volume groups: `vg1` on the NVMe disks for the system (16 GiB
striped swap, 100 GiB mirrored root, the rest striped as `/home`) and
`vg2` on the SATA disks for `/data`. A template for machines that mix
fast and large disks; needs both kinds.

```yaml
parts:
  - {type: slice, target: [".*"], index: 127, size: 1M, filesystem: bios}
  - {type: slice, target: [".*"], index: 128, size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
  - {type: slice, target: ["nvme.*"], size: rest, filesystem: lvm, vg: vg1}
  - {type: slice, target: ["sd.*"], size: rest, filesystem: lvm, vg: vg2}
  - {type: lvm, vg: vg1, name: swap, size: 16G, raid: 0, filesystem: swap}
  - {type: lvm, vg: vg1, name: root, size: 100G, raid: 1, filesystem: ext4, mountpoint: /}
  - {type: lvm, vg: vg1, name: home, size: rest, raid: 0, filesystem: ext4, mountpoint: /home}
  - {type: lvm, vg: vg2, name: data, size: rest, filesystem: ext4, mountpoint: /data}
```

### `zfs`

FreeBSD's default, and to it what `lvm-linear` is to the Linux systems:
every disk in one pool, striped, no redundancy; a 512 KiB boot
partition, a 512 MiB EFI system partition and 2 GiB of swap on the
first disk. Fits any machine with one or more disks; a small second
disk (a Linode's swap disk) becomes a small device of the pool.

```yaml
parts:
  - {type: slice, target: [first], size: 512K, filesystem: bios}
  - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 2G, filesystem: swap}
  - {type: zfs, target: [".*"], size: rest, pool: zroot, mountpoint: /}
```

### `zfs-mirror`

Every disk in one mirrored pool; boot partitions and 2 GiB of swap on
every disk. The usual choice for a dedicated server with two NVMe
disks, as `lvm-raid1` is for the Linux systems: the pool survives the
loss of one disk, and the machine starts from the other. Needs two
disks; on a machine with other disks beside the pair, name the pair
(`--disks /dev/nvme0n1,/dev/nvme1n1`) or make a layout on `nvme.*`.

```yaml
parts:
  - {type: slice, target: [".*"], size: 512K, filesystem: bios}
  - {type: slice, target: [".*"], size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [".*"], size: 2G, filesystem: swap}
  - {type: zfs, target: [".*"], size: rest, pool: zroot, vdev: mirror, mountpoint: /}
```

### `zfs-raidz`

As `zfs-mirror`, with the disks in one raidz device: the space of all
disks but one, and one disk may fail. Needs three disks.

```yaml
parts:
  - {type: slice, target: [".*"], size: 512K, filesystem: bios}
  - {type: slice, target: [".*"], size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [".*"], size: 2G, filesystem: swap}
  - {type: zfs, target: [".*"], size: rest, pool: zroot, vdev: raidz, mountpoint: /}
```

## Managing layouts

```
ump config disk-layout list                          # the layouts, their parts and whether they are valid
ump config disk-layout get NAME                      # print one
ump config disk-layout create [NAME] [--from NAME]   # copy an entry (default: lvm-linear), then your editor on it; asks on a terminal
ump config disk-layout edit [NAME]                   # your editor on that layout alone, or on all of layouts.yaml; checked afterwards
ump config disk-layout delete NAME [--yes]
```

`create` and `delete` edit `layouts.yaml` in place; its comments survive.
`edit NAME` opens that layout alone, in a file of its own: when the
editor returns it is checked and put back into `layouts.yaml`, the other
layouts and the comments untouched; an invalid edit saves nothing, is
offered again on a terminal, and is kept where ump says otherwise.
`edit` without a name opens the whole file, checks every layout when
the editor returns and keeps the file either way, naming the problem. The command is also `dl` or `layout`,
and the verbs have the short forms of every `ump config` entity (`ump
c dl ls`); see the [CLI reference](cli.md#ump-config).

## Using a layout

```yaml
# in a profile
disks: lvm-raid1
```

```console
$ ump server reinstall web-01 --layout lvm-raid1             # for one installation
$ ump server reinstall web-01 --layout lvm-raid1 --dry-run   # the plan on that machine, nothing erased
$ ump server reinstall web-01 --disks /dev/nvme0n1,/dev/nvme1n1   # leave the other disks alone
```

An inline layout in a profile has the same fields:

```yaml
disks:
  description: one disk, no LVM
  parts:
    - {type: slice, target: [first], size: 1M, filesystem: bios}
    - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: rest, filesystem: ext4, mountpoint: /}
```

Root on a plain partition needs no separate `/boot`.

## Examples

**Root on an md RAID1 over the NVMe disks, no LVM.** The md part makes a
partition on every NVMe disk and one array over them:

```yaml
md-raid1:
  description: root on an md raid1 over the NVMe disks
  parts:
    - {type: slice, target: [".*"], index: 127, size: 1M, filesystem: bios}
    - {type: slice, target: [".*"], index: 128, size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
    - {type: md, target: ["nvme.*"], size: rest, raid: 1, filesystem: ext4, mountpoint: /}
```

**md RAID1 underneath LVM.** The array becomes the physical volume, and
the volumes are plain (linear) on top of it:

```yaml
md-lvm:
  description: an md raid1 over two disks as the one physical volume of vg0
  parts:
    - {type: slice, target: [".*"], index: 127, size: 1M, filesystem: bios}
    - {type: slice, target: [".*"], index: 128, size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
    - {type: md, target: [".*"], size: rest, raid: 1, filesystem: lvm, vg: vg0}
    - {type: lvm, vg: vg0, name: swap, size: 8G, filesystem: swap}
    - {type: lvm, vg: vg0, name: root, size: 50G, filesystem: ext4, mountpoint: /}
    - {type: lvm, vg: vg0, name: var, size: rest, filesystem: xfs, mountpoint: /var}
```

**A separate data disk.** Percentages are of the containing disk or
volume group:

```yaml
system-and-data:
  description: the system on the first disk, every other disk as one xfs volume at /data
  parts:
    - {type: slice, target: [first], size: 1M, filesystem: bios}
    - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
    - {type: slice, target: [first], size: rest, filesystem: lvm, vg: vg0}
    - {type: slice, target: ["sd[b-z]"], size: rest, filesystem: lvm, vg: data}
    - {type: lvm, vg: vg0, name: root, size: 40%, filesystem: ext4, mountpoint: /}
    - {type: lvm, vg: vg0, name: home, size: rest, filesystem: ext4, mountpoint: /home}
    - {type: lvm, vg: data, name: data, size: rest, raid: 0, filesystem: xfs, mountpoint: /data}
```

**FreeBSD on a mirror of the NVMe disks.** A zfs part with a `vdev`,
and the boot partitions and swap on the pool's disks; the other disks
are erased and stay empty, unless `--disks` names the pair:

```yaml
zfs-nvme-mirror:
  description: FreeBSD, the NVMe disks in one mirrored pool with 8 GiB of swap on each
  parts:
    - {type: slice, target: ["nvme.*"], size: 512K, filesystem: bios}
    - {type: slice, target: ["nvme.*"], size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: ["nvme.*"], size: 8G, filesystem: swap}
    - {type: zfs, target: ["nvme.*"], size: rest, pool: zroot, vdev: mirror, mountpoint: /}
```

Targets are regular expressions on the whole device path, so `sd[b-z]`
matches `/dev/sdb` to `/dev/sdz` and `first` is the first disk however
it is named. Mind that a target can match a disk twice over two parts
that both take `rest`; the installer stops there.
