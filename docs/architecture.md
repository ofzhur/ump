# Architecture

This document outlines how UMP is structured. It is a design sketch, not a
specification: names and interfaces will change once implementation
starts.

## Overview

```
 operator workstation / CI runner                         target machine
┌───────────────────────────────────────────┐        ┌──────────────────────────┐
│ ump (CLI)                                 │        │ staging environment      │
│                                           │  SSH   │ (rescue / UMP image)     │
│  ┌─────────┐  ┌──────────┐  ┌──────────┐  │───────▶│                          │
│  │ config  │  │ planner  │  │ runner   │  │        │  ump-agent               │
│  │ profiles│  │ layouts  │  │ pipeline │  │◀───────│   inspect / partition /  │
│  │ secrets │  │ templates│  │ steps    │  │ events │   install / configure    │
│  └─────────┘  └──────────┘  └────┬─────┘  │        │                          │
│                                  │        │        │  disks ──▶ installed OS  │
│  ┌───────────────────────────────▼─────┐  │        └──────────────────────────┘
│  │ providers                           │  │                    ▲
│  │ hetzner-robot · hetzner-cloud · ovh │──┼── HTTPS ──▶ provider API
│  │ redfish · ipmi · ssh · ...          │  │            (rescue, reboot, rDNS,
│  └─────────────────────────────────────┘  │             create, metadata)
│                                           │
│  local state: cache, run logs             │──── handoff ──▶ ansible-playbook / script
└───────────────────────────────────────────┘
```

UMP consists of two binaries built from one Go module:

- **`ump`** — the CLI the operator runs. It talks to provider APIs, computes
  plans, drives the installation and performs the handoff.
- **`ump-agent`** — a static Linux binary (amd64 and arm64) that is
  uploaded into the staging environment and does the on-machine work.

There is no server, no database and no long-running process. Everything
UMP needs is in the configuration files, the provider APIs and the machine
itself.

## Language and dependencies

Go, as in the earlier tool:

- static binaries, trivial cross-compilation for the agent
- good SSH support (`golang.org/x/crypto/ssh`) — no dependency on a local
  `ssh` binary, which matters for Windows and CI
- existing provider SDKs (Hetzner Robot and Cloud, OVH, AWS, ...) and
  Redfish clients
- the CLI and the agent share data types (facts, plans, layouts), so a
  plan computed by the CLI is exactly what the agent executes

On the target, the agent calls standard tools (`sgdisk`, `mdadm`, `lvm`,
`mkfs.*`, `wipefs`, `unsquashfs`, `chroot`) instead of reimplementing them.
Those tools are well tested and present in practically every rescue system.

## Proposed code layout

```
cmd/
  ump/                CLI entry point
  ump-agent/          agent entry point
internal/
  cli/                cobra commands, output formatting (table/json/yaml)
  config/             config loading, profiles, inheritance, validation
  secrets/            secret references: env, file, command (pass, op, sops)
  provider/           provider interfaces + registry
    hetznerrobot/
    hetznercloud/
    redfish/
    ssh/              "generic" provider: a static list of hosts
    fake/             in-memory provider for tests
  entry/              entry methods: rescue, kexec, virtual-media, pxe, manual
  transport/          SSH connection handling, retries, file upload
  pipeline/           run orchestration, steps, resume, run logs
  plan/               resolve profile + facts -> plan
  layout/             disk layout model, validation, resolver
  facts/              facts model (shared with agent)
  agent/
    proto/            message types shared by CLI and agent
    inspect/          hardware discovery
    disk/             wipe, partition, md, lvm, mkfs, mount
    image/            fetch, verify, unpack
    osfamily/         offline configuration per distribution
      debian/
    network/          canonical network model + renderers
    boot/             bootloader installation (UEFI + legacy)
  handoff/            ansible, ansible-pull, script
images/               recipes for UMP staging and OS images
docs/
```

## The install pipeline

`ump install <machine> --profile <name>` is a sequence of steps. Each step is
idempotent where possible and records its result in the run log, so a run
can be resumed with `ump install --resume <run-id>`.

| # | Step | Runs on | What happens |
|---|---|---|---|
| 1 | `resolve` | CLI | Look up the machine at the provider, load and validate the profile |
| 2 | `enter` | CLI ↔ provider | Bring the machine into the staging environment using the chosen entry method; wait for SSH |
| 3 | `bootstrap` | CLI → target | Upload `ump-agent` (matching architecture), check required tools |
| 4 | `inspect` | agent | Collect facts: disks, RAM, CPU, firmware mode, NICs, addresses |
| 5 | `plan` | CLI | Resolve layout + templates + network against facts and provider metadata |
| 6 | `confirm` | CLI | Show the plan, require confirmation or a guard (see [safety](#safety)) |
| 7 | `prepare-disks` | agent | Stop old md/LVM, wipe signatures, partition, build RAID/LVM, mkfs, mount |
| 8 | `install-image` | agent | Download (or receive) the image, verify checksum, unpack to target root |
| 9 | `configure` | agent | Offline configuration in a chroot: fstab, hostname, users, SSH, network, sudo, packages, kernel options |
| 10 | `bootloader` | agent | Install bootloader for UEFI and/or legacy BIOS on all boot disks |
| 11 | `finalize` | agent + provider | Unmount cleanly, disable provider rescue mode, reboot |
| 12 | `verify` | CLI | Wait for SSH on the installed system with the new host key, check hostname, mounts, network |
| 13 | `handoff` | CLI | Run the configured handoff (Ansible, script, ...) |

Steps 2–3 and 11 depend on the provider and entry method. Steps 4–10 are the
same for every machine — this is where the homogeneity comes from.

### Waiting and timeouts

Reboots into rescue or into the installed system can take anywhere from 30
seconds (cloud VM) to 10+ minutes (bare metal with slow firmware and many
disks). Each step has a configurable timeout; waiting is done by polling SSH
and, where available, the provider's power/status API. The earlier tool's
experience was a fixed `50 × 20s` loop — UMP should report what it is
waiting for and why.

## Providers

Providers are small. They translate between a provider API and UMP's model
and declare what they can do:

```go
// Every provider implements this.
type Provider interface {
	Type() string               // "hetzner-robot"
	Capabilities() CapabilitySet
}

// Optional interfaces — a provider implements those it supports.
type Inventory interface {
	List(ctx context.Context) ([]Machine, error)
	Get(ctx context.Context, id string) (Machine, error)
}

type Power interface {
	Reboot(ctx context.Context, m Machine, mode RebootMode) error // soft, hard, power-cycle
	PowerOn(ctx context.Context, m Machine) error
	PowerOff(ctx context.Context, m Machine) error
}

type Rescue interface {
	EnableRescue(ctx context.Context, m Machine, opts RescueOptions) error // SSH key, rescue flavour
	DisableRescue(ctx context.Context, m Machine) error
}

type Creator interface {
	Create(ctx context.Context, spec CreateSpec) (Machine, error)
	Destroy(ctx context.Context, m Machine) error
}

type NetworkInfo interface {
	// Additional addresses, subnets, gateways the provider assigned
	// (e.g. an IPv6 /64 or failover IPs) that cannot be discovered from
	// inside the machine.
	Network(ctx context.Context, m Machine) (ProviderNetwork, error)
}

type ReverseDNS interface {
	SetPTR(ctx context.Context, ip netip.Addr, name string) error
}

type VirtualMedia interface {
	InsertMedia(ctx context.Context, m Machine, isoURL string) error
	EjectMedia(ctx context.Context, m Machine) error
	SetNextBoot(ctx context.Context, m Machine, dev BootDevice) error
}
```

Capabilities are checked with type assertions. The CLI uses them to choose
the entry method and to show which commands are available for which
machine. Provider-specific quirks stay inside the provider — for example,
Hetzner Robot requires the SSH key to be registered at the account before
it can be used for rescue, and needs a power reset instead of a hardware
reset when the server is switched off. Both were handled in the earlier tool and move
into the `hetznerrobot` package unchanged in spirit.

Adding a provider means implementing `Provider`, `Inventory` and at least
one of `Rescue`, `VirtualMedia` or `Power` (to be usable with an entry
method), plus registering it.

## Entry methods

Entry methods sit between providers and the pipeline:

```go
type EntryMethod interface {
	Name() string
	Supported(p Provider, m Machine) bool
	// Enter brings the machine into the staging environment and returns
	// SSH connection details for it.
	Enter(ctx context.Context, p Provider, m Machine, key ssh.Signer) (Target, error)
	// Leave reverts anything Enter changed at the provider (rescue flag,
	// virtual media, next-boot device) so the next boot is from disk.
	Leave(ctx context.Context, p Provider, m Machine) error
}
```

Default selection order: `rescue` → `virtual-media` → `kexec` → `manual`.
The profile or a CLI flag can force one.

`kexec` deserves a note: for clouds without a usable rescue system, UMP
connects to the running stock OS, uploads a staging kernel and initramfs
and kexecs into it. The machine then runs entirely from RAM and the disk
is free to be overwritten. This is the trick that makes "any cloud VM"
installable without provider-specific image import.

## The agent

The agent is a static binary started over SSH. The CLI and the agent talk
over the SSH session's stdin/stdout with newline-delimited JSON messages:

```
CLI   → agent   {"id":1,"op":"inspect"}
agent → CLI     {"id":1,"event":"progress","msg":"scanning block devices"}
agent → CLI     {"id":1,"result":{"disks":[...],"firmware":"uefi",...}}
CLI   → agent   {"id":2,"op":"prepare-disks","plan":{...}}
agent → CLI     {"id":2,"event":"log","level":"info","msg":"wipefs /dev/nvme0n1"}
...
```

Why an agent instead of shell scripts:

- the plan is a typed structure the CLI computed, the agent executes it
  verbatim — no re-parsing of JSON with `jq` in bash
- structured progress and errors flow back to the CLI and into the run log
- every step can be unit tested, and integration tested in QEMU
- `--dry-run` on the agent prints the exact commands without running them

The agent has no network listener, no persistent state on the target and
removes itself before the final reboot.

## Offline configuration

UMP configures the installed system **before its first boot**, from the
staging environment, by writing files and running commands in a chroot.
It does not depend on cloud-init or any first-boot agent for the basics:

- the result is deterministic and visible in the plan
- the same mechanism works on bare metal, where there is no metadata
  service
- a broken network config is caught before the reboot, not after

cloud-init is removed from stock cloud images by default (as the earlier tool already
did), so that provider metadata cannot silently change what UMP configured.
A profile may opt in to keep it.

Only things that require a running system are deferred to an optional
first-boot unit (`ump-firstboot.service`), for example `ansible-pull`.

## Network model

Network configuration is described once, in a canonical model, and rendered
by the OS family driver:

```
interfaces: matched by MAC (from facts), renamed to stable names (e.g. "ext0")
addresses:  static from facts/provider metadata, or DHCP
routes:     including on-link gateways (Hetzner style /32 setups)
ipv6:       provider-assigned subnet, first address by default
dns:        resolvers and search domains
extras:     bonds, VLANs, bridges (later)
```

Default renderer: **systemd-networkd** — it is available on all target
distributions and produces the same files everywhere. Netplan (Ubuntu) and
NetworkManager (RHEL-like) can be added as alternative renderers. By
default, the address configuration is static, taken from what was observed
in the staging environment plus provider metadata — this is what
works reliably on bare metal providers. IPv6 is a first-class citizen from
the start (it was a TODO in the earlier tool).

## Bootloader

Every installation must boot whether the firmware starts it in UEFI or
legacy mode, and must survive the loss of any single boot disk when the
layout uses RAID:

- GPT on all boot disks with an ESP and a BIOS boot partition
- GRUB installed for both `x86_64-efi` and `i386-pc` on every boot disk
- the ESPs are kept in sync (not on md RAID; copied, with a hook for kernel
  updates)
- `/boot` on md RAID1 when the layout is mirrored
- UEFI boot entries created for each disk; provider-specific boot order
  (e.g. PXE first at Hetzner, so rescue keeps working) is respected

The earlier tool used GRUB for UEFI and extlinux for legacy. UMP standardises on GRUB
for both, which means one configuration to generate.

## Safety

Installing an OS destroys data. UMP should make it hard to do on the wrong
machine:

- `ump plan` never changes anything; `ump install` always prints the plan
- interactive confirmation requires typing the machine name
- non-interactive runs need `--yes` **and** a guard, e.g.
  `--confirm-machine hetzner-robot/1234567` or
  `--confirm-disks <serial>,<serial>` — so a script cannot accidentally hit
  the wrong host
- disk selectors match on stable attributes (type, size, serial), never on
  kernel device names alone
- a machine that is already in rescue mode, has mounted target disks or
  shows unexpected hardware compared to the plan stops the run
- every run leaves a log with the plan and all commands executed
- approval can be separated from execution: `ump approve <run-id>` records
  a TTY-only, plan-hash-bound, expiring approval that
  `ump install --approved` consumes — the basis of the agent workflow in
  [the skill](skill.md)
- UMP never prompts without a TTY; it fails with exit code 3 and names the
  missing approval

## Agent interface

UMP is meant to be driven by AI agents as well as people; the operating
knowledge for that lives in [the skill](skill.md). The tool's side of the
contract:

- stable `-o json` for every command; plan output carries a `plan_hash`
  and a text `summary`
- approval as a separate, TTY-only command; no prompts without a TTY
- structured errors with a code and a hint
- secrets redacted from every output stream, including verbose and debug
- `ump inventory export` and `ump machine import` as the seams to Ansible
  and Terraform; the machine overlay (`machines.yaml`) for per-machine
  intent
- `ump doctor` to check that the installed skill and the CLI match

None of this is agent-specific; scripts and CI benefit from the same
properties.

## Secrets and keys

Provider credentials are never stored as plaintext in the main config
(they were in the earlier tool). Config values reference secrets:

```yaml
password: { env: HROBOT_PASS }
password: { file: ~/.config/ump/secrets/hrobot }
password: { command: "pass show robot/password" }
```

SSH keys come from files or the SSH agent. UMP uses a dedicated key for
staging access by default (generated per run or per account), and installs
the users' own keys into the new system. Host keys of the installed
system are generated in the staging environment, so UMP knows them before
the first boot and can verify the installed system without
trust-on-first-use; they can be written to `known_hosts` on request.

## State

UMP keeps only local, disposable state under XDG directories:

```
~/.config/ump/          config, profiles, layouts (user editable)
~/.cache/ump/           machine listing cache, downloaded images and agents
~/.local/state/ump/runs/<run-id>/
                        plan.json, facts.json, events.jsonl, result.json
```

Deleting the cache and state is always safe.

## Concurrency

`ump install` accepts several machines (`ump install web-01 web-02 web-03`)
or a selector, and runs pipelines in parallel with a configurable limit.
Output switches to a per-machine progress view; per-machine details are in
the run logs.

## Testing strategy

- **Unit tests** for layout resolution, templates, network rendering and
  config validation — mostly pure functions over facts.
- **Agent integration tests** in QEMU: VMs with different disk sets
  (single disk, 2× NVMe, NVMe + SATA, 4 disks), booted in both UEFI (OVMF)
  and BIOS mode, running the full pipeline through the `manual` entry
  method and verifying that the result boots.
- **Provider tests** against recorded API responses; a `fake` provider
  for end-to-end CLI tests.
- Real-provider smoke tests run manually or on a schedule against cheap
  test machines.
