# Disk layouts (draft)

A disk layout describes *how* disks should be used, independent of the
exact hardware. UMP resolves it against the machine's facts into concrete
partitions during `plan`.

## Lessons from the earlier tool

The earlier tool's layout format (`disklayouts.json`) already had the right
building blocks — a dedicated boot area, LVM volume groups over sets of
disks, partitions with RAID level, size and mount point. What UMP
changes:

- **Disks are selected by attributes, not by device-name regex.**
  `sd.*` matches SATA SSDs, HDDs and USB sticks alike, and names are not
  stable between boots. UMP selects by type, size, count and, if needed,
  model or serial.
- **RAID is explicit and has one model.** Either Linux md RAID underneath
  LVM, or LVM RAID — not a mix inside one layout.
- **YAML instead of JSON**, with validation and helpful errors.
- **Resolution happens in the CLI**, so the result is visible in the plan
  before anything is touched.

## Format

```yaml
name: mirrored-lvm
description: Two identical disks, mirrored boot and LVM on md RAID1

disks:
  # A named disk group selects physical disks from the facts.
  system:
    select:
      type: [nvme, ssd]      # nvme | ssd | hdd
      min_size: 200G
    count: 2                 # exactly 2; also "min: 2", "all"
    prefer: smallest         # smallest | largest | fastest

boot:
  on: system                 # boot partitions go on every disk of the group
  esp: 512M                  # EFI system partition, kept in sync per disk
  bios_boot: 1M              # for legacy boot via GRUB
  boot: { size: 1G, fs: ext4, raid: 1 }   # /boot on md RAID1 across the group

volumes:
  - name: pv0
    on: system
    size: rest
    raid: 1                  # md RAID level across the group (0, 1, 5, 6, 10)
    lvm:
      vg: vg0
      lvs:
        - { name: root, size: 50G,  fs: ext4, mount: / }
        - { name: swap, size: 8G,   fs: swap }
        - { name: var,  size: 80%,  fs: ext4, mount: /var }   # % of remaining VG space
```

### Sizes

`10G`, `512M`, `2T` are absolute (binary units). `NN%` is a percentage of
the remaining space in the containing device or VG. `rest` takes
everything left; at most one `rest` per container. Sizes may have
`min:`/`max:` bounds, e.g. `{ size: 10%, min: 20G, max: 200G }`.

### Disk groups

A layout can define several groups, e.g. NVMe for the system and HDDs for
bulk data:

```yaml
disks:
  system: { select: { type: nvme }, count: 2 }
  bulk:   { select: { type: hdd }, count: { min: 2 } }

volumes:
  - name: system-pv
    on: system
    size: rest
    raid: 1
    lvm:
      vg: vg0
      lvs:
        - { name: root, size: 50G, fs: ext4, mount: / }
        - { name: var,  size: rest, fs: ext4, mount: /var }
  - name: data
    on: bulk
    size: rest
    raid: 6
    fs: xfs
    mount: /srv
```

Disks that no group selects are left untouched. That is deliberate and
reported in the plan.

### Fallbacks

A layout can list alternatives tried in order, so one profile works on
heterogeneous hardware:

```yaml
name: auto-mirror
alternatives:
  - mirrored-lvm           # 2+ equal disks: mirror
  - single-disk-lvm        # otherwise: one disk, no RAID
```

## Resolution

During `plan`, UMP:

1. matches each disk group against the facts (failing if `count` cannot be
   satisfied, or if one physical disk would land in two groups)
2. computes partition tables per disk and RAID/LVM membership
3. checks that every size fits; for RAID, the smallest member determines
   usable capacity
4. produces a concrete plan with device paths *and* serial numbers — the
   agent re-checks the serials before writing

## Supported building blocks (initially)

| Feature | Status |
|---|---|
| GPT, ESP + BIOS boot on every boot disk | planned |
| md RAID 0/1/5/6/10 | planned |
| LVM (VG, LV) | planned |
| ext4, xfs, swap, vfat | planned |
| btrfs (with subvolumes) | later |
| LUKS encryption (with remote unlock via dropbear / TPM) | later |
| LVM RAID | later |
| ZFS | open question — depends on distribution licensing |
