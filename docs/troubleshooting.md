# Troubleshooting

What to do when a server does not come back: the UMP rescue system does
not answer after the jump, or the installed system does not boot. Most
of it works without a console, because many dedicated servers have
none at hand. The last sections describe a real case and how to turn
what you find into a fix for ump.

## Where it can stop

`ump server reinstall` and `ump server rescue` go through these steps;
the output names each one, so the last line printed says where it
stopped:

| Step | Output | When it fails |
|---|---|---|
| Root on the server | `Checking root access ...`, `Going through the provider's rescue system.` | exit 5 with `--via direct`; the provider's rescue system is used otherwise |
| The provider's rescue system | `Waiting for SSH on ...`, `Provider rescue system reached (...)` | exit 5; the provider's own rescue system did not come up |
| Loading the UMP rescue image | `==> downloading`, `==> verifying checksums`, `==> memory`, `==> loading` | an error from `kexec-into-rescue.sh`, exit 3 when the memory is too small; the server keeps running what it ran |
| The jump | `==> booting into the UMP rescue system` | nothing more is printed: see [the rescue system does not answer](#the-rescue-system-does-not-answer) |
| The UMP rescue system | `Waiting for the UMP rescue system ... up after ...` | exit 5 after the wait (15 minutes by default) |
| The installation | the installer's `==>` lines | exit 1; the server stays in the rescue system, where you can log in and look |
| The first boot | `Waiting for ... the new system` | exit 5: see [the installed system does not boot](#the-installed-system-does-not-boot) |

## Getting a server back

Whatever happened, these bring a server back to a known state:

- **The system on its disk.** `ump server unrescue NAME --reboot`
  disarms the provider's rescue system and resets the server, so it
  boots from disk. The UMP rescue system lives in RAM, so any reset
  leaves it.
- **The provider's rescue system.** `ump server rescue NAME --via
  provider --no-image` resets into the provider's rescue system and
  stops there (Hetzner Robot, Hetzner Cloud, OVH, Linode's Finnix). Log in with the
  printed `ssh` command and look around; nothing is written to the
  disks.
- **A hard reset.** `ump server reboot NAME --hard --yes` cuts the
  power; with the provider's rescue system armed, the server comes back
  in it.
- **The console.** Every provider has one, in its web panel. Hetzner
  Cloud, Vultr, DigitalOcean and Linode (Lish) open it at once; for a
  Hetzner Robot server a remote console (KVM) is requested through
  Robot's support pages and attached for a few hours; OVH has KVM for
  VPS and IPMI or KVM for dedicated servers. A console shows a kernel
  panic or a boot loader at once; everything below is for when there
  is none.

A rescue kernel that crashes reboots by itself after 30 seconds
(`panic=30` on its command line), into the system on the disk, because
the provider's rescue system is armed for one boot only. A crash
therefore ends with the old system answering again, and ump waiting in
vain until its limit.

## The rescue system does not answer

The jump happened (`==> booting into the UMP rescue system`), and
nothing answers on the address: no SSH, often no ping either. In order
of likelihood:

1. **The network did not come up.** The rescue system runs, but its
   NIC driver did not load, or the configuration carried over does not
   fit. Typical for bare-metal servers, whose NICs are real hardware
   (cloud machines all have virtio). See the case study below.
2. **The kernel hangs or crashes.** Too little memory for the image
   (ump checks this before the jump; `--ignore-memory` skips the
   check), firmware that does not survive kexec, a graphics or IOMMU
   quirk. A crash reboots after 30 seconds; a hang stays.
3. **The image did not arrive.** A truncated download is caught by the
   checksums before the jump, so this one shows as an error, not as
   silence.

### Finding out without a console

The provider's rescue system is the lab: it runs on the same hardware
and has a working network. Reset into it, and look at the hardware
first:

```console
$ ump server rescue NAME --via provider --no-image --no-connect --yes
$ ssh ... root@ADDRESS
# [ -d /sys/firmware/efi ] && echo UEFI || echo BIOS
# for n in /sys/class/net/*; do [ -e $n/device ] && echo "${n##*/} $(cat $n/address) $(basename $(readlink $n/device/driver))"; done
# lspci -nn | grep -iE 'ether|nvme|sata|raid'
# cat /proc/cmdline                 # the consoles the rescue image will inherit
```

The NIC driver named there must load in the UMP rescue system. Whether
it can is visible without booting the image: unpack it in the
provider's rescue system and compare what the driver needs with what
the image has.

```console
# cd /tmp && curl -fsSL -o i.zst https://ump.zhurko.de/dl/rescue/amd64/initrd.zst
# mkdir x && cd x && zstd -dc ../i.zst | cpio -idm
# apt-get install -y squashfs-tools && unsquashfs -d r rootfs.squashfs usr/lib/modules
# M=$(ls -d r/usr/lib/modules/*); f=$(find $M -name 'igb.ko*')
# modinfo -F depends $f             # igb: dca,i2c-algo-bit
# find $M -name 'dca.ko*'           # nothing: igb cannot load
```

To watch the jump itself, arm the provider's rescue system first, so
that every reboot lands back in it, and load the image by hand with
extra kernel parameters (`-c`; a later `panic=` overrides the default):

```console
$ ump server rescue NAME --via provider --no-reboot --yes      # armed for the next boot
# curl -fsSL https://ump.zhurko.de/dl/rescue/kexec-into-rescue.sh -o /tmp/k.sh
# bash /tmp/k.sh -u https://ump.zhurko.de/dl/rescue --dry-run          # what would be loaded
# nohup bash /tmp/k.sh -u https://ump.zhurko.de/dl/rescue -c 'panic=10 ...' > /tmp/k.log 2>&1 &
```

Each run answers one question by whether, and when, the server comes
back into the provider's rescue system (`uptime` there says it
rebooted):

| Extra parameters | Comes back | Means |
|---|---|---|
| `panic=10 rdinit=/nonexistent` | within a minute or two | kexec works and the kernel boots on this hardware (it panics on purpose) |
| `panic=10 systemd.unit=reboot.target` | within a minute or two | the initramfs mounts the system and systemd starts (and reboots on purpose) |
| `panic=10` | not at all, and nothing answers | the system runs but is unreachable: the network |
| `panic=10 systemd.mask=ump-rescue-setup.service` | answers ping (no SSH: no keys) | DHCP works where the carried-over static configuration does not |

When even that is not enough, a small service put into the image tells
more. The initramfs is a cpio archive with `/init`, busybox and
`rootfs.squashfs`; files copied to `/newroot` just before `exec
switch_root` in `/init` land in the running system's writable layer. A
service there can check what it needs to (driver loaded, interface
present, carrier, address, gateway answering) and reboot with `echo b
> /proc/sysrq-trigger` after a delay that encodes the answer, 75
seconds per step; the time until the server answers again in the
provider's rescue system is the result. Repack with `find . | cpio -o
-H newc | zstd > /tmp/dbg.zst` and load it with the image's kexec
(`kexec -s -l vmlinuz --initrd=/tmp/dbg.zst --command-line="..."`, the
command line from `--dry-run`). Two notes: a provider's rescue system
starts empty after each boot, so the files go there again every time;
and on macOS `nc -w` does not limit how long a connect takes (a dead
host blocks for 75 seconds), use `nc -z -G 2 -w 2` when timing.

Persistent logs are no way out here: the provider's rescue systems lack
`ramoops`, a Robot reset of an AX server cuts the power (memory is lost), and the
disks are not to be written while debugging.

## The installed system does not boot

The installation finished, the server rebooted, and the new system does
not answer. Go back into a rescue system and look at the disks; they
are as the installer left them:

```console
$ ump server rescue NAME           # the UMP rescue system, with every tool
# lsblk -f                         # partitions, md arrays, LVM
# mdadm --assemble --scan; vgchange -ay
# mount /dev/vg0/root /mnt         # the layout's root volume (lvm-linear: vg0/root)
# cat /mnt/etc/ump/install         # what was installed when, with which profile
```

What to check, most likely first:

- **The network configuration.** Ubuntu and Debian get
  `/etc/netplan/10-ump.yaml`, which matches the uplink by MAC address
  with the static address and gateway the server had; NixOS gets its
  links in `/etc/nixos/ump/`. A provider that changed the MAC, a second
  NIC that took the name, or DHCP-only networks show up here.
- **The firewall.** `/etc/nftables.conf` comes from the profile's
  firewall; a policy `drop` without SSH allowed locks everything out
  while the system runs fine. `nft -c -f /mnt/etc/nftables.conf` checks
  the syntax.
- **The boot loader.** BIOS machines need a `bios` part in the disk
  layout and get GRUB on every disk of the layout; UEFI machines need
  an `efi` part and get GRUB on the fallback path (`EFI/BOOT`). The
  installer refuses a layout that does not fit the firmware it sees,
  but a machine whose firmware switches between BIOS and UEFI boot, or
  boots another disk first, ends up with no boot loader to start.
- **The kernel command line.** The profile's kernel parameters
  (`/mnt/etc/default/grub`) apply on the first boot; a parameter the
  hardware does not take can stop it.
- **The logs of the failed boot.** With a persistent journal,
  `journalctl -D /mnt/var/log/journal --list-boots` and `-b -1` show
  how far it got. The userdata's output is in
  `/mnt/var/log/ump-userdata.log`; NixOS's `nixos-install` output in
  `/mnt/var/log/ump-nixos-install.log`.

Fix it in place (edit, `chroot /mnt` for `update-grub` or `netplan
generate`), then `ump server unrescue NAME --reboot`; or change the
profile and run `ump server reinstall` again.

## Case study: Hetzner AX41-NVMe, 2026-10-16

`ump server rescue` on a Hetzner Robot AX41-NVMe went through the
provider's rescue system, loaded the image, jumped, and then nothing
answered for good: no ping, no SSH, no console.

- The hardware, seen from the Hetzner rescue system: legacy BIOS boot,
  an Intel I210 NIC (driver `igb`), an AMD Ryzen with the IOMMU on, no
  `console=` on its command line.
- The jump experiments above: the kernel booted (`rdinit=/nonexistent`
  came back), systemd started (`reboot.target` came back), the normal
  boot with `panic=10` never came back and never answered, and neither
  did DHCP. So the system ran, without a network.
- The timing service then showed that `modprobe igb` failed.
  `modinfo -F depends igb` said `dca,i2c-algo-bit`, and the image had
  no `dca`: the build pruned `drivers/dca` (and `net/xfrm`,
  `drivers/gnss`, `drivers/mtd`) to save space. `depmod` drops a
  missing dependency without a word, so nothing at build time noticed;
  `ixgbe`, `ice` and `sfc` were broken the same way. Every cloud test
  had passed, because virtio needs none of these.
- The fix: the build keeps those directories, removes every other
  module left without its dependencies, and stops when that would hit
  a server NIC or storage driver; the QEMU boot test loads those
  drivers (a driver loads without its hardware); the jump boots with
  `panic=30`. Afterwards the same server reached the UMP rescue system
  in under two minutes.

## Contributing what you find

A server ump cannot handle is worth a report even without a fix.

**Report** what the steps above found:

- the provider, the product (`ump server show NAME`), BIOS or UEFI,
  the NIC and its driver, the storage controllers (`lspci -nn`);
- the output of `ump server rescue` or `reinstall` up to where it
  stopped, and of `kexec-into-rescue.sh --dry-run` (remove the
  `ump.authorized_keys=` value);
- which of the jump experiments came back, and the image version
  (`/etc/ump-rescue-version`, or the version in the manifest);
- for a system that does not boot: the layout, the profile and what
  the disks showed.

**Fix** where the cause lives:

| Cause | Where | Check |
|---|---|---|
| a driver missing or unloadable in the rescue system | the module pruning and `keep` list in [`images/rescue/finish.sh`](../images/rescue/finish.sh), the driver list in [`images/rescue/test-boot.sh`](../images/rescue/test-boot.sh) | `make rescue-image`, `make rescue-test` |
| firmware missing | [`images/rescue/packages.txt`](../images/rescue/packages.txt) (firmware packages), the firmware pruning in `finish.sh` | `make rescue-test`; `dmesg` on the hardware |
| a tool the installer needs | `packages.txt`, and the tool list in `test-boot.sh` | `make rescue-test` |
| the jump: consoles, network carried over, memory | [`images/rescue/kexec-into-rescue.sh`](../images/rescue/kexec-into-rescue.sh) | `make rescue-test` (it kexecs inside QEMU) |
| network setup in the rescue system | [`images/rescue/overlay/usr/local/sbin/ump-rescue-setup`](../images/rescue/overlay/usr/local/sbin/ump-rescue-setup) | `make rescue-test` |
| the installed system: disks, boot loader, network, firewall | [`install/install.sh`](../install/install.sh), [`install/nixos/ump.nix`](../install/nixos/ump.nix) | `images/test-install.sh OS` |
| the provider: rescue system, reset, reinstall | `internal/provider/<type>` | `make check` |

QEMU only has virtio, so a change for real hardware is done when it ran
on that hardware too: build the image, copy `images/rescue/out/<arch>`
to the provider's rescue system, serve it there with `python3 -m
http.server --bind 127.0.0.1 8000` and jump with `-u
http://127.0.0.1:8000`, before anything is published. The
[rescue image README](../images/rescue/README.md) covers building,
testing and publishing; `make check` runs the Go tests and the scripts'
syntax checks, and a pull request names the hardware it was tried on.
