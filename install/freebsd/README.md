# The UMP installer for FreeBSD

[`install.sh`](install.sh) puts FreeBSD on a machine's disks, with its
root on ZFS, the way a specification says: the same specification
[the installer of the other systems](../README.md) takes, as far as it
applies. It runs as root in the [UMP FreeBSD rescue
system](../../images/freebsd/README.md), which is FreeBSD running from
memory with no disk in use, and refuses to run anywhere else unless
told to with `--force`, because it erases every disk of its plan. It is
POSIX sh with `jq` and FreeBSD's own tools: `gpart`, `zpool`, `zfs`,
`tar`, `pkg`, `pw`, `pf`.

FreeBSD is not unpacked from an image UMP builds: the installer takes
the release's own distribution sets (`base.txz`, `kernel.txz`) and a
pinned set of FreeBSD's packages, all from the storage, checked against
the manifest there. No installation asks `download.freebsd.org` or
`pkg.FreeBSD.org` for anything.

## What it does

1. **Looks at the machine:** architecture, firmware (`machdep.bootmethod`;
   arm64 is UEFI), the disks (`kern.disks` without memory disks, CDs and
   those on USB; NVMe first, the rest by name), the uplink (`ump.mac`,
   `ump.ip` and `ump.ip6` of the [boot config](#the-boot-config), else
   the interface with the default route and what is configured on it,
   unless the specification gives the uplink), every wired interface
   with its MAC.
2. **Finds the plan's disks**, or plans itself ([The plan](#the-plan)),
   and prints the plan as the installer of the other systems prints
   one: the machine, the image, the disks with their partitions, the
   pool, the network, the interfaces, the users, userdata and firewall.
   A plan that cannot work is refused here: a disk that is not there or
   cannot be told from one that is left alone, a vdev with too few
   disks, a UEFI machine without an `efi` part or a BIOS machine without
   a `bios` part, parts that do not fit, a user FreeBSD's base has, an
   unknown time zone. `--dry-run` stops here.
3. **Fetches the manifest**, so that a missing image costs no disk. It
   must be of the family `freebsd` and of the release the rescue system
   is (the pool is made by the release that will import it); the pool
   must hold twice the system; the users' shells and bash for the
   userdata must be in the base system or among the packages.
4. **Clears and partitions the disks:** pools exported, swap off; per
   disk the partition table destroyed, the first and last MiB zeroed, a
   new GPT, the partitions with their labels (`boot0`, `efi0`, `swap0`,
   `zfs0`, ... by the disk's place in the plan), the first and last MiB
   of each zeroed, where a pool or file system from before would be
   found again. The disk the rescue system was started from is treated
   like every other: nothing of it is in use.
5. **Makes the pool and its datasets** ([ZFS](#zfs)).
6. **Unpacks the sets**, `base.txz` and `kernel.txz`, each straight from
   the download onto the pool, its SHA-256 taken on the way and compared
   with the manifest's.
7. **Configures the machine:** `/etc/fstab` (the first EFI system
   partition, swap), `/boot/loader.conf`, `/etc/rc.conf` (hostname, the
   services, the network), `/etc/resolv.conf`, `/etc/hosts`, the time
   zone, sshd (keys alone, no root unless the profile lists it, and
   `UseDNS no`: FreeBSD's sshd looks every client up in the DNS by
   default, and a resolver that does not answer would hold every login
   for half a minute), the SSH host keys, the files the first boot
   seeds its random numbers from.
8. **Installs the boot loader:** every `efi` partition gets a FAT32
   file system and FreeBSD's `loader.efi` on the removable path
   (`EFI/BOOT/BOOTX64.EFI`, `BOOTAA64.EFI`), which every firmware tries
   without a boot entry, and none is written, so the firmware's boot
   order stays as the provider set it; on amd64 every `bios` partition
   gets `gptzfsboot` and its disk `pmbr`. Both on a machine of either
   firmware, so the disks start either way.
9. **Adds the packages** of the manifest from `packages.tar`, unpacked
   onto the pool (not into memory): `pkg` itself with the rescue
   system's `pkg-static`, then the wanted ones (`sudo`, `bash`,
   `python3`) with the new system's `pkg add`, which takes what they
   depend on from the files beside them. The log is
   `/var/log/ump-pkg.log`.
10. **Users:** `pw useradd -m` without a password, the groups (made when
    missing), the keys; `sudo: true` makes a member of `wheel` with a
    `NOPASSWD` file in `/usr/local/etc/sudoers.d`. A shell the new
    system lacks is taken from `/usr/local/bin` under the same name:
    `/bin/bash` of a profile is the package's `/usr/local/bin/bash`.
    Root's password field becomes `*` (the base system's root has an
    empty one), and root logs in over SSH only when the profile lists
    it.
11. **Userdata:** every entry (type `bash`) is written to
    `/etc/ump/userdata/NN-NAME.sh` and run in order by
    `/usr/local/bin/bash` in a chroot of the new system, with devfs, the
    rescue system's resolver and `UMP_USERDATA`, `UMP_HOSTNAME`,
    `UMP_OS`, `UMP_PROFILE` and `UMP_ADMIN`; the output goes to
    `/var/log/ump-userdata.log`, and a failing entry fails the
    installation. `pkg install` starts nothing; `sysrc` and `service
    NAME enable` work.
12. **Firewall:** with `policy: drop`, `/etc/pf.conf` is written,
    checked with `pfctl -nf` and enabled ([The firewall](#the-firewall)).
13. Writes `/etc/ump/install` and `/etc/ump/spec.json` (0600); with
    `--record DIR` it copies the record and the new system's public SSH
    host keys into DIR.
14. Exports the pool and reports; a reboot starts the new system.

## Usage

```
install.sh --spec FILE [--record DIR] [--dry-run] [--force]
```

`ump server reinstall` will fetch it from the storage
(`install/freebsd/install.sh`) and run it with the specification, as it
runs the other installer in the Linux rescue system. By hand, in the
FreeBSD rescue system, with a specification such as
`/etc/ump/spec.json` of an installed system:

```console
# fetch -o /tmp/install.sh https://ump.zhurko.de/dl/install/freebsd/install.sh
# sh /tmp/install.sh --spec spec.json --dry-run
# sh /tmp/install.sh --spec spec.json && shutdown -r now
```

Exit status 0 means the machine is ready to reboot. 2 is a refusal:
usage, not the rescue system, a missing tool, the specification, a
layout that is not ZFS, the disks not those of the plan, the image; it
comes with the line `install.sh: nothing was written to the disks`. 1
is a failed step after the first disk was cleared, with the reason on
stderr: the machine stays in the rescue system that runs, and a reboot
then finds no system on its disks.

## The boot config

The FreeBSD rescue system is started from a small image that the Linux
rescue system wrote to the start of a disk, with a boot config beside
it; the rescue system copies it to `/etc/ump/boot`. The installer reads
two of its files (`UMP_BOOT_CONFIG` names another directory):

| File | What the installer takes |
|---|---|
| `cmdline` | `ump.mac`, `ump.ip=ADDRESS/PREFIX,GATEWAY` and `ump.ip6`: the uplink, as the Linux rescue system had it. A word missing there is looked up in the kernel environment (`kenv`) |
| `plan.json` | the plan |

## The plan

The installer of the Linux rescue system plans the disks, as for every
system: there `--dry-run` ends, and there the targets of a
[layout](../../docs/disk-layouts.md) mean what they mean everywhere
(`nvme.*` is a Linux path). FreeBSD names the same disks `nda0`, `ada0`,
`da0`, `vtbd0`, so the plan travels as `plan.json`:

```json
{
  "version": 1,
  "firmware": "uefi",
  "boot_disk": "/dev/nvme0n1",
  "disks": [
    {"name": "/dev/nvme0n1", "size": 512110190592, "serial": "S4EWNX0N123456", "wwn": "eui.0025388b01234567",
     "parts": [{"index": 1, "kind": "bios", "size": "512K"}, {"index": 2, "kind": "efi", "size": "512M"},
               {"index": 3, "kind": "swap", "size": "2G"}, {"index": 4, "kind": "zfs", "size": "rest"}]},
    {"name": "/dev/nvme1n1", "size": 512110190592, "serial": "S4EWNX0N123457", "wwn": "eui.0025388b01234568",
     "parts": [{"index": 1, "kind": "bios", "size": "512K"}, {"index": 2, "kind": "efi", "size": "512M"},
               {"index": 3, "kind": "swap", "size": "2G"}, {"index": 4, "kind": "zfs", "size": "rest"}]}
  ],
  "left": [{"name": "/dev/sda", "size": 4000787030016, "serial": "ZC1A2B3C"}],
  "pool": {"name": "zroot", "vdev": "mirror", "mountpoint": "/"}
}
```

| Key | Meaning |
|---|---|
| `version` | 1; any other is refused |
| `firmware` | `uefi` or `bios`, as the Linux rescue system saw it; the installer checks the parts against the firmware it was started by itself, and warns when the two differ |
| `boot_disk` | the name of a disk of `disks`: `boot_disk=` of the record; without it the first disk with an `efi` or `bios` part |
| `disks` | the disks to erase, each with its `name` there, its `size` in bytes, its `serial` number (empty when the system reads none) and its `parts`; `wwn` is not read |
| `parts` | the partitions: `index` (1 to 128), `kind` (`bios`, `efi`, `swap` or `zfs`) and `size` as a layout has it (`512K`, `512M`, `2G`, `20%`, `rest`). One of a kind on a disk; a `bios` part is FreeBSD's `freebsd-boot` partition and 512 KiB whatever the size says (`pmbr` loads the whole partition) |
| `left` | the disks the installation leaves alone (`--disks` named others), with `name`, `size` and `serial`: so that they are known here too |
| `pool` | `name` (`zroot` when absent), `vdev` (`stripe`, the default: every disk a device of its own; `mirror`, `raidz`, `raidz2`, `raidz3`: one device over all the disks with a `zfs` part) and `mountpoint`, which must be `/` |

The installer applies it and plans nothing from targets. It finds each
disk of the plan among its own:

1. by serial number, where both systems read one (`lsblk -o SERIAL`,
   `diskinfo -s`) and the size agrees; the disks of `left` likewise;
2. else by size, when one disk of that size is left;
3. else in order among the disks of that size.

It refuses, before it writes:

- a disk of the plan that is not here (FreeBSD has no driver for its
  controller, or it is another machine);
- a disk of the plan found by size or order while a disk outside the
  plan has the same size and no serial number says which is which: one
  of `left` that was not found by its serial number, or one this
  machine has beyond the plan. It cannot tell which one to leave alone.
- a disk left alone that holds a member of a pool with the new pool's
  name: the kernel takes its root pool by name, and started the old
  pool in the new one's place when this was tried. The disk goes into
  the installation, or the layout's `zfs` part gets another `pool`
  name;
- a disk left alone that carries a GPT label the plan makes (`zfs0`,
  ...): FreeBSD would find one of the two under `/dev/gpt`.

Where every disk of a size is in the plan, the order changes nothing
that matters: a mirror over two disks alike is the same mirror either
way. Virtio disks without serial numbers are that case.

The plan is checked like a layout: `mirror` needs 2 disks, `raidz` 3,
`raidz2` 4, `raidz3` 5; a UEFI machine an `efi` part, a BIOS machine a
`bios` part; an `efi` part 33 MiB at least (FAT32).

**Without a `plan.json`** (by hand, in tests) the installer plans
itself from the specification's layout, with `first` (the first disk of
at least 4 GiB) and `.*` (every disk) as the only targets; `disks` of
the specification then names FreeBSD's disks (`nda0`). The layout's
parts are slices with the file system `efi`, `bios` or `swap` and one
part of the type `zfs` with `pool`, `vdev` and `mountpoint: /`; any
other part is refused.

## ZFS

```
zpool create -f -o altroot=/mnt -o ashift=12 -O compression=lz4 -O atime=off -O mountpoint=none \
    zroot [mirror|raidz|raidz2|raidz3] /dev/gpt/zfs0 /dev/gpt/zfs1 ...
```

The members are the `zfs` partitions by their GPT labels. No
encryption and no autotrim in this version. On a machine with less than
4 GB of memory the ARC of the rescue system is held to a quarter of it
meanwhile. The datasets are FreeBSD's own arrangement (bsdinstall's,
without `usr/ports` and `usr/src`):

| Dataset | Mountpoint | Set |
|---|---|---|
| `zroot/ROOT` | none | |
| `zroot/ROOT/default` | `/` | `canmount=noauto`; the pool's `bootfs` |
| `zroot/home` | `/home` | |
| `zroot/tmp` | `/tmp` | `setuid=off` |
| `zroot/usr`, `zroot/var` | `/usr`, `/var` | `canmount=off`: both are in the boot environment |
| `zroot/var/audit`, `var/crash`, `var/log` | | `exec=off`, `setuid=off` |
| `zroot/var/mail` | | `atime=on` |
| `zroot/var/tmp` | | `setuid=off` |

The system is in `zroot/ROOT/default`, a boot environment that `bectl`
and `freebsd-update` find (`bectl list`: `default NR`).

**Swap** is a plain partition on every disk that has the part, each in
`/etc/fstab`; not mirrored. **The EFI system partition** is made on
every disk that has the part, each with the loader; `/etc/fstab` mounts
the first at `/boot/efi` with `failok` and no check at boot, so that a
mirror that lost that disk still starts. The others hold the copy the
installation made: keeping them current after a loader update is the
operator's, as on any FreeBSD mirror.

## What of the specification applies

| Specification | On FreeBSD |
|---|---|
| `os`, `storage` | `os/<os>/<arch>/manifest.json` of the storage; its `family` must be `freebsd` |
| `hostname` | `hostname=` in `/etc/rc.conf`, and `/etc/hosts` |
| `timezone` | `/etc/localtime` (a copy of the zone file) and `/var/db/zoneinfo` |
| `kernel.mitigations` | `false`: `vm.pmap.pti="0"` in `/boot/loader.conf` on amd64 (page table isolation off); `true`: FreeBSD's defaults |
| `kernel.ipv6` | `false`: no interface gets an IPv6 address (the kernel has IPv6 built in; nothing switches it off); `true`: the uplink's |
| `kernel.extra` | words `NAME=VALUE`: loader tunables, appended to `/boot/loader.conf`; other words are left out with a warning |
| `layout`, `disks` | the plan, above; the layout's name is recorded |
| `users` | step 10 |
| `userdata` | type `bash`, step 11; another type is refused |
| `firewall` | pf, below |
| `network.rename` | below |
| `network.uplink` | the uplink it names instead of the rescue system's, as [for the other systems](../README.md#the-uplink-of-the-specification): `static`, `dhcp` or `none`; IPv6 `static`, `auto` or `none`; `dns`, `dns_from_dhcp` |
| `network.private_networks` | recorded, no interface configured, as elsewhere |
| `profile` | recorded |
| `keep_data` | refused |

## The network

`/etc/rc.conf`, for a machine with two interfaces:

```
ump_ifnames_enable="YES"
ump_ifnames="ext01=0c:c4:7a:aa:bb:11 int01=0c:c4:7a:aa:bb:10"
ifconfig_int01="up"
ifconfig_ext01="inet 203.0.113.10/26"
defaultrouter="203.0.113.1"
```

A gateway outside the address's network (a /32, as some providers
give) is reached on-link instead: `static_routes="umpgw umpdefault"`,
`route_umpgw="-host GATEWAY -interface ext01"`,
`route_umpdefault="default GATEWAY"`. Without an address the uplink is
`DHCP`; FreeBSD's dhclient identifies the machine by its MAC by itself,
as iPXE and the Linux rescue system do. IPv6, when the profile keeps
it: `ifconfig_ext01_ipv6` with the address, and `ipv6_defaultrouter`
for a link-local gateway (any other is reached on-link, as above), or
`inet6 accept_rtadv` with `rtsold` for `auto`. The resolvers (1.1.1.1
and 8.8.8.8, or those of `network.uplink`) are in `/etc/resolv.conf`
and stay there (`resolvconf=NO` in `/etc/resolvconf.conf`), unless the
specification wants the lease's.

**Interface names.** FreeBSD renames an interface by its driver's name
(`ifconfig_em0_name`), which is the kernel's order and not the MAC. So
the installer brings an rc script, `/usr/local/etc/rc.d/ump_ifnames`,
which runs before `netif`: for every `NAME=MAC` of `ump_ifnames` it
renames the interface with that MAC. The names are those of the other
systems, by the same rule (the uplink and the interfaces with a public
address `ext01`, ...; the rest `int01`, ...), and `/etc/ump/install`
lists them the same way. `rename: false` keeps the kernel's names and
writes no script. sysctl names (`dev.em.0`) keep the driver's.

Services: `sshd`, `ntpd` (with `ntpd_sync_on_start`), `zfs`, pf; no
sendmail; `syslogd -ss`.

## The firewall

For `{policy: drop, allow: [icmp, ssh, "tcp/8000-8100", {proto: tcp,
port: 9100, from: 10.0.0.0/8}]}`, `/etc/pf.conf` is:

```
set skip on lo0
set block-policy drop
block in all
pass out all
pass in inet proto icmp
pass in inet6 proto icmp6
pass in proto tcp to port 22
pass in proto tcp to port 8000:8100
pass in inet proto tcp from 10.0.0.0/8 to port 9100
```

with `pf_enable="YES"` and `pflog_enable="YES"` in `/etc/rc.conf`. pf
keeps state, so the answers to the server's own connections pass, as
`ct state established,related` lets them on the other systems.
`policy: accept` writes nothing and enables nothing. One difference to
the nftables ruleset: pf has no forward chain, so nothing corresponds
to "forwarded connections from outside are held to the same list":
`block in all` holds for every packet that comes in on any interface
but `lo0`. Jails and virtual machines behind the host need rules of
their own.

## Packages and updates

The installed system has what the manifest's packages bring and
FreeBSD's repository configured as the base brings it
(`/etc/pkg/FreeBSD.conf`, the quarterly branch): `pkg install`, in
userdata too, needs that host reachable, as `apt-get install` needs an
archive.

The installer runs no `freebsd-update`: the base is the release as
published, without its patches, and the packages are those of the day
the image was built. `freebsd-update fetch install` and `pkg upgrade`
are the operator's, or userdata's.

Not there yet: the operator's tools of the other systems (htop, tmux,
...), UMP's message of the day and bash prompt.

## The storage

| Path | What the installer takes |
|---|---|
| `os/<os>/<arch>/manifest.json` | `family` (`freebsd`), `release` (`15.1-RELEASE`: the rescue system's own, unless `--force`), `version`, `unpacked_mb`, `rescue` (named in a refusal), `unsupported` (a reason there is nothing for this machine), `files` with the `sha256` of `base.txz`, `kernel.txz` and `packages.tar`, and `packages`: each with `name`, `file` and `wanted` (`true` for those to add; what they depend on comes with them); `pkg` must be among them |
| `os/<os>/<arch>/base.txz`, `kernel.txz` | the release's distribution sets, as FreeBSD publishes them |
| `os/<os>/<arch>/packages.tar` | the packages' files, an uncompressed tar |

## The record

`/etc/ump/install` has the keys of every system, with FreeBSD's
values:

```
os=freebsd-15.1
image_version=15.1-RELEASE
image_kernel=15.1-RELEASE
installed=2026-11-26T14:46:59Z
profile=freebsd-15.1
layout=zfs-mirror
disks=/dev/nda0 /dev/nda1
boot_disk=/dev/nda0
root=zroot/ROOT/default
hostname=web-01
admin=ump
interfaces=ext01=0c:c4:7a:aa:bb:11 int01=0c:c4:7a:aa:bb:10
uplink=ext01
ipv6=off
mitigations=off
firewall=drop
userdata=freebsd
private=default:10.0.0.7
private_network=default
private_address=10.0.0.7
```

`image_version` and `image_kernel` are `freebsd-version -u` and `-k` of
the new system; with `network.uplink` also `network=`, `network6=` and
`dns=`. The last line the installer prints is

```
==> installed: freebsd-15.1 15.1-RELEASE as web-01, root on zroot/ROOT/default (578M used; layout zfs-mirror over 2 disk(s)); reboot to start it
```

## Testing

[`images/test-freebsd-render.sh`](../../images/test-freebsd-render.sh)
(`make freebsd-render-test`, part of `make check`) runs the installer's
renderers without a machine, under any sh with `jq`, and compares what
they give with
[`images/testdata/freebsd-render`](../../images/testdata/freebsd-render):
plans against the disks of machines (which disk is which, the commands
that partition them and make the pool, `/etc/fstab`, the refusals), the
layouts planned without a `plan.json`, `/etc/rc.conf` and the resolvers
for every uplink of [`images/test-network.sh`](../../images/test-network.sh),
`/etc/pf.conf`, `/boot/loader.conf`, the record.

[`images/freebsd/test-install.sh`](../../images/freebsd/test-install.sh)
(`make freebsd-install-test`) installs a machine in QEMU: the disks
staged as the Linux rescue system leaves them (the rescue image, its
boot config, a `plan.json`), the rescue system started from them by the
firmware, `--dry-run` and the refusals with no write to any disk
meanwhile (QEMU counts them), the installation, and the new system
checked over SSH as its administrator, with a port the firewall lets in
and one it does not tried from outside. `LAYOUT=zfs-mirror` and
`zfs-raidz` start the system again without its first disk; `PLAN=none`
leaves the planning to the installer; `SERIALS=1` gives the disks
serial numbers in another order than the plan's; `LEFT=12` adds a disk
the plan leaves alone, which must not be written to; `NET=dhcp`,
`NET=onlink`, `UPLINK=`, `IPV6=` and `RENAME=no` change the network;
`AGAIN=1` installs a second time over the first; `FIRMWARE=bios` on
amd64. Emulated amd64 runs with the CPU model `qemu64`: with QEMU's
newer models the kernel of FreeBSD 15.1 ends in a page fault while
`zfs.ko` loads.

## Not in this version

`--keep-data`, pools beside the root pool, encryption, `freebsd-update`
during the installation, the operator's tools, an unattended
installation.
