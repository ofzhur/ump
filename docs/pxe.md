# PXE serving

Machines that no provider's API knows, and that have no rescue system
to boot into, ump boots over the network: your own hardware in a rack,
an office or a lab, or servers on an ISP's private network where the
ISP lets you run a PXE server. ump boots them into [UMP's rescue
system](../images/rescue/README.md), and from there `ump server rescue`
and `ump server reinstall` work as they do at every provider. The
machines are described in `pxe.yaml` in the home and become the servers
`pxe/NAME` of the account `pxe`.

PXE serving runs on **private networks only**. The network's DHCP
server hands out the addresses and points the machines at iPXE, and
iPXE at ump: chain mode. ump hands out no addresses and answers no
DHCP. What it adds is one optional process, `ump pxe serve`, the PXE
server of a site, which serves what the CLI rendered and armed and
reports what it saw; it decides nothing of its own.

```console
$ ump config pxe create web-07 --mac 0c:c4:7a:aa:bb:01 --set address=192.0.2.17
$ ump pxe serve --http 192.0.2.5:8067 --tftp 192.0.2.5:69       # on a host of the private network
$ ump server rescue pxe/web-07                                  # arms a network boot; power-cycle the machine
$ ump server reinstall pxe/web-07                               # the same, then the installation
$ ump pxe status                                                # what the PXE servers see
```

```
 your machine (ump)                  the backend (the home)                  the site: a private network
 ump config pxe ...   ──renders──►   .pxe/<site>/site.json         ◄──reads──  ump pxe serve --site SITE
 ump server rescue    ──arms─────►   .pxe/<site>/arm/NAME.json     ◄──reads──    │ HTTP 8067, TFTP 69
 ump server list      ◄──reads────   .pxe/<site>/outcome/NAME.json ◄──writes─    │
 ump pxe status       ◄──reads────   .pxe/<site>/status/HOST.json  ◄──writes─    ▼
                                                                    the site's DHCP server, the machines
```

The CLI and the PXE server never talk to each other: they meet in the
[backend](state.md), which both reach even when you cannot reach the
private network. `.pxe/` is never mirrored, so the servers' heartbeats
cost the CLIs nothing.

## How a machine boots

1. Its firmware asks for an address over DHCP, with its client
   architecture (option 93). The site's DHCP server answers with the
   address, an iPXE binary for that architecture and the TFTP server to
   fetch it from (`ump pxe serve --tftp`, or another).
2. iPXE asks DHCP again, now as iPXE (user class `iPXE`, option 175),
   and gets `http://HOST:8067/boot.ipxe`: the PXE server.
3. `boot.ipxe` is the same for every machine. It asks for the machine's
   own script with what iPXE knows of it: MAC, address, architecture,
   firmware, memory and, on BIOS, whether the CPU has a 64-bit mode.
4. The machine's script boots its disk (iPXE exits, and the firmware
   takes the next boot entry), or UMP's rescue system: kernel and
   initrd over HTTP, verified against the rescue image's manifest and
   cached by the server, with a boot config in front of the initrd (a
   cpio): the hostname, root's keys, DHCP on the NIC it booted from
   alone, the server's URL and a token.
5. The rescue system says hello to the server: its addresses, MACs and
   host keys, the DMI, the disks, the memory, the firmware, the UEFI
   boot order and a BMC's address. `ump server rescue` waits for that
   and connects at the address it reported, with its host keys pinned.

Every failure ends in the disk: no answer from the server, nothing to
serve, a rescue image that is missing or does not verify, a machine
that boots another architecture than `pxe.yaml` says, too little memory
for the rescue system, an x86 BIOS machine without a 64-bit CPU. The
reason shows in `ump pxe status`. A MAC without a machine gets its disk
too.

| Option 93 | Firmware | Kernel ump serves |
|---|---|---|
| 0 | x86 BIOS | amd64 |
| 7, 9 | x86-64 UEFI | amd64, with its EFI stub |
| 11 | arm64 UEFI | arm64, the Image unpacked by the server |
| others | 32-bit EFI, ARM32, ... | none: the disk |

IPv6 network boot is not supported. A boot order that tries "PXE IPv6"
after IPv4 waits minutes for DHCPv6 at every boot: turn it off, or put
the disk right after PXE IPv4.

## Modes

| Mode | Where | The machine boots |
|---|---|---|
| `local` | `pxe.yaml` (the default) | its disk, at every boot |
| `rescue` | `pxe.yaml` | UMP's rescue system at every boot, with UMP's key, `staging.keys` and its `rescue-keys` |
| armed rescue | `ump server rescue`, `reinstall` | UMP's rescue system once, with the keys of the run too |
| `install` | - | refused for now; unattended installations come in a later release |

An armed rescue boots once: it is consumed when its initrd was sent
completely, so a rescue system that does not come up (a NIC without its
driver, a kernel panic) costs one boot, and the next boot is from disk.
The arm lasts as long as `ump server rescue` waits (`wait`, 15 minutes),
a day with `--no-wait` or `--no-reboot`; `ump server unrescue` or `ump
pxe disarm` takes it back. A standing `mode: rescue` whose rescue system
said no hello after three boots within 30 minutes gets its disk for the
rest of them, and `ump pxe status` says why. `reinstall` refuses a
machine in `mode: rescue` (exit code 3): it would boot the rescue
system again, not the new one.

`ump server list` shows the status from the mode, the arm and what the
servers report: `local`, `rescue`, `armed rescue`.

## `pxe.yaml`

`pxe.yaml` lives in the home next to `config.yaml`
(`~/.config/ump/pxe.yaml`), and moves with a shared
[backend](state.md). `ump config pxe` edits it, and every edit through
it ends with `apply`, which checks the file and renders what the PXE
servers of each site serve, `.pxe/<site>/site.json`. A hand edit
reaches no server until `ump config pxe apply`.

```yaml
# pxe.yaml: the machines ump boots over the network (ump config pxe)
unknown: ignore                    # a MAC without an entry: ignore, or local (listed by ump pxe status --unknown)

defaults:                          # under every machine; a site's, then a machine's values replace these
  mode: local                      # local (the disk) or rescue (UMP's rescue system at every boot)
  rescue-keys: [oleg]              # root's keys in the rescue system, besides UMP's and staging.keys
  network:                         # the installed system's uplink
    ipv4: dhcp
    dns: dhcp

sites:                             # optional; without it every machine is in the site default
  fra1:
    description: Frankfurt, rack 1, VLAN 40
    jump: pxe1                     # SSH to its machines goes through this host: an ump server, or [user@]host[:port]
    network:
      ipv4: {gateway: 192.0.2.1}
      dns: [192.0.2.53, 192.0.2.54]

machines:
  web-07:                          # the server pxe/web-07, and its hostname
    site: fra1                     # required when sites exist
    mac: 0c:c4:7a:aa:bb:01         # the MACs it boots from; the first is the uplink
    network:
      ipv4: {address: 192.0.2.17/24}   # the gateway comes from the site
  db-01:
    site: fra1
    mac: [0c:c4:7a:aa:bb:10, 0c:c4:7a:aa:bb:11]
    arch: amd64
    firmware: uefi
    network:
      uplink: 0c:c4:7a:aa:bb:11    # the installed system's interface, when not the first MAC
      ipv4: dhcp
    address: 192.0.2.30            # where ump reaches it: the DHCP server's reservation for that MAC
    note: the old database host
```

| Key | Where | Meaning |
|---|---|---|
| `mac` | machine | one MAC or a list (`-`, `.` and bare hex are taken); the first is the uplink unless `network.uplink` says otherwise. Each MAC is one machine's |
| `site` | machine | the site whose PXE servers serve it; required when `sites` exist, else `default` |
| `mode` | all | `local` or `rescue` |
| `rescue-keys` | all | root's keys in its rescue system, besides UMP's and `staging.keys`: named keys, files, key lines (not `local` or `agent`, which would be another key for everyone who applies the file) |
| `console` | all | the rescue kernel's serial console, such as `ttyS1,115200n8`; `none` for none. amd64 gets `ttyS0,115200n8` and the screen by default, arm64 none: its kernel takes the console from the firmware (SPCR), and a wrong one silences it |
| `network` | all | the installed system's uplink ([below](#the-installed-systems-network)), merged per field |
| `address` | machine | where ump reaches the installed system when its IPv4 is DHCP: the DHCP server's reservation |
| `arch`, `firmware` | machine | `amd64` or `arm64`; `uefi` or `bios`. A machine that boots as another architecture gets its disk; the firmware is for the unattended installations to come |
| `hostname` | machine | the rescue system's and the installed system's hostname; default the name |
| `jump` | `defaults`, site | the host SSH to the machines goes through ([below](#sites-and-jump-hosts)) |
| `description` | site | one line |
| `unknown` | top, site | `ignore` or `local` for a MAC without a machine; both answer with the disk, `local` also lists the MAC in `ump pxe status --unknown` |
| `note` | machine | free text, shown by `ump server show` |

A machine also takes reinstall's values under the names of its flags
(`profile`, `layout`, `firewall`, `os`, `group`, `private-ip`, `disks`,
...), as a [multiinstall plan](cli.md#install-several-ump-server-multiinstall)
does for one server; `ump config pxe list` shows the profile. They are
checked and kept for the unattended installations to come; `ump server
reinstall pxe/NAME` takes its flags, as for any server. `via` and
`unattended` are no keys here.

Values of `defaults` come under every machine, a site's under its
machines', and a machine's own win; a value replaces the one below it,
lists too, and `network` merges per field, so a site gives the gateway
and the resolvers and a machine its address.

The file is checked strictly and with line numbers: one document, no
key twice or unknown, names that are hostnames, each MAC and each
static address once, a site for every machine when `sites` exist. An
invalid `pxe.yaml` is exit code 2, and `ump server list` reports the
account `pxe` as failed (exit code 4) until it is fixed. The keys a
later version will take (`interfaces`, `bond`, `vlans`, `routes`,
`mtu`, `proxy`, `boot-network`, ...) are refused with that reason.

```console
$ ump config pxe list
NAME     SITE   MAC                 STATUS   PROFILE   ADDRESS      SEEN
web-07   fra1   0c:c4:7a:aa:bb:01   local    -         192.0.2.17   2m ago
db-01    fra1   0c:c4:7a:aa:bb:10   rescue   -         192.0.2.30   -
$ ump config pxe get web-07 --merged       # with the defaults' and its site's values
$ ump config pxe create app-01 --mac 0c:c4:7a:aa:bb:20 --site fra1 --set network.ipv4.address=192.0.2.21/24
$ ump config pxe edit web-07               # that machine alone, in your editor; the whole file without a name
$ ump config pxe delete db-01              # from pxe.yaml, the state and its site
$ ump config pxe apply                     # after a hand edit
```

`apply` renders `site.json` for every site, removes it for a site gone
from `pxe.yaml` (its servers then serve nothing), and waits up to 30
seconds for the running servers of a changed site to serve the new one;
servers that do not are named (exit code 5). A change of a machine's
site or MACs is refused while a boot of it is armed (exit code 3), by
every save and by `apply`, against what its site serves: `ump pxe
disarm` it first. `edit` of the whole file saves it once it is valid,
and a machine it takes out leaves the fleet as with `delete`, which it
asks first. `delete` asks first, forgets the machine in the state (with
its private addresses) and removes what its site holds of it. A machine
a hand edit took out of `pxe.yaml` stays in the state until `delete`,
which `apply` says: `pxe.yaml` is ump's own configuration, not a
provider that may lose a server.

### The installed system's network

The rescue system runs DHCP on the NIC it booted from and leaves the
others without addresses. The installed system configures what
`network` says, on the uplink:

```yaml
network:
  uplink: 0c:c4:7a:aa:bb:11          # by MAC; default: the machine's first
  ipv4: dhcp                         # dhcp | none | {address: A/P, gateway: G}
  ipv6: none                         # none | auto | {address: A/P, gateway: G}; only when the profile keeps IPv6
  dns: dhcp                          # dhcp (the lease's) | [addresses]
```

DHCP uses the MAC as the client identifier, as iPXE and the rescue
system do, so a reservation for the MAC holds for all three. A static
address without `dns` gets 1.1.1.1 and 8.8.8.8: a network that blocks
public resolvers gives its own. The uplink may not overlap the
machine's [private networks](networks.md#uplinks-and-private-networks)
(exit code 2, before anything is armed).

ump reaches the installed system at its configured address alone: the
static `network.ipv4` or `address`. An address learned at boot (iPXE's,
the rescue system's) serves the rescue session that armed it and
nothing else; it never reaches `ump ssh`, `ump provision` or the
inventory. A DHCP machine without `address` is listed without an
address, is left out of the inventory, and `ump server reinstall`
refuses it.

## Sites and jump hosts

A site is a private network: its DHCP server, its machines, and one or
more PXE servers (`ump pxe serve --site NAME`) on hosts attached to it.
Without `sites`, every machine is in the site `default`. Every machine
has one site; several servers of one site are fine, each with its own
status, `--data` and `--cache`, and the one the DHCP server's URL names
serves a boot.

The machines of a private network often have no other route, and
neither do you. A site's `jump` (or `defaults.jump` for all) is the
host that SSH to its machines goes through, as ssh's `ProxyJump`: for
`ump ssh`, the rescue system, `reinstall` and its check of the new
system, and for Ansible, whose host variables get
`ansible_ssh_common_args: -o ProxyJump=JUMP -o
StrictHostKeyChecking=accept-new` ([provisioning](provisioning.md)).
It is `[user@]host[:port]`, several hops separated by commas; a hop
that is an ump server's name stands for its administrator at its
address, as `ump ssh` logs in there. The PXE host itself is the usual
choice:

```yaml
sites:
  fra1:
    jump: pxe1                       # the ump server pxe1: ump@192.0.2.5
```

ssh offers the jump host the keys of your own SSH setup (your agent,
`~/.ssh/id_*`, `~/.ssh/config`), not UMP's key, which it offers only to
the machine behind it. A pipeline that holds UMP's key alone puts it
into an agent. A hop named by an ump server resolves when that server's
account is listed (`--account` can leave it out), and a jump host's own
`jump` is not followed.

A private address is often another network's too (`10.0.0.5` behind
the jump host and at the office). Where a server of another account
has a machine's address, the address goes the way of the server a
command acts on, never through the other's jump host; one run acts on
one of them (exit code 2).

## Running `ump pxe serve`

The PXE server is ump itself, run on a host attached to the private
network: a server of the fleet, which ump installed like any other, or
a machine of its own. It needs the backend of the home: `UMP_STATE` and
the backend's credential variables (`AWS_ACCESS_KEY_ID`, ...; see
[the state backend](state.md#backends)), else the home of the user it
runs as. It touches `.pxe/<site>/` there and nothing else of the home:
it reads `site.json` and the arms, and writes the outcomes of the arms
it took and its status. It reads no `pxe.yaml`, never pulls or pushes
the mirror and never checks for updates.

```console
# export UMP_STATE=r2://0123456789abcdef0123456789abcdef/fleet-state/ump AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
# ump pxe serve --site fra1 --http 192.0.2.5:8067 --tftp 192.0.2.5:69 --check
Site fra1: site.json 9dde0265b952e433, 2 machines.
Rescue image 2026.11.01 for amd64: verified, in ~/.cache/ump/pxe/fra1.
HTTP 192.0.2.5:8067: bound.
TFTP 192.0.2.5:69: bound, 10 iPXE binaries in /usr/lib/ipxe: ipxe-amd64.efi, ipxe-arm64.efi, ipxe-loong64.efi, ipxe-riscv64.efi, ipxe.efi, ipxe.lkrn, ipxe.pxe, snponly.efi, undionly.kkpxe, undionly.kpxe.
# ump pxe serve --site fra1 --http 192.0.2.5:8067 --tftp 192.0.2.5:69
pxe: fra1: serving site fra1: HTTP 192.0.2.5:8067, TFTP 192.0.2.5:69 (/usr/lib/ipxe)
pxe: fra1: 0c:c4:7a:aa:bb:01 (amd64 uefi, 192.0.2.17): web-07: rescue (arm 8a92e77a)
pxe: fra1: web-07: the arm 8a92e77a is consumed
pxe: fra1: web-07: rescue system up at 192.0.2.17 (arm 8a92e77a)
```

On the PXE host it runs as a systemd service, `ump-pxe@SITE`, from the
deb or rpm package or from units written by hand ([as a
service](#as-a-service)); for a lab, in a terminal. It tells systemd
when it is ready (`Type=notify`), and on SIGTERM it lets the transfers
under way finish before it stops.

| Flag | Meaning |
|---|---|
| `--site` | the site it serves; default `default` |
| `--http` | where HTTP listens; default `:8067`, every interface. On a host with public addresses, give the private network's address: the server warns otherwise |
| `--url` | where the machines reach it, when that is not the address they asked (NAT, a container, a port forward) |
| `--tftp` | serve the iPXE binaries read-only over TFTP on this address (`192.0.2.5:69`); port 69 needs root or `CAP_NET_BIND_SERVICE` |
| `--ipxe-dir` | the iPXE binaries TFTP serves; default the distribution's ipxe package: `/usr/lib/ipxe` (Debian, Ubuntu), else `/usr/share/ipxe` (Fedora, RHEL, Arch) |
| `--data` | what outlives a restart, its journal of arms and boots; default `$STATE_DIRECTORY`, else `~/.local/state/ump-pxe/SITE` |
| `--cache` | the rescue image; default `$CACHE_DIRECTORY`, else `~/.cache/ump/pxe/SITE` |
| `--poll` | how often it reads the backend; default `10s` |
| `--check` | read, fetch and verify the rescue image, bind the ports, and exit: 0 when it can serve |

Every flag can also come from the environment, as `UMP_PXE_` and its
name in upper case: `UMP_PXE_SITE=fra1`, `UMP_PXE_IPXE_DIR=...`; a flag
wins. `-v` logs every request.

The rescue image comes from `staging.image` (default
`<storage>/rescue`) as `ump config pxe apply` rendered it. It is
checked for size and sha256 against its manifest, rechecked hourly, and
a boot keeps the version it started with; an image without the feature
`boot-config` (one built before PXE serving) boots nothing, and the
status says so. While the backend does not answer, the server
serves what it read last and keeps the arms it consumed in its journal,
to write their outcomes later. A missing `site.json`, one of a newer
ump, or one edited by hand means the disk for every machine, and a
warning in the status.

**Ports.** TCP 8067 and, with `--tftp`, UDP 69, from the private
network alone. TFTP answers every transfer from a port of its own, as
RFC 1350 has it: a stateful firewall lets the answers out, and on a
host with several addresses `--tftp` takes the private network's, so
that the answers come from the address the machines asked.

**What its credential reaches.** The server needs no more than
`.pxe/<site>/`, but on R2 an API token is scoped to a bucket, not to a
prefix, so its token reaches the whole home, and so does an SSH, HTTP
or file backend. Treat the PXE host as a host that holds the home.

### As a service

The deb and rpm packages install ump as `/usr/bin/ump`, with two
systemd units: `ump-pxe@.service`, the server of the site its instance
names, and `ump-pxe-check@.service`, which runs its `--check` once with
the same settings, user and sandbox. Both are disabled: nothing runs
until you enable a site. The packages recommend the distribution's iPXE
binaries for `--tftp` (Debian, Ubuntu: `ipxe`; Fedora, RHEL:
`ipxe-bootimgs-x86` and `ipxe-bootimgs-aarch64`). They are on the
storage, under `pkg/latest/` and `pkg/v/VERSION/` with their
`SHA256SUMS`, and with every
[release](https://github.com/ofzhur/ump/releases).

```console
# curl -fsSLO https://ump.zhurko.de/dl/pkg/latest/ump-amd64.deb && apt install ./ump-amd64.deb   # Debian, Ubuntu; ump-arm64.deb
# dnf install https://ump.zhurko.de/dl/pkg/latest/ump-x86_64.rpm                                 # Fedora, RHEL; ump-aarch64.rpm
# install -m 0600 /usr/share/ump/pxe.env.example /etc/ump/pxe/fra1.env && $EDITOR /etc/ump/pxe/fra1.env
# systemctl start ump-pxe-check@fra1; journalctl -u ump-pxe-check@fra1 -b -o cat --no-pager
# systemctl enable --now ump-pxe@fra1
# journalctl -u ump-pxe@fra1 -f
```

`/etc/ump/pxe/SITE.env` holds a site's settings as environment
variables: the backend (`UMP_STATE`), its secrets, and the flags as
`UMP_PXE_` and their names; the site is the instance's name. systemd
reads the file, the server never does: keep it root's alone (0600).

```sh
# /etc/ump/pxe/fra1.env
UMP_STATE=r2://0123456789abcdef0123456789abcdef/fleet-state/ump
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
UMP_PXE_HTTP=192.0.2.5:8067
UMP_PXE_TFTP=192.0.2.5:69
```

The server runs as a user systemd makes up for it (`DynamicUser=`),
never as ump's administrator `ump`, with the capability to bind port 69
alone, and writes nothing but its directories: `/var/lib/ump-pxe/SITE/`
(`--data`) and `/var/cache/ump-pxe/SITE/` (`--cache`). The check has
its own (`ump-pxe-check/SITE`), so it never takes the service's, and
says "the address is in use" while the service runs.

**The backend** of a service is one it reaches over the network with
the secrets of its env file or credentials: S3, R2, HTTP, Consul or
Vault. A `file://` or `ssh://` backend is for `ump pxe serve` run by
hand as the user whose home it is: the service's user can read none of
the files ump writes into a directory (0600, the user's), and has no
SSH keys.

**Credentials.** The backend's secrets (`AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `UMP_STATE_TOKEN`, `VAULT_TOKEN`,
`CONSUL_HTTP_TOKEN`, `UMP_STATE_USER`, `UMP_STATE_PASSWORD`) can come
as systemd credentials of those names instead, which win over the env
file's; encrypted with the host's key or its TPM, they are no plain
text on disk. `LoadCredential=` takes a plain file the same way.

```console
# systemd-creds encrypt --name=AWS_SECRET_ACCESS_KEY - /etc/ump/pxe/fra1.secret   # the secret on stdin, then Ctrl-D
```

```ini
# systemctl edit ump-pxe@fra1, and the same for ump-pxe-check@fra1
[Service]
LoadCredentialEncrypted=AWS_SECRET_ACCESS_KEY:/etc/ump/pxe/fra1.secret
```

**Upgrades.** A newer package restarts the servers that run, each after
the transfers under way (`TimeoutStopSec=90`); the journal in `--data`
carries the arms a server took across the restart. A packaged ump's
update hint names its package, not `get.sh`, which would put a second
ump into `/usr/local/bin`. Removing the package stops and disables
every site, and `apt purge` also removes `/var/lib/ump-pxe` and
`/var/cache/ump-pxe`; `/etc/ump/pxe/` stays.

**Without the packages**, on any host with systemd, ump comes from
`get.sh` and the units from ump itself, which prints them as the
packages have them, `--exec` naming where ump is:

```console
# curl -fsSL https://ump.zhurko.de/dl/get.sh | sh                                 # /usr/local/bin/ump
# ump pxe unit --exec /usr/local/bin/ump > /etc/systemd/system/ump-pxe@.service
# ump pxe unit --check --exec /usr/local/bin/ump > /etc/systemd/system/ump-pxe-check@.service
# systemctl daemon-reload
# install -d /etc/ump/pxe && install -m 0600 /dev/null /etc/ump/pxe/fra1.env && $EDITOR /etc/ump/pxe/fra1.env
```

Then the same steps: the check, `systemctl enable --now ump-pxe@fra1`,
the journal. The iPXE binaries come from the distribution's ipxe
package. To upgrade, run `get.sh` again and `systemctl restart
'ump-pxe@*'`; write the units again when a release says they changed.

This is `ump-pxe@.service`. The check's unit has the same lines but
runs `ump pxe serve --site %i --check` once (`Type=oneshot`, no
`Restart=`, no `[Install]`), in directories of its own.

```ini
# ump-pxe@.service: ump pxe serve, the PXE server of the site the
# instance names (systemctl enable --now ump-pxe@fra1). Its settings are
# in /etc/ump/pxe/SITE.env: UMP_STATE, the backend's secrets (or systemd
# credentials of their names, docs/pxe.md), the UMP_PXE_ variables of the
# flags. ump pxe unit prints this unit.
[Unit]
Description=UMP PXE server, site %i
Documentation=https://github.com/ofzhur/ump/blob/main/docs/pxe.md
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/ump pxe serve --site %i
EnvironmentFile=/etc/ump/pxe/%i.env
DynamicUser=yes
StateDirectory=ump-pxe/%i
CacheDirectory=ump-pxe/%i
UMask=0077
Restart=on-failure
RestartSec=5
TimeoutStopSec=90
# TFTP on port 69 (--tftp); HTTP on 8067 needs nothing. No PrivateUsers=:
# with it, the capability binds no port of the host's network.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectClock=yes
ProtectHostname=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
```

In the fleet, the role
[ofzhur.ansible.ump_pxe](https://github.com/ofzhur/ansible/blob/main/roles/ump_pxe/README.md)
does this on a host ump installed: it installs the package, writes the
site's env file and credentials, enables `ump-pxe@SITE` and opens HTTP
and TFTP on the private network alone (through the collection's
firewall, on Ubuntu and Debian).

### `ump pxe status` and `disarm`

```console
$ ump pxe status
SERVER   SITE   VERSION   URL                     UPDATED   SITE.JSON
pxe1     fra1   v0.8.0    http://192.0.2.5:8067   4s ago    9dde0265b952e433 (current)

NAME     SITE   MAC                 STATUS         ANSWER   SEEN      BY     ADDRESS
web-07   fra1   0c:c4:7a:aa:bb:01   local          rescue   16s ago   pxe1   192.0.2.17
db-01    fra1   0c:c4:7a:aa:bb:10   armed rescue   local    3d ago    pxe1   192.0.2.30
$ ump pxe status web-07 -o wide           # one machine, with its rescue system's address
$ ump pxe status --unknown                # MACs without a machine the servers saw (unknown: local)
$ ump pxe disarm web-07                   # take an armed boot back
```

`status` shows the servers of each site, whether they run and serve
the `site.json` in the backend and what they warn of, and the machines:
their status (what `pxe.yaml` and their arm say now), what their last
network boot got (`ANSWER`) and why the disk when their arm or mode
said otherwise, the address iPXE had, and the signals of the servers:

- the rescue system does not come up: three boots without a hello
  within 30 minutes (a NIC driver, the firmware: see the console);
- its disk does not boot: five network boots within 15 minutes of a
  machine that gets its disk;
- the boot order starts with another entry than the network boot it
  came from (UEFI, from the hello's `efibootmgr`).

## The DHCP server

The site's DHCP server is asked twice per boot. The firmware's PXE
asks first and gets an iPXE binary for its client architecture (option
93) and the TFTP server (`next-server`); iPXE then asks as iPXE, with
the user class `iPXE` (option 77) and option 175, and gets the URL of
the PXE server. A DHCP server that hands iPXE the binary again makes it
load itself forever, so every recipe tells iPXE apart. Machines whose
NIC boots iPXE itself (QEMU, a NIC flashed with iPXE) need the URL
alone.

The iPXE binaries come from the distribution's ipxe package. Serve them
with `ump pxe serve --tftp` (the directory is the package's), or from a
TFTP server you have.

| Option 93 | Firmware | Debian, Ubuntu: `ipxe`, `/usr/lib/ipxe` | Fedora, RHEL: `ipxe-bootimgs-*`, `/usr/share/ipxe` |
|---|---|---|---|
| 0 | x86 BIOS | `undionly.kpxe` | `undionly.kpxe` |
| 7, 9 | x86-64 UEFI | `snponly.efi` | `ipxe-snponly-x86_64.efi` |
| 11 | arm64 UEFI | `ipxe-arm64.efi` (Debian 13; Ubuntu 24.04 has none) | `arm64-efi/snponly.efi` |

`snponly.efi` drives the NIC with the firmware's own driver; where the
firmware's is broken, Debian's `ipxe.efi` brings iPXE's own drivers,
and its `undionly.kkpxe` is for BIOSes that hang when iPXE exits to
the disk.
The recipes take 192.0.2.5 as the PXE host, with `--tftp` and HTTP on
8067; replace the file names with your distribution's.

### dnsmasq

```ini
# /etc/dnsmasq.d/pxe.conf
dhcp-range=192.0.2.100,192.0.2.199,12h
dhcp-match=set:ipxe,175                        # iPXE sends option 175 (or: dhcp-userclass=set:ipxe,iPXE)
dhcp-match=set:bios,option:client-arch,0
dhcp-match=set:efi64,option:client-arch,7
dhcp-match=set:efi64,option:client-arch,9
dhcp-match=set:arm64,option:client-arch,11
dhcp-boot=tag:ipxe,http://192.0.2.5:8067/boot.ipxe
dhcp-boot=tag:!ipxe,tag:bios,undionly.kpxe,,192.0.2.5
dhcp-boot=tag:!ipxe,tag:efi64,snponly.efi,,192.0.2.5
dhcp-boot=tag:!ipxe,tag:arm64,ipxe-arm64.efi,,192.0.2.5
dhcp-host=0c:c4:7a:aa:bb:10,192.0.2.30         # a reservation: address in pxe.yaml
```

dnsmasq can serve the binaries itself instead (`enable-tftp`,
`tftp-root=/usr/lib/ipxe`, and its own address as the last field of
`dhcp-boot`).

### ISC dhcpd

```
# /etc/dhcp/dhcpd.conf
option client-architecture code 93 = unsigned integer 16;
subnet 192.0.2.0 netmask 255.255.255.0 {
  range 192.0.2.100 192.0.2.199;
  option routers 192.0.2.1;
  option domain-name-servers 192.0.2.53;
  next-server 192.0.2.5;
  if exists user-class and option user-class = "iPXE" {
    filename "http://192.0.2.5:8067/boot.ipxe";
  } elsif option client-architecture = 00:07 or option client-architecture = 00:09 {
    filename "snponly.efi";
  } elsif option client-architecture = 00:0b {
    filename "ipxe-arm64.efi";
  } else {
    filename "undionly.kpxe";
  }
}
host db-01 { hardware ethernet 0c:c4:7a:aa:bb:10; fixed-address 192.0.2.30; }
```

### Kea

```json
"Dhcp4": {
  "client-classes": [
    { "name": "ipxe", "test": "substring(option[77].hex,0,4) == 'iPXE'",
      "boot-file-name": "http://192.0.2.5:8067/boot.ipxe" },
    { "name": "bios", "test": "not member('ipxe') and option[93].hex == 0x0000",
      "next-server": "192.0.2.5", "boot-file-name": "undionly.kpxe" },
    { "name": "efi64", "test": "not member('ipxe') and (option[93].hex == 0x0007 or option[93].hex == 0x0009)",
      "next-server": "192.0.2.5", "boot-file-name": "snponly.efi" },
    { "name": "arm64", "test": "not member('ipxe') and option[93].hex == 0x000b",
      "next-server": "192.0.2.5", "boot-file-name": "ipxe-arm64.efi" }
  ],
  "subnet4": [ { "id": 1, "subnet": "192.0.2.0/24",
    "pools": [ { "pool": "192.0.2.100 - 192.0.2.199" } ],
    "reservations": [ { "hw-address": "0c:c4:7a:aa:bb:10", "ip-address": "192.0.2.30" } ] } ]
}
```

iPXE sends its user class as raw bytes, not in RFC 3004's form, hence
the `substring` over option 77's bytes.

### Asking your ISP

On an ISP's private network, the ISP's DHCP server (where it runs one)
does this for you, if the ISP allows a PXE server there and can set
boot files per client class. Ask for the machines' MACs, or the whole
network:

- next-server: the PXE host's private address (192.0.2.5), where TFTP
  serves the iPXE binaries;
- boot file by client architecture (option 93): 0 `undionly.kpxe`, 7
  and 9 `snponly.efi`, 11 the arm64 binary;
- for clients with the user class `iPXE` (option 77), or option 175:
  `http://192.0.2.5:8067/boot.ipxe`;
- a reservation per machine whose installed system takes DHCP: its
  `address` in `pxe.yaml`.

A DHCP server that sets one boot file for every client, or none (many
routers and CPEs), cannot chain. Where nobody else's DHCP runs on the
network, run your own (dnsmasq on the PXE host, above); where one runs
that you cannot change, dnsmasq in proxy mode on the PXE host adds the
boot files and leaves the addresses to it (`dhcp-range=192.0.2.0,proxy`
and `pxe-service`; see dnsmasq's manual).

## Troubleshooting

**Secure Boot.** Neither the distributions' iPXE binaries nor UMP's
rescue system boot with Secure Boot on: turn it off, as for any
provider's rescue system. With it on, the firmware fetches the iPXE
binary over TFTP and refuses to start it, and goes on to the next boot
entry; the PXE server never sees `/boot.ipxe`.

**A machine that never boots from the network** never shows in `ump
pxe status` (with `unknown: local`, a wrong MAC in `pxe.yaml` shows in
`--unknown`). Check, from the machine outwards:

- the firmware: network boot (PXE, "Network Stack") enabled for the
  NIC, IPv4, and first in the boot order;
- the switch port: edge or portfast, or spanning tree holds the link
  past the firmware's DHCP timeout; LACP with fallback (a bond's ports
  come up one by one under PXE); an untagged VLAN, as the firmware
  tags nothing; MAC authentication on 802.1X ports;
- the DHCP server's log, or `tcpdump -ni IF port 67 or port 68 or port
  69` on the PXE host: an offer without a boot file, a binary that is
  fetched but never followed by iPXE's request (Secure Boot, or a
  binary for another architecture), iPXE that fetches the binary again
  (the DHCP server does not tell iPXE apart);
- no other PXE server on the network (WDS, MAAS, Foreman, FOG,
  Cobbler, a router's netboot option), which may answer first.

**The boot order.** Every boot asks the PXE server, and the network
stays first: an armed boot works without power control, and a machine
whose mode is `local` gets its disk from the next boot entry. ump never
changes the boot order: the systems it installs boot from the EFI
removable path without a boot entry of their own (`grub-install
--removable --no-nvram`). A firmware update, a CMOS reset or another
system's installer can put a disk first; then the status says that the
boot order starts with another entry, and an armed boot is not taken
(`ump server rescue` says after 3 minutes that no network boot was
seen).

**The rescue system does not come up.** The console says why (a NIC
without its driver, too little memory, a kernel that panics and
reboots after 30 seconds). An armed rescue is consumed at its first
boot, so the machine is back on its disk; a standing one stops after
three boots without a hello. `console:` in `pxe.yaml` names the
serial console of a machine whose output goes elsewhere.

**`ump server rescue` says no PXE server takes the arm.** It waits 30
seconds for a running server of the machine's site to take the arm:
check that `ump pxe serve --site SITE` runs with the same backend
(`ump pxe status` shows when each server last wrote its status) and
serves the current `site.json`.

## Security

The private network is trusted by nature: DHCP, TFTP and HTTP are
unauthenticated, and without Secure Boot anyone on the network can boot
any machine into anything. Keep PXE on a network of its own, with the
egress an installation needs (HTTPS to the storage and the
distributions' mirrors, DNS, NTP).

- What a machine gets is no secret: the boot config travels over plain
  HTTP and holds public keys, the hostname and a token for the hello.
  A machine on the network that claims a known MAC gets those, and with
  the token it can report in as that machine's rescue system, so that
  `ump server rescue` or `reinstall` connects to it. A hello counts
  only from a boot whose initrd was sent to the end, and for an armed
  rescue only from the boot that consumed the arm, so such a machine
  takes the arm from the real one, which then boots its disk. Keep the
  network to machines you trust.
- Addresses learned at boot never reach the inventory, `ump ssh` or
  `ump provision`; the rescue session that armed a boot connects at the
  reported address with the reported host keys pinned.
- A MAC without a machine gets its disk.
- The PXE server runs nothing a client sends: hellos are data.

## Testing

`make pxe-test` boots a QEMU machine against `ump pxe serve`, built
from the tree, in a home of its own, on QEMU's user network, whose DHCP
server stands for the site's and hands out the URL. It checks a rescue
by `ump server rescue --via provider` through the site's jump host, the
boot config (the hostname, root's keys by MAC, DHCP on the boot NIC
alone), the arm consumed at the next boot, and a MAC without a machine;
`TFTP=1` also fetches an iPXE binary from `--tftp`. It needs the rescue
image built (`make rescue-image`). UEFI has the firmware start the
distribution's iPXE binary (Debian's package, downloaded and checked,
where the host has none); `FIRMWARE=bios` boots amd64 with SeaBIOS and
QEMU's own iPXE ROM. It runs before a release, not in `make check`.
