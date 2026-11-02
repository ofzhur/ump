#!/bin/sh
# After ump is removed (deb postrm, rpm %postun 0): systemd forgets the
# units. A deb's purge also removes what the PXE servers kept, their
# journals and caches; /etc/ump/pxe/ holds the sites' settings and stays.
set -e
case "$1" in
remove|purge|0) ;;
*) exit 0 ;;   # an upgrade
esac
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
if [ "$1" = purge ]; then
	for d in ump-pxe ump-pxe-check; do
		rm -rf "/var/lib/$d" "/var/cache/$d" "/var/lib/private/$d" "/var/cache/private/$d"
	done
fi
exit 0
