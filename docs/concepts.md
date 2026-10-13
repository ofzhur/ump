# Concepts

This document defines the vocabulary used across UMP. The other documents
assume these terms.

## Machine

A **machine** is anything UMP can install an operating system on: a
dedicated server, a cloud VM or a physical box in a rack. A machine is
always seen *through* a provider and is identified by a **machine reference**:

| Form | Example | Meaning |
|---|---|---|
| `provider/id` | `hetzner-robot/123456` | Unambiguous, provider-native ID |
| `name` | `web-01` | Name as known at the provider |
| `ip` | `203.0.113.10` | Primary IPv4 or IPv6 address |

Names are not unique: accounts that were not set up with UMP's naming
convention can hold the same name twice, and some providers do not
enforce unique names even within one account. UMP never fails because of
that. A listing shows every server a reference matches. A command that
needs exactly one server asks which one is meant when it runs in a
terminal; without a terminal it fails and lists the candidates, so a
script or an agent retries with `provider/id` or `--account`.

UMP keeps no authoritative inventory of its own. The provider APIs are the
source of truth; UMP only caches listings locally to show what changed
between runs.

## Provider

A **provider** is an integration with something that can tell UMP about
machines and act on them: a bare metal API (Hetzner Robot, OVH, ...), a
cloud API (Hetzner Cloud, AWS, ...), an out-of-band controller (Redfish,
IPMI) or simply a list of SSH hosts.

A configured provider with credentials is an **account**. You can have
several accounts of the same provider type.

Providers differ widely in what they can do, so each one declares its
**capabilities** (see [providers](providers.md)):

- `inventory` — list and look up machines
- `power` — reboot, power on, power off
- `rescue` — boot the machine into a provider rescue system with our SSH key
- `create` / `destroy` — create and delete instances (clouds)
- `rdns` — manage reverse DNS
- `rename` — set the machine name at the provider
- `console` — get a serial / VNC console URL
- `virtual-media` — attach an ISO image (Redfish, some IPMI)
- `network-info` — report additional IPs, subnets and gateways

UMP picks the workflow based on what the provider can do, not on the
provider's name.

## Entry method

The **entry method** is how a machine gets from "whatever it is running now"
to the staging environment. This is the main place where providers differ.

| Method | How | Typical for |
|---|---|---|
| `rescue` | Provider API enables a rescue system, then a reboot | Hetzner Robot, Hetzner Cloud, OVH, Scaleway |
| `kexec` | SSH into the running OS, kexec into the UMP staging kernel | Cloud VMs without rescue, already-installed servers |
| `virtual-media` | Mount the UMP ISO through Redfish/IPMI, boot from it | Own hardware, enterprise servers |
| `pxe` | Netboot the UMP staging image through iPXE | Own racks, larger installations |
| `manual` | The user boots something with SSH and points UMP at it | Anything else |

Every entry method ends in the same state: a Linux system running from
RAM, reachable over SSH with UMP's key, with the target disks unmounted.

## Staging environment

The **staging environment** is the temporary Linux system the installation
runs in. It can be the provider's rescue system or UMP's own minimal
image (used by `kexec`, `virtual-media` and `pxe`).

UMP expects very little from it: a Linux kernel, SSH, and the usual block
device tools (`sgdisk`, `mdadm`, `lvm2`, `mkfs.*`, `wipefs`). Missing tools
are reported during inspection, before anything changes. In practice the
provider's rescue system is only the first hop: UMP kexecs from it into
its own image ([`images/rescue/`](../images/rescue/README.md)), so the
environment the agent runs in is the same at every provider.

## Agent

The **agent** is `ump` itself in its second role: the CLI uploads a copy
of the binary for the target's platform into the staging environment over
SSH and starts it as `ump agent`. It does the actual work on the machine:
inspecting hardware, partitioning, unpacking the image, configuring the
installed system. The CLI drives it remotely. See
[architecture](architecture.md#one-binary-two-roles).

## Facts

**Facts** are what the agent finds out about the machine during
inspection: disks (model, serial, size, rotational, transport), RAM, CPU,
firmware mode (UEFI or legacy BIOS), NICs and MAC addresses, current IPs,
gateways and DNS servers. Facts are combined with provider metadata, for
example the IPv6 subnet a provider assigned.

Facts feed the plan and are available to templates (hostname, network,
handoff variables).

## Image

An **image** is the operating system to install: a root filesystem archive
(`squashfs` or `tar.zst`) plus a small metadata file describing it:

- OS family and version (`debian-13`, `ubuntu-24.04`, ...)
- architecture
- checksum and optional signature
- what the image already contains (kernel, bootloader packages, SSH server)

Images are referenced by URL and verified by checksum. UMP can use stock
distribution cloud images as well as images built from a UMP image recipe.
Using the *same image* everywhere is a key part of making machines
homogeneous.

## OS family

The **OS family** driver knows how to configure a given distribution
offline: package manager, initramfs tooling, bootloader packages, network
renderer and service manager quirks. Debian and Ubuntu come first; the
interface leaves room for RHEL-like systems and others later.

## Disk layout

A **disk layout** is an abstract, declarative description of how disks
should be partitioned: which disks to use (by selectors like type, size or
count, not by device name), RAID levels, LVM volume groups, filesystems,
sizes and mount points. The same layout can be applied to machines with
different hardware. See [disk layouts](disk-layouts.md).

## Plan

The **plan** is the concrete result of resolving a profile against a
machine's facts: exact devices, partition sizes, RAID members, the rendered
hostname, the rendered network configuration, users and the handoff step.

The plan is always shown before destructive actions. `ump plan` produces it
without changing anything. `ump install` produces the same plan and asks for
confirmation (or accepts `--yes` together with a guard, see
[architecture](architecture.md#safety)).

## Profile

A **profile** bundles everything that describes *what a machine should
become*:

- image
- disk layout
- users and SSH keys
- network policy
- hostname template
- base settings (timezone, locale, kernel options, extra packages)
- handoff

Profiles are named and stored in configuration. Profiles can extend other
profiles, so "base" can be shared and "database" can override only the disk
layout.

## Handoff

The **handoff** is the last step: after the machine boots the installed
system and is verified, UMP passes control to configuration management.

- `ansible` — run `ansible-playbook` from the operator's machine against the
  freshly installed host
- `ansible-pull` — install a first-boot unit that runs `ansible-pull` from a
  repository
- `script` — run a local command with the machine's facts as environment /
  JSON input
- `remote-script` — run a script on the new machine
- `none`

UMP deliberately does not try to grow into configuration management.

## Run

A **run** is one execution of `ump install` against one machine. Each run
gets an ID and a local log directory with the plan, the agent's structured
log and the final result. Runs consist of **steps**, and long steps can be
resumed (for example after the operator's network connection dropped while
waiting for a reboot).

## Approval

An **approval** is recorded consent to a destructive run: bound to the
plan's hash, created by `ump approve` in an interactive terminal, expiring,
and consumed by `ump install --approved`. Separating approval from
execution lets an agent prepare and run an install while the decision
stays human.

## Machine overlay

The **machine overlay** (`ump/machines.yaml`) holds per-machine intent
that providers cannot carry: the profile, labels such as environment and
role, and one-off overrides. It lives in the infrastructure repository and
is read by UMP and, through the inventory export, by Ansible. See
[the skill](skill.md#the-machine-overlay).

## Skill

The **skill** is the agent-facing half of UMP: operating knowledge in the
Agent Skills format that tells an AI agent how to run a hybrid fleet and
its Terraform and Ansible repository with UMP — ownership of facts,
conventions, runbooks, the adoption path for an existing repository and
the safety model. See [the skill](skill.md).
