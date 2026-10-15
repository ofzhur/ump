# The UMP installer

[`install.sh`](install.sh) puts an operating system on a machine's disks
from a [UMP base image](../images/ubuntu/README.md), or NixOS with
`nixos-install` (see [NixOS](../docs/oses.md#nixos)), the way a specification
says. It runs as root in the [UMP rescue
system](../images/rescue/README.md), where no disk is in use, and
refuses to run anywhere else unless told to with `--force`, because it
erases every disk of the machine it runs on. `ump server reinstall` renders the
specification from a [profile](../docs/profiles.md), fetches the
installer from the storage and runs it there with the specification on
its stdin; it can be run by hand just the same, with a specification
file or with the flags alone (which give the `lvm-linear` layout with
root and the keys of a file).

## What it does

1. **Looks at the machine:** architecture, firmware (UEFI or BIOS), the
   disks (whole devices that are not removable media, RAM or loop
   devices, and not on USB; NVMe first, so `first` in a layout is an
   NVMe disk when there is one), the network of the rescue system (the
   address, gateway and MAC that `ump server rescue` carried over on the kernel
   command line, or what is configured).
2. **Plans the layout** against the disks: every partition with its
   number and size per disk, every md array, every volume group and
   volume. A target that matches no disk, two parts taking the rest of
   one disk, an md or raid level with too few members, a UEFI machine
   without an `efi` part or a BIOS machine without a `bios` part stop it
   here. Prints the plan, with the users, userdata, firewall and
   provision step; `--dry-run` stops here.
3. **Fetches the manifest** of the image, so that a missing image costs
   no disk.
4. **Releases and wipes the disks:** swap off, everything mounted from
   them unmounted, volume groups deactivated, md arrays stopped, then the
   signatures inside the old partitions, the partition tables and the
   signatures of the disks themselves.
5. **Builds the layout:** GPT partitions (the one that takes the rest of
   a disk last), md arrays (`mdadm --create`, metadata 1.2), file systems
   (ext4 without the `orphan_file` and `metadata_csum_seed` features,
   which the GRUB of Ubuntu 22.04 cannot read; xfs, btrfs, vfat, swap),
   physical volumes, volume groups and
   logical volumes (striped, mirrored, raid5, raid6, raid10 or linear;
   striping over a single physical volume becomes linear). Mirrored and
   parity volumes synchronise in the background after the first boot.
   `/boot` stays a plain partition on purpose: the boot loader never has
   to read LVM or md.
6. **Mounts** root, then the other mountpoints by depth, and **unpacks
   the image** straight from the download into them, checking the
   archive's SHA-256 against the manifest. The manifest's `family` decides
   what follows: steps 7 to 11 are for an image (Ubuntu); for `nixos` the
   installer writes the profile, what it found on the machine and the
   firewall ruleset next to UMP's NixOS module ([`nixos/ump.nix`](nixos/ump.nix)),
   with the userdata as NixOS modules, and runs `nixos-install` from the
   manifest's channel instead (see [NixOS](../docs/oses.md#how-the-installation-runs)).
7. **Configures the machine:** hostname, `/etc/fstab` by UUID (swap
   included), a netplan file with every wired interface matched by MAC
   and, unless the profile says `network: {rename: false}`, renamed
   uniformly (`ext01`, ... for the uplink and the interfaces with a
   public address, `int01`, ... for the rest), the uplink with the
   static address and on-link gateway, or DHCP when nothing is known,
   the others up without an address,
   IPv6 only when the profile keeps it, resolvers 1.1.1.1 and 8.8.8.8,
   the kernel command line (`mitigations=off ipv6.disable=1` unless the
   profile keeps them, the profile's extra, and `kho=off`: the kernel's
   kexec handover would reserve up to half the memory of a small machine
   for live updates, and stop ump from re-entering its rescue system),
   time zone, a fresh machine id and SSH host keys.
8. **Users:** every user is created with its shell, groups (created
   when missing) and keys, and `sudo: true` makes it a member of `sudo`
   with a `NOPASSWD` drop-in, since no passwords are set. Root logs in
   over SSH only when the profile lists it (`PermitRootLogin
   prohibit-password` in `/etc/ssh/sshd_config.d/10-ump-root.conf`);
   otherwise the image's `PermitRootLogin no` stands, and the
   administrator (the first sudo user, `ump` in the default profile) is
   the way in, for ump as well.
9. **Userdata:** `nftables` is installed from the Ubuntu archive when
   the firewall needs it and the image lacks it; then every entry of
   the profile's `userdata` is written to `/etc/ump/userdata/NN-NAME.sh`
   in the new system and run there in order, as root in a chroot, with
   the rescue system's resolver and no service started (`policy-rc.d`).
   The output goes to `/var/log/ump-userdata.log` of the new system; a
   failing script stops the installation with its output shown.
10. **Firewall:** with `policy: drop`, `/etc/nftables.conf` is written
    and `nftables.service` enabled: input drop, with the loopback
    interface, established and related connections and the allowed
    entries accepted; output accept; forward accept, except that a new
    connection coming in on an external interface (`ext*`) is held to
    the same allowed entries, matched on the port it was addressed to
    (`ct original proto-dst`, since the port a container publishes is
    rewritten by DNAT before the forward hook). So containers and
    virtual machines reach out freely, and a port Docker publishes with
    `-p` is reachable from outside only when the profile allows it,
    whatever rules the container engine adds (their chains and UMP's
    are evaluated both; a drop in either is final).
11. **Installs the boot loader:** GRUB on the EFI removable path
    (`EFI/BOOT/BOOT*.EFI`), which every firmware tries without a boot
    entry, and without writing one, so the firmware's boot order stays as
    the provider set it, with its network boot (which is how rescue
    systems get in) ahead of the disk; on amd64 also the BIOS loader into
    the BIOS boot partition of every disk that has one, so the disk boots
    either way. With md arrays or raid volumes, `mdadm.conf` is written
    and the initramfs rebuilt first. Then `update-grub`.
12. Writes `/etc/ump/install` (what was installed, when, on which disks,
    which profile and layout, the administrator, the interfaces with
    their MAC addresses, the provision step) and
    `/etc/ump/spec.json` (the specification), unmounts everything and
    reports; `ump server reinstall` then reboots the machine and waits for the
    new system.

## Usage

```
install.sh --spec FILE [--dry-run] [--force]
install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--disks /dev/sda,/dev/sdb]
           [--keys FILE] [--ipv6] [--mitigations] [--timezone ZONE] [--dry-run] [--force]
```

The specification is JSON with the fields of a resolved profile (`os`,
`hostname`, `timezone`, `storage`, `kernel`, `layout` with its parts,
`disks`, `users` with key lines, `userdata` with scripts, `firewall`, `provision`,
`network`);
`ump server reinstall` writes it, and `/etc/ump/spec.json` of an installed system
is one to start from. Without `--spec`, the flags give the `lvm-linear`
layout with root alone (which then logs in over SSH, since it is the
only user): `--storage` is the base URL that serves
`os/<os>/<arch>/manifest.json` and `rootfs.tar.zst` (see [the storage
layout](../images/rescue/README.md#your-own-storage)), `--keys` defaults
to `/root/.ssh/authorized_keys`, which is where `ump server rescue` put UMP's
key and yours. By hand, from the rescue system:

```console
# curl -fsSL https://ump.zhurko.de/dl/install/install.sh -o /tmp/install.sh
# bash /tmp/install.sh --storage https://ump.zhurko.de/dl --hostname web-01 --dry-run
# bash /tmp/install.sh --storage https://ump.zhurko.de/dl --hostname web-01 && systemctl reboot
```

Exit status 0 means the machine is ready to reboot; 2 is a usage or
environment problem (not the rescue system, a missing tool), 1 a failed
step, with the reason on stderr. A failed installation leaves the
machine in the rescue system, with whatever was mounted unmounted again.

## Turning IPv6 or the mitigations back on

The defaults trade the CPU vulnerability mitigations and IPv6 for speed.
On an installed system, edit `/etc/default/grub.d/ump-cmdline.cfg`
(remove `mitigations=off` or `ipv6.disable=1`), run `update-grub`, for
IPv6 add the address and the default route to `/etc/netplan/10-ump.yaml`
(`netplan apply` or `netplan try`), and reboot. `ump server reinstall
--mitigations --ipv6` installs with both on, as does `kernel: {mitigations:
true, ipv6: true}` in the [profile](../docs/profiles.md).
