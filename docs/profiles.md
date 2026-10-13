# OS profiles

A profile says what a server becomes when `ump server reinstall` installs
it: which operating system image, which [disk layout](disk-layouts.md),
which users with which SSH keys, which extra packages, what the
[firewall](firewalls.md) lets in, the kernel command line, and what
happens after the first boot. It is one YAML file per profile.

```console
$ ump config os list
NAME      OS             DISKS        USERS   FIREWALL   DESCRIPTION
default   ubuntu-26.04   lvm-linear   ump     default    Ubuntu 26.04 on every disk as one linear volume group; the sudo user ump with your keys, no root over SSH; ICMP and SSH open, nothing else.
```

## Where profiles live

Profiles are files in the profiles directory next to the configuration,
`~/.config/ump/profiles/` (`$XDG_CONFIG_HOME/ump/profiles/` when that is
set), one `NAME.yaml` per profile, plus `layouts.yaml` with the named
disk layouts and `firewalls.yaml` with the named firewalls. The first
command that needs the directory creates it and downloads the published
[`default.yaml`](../profiles/default.yaml) (with
[`ubuntu-24.04.yaml`](../profiles/ubuntu-24.04.yaml) and
[`ubuntu-22.04.yaml`](../profiles/ubuntu-22.04.yaml), the same profile
for those releases),
[`layouts.yaml`](../profiles/layouts.yaml) and
[`firewalls.yaml`](../profiles/firewalls.yaml) from the storage, so that
there is always something to look at and start from. From then on the
files are yours: ump never overwrites them on its own. `ump config os
update` fetches the published files again when you ask: a file
still as published is replaced, one you edited is kept, with the new
published version next to it in `.published/` for you to compare, and
profiles under other names are never touched.

When `ump server reinstall` needs the profile `NAME`, it takes, in this
order:

1. the file itself, when `--profile` is a path (it contains a `/` or ends
   in `.yaml`);
2. `~/.config/ump/profiles/NAME.yaml`;
3. `profiles/NAME.yaml` on the storage (`https://ump.zhurko.de` by
   default, see [configuration](configuration.md#storage)).

So `default` is the published default profile until you edit your copy,
and a profile that only exists on a storage of your own works without a
local copy. Which profile a server is installed with: `--profile`, else
the profile set for the server with `ump server set NAME --profile NAME`
(kept in the configuration file), else `default`.

## The default profile

[`default.yaml`](../profiles/default.yaml) is what you get without
changing anything:

```yaml
name: default
description: Ubuntu 26.04 on every disk as one linear volume group; the sudo user ump with your keys, no root over SSH; ICMP and SSH open, nothing else.

os: ubuntu-26.04
timezone: UTC

kernel:
  mitigations: false   # mitigations=off: the CPU vulnerability mitigations cost performance
  ipv6: false          # ipv6.disable=1: turn it on when the server needs IPv6
  extra: ""

disks: lvm-linear      # a layout from layouts.yaml, or an inline one

users:
  - name: ump          # the administrator: ump logs in as this user afterwards, and so do you
    sudo: true         # member of sudo, without a password (no passwords are set)
    shell: /bin/zsh    # zsh with oh-my-zsh; /bin/bash is there too
    keys: [ump, local] # UMP's own key, and the public keys next to your ~/.ssh/id_* files

packages: []

firewall: default      # a firewall from firewalls.yaml, or an inline one

provision:
  type: none

network:
  rename: true         # uniform interface names: ext01, ... and int01, ...
```

Installed with it, a server runs the [UMP Ubuntu 26.04 base
image](../images/ubuntu/README.md) with every disk in one volume group
and root as one linear volume over all of it (the
[`lvm-linear`](disk-layouts.md#lvm-linear) layout), the server's name as
hostname, the address and gateway it had before as a static netplan
configuration on `ext01`, the resolvers 1.1.1.1 and 8.8.8.8, UTC, the
sudo user `ump` with UMP's key and your own public keys, no root login
over SSH and no passwords anywhere, an nftables firewall that lets ICMP
and SSH in and nothing else ([`default`](firewalls.md#the-published-firewalls)),
and a kernel command line with `mitigations=off`, `ipv6.disable=1` and
`kho=off`. The image brings `tuned` with the `throughput-performance`
profile, the `performance` CPU governor, no cloud-init and no snaps; its
[README](../images/ubuntu/README.md#defaults) lists the rest.

To make it yours, edit the copy: `ump config os edit default`. The usual
first changes are the administrator's name and shell, a second user,
`kernel.ipv6: true` for servers that need IPv6, and a firewall for the
server's role.

## Choosing the operating system

The image is the profile's `os` field: the name of a directory under
`os/` on the storage, which holds one archive per architecture (see
[the Ubuntu base image](../images/ubuntu/README.md)). The storage
publishes:

| `os` | Release | Profile published with it |
|---|---|---|
| `ubuntu-26.04` | Ubuntu 26.04 LTS (resolute) | `default` |
| `ubuntu-24.04` | Ubuntu 24.04 LTS (noble) | `ubuntu-24.04` |
| `ubuntu-22.04` | Ubuntu 22.04 LTS (jammy) | `ubuntu-22.04` |

The three published profiles are the same apart from the image, so a
release is chosen in one of three ways:

- **A profile per release:** `ump server reinstall NAME --profile
  ubuntu-24.04`, or `ump server set NAME --profile ubuntu-24.04` once,
  after which every reinstall of that server takes it.
- **Your own profile:** `ump config os create NAME` asks which image
  (the list comes from the storage's `os/index.json`), or set `os:` in
  a copy of `default.yaml`.
- **For one installation:** `ump server reinstall NAME --os
  ubuntu-22.04` overrides the profile's image and changes nothing else.

More images come later; [the storage
layout](../images/rescue/README.md#your-own-storage) says what an image
directory must hold, and anything that follows it works with any `os`
name (`ump config os create` takes "another name" for it).

## Fields

```yaml
name: default                 # optional; the file name otherwise
description: ...              # optional; shown by ump config os list
os: ubuntu-26.04              # the image: os/<os>/<arch>/ on the storage
timezone: UTC                 # optional; UTC
kernel:                       # optional; both switches off unless set
  mitigations: false          # mitigations=off on the kernel command line when false or absent
  ipv6: false                 # ipv6.disable=1 when false or absent
  extra: ""                   # anything else for the kernel command line
disks: lvm-linear             # a layout name from layouts.yaml, or an inline layout
users:
  - name: ump                 # the administrator: the first user with sudo (see below)
    sudo: true                # member of sudo without a password
    shell: /bin/zsh           # optional; /bin/bash
    groups: [adm]             # optional; created when missing
    keys: [ump, local]        # at least one key reference (see Keys)
  - name: root                # only when listed does root log in over SSH, with keys only
    keys: [ump]
packages: [htop]              # optional; Ubuntu packages, installed from the archive at installation
firewall: default             # a firewall name from firewalls.yaml, or an inline one;
                              # absent: no firewall
provision:                    # optional; the step after the first boot
  type: none                  # none | ansible-pull | ansible-bootstrap
  repo: ...                   # whatever the step needs; kept with the type in /etc/ump/install
network:                      # optional
  rename: true                # uniform interface names; false keeps the kernel's
```

| Field | Meaning |
|---|---|
| `name` | The profile's name. Optional: the file name without `.yaml` otherwise. |
| `description` | One line, shown by `ump config os list` and recorded on the installed system. |
| `os` | The image to install, a directory name under `os/` on the storage; `ubuntu-26.04` is the one published. No `/` or spaces. |
| `timezone` | The time zone of the new system (`Europe/Berlin`, ...). Default `UTC`. |
| `kernel.mitigations` | `true` keeps the CPU vulnerability mitigations on. Default off, `mitigations=off`. |
| `kernel.ipv6` | `true` keeps IPv6 on and configures the server's IPv6 address. Default off, `ipv6.disable=1`. |
| `kernel.extra` | Anything else for the kernel command line, as one string. `kho=off` is always added: the kernel's kexec handover would reserve memory and keep ump from re-entering its rescue system. |
| `disks` | The [disk layout](disk-layouts.md): a name from `layouts.yaml`, or a mapping with `parts` (an inline layout, with the same fields as a named one). Required. |
| `users` | The accounts of the new system, in order. At least one, and one of them must be able to administer the system (below). |
| `users[].name` | Lower-case letters, digits, `_` and `-`, up to 32 characters, starting with a letter or `_`. `root` is allowed and special. |
| `users[].sudo` | `true` makes the user a member of `sudo` with a `NOPASSWD` drop-in, since no passwords are set. |
| `users[].shell` | A path. Default `/bin/bash`. The image ships `bash` and `zsh` with oh-my-zsh. |
| `users[].groups` | Extra groups, created when missing. |
| `users[].keys` | Key references (below). Every user needs at least one: without a password, a user without keys could not log in. |
| `packages` | Ubuntu package names, installed from the archive inside the new system during the installation, through the rescue system's network. |
| `firewall` | A name from `firewalls.yaml`, or an inline [firewall](firewalls.md) (`{policy: drop, allow: [icmp, ssh]}`). Absent means no firewall. |
| `provision.type` | The step after the first boot: `none`, `ansible-pull` or `ansible-bootstrap`. The type and the other keys of the section are recorded in `/etc/ump/install` on the server (`provision=...`, `provision_repo=...`); running the step is not implemented yet, `ump server reinstall` says so. |
| `network.rename` | `true` (the default) names the interfaces uniformly: `ext01`, `ext02`, ... for the ones with a public address, `int01`, ... for the rest; `false` keeps the kernel's names. See [interface names](#interface-names). |

Unknown fields are an error, so a typo in a field name is caught by
`ump config os edit`, not at installation.

### Users, the administrator and root

No passwords are set on an installed system, so every user needs at least
one key, and SSH is the only way in. One user must be able to administer
the system: the first user with `sudo: true` is the **administrator**,
the user ump logs in as afterwards (`ump ssh NAME` without `--user`, the
check after an installation, and the direct way back into the rescue
system) and the one you use day to day. `root` counts as the
administrator only when no user has sudo and root is listed with keys.

Root cannot log in over SSH unless the profile lists `root` as a user;
then it logs in with keys only, never with a password
(`PermitRootLogin prohibit-password`). Root remains the user of the
rescue system, which is how ump installs the machine.

### Keys

A key reference in `users[].keys` is one of:

| Reference | Meaning |
|---|---|
| `ump` | UMP's own key (`ump config key`), which is how ump reaches the server afterwards. Give it to the administrator. |
| `local` | the public keys next to your `~/.ssh/id_*` files (`id_ed25519.pub`, `id_rsa.pub`, ...); nothing when there are none |
| `agent` | every key your ssh-agent holds (`ssh-add -L`); an error when it holds none |
| a path | a file with one key per line, `~` allowed (`~/.ssh/ops.pub`) |
| a key line | `ssh-ed25519 AAAA... comment`, as it stands |

References are resolved on your machine when the profile is used, so
`local` and `agent` mean the keys of whoever runs `ump server reinstall`.
A user whose references together yield no key is an error before any
disk is touched (`agent` is one by itself when the agent holds nothing).
`ump server reinstall --ssh-key FILE` adds the keys of a file to the
administrator for one installation.

### Interface names

With `network.rename` on, the installer names the wired interfaces of the
machine uniformly instead of leaving the kernel's `enp0s3`, `eno1` or
`eth0`: `ext01`, `ext02`, ... for the interfaces with a public address
(the uplink the rescue system came in on first, then the others in the
kernel's order) and `int01`, `int02`, ... for the rest, whether they
carry a private address or nothing. The names are bound to the MAC
addresses in `/etc/netplan/10-ump.yaml` (`set-name`), so they survive
reboots and kernel upgrades, and `/etc/ump/install` lists them
(`interfaces=ext01=aa:bb:... int01=...`). Only the uplink is configured;
the others are up without an address, ready for your netplan. The
[firewall](firewalls.md) uses the `ext*` names to tell outside traffic
apart. `rename: false` keeps the kernel's names, with the uplink matched
by MAC as before.

## Managing profiles

```
ump config os                              # a small wizard over the commands below, on a terminal
ump config os list                         # the profiles in ~/.config/ump/profiles, with os, layout, users, firewall
ump config os get NAME                     # print one
ump config os create NAME                  # on a terminal: a few questions (below), then the file is written
ump config os create NAME --from NAME      # copy one instead, rename it, open it in your editor
ump config os edit NAME                    # your editor ($VISUAL, $EDITOR, else vi), then a check
ump config os delete NAME [--yes]
ump config os update                       # refresh the published profiles, layouts.yaml and firewalls.yaml from the storage; edited files are kept
```

`create NAME` on a terminal asks, with the default profile's answers
preselected: the image (listed from the storage with kernel and build
date, or another name), the disk layout and the firewall by name, the
administrator with shell (zsh or bash) and keys, the mitigations and
IPv6 switches, the time zone, extra packages, the interface naming, the
provision step and a description. It checks the result like an
installation would, writes the file, prints it and offers your editor.
`--from NAME` copies a profile instead, and without a terminal that is
the only form. The same questions are behind "create a profile" in the
`ump config os` wizard.

`edit` and `create --from` check the file when the editor returns: the YAML, the
fields, the layout (an inline one, or that the named one exists in
`layouts.yaml` and is valid), the firewall likewise, and that an
administrator and keys are there. An invalid profile is kept, with the
problem named, so nothing is lost; fix it with another `edit`. What is
not checked until an installation: that the key references yield keys,
and that the image exists on the storage.

`ump config os` has the aliases `profile` and `profiles`.

### Checks

A profile is valid when:

- `os` is set and looks like an image name;
- `disks` names a layout or holds one, and the layout passes [its
  checks](disk-layouts.md#rules);
- there is at least one user, no user twice, every user has at least one
  key reference, names and groups are valid, shells are paths;
- a user has `sudo: true`, or `root` is listed;
- package names look like Debian package names;
- an inline firewall passes [its checks](firewalls.md#fields); a named
  one exists in `firewalls.yaml`;
- `provision.type` is `none`, `ansible-pull` or `ansible-bootstrap`.

## Using a profile

```console
$ ump server reinstall web-01                       # the profile set for web-01, else default
$ ump server reinstall web-01 --profile web         # a named profile
$ ump server reinstall web-01 --profile ./web.yaml  # a file
$ ump server set web-01 --profile web               # remember it: reinstall takes it from now on
$ ump server set web-01 --clear
```

`ump server reinstall --dry-run` brings the server into the rescue system
and prints what the profile resolves to on that machine: every partition,
array and volume per disk, the network, the users, packages, firewall and
provision step, without touching a disk.

The flags of `ump server reinstall` override a profile for one
installation:

| Flag | Overrides |
|---|---|
| `--layout NAME` | `disks`, with a layout from `layouts.yaml` |
| `--firewall NAME` | `firewall`, with a firewall from `firewalls.yaml` |
| `--os NAME` | `os` |
| `--timezone ZONE` | `timezone` |
| `--ipv6` | `kernel.ipv6: true` |
| `--mitigations` | `kernel.mitigations: true` |
| `--ssh-key FILE` | adds the file's keys to the administrator (repeatable) |
| `--disks /dev/sda,/dev/sdb` | the disks the layout is applied to (default: every disk of the server) |
| `--hostname NAME` | the hostname (default: the server's name at the provider) |
| `--set FIELD=VALUE` | any field, by its path in the YAML, with the value as YAML (repeatable): `--set kernel.extra=quiet`, `--set users[0].shell=/bin/bash`, `--set users[ump].keys='[ump, agent]'`, `--set 'packages=[htop, nginx]'`, `--set 'users[+]={name: ops, sudo: true, keys: [local]}'`, `--set network.rename=false`; an unknown field is refused like in a profile file |

The resolved profile travels to the installer as a JSON specification and
stays on the installed system as `/etc/ump/spec.json` (mode 0600);
`/etc/ump/install` records the essentials in `key=value` lines: the
image and its version, when it was installed, the profile and layout, the
disks, the boot disk, root, hostname, administrator, interfaces, whether
IPv6 and the mitigations are on, the firewall policy and the provision
step. The message of the day shows the profile too.

## Example: a web server profile

```console
$ ump config os create web
```

```yaml
name: web
description: like default, with a second administrator, HTTP and HTTPS open, and IPv6

os: ubuntu-26.04
timezone: Europe/Berlin

kernel:
  ipv6: true

disks: lvm-raid1               # mirrored root on the NVMe disks

users:
  - name: ops
    sudo: true
    keys: [ump, local]
  - name: deploy
    groups: [www-data]
    keys: [~/.ssh/deploy.pub]

packages: [nginx, certbot]

firewall: web                  # ICMP, SSH, HTTP, HTTPS

provision:
  type: ansible-pull
  repo: git@github.com:example/infra.git
```

The administrator here is `ops`, the first user with sudo. `ump ssh
web-01` logs in as `ops` once the server has the profile set (`ump server
set web-01 --profile web`); until then, `ump ssh web-01 --user ops`.
