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
├── server (s, srv)
│   ├── list (ls, l)  [ref...] [-o table|wide|json|yaml|name|ansible] [--no-headers] [--diff] [--since <t>] [--cached] [--refresh]
│   │                                                  all servers of all accounts (--account to restrict);
│   │                                                  what changed since the last listing; the cached listing;
│   │                                                  the Ansible inventory of the servers ump installed
│   ├── show (v, view) <ref>... [-o text|json|yaml]    everything the provider knows about a server
│   ├── reinstall (install) <ref> [--profile <name|file>] [--layout <name>] [--firewall <name>] [--os <name>] [--hostname <name>] [--user <name>[:<key>,...]]... [--userdata <list>]... [--ssh-key <ref>]... [--set <field>=<value>]... [--ipv6] [--mitigations] [--disks <list>] [--timezone <zone>] [--storage <url>] [--group <list>]... [--hosts <pattern>] [--playbook <list>]... [--no-provision] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes] [-o json|yaml]
│   ├── rescue       <ref> [--image <url>] [--via auto|direct|provider] [--no-image] [--ssh-key <ref>]... [--no-reboot] [--no-wait] [--no-connect] [--force] [--yes] [-- command]
│   ├── unrescue     <ref> [--reboot] [--wait] [--yes]
│   ├── reboot       <ref> [--hard|--power-cycle] [--wait] [--yes]
│   ├── start        <ref> [--wait]
│   ├── stop         <ref> [--hard] [--yes]
│   ├── rename       <ref> <new-name> [--force]        set the name at the provider (hostname at the next install)
│   ├── rdns (ptr)
│   │   ├── get      [ref...] [-o table|json|yaml]     reverse DNS entries of servers' public addresses
│   │   ├── set      <ref> [ptr-name] [--ip <addr>]    set the PTR of the primary IPv4 (or --ip); default: the server name
│   │   └── delete   <ref> [--ip <addr>]               remove the PTR
│   ├── set          <ref> [--profile <name> | --clear] [--price <amount> [--currency <cur>] [--period month|hour] | --clear-price] [--attr k=v]... [--unset-attr k]... [--group <list>]... [--ungroup <list>]...
│   │                                                  what ump remembers about a server: profile, price, your attributes, groups
│   ├── add (a, new) <name> --ipv4 <addr> [--ipv6 <addr>] [--location <s>] [--product <s>] [--kind bare-metal|cloud] [--note <s>]
│   │                                                  a server by hand, without a provider API (account "manual")
│   └── remove (rm, d) <name> [--yes]                  forget a server added by hand
├── provision        [--hosts <pattern>] [--playbook <list>]... [--yes] [-o json|yaml] [-- ansible-args]
│                                                      the playbooks of the infrastructure repository on the fleet
├── ssh              <ref> [--user <name>] [--staging] [-- command]
├── config (c, cfg)                                    each entity prints its help when called bare
│   ├── isp (i, provider, account)                     the accounts
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the accounts: type, servers, settings, where the secrets come from
│   │   ├── get      (g, show) <name> [-o json|yaml]   one account: type, settings, where its secrets come from
│   │   ├── types                                      provider types and their settings
│   │   ├── add      (a, new, create) [<type> <name> --set k=v ...]   the wizard on a terminal: type, name, credentials, test; with arguments the non-interactive form (CI, agents)
│   │   ├── edit     (e) [<name>]                      that account alone in your editor, or the whole configuration file; checked afterwards
│   │   ├── remove   (rm, d, delete) <name>
│   │   └── test     (t) [<name>...]                   check credentials and API access
│   ├── os (o, profile)                                the profiles
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the profiles in ~/.config/ump/profiles
│   │   ├── get      (g, show) <name>                  print one
│   │   ├── create   (c, new, add) [<name>] [--from <name>]   the wizard on a terminal (name, image, layout, firewall, users, ...); --from copies one
│   │   ├── edit     (e) <name>                        your editor, then a check
│   │   ├── delete   (d, rm, remove) <name> [--yes]
│   │   ├── update   (up)                              refresh the published profiles and entries; yours are kept
│   │   └── default  (def) [<name>]                    show or set the default profile
│   ├── disk-layout (dl, layout)                    the named layouts of layouts.yaml
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the layouts, with their parts and whether they are valid
│   │   ├── get      (g, show) <name>                  print one
│   │   ├── create   (c, new, add) [<name>] [--from <name>]   copy one into the file and open it in your editor; asks on a terminal
│   │   ├── edit     (e) [<name>]                      that layout alone in your editor, or all of layouts.yaml; checked afterwards
│   │   └── delete   (d, rm, remove) <name> [--yes]
│   ├── firewall (fw)                               the named firewalls of firewalls.yaml
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the firewalls, with their policy and what they allow
│   │   ├── get      (g, show) <name>                  print one
│   │   ├── create   (c, new, add) [<name>] [--from <name>]   copy one into the file and open it in your editor; asks on a terminal
│   │   ├── edit     (e) [<name>]                      that firewall alone in your editor, or all of firewalls.yaml; checked afterwards
│   │   └── delete   (d, rm, remove) <name> [--yes]
│   ├── user (u)                                    the named users of users.yaml
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the users, with sudo, shell, keys
│   │   ├── get      (g, show) <name>                  print one
│   │   ├── create   (c, new, add) [<name>] [--from <name>]   copy one into the file and open it in your editor; asks on a terminal
│   │   ├── edit     (e) [<name>]                      that user alone in your editor, or all of users.yaml; checked afterwards
│   │   └── delete   (d, rm, remove) <name> [--yes]
│   ├── userdata (ud)                                  the named userdata of userdata.yaml: what runs in a new system before its first boot
│   │   ├── list     (ls, l) [-o wide|json|yaml]       the entries, with type, lines and whether they are valid
│   │   ├── get      (g, show) <name>                  print one
│   │   ├── create   (c, new, add) [<name>] [--from <name>]   copy one into the file and open it in your editor; asks on a terminal
│   │   ├── edit     (e) [<name>]                      that entry alone in your editor, or all of userdata.yaml; checked afterwards
│   │   └── delete   (d, rm, remove) <name> [--yes]
│   ├── state (st)                                     the state backend: the local directory, or one shared by a team
│   │   ├── view     (v, show)                         which backend, the mirror, what the state holds
│   │   ├── setup    (configure, edit)                 the wizard on a terminal: pick a backend, migrate the state there
│   │   ├── set      --url <url> [backend flags]       point ump at a backend as it is
│   │   ├── migrate  --to <url> [backend flags] [--force]   copy the state there and point ump at it
│   │   ├── clear                                      the local directory again
│   │   ├── pull | push [--force]                      the mirror and the backend, by hand
│   │   ├── test     (t)                               does the backend answer?
│   │   ├── locks                                      the locks held: the servers', the fleet's
│   │   └── unlock   <server>|fleet                    release one
│   └── key (k)                                        UMP's own key and the named keys
│       ├── list     (ls, l) [-o wide|json|yaml]       ump and the named keys, with fingerprints; wide adds the public key lines
│       ├── get      (g, show) [<name>] [-o json|yaml]   a public key with its fingerprints: ump (UMP's own, the default) or a named one
│       ├── add      (a, new, create) [<name>] (--file <f>|--line <l>|--agent)   keep a public key under a name; asks on a terminal
│       ├── edit     (e) [<name>]                      that key alone in your editor, or all of keys.yaml; checked afterwards
│       ├── remove   (rm, d, delete) <name>
│       └── roll     [--no-servers] [--drop-old] [--yes]  a new key pair for UMP; every server ump installed gets it and loses the old one
├── completion
└── version
```

## `ump server list`

```
ump server list [NAME|ID|IP|PROVIDER/ID ...] [--account <name>...]
                [-o table|wide|json|yaml|name|ansible] [--no-headers]
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
    "status": "running",
    "labels": {"env": "prod"}
  }
]
```

`status` is the machine's power state where the provider tells it
(`running`, `off`), else the provider's own status, lower-cased
(`ready`, `cancelled`, `active`, ...); `location` is the datacenter or region,
lower-cased; `labels` are present where the provider has them. `os` is
the operating system ump installed last (`ubuntu-26.04`) and
`installed_at` when, from the [state](state.md); `<none>` in the table
for a server ump never installed. `groups` are the Ansible groups ump
put the server in, as in the inventory: `common` first, then those of
`--group` ([provisioning](provisioning.md)); none for a server ump never
installed. `profile` and `layout` are the profile and the disk layout of
that installation (`inline` when the profile carried its own layout).
`-o wide` adds the ID, IPv6, product, kind, profile, layout and
attributes columns (the ID right after the provider, the profile and
the layout after the groups); the price is in the default table; `-o name`
prints `provider/id` per line. `defaults.output` in the configuration
sets the default format. `price` comes from the provider's API where it
tells (Hetzner Cloud) or from `ump server set --price`; `attributes`
from `ump server set --attr`; see [inventory](inventory.md).

`-o ansible` prints the Ansible inventory of the servers ump installed
(the state has their installation), in the JSON of an inventory
script, for the inventory plugin `ofzhur.ansible.ump`: each server under
the hostname of its installation, in `common` and its groups, with the
variables of its host file (`ansible_host`, `ump_admin`, `ump_ref`,
`ump_os`, ...; [provisioning](provisioning.md#ump-as-the-inventory)).
`--account`, references and `--cached` select as for the other
formats, and the exit code is the same. A server without an address or
a valid hostname, or whose hostname a later installation took, is left
out with a note on stderr; nothing else goes there unless something is
wrong (the plugin warns with it).

```json
{
  "_meta": {"hostvars": {"web-01": {"ansible_host": "203.0.113.10", "ump_admin": "ump", "ump_ref": "hetzner-robot/123456", "...": "..."}}},
  "all": {"children": ["common", "web"]},
  "common": {"hosts": ["web-01"]},
  "web": {"hosts": ["web-01"]}
}
```

All accounts are asked in parallel. On a terminal, the table is drawn
while they answer: each account's servers appear as soon as it answered,
with a line naming the accounts still being asked, and the finished
table replaces the drawing. In a pipe, with `-o json`, `yaml` or `name`
and with `-v`, the output is printed once, when every account answered.

### The listing cache and `--diff`

A listing a provider confirmed less than the cache TTL ago is served
from the cache without asking it: 5 minutes by default, `cache_ttl` in
the [configuration](configuration.md) or `UMP_CACHE_TTL` for one run
(`1m`, `30s`; `0` asks every time). The commands that look a server up
(`show`, `ssh`, `reboot`, `reinstall`, ...) use the cache the same way. A
command that changes a server (`reinstall`, `install`, `rescue`,
`unrescue`, `start`, `stop`, `reboot`, `rename`, `rdns set`) makes the
listings of its account ask the provider again, until 3 minutes after it
finished, because a provider shows some changes only after a while (a
server switched off runs on for a minute); adding, replacing or removing
an account drops its cached listing. `--refresh` asks the providers
anyway. A provider that does not answer (offline, its API down; a
connection must be up within 10 seconds) is listed from its last
listing in the cache, whatever its age, with a note saying when that
was; the exit code still says the account could not be listed (4). The cache lives in the home, so with a shared [state
backend](state.md) it is shared too: a listing or a change made by
someone else's ump counts here as well, and when two list at the same
moment the newer listing wins without a conflict being reported. A
change made in a provider's own console shows when the TTL ran out.

Every successful listing of an account (by `list`, `show` or `config isp
test`) is recorded under `~/.cache/ump/listings/<config>/<account>/`. A
new snapshot is written only when the listing changed, so the cache holds
the history of observed states (the newest 50 per account), and deleting
it is always safe.

```
ump server list --diff                  # since the previous listing
ump server list --since 7d              # since the state as of a week ago (also 12h, 2026-10-01, RFC 3339)
ump server list --cached [ref...]       # the last listing, however old, without any API call
ump server list --refresh               # ask the providers even when the cached listing is fresh
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
API, and the command says so), the label at Linode and Vultr, the
droplet name at DigitalOcean (which also sets the droplet's reverse DNS
when the name is a fully qualified domain name).
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
the forward record before setting the PTR there, and so does Linode;
Vultr sets reverse DNS for instances, not for bare metal servers;
DigitalOcean has no reverse DNS API and derives it from the droplet name. Providers without
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
answering and answers again (exit code 5 after 15 minutes, or the
`wait` of the [configuration](configuration.md); the line says the
limit and marks each minute waited).

Rebooting and stopping are disruptive: on a terminal they ask for
confirmation, without one they need `--yes` (exit code 3 otherwise), as
the [safety model](concepts.md#safety) says. Provider differences:
Hetzner Robot presses the power button for on and off and has no reset
for a switched-off server; OVHcloud dedicated servers know one reboot (a
hardware reset) and cannot be switched on or off through the API; Vultr
has one reboot and stops by halting (no ACPI shutdown in its API);
Linode has one reboot, and its shutdown powers off what does not stop
in time.

## Rescue: `ump server rescue|unrescue`

```
ump server rescue REF [--image URL] [--via auto|direct|provider] [--no-image] [--ssh-key REF]... [--no-reboot] [--no-wait] [--no-connect] [--force] [--yes] [-o json|yaml] [-- COMMAND...]
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

`rescue` never erases a server. Where the provider has no rescue
system for ump (DigitalOcean, Vultr, servers added by hand),
the only way is the jump over SSH from the running system, and when
that jump fails nothing but the provider's console brings the server
back; so `rescue` refuses it with exit code 3 unless `--force` is
given (a server that already runs the UMP rescue system is connected
to either way), and stops with exit code 5 when the running system
does not let UMP in. Only `reinstall`, which erases the server anyway
and asks once, makes the jump without `--force` and may have the
provider rebuild the server first.

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
3. Otherwise the provider's rescue system is enabled with UMP's key,
   the keys chosen with `ump config key rescue` and any `--ssh-key`
   references (registering keys the provider does not know yet), the
   server is reset into it, and once it answers ump reports it and loads
   the image from there the same way.

The same keys go into the UMP rescue system. With none but UMP's, a
boxed warning comes first; see [keys in rescue
systems](keys.md#keys-in-rescue-systems).

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
the commands to load the image by hand. Every wait prints its limit
(15 minutes unless `wait` in the configuration or `UMP_WAIT` says
otherwise) and marks each minute waited. Exit status 5 means the server
did not answer in time: no root access with `--via direct`, or the
rescue system never came up after the kexec (reset the server with
`ump server reboot --hard` and look at its console). A rescue kernel
that crashes reboots by itself after 30 seconds, into the system on the
disk. [Troubleshooting](troubleshooting.md) says how to find out what
went wrong without a console.

`unrescue` disables the provider's rescue system so that the next boot
is from disk, and with `--reboot` resets the server now. The UMP rescue
system itself runs from RAM, so a reset leaves it; whether that boots
from disk depends on the provider: Hetzner clears its rescue flag once
its rescue system has booted, OVHcloud dedicated servers keep rescue as
their boot option until `unrescue`.

## Install: `ump server reinstall`

```
ump server reinstall REF [--profile NAME|FILE] [--layout NAME] [--firewall NAME] [--os NAME] [--hostname NAME] [--user NAME[:KEY,...]]... [--userdata LIST]... [--ssh-key REF]... [--set FIELD=VALUE]... [--ipv6] [--mitigations] [--disks LIST] [--timezone ZONE] [--storage URL] [--group LIST]... [--hosts PATTERN] [--playbook LIST]... [--no-provision] [--via auto|direct|provider] [--dry-run] [--no-reboot] [--yes] [-o json|yaml]
```

Puts an operating system on the server's disks from a [UMP base
image](../images/base/README.md) the way a [profile](profiles.md)
says, and boots it. A server whose provider rescue system takes no SSH
key (an OVHcloud VPS), or whose provider has none and whose running
system does not let UMP in (Vultr), is first reinstalled by the
provider with its stock Linux and UMP's key, which `reinstall` may do
since it erases the server anyway; `rescue` never does. Where the
provider can do neither (DigitalOcean, servers added by hand), the
running system must let UMP in before the installation, with one of
your keys or UMP's; without that, `reinstall` stops before it changes
anything. So does an OS the storage has no image of for the machine's
architecture (Arch Linux or CachyOS on arm64), told by the provider or the running
system; when neither tells, ump warns and asks. The profile (`--profile`, default `default`: a file
of that name in `~/.config/ump/profiles/`, else the published one, or a
path) names the image, the disk layout (by name from `layouts.yaml`, or
its own), the users with their keys, the userdata that runs in the new
system and what the firewall lets in. The server is
brought into the UMP rescue system the way `rescue` does it (as it is
when it runs it already, by a kexec from the running system when that
lets UMP in as root, else through the provider's rescue system); the
[installer](../install/README.md) is fetched from the storage and run
there with the resolved profile, with its progress shown; the server is
rebooted into the new system; ump waits until it answers over SSH with
UMP's key, reports what came up and [provisions](provisioning.md) the
fleet with it:

```console
$ ump server reinstall web-01
Install ubuntu-26.04 on hetzner-cloud/123 (web-01, 203.0.113.10) as web-01? Every disk of the server is erased. Then ump provision runs 00-base on the fleet. [y/N] y
Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 24.04.3 LTS).
Loading the UMP rescue image from https://ump.zhurko.de/dl/rescue on web-01, without the provider's rescue system.
...
Waiting for the UMP rescue system on 203.0.113.10 ... up after 11s (ump-rescue 2026.10.13).
Installing ubuntu-26.04 on web-01 as web-01 ...
Provisioning after the first boot: 00-base on the fleet.
==> machine: amd64, BIOS firmware, 2 CPUs, 937 MB RAM
==> disks (1): /dev/sda (19.1G SSD)
==> layout lvm-linear: /dev/sda gets 1 MiB BIOS boot, 512 MiB EFI (vfat), 1 GiB /boot (ext4) and the rest as LVM; ...
...
==> installed: ubuntu-26.04 2026.10.13 (kernel 7.0.0-39-generic) as web-01 on vg0/root (17000 MB over 1 disk(s)); reboot to start it
Rebooting web-01 into ubuntu-26.04 ...
Waiting for SSH on 203.0.113.10 to go down ... down after 4s.
Waiting for SSH on 203.0.113.10 ... up after 18s.
web-01 is up: Ubuntu 26.04 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).
Wrote inventory/hosts/web-01.yml: web-01 in common.
Provisioning: 00-base on the fleet (4 hosts) ...
...
web-01 is provisioned. Provisioned 4 hosts, 1 changed.
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
no root login over SSH; `agent` in a profile takes what `ssh-add -L`
lists), the time zone, and a kernel command
line with `mitigations=off`, `ipv6.disable=1` and `kho=off` (the kernel's
kexec handover reserves memory for live updates and gets in the way of
ump's own re-entry). The first two cost performance and most servers
need neither; the profile's `kernel` section, `--mitigations` and
`--ipv6` keep them on, and [the installer's
README](../install/README.md#turning-ipv6-or-the-mitigations-back-on)
says how to turn them on later.

The flags override the profile for one installation: `--layout`,
`--firewall`, `--os`, `--timezone`, `--ipv6`, `--mitigations` and
`--disks`; `--user` (repeatable) replaces the users, each with sudo:
`NAME:KEY[,KEY...]` with those [key references](keys.md#key-references),
`NAME` alone as it is in `users.yaml`. The first, `root` aside, is the
administrator, the user ump logs in as, and gets UMP's key when it
lacks it; `root` takes its keys alone, as in a profile, and is the
administrator only when it is the only one. `--ssh-key` adds keys (any
key reference) to the administrator and to the rescue system on the
way; `--userdata NAME,...` (repeatable) replaces the userdata with
entries of `userdata.yaml`, in order. `--set FIELD=VALUE` overrides any
other field, after the other flags ([overrides](profiles.md#overrides)).
`--storage` names another storage than the configured one for the
installer, the images, the profiles and the rescue image.

After the first boot ump [provisions](provisioning.md) from the
infrastructure repository it runs in: it needs `ansible.cfg`,
`inventory/` and a playbook in `playbooks/` in the current directory and
`ansible-playbook` in `PATH`, and stops with exit status 2 before it
touches anything when one is missing. The server is in the group
`common` and the groups of `--group` (repeatable, or comma-separated;
lower-case letters, digits and `_`; not `all`, `ungrouped`, `localhost`
or `_meta`). The playbooks pass `ansible-playbook --syntax-check` first;
a hostname that is a group of a server or of the inventory, a group
that is another host of the inventory (`ansible-inventory --list`), a
host file of the hostname that ump did not write for this server (no
`ump_ref`, or another server's) and a world-writable repository stop the
reinstall too, and so does `--hosts` that matches no host of the
inventory with the server in it. Where a file of `inventory/` points to
host files in a storage (`inventory/hosts.yml` with `url:`), they are
there: ump reads and writes them with the collection's credentials from
`[ofzhur_storage]` of `ansible.cfg`, never those of its state ([host
files in a storage](provisioning.md#host-files-in-a-storage)), and
refuses the pointer beside host files in `inventory/hosts/` or beside
ump as the inventory. Once the new system answers, ump records it and
runs `ump provision`: the host files of every server it installed, then
every `playbooks/*.yml` on the whole fleet, each host logged in to as
its administrator with UMP's key only (`IdentitiesOnly`); `--hosts
PATTERN` and `--playbook NAME` narrow it (`--hosts web-01` for the new
server alone), and the reinstall's question covers it. Where a file of
`inventory/` says `plugin: ofzhur.ansible.ump`, ump is the inventory: no
host file is written, and one of the server in `inventory/hosts/` stops
the reinstall up front, as does the hostname as another host of the
inventory. A successful installation records the groups with the server,
none without `--group` (the disks were wiped). `--no-provision` needs no
layout and runs nothing; where the layout is there, the host file is
still written, under the fleet's lock, once `ansible-inventory` has
checked the names, and the next `ump provision` includes the server.
When ump cannot record the installation, it prints the record to add to
`state.yaml` by hand and provisions nothing (exit status 6): the
inventory would have the old one.

`--dry-run` makes the checks, brings the server into the rescue system,
prints the installer's plan (machine, every partition, array and volume
per disk, network, users, userdata, firewall) and the playbooks, and
leaves it there without touching a disk or the inventory; a layout whose
targets match no disk, or a raid level with too few members, stops there
too. `--no-reboot` installs, records the installation and leaves the
server in the rescue system, not provisioned: `ump provision --hosts
HOSTNAME` after the reboot (when ump is the inventory, the server is in
it already). `-o json` prints the result (the profile and layout, the
way in, the hostname, what answered after the reboot, `groups` as in
the inventory, `playbooks`, `provisioned`, and `provision`, the result
of `ump provision`).

Exit status 1 means the installer failed (its output says where; the
server stays in the rescue system, `ump ssh REF --staging` gets there),
5 that the new system did not answer after the reboot (look at the
server's console), 6 that the provisioning did not complete: the server
is installed and recorded; fix what Ansible reports about the hosts it
names and run `ump provision`. The server is provisioned when its own
line of the recap has nothing failed or unreachable, also in a run that
failed on other hosts (`provisioned: true`, exit status 6 still).
Providers that keep their rescue system as the boot option (OVHcloud)
have it disabled before the reboot. Asks for confirmation on a
terminal; without one, `--yes` is required.

## Provision: `ump provision`

```console
$ ump provision                                     # every playbook on the fleet
$ ump provision --hosts web-01                      # on one server
$ ump provision --hosts web --playbook 10-web       # some hosts, some playbooks
$ ump provision --hosts 'all:!lab-*' -- --check --diff
```

Converges the fleet from the infrastructure repository ump runs in, with
the same layout and checks as `reinstall`: every `playbooks/*.yml`, in
file-name order, on the hosts its plays name. `--playbook NAME` (the
file name without `.yml`; repeatable, or comma-separated) runs those
alone, `--hosts PATTERN` (an Ansible host pattern) the hosts it matches
alone; arguments after `--` go to `ansible-playbook` (not `--limit` or
`--inventory`, in any form). Under the fleet's lock, one run at a time
(another waits for it up to the configured `wait`, then exit status 3),
ump checks the names and that `--hosts` matches a host of the inventory
as it will be (else exit status 2, nothing written), writes the host
files of every server it installed (into `inventory/hosts/`, or the
storage `inventory/hosts.yml` points to; none when ump is the
inventory), and runs the playbooks as one, every host logged in to as
its administrator (`ump_admin`) with UMP's key. It prints `Provisioned 4
hosts, 1 changed.`; `-o json` the recap per host (`ok`, `changed`,
`unreachable`, `failed`, `skipped`, `rescued`, `ignored`), `failed`,
`unreachable` and `exit_status`. Exit status 6 names the hosts that
failed or were unreachable. Without `--hosts` it asks on a terminal (the
whole fleet); without one, `--yes` is required. See
[provisioning](provisioning.md).

## `ump server set`, `add` and `remove`

```
ump server set REF [--profile NAME | --clear] [--price AMOUNT [--currency CUR] [--period month|hour] | --clear-price] [--attr KEY=VALUE]... [--unset-attr KEY]... [--group LIST]... [--ungroup LIST]...
ump server add NAME --ipv4 ADDR [--ipv6 ADDR] [--location S] [--product S] [--kind bare-metal|cloud] [--note S]
ump server remove NAME [--yes]
```

`set` writes what ump cannot learn from the provider into the
[state](state.md#the-state-document) (`servers:`): the profile a server is reinstalled
with (`--clear` forgets it), its price where the API does not say
(`--price`, default currency `EUR` and period `month`; it wins over the
API's price; `--clear-price` forgets it), and attributes of your own
(`--attr owner=alice`, repeatable; `--unset-attr` forgets one). Price
and attributes show in `ump server list -o wide` and `ump server show`
and travel in `-o json`. `--group` and `--ungroup` (repeatable, or
comma-separated) add groups to a server ump installed and take them
away; it prints them, `common` first, and runs nothing: the next `ump
provision` applies them. A group that is the server's hostname, or,
from the infrastructure repository, another host of its inventory
(`ansible-inventory`), is refused, and so is a server ump did not
install (exit status 2).

`add` puts a server no provider lists into the [state](state.md#the-state-document)
(`manual:`), listed by the account `manual` of provider type `manual`;
`remove` forgets it. Such a server has no rescue system behind it: `ump
server rescue` and `reinstall` work when its running system lets UMP in
over SSH, and a failed installation is yours to recover, which ump says
before erasing anything. See [inventory](inventory.md).

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

Every entity of `ump config` answers the same verbs: `list` (with `-o
table|wide|json|yaml`; `wide` adds what the table holds back, where
there is anything, and `json` and `yaml` print the entries as data),
`get NAME` (`show` and `g` are the same) prints one, `create` or `add`
makes one, `edit [NAME]` opens one entry or the whole file, `delete` or
`remove` takes one away.

```
ump config isp         list|types|add|remove|test
ump config os          list|get|create|edit|delete|update|default
ump config disk-layout list|get|create|edit|delete
ump config firewall    list|get|create|edit|delete
ump config user        list|get|create|edit|delete
ump config userdata    list|get|create|edit|delete
ump config key         show|list|add|remove
ump config state       view|setup|set|migrate|clear|pull|push|test|locks|unlock
```

An entity called without a verb prints its help. The verbs have the
same short forms under every entity, and the entities short names of
their own, so `ump c o ls` is `ump config os list`:

| Long | Short |
|---|---|
| `config` | `c`, `cfg` |
| `isp` | `i`, `provider`, `account` |
| `os` | `o`, `profile` |
| `disk-layout` | `dl`, `layout` |
| `firewall` | `fw` |
| `user` | `u` |
| `userdata` | `ud` |
| `key` | `k` |
| `state` | `st` |
| `list` | `ls`, `l` |
| `get NAME` | `g`, `show` |
| `view` | `v`, `show` |
| `create` | `c`, `new`, `add` |
| `add` | `a`, `new`, `create` |
| `edit` | `e` |
| `delete` | `d`, `rm`, `remove` |
| `remove` | `rm`, `d`, `delete` |
| `update` | `up` |
| `default` | `def` |
| `test` | `t` |

`ump server` is `s` or `srv`, its `show` also `v` or `view`, and its
`add` and `remove` take the short forms above.

`ump config isp` is the [provider accounts](providers.md): `list` shows
them in a table (type, the servers the last listing found, settings,
and where each secret comes from; `-o wide` spells the references out),
`add` is the wizard on a terminal and the non-interactive form with
arguments. A command group refuses an unknown word with exit code 2.
`ump config os` is the [profiles](profiles.md): the first run
downloads the published profiles, layouts, firewalls and users into
`~/.config/ump/profiles`, `list` shows what is there (the default marked
with `*`, and whether each is published or yours), `get` prints one,
`create [NAME]` asks on a terminal what the profile should hold (the
name when none is given; the image, from the storage's `os/index.json`; the disk layout; the
firewall; the users from `users.yaml`, a new one defined on the spot;
the kernel switches; the time zone; the userdata; the interface names)
and writes it, or with `--from NAME` copies another
profile and opens it in your editor, `edit NAME` opens it and checks it
afterwards (an invalid profile is kept, with the problem named; a
published profile is refused, make your own from it), `delete NAME`
removes it, `update` fetches the published profiles (`ubuntu-26.04`,
`ubuntu-24.04`, `ubuntu-22.04`, `debian-13`, `debian-12`, `nixos-25.11`, `archlinux`, `cachyos`) and the published entries of
`layouts.yaml`, `firewalls.yaml` and `users.yaml` again, replacing them
and keeping yours, `default [NAME]` shows or sets the default profile. `ump server set
REF --profile NAME` remembers which profile a server is reinstalled with
(`servers:` in the configuration file), `--clear` forgets it.
`ump config disk-layout` (`dl`) is the same for the named [disk
layouts](disk-layouts.md) in `layouts.yaml` that profiles refer to:
`list` shows each with its parts and whether it is valid, `get` prints
one, `create NAME
--from NAME` copies an entry (the file's comments survive) and opens
it in your editor (on a terminal it asks for the name and the entry to
start from when they are not given), `edit NAME` opens that layout
alone and puts it back when it is valid, the others untouched, `edit`
without a name opens the whole file and checks every layout afterwards,
`delete` removes one. `ump config firewall`
(`fw`) is the same for the named [firewalls](firewalls.md) in
`firewalls.yaml` (`default`: ICMP and SSH; `web`: plus HTTP and HTTPS;
`open`: none), which a profile names in its `firewall` field; `ump
server reinstall --firewall NAME` picks one for an installation. `ump
config user` (`u`) is the same for the named [users](profiles.md#users)
in `users.yaml` (`ump`, the administrator UMP provides), which a
profile lists by name in `users`, and `ump config userdata` (`ud`) for
the named [userdata](userdata.md) in `userdata.yaml` (`ubuntu`, what
every Ubuntu gets), the scripts a profile lists in `userdata` to run
in the new system before its first boot.

Without arguments and on a terminal, `ump config isp add` is an
interactive wizard. On a fresh system it first asks whether everything
should stay in `~/.config/ump` on this machine, and runs the backend
wizard (`ump config state setup`) when the answer is no, so that a team's
first account lands in the shared state. Then it asks for the provider type, an account name and
the credentials; the secrets become files under `secrets/` in the state,
readable only by you, wherever it lives (environment variables or
a command such as `pass show ...` are for the non-interactive form
below), then it tests the credentials with one API call and writes the
account to the configuration. For OVHcloud it can request a consumer key for you and hand
you the validation URL. `q` at any menu or yes/no question leaves the
wizard, and Ctrl-C aborts at any prompt; an account is written only after
its test passed (or you chose to save it anyway), so leaving never saves a
half-entered account. Without a terminal it refuses with exit code 3.

`ump config isp add <type> <name> --set KEY=VALUE ...` is the
non-interactive form. A value is plain text or a secret reference: `env:NAME`,
`file:PATH`, `command:CMD`. The credentials are tested before saving
unless `--no-test` is given; `ump config isp types` lists the settings of
every provider type. `view` shows the configuration with plaintext
secrets redacted (references are shown as they are), `test` checks every
account, `remove` deletes an account together with the secret files the
wizard created for it.

### `ump config state`

[The state backend](state.md): `view`, `setup` (on a terminal, a
wizard that picks a backend and, when this machine holds state, offers
to migrate it there; a backend that holds state is used as it is),
`set --url URL` (use a backend as it is, from flags), `migrate --to
URL` (copy the state there first), `clear` (the local directory again),
`pull` and `push` for the mirror by hand, `test`, and `locks` and
`unlock SERVER|fleet` for the locks that `reinstall` and `rescue` take
of a server, and `provision` of the fleet. `UMP_STATE` in the
environment names the backend ahead of the pointer file.

### `ump config key`

UMP has its own SSH key pair, generated on first use (by `ump config key
show` or the first `ump server rescue`) at `~/.config/ump/staging_ed25519`, or at the
path set as `staging.key` in the configuration. It is what rescue
systems and installed systems are reached with, so it works whatever
personal keys are around. `ump config key get` prints the public key and
its fingerprints (`-o json` for scripts); put the public key wherever
UMP should be able to log in. The subcommands keep more public keys
under names in `~/.config/ump/keys.yaml`, for the `keys` of
[users](keys.md): `add NAME --file FILE` (every key line of the
file; a second one becomes `NAME-2`), `add NAME --line 'ssh-... AAAA...'`,
`add NAME --agent` (what `ssh-add -L` lists), `add` alone on a terminal
(asks for the name and where the key comes from), `list` (with `ump`, UMP's
own) and `remove NAME`. The names `ump`, `local` and `agent` are taken.
`rescue REF...` chooses the keys that go into every rescue system next
to UMP's (`staging.keys`), `rescue` alone shows them, `rescue --clear`
drops them; see [keys in rescue systems](keys.md#keys-in-rescue-systems).

## Examples

```console
# set up provider accounts interactively, or without a terminal:
$ ump config isp add
$ ump config isp add hetzner-robot main --set login=env:HROBOT_USER --set password=env:HROBOT_PASS
$ ump config isp test main

# what do I have?
$ ump server list
$ ump server list -o wide --account main
$ ump server list -o json | jq '.[] | select(.status=="off")'
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
$ ump server reinstall web-01 --user oleg:local --userdata ubuntu,web
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
| 6 | provisioning failed: Ansible did not complete; the hosts that failed or were unreachable are named (after a reinstall, the server is installed and recorded) |
