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
│  │ config  │  │ planner  │  │ runner   │  │        │  ump agent (same binary) │
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

`ump` is **one binary**. The same program runs on the operator's machine
as the CLI and on the target as the agent (`ump agent`, an internal
subcommand started over SSH). Every `ump` build carries compressed copies
of itself for the target platforms, so the macOS or Windows `ump` on a
laptop can put a matching agent on any Linux machine — see
[One binary, two roles](#one-binary-two-roles).

There is no server, no database and no long-running process. Everything
UMP needs is in the binary, the configuration files, the provider APIs and
the machine itself. The only things fetched from elsewhere are data: OS
images and the staging kernel and initramfs, by URL with pinned checksums.

## Language and dependencies

Go, because:

- static binaries; cross-compilation makes the embedded target payloads
  cheap
- good SSH support (`golang.org/x/crypto/ssh`) — no dependency on a local
  `ssh` binary, which matters for Windows and CI
- existing provider SDKs (Hetzner Robot and Cloud, OVH, AWS, ...) and
  Redfish clients
- both roles are one program, so a plan computed by the CLI is exactly
  what the agent executes, down to the struct

On the target, the agent calls standard tools (`sgdisk`, `mdadm`, `lvm`,
`mkfs.*`, `wipefs`, `unsquashfs`, `chroot`) instead of reimplementing them.
Those tools are well tested and present in practically every rescue system.

## One binary, two roles

| Role | Runs on | Started by | Does |
|---|---|---|---|
| CLI | operator workstation, CI runner | the user or an AI agent | config, providers, planning, driving the pipeline, handoff |
| agent | the target's staging environment, from RAM | the CLI, over SSH, as `ump agent` | inspect, partition, install, configure, bootloader |

Both roles are the same code. The CLI refuses to drive an agent whose
version and commit differ from its own; this is checked in the first
handshake message.

How the agent gets onto the target:

1. If the operator's OS and architecture match the target's (a Linux
   workstation or CI runner installing a Linux machine), the CLI uploads
   the binary it is running.
2. Otherwise it uses an **embedded payload**: every release of `ump`
   contains compressed builds of itself for `linux/amd64` and
   `linux/arm64`. The payloads are built from the same commit with the
   `agent` build tag, which leaves out provider SDKs and operator-side
   commands and keeps each one at a few megabytes. The CLI decompresses
   the right one in memory and uploads it over the SSH session.
3. `--agent-binary <path>` overrides both, for development and for
   debugging the agent.

Release builds are therefore two-stage: build the `agent`-tagged payloads
for Linux, embed them, then build `ump` for every operator platform
(Linux, macOS, Windows; amd64 and arm64). A development build without
payloads still works on a matching platform and fails with a clear
message otherwise.

What is inside the binary:

- the CLI and the agent role, with the target payloads
- the skill (`ump skill install` writes it out) and the reference
  repository skeleton and templates (`ump repo init`)
- built-in defaults: a `base` profile, common disk layouts, image
  definitions for Debian and Ubuntu
- the Ansible inventory protocol (`ump inventory ansible`) — no
  interpreter or helper script is needed anywhere

What is not, by design, because it is data rather than tool: OS images
and the staging kernel and initramfs (for `kexec`, the virtual-media ISO
and PXE). Both are referenced by URL with a pinned checksum, cached under
`~/.cache/ump/`, and can be pushed to the target over SSH when it has no
outbound access.

## Proposed code layout

```
cmd/
  ump/                the one entry point; `ump agent` is the target-side role
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
    payload/          embedded target builds (linux/amd64, linux/arm64)
    proto/            message types shared by CLI and agent
    inspect/          hardware discovery
    disk/             wipe, partition, md, lvm, mkfs, mount
    image/            fetch, verify, unpack
    osfamily/         offline configuration per distribution
      debian/
    network/          canonical network model + renderers
    boot/             bootloader installation (UEFI + legacy)
  handoff/            ansible, ansible-pull, script
  repo/               `ump repo init` (skeleton, templates), `ump repo audit`
  skill/              `ump skill install`: writes the embedded skill
skills/
  ump/                the skill, embedded into the binary at build time
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
| 3 | `bootstrap` | CLI → target | Upload `ump` for the target's platform (itself or the embedded payload), start `ump agent`, handshake versions, check required tools |
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
and, where available, the provider's power/status API. UMP reports what
it is waiting for and for how long, rather than silently counting down a
fixed number of retries.

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
reset when the server is switched off. Both belong in the `hetznerrobot`
package and nowhere else.

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

The agent is `ump` itself, started over SSH as `ump agent` (see
[One binary, two roles](#one-binary-two-roles)). The CLI and the agent
talk over the SSH session's stdin/stdout with newline-delimited JSON
messages:

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

cloud-init is removed from stock cloud images by default, so that
provider metadata cannot silently change what UMP configured.
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
the start.

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

UMP uses GRUB for both firmware modes rather than a second loader for
legacy boot, which means one configuration to generate.

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

Provider credentials are never stored as plaintext in the main config.
Config values reference secrets:

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
~/.cache/ump/           listing snapshots (listings/<config>/<account>/<time>.json),
                        downloaded OS and staging images
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
