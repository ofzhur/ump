# CLI

The command structure follows kubectl. `ump get servers` and `ump config`
are implemented; the rest of this document is the intended shape and will
be converted to the same style as commands are built.

## Conventions

- Commands follow the kubectl shape, `ump <verb> <resource>`: `ump get
  servers` lists, `ump config` configures. The central workflow verbs
  (`plan`, `install`, `ssh`) are top-level because they are used most.
  Resource-first forms in the draft below (`machine reboot`, `profile
  show`, ...) will become `ump reboot server`, `ump describe profile` and
  so on when they are implemented.
- Machines are addressed by a **machine reference**: `provider/id`, a
  name or an IP address (see [concepts](concepts.md#machine)). Several
  references or `--selector` can be given where it makes sense. Names
  need not be unique: `get` lists every match (with a note on stderr),
  commands that act on one server ask which one is meant on a terminal
  and otherwise fail with the candidates; `provider/id` and `--account`
  disambiguate.
- Every listing supports `-o table|wide|json|yaml`. JSON output is stable
  and meant for scripting.
- Destructive commands ask for confirmation. `--yes` skips the prompt but
  needs a guard flag (see [architecture](architecture.md#safety)).
- Global flags: `--config <file>`, `--account <name>`, `-v/--verbose`,
  `--no-color`.
- UMP never prompts when stdin is not a TTY. It fails with exit code 3 and
  names the approval that is missing (see `ump approve`).

## Overview

Implemented today:

```
ump
├── get
│   └── servers  [ref...] [-o table|wide|json|yaml|name] [--no-headers]
│                [--diff] [--since <time>] [--cached]
│                                                  all servers of all accounts (--account to restrict);
│                                                  what changed since the last listing; the cached listing
├── describe
│   └── server   <ref>... [-o text|json|yaml]       everything the provider knows about a server
├── rename
│   └── server   <ref> <new-name> [--force]         set the name at the provider (hostname at the next install)
├── get rdns     [ref...] [-o table|json|yaml]      reverse DNS entries of servers' public addresses
├── set rdns     <ref> [ptr-name] [--ip <addr>]     set the PTR of the primary IPv4 (or --ip); default: the server name
├── delete rdns  <ref> [--ip <addr>]                remove the PTR
├── reboot       server <ref> [--hard|--power-cycle] [--wait] [--yes]
├── start        server <ref> [--wait]
├── stop         server <ref> [--hard] [--yes]
├── rescue       server <ref> [--image <url>] [--via auto|direct|provider] [--no-image] [--ssh-key <file>]... [--no-reboot] [--no-wait] [--no-connect] [--yes] [-- command]
├── install      server <ref> [--os <name>] [--hostname <name>] [--ssh-key <file>]... [--ipv6] [--mitigations] [--disks <list>] [--timezone <zone>] [--storage <url>] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes]
├── unrescue     server <ref> [--reboot] [--wait] [--yes]
├── ssh          server <ref> [--user <name>] [--staging] [-- command]
├── config                                         interactive wizard: add an account, test it, save it
│   ├── view                                       the configuration, secrets redacted
│   ├── path                                       where the configuration lives
│   ├── types                                      provider types and their settings
│   ├── add      <type> <name> --set k=v ...       non-interactive form (CI, agents)
│   ├── remove   <name>
│   ├── test     [<name>...]                       check credentials and API access
│   └── key      [-o json|yaml]                     UMP's own SSH public key (generated on first use)
└── version
```

Draft of the rest:

```
ump
├── plan       <machine> --profile <name>          show what install would do
├── approve    <run-id>                            approve a plan (interactive, TTY only)
├── install    <machine>... --profile <name>       install the OS
├── facts      <machine>                           inspect hardware (enters staging env if needed)
│
├── machine
│   ├── show     <machine>
│   ├── import   --from terraform [file]           names and labels of machines Terraform created
│   ├── create   --account <name> --type <type> --location <loc>   (clouds)
│   └── destroy  <machine>                                          (clouds)
│
├── profile
│   ├── list
│   ├── show     <name>                            fully resolved (inheritance applied)
│   └── validate [<name>]
│
├── layout
│   ├── list
│   ├── show     <name>
│   └── validate [<name>]
│
├── image
│   ├── list
│   ├── show     <name>
│   ├── fetch    <name>                            pre-download and verify
│   └── build    <recipe>                          build an image from a recipe (later)
│
├── inventory
│   ├── export   [--format ansible|json|hosts] [--group-by env,role,provider,location,profile]
│   ├── ansible  --list | --host <name>            Ansible's script-inventory protocol, behind an exec wrapper
│   └── check                                      provider machines vs. terraform outputs vs. ansible inventory
│
├── repo
│   ├── init     [dir]                             write the reference repository skeleton
│   └── audit    [dir] [-o markdown|json]          repository health report (see skill.md)
│
├── run
│   ├── list                                       past runs
│   ├── show     <run-id>                          plan, steps, result
│   └── logs     <run-id> [-f]
│
├── skill
│   ├── install  [--user|--project] [--with-permissions]
│   └── path
│
├── doctor                                         tools, versions, credentials reachable, skill version
├── agent                                          (internal) the target-side role, started over SSH by install
└── version
```

## `ump get servers`

```
ump get servers [NAME|ID|IP|PROVIDER/ID ...] [--account <name>...]
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
    "id": "1234567",
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

Every successful listing of an account (by `get`, `describe` or `config
test`) is recorded under `~/.cache/ump/listings/<config>/<account>/`. A
new snapshot is written only when the listing changed, so the cache holds
the history of observed states (the newest 50 per account), and deleting
it is always safe.

```
ump get servers --diff                  # since the previous listing
ump get servers --since 7d              # since the state as of a week ago (also 12h, 2026-10-01, RFC 3339)
ump get servers --cached [ref...]       # the last listing, without any API call
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
    {"change": "added", "machine": {"provider": "hetzner-robot", "id": "1234568", "...": "..."}},
    {"change": "changed", "fields": ["status"], "before": {"...": "..."}, "machine": {"...": "..."}},
    {"change": "removed", "machine": {"...": "..."}}
  ],
  "unchanged": 3
}
```

The exit code does not depend on whether something changed; it is 4 when
an account could not be listed, as for a plain listing.

## `ump describe server`

```
ump describe server NAME|ID|IP|PROVIDER/ID ... [--account <name>...] [-o text|json|yaml]
```

Finds each server across the configured accounts with the same references
as `get`. A name that matches several servers is asked about on a
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
array; each element has the machine fields of `get servers` plus
`details`, a list of `{"name": ..., "fields": {...}}` with typed values
(sizes in bytes as numbers, booleans as booleans):

```console
$ ump describe server web-01 -o json | jq '.[0].details[] | select(.name == "Power").fields'
{
  "state": "running",
  "reset_types": ["sw", "hw", "man"]
}
```

Exit codes: 1 when a reference matched nothing or was ambiguous without a
terminal, 3 when the question was left with `q`, 4 when a provider call
failed or an account could not be listed.

## `ump rename server`

```
ump rename server NAME|ID|IP|PROVIDER/ID NEW-NAME [--force] [-o json|yaml|name]
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
renames anyway. The server reference is resolved like for `describe`
(duplicates are asked about on a terminal). Without `-o` the result is
one sentence; `-o json` prints the renamed server as `get` would. Exit
codes: 1 for not found, ambiguous without a terminal, a name in use, or a
provider that cannot rename; 2 for an invalid name; 4 when the provider
refused.

Reverse DNS is not touched; see `ump set rdns`.

## Reverse DNS: `ump get|set|delete rdns`

```
ump get rdns [NAME|ID|IP|PROVIDER/ID ...] [-o table|json|yaml]
ump set rdns NAME|ID|IP|PROVIDER/ID [PTR-NAME] [--ip <address>] [-o json|yaml]
ump delete rdns NAME|ID|IP|PROVIDER/ID [--ip <address>]
```

`get rdns` lists the reverse DNS (PTR) entries of servers' public
addresses: the primary IPv4 always, plus every other address of the
server that has an entry (an IPv6 address in the server's prefix, an
additional IP). `set rdns` sets the entry of the primary IPv4, or of the
address given with `--ip`, which must belong to the server; without a
PTR name the server's name is used, which is the convention: the machine
name is the hostname is the reverse DNS. `delete rdns` removes the entry;
Hetzner Cloud then shows its default again.

Providers differ: Hetzner Robot and Hetzner Cloud accept any name;
OVHcloud checks that the name resolves to the address first, so create
the forward record before setting the PTR there. Providers without
reverse DNS support are noted on stderr by `get rdns` and refused with
exit code 1 by `set` and `delete`. `ump config types` shows which
capabilities each provider type has.

## Power: `ump reboot|start|stop server`

```
ump reboot server REF [--hard | --power-cycle] [--wait] [--yes]
ump start  server REF [--wait]
ump stop   server REF [--hard] [--yes]
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
the [safety model](architecture.md#safety) says. Provider differences:
Hetzner Robot presses the power button for on and off and has no reset
for a switched-off server; OVHcloud dedicated servers know one reboot (a
hardware reset) and cannot be switched on or off through the API.

## Rescue: `ump rescue|unrescue server`

```
ump rescue   server REF [--image URL] [--via auto|direct|provider] [--no-image] [--ssh-key FILE]... [--no-reboot] [--no-wait] [--no-connect] [--yes] [-o json|yaml] [-- COMMAND...]
ump unrescue server REF [--reboot] [--wait] [--yes]
```

`rescue` brings the server into UMP's own rescue system, the Debian image
from [`images/rescue/`](../images/rescue/README.md) that runs from RAM
with the disks untouched, waits until it answers and hands the terminal
over to `ssh`, so you land in its root shell; exiting it returns to your
shell. With a command after `--`, the command runs there instead and its
exit status becomes ump's, which is the form for scripts and agents
(`ump rescue server web-01 --yes -- lsblk`). `--no-connect` prints the
`ssh` command instead, as does a terminal-less run without a command;
`-o json` prints the details (address, the way the server got there and
the image version, the provider's password where it issues one, the
`ssh` command) without connecting.

How the server gets there depends on what answers, and ump says so on
the way:

1. The server already runs the UMP rescue system: ump connects as it is.
2. The running system lets UMP in as root over SSH, with UMP's own key
   (see [`ump config key`](#ump-config-key)) or your own keys: ump has
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
`ump reboot --hard` and look at its console).

`unrescue` disables the provider's rescue system so that the next boot
is from disk, and with `--reboot` resets the server now. The UMP rescue
system itself runs from RAM, so a reset leaves it; whether that boots
from disk depends on the provider: Hetzner clears its rescue flag once
its rescue system has booted, OVHcloud dedicated servers keep rescue as
their boot option until `unrescue`.

## Install: `ump install server`

```
ump install server REF [--os ubuntu-26.04] [--hostname NAME] [--ssh-key FILE]... [--ipv6] [--mitigations] [--disks LIST] [--timezone ZONE] [--storage URL] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes] [-o json|yaml]
```

Puts an operating system on the server's disks from a [UMP base
image](../images/ubuntu/README.md) and boots it. The server is brought
into the UMP rescue system the way `rescue` does it (as it is when it
runs it already, by a kexec from the running system when that lets UMP
in as root, else through the provider's rescue system); the
[installer](../install/README.md) is fetched from the storage and run
there, with its progress shown; the server is rebooted into the new
system; ump waits until it answers over SSH with UMP's key and reports
what came up:

```console
$ ump install server web-01
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

Every disk of the server is erased. The default layout, `lvm-linear`,
puts the boot partitions on the first disk and makes every disk a
physical volume of one volume group, with root as one linear volume over
all of it (`--disks` limits the disks). The new system gets the server's
name as hostname (`--hostname` for another; it must be a valid one), the
address, gateway and interface of the rescue system (which `rescue`
carried over from the running system), resolvers 1.1.1.1 and 8.8.8.8,
root with UMP's key, your public keys next to the usual `~/.ssh/id_*`
files, the keys the rescue system had, and `--ssh-key` files (a key that
lives only in an agent: `ssh-add -L > me.pub` and `--ssh-key me.pub`),
the time zone (`--timezone`, default UTC), and a kernel command
line with `mitigations=off` and `ipv6.disable=1`. Both cost performance
and most servers need neither; `--mitigations` and `--ipv6` keep them on,
and [the installer's README](../install/README.md#turning-ipv6-or-the-mitigations-back-on)
says how to turn them on later. `--os` names the image (`ubuntu-26.04`
is the first and so far the only one), `--storage` another storage than
the configured one for the installer, the images and the rescue image.

`--dry-run` brings the server into the rescue system, prints the
installer's plan (machine, disks, layout, network, keys) and leaves it
there without touching a disk. `--no-reboot` installs and leaves the
server in the rescue system. `-o json` prints the result (the way in, the
hostname, what answered after the reboot). Exit status 1 means the
installer failed (its output says where; the server stays in the rescue
system, `ump ssh server REF --staging` gets there), 5 that the new system
did not answer after the reboot (look at the server's console). Providers
that keep their rescue system as the boot option (OVHcloud) have it
disabled before the reboot. Asks for confirmation on a terminal; without
one, `--yes` is required.

## `ump ssh server`

```
ump ssh server REF [--user <name>] [--staging] [-- COMMAND...]
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

Without a subcommand, `ump config` is an interactive wizard: it asks for
the provider type, an account name and the credentials, asks where the
secrets should be kept (files under `~/.config/ump/secrets/` readable only
by you, environment variables, or a command such as `pass show ...`),
tests the credentials with one API call and writes the account to the
configuration. For OVHcloud it can request a consumer key for you and hand
you the validation URL. `q` at any menu or yes/no question leaves the
wizard, and Ctrl-C aborts at any prompt; an account is written only after
its test passed (or you chose to save it anyway), so leaving never saves a
half-entered account. Without a terminal it refuses with exit code 3.

`ump config add <type> <name> --set KEY=VALUE ...` is the non-interactive
form. A value is plain text or a secret reference: `env:NAME`,
`file:PATH`, `command:CMD`. The credentials are tested before saving
unless `--no-test` is given; `ump config types` lists the settings of
every provider type. `view` shows the configuration with plaintext
secrets redacted (references are shown as they are), `test` checks every
account, `remove` deletes an account together with the secret files the
wizard created for it.

### `ump config key`

UMP has its own SSH key pair, generated on first use (by `ump config key`
or the first `ump rescue`) at `~/.config/ump/staging_ed25519`, or at the
path set as `staging.key` in the configuration. It is what rescue
systems and, later, the install pipeline are reached with, so it works
whatever personal keys are around. `ump config key` prints the public
key and its fingerprints (`-o json` for scripts); put the public key
wherever UMP should be able to log in.

## Workflow commands

### `ump plan`

```
ump plan <machine> --profile <name> [--layout <name>] [--image <name>]
         [--entry rescue|kexec|virtual-media|manual] [--hostname <tmpl>]
         [-o text|json]
```

Resolves the profile against the machine and prints the plan. If facts
are not cached for the machine, `plan` needs to inspect it — for a
running machine that means entering the staging environment, which
reboots it. `plan` asks before doing that, and `--facts <file>` lets you
plan from previously collected facts instead.

### `ump install`

```
ump install <machine>... --profile <name> [plan flags]
            [--approved <run-id> | --yes --confirm-machine <ref>] [--parallel N]
            [--skip-handoff] [--keep-rescue] [--dry-run]
ump install --resume <run-id>
```

Runs the full pipeline (see [architecture](architecture.md#the-install-pipeline)).
`--dry-run` goes up to and including `plan` and then prints the
commands the agent would run. Without a TTY, an install needs either an
approval from `ump approve` or `--yes` with a guard.

### `ump approve`

```
ump approve <run-id> [--ttl 1h]
```

Shows the plan recorded by `ump plan` (or by an `install` that stopped at
the confirmation step) and records an approval bound to the plan's hash.
Refuses to run without a TTY: this is the step a human does, and an agent
in a non-interactive shell cannot do it for itself. The approval expires,
and is void if the facts or the profile change before the install. Built
for the [skill](skill.md) workflow; useful on its own for a four-eyes
process.

### `ump ssh`

```
ump ssh <machine> [--user <name>] [--staging] [-- command...]
```

Resolves the machine to an address and opens an SSH session, or runs a
command. `--staging` uses UMP's staging key and `root`, for when the
machine is in rescue.

### `ump facts`

```
ump facts <machine> [--refresh] [-o yaml|json]
```

Shows the hardware facts. Uses the cache from the last run unless
`--refresh` is given.

## Examples

```console
# set up provider accounts interactively, or without a terminal:
$ ump config
$ ump config add hetzner-robot main --set login=env:HROBOT_USER --set password=env:HROBOT_PASS
$ ump config test main

# what do I have?
$ ump get servers
$ ump get servers -o wide --account main
$ ump get servers -o json | jq '.[] | select(.status=="ready")'
$ ump describe server web-01
$ ump rename server web-01 prod-web-01
$ ump set rdns prod-web-01 prod-web-01.example.net
$ ump reboot server prod-web-01 --wait
$ ump rescue server prod-web-01            # into a root shell of the rescue system
$ ump rescue server prod-web-01 --yes -- lsblk
$ ump unrescue server prod-web-01 --reboot
$ ump ssh server prod-web-01 -- uptime

# reinstall one server
$ ump plan web-01 --profile base
$ ump install web-01 --profile base

# reinstall three servers from CI, without prompts
$ ump install web-01 web-02 web-03 --profile base \
      --yes --confirm-machine web-01,web-02,web-03 --parallel 3

# take over a cloud VM that terraform created, using kexec
$ ump install hetzner-cloud/7654321 --profile base --entry kexec

# install a box in the rack through its Redfish controller
$ ump install rack1-07 --profile storage --entry virtual-media

# a machine that is not behind any API: boot something with SSH yourself
$ ump install 192.0.2.50 --profile base --entry manual

# agent-driven flow: the agent plans, a human approves, the agent installs
$ ump plan prod-web-03 --profile web -o json       # agent
$ ump approve run-7f3a                             # human, in a terminal
$ ump install prod-web-03 --approved run-7f3a      # agent

# Ansible inventory straight from the providers
$ ump inventory export --format ansible --group-by env,role,provider
```

Everything after `ump unrescue server` in the examples is draft.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error |
| 2 | usage / validation error |
| 3 | aborted by user or guard mismatch |
| 4 | provider API error |
| 5 | machine unreachable / timeout |
| 6 | installation failed on the target |
| 7 | verification or handoff failed (the OS is installed) |
