# The UMP rescue image

Every provider has a rescue system, and every one is different: another
base distribution, other tools, other quirks. UMP uses the provider's
rescue system for one thing only, to get root over SSH once; then it
downloads **this** image and `kexec`s into it, so that the inspection and
installation steps run in the same environment everywhere: a Debian
system with a generic kernel, the firmware of server NICs and storage
controllers, and the tools the installer calls, running entirely from
RAM with no disk mounted. The same image serves the `kexec` entry method
on clouds without a rescue system, and later virtual media and PXE.

The image is one kernel plus one initramfs per architecture (amd64,
arm64). The initramfs is small: a static busybox, the `loop`,
`squashfs` and `overlay` modules, [`init`](init) and the whole system as
`rootfs.squashfs`. `init` mounts the squashfs read-only, puts a
writable layer in RAM on top (overlayfs) and starts systemd there, so
the system costs its compressed size in memory instead of its unpacked
size. There is still nothing to fetch but the two files, and no disk is
mounted. When `init` cannot mount the system it says why on the
console and opens a shell there.

## Building

Needs Docker or Podman; the build runs as root in a privileged Debian
container (mmdebstrap mounts `/proc` and `/sys` into the chroot):

```console
$ make rescue-image                  # out/amd64/ and out/arm64/
$ make rescue-image RESCUE_ARCH=arm64  # one architecture
```

or, on a Debian trixie host as root, `images/rescue/build.sh arm64`.
The container runs on the host's own architecture either way: the other
architecture is bootstrapped through `qemu-user-static` and its `kexec`
is cross-compiled, which is slower than native but works the same (an
emulated container, as Docker Desktop offers, does not: mmdebstrap's apt
hooks break on the file descriptors the emulation adds). `VERSION`
(default: the date), `SUITE`, `MIRROR` and `OUT` can be set in the
environment; `BUILD_KEXEC=0` skips the static `kexec`.

The result is in `images/rescue/out/<arch>/`:

| File | What |
|---|---|
| `vmlinuz` | Debian's generic kernel (`linux-image-<arch>`): a bzImage on amd64, a gzip-compressed Image on arm64 |
| `initrd.zst` | the whole system as a zstd-compressed cpio archive |
| `kexec` | a static `kexec` (kexec-tools), for rescue systems without one |
| `manifest.json` | version, kernel, SHA-256 and size of each file; uploaded last |
| `SHA256SUMS` | the same for `sha256sum -c` |
| `packages.txt` | every package and version in the image |

`manifest.json` is what UMP will fetch first: it pins the checksums of the
other files. Publish the directory as is (`<base-url>/<arch>/...`);
`kexec-into-rescue.sh` and later `ump` take the base URL.

## Publishing

```console
$ make upload UPLOAD_TARGET=s3://my-bucket/dl       # every built image, the binaries, the scripts and the profiles
$ images/upload.sh rescue                           # one kind: rescue, os, index, bin, scripts or profiles
```

[`images/upload.sh`](../upload.sh) writes the storage layout below to a
target named by `UPLOAD_TARGET`:

| Target | What it needs |
|---|---|
| `s3://bucket[/prefix]` | an S3 bucket, or any S3-compatible store with `S3_ENDPOINT=https://...`; the AWS CLI or rclone, and the credentials the way they take them (`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, or a profile) |
| `scp://[user@]host/abs/path` | a directory on a web server, reached over SSH with your keys (`UPLOAD_SSH_OPTIONS="-i KEY -p PORT"` for anything else); rsync when both sides have it, scp otherwise |
| `file:///abs/path` | a local directory, for example a mounted web root |

`DRY_RUN=1` prints what would be done. Keep the credentials out of the
repository: in a file the shell sources (mode 0600), or in `pass`/`op`,
exported for the command:

```console
$ set -a; . ~/.config/ump/upload.env; set +a
$ make upload
```

A subfolder (`/dl` on this project's storage, under the hostname that
also serves the project's pages) keeps the files apart from whatever
else the hostname serves, so that a rule for `/dl/*` alone can let
scripted clients through a bot protection. Per image, `manifest.json`
goes up last, so a reader never sees a
manifest whose files are not there yet, and every version uploaded is
kept under `v/` for rollbacks (large files go up once and are copied
on the target side). Whatever serves the target over HTTP is the
storage, `storage` in the ump configuration; this project's is
`https://ump.zhurko.de/dl`, ump's default, and the rescue image's base URL
is `<storage>/rescue`.

### Your own storage

Nothing ties ump to that hostname: any web server or object store that
answers plain `GET` requests, without authentication, redirects to login
pages or bot challenges, and that the servers can reach, does. Set
`storage: https://files.example.net/ump` in the configuration (or
`--storage` on `ump server reinstall`, `--image` on `ump server rescue` for the rescue
image alone) and serve this below it:

| Path | What it is |
|---|---|
| `get.sh` | [the install script](../../install/get.sh) of ump itself: `curl -fsSL <storage>/get.sh \| sh` |
| `bin/latest/ump-<os>-<arch>`, `SHA256SUMS`, `VERSION` | the ump binaries (`linux`, `darwin`, `windows`; `amd64`, `arm64`) of the latest release, which get.sh takes |
| `bin/v/<version>/...` | the same per release (`UMP_VERSION=` of get.sh) |
| `rescue/kexec-into-rescue.sh` | the entry script, as in this directory; ump has the server fetch and run it |
| `rescue/<arch>/manifest.json` | fetched first: version, kernel, and the SHA-256 and size of the files below |
| `rescue/<arch>/vmlinuz` | the rescue kernel |
| `rescue/<arch>/initrd.zst` | the rescue system: a zstd-compressed initramfs with `init` and the system as a squashfs |
| `rescue/<arch>/kexec` | a static `kexec` binary, used where the rescue system has none (optional, listed in the manifest when present) |
| `install/install.sh` | [the installer](../../install/README.md); ump has the rescue system fetch and run it |
| `profiles/ubuntu-26.04.yaml`, `profiles/ubuntu-24.04.yaml`, `profiles/ubuntu-22.04.yaml`, `profiles/layouts.yaml`, `profiles/firewalls.yaml`, `profiles/users.yaml` | the published [profile](../../docs/profiles.md), [layouts](../../docs/disk-layouts.md) and [firewalls](../../docs/firewalls.md); a copy in `~/.config/ump/profiles/` wins |
| `os/index.json` | `{"images": ["ubuntu-22.04", ...]}`: the images the storage has, which the profile wizard offers (optional; the known names are tried without it) |
| `os/<os>/<arch>/manifest.json` | an [OS image](../base/README.md): version, kernel, unpacked size, and the SHA-256 and size of the archive |
| `os/<os>/<arch>/rootfs.tar.zst` | the system, a zstd-compressed tar |
| `*/SHA256SUMS`, `*/packages.txt` | for people; nothing fetches them |
| `rescue/v/<version>/<arch>/...`, `os/<os>/v/<version>/<arch>/...` | the same per version, kept for rollbacks (optional) |

`<arch>` is `amd64` or `arm64`, `<os>` is `ubuntu-26.04`. `make upload`
produces exactly this layout from the built images and binaries (`make
release` builds and publishes the binaries alone, from a tagged
commit), and
[`images/upload.sh`](../upload.sh) is a short script to adapt for a store
that is not S3-compatible.

The clients of that hostname are rescue systems with `curl`, not
browsers, and they come from datacenter address space. A bot protection
in front of the storage (a JavaScript challenge, a login page) answers such requests with a web
page that no script can pass, so the hostname needs a rule that lets
scripted clients through, and a test from a server, not from a laptop,
before relying on it. Blocked, the entry script stops at its first
download (it says so, with the HTTP status) and boots nothing, so a
later `ump ssh --staging` lands in the provider's rescue system, not in
UMP's; the `-o file &&` form of the one-liner below fails the same way
instead of feeding an empty download to `bash`.

## What is in it

[`packages.txt`](packages.txt) lists the packages: systemd, udev,
networkd and resolved, OpenSSH, iproute2 and ethtool, the disk tools
(`sgdisk`, `parted`, `mdadm`, `lvm2`, `cryptsetup`, `e2fsprogs`,
`xfsprogs`, `btrfs-progs`, `dosfstools`, `wipefs`, `nvme-cli`,
`smartmontools`), image tools (`squashfs-tools`, `zstd`, `tar`,
`rsync`, `curl`), `kexec-tools`, `efibootmgr`, `ipmitool`, `dmidecode`,
`lshw`, `jq`, `tmux`, and the firmware packages for NICs and storage
controllers. [`finish.sh`](finish.sh) removes firmware for GPUs, Wi-Fi,
Bluetooth and sound, documentation and locales, because none of it
belongs on a server rescue system that lives in RAM, and kernel modules
of the same kind. A module whose dependency went with them goes too,
since modprobe would fail on it anyway; the build stops when that would
be a server NIC or storage driver (`igb` needs `dca`, `ixgbe`
`xfrm_algo`, `ice` `gnss`, `sfc` `mtd`, so those stay). Until 2026-10-17
the image removed `dca`, and Intel I210 NICs (`igb`, Hetzner's AX
servers) booted without network; [troubleshooting](../../docs/troubleshooting.md#case-study-hetzner-ax41-nvme-2026-10-16)
tells how that was found without a console.

Root has no password. It logs in over SSH with the keys from the kernel
command line only, and on the console (serial or local) without a
password, since the image keeps no state and no secrets.

## Configuration at boot

[`ump-rescue-setup`](overlay/usr/local/sbin/ump-rescue-setup) runs
before networkd and sshd and reads the kernel command line:

| Parameter | Meaning |
|---|---|
| `ump.hostname=NAME` | the hostname |
| `ump.authorized_keys=BASE64` | root's `authorized_keys`, base64 on one line |
| `ump.mac=aa:bb:cc:dd:ee:ff` | the interface to configure (or `ump.iface=NAME`) |
| `ump.ip=203.0.113.10/26,203.0.113.1` | IPv4 address with prefix, and the gateway (on-link, so `/32` setups work) |
| `ump.ip6=2001:db8::2/64,fe80::1` | IPv6 address with prefix, and the gateway |
| `ump.dns=10.0.0.53,10.0.0.54` | resolvers; without it 1.1.1.1 and 8.8.8.8, also for DHCP interfaces (the lease's resolvers are ignored) |

Without `ump.ip` and `ump.ip6`, every Ethernet interface uses DHCP and
router advertisements, which is what cloud VMs need. SSH host keys are
generated at every boot. `console=ttyS0,115200n8` (or the provider's
serial console) gives a root shell on the out-of-band console.

## Entering it by hand

`ump server rescue` and `ump server reinstall` do this for you. From a
provider's rescue system or any running Linux, as root, the script alone
does it too (it is published next to the images, so one line is enough):

```console
# curl -fsSL https://files.example.net/rescue/kexec-into-rescue.sh -o /tmp/kexec-into-rescue.sh && bash /tmp/kexec-into-rescue.sh -u https://files.example.net/rescue
```

With ump, from your own machine, that is `ump server rescue web-01`. It
checks whether the server lets UMP in as root over SSH and, if so, runs
the script there right away (no reboot through the provider); otherwise
it puts the server into the provider's rescue system, runs the script
there, and connects once the UMP rescue system answers with its version:

```console
$ ump server rescue web-01
Checking root access to 203.0.113.10 over SSH ... no.
Going through the provider's rescue system.
Rescue system enabled for hetzner-cloud/123 (web-01, 203.0.113.10); rebooting into it.
...
Waiting for SSH on 203.0.113.10 ..... up after 35s.
Provider rescue system reached (Debian GNU/Linux 12 (bookworm)); loading the UMP rescue image from https://ump.zhurko.de/dl/rescue.
==> downloading the rescue image for amd64 from https://ump.zhurko.de/dl/rescue
...
==> booting into the UMP rescue system; reconnect as root@203.0.113.10
Waiting for the UMP rescue system on 203.0.113.10 ... up after 14s (ump-rescue 2026.10.12).
Connecting to the UMP rescue system of web-01 as root; exit the shell to return.
```

ump has the server fetch the script from the base URL, the same place
the image comes from, so the published copy is the one that runs and the
server must reach the base URL (ump says so when it cannot). `--image`
or `staging.image` in the configuration names the base URL, `--no-image`
stops in the provider's rescue system, and `--via direct|provider`
forces one of the two ways.

[`kexec-into-rescue.sh`](kexec-into-rescue.sh) downloads the image for
the machine's architecture, verifies the checksums, takes the current
address, gateway, hostname, `console=` settings and `root`'s
`authorized_keys` (which is where `ump server rescue` put UMP's key) plus any
`-K` keys, loads the image with `kexec` (the published static one, or the
system's), syncs and remounts the file systems of a system that runs
from disk read-only, and boots it with `panic=30`: a kernel that
crashes reboots after 30 seconds instead of hanging, so a server without
a console comes back by itself, with the system on its disk (a
provider's rescue system is armed for one boot only). Reconnect as root
to the same address; the host key is new. `--dry-run` prints what would
be loaded, `--no-exec` loads without booting. When the script runs over
an SSH session, that session dies with the kernel without a proper
close; an `ssh -o ServerAliveInterval=5` client notices in seconds
instead of waiting for the TCP timeout.

## Testing

```console
$ make rescue-test TEST_ARCH=arm64     # default: this machine's architecture
```

boots the built image in QEMU (`-kernel`/`-initrd`, 2 GB, a virtio disk
that must stay unmounted, user networking with SSH forwarded to
`127.0.0.1:2222`), waits for SSH with a throwaway key passed through
`ump.authorized_keys`, and checks the hostname, the root file system,
every tool the installer needs, that the drivers of the common server
NICs and storage controllers load (QEMU only has virtio, and a driver
loads without its hardware), the network, the services and that nothing
failed. Then it does what a provider rescue system will do: inside the
guest it runs `kexec-into-rescue.sh` against an HTTP server on the host,
which downloads and verifies the image, reads the DHCP configuration,
kexecs with it carried over as static configuration, and checks that
the new system came up with that address, gateway and DNS, the same
key and a new boot ID (`KEXEC_TEST=0` skips this). It uses HVF on macOS
and KVM on Linux where the architectures match, and plain emulation
otherwise.

## Memory

Everything lives in RAM, so size is the one thing that limits where the
image boots. A kexec holds the running system, the downloaded files
(on disk when the running system has one, in RAM in a rescue system)
and the kernel's copy of them at the same time; the new kernel then
unpacks the initramfs, whose bulk is the squashfs, which stays
compressed in memory from then on. Until 2026.10.15 the initramfs was
the unpacked system itself (236 MB on amd64), which needed about 600 MB
to boot and panicked on a 512 MB DigitalOcean droplet ("Unable to mount
root fs on unknown-block(0,0)": the initramfs did not fit, so there was
no root). The manifest says how much memory the image needs
(`memory_mb`: twice the initramfs, four times the kernel, and 160 MB
for the system and the tools), and `kexec-into-rescue.sh` stops before
the jump on a machine with less (`--ignore-memory` jumps anyway), so a
machine too small for the image keeps running what it runs.
That is why [`finish.sh`](finish.sh) is so strict about what stays:
no locales (the C.UTF-8 locale is built into glibc), no Python (which
`xfsprogs` would pull in for `xfs_scrub`), no ICU, no device-tree
blobs, no udev hardware database, no sound, media, wireless, InfiniBand
or exotic file system modules. Netfilter stays (since 2026-10-17): the
installer checks the new system's firewall rules with `nft -c`, which
needs the kernel's nf_tables. Debian's arm64 kernel lacks
`kexec_file_load`, so `kexec-into-rescue.sh` falls back to the classic
`kexec_load`, which keeps one more copy in user space while loading; the
image is sized so that this still works with 1 GB of RAM. Measured on
the 2026.10.12 build (the unpacked layout):

| | amd64 | arm64 |
|---|---|---|
| `vmlinuz` | 12 MB (bzImage) | 14 MB (gzip Image) |
| `initrd.zst` | 96 MB | 91 MB |
| unpacked in RAM | 236 MB | 266 MB |
| boot, checks and kexec entry with 1 GB | passes (QEMU) | passes (QEMU) |

The first run on a real machine, a Hetzner Cloud instance with 1 GB
(937 MB visible to the kernel) on 2026-10-12, measured: the provider's
rescue system used 202 MB; with the image loaded through
`kexec_file_load` and the downloads deleted, 369 MB; the UMP rescue
system after boot, 395 MB (253 MB of it the unpacked root file system),
with 542 MB available. SSH answered about ten seconds after `kexec -e`.

Loading the image takes about twice the initramfs and four times the
kernel in free memory, on top of the running system (`kexec_file_load`
reads the files into the kernel and copies them into place). On a small
machine that runs from disk, a 512 MB droplet with its own Ubuntu,
`kexec-into-rescue.sh` adds a swap file on that disk for the load, so
that the running system's memory makes room; a system in RAM has no
disk to swap to and must have the room itself, else the script stops
before the jump.

Measured on the 2026.10.15 build (squashfs layout, amd64, QEMU): the
initramfs is 101 MB, the manifest asks for 412 MB. With 512 MB (463 MB
visible to the kernel) the image boots, passes every check and runs on
an overlay root (185 MB used, 102 MB of it the squashfs). A jump from
there with the download on a disk (`KEXEC_DISK=1 MEM=512
test-boot.sh`) found 274 MB free for a load that needs 316 MB, added a
170 MB swap file, loaded, jumped and came back with the configuration
carried over; without the swap file the kernel's out-of-memory killer
stopped the load (the running system stays as it is then).

## Limitations and next steps

- One static network configuration on one interface; bonds and VLANs
  come with the network model of the installer.
- The image is built from Debian trixie's packages at build time and
  not reproducible bit for bit; the manifest pins what was built.
- Secure Boot is not considered: `kexec` into an unsigned kernel needs
  Secure Boot off, which is how provider rescue systems run anyway.
