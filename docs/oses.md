# Operating systems

ump installs prebuilt operating systems, made for servers and the same
on every machine: a dedicated server at Hetzner, a cloud instance at
Vultr or a box in your rack ends up with the same system, the same disk
layout, the same users, network names, firewall and tools. They are
published on the storage for amd64 and arm64, boot from UEFI and, on
amd64, from BIOS, and are meant to be used as they are: pick one, and
the machine is a well-kept server a few minutes later.

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
| `nixos-25.11` | NixOS 25.11, from its stable channel | `nixos-25.11` | `nixos-install`, with the profile as NixOS modules |

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

A complete Ubuntu LTS server system as UMP builds it: the generic kernel
with its firmware, GRUB, LVM, md, netplan with systemd-networkd, OpenSSH,
`tuned` for throughput, and the tools above, with no cloud-init, no
snaps and no recommends. The three releases are built the same way and
differ only in their packages' versions. Updates come from Ubuntu as
usual (`apt`, `unattended-upgrades` if you add it). See [the Ubuntu
base image](../images/ubuntu/README.md) for what it holds and its
defaults.

## NixOS

NixOS 25.11 from its stable channel (fixes, no new major versions),
installed by `nixos-install`. The profile becomes NixOS configuration:
UMP's module in `/etc/nixos/ump` with the users, network, firewall and
boot loader, and the userdata as NixOS modules merged with it, so the
machine is what `/etc/nixos` says and `nixos-rebuild` manages it from
then on. See [NixOS](nixos.md).

## More systems

Debian is next; systems that take a declarative configuration (Talos,
cloud-init images) may follow as further userdata types. An image of
your own works too: the [storage layout](../images/rescue/README.md#your-own-storage)
says what an `os/<name>/<arch>/` directory must hold, and any `os` name
that follows it can be installed.
