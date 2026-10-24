# Private networks

A private network is an IPv4 network in which every server ump installs
gets an address of its own: the lowest free one, or the one you give,
kept across reinstalls and never another server's. A server can be in
several networks, with an address in each. ump keeps the addresses in
the [state](state.md), hands them to the installer, which records them,
and puts them into the [inventory](inventory.md), where the roles that
connect the servers (a mesh per network) take them from. Nothing
configures an interface with them yet.

```console
$ ump config network list
NAME      CIDR          ADDRESSES   RESERVED   STATE   DESCRIPTION
default   10.0.0.0/24   254         -          ok      the private network of every server ump installs
```

## Where networks live

Named networks are the entries of `networks.yaml` in the profiles
directory, `~/.config/ump/profiles/networks.yaml`; the first use
downloads the published [`networks.yaml`](../profiles/networks.yaml)
there (see [profiles](profiles.md#where-profiles-live)), and so does the
first `ump config network` command in a profiles directory from before
it. A [profile](profiles.md) names one in `network.private`, a list of
them (the server is in each, in that order), or says `none`; a profile
that says nothing has none, so older profiles stay as they were. Every
published profile says `default`, and so does a profile the wizard
writes.

```yaml
# networks.yaml
networks:
  default:
    description: the private network of every server ump installs
    cidr: 10.0.0.0/24
```

```yaml
# in a profile
network:
  rename: true
  private: default            # a name from networks.yaml, a list of them ([default, db]), or none
```

`ump config os update` adds the published entry (`default`) where
`networks.yaml` lacks it and keeps every network there: the ones you
added, and a `default` you edited, which survives updates as it is (the
servers' addresses were given in it). A published network that would
overlap one of yours is left out, and the update says so.

## Fields

```yaml
db:
  description: the databases  # optional; shown by ump config network list
  cidr: 10.0.1.0/24           # an IPv4 network, /8 to /30
  reserved: [10.0.1.1]        # optional; addresses no server gets
```

| Field | Meaning |
|---|---|
| `cidr` | The network: its own address and a prefix length of `/8` to `/30` (`10.0.1.0/24`, not `10.0.1.5/24`). IPv4 only, for now. |
| `reserved` | Addresses inside the network that no server gets, automatically or by hand: a gateway's, a load balancer's, an address of a machine ump does not install. Each once, neither the network's own address nor its broadcast address. |
| `description` | One line, shown by `ump config network list`. |

The network's own address and its broadcast address are never given
either, so a `/24` gives 254 addresses, minus the reserved ones (the
`ADDRESSES` column). Networks do not overlap: an edit that makes one
overlap another is refused, and ump uses no `networks.yaml` where two
do. A name is lower-case letters, digits, `.`, `_` and `-`; `none` is
no network's name: it is what a profile says for no address.

## Addresses

```console
$ ump server reinstall web-01                          # 10.0.0.1: the lowest free address of default
$ ump server reinstall db-01 --private-ip 10.0.0.10    # given by hand
$ ump server reinstall web-01                          # keeps 10.0.0.1
$ ump server reinstall db-01 --private-network default,db --private-ip db=10.0.1.10
                                                       # keeps 10.0.0.10 in default, 10.0.1.10 in db
$ ump server reinstall lab-01 --private-network none   # no address; lab-01's are free again
$ ump server set db-01 --private-ip default=10.0.0.11  # without a reinstall; ump provision carries it
$ ump server list                                      # the PRIVATE-IPV4 column: 10.0.0.11,10.0.1.10
```

- **The networks** of an installation are those of `--private-network
  NAME[,NAME]` (repeatable, in order), else the profile's
  `network.private` (`--set network.private=NAME`, or `[NAME, NAME]`,
  too); `none` gives the server no address and frees those it had. The
  server gets an address in each, and the first network is its first:
  the one of the inventory's single variables and of `private_network`
  in listings.
- **Automatic:** the lowest address of the network that is neither its
  own nor its broadcast address, nor reserved, nor another server's.
- **By hand:** `--private-ip NETWORK=ADDR` (repeatable) gives that
  address in that network, one of the server's; `--private-ip ADDR`
  alone does so for a server in one network. It must be in the network,
  neither its own nor its broadcast address nor reserved, and no other
  server's.
- **Kept:** a server that is reinstalled in a network it has an address
  in keeps that address, as long as the network still gives it (after an
  edit of `cidr` or `reserved` that leaves it out, the lowest free one),
  whatever the order of its networks; the address of a network it
  leaves is free again.
- **One server's alone:** an address is one server's in every network
  that has it, whatever the network's name: another server's address in
  a renamed network, or in one with a `cidr` that overlaps, is taken
  too. Every check comes before anything is touched, and a failure stops
  ump with exit status 2, naming the server that has the address. In
  `ump server multiinstall` the servers of the run count too: the
  addresses the servers keep come first, then those given by hand
  (refused when another server of the run keeps it), then the lowest
  free ones, in order; `private-ip` goes in a plan per server (a list of
  `NETWORK=ADDR`), and is refused in `defaults` and for several servers
  on the command line, which would give them all one address.
- **Recorded before the installation:** once the question is answered,
  ump takes the server's lock (a multiinstall every server's, and one
  held elsewhere stops it with exit status 3, nothing touched), then
  records the addresses in the state, in one read-modify-write that
  checks them again; an address another ump gave meanwhile, or a server
  whose address changed meanwhile, stops the installation, or all of a
  multiinstall, with nothing touched (exit status 2, run it again). So
  no two servers ever get one address, and what the installed system
  records is what the state says. An installation that fails before its
  installer wrote the disks gives its server the addresses it had before
  back, under the server's lock, unless another server got one of them
  meanwhile (then it keeps the new ones); once the installer wrote them
  (or the connection ended while it ran), the addresses are the new
  system's, whatever fails after. `--dry-run` records nothing.
- **Later:** `ump server set NAME --private-network NAME[,NAME]`
  replaces the networks of a server ump installed (the address it has
  in each, else the lowest free one), `--private-ip NETWORK=ADDR`
  changes its address in one of them, `--private-network none` takes
  them all away; the same checks apply, within the state's
  read-modify-write, under the server's lock (exit status 3 while an
  installation holds it: a multiinstall from its question on, for every
  server of the run). Nothing runs: the next `ump provision` writes the
  host file with them.
- **A server that is gone** from its provider keeps its entry in the
  state, and its addresses with it, until the entry is removed (in
  `state.yaml`; `ump server remove` does it for a server added by hand).

## Where the addresses show

| Where | What |
|---|---|
| `ump server reinstall` | the question and the progress: `Installing ubuntu-26.04 (profile ubuntu-26.04 from ..., layout lvm-linear, private addresses 10.0.0.1 in default, 10.0.1.1 in db) on web-01 as web-01 ...`; `-o json`: `private_networks`, and `private_network` and `private_address` of the first |
| `ump server multiinstall` | the `PRIVATE-IPV4` column of the table (comma-separated, `none` without), the progress line of each server, `-o json` per server |
| `state.yaml` | `private_networks: [{network: default, cidr: 10.0.0.0/24, address: 10.0.0.1}, ...]` per server, in order ([state](state.md)) |
| the installer | `network.private_networks` of the specification (`network`, `cidr`, `address` each), and `network.private`, the first; `/etc/ump/install` records `private=default:10.0.0.1,db:10.0.1.1`, and `private_network=default` and `private_address=10.0.0.1` of the first |
| the inventory | `ump_private_networks` (`{default: {address: 10.0.0.1, cidr: 10.0.0.0/24}, db: {...}}`), and of the first network `ump_private_address` (`10.0.0.1`), `ump_private_network` (`default`) and `ump_private_cidr` (`10.0.0.0/24`), in the host files and `ump server list -o ansible`, absent for a server without one ([inventory](inventory.md#what-ump-installed)) |
| `ump server list` | `PRIVATE-IPV4` right after `IPV4` (`10.0.0.1,10.0.1.1`); `private_networks`, and `private_network` and `private_address` of the first, in `-o json` and `-o yaml` |
| `ump server show` | `Private: 10.0.0.1 in default (10.0.0.0/24), 10.0.1.1 in db (10.0.1.0/24)` under UMP |

## Mesh roles

The addresses are there for the roles that connect the servers, such
as `ofzhur.ansible.wireguard` in the collection
[ofzhur.ansible](https://github.com/ofzhur/ansible): a mesh per
network, each host on one tunnel interface per network it is in, with
its address there inside the network's CIDR and `ansible_host` as its
endpoint. The networks are apart: a host's peers in a network are the
other hosts of that network, hosts of different networks are in no
mesh together, a host in several networks is a peer in each, and a host
without `ump_private_networks` stays out. Within a network the role
isolates by group: a host's groups for that are its groups listed in
`wireguard_isolation` (for example `[foo, bar]`). Two hosts get a
tunnel unless they are in different groups; a host in a group accepts
new connections over the mesh only from hosts of its group, and reaches
the others itself (the collection's documentation has the details). The
inventory
has everything such a role needs, so it reads the same from host files
as from ump itself:

```yaml
# a role's view of its peers in the network net
mesh_peers: "{{ groups['common'] | map('extract', hostvars)
                | selectattr('ump_private_networks', 'defined')
                | selectattr('ump_private_networks', 'contains', net)
                | rejectattr('inventory_hostname', 'equalto', inventory_hostname)
                | list }}"
```

`ump_private_address`, `ump_private_network` and `ump_private_cidr`
are those of a host's first network, for roles written for one.

## Provider uplinks

Not implemented yet; notes for the topology to come.

Several providers connect their servers privately, without the public
internet: OVHcloud's vRack (dedicated servers and Public Cloud, across
locations), Hetzner's vSwitch (Robot servers, a VLAN within a network
zone) and Cloud Networks (cloud servers, which a vSwitch can join),
Vultr's VPC, Linode's VLAN and VPC, and DigitalOcean's VPC (each within
a location or region). Between servers of one provider, and mostly of
one location, such an uplink carries the traffic off the public
internet: no public bandwidth, lower latency, and no tunnel needed to
reach the peers.

Private is not encrypted: traffic over a vRack, a vSwitch or a VPC
crosses the provider's network as it is. Confidential traffic keeps a
tunnel over the uplink, which costs little there (no public bandwidth,
a short path); the uplink spares the tunnel only for what may cross the
provider's network in the clear.

A mesh can make use of that: the servers on one uplink form an island
that talks over it directly (or through tunnels over it, for
confidential traffic), and tunnels run only between islands, that
is between providers or locations, through one or two gateways per
island instead of every pair of servers (a handful of tunnels instead
of n²). What it takes:

- **The uplink per server:** which interface is attached (the installer
  names it `int01` and leaves it up without an address), and its
  attachment at the provider (vRack membership, vSwitch VLAN, VPC),
  through the provider's API where there is one, as a capability of
  the provider type ([ISPs](isps.md)).
- **The addressing on it:** a layer-2 uplink (vRack, vSwitch, Linode
  VLAN) takes the private address as it is; a layer-3 VPC (Hetzner
  Cloud Networks, Vultr, DigitalOcean, Linode VPC) gives addresses of
  its own, so the private address is routed over it, or the network's
  `cidr` is the VPC's.
- **The islands in the inventory:** per host, which uplink it shares
  with which peers (provider, location, uplink ID), so that a role
  peers directly where it can and tunnels where it must.
- **MTU:** a vSwitch takes 1400 bytes; a tunnel takes its header off
  whatever the path allows.

## Managing networks

```
ump config network list                          # the networks, their CIDR, addresses and reserved ones
ump config network get NAME                      # print one
ump config network create [NAME] [--from NAME]   # copy an entry (default: default), then your editor on it (give it a cidr of its own); asks on a terminal
ump config network edit [NAME]                   # your editor on that network alone, or on all of networks.yaml; checked afterwards
ump config network delete NAME [--yes]
```

`create` and `delete` edit `networks.yaml` in place; its comments
survive. A copy overlaps its original until its `cidr` changes, which
`create` says. `edit NAME` opens that network alone and puts it back
when it is valid, the others untouched; `edit` without a name opens the whole
file and checks every network when the editor returns. Deleting a
network leaves the servers' addresses in it as they are, in the state
and the inventory, until a reinstall or `ump server set` takes them
out. The command is also `net`, and
the verbs have the short forms of every `ump config` entity (`ump c net
ls`); see the [CLI reference](cli.md#ump-config).
