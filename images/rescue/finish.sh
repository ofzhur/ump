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
