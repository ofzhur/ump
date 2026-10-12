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
$ make rescue-image RESCUE_ARCH=arm64
$ make rescue-image RESCUE_ARCH=amd64
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

## Uploading to R2

```console
$ make rescue-upload RESCUE_ARCH=arm64     # builds the image first when out/arm64 is missing
```

[`upload.sh`](upload.sh) puts the files into an S3-compatible bucket
through its S3 API (any S3-compatible store works the same), using the
AWS CLI or rclone, whichever is installed. The settings come from the
environment, so that nothing is written down in the repository:

| Variable | Meaning |
|---|---|
| `STORE_ACCOUNT_ID` | the account ID; the endpoint becomes `https://<id>.storage.example.net` |
| `STORE_ENDPOINT` | the S3 endpoint URL, instead of `STORE_ACCOUNT_ID` |
| `STORE_ACCESS_KEY_ID`, `STORE_SECRET_ACCESS_KEY` | an API token with object read and write on the bucket (from the store's console) |
| `STORE_BUCKET` | the bucket |
| `STORE_PREFIX` | key prefix in the bucket, default `rescue` |
| `DRY_RUN=1` | print what would be uploaded |

Keep them in a file the shell sources, for example `~/.config/ump/r2.env`
with mode 0600, or in `pass`/`op` and export them for the command:

```console
$ set -a; . ~/.config/ump/r2.env; set +a
$ make rescue-upload RESCUE_ARCH=amd64
```

The layout in the bucket is `<prefix>/<arch>/` for the current image
(what the kexec script and `ump` fetch), `<prefix>/v/<version>/<arch>/`
for every version uploaded, kept for rollbacks, and
`<prefix>/kexec-into-rescue.sh`; `manifest.json` goes up last, so a
reader never sees a manifest whose files are not there yet.
Give the bucket a public hostname (an R2 custom domain or `r2.dev`) and
that hostname plus the prefix is the base URL:
`UMP_RESCUE_URL=https://files.example.net/rescue`. This project's images
are published at `https://ump.zhurko.de/rescue`.

The clients of that hostname are rescue systems with `curl`, not
browsers, and they come from datacenter address space. A bot protection in
front of the storage (a JavaScript challenge, a login page) answers
such requests with a web page that no script can pass, so the hostname
needs a rule that lets scripted clients through, and a test from a
server, not from a laptop, before relying on it.

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
| `ump.dns=1.1.1.1,9.9.9.9` | resolvers |

Without `ump.ip` and `ump.ip6`, every Ethernet interface uses DHCP and
router advertisements, which is what cloud VMs need. SSH host keys are
generated at every boot. `console=ttyS0,115200n8` (or the provider's
serial console) gives a root shell on the out-of-band console.

## Entering it by hand

Until `ump install` does this itself, from a provider's rescue system or
any running Linux, as root (the script is published next to the images,
so one line is enough):

```console
# curl -fsSL https://files.example.net/rescue/kexec-into-rescue.sh | bash -s -- -u https://files.example.net/rescue
```

With ump, from your own machine, that is one command, which puts the
server into the provider's rescue system, runs the line there and
returns when the connection drops with the old kernel:

```console
$ ump rescue server web-01 --yes -- 'curl -fsSL https://files.example.net/rescue/kexec-into-rescue.sh | bash -s -- -u https://files.example.net/rescue'
$ ump ssh server web-01 --staging          # a minute later: the UMP rescue system
```

[`kexec-into-rescue.sh`](kexec-into-rescue.sh) downloads the image for
the machine's architecture, verifies the checksums, takes the current
address, gateway, DNS, hostname, `console=` settings and `root`'s
`authorized_keys` (which is where `ump rescue` put UMP's key), loads the
image with `kexec` (the published static one, or the system's) and boots
it. Reconnect as root
to the same address; the host key is new. `--dry-run` prints what would
be loaded, `--no-exec` loads without booting. When the script runs over
an SSH session, that session dies with the kernel without a proper
close; an `ssh -o ServerAliveInterval=5` client notices in seconds
instead of waiting for the TCP timeout.

## Testing

```console
$ make rescue-test RESCUE_ARCH=arm64
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

## Limitations and next steps

- One static network configuration on one interface; bonds and VLANs
  come with the network model of the installer.
- The image is built from Debian trixie's packages at build time and
  not reproducible bit for bit; the manifest pins what was built.
- Secure Boot is not considered: `kexec` into an unsigned kernel needs
  Secure Boot off, which is how provider rescue systems run anyway.
- `ump install` will fetch `manifest.json` from the configured base URL
  (`staging.kexec_image`), verify the files and do what
  `kexec-into-rescue.sh` does, including waiting for the new host key.
