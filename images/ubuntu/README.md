# The UMP Ubuntu base image

A complete Ubuntu 26.04 LTS server system as a root file system archive,
with the generic kernel and its firmware, LVM, netplan, SSH and the tools
a server needs, plus the UMP defaults that do not depend on the machine.
[`ump install`](../../docs/cli.md#install-ump-install-server) unpacks it
onto a machine's disks from the UMP rescue system and
[`install/install.sh`](../../install/README.md) does what depends on the
machine. One archive per architecture, `amd64` and `arm64`.

## Building

```console
$ make os-image                      # images/ubuntu/out/amd64/ and out/arm64/
$ make os-image OS_ARCH=arm64        # one architecture
```

[`build.sh`](build.sh) runs as root in a privileged `debian:trixie`
container (the Makefile starts it with Docker or Podman): `mmdebstrap`
bootstraps Ubuntu `resolute` from the archive with the packages of
[`packages.txt`](packages.txt), the kernel and the boot loader packages of
the architecture, copies [`overlay/`](overlay) in and runs
[`finish.sh`](finish.sh) inside. The other architecture is built from the
same container through `qemu-user-static`, which takes several times as
long. The result:

| File | What it is |
|---|---|
| `rootfs.tar.zst` | the system, a tar with numeric owners and extended attributes, zstd-compressed |
| `manifest.json` | version (the build date), os, suite, arch, kernel, unpacked size, and the SHA-256 and size of the archive |
| `packages.txt` | every package and version in it |
| `SHA256SUMS` | the checksum again, for people |

## What is in it

Ubuntu's `linux-image-generic` with `linux-firmware`, GRUB for UEFI and,
on amd64, for BIOS (`grub-efi-*-bin`, `grub-pc-bin`, `grub2-common`),
`lvm2`, `mdadm`, the file system tools, `netplan.io` rendering to
systemd-networkd, `systemd-resolved`, OpenSSH, `tuned`, `cpufrequtils`,
`numactl`, `smartmontools`, `nvme-cli`, the hardware tools (`pciutils`,
`dmidecode`, `lshw`), and `vim`, `tmux`, `htop`, `git`, `jq` and the
usual shell tools. No cloud-init, no snaps, no recommends. The full list
is in `packages.txt` of a build.

## Defaults

- **Resolvers:** 1.1.1.1 and 8.8.8.8, in `systemd-resolved` and in the
  netplan configuration the installer writes. Replace both to use your
  own.
- **Performance:** `tuned` is enabled with the `throughput-performance`
  profile, and `cpufrequtils` sets the `performance` governor
  (`/etc/default/cpufrequtils`).
- **Kernel command line:** the installer writes
  `/etc/default/grub.d/ump-cmdline.cfg` with `mitigations=off` and
  `ipv6.disable=1`, because both cost performance and most servers need
  neither, and `kho=off`, because the kernel's kexec handover would
  reserve a large part of the memory for live updates that no server
  does, and stop ump from re-entering its rescue system. To turn one
  back on, remove it from that file, run
  `update-grub`, for IPv6 add the address and route to
  `/etc/netplan/10-ump.yaml` (`netplan apply`), and reboot.
  `ump install --mitigations` and `--ipv6` keep them on from the start.
- **SSH:** root logs in with keys only; no passwords.
- **Boot loader:** a 2 s menu on the console, no OS prober; the kernel
  command line file above is read by `update-grub`.
- Time zone UTC (`ump install --timezone`), locale `C.UTF-8`, no swap.
- Every installation gets its own machine id and SSH host keys.

## Publishing

`make upload` publishes every built image to `os/<os>/<arch>/` of the
storage, next to the rescue image; see [the storage
layout](../rescue/README.md#your-own-storage).

## Limitations

- No Secure Boot: the boot loader is the unsigned GRUB on the removable
  path; machines with Secure Boot on need it off.
- One layout, all disks in one linear volume group, and no swap; more
  layouts and a benchmarking round come next.
