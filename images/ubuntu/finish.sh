#!/bin/sh
# Runs inside the freshly bootstrapped root (chroot) at the end of build.sh:
# the defaults of a UMP-installed system that do not depend on the machine.
# What does (hostname, network, keys, boot loader) is done by install.sh
# on the machine itself.
set -eu
export DEBIAN_FRONTEND=noninteractive

echo "${UMP_VERSION:-dev}" > /etc/ump-image-version
echo "ubuntu" > /etc/hostname
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n' > /etc/hosts

# package sources: the archive of the architecture, with updates and security
case "${UMP_ARCH:-amd64}" in
amd64|i386)
	archive="http://archive.ubuntu.com/ubuntu"; security="http://security.ubuntu.com/ubuntu" ;;
*)
	archive="http://ports.ubuntu.com/ubuntu-ports"; security="http://ports.ubuntu.com/ubuntu-ports" ;;
esac
rm -f /etc/apt/sources.list
cat > /etc/apt/sources.list.d/ubuntu.sources <<SRC
Types: deb
URIs: $archive
Suites: ${UMP_SUITE:-resolute} ${UMP_SUITE:-resolute}-updates
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: $security
Suites: ${UMP_SUITE:-resolute}-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
SRC

# time, locale
ln -sf /usr/share/zoneinfo/UTC /etc/localtime
echo UTC > /etc/timezone
printf 'LANG=C.UTF-8\n' > /etc/default/locale

# services: netplan renders to networkd; resolved; time; ssh; tuned;
# the boot and login records of last (wtmpdb); the activity history of
# sar (sysstat, every 10 minutes) and atop (every 10 minutes, /var/log/atop)
systemctl enable systemd-networkd.service systemd-resolved.service systemd-timesyncd.service ssh.service tuned.service >/dev/null
for u in wtmpdb-update-boot.service sysstat.service sysstat-collect.timer sysstat-summary.timer atop.service atop-rotate.timer; do
	[ -f "/usr/lib/systemd/system/$u" ] || [ -f "/lib/systemd/system/$u" ] || continue   # not every release has them all
	systemctl enable "$u" >/dev/null
done
sed -i 's/^ENABLED=.*/ENABLED="true"/' /etc/default/sysstat
ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf

# root: no password, and no login over SSH unless the profile lists it
# (install.sh); the administrator is the profile's sudo user
passwd -l root >/dev/null 2>&1 || true

# the shell: bash with the colored prompt and completion of /etc/skel;
# root gets the same file (another shell is the configuration
# management's business)
cp /etc/skel/.bashrc /root/.bashrc

# the message of the day: UMP's own script alone (no help text, no news)
chmod 755 /etc/update-motd.d/00-ump
for f in /etc/update-motd.d/*; do [ "$f" = /etc/update-motd.d/00-ump ] || rm -f "$f"; done

# the boot loader configuration file that grub-mkconfig reads; the
# settings are in /etc/default/grub.d/
[ -f /etc/default/grub ] || printf 'GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=""\n' > /etc/default/grub

# the initramfs must know LVM, since the root file system lives on it
kver="$(ls /boot/vmlinuz-* | sed 's|.*/vmlinuz-||' | sort -V | tail -n 1)"
if ! lsinitramfs "/boot/initrd.img-$kver" 2>/dev/null | grep -q 'sbin/lvm$'; then
	update-initramfs -u -k "$kver"
fi

# every installation is its own machine: no id, no host keys, no logs
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id /etc/ssh/ssh_host_*
apt-get clean
rm -rf /var/lib/apt/lists/* /var/cache/apt/*.bin /tmp/* /var/tmp/*
find /var/log -type f -delete
