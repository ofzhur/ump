# UMP — Unus Mundus Provisioner

> *Unus mundus* — "one world": the idea that beneath many different surfaces
> lies a single underlying reality.

UMP is a command line tool that puts **the same operating system, in the
same shape, on any server**: a dedicated server rented from a bare metal
provider or a VM in a public cloud, from one profile that says what the
machine should become.

Every provider has its own API, its own rescue system, its own stock
images and its own way of doing networking. UMP hides all of that behind
one CLI. `ump server list` shows the servers of every configured account;
`ump server reinstall NAME` boots a server into UMP's own rescue system,
erases its disks and installs an operating system (Ubuntu, Debian or
NixOS) the way a profile says; `ump ssh NAME` logs in. The result is a homogeneous fleet: the same
base system, disk layout, users and keys, firewall and interface names on
every machine, whatever it is and wherever it runs, ready for Ansible or
whatever configures it from there.

The same listing is the **inventory** of that fleet: one table over all
providers, in JSON for whatever consumes it, with what changed since the
last look, with the servers' prices (from the API where it tells, set by
hand where it does not) and facts of your own, and with servers no
provider lists added by hand. A provisioner that already knows every
machine needs no second tool to count them. See [inventory](docs/inventory.md).
Everything ump keeps, the accounts and secrets, the profiles, its key,
the state of the servers and the listing cache, is one directory, its
[home](docs/state.md): yours alone in `~/.config/ump`, or behind a
backend of your choice (a directory, SSH, S3, R2, HTTP, Consul, Vault)
that a team shares, mirrored on each machine and locked per server.

```console
$ curl -fsSL https://ump.zhurko.de/dl/get.sh | sh   # Linux or macOS, amd64 or arm64
$ ump config isp add                             # add a provider account (wizard)
$ ump server list                                # every server of every account
$ ump config os edit default                     # the profile: image, disks, users, firewall
$ ump server reinstall web-01 --dry-run          # the plan on that machine, nothing erased
$ ump server reinstall web-01                    # erase, install, reboot, verify
$ ump ssh web-01
```

The [quickstart](docs/quickstart.md) walks through these with their
output. Once a day ump says when a newer release is published, and the
same one-liner updates it; the scripts, images and profiles it uses come
from the storage at every use, so they are current without a new binary.

## Platforms

What ump works with: the providers whose machines it finds and controls,
the operating systems it puts on them, and later the configuration
management that takes over from there. See [platforms](docs/platforms.md).

**Providers.** Every account's machines are listed together, with their
prices where the provider says them; ump shows, renames, powers and
reinstalls them and sets their reverse DNS:

| Provider | Type | Servers | Rescue system for ump |
|---|---|---|---|
| Hetzner Robot | `hetzner-robot` | dedicated servers | yes |
| Hetzner Cloud | `hetzner-cloud` | the servers of a project | yes |
| OVHcloud | `ovh` | dedicated servers, VPS and Public Cloud instances | yes |
| DigitalOcean | `digitalocean` | droplets | no: ump must be able to log in before it installs |
| Linode (Akamai) | `linode` | cloud instances | yes: Linode's rescue mode, set up through its console |
| Vultr | `vultr` | cloud instances and bare metal servers | no: Vultr reinstalls with UMP's key when needed |
| by hand | `manual` | any machine with an address (`ump server add`) | no: ump must be able to log in |

Several accounts per provider are fine. [ISPs](docs/isps.md) says what
each does in detail, [provider accounts](docs/providers.md) how to set
them up.

**Operating systems.** Server systems out of the box, the same on every
machine, for amd64 and arm64, booting from UEFI and, on amd64, BIOS:
Ubuntu 26.04 (the default), 24.04 and 22.04 LTS and Debian 13 and 12,
prebuilt by UMP with LVM, md, netplan, SSH and the tools a server needs,
and nothing that does not belong there (no cloud-init, no snaps); and
NixOS 25.11,
installed from NixOS's public servers and its stable channel, with the
profile as NixOS modules. Each gets the
same disks, users and keys, interface names, firewall and message of
the day. See [operating systems](docs/oses.md).

**Configuration management** is planned: handing an installed machine
over to the tool that keeps it configured, with what ump knows about it
([roadmap](docs/roadmap.md)).

## The objects

Seven kinds of things are configured, all as YAML in `~/.config/ump/`,
each with its own `ump config` command (`list`, `get`, `create`,
`edit`, `delete`; `create` and `add` ask their questions on a terminal):

| Object | Command | What it is |
|---|---|---|
| [Provider accounts](docs/providers.md) | `ump config isp` | credentials for a provider API; secrets as references to files, environment variables or commands, never plaintext |
| [OS profiles](docs/profiles.md) | `ump config os` | what a server becomes: image, disk layout, users and keys, userdata, firewall, kernel command line, the step after the first boot |
| [Disk layouts](docs/disk-layouts.md) | `ump config disk-layout` | partitions, md arrays, volume groups and volumes, written once and applied to whatever disks a server has |
| [Firewalls](docs/firewalls.md) | `ump config firewall` | the input policy and the allowed ports, on the host and for the ports containers publish |
| [Users](docs/users.md) | `ump config user` | the accounts of an installed system: sudo, groups, shell, keys; listed by name in profiles, the first with sudo is the administrator |
| [Userdata](docs/userdata.md) | `ump config userdata` | what a freshly installed system gets on top of the profile: a script for Ubuntu, a NixOS module for NixOS; listed by name in profiles |
| [Keys](docs/keys.md) | `ump config key` | UMP's own SSH key, and public keys kept under names for the users |

The first use downloads the published defaults: the profiles
`ubuntu-26.04` (the default: Ubuntu 26.04 on every disk as one volume
group, the sudo user `ump` with your keys, ICMP and SSH open),
`ubuntu-24.04`, `ubuntu-22.04`, `debian-13`, `debian-12` and
`nixos-25.11` (NixOS from its
stable channel, see [NixOS](docs/oses.md#nixos)), the layouts `lvm-linear`,
`lvm-striped`, `lvm-raid1` and `lvm-example`, the firewalls `default`,
`web` and `open`, and the user `ump`. They are templates: make your own
from them (`ump config os create`, `ump config user create`, ...), and
`ump config os update` refreshes the published ones while keeping yours.

## How an installation works

1. ump looks the server up at its provider and resolves the profile on
   your machine: the layout, the firewall, the users' keys.
2. The server is brought into **UMP's rescue system**, a Debian that runs
   from RAM with the disks untouched: by a `kexec` from the running system
   when that lets UMP in as root, else through the provider's rescue
   system and a `kexec` from there. Either way the environment is the same
   at every provider.
3. The **installer** is fetched from the storage and run there with the
   resolved profile: it plans every partition, array and volume against
   the disks it finds, wipes the disks, builds the layout, unpacks the
   image, and configures what depends on the machine (hostname, network
   with uniform interface names, users and keys, userdata, firewall, boot
   loader for UEFI and BIOS).
4. The server reboots into the new system; ump waits until it answers
   over SSH with UMP's key and reports what came up.

`--dry-run` stops after the plan. Everything the server fetches (the
rescue image, the installer, the profiles, the OS image) comes from one
**storage**, `https://ump.zhurko.de/dl` by default, which can be [one of your
own](images/rescue/README.md#your-own-storage).

## Documentation

**Getting started**

| Document | Contents |
|---|---|
| [Quickstart](docs/quickstart.md) | install ump, decide where it keeps its things, add an account, look at the defaults, dry-run and reinstall a server, log in |
| [Concepts](docs/concepts.md) | the vocabulary and how the pieces fit, briefly |
| [Inventory](docs/inventory.md) | the listing as the inventory of the fleet: formats, what changed, prices, attributes, servers added by hand |
| [The state backend](docs/state.md) | where ump keeps everything: the local directory, or a backend shared by a team (a directory, SSH, S3, R2, HTTP, Consul, Vault), the setup wizard and migration, the mirror, locks per server |

**Objects**

| Document | Contents |
|---|---|
| [Provider accounts](docs/providers.md) | the provider types, their credentials, the wizard and the non-interactive form, secrets |
| [OS profiles](docs/profiles.md) | every field of a profile, the default profile, users, keys and the administrator, managing and using profiles |
| [Disk layouts](docs/disk-layouts.md) | parts, sizes and rules, the published layouts, examples with md and LVM |
| [Firewalls](docs/firewalls.md) | policy and allowed entries, forwarded ports of containers, the published firewalls |
| [Users](docs/users.md) | the fields of a user, the administrator and root, the published user, managing and using users |
| [Userdata](docs/userdata.md) | what a new system gets on top of its profile: fields, how it runs, the published entries, managing and using them |
| [Keys](docs/keys.md) | UMP's own key, the key references, named keys and how they reach a server |

**Platforms**

| Document | Contents |
|---|---|
| [Platforms](docs/platforms.md) | what ump works with: providers, operating systems, configuration management (planned) |
| [ISPs](docs/isps.md) | what each provider type does in detail: IDs, prices, power, reverse DNS, the way into a rescue system, what reinstall needs |
| [Operating systems](docs/oses.md) | what ump installs out of the box: Ubuntu 26.04, 24.04, 22.04, Debian 13, 12 and NixOS 25.11, what every system gets, how each is installed, NixOS configuration and userdata as modules |
| [Base images](images/base/README.md) | the Ubuntu and Debian systems UMP builds, what they hold and their defaults |

**Reference**

| Document | Contents |
|---|---|
| [CLI](docs/cli.md) | every command with its flags, output formats and exit codes |
| [Configuration](docs/configuration.md) | `config.yaml`, where everything lives, secret references, the storage and UMP's key |
| [Rescue image](images/rescue/README.md) | UMP's own rescue system and how it is built, published and entered; the layout of the storage |
| [Troubleshooting](docs/troubleshooting.md) | when the rescue system does not answer or the installed system does not boot: getting a server back, finding out without a console, a case study, contributing fixes |
| [Installer](install/README.md) | what runs in the rescue system: disks, md and LVM, the system, users, userdata, firewall, the boot loader |
| [Published profiles](profiles/README.md) | the directory the published profiles, layouts, firewalls, users and userdata come from |
| [Roadmap](docs/roadmap.md) | what is there, what comes next, open questions |
| [Skill](docs/skill.md) | design outline for a planned part: operating a fleet and its Terraform and Ansible repository with an AI agent |

## Principles

- **A CLI is enough.** No daemon, no database, no central state: one
  static binary, a configuration directory, the provider APIs and the
  machine itself. The provider APIs are the inventory; ump caches
  listings only to show what changed.
- **"Rescue → partition → unpack → configure → reboot" is universal.**
  Every provider can get a machine into some Linux with SSH. UMP uses
  that once, to get into its own rescue system, and from there the work
  is identical everywhere.
- **Provider differences belong at the edges.** A provider answers a few
  questions: which servers exist, how to reset them, how to get into a
  rescue system. Its quirks stay in its package.
- **Declarative inputs, a visible plan, explicit confirmation.** Profiles,
  layouts and firewalls are checked when edited; `--dry-run` shows the
  plan on the machine; nothing destructive happens without a yes, and
  nothing prompts without a terminal.
- **Usable by scripts and agents as by people.** `-o json` everywhere,
  stable exit codes, `--yes` where something is destructive, secret
  references instead of secrets.

## Non-goals

- Being a configuration management tool. UMP stops at "reachable,
  consistent base system" and hands off.
- Lifecycle orchestration of fleets (autoscaling, scheduling,
  reconciliation loops). UMP is an operator's tool, not a control plane.
- Ordering, billing or contract management at providers.
- Replacing Terraform for cloud resources. UMP is happy to take over
  instances that something else created.

## Author, support and services

UMP is made by Oleg Zhurko <oleg@zhurko.de>, a DevOps and infrastructure
engineer. Issues and pull requests are welcome here. For paid work
around it, such as setting UMP up for your fleet, a provider or an OS
image of your own, profiles and the provisioning that follows, or
running your infrastructure with it, write to the address above.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). Use it,
change it, ship it, commercially or not; keep the license and the
notice with copies, and name your changes. The images it installs stay
under their own licenses (Ubuntu, Debian and the software in them).
