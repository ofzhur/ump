#!/bin/sh
# Runs inside the freshly bootstrapped root (chroot) at the end of build.sh:
# configure the rescue system and remove what a RAM-only system does not need.
set -eu
export DEBIAN_FRONTEND=noninteractive

echo "ump-rescue" > /etc/hostname
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n127.0.1.1\tump-rescue\n' > /etc/hosts
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
echo "${UMP_VERSION:-dev}" > /etc/ump-rescue-version

# root logs in with the keys from the kernel command line (ump-rescue-setup)
# or on the console, which auto-logs in; there is no password.
passwd -d root >/dev/null
rm -f /etc/ssh/ssh_host_*

# services: our setup first, then networkd, resolved, time and ssh
systemctl enable ump-rescue-setup.service systemd-networkd.service systemd-resolved.service systemd-timesyncd.service ssh.service >/dev/null
systemctl disable smartmontools.service smartd.service mdmonitor.service >/dev/null 2>&1 || true
ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf

# C.UTF-8 is built into glibc; no locale package is needed
printf 'LANG=C.UTF-8\n' > /etc/default/locale

# Dependencies that the rescue system has no use for: xfsprogs pulls in
# Python for xfs_scrub_all, the kernel package pulls in initramfs-tools
# (this system is the initramfs). They are removed behind dpkg's back,
# which is fine for a system that never installs anything again.
for p in $(dpkg-query -W -f '${Package}\n' 'python3-*' 2>/dev/null) python3 python3-minimal libpython3-stdlib \
	$(dpkg-query -W -f '${Package}\n' 'python3.*' 'libpython3.*' 2>/dev/null); do
	dpkg --purge --force-all "$p" >/dev/null 2>&1 || true
done
for p in initramfs-tools initramfs-tools-core klibc-utils libklibc; do
	dpkg --purge --force-all "$p" >/dev/null 2>&1 || true
done
rm -rf /usr/lib/python3* /usr/bin/python3* /etc/python3* /usr/share/python3 /etc/initramfs-tools /usr/share/initramfs-tools
# xfs_scrub is the one user of ICU (36 MB); mkfs.xfs and xfs_repair are not
rm -f /usr/sbin/xfs_scrub* /usr/lib/*/libicu*
# udev's hardware database only decorates devices with vendor names (18 MB);
# ipmitool's vendor list is 5 MB of names
rm -rf /usr/lib/udev/hwdb.d /usr/lib/udev/hwdb.bin /etc/udev/hwdb.bin /usr/share/misc/enterprise-numbers.txt

# device tree blobs: kexec and the hypervisors pass the running one
rm -rf /usr/lib/linux-image-*

# kernel modules a server rescue system never loads
kver="$(ls /lib/modules | head -n 1)"
for d in kernel/sound kernel/drivers/media kernel/drivers/net/wireless kernel/net/wireless kernel/net/mac80211 \
	kernel/drivers/bluetooth kernel/net/bluetooth kernel/drivers/staging kernel/drivers/iio kernel/drivers/isdn \
	kernel/drivers/input/touchscreen kernel/drivers/input/joystick kernel/drivers/input/tablet kernel/drivers/usb/gadget \
	kernel/drivers/android kernel/drivers/gpu/drm/amd kernel/drivers/gpu/drm/radeon kernel/drivers/gpu/drm/nouveau \
	kernel/drivers/gpu/drm/i915 kernel/drivers/gpu/drm/xe kernel/drivers/gpu/drm/msm kernel/drivers/gpu/drm/panfrost \
	kernel/drivers/gpu/drm/etnaviv kernel/drivers/gpu/drm/lima kernel/drivers/gpu/drm/v3d kernel/drivers/gpu/drm/vc4 \
	kernel/drivers/gpu/drm/tegra kernel/drivers/gpu/drm/nvidia kernel/drivers/comedi kernel/drivers/greybus \
	kernel/drivers/most kernel/drivers/leds kernel/drivers/hwmon/pmbus kernel/drivers/w1 \
	kernel/drivers/pcmcia kernel/drivers/parport kernel/drivers/infiniband kernel/drivers/net/wwan \
	kernel/drivers/net/can kernel/drivers/net/usb kernel/drivers/net/ieee802154 kernel/drivers/net/hamradio \
	kernel/drivers/net/wan kernel/drivers/net/ppp kernel/drivers/net/slip kernel/drivers/net/arcnet \
	kernel/drivers/net/appletalk kernel/drivers/net/fddi kernel/drivers/net/hippi kernel/drivers/net/plip \
	kernel/net/bridge/netfilter \
	kernel/net/sched kernel/net/openvswitch kernel/net/batman-adv kernel/net/can kernel/net/dccp kernel/net/sctp \
	kernel/net/tipc kernel/net/rds kernel/net/l2tp kernel/net/nfc kernel/net/ieee802154 kernel/net/6lowpan \
	kernel/net/ax25 kernel/net/netrom kernel/net/rose kernel/net/x25 kernel/net/appletalk kernel/net/atm \
	kernel/net/phonet kernel/net/caif kernel/net/mpls kernel/net/hsr kernel/net/key \
	kernel/net/smc kernel/net/rxrpc kernel/net/qrtr kernel/net/vmw_vsock kernel/net/mptcp \
	kernel/fs/ocfs2 kernel/fs/gfs2 kernel/fs/dlm kernel/fs/orangefs kernel/fs/ceph kernel/fs/afs kernel/fs/9p \
	kernel/fs/coda kernel/fs/ubifs kernel/fs/jffs2 kernel/fs/cramfs kernel/fs/romfs kernel/fs/minix \
	kernel/fs/sysv kernel/fs/ufs kernel/fs/hfs kernel/fs/hfsplus kernel/fs/befs kernel/fs/bfs kernel/fs/efs \
	kernel/fs/freevxfs kernel/fs/qnx4 kernel/fs/qnx6 kernel/fs/adfs kernel/fs/affs kernel/fs/omfs \
	kernel/fs/smb kernel/fs/ksmbd kernel/fs/erofs kernel/fs/zonefs kernel/fs/exfat kernel/fs/jfs \
	kernel/drivers/misc/lis3lv02d kernel/drivers/misc/cardreader kernel/drivers/platform/x86/intel \
	kernel/drivers/spi kernel/drivers/i3c kernel/drivers/rapidio \
	kernel/drivers/siox kernel/drivers/slimbus kernel/drivers/soundwire kernel/drivers/thunderbolt \
	kernel/drivers/usb/serial kernel/drivers/usb/misc kernel/drivers/usb/image kernel/drivers/usb/class \
	kernel/drivers/video/backlight kernel/drivers/vdpa kernel/drivers/vfio kernel/drivers/visorbus \
	kernel/drivers/zorro kernel/drivers/target kernel/drivers/memstick kernel/drivers/mmc kernel/drivers/mfd \
	kernel/drivers/regulator kernel/drivers/accessibility kernel/drivers/auxdisplay kernel/drivers/firewire \
	kernel/drivers/hte kernel/drivers/mcb kernel/drivers/nfc kernel/drivers/ntb \
	kernel/drivers/peci kernel/drivers/power kernel/drivers/rpmsg kernel/drivers/ssb kernel/drivers/bcma \
	kernel/drivers/uwb kernel/drivers/ipack kernel/drivers/fpga kernel/drivers/extcon \
	kernel/drivers/counter kernel/drivers/cxl kernel/drivers/dax kernel/drivers/nvdimm \
	kernel/drivers/crypto/ccp kernel/drivers/crypto/qat kernel/drivers/crypto/intel kernel/drivers/crypto/hisilicon \
	kernel/sound kernel/drivers/tty/serial/8250/8250_pci.ko; do
	rm -rf "/lib/modules/$kver/$d"
done
# Some of the remaining modules need one of those (igb needs dca, ixgbe
# xfrm_algo, ice gnss, sfc mtd), and depmod quietly drops the missing
# dependency: modprobe then fails on unknown symbols, and a bare-metal
# server boots without its network. Those directories stay above; any
# other module left without what it needs goes too, and the build stops
# when that would be one of the network and storage drivers below
# (test-boot.sh loads the same list).
keep=" igb igc e1000e ixgbe i40e ice bnxt_en tg3 bnx2 bnx2x r8169 mlx4_en mlx5_core qede be2net sfc atlantic ena
	virtio_net nvme ahci megaraid_sas mpt3sas aacraid hpsa smartpqi raid1 raid10 raid456 dm_mod "
moddir="/lib/modules/$kver"
while :; do
	have="$(find "$moddir" -name '*.ko*' | sed 's#.*/##; s/\.ko.*//; s/-/_/g' | sort -u)"
	broken=""
	for f in $(find "$moddir/kernel" -name '*.ko*'); do
		for dep in $(modinfo -F depends "$f" | tr ',' ' '); do
			echo "$have" | grep -qx "$(echo "$dep" | tr - _)" || { broken="$broken $f"; break; }
		done
	done
	[ -n "$broken" ] || break
	for f in $broken; do
		name="$(basename "$f" | sed 's/\.ko.*//; s/-/_/g')"
		case "$keep" in
		*[[:space:]]"$name"[[:space:]]*)
			echo "finish.sh: the module pruning leaves $name without what it needs; keep its dependencies" >&2
			exit 1 ;;
		esac
		echo "removing ${f#"$moddir/"}: it needs a removed module"
		rm -f "$f"
	done
done
depmod -a "$kver"

# firmware that a server rescue system never needs
for d in amdgpu radeon nvidia i915 intel/ipu intel/vsc intel/ibt-* intel/sof* \
	iwlwifi-* ath6k ath9k_htc ath10k ath11k ath12k brcm cypress mediatek mrvl mwl8k mwlwifi \
	rtlwifi rtw88 rtw89 rsi ti-connectivity wfx qca cirrus dvb-* v4l-* as102_data* \
	rtl_bt moxa ueagle* vicam yamaha matrox r128 i2400m* libertas; do
	rm -rf /lib/firmware/$d /usr/lib/firmware/$d
done
rm -rf /lib/firmware/*.zst-wifi 2>/dev/null || true

# what a RAM-only system does not need
rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/info/* /usr/share/lintian/* \
	/var/cache/apt/* /var/lib/apt/lists/* /var/log/*.log /var/log/apt /var/log/journal
find /usr/share/locale -mindepth 1 -maxdepth 1 ! -name locale.alias -exec rm -rf {} +
find /var/log -type f -delete
rm -rf /tmp/* /root/.bash_history

# the shell should tell at a glance where one is
cat > /etc/motd <<'MOTD'

  UMP rescue system: running from RAM, no disk is mounted.
  Tools: sgdisk, parted, mdadm, lvm, cryptsetup, mkfs.*, wipefs, nvme, smartctl,
         unsquashfs, zstd, curl, rsync, kexec, efibootmgr, ipmitool, jq.

MOTD
