# Operating systems

ump gives you server operating systems out of the box, the same on
every machine: a dedicated server at Hetzner, a cloud instance at Vultr
or a box in your rack ends up with the same system, the same disk
layout, the same users, network names, firewall and tools. Ubuntu and
Debian come as UMP's own prebuilt images from its storage; NixOS is installed from
NixOS's public servers with UMP's configuration on top, and Arch Linux,
a rolling release, from its mirrors with the packages of the day, as is
CachyOS, Arch with packages built for newer CPUs (experimental). They work on amd64 and
arm64 (Arch Linux and CachyOS on amd64 only), boot from UEFI and,
on amd64, from BIOS, and are meant to be used as they are: pick one, and
the machine is a well-kept server a few minutes later.
How they compare on the same hardware, and how long each takes to
install: [benchmarks](benchmarks.md).

```console
$ ump server reinstall web-01                          # the default: Ubuntu 26.04
$ ump server reinstall web-01 --profile debian-13      # Debian
$ ump server reinstall web-01 --profile nixos-25.11    # NixOS
$ ump server reinstall web-01 --profile archlinux      # Arch Linux
$ ump server reinstall web-01 --profile cachyos        # CachyOS
$ ump server reinstall web-01 --os ubuntu-24.04        # another Ubuntu, same profile
```

| `os` | System | Published profile | How it is installed |
|---|---|---|---|
| `ubuntu-26.04` | Ubuntu 26.04 LTS (resolute), the default | `ubuntu-26.04` | UMP's image, unpacked and configured in place |
| `ubuntu-24.04` | Ubuntu 24.04 LTS (noble) | `ubuntu-24.04` | the same |
| `ubuntu-22.04` | Ubuntu 22.04 LTS (jammy) | `ubuntu-22.04` | the same |
| `debian-13` | Debian 13 (trixie) | `debian-13` | the same |
| `debian-12` | Debian 12 (bookworm) | `debian-12` | the same |
| `nixos-25.11` | NixOS 25.11, from its stable channel | `nixos-25.11` | `nixos-install` from NixOS's public servers, with the profile as NixOS modules |
| `archlinux` | Arch Linux, the rolling release, amd64 only | `archlinux` | `pacstrap` from the Arch mirrors, then configured in place like Ubuntu |
| `cachyos` | CachyOS, experimental: Arch Linux with packages built for x86-64-v3 and CachyOS's server kernel, rolling, amd64 only | `cachyos` | the same as Arch Linux, with CachyOS's repositories in front |

`ump config os create` lists what the storage offers (its
`os/index.json`), and [profiles](profiles.md#choosing-the-operating-system) says how a
profile, a server's own setting or `--os` picks one.

## What every system gets

Whichever system it is, ump installs it the same way, from the same
[profile](profiles.md), from UMP's own rescue system:

- **Disks** as the [layout](disk-layouts.md) says: partitions, md
  arrays, LVM with linear, striped or mirrored volumes, on whatever
  disks the machine has.
- **Network** with uniform interface names: `ext01`, ... for the public
  interfaces, `int01`, ... for the rest, bound to their MAC addresses,
  and the uplink configured statically with the addresses the machine
  had.
- **Users** from the profile with their SSH keys, the administrator
  (`ump` by default) with sudo and no password set, no root login over
  SSH unless the profile lists root, no password logins at all.
- **A firewall** with an input policy and the allowed ports, which also
  holds the ports containers publish to the same list.
- **Kernel switches:** CPU vulnerability mitigations and IPv6 off unless
  the profile turns them on, both for performance.
- **Tools an operator wants:** `htop`, `atop`, `iotop`, `sar`, `tcpdump`,
  `mtr`, `dig`, `lsof`, `strace`, `jq`, `vim`, `tmux`, the disk and
  hardware tools, the same bash prompt and aliases, and a message of the
  day with UMP's banner, the host,
  its installation, load, memory, disk and addresses (`ump-motd`).
- **Userdata:** what a system of yours needs on top: a script on Ubuntu,
  Debian, Arch Linux and CachyOS, a NixOS module on NixOS ([userdata](userdata.md)).
- **A record** of what was installed, from which profile and layout, in
  `/etc/ump/install`, which `ump server show` reads back.

Measured on 2026-10-17 with `ump server reinstall`, from the running system
to the new one answering: Arch Linux in 95 s on a Hetzner Cloud server
and 159 s on a Linode nanode; CachyOS in 80 to 97 s on Hetzner Cloud,
100 s on OVHcloud, 158 s on Linode and 160 s on Vultr.

## Ubuntu

A complete Ubuntu LTS server system, built by UMP and served from its
storage as one archive per release and architecture: 26.04 (resolute,
the default), 24.04 (noble) and 22.04 (jammy), built the same way and
differing only in their packages' versions. The installer unpacks it
and configures what depends on the machine. Out of the box:

- **Base:** the generic kernel with its firmware and, on amd64, the CPU
  microcode of Intel and AMD (applied from the first boot), GRUB for
  UEFI and, on amd64, BIOS, LVM, md, the file system tools, netplan rendering to
  systemd-networkd, `systemd-resolved`, OpenSSH. No cloud-init, no
  snaps, no recommends.
- **Performance:** `tuned` with the `throughput-performance` profile and
  the `performance` CPU governor; mitigations and IPv6 off on the kernel
  command line unless the profile turns them on
  (`/etc/default/grub.d/ump-cmdline.cfg`, with how to change it).
- **History:** `last` from the first boot, `sar` every ten minutes
  (`sysstat`), per-process history with `atop -r`.
- **Shell:** bash with a colored prompt (red for root), completion and
  the color aliases, the same on every system ([`install/bashrc.sh`](../install/bashrc.sh)).
- **SSH:** keys only, no root login unless the profile lists root
  (`/etc/ssh/sshd_config.d/50-ump.conf`, `10-ump-root.conf`).
- **Network:** `/etc/netplan/10-ump.yaml` with the interfaces matched by
  MAC address and renamed, the uplink static, resolvers 1.1.1.1 and
  8.8.8.8.
- **Firewall:** `/etc/nftables.conf`, loaded at boot by `nftables`.
- **Userdata:** bash scripts, run as root in the new system before its
  first boot ([userdata](userdata.md)).

Updates come from Ubuntu as usual (`apt`). [The Ubuntu base
image](../images/base/README.md) has the full package list, every
default, and how the image is built and published.

## Debian

Debian 13 (trixie) and 12 (bookworm), built by UMP exactly like the
Ubuntu image: the same script, package list and defaults, from
`deb.debian.org`, so a Debian machine has the same tools, network
configuration (netplan with systemd-networkd), firewall, SSH policy,
shell, performance settings and message of the day as an Ubuntu one.
What differs is Debian's own: its kernel (`linux-image-amd64` or
`arm64`) with the firmware of server NICs and storage controllers from
the `firmware-*` packages and, on amd64, `intel-microcode` and
`amd64-microcode` (Debian's kernel does not pull them in as Ubuntu's
does), its apt sources (`deb.debian.org` with
updates, `security.debian.org`), and the age of its packages. Debian 12
has no `wtmpdb` (`last` reads the classic `wtmp`); Debian 13 has no
`cpufrequtils` (`tuned` sets the CPU's performance settings). Userdata
is bash, as on Ubuntu; the published entry is `debian`. See [the base
images](../images/base/README.md).

## NixOS

ump installs NixOS from its stable channel with the same profiles,
layouts, users, firewalls and keys as Ubuntu. What differs is how the
result is held: on Ubuntu ump configures the unpacked image in place; on
NixOS everything becomes NixOS configuration, so the machine is what
`/etc/nixos` says, and `nixos-rebuild` keeps managing it afterwards.

```console
$ ump server reinstall web-01 --profile nixos-25.11
```

### What is installed

The published profile [`nixos-25.11`](../profiles/nixos-25.11.yaml) is
the Ubuntu profile with `os: nixos-25.11` and the userdata `nixos`:
every disk as one linear volume group, the sudo user `ump` with your
keys, no root over SSH, ICMP and SSH open. Any other layout, firewall
or user works the same way as on Ubuntu.

NixOS comes from the channel `nixos-25.11`. That channel follows the
25.11 release: it gets security and bug fixes but no new major versions
of anything, and it advances only to commits NixOS's own build farm has
tested, so its packages are in the binary cache. ump installs whatever
the channel points to at installation time and records the commit in
`/etc/ump/install` (`image_version=... (nixpkgs <commit>)`); the
installed system keeps the channel, so `nixos-rebuild switch --upgrade`
moves it along later, when you choose to.

### How the configuration is put together

```
/etc/nixos/
├── configuration.nix          imports the three below; yours to edit
├── hardware-configuration.nix written by nixos-generate-config (file systems, kernel modules)
├── ump/
│   ├── default.nix            UMP's module (install/nixos/ump.nix of the storage)
│   ├── spec.json              the resolved profile
│   ├── facts.json             what the installer found on the machine
│   ├── nftables.conf          the firewall ruleset
│   └── motd.sh                the message of the day, the same script as on Ubuntu
└── userdata/
    ├── 01-nixos.nix           the profile's userdata, as NixOS modules, in order
    └── 02-web.nix
```

UMP's module is one fixed file, the same for every machine: it reads
`spec.json` (users and keys, time zone, kernel switches, firewall,
interface naming) and `facts.json` (the interfaces with their MAC
addresses, the uplink's addresses and gateway, the boot disks, md
arrays, swap volumes) and turns them into NixOS options: the hostname,
systemd-networkd with the interfaces renamed to `ext01`, `int01`, ... by
MAC address and a static uplink, the users with their keys and sudo
without a password, sshd, GRUB for UEFI and BIOS (the EFI binary on the
removable path, so no firmware variable is written), md and LVM in the
initrd, the swap volumes of the layout, the `performance` CPU governor
(what `tuned` sets on Ubuntu; NixOS's default is `powersave`), the
analysis tools UMP puts on every server, and the firewall: the same nftables ruleset UMP writes on
Ubuntu, including the chain that holds ports containers publish to the
same list. The message of the day is the same too: UMP's banner with the
host, the installation, uptime and load, memory, the root disk and the
addresses, from [`install/motd.sh`](../install/motd.sh), shown at
interactive SSH logins and by `ump-motd`.

### Userdata on NixOS

On NixOS, userdata is a NixOS module, type `nix`:

```yaml
# userdata.yaml
userdata:
  web:
    type: nix
    script: |
      { pkgs, ... }: {
        services.nginx.enable = true;
        environment.systemPackages = [ pkgs.certbot ];
      }
```

The installer writes each entry to `/etc/nixos/userdata/NN-NAME.nix`,
and `configuration.nix` imports every file there, so NixOS merges them
with UMP's module the way it merges any modules:

- **Lists add up:** packages, kernel parameters, groups, extra users.
- **UMP's defaults yield:** the time zone, IPv6, the state version, Nix's
  experimental features and the absence of NixOS's own firewall are set
  with `lib.mkDefault`, so a userdata module that sets them wins.
- **The rest holds:** the administrator and its keys, sshd and its
  policy, the network, the boot loader and the firewall ruleset are set
  plainly; a module changes them only with `lib.mkForce`, on purpose.

Ports for a service go into the profile's firewall (`ump config
firewall`), not into the module: the ruleset is UMP's, the same on
every system. A profile for NixOS takes `nix` userdata only, and a
profile for Ubuntu `bash` userdata only; ump refuses a mix before it
touches the machine. The published entry `nixos` does nothing yet, like
`ubuntu`: it is the place for what every NixOS of yours should get.

### How the installation runs

From the UMP rescue system, after the disks are partitioned, assembled,
formatted and mounted as on Ubuntu:

1. Nix comes from its release tarball (the manifest names version and
   checksum), with its store bind-mounted from the new root, so nothing
   of NixOS is held in the rescue system's memory.
2. The channel is added and fetched, and `nixos-install` comes from it.
3. `nixos-generate-config` writes the hardware configuration; the
   installer adds UMP's module with its data, the userdata modules and
   `configuration.nix`.
4. Evaluating a NixOS configuration takes about a gigabyte of memory, so
   on a smaller machine a swap file on the new root makes room while
   `nixos-install` runs (it is removed afterwards).
5. `nixos-install` builds the system from the binary cache (about a
   gigabyte of downloads) and installs GRUB; its whole output is in
   `/var/log/ump-nixos-install.log` of the new system.

Then the machine reboots into NixOS, and ump logs in as the
administrator as after any installation. Machines with 1 GB of memory
and more work; the downloads take a few minutes.

### What the storage holds for NixOS

No prebuilt image: UMP's storage holds only the manifest
(`os/nixos-25.11/<arch>/manifest.json`: `"family": "nixos"`, the
channel, the state version and the Nix release with its checksum) and
UMP's module (`install/nixos/ump.nix`, published with the installer).
Everything else comes from NixOS's own public servers at installation:
Nix from `releases.nixos.org`, nixpkgs from `channels.nixos.org`, every
package from the binary cache `cache.nixos.org` (about a gigabyte), so
an installation is always the current state of the stable channel and
nothing on UMP's side goes stale. [`images/nixos/build.sh`](../images/nixos/build.sh)
writes the manifest (the Nix version and checksum from the official Nix
installer), `images/upload.sh os` publishes it, and
[`images/test-install.sh`](../images/test-install.sh) (`images/test-install.sh nixos-25.11`)
installs NixOS in QEMU from the UMP rescue image and checks the result.

## Arch Linux

Arch Linux is a rolling release: there are no versions to pick, and an
image would be out of date within weeks. So ump installs it the way
Arch's own installation guide does, at the time of the installation:

1. The monthly **bootstrap tarball** from `archive.archlinux.org`, whose
   URL and checksum the manifest pins, is unpacked onto the new disk
   (not into the rescue system's memory, so 1 GB machines work).
2. In it, the **pacman keyring** is set up and brought up to date, so
   that the packages of the day verify; pacman checks every package's
   signature.
3. **`pacstrap`** installs the manifest's packages from the mirror
   (`geo.mirror.pkgbuild.com`, Arch's CDN) as they are that day: the
   counterpart of the Ubuntu image's package list, from `base`, `linux`
   and `linux-firmware` to the same operator tools
   ([`images/arch/packages.txt`](../images/arch/packages.txt)).
4. From there the system is **configured in place like Ubuntu**: the
   same fstab, users and keys, SSH policy, firewall, shell, message of
   the day and userdata.

What is Arch's own:

- **Network:** systemd-networkd directly, without netplan: `.link`
  files in `/etc/systemd/network/` give the interfaces their names by
  MAC address, `20-ump-ext01.network` has the uplink's static
  addresses; `systemd-resolved` with resolvers 1.1.1.1 and 8.8.8.8.
- **Boot:** `mkinitcpio` with the `lvm2` hook (and `mdadm_udev` for md
  arrays), GRUB for UEFI and BIOS, the CPU microcode of Intel and AMD;
  the kernel command line in `/etc/default/grub` (`grub-mkconfig -o
  /boot/grub/grub.cfg` after a change).
- **Administrators** are in `wheel`, with their own sudoers file as
  elsewhere.
- **Message of the day:** Arch has no `update-motd.d`, so
  `/etc/profile.d/ump-motd.sh` shows UMP's banner at an SSH login, as on
  NixOS.
- **The installation record** says `rolling, packages of DATE` as the
  version, and the kernel installed (`7.2.10-arch1-1`).
- **Firewall:** `nftables.service` loads `/etc/nftables.conf` at boot
  and then shows as inactive (Arch's unit does not stay active, unlike
  Debian's); `nft list ruleset` shows the rules in force.
- **Logs:** what pacstrap did is in `/var/log/ump-pacstrap.log`.

Updates are Arch's: `pacman -Syu`, regularly (a rolling release is
meant to be kept current). Userdata is bash, as on Ubuntu; the published
entry is `archlinux` (`pacman -S --noconfirm --needed ...` installs
packages). Arch Linux is built for x86_64 only (Arch Linux ARM is a
separate project), so `ump server reinstall` refuses it on an arm64
machine before anything happens to the machine: not after a reboot into
the rescue system or, on a provider without one, after the provider
reinstalled it. The architecture comes from the provider (Hetzner Cloud
says it; Linode and DigitalOcean have x86 only; Hetzner's RX servers are
ARM) or from the running system over SSH; when neither tells, ump warns
and asks whether the machine is amd64. The same holds for any OS the
storage has no image of for an architecture.

The manifest is written by [`images/arch/build.sh`](../images/arch/build.sh):
the newest bootstrap release, its checksum, the mirror, and the package
list, each package checked against the Arch package database.

## CachyOS

**Experimental.** We do not recommend CachyOS for production unless you
know what you are doing and can reinstall the machine when an update
goes wrong:

- It is a rolling release with two sets of repositories at once:
  CachyOS's rebuilds of core packages (glibc, systemd, pacman, the
  compiler) replace Arch's, so an update depends on two projects
  staying in step.
- It is a small project's own repositories, kernel and keys, with no
  long-term support release and no server edition yet.
- The installer and the profile are new in UMP and tested on fewer
  providers than the others.

It is a good fit for performance testing and for machines you can
rebuild from their profile at any time; on an i9-13900 dedicated server it was,
with Arch Linux, the fastest of the published systems on HTTP. For a
server that has to keep running, take Ubuntu, Debian or NixOS, or Arch
Linux if you want a rolling release.

[CachyOS](https://cachyos.org) is Arch Linux with repositories of its
own in front of Arch's: Arch's packages rebuilt for newer CPUs
(x86-64-v3, which needs AVX2: Intel since Haswell, AMD since Zen; the
project reports 5 to 20 % more speed than the plain x86-64 builds), its
own kernels and pacman. ump installs it as it installs Arch Linux, with
one step more, so everything said about [Arch Linux](#arch-linux) holds
(network, boot, firewall, updates with `pacman -Syu`, amd64 only):

1. The same **bootstrap tarball** and pacman keyring as Arch.
2. **The CPU level:** the bootstrap's glibc tells which levels the
   machine runs (`/usr/lib/ld-linux-x86-64.so.2 --help`). With
   x86-64-v3, CachyOS's `cachyos-v3`, `cachyos-core-v3` and
   `cachyos-extra-v3` repositories come first; without it, only the
   plain x86_64 `cachyos` repository (its kernel and tools) and Arch's
   packages. A virtual machine that hides AVX2 from its guest gets the
   latter; Hetzner Cloud, Linode, OVHcloud and Vultr all show
   x86-64-v3.
3. **CachyOS's keyring:** the `cachyos-keyring` package, pinned by its
   checksum in the manifest as the bootstrap is, makes pacman trust the
   keys CachyOS signs its packages with.
4. **`pacstrap`** installs Arch's package list with CachyOS's kernel,
   `linux-cachyos-server` (CachyOS's variant for servers: 300 Hz, no
   full preemption), in place of Arch's `linux`, and CachyOS's keyring,
   mirror lists and hooks
   ([`images/arch/cachyos-packages.txt`](../images/arch/cachyos-packages.txt)).
   Where CachyOS has a package (glibc, systemd, python, pacman itself),
   theirs wins.
5. The new system's `/etc/pacman.conf` lists CachyOS's repositories in
   front of Arch's, from CachyOS's mirror lists, with `Architecture =
   auto`, as CachyOS's own installer writes it.

The result reports itself as CachyOS (`/etc/os-release`), the
installation record names the repositories it took (`rolling, packages
of DATE, CachyOS repositories x86-64-v3`). CachyOS's desktop settings
(`cachyos-settings`: zram, NetworkManager, audio) are left out; the
tuning is UMP's (`tuned` with `throughput-performance`). The userdata
entry is `cachyos`.

Not x86-64-v4: those packages need AVX-512, gain little over v3 on most
server work, and a virtual machine moved to a host without AVX-512 would
no longer start its programs. The manifest's `cachyos.levels` lists
the levels the installer may take, newest first.

CachyOS is planning a Server Edition (hardened, pre-tuned); until it
is out, this is CachyOS's packages with UMP's server setup. The
manifest takes CachyOS's packages from `mirror.cachyos.org`, not their
CDN (`cdn77.cachyos.org`), which answers a `+` in a file name
(`glibc-2.44+r50...`) with 404 to Arch's pacman. CachyOS's own pacman, on
the installed system, uses their mirror lists.

## More systems

Systems that take a declarative configuration (Talos,
cloud-init images) may follow as further userdata types. An image of
your own works too: the [storage layout](../images/rescue/README.md#your-own-storage)
says what an `os/<name>/<arch>/` directory must hold, and any `os` name
that follows it can be installed.
