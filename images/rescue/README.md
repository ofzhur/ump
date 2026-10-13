# The UMP rescue image

Every provider has a rescue system, and every one is different: another
base distribution, other tools, other quirks. UMP uses the provider's
rescue system for one thing only, to get root over SSH once; then it
downloads **this** image and `kexec`s into it, so that the inspection and
installation steps run in the same environment everywhere: a Debian
system with a generic kernel, the firmware of server NICs and storage
controllers, and the tools the UMP agent calls, running entirely from
RAM with no disk mounted. The same image serves the `kexec` entry method
on clouds without a rescue system, and later virtual media and PXE.

The image is one kernel plus one initramfs per architecture (amd64,
arm64). The initramfs **is** the whole system: the kernel starts
`/init`, which is systemd, and nothing is ever switched to. There is
no squashfs to fetch and nothing to mount.

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
$ make upload                              # every built image (rescue and OS) and the scripts
$ images/upload.sh rescue                  # one kind: rescue, os or scripts
```

[`images/upload.sh`](../upload.sh) puts the files into an S3-compatible bucket
through its S3 API (any S3-compatible store works the same), using the
AWS CLI or rclone, whichever is installed. The settings come from the
environment, so that nothing is written down in the repository:

| Variable | Meaning |
|---|---|
| `STORE_ACCOUNT_ID` | the account ID; the endpoint becomes `https://<id>.storage.example.net` |
| `STORE_ENDPOINT` | the S3 endpoint URL, instead of `STORE_ACCOUNT_ID` |
| `STORE_ACCESS_KEY_ID`, `STORE_SECRET_ACCESS_KEY` | an API token with object read and write on the bucket (from the store's console) |
| `STORE_BUCKET` | the bucket |
| `STORE_PREFIX` | key prefix in the bucket, default none (the bucket root is the storage) |
| `DRY_RUN=1` | print what would be uploaded |

Keep them in a file the shell sources, for example `~/.config/ump/r2.env`
with mode 0600, or in `pass`/`op` and export them for the command:

```console
$ set -a; . ~/.config/ump/r2.env; set +a
$ make upload
```

The layout in the bucket is the storage layout below; per image,
`manifest.json` goes up last, so a reader never sees a manifest whose
files are not there yet, and every version uploaded is kept under `v/`
for rollbacks. Give the bucket a public hostname (an R2 custom domain or
`r2.dev`) and that hostname is the storage, `storage` in the ump
configuration; this project's is `https://ump.zhurko.de`, ump's default,
and the rescue image's base URL is `<storage>/rescue`.

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
| `rescue/<arch>/initrd.zst` | the rescue system, as a zstd-compressed initramfs |
| `rescue/<arch>/kexec` | a static `kexec` binary, used where the rescue system has none (optional, listed in the manifest when present) |
| `install/install.sh` | [the installer](../../install/README.md); ump has the rescue system fetch and run it |
| `profiles/default.yaml`, `profiles/layouts.yaml`, `profiles/firewalls.yaml` | the published [profile, layouts and firewalls](../../profiles/README.md); a copy in `~/.config/ump/profiles/` wins |
| `os/<os>/<arch>/manifest.json` | an [OS image](../ubuntu/README.md): version, kernel, unpacked size, and the SHA-256 and size of the archive |
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
browsers, and they come from datacenter address space. A bot protection in
front of the storage (a JavaScript challenge, a login page) answers
such requests with a web page that no script can pass, so the hostname
needs a rule that lets scripted clients through, and a test from a
server, not from a laptop, before relying on it. Blocked, the entry
script stops at its first download with HTTP 403 and boots nothing, so
a later `ump ssh --staging` lands in the provider's rescue system, not
in UMP's; the `-o file &&` form of the one-liner below fails the same
way instead of feeding an empty download to `bash`.

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
belongs on a server rescue system that lives in RAM.

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

Until `ump server reinstall` does this itself, from a provider's rescue system or
any running Linux, as root (the script is published next to the images,
so one line is enough):

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
Provider rescue system reached (Debian GNU/Linux 12 (bookworm)); loading the UMP rescue image from https://ump.zhurko.de/rescue.
==> downloading the rescue image for amd64 from https://ump.zhurko.de/rescue
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
from disk read-only, and boots it. Reconnect as root
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
every tool the agent needs, the network, the services and that nothing
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
image boots. A kexec from a RAM-based rescue system holds the running
system, the downloaded files and the kernel's copy of them at the same
time, and the new kernel then unpacks the whole initramfs into memory.
That is why [`finish.sh`](finish.sh) is so strict about what stays:
no locales (the C.UTF-8 locale is built into glibc), no Python (which
`xfsprogs` would pull in for `xfs_scrub`), no ICU, no device-tree
blobs, no udev hardware database, no sound, media, wireless, InfiniBand,
netfilter or exotic file system modules. Debian's arm64 kernel lacks
`kexec_file_load`, so `kexec-into-rescue.sh` falls back to the classic
`kexec_load`, which keeps one more copy in user space while loading; the
image is sized so that this still works with 1 GB of RAM, the smallest
cloud instance class. Measured on the 2026.10.12 build:

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

## Limitations and next steps

- One static network configuration on one interface; bonds and VLANs
  come with the network model of the installer.
- The image is built from Debian trixie's packages at build time and
  not reproducible bit for bit; the manifest pins what was built.
- Secure Boot is not considered: `kexec` into an unsigned kernel needs
  Secure Boot off, which is how provider rescue systems run anyway.
- `ump server reinstall` will fetch `manifest.json` from the configured base URL
  (`staging.kexec_image`), verify the files and do what
  `kexec-into-rescue.sh` does, including waiting for the new host key.
