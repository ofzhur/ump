# OS profiles

A profile says what a server becomes when `ump server reinstall` installs
it: which operating system image, which [disk layout](disk-layouts.md),
which users with which SSH keys, which [userdata](userdata.md) runs in
the new system, what the [firewall](firewalls.md) lets in, the kernel command line, and what
happens after the first boot. It is one YAML file per profile.

```console
$ ump config os list
    NAME           OS             DISKS        USERS   FIREWALL   FROM        DESCRIPTION
*   ubuntu-26.04   ubuntu-26.04   lvm-linear   ump     default    published   Ubuntu 26.04 on every disk as one linear volume group; the sudo user ump with your keys, no root over SSH; ICMP and SSH open, nothing else.
    ubuntu-24.04   ubuntu-24.04   lvm-linear   ump     default    published   Ubuntu 24.04 on every disk ...
    ubuntu-22.04   ubuntu-22.04   lvm-linear   ump     default    published   Ubuntu 22.04 on every disk ...
    web            ubuntu-26.04   lvm-raid1    ump,ops web        yours       Web servers
* the default (ump config os default NAME changes it); published profiles are replaced by ump config os update, make your own with ump config os create NAME --from ubuntu-26.04
```

## Where profiles live

Profiles are files in the profiles directory next to the configuration,
`~/.config/ump/profiles/` (`$XDG_CONFIG_HOME/ump/profiles/` when that is
set), one `NAME.yaml` per profile, plus three files of named entries
the profiles refer to: `layouts.yaml` with the [disk
layouts](disk-layouts.md), `firewalls.yaml` with the
[firewalls](firewalls.md) and `users.yaml` with the [users](#users).
The first command that needs the directory creates it and downloads
the published files from the storage, so that there is always
something to look at and start from.

**Published and yours.** The published profiles
([`ubuntu-26.04.yaml`](../profiles/ubuntu-26.04.yaml),
[`ubuntu-24.04.yaml`](../profiles/ubuntu-24.04.yaml),
[`ubuntu-22.04.yaml`](../profiles/ubuntu-22.04.yaml),
[`debian-13.yaml`](../profiles/debian-13.yaml),
[`debian-12.yaml`](../profiles/debian-12.yaml): the same profile with
each image and the userdata of its distribution; [`nixos-25.11.yaml`](../profiles/nixos-25.11.yaml) and
[`archlinux.yaml`](../profiles/archlinux.yaml), the same for NixOS and Arch Linux) and the published entries of the named files (the
layouts `lvm-*`, the firewalls `default`, `web` and `open`, the user
`ump`) are templates: `ump config os update` fetches them again and
replaces them, which is why `ump config os edit` refuses a published
profile. Make your own instead: `ump config os create NAME` (guided) or
`ump config os create NAME --from ubuntu-26.04` (a copy), `ump config
disk-layout create`, `ump config firewall create`, `ump config user
create`. Everything with a name of your own is yours and never touched
by an update; `ump config os list` says which is which in its `FROM`
column. (An entry of yours in a named file under a published name is
replaced by the update too, so give yours their own names.)

When `ump server reinstall` needs the profile `NAME`, it takes, in this
order:

1. the file itself, when `--profile` is a path (it contains a `/` or ends
   in `.yaml`);
2. `~/.config/ump/profiles/NAME.yaml`;
3. `profiles/NAME.yaml` on the storage (`https://ump.zhurko.de/dl` by
   default, see [configuration](configuration.md#storage)).

Which profile a server is installed with: `--profile`, else the profile
set for the server with `ump server set NAME --profile NAME` (kept in
the configuration file), else the **default profile**: `ubuntu-26.04`
until you change it with `ump config os default NAME`; `ump config os
list` marks it with `*`.

## The default profile

[`ubuntu-26.04.yaml`](../profiles/ubuntu-26.04.yaml) is what you get
without changing anything:

```yaml
name: ubuntu-26.04
description: Ubuntu 26.04 on every disk as one linear volume group; the sudo user ump with your keys, no root over SSH; ICMP and SSH open, nothing else.

os: ubuntu-26.04
timezone: UTC

kernel:
  mitigations: false   # mitigations=off: the CPU vulnerability mitigations cost performance
  ipv6: false          # ipv6.disable=1: turn it on when the server needs IPv6
  extra: ""

disks: lvm-linear      # a layout from layouts.yaml, or an inline one

users: [ump]           # users from users.yaml: ump is the administrator, sudo without a
                       # password, UMP's key and the public keys next to your ~/.ssh/id_*

userdata: [ubuntu]     # entries from userdata.yaml, run in the new system before its first boot

firewall: default      # a firewall from firewalls.yaml, or an inline one

provision:
  type: none

network:
  rename: true         # uniform interface names: ext01, ... and int01, ...
```

Installed with it, a server runs the [UMP Ubuntu 26.04 base
image](../images/base/README.md) with every disk in one volume group
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
[README](../images/base/README.md#defaults) lists the rest.

To make it yours, create a profile from it: `ump config os create mine
--from ubuntu-26.04`, then `ump config os default mine`. The usual
first changes are your own users (`ump config user create`), `kernel.ipv6:
true` for servers that need IPv6, and a firewall for the server's role.

## Choosing the operating system

The image is the profile's `os` field: the name of a directory under
`os/` on the storage, which holds one archive per architecture (see
[the base images](../images/base/README.md); NixOS has a manifest only,
see [operating systems](oses.md#nixos)). The storage
publishes:

| `os` | Release | Profile published with it |
|---|---|---|
| `ubuntu-26.04` | Ubuntu 26.04 LTS (resolute) | `ubuntu-26.04`, the default |
| `ubuntu-24.04` | Ubuntu 24.04 LTS (noble) | `ubuntu-24.04` |
| `ubuntu-22.04` | Ubuntu 22.04 LTS (jammy) | `ubuntu-22.04` |
| `debian-13` | Debian 13 (trixie) | `debian-13` |
| `debian-12` | Debian 12 (bookworm) | `debian-12` |
| `nixos-25.11` | NixOS 25.11 from its stable channel, installed by `nixos-install` (see [NixOS](oses.md#nixos)) | `nixos-25.11` |
| `archlinux` | Arch Linux, the rolling release, installed by `pacstrap` from its mirrors, amd64 only (see [Arch Linux](oses.md#arch-linux)) | `archlinux` |

The published Ubuntu profiles are the same apart from the image, and
the NixOS one apart from the image and its userdata (`nixos`, a NixOS
module, where the Ubuntu profiles have `ubuntu`, a script). A system or
release is chosen in one of three ways:

- **A profile per release:** `ump server reinstall NAME --profile
  ubuntu-24.04`, or `ump server set NAME --profile ubuntu-24.04` once,
  after which every reinstall of that server takes it.
- **Your own profile:** `ump config os create NAME` asks which image
  (the list comes from the storage's `os/index.json`), or set `os:` in
  a copy of a published profile; `ump config os default NAME` then
  makes it the default.
- **For one installation:** `ump server reinstall NAME --os
  ubuntu-22.04` overrides the profile's image and changes nothing else
  (between Ubuntu and NixOS the userdata must change too, so take the
  other profile instead).

More images come later; [the storage
layout](../images/rescue/README.md#your-own-storage) says what an image
directory must hold, and anything that follows it works with any `os`
name (`ump config os create` takes "another name" for it).

## Fields

```yaml
name: ubuntu-26.04            # optional; the file name otherwise
description: ...              # optional; shown by ump config os list
os: ubuntu-26.04              # the image: os/<os>/<arch>/ on the storage
timezone: UTC                 # optional; UTC
kernel:                       # optional; both switches off unless set
  mitigations: false          # mitigations=off on the kernel command line when false or absent
  ipv6: false                 # ipv6.disable=1 when false or absent
  extra: ""                   # anything else for the kernel command line
disks: lvm-linear             # a layout name from layouts.yaml, or an inline layout
users:
  - ump                       # a user from users.yaml (ump config user), by name
  - name: ops                 # or an inline user, with the same fields
    sudo: true                # member of sudo without a password; the first such user is the administrator
    shell: /bin/bash          # optional; /bin/bash
    groups: [adm]             # optional; created when missing
    keys: [ump, local]        # at least one key reference (see Keys)
  - name: root                # only when listed does root log in over SSH, with keys only
    keys: [ump]
userdata: [ubuntu, web]       # optional; entries from userdata.yaml, or inline ones, run in order
                              # as root in the new system before its first boot
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
| `users` | The accounts of the new system, in order: names from `users.yaml`, inline users, or both. At least one, no name twice, and one of them must be able to administer the system (below). |
| `users[].name` | Lower-case letters, digits, `_` and `-`, up to 32 characters, starting with a letter or `_`. `root` is allowed and special. In `users.yaml` the entry's key is the name. |
| `users[].sudo` | `true` makes the user a member of `sudo` with a `NOPASSWD` drop-in, since no passwords are set. |
| `users[].shell` | A path. Default `/bin/bash`, which the image ships with a colored prompt and completion; another shell needs its package, installed by userdata. |
| `users[].groups` | Extra groups, created when missing. |
| `users[].keys` | Key references (below). Every user needs at least one: without a password, a user without keys could not log in. |
| `userdata` | What runs in the new system before its first boot, in order: names from `userdata.yaml`, inline entries (`{name: web, script: "..."}`), or both. See [userdata](userdata.md). |
| `firewall` | A name from `firewalls.yaml`, or an inline [firewall](firewalls.md) (`{policy: drop, allow: [icmp, ssh]}`). Absent means no firewall. |
| `provision.type` | The step after the first boot: `none`, `ansible-pull` or `ansible-bootstrap`. The type and the other keys of the section are recorded in `/etc/ump/install` on the server (`provision=...`, `provision_repo=...`); running the step is not implemented yet, `ump server reinstall` says so. |
| `network.rename` | `true` (the default) names the interfaces uniformly: `ext01`, `ext02`, ... for the ones with a public address, `int01`, ... for the rest; `false` keeps the kernel's names. See [interface names](#interface-names). |

Unknown fields are an error, so a typo in a field name is caught by
`ump config os edit`, not at installation.

### Users

[Users](users.md) are objects of their own: [`users.yaml`](../profiles/users.yaml)
next to the profiles holds them by name, with `description`, `sudo`,
`groups`, `shell` and `keys`, and a profile lists the names it wants
(`users: [ump, ops]`), so the same user goes into any number of
profiles and is changed in one place. A profile may also carry inline
users, with `name` and the same fields, for what belongs to that profile
alone, and mix both. The published entry is `ump`, the administrator
UMP provides (sudo without a password, bash, `keys: [ump, local]`);
`ump config os update` replaces it and keeps the users you added.

```console
$ ump config user list
NAME   SUDO   SHELL      KEYS         STATE   DESCRIPTION
ump    yes    /bin/bash  ump, local   ok      the administrator UMP provides
$ ump config user create ops --from ump      # then your editor on users.yaml
$ ump config user get ops
$ ump config user edit                       # the whole file, checked afterwards
$ ump config user delete ops
```

`ump config user create` alone asks for the name and the user to start
from on a terminal, and `ump config os create` defines a user on the spot when you name one it
does not know.

### The administrator and root

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

A [key](keys.md) reference in `users[].keys` is one of:

| Reference | Meaning |
|---|---|
| `ump` | UMP's own key (`ump config key`), which is how ump reaches the server afterwards. Give it to the administrator. |
| `local` | the public keys next to your `~/.ssh/id_*` files (`id_ed25519.pub`, `id_rsa.pub`, ...); nothing when there are none |
| `agent` | every key your ssh-agent holds (`ssh-add -L`); an error when it holds none |
| a key name | a key kept under that name with `ump config key add NAME` (below) |
| a path | a file with one key per line, `~` allowed (`~/.ssh/ops.pub`) |
| a key line | `ssh-ed25519 AAAA... comment`, as it stands, inlined |

Named keys live in `~/.config/ump/keys.yaml`, so a user can carry
several people's keys without files lying around, and the same key goes
to any user by name:

```console
$ ump config key add oleg --file ~/.ssh/id_ed25519.pub
$ ump config key add laptop --line 'ssh-ed25519 AAAA... oleg@laptop'
$ ump config key add work --agent          # every key the agent holds: work, work-2, ...
$ ump config key list                      # ump (UMP's own) and the named ones, with fingerprints
$ ump config key remove laptop
```

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
ump config os list                         # the profiles in ~/.config/ump/profiles, with os, layout, users, firewall
ump config os get NAME                     # print one
ump config os create [NAME]                # on a terminal: the wizard (the name, then the questions below), then the file is written
ump config os create NAME --from NAME      # copy one instead, rename it, open it in your editor
ump config os edit NAME                    # your editor ($VISUAL, $EDITOR, else vi), then a check
ump config os delete NAME [--yes]
ump config os update                       # refresh the published profiles and the published entries of layouts.yaml, firewalls.yaml and users.yaml; yours are kept
ump config os default [NAME]               # show or set the default profile
```

`create [NAME]` on a terminal asks for the name when none is given,
then, with the default profile's answers preselected: the image (listed from the storage with kernel and build
date, or another name), the disk layout and the firewall by name, the
users by name from `users.yaml` (a name it does not know is defined on
the spot: sudo, shell, keys, description, and kept there), the
mitigations and IPv6 switches, the time zone, the userdata by name
from `userdata.yaml`, the interface naming, the provision step and a description. It checks the result like an
installation would, writes the file, prints it and offers your editor.
`--from NAME` copies a profile instead, and without a terminal that is
the only form.

`edit` and `create --from` check the file when the editor returns: the YAML, the
fields, the layout (an inline one, or that the named one exists in
`layouts.yaml` and is valid), the firewall likewise, and that an
administrator and keys are there. An invalid profile is kept, with the
problem named, so nothing is lost; fix it with another `edit`. What is
not checked until an installation: that the key references yield keys,
and that the image exists on the storage.

`ump config os` is also `o` or `profile`, and the verbs have the short
forms of every `ump config` entity (`ump c o ls`); see the [CLI
reference](cli.md#ump-config).

### Checks

A profile is valid when:

- `os` is set and looks like an image name;
- `disks` names a layout or holds one, and the layout passes [its
  checks](disk-layouts.md#rules);
- there is at least one user, no user twice, every named user exists in
  `users.yaml`, every user has at least one key reference, names and
  groups are valid, shells are paths;
- a user has `sudo: true`, or `root` is listed;
- package names look like Debian package names;
- an inline firewall passes [its checks](firewalls.md#fields); a named
  one exists in `firewalls.yaml`;
- `provision.type` is `none`, `ansible-pull` or `ansible-bootstrap`.

## Using a profile

```console
$ ump server reinstall web-01                       # the profile set for web-01, else the default profile
$ ump server reinstall web-01 --profile web         # a named profile
$ ump server reinstall web-01 --profile ./web.yaml  # a file
$ ump server set web-01 --profile web               # remember it: reinstall takes it from now on
$ ump server set web-01 --clear
```

`ump server reinstall --dry-run` brings the server into the rescue system
and prints what the profile resolves to on that machine: every partition,
array and volume per disk, the network, the users, userdata, firewall and
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
| `--set FIELD=VALUE` | any field, by its path in the YAML, with the value as YAML (repeatable): `--set kernel.extra=quiet`, `--set users[0].shell=/bin/bash`, `--set users[ump].keys='[ump, agent]'`, `--set 'userdata=[ubuntu, web]'`, `--set 'users[+]={name: ops, sudo: true, keys: [local]}'`, `--set network.rename=false`; an unknown field is refused like in a profile file |

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
description: like ubuntu-26.04, with a second administrator, HTTP and HTTPS open, and IPv6

os: ubuntu-26.04
timezone: Europe/Berlin

kernel:
  ipv6: true

disks: lvm-raid1               # mirrored root on the NVMe disks

users:
  - ump                        # from users.yaml
  - ops                        # yours in users.yaml: ump config user create ops --from ump
  - name: deploy               # inline: only this profile has it
    groups: [www-data]
    keys: [deploy]             # a named key: ump config key add deploy --file ~/.ssh/deploy.pub

userdata: [ubuntu, web]        # ubuntu: what every Ubuntu gets; web: yours, ump config userdata create web

firewall: web                  # ICMP, SSH, HTTP, HTTPS

provision:
  type: ansible-pull
  repo: git@github.com:example/infra.git
```

The administrator here is `ops`, the first user with sudo. `ump ssh
web-01` logs in as `ops` once the server has the profile set (`ump server
set web-01 --profile web`); until then, `ump ssh web-01 --user ops`.
