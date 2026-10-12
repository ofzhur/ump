# UMP — Unus Mundus Provisioner

> *Unus mundus* — "one world": the idea that beneath many different surfaces
> lies a single underlying reality.

UMP is a command line tool that puts **the same operating system, in the same
shape, on any machine** — a dedicated server rented from a bare metal provider,
a VM in a public cloud or a box in your own rack.

Every provider has its own API, its own rescue system, its own stock images
and its own way of doing networking. UMP hides all of that behind one CLI
and one declarative description of what a machine should look like. The
result is a homogeneous fleet: identical disk layout conventions, identical
base OS, identical users and SSH access, a network configuration in the
same format, and a clean handoff to your configuration management (Ansible
or anything else).

> **Status:** early implementation. The CLI skeleton, the configuration
> wizard, and server listing and details for Hetzner Robot, Hetzner Cloud
> and OVHcloud exist (see [Getting started](#getting-started)). Everything else in
> these documents is design: it captures the ideas and the intended
> architecture so that the implementation starts from a shared picture.

## Principles

- **A CLI is enough.** An API service and a database in front of a
  provisioner add operational weight without adding capability. UMP is CLI
  only — no daemon, no central state.
- **"Rescue → partition → unpack image → configure → reboot" is universal.**
  Almost every provider can bring a machine into *some* Linux environment that
  we can SSH into. Once there, the work is identical everywhere.
- **Provider differences belong at the edges.** Providers should only answer
  a few questions: which machines exist, how do I power-cycle them, and how do
  I get them into a staging environment?
- **Bash does not scale for the installer.** Disk planning, RAID/LVM, bootloaders
  and network rendering need structure, tests and a dry run. UMP uploads
  itself to the target and does this work in Go, in its agent role.

## What UMP does

```
              ┌────────────── one profile ──────────────┐
              │ image · disk layout · users · network · │
              │ hostname template · handoff             │
              └────────────────────┬────────────────────┘
                                   │
   ump install web-01 --profile base
                                   │
   ┌───────────────┬───────────────┼───────────────┬───────────────┐
   ▼               ▼               ▼               ▼               ▼
Hetzner Robot  Hetzner Cloud     OVH / ...     Redfish/IPMI    plain SSH
 (rescue)       (rescue)        (rescue)     (virtual media)   (kexec)
   └───────────────┴───────────────┼───────────────┴───────────────┘
                                   ▼
                  staging environment (Linux + SSH)
                                   │
          inspect → plan → partition → install → configure
                                   │
                                   ▼
               reboot into installed OS → verify → handoff
                                                  (Ansible, script, ...)
```

1. **Find** the machine through the provider API (by name, IP or provider ID).
2. **Enter** a staging environment: the provider's rescue system, a kexec
   from the running OS, an ISO via out-of-band management, or PXE.
3. **Inspect** the hardware (disks, RAM, NICs, firmware mode, addresses).
4. **Plan** — resolve the abstract disk layout against the real disks, render
   the hostname and network config. Shown to the user before anything is
   destroyed.
5. **Partition and install** the OS image onto the disks.
6. **Configure** the installed system offline: bootloader (UEFI and legacy),
   fstab, hostname, users and SSH keys, network, sudo, base packages.
7. **Reboot** into the new system, leave rescue mode and **verify** that it
   comes back reachable with the expected configuration.
8. **Hand off** to configuration management — run an Ansible playbook, call a
   script, or let the machine pull its configuration.

## A first impression

The first two commands work today; `plan` and `install` are the intended
user experience.

```console
$ ump config                     # wizard: provider type, credentials, connection test
$ ump get servers
ACCOUNT   PROVIDER        ID        NAME      IPV4            LOCATION    STATUS
hrobot    hetzner-robot   1234567   web-01    203.0.113.10    fsn1-dc7    ready
hcloud    hetzner-cloud   7654321   edge-01   198.51.100.22   hel1-dc2    running

$ ump plan web-01 --profile base
machine   hetzner-robot/1234567 (web-01, 203.0.113.10)
hostname  web-01-fsn1-dc7.example.net
image     debian-13 (amd64, sha256:4e1f…)
disks     nvme0n1 (1.92T)  nvme1n1 (1.92T)
layout    auto-mirror → mirrored-lvm
  efi    512M  vfat   on nvme0n1p1, nvme1n1p1 (synced)
  /boot  1G    ext4   md0 raid1 (nvme0n1p3, nvme1n1p3)
  vg0          lvm    md1 raid1 (nvme0n1p4, nvme1n1p4)
    /     50G  ext4   vg0/root
    swap  8G   swap   vg0/swap
    /var  80%  ext4   vg0/var
network   eno1 (aa:bb:cc:dd:ee:ff) 203.0.113.10/26 gw 203.0.113.1, 2001:db8:1:2::2/64
users     oleg (sudo), deploy
handoff   ansible-playbook site.yml --limit web-01

ALL DATA ON nvme0n1, nvme1n1 WILL BE DESTROYED.

$ ump install web-01 --profile base
```

## Getting started

```console
$ git clone git@github.com:ofzhur/ump.git && cd ump
$ make                  # builds ./ump; `make install` puts it into your Go bin directory
$ ./ump config          # interactive: pick hetzner-robot, hetzner-cloud or ovh, enter credentials
$ ./ump get servers     # every server of every account; -o wide|json|yaml|name
$ ./ump describe server web-01   # everything the provider knows about one server
$ ./ump get servers --diff       # what was added, removed or changed since the last listing
$ ./ump rename server web-01 prod-web-01   # the name at the provider; the hostname follows at the next install
$ ./ump set rdns prod-web-01     # reverse DNS of the primary IPv4 = the server name
$ ./ump reboot server prod-web-01 --wait
$ ./ump rescue server prod-web-01          # boot the provider's rescue system with your SSH key
$ ./ump config test     # re-check credentials any time
```

Go 1.26 or newer builds it; an older `go` downloads the pinned toolchain
automatically. `make check` runs gofmt, vet and the tests, `make dist`
cross-compiles for Linux, macOS and Windows into `dist/`, and `make help`
lists the targets.

Credentials never land in `config.yaml` as plaintext: the wizard stores
them in files only you can read, or references environment variables or
a command such as `pass show …`. Without a terminal (CI, agents), use
`ump config add TYPE NAME --set KEY=VALUE …` — `ump config types` lists
the settings of each provider. See [CLI](docs/cli.md) and
[Configuration](docs/configuration.md).

## Two parts: the tool and the skill

`ump` is the mechanism. The second part of the project is a **skill** —
packaged operating knowledge in the Agent Skills format — that lets an AI
agent run a hybrid fleet and its Terraform and Ansible repository with
UMP: which tool owns which fact, how a machine goes from order to
service, what to check every week, how to take over a repository that has
grown for years. The tool is designed to be driven by an agent as safely
as by a person: machine-readable output everywhere, plans before changes,
and approval as a separate, human-only step. The skill ships inside the
binary — `ump skill install` writes it out — so the two never drift
apart. See [the skill](docs/skill.md).

## Documentation

| Document | Contents |
|---|---|
| [Concepts](docs/concepts.md) | The vocabulary: machines, providers, entry methods, images, layouts, profiles, handoff |
| [Architecture](docs/architecture.md) | Components, the install pipeline, the agent, provider interfaces |
| [CLI](docs/cli.md) | Draft command reference |
| [Configuration](docs/configuration.md) | Config files, profiles, users, network, secrets |
| [Disk layouts](docs/disk-layouts.md) | The declarative disk layout format |
| [Providers](docs/providers.md) | Provider capabilities and planned integrations |
| [Skill](docs/skill.md) | The UMP skill: operating a hybrid fleet and its Terraform/Ansible repository with an AI agent; reference repository layout; safety model |
| [Roadmap](docs/roadmap.md) | Milestones and open questions |

## Goals

- One static binary: the CLI, the on-target agent, the skill and the
  repository templates are all inside it. No server, no database.
- The same result on bare metal and in the cloud.
- Declarative inputs, a visible plan, and explicit confirmation before
  anything destructive happens.
- Provider integrations that are small and easy to add.
- Linux distribution support through pluggable OS families, starting with
  Debian and Ubuntu.
- Clean handoff instead of reinventing configuration management.
- Usable by AI agents as well as people: stable JSON output, plan/approve
  separation, and a skill that carries the operating procedures.

## Non-goals

- Being a configuration management tool. UMP stops at "reachable, consistent
  base system" and hands off.
- Lifecycle orchestration of fleets (autoscaling, scheduling, reconciliation
  loops). UMP is an operator's tool, not a control plane.
- Ordering, billing or contract management at providers.
- Replacing Terraform for cloud resource management. UMP can provision
  a cloud instance for convenience, but it is happy to take over instances
  that something else created.
- An autonomous operator. The skill lets an agent do the work; every
  destructive step needs a human approval the agent cannot produce itself.

## Author

Oleg Zhurko <oleg@zhurko.de>

## License

To be decided.
