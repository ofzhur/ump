# Quickstart

From nothing to a freshly installed server in six commands. You need a
server at [Hetzner Robot, Hetzner Cloud or OVHcloud](providers.md) with a
public address, the API credentials of that account, and `ssh` on your
machine. The server will be erased.

## 1. Install ump

```console
$ curl -fsSL https://ump.zhurko.de/get.sh | sh
```

Linux or macOS, amd64 or arm64, into `/usr/local/bin` (with `sudo` when
needed, else `~/.local/bin`); the script checks the binary against the
published `SHA256SUMS`. `UMP_VERSION=v1.2.3` picks a release,
`UMP_INSTALL_DIR` another directory. Windows users take
`ump-windows-amd64.exe` from
[`bin/latest/`](https://ump.zhurko.de/bin/latest/SHA256SUMS). Or build it
yourself with Go 1.26 or newer:

```console
$ git clone git@github.com:ofzhur/ump.git && cd ump && make     # ./ump; make install puts it into your Go bin directory
```

## 2. Add a provider account

```console
$ ump config isp
```

The wizard asks for the provider type, a name for the account, where to
keep the secrets (files only you can read, environment variables, or a
command such as `pass show ...`), and the credentials; it tests them with
one API call and saves the account to `~/.config/ump/config.yaml`.
Without a terminal:

```console
$ ump config isp add hetzner-cloud hcloud --set token=env:HCLOUD_TOKEN
$ ump config isp test
```

Where the credentials come from and what each provider can do is in
[provider accounts](providers.md).

## 3. Look at your servers

```console
$ ump server list
ACCOUNT   PROVIDER        ID        NAME      IPV4            LOCATION    STATUS
hcloud    hetzner-cloud   234567    web-01    198.51.100.22   hel1        running
hrobot    hetzner-robot   123456    db-01     203.0.113.10    fsn1-dc7    ready

$ ump server show web-01            # everything the provider knows about it
$ ump server list -o json           # for scripts; also yaml, wide, name
```

Servers are addressed by name, provider ID, IP address or `provider/id`,
across every account. `ump server list --diff` shows what changed since
the last listing.

## 4. Look at what an installation gives you

```console
$ ump config os list
Downloaded the published profiles, layouts, firewalls and users into ~/.config/ump/profiles (ump config os); make your own from them.
    NAME           OS             DISKS        USERS   FIREWALL   FROM        DESCRIPTION
*   ubuntu-26.04   ubuntu-26.04   lvm-linear   ump     default    published   Ubuntu 26.04 on every disk as one linear volume group; the sudo user ump with your keys, no root over SSH; ICMP and SSH open, nothing else.
    ubuntu-24.04   ubuntu-24.04   lvm-linear   ump     default    published   Ubuntu 24.04 on every disk ...
    ubuntu-22.04   ubuntu-22.04   lvm-linear   ump     default    published   Ubuntu 22.04 on every disk ...
* the default (ump config os default NAME changes it); published profiles are replaced by ump config os update, make your own with ump config os create NAME --from ubuntu-26.04

$ ump config os get ubuntu-26.04    # the profile itself
$ ump config disk-layout list       # the named disk layouts: lvm-linear, lvm-striped, lvm-raid1, lvm-example
$ ump config firewall list          # the named firewalls: default, web, open
```

The first of these commands downloads the published defaults into
`~/.config/ump/profiles/`. From then on they are your files. The
[default profile](profiles.md#the-default-profile) installs Ubuntu 26.04
with every disk in one volume group, a sudo user `ump` with UMP's own key
and the public keys next to your `~/.ssh/id_*` files, no root login and
no passwords, an nftables firewall that lets ICMP and SSH in, and IPv6
and the CPU mitigations off. Change what you like:

```console
$ ump config os edit default        # your editor, then a check
```

Common first edits: rename the `ump` user or add yourself, set
`kernel.ipv6: true` if the server needs IPv6, pick another layout
(`disks: lvm-raid1` for two NVMe disks) or firewall (`firewall: web`).
[Profiles](profiles.md), [disk layouts](disk-layouts.md) and
[firewalls](firewalls.md) describe every field and the published ones.

## 5. See the plan, then install

```console
$ ump server reinstall web-01 --dry-run
Bring hetzner-cloud/234567 (web-01, 198.51.100.22) into the rescue system and show the installation plan for ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear)? (dry run: nothing is erased) [y/N] y
Checking root access to 198.51.100.22 over SSH ... no.
Going through the provider's rescue system.
...
Waiting for the UMP rescue system on 198.51.100.22 ... up after 14s (ump-rescue 2026.10.13).
Installation plan for ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear) on web-01:
==> machine: amd64, BIOS firmware, 2 CPUs, 3915 MB RAM
==> image: ubuntu-26.04 for amd64 from https://ump.zhurko.de/os/ubuntu-26.04/amd64 (profile ubuntu-26.04, layout lvm-linear)
==> disks (1): /dev/sda (38.1G SSD)
==>   /dev/sda: 1: 1M bios, 2: 512M efi at /boot/efi, 3: 1G ext4 at /boot, 4: rest lvm (vg0)
==>   vg0/root: rest linear ext4 at /
==> network: ext01 (96:00:01:ab:cd:ef) 198.51.100.22/32,172.31.1.1; IPv6 off; DNS 1.1.1.1 8.8.8.8; hostname web-01; time zone UTC
==> interfaces: ext01 (ext, 96:00:01:ab:cd:ef, now eth0)
==> users: ump (2 key(s), sudo)
==> packages: none; firewall: drop, allow icmp, tcp/22; provision: none
==> dry run: stopping here, no disk was touched
Dry run done; web-01 stays in the UMP rescue system (ump ssh web-01 --staging).
```

The dry run brings the server into UMP's rescue system, a Debian that
runs from RAM with the disks untouched, and prints what the profile
resolves to on that machine: every partition, array and volume per disk,
the network, users, packages and firewall. The server stays there until
you reboot it; `ump server unrescue web-01 --reboot` boots the old system
again, and a real installation continues from the rescue system as it is.

```console
$ ump server reinstall web-01
Install ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear) on hetzner-cloud/234567 (web-01, 198.51.100.22) as web-01? Every disk of the server is erased. [y/N] y
...
Installing ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear) on web-01 as web-01 ...
==> machine: amd64, BIOS firmware, 2 CPUs, 3.8 GB RAM
==> disks (1): /dev/sda (38.1G SSD)
...
==> installed: ubuntu-26.04 2026.10.13 (kernel 7.0.0-39-generic) as web-01, root on /dev/vg0/root (37000 MB; layout lvm-linear over 1 disk(s)); reboot to start it
Rebooting web-01 into ubuntu-26.04 ...
Waiting for SSH on 198.51.100.22 to go down ... down after 4s.
Waiting for SSH on 198.51.100.22 ... up after 18s.
web-01 is up: Ubuntu 26.04 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).
```

A few minutes on a cloud VM, longer on bare metal with many disks. The
server's name at the provider becomes its hostname (`--hostname` for
another), and the address, gateway and interface it had are carried over.
`--profile NAME`, `--layout NAME` and `--firewall NAME` pick others for
this installation; without a terminal, add `--yes`.

## 6. Log in

```console
$ ump ssh web-01
$ ump ssh web-01 -- sudo nft list ruleset       # the firewall
$ ump ssh web-01 -- lsblk                       # the disks
$ ump ssh web-01 -- cat /etc/ump/install        # what was installed, when, from which profile
```

`ump ssh` logs in as the profile's administrator (`ump`) with UMP's key
and your own keys, so `ssh ump@198.51.100.22` works just as well. The
message of the day shows the host, the profile it was installed from,
uptime, memory, the root disk and the addresses.

## Next

- **A profile per role.** `ump config os create web` copies the default
  profile and opens it; `ump server set web-01 --profile web` remembers
  it for the server, so `ump server reinstall web-01` takes it from then
  on. See [profiles](profiles.md).
- **Your own disk layouts and firewalls.** `ump config disk-layout create
  NAME --from lvm-raid1`, `ump config firewall create NAME --from web`;
  both check the file after your editor returns. See [disk
  layouts](disk-layouts.md) and [firewalls](firewalls.md).
- **The rescue system by itself.** `ump server rescue web-01` boots a
  server into UMP's rescue system and hands you a root shell there, with
  the disks untouched; `ump server rescue web-01 --yes -- lsblk` runs one
  command. `ump server unrescue web-01 --reboot` leaves it.
- **Names and reverse DNS.** `ump server rename web-01 prod-web-01` sets
  the name at the provider (the hostname follows at the next install),
  `ump server rdns set prod-web-01` makes the reverse DNS of the primary
  address the server name.
- **Staying current.** Once a day ump says when a newer release is
  published; the same `get.sh` one-liner installs it. The rescue image,
  the installer and the OS image come from the storage at every use, so
  they are current without a new binary, and `ump config os update`
  refreshes your copies of the published profile, layouts and firewalls
  while keeping the files you edited.
- **Scripts and agents.** Every command has `-o json`; nothing prompts
  without a terminal (`--yes` where something is destructive, exit code 3
  otherwise). See the [CLI reference](cli.md).
- **A storage of your own.** Everything ump puts on a server (the rescue
  image, the installer, the profiles, the OS image) comes from one base
  URL, `https://ump.zhurko.de` by default; [the rescue image's
  README](../images/rescue/README.md#your-own-storage) says what to serve
  to run your own, and `storage:` in the [configuration](configuration.md)
  points ump at it.
