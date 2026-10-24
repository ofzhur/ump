# Inventory

Every server ump can install, it can also list, and that makes it the
inventory of a fleet spread over providers: one table with every
account's servers, the same columns whatever the provider, in JSON for
anything that wants to consume it, with what changed since the last
look, with prices and with facts of your own. Servers no provider lists
are added by hand, so the inventory is complete.

```console
$ ump server list                         # every server of every account
$ ump server list -o wide                 # with the ID, profile, layout, IPv6, product, kind and attributes
$ ump server list -o json | jq '.[] | select(.price == null)'
$ ump server list --diff                  # added, removed and changed since the last listing
$ ump server list --refresh               # ask the providers, not the cache
$ ump server list --cached                # the last listing, however old, without the APIs
$ ump server list -o ansible              # the Ansible inventory of the servers ump installed
$ ump server show web-01                  # everything the provider knows, plus what ump knows
```

## The listing

`ump server list` asks every configured account, all in parallel (on a
terminal each account's servers appear as soon as it answered), and
prints one row per server: account, provider, name, IPv4, location,
status, the operating system ump installed last (the profile's `os`, or
`--os`) and the Ansible groups it put the server in (from the state),
and the price. The groups are as in the inventory: `common` first, then
those of `--group` ([provisioning](provisioning.md)); a server ump did
not install has none. `-o wide` adds the provider's ID, the profile and
the disk layout of the last installation (`inline` when the profile
carried its own layout) after the groups, the IPv6 address, product,
kind and the attributes; `-o json` and `-o yaml` print every field,
`os`, `installed_at`, `groups`, `profile` and `layout` among them, and
the provider's labels; `-o name` prints `provider/id` per line, `-o
ansible` the Ansible inventory of the servers ump installed
([below](#what-ump-installed)). `--account NAME` restricts the
listing, and references after the command select servers (`ump server
list web-01 db-01`). An account whose API fails is reported on stderr
and the others are still listed; the exit code says so (4).

A listing is served from the cache for 5 minutes (`cache_ttl` in the
configuration, `UMP_CACHE_TTL` for one run, `0` turns it off), unless a
command changed one of the account's servers since; `--refresh` asks the
providers anyway. A provider that does not answer is listed from its
last listing, with a note how old it is. Every listing is recorded, so `--diff` shows what was added, removed or
changed since the previous one (`--since 2h`, `--since 2026-10-01` for
an earlier point), and `--cached` prints the last listing without
touching an API. See [the CLI reference](cli.md#ump-server-list).

## Prices

A server's price is shown in the listing and `ump server show`, and
carried in `-o json` as `price` (`amount`, `currency`, `period`
and `source`):

| Provider | Where the price comes from |
|---|---|
| Hetzner Cloud | the API: the server type's monthly price at the server's location, net, EUR (`"source": "provider"`), for retired types too |
| Hetzner Robot | not in the API: set it by hand |
| OVHcloud | not in the API: set it by hand |
| DigitalOcean | the API: the size's monthly list price, USD (`"source": "provider"`) |
| Linode | the API: the type's monthly list price in the instance's region, USD (`"source": "provider"`); none for a retired type |
| Vultr | the API: the plan's monthly list price, USD (`"source": "provider"`); none for a plan Vultr no longer offers |
| manual servers | set it by hand |

Set by hand, a price wins over the API's and stays in the
[state](state.md#the-state-document):

```console
$ ump server set rack-07 --price 89                       # EUR per month
$ ump server set vps-x --price 0.0071 --currency USD --period hour
$ ump server set rack-07 --clear-price
```

`ump server list -o json` gives the raw numbers for a sum per month,
per account or per attribute; a server without a price is one the
inventory still needs a number for.

## Attributes

Attributes are facts of your own about a server, for the inventory and
for scripts: an owner, a role, a cost center, a ticket. They live in the
[state](state.md#the-state-document), show in `-o wide` and `ump server show`, and come
along in `-o json` as `attributes`.

```console
$ ump server set web-01 --attr owner=alice --attr role=web --attr cc=4711
$ ump server set web-01 --unset-attr cc
$ ump server list -o json | jq '.[] | select(.attributes.role == "web") | .name'
```

Keys are letters, digits, `-`, `_` and `.`; values are free text. The
provider's own labels (Hetzner Cloud labels, OVH tags) are a separate
field, `labels`, and stay the provider's.

## Servers added by hand

A machine in your own rack, at a provider ump has no support for, or
anything with an address goes into the inventory with `ump server add`:

```console
$ ump server add rack-07 --ipv4 192.0.2.7 --location basement --product "Dell R640" --note "KVM at 192.0.2.250"
$ ump server add vps-x --ipv4 203.0.113.5 --kind cloud
$ ump server list --account manual
$ ump server remove vps-x
```

Manual servers are listed by the account `manual` of provider type
`manual`, with the status `manual`, and `ump server set` gives them a
profile, a price and attributes like any other. They live in the
[state](state.md#the-state-document) under `manual:`, so a team sharing a state backend
sees them all; `ump server remove` forgets one (the machine itself is
not touched).

## What ump installed

After `ump server reinstall`, the state records what the server got
(OS and image version, kernel, profile, layout, hostname, administrator,
the interface names with their MAC addresses, when, its groups), and
`ump server show` prints it under `UMP:`. Run from your infrastructure
repository, ump also writes the server into its Ansible inventory, one
file per server in `inventory/hosts/` with its groups, address,
administrator and these facts (`ump_os`, `ump_profile`,
`ump_attributes`, ...): see [provisioning](provisioning.md#host-files).

The same inventory comes from the listing: `ump server list -o ansible`
prints the servers ump installed with the hosts, groups and variables
of their host files, in the JSON of an inventory script, and the
collection's inventory plugin `ofzhur.ansible.ump` reads it, so that
ump itself is the inventory of the repository and no host file is
needed ([ump as the inventory](provisioning.md#ump-as-the-inventory)).
`--account`, references and `--cached` select as for the other formats.

What ump can do with a manual server is what SSH allows. `ump server
rescue` and `ump server reinstall` work when the running system lets UMP
in (UMP's key or yours, as root or a sudo user; `ump config key get`
prints the key): the rescue image is loaded over SSH and entered with
kexec, without a reboot through anyone. There is no provider rescue
system behind it, and that is the risk: when an installation fails, or
the new system does not boot, nothing can bring the machine back but a
console, the provider's own rescue system or a reinstall by their means.
ump says so before it erases anything:

```
Warning: manual/rack-07 (rack-07, 192.0.2.7) has no provider rescue system behind it (provider type manual).
ump reaches it over SSH alone; if the installation fails or the new system does not boot, the machine is
yours to recover (a console, the provider's own rescue system).
Install ubuntu-26.04 (...) on manual/rack-07 (rack-07, 192.0.2.7) as rack-07? Every disk of the server is
erased. There is no rescue system to fall back on: a failed installation is yours to recover. [y/N]:
```

Power control, renaming and reverse DNS are not available for them.
