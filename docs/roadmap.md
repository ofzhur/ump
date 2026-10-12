# Roadmap and open questions

## Milestones

Done so far: the Go module and CLI skeleton with the kubectl-style command
shape, configuration with secret references, the `ump config` wizard, and
`ump get servers`, `ump describe server`, `ump rename server`,
`ump get|set|delete rdns`, `ump reboot|start|stop server` and
`ump rescue|unrescue server` for Hetzner Robot, Hetzner Cloud and
OVHcloud, and the listing cache with `--diff`, `--since` and `--cached`
(October 2026).

### M0 — Skeleton

- Go module, the single `ump` binary with the agent role, embedded target
  payloads (linux amd64/arm64) and the two-stage release build for
  linux/darwin/windows; CI
- config loading, secret references, profile inheritance, validation
- output formatting (`table`, `json`, `yaml`)

### M1 — Install through SSH

- `ssh` provider with `manual` entry
- agent: `inspect`, `prepare-disks`, `install-image`, `configure`,
  `bootloader`, `finalize`
- Debian family driver, systemd-networkd renderer
- disk layouts: GPT, md RAID, LVM, ext4/xfs/swap
- QEMU integration tests: 1, 2 and 4 disks, UEFI and BIOS
- `ump plan`, `ump install`, run logs (first `ump install`: done for
  Ubuntu 26.04 with the lvm-linear layout, through the UMP rescue
  system, with the installer and the base image fetched from the storage;
  next: more layouts and a benchmarking round)
- no prompts without a TTY; `ump approve` and `--approved`

### M2 — Hetzner Robot

- Robot API: inventory, rename, rDNS, rescue, reset and SSH key registration (done)
- `rescue` entry method (the provider side is done: `ump rescue server`)
- `ump get servers`, `ump describe server` and `ump rename server` (done), `ump machine reboot|rescue`
- listing cache with "added/removed since last time" (done)
- IPv6 from provider network info

### M3 — Handoff and fleet use

- `ansible`, `ansible-pull`, `script` handoff
- parallel installs, `--resume`
- non-interactive guards for CI
- `ump inventory export`, `ump machine import --from terraform`, the
  machine overlay
- skill v0 (operate): `SKILL.md`, ownership, CLI cheat sheet, runbooks for
  add / reinstall / decommission / drift; `ump skill install`, `ump doctor`

### M4 — Cloud and kexec

- staging image (kernel + initramfs) build (done: `images/rescue/`, with
  `kexec-into-rescue.sh` for the manual path)
- `kexec` entry method (done for the rescue: `ump rescue server` kexecs
  from a running system that lets UMP in as root, or from the provider's
  rescue system)
- `hetzner-cloud` provider incl. `create`/`destroy`
- Ubuntu family support

### M5 — Own hardware

- `redfish` provider, staging ISO, `virtual-media` entry
- `ipmi` + iPXE

### M6 — Skill v1: organize

- `ump repo audit`, `ump inventory check`, the adoption guide and migration
  phases
- `ump repo init`: reference repository skeleton and templates (PR, runbook,
  ADR, health report)
- health and drift reports as runbooks
- validated by taking over a real, grown Terraform/Ansible repository

### Later

- more providers (OVH, Scaleway, AWS, GCP, libvirt/Proxmox)
- RHEL-like family driver
- LUKS with remote unlock, btrfs
- image build recipes (`ump image build`)
- bonds/VLANs/bridges in the network model

## Open questions

- **Images: build our own or use distribution cloud images?** Own images
  give full control and identical content everywhere; stock cloud images
  are less work and trusted. Possibly both: stock images as input,
  customised during `configure`.
- **Staging environment: always our own?** Decided: yes. Provider rescue
  systems differ (Debian vs. other bases, tool versions), so they are
  used only to get root once; UMP then kexecs into its own image
  (`images/rescue/`), which makes steps 4–10 identical everywhere at the
  cost of one more reboot-like step.
- **Inventory for handoff.** Proposed answer in [the skill](skill.md#who-owns-what):
  UMP exports, Ansible consumes it as a dynamic inventory, nothing is
  copied by hand. Still open: whether an explicit "expected machines" list
  is needed to notice machines that should exist but do not.
- **Machine-level overrides.** Proposed answer: the machine overlay
  (`ump/machines.yaml`, see [the skill](skill.md#the-machine-overlay)) with
  defaults, match rules and per-machine entries, in git.
- **ZFS.** Popular on bare metal, but licensing and distribution support
  differ. Possibly as an optional OS family feature.
- **Windows / BSD targets.** Out of scope for now; the agent and OS family
  split should not make them impossible.
- **License.** To be decided before the first public release.
- **Skill questions** — one skill or two, approval without a terminal, an
  MCP server next to the CLI: see [the skill](skill.md#open-questions).
