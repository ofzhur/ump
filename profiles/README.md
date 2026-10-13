# Profiles

A profile says what a server becomes when `ump server reinstall` installs it:
which image, which disk layout, which users with which keys, which extra
packages, what the firewall lets in, and what happens after the first
boot. It is YAML, and the one in this directory,
[`default.yaml`](default.yaml), is the default. `ump server reinstall NAME`
takes it unless `--profile` names another one.

Where ump looks for a profile named `NAME`: `--profile` with a path
takes that file; else `~/.config/ump/profiles/NAME.yaml` (the profiles
directory next to the configuration); else `profiles/NAME.yaml` on the
storage, where `make upload` publishes this directory. So copy
`default.yaml` to `~/.config/ump/profiles/default.yaml` and make it
yours; the layouts work the same with `layouts.yaml`.

The flags of `ump server reinstall` override a profile for one installation:
`--layout`, `--os`, `--timezone`, `--ipv6`, `--mitigations`, `--disks`,
`--ssh-key` (keys added to root).

## Fields

```yaml
name: default                 # optional; the file name otherwise
description: ...              # optional
os: ubuntu-26.04              # the image: os/<os>/<arch>/ on the storage
timezone: UTC                 # optional, UTC
kernel:                       # optional; everything off unless said
  mitigations: false          # CPU vulnerability mitigations; mitigations=off when false
  ipv6: false                 # ipv6.disable=1 when false
  extra: ""                   # anything else for the kernel command line
disks: lvm-linear             # a layout name from layouts.yaml, or an inline layout (below)
users:
  - name: root
    keys: [ump, local]        # see Keys
  - name: ops
    sudo: true                # member of sudo without a password (no passwords are set)
    groups: [adm]             # optional, created when missing
    shell: /bin/bash          # optional
    keys: [agent, ~/.ssh/ops.pub]
packages: [htop]              # optional; Ubuntu packages, installed from the archive at installation
firewall:                     # optional; no firewall without it
  policy: drop                # drop: only what is allowed gets in; accept: no firewall
  allow: [icmp, ssh, tcp/80, udp/53, "tcp/8000-8100", {proto: tcp, port: 443, from: 10.0.0.0/8}]
provision:                    # optional; the step after the first boot
  type: none                  # none | ansible-pull | ansible-bootstrap
  repo: ...                   # whatever the step needs; kept with the type in /etc/ump/install
```

Every user needs at least one key: no passwords are set, so a user
without keys could not log in. The firewall always lets the loopback
interface and the answers to the server's own connections through;
`policy: drop` with `icmp` and `ssh` is what the default profile does.
The provision step is recorded on the installed system and reported by
`ump server reinstall`; running it is not implemented yet.

### Keys

A key reference is one of:

| Reference | Meaning |
|---|---|
| `ump` | UMP's own key (`ump config key`), which is how ump reaches the server afterwards |
| `local` | the public keys next to your `~/.ssh/id_*` files (`id_ed25519.pub`, `id_rsa.pub`, ...) |
| `agent` | every key your ssh-agent holds (`ssh-add -L`); an error when it holds none |
| a path | a file with one key per line, `~` allowed |
| a key line | `ssh-ed25519 AAAA... comment` as it stands |

## Layouts

[`layouts.yaml`](layouts.yaml) holds named layouts; a profile may hold
its own under `disks:` with the same shape. A layout is a list of
parts, applied in order:

```yaml
parts:
  - {type: slice, target: [first], size: 1M, filesystem: bios}
  - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
  - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
  - {type: slice, target: [".*"], size: rest, filesystem: lvm, vg: vg0}
  - {type: lvm, vg: vg0, name: root, size: rest, filesystem: ext4, mountpoint: /}
```

| Field | slice | md | lvm |
|---|---|---|---|
| `target` | the disks: `first` (the first disk, NVMe before others) or a regular expression on the device path (`nvme.*`, `sd.*`, `.*`) | same | |
| `index` | the partition number (default: the next free one); the ported layouts use 127 and 128 for the boot partitions on every disk | | |
| `size` | `1M`, `512M`, `1G`, `8G`, `20%` of the disk, or `rest` | the member partition on each disk | `8G`, `20%` of the volume group, or `rest` |
| `raid` | | the md level: 0, 1, 4, 5, 6, 10 | 0 (striped over the physical volumes), 1, 5, 6, 10; none is linear |
| `filesystem` | `ext4`, `xfs`, `btrfs`, `vfat`, `swap`, `efi`, `bios`, `lvm` (a physical volume of `vg`), `none` | same but `efi` and `bios` | `ext4`, `xfs`, `btrfs`, `swap` |
| `mountpoint` | where it is mounted; `efi` needs `/boot/efi` | same | same |
| `vg` | the volume group this physical volume joins | same | the volume group of the volume |
| `name` | | | the volume's name |
| `label` | a file system label | same | same |

A `slice` makes one partition on every matching disk; an `md` part makes
a partition of that size on every matching disk and one array over them;
an `lvm` part makes a volume in a group that an earlier `slice` or `md`
with `filesystem: lvm` created. The layouts in `layouts.yaml`:

| Layout | What it is |
|---|---|
| `lvm-linear` | every disk in one volume group, root linear over all of them, boot partitions on the first disk (the default) |
| `lvm-striped` | the NVMe disks in one volume group, root and 8 GB of swap striped, boot partitions on every disk |
| `lvm-raid1` | as `lvm-striped`, but root mirrored |
| `lvm-example` | two groups: NVMe for the system (mirrored root, striped home and swap), SATA for `/data` |

Rules the installer keeps: there must be exactly one `/`; `/boot` must
be a plain partition when root is on LVM or md (the boot loader reads
plain partitions only); the boot loader needs an `efi` part for UEFI
machines and a `bios` part for BIOS machines on amd64, and it is
installed on every disk that has a `bios` part; a target that matches no
disk, or an md or raid level with too few members, stops the
installation before any disk is touched. Mirrored and parity volumes
synchronise in the background after the first boot.
