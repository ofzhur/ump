# The UMP installer

[`install.sh`](install.sh) puts an operating system on a machine's disks
from a [UMP base image](../images/ubuntu/README.md). It runs as root in
the [UMP rescue system](../images/rescue/README.md), where no disk is in
use, and refuses to run anywhere else unless told to with `--force`,
because it erases every disk of the machine it runs on. `ump install`
fetches it from the storage and runs it there; it can be run by hand just
the same.

## What it does

1. **Looks at the machine:** architecture, firmware (UEFI or BIOS), the
   disks (whole devices that are not removable media, RAM or loop
   devices, and not on USB), the network of the rescue system (the
   address, gateway and MAC that `ump rescue` carried over on the kernel
   command line, or what is configured), and root's authorized keys.
   Prints the plan; `--dry-run` stops here.
2. **Fetches the manifest** of the image, so that a missing image costs
   no disk.
3. **Releases and wipes the disks:** swap off, everything mounted from
   them unmounted, volume groups deactivated, md arrays stopped, then the
   signatures inside the old partitions, the partition tables and the
   signatures of the disks themselves.
4. **Partitions:** the first disk (NVMe before the others) gets a 1 MiB
   BIOS boot partition, a 512 MiB EFI system partition (FAT32), a 1 GiB
   `/boot` (ext4) and the rest as an LVM physical volume; every other
   disk becomes one physical volume. `vg0/root` is one linear logical
   volume over all of them, ext4. `/boot` stays a plain partition on
   purpose: the boot loader never has to read LVM.
5. **Unpacks the image** straight from the download into the mounted
   volumes, and checks the archive's SHA-256 against the manifest.
6. **Configures the machine:** hostname, `/etc/fstab` by UUID, a netplan
   file for the interface (matched by MAC) with the static address and
   on-link gateway, or DHCP when nothing is known, IPv6 only with
   `--ipv6`, resolvers 1.1.1.1 and 8.8.8.8, root's `authorized_keys`,
   the kernel command line (`mitigations=off ipv6.disable=1` unless
   `--mitigations`, `--ipv6`, and `kho=off`: the kernel's kexec handover
   would reserve up to half the memory of a small machine for live
   updates, and stop ump from re-entering its rescue system), time zone,
   a fresh machine id and SSH host keys.
7. **Installs the boot loader:** GRUB on the EFI removable path
   (`EFI/BOOT/BOOT*.EFI`), which every firmware tries without a boot
   entry, and without writing one, so the firmware's boot order stays as
   the provider set it, with its network boot (which is how rescue
   systems get in) ahead of the disk; on amd64 also the BIOS loader into
   the BIOS boot partition, so the disk boots either way. Then
   `update-grub`.
8. Writes `/etc/ump/install` (what was installed, when, on which disks),
   unmounts everything and reports; `ump install` then reboots the
   machine and waits for the new system.

## Usage

```
install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--arch amd64|arm64]
           [--layout lvm-linear] [--disks /dev/sda,/dev/sdb] [--keys FILE]
           [--ipv6] [--mitigations] [--timezone ZONE] [--force] [--dry-run]
```

`--storage` is the base URL that serves `os/<os>/<arch>/manifest.json`
and `rootfs.tar.zst` (see [the storage
layout](../images/rescue/README.md#your-own-storage)); `--keys` defaults
to `/root/.ssh/authorized_keys`, which is where `ump rescue` put UMP's
key and yours. By hand, from the rescue system:

```console
# curl -fsSL https://ump.zhurko.de/install/install.sh -o /tmp/install.sh
# bash /tmp/install.sh --storage https://ump.zhurko.de --hostname web-01 --dry-run
# bash /tmp/install.sh --storage https://ump.zhurko.de --hostname web-01 && systemctl reboot
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
(`netplan apply` or `netplan try`), and reboot. `ump install
--mitigations --ipv6` installs with both on.
