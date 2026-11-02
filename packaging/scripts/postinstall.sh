#!/bin/sh
# After ump is installed or upgraded (deb postinst, rpm %post): systemd
# reads the units again, and an upgrade restarts the PXE servers that
# run, each after the transfers under way. Nothing is enabled or started.
set -e
upgrade=""
case "$1" in
configure) upgrade="${2:-}" ;;                    # deb: the version upgraded from
abort-*) ;;                                       # deb: an upgrade or removal undone
[0-9]*) if [ "$1" -ge 2 ]; then upgrade=1; fi ;;  # rpm: the packages of ump after this
*) exit 0 ;;
esac
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	if [ -n "$upgrade" ]; then
		systemctl try-restart 'ump-pxe@*.service' || true
	fi
fi
exit 0
