# The UMP FreeBSD rescue image

FreeBSD is installed with FreeBSD's own tools: `gpart`, `zpool` and
`zfs` of the release that will import the pool. They run on FreeBSD
alone, so an installation of FreeBSD goes through a second rescue
system, this one: the release's GENERIC kernel with a root in memory
made from its `base.txz`, and the tools an installer for FreeBSD calls.
Nothing jumps from Linux into a FreeBSD kernel reliably, so the
machine's firmware starts it. The image is a small disk with a
partition table of its own: from the UMP Linux
[rescue system](../rescue/README.md) it is written to the start of a
disk, a [boot config](#configuration-at-boot) put into its first
partition, and the machine rebooted. The system then runs from memory
with no disk mounted, so that an installer there repartitions the very
disk it came from.

One image per release and architecture, `disk.img.zst`: FreeBSD 15.1
and 14.5, amd64 and arm64. One per release, because a pool is made by
the release that will import it, with no feature of a newer ZFS. The
amd64 image starts under UEFI and under BIOS, the arm64 one under UEFI.

## Building

Needs Docker or Podman. The build runs as root in a Debian container
without privileges: nothing is mounted and nothing of FreeBSD runs, so
either architecture is built on any host, without emulation.

```console
$ make freebsd-image                                        # 15.1 and 14.5, amd64 and arm64
$ make freebsd-image FREEBSD_LIST=15.1 FREEBSD_ARCH=arm64   # one release, one architecture
```

or, on a Debian trixie host as root, `images/freebsd/build.sh arm64 15.1`.
[`build.sh`](build.sh) takes these from FreeBSD:

- **The distribution sets** `base.txz` and `kernel.txz` of the release
  and its source, `src.txz`, from `download.freebsd.org`, checked
  against [`SHA256SUMS`](SHA256SUMS), which has their checksums from
  each release's `MANIFEST`. Another release needs its lines there
  first.
- **The package catalogue** of the release's quarterly repository at
  `pkg.FreeBSD.org`. It must be signed by a key the release trusts: the
  fingerprints are those of `/usr/share/keys/pkg/trusted` in `base.txz`.
- **Packages**, each checked against the catalogue's checksum: those of
  [`packages.txt`](packages.txt) with what they depend on, for the
  installed system, and `pkg` and `jq` for the rescue system.
- **The source of the packages** whose licenses ask for it
  ([below](#licenses-and-sources)): the port each was built from, out
  of FreeBSD's ports tree (`git.FreeBSD.org`) at the commit the package
  names, and the files that port's `distinfo` names, from FreeBSD's
  distribution files (`distcache.FreeBSD.org`), each checked against
  the `distinfo`.

The memory root is `base.txz` without the patterns of
[`rescue-exclude.txt`](rescue-exclude.txt), the modules of `kernel.txz`
without those of the same list, three files from the packages, and
[`overlay/`](overlay). `tar` unpacks, Debian's `makefs` makes the two
UFS file systems, `mkfs.vfat` and `mtools` the FAT one, `sgdisk` the
partition table. `VERSION` (default: the date), `OUT`, `MIRROR`,
`PKG_MIRROR`, `PKG_BRANCH`, `DISTCACHE` and `PORTS_GIT` can be set in
the environment. A second build takes the sets, the source and the
packages that the first left in `out/`, when their checksums still
hold, and the distribution files and ports of `out/cache/`, and
downloads little more than the catalogue; an image then takes about a
minute.

The result is two directories per release and architecture, named as
the storage has them. `images/freebsd/out/rescue/freebsd-<release>/<arch>/`
is the rescue image:

| File | What |
|---|---|
| `disk.img.zst` | the disk image, zstd-compressed |
| `manifest.json` | version, `system` (`freebsd`), release, kernel, what the image can do (`features`), the memory it needs (`memory_mb`), the size of the disk image (`disk_mb`), SHA-256 and size of `disk.img.zst`, where the storage has the source of its FreeBSD (`source`: the path of `src.txz`, its SHA-256 and size), and the packages whose files are in the image, with their licenses (`packages`); uploaded last |
| `SHA256SUMS` | the same for `sha256sum -c` |
| `packages.txt` | the packages whose files are in the image |

`images/freebsd/out/os/freebsd-<release>/<arch>/` is what an installer
puts onto the disks. FreeBSD is not unpacked from an image UMP builds:

| File | What |
|---|---|
| `base.txz`, `kernel.txz` | the release's distribution sets, as FreeBSD publishes them |
| `packages.tar` | FreeBSD's packages, each as `NAME-VERSION.pkg`, in a tar without compression (they are compressed themselves) |
| `src.txz` | the release's source, as FreeBSD publishes it: the source of both sets |
| `packages-source.tar` | the source of the packages whose licenses ask for it ([below](#licenses-and-sources)), in a tar without compression |
| `manifest.json` | version, `os` (`freebsd-15.1`), `family` (`freebsd`), release, kernel, the rescue image the system is installed from (`rescue`), the size of the installed system (`unpacked_mb`), SHA-256 and size of the five files, and every package: name, version, file, SHA-256, size, whether `packages.txt` names it (`wanted`) or another package needs it, the port it was built from (`origin`), its `licenses`, and whether its source is in `packages-source.tar` (`source`) |
| `SHA256SUMS`, `packages.txt` | for people |

With both in the storage, an installation needs neither
`download.freebsd.org`, which moves old releases away, nor
`pkg.FreeBSD.org`, and every machine gets the same packages: those of
the day the image was built.

## Publishing

`make upload` publishes every built image to the storage, these to
`rescue/freebsd-<release>/<arch>/` and `os/freebsd-<release>/<arch>/`
beside the Linux rescue image and the other systems; see
[the storage layout](../rescue/README.md#your-own-storage).
`images/upload.sh rescue os` publishes the images alone.

## Licenses and sources

UMP's [license](../../LICENSE) covers this directory: the scripts, the
lists and the overlay. What they put into the storage is FreeBSD's and
stays under its own licenses, and it is published with its source
wherever a license asks for that, in the same place as the binaries.
No installer fetches a source: it is there for whoever takes the
binaries from the storage.

**FreeBSD itself**, the two sets and what the rescue image has of
them, is under BSD licenses for the most part, as `/COPYRIGHT` of
`base.txz` says, with parts under others: ZFS and DTrace under the
CDDL, the C++ library under the Apache license with LLVM's exceptions,
a few programs and files under GNU licenses. `src.txz` is the release's
own source of all of it, taken from where the sets are and checked
against [`SHA256SUMS`](SHA256SUMS) as they are. The storage has it
beside them, `os/freebsd-<release>/<arch>/src.txz`: one file per
release, the same for both architectures.

**The packages** are FreeBSD's as well, as its repository has them,
each with its license inside (`/usr/local/share/licenses/NAME-VERSION/`).
[`licenses.txt`](licenses.txt) has the license names of FreeBSD's
ports, each with whether it asks whoever passes a program on for the
source. `build.sh` decides by it, from the licenses the signed
catalogue names for a package, and stops at a package under a license
the list lacks and at one that names none: somebody reads the license
first. Of today's packages:

| Package | Licenses | With its source |
|---|---|---|
| `bash` | GPLv3+ | yes |
| `readline` | GPLv3 | yes |
| `gettext-runtime` | GPLv3+ and LGPL21+ | yes |
| `python312` | PSFL | yes: its module `readline` is linked with the library of `readline`, and a line of `licenses.txt` says so |
| `expat`, `libffi` | MIT | no |
| `indexinfo`, `mpdecimal`, `pkg` | BSD2CLAUSE | no |
| `sudo` | its own: ISC, with files under BSD licenses and zlib's | no |
| `python3` | none: symbolic links to the programs of `python312` | no |

`packages-source.tar`, beside `packages.tar`, has that source as
FreeBSD built the packages from it: a `README`, and per package
`NAME-VERSION/` with

- `ports/`: the port's directory of FreeBSD's ports tree as it was at
  the commit the package names (`ports_top_git_hash`), and the
  directories its makefiles take files from (`devel/gettext` for
  `gettext-runtime`): the Makefile, the `distinfo`, FreeBSD's patches,
  the packing list; with them the tree's `COPYRIGHT`;
- `distfiles/`: every file the `distinfo` names, the authors' release
  and their patches, each with the size and the SHA-256 the `distinfo`
  has for it.

The directory comes out of the ports tree's repository by that commit,
so git's hashes hold for it from the commit down; the commit is the one
the catalogue has for the package. FreeBSD's distribution files are
served without TLS, as the ports tree has their address, and the
`distinfo` is what vouches for them. Nothing is built from the source
here: that it gives the package again is FreeBSD's word, the port and
its `distinfo` at that commit.

**The rescue image** has the release's kernel and a part of its base,
so its source is the same `src.txz`: the image's manifest names it
(`source`), and so does `/etc/ump-rescue-source` in the image, with the
checksum. The image keeps FreeBSD's notices, `/COPYRIGHT` and the
license files of `/usr/share/doc` (`legal`, `llvm`, `atf`, `kyua`), and
has the licenses of `pkg`, `jq` and `oniguruma` beside the files taken
from them, in `/usr/local/share/licenses`. None of the three asks for
its source (BSD2CLAUSE, MIT, BSD2CLAUSE); one that did would have it in
a `packages-source.tar` beside `disk.img.zst`.

In a build's output the sources are `src.txz` and `packages-source.tar`
of `out/os/freebsd-<release>/<arch>/`, and what was downloaded for the
second stays in `out/cache/`. `build.sh` ends with
[`test-sources.sh`](test-sources.sh), which fails a build whose
packages are not all decided, or whose sources are not all there
([Testing](#testing)), and [`images/upload.sh`](../upload.sh) refuses
the sets without `src.txz`, a manifest that names a
`packages-source.tar` which is not there, and a rescue image without
the `src.txz` of its build.

## The disk image

A disk of 149 to 177 MiB with a GPT:

| # | Type | GPT label | Size | Holds |
|---|---|---|---|---|
| 1 | EFI system | `umpboot` | 64 MiB | FAT32: FreeBSD's `loader.efi` as `EFI/BOOT/BOOTX64.EFI` or `BOOTAA64.EFI`, and the directory `ump` for the boot config |
| 2 | freebsd-boot (amd64 only) | `umpbootcode` | 512 KiB | `gptboot`; the protective MBR has `pmbr` in its first 440 bytes |
| 3 | freebsd-ufs | `umpbootfs` | 82 to 110 MiB | UFS: `/boot/loader` (amd64), [`/boot/loader.conf`](loader.conf), the loader's defaults and Lua files, `/boot/kernel/kernel`, and the memory root as `/boot/mfsroot.gz` |

UEFI firmware starts the loader from the removable path of the first
partition; no boot entry is written, and the boot order stays as it
was. A BIOS runs `pmbr`, which loads `gptboot`, which starts
`/boot/loader` of the third partition. Either loader finds
`loader.conf` on that partition, loads the kernel and unpacks the
memory root while it loads it; the kernel mounts it as its root, `md0`,
writable. The kernel's messages go to the serial port and the screen. A
kernel that panics reboots after 15 seconds (FreeBSD's default), into
the same image.

Both UFS file systems are UFS1. Debian's `makefs` is NetBSD's, and
FreeBSD's loader refuses its UFS2 ("UFS2 superblock failed:
fs->fs_sblockloc (0x2000) != SBLOCK_UFS2 (0x10000)"); its UFS1 is taken
by `loader.efi`, by `gptboot` and the BIOS loader, and by the kernel.
The table is laid out for 512-byte sectors.

## Starting it

Whatever writes the image does three things, here by hand from a Linux
with `zstd`, `sgdisk` and a disk of 512-byte sectors that may be
erased:

```console
# curl -fsSL https://files.example.net/rescue/freebsd-15.1/amd64/disk.img.zst | zstd -dc | dd of=/dev/sda bs=1M conv=fsync
# sgdisk -e -G /dev/sda
# partprobe /dev/sda && udevadm settle
# mount -t vfat /dev/disk/by-partlabel/umpboot /mnt
# mkdir -p /mnt/ump && cp /root/.ssh/authorized_keys /mnt/ump/
# echo "ump.hostname=web-01 ump.mac=52:54:00:12:34:56 ump.ip=203.0.113.10/26,203.0.113.1" > /mnt/ump/cmdline
# umount /mnt && reboot
```

`sgdisk -e` moves the backup partition table, which the image has
where it ended, to the end of the disk, and `-G` gives the disk
identifiers of its own, for a machine that gets the image on several
disks. Every other disk the firmware might start instead must hold
nothing it can start. Without keys in the boot config nobody logs in
over SSH.

## What is in it

- **The kernel:** GENERIC, as `kernel.txz` has it, with the drivers of
  server hardware built in: on amd64 (15.1) virtio, `nvme`, `ahci`, `em` (with
  igb), `igc`, `ix`, `ixl`, `ice`, `bge`, `bce`, `bxe`, `re`, `mlx5en`,
  `vmx`, and the controllers `mps`, `mpr`, `mpi3mr`, `mrsas`, `mfi`,
  `aacraid`, `ciss`, `smartpqi`; on arm64 fewer, and `ixl`, `bxe`,
  `aacraid`, `ciss` and `smartpqi` not at all: FreeBSD builds them for
  amd64 alone.
- **Modules:** all of `kernel.txz` but wireless LAN, sound, Bluetooth
  and graphics with their firmware, and the QuickAssist accelerators.
  `zfs` is loaded at boot; `devmatch` loads the drivers GENERIC lacks
  for the hardware it finds (`if_bnxt`, `mlx4en`, `if_ena`, ...).
- **The userland:** `base.txz` without the compiler, linker and
  debugger with their libraries (over 200 MB), static libraries,
  headers, manual pages, locales but `C.UTF-8`, tests, `/rescue`, and
  what else [`rescue-exclude.txt`](rescue-exclude.txt) lists. What
  stays is a whole FreeBSD: `sh`, `sshd`, `gpart`, `zpool`, `zfs`,
  `newfs_msdos`, `fetch`, `tar`, `pw`, `pfctl`, `camcontrol`,
  `nvmecontrol`, `pciconf`, `tcpdump`, `dhclient`. The memory root has
  none of the boot loader's files and no kernel file, only the modules:
  an installer takes the boot code from the system it installed.
- **From packages:** `pkg-static`, with which an installer adds
  packages to a new system, and `jq` with its two libraries, since the
  base has no tool for JSON; the licenses of the three packages.
- **Notices:** `/COPYRIGHT` and the license files of `/usr/share/doc`,
  as `base.txz` has them, and `/etc/ump-rescue-source`: what the
  image is made of, and where the source is
  ([Licenses and sources](#licenses-and-sources)).
- **UMP's:** `/etc/ump-rescue-version`, the rc scripts
  [`ump_rescue_setup`](overlay/etc/rc.d/ump_rescue_setup) and
  [`ump_rescue_report`](overlay/etc/rc.d/ump_rescue_report),
  [`rc.conf`](overlay/etc/rc.conf) (sshd; no sendmail, no cron, no pool
  imported), [`sshd_config`](overlay/etc/ssh/sshd_config) as the Linux
  rescue system has it, `/tmp` in memory, a message of the day.

Root has no password. It logs in over SSH with the keys of the boot
config only, and on the console (serial or local) without a password,
since the image keeps no state and no secrets. SSH host keys are
generated at every boot.

## Configuration at boot

The boot config is the one the Linux rescue image takes from a PXE
server, under the same names: a directory `ump`, here on the FAT
partition `umpboot`.

| File | What |
|---|---|
| `ump/cmdline` | words of a kernel command line, below |
| `ump/authorized_keys` | root's keys; it wins over `ump.authorized_keys` |
| `ump/plan.json`, any other file | not read here: for the installer |
| `ump/boot.log` | written by the rescue system at every boot, [below](#when-it-does-not-answer) |

[`ump_rescue_setup`](overlay/etc/rc.d/ump_rescue_setup) runs before the
hostname, the network and sshd. It mounts the partition read-only,
copies every file of `ump` but `boot.log` to `/etc/ump/boot` (0700, the
files 0600, their names in small letters), unmounts it and takes these
words of `cmdline`, which mean what they mean
[in the Linux rescue system](../rescue/README.md#configuration-at-boot).
A value with a character that does not belong in it is ignored, and
the log says so:

| Parameter | Meaning |
|---|---|
| `ump.hostname=NAME` | the hostname; `ump-rescue` without it |
| `ump.authorized_keys=BASE64` | root's `authorized_keys`, base64 on one line |
| `ump.mac=aa:bb:cc:dd:ee:ff` | the interface to configure; without it, or with no such interface, the first Ethernet interface |
| `ump.ip=203.0.113.10/26,203.0.113.1` | IPv4 address with prefix, and the gateway (on-link, so `/32` setups work) |
| `ump.ip6=2001:db8::2/64,fe80::1` | IPv6 address with prefix, and the gateway |
| `ump.dns=10.0.0.53,10.0.0.54` | resolvers; without it 1.1.1.1 and 8.8.8.8, whatever a lease says; `dhcp` for the lease's alone |
| `ump.boot-mac=aa:bb:cc:dd:ee:ff` | DHCP on that interface alone, with the MAC as the client identifier (FreeBSD's `dhclient` sends it by itself); the other Ethernet interfaces come up without addresses |
| `ump.time=1793613600` | the clock, in seconds since 1970, when it is more than 5 minutes off |

Without `ump.ip`, `ump.ip6` and `ump.boot-mac`, every Ethernet
interface uses DHCP, each waited for. `ump.iface` and the `ump.pxe`
words of the Linux image mean nothing here. A word that `cmdline`
lacks is looked up in the kernel environment (`kenv`), which the loader
fills from its `loader.conf`, under UEFI from `efi/freebsd/loader.env`
of the same partition (words as in `cmdline`), and from `set` at its
prompt: enough to start the image by hand. A value there is 128 bytes
at most, so a key longer than an ed25519 one needs the file. The
settings go to `/etc/rc.conf.local`, what was decided to
`/var/log/ump-rescue-setup.log`.

The rescue system is up when root, with a key of the boot config, gets
a version and the release from

```console
# cat /etc/ump-rescue-version && uname -sr
2026.11.26
FreeBSD 15.1-RELEASE
```

## When it does not answer

Between the reboot and the first answer over SSH there is only the
console, and many machines have none within reach. So
[`ump_rescue_report`](overlay/etc/rc.d/ump_rescue_report) writes
`ump/boot.log` to the partition `umpboot` once the network scripts ran,
whether they got an address or not: what the setup decided, the
interfaces with their link state, the routes, the resolvers, the
network and storage devices with the drivers that took them (`none0`
is a device without a driver), the disks, and the kernel's messages,
7 to 10 KB. It is read from there in the Linux rescue system, on the
next way in:

```console
# mount -o ro /dev/disk/by-partlabel/umpboot /mnt && cat /mnt/ump/boot.log
```

No log after a boot means the kernel did not reach its rc scripts, or
the firmware did not start the disk. An interface that waits for a
lease it does not get holds the boot for a minute and a half, and the
log is written after that.

## Testing

```console
$ make freebsd-test TEST_ARCH=arm64                        # both releases; default: this machine's architecture
$ images/freebsd/test-boot.sh amd64 14.5                   # one image, with UEFI and then BIOS
$ FIRMWARE=bios images/freebsd/test-boot.sh amd64 15.1     # one firmware
$ make freebsd-sources-test                                # the sources of every built image; no machine
```

[`test-boot.sh`](test-boot.sh) stages a disk as above (the image,
`sgdisk -e -G`, a boot config written with `mtools`) and starts it in
QEMU with UEFI firmware, on amd64 also with SeaBIOS, four times per
firmware:

- **static:** hostname and key from the boot config, an IPv4 address
  with `/32` and an on-link gateway, an IPv6 address with a link-local
  gateway, the resolvers, and `ump.time` on a machine whose clock is in
  2020. Then the system: the version, the root on `md0` and no disk
  mounted, the boot config in `/etc/ump/boot`, sshd's settings, every
  tool an installer calls, `jq` and `pkg-static` running, `pf.ko`
  loading and `pfctl` taking a ruleset, the time zones, the CA
  certificates where OpenSSL finds them, FreeBSD's notices, the
  packages' licenses and `/etc/ump-rescue-source`, a GPT, a FAT32 file
  system and a ZFS pool made on a second disk, the drivers
  of server NICs and storage controllers in the kernel or loading
  (QEMU only has virtio, and a driver loads without its hardware), and
  no program or library left without a library it needs. After the
  shutdown, `boot.log` on the partition is this boot's.
- **dhcp:** two NICs and `ump.boot-mac` naming the second: DHCP there
  alone, the lease's resolver (`ump.dns=dhcp`), the key from
  `ump.authorized_keys`, an `ump.ip` that is no address ignored, and
  under UEFI the hostname from `loader.env` while `cmdline` still wins
  where it has the word.
- **memory:** a boot config with the key alone, on a machine with the
  manifest's `memory_mb`: DHCP on every interface, the public
  resolvers, `zfs` loaded.
- **silent:** a NIC that gets no lease. Nobody logs in; the test waits
  for the login prompt on the serial console and reads `boot.log` from
  the disk.

It uses HVF on macOS and KVM on Linux where the architectures match,
and plain emulation otherwise. Emulated amd64 runs with QEMU's `qemu64`
CPU: with its models of newer CPUs (`Haswell`, `max`) the 15.1 kernel
faults while `zfs.ko` loads, and whether hardware of that kind does so
too is not known yet. The disk is written by `zstd`, `sgdisk` and
`mtools` of the host, or of a Debian container the test starts where
the host lacks one of them. `MEM`, `TIMEOUT`, `PORT` and `KEEP=1` (the
serial logs stay) are in the script's header.

[`test-sources.sh`](test-sources.sh) needs no machine: it reads the two
directories of a build, with `tar`, `zstd` and `jq`. `src.txz` must
have the checksum `SHA256SUMS` pins, and the manifests must name it.
Every package of `packages.tar` is asked for its licenses, in its own
manifest, and `licenses.txt` must decide it. Each package that goes
out with its source must have its port in `packages-source.tar`, with
the ports tree's `COPYRIGHT` and a `distinfo`, and under `distfiles/`
every file the `distinfo` names, with that size and SHA-256, and no
other; the distribution files the
package itself says it was built from (`catalog.mk`, beside its
license) must be among them. The same holds for the packages whose
files are in the rescue image, by the licenses its manifest has for
them.

## Memory

The memory root is held unpacked: the loader hands it to the kernel as
it is, and it stays in memory, its free space included, 32 MiB for
what the running system writes. On the builds of 2026-11-26:

| | amd64 15.1 | amd64 14.5 | arm64 15.1 | arm64 14.5 |
|---|---|---|---|---|
| base, trimmed | 112 MB | 104 MB | 101 MB | 96 MB |
| modules | 89 MB | 88 MB | 54 MB | 51 MB |
| memory root, unpacked / as `mfsroot.gz` | 246 / 77 MB | 238 / 74 MB | 198 / 64 MB | 190 / 60 MB |
| kernel | 29 MB | 28 MB | 19 MB | 18 MB |
| disk image | 177 MB | 173 MB | 153 MB | 149 MB |
| `disk.img.zst` | 85 MB | 82 MB | 69 MB | 65 MB |
| `memory_mb` | 592 | 582 | 524 | 514 |
| free after a boot with that much | 167 MB | 173 MB | 199 MB | 201 MB |
| the sets and `packages.tar` | 247 MB | 249 MB | 223 MB | 224 MB |
| their source: `src.txz` and `packages-source.tar` | 241 + 45 MB | 215 + 45 MB | 241 + 45 MB | 215 + 45 MB |

`memory_mb` is the memory root, twice the kernel, and 288 MB for the
running system, an installer and ZFS's cache; whatever stages the image
should refuse a machine with less, before it erases anything. A machine
with exactly that much comes up with 170 to 200 MB free (`test-boot.sh`
boots one), which is little for an installation; with 1 GB it has 590
to 690 MB. From power-on to root over SSH, QEMU takes 23 to 37 s on
arm64 with HVF and 23 to 46 s on emulated amd64 where the host is
idle, the loader's countdown of 2 s included; a real machine adds its
firmware's time.

## Limitations and next steps

- No IPv6 by router advertisements: IPv6 comes from `ump.ip6` alone.
- One static configuration on one interface, or DHCP; no bonds, no
  VLANs.
- Disks with 4096-byte logical sectors cannot carry the image.
- Secure Boot must be off: FreeBSD's loader is not signed.
- The modules are trimmed by a list of patterns; those the kernel has
  built in are still there as files, 6 to 10 MB that could go.
- The image is the release as published, without its patches, and the
  packages are a quarterly snapshot of the build day; the manifests pin
  what was built.
- Whether a package asks for its source is decided by the license its
  port names. A package linked with the library of another one that
  does is found by reading, not by the build: `python312` has its line
  in `licenses.txt`, and a new package needs the same look.
