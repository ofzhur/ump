# Roadmap and open questions

## Milestones

### M0 — Skeleton

- Go module, `ump` and `ump-agent` binaries, CI, release builds
  (linux/darwin/windows for the CLI, linux amd64/arm64 for the agent)
- config loading, secret references, profile inheritance, validation
- output formatting (`table`, `json`, `yaml`)

### M1 — Install through SSH

- `ssh` provider with `manual` entry
- agent: `inspect`, `prepare-disks`, `install-image`, `configure`,
  `bootloader`, `finalize`
- Debian family driver, systemd-networkd renderer
- disk layouts: GPT, md RAID, LVM, ext4/xfs/swap
- QEMU integration tests: 1, 2 and 4 disks, UEFI and BIOS
- `ump plan`, `ump install`, run logs

### M2 — Hetzner Robot

- port of the earlier tool's integration
- `rescue` entry method
- `ump machine list|show|reboot|rescue|rename`
- listing cache with "added/removed since last time" (from the earlier tool)
- IPv6 from provider network info

### M3 — Handoff and fleet use

- `ansible`, `ansible-pull`, `script` handoff
- parallel installs, `--resume`
- non-interactive guards for CI

### M4 — Cloud and kexec

- staging image (kernel + initramfs) build
- `kexec` entry method
- `hetzner-cloud` provider incl. `create`/`destroy`
- Ubuntu family support

### M5 — Own hardware

- `redfish` provider, staging ISO, `virtual-media` entry
- `ipmi` + iPXE

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
- **Staging environment: always our own?** Provider rescue systems differ
  (Debian vs. other bases, tool versions). Using a provider rescue only to
  kexec into UMP's own staging image would make step 4–10 fully identical
  everywhere, at the cost of one more reboot-like step.
- **Agent fetch vs. push.** Downloading the agent from a release URL inside
  the staging environment vs. uploading it from the CLI. Upload works
  offline and guarantees version match; download is faster on slow
  operator links.
- **Inventory for handoff.** Should UMP write/update an Ansible inventory
  file, or only pass the host on the command line and leave inventory to
  the user?
- **Machine-level overrides.** Some machines need one-off settings
  (different layout, extra IPs). In the profile with a match rule, in a
  per-machine file, or on the command line only?
- **ZFS.** Popular on bare metal, but licensing and distribution support
  differ. Possibly as an optional OS family feature.
- **Windows / BSD targets.** Out of scope for now; the agent and OS family
  split should not make them impossible.
- **License.** To be decided before the first public release.
