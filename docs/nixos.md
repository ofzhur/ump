# NixOS

ump installs NixOS from its stable channel with the same profiles,
layouts, users, firewalls and keys as Ubuntu. What differs is how the
result is held: on Ubuntu ump configures the unpacked image in place; on
NixOS everything becomes NixOS configuration, so the machine is what
`/etc/nixos` says, and `nixos-rebuild` keeps managing it afterwards.

```console
$ ump server reinstall web-01 --profile nixos-25.11
```

## What is installed

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

## How the configuration is put together

```
/etc/nixos/
├── configuration.nix          imports the three below; yours to edit
├── hardware-configuration.nix written by nixos-generate-config (file systems, kernel modules)
├── ump/
│   ├── default.nix            UMP's module (install/nixos/ump.nix of the storage)
│   ├── spec.json              the resolved profile
│   ├── facts.json             what the installer found on the machine
│   └── nftables.conf          the firewall ruleset
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
same list.

## Userdata on NixOS

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

## How the installation runs

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

## The image on the storage

NixOS has no image to unpack, only a manifest: `os/nixos-25.11/<arch>/
manifest.json` with `"family": "nixos"`, the channel, the state version
and the Nix release tarball with its checksum.
[`images/nixos/build.sh`](../images/nixos/build.sh) writes it (taking
the Nix version and checksum from the official Nix installer), and
`images/upload.sh os` publishes it with the Ubuntu images; UMP's module
goes up with the installer (`images/upload.sh scripts`).
[`images/nixos/test-install.sh`](../images/nixos/test-install.sh)
installs NixOS in QEMU from the UMP rescue image and checks the result.
