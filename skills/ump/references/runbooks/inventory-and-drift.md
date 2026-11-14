# Inventory and drift

`ump server list` is the one listing over every provider account, and
the same data feeds the Ansible inventory. All of this is Observe,
except `server set`, `add` and `remove` (Change: they write the state).

## What is there

```sh
ump server list                               # every server, price included
ump server list -o wide                       # plus ID, IPv6, product, kind, profile, layout, attributes
ump server list -o json                       # for scripts
ump server list -o json | jq -r '.[] | select((.os // "") == "") | .name'    # servers ump never installed
ump server list -o ansible                    # the Ansible inventory of the servers ump installed
```

Exit 4: an account could not be listed; the others are, and a provider
that did not answer is listed from its last listing with a note.

## What changed (the drift check)

```sh
ump server list --diff                        # since the previous listing of each account
ump server list --since 7d                    # since the state as of a week ago (12h, 2026-10-01, ...)
ump server list --diff -o json                # changes: added, removed, changed (with the fields)
```

For a regular report: the added servers (to name and install, or to
ask about), the removed ones (cancelled? expired?), status changes
(`running` → `off`), and servers in no inventory. A server every account
of its type leaves out is forgotten by ump, which stderr says: report
it.

## What ump knows beyond the provider

```sh
ump server set NAME --price 39 --currency EUR --period month   # where the API does not say
ump server set NAME --attr owner=team-a --attr cost-center=42
ump server set NAME --profile web                              # reinstalled with web from now on
ump server set NAME --group monitoring                         # applied at the next ump provision
ump server add lab-01 --ipv4 192.0.2.50 --kind bare-metal --note "rack 3"   # a server no provider lists
ump server remove lab-01 --yes                                 # forget it
```

Each is a Change to the state: say what you set, and set only what the
task names.

## The inventory files

The servers ump installed are in the repository's inventory, one of
three ways: host files in `inventory/hosts/` (committed with the
repository), host files in a storage (`inventory/hosts.yml`), or ump as
the inventory (`inventory/ump.yml`). Compare `ump server list -o
ansible` with `ansible-inventory --list` to find hosts the inventory
lacks or still has. Host files are ump's: never edit them; overrides go
to `inventory/host_vars/`.
