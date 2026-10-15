# Quickstart

From nothing to a freshly installed server in six commands. You need a
server at [Hetzner Robot, Hetzner Cloud, OVHcloud, DigitalOcean, Linode or Vultr](isps.md) with a
public address, the API credentials of that account, and `ssh` on your
machine. The server will be erased. At DigitalOcean, the droplet must
also let you log in over SSH with a key on this machine (pick it when
you create the droplet); see [DigitalOcean](isps.md#digitalocean-digitalocean).

## 1. Install ump

```console
$ curl -fsSL https://ump.zhurko.de/dl/get.sh | sh
```

Linux or macOS, amd64 or arm64, into `/usr/local/bin` (with `sudo` when
needed, else `~/.local/bin`); the script checks the binary against the
published `SHA256SUMS`. `UMP_VERSION=v1.2.3` picks a release,
`UMP_INSTALL_DIR` another directory. Windows users take
`ump-windows-amd64.exe` from
[`bin/latest/`](https://ump.zhurko.de/dl/bin/latest/SHA256SUMS). Or build it
yourself with Go 1.26 or newer:

```console
$ git clone git@github.com:ofzhur/ump.git && cd ump && make     # ./ump; make install puts it into your Go bin directory
```

## 2. Decide where ump keeps its things

Everything ump keeps is its state: the accounts and their secrets,
the profiles, UMP's own SSH key, what it knows about your servers, the
listing cache. It lives in a state backend; by default that is the
local directory `~/.config/ump`, and for one person on one machine
that is all there is to it: skip to the next step.

Decide now if the state should be somewhere shared instead, because
the account you add next lands wherever the state is. A team, a second
machine, or CI wants a shared **state backend**: a directory in the
fleet's git repository, a directory on a jump host over SSH, an S3 or
R2 bucket, an HTTP service, Consul or Vault. Everyone using the same
backend has the same state, UMP's key included, and so reaches the
same servers.

```console
$ ump config state setup                                # the wizard: pick a backend, migrate the state there
$ ump config state migrate --to file:///srv/fleet/ump   # or in one line
$ ump config state view
Backend:   file:///srv/fleet/ump (backend.yaml)
Mirror:    ~/.cache/ump/homes/3f9a1c2b7e4d
```

From then on ump works on a mirror of the backend: pulled before every
command, pushed after it, with conflicts named rather than overwritten.
`UMP_STATE=s3://fleet/ump` in the environment names a backend ahead of
the file, which is how CI gets the same state without anything on
disk. Mind that the state holds secrets in clear text: choose a backend
that only your team can read. Moving later is the same wizard, which
offers the migration when this machine holds state; see [the state
backend](state.md).

## 3. Add a provider account

```console
$ ump config isp add
```

The wizard asks for the provider type, a name for the account and the
credentials; it tests them with one API call and saves the account to
`~/.config/ump/config.yaml`, the secrets as files under `secrets/`
there, readable only by you. Without a terminal, or in a script:

```console
$ ump config isp add hetzner-cloud hcloud --set token=env:HCLOUD_TOKEN
$ ump config isp test
```

Where the credentials come from and what each provider can do is in
[provider accounts](providers.md).

## 4. Look at your servers

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

## 5. Look at what an installation gives you

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
$ ump config user list              # the named users: ump, the administrator
$ ump config key list               # UMP's own key, and the keys you keep under names
```

The first of these commands downloads the published defaults into
`~/.config/ump/profiles/`; they are templates, kept current by `ump
config os update`, and what you make from them is yours. The [default
profile](profiles.md#the-default-profile) installs Ubuntu 26.04 with
every disk in one volume group, the sudo user `ump` with UMP's own key
and the public keys next to your `~/.ssh/id_*` files, no root login and
no passwords, an nftables firewall that lets ICMP and SSH in, and IPv6
and the CPU mitigations off. Make it yours:

```console
$ ump config os create mine         # a few questions: image, layout, firewall, users, ...
$ ump config os default mine        # what reinstall takes from now on
```

Common first choices: a user of your own next to `ump` (`ump config
user create oleg --from ump`, with your keys), `kernel.ipv6: true` if
the server needs IPv6, another layout (`lvm-raid1` for two NVMe disks)
or firewall (`web`). [Profiles](profiles.md), [disk
layouts](disk-layouts.md), [firewalls](firewalls.md), [users](users.md),
[userdata](userdata.md)
and [keys](keys.md) describe every field and the published ones.

## 6. See the plan, then install

```console
$ ump server reinstall web-01 --dry-run
Bring hetzner-cloud/234567 (web-01, 198.51.100.22) into the rescue system and show the installation plan for ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear)? (dry run: nothing is erased) [y/N] y
Checking root access to 198.51.100.22 over SSH ... no.
Going through the provider's rescue system.
...
Waiting for the UMP rescue system on 198.51.100.22 ... up after 14s (ump-rescue 2026.10.13).
Installation plan for ubuntu-26.04 (profile ubuntu-26.04 from ~/.config/ump/profiles/ubuntu-26.04.yaml, layout lvm-linear) on web-01:
==> machine: amd64, BIOS firmware, 2 CPUs, 3915 MB RAM
==> image: ubuntu-26.04 for amd64 from https://ump.zhurko.de/dl/os/ubuntu-26.04/amd64 (profile ubuntu-26.04, layout lvm-linear)
==> disks (1): /dev/sda (38.1G SSD)
==>   /dev/sda: 1: 1M bios, 2: 512M efi at /boot/efi, 3: 1G ext4 at /boot, 4: rest lvm (vg0)
==>   vg0/root: rest linear ext4 at /
==> network: ext01 (96:00:01:ab:cd:ef) 198.51.100.22/32,172.31.1.1; IPv6 off; DNS 1.1.1.1 8.8.8.8; hostname web-01; time zone UTC
==> interfaces: ext01 (ext, 96:00:01:ab:cd:ef, now eth0)
==> users: ump (2 key(s), sudo)
==> userdata: ubuntu; firewall: drop, allow icmp, tcp/22; provision: none
==> dry run: stopping here, no disk was touched
Dry run done; web-01 stays in the UMP rescue system (ump ssh web-01 --staging).
```

The dry run brings the server into UMP's rescue system, a Debian that
runs from RAM with the disks untouched, and prints what the profile
resolves to on that machine: every partition, array and volume per disk,
the network, users, userdata and firewall. The server stays there until
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

## 7. Log in

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
- **Your own disk layouts, firewalls and users.** `ump config disk-layout
  create NAME --from lvm-raid1`, `ump config firewall create NAME --from
  web`, `ump config user create NAME --from ump`; each checks the file
  after your editor returns. See [disk layouts](disk-layouts.md),
  [firewalls](firewalls.md), [users](users.md) and [userdata](userdata.md).
- **Keys under names.** `ump config key add oleg --file
  ~/.ssh/id_ed25519.pub` keeps a public key as `oleg`, for any user's
  `keys`; `ump config key add` alone asks for the name and the key. See
  [keys](keys.md).
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
  refreshes the published profiles, layouts, firewalls and users while
  keeping what you made.
- **The inventory.** `ump server list -o wide` shows prices and your
  attributes, `-o json` feeds anything, `--diff` shows what changed;
  `ump server set NAME --price 89 --attr owner=alice` records what the
  API does not know, `ump server add` a machine no provider lists. See
  [inventory](inventory.md).
- **A shared state, later.** If you skipped step 2, `ump config state
  setup` (or `migrate --to URL`) moves the state to a backend at any
  time, with everything in it, and `clear` brings it back. See [the
  state backend](state.md).
- **Scripts and agents.** Every command has `-o json`; nothing prompts
  without a terminal (`--yes` where something is destructive, exit code 3
  otherwise). See the [CLI reference](cli.md).
- **A storage of your own.** Everything ump puts on a server (the rescue
  image, the installer, the profiles, the OS image) comes from one base
  URL, `https://ump.zhurko.de/dl` by default; [the rescue image's
  README](../images/rescue/README.md#your-own-storage) says what to serve
  to run your own, and `storage:` in the [configuration](configuration.md)
  points ump at it.
