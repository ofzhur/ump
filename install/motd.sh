#!/bin/sh
# The message of the day of a UMP-installed system: who this machine is
# and how it does, in brief. The same script on every system UMP installs:
# on Ubuntu /etc/update-motd.d/00-ump (pam_motd shows it at login), on
# NixOS part of UMP's module (shown at interactive SSH logins); ump-motd
# shows it on both. POSIX sh with coreutils, procps and iproute2.
B='\033[1m'; C='\033[1;36m'; G='\033[1;32m'; D='\033[2m'; N='\033[0m'
printf '%b' "$C"
cat <<'ART'

 ██╗   ██╗███╗   ███╗██████╗
 ██║   ██║████╗ ████║██╔══██╗
 ██║   ██║██╔████╔██║██████╔╝
 ██║   ██║██║╚██╔╝██║██╔═══╝
 ╚██████╔╝██║ ╚═╝ ██║██║
  ╚═════╝ ╚═╝     ╚═╝╚═╝
ART
printf '%b' "$N"
printf '  %bUnus Mundus Provisioner%b  %bthe same system on every machine%b\n\n' "$B" "$N" "$D" "$N"

os="$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"')"
row() { printf '  %b%-10s%b %s\n' "$G" "$1" "$N" "$2"; }
row "host" "$(hostname -f 2>/dev/null || hostname 2>/dev/null || uname -n) ($os, $(uname -r), $(uname -m))"
if [ -r /etc/ump/install ]; then
	profile="$(sed -n 's/^profile=//p' /etc/ump/install)"
	when="$(sed -n 's/^installed=//p' /etc/ump/install)"
	layout="$(sed -n 's/^layout=//p' /etc/ump/install)"
	row "installed" "${when%T*} by UMP, profile ${profile:-?}, layout ${layout:-?}"
fi
# uptime from /proc/uptime: not every uptime knows -p (NixOS's is coreutils')
up="$(awk '{s = int($1); d = int(s / 86400); h = int(s % 86400 / 3600); m = int(s % 3600 / 60)
	out = ""; if (d) out = d " day" (d > 1 ? "s" : ""); if (h) out = out (out ? ", " : "") h " hour" (h > 1 ? "s" : "")
	if (!d) out = out (out ? ", " : "") m " minute" (m != 1 ? "s" : ""); print out}' /proc/uptime)"
row "uptime" "$up, load $(cut -d' ' -f1-3 /proc/loadavg)"
row "memory" "$(free -m | awk '/^Mem:/ {printf "%d of %d MB used", $3, $2}'), $(nproc) CPU(s)"
row "disk" "$(df -h / | awk 'NR == 2 {printf "/ %s of %s used (%s) on %s", $3, $2, $5, $1}')"
ip -br -4 addr show 2>/dev/null | awk '$1 != "lo" && $3 != "" {printf "%s %s  ", $1, $3}' | sed 's/  $//' | {
	read -r addrs
	[ -n "$addrs" ] && row "network" "$addrs"
}
printf '\n'
