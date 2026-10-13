# CLI

Everything happens to servers: `ump server ...` lists, shows, installs and
controls them, `ump ssh` logs in, `ump config isp` sets [provider
accounts](providers.md) up, and `ump config os`, `disk-layout` and
`firewall` manage the [profiles](profiles.md), [disk
layouts](disk-layouts.md) and [firewalls](firewalls.md). `ump --help`
and `ump COMMAND --help` say the same at the terminal.

## Conventions

- Servers are addressed by a **server reference**: `provider/id`, a
  provider ID, a name or an IP address (see
  [concepts](concepts.md#servers-and-references)). Several references
  can be given where it makes sense. Names need not be unique: `list`
  lists every match (with a note on stderr), commands that act on one
  server ask which one is meant on a terminal and otherwise fail with
  the candidates; `provider/id` and `--account` disambiguate.
- Every listing supports `-o table|wide|json|yaml`. JSON output is stable
  and meant for scripting.
- Destructive commands (reinstall, rescue, reboot, stop, delete) ask for
  confirmation on a terminal; `--yes` skips the question and is required
  without one.
- Global flags: `--config <file|dir>`, `--account <name>` (repeatable),
  `-v/--verbose` (log the API requests to stderr).
- UMP never prompts when stdin is not a terminal. It fails with exit
  code 3 and says what was needed.

## Overview

```
ump
├── server (s)
│   ├── list (ls, l)  [ref...] [-o table|wide|json|yaml|name] [--no-headers] [--diff] [--since <t>] [--cached]
│   │                                                  all servers of all accounts (--account to restrict);
│   │                                                  what changed since the last listing; the cached listing
│   ├── show (describe) <ref>... [-o text|json|yaml]   everything the provider knows about a server
│   ├── reinstall (install) <ref> [--profile <name|file>] [--layout <name>] [--firewall <name>] [--os <name>] [--hostname <name>] [--ssh-key <file>]... [--ipv6] [--mitigations] [--disks <list>] [--timezone <zone>] [--storage <url>] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes] [-o json|yaml]
│   ├── rescue       <ref> [--image <url>] [--via auto|direct|provider] [--no-image] [--ssh-key <file>]... [--no-reboot] [--no-wait] [--no-connect] [--yes] [-- command]
│   ├── unrescue     <ref> [--reboot] [--wait] [--yes]
│   ├── reboot       <ref> [--hard|--power-cycle] [--wait] [--yes]
│   ├── start        <ref> [--wait]
│   ├── stop         <ref> [--hard] [--yes]
│   ├── rename       <ref> <new-name> [--force]        set the name at the provider (hostname at the next install)
│   ├── rdns (ptr)
│   │   ├── get      [ref...] [-o table|json|yaml]     reverse DNS entries of servers' public addresses
│   │   ├── set      <ref> [ptr-name] [--ip <addr>]    set the PTR of the primary IPv4 (or --ip); default: the server name
│   │   └── delete   <ref> [--ip <addr>]               remove the PTR
│   └── set          <ref> --profile <name> | --clear  remember the profile the server is reinstalled with
├── ssh              <ref> [--user <name>] [--staging] [-- command]
├── config
│   ├── isp                                            interactive wizard: add an account, test it, save it
│   │   ├── view                                       the configuration, secrets redacted
│   │   ├── types                                      provider types and their settings
│   │   ├── add      <type> <name> --set k=v ...       non-interactive form (CI, agents)
│   │   ├── remove   <name>
│   │   └── test     [<name>...]                       check credentials and API access
│   ├── os                                             interactive wizard over the profiles
│   │   ├── list                                       the profiles in ~/.config/ump/profiles
│   │   ├── get      <name>                            print one
│   │   ├── create   <name> [--from <name>]            guided on a terminal (image, layout, firewall, users, ...); --from copies one
│   │   ├── edit     <name>                            your editor, then a check
│   │   ├── delete   <name> [--yes]
│   │   └── update                                     refresh the published files; edited ones are kept
│   ├── disk-layout (dl)                               interactive wizard over the named layouts of layouts.yaml
│   │   ├── list                                       the layouts, with their parts and whether they are valid
│   │   ├── get      <name>                            print one
│   │   ├── create   <name> [--from <name>]            copy one into the file and open it in your editor
│   │   ├── edit     [<name>]                          your editor on layouts.yaml, then a check of every layout
│   │   └── delete   <name> [--yes]
│   ├── firewall (fw)                                  interactive wizard over the named firewalls of firewalls.yaml
│   │   ├── list                                       the firewalls, with their policy and what they allow
│   │   ├── get      <name>                            print one
│   │   ├── create   <name> [--from <name>]            copy one into the file and open it in your editor
│   │   ├── edit     [<name>]                          your editor on firewalls.yaml, then a check of every firewall
│   │   └── delete   <name> [--yes]
│   ├── path                                           where the configuration lives
│   └── key      [-o json|yaml]                        UMP's own SSH public key (generated on first use)
├── completion
└── version
```

## `ump server list`

```
ump server list [NAME|ID|IP|PROVIDER/ID ...] [--account <name>...]
                [-o table|wide|json|yaml|name] [--no-headers]
```

Lists dedicated servers and cloud instances from every configured account
(`--account` restricts it), queried in parallel. Positional arguments
select single servers by name, provider ID, IP address (an address inside
the machine's IPv6 prefix matches too) or `provider/id`. An account whose
API fails is reported on stderr, the others are still listed, and the exit
code is 4. A reference that matches nothing gives exit code 1.

`-o json` is a plain array of machines with stable field names:

```json
[
  {
    "provider": "hetzner-robot",
    "account": "hrobot",
    "id": "123456",
    "name": "web-01",
    "kind": "bare-metal",
    "ipv4": "203.0.113.10",
    "ipv6": "2001:db8:1:2::/64",
    "location": "fsn1-dc7",
    "product": "AX41-NVMe",
    "status": "ready",
    "labels": {"env": "prod"}
  }
]
```

`status` is the provider's own status, lower-cased (`ready`, `running`,
`off`, `cancelled`, ...); `location` is the datacenter or region,
lower-cased; `labels` are present where the provider has them. `-o wide`
adds the IPv6, product and kind columns; `-o name` prints `provider/id`
per line. `defaults.output` in the configuration sets the default format.

### The listing cache and `--diff`

Every successful listing of an account (by `list`, `show` or `config isp
test`) is recorded under `~/.cache/ump/listings/<config>/<account>/`. A
new snapshot is written only when the listing changed, so the cache holds
the history of observed states (the newest 50 per account), and deleting
it is always safe.

```
ump server list --diff                  # since the previous listing
ump server list --since 7d              # since the state as of a week ago (also 12h, 2026-10-01, RFC 3339)
ump server list --cached [ref...]       # the last listing, without any API call
```

`--diff` lists the current servers, compares them with the previous
snapshot of each account and prints only what changed, with a `CHANGE`
column (`added`, `removed`, `changed`) and a `DETAILS` column naming the
changed fields (`status: ready → off`); removed servers show their last
known state. A one-line summary goes to stderr (`1 added, 0 removed, 1
changed, 3 unchanged since 2026-10-05 09:00 CEST.`), as does a note for an
account that has no earlier snapshot. `-o name` prints `+`, `-` or `~`
before each reference; `-o json` and `-o yaml` print a report:

```json
{
  "accounts": [{"account": "hrobot", "compared": true, "since": "2026-10-05T07:00:00Z"}],
  "changes": [
    {"change": "added", "machine": {"provider": "hetzner-robot", "id": "123457", "...": "..."}},
    {"change": "changed", "fields": ["status"], "before": {"...": "..."}, "machine": {"...": "..."}},
    {"change": "removed", "machine": {"...": "..."}}
  ],
  "unchanged": 3
}
```

The exit code does not depend on whether something changed; it is 4 when
an account could not be listed, as for a plain listing.

## `ump server show`

```
ump server show NAME|ID|IP|PROVIDER/ID ... [--account <name>...] [-o text|json|yaml]
```

Finds each server across the configured accounts with the same references
as `list`. A name that matches several servers is asked about on a
terminal (the question goes to stderr, so `-o json` output stays clean);
without a terminal the command fails with the candidates, and
`provider/id` or `--account` picks one. Prints the common fields followed
by everything the provider reports about it, grouped in sections:

| Provider | Sections |
|---|---|
| `hetzner-robot` | Contract (product, traffic, paid until, cancellation, storage box), Addresses, Features (reset, rescue, VNC, ...), Power (state and reset types from `/reset`), Rescue (active, systems, authorized keys from `/boot/rescue`) |
| `hetzner-cloud` | Server (created, locked, rescue, backups, volumes), Type, Image, Location, Network (PTR records, floating IPs, private networks, firewalls), Traffic, Protection |
| `ovh` dedicated | Server, Location, Boot (boot option, root device, rescue mail), Addresses, Service (creation, expiration, renewal), Hardware (memory, CPU, disks, boot mode) |
| `ovh` instance | Instance (created, plan, billing), Flavor, Image, Addresses, Access (SSH key), Project |

A section whose extra API call fails is kept with an `error` field, so a
partial description stays honest. `-o json` and `-o yaml` always print an
array; each element has the machine fields of `server list` plus
`details`, a list of `{"name": ..., "fields": {...}}` with typed values
(sizes in bytes as numbers, booleans as booleans):

```console
$ ump server show web-01 -o json | jq '.[0].details[] | select(.name == "Power").fields'
{
  "state": "running",
  "reset_types": ["sw", "hw", "man"]
}
```

Exit codes: 1 when a reference matched nothing or was ambiguous without a
terminal, 3 when the question was left with `q`, 4 when a provider call
failed or an account could not be listed.

## `ump server rename`

```
ump server rename NAME|ID|IP|PROVIDER/ID NEW-NAME [--force] [-o json|yaml|name]
```

Sets the name the server is known by at its provider: the server name at
Hetzner Robot and Hetzner Cloud, the instance name of an OVHcloud Public
Cloud instance (OVHcloud dedicated servers cannot be renamed through the
API, and the command says so).
The name is a label at the provider: the hostname of an installed
operating system does not change. UMP uses the provider name as the
machine name everywhere, including the hostname it sets when it installs
the machine, so the new name has to be a valid hostname (lower-case
letters, digits and hyphens, dots between labels).

A name that another listed server already has is refused, so that renames
do not create the duplicates that make references ambiguous; `--force`
renames anyway. The server reference is resolved like for `show`
(duplicates are asked about on a terminal). Without `-o` the result is
one sentence; `-o json` prints the renamed server as `list` would. Exit
codes: 1 for not found, ambiguous without a terminal, a name in use, or a
provider that cannot rename; 2 for an invalid name; 4 when the provider
refused.

Reverse DNS is not touched; see `ump server rdns set`.

## Reverse DNS: `ump server rdns`

```
ump server rdns get [NAME|ID|IP|PROVIDER/ID ...] [-o table|json|yaml]
ump server rdns set NAME|ID|IP|PROVIDER/ID [PTR-NAME] [--ip <address>] [-o json|yaml]
ump server rdns delete NAME|ID|IP|PROVIDER/ID [--ip <address>]
```

`rdns get` lists the reverse DNS (PTR) entries of servers' public
addresses: the primary IPv4 always, plus every other address of the
server that has an entry (an IPv6 address in the server's prefix, an
additional IP). `rdns set` sets the entry of the primary IPv4, or of the
address given with `--ip`, which must belong to the server; without a
PTR name the server's name is used, which is the convention: the machine
name is the hostname is the reverse DNS. `rdns delete` removes the entry;
Hetzner Cloud then shows its default again.

Providers differ: Hetzner Robot and Hetzner Cloud accept any name;
OVHcloud checks that the name resolves to the address first, so create
the forward record before setting the PTR there. Providers without
reverse DNS support are noted on stderr by `rdns get` and refused with
exit code 1 by `set` and `delete`. `ump config isp types` shows which
capabilities each provider type has.

## Power: `ump server reboot|start|stop`

```
ump server reboot REF [--hard | --power-cycle] [--wait] [--yes]
ump server start REF [--wait]
ump server stop REF [--hard] [--yes]
```

`reboot` asks the operating system to reboot (ACPI) unless `--hard`
resets the machine or `--power-cycle` cuts the power and switches it on
again; a server that is switched off is switched on instead. `stop` asks
the operating system to shut down unless `--hard` cuts the power. The
commands return when the provider accepted the action (Hetzner Cloud
actions are followed to completion); `--wait` then waits until SSH stops
answering and answers again (exit code 5 after 10 minutes).

Rebooting and stopping are disruptive: on a terminal they ask for
confirmation, without one they need `--yes` (exit code 3 otherwise), as
the [safety model](concepts.md#safety) says. Provider differences:
Hetzner Robot presses the power button for on and off and has no reset
for a switched-off server; OVHcloud dedicated servers know one reboot (a
hardware reset) and cannot be switched on or off through the API.

## Rescue: `ump server rescue|unrescue`

```
ump server rescue REF [--image URL] [--via auto|direct|provider] [--no-image] [--ssh-key FILE]... [--no-reboot] [--no-wait] [--no-connect] [--yes] [-o json|yaml] [-- COMMAND...]
ump server unrescue REF [--reboot] [--wait] [--yes]
```

`rescue` brings the server into UMP's own rescue system, the Debian image
from [`images/rescue/`](../images/rescue/README.md) that runs from RAM
with the disks untouched, waits until it answers and hands the terminal
over to `ssh`, so you land in its root shell; exiting it returns to your
shell. With a command after `--`, the command runs there instead and its
exit status becomes ump's, which is the form for scripts and agents
(`ump server rescue web-01 --yes -- lsblk`). `--no-connect` prints the
`ssh` command instead, as does a terminal-less run without a command;
`-o json` prints the details (address, the way the server got there and
the image version, the provider's password where it issues one, the
`ssh` command) without connecting.

How the server gets there depends on what answers, and ump says so on
the way:

1. The server already runs the UMP rescue system: ump connects as it is.
2. The running system lets UMP in over SSH as root, or as a sudo user
   where root cannot log in (ump tries root, then the administrator of
   the profile set for the server, then `ump`, the default profile's),
   with UMP's own key (see [`ump config key`](#ump-config-key)) or your
   own keys: ump has
   the server fetch the entry script from the image's base URL and run
   it; the script downloads the image, carries the address, gateway,
   DNS, hostname and keys over and kexecs into it, with no reboot
   through the provider. The running system is not shut down; its file
   systems are synced and remounted read-only first.
3. Otherwise the provider's rescue system is enabled with UMP's key and
   any `--ssh-key` files (registering keys the provider does not know
   yet), the server is reset into it, and once it answers ump reports
   it and loads the image from there the same way.

The terminal is handed over only once the UMP rescue system answers with
its version. `--via direct` or `--via provider` forces one way (the
first fails when there is no root access instead of rebooting the
server); `--no-image` stops in the provider's rescue system, for the
provider's own tools such as `installimage`. The script and the image
come from the base URL in the configuration (`staging.image`, default
the image published for this project) or `--image`; the server must
reach it, and [`images/rescue/README.md`](../images/rescue/README.md)
says what to serve there for a storage of your own.

`--no-reboot` only arms the provider's rescue system for the next boot,
`--no-wait` returns right after the reset; both stop at the provider's
rescue system and print the `ssh` command for later. Rescue systems have
a fresh host key every boot, so the connections skip host key checks;
providers that report the host key fingerprints (Hetzner Robot) have
them printed for comparison. A provider rescue system that does not take
UMP's key (one that only issues a password) is reported together with
the commands to load the image by hand. Exit status 5 means the server
did not answer in time: no root access with `--via direct`, or the
rescue system never came up after the kexec (reset the server with
`ump server reboot --hard` and look at its console).

`unrescue` disables the provider's rescue system so that the next boot
is from disk, and with `--reboot` resets the server now. The UMP rescue
system itself runs from RAM, so a reset leaves it; whether that boots
from disk depends on the provider: Hetzner clears its rescue flag once
its rescue system has booted, OVHcloud dedicated servers keep rescue as
their boot option until `unrescue`.

## Install: `ump server reinstall`

```
ump server reinstall REF [--profile NAME|FILE] [--layout NAME] [--firewall NAME] [--os NAME] [--hostname NAME] [--ssh-key FILE]... [--ipv6] [--mitigations] [--disks LIST] [--timezone ZONE] [--storage URL] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes] [-o json|yaml]
```

Puts an operating system on the server's disks from a [UMP base
image](../images/ubuntu/README.md) the way a [profile](profiles.md)
says, and boots it. A server whose provider rescue system takes no SSH
key (an OVHcloud VPS) is first reinstalled by the provider with its
stock Linux and UMP's key, which `reinstall` may do since it erases the
server anyway; `rescue` never does. The profile (`--profile`, default `default`: a file
of that name in `~/.config/ump/profiles/`, else the published one, or a
path) names the image, the disk layout (by name from `layouts.yaml`, or
its own), the users with their keys, extra packages, what the firewall
lets in, and the provision step after the first boot. The server is
brought into the UMP rescue system the way `rescue` does it (as it is
when it runs it already, by a kexec from the running system when that
lets UMP in as root, else through the provider's rescue system); the
[installer](../install/README.md) is fetched from the storage and run
there with the resolved profile, with its progress shown; the server is
rebooted into the new system; ump waits until it answers over SSH with
UMP's key and reports what came up:

```console
$ ump server reinstall web-01
Install ubuntu-26.04 on hetzner-cloud/123 (web-01, 203.0.113.10) as web-01? Every disk of the server is erased. [y/N] y
Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 24.04.3 LTS).
Loading the UMP rescue image from https://ump.zhurko.de/rescue on web-01, without the provider's rescue system.
...
Waiting for the UMP rescue system on 203.0.113.10 ... up after 11s (ump-rescue 2026.10.13).
Installing ubuntu-26.04 on web-01 as web-01 ...
==> machine: amd64, BIOS firmware, 2 CPUs, 937 MB RAM
==> disks (1): /dev/sda (19.1G SSD)
==> layout lvm-linear: /dev/sda gets 1 MiB BIOS boot, 512 MiB EFI (vfat), 1 GiB /boot (ext4) and the rest as LVM; ...
...
==> installed: ubuntu-26.04 2026.10.13 (kernel 7.0.0-39-generic) as web-01 on vg0/root (17000 MB over 1 disk(s)); reboot to start it
Rebooting web-01 into ubuntu-26.04 ...
Waiting for SSH on 203.0.113.10 to go down ... down after 4s.
Waiting for SSH on 203.0.113.10 ... up after 18s.
web-01 is up: Ubuntu 26.04 LTS, hostname web-01, root on /dev/mapper/vg0-root.
```

Every disk of the server is erased. The default profile's layout,
`lvm-linear`, puts the boot partitions on the first disk and makes every
disk a physical volume of one volume group, with root as one linear
volume over all of it; `lvm-striped`, `lvm-raid1` and `lvm-example` are
the other published [layouts](disk-layouts.md), and a profile can carry
its own (`--disks` limits the disks). The new system gets the server's
name as hostname (`--hostname` for another; it must be a valid one), the
address, gateway and interface of the rescue system (which `rescue`
carried over from the running system), uniform interface names (`ext01`,
`int01`, ...; see [the profiles](profiles.md#interface-names)), resolvers
1.1.1.1 and 8.8.8.8, the profile's users with their keys (the default:
the sudo user `ump` with UMP's key and your `~/.ssh/id_*.pub` keys, and
no root login over SSH; `--ssh-key` adds files to the administrator;
`agent` in a profile takes what `ssh-add -L` lists), the time zone
(`--timezone`), and a kernel command
line with `mitigations=off`, `ipv6.disable=1` and `kho=off` (the kernel's
kexec handover reserves memory for live updates and gets in the way of
ump's own re-entry). The first two cost performance and most servers
need neither; the profile's `kernel` section, `--mitigations` and
`--ipv6` keep them on, and [the installer's
README](../install/README.md#turning-ipv6-or-the-mitigations-back-on)
says how to turn them on later. The flags override the profile for one
installation: `--layout`, `--firewall`, `--os`, `--timezone`, `--ipv6`,
`--mitigations`, `--disks`, `--ssh-key`; `--storage` names another
storage than the configured one for the installer, the images, the
profiles and the rescue image.

`--dry-run` brings the server into the rescue system, prints the
installer's plan (machine, every partition, array and volume per disk,
network, users, packages, firewall, provision) and leaves it there
without touching a disk; a layout whose targets match no disk, or a raid
level with too few members, stops there too. `--no-reboot` installs and leaves the
server in the rescue system. `-o json` prints the result (the profile and layout, the way in, the
hostname, what answered after the reboot). A provision step other than
`none` is recorded on the server and reported; running it is not
implemented yet. Exit status 1 means the
installer failed (its output says where; the server stays in the rescue
system, `ump ssh REF --staging` gets there), 5 that the new system
did not answer after the reboot (look at the server's console). Providers
that keep their rescue system as the boot option (OVHcloud) have it
disabled before the reboot. Asks for confirmation on a terminal; without
one, `--yes` is required.

## `ump ssh`

```
ump ssh REF [--user <name>] [--staging] [-- COMMAND...]
```

Resolves the server and starts the local `ssh` client against its primary
address: an interactive session, or the command after `--`, whose exit
status becomes ump's. UMP's own key is offered together with your own
keys (the agent's and the usual `~/.ssh/id_*` files), so a machine that
knows only UMP's key is reachable too; your SSH configuration applies.
`--staging` connects the way UMP does to rescue and staging systems: with
UMP's key alone and without host key checks, because those systems have
a fresh host key every boot (a rescue system seen through a known address
needs `--staging` for that reason).

Provider differences: Hetzner Robot takes the fingerprints of keys
registered at the account and issues a password only without keys;
Hetzner Cloud takes project keys and always issues a password; OVHcloud
dedicated servers take one rescue SSH key and mail the password to the
rescue mail address otherwise; OVHcloud Public Cloud instances are
rebooted into rescue mode by OVHcloud itself, with the instance's own
keys and a returned password.

## `ump config`

```
ump config isp         [view|types|add|remove|test]
ump config os          [list|get|create|edit|delete|update]
ump config disk-layout [list|get|create|edit|delete]      (alias: dl)
ump config firewall    [list|get|create|edit|delete]      (alias: fw)
ump config path | key
```

`ump config isp` is the [provider accounts](providers.md): the wizard
without a subcommand, and the subcommands for the non-interactive forms.
`ump config os` is the [profiles](profiles.md): the first run
downloads the published default profile, layouts and firewalls into
`~/.config/ump/profiles`, `list` shows what is there, `get` prints one,
`create NAME` asks on a terminal what the profile should hold (the
image, from the storage's `os/index.json`; the disk layout; the
firewall; the administrator with shell and keys; the kernel switches;
the time zone; packages; the interface names; the provision step) and
writes it, or with `--from NAME` copies another profile and opens it in
your editor, `edit NAME` opens it and checks it afterwards (an invalid
profile is kept, with the problem named), `delete NAME` removes it,
`update` fetches the published profiles (`default`, `ubuntu-24.04`,
`ubuntu-22.04`), `layouts.yaml` and `firewalls.yaml` again (a file still as published is replaced, one you
edited is kept with the new version next to it in `.published/`), and
without a subcommand it is a small wizard over these. `ump server set
REF --profile NAME` remembers which profile a server is reinstalled with
(`servers:` in the configuration file), `--clear` forgets it.
`ump config disk-layout` (`dl`) is the same for the named [disk
layouts](disk-layouts.md) in `layouts.yaml` that profiles refer to:
`list` shows each with its parts and whether it is valid, `get` prints
one, `create NAME --from NAME` copies an entry (the file's comments
survive) and opens the file in your editor, `edit` opens it and checks
every layout afterwards, `delete` removes one. `ump config firewall`
(`fw`) is the same for the named [firewalls](firewalls.md) in
`firewalls.yaml` (`default`: ICMP and SSH; `web`: plus HTTP and HTTPS;
`open`: none), which a profile names in its `firewall` field; `ump
server reinstall --firewall NAME` picks one for an installation.

Without a subcommand, `ump config isp` is an interactive wizard: it asks for
the provider type, an account name and the credentials, asks where the
secrets should be kept (files under `~/.config/ump/secrets/` readable only
by you, environment variables, or a command such as `pass show ...`),
tests the credentials with one API call and writes the account to the
configuration. For OVHcloud it can request a consumer key for you and hand
you the validation URL. `q` at any menu or yes/no question leaves the
wizard, and Ctrl-C aborts at any prompt; an account is written only after
its test passed (or you chose to save it anyway), so leaving never saves a
half-entered account. Without a terminal it refuses with exit code 3.

`ump config isp add <type> <name> --set KEY=VALUE ...` is the non-interactive
form. A value is plain text or a secret reference: `env:NAME`,
`file:PATH`, `command:CMD`. The credentials are tested before saving
unless `--no-test` is given; `ump config isp types` lists the settings of
every provider type. `view` shows the configuration with plaintext
secrets redacted (references are shown as they are), `test` checks every
account, `remove` deletes an account together with the secret files the
wizard created for it.

### `ump config key`

UMP has its own SSH key pair, generated on first use (by `ump config key`
or the first `ump server rescue`) at `~/.config/ump/staging_ed25519`, or at the
path set as `staging.key` in the configuration. It is what rescue
systems and installed systems are reached with, so it works whatever
personal keys are around. `ump config key` prints the public key and its
fingerprints (`-o json` for scripts); put the public key wherever UMP
should be able to log in.

## Examples

```console
# set up provider accounts interactively, or without a terminal:
$ ump config isp
$ ump config isp add hetzner-robot main --set login=env:HROBOT_USER --set password=env:HROBOT_PASS
$ ump config isp test main

# what do I have?
$ ump server list
$ ump server list -o wide --account main
$ ump server list -o json | jq '.[] | select(.status=="ready")'
$ ump server show web-01
$ ump server rename web-01 prod-web-01
$ ump server rdns set prod-web-01 prod-web-01.example.net
$ ump server reboot prod-web-01 --wait
$ ump server rescue prod-web-01            # into a root shell of the rescue system
$ ump server rescue prod-web-01 --yes -- lsblk
$ ump server unrescue prod-web-01 --reboot
$ ump ssh prod-web-01 -- uptime

# reinstall one server
$ ump server reinstall web-01 --dry-run            # the plan on that machine, nothing erased
$ ump server reinstall web-01                      # the profile set for it, else default
$ ump server reinstall web-01 --profile web --layout lvm-raid1 --firewall web
$ ump server set web-01 --profile web              # from now on without --profile

# without a terminal (CI, agents): --yes, and -o json for the result
$ ump server reinstall hetzner-cloud/234567 --yes -o json
$ ump server rescue web-01 --yes -- lsblk

# the profiles and what they refer to
$ ump config os list
$ ump config os create web                         # from default, then your editor
$ ump config disk-layout get lvm-raid1
$ ump config firewall create mail --from default
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error, including a failed installation (the server stays in the rescue system) |
| 2 | usage or validation error |
| 3 | aborted: the question was answered no or left, or there was no terminal and no `--yes` |
| 4 | provider API error, or an account that could not be listed |
| 5 | the server did not answer in time (rescue system, reboot, new system) |
