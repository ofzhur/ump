# Operating systems

ump gives you server operating systems out of the box, the same on
every machine: a dedicated server at Hetzner, a cloud instance at Vultr
or a box in your rack ends up with the same system, the same disk
layout, the same users, network names, firewall and tools. Ubuntu comes
as UMP's own prebuilt image from its storage; NixOS is installed from
NixOS's public servers with UMP's configuration on top. Both work on
amd64 and arm64, boot from UEFI and, on amd64, from BIOS, and are meant
to be used as they are: pick one, and the machine is a well-kept server
a few minutes later.

```console
$ ump server reinstall web-01                          # the default: Ubuntu 26.04
$ ump server reinstall web-01 --profile nixos-25.11    # NixOS
$ ump server reinstall web-01 --os ubuntu-24.04        # another Ubuntu, same profile
```

| `os` | System | Published profile | How it is installed |
|---|---|---|---|
| `ubuntu-26.04` | Ubuntu 26.04 LTS (resolute), the default | `ubuntu-26.04` | UMP's image, unpacked and configured in place |
| `ubuntu-24.04` | Ubuntu 24.04 LTS (noble) | `ubuntu-24.04` | the same |
| `ubuntu-22.04` | Ubuntu 22.04 LTS (jammy) | `ubuntu-22.04` | the same |
| `nixos-25.11` | NixOS 25.11, from its stable channel | `nixos-25.11` | `nixos-install` from NixOS's public servers, with the profile as NixOS modules |

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
  hardware tools, and a message of the day with UMP's banner, the host,
  its installation, load, memory, disk and addresses (`ump-motd`).
- **Userdata:** what a system of yours needs on top: a script on Ubuntu,
  a NixOS module on NixOS ([userdata](userdata.md)).
- **A record** of what was installed, from which profile and layout, in
  `/etc/ump/install`, which `ump server show` reads back.

## Ubuntu

A complete Ubuntu LTS server system, built by UMP and served from its
storage as one archive per release and architecture: 26.04 (resolute,
the default), 24.04 (noble) and 22.04 (jammy), built the same way and
differing only in their packages' versions. The installer unpacks it
and configures what depends on the machine. Out of the box:

- **Base:** the generic kernel with its firmware, GRUB for UEFI and, on
  amd64, BIOS, LVM, md, the file system tools, netplan rendering to
  systemd-networkd, `systemd-resolved`, OpenSSH. No cloud-init, no
  snaps, no recommends.
- **Performance:** `tuned` with the `throughput-performance` profile and
  the `performance` CPU governor; mitigations and IPv6 off on the kernel
  command line unless the profile turns them on
  (`/etc/default/grub.d/ump-cmdline.cfg`, with how to change it).
- **History:** `last` from the first boot, `sar` every ten minutes
  (`sysstat`), per-process history with `atop -r`.
- **Shell:** bash with a colored prompt (red for root) and completion.
- **SSH:** keys only, no root login unless the profile lists root
  (`/etc/ssh/sshd_config.d/50-ump.conf`, `10-ump-root.conf`).
- **Network:** `/etc/netplan/10-ump.yaml` with the interfaces matched by
  MAC address and renamed, the uplink static, resolvers 1.1.1.1 and
  8.8.8.8.
- **Firewall:** `/etc/nftables.conf`, loaded at boot by `nftables`.
- **Userdata:** bash scripts, run as root in the new system before its
  first boot ([userdata](userdata.md)).

Updates come from Ubuntu as usual (`apt`). [The Ubuntu base
image](../images/ubuntu/README.md) has the full package list, every
default, and how the image is built and published.

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
initrd, the swap volumes of the layout, the analysis tools UMP puts on
every server, and the firewall: the same nftables ruleset UMP writes on
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
[`images/nixos/test-install.sh`](../images/nixos/test-install.sh)
installs NixOS in QEMU from the UMP rescue image and checks the result.

## More systems

Debian is next; systems that take a declarative configuration (Talos,
cloud-init images) may follow as further userdata types. An image of
your own works too: the [storage layout](../images/rescue/README.md#your-own-storage)
says what an `os/<name>/<arch>/` directory must hold, and any `os` name
that follows it can be installed.
