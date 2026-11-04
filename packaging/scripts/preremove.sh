#!/bin/sh
# Before ump is removed (deb prerm remove, rpm %preun 0), not before an
# upgrade: the PXE servers and checks stop, and no site stays enabled.
set -e
case "$1" in
remove|0) ;;
*) exit 0 ;;   # deb: upgrade, deconfigure, failed-upgrade; rpm: an upgrade
esac
command -v systemctl >/dev/null 2>&1 || exit 0
if [ -d /run/systemd/system ]; then
	systemctl stop 'ump-pxe@*.service' 'ump-pxe-check@*.service' || true
fi
for link in /etc/systemd/system/*.wants/ump-pxe@*.service; do
	[ -L "$link" ] || continue
	systemctl --no-reload disable "${link##*/}" >/dev/null 2>&1 || rm -f "$link"
done
exit 0
