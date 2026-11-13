#!/usr/bin/env bash
# UMP installer. Runs as root in the UMP rescue system, where no disk is in
# use, and puts an operating system on the machine's disks the way a
# specification says (ump install renders it from a profile): wipes the
# disks, partitions them, builds md arrays and volume groups, then puts the
# system there the way its family wants it, and leaves the machine ready to
# reboot into it. With --keep-data it replaces the old system instead and
# leaves its data: no disk is wiped or partitioned. See README.md.
#
#   image (Ubuntu)  unpacks the image and configures what depends on the
#                   machine (hostname, network, users and keys, userdata,
#                   firewall, boot loader)
#   nixos           writes the profile, what this script found on the
#                   machine and the firewall ruleset next to UMP's NixOS
#                   module (install/nixos/ump.nix on the storage), with the
#                   profile's userdata as NixOS modules, and runs
#                   nixos-install from the image's channel
#
# Usage: install.sh --spec FILE [--record DIR] [--dry-run] [--force]
#        install.sh --spec FILE --keep-data [--root DEVICE] [--keep-root] [--format MOUNTPOINT]...
#                   (--dry-run | --plan FILE | --confirm HASH) [--record DIR]
#        install.sh --storage URL --hostname NAME [--os ubuntu-26.04] [--disks /dev/sda,/dev/sdb]
#                   [--keys FILE] [--ipv6] [--mitigations] [--timezone ZONE] [--record DIR] [--dry-run] [--force]
#
#   --spec FILE      the specification (JSON): os, hostname, timezone, storage,
#                    kernel, layout, disks, users, userdata, firewall, network
#                    (rename: false keeps the kernel's interface names;
#                    private_networks: the server's addresses in its private
#                    networks, private the first, recorded in /etc/ump/install,
#                    configured nowhere yet; uplink: the uplink's interface by
#                    MAC, its IPv4 and IPv6 and its resolvers, where the
#                    provider knows them, else what the rescue system has)
#   --record DIR     copy /etc/ump/install and the SSH host keys (public) of
#                    the new system into DIR at the end (with --keep-data
#                    also the plan, keep-data.json)
#   --keep-data      replace the old system and keep its data: the old root is
#                    found (the file system whose /etc/fstab mounts it at /),
#                    it and a separate /boot are formatted, every other volume
#                    of the old fstab is kept and mounted where it was (a
#                    separate /var at /var.old), the EFI system partition gets
#                    the new boot loader, the rest is not touched; the layout
#                    is not applied (keep_data in the specification says the
#                    same: {root, keep_root, format, confirm})
#   --root DEVICE    the old root, where several old systems are found or none
#                    (a device, UUID=..., DEVICE:SUBVOLUME on btrfs)
#   --keep-root      do not format the root: the old system moves into
#                    /old-root-DATE on its file system (whence it cannot be
#                    started again), the new one is unpacked beside it; a
#                    separate /boot is formatted all the same
#   --format MOUNTPOINT  format this volume of the old fstab too and mount it
#                    there again, with the file system it had (ext4 for ext2
#                    and ext3; repeatable; /var: the new /var)
#   --plan FILE      keep-data: print the plan, write it to FILE as JSON and
#                    stop; nothing is written to a disk (--dry-run: the same
#                    without the file)
#   --confirm HASH   keep-data: write, if the plan is still the one with this
#                    hash (what --plan and --dry-run print); without it
#                    keep-data writes nothing
#   without --spec   the lvm-linear layout with root alone; the flags are:
#   --storage URL    base URL of the storage, which serves os/<os>/<arch>/
#                    manifest.json and rootfs.tar.zst (or UMP_STORAGE)
#   --hostname NAME  hostname of the new system
#   --os NAME        the image to install (default ubuntu-26.04)
#   --disks LIST     the disks to use, comma-separated (default: every disk; USB
#                    devices are left out)
#   --keys FILE      authorized_keys for root (default: /root/.ssh/authorized_keys)
#   --ipv6           keep IPv6 on; by default the kernel gets ipv6.disable=1
#   --mitigations    keep the CPU vulnerability mitigations; by default mitigations=off
#   --timezone ZONE  default UTC
#   --force          run outside the UMP rescue system too (it erases every disk
#                    of the machine it runs on)
#   --dry-run        print the plan and stop before touching a disk
#
# Exit status: 0 done; 1 a failed step; 2 refused (usage, the environment).
# With --keep-data 2 is every end before the first write: nothing was
# changed; 1 every failure after it.
set -Eeuo pipefail
# the new system's files as a root shell over SSH makes them, whatever
# umask the caller has (a PXE machine's runner has 077: /etc/ump would
# be root's alone)
umask 022

spec_file=""
storage="${UMP_STORAGE:-}"
hostname=""
os="ubuntu-26.04"
disks_arg=""
keys="/root/.ssh/authorized_keys"
ipv6=0
mitigations=0
timezone="UTC"
force=0
dry=0
record=""
keep=0
keep_root_arg=""
keep_root=0
keep_format=()
plan_file=""
confirm=""
while [ $# -gt 0 ]; do
	case "$1" in
	--spec) spec_file="$2"; shift 2 ;;
	--record) record="$2"; shift 2 ;;
	--storage) storage="$2"; shift 2 ;;
	--hostname) hostname="$2"; shift 2 ;;
	--os) os="$2"; shift 2 ;;
	--disks) disks_arg="$2"; shift 2 ;;
	--keys) keys="$2"; shift 2 ;;
	--ipv6) ipv6=1; shift ;;
	--mitigations) mitigations=1; shift ;;
	--timezone) timezone="$2"; shift 2 ;;
	--force) force=1; shift ;;
	--dry-run) dry=1; shift ;;
	--keep-data) keep=1; shift ;;
	--root) keep_root_arg="$2"; shift 2 ;;
	--keep-root) keep_root=1; shift ;;
	--format) keep_format+=("$2"); shift 2 ;;
	--plan) plan_file="$2"; shift 2 ;;
	--confirm) confirm="$2"; shift 2 ;;
	-h|--help) sed -n '2,/^set -Eeuo/{/^set -Eeuo/!p;}' "$0"; exit 0 ;;
	*) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
	esac
done
[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 2; }
case "$(uname -m)" in
x86_64) arch=amd64 ;;
aarch64) arch=arm64 ;;
*) echo "install.sh: unsupported machine $(uname -m)" >&2; exit 2 ;;
esac
if [ ! -f /etc/ump-rescue-version ] && [ "$force" = 0 ]; then
	echo "install.sh: this is not the UMP rescue system. The installer erases every disk of the machine it runs on; run it from the rescue system (ump install), or pass --force if that is what you want." >&2
	exit 2
fi
# Every command this script relies on, checked up front: the rescue system
# is minimal, and a missing tool must fail here, not halfway through the
# disks. mdadm is needed for md layouts and to stop old arrays.
for t in jq lsblk sgdisk wipefs partprobe udevadm blockdev pvcreate vgcreate lvcreate pvscan vgchange vgs swapoff mount umount mountpoint \
	mkfs.ext4 mkfs.vfat mkswap blkid zstd tar curl chroot dd sha256sum mktemp nproc awk sed grep sort head tail cut tr seq date \
	ln chmod chown mkdir rm cat basename ip sync sleep touch wc uname id tee; do
	command -v "$t" >/dev/null || { echo "install.sh: $t is missing in this rescue system; the UMP rescue image has it (images/rescue/packages.txt)" >&2; exit 2; }
done
command -v mdadm >/dev/null || echo "install.sh: warning: mdadm is missing; old md arrays cannot be stopped and md parts cannot be made" >&2

log() { printf '==> %s\n' "$*"; }
# (keep-data: before the first write a failure is a refusal, status 2:
# nothing was changed)
die() {
	dying=1; echo "install.sh: $*" >&2
	if [ "${keep:-0}" = 1 ] && [ "${kd_wrote:-0}" = 0 ]; then echo "install.sh: keep-data: nothing was written to the disks" >&2; exit 2; fi
	exit 1
}
# a command that fails under set -e names its line, so that a failure is
# never silent (die says why on its own); set -E carries this into
# functions, and only the main shell speaks, not a command substitution
# whose status the caller looks at
trap 'rc=$?; [ "$rc" = 0 ] || [ -n "${dying:-}" ] || [ "$BASHPID" != "$$" ] || echo "install.sh: failed at line $LINENO (exit status $rc)" >&2' ERR
warn() { echo "install.sh: warning: $*" >&2; }
TARGET=/mnt/target

# --- the specification -------------------------------------------------

if [ -n "$spec_file" ]; then
	spec="$(cat "$spec_file")" || die "cannot read $spec_file"
	jq -e . >/dev/null <<< "$spec" || die "$spec_file is not valid JSON"
else
	[ -n "$storage" ] || { echo "install.sh: --storage URL is required" >&2; exit 2; }
	[ -n "$hostname" ] || { echo "install.sh: --hostname NAME is required" >&2; exit 2; }
	[ -f "$keys" ] || die "$keys does not exist; the new system would have no way in"
	spec="$(jq -n --arg os "$os" --arg hostname "$hostname" --arg timezone "$timezone" --arg storage "${storage%/}" \
		--argjson mitigations "$([ "$mitigations" = 1 ] && echo true || echo false)" --argjson ipv6 "$([ "$ipv6" = 1 ] && echo true || echo false)" \
		--arg disks "$disks_arg" --rawfile keys "$keys" '{
		os: $os, hostname: $hostname, timezone: $timezone, storage: $storage,
		kernel: {mitigations: $mitigations, ipv6: $ipv6, extra: ""},
		layout: {name: "lvm-linear", parts: [
			{type: "slice", target: ["first"], size: "1M", filesystem: "bios"},
			{type: "slice", target: ["first"], size: "512M", filesystem: "efi", mountpoint: "/boot/efi"},
			{type: "slice", target: ["first"], size: "1G", filesystem: "ext4", mountpoint: "/boot"},
			{type: "slice", target: [".*"], size: "rest", filesystem: "lvm", vg: "vg0"},
			{type: "lvm", vg: "vg0", name: "root", size: "rest", filesystem: "ext4", mountpoint: "/"}]},
		disks: ($disks | if . == "" then [] else split(",") end),
		users: [{name: "root", shell: "/bin/bash", groups: [], sudo: false, keys: ($keys | split("\n") | map(select(test("^[a-z]"))))}],
		userdata: [], firewall: {policy: "accept", allow: []},
		profile: {name: "command line"}}')"
fi
s() { jq -r "$1 // empty" <<< "$spec"; }           # a string, empty when absent
sb() { jq -r "$1 // false" <<< "$spec"; }           # a boolean
os="$(s .os)"; hostname="$(s .hostname)"; timezone="$(s .timezone)"; storage="$(s .storage)"
[ -n "$os" ] && [ -n "$hostname" ] && [ -n "$storage" ] || die "the specification needs os, hostname and storage"
[ -n "$timezone" ] || timezone=UTC
storage="${storage%/}"
mitigations="$(sb .kernel.mitigations)"; ipv6="$(sb .kernel.ipv6)"; kernel_extra="$(s .kernel.extra)"
layout_name="$(s .layout.name)"; [ -n "$layout_name" ] || layout_name="inline"
# keep-data: the flags, or keep_data of the specification, which the flags
# add to; the layout is not applied then, and need not be there
if jq -e '.keep_data != null and .keep_data != false' >/dev/null <<< "$spec"; then
	keep=1
	jq -e '.keep_data | type == "object"' >/dev/null <<< "$spec" || { echo "install.sh: keep_data of the specification is an object (root, keep_root, format, confirm)" >&2; exit 2; }
	[ -n "$keep_root_arg" ] || keep_root_arg="$(s .keep_data.root)"
	[ "$(sb .keep_data.keep_root)" != true ] || keep_root=1
	while IFS= read -r x; do [ -z "$x" ] || keep_format+=("$x"); done < <(jq -r '.keep_data.format // [] | .[]' <<< "$spec")
	[ -n "$confirm" ] || confirm="$(s .keep_data.confirm)"
fi
if [ "$keep" = 1 ]; then
	layout_name="keep-data"
	[ -z "$(jq -r '.disks // [] | .[]' <<< "$spec")" ] || { echo "install.sh: keep-data takes no disks: it finds the old system and wipes none" >&2; exit 2; }
	# (a plan file from before is no plan of this run)
	[ -z "$plan_file" ] || { dry=1; rm -f "$plan_file"; }
	[ "$dry" = 1 ] || [[ "$confirm" =~ ^[0-9a-f]{64}$ ]] || { echo "install.sh: keep-data writes only what was confirmed: --dry-run or --plan FILE shows the plan and its hash, --confirm HASH installs it" >&2; exit 2; }
	for t in mdadm btrfs cryptsetup lvs dumpe2fs stat du df timeout base64 readlink od; do
		command -v "$t" >/dev/null || { echo "install.sh: $t is missing in this rescue system; keep-data needs it (images/rescue/packages.txt)" >&2; exit 2; }
	done
elif [ -n "$keep_root_arg$plan_file$confirm" ] || [ "$keep_root" = 1 ] || [ ${#keep_format[@]} -gt 0 ]; then
	echo "install.sh: --root, --keep-root, --format, --plan and --confirm belong to --keep-data" >&2; exit 2
fi
[ "$keep" = 1 ] || jq -e '.layout.parts | length > 0' >/dev/null <<< "$spec" || die "the layout has no parts"
jq -e '.users | length > 0' >/dev/null <<< "$spec" || die "the specification names no users"
profile_name="$(s .profile.name)"
# the private networks, in order; a specification from before several
# networks has the one, private
privates="$(jq -c '.network.private_networks // [.network.private | select(. != null)]' <<< "$spec")"
private_list="$(jq -r 'map("\(.network):\(.address)") | join(",")' <<< "$privates")"
private_network="$(jq -r '.[0].network // empty' <<< "$privates")"; private_address="$(jq -r '.[0].address // empty' <<< "$privates")"
image_url="$storage/os/$os/$arch"
efi=0; [ -d /sys/firmware/efi ] && efi=1

# kho=off: the kernel's kexec handover (on by default in Ubuntu's kernel)
# reserves a scratch area of up to half the memory of a small machine for
# live updates of hypervisors, and that reservation starved the kexec
# into the rescue system on a 1 GB machine
kernel_cmdline="kho=off"
[ "$mitigations" = true ] || kernel_cmdline="$kernel_cmdline mitigations=off"
[ "$ipv6" = true ] || kernel_cmdline="$kernel_cmdline ipv6.disable=1"
[ -n "$kernel_extra" ] && kernel_cmdline="$kernel_cmdline $kernel_extra"

# --- the network: the uplink, and the files that configure it ----------

# Functions only, which images/test-network.sh runs on machines of its
# own. The uplink is the interface with the MAC mac and the name uplink;
# net4 is static, dhcp or none, ip4 ADDRESS/PREFIX[,GATEWAY] when static;
# net6 static, auto (router advertisements) or none, ip6 likewise; dns
# the resolvers, empty when they come from DHCP, and dns_source says
# whence: public (1.1.1.1 and 8.8.8.8, ump's default), spec or dhcp;
# client_id is mac when DHCP identifies the machine by its MAC, and
# lease_dns no when the uplink takes its address from a DHCP lease but
# not the lease's resolvers (the specification lists its own). The
# interfaces are IF_MAC, IF_NAME and IF_KIND; uplink_spec is the
# specification's uplink, empty without one.

# net_from_spec JSON: the uplink of the specification (network.uplink,
# which the provider gives where it knows the machine's network, as for
# a PXE machine); without a mac there, mac stays as it is
net_from_spec() {
	local m gw
	m="$(jq -r '.mac // empty' <<< "$1" | tr 'A-F-' 'a-f:')"
	if [ -n "$m" ]; then
		[[ "$m" =~ ^([0-9a-f]{2}:){5}[0-9a-f]{2}$ ]] || die "network.uplink.mac: $m is not a MAC address"
		mac="$m"
	fi
	net4="$(jq -r '.ipv4.mode // "" | if . == "" then "none" else . end' <<< "$1")"
	net6="$(jq -r '.ipv6.mode // "" | if . == "" then "none" else . end' <<< "$1")"
	ip4=""; ip6=""
	case "$net4" in
	static)
		ip4="$(jq -r '.ipv4.address // empty' <<< "$1")"; gw="$(jq -r '.ipv4.gateway // empty' <<< "$1")"
		case "$ip4" in */*) ;; *) die "network.uplink.ipv4: a static address is ADDRESS/PREFIX, not \"$ip4\"" ;; esac
		ip4="$ip4${gw:+,$gw}" ;;
	dhcp|none) ;;
	*) die "network.uplink.ipv4: mode $net4 is not static, dhcp or none" ;;
	esac
	case "$net6" in
	static)
		ip6="$(jq -r '.ipv6.address // empty' <<< "$1")"; gw="$(jq -r '.ipv6.gateway // empty' <<< "$1")"
		case "$ip6" in */*) ;; *) die "network.uplink.ipv6: a static address is ADDRESS/PREFIX, not \"$ip6\"" ;; esac
		ip6="$ip6${gw:+,$gw}" ;;
	auto|none) ;;
	*) die "network.uplink.ipv6: mode $net6 is not static, auto or none" ;;
	esac
	if [ "$net6" != none ] && [ "$ipv6" != true ]; then
		warn "network.uplink.ipv6 is $net6, and the kernel's IPv6 is off (kernel.ipv6): no IPv6"
		net6=none; ip6=""
	fi
	[ "$net4$net6" != nonenone ] || die "network.uplink has neither IPv4 nor IPv6; the new system would be unreachable"
	client_id=""
	[ "$net4" != dhcp ] || client_id=mac
	dns="$(jq -r '.dns // [] | join(" ")' <<< "$1")"; dns_source=spec
	if [ "$(jq -r '.dns_from_dhcp // false' <<< "$1")" = true ]; then
		[ "$net4" = dhcp ] || [ "$net6" = auto ] || die "network.uplink.dns_from_dhcp: no lease to take resolvers from (ipv4 $net4, ipv6 $net6)"
		dns=""; dns_source=dhcp
	elif [ -z "$dns" ]; then
		dns="1.1.1.1 8.8.8.8"; dns_source=public
	fi
	# resolvers of its own: those alone, not the lease's besides
	lease_dns=yes
	[ "$net4" != dhcp ] || [ "$dns_source" != spec ] || lease_dns=no
}

# net_from_machine: the uplink as the rescue system found it (ip4, ip6),
# as ump has always configured it: static with the addresses found, DHCP
# without any, and the public resolvers
net_from_machine() {
	net4=none; net6=none
	[ -n "$ip4$ip6" ] || net4=dhcp
	[ -z "$ip4" ] || net4=static
	[ -z "$ip6" ] || net6=static
	client_id=""; dns="1.1.1.1 8.8.8.8"; dns_source=public; lease_dns=yes
}

# netplan_yaml: /etc/netplan/10-ump.yaml (Ubuntu, Debian): every wired
# interface by MAC, the uplink configured, the others up without
# addresses
netplan_yaml() {
	local i a4="" g4="" a6="" g6="" keys=0
	echo "# written by UMP at installation; netplan try / netplan apply after editing"
	echo "network:"
	echo "  version: 2"
	echo "  renderer: networkd"
	echo "  ethernets:"
	for i in "${!IF_MAC[@]}"; do
		[ "${IF_NAME[$i]}" = "$uplink" ] && continue
		echo "    ${IF_NAME[$i]}:"
		echo "      match:"
		echo "        macaddress: \"${IF_MAC[$i]}\""
		[ "$rename" = yes ] && echo "      set-name: ${IF_NAME[$i]}"
		echo "      dhcp4: false"
		echo "      optional: true"
		[ "$ipv6" = true ] || echo "      link-local: []"
	done
	echo "    $uplink:"
	echo "      match:"
	echo "        macaddress: \"$mac\""
	[ "$rename" = yes ] && echo "      set-name: $uplink"
	if [ "$net4" = dhcp ]; then
		echo "      dhcp4: true"
		[ -z "$client_id" ] || echo "      dhcp-identifier: $client_id"
		[ "$lease_dns" = yes ] || printf '      dhcp4-overrides:\n        use-dns: false\n'
	else
		echo "      dhcp4: false"
	fi
	if [ "$net4" = static ]; then a4="${ip4%%,*}"; case "$ip4" in *,*) g4="${ip4#*,}" ;; esac; fi
	if [ "$ipv6" = true ] && [ "$net6" = static ]; then a6="${ip6%%,*}"; case "$ip6" in *,*) g6="${ip6#*,}" ;; esac; fi
	# without an uplink in the specification a static uplink has both
	# lists, empty or not, as it always had
	[ -n "$uplink_spec" ] || [ "$net4" = dhcp ] || keys=1
	if [ -n "$a4$a6" ] || [ "$keys" = 1 ]; then
		echo "      addresses:"
		[ -z "$a4" ] || echo "        - \"$a4\""
		[ -z "$a6" ] || echo "        - \"$a6\""
	fi
	if [ -n "$g4$g6" ] || [ "$keys" = 1 ]; then
		echo "      routes:"
		[ -z "$g4" ] || printf '        - to: default\n          via: "%s"\n          on-link: true\n' "$g4"
		[ -z "$g6" ] || printf '        - to: "::/0"\n          via: "%s"\n          on-link: true\n' "$g6"
	fi
	if [ "$ipv6" != true ]; then
		echo "      link-local: []"
	elif [ "$net6" = auto ]; then
		echo "      accept-ra: true"
		echo "      ipv6-address-generation: eui64"
	else
		echo "      accept-ra: false"
	fi
	if [ -n "$dns" ]; then
		echo "      nameservers:"
		echo "        addresses: [$(echo "$dns" | sed 's/ /, /g')]"
	fi
}

# networkd_files DIR: the same for systemd-networkd itself (Arch), in DIR:
# the names by MAC (.link), the others up without addresses, the uplink
networkd_files() {
	local nd="$1" i d
	for i in "${!IF_MAC[@]}"; do
		if [ "$rename" = yes ]; then
			printf '# written by UMP at installation: the uniform name of this interface\n[Match]\nMACAddress=%s\n\n[Link]\nName=%s\n' \
				"${IF_MAC[$i]}" "${IF_NAME[$i]}" > "$nd/10-ump-${IF_NAME[$i]}.link"
		fi
		[ "${IF_NAME[$i]}" = "$uplink" ] && continue
		printf '# written by UMP at installation: up, without addresses\n[Match]\nMACAddress=%s\n\n[Link]\nRequiredForOnline=no\n\n[Network]\nLinkLocalAddressing=no\n' \
			"${IF_MAC[$i]}" > "$nd/20-ump-${IF_NAME[$i]}.network"
	done
	{
		echo "# written by UMP at installation; networkctl reload after editing"
		echo "[Match]"
		echo "MACAddress=$mac"
		echo
		echo "[Network]"
		[ "$net4" != dhcp ] || echo "DHCP=ipv4"
		[ "$net4" != static ] || echo "Address=${ip4%%,*}"
		[ "$ipv6" != true ] || [ "$net6" != static ] || echo "Address=${ip6%%,*}"
		for d in $dns; do echo "DNS=$d"; done
		if [ "$ipv6" != true ]; then
			echo "LinkLocalAddressing=no"; echo "IPv6AcceptRA=no"
		elif [ "$net6" = auto ]; then
			echo "IPv6AcceptRA=yes"
		else
			echo "IPv6AcceptRA=no"
		fi
		if [ "$net4" = static ]; then case "$ip4" in *,*) printf '\n[Route]\nGateway=%s\nGatewayOnLink=yes\n' "${ip4#*,}" ;; esac; fi
		if [ "$ipv6" = true ] && [ "$net6" = static ]; then case "$ip6" in *,*) printf '\n[Route]\nGateway=%s\nGatewayOnLink=yes\n' "${ip6#*,}" ;; esac; fi
		[ -z "$client_id" ] || printf '\n[DHCPv4]\nClientIdentifier=%s\n' "$client_id"
		[ "$lease_dns" = yes ] || printf 'UseDNS=no\n'
		if [ "$ipv6" = true ] && [ "$net6" = auto ]; then printf '\n[IPv6AcceptRA]\nToken=eui64\n'; fi
	} > "$nd/20-ump-$uplink.network"
}

# resolved_conf: the global resolvers of systemd-resolved
# (/etc/systemd/resolved.conf.d/ump.conf), the same the uplink has
resolved_conf() {
	case "$dns_source" in
	public) printf '# UMP: public resolvers, independent of the provider; the .network\n# file sets the same per interface. Replace both to use your own.\n[Resolve]\nDNS=%s\n' "$dns" ;;
	spec) printf '# UMP: the resolvers of the specification (network.uplink.dns); the\n# uplink'"'"'s configuration sets the same. Replace both to use others.\n[Resolve]\nDNS=%s\n' "$dns" ;;
	dhcp) printf '# UMP: no resolvers here: they come from the DHCP lease of the uplink\n# (network.uplink.dns_from_dhcp).\n[Resolve]\n' ;;
	esac
}

# nixos_net_facts: the uplink as facts.json has it for UMP's NixOS
# module; with an uplink in the specification also dhcp4 (DHCP, which
# then identifies the machine by its MAC), dhcp4_dns (the lease's
# resolvers, unless the specification lists its own) and auto6 (router
# advertisements)
nixos_net_facts() {
	local more='{}'
	if [ -n "$uplink_spec" ]; then
		more="$(jq -n --argjson dhcp4 "$([ "$net4" = dhcp ] && echo true || echo false)" \
			--argjson dhcp4_dns "$([ "$net4" = dhcp ] && [ "$lease_dns" = yes ] && echo true || echo false)" \
			--argjson auto6 "$([ "$net6" = auto ] && echo true || echo false)" '{dhcp4: $dhcp4, dhcp4_dns: $dhcp4_dns, auto6: $auto6}')"
	fi
	jq -n --arg uplink_mac "$mac" --arg ip4 "${ip4%%,*}" --arg gw4 "$(case "$ip4" in *,*) echo "${ip4#*,}" ;; esac)" \
		--arg ip6 "${ip6%%,*}" --arg gw6 "$(case "$ip6" in *,*) echo "${ip6#*,}" ;; esac)" --arg dns "$dns" --argjson more "$more" \
		'{uplink_mac: $uplink_mac, ip4: $ip4, gw4: $gw4, ip6: $ip6, gw6: $gw6, dns: ($dns | split(" "))} + $more'
}

# net_plan: the uplink, as the plan shows it
net_plan() {
	local addresses="$ip4${ip6:+ $ip6}"
	if [ -n "$uplink_spec" ]; then
		addresses="IPv4 $net4${ip4:+ $ip4}$([ "$net4" != dhcp ] || echo " (client ID: MAC)"), IPv6 $net6${ip6:+ $ip6} (the specification's uplink)"
	fi
	echo "$uplink ($mac) $addresses; IPv6 $([ "$ipv6" = true ] && echo on || echo off); DNS ${dns:-from DHCP}$([ "$lease_dns" = yes ] || echo ", not the lease's")"
}

# net_record: the lines of /etc/ump/install that say how the
# specification's uplink is configured: network (IPv4) and network6 are
# static, dhcp, auto or none, dns the resolvers or dhcp
net_record() {
	printf 'network=%s\nnetwork6=%s\ndns=%s\n' "$net4" "$net6" "$([ "$dns_source" = dhcp ] && echo dhcp || echo "${dns// /,}")"
}

# --- keep-data: the old system on the disks, and the plan for it --------

# Functions only, which images/test-keep-plan.sh runs on machines of its
# own. Keep-data replaces the old system and leaves its data: nothing is
# wiped or partitioned; the old root is found, its /etc/fstab read, and
# every entry of it is formatted, kept, set aside or left alone, as the
# plan says. What the machine holds comes through the kd_fact_ functions
# alone (the test has its own): they mount read-only, replay no journal
# and write nothing. KD_JQ turns the facts into the plan; kd_build
# gathers the facts and prints it.

KD_DIR=/run/ump-keep-data
KD_LOOK="$KD_DIR/look"
KD_MARK=.ump-keep-data   # at the top of the root's file system: what an interrupted installation saved
KD_STAMP=""     # the date in the names of what is set aside, when writing begins
kd_wrote=0      # 1 from the first write on: before it, a refusal has changed nothing

# kd_look DEV FSTYPE [OPTIONS]: DEV read-only at $KD_LOOK, without the
# journal: a look must not change the device. The device itself is
# read-only meanwhile: a file system that was not unmounted cleanly is
# read as the disk has it, and whatever its driver would write (an
# error it finds, into the superblock) does not reach the disk
kd_look() {
	local t="$2" o=ro
	case "$2" in
	ext2|ext3|ext4) t=ext4; o=ro,noload,errors=continue ;;
	xfs) o=ro,norecovery ;;
	btrfs) o=ro,rescue=nologreplay ;;
	"") return 1 ;;
	esac
	mkdir -p "$KD_LOOK"
	kd_unlook
	if [ "$(blockdev --getro "$1" 2>/dev/null)" = 0 ] && blockdev --setro "$1" 2>/dev/null; then echo "$1" > "$KD_DIR/read-only"; fi
	mount -t "$t" -o "$o${3:+,$3}" "$1" "$KD_LOOK" 2>/dev/null || { kd_unlook; return 1; }
}
kd_unlook() {
	if mountpoint -q "$KD_LOOK"; then umount "$KD_LOOK" || die "keep-data: $KD_LOOK cannot be unmounted"; fi
	if [ -s "$KD_DIR/read-only" ]; then
		blockdev --setrw "$(cat "$KD_DIR/read-only")" || die "keep-data: $(cat "$KD_DIR/read-only") cannot be made writable again (blockdev --setrw)"
		rm -f "$KD_DIR/read-only"
	fi
}

# kd_dirty DEV FSTYPE: true when the file system was not unmounted
# cleanly: its journal or log holds what the look does not see
kd_dirty() {
	local r=false out=""
	# (what they print is taken whole or cut short, never searched in a
	# pipeline, whose status a reader that has seen enough would spoil)
	case "$2" in
	ext*) out="$(dumpe2fs -h "$1" 2>/dev/null | sed -n 's/^Filesystem features: *//p' || true)"; case " $out " in *" needs_recovery "*) r=true ;; esac ;;
	xfs) if command -v xfs_logprint >/dev/null; then out="$(xfs_logprint -t "$1" 2>/dev/null | head -n 20 || true)"; case "$out" in *"<DIRTY>"*) r=true ;; esac; fi ;;
	btrfs) out="$(btrfs inspect-internal dump-super "$1" 2>/dev/null | awk '$1 == "log_root" {print $2}' || true)"; case "$out" in ""|0) ;; *) r=true ;; esac ;;
	esac
	echo "$r"
}

# kd_marker_ok DIR: what an interrupted installation saved in DIR is
# whole: the old fstab and the first plan are the ones it wrote
kd_marker_ok() { [ -f "$1/SHA256SUMS" ] && [ -f "$1/etc/fstab" ] && [ -f "$1/plan.json" ] && [ -f "$1/state" ] && (cd "$1" && sha256sum -c --status SHA256SUMS 2>/dev/null); }
# kd_state DIR KEY: a value of its state
kd_state() { sed -n "s/^$2=//p" "$1/state" 2>/dev/null | head -n 1; }

# kd_fact_lsblk: every block device, as lsblk's JSON
kd_fact_lsblk() { lsblk -J -b -o NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,PARTUUID,PARTLABEL,PARTTYPE,PARTN,PTTYPE,MOUNTPOINTS,RO,RM,TRAN,START; }

# kd_fact_realpath PATH: the device a path under /dev leads to
kd_fact_realpath() { readlink -f "$1" 2>/dev/null || printf '%s\n' "$1"; }

# kd_fact_fs DEV FSTYPE: the old systems on a file system: {roots:
# [{subvolume, system, hostname, resume}], subvolumes: [{id, top, path}],
# default, dirty, broken}. A root holds /etc/fstab and /etc/os-release;
# on btrfs every subvolume is looked at. resume: what an interrupted
# keep-data installation saved of the old root's files, at the top of
# the file system ($KD_MARK, whose state names the subvolume); broken:
# that is there and damaged, and nothing on the file system is taken
# for an old system
kd_fact_fs() {
	local dev="$1" fs="$2" o="" subs='[]' def="" roots='[]' p base b f resume system host mark="" msub="" broken=false dirty
	[ "$fs" != btrfs ] || o=subvolid=5
	dirty="$(kd_dirty "$dev" "$fs")"
	kd_look "$dev" "$fs" "$o" || { jq -n -c --argjson dirty "$dirty" '{error: "it cannot be mounted", dirty: $dirty}'; return 0; }
	if [ "$fs" = btrfs ]; then
		subs="$(btrfs subvolume list "$KD_LOOK" 2>/dev/null | sed -n 's/^ID \([0-9]*\) gen [0-9]* top level \([0-9]*\) path \(.*\)$/\1\t\2\t\3/p' \
			| jq -R -s -c 'split("\n") | map(select(. != "") | split("\t") | {id: (.[0] | tonumber), top: (.[1] | tonumber), path: .[2]})')"
		def="$(btrfs subvolume get-default "$KD_LOOK" 2>/dev/null | sed -n 's/^ID [0-9]* gen [0-9]* top level [0-9]* path \(.*\)$/\1/p')"
	fi
	if [ -e "$KD_LOOK/$KD_MARK" ] || [ -L "$KD_LOOK/$KD_MARK" ]; then
		if [ -d "$KD_LOOK/$KD_MARK" ] && [ ! -L "$KD_LOOK/$KD_MARK" ] && kd_marker_ok "$KD_LOOK/$KD_MARK"; then mark="$KD_LOOK/$KD_MARK"; msub="$(kd_state "$mark" subvolume)"; else broken=true; fi
	fi
	while IFS= read -r p; do
		[ "$broken" = false ] || break
		p="${p#=}"; base="$KD_LOOK${p:+/$p}"
		if [ -n "$mark" ] && [ "$p" = "$msub" ]; then
			resume=true; b="$mark"
		elif [ -f "$base/etc/fstab" ] && [ ! -L "$base/etc/fstab" ] && { [ -e "$base/etc/os-release" ] || [ -L "$base/etc/os-release" ]; }; then
			resume=false; b="$base"
		else
			continue
		fi
		# os-release is a link into /usr, which must not lead into this system's
		system=""
		for f in "$b/usr/lib/os-release" "$b/etc/os-release"; do
			if [ -f "$f" ] && [ ! -L "$f" ]; then system="$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$f" | head -n 1)"; break; fi
		done
		host=""; if [ -f "$b/etc/hostname" ] && [ ! -L "$b/etc/hostname" ]; then host="$(head -n 1 "$b/etc/hostname")"; fi
		roots="$(jq -c --arg s "$p" --arg system "$system" --arg host "$host" --argjson resume "$resume" '. + [{subvolume: $s, system: $system, hostname: $host, resume: $resume}]' <<< "$roots")"
	done < <(echo "="; jq -r '.[] | "=" + .path' <<< "$subs"; if [ -n "$msub" ] && ! jq -e --arg s "$msub" 'any(.path == $s)' >/dev/null <<< "$subs"; then echo "=$msub"; fi)
	kd_unlook
	# (what can be long goes to jq on its input: an argument has a limit)
	printf '%s\n' "$roots" "$subs" | jq -c -s --arg default "$def" --argjson dirty "$dirty" --argjson broken "$broken" '{roots: .[0], subvolumes: .[1], default: $default, dirty: $dirty, broken: $broken}'
}

# kd_fact_has_uuid UUID: a file system with this UUID is there
kd_fact_has_uuid() { [ -n "$(blkid -U "$1" 2>/dev/null)" ]; }

# kd_journal FILE: the saved fstab on stdin, with the file systems an
# interrupted installation made anew named by the UUIDs they have now:
# FILE has "OLD NEW" per line, written before each mkfs (one that was
# made anew twice has a line for each time); a line whose mkfs never ran
# changes nothing
kd_journal() {
	local text o n i
	local -a olds=() news=()
	local -A final=()
	text="$(cat)"
	if [ -f "$1" ] && [ ! -L "$1" ]; then
		while read -r o n; do
			if [ -n "$o" ] && [ -n "$n" ]; then olds+=("$o"); news+=("$n"); fi
		done < "$1"
		for ((i = ${#olds[@]} - 1; i >= 0; i--)); do
			o="${olds[$i]}"; n="${news[$i]}"
			if [ -n "${final[$n]:-}" ]; then final[$o]="${final[$n]}"
			elif kd_fact_has_uuid "$n"; then final[$o]="$n"; fi
		done
		for o in "${!final[@]}"; do
			text="$(printf '%s\n' "$text" | awk -v o="$o" -v n="${final[$o]}" 'tolower($1) == "uuid=" tolower(o) && !/^[ \t]*#/ {$1 = "UUID=" n} {print}')"
		done
	fi
	printf '%s\n' "$text"
}

# kd_saved_json DIR RAM: what an interrupted installation saved (in DIR:
# $KD_MARK of the root's file system, or the copy in memory), as
# kd_fact_root gives it: the old fstab (kd_journal), the first plan, and
# how far that installation came (saved: {root: old, aside or gone;
# aside: the name the old system was moved to})
kd_saved_json() {
	local b="$1" keyfiles='[]' k fstab hostid="" zc=false first=null
	rf() { if [ -f "$b/$1" ] && [ ! -L "$b/$1" ]; then cat "$b/$1"; fi; }
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		if [ -f "$b$k" ] && [ ! -L "$b$k" ]; then keyfiles="$(jq -c --arg p "$k" --arg m "$(stat -c %a "$b$k")" '. + [{path: $p, mode: $m}]' <<< "$keyfiles")"; fi
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $3}' | sort -u)
	fstab="$(rf etc/fstab | kd_journal "$b/formats")"
	if [ -f "$b/etc/hostid" ] && [ ! -L "$b/etc/hostid" ]; then hostid="$(base64 -w0 < "$b/etc/hostid")"; fi
	if [ -f "$b/etc/zfs/zpool.cache" ]; then zc=true; fi
	if jq -e 'type == "object"' "$b/plan.json" >/dev/null 2>&1; then first="$(jq -c . "$b/plan.json")"; fi
	printf '%s\n' "$first" | jq -c --arg fstab "$fstab" --arg crypttab "$(rf etc/crypttab)" --arg mdadm_conf "$(rf etc/mdadm/mdadm.conf; rf etc/mdadm.conf)" \
		--arg hostid "$hostid" --argjson zpool_cache "$zc" --argjson keyfiles "$keyfiles" --argjson ram "$2" \
		--arg root "$(kd_state "$b" root)" --arg aside "$(kd_state "$b" aside)" \
		'{fstab: $fstab, crypttab: $crypttab, mdadm_conf: $mdadm_conf, hostid: $hostid, zpool_cache: $zpool_cache, keyfiles: $keyfiles, opens: {}, modules: [],
		  resume: true, ram: $ram, first: ., saved: {root: $root, aside: $aside}}'
}

# kd_fact_root DEV SUBVOLUME FSTYPE: what the old root says: {fstab,
# crypttab, mdadm_conf, hostid (base64), zpool_cache, keyfiles: [{path,
# mode}] (the key files of crypttab that are files of the root), opens:
# {NAME: false} for a crypttab entry whose key file does not open its
# LUKS container (tried without opening it), modules: the versions of
# the kernels it has modules for, resume (then first and saved, as
# kd_saved_json has them)}
kd_fact_root() {
	local dev="$1" sub="$2" fs="$3" o="" b keyfiles='[]' opens='{}' modules='[]' k n src co d hostid="" zc=false
	[ "$fs" != btrfs ] || o=subvolid=5
	kd_look "$dev" "$fs" "$o" || { echo '{}'; return 0; }
	if [ -d "$KD_LOOK/$KD_MARK" ] && [ ! -L "$KD_LOOK/$KD_MARK" ] && kd_marker_ok "$KD_LOOK/$KD_MARK" && [ "$(kd_state "$KD_LOOK/$KD_MARK" subvolume)" = "$sub" ]; then
		kd_saved_json "$KD_LOOK/$KD_MARK" false
		kd_unlook
		return 0
	fi
	b="$KD_LOOK${sub:+/$sub}"
	[ -d "$b" ] || { kd_unlook; echo '{}'; return 0; }
	rf() { if [ -f "$b/$1" ] && [ ! -L "$b/$1" ]; then cat "$b/$1"; fi; }
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		if [ -f "$b$k" ] && [ ! -L "$b$k" ]; then
			keyfiles="$(jq -c --arg p "$k" --arg m "$(stat -c %a "$b$k")" '. + [{path: $p, mode: $m}]' <<< "$keyfiles")"
		fi
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $3}' | sort -u)
	while read -r n src k co; do
		[ -f "$b$k" ] && [ ! -L "$b$k" ] || continue
		# options that say how the key is read, or that it is no LUKS: not tried
		case ",$co," in *,keyfile-size=*|*,keyfile-offset=*|*,header=*|*,plain,*|*,tcrypt,*|*,bitlk,*|*,swap,*|*,tmp,*|*,tmp=*) continue ;; esac
		case "$src" in UUID=*) d="/dev/disk/by-uuid/${src#UUID=}" ;; /dev/*) d="$src" ;; *) continue ;; esac
		[ -b "$d" ] && cryptsetup isLuks "$d" 2>/dev/null || continue
		cryptsetup open --test-passphrase --key-file "$b$k" "$d" >/dev/null 2>&1 || opens="$(jq -c --arg n "$n" '.[$n] = false' <<< "$opens")"
	done < <(rf etc/crypttab | awk '$1 !~ /^#/ && $3 ~ /^\// && $3 !~ /^\/dev\// {print $1, $2, $3, $4}')
	# the kernels the old system has modules for (/lib a link into /usr
	# must not lead into this system's)
	modules="$(for d in "$b/usr/lib/modules" "$b/lib/modules"; do
			[ -d "$d" ] && [ ! -L "$d" ] && [ ! -L "${d%/modules}" ] || continue
			for k in "$d"/*/; do [ -d "$k" ] && basename "$k"; done
		done | sort -u | jq -R -s -c 'split("\n") | map(select(. != ""))')"
	if [ -f "$b/etc/hostid" ] && [ ! -L "$b/etc/hostid" ]; then hostid="$(base64 -w0 < "$b/etc/hostid")"; fi
	if [ -f "$b/etc/zfs/zpool.cache" ]; then zc=true; fi
	jq -n -c --arg fstab "$(rf etc/fstab)" --arg crypttab "$(rf etc/crypttab)" --arg mdadm_conf "$(rf etc/mdadm/mdadm.conf; rf etc/mdadm.conf)" \
		--arg hostid "$hostid" --argjson zpool_cache "$zc" --argjson keyfiles "$keyfiles" --argjson opens "$opens" --argjson modules "$modules" \
		'{fstab: $fstab, crypttab: $crypttab, mdadm_conf: $mdadm_conf, hostid: $hostid, zpool_cache: $zpool_cache, keyfiles: $keyfiles, opens: $opens, modules: $modules, resume: false}'
	kd_unlook
}

# kd_fact_ram: {device, subvolume, system, hostname} when an installation
# failed in this rescue system between its first write and the moment
# the old system's files were saved on the root (the root was being
# formatted): they are in memory still ($KD_DIR/old, while
# $KD_DIR/unsaved is there), and the root is the device they name, as
# long as it holds nothing, or the file system that installation made
kd_fact_ram() {
	local old="$KD_DIR/old" dev now system=""
	[ -e "$KD_DIR/unsaved" ] && kd_marker_ok "$old" || return 0
	dev="$(kd_state "$old" device)"
	[ -b "$dev" ] || return 0
	now="$(kd_fact_uuid "$dev")"
	[ -z "$now" ] || [ "$now" = "$(kd_state "$old" uuid_new)" ] || return 0
	if [ -f "$old/etc/os-release" ]; then system="$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$old/etc/os-release" | head -n 1)"; fi
	jq -n -c --arg d "$dev" --arg s "$(kd_state "$old" subvolume)" --arg system "$system" --arg host "$(head -n 1 "$old/etc/hostname" 2>/dev/null || true)" \
		'{device: $d, subvolume: $s, system: $system, hostname: $host}'
}
# kd_fact_root_ram: kd_fact_root's answer from that copy
kd_fact_root_ram() { kd_saved_json "$KD_DIR/old" true; }

# kd_fact_usage DEV SUBVOLUME FSTYPE SIZES: the old root's file system:
# {size_mb, used_mb, free_mb, features (ext4's that an older GRUB cannot
# read), dirty (not unmounted cleanly), immutable (the entries at its top
# that cannot be renamed: chattr +i or +a), usage: [{path, kb}]}: with
# SIZES 1 what the root holds beyond the system itself: the home
# directories, /root, /srv, /opt, /etc, the directories under /var/lib,
# whatever else lies at the top
kd_fact_usage() {
	local dev="$1" sub="$2" fs="$3" sizes="$4" o="" df usage='[]' feat="" dirty d tree="" immutable='[]'
	[ "$fs" != btrfs ] || { o=subvolid=5; [ -z "$sub" ] || o="subvol=/$sub"; }
	dirty="$(kd_dirty "$dev" "$fs")"
	kd_look "$dev" "$fs" "$o" || { jq -n -c --argjson dirty "$dirty" '{dirty: $dirty}'; return 0; }
	df="$(df -m --output=size,used,avail "$KD_LOOK" | tail -n 1)"
	case "$fs" in ext*)
		feat="$(dumpe2fs -h "$dev" 2>/dev/null | sed -n 's/^Filesystem features: *//p' | tr ' ' '\n' | grep -xE 'orphan_file|metadata_csum_seed' | paste -sd' ' - || true)" ;;
	esac
	if command -v lsattr >/dev/null; then
		immutable="$(cd "$KD_LOOK" && for d in * .[!.]* ..?*; do
			[ -e "$d" ] || [ -L "$d" ] || continue
			case "$d" in lost+found|old-root-*|failed-install-*|"$KD_MARK") continue ;; esac
			[ -L "$d" ] || lsattr -d -- "$d" 2>/dev/null | awk '$1 ~ /[ia]/ {sub(/^[^ ]+ /, ""); print "/" $0}'
		done | jq -R -s -c 'split("\n") | map(select(. != ""))')"
	fi
	if [ "$sizes" = 1 ]; then
		# the system's own directories are not counted, nor a swap file
		usage="$(cd "$KD_LOOK" && for d in usr/local home/* var/lib/* var/* *; do
			[ -e "$d" ] && [ ! -L "$d" ] || continue
			case "$d" in bin|boot|dev|home|lib|lib32|lib64|libx32|lost+found|proc|run|sbin|snap|sys|tmp|usr|var|swapfile|swap.img|var/lib|var/cache|var/tmp|var/run|var/lock) continue ;; esac
			timeout 900 du -sk -- "$d" 2>/dev/null || true
		done | awk -F'\t' '!seen[$2]++' | jq -R -s -c 'split("\n") | map(select(. != "") | split("\t") | {path: ("/" + .[1]), kb: (.[0] | tonumber)})')"
		# a subvolume that is deleted gives its space back
		if [ "$fs" = btrfs ] && [ -n "$sub" ]; then tree="$(timeout 900 du -sm "$KD_LOOK" 2>/dev/null | cut -f1 || true)"; fi
	fi
	kd_unlook
	set -- $df
	jq -c --argjson size "${1:-0}" --argjson used "${2:-0}" --argjson free "${3:-0}" --argjson tree "${tree:-0}" --arg features "$feat" --argjson dirty "$dirty" --argjson immutable "$immutable" \
		'{size_mb: $size, used_mb: $used, free_mb: $free, tree_mb: $tree, features: $features, dirty: $dirty, immutable: $immutable, usage: .}' <<< "$usage"
}

# kd_fact_boot DEV FSTYPE: what a device holds of a /boot: {kernels: the
# versions of its vmlinuz-VERSION files, loaders: the boot loaders'
# directories there (grub, grub2, extlinux, syslinux, loader, EFI)}
kd_fact_boot() {
	local kernels loaders f
	kd_look "$1" "$2" || { echo '{"error": "it cannot be mounted"}'; return 0; }
	kernels="$(for f in "$KD_LOOK"/vmlinuz-* "$KD_LOOK"/vmlinux-*; do [ -f "$f" ] && { f="${f##*/}"; printf '%s\n' "${f#vmlinu?-}"; }; done | sort -u | jq -R -s -c 'split("\n") | map(select(. != ""))')"
	loaders="$(for f in grub grub2 extlinux syslinux loader EFI; do [ -d "$KD_LOOK/$f" ] && printf '%s\n' "$f"; done | jq -R -s -c 'split("\n") | map(select(. != ""))')"
	kd_unlook
	jq -n -c --argjson kernels "$kernels" --argjson loaders "$loaders" '{kernels: $kernels, loaders: $loaders}'
}

# kd_fact_esp DEV ID...: the EFI system partition: {free_mb, loaders,
# fallback}: the boot loaders on it that are the old system's: a
# directory under EFI (but BOOT, which the new loader takes) with a file
# that names one of the IDs, those of the old root and /boot (GRUB's
# image or its grub.cfg does), or systemd-boot whose entries do. The
# firmware would start them, and they would look for a system that is
# gone. fallback: EFI/BOOT holds a boot loader now, which the new one
# replaces
kd_fact_esp() {
	local dev="$1" free loaders='[]' d n u hit fallback=false; shift
	kd_look "$dev" vfat || { echo '{"error": "it cannot be mounted"}'; return 0; }
	free="$(df -m --output=avail "$KD_LOOK" | tail -n 1 | tr -d ' ')"
	for d in "$KD_LOOK"/EFI/*/; do
		[ -d "$d" ] || continue
		n="$(basename "$d")"
		# not BOOT, which the new loader takes; not what an earlier keep-data set aside
		case "$(printf '%s' "$n" | tr a-z A-Z)" in
		BOOT) if [ -n "$(ls -A "$d" 2>/dev/null)" ]; then fallback=true; fi; continue ;;
		*-OLD-[0-9]*-[0-9]*) continue ;;
		esac
		hit=0
		for u in "$@"; do
			[ -n "$u" ] || continue
			if grep -rqsiF -- "$u" "$d"; then hit=1; fi
			case "$(printf '%s' "$n" | tr a-z A-Z)" in SYSTEMD) if grep -rqsiF -- "$u" "$KD_LOOK/loader"; then hit=1; fi ;; esac
		done
		[ "$hit" = 0 ] || loaders="$(jq -c --arg n "EFI/$n" '. + [$n]' <<< "$loaders")"
	done
	kd_unlook
	jq -n -c --argjson free "${free:-0}" --argjson loaders "$loaders" --argjson fallback "$fallback" '{free_mb: $free, loaders: $loaders, fallback: $fallback}'
}

# kd_fact_mbr DISK: grub when GRUB's boot code is in the disk's first sector
kd_fact_mbr() { if dd if="$1" bs=512 count=1 status=none 2>/dev/null | grep -qa GRUB; then echo grub; fi; }

# kd_fact_lvs: the logical volumes: PATH|SEGMENT TYPE|ACTIVE|VOLUME'S UUID per line
kd_fact_lvs() { lvs --noheadings --separator '|' -o lv_dm_path,segtype,lv_active,lv_uuid 2>/dev/null | sed 's/^ *//' || true; }

# The plan, from the facts: {machine: {efi, arch, os, image: {version,
# sha256} (the image the manifest names), unpacked_mb, root_fs, final},
# options: {root, keep_root, format}, lsblk, fs: {DEV:
# what kd_fact_fs says}, roots: {"DEV<tab>SUBVOLUME": kd_fact_root's,
# with fstab_sha256}, usage, esp: {DEV: kd_fact_esp's}, boot: {DEV:
# kd_fact_boot's}, links: {PATH: device}, mbr: {DISK: grub}, lvs}. A
# refusal is an error {refused: why}.
read -r -d '' KD_JQ <<'JQ' || true
def refuse($m): error({refused: $m});
def norm_uuid: ascii_downcase | gsub("[-:]"; "");
def lines: split("\n") | map(sub("^[ \t]+"; "") | sub("[ \t\r]+$"; "")) | map(select(length > 0 and (startswith("#") | not)));
def words: [splits("[ \t]+")] | map(select(. != ""));
def fstab: lines | map(words | select(length >= 2)
	| {spec: .[0], mountpoint: (.[1] | if length > 1 then sub("/+$"; "") else . end), type: (.[2] // "auto"), options: (.[3] // "defaults"), dump: (.[4] // "0"), pass: (.[5] // "0")});
def crypttab: lines | map(words | select(length >= 2) | {name: .[0], source: .[1], keyfile: (.[2] // "none"), options: (.[3] // "")});
# the arrays of an mdadm.conf by the names it gives them, /dev/md0 and /dev/md/0 alike
def mdconf: lines | map(words | select(.[0] == "ARRAY" and length >= 3)
		| {device: .[1], uuid: ((map(select(test("^(UUID|uuid)=")))[0] // "") | sub("^(UUID|uuid)="; "") | norm_uuid)} | select(.uuid != ""))
	| map(., (if (.device | test("^/dev/md/[0-9]+$")) then .device = "/dev/md" + (.device | sub("^/dev/md/"; ""))
		elif (.device | test("^/dev/md[0-9]+$")) then .device = "/dev/md/" + (.device | sub("^/dev/md"; "")) else empty end))
	| map({key: .device, value: .uuid}) | from_entries;
def opts: split(",") | map(select(. != ""));
def opt($k): (opts | map(select(. == $k or startswith($k + "="))) | .[0]) as $o
	| if $o == null then null elif $o == $k then "" else $o[($k | length) + 1:] end;
def with_opt($o): if (opts | index($o)) != null then . else (opts + [$o] | join(",")) end;
def without_opt($k): opts | map(select((. == $k or startswith($k + "=")) | not)) | join(",") | if . == "" then "defaults" else . end;
def masked: gsub("(?<k>(^|,)(password|passwd|pass|pw)=)[^,]*"; "\(.k)***");
def size: if . >= 10240 then "\(. / 1024 | floor)G" elif . >= 1024 then "\(. * 10 / 1024 | floor | . / 10)G" else "\(.)M" end;
# the file systems of the kernel itself and of the network, which have no device here; those of a device the new system mounts for certain
def pseudo: . as $t | ["tmpfs", "ramfs", "proc", "sysfs", "devpts", "devtmpfs", "debugfs", "tracefs", "securityfs", "configfs", "hugetlbfs", "mqueue", "binfmt_misc", "cgroup", "cgroup2", "efivarfs", "bpf", "fusectl", "pstore", "autofs", "rpc_pipefs", "nfsd"] | index($t) != null;
def network: . as $t | (["nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "glusterfs", "9p", "virtiofs", "davfs", "lustre", "ocfs2", "gfs2", "beegfs"] | index($t) != null) or ($t | test("^fuse(\\.|$)|sshfs"));
def knownfs: . as $t | ["ext2", "ext3", "ext4", "xfs", "btrfs", "vfat", "exfat", "iso9660", "udf", "f2fs", "squashfs"] | index($t) != null;
# a label a file system made anew can take
def fits($fs): if . == null or . == "" then "" elif $fs == "xfs" and (length > 12 or test(" ")) then "" elif $fs == "vfat" and length > 11 then "" elif length > 16 and $fs != "btrfs" then "" else . end;
# every block device once, with all it sits on (ancestors); no loop, RAM or optical devices; usb: on a USB or removable disk
def devices:
	def w($anc; $usb): . as $d | (($d.tran == "usb") or ($d.rm == true) or $usb) as $u
		| ($d | del(.children) | .ancestors = $anc | .usb = $u), ((.children // [])[] | w($anc + [$d.path]; $u));
	[.blockdevices[] | w([]; false)] | group_by(.path) | map(.[0] + {ancestors: (map(.ancestors) | add | unique), usb: (map(.usb) | any)})
	| map(select(.name | test("^(loop|ram|zram|sr|fd|nbd)") | not))
	| map(.mountpoints = ((.mountpoints // []) | map(select(. != null))) | .size_mb = ((.size // 0) / 1048576 | floor));
# the file systems an old root can be on, a btrfs of several devices once; none on a USB or removable disk
def filesystems: map(select((.fstype == "ext2" or .fstype == "ext3" or .fstype == "ext4" or .fstype == "xfs" or .fstype == "btrfs") and (.usb | not)))
	| group_by(if .fstype == "btrfs" then "btrfs " + (.uuid // .path) else "dev " + .path end) | map(.[0]) | sort_by(.path);
def plan:
	. as $f
	| ($f.machine) as $m
	| ($f.options // {}) as $o
	| ($f.lsblk | devices) as $devs
	| ($devs | map({key: .path, value: .}) | from_entries) as $by
	| "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" as $espguid
	| "21686148-6449-6e6f-744e-656564454649" as $biosguid
	| def dev($p): $by[$p];
	  def chain($p): [$p] + (dev($p).ancestors // []);
	  def disks_of($p): chain($p) | map(select(dev(.).type == "disk")) | unique;
	  def samefs($a; $b): $a == $b or (dev($a).fstype == "btrfs" and dev($b).fstype == "btrfs" and dev($a).uuid != null and dev($a).uuid == dev($b).uuid);
	  def fskey($p): if dev($p).fstype == "btrfs" and dev($p).uuid != null then ([$devs[] | select(.fstype == "btrfs" and .uuid == dev($p).uuid)][0].path) else $p end;
	  def extfs: . == "ext2" or . == "ext3" or . == "ext4";
	  def isarray: (.type // "") | test("^(raid|md|linear)");
	  def isesp: .fstype == "vfat" and ((.parttype // "") | ascii_downcase | (. == $espguid or . == "0xef"));
	  # read-only: an array is as its members are (one that was assembled read-only here is made writable when writing begins)
	  def isro($p): dev($p) as $d | if ($d | isarray) then ($d.ancestors | any(dev(.) as $a | ($a | isarray | not) and $a.ro == true)) else $d.ro == true end;
	  # what a device is, for whoever must recognise it
	  def what($p): dev($p) as $d | "\($d.fstype // "no file system")\(if ($d.label // "") != "" then ", label " + $d.label else "" end), \($d.size_mb | size)";
	  def kernelname: test("^/dev/(sd|vd|hd|xvd|nvme|mmcblk|loop|md[0-9_]|md/|dm-)");
	  # resolve: the device an fstab or crypttab field names: {how: found|missing|ambiguous|crypt|none, device}
	  def resolve($md; $cnames):
		. as $s
		| ($s | sub("^/dev/disk/by-uuid/"; "UUID=") | sub("^/dev/disk/by-label/"; "LABEL=") | sub("^/dev/disk/by-partuuid/"; "PARTUUID=") | sub("^/dev/disk/by-partlabel/"; "PARTLABEL=")) as $t
		| def unq: sub("^\""; "") | sub("\"$"; "");
		  def fsd: $devs | map(select(.fstype != "linux_raid_member" and .fstype != "LVM2_member"));
		  def pick: (if length > 1 and (map(.fstype) | unique) == ["btrfs"] and (map(.uuid) | unique | length) == 1 then .[0:1] else . end)
			| if length == 1 then {how: "found", device: .[0].path}
			  elif length == 0 then {how: "missing", device: null}
			  else {how: "ambiguous", device: null, matches: map(.path)} end;
		  if ($t | startswith("UUID=")) then ($t[5:] | unq | ascii_downcase) as $u | [fsd[] | select(((.uuid // "") | ascii_downcase) == $u)] | pick
		  elif ($t | startswith("LABEL=")) then ($t[6:] | unq) as $l | [fsd[] | select(.label == $l)] | pick
		  elif ($t | startswith("PARTUUID=")) then ($t[9:] | unq | ascii_downcase) as $u | [$devs[] | select(((.partuuid // "") | ascii_downcase) == $u)] | pick
		  elif ($t | startswith("PARTLABEL=")) then ($t[10:] | unq) as $l | [$devs[] | select(.partlabel == $l)] | pick
		  elif ($t | startswith("/dev/")) then
			if $md[$t] != null then
				# an array by the name the old mdadm.conf gave it: here it has another
				[$devs[] | select(isarray) | select(.ancestors | any(dev(.) | (.fstype == "linux_raid_member" and ((.uuid // "") | norm_uuid) == $md[$t])))] | pick
			else
				($f.links[$t] // $t) as $p
				| [$devs[] | select(.path == $p or ("/dev/" + .kname) == $p or .path == $t)]
				| if length == 0 and ($t | test("^/dev/mapper/")) and (($cnames | index($t | sub("^/dev/mapper/"; ""))) != null)
				  then {how: "crypt", device: null, name: ($t | sub("^/dev/mapper/"; ""))} else pick end
			end
		  else {how: "none", device: null} end;
	  # the subvolume a btrfs entry mounts: subvol=, subvolid=, else the default one ("" is the top)
	  def subvol_of($opts; $d):
		($f.fs[fskey($d)] // {}) as $i
		| ($opts | opt("subvol")) as $sv | ($opts | opt("subvolid")) as $id
		| if $sv != null then ($sv | sub("^/+"; ""))
		  elif $id != null then (if $id == "5" or $id == "0" then "" else ((($i.subvolumes // []) | map(select((.id | tostring) == $id) | .path) | .[0]) // "?") end)
		  else ($i.default // "") end;
	  def rootfiles($c): $f.roots[$c.device + "\t" + $c.subvolume] // {};
	  # the device the fstab of a candidate names as its root: resolve's answer, null without one such entry
	  def rootspec($c): rootfiles($c) as $r | (($r.fstab // "") | fstab | map(select(.mountpoint == "/"))) as $e
		| if ($e | length) != 1 then null else ($e[0].spec | resolve(($r.mdadm_conf // "") | mdconf; [])) + {spec: $e[0].spec, options: $e[0].options} end;
	  # an old system is the one its own fstab mounts at /: a copy of it, or a snapshot, names another
	  def consistent($c):
		if $c.resume then true else
			rootspec($c) as $x
			| if $x == null or $x.how != "found" or (samefs($x.device; $c.device) | not) then false
			  elif dev($c.device).fstype != "btrfs" then true
			  else subvol_of($x.options; $c.device) == $c.subvolume end
		end;
	  def describe: "\(.device)\(if .subvolume != "" then ":" + .subvolume else "" end)\(if .system != "" then " (" + .system + ")" else "" end)";

	# nothing of the machine's disks may be in use
	([$devs[] | select(.mountpoints | length > 0) | "\(.path) (\(.mountpoints | map(if . == "[SWAP]" then "swap" else . end) | join(", ")))"]) as $busy
	| if ($busy | length) > 0 then refuse("the disks are in use: \($busy | join(", ")); unmount them (umount, swapoff) or reboot the rescue system") else . end
	| ([($f.fs // {}) | to_entries[] | .key as $d | (.value.roots // [])[]
		| {device: $d, subvolume: (.subvolume // ""), system: (.system // ""), hostname: (.hostname // ""), resume: (.resume // false)} + (if .ram == true then {ram: true} else {} end)]
		| map(. + {consistent: consistent(.)})) as $cands
	| ([$devs[] | select(.fstype == "crypto_LUKS") | .path]) as $luks
	| ([$devs[] | select(.fstype == "zfs_member") | .path]) as $zfsm
	# what was not seen: file systems that could not be looked at, or not whole
	| ([($f.fs // {}) | to_entries[] | select(.value.broken == true) | .key]) as $broken
	| ([($f.fs // {}) | to_entries[] | select(.value.error != null) | .key]) as $unseen
	| ([($f.fs // {}) | to_entries[] | select(.value.dirty == true and ((.value.roots // []) | length) == 0 and .value.broken != true) | .key]) as $unclean
	| ((if ($broken | length) > 0 then "; \($broken | join(", ")) holds what an interrupted keep-data installation saved of the old system, and that is damaged: it is not read (--root \($broken[0]) installs onto it without the old system's other volumes, which stay as they are)" else "" end)
	   + (if ($unseen | length) > 0 then "; \($unseen | map(. + " (" + (dev(.).fstype // "?") + ")") | join(", ")) could not be looked at (it cannot be mounted read-only)" else "" end)
	   + (if ($unclean | length) > 0 then "; \($unclean | map(. + " (" + (dev(.).fstype // "?") + ")") | join(", ")) was not unmounted cleanly and is read without its journal: an old system on it may not be seen (mount it once in the rescue system, which replays the journal: mount DEVICE /mnt; umount /mnt; and run again)" else "" end)) as $hints

	# --- the old root: the one old system, or the one --root names
	| (if ($o.root // "") != "" then
		(($o.root | resolve({}; [])) as $x
		 | if $x.how == "found" or $x.how == "ambiguous" then {x: $x, sub: null}
		   else ((($o.root | capture("^(?<d>.+):(?<s>[^:]*)$")) // null) as $c
			| if $c == null then {x: $x, sub: null} else {x: ($c.d | resolve({}; [])), sub: ($c.s | sub("^/+"; ""))} end) end) as $r0
		| if $r0.x.how == "ambiguous" then refuse("--root \($o.root) names several devices (\($r0.x.matches | join(", ")))")
		  elif $r0.x.how != "found" then refuse("--root \($o.root): no such device on this machine")
		  else
			fskey($r0.x.device) as $d | dev($d) as $dd
			| ($cands | map(select(.device == $d))) as $here
			| if (chain($d) | any(dev(.).fstype == "crypto_LUKS")) then refuse("--root \($d) is encrypted (LUKS): an encrypted root is not supported in this version")
			  elif $dd.fstype == "zfs_member" then refuse("--root \($d) is a member of a ZFS pool: a ZFS root is not supported")
			  elif ($here | any(.ram == true)) then ($here | map(select(.ram == true)) | .[0]) + {by: "option", known: true}
			  elif (($dd.fstype // "") | (extfs or . == "xfs" or . == "btrfs")) | not then refuse("--root \($d) holds \($dd.fstype // "no file system"): the root to replace is an ext4, xfs or btrfs file system")
			  else
				(if $r0.sub != null then ($here | map(select(.subvolume == $r0.sub)))
				   elif ($here | length) <= 1 then $here
				   else ($here | map(select(.consistent))) end) as $pick
				| if ($pick | length) == 1 then $pick[0] + {by: "option", known: true}
				  elif ($pick | length) > 1 or ($r0.sub == null and ($here | length) > 1) then
					refuse("--root \($d) holds several old systems (\($here | map(describe) | join(", "))); name the subvolume: --root \($d):SUBVOLUME")
				  elif $r0.sub != null and $r0.sub != "" and ((($f.fs[$d].subvolumes // []) | any(.path == $r0.sub)) | not) then
					refuse("--root \($o.root): \($d) has no subvolume \($r0.sub)")
				  elif $dd.fstype == "btrfs" and $r0.sub == null and (($f.fs[$d].subvolumes // []) | length) > 0 then
					refuse("--root \($d) is a btrfs with subvolumes and no old system at its top; name the subvolume: --root \($d):SUBVOLUME")
				  else {device: $d, subvolume: ($r0.sub // ""), system: "", hostname: "", resume: false, consistent: false, by: "option", known: false} end
			  end
		  end
	   else
		($cands | map(select(.consistent))) as $ok
		| if ($ok | length) == 1 then $ok[0] + {by: "detection", known: true}
		  elif ($ok | length) > 1 then error({refused: "several old systems are on this machine: \($ok | map(describe) | join(", ")); --root DEVICE names the one to replace", candidates: $cands})
		  elif ($cands | length) > 0 then
			# why none of them is the root: the root its fstab names is on several devices, or elsewhere
			([$cands[] | . as $c | rootspec($c) | select(. != null and .how == "ambiguous") | "\(.spec), which the /etc/fstab on \($c | describe) mounts at /, is on several devices here (\(.matches | join(", "))): a copy or a snapshot of a volume is among them"]) as $twice
			| if ($twice | length) > 0 then error({refused: "\($twice | join("; ")): which one is the old root is not certain; --root DEVICE names it\($hints)", candidates: $cands})
			  else error({refused: "an old system's files are on \($cands | map(describe) | join(", ")), but the /etc/fstab there does not mount it at /: the root is not identified; --root DEVICE names it\($hints)", candidates: $cands}) end
		  elif ($luks | length) > 0 then refuse("no old system is found (a file system with /etc/fstab and /etc/os-release), and \($luks | join(", ")) \(if ($luks | length) > 1 then "are LUKS containers" else "is a LUKS container" end), not opened here: an encrypted root is not supported in this version\($hints)")
		  elif ($zfsm | length) > 0 then refuse("no old system is found (a file system with /etc/fstab and /etc/os-release), and \($zfsm | join(", ")) \(if ($zfsm | length) > 1 then "are members of ZFS pools" else "is a member of a ZFS pool" end), not imported here: a ZFS root is not supported; --root DEVICE names a root on another file system\($hints)")
		  else refuse("no old system is found (a file system with /etc/fstab and /etc/os-release); --root DEVICE names the root to replace\($hints)") end
	   end) as $root
	| dev($root.device) as $rd
	| if (chain($root.device) | any(dev(.).fstype == "crypto_LUKS")) then refuse("the old root \($root.device) is encrypted (LUKS): an encrypted root is not supported in this version") else . end
	| if isro($root.device) then refuse("the old root \($root.device) is read-only (a write-protected disk, or blockdev --setro): nothing can be installed onto it") else . end
	| if $rd.usb then refuse("the old root \($root.device) is on a USB or removable device: the installer installs onto none") else . end
	| rootfiles($root) as $R
	| (($R.mdadm_conf // "") | mdconf) as $md
	| (($R.fstab // "") | fstab | to_entries | map(.value + {i: .key})) as $E
	| ($f.fs[$root.device] // {}) as $rfs
	| ($o.keep_root == true) as $keeproot
	| ([$devs[] | select(.fstype == "btrfs" and .uuid == $rd.uuid)] | length) as $rdevs
	| ($E | map(select(.mountpoint == "/"))) as $rootE
	| ($rd.fstype == "btrfs" and $root.subvolume != "") as $subroot
	| if $subroot and $root.known and ($root.resume | not) and (($rootE | length) != 1 or (($rootE[0].options | opt("subvol")) == null and ($rootE[0].options | opt("subvolid")) == null))
	  then refuse("the old root is the btrfs subvolume \($root.subvolume) of \($root.device), which its fstab does not name (the default subvolume): a layout this installer does not read with certainty") else . end
	| if $rd.fstype == "btrfs" and ($subroot | not) and ($keeproot | not) and $rdevs > 1
	  then refuse("the old root is a btrfs over \($rdevs) devices, at its top: it cannot be made anew on one of them; --keep-root keeps the file system") else . end
	# a second attempt: how far the interrupted one came (old: the old root is there; aside: moved aside whole; gone)
	| ((($R.saved // {}).root) // "") as $sroot
	| ((($R.saved // {}).aside) // "") as $saside
	| if $root.resume and $keeproot and $sroot == "gone" then refuse("the interrupted installation replaced the old root on \($root.device) already: --keep-root has nothing left to keep; run it without --keep-root") else . end
	# a device named as the kernel named it: an array the old mdadm.conf names is known by its UUID, the rest
	# by nothing but the name. When the root's own name does not lead to the root, the names here are not
	# the old system's
	| def kn: . as $s | ($s | kernelname) and ($md[$s] == null);
	  (if ($rootE | length) == 1 and ($rootE[0].spec | kn) and ($root.resume | not)
	   then (($rootE[0].spec | resolve($md; [])) as $x | $x.how != "found" or (samefs($x.device; $root.device) | not)) else false end) as $knbad
	| "named by a kernel name, and the kernel names here are not the old system's (its root's, \($rootE[0].spec // "?"), does not lead to the root)" as $knwhy
	# the root made anew: with the file system of the layout; else it stays what it is
	| (if $keeproot or $subroot then $rd.fstype else ($m.root_fs // "ext4") end) as $newfs
	# GRUB reads /boot on the root: a partition, a linear volume or an array, with an ext file system
	| ((($f.lvs // []) | map(select(.path == $root.device) | .segtype) | .[0]) // "") as $rseg
	| ((chain($root.device) | all(dev(.) as $a | $a.type == "part" or $a.type == "disk" or $a.type == "lvm" or ($a | isarray)))
	   and ($rseg == "" or $rseg == "linear" or $rseg == "striped") and (($newfs // "") | extfs)) as $grubroot

	# --- the old crypttab: what the new system opens again
	| (($R.crypttab // "") | crypttab | map(. as $c
		| ($c.source | resolve($md; [])) as $x
		| if $x.how == "ambiguous" then refuse("the old crypttab's \($c.source) (\($c.name)) names several devices here (\($x.matches | join(", ")))") else . end
		| (if $x.device != null then dev($x.device) else null end) as $d
		| ($c.options | opts) as $co
		| (if ($c.keyfile == "none" or $c.keyfile == "-" or $c.keyfile == "") then "passphrase"
		   elif ($c.keyfile | startswith("/dev/")) then "device"
		   elif (($R.keyfiles // []) | any(.path == $c.keyfile)) then (if ($R.opens // {})[$c.name] == false then "wrong" else "file" end)
		   else "elsewhere" end) as $key
		# made anew at every boot: systemd-cryptsetup runs mkswap or mkfs on it, whatever it holds
		| (if ($co | any(. == "swap")) then "swap" elif ($co | any(. == "tmp" or startswith("tmp="))) then "tmp"
		   elif ($c.keyfile | test("^/dev/(u?random|hwrng)$")) and (($d.fstype // "") != "crypto_LUKS") then "a random key" else "" end) as $vol
		| ($c.source | kn) as $k
		# a small file system in front (offset=), which names it: as its documentation has it
		| ($vol != "" and ($co | any(startswith("offset="))) and ($c.source | test("^(UUID|LABEL)=|^/dev/disk/by-(uuid|label)/"))) as $labelled
		| (if $x.how == "none" then null
		   elif $d == null then "\($c.source) is not there"
		   elif $k and $knbad then $knwhy
		   elif $vol != "" then
			(if $d.fstype != null and ($labelled | not) then "it is made anew at every boot (\($vol)), and \($d.path) holds a file system now (\(what($d.path))), which that would overwrite"
			 elif $d.usb then "it is made anew at every boot (\($vol)), and \($d.path) is on a USB or removable device"
			 elif $k and (($d.partuuid // "") == "") then "it is made anew at every boot (\($vol)), and \($c.source) is a kernel name, with no PARTUUID to name it by: the new system could overwrite another device"
			 else null end)
		   elif $d.fstype != "crypto_LUKS" and ($co | any(. == "plain" or . == "tcrypt" or . == "bitlk" or startswith("header=")) | not) then "\($d.path) holds \($d.fstype // "no file system"), not a LUKS container"
		   else null end) as $why
		| $c + {device: $x.device, keep: ($why == null), why: $why, key: $key, volatile: $vol, kn: ($k and $d != null),
			nofail: ($key != "file" and $key != "device" or $x.how == "none")}
		| if .keep and .kn then
			(if $d.fstype == "crypto_LUKS" and (($d.uuid // "") != "") then .source_new = "UUID=" + $d.uuid
			 elif ($d.partuuid // "") != "" then .source_new = "PARTUUID=" + $d.partuuid else . end)
		  else . end)) as $C

	# --- every entry of the old fstab
	| def unusedpath: test("^/(usr|bin|sbin|lib|lib32|lib64|libx32|etc|dev|proc|sys|run)(/|$)") and (test("^/usr/local(/|$)") | not);
	  def asidepath: . == "/var" or . == "/var/lib" or . == "/var/cache";
	  def classify:
		. as $e
		| ($e.spec | resolve($md; ($C | map(.name)))) as $x
		| ($e.spec | test("^(UUID|LABEL|PARTUUID|PARTLABEL)=|^/dev/")) as $isdev
		| (if $x.device != null then dev($x.device) else null end) as $d
		| (($e.spec | kn) and $d != null) as $k
		| def out($a; $n): $e + {device: $x.device, action: $a, note: $n} + (if $k then {kn: true} else {} end);
		  def crypt:
			($C | map(select(.name == $x.name)) | .[0]) as $c
			| if $c.keep then $e + {device: $c.device, action: "keep", crypt: $c.name, nofail: $c.nofail,
				note: ("encrypted: opened by the new system as " + $c.name + ", "
					+ (if $c.key == "file" then "with the key file " + $c.keyfile
					   elif $c.key == "device" then "with a key from " + $c.keyfile
					   elif $c.key == "elsewhere" then "with the key file " + $c.keyfile + ", which is not on the old root (nofail)"
					   elif $c.key == "wrong" then "with the key file " + $c.keyfile + ", which does not open it now (nofail)"
					   else "with a passphrase (nofail)" end))} + (if $c.volatile != "" then {volatile: $c.volatile} else {} end)
			  else $e + {device: null, action: "drop", note: ("its encrypted volume " + $c.name + " is dropped: " + $c.why)} end;
		  if $e.mountpoint == "/" then out("root"; "")
		  elif $x.how == "ambiguous" then refuse("the old fstab's \($e.spec) (\($e.mountpoint)) names several devices here (\($x.matches | join(", "))): which one is meant is not certain")
		  elif $e.type == "swap" then
			if ($isdev | not) then out("drop"; "a swap file of the old root")
			elif $x.how == "crypt" then crypt
			elif $d == null then out("drop"; "the device is not there")
			elif $k and $knbad then out("drop"; $knwhy)
			elif $d.fstype != "swap" then out("drop"; "\($d.path) holds \($d.fstype // "nothing"), not swap space")
			else out("swap"; "") end
		  elif ($isdev | not) then
			# no device of this machine: a file system of the kernel's own stays where it is; a bind mount or a
			# network file system where the new system has its own is mounted beside it, or not at all
			if ($e.type | pseudo) then out("text"; "")
			elif ($e.mountpoint | asidepath) then out("text"; "at \($e.mountpoint) in the old system: the new system's own is on the root") + {to: ($e.mountpoint + ".old")}
			elif ($e.mountpoint | unusedpath) then out("unused"; "")
			elif $e.type == "zfs" then out("text"; "a ZFS dataset: with nofail") + {nofail: true}
			elif ($e.type | network) or $e.type == "overlay" or ($e.spec | startswith("/")) or ($e.options | opts | any(. == "bind" or . == "rbind")) then out("text"; "")
			else out("text"; "\($e.type): with nofail, the new system may not know it") + {nofail: true, unsure: true} end
		  elif $x.how == "crypt" then crypt
		  elif $d == null then
			if ($C | any(.keep)) and ($e.spec | test("^(UUID|LABEL)=")) then
				$e + {device: null, action: "keep", nofail: true, note: "not found now, perhaps inside an encrypted volume: kept with nofail"}
			else out("drop"; "the device is not there") end
		  elif $k and $knbad then
			if $e.mountpoint == "/boot" then refuse("the old fstab names /boot by a kernel name (\($e.spec)), and the kernel names here are not the old system's (its root's, \($rootE[0].spec), does not lead to the root): what would be formatted as /boot is not certain; name /boot by UUID= in the old /etc/fstab (blkid shows it), or by LABEL=")
			else out("drop"; $knwhy) end
		  elif ($e.type | (extfs or . == "xfs" or . == "btrfs" or . == "vfat")) and $d.fstype != $e.type and ((($e.type | extfs) and (($d.fstype // "") | extfs)) | not) then
			out("drop"; "the old fstab says \($e.type), \($d.path) holds \($d.fstype // "no file system")")
		  elif samefs($d.path; $root.device) then
			if $d.fstype != "btrfs" then refuse("the old fstab mounts the root's own device \($d.path) at \($e.mountpoint) too: not a layout this installer reads with certainty")
			elif ($subroot | not) then refuse("the old root is the top of the btrfs \($root.device), and \($e.mountpoint) is a subvolume of the same file system: not supported in this version (the root as a subvolume of its own is)")
			else subvol_of($e.options; $root.device) as $sv
				| if $sv == $root.subvolume or ($sv | startswith($root.subvolume + "/")) or $sv == "" or $sv == "?" then
					refuse("the old fstab mounts \(if $sv == "" then "the top" else "the subvolume " + $sv end) of the root's btrfs at \($e.mountpoint): it holds or is the root subvolume \($root.subvolume), which is replaced; not a layout this installer reads with certainty")
				  elif $e.mountpoint == "/boot" then
					refuse("the old /boot is the subvolume \($sv) of the root's btrfs: the new /boot would be in the root subvolume, and the old one mounted over it; not supported in this version")
				  else out("keep"; "the subvolume \($sv) of the root's btrfs") end
			end
		  elif $e.mountpoint == "/boot" then (if $d.fstype == "vfat" and $m.efi then out("esp"; "") else out("boot"; "") end)
		  elif $e.mountpoint == "/boot/efi" or $e.mountpoint == "/efi" then
			if $d.fstype != "vfat" then refuse("the old fstab mounts \($d.path) (\($d.fstype // "no file system")) at \($e.mountpoint): not an EFI system partition")
			elif $m.efi then out("esp"; "") else out("keep"; "") end
		  elif ($e.mountpoint | unusedpath) then out("unused"; "")
		  elif ($e.mountpoint | asidepath) then out("aside"; "") + {to: ($e.mountpoint + ".old")}
		  elif (($d.fstype // "") | knownfs) | not then out("keep"; "\($d.fstype // "no file system the installer knows"): with nofail, the new system may not be able to mount it") + {nofail: true, unsure: true}
		  elif $d.usb then out("keep"; "on a USB or removable device: with nofail") + {nofail: true}
		  else out("keep"; "") end;
	  ($E | map(select(.mountpoint != "none" or .type == "swap") | classify)) as $E1
	# an EFI system partition named as the kernel named it must be one by its partition type too
	| ($E1 | map(if .action == "esp" and .kn == true and (dev(.device) | isesp | not) then .action = "drop" | .note = "named by a kernel name, and \(.device) is no EFI system partition by its type" else . end)) as $E1
	# one ESP: the first; a second one stays mounted where it was
	| ($E1 | map(select(.action == "esp")) | .[0]) as $espE
	| ($E1 | map(if .action == "esp" and .i != $espE.i then .action = "keep" else . end)) as $E2
	| ($E2 | map(select(.action == "boot"))) as $bootEs
	| if ($bootEs | length) > 1 then refuse("the old fstab mounts /boot twice") else . end
	# how many entries, the root among them, are on the file system of a device
	| def users($p): [$root.device, ($E2[] | select(.action != "root") | .device // empty)] | map(select(samefs(.; $p))) | length;
	  if ($bootEs | length) == 1 and users($bootEs[0].device) > 1
	  then refuse("the old /boot (\($bootEs[0].device)) is mounted elsewhere by the old fstab too: it cannot be formatted") else . end
	# a /boot too small for the new kernels is left alone, and /boot is on the root, where GRUB reads that
	| (($bootEs | length) == 1 and dev($bootEs[0].device).size_mb < 300) as $bootsmall
	| if $bootsmall and ($grubroot | not) then refuse("the old /boot (\($bootEs[0].device), \(dev($bootEs[0].device).size_mb) MB) is too small for the new system's kernels (300 MB at least), and /boot cannot be on the root instead (GRUB reads it on a partition, a linear volume or an md array with an ext4 file system; this root is \($newfs // "?") on \($root.device)\(if $rseg != "" then ", " + $rseg else "" end)): make a larger /boot in the rescue system first (keep-data changes no partition), or install without --keep-data") else . end
	| ($E2 | map(if .action == "boot" and $bootsmall then .action = "unused" | .note = "too small for the new system's kernels (\(dev(.device).size_mb) MB, 300 MB at least): /boot is on the root" else . end)) as $E2
	| ($E2 | map(select(.action == "boot")) | .[0]) as $bootE
	| (if ($bootEs | length) == 1 then $bootEs[0].device else null end) as $oldboot

	# --- --format: volumes of the old fstab that are formatted too
	| (($o.format // []) | map(if length > 1 then sub("/+$"; "") else . end) | unique) as $fmt
	| ($fmt | map(. as $mp
		| ($E2 | map(select(.mountpoint == $mp))) as $hit
		| if $mp == "/" then (if $keeproot then refuse("--format / and --keep-root contradict each other") else empty end)
		  elif ($hit | length) == 0 then refuse("--format \($mp): the old fstab has no such mountpoint")
		  elif ($hit | length) > 1 then refuse("--format \($mp): the old fstab has it \($hit | length) times")
		  else $hit[0] as $h
			| if $h.action == "boot" then empty
			  elif $h.action == "esp" then refuse("--format \($mp): the EFI system partition is never formatted (other systems' boot loaders may be on it)")
			  elif $h.action == "unused" then refuse("--format \($mp): the new system does not use it; it stays as it is")
			  elif $h.action == "drop" then refuse("--format \($mp): \($h.note)")
			  elif $h.action == "text" then refuse("--format \($mp): not a volume of this machine (\($h.spec), \($h.type))")
			  elif $h.crypt != null or $h.device == null then refuse("--format \($mp): it is encrypted, and the installer opens no encrypted volume")
			  elif $h.kn == true then refuse("--format \($mp): the old fstab names it by a kernel name (\($h.spec)), which may be another device here (\($h.device): \(what($h.device))): it is not formatted; name it by UUID= or LABEL= in the old /etc/fstab")
			  elif samefs($h.device; $root.device) then refuse("--format \($mp): \($h.device) is the root's own file system\(if $subroot then ", of which \($mp) is a subvolume" else "" end): it cannot be formatted")
			  elif users($h.device) > 1 then refuse("--format \($mp): \($h.device) is mounted elsewhere by the old fstab too (subvolumes): it cannot be formatted")
			  elif (dev($h.device).fstype | (extfs or . == "xfs" or . == "btrfs" or . == "vfat")) | not then refuse("--format \($mp): \($h.device) holds \(dev($h.device).fstype); the installer makes ext4 (of ext2 and ext3 too), xfs, btrfs and vfat")
			  elif ([$devs[] | select(.fstype == "btrfs" and .uuid == dev($h.device).uuid)] | length) > 1 then refuse("--format \($mp): a btrfs over several devices cannot be made anew on one")
			  elif isro($h.device) then refuse("--format \($mp): \($h.device) is read-only")
			  elif dev($h.device).usb then refuse("--format \($mp): \($h.device) is on a USB or removable device")
			  elif ($mp | test("\\\\")) then refuse("--format \($mp): a mountpoint with escaped characters is not supported")
			  else $h.i end
		  end)) as $fmti
	# a device the old system named as its kernel did is named by its UUID: the new kernel may name it otherwise;
	# a volume made anew has no subvolumes
	| ($E2 | map(if (.i as $i | $fmti | index($i)) != null then .action = "format" | .fs = (dev(.device).fstype | if extfs then "ext4" else . end) | del(.to) | .options = (.options | without_opt("subvol") | without_opt("subvolid")) else . end)
		| map(if .device != null and .crypt == null and (.spec | kernelname) and ((dev(.device).uuid // "") != "") then .spec_new = "UUID=" + dev(.device).uuid else . end)) as $E3
	# what an entry without a device needs of the old root: the source of a bind mount, the file of a loop
	# mount, the directories of an overlay. Where they are now: on a kept volume, as before; on one that
	# is set aside, under its new name; on the old root or a formatted volume, gone
	| def fate($s):
		([$E3[] | select(.action != "root" and .action != "swap" and .action != "drop" and .action != "unused" and (.mountpoint as $mp | $s == $mp or ($s | startswith($mp + "/"))))] | sort_by(.mountpoint | length) | last) as $h
		| if $h == null then {on: "root"}
		  elif $h.to != null then {on: "aside", path: ($h.to + $s[($h.mountpoint | length):])}
		  elif $h.action == "format" then {on: "format"}
		  else {on: "kept"} end;
	  ($E3 | map(
		if .action != "text" or (.type | pseudo) or (.type | network) then .
		elif .type == "overlay" then
			([((.options | opt("lowerdir")) // "" | split(":") | .[]), ((.options | opt("upperdir")) // empty), ((.options | opt("workdir")) // empty)] | map(select(startswith("/")))) as $dirs
			| ($dirs | map(select(fate(.).on != "kept"))) as $gone
			| if ($gone | length) > 0 then .action = "drop" | .note = "an overlay whose directories (\($gone | join(", "))) were on the old root, or on a volume that is set aside or formatted" else . end
		elif (.spec | test("^/[^/]") | not) then .
		elif (.options | opts | any(. == "bind" or . == "rbind")) then
			fate(.spec) as $w
			| if $w.on == "aside" then .spec_new = $w.path | .source = "aside" elif $w.on == "kept" then . else .source = $w.on end
		else
			fate(.spec) as $w
			| if $w.on == "aside" then .spec_new = $w.path | .source = "aside"
			  elif $w.on == "kept" then .
			  else .action = "drop" | .note = "its file \(.spec) was \(if $w.on == "root" then "on the old root" else "on a volume that is formatted" end)" end
		end)) as $E3
	| ($C | map(select(.keep and .volatile != ""))) as $Cvol

	# --- how the machine boots
	| (if $m.efi then
		(if $espE != null then {device: $espE.device, from: "fstab"}
		 else ([$devs[] | select(isesp and (.usb | not))]) as $esps
			| (disks_of($root.device)) as $rdisks
			| (($esps | map(select(.path as $p | disks_of($p) | any(. as $x | $rdisks | index($x) != null)))) + $esps | .[0]) as $e
			| if $e == null then refuse("this machine boots with UEFI and has no EFI system partition: it cannot be made bootable without repartitioning, which keep-data never does")
			  else {device: $e.path, from: "partition type"} end
		 end)
	   else null end) as $esp
	| if $m.efi and (chain($esp.device) | any(dev(.).fstype == "crypto_LUKS" or dev(.).type == "lvm")) then refuse("the EFI system partition \($esp.device) is not a plain partition") else . end
	| if $m.efi and isro($esp.device) then refuse("the EFI system partition \($esp.device) is read-only: the new boot loader cannot be put there") else . end
	| if $m.efi and ($E3 | any(.action == "format" and samefs(.device; $esp.device))) then refuse("--format \($E3 | map(select(.action == "format" and samefs(.device; $esp.device)) | .mountpoint) | join(", ")): \($esp.device) is the EFI system partition, which is never formatted (other systems' boot loaders may be on it)") else . end
	| if ($m.efi | not) and $m.arch != "amd64" then refuse("this machine does not boot with UEFI: on \($m.arch) the installer has no other way to make it bootable") else . end
	| (if $bootE != null then $bootE.device else $root.device end) as $bootdev
	| if $bootE != null and (chain($bootdev) | any(dev(.).fstype == "crypto_LUKS")) then refuse("the old /boot (\($bootdev)) is encrypted") else . end
	| if $bootE != null and isro($bootdev) then refuse("the old /boot (\($bootdev)) is read-only") else . end
	| if $bootE != null and dev($bootdev).usb then refuse("the old /boot (\($bootdev)) is on a USB or removable device") else . end
	# a /boot the old fstab named as the kernel did is the old /boot when it holds the old system's kernels
	| (($f.boot // {})[$bootdev] // {}) as $bootfacts
	| ((($bootfacts.kernels // []) | map(select(. as $v | ($R.modules // []) | index($v) != null)))) as $bootproof
	| if $bootE != null and $bootE.kn == true and $m.final == true and ($bootproof | length) == 0
	  then refuse("the old fstab names /boot by a kernel name (\($bootE.spec)), which is \($bootdev) here (\(what($bootdev))), and that does not hold the old system's kernels (\(if (($bootfacts.kernels // []) | length) > 0 then "its vmlinuz files are of " + ($bootfacts.kernels | join(", ")) else "no vmlinuz-VERSION is on it" end); the old root has \(if (($R.modules // []) | length) > 0 then "the modules of " + ($R.modules | join(", ")) else "no kernel modules to tell by" end)): it is not formatted; name /boot by UUID= in the old /etc/fstab (blkid shows it), or by LABEL=") else . end
	# GRUB's place on a disk under BIOS: "" when it has one, else what is wrong
	| def biosboot($p): ([$devs[] | select((.ancestors | index($p)) != null and ((.parttype // "") | ascii_downcase) == $biosguid)] | length) > 0;
	  def grubroom($p):
		if dev($p).pttype == "dos" then
			([$devs[] | select(.type == "part" and (.ancestors | index($p)) != null) | .start]) as $s
			| if ($s | length) == 0 or ($s | any(. == null)) then "where its partitions start is not known"
			  elif ($s | min) < 2048 then "its first partition starts at sector \($s | min), which leaves GRUB no room before it (2048 sectors, as partitioning tools have left since 2010)"
			  else "" end
		elif biosboot($p) then ""
		else "a GPT without a BIOS boot partition (or no partition table)" end;
	  (if $m.efi then {must: [], also: [], left: []} else
		(disks_of($bootdev)) as $bd
		| ($bd | map(select(grubroom(.) != ""))) as $bad
		| if ($bd | length) == 0 then refuse("the disk under \($bootdev) is not found: where the boot loader goes is not certain")
		  elif ($bad | length) > 0 then refuse("this machine boots with BIOS, and GRUB has no place on \($bad | map(. + " (" + grubroom(.) + ")") | join(", ")): keep-data changes no partition table, and the new system could not be started")
		  else
			# and every other disk that carries GRUB's boot code now: the BIOS may start any of them
			([$devs[] | select(.type == "disk" and (.usb | not) and $f.mbr[.path] == "grub") | .path | select(. as $p | ($bd | index($p)) == null)]) as $others
			| {must: $bd, also: ($others | map(select(grubroom(.) == ""))), left: ($others | map(select(grubroom(.) != "")))}
		  end
	   end) as $bios

	# --- the root: what becomes of it
	| ($f.usage // {}) as $U
	| (($m.unpacked_mb // 0)) as $need
	| if ($keeproot | not) and ($subroot | not) and $need > 0 and $rd.size_mb < $need * 2
	  then refuse("the old root \($root.device) (\($rd.size_mb) MB) is too small for \($m.os) (\($need) MB unpacked; twice that is needed): grow it in the rescue system first (a logical volume: lvextend -r -L \($need * 2)M \($root.device)), or install without --keep-data") else . end
	| if ($keeproot or $subroot) and $need > 0 and ($U.free_mb != null) and (($U.free_mb + (if $keeproot then 0 else ($U.tree_mb // 0) end)) < $need + 1024)
	  then refuse("the old root \($root.device) has \($U.free_mb) MB free: too little for \($m.os) beside what stays (\($need) MB unpacked and 1024 MB to spare are needed): free or grow it in the rescue system first\(if $keeproot then ", or run without --keep-root" else "" end)") else . end
	| if $keeproot and ($subroot | not) and ((($U.immutable // []) | length) > 0)
	  then refuse("--keep-root: \($U.immutable | join(", ")) at the top of the old root cannot be renamed (chattr +i or +a): the old system could not be moved aside whole; take the attribute off in the rescue system (mount \($root.device) /mnt; chattr -ia /mnt\($U.immutable[0]); umount /mnt) and run again") else . end
	| if $m.efi and ($f.esp[$esp.device] != null) and ($f.esp[$esp.device].error != null) then refuse("the EFI system partition \($esp.device) cannot be read: \($f.esp[$esp.device].error)") else . end
	| if $m.efi and ($f.esp[$esp.device] != null) and (($f.esp[$esp.device].free_mb // 0) < 16) then refuse("the EFI system partition \($esp.device) has \($f.esp[$esp.device].free_mb // 0) MB free: too little for the new boot loader (16 MB); remove what is not needed from it in the rescue system") else . end
	| (($f.esp[($esp.device // "")] // {}).loaders // []) as $loaders
	| (if $subroot then ($rfs.subvolumes // []) | map(.path | select(startswith($root.subvolume + "/"))) else [] end) as $nested
	# a btrfs made anew takes its subvolumes with it: those the system makes by itself go, any other is data
	| (if $rd.fstype == "btrfs" and ($subroot | not) and ($keeproot | not) then (($rfs.subvolumes // []) | map(.path)) else [] end) as $topsubs
	| ($topsubs | map(select(test("^var/lib/(machines|portables)$|^var/lib/(docker|containers/storage)/btrfs/subvolumes/") | not))) as $datasubs
	| if ($datasubs | length) > 0 then refuse("the old root is the top of the btrfs \($root.device), which holds the subvolumes \($datasubs | join(", ")): made anew, the file system would take them with it; --keep-root keeps the file system, and the old system moves into /old-root-DATE") else . end
	# the default subvolume cannot be deleted: the top is the default while the root subvolume is replaced
	| (($rfs.default // "") as $dflt | if $subroot and $dflt != "" and ($dflt == $root.subvolume or (($keeproot | not) and ($dflt | startswith($root.subvolume + "/")))) then {reset: true, restore: ($dflt == $root.subvolume)} else {reset: false, restore: false} end) as $default

	# --- the views of the plan
	| ([(if $keeproot then empty
	     elif $subroot then {device: $root.device, filesystem: "btrfs", mountpoint: "/", how: "subvolume", subvolume: $root.subvolume, size_mb: $rd.size_mb}
	     else {device: $root.device, filesystem: $newfs, mountpoint: "/", how: "mkfs", was: $rd.fstype, uuid: $rd.uuid, label: ($rd.label // ""), size_mb: $rd.size_mb} end),
	    (if $bootE != null then {device: $bootdev, filesystem: "ext4", mountpoint: "/boot", how: "mkfs", was: dev($bootdev).fstype, uuid: dev($bootdev).uuid, label: (dev($bootdev).label // ""), size_mb: dev($bootdev).size_mb} else empty end),
	    ($E3[] | select(.action == "format") | {device: .device, filesystem: .fs, mountpoint: .mountpoint, how: "mkfs", was: dev(.device).fstype, uuid: dev(.device).uuid, label: (dev(.device).label // ""), size_mb: dev(.device).size_mb})]) as $format
	| ([(if $esp != null then {device: $esp.device, filesystem: "vfat", mountpoint: "/boot/efi", uuid: (dev($esp.device).uuid // ""),
			note: ("the EFI system partition" + (if $esp.from != "fstab" then ", found by its partition type" elif $espE.mountpoint != "/boot/efi" then ", at \($espE.mountpoint) in the old system" else "" end)
				+ ": not formatted, the new boot loader is added"
				+ (if ($loaders | length) > 0 then "; the old system's (\($loaders | join(", "))) \(if ($loaders | length) > 1 then "move" else "moves" end) aside on it, with -old-DATE after the name" else "" end))} else empty end),
	    (if $keeproot and ($subroot | not) then {device: $root.device, filesystem: $rd.fstype, mountpoint: "/", uuid: ($rd.uuid // ""), note: "the root file system, not formatted: the new system is unpacked beside the old one, which moves into /old-root-DATE"} else empty end),
	    ($E3[] | select(.action == "swap") | {device: .device, filesystem: "swap", mountpoint: "swap", uuid: (dev(.device).uuid // ""), note: ""}),
	    ($E3[] | select((.action == "keep" or .action == "text") and .volatile == null)
			| {device: .device, filesystem: (if .device != null and (.crypt == null) then (dev(.device).fstype // .type) else .type end), mountpoint: (.to // .mountpoint)}
			+ (if .device != null then {uuid: (dev(.device).uuid // "")} else {} end)
			+ {note: (.note // "")}
			+ (if .action == "text" then {spec: (.spec_new // .spec)} else {} end))]) as $keep
	# what the new system makes anew at every boot, as the old one did: the encrypted swap and /tmp of crypttab
	| ([$Cvol[] | . as $c | {device: .device, filesystem: (if .volatile == "swap" then "swap" else "-" end), mountpoint: (([$E3[] | select(.crypt == $c.name) | .mountpoint] | .[0]) // "-"),
		name: .name, source: (.source_new // .source), options: .options, how: .volatile}]) as $volatile
	| ([($E3[] | select(.action == "aside") | {device: .device, filesystem: dev(.device).fstype, from: .mountpoint, mountpoint: .to, uuid: (dev(.device).uuid // ""), how: "mounted"}),
	    (if $keeproot and ($subroot | not) then {device: $root.device, filesystem: $rd.fstype, from: "/", mountpoint: "/old-root-DATE", how: "moved"}
	     elif $keeproot then {device: $root.device, filesystem: "btrfs", from: $root.subvolume, mountpoint: ($root.subvolume + "-old-DATE"), how: "renamed"} else empty end)]) as $aside
	| ([$E3[] | select(.action == "unused") | {device: .device, filesystem: (if .device != null then dev(.device).fstype else .type end), mountpoint: .mountpoint}
		+ (if .device != null then {uuid: (dev(.device).uuid // "")} else {spec: .spec} end) + (if (.note // "") != "" then {note: .note} else {} end)]) as $unused
	| ([$E3[] | select(.action == "drop") | {spec: .spec, mountpoint: .mountpoint, reason: .note}]) as $dropped
	| ([$root.device, ($esp.device // empty), ($E3[] | select(.action != "drop") | .device // empty), ($C[] | select(.keep) | .device // empty)] | map(fskey(.)) | unique) as $used
	| ([$devs[].ancestors[]] | unique) as $parents
	| ([$devs[] | select(.path as $p | ($parents | index($p)) == null and ($used | index(fskey($p))) == null)
		| select((.type == "disk" and .fstype == null and .pttype != null) | not)
		| . as $d
		| {device: .path, filesystem: (.fstype // ""), label: (.label // ""), size_mb: .size_mb,
		   note: (if .fstype == "zfs_member" then "a member of the ZFS pool \(.label // "?")"
			  elif .fstype == "crypto_LUKS" then "a LUKS container the old crypttab does not open"
			  elif .fstype == "LVM2_member" then "an LVM physical volume without volumes"
			  elif .fstype == "linux_raid_member" then "a member of an md array that is not assembled"
			  elif ((.parttype // "") | ascii_downcase) == $biosguid then "the BIOS boot partition"
			  elif .fstype == null then "no file system"
			  elif ($cands | any(.device == $d.path and (.consistent | not))) then "holds an operating system's files, whose fstab names another root"
			  elif ($cands | any(.device == $d.path)) then "holds another old system"
			  elif .usb then "on a USB or removable device"
			  else "" end)}]) as $untouched
	| (if $keeproot then [] else
		[($U.usage // [])[] | select(if (.path | test("^/(etc|root|home/[^/]+)$")) then true
			elif (.path | test("^/(var/|usr/local$|mnt$|media$)")) then .kb >= 65536 else .kb >= 1024 end)
		 | {path: .path, mb: ((.kb + 1023) / 1024 | floor)}] | sort_by(.path) end) as $lost
	| ([$devs[] | select(.fstype == "zfs_member")] | {members: map(.path), pools: (map(.label // "?") | unique)}) as $zfs
	| ($m.os | test("^ubuntu-")) as $ubuntu
	| ([$E3[] | select(.action == "keep" or .action == "text" or .action == "aside" or .action == "format")]) as $live
	| ((if ($C | any(.keep)) then ["cryptsetup"] else [] end)
	   + (if ($zfs.members | length) > 0 and $ubuntu then ["zfsutils-linux"] else [] end)
	   + (if ((($f.lvs // []) | map(select(.segtype | test("thin")) | .path)) as $thin | ([$root.device, $bootdev, ($live[] | .device // empty)] | any(. as $p | $thin | index($p) != null))) then ["thin-provisioning-tools"] else [] end)
	   + (if ($live | any(.type == "nfs" or .type == "nfs4")) then ["nfs-common"] else [] end)
	   + (if ($live | any(.type == "cifs" or .type == "smb3")) then ["cifs-utils"] else [] end)) as $packages
	# the volumes the home directories lie on: those the installer mounts for the users' section, and those
	# it cannot (encrypted, of the network, of ZFS, of a file system it does not know, read-only), which hide
	# what it makes on the root once the new system mounts them
	| ([$E3[] | select(.action == "keep" and .device != null and .crypt == null and .unsure != true and (.mountpoint | test("^/(home|root)(/|$)"))) | select(isro(.device) | not)]) as $homes
	| ([$E3[] | select((.action == "keep" or .action == "text") and ((.to // .mountpoint) | test("^/(home|root)(/|$)")) and (.i as $i | ($homes | map(.i) | index($i)) == null))]) as $hidden
	| ($ubuntu and ($zfs.members | length) > 0) as $zfson
	# the bind mounts whose source went with the old root or a formatted volume: an empty directory in its place
	| ([$E3[] | select(.action == "text" and (.source == "root" or .source == "format"))]) as $binds
	| ([$devs[] | select(isesp and .path != ($esp.device // ""))
		| select(.path as $p | ($E3 | any(.device == $p)) | not) | .path]) as $otheresp
	# a second attempt after an interrupted one: what the first plan kept, formatted or set aside and this one does not
	| def mounted: [(.format // [])[], (.keep // [])[], (.aside // [])[], (.volatile // [])[] | .mountpoint | select(. != "/" and (test("DATE$") | not))];
	  (if $root.resume and $R.first != null then
		(({format: $format, keep: $keep, aside: $aside, volatile: $volatile} | mounted) as $now
		 | reduce $now[] as $x ($R.first | mounted; index($x) as $i | if $i == null then . else del(.[$i]) end))
	   else [] end) as $missing
	| ($root.resume and (($missing | length) > 0 or $R.first == null)) as $shrunk

	| ([
		(if ($root.known | not) then "\($root.device) holds no old system (no /etc/fstab and /etc/os-release): it becomes the root because --root says so, and no other volume is mounted in the new system" else empty end),
		(if $root.known and ($root.consistent | not) and ($root.resume | not) then "the /etc/fstab on \($root.device) does not mount it at /: it is the root because --root says so" else empty end),
		(if $root.resume then "\($root.device) \(if $root.ram == true then "was being formatted by a keep-data installation that failed: the old system's fstab is the one this rescue system still has in memory" else "holds an interrupted keep-data installation: the old system's fstab is the one saved there" end)" else empty end),
		(if $shrunk then "this plan keeps less than the one the interrupted installation was confirmed with: \(if ($missing | length) > 0 then ($missing | join(", ")) + " \(if ($missing | length) > 1 then "are" else "is" end) not in it any more" else "that plan was not saved" end); what the old fstab named there is not found now. Look at it before you confirm" else empty end),
		(if $root.resume and ($keeproot | not) and ($subroot | not) and $saside != "" and $sroot != "gone" then "the interrupted installation was moving the old system into /\($saside) (--keep-root): without --keep-root it is lost with the root now" else empty end),
		(if $U.dirty == true then "the old root \($root.device) was not unmounted cleanly: it is read as the disk has it, without its journal; what the old system wrote last may be missing here" else empty end),
		(if $knbad then "the old fstab names its root as its kernel did (\($rootE[0].spec)), and that name does not lead to the root here: the kernel names of this rescue system are not the old system's, and every other entry named that way is dropped" else empty end),
		($cands[] | select(.device != $root.device or .subvolume != $root.subvolume)
			| if .device == $root.device and $saside != "" and .subvolume == $saside then "\(describe) is the old system, which the interrupted installation set aside: left as it is"
			  else "\(describe) holds an operating system's files too\(if .consistent then "" else ", a copy or snapshot whose fstab names another root" end): left as it is" end),
		($unseen[] | "\(.) (\(dev(.).fstype // "?")) could not be looked at (it cannot be mounted read-only): no old system was looked for on it"),
		($broken[] | select(. != $root.device) | "\(.) holds what an interrupted keep-data installation saved, damaged: it is not read"),
		($E3[] | select(.kn == true and .action != "drop" and .action != "root")
			| "\(.spec) (\(.mountpoint)) is named as the old system's kernel named it; here that is \(.device) (\(what(.device))): \(if .action == "boot" then "it holds the old system's kernels (\($bootproof | join(", "))), so it is the old /boot" elif .action == "esp" then "it is an EFI system partition by its type" else "taken for the volume meant, and named by its UUID from now on" end)"),
		($dropped[] | "\(.spec) (\(.mountpoint)) of the old fstab is dropped: \(.reason)"),
		($C[] | select(.keep | not) | "\(.name) of the old crypttab is dropped: \(.why)"),
		($C[] | select(.keep and .kn) | "\(.name) of the old crypttab names \(.source) as the old system's kernel did; here that is \(.device) (\(what(.device)))\(if .source_new != null then ": named \(.source_new) from now on" else "" end)"),
		($C[] | select(.keep and .nofail and .volatile == "") | "the encrypted volume \(.name) \(if .key == "passphrase" then "asks for a passphrase" elif .key == "wrong" then "is not opened by its key file (\(.keyfile))" elif .key == "elsewhere" then "has its key (\(.keyfile)) outside the old root" else "is a file (\(.source)), which may not be there at boot" end): nofail in crypttab and fstab, so that the new system boots without it; open it after the boot (systemctl start systemd-cryptsetup@\(.name))"),
		($Cvol[] | "\(.name) of the old crypttab (\(.source_new // .source), \(.volatile)) is made anew at every boot of the new system, as it was by the old one: whatever \(.device) holds then is overwritten"),
		(if ($nested | length) > 0 and ($keeproot | not) then "the subvolumes inside the root subvolume go with it: \($nested | join(", "))" else empty end),
		(if ($topsubs | length) > 0 then "the subvolumes of the old root's btrfs go with it, which is made anew: \($topsubs | join(", "))" else empty end),
		(if $default.reset and $keeproot then "the root subvolume \($root.subvolume) is the default subvolume of \($root.device): the new \($root.subvolume) becomes the default (the old one, renamed, would be mounted in its place)"
		 elif $default.reset then "the root subvolume \($root.subvolume) \(if $default.restore then "is" else "holds" end) the default subvolume of \($root.device), which cannot be deleted: the default is the top of the file system while the root subvolume is replaced\(if $default.restore then ", and the new \($root.subvolume) after" else ", and stays that" end)" else empty end),
		(if $bootsmall then "the old /boot (\($oldboot), \(dev($oldboot).size_mb) MB) is too small for the new system's kernels (300 MB at least): it is left as it is, and /boot is on the root" else empty end),
		(if ($zfs.members | length) > 0 then
			(if $ubuntu then "ZFS: \($zfs.members | join(", ")) (\($zfs.pools | join(", "))) \(if ($zfs.members | length) > 1 then "are" else "is" end) left as \(if ($zfs.members | length) > 1 then "they are" else "it is" end); zfsutils-linux is installed, and the pools are imported at boot\(if ($R.hostid // "") != "" then " (the old /etc/hostid is carried over)" else "" end)"
			 else "ZFS: \($zfs.members | join(", ")) (\($zfs.pools | join(", "))) \(if ($zfs.members | length) > 1 then "are" else "is" end) left as \(if ($zfs.members | length) > 1 then "they are" else "it is" end); \($m.os) has no ZFS packaged with its kernel: install it and import the pools yourself\(if ($R.hostid // "") != "" then " (the old /etc/hostid is carried over)" else "" end)" end)
		 else empty end),
		(if ($homes | length) > 0 then "\($homes | map(.mountpoint) | join(", ")) is kept: the profile's users get their home directories and keys there; a home directory that is there already goes to the user of its name (its files change owner where the user's ID differs), its authorized_keys is replaced (the old one saved beside it)" else empty end),
		($hidden[] | "\(.to // .mountpoint) is kept and not mounted by the installer (\(if .crypt != null then "encrypted" elif .device == null then .type else "a file system or device it does not write to" end)): the profile's users' home directories and keys are made on the root, where it hides them once it is mounted"),
		(if ($hidden | length) > 0 or $zfson then "the profile's users' keys are also in /etc/ssh/authorized_keys/NAME, which the new system's sshd reads besides (sshd_config.d/20-ump-keep-data.conf): they get in whatever \(if ($hidden | length) > 0 then ($hidden | map(.to // .mountpoint) | join(", ")) else "a ZFS dataset mounted over /home or /root" end) holds" else empty end),
		(if ($otheresp | length) > 0 then "other EFI system partitions (\($otheresp | join(", "))) are left as they are: a firmware that starts from one of them finds the old boot loader" else empty end),
		(if $esp != null and (($f.esp[$esp.device] // {}).fallback == true) then "EFI/BOOT on the EFI system partition holds a boot loader now, whichever system put it there: the new system's replaces it" else empty end),
		(if ($loaders | any(ascii_downcase == "efi/systemd")) then "the old system's systemd-boot moves aside with its entries (/loader on the EFI system partition, which another system that starts through it shares)" else empty end),
		($binds[] | "the source \(.spec) of the bind mount at \(.mountpoint) was \(if .source == "root" then "on the old root" else "on a volume that is formatted" end): the new system gets an empty directory there, so that the mount does not fail\(if .source == "root" and $keeproot then " (the old one is under /old-root-DATE)" else "" end)"),
		($E3[] | select(.action == "text" and .source == "aside") | "\(.spec) of the entry at \(.mountpoint) is on a volume that is set aside: the entry names \(.spec_new) from now on"),
		($E3[] | select(.action != "drop" and .unsure == true) | "\(.device // .spec) (\(.mountpoint)) is \(if .device != null then (dev(.device).fstype // "of no file system the installer knows") else .type end), which the new system may not be able to mount: kept with nofail, so that it boots without it"),
		($E3[] | select(.action == "text" and .type == "zfs" and ($zfson | not)) | "\(.spec) (\(.mountpoint)) is a ZFS dataset, and the new system has no ZFS: kept with nofail"),
		($E3[] | select(.action == "unused" and .device == null) | "\(.spec) (\(.mountpoint), \(.type)) of the old fstab is not mounted by the new system, whose own \(.mountpoint) is on the root"),
		($E3[] | select(.action == "text" and .to != null) | "\(.spec) (\(.mountpoint), \(.type)) of the old fstab is mounted at \(.to): the new system's own \(.mountpoint) is on the root"),
		(if ($live | any(.type == "nfs" or .type == "nfs4" or .type == "cifs" or .type == "smb3")) then "network file systems are kept as the old fstab has them; nfs-common or cifs-utils is installed for them on Ubuntu and Debian" else empty end),
		(if $keeproot and $bootE == null and (($U.features // "") != "") and ($m.os | test("^(ubuntu-2[02]\\.|debian-1[012]$)")) then "the root file system has the ext4 features \($U.features), which the GRUB of \($m.os) cannot read, and /boot is on it: the new system may not boot" else empty end),
		($bios.left[] | "\(.) carries GRUB's boot code now and has no place for the new one (\(grubroom(.))): it is left as it is, and a BIOS that starts from it finds the old boot loader"),
		(($f.lvs // [])[] | select(.active != "active") | "the logical volume \(.path) is not active here (\(.segtype)): what is on it is not seen")
	  ]) as $warnings

	# (the image is in the plan, and so in its hash: what is confirmed is this image on these disks)
	| {version: 1, os: $m.os, image: ($m.image // {version: "", sha256: ""}), firmware: (if $m.efi then "uefi" else "bios" end), arch: $m.arch,
	   options: {root: ($o.root // ""), keep_root: $keeproot, format: $fmt}}
	  + (if $root.resume then {resumed: true} else {} end) + (if $shrunk then {shrunk: true} else {} end)
	  + {old: {system: $root.system, hostname: $root.hostname, fstab_sha256: ($R.fstab_sha256 // "")},
	   root: ({device: $root.device, subvolume: $root.subvolume, filesystem: $newfs, was: $rd.fstype, uuid: ($rd.uuid // ""), size_mb: $rd.size_mb, by: $root.by}
		+ (if $root.known then {} else {known: false} end)
		+ (if ($keeproot or $subroot) and $U.free_mb != null then {free_mb: $U.free_mb} else {} end)),
	   candidates: $cands,
	   boot: {esp: ($esp.device // ""), boot: (if $bootE != null then $bootdev else "" end), bios_disks: ($bios.must + $bios.also), bios_optional: $bios.also,
		disk: ((disks_of($esp.device // $bootdev))[0] // ""), disks: ([$root.device, $bootdev, ($esp.device // empty)] | map(disks_of(.)[]) | unique)},
	   format: $format, keep: $keep, volatile: $volatile, aside: $aside, unused: $unused, untouched: $untouched, dropped: $dropped, lost: $lost,
	   crypttab: ($C | map(select(.keep) | {name, device, source: (.source_new // .source), keyfile, options: (if .nofail then (.options | with_opt("nofail")) else .options end)})),
	   packages: $packages, warnings: $warnings,
	   entries: ($E3 | map({spec, mountpoint, type, options: (.options | masked), device, action})),
	   private: {entries: $E3, crypttab: $C, keyfiles: ($R.keyfiles // []), hostid: ($R.hostid // ""), zpool_cache: ($R.zpool_cache // false),
		root_entry: (if ($keeproot or $subroot) and ($rootE | length) == 1 then $rootE[0] else null end),
		loaders: $loaders,
		# what a boot loader of the old system names its root or /boot by: the file system, the volume, the
		# array, or the partition's number on the disk it was started from (GRUB's "(,gpt3)/grub")
		loader_ids: ([$root.device, ($oldboot // $root.device)] | unique | map(. as $p
			| (dev($p).uuid // empty),
			  ((($f.lvs // []) | map(select(.path == $p) | .uuid) | .[0]) // empty | select(. != "")),
			  (dev($p).ancestors[] | dev(.) | select(.fstype == "linux_raid_member") | (.uuid // "") | norm_uuid | select(. != "") | "mduuid/" + .),
			  (if $esp != null and dev($p).type == "part" and (disks_of($p) == disks_of($esp.device)) then
				((dev($p).partn // ((dev($p).kname | capture("(?<n>[0-9]+)$").n) // empty)) | tostring) as $n
				| (if dev($p).pttype == "dos" then ",msdos" else ",gpt" end) + $n + ")"
			   else empty end)) | unique),
		binds: ($binds | map(.spec)), md: ([$devs[] | select(isarray)] | length > 0),
		zfs: $zfson, homes: ($homes | map(.i)), keys_outside: (($hidden | length) > 0 or $zfson),
		default: $default, saved: {root: $sroot, aside: $saside}, ram: ($root.ram == true),
		labels: ($format | map(select(.how == "mkfs") | . as $r | {key: $r.device, value: (($r.label // "") | fits($r.filesystem))}) | from_entries),
		# the old fstab as a second attempt reads it: a device named as the kernel named it, and one that is
		# formatted (whose label may go), by the UUID it has now
		resolved: ([$E3[] | select(.device != null and .crypt == null and ((dev(.device).uuid // "") != "") and ((.spec | kernelname) or .action == "boot" or .action == "format"))
			| {spec, mountpoint, to: ("UUID=" + dev(.device).uuid)}]),
		rw: ([$format[].device, $root.device, $bootdev, ($esp.device // empty), ($homes[] | .device)] | map(chain(.)[]) | map(select(dev(.) | isarray)) | unique)}};
# the plan as people read it, a line each
def text:
	def pad($n): . + ([range(0; $n - length)] | map(" ") | join(""));
	def fsname: .filesystem | if . == null or . == "" then "-" else . end;
	([.format[], .keep[], .volatile[], .aside[], .unused[], .untouched[] | (.device // .spec // "-") | length] | max // 0) as $w
	| ([.format[], .keep[], .volatile[], .aside[], .unused[], .untouched[] | fsname | length] | max // 0) as $v
	| def row: "  \((.device // .spec // "-") | pad($w))  \(fsname | pad($v))  \(.mountpoint)";
	  def note: if (.note // "") != "" then "  (\(.note))" else "" end;
	"keep-data: the old system\(if .old.system != "" then " " + .old.system else "" end)\(if .old.hostname != "" then " (" + .old.hostname + ")" else "" end) on \(.root.device)\(if .root.subvolume != "" then ", subvolume " + .root.subvolume else "" end)\(if .root.by == "option" then " (--root)" else "" end)\(if .resumed == true then ", after an interrupted installation" else "" end); \(.firmware | ascii_upcase) firmware; the layout of the profile is not applied, no disk is wiped or partitioned",
	"FORMAT (what is on them is lost)",
	(.format[] | row + (if .how == "subvolume" then "  (the subvolume \(.subvolume) is deleted and made anew; the rest of the file system stays)"
		else "  (\(.size_mb | size)\(if (.was // "") != "" and .was != .filesystem then ", was " + .was else "" end)\(if (.label // "") != "" then ", label " + .label else "" end))" end)),
	(if (.format | length) == 0 then "  nothing" else empty end),
	"KEEP (as they are, mounted where they were)",
	(.keep[] | row + note),
	(if (.keep | length) == 0 then "  nothing" else empty end),
	(if (.volatile | length) > 0 then "MADE ANEW AT EVERY BOOT (crypttab: what is on them is overwritten each time the new system starts, as the old one did)",
		(.volatile[] | row + "  (\(.name): \(.source), \(.options))") else empty end),
	(if (.aside | length) > 0 then "ASIDE (kept, under another name)", (.aside[] | "  \(.device | pad($w))  \(fsname | pad($v))  \(.from) -> \(.mountpoint)  (\(.how))") else empty end),
	(if (.unused | length) > 0 then "UNUSED (left as they are, not mounted by the new system)", (.unused[] | row + note) else empty end),
	(if (.untouched | length) > 0 then "UNTOUCHED (not in the old fstab: left as they are)", (.untouched[] | "  \(.device | pad($w))  \(fsname | pad($v))  \(.size_mb | size)\(if .label != "" and .filesystem != "zfs_member" then "  " + .label else "" end)" + note) else empty end),
	(if .options.keep_root then empty else "LOST (on the old root, beyond the system itself)", (.lost[] | "  \(.path)  \(.mb | size)"), (if (.lost | length) == 0 then "  nothing found" else empty end) end),
	(if (.boot.bios_disks | length) > 0 then "BOOT LOADER (BIOS): \(.boot.bios_disks - .boot.bios_optional | join(", "))\(if (.boot.bios_optional | length) > 0 then "; and \(.boot.bios_optional | join(", ")), which \(if (.boot.bios_optional | length) > 1 then "carry" else "carries" end) GRUB now (tried: a failure there is a warning)" else "" end)" else empty end),
	(if (.packages | length) > 0 then "PACKAGES (installed for what is kept): \(.packages | join(", "))" else empty end),
	(if (.warnings | length) > 0 then "WARNINGS", (.warnings[] | "  " + .) else empty end);
JQ

# kd_jq FILTER: the plan's program over stdin
kd_jq() { local f="$1"; shift; jq -c "$@" "$KD_JQ $f"; }

# kd_refused WHY [JSON]: the refusal as the plan's JSON; ends kd_build with 9
kd_refused() {
	local more="${2:-}"
	[ -n "$more" ] || more='{}'
	jq -c --arg why "$1" '{version: 1, refused: $why} + .' <<< "$more"
	exit 9
}

# kd_build: the plan as JSON, with what only the installer needs under
# private and its hash (sha256 of the rest, keys sorted); status 9 and
# {refused: why} when the installer refuses the machine. Reads only.
kd_build() {
	local lsblk fs='{}' roots='{}' links='{}' mbr='{}' esp='{}' boot='{}' usage=null lvs facts plan dev fstype info rinfo sub key path disk root rsub rfs ids e pass ram rdev=""
	lsblk="$(kd_fact_lsblk)"
	jq -e '.blockdevices | length > 0' >/dev/null <<< "$lsblk"
	# an installation that failed here while it formatted the root: what it held of the old system is in memory
	ram="$(kd_fact_ram)"
	if [ -n "$ram" ]; then
		rdev="$(jq -r .device <<< "$ram")"
		rinfo="$(kd_fact_root_ram)"
		fs="$(jq -c '{(.device): {roots: [{subvolume, system, hostname, resume: true, ram: true}], subvolumes: [], default: ""}}' <<< "$ram")"
		roots="$(printf '%s\n' "$ram" "$rinfo" | jq -c -s --arg sum "$(jq -j '.fstab // ""' <<< "$rinfo" | sha256sum | cut -d' ' -f1)" '{(.[0].device + "\t" + .[0].subvolume): (.[1] + {fstab_sha256: $sum})}')"
	fi
	while IFS=$'\t' read -r dev fstype; do
		[ -n "$dev" ] && [ "$dev" != "$rdev" ] || continue
		info="$(kd_fact_fs "$dev" "$fstype")"
		fs="$(printf '%s\n' "$fs" "$info" | jq -c -s --arg d "$dev" '.[0] + {($d): .[1]}')"
		while IFS= read -r sub; do
			sub="${sub#=}"
			rinfo="$(kd_fact_root "$dev" "$sub" "$fstype")"
			roots="$(printf '%s\n' "$roots" "$rinfo" | jq -c -s --arg k "$dev"$'\t'"$sub" --arg sum "$(jq -j '.fstab // ""' <<< "$rinfo" | sha256sum | cut -d' ' -f1)" '.[0] + {($k): (.[1] + {fstab_sha256: $sum})}')"
		done < <(jq -r '(.roots // [])[] | "=" + (.subvolume // "")' <<< "$info")
	done < <(kd_jq 'devices | filesystems | map(select(.mountpoints | length == 0)) | .[] | [.path, .fstype] | @tsv' -r <<< "$lsblk")
	if [ "$efi" = 0 ]; then
		while IFS= read -r disk; do
			[ -n "$disk" ] || continue
			mbr="$(jq -c --arg d "$disk" --arg v "$(kd_fact_mbr "$disk")" '.[$d] = $v' <<< "$mbr")"
		done < <(kd_jq 'devices | .[] | select(.type == "disk") | .path' -r <<< "$lsblk")
	fi
	lvs="$(kd_fact_lvs | jq -R -s -c 'split("\n") | map(select(. != "") | split("|") | {path: .[0], segtype: (.[1] // ""), active: (.[2] // ""), uuid: (.[3] // "")})')"
	for pass in 1 2; do
		# what the fields of the old fstab and crypttab under /dev lead to here
		while IFS= read -r path; do
			[ -n "$path" ] || continue
			jq -e --arg s "$path" 'has($s)' >/dev/null <<< "$links" || links="$(jq -c --arg s "$path" --arg p "$(kd_fact_realpath "$path")" '.[$s] = $p' <<< "$links")"
		done < <({ jq -r '.[] | (.fstab // ""), (.crypttab // "")' <<< "$roots" | awk '$1 !~ /^#/ {print $1; print $2}'; printf '%s\n' "$keep_root_arg" "${keep_root_arg%:*}"; } | grep '^/dev/' | sort -u || true)
		facts="$(printf '%s\n' "$lsblk" "$fs" "$roots" "$links" "$mbr" "$esp" "$usage" "$lvs" "$boot" | jq -c -s \
			--argjson efi "$([ "$efi" = 1 ] && echo true || echo false)" --arg arch "$arch" --arg os "$os" --argjson final "$([ "$pass" = 2 ] && echo true || echo false)" \
			--arg image_version "${image_version:-}" --arg image_sum "${want_sum:-}" \
			--argjson unpacked "${unpacked_mb:-0}" --arg root_fs "$(kd_root_fs)" --arg root "$keep_root_arg" --argjson keep_root "$([ "$keep_root" = 1 ] && echo true || echo false)" \
			--argjson format "$(printf '%s\n' "${keep_format[@]:-}" | jq -R -s -c 'split("\n") | map(select(. != ""))')" \
			'{machine: {efi: $efi, arch: $arch, os: $os, image: {version: $image_version, sha256: $image_sum}, unpacked_mb: $unpacked, root_fs: $root_fs, final: $final}, options: {root: $root, keep_root: $keep_root, format: $format},
			  lsblk: .[0], fs: .[1], roots: .[2], links: .[3], mbr: .[4], esp: .[5], usage: .[6], lvs: .[7], boot: .[8]}')"
		plan="$(kd_jq 'try plan catch (if type == "object" and has("refused") then . else error end)' <<< "$facts")"
		if jq -e 'has("refused")' >/dev/null <<< "$plan"; then
			kd_refused "$(jq -r .refused <<< "$plan")" "$(jq -c 'del(.refused)' <<< "$plan")"
		fi
		[ "$pass" = 1 ] || break
		# the root is known: what it holds, what a separate /boot holds, and what is on the ESP
		root="$(jq -r .root.device <<< "$plan")"; rsub="$(jq -r .root.subvolume <<< "$plan")"; rfs="$(jq -r '.root.was // ""' <<< "$plan")"
		key="$root"$'\t'"$rsub"
		if ! jq -e --arg k "$key" 'has($k)' >/dev/null <<< "$roots"; then
			info="$(kd_fact_root "$root" "$rsub" "$rfs")"
			roots="$(printf '%s\n' "$roots" "$info" | jq -c -s --arg k "$key" --arg sum "$(jq -j '.fstab // ""' <<< "$info" | sha256sum | cut -d' ' -f1)" '.[0] + {($k): (.[1] + {fstab_sha256: $sum})}')"
		fi
		usage="$(kd_fact_usage "$root" "$rsub" "$rfs" "$([ "$keep_root" = 1 ] && echo 0 || echo 1)")"
		e="$(jq -r .boot.boot <<< "$plan")"
		if [ -n "$e" ]; then
			boot="$(kd_fact_boot "$e" "$(jq -r --arg d "$e" '[.format[] | select(.device == $d) | .was // ""][0] // ""' <<< "$plan")" | jq -c --arg d "$e" '{($d): .}')"
		fi
		e="$(jq -r .boot.esp <<< "$plan")"
		if [ -n "$e" ]; then
			mapfile -t ids < <(jq -r '.private.loader_ids[]' <<< "$plan")
			esp="$(kd_fact_esp "$e" "${ids[@]}" | jq -c --arg d "$e" '{($d): .}')"
		fi
	done
	jq -c --arg hash "$(jq -cS 'del(.private)' <<< "$plan" | sha256sum | cut -d' ' -f1)" '. + {hash: $hash}' <<< "$plan"
}

# kd_root_fs: the file system of a root made anew: the one the profile's
# layout gives /, where that is one the installer makes; else ext4
kd_root_fs() {
	jq -r '[(.layout.parts // [])[] | select(.mountpoint == "/") | .filesystem] | if length == 1 and (.[0] == "ext4" or .[0] == "xfs" or .[0] == "btrfs") then .[0] else "ext4" end' <<< "$spec"
}

# kd_public: the plan as others get it (--plan, the record), without what
# only the installer needs
kd_public() { jq 'del(.private)' <<< "$kd_plan"; }

# kd_text: the plan as people read it
kd_text() { kd_jq 'text' -r <<< "$kd_plan"; }

# kd_fact_uuid DEV: the UUID of what is on a device now
kd_fact_uuid() { blkid -s UUID -o value "$1" 2>/dev/null || true; }

# kd_new_uuid FS: a UUID for a file system that is made anew, as blkid
# will show it: known before mkfs runs, so that what is saved for a
# second attempt can name the new file system
kd_new_uuid() {
	local h
	case "$1" in
	vfat) h="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n' | tr a-f A-F)"; printf '%s-%s\n' "${h:0:4}" "${h:4:4}" ;;
	*) cat /proc/sys/kernel/random/uuid ;;
	esac
}

# kd_cleanup: nothing of the old system stays mounted, and no key stays in
# memory, unless memory is the only place the old system's files are in
# (an installation that failed while it formatted the root: the next run
# in this rescue system reads them there)
kd_cleanup() {
	local m
	for m in "$KD_LOOK" "$KD_DIR/top"; do
		if mountpoint -q "$m" 2>/dev/null; then umount "$m" 2>/dev/null || umount -l "$m" 2>/dev/null || true; fi
	done
	if [ -s "$KD_DIR/read-only" ]; then blockdev --setrw "$(cat "$KD_DIR/read-only")" 2>/dev/null || true; rm -f "$KD_DIR/read-only"; fi
	[ -e "$KD_DIR/unsaved" ] || rm -rf "${KD_DIR:?}/old"
	return 0
}

# kd_exit CLEANUP STATUS: the end of a keep-data run, whatever ended it:
# 0 is the new system installed, 2 nothing written (a refusal, or a
# failure before the first write), 1 a failure after writing began
kd_exit() {
	trap - EXIT
	set +e
	"$1"
	[ "$2" != 0 ] || exit 0
	[ "$kd_wrote" = 1 ] || exit 2
	exit 1
}

# kd_detect: the plan into kd_plan; a refusal ends the run with status 2,
# after the plan file (--plan) got it
kd_detect() {
	local rc
	mkdir -p "$KD_DIR"; chmod 700 "$KD_DIR"
	log "keep-data: looking for the old system (read-only: nothing is written)"
	udevadm settle 2>/dev/null || true
	# arrays and volume groups the rescue system did not bring up by
	# itself: arrays read-only, so that nothing is synchronised or marked
	mdadm --assemble --scan --readonly >/dev/null 2>&1 || true
	udevadm settle 2>/dev/null || true
	vgchange -ay >/dev/null 2>&1 || true
	udevadm settle 2>/dev/null || true
	# (a refusal is no failure for the trap that reports failed commands)
	dying=1; set +e; ( set -e; kd_build ) > "$KD_DIR/plan.json"; rc=$?; set -e; dying=""
	kd_cleanup
	case "$rc" in
	0) kd_plan="$(cat "$KD_DIR/plan.json")" ;;
	9)
		kd_plan="$(cat "$KD_DIR/plan.json")"
		[ -n "$(jq -r '.refused // empty' <<< "$kd_plan" 2>/dev/null)" ] || die "keep-data: the machine's storage could not be read"
		if [ -n "$plan_file" ]; then kd_public > "$plan_file"; fi
		echo "install.sh: keep-data: refused: $(jq -r .refused <<< "$kd_plan")" >&2
		echo "install.sh: keep-data: nothing was written to the disks" >&2
		exit 2 ;;
	2) exit 2 ;;
	*) die "keep-data: the machine's storage could not be read (status $rc)" ;;
	esac
}

# kd_confirmed: the plan is the one that was confirmed, or nothing is written
kd_confirmed() {
	local hash
	hash="$(jq -r .hash <<< "$kd_plan")"
	if [ "$confirm" != "$hash" ]; then
		echo "install.sh: keep-data: refused: the plan (above, $hash) is not the one that was confirmed ($confirm): the disks, the options or the image on the storage changed since; look at it again and confirm it" >&2
		echo "install.sh: keep-data: nothing was written to the disks" >&2
		exit 2
	fi
}

# kd_may_format DEV FS: format()'s guard in keep mode: the device is one
# the confirmed plan formats, and still holds what the plan saw there;
# each once
kd_may_format() {
	local e u i
	case "$2" in ext4|xfs|btrfs|vfat) ;; *) die "keep-data: $1 as $2: not a file system keep-data makes" ;; esac
	for i in "${!KD_FORMAT[@]}"; do
		e="${KD_FORMAT[$i]}"
		[ "${e%% *}" = "$1" ] || continue
		u="$(kd_fact_uuid "$1")"
		[ "$u" = "${e#* }" ] || die "keep-data: $1 is not what the confirmed plan saw (UUID ${e#* } then, ${u:-none} now): not formatted"
		unset "KD_FORMAT[$i]"
		return 0
	done
	die "keep-data: $1 is not in the confirmed plan: not formatted"
}

# kd_check_write: once more, before the first write, what the plan's
# rules already say: no device is formatted twice, and none that the
# root (kept, or a subvolume), /boot or the EFI system partition is on
# beside its own row
kd_check_write() {
	local bad
	bad="$(jq -r '([.format[] | select(.how == "mkfs") | .device] | group_by(.) | map(select(length > 1) | .[0])) as $twice
		| ([.format[] | select(.how == "mkfs" and .mountpoint != "/") | .device]) as $others
		| ([(.root.device), (.boot.esp | select(. != ""))] | map(select(. as $d | $others | index($d) != null))) as $own
		| ($twice + $own) | unique | join(", ")' <<< "$kd_plan")"
	[ -z "$bad" ] || die "keep-data: the plan formats $bad twice, or under the root or the EFI system partition: nothing is written (a fault of the installer: report it)"
}

# kd_save_old: what the new system takes over from the old root, into
# memory before the root goes ($KD_DIR/old, as it is then saved on the
# root's file system for a second attempt): its fstab, with the devices
# the plan found for kernel names and for what it formats named by their
# UUIDs, its crypttab and the key files crypttab names, the host ID and
# the pool cache of ZFS, the plan that was confirmed, and the state: the
# root device and subvolume, the date, how far the installation came. A
# second attempt keeps what the first saved, its plan included
kd_save_old() {
	local dev sub fs o="" b old="$KD_DIR/old" f k t
	dev="$(jq -r .root.device <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"; fs="$(jq -r '.root.was // ""' <<< "$kd_plan")"
	if [ "$(jq -r .private.ram <<< "$kd_plan")" = true ]; then
		kd_marker_ok "$old" || die "keep-data: what this rescue system held of the old system in memory is gone"
		return 0
	fi
	[ "$fs" != btrfs ] || o=subvolid=5
	rm -rf "${KD_DIR:?}/old" "${KD_DIR:?}/unsaved"; mkdir -p "$old/etc"; chmod 700 "$old"
	kd_look "$dev" "$fs" "$o" || die "keep-data: the old root $dev cannot be mounted"
	if [ "$(jq -r '.resumed // false' <<< "$kd_plan")" = true ]; then
		kd_marker_ok "$KD_LOOK/$KD_MARK" || die "keep-data: what the interrupted installation saved on $dev is gone or damaged"
		cp -a "$KD_LOOK/$KD_MARK/." "$old/"
		kd_unlook
		return 0
	fi
	b="$KD_LOOK${sub:+/$sub}"
	for f in etc/fstab etc/crypttab etc/mdadm/mdadm.conf etc/mdadm.conf etc/hostid etc/hostname etc/zfs/zpool.cache; do
		if [ -f "$b/$f" ] && [ ! -L "$b/$f" ]; then mkdir -p "$old/$(dirname "$f")"; cp -a "$b/$f" "$old/$f"; fi
	done
	[ -f "$old/etc/fstab" ] || : > "$old/etc/fstab"
	for f in usr/lib/os-release etc/os-release; do
		if [ -f "$b/$f" ] && [ ! -L "$b/$f" ]; then cp "$b/$f" "$old/etc/os-release"; break; fi
	done
	while IFS= read -r k; do
		[ -n "$k" ] || continue
		[ -f "$b$k" ] && [ ! -L "$b$k" ] || die "keep-data: the key file $k of the old crypttab is gone"
		mkdir -p "$old$(dirname "$k")"
		cp -a "$b$k" "$old$k"
	done < <(jq -r '.private.keyfiles[].path' <<< "$kd_plan")
	kd_unlook
	# the fstab as a second attempt reads it
	while IFS=$'\x1f' read -r f k t; do
		[ -n "$f" ] || continue
		KD_S="$f" KD_M="$k" KD_T="$t" awk '{x = $2; sub(/\/+$/, "", x)} $1 == ENVIRON["KD_S"] && (x == ENVIRON["KD_M"] || $2 == ENVIRON["KD_M"]) && !/^[ \t]*#/ {$1 = ENVIRON["KD_T"]} {print}' "$old/etc/fstab" > "$old/etc/fstab.new"
		mv "$old/etc/fstab.new" "$old/etc/fstab"
	done < <(jq -r '.private.resolved[] | [.spec, .mountpoint, .to] | join("\u001f")' <<< "$kd_plan")
	kd_public > "$old/plan.json"
	printf 'device=%s\nsubvolume=%s\nstamp=%s\nroot=old\naside=\nuuid_old=%s\nuuid_new=\n' "$dev" "$sub" "$KD_STAMP" "$(jq -r '.root.uuid // ""' <<< "$kd_plan")" > "$old/state"
	(cd "$old" && sha256sum etc/fstab plan.json > SHA256SUMS)
}

# kd_state_set DIR KEY VALUE...: new values in a state, the file replaced
# whole, and on the disk before the step it announces
kd_state_set() {
	local d="$1"; shift
	cp "$d/state" "$d/state.new"
	while [ $# -ge 2 ]; do
		{ grep -v "^$1=" "$d/state.new" || true; printf '%s=%s\n' "$1" "$2"; } > "$d/state.tmp"
		mv "$d/state.tmp" "$d/state.new"; shift 2
	done
	sync "$d/state.new" 2>/dev/null || true
	mv "$d/state.new" "$d/state"
	sync -f "$d" 2>/dev/null || sync
}

# kd_marker_write TOP: what kd_save_old holds, onto the root's file
# system mounted at TOP: under another name first, on the disk, then
# under its own. From then on a second attempt finds the old system's
# files there, and the copy in memory is no longer the only one
kd_marker_write() {
	local m="${1:?}/$KD_MARK"
	if [ -d "$m" ] && [ ! -L "$m" ] && kd_marker_ok "$m"; then
		# a second attempt: what the first saved stays, with the state of this one
		cp "$KD_DIR/old/state" "$m/state.new" || die "keep-data: the state cannot be saved on the root ($m)"
		sync "$m/state.new" 2>/dev/null || true
		mv "$m/state.new" "$m/state"
	else
		rm -rf "$m" "$m.new"
		mkdir -m 700 "$m.new" || die "keep-data: the old system's files cannot be saved on the root ($m)"
		cp -a "$KD_DIR/old/." "$m.new/" || die "keep-data: the old system's files cannot be saved on the root ($m)"
		sync -f "$m.new" 2>/dev/null || sync
		mv "$m.new" "$m" || die "keep-data: the old system's files cannot be saved on the root ($m)"
	fi
	sync -f "$1" 2>/dev/null || sync
	kd_marker_ok "$m" || die "keep-data: what was saved of the old system on the root ($m) does not read back"
	rm -f "$KD_DIR/unsaved"
}

# kd_write: the storage of the confirmed plan: the root made anew (a file
# system, or its btrfs subvolume) or its old system moved aside, /boot
# and what --format names formatted. Nothing else is written to: no
# partition table, no array, no volume group. Before the first step that
# destroys, the old system's files are saved for a second attempt at the
# top of the root's file system ($KD_MARK); where the root itself is
# formatted, they are in memory until the new file system has them
# ($KD_DIR/unsaved says so, and the next run in this rescue system reads
# them there)
kd_write() {
	local dev fs mp sub top="$KD_DIR/top" e p old="$KD_DIR/old" was new first aside sroot id now dflt
	KD_STAMP="$(date -u +%Y%m%d-%H%M%S)"; now="$KD_STAMP"
	# what format() may make anew: the plan's devices, each with the UUID it had
	mapfile -t KD_FORMAT < <(jq -r '.format[] | select(.how == "mkfs") | "\(.device) \(.uuid // "")"' <<< "$kd_plan")
	kd_check_write
	kd_save_old
	# a second attempt: how far the first one came, and the name it gave what it set aside
	sroot="$(kd_state "$old" root)"; aside="$(kd_state "$old" aside)"
	dev="$(jq -r .root.device <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"; fs="$(jq -r '.root.was // ""' <<< "$kd_plan")"
	# the default subvolume that the new root subvolume is to be: this plan's word, or the first attempt's
	dflt="$(kd_state "$old" default)"; [ "$(jq -r .private.default.restore <<< "$kd_plan")" != true ] || dflt=restore
	log "keep-data: writing begins"
	kd_wrote=1
	for p in $(jq -r '.private.rw[]' <<< "$kd_plan"); do
		mdadm --readwrite "$p" >/dev/null 2>&1 || true
		[ "$(blockdev --getro "$p" 2>/dev/null)" != 1 ] || die "keep-data: the array $p stays read-only (mdadm --readwrite $p)"
	done
	mkdir -p "$top"
	if [ "$fs" = btrfs ] && [ -n "$sub" ]; then
		mount -t btrfs -o subvolid=5 "$dev" "$top" || die "keep-data: $dev cannot be mounted"
		if [ "$keep_root" = 1 ]; then
			if [ "$sroot" = aside ] && [ -n "$aside" ] && [ -d "$top/$aside" ]; then
				# a second attempt: the old root is aside already; what the first made in its place goes aside too
				KD_STAMP="${aside##*-old-}"
				kd_marker_write "$top"
				if [ -d "$top/$sub" ]; then
					log "the subvolume $sub of the interrupted installation is now $sub-failed-$now (it can be deleted)"
					mv "$top/$sub" "$top/$sub-failed-$now" || die "keep-data: the subvolume $sub cannot be renamed"
				fi
			else
				[ -d "$top/$sub" ] || die "keep-data: $dev has no subvolume $sub any more"
				kd_state_set "$old" stamp "$KD_STAMP" root old aside "$sub-old-$KD_STAMP" default "$dflt"
				kd_marker_write "$top"
				log "the old root subvolume $sub is now $sub-old-$KD_STAMP"
				mv "$top/$sub" "$top/$sub-old-$KD_STAMP" || die "keep-data: the subvolume $sub cannot be renamed"
				kd_state_set "$top/$KD_MARK" root aside
			fi
		else
			kd_state_set "$old" stamp "$KD_STAMP" root gone default "$dflt"
			kd_marker_write "$top"
			if [ "$(jq -r .private.default.reset <<< "$kd_plan")" = true ]; then
				log "the default subvolume of $dev is its top while $sub is replaced"
				btrfs subvolume set-default 5 "$top" >/dev/null || die "keep-data: the default subvolume of $dev cannot be changed"
			fi
			if [ -d "$top/$sub" ]; then
				log "deleting the old root subvolume $sub of $dev"
				# the subvolumes inside it first, the deepest first
				while IFS= read -r p; do
					[ -n "$p" ] || continue
					btrfs subvolume delete "$top/$p" >/dev/null || die "keep-data: the subvolume $p cannot be deleted"
				done < <(btrfs subvolume list "$top" | sed -n 's/^ID [0-9]* gen [0-9]* top level [0-9]* path //p' | awk -v s="$sub/" 'index($0, s) == 1 {print length($0) "\t" $0}' | sort -rn | cut -f2-)
				btrfs subvolume delete "$top/$sub" >/dev/null || die "keep-data: the subvolume $sub cannot be deleted"
				btrfs subvolume sync "$top" >/dev/null 2>&1 || true
			fi
		fi
		btrfs subvolume create "$top/$sub" >/dev/null || die "keep-data: the subvolume $sub cannot be made"
		if [ "$dflt" = restore ]; then
			id="$(btrfs subvolume list "$top" | awk -v s="$sub" '{p = $0; sub(/^ID [0-9]* gen [0-9]* top level [0-9]* path /, "", p)} p == s {print $2}')"
			if [ -n "$id" ] && btrfs subvolume set-default "$id" "$top" >/dev/null; then log "the default subvolume of $dev is the new $sub"
			else warn "the default subvolume of $dev could not be set to the new $sub: it is the top of the file system"; fi
		fi
	elif [ "$keep_root" = 1 ]; then
		mount -t "$fs" "$dev" "$top" || die "keep-data: the old root $dev cannot be mounted"
		if [ "$sroot" = aside ] && [ -n "$aside" ] && [ -d "$top/$aside" ]; then
			# a second attempt: the old system is aside already, whole; what the first unpacked goes aside too
			KD_STAMP="${aside#old-root-}"
			kd_marker_write "$top"
			first="$top/failed-install-$now"
			log "what the interrupted installation unpacked moves into /failed-install-$now on $dev (it can be removed)"
		else
			# (a second attempt that finds the move begun carries on into the same directory)
			if [ "$sroot" = old ] && [ -n "$aside" ] && [ -d "$top/$aside" ]; then KD_STAMP="${aside#old-root-}"; fi
			kd_state_set "$old" stamp "$KD_STAMP" root old aside "old-root-$KD_STAMP"
			kd_marker_write "$top"
			first="$top/old-root-$KD_STAMP"
			log "the old system moves into /old-root-$KD_STAMP on $dev"
		fi
		mkdir -p "$first" || die "keep-data: $first cannot be made"
		for e in "$top"/* "$top"/.[!.]* "$top"/..?*; do
			[ -e "$e" ] || [ -L "$e" ] || continue
			case "${e##*/}" in lost+found|old-root-*|failed-install-*|"$KD_MARK"|"$KD_MARK".new) continue ;; esac
			mv "$e" "$first/" || die "keep-data: ${e#"$top"} cannot be moved aside; what was moved is in /${first##*/} of $dev, and the same command carries on"
		done
		kd_state_set "$top/$KD_MARK" root aside
	else
		new="$(kd_new_uuid "$(jq -r .root.filesystem <<< "$kd_plan")")"
		kd_state_set "$old" stamp "$KD_STAMP" root gone uuid_new "$new"
		# from here until the new file system holds them, the old system's files are in memory alone
		echo "$dev" > "$KD_DIR/unsaved"
		log "formatting $dev ($(jq -r .root.filesystem <<< "$kd_plan"), /)"
		format "$dev" "$(jq -r .root.filesystem <<< "$kd_plan")" "$(jq -r --arg d "$dev" '.private.labels[$d] // ""' <<< "$kd_plan")" "$new"
		udevadm settle
		mount "$dev" "$top" || die "keep-data: the new root $dev cannot be mounted"
		kd_marker_write "$top"
	fi
	# the other file systems that are made anew: each with a UUID that is
	# written down first, beside the old one, so that a second attempt
	# reads the saved fstab's entry as the new file system
	while IFS=$'\x1f' read -r p fs mp was; do
		[ "$mp" != / ] || continue
		new="$(kd_new_uuid "$fs")"
		if [ -n "$was" ]; then
			printf '%s %s\n' "$was" "$new" >> "$top/$KD_MARK/formats"
			sync "$top/$KD_MARK/formats" 2>/dev/null || true
			sync -f "$top" 2>/dev/null || sync
		fi
		log "formatting $p ($fs, $mp)"
		format "$p" "$fs" "$(jq -r --arg d "$p" '.private.labels[$d] // ""' <<< "$kd_plan")" "$new"
		udevadm settle
	done < <(jq -r '.format[] | select(.how == "mkfs") | [.device, .filesystem, .mountpoint, (.uuid // "")] | join("\u001f")' <<< "$kd_plan")
	umount "$top" || die "keep-data: $dev cannot be unmounted"
	udevadm settle
}

# kd_done: the installation is complete: what was saved of the old system
# for a second attempt goes, the key files with it
kd_done() {
	local dev sub top="$KD_DIR/top"
	dev="$(jq -r .root.device <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"
	if [ -n "$sub" ]; then
		mkdir -p "$top"
		if mount -t btrfs -o subvolid=5 "$dev" "$top"; then
			rm -rf "${top:?}/$KD_MARK" "${top:?}/$KD_MARK.new"
			umount "$top" || true
		else
			warn "the top of $dev could not be mounted: /$KD_MARK there (the old system's fstab and the key files of its crypttab) is to be removed by hand"
		fi
	else
		rm -rf "${TARGET:?}/$KD_MARK" "${TARGET:?}/$KD_MARK.new"
	fi
	rm -rf "${KD_DIR:?}/old" "${KD_DIR:?}/unsaved"
}

# kd_mounts: what the new system is unpacked into: the root, /boot, the
# EFI system partition and the volumes that were formatted for it
kd_mounts() {
	local dev fs mp sub
	MOUNTS=(); SWAPS=()
	dev="$(jq -r .root.device <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"
	MOUNTS+=("/ $dev $(jq -r .root.filesystem <<< "$kd_plan")")
	[ -z "$sub" ] || MOUNT_OPTS[/]="subvol=/$sub"
	efi_dev="$(jq -r .boot.esp <<< "$kd_plan")"
	[ -z "$efi_dev" ] || MOUNTS+=("/boot/efi $efi_dev vfat")
	while IFS=$'\x1f' read -r dev fs mp; do
		[ "$mp" = / ] || MOUNTS+=("$mp $dev $fs")
	done < <(jq -r '.format[] | select(.how == "mkfs") | [.device, .filesystem, .mountpoint] | join("\u001f")' <<< "$kd_plan")
}

# kd_esp_aside: the old system's boot loaders on the EFI system partition
# get another name: the firmware's entries for them lead nowhere then,
# and it starts the new one
kd_esp_aside() {
	local l esp="$TARGET/boot/efi"
	while IFS= read -r l; do
		[ -n "$l" ] && [ -d "$esp/$l" ] || continue
		log "the old boot loader $l on the EFI system partition is now $l-old-$KD_STAMP"
		mv "$esp/$l" "$esp/$l-old-$KD_STAMP" || die "keep-data: $l on the EFI system partition cannot be renamed"
		case "$(printf '%s' "$l" | tr a-z A-Z)" in
		EFI/SYSTEMD) if [ -d "$esp/loader" ]; then mv "$esp/loader" "$esp/loader-old-$KD_STAMP" || true; fi ;;
		esac
	done < <(jq -r '.private.loaders[]' <<< "$kd_plan")
}

# kd_fstab: the new /etc/fstab: the root, /boot, the EFI system partition,
# swap, then the old entries that are kept, in their order and with their
# options. A device the old fstab named by a kernel name is named by its
# UUID; one that was formatted has a new one
kd_fstab() {
	local dev fs opts sub u spec mp type dump pass action nofail new last
	if [ "$keep_root" = 1 ]; then echo "# /etc/fstab, written by UMP (keep-data: the new system beside the old one, which was set aside; the old system's other volumes as they were)"
	else echo "# /etc/fstab, written by UMP (keep-data: the root made anew, the old system's other volumes as they were)"; fi
	dev="$(jq -r .root.device <<< "$kd_plan")"; fs="$(jq -r .root.filesystem <<< "$kd_plan")"; sub="$(jq -r .root.subvolume <<< "$kd_plan")"
	# a root that was not made anew keeps the options it had
	opts="$(kd_jq '.private.root_entry | if . == null then "" else (.options | opts | map(select((startswith("subvol=") or startswith("subvolid=")) | not)) | join(",")) end
		| if $sub != "" then (if . == "" or . == "defaults" then "" else . + "," end) + "subvol=/" + $sub else . end' -r --arg sub "$sub" <<< "$kd_plan")"
	if [ -z "$opts" ]; then opts=defaults; [ "$fs" != ext4 ] || opts=defaults,errors=remount-ro; fi
	last="$(jq -r '.private.root_entry | if . == null then "0 1" else "\(.dump) \(.pass)" end' <<< "$kd_plan")"
	printf 'UUID=%s  /  %s  %s  %s\n' "$(kd_fact_uuid "$dev")" "$fs" "$opts" "$last"
	dev="$(jq -r .boot.boot <<< "$kd_plan")"
	[ -z "$dev" ] || printf 'UUID=%s  /boot  ext4  defaults  0 2\n' "$(kd_fact_uuid "$dev")"
	dev="$(jq -r .boot.esp <<< "$kd_plan")"
	[ -z "$dev" ] || printf 'UUID=%s  /boot/efi  vfat  umask=0077  0 1\n' "$(kd_fact_uuid "$dev")"
	while IFS=$'\x1f' read -r action spec mp type opts dump pass dev new; do
		case "$action" in
		format)
			# the new file system: by its UUID, unless the old entry named
			# the device in a way that still holds (a volume's path, a label)
			u="$(kd_fact_uuid "$dev")"
			case "$spec" in UUID=*|/dev/disk/by-uuid/*) spec="UUID=$u" ;; *) [ "$new" = "$spec" ] || spec="UUID=$u" ;; esac ;;
		*) spec="$new" ;;
		esac
		printf '%s  %s  %s  %s  %s %s\n' "$spec" "$mp" "$type" "$opts" "$dump" "$pass"
	done < <(kd_jq '.private.entries | (map(select(.action == "swap")) + map(select(.action == "keep" or .action == "text" or .action == "aside" or .action == "format")))[]
		| [.action, .spec, (.to // .mountpoint), (if .action == "format" then .fs else .type end),
		   (if .nofail == true then (.options | with_opt("nofail")) else .options end), .dump, .pass, (.device // "-"), (.spec_new // .spec)] | join("\u001f")' -r <<< "$kd_plan")
}

# kd_crypttab: the new /etc/crypttab: the old entries whose devices are
# there (one named as the kernel named it by its UUID or PARTUUID);
# those that cannot be opened unattended with nofail
kd_crypttab() {
	kd_jq '.private.crypttab | map(select(.keep)) | if length == 0 then empty else
		"# /etc/crypttab, written by UMP (keep-data: the old system'"'"'s entries)",
		(.[] | (if .nofail then (.options | with_opt("nofail")) else .options end) as $o | "\(.name)  \(.source_new // .source)  \(.keyfile)\(if $o != "" then "  " + $o else "" end)") end' -r <<< "$kd_plan"
}

# kd_carry: what the kept volumes need in the new system: crypttab and
# its key files, the host ID of ZFS, the mountpoints, the sources of
# bind mounts
kd_carry() {
	local old="$KD_DIR/old" ct k mp spec d
	ct="$(kd_crypttab)"
	if [ -n "$ct" ]; then
		printf '%s\n' "$ct" > "$TARGET/etc/crypttab"
		while IFS= read -r k; do
			[ -n "$k" ] || continue
			mkdir -p "$TARGET$(dirname "$k")"
			cp -a "$old$k" "$TARGET$k" || die "keep-data: the key file $k could not be put into the new system"
		done < <(jq -r '.private.keyfiles[].path' <<< "$kd_plan")
		log "crypttab: $(jq -r '[.private.crypttab[] | select(.keep) | .name] | join(", ")' <<< "$kd_plan")$(jq -r '.private.keyfiles | if length > 0 then "; key files " + (map(.path) | join(", ")) else "" end' <<< "$kd_plan")"
	fi
	if [ -f "$old/etc/hostid" ]; then cp -a "$old/etc/hostid" "$TARGET/etc/hostid"; fi
	if [ -f "$old/etc/zfs/zpool.cache" ] && [ "$(jq -r .private.zfs <<< "$kd_plan")" = true ]; then
		mkdir -p "$TARGET/etc/zfs"; cp -a "$old/etc/zfs/zpool.cache" "$TARGET/etc/zfs/zpool.cache"
	fi
	# the mountpoints; fstab writes a space in a name as \040
	while IFS= read -r mp; do
		case "$mp" in /dev/*|/proc/*|/sys/*|/run/*) continue ;; /*) ;; *) continue ;; esac
		d="$TARGET$(printf "${mp//%/%%}")"
		if [ -d "$d" ]; then
			[ -z "$(ls -A "$d" 2>/dev/null)" ] || warn "$mp of the new system holds files, which the kept volume mounted there hides"
		else
			mkdir -p "$d" 2>/dev/null || warn "the mountpoint $mp could not be made"
		fi
	done < <(jq -r '.private.entries[] | select(.action == "keep" or .action == "text" or .action == "aside") | (.to // .mountpoint)' <<< "$kd_plan")
	# a bind mount's source on the root is gone with the old root, one on
	# a formatted volume with what that held: an empty one, so that the
	# mount does not fail the boot
	while IFS= read -r spec; do
		d="$TARGET$(printf "${spec//%/%%}")"
		if [ ! -e "$d" ]; then mkdir -p "$d" 2>/dev/null || warn "the source $spec of a bind mount could not be made"; fi
	done < <(jq -r '.private.binds[]' <<< "$kd_plan")
}

# kd_mount_homes: the kept volumes the users' home directories lie on
# (/home, /root), mounted for the users' section: the profile's users
# and their keys must be there when the new system mounts them
kd_mount_homes() {
	local mp dev type opts
	while IFS=$'\x1f' read -r mp dev type opts; do
		[ -n "$mp" ] || continue
		mkdir -p "$TARGET$mp"
		mount -t "$type" -o "$opts" "$dev" "$TARGET$mp" 2>/dev/null || mount -t "$type" "$dev" "$TARGET$mp" \
			|| die "keep-data: the kept $mp ($dev) cannot be mounted: the users' home directories and keys would not be there"
		MOUNTS+=("$mp $dev $type")
		log "users: the kept $mp ($dev) is mounted for their home directories"
	done < <(kd_jq '.private as $p | $p.entries[] | select(.i as $i | $p.homes | index($i) != null)
		| [.mountpoint, .device, (if .type == "auto" then "auto" else .type end), (.options | opts | map(select(. != "noauto" and . != "ro")) | join(",") | if . == "" then "defaults" else . end)] | join("\u001f")' -r <<< "$kd_plan" | awk -F'\x1f' '{print length($1) "\t" $0}' | sort -n | cut -f2-)
}

# kd_home_uid NAME: the useradd option for a new user's ID where /home is
# a kept volume: the ID its home directory there belongs to, when that is
# free; else the first free one that no other home directory there
# belongs to (the files of a user from before are not the new user's)
kd_home_uid() {
	local d="$TARGET/home/$1" u others
	[ "$(jq -r '.private.homes | length' <<< "$kd_plan")" -gt 0 ] || return 0
	if [ -d "$d" ]; then
		u="$(stat -c %u "$d")"
		if [ "$u" -ge 1000 ] && [ "$u" -lt 60000 ] && ! in_target getent passwd "$u" >/dev/null 2>&1; then printf '%s\n' -u "$u"; return 0; fi
	fi
	others=" $(for u in "$TARGET"/home/*; do [ "$u" = "$d" ] || [ ! -d "$u" ] || stat -c %u "$u"; done | sort -un | paste -sd' ' -) "
	for ((u = 1000; u < 60000; u++)); do
		case "$others" in *" $u "*) continue ;; esac
		in_target getent passwd "$u" >/dev/null 2>&1 || { printf '%s\n' -u "$u"; return 0; }
	done
}

# kd_home_take NAME HOME: a home directory that was there before goes to
# the user of its name; the authorized_keys it had is saved beside the new
kd_home_take() {
	local d="$TARGET$2" was now
	[ -d "$d" ] || return 0
	was="$(stat -c %u "$d")"; now="$(in_target id -u "$1")"
	if [ "$was" != "$now" ]; then
		log "users: $2 was there, and belonged to the ID $was: its files go to $1 ($now)"
		in_target chown -R --from="$was" "$1:" "$2" || warn "could not change the owner of $2"
		in_target chown "$1:" "$2" || true
	fi
	if [ -f "$d/.ssh/authorized_keys" ] && [ ! -L "$d/.ssh/authorized_keys" ]; then
		cp -p "$d/.ssh/authorized_keys" "$d/.ssh/authorized_keys.old-$KD_STAMP"
	fi
}

# kd_keys_outside: the profile's users' keys also where no kept volume
# hides them: /home or /root that the installer could not mount (of the
# network, encrypted, a ZFS dataset) covers the home directories it made
# on the root, and nobody would get in
kd_keys_outside() {
	local n name i
	[ "$(jq -r .private.keys_outside <<< "$kd_plan")" = true ] || return 0
	mkdir -p -m 755 "$TARGET/etc/ssh/authorized_keys" "$TARGET/etc/ssh/sshd_config.d"
	n="$(jq '.users | length' <<< "$spec")"
	for ((i = 0; i < n; i++)); do
		name="$(jq -r ".users[$i].name" <<< "$spec")"
		jq -r ".users[$i].keys[]" <<< "$spec" | awk '!seen[$0]++' > "$TARGET/etc/ssh/authorized_keys/$name"
		chmod 644 "$TARGET/etc/ssh/authorized_keys/$name"
	done
	printf '%s\n' "# UMP (keep-data): /home or /root is a volume of the old system that the" \
		"# installer could not mount. The keys of the profile's users are in" \
		"# /etc/ssh/authorized_keys/NAME too, so that they get in whatever that" \
		"# volume holds." \
		"AuthorizedKeysFile .ssh/authorized_keys /etc/ssh/authorized_keys/%u" > "$TARGET/etc/ssh/sshd_config.d/20-ump-keep-data.conf"
	log "ssh: the users' keys also in /etc/ssh/authorized_keys (a kept volume may cover their home directories)"
}

# kd_mdadm_conf: the arrays for the new system's mdadm.conf: those that
# are there, each under the name the old mdadm.conf gave it, else the one
# it has here, and by its UUID alone (no name of another host)
kd_mdadm_conf() {
	local old="$KD_DIR/old" line u name f
	mdadm --detail --scan | while IFS= read -r line; do
		u="$(printf '%s\n' "$line" | sed -n 's/.*UUID=\([0-9a-fA-F:]*\).*/\1/p')"
		name="$(printf '%s\n' "$line" | awk '{print $2}')"
		[ -n "$u" ] || { printf '%s\n' "$line"; continue; }
		for f in "$old/etc/mdadm/mdadm.conf" "$old/etc/mdadm.conf"; do
			[ -f "$f" ] || continue
			name="$(awk -v u="$u" '$1 == "ARRAY" {for (i = 3; i <= NF; i++) if (tolower($i) == "uuid=" tolower(u)) {print $2; exit}}' "$f" | grep . || echo "$name")"
		done
		printf 'ARRAY %s%s UUID=%s\n' "$name" "$(printf '%s\n' "$line" | sed -n 's/.*\( metadata=[^ ]*\).*/\1/p')" "$u"
	done
}

# kd_verify_fstab: the new fstab, read by the new system's findmnt: what
# it finds wrong is a warning (an entry that cannot be mounted stops the
# boot unless it has nofail); the volumes crypttab opens are not there yet
kd_verify_fstab() {
	local out
	out="$(in_target findmnt --verify 2>&1 | awk -v skip="$(jq -r '[.private.crypttab[] | select(.keep) | "/dev/mapper/" + .name] | join(" ")' <<< "$kd_plan")" '
		BEGIN {n = split(skip, s, " "); for (i = 1; i <= n; i++) crypt[s[i]] = 1}
		/^[^ ]/ {target = $0; next}
		/\[E\]/ {for (c in crypt) if (index($0, c)) next; print target ":" $0}')" || true
	[ -z "$out" ] || { warn "findmnt --verify finds fault with the new /etc/fstab (an entry that cannot be mounted stops the boot, unless it has nofail):"; printf '%s\n' "$out" | sed 's/^/install.sh:   /' >&2; }
}

# kd_packages: the packages what is kept needs in the new system
# (cryptsetup for crypttab, ZFS on Ubuntu, the tools of thin volumes and
# network file systems), from the system's archive
kd_packages() {
	local pk=() p
	mapfile -t pk < <(jq -r '.packages[]' <<< "$kd_plan")
	[ ${#pk[@]} -gt 0 ] || return 0
	log "packages: ${pk[*]} (what is kept needs them)"
	net_on
	run_in_target "apt-get update" apt-get update -q
	# systemd's part of cryptsetup is a package of its own in newer releases
	for p in "${pk[@]}"; do
		if [ "$p" = cryptsetup ] && in_target apt-cache show systemd-cryptsetup >/dev/null 2>&1; then pk+=(systemd-cryptsetup); fi
	done
	run_in_target "apt-get install ${pk[*]}" apt-get install -y -q --no-install-recommends "${pk[@]}"
	net_off
	if [ "$(jq -r .private.zfs <<< "$kd_plan")" = true ]; then
		# the pools by the cache the old system left, else whatever is found
		[ -f "$TARGET/etc/zfs/zpool.cache" ] || in_target systemctl enable zfs-import-scan.service >/dev/null 2>&1 || warn "could not enable zfs-import-scan.service"
	fi
}

# kd_record: the lines of /etc/ump/install that say what was kept
kd_record() {
	printf 'keep_data=yes\nkeep_root=%s\nkeep_plan=%s\n' "$([ "$keep_root" = 1 ] && echo yes || echo no)" "$(jq -r .hash <<< "$kd_plan")"
	printf 'old_system=%s\n' "$(jq -r '.old.system' <<< "$kd_plan")"
	printf 'formatted=%s\n' "$(jq -r '[.format[] | "\(.device):\(.mountpoint)"] | join(" ")' <<< "$kd_plan")"
	printf 'kept=%s\n' "$(jq -r '[.keep[] | select(.device != null) | "\(.device):\(.mountpoint)"] | join(" ")' <<< "$kd_plan")"
	printf 'aside=%s\n' "$(jq -r --arg d "$KD_STAMP" '[.aside[] | "\(.device):\(.mountpoint | sub("DATE$"; $d))"] | join(" ")' <<< "$kd_plan")"
}

# --- the disks ---------------------------------------------------------

# keep-data: however the run ends from here on, its status says whether
# anything was written (kd_exit)
[ "$keep" = 0 ] || trap 'rc=$?; kd_exit kd_cleanup "$rc"' EXIT

# whole block devices that are not removable media, RAM or loop devices,
# and not on USB (an install medium or virtual media)
all_disks=()
# (raw lsblk output drops empty columns, so the one that can be empty is last)
while IFS= read -r line; do
	set -- $line
	path="$1"; type="$2"; ro="$3"; tran="${4:-}"
	[ "$type" = disk ] || continue
	[ "$ro" = 0 ] || continue
	case "$(basename "$path")" in loop*|ram*|zram*|sr*|fd*|nbd*|md*|dm-*) continue ;; esac
	[ "$tran" = usb ] && continue
	all_disks+=("$path")
done < <(lsblk -dnr -o PATH,TYPE,RO,TRAN | sort)
disks=()
wanted="$(jq -r '.disks // [] | .[]' <<< "$spec")"
if [ -n "$wanted" ]; then
	for d in $wanted; do
		[ -b "$d" ] || die "$d is not a block device"
		[ "$(lsblk -dnr -o TYPE "$d")" = disk ] || die "$d is not a whole disk"
		disks+=("$d")
	done
else
	# NVMe first, then the rest by name: the first one is "first" in layouts
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) disks+=("$d") ;; esac; done
	for d in "${all_disks[@]}"; do case "$d" in /dev/nvme*) ;; *) disks+=("$d") ;; esac; done
fi
[ ${#disks[@]} -gt 0 ] || die "no disk found"
# "first" in layouts: the first disk of at least 4 GiB, so that a small
# helper disk (a Linode's swap disk, a provider's configuration disk)
# never takes the boot partitions, whatever name the kernel gave it this
# boot; the first disk when none is that big
boot_disk="${disks[0]}"
for d in "${disks[@]}"; do
	if [ "$(blockdev --getsize64 "$d")" -ge $((4 * 1024 * 1024 * 1024)) ]; then boot_disk="$d"; break; fi
done
describe_disk() {
	local size rota tran model
	size="$(lsblk -dnr -o SIZE "$1")"; rota="$(lsblk -dnr -o ROTA "$1")"
	tran="$(lsblk -dnr -o TRAN "$1")"; model="$(lsblk -dn -o MODEL "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
	printf '%s %s%s%s' "$size" "$([ "$rota" = 1 ] && echo HDD || echo SSD)" "${tran:+ $tran}" "${model:+ $model}"
}
disk_bytes() { blockdev --getsize64 "$1"; }

# --- the plan: every partition, array and volume, before anything is touched ---

# partitions: one entry per planned partition
P_DISK=(); P_INDEX=(); P_SIZE=(); P_FS=(); P_MOUNT=(); P_LABEL=(); P_VG=(); P_MD=(); P_DEV=()
# md arrays
M_LEVEL=(); M_FS=(); M_MOUNT=(); M_LABEL=(); M_VG=(); M_DEV=()
# logical volumes
L_VG=(); L_NAME=(); L_SIZE=(); L_RAID=(); L_FS=(); L_MOUNT=(); L_LABEL=(); L_DEV=()
declare -A VG_PVS=()        # volume group -> physical volume devices
declare -A VG_NPV=()        # volume group -> number of physical volumes
declare -A DISK_REST=()     # disk -> 1 when a partition takes the rest

match_disks() { # match_disks TARGETS_JSON -> the matching disks, in order, once each
	local t d out=()
	while IFS= read -r t; do
		[ -n "$t" ] || continue
		for d in "${disks[@]}"; do
			if [ "$t" = first ]; then [ "$d" = "$boot_disk" ] || continue
			else [[ "$d" =~ $t ]] || continue; fi
			case " ${out[*]:-} " in *" $d "*) ;; *) out+=("$d") ;; esac
		done
	done < <(jq -r '.[]' <<< "$1")
	printf '%s\n' "${out[@]:-}"
}
next_index() { # next_index DISK: the next free partition number below 100
	local i max=0
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$1" ] || continue
		[ "${P_INDEX[$i]}" -lt 100 ] && [ "${P_INDEX[$i]}" -gt "$max" ] && max="${P_INDEX[$i]}"
	done
	echo $((max + 1))
}
plan_partition() { # plan_partition DISK INDEX SIZE FS MOUNT LABEL VG MD
	local i
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$1" ] && [ "${P_INDEX[$i]}" = "$2" ] && die "partition $2 of $1 is planned twice"
	done
	case "$3" in rest|all|100%)
		[ -z "${DISK_REST[$1]:-}" ] || die "two parts take the rest of $1"
		DISK_REST[$1]=1 ;;
	esac
	P_DISK+=("$1"); P_INDEX+=("$2"); P_SIZE+=("$3"); P_FS+=("$4"); P_MOUNT+=("$5"); P_LABEL+=("$6"); P_VG+=("$7"); P_MD+=("$8"); P_DEV+=("")
}
min_members() { # min_members LEVEL KIND(md|lvm)
	case "$2:$1" in
	md:0|md:1|md:10) echo 2 ;; md:4|md:5) echo 3 ;; md:6) echo 4 ;;
	lvm:0|lvm:1) echo 2 ;; lvm:5) echo 3 ;; lvm:6) echo 5 ;; lvm:10) echo 4 ;;
	*) echo 1 ;;
	esac
}

# (the layout's plan and the whole-disk steps are functions, for a run
# that applies the layout; --keep-data calls none of them)
plan_layout() {
nparts="$(jq '.layout.parts | length' <<< "$spec")"
for ((n = 0; n < nparts; n++)); do
	part="$(jq -c ".layout.parts[$n]" <<< "$spec")"
	f() { jq -r "$1 // empty" <<< "$part"; }
	type="$(f .type)"; size="$(f .size)"; fs="$(f .filesystem)"; mount="$(f .mountpoint)"; label="$(f .label)"; vg="$(f .vg)"
	raid="$(jq -r '.raid // "none"' <<< "$part")"
	at="layout $layout_name, part $((n + 1)) ($type)"
	case "$type" in
	slice|md)
		mapfile -t targets < <(match_disks "$(jq -c '.target // []' <<< "$part")")
		[ -n "${targets[0]:-}" ] || die "$at: no disk matches $(jq -c '.target' <<< "$part") (disks: ${disks[*]})"
		if [ "$type" = slice ]; then
			index="$(jq -r '.index // 0' <<< "$part")"
			for d in "${targets[@]}"; do
				i="$index"; [ "$i" -gt 0 ] || i="$(next_index "$d")"
				plan_partition "$d" "$i" "$size" "$fs" "$mount" "$label" "$vg" ""
			done
		else
			need="$(min_members "$raid" md)"
			[ ${#targets[@]} -ge "$need" ] || die "$at: raid $raid needs $need disks, ${#targets[@]} match"
			j=${#M_LEVEL[@]}
			M_LEVEL+=("$raid"); M_FS+=("$fs"); M_MOUNT+=("$mount"); M_LABEL+=("$label"); M_VG+=("$vg"); M_DEV+=("/dev/md$j")
			for d in "${targets[@]}"; do plan_partition "$d" "$(next_index "$d")" "$size" "md" "" "" "" "$j"; done
		fi
		;;
	lvm)
		name="$(f .name)"
		L_VG+=("$vg"); L_NAME+=("$name"); L_SIZE+=("$size"); L_RAID+=("$raid"); L_FS+=("$fs"); L_MOUNT+=("$mount"); L_LABEL+=("$label"); L_DEV+=("/dev/$vg/$name")
		;;
	*) die "$at: unknown part type" ;;
	esac
done
# the volume groups: how many physical volumes each will have, for the
# raid levels that need a minimum
for i in "${!P_DISK[@]}"; do [ "${P_FS[$i]}" = lvm ] && VG_NPV[${P_VG[$i]}]=$(( ${VG_NPV[${P_VG[$i]}]:-0} + 1 )); done
for j in "${!M_LEVEL[@]}"; do [ "${M_FS[$j]}" = lvm ] && VG_NPV[${M_VG[$j]}]=$(( ${VG_NPV[${M_VG[$j]}]:-0} + 1 )); done
for k in "${!L_VG[@]}"; do
	[ -n "${VG_NPV[${L_VG[$k]}]:-}" ] || die "volume group ${L_VG[$k]} of volume ${L_NAME[$k]} gets no physical volume on this machine"
	need="$(min_members "${L_RAID[$k]}" lvm)"
	if [ "${L_RAID[$k]}" = 0 ] && [ "${VG_NPV[${L_VG[$k]}]}" -lt 2 ]; then
		L_RAID[$k]="none"   # striping over one physical volume is linear
	elif [ "${VG_NPV[${L_VG[$k]}]}" -lt "$need" ]; then
		die "volume ${L_NAME[$k]}: raid ${L_RAID[$k]} needs $need physical volumes in ${L_VG[$k]}, this machine gives ${VG_NPV[${L_VG[$k]}]}"
	fi
done
# what the boot loader needs
has_bios=0; has_efi=0; root_dev=""
for i in "${!P_DISK[@]}"; do
	[ "${P_FS[$i]}" = bios ] && has_bios=1
	[ "${P_FS[$i]}" = efi ] && has_efi=1
done
[ "$has_efi" = 1 ] || [ "$efi" = 0 ] || die "this machine boots with UEFI and the layout has no efi part"
[ "$has_bios" = 1 ] || [ "$efi" = 1 ] || [ "$arch" != amd64 ] || die "this machine boots with BIOS and the layout has no bios part"
jq -e '[.layout.parts[] | select(.mountpoint == "/")] | length == 1' >/dev/null <<< "$spec" || die "the layout must mount exactly one part at /"
return 0
}
has_bios=0; has_efi=0; root_dev=""
if [ "$keep" = 0 ]; then plan_layout; fi

# the network: the uplink of the specification where it has one (a PXE
# machine's), else what the rescue system was given on its kernel command
# line (ump rescue carries the running system's over), else what is
# configured
uplink_spec="$(jq -c '.network.uplink // empty' <<< "$spec")"
cmdline="$(cat /proc/cmdline)"
param() { for w in $cmdline; do case "$w" in "$1="*) printf '%s' "${w#*=}"; return 0 ;; esac; done; return 1; }
mac="$(param ump.mac || true)"
ip4="$(param ump.ip || true)"
ip6="$(param ump.ip6 || true)"
if [ -z "$mac" ]; then
	iface="$(ip -o route show default 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") print $(i + 1)}' | head -n 1)"
	[ -n "$iface" ] && mac="$(cat "/sys/class/net/$iface/address")"
	if [ -n "$iface" ] && [ -z "$ip4" ]; then
		a="$(ip -o -4 addr show dev "$iface" scope global 2>/dev/null | awk '{print $4}' | head -n 1)"
		g="$(ip -o -4 route show default dev "$iface" 2>/dev/null | awk '{print $3}' | head -n 1)"
		[ -n "$a" ] && ip4="$a${g:+,$g}"
	fi
	if [ -n "$iface" ] && [ -z "$ip6" ]; then
		a="$(ip -o -6 addr show dev "$iface" scope global 2>/dev/null | awk '$0 !~ /deprecated|tentative|temporary/ {print $4}' | head -n 1)"
		g="$(ip -o -6 route show default dev "$iface" 2>/dev/null | awk '{print $3}' | head -n 1)"
		[ -n "$a" ] && ip6="$a${g:+,$g}"
	fi
fi
if [ -n "$uplink_spec" ]; then
	net_from_spec "$uplink_spec"
else
	net_from_machine
fi
[ -n "$mac" ] || die "no network interface with a default route; the new system would be unreachable"

# chroot_resolv FILE: the resolver for a chroot, the rescue system's own
# (its systemd-resolved falls back to TCP where a firewall at the provider
# lets no UDP answers in), else the uplink's static resolvers or the
# public ones
chroot_resolv() {
	if grep -q '^nameserver' /etc/resolv.conf 2>/dev/null; then
		grep -E '^(nameserver|options|search) ' /etc/resolv.conf > "$1"
	else
		printf 'nameserver %s\n' ${dns:-1.1.1.1 8.8.8.8} > "$1"
	fi
}

# the interfaces: every wired one, by MAC. The uplink and those with a
# public address are external (ext01, ext02, ...), the rest internal
# (int01, ...), unless the profile keeps the kernel's names.
rename="$(jq -r 'if .network.rename == false then "no" else "yes" end' <<< "$spec")"
IF_MAC=(); IF_NAME=(); IF_KIND=(); IF_OLD=()
next_ext=1; next_int=1
private='^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|169\.254\.)'
for d in $(ls /sys/class/net | sort -V); do
	[ "$d" = lo ] && continue
	[ -e "/sys/class/net/$d/device" ] || continue         # a device, not a bond, vlan or bridge
	[ "$(cat "/sys/class/net/$d/type" 2>/dev/null)" = 1 ] || continue   # ethernet
	[ -d "/sys/class/net/$d/wireless" ] && continue
	m="$(cat "/sys/class/net/$d/address" 2>/dev/null || true)"
	case "$m" in ""|00:00:00:00:00:00) continue ;; esac
	kind=int
	if [ "$m" = "$mac" ]; then
		kind=ext
	elif ip -o -4 addr show dev "$d" scope global 2>/dev/null | awk '{print $4}' | grep -qvE "$private"; then
		kind=ext
	elif ip -o -6 addr show dev "$d" scope global 2>/dev/null | awk '{print $4}' | grep -qviE '^f[cd]'; then
		kind=ext
	fi
	IF_MAC+=("$m"); IF_KIND+=("$kind"); IF_OLD+=("$d")
	if [ "$rename" = yes ]; then
		if [ "$kind" = ext ]; then IF_NAME+=("$(printf 'ext%02d' "$next_ext")"); next_ext=$((next_ext + 1))
		else IF_NAME+=("$(printf 'int%02d' "$next_int")"); next_int=$((next_int + 1)); fi
	else
		IF_NAME+=("$d")
	fi
done
uplink=""
for i in "${!IF_MAC[@]}"; do [ "${IF_MAC[$i]}" = "$mac" ] && uplink="${IF_NAME[$i]}"; done
if [ -z "$uplink" ]; then
	[ -z "$uplink_spec" ] || die "the uplink of the specification, $mac, is no interface of this machine (it has ${IF_MAC[*]:-none})"
	# the uplink's MAC was given, but no such interface is up here: keep it
	uplink=uplink; [ "$rename" = yes ] && uplink=ext01
	IF_MAC=("$mac" "${IF_MAC[@]}"); IF_KIND=(ext "${IF_KIND[@]}"); IF_OLD=("?" "${IF_OLD[@]}"); IF_NAME=("$uplink" "${IF_NAME[@]}")
fi

# --- the plan, printed ---------------------------------------------------

log "machine: $arch, $([ "$efi" = 1 ] && echo UEFI || echo BIOS) firmware, $(nproc) CPUs, $(awk '/^MemTotal/ {printf "%d", $2 / 1024}' /proc/meminfo) MB RAM"
log "image: $os for $arch from $image_url (profile ${profile_name:-?}, layout $layout_name)"
print_layout() {
d=""; for x in "${disks[@]}"; do d="${d:+$d, }$x ($(describe_disk "$x"))"; done
log "disks (${#disks[@]}): $d"
for x in "${disks[@]}"; do
	line=""
	for i in "${!P_DISK[@]}"; do
		[ "${P_DISK[$i]}" = "$x" ] || continue
		what="${P_FS[$i]}"
		[ "${P_FS[$i]}" = lvm ] && what="lvm (${P_VG[$i]})"
		[ "${P_FS[$i]}" = md ] && what="md${P_MD[$i]} member"
		line="${line:+$line, }${P_INDEX[$i]}: ${P_SIZE[$i]} $what${P_MOUNT[$i]:+ at ${P_MOUNT[$i]}}"
	done
	log "  $x: ${line:-untouched}"
done
for j in "${!M_LEVEL[@]}"; do
	what="${M_FS[$j]}"; [ "${M_FS[$j]}" = lvm ] && what="lvm (${M_VG[$j]})"
	log "  md$j: raid ${M_LEVEL[$j]} over $(for i in "${!P_DISK[@]}"; do [ "${P_MD[$i]}" = "$j" ] && printf '%s ' "${P_DISK[$i]}"; done), $what${M_MOUNT[$j]:+ at ${M_MOUNT[$j]}}"
done
for k in "${!L_VG[@]}"; do
	r="linear"; [ "${L_RAID[$k]}" != none ] && r="raid ${L_RAID[$k]}"
	log "  ${L_VG[$k]}/${L_NAME[$k]}: ${L_SIZE[$k]} $r ${L_FS[$k]}${L_MOUNT[$k]:+ at ${L_MOUNT[$k]}}"
done
return 0
}
if [ "$keep" = 0 ]; then print_layout; fi
log "network: $(net_plan); hostname $hostname; time zone $timezone"
ifs=""
for i in "${!IF_MAC[@]}"; do
	was=""
	if [ "${IF_OLD[$i]}" != "${IF_NAME[$i]}" ]; then was=", now ${IF_OLD[$i]}"; fi
	ifs="${ifs:+$ifs, }${IF_NAME[$i]} (${IF_KIND[$i]}, ${IF_MAC[$i]}$was)"
done
log "interfaces: $ifs$([ "$rename" = yes ] || printf '; kernel names kept')"
if [ -n "$private_list" ]; then
	log "$(jq -r '(if length > 1 then "private networks: " else "private network: " end) + (map("\(.address) in \(.network) (\(.cidr))") | join(", "))' <<< "$privates"); recorded, no interface configured"
fi
log "users: $(jq -r '[.users[] | "\(.name) (\(.keys | length) key(s)\(if .sudo then ", sudo" else "" end))"] | join(", ")' <<< "$spec")"
log "userdata: $(jq -r '.userdata | if length == 0 then "none" else map(.name) | join(" ") end' <<< "$spec"); firewall: $(jq -r '.firewall | if .policy == "drop" then "drop, allow " + ([.allow[] | if .proto == "icmp" then "icmp" else "\(.proto)/\(.port)\(if .from then " from " + .from else "" end)" end] | join(", ")) else "none" end' <<< "$spec")"
log "kernel command line: $kernel_cmdline"
if [ "$dry" = 1 ] && [ "$keep" = 0 ]; then
	log "dry run: stopping here, no disk was touched"
	exit 0
fi

# --- the image: the manifest first, so that a missing image costs no disk ---

fetch_manifest() {
log "fetching the manifest"
manifest="$(curl -fsSL "$image_url/manifest.json")" || die "cannot fetch $image_url/manifest.json; the storage must serve the image (see images/base/README.md)"
family="$(printf '%s' "$manifest" | jq -r '.family // "image"')"
image_version="$(printf '%s' "$manifest" | jq -r '.version // empty')"
image_kernel="$(printf '%s' "$manifest" | jq -r '.kernel // empty')"
# the size of the installed system: the root file system must hold twice it
unpacked_mb="$(printf '%s' "$manifest" | jq -r '.unpacked_mb // empty')"
# a manifest may say why there is nothing for this machine (Arch Linux on
# arm64)
unsupported="$(printf '%s' "$manifest" | jq -r '.unsupported // empty')"
[ -z "$unsupported" ] || die "$os on $arch: $unsupported"
case "$family" in
image)
	want_sum="$(printf '%s' "$manifest" | jq -r '.files["rootfs.tar.zst"].sha256 // empty')"
	[ -n "$want_sum" ] || die "the manifest names no rootfs.tar.zst checksum"
	log "image $os version ${image_version:-?}, kernel ${image_kernel:-?}, ${unpacked_mb:-?} MB unpacked"
	;;
nixos)
	for k in channel nix.url nix.sha256 state_version; do
		[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the NixOS manifest names no $k"
	done
	log "image $os: NixOS from the channel $(printf '%s' "$manifest" | jq -r .channel), installed with nixos-install"
	;;
arch)
	for k in bootstrap.url bootstrap.sha256 mirror; do
		[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the Arch Linux manifest names no $k"
	done
	[ "$(printf '%s' "$manifest" | jq -r '.packages | length')" -gt 0 ] || die "the Arch Linux manifest names no packages"
	if [ -n "$(printf '%s' "$manifest" | jq -r '.cachyos // empty')" ]; then
		for k in cachyos.mirror cachyos.keyring.url cachyos.keyring.sha256; do
			[ -n "$(printf '%s' "$manifest" | jq -r ".$k // empty")" ] || die "the CachyOS manifest names no $k"
		done
		log "image $os: CachyOS from $(printf '%s' "$manifest" | jq -r .cachyos.mirror) over Arch Linux from $(printf '%s' "$manifest" | jq -r .mirror) (bootstrap $(printf '%s' "$manifest" | jq -r .bootstrap.release)), installed with pacstrap"
		log "CachyOS is experimental in UMP: not for production unless you know what you are doing (docs/oses.md#cachyos)"
	else
		log "image $os: Arch Linux from $(printf '%s' "$manifest" | jq -r .mirror) (bootstrap $(printf '%s' "$manifest" | jq -r .bootstrap.release)), installed with pacstrap"
	fi
	;;
*) die "the manifest's family $family is not one this installer knows (image, nixos, arch)" ;;
esac
# the userdata must be of the kind the system takes
want_ud=bash; [ "$family" = nixos ] && want_ud=nix
bad_ud="$(jq -r --arg t "$want_ud" '[.userdata[] | select((.type // "bash") != $t) | .name] | join(", ")' <<< "$spec")"
[ -z "$bad_ud" ] || die "userdata $bad_ud is not of the type $os takes ($want_ud)"
return 0
}
fetch_manifest
if [ "$keep" = 1 ]; then
	# keep-data: the old system found, read-only, and what becomes of every
	# volume: the plan. The image's size is part of it, hence the manifest
	# first. Nothing is written without the plan confirmed (--confirm).
	[ "$family" = image ] || die "keep-data installs the images (Ubuntu, Debian) in this version; $os is of the family $family, which it does not support yet"
	kd_detect
	while IFS= read -r line; do log "$line"; done < <(kd_text)
	log "plan: $(jq -r .hash <<< "$kd_plan") (sha256; --confirm takes it)"
	if [ "$dry" = 1 ]; then
		if [ -n "$plan_file" ]; then
			kd_public > "$plan_file"; log "plan file: $plan_file"
			log "the plan is made: stopping here, no disk was touched; --confirm with its hash installs it"
		else
			log "dry run: stopping here, no disk was touched"
		fi
		exit 0
	fi
	kd_confirmed
	mapfile -t disks < <(jq -r '.boot.disks[]' <<< "$kd_plan")
	boot_disk="$(jq -r .boot.disk <<< "$kd_plan")"
	[ -z "$(jq -r .boot.esp <<< "$kd_plan")" ] || has_efi=1
fi

# --- the disks ---------------------------------------------------------

# kill_chrooted: ends what still runs in the new system or a bootstrap
# (a gpg-agent that pacman-key started there, a daemon a userdata
# script left): it keeps the root file system busy, and an attempt
# after a failed one could not release the disks. Every process whose
# root is not the rescue system's: nothing of the rescue system runs
# chrooted, and once its file system is unmounted lazily, the root of a
# process left behind no longer reads as a path under $TARGET.
kill_chrooted() {
	local p r found=""
	for p in /proc/[0-9]*; do
		r="$(readlink "$p/root" 2>/dev/null)" || continue
		[ "$r" = / ] || { kill -9 "${p#/proc/}" 2>/dev/null && found=1; }
	done
	[ -z "$found" ] || sleep 1
	return 0
}

# The whole-disk steps: every disk released, wiped and partitioned, the
# layout's arrays, volume groups and file systems made. Never with
# --keep-data, which calls kd_write instead.
wipe_disks() {
[ "$keep" = 0 ] || die "keep-data: the disks are never wiped"
# (what a keep-data installation that failed here still held in memory
# of an old system is of no use once the disks are wiped)
rm -rf /run/ump-keep-data
log "releasing the disks"
kill_chrooted
swapoff -a 2>/dev/null || true
# a swap file of an earlier attempt whose file system was unmounted under
# it cannot be found by its name any more, and keeps its disk busy
stale_swap="$(awk 'NR > 1 && $2 == "file" {print $1}' /proc/swaps)"
[ -z "$stale_swap" ] || die "a swap file of an earlier attempt is still active ($stale_swap) and keeps a disk busy; reboot the rescue system (ump server rescue with a reboot) and run the installation again"
while read -r src tgt; do
	case "$src" in /dev/*) ;; *) continue ;; esac
	for x in "${disks[@]}"; do case "$src" in "$x"*|/dev/mapper/*|/dev/md*|/dev/dm-*) umount -l "$tgt" 2>/dev/null || true; break ;; esac; done
done < <(awk '{print $1, $2}' /proc/mounts | sort -k2 -r)
# whatever an earlier attempt left on $TARGET
for _ in 1 2 3 4 5; do mountpoint -q "$TARGET" && umount -R -l "$TARGET" 2>/dev/null || break; done
vgchange -an >/dev/null 2>&1 || true
# a mapping LVM no longer knows (its metadata wiped by an earlier
# attempt) still holds its disk: removed when it sits on one of ours
for dm in /sys/block/dm-*; do
	[ -d "$dm/slaves" ] || continue
	for sl in "$dm"/slaves/*; do
		for x in "${disks[@]}"; do
			case "/dev/${sl##*/}" in "$x"*) dmsetup remove "$(cat "$dm/dm/name")" 2>/dev/null || true; continue 3 ;; esac
		done
	done
done
command -v mdadm >/dev/null && mdadm --stop --scan >/dev/null 2>&1 || true

log "wiping the disks"
for x in "${disks[@]}"; do
	for p in $(lsblk -nr -o PATH "$x" | tail -n +2); do wipefs -af "$p" >/dev/null 2>&1 || true; done
	sgdisk --zap-all "$x" >/dev/null 2>&1 || true
	wipefs -af "$x" >/dev/null
	dd if=/dev/zero of="$x" bs=1M count=2 conv=notrunc status=none 2>/dev/null || true
done
partprobe >/dev/null 2>&1 || true
udevadm settle
pvscan --cache >/dev/null 2>&1 || true

log "partitioning"
sg_size() { # sg_size DISK SIZE -> the end argument for sgdisk
	case "$2" in
	rest|all|100%) echo 0 ;;
	*%) echo "+$(( $(disk_bytes "$1") / 100 * ${2%\%} / 1024 / 1024 ))M" ;;
	*) echo "+$2" ;;
	esac
}
type_code() { case "$1" in bios) echo ef02 ;; efi) echo ef00 ;; swap) echo 8200 ;; lvm) echo 8e00 ;; md) echo fd00 ;; *) echo 8300 ;; esac; }
for x in "${disks[@]}"; do
	args=()
	# every partition in index order, the one that takes the rest last
	for pass in fixed rest; do
		for i in $(for i in "${!P_DISK[@]}"; do [ "${P_DISK[$i]}" = "$x" ] && echo "${P_INDEX[$i]} $i"; done | sort -n | cut -d' ' -f2); do
			case "${P_SIZE[$i]}" in rest|all|100%) [ "$pass" = rest ] || continue ;; *) [ "$pass" = fixed ] || continue ;; esac
			name="${P_LABEL[$i]:-${P_FS[$i]}}"
			args+=(-n "${P_INDEX[$i]}:0:$(sg_size "$x" "${P_SIZE[$i]}")" -t "${P_INDEX[$i]}:$(type_code "${P_FS[$i]}")" -c "${P_INDEX[$i]}:$name")
		done
	done
	[ ${#args[@]} -gt 0 ] || continue
	sgdisk "${args[@]}" "$x" >/dev/null || die "partitioning $x failed"
done
partprobe >/dev/null 2>&1 || true
udevadm settle
partdev() { # partdev DISK N
	local p
	p="$(lsblk -nr -o PATH,PARTN "$1" 2>/dev/null | awk -v n="$2" '$2 == n {print $1}' | head -n 1)"
	if [ -z "$p" ]; then case "$1" in *[0-9]) p="${1}p$2" ;; *) p="$1$2" ;; esac; fi
	[ -b "$p" ] || die "partition $2 of $1 did not appear"
	printf '%s' "$p"
}
for i in "${!P_DISK[@]}"; do P_DEV[$i]="$(partdev "${P_DISK[$i]}" "${P_INDEX[$i]}")"; done

if [ ${#M_LEVEL[@]} -gt 0 ]; then
	log "md arrays"
	for j in "${!M_LEVEL[@]}"; do
		members=(); for i in "${!P_DISK[@]}"; do [ "${P_MD[$i]}" = "$j" ] && members+=("${P_DEV[$i]}"); done
		mdadm --create "${M_DEV[$j]}" --level="${M_LEVEL[$j]}" --raid-devices="${#members[@]}" --metadata=1.2 --force --run "${members[@]}" >/dev/null 2>&1 || die "mdadm --create ${M_DEV[$j]} failed"
	done
	udevadm settle
fi
return 0
}

format() { # format DEV FS LABEL [UUID]
	local l=() u="${4:-}"
	# keep-data: only a device the confirmed plan names, still as the plan saw it
	[ "$keep" = 0 ] || kd_may_format "$1" "$2"
	case "$2" in
	efi|vfat) [ -n "$3" ] && l=(-n "$3"); [ -z "$u" ] || l+=(-i "${u//-/}"); mkfs.vfat -F 32 "${l[@]}" "$1" >/dev/null ;;
	# without orphan_file and metadata_csum_seed, which a current mkfs.ext4
	# turns on and the GRUB of older releases (Ubuntu 22.04) cannot read
	ext4) [ -n "$3" ] && l=(-L "$3"); [ -z "$u" ] || l+=(-U "$u"); mkfs.ext4 -q -F -O ^orphan_file,^metadata_csum_seed "${l[@]}" "$1" ;;
	xfs) [ -n "$3" ] && l=(-L "$3"); [ -z "$u" ] || l+=(-m "uuid=$u"); command -v mkfs.xfs >/dev/null || die "mkfs.xfs is missing"; mkfs.xfs -f -q "${l[@]}" "$1" ;;
	btrfs) [ -n "$3" ] && l=(-L "$3"); [ -z "$u" ] || l+=(-U "$u"); command -v mkfs.btrfs >/dev/null || die "mkfs.btrfs is missing"; mkfs.btrfs -f -q "${l[@]}" "$1" >/dev/null ;;
	swap) [ -n "$3" ] && l=(-L "$3"); mkswap -f "${l[@]}" "$1" >/dev/null ;;
	lvm) pvcreate -ff -y "$1" >/dev/null ;;
	bios|none|md) ;;
	*) die "unknown file system $2" ;;
	esac
}
add_pv() { VG_PVS[$1]="${VG_PVS[$1]:-}${VG_PVS[$1]:+ }$2"; }

make_layout() {
[ "$keep" = 0 ] || die "keep-data: no layout is made"
log "file systems"
for i in "${!P_DISK[@]}"; do
	format "${P_DEV[$i]}" "${P_FS[$i]}" "${P_LABEL[$i]}"
	[ "${P_FS[$i]}" = lvm ] && add_pv "${P_VG[$i]}" "${P_DEV[$i]}"
done
for j in "${!M_LEVEL[@]}"; do
	format "${M_DEV[$j]}" "${M_FS[$j]}" "${M_LABEL[$j]}"
	[ "${M_FS[$j]}" = lvm ] && add_pv "${M_VG[$j]}" "${M_DEV[$j]}"
done
if [ ${#VG_PVS[@]} -gt 0 ]; then
	log "volume groups: $(for vg in "${!VG_PVS[@]}"; do printf '%s (%s) ' "$vg" "${VG_PVS[$vg]}"; done)"
	for vg in "${!VG_PVS[@]}"; do vgcreate -y "$vg" ${VG_PVS[$vg]} >/dev/null || die "vgcreate $vg failed"; done
	for k in "${!L_VG[@]}"; do
		npv="$(vgs --noheadings -o pv_count "${L_VG[$k]}" | tr -d ' ')"
		sz=()
		case "${L_SIZE[$k]}" in
		rest|all|100%) sz=(-l 100%FREE) ;;
		*%) sz=(-l "${L_SIZE[$k]}VG") ;;
		*) sz=(-L "${L_SIZE[$k]}") ;;
		esac
		kind=()
		case "${L_RAID[$k]}" in
		0) kind=(--type striped -i "$npv") ;;
		1) kind=(--type raid1 -m 1) ;;
		5) kind=(--type raid5 -i $((npv - 1))) ;;
		6) kind=(--type raid6 -i $((npv - 2))) ;;
		10) kind=(--type raid10 -i $((npv / 2)) -m 1) ;;
		esac
		lvcreate -y "${sz[@]}" "${kind[@]}" -n "${L_NAME[$k]}" "${L_VG[$k]}" >/dev/null || die "lvcreate ${L_VG[$k]}/${L_NAME[$k]} failed"
	done
	udevadm settle
	for k in "${!L_VG[@]}"; do format "${L_DEV[$k]}" "${L_FS[$k]}" "${L_LABEL[$k]}"; done
fi
return 0
}
if [ "$keep" = 1 ]; then
	kd_write
else
	wipe_disks
	make_layout
fi

# --- mounting: root first, then the rest by depth -------------------------

MOUNTS=()   # "mountpoint dev fs"
declare -A MOUNT_OPTS=()   # mountpoint -> mount options (keep-data: the root's subvolume)
SWAPS=()
efi_dev=""
collect() { # collect DEV FS MOUNT
	case "$2" in
	swap) SWAPS+=("$1") ;;
	efi) [ -n "$efi_dev" ] || { efi_dev="$1"; MOUNTS+=("$3 $1 vfat"); } ;;
	bios|lvm|md|none) ;;
	*) [ -n "$3" ] && MOUNTS+=("$3 $1 $2") ;;
	esac
}
for i in "${!P_DISK[@]}"; do collect "${P_DEV[$i]}" "${P_FS[$i]}" "${P_MOUNT[$i]}"; done
for j in "${!M_LEVEL[@]}"; do collect "${M_DEV[$j]}" "${M_FS[$j]}" "${M_MOUNT[$j]}"; done
for k in "${!L_VG[@]}"; do collect "${L_DEV[$k]}" "${L_FS[$k]}" "${L_MOUNT[$k]}"; done
[ "$keep" = 0 ] || kd_mounts
mapfile -t MOUNTS < <(printf '%s\n' "${MOUNTS[@]}" | awk '{print length($1), $0}' | sort -n | cut -d' ' -f2-)
root_dev=""
for m in "${MOUNTS[@]}"; do set -- $m; [ "$1" = / ] && root_dev="$2"; done
[ -n "$root_dev" ] || die "nothing is mounted at /"
root_mb="$(lsblk -dnr -o SIZE -b "$root_dev" | awk '{printf "%d", $1 / 1024 / 1024}')"
# (keep-data checked the root, or the room on it, for its plan)
if [ "$keep" = 0 ] && [ -n "$unpacked_mb" ] && [ "$root_mb" -lt $((unpacked_mb * 2)) ]; then
	die "the root file system ($root_mb MB) is too small for the image ($unpacked_mb MB unpacked)"
fi

log "mounting the new system at $TARGET"
mkdir -p "$TARGET"
# the directory above $TARGET a private mount of its own, before the new
# system is mounted: nothing mounted there is copied into the mount
# namespaces of the rescue system's services (resolved, udevd, networkd).
# A copy of a mount made private later (the Arch Linux bootstrap's) would
# never see it unmounted, and keep the disks busy for the next attempt.
# Not $TARGET itself: nixos-generate-config would take that mount for a
# second root file system.
target_parent="$(dirname "$TARGET")"
if ! mountpoint -q "$target_parent"; then
	mount --bind "$target_parent" "$target_parent" && mount --make-private "$target_parent" || die "cannot make $target_parent a private mount"
fi
for m in "${MOUNTS[@]}"; do
	set -- $m
	mkdir -p "$TARGET$1"
	mount -t "$3" ${MOUNT_OPTS[$1]:+-o "${MOUNT_OPTS[$1]}"} "$2" "$TARGET$1" || die "mounting $2 at $1 failed"
done
cleanup() {
	set +e
	kill_chrooted
	# a swap file on the new root keeps it busy: off before it is unmounted
	[ -n "${target_swap:-}" ] && swapoff "$target_swap" 2>/dev/null && rm -f "$target_swap"
	# what is mounted into the Arch Linux bootstrap
	[ -n "${arch_boot:-}" ] && arch_umount
	mountpoint -q /nix && umount -l /nix 2>/dev/null
	for m in "$TARGET/dev/pts" "$TARGET/run" "$TARGET/sys" "$TARGET/proc" "$TARGET/dev"; do mountpoint -q "$m" && umount -l "$m" 2>/dev/null; done
	for ((n = ${#MOUNTS[@]} - 1; n >= 0; n--)); do set -- ${MOUNTS[$n]}; mountpoint -q "$TARGET$1" && umount -l "$TARGET$1" 2>/dev/null; done
	[ "$keep" = 0 ] || kd_cleanup
}
trap cleanup EXIT
[ "$keep" = 0 ] || trap 'rc=$?; kd_exit cleanup "$rc"' EXIT
[ "$keep" = 0 ] || [ -z "$efi_dev" ] || kd_esp_aside

# the administrator: the first sudo user other than root, else root, as
# ump picks it; ump logs in as it
admin="$(jq -r '([.users[] | select(.sudo == true and .name != "root")][0].name) // ([.users[] | select(.name == "root")][0].name) // .users[0].name' <<< "$spec")"

# the firewall ruleset, the same for every system: input policy, and the
# same list for new connections forwarded in from the external interfaces
nft_ruleset() {
	ext=""; for i in "${!IF_MAC[@]}"; do [ "${IF_KIND[$i]}" = ext ] && ext="${ext:+$ext, }\"${IF_NAME[$i]}\""; done
	# the allowed entries, for the input chain (dport) and for the forward
	# chain, where the destination port is the container's after the DNAT
	# of a published port, so the original one is matched; ICMPv6 is
	# protocol 58, by number because an older nft resolves the name
	# through /etc/protocols only
	allow() { # allow input|forward
		if [ "$1" = input ]; then
			jq -r '.firewall.allow[] | if .proto == "icmp" then "    ip protocol icmp accept\n    meta l4proto 58 accept" else (if .from then (if (.from | test(":")) then "    ip6 saddr " + .from + " " else "    ip saddr " + .from + " " end) else "    " end) + .proto + " dport " + .port + " accept" end' <<< "$spec"
		else
			jq -r '.firewall.allow[] | if .proto == "icmp" then "    ip protocol icmp accept\n    meta l4proto 58 accept" else (if .from then (if (.from | test(":")) then "    ip6 saddr " + .from + " " else "    ip saddr " + .from + " " end) else "    " end) + "meta l4proto " + .proto + " ct original proto-dst " + .port + " accept" end' <<< "$spec"
		fi
	}
	echo "#!/usr/sbin/nft -f"
	echo "# written by UMP from the profile's firewall section; nft -f after editing"
	echo "flush ruleset"
	echo "table inet filter {"
	echo "  chain input {"
	echo "    type filter hook input priority 0; policy drop;"
	echo "    iif lo accept"
	echo "    ct state established,related accept"
	echo "    ct state invalid drop"
	allow input
	echo "  }"
	echo "  # Forwarded traffic (containers, virtual machines) passes, except"
	echo "  # new connections coming in on the external interfaces: those are"
	echo "  # held to the same list, by the port they were addressed to. A"
	echo "  # port a container publishes (docker run -p) is reachable only"
	echo "  # when it is allowed here, whatever the container engine adds."
	echo "  chain forward {"
	echo "    type filter hook forward priority 0; policy accept;"
	echo "    ct state established,related accept"
	echo "    ct state invalid drop"
	echo "    iifname { ${ext:-\"ext*\"} } ct state new jump forward-in"
	echo "  }"
	echo "  chain forward-in {"
	allow forward
	echo "    drop"
	echo "  }"
	echo "  chain output {"
	echo "    type filter hook output priority 0; policy accept;"
	echo "  }"
	echo "}"
}

# --- Arch Linux: pacstrap from the mirrors, out of the monthly bootstrap ---

# Arch is a rolling release: there is no image to unpack, but the official
# bootstrap tarball (pinned by the manifest, from archive.archlinux.org)
# runs pacstrap, which installs the manifest's packages as the mirror has
# them today; pacman checks every package's signature against the Arch
# keyring. The bootstrap lives on the new disk, not in the rescue system's
# memory, and is removed afterwards.
arch_umount() {
	local m
	kill_chrooted
	for m in mnt dev sys proc; do
		mountpoint -q "$arch_boot/$m" && umount -R -l "$arch_boot/$m" 2>/dev/null
	done
	return 0
}

# CachyOS: Arch Linux with CachyOS's repositories in front of Arch's,
# whose packages are built for newer CPUs (x86-64-v3: AVX2 and more) and
# win over Arch's of the same name, its own pacman and kernel among them.
# The keyring package comes pinned by its checksum, as the bootstrap
# does; it makes pacman trust the keys that sign CachyOS's packages.
cachyos_level=""

# cachyos_conf FILE Server|Include LEVEL: CachyOS's repositories in front
# of the first repository of the pacman.conf FILE: those of LEVEL
# (x86-64-v3, x86-64-v4; none for the plain x86_64 ones alone), with the
# manifest's mirror (Server, for the bootstrap's stock pacman, which
# knows no $arch_v3) or CachyOS's mirror lists (Include, for the new
# system, whose pacman is CachyOS's)
cachyos_conf() {
	local file="$1" how="$2" level="$3" cmirror blocks="" r v=""
	cmirror="$(jq -r .cachyos.mirror <<< "$manifest")"
	case "$level" in x86-64-v3) v=v3 ;; x86-64-v4) v=v4 ;; esac
	if [ -n "$v" ]; then
		for r in "cachyos-$v" "cachyos-core-$v" "cachyos-extra-$v"; do
			if [ "$how" = Server ]; then
				blocks="$blocks[$r]\nServer = $cmirror/x86_64_$v/\$repo\n\n"
			else
				blocks="$blocks[$r]\nInclude = /etc/pacman.d/cachyos-$v-mirrorlist\n\n"
			fi
		done
	fi
	if [ "$how" = Server ]; then
		blocks="$blocks[cachyos]\nServer = $cmirror/x86_64/\$repo\n\n"
	else
		blocks="$blocks[cachyos]\nInclude = /etc/pacman.d/cachyos-mirrorlist\n\n"
	fi
	awk -v b="$blocks" '!done && /^\[/ && $0 != "[options]" {printf "%s", b; done = 1} {print}' "$file" > "$file.ump" \
		&& mv "$file.ump" "$file" || die "cannot add CachyOS's repositories to $file"
	grep -q '^\[cachyos\]' "$file" || die "cannot add CachyOS's repositories to $file"
}

cachyos_repos() {
	local url sum have l levels archs=x86_64
	url="$(jq -r .cachyos.keyring.url <<< "$manifest")"
	sum="$(jq -r .cachyos.keyring.sha256 <<< "$manifest")"
	# the newest of the manifest's CPU levels this machine runs, as the
	# bootstrap's glibc tells it
	levels="$(in_boot /usr/lib/ld-linux-x86-64.so.2 --help 2>/dev/null || true)"
	for l in $(jq -r '.cachyos.levels // [] | .[]' <<< "$manifest"); do
		if grep -q "$l (supported, searched)" <<< "$levels"; then
			cachyos_level="$l"
			break
		fi
	done
	if [ -n "$cachyos_level" ]; then
		log "CachyOS: this CPU runs $cachyos_level; its repositories first"
		archs="x86_64 x86_64_${cachyos_level#x86-64-}"
	else
		cachyos_level=x86-64
		log "CachyOS: this CPU runs none of $(jq -r '.cachyos.levels // [] | join(", ")' <<< "$manifest"); the plain x86_64 repository"
	fi
	log "CachyOS keyring"
	curl -fsSL -o "$arch_boot/root/cachyos-keyring.pkg.tar.zst" "$url" || die "cannot download $url"
	have="$(sha256sum "$arch_boot/root/cachyos-keyring.pkg.tar.zst" | cut -d' ' -f1)"
	[ "$have" = "$sum" ] || die "checksum mismatch for $url: manifest $sum, download $have"
	{ in_boot pacman -U --noconfirm /root/cachyos-keyring.pkg.tar.zst && in_boot pacman-key --populate cachyos; } >> "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "the CachyOS keyring could not be set up"; }
	rm -f "$arch_boot/root/cachyos-keyring.pkg.tar.zst"
	cachyos_conf "$arch_boot/etc/pacman.conf" Server "$cachyos_level"
	sed -i "s/^#*Architecture = .*/Architecture = $archs/" "$arch_boot/etc/pacman.conf"
}

arch_pacstrap() {
	local url sum mirror release have pkgs plog
	url="$(jq -r .bootstrap.url <<< "$manifest")"
	sum="$(jq -r .bootstrap.sha256 <<< "$manifest")"
	release="$(jq -r .bootstrap.release <<< "$manifest")"
	mirror="$(jq -r .mirror <<< "$manifest")"
	mapfile -t pkgs < <(jq -r '.packages[]' <<< "$manifest")

	arch_boot="$TARGET/.ump-arch-bootstrap"
	log "downloading the Arch Linux bootstrap $release"
	mkdir -p "$arch_boot"
	have="$(mktemp)"
	curl -fsSL "$url" | tee >(sha256sum | cut -d' ' -f1 > "$have") | zstd -dc | tar --warning=no-unknown-keyword -xpf - -C "$arch_boot" --strip-components=1 \
		|| die "cannot download or unpack $url"
	for _ in $(seq 1 100); do [ -s "$have" ] && break; sleep 0.1; done
	[ "$(cat "$have")" = "$sum" ] || die "checksum mismatch for $url: manifest $sum, download $(cat "$have")"
	rm -f "$have"

	printf 'Server = %s/$repo/os/$arch\n' "$mirror" > "$arch_boot/etc/pacman.d/mirrorlist"
	# parallel downloads; and no free space check, which needs the
	# bootstrap's / to be a mount point (the installer checked the root's
	# size already; the new system keeps Arch's pacman.conf with the check)
	sed -i -e 's/^#ParallelDownloads.*/ParallelDownloads = 5/' -e 's/^CheckSpace/#CheckSpace/' "$arch_boot/etc/pacman.conf"
	rm -f "$arch_boot/etc/resolv.conf"
	chroot_resolv "$arch_boot/etc/resolv.conf"
	# the new system with everything mounted in it (/boot, /boot/efi) as
	# the bootstrap's /mnt, first, while the bootstrap has no mounts of its
	# own yet; private, so that nothing mounted later shows up twice. /sys
	# and /dev as arch-chroot binds them: slaves, so that unmounting them
	# never reaches the rescue system's own.
	mkdir -p "$arch_boot/mnt"
	mount --rbind "$TARGET" "$arch_boot/mnt"
	mount --make-rprivate "$arch_boot/mnt"
	mount -t proc proc "$arch_boot/proc"
	mount --rbind /sys "$arch_boot/sys"
	mount --make-rslave "$arch_boot/sys"
	mount --rbind /dev "$arch_boot/dev"
	mount --make-rslave "$arch_boot/dev"
	in_boot() { chroot "$arch_boot" /usr/bin/env -i HOME=/root LANG=C.UTF-8 PATH=/usr/local/sbin:/usr/local/bin:/usr/bin "$@"; }

	log "pacman keyring"
	plog="$TARGET/.ump-pacstrap.log"
	{ in_boot pacman-key --init && in_boot pacman-key --populate archlinux; } > "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "the pacman keyring could not be set up"; }
	[ -z "$(jq -r '.cachyos // empty' <<< "$manifest")" ] || cachyos_repos
	# the newest keys first: the bootstrap's may predate who signs today
	in_boot pacman -Sy --noconfirm archlinux-keyring >> "$plog" 2>&1 \
		|| { tail -n 20 "$plog" >&2; die "cannot update archlinux-keyring from $mirror"; }

	log "pacstrap: ${#pkgs[@]} packages from $mirror (this takes a few minutes)"
	# without -K: the new system takes the bootstrap's keyring, made for
	# this installation a moment ago, instead of making and filling another
	in_boot pacstrap /mnt "${pkgs[@]}" >> "$plog" 2>&1 \
		|| { tail -n 40 "$plog" >&2; die "pacstrap failed"; }
	# the new system's pacman.conf (CachyOS's pacman's, which knows only
	# Arch's repositories) gets CachyOS's in front, from their mirror lists
	if [ -n "$cachyos_level" ]; then
		cachyos_conf "$TARGET/etc/pacman.conf" Include "$cachyos_level"
		sed -i 's/^#*Architecture = .*/Architecture = auto/' "$TARGET/etc/pacman.conf"
	fi

	arch_umount
	# the new system was mounted in there: nothing may be left mounted
	# before the bootstrap is deleted
	if awk -v d="$arch_boot/" 'index($2, d) == 1 {found = 1} END {exit !found}' /proc/mounts; then
		die "something is still mounted under $arch_boot; not deleting it (the new system is mounted in there)"
	fi
	rm -rf "$arch_boot"
	arch_boot=""
	mkdir -p "$TARGET/var/log"
	mv "$plog" "$TARGET/var/log/ump-pacstrap.log"
	image_version="rolling, packages of $(date -u +%Y-%m-%d)${cachyos_level:+, CachyOS repositories $cachyos_level}"
	image_kernel="$(ls "$TARGET/usr/lib/modules" | head -n 1)"
}

# --- an image (Ubuntu, Debian), or Arch Linux: then configured in place ----

install_image() {
if [ "$family" = arch ]; then
	arch_pacstrap
else
log "downloading and unpacking $image_url/rootfs.tar.zst"
sumfile="$(mktemp)"
rm -f "$sumfile"
curl -fsSL "$image_url/rootfs.tar.zst" \
	| tee >(sha256sum | cut -d' ' -f1 > "$sumfile") \
	| zstd -dc \
	| tar --xattrs --xattrs-include='*' --acls --numeric-owner -xpf - -C "$TARGET"
for _ in $(seq 1 100); do [ -s "$sumfile" ] && break; sleep 0.1; done
have_sum="$(cat "$sumfile" 2>/dev/null || true)"
rm -f "$sumfile"
[ "$have_sum" = "$want_sum" ] || die "checksum mismatch for rootfs.tar.zst: manifest $want_sum, download ${have_sum:-?}; nothing was configured"
fi
sync

# --- the machine's own configuration ------------------------------------

log "configuring $hostname"
[ -f "$TARGET/usr/share/zoneinfo/$timezone" ] || die "unknown time zone $timezone"
echo "$hostname" > "$TARGET/etc/hostname"
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n127.0.1.1\t%s\n' "$hostname" > "$TARGET/etc/hosts"
ln -sf "/usr/share/zoneinfo/$timezone" "$TARGET/etc/localtime"
echo "$timezone" > "$TARGET/etc/timezone"

if [ "$keep" = 1 ]; then
kd_fstab > "$TARGET/etc/fstab"
kd_carry
else
{
	echo "# /etc/fstab, written by UMP (layout $layout_name over ${disks[*]})"
	for m in "${MOUNTS[@]}"; do
		set -- $m
		uuid="$(blkid -s UUID -o value "$2")"
		case "$1:$3" in
		/boot/efi:*) printf 'UUID=%s  %s  vfat  umask=0077  0 1\n' "$uuid" "$1" ;;
		/:ext4) printf 'UUID=%s  /  ext4  defaults,errors=remount-ro  0 1\n' "$uuid" ;;
		/:*) printf 'UUID=%s  /  %s  defaults  0 1\n' "$uuid" "$3" ;;
		*) printf 'UUID=%s  %s  %s  defaults  0 2\n' "$uuid" "$1" "$3" ;;
		esac
	done
	for sw in "${SWAPS[@]:-}"; do [ -n "$sw" ] && printf 'UUID=%s  none  swap  sw  0 0\n' "$(blkid -s UUID -o value "$sw")"; done
} > "$TARGET/etc/fstab"
fi

if [ "$family" = arch ]; then
# Arch: systemd-networkd itself, the same configuration as netplan writes
# on Ubuntu and Debian
mkdir -p "$TARGET/etc/systemd/network"
networkd_files "$TARGET/etc/systemd/network"
ln -sf ../run/systemd/resolve/stub-resolv.conf "$TARGET/etc/resolv.conf"
echo "LANG=C.UTF-8" > "$TARGET/etc/locale.conf"
echo "KEYMAP=us" > "$TARGET/etc/vconsole.conf"
else
mkdir -p "$TARGET/etc/netplan"
netplan_yaml > "$TARGET/etc/netplan/10-ump.yaml"
chmod 600 "$TARGET/etc/netplan/10-ump.yaml"
# the image's resolved configuration has the public resolvers; others
# come from the specification's uplink
if [ "$dns_source" != public ]; then
	mkdir -p "$TARGET/etc/systemd/resolved.conf.d"
	resolved_conf > "$TARGET/etc/systemd/resolved.conf.d/ump.conf"
fi
fi

if [ "$family" = arch ]; then
	# Arch's GRUB reads /etc/default/grub alone (no grub.d)
	[ -f "$TARGET/etc/default/grub" ] || die "the new system has no /etc/default/grub"
	sed -i "/^GRUB_CMDLINE_LINUX_DEFAULT=/c\\
# UMP: the kernel command line of this machine: mitigations=off and\\
# ipv6.disable=1 unless the profile turns them on, kho=off always (see\\
# docs/oses.md). Change it here, run grub-mkconfig -o /boot/grub/grub.cfg\\
# and reboot.\\
GRUB_CMDLINE_LINUX_DEFAULT=\"$kernel_cmdline\"" "$TARGET/etc/default/grub"
else
mkdir -p "$TARGET/etc/default/grub.d"
cat > "$TARGET/etc/default/grub.d/ump-cmdline.cfg" <<GRUB
# UMP: the kernel command line of this machine. By default the CPU
# vulnerability mitigations are off (mitigations=off) and IPv6 is off
# (ipv6.disable=1): both cost performance, and most servers need neither.
# To turn one back on, remove it from the line below and run update-grub;
# for IPv6 also add the address and route to /etc/netplan/10-ump.yaml
# (netplan apply). Then reboot. kho=off keeps the kernel's kexec handover
# from reserving a large part of the memory for live updates, which no
# server needs and which stops ump from re-entering its rescue system.
GRUB_CMDLINE_LINUX_DEFAULT="$kernel_cmdline"
GRUB
fi

: > "$TARGET/etc/machine-id"
rm -f "$TARGET/var/lib/dbus/machine-id"

mount --bind /dev "$TARGET/dev"
mount -t devpts devpts "$TARGET/dev/pts" 2>/dev/null || true
mount -t proc proc "$TARGET/proc"
mount -t sysfs sysfs "$TARGET/sys"
mount --bind /run "$TARGET/run"
in_target() { chroot "$TARGET" /usr/bin/env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive "$@"; }
run_in_target() { # run_in_target WHAT COMMAND...: the output is shown only when it fails
	local what="$1" out; shift
	if ! out="$(in_target "$@" 2>&1)"; then printf '%s\n' "$out" >&2; die "$what failed"; fi
}
in_target ssh-keygen -A >/dev/null

# the shell setup and the message of the day, the same on every system:
# from the storage, else the image's own stay
if curl -fsSL -o "$TARGET/etc/skel/.bashrc.ump" "$storage/install/bashrc.sh"; then
	mv "$TARGET/etc/skel/.bashrc.ump" "$TARGET/etc/skel/.bashrc"
	cp "$TARGET/etc/skel/.bashrc" "$TARGET/root/.bashrc"
else
	rm -f "$TARGET/etc/skel/.bashrc.ump"
	warn "cannot fetch $storage/install/bashrc.sh; the image's shell setup stays"
fi
if [ "$family" = arch ]; then
	# Arch has no update-motd.d: the login shell shows it, as on NixOS
	mkdir -p "$TARGET/usr/local/bin" "$TARGET/etc/profile.d"
	if curl -fsSL -o "$TARGET/usr/local/bin/ump-motd" "$storage/install/motd.sh"; then
		chmod 755 "$TARGET/usr/local/bin/ump-motd"
		cat > "$TARGET/etc/profile.d/ump-motd.sh" <<'MOTD'
# the message of the day of UMP, once per SSH login
if [ -n "${SSH_CONNECTION:-}" ] && [ -t 1 ] && [ -z "${UMP_MOTD_SHOWN:-}" ]; then
	export UMP_MOTD_SHOWN=1
	/usr/local/bin/ump-motd
fi
MOTD
	else
		warn "cannot fetch $storage/install/motd.sh; no message of the day"
	fi
else
mkdir -p "$TARGET/etc/update-motd.d"
if curl -fsSL -o "$TARGET/etc/update-motd.d/00-ump" "$storage/install/motd.sh"; then
	chmod 755 "$TARGET/etc/update-motd.d/00-ump"
	ln -sf /etc/update-motd.d/00-ump "$TARGET/usr/local/bin/ump-motd"
else
	warn "cannot fetch $storage/install/motd.sh; the image's message of the day stays"
fi
# Ubuntu's news for the message of the day comes from 50-motd-news, which
# UMP's images leave out; its timer would start the missing script every
# few hours and leave the system degraded (images up to 2026-10-19 still
# have the timer on)
if [ ! -e "$TARGET/etc/update-motd.d/50-motd-news" ] && { [ -e "$TARGET/usr/lib/systemd/system/motd-news.timer" ] || [ -e "$TARGET/lib/systemd/system/motd-news.timer" ]; }; then
	rm -f "$TARGET/etc/systemd/system/timers.target.wants/motd-news.timer"
	ln -sf /dev/null "$TARGET/etc/systemd/system/motd-news.timer"
fi
fi

# --- users --------------------------------------------------------------

log "users"
[ "$keep" = 0 ] || kd_mount_homes
# the group of the administrators: sudo on Ubuntu and Debian, wheel on Arch
sudo_group=sudo; [ "$family" = arch ] && sudo_group=wheel
nusers="$(jq '.users | length' <<< "$spec")"
for ((n = 0; n < nusers; n++)); do
	u="$(jq -c ".users[$n]" <<< "$spec")"
	name="$(jq -r .name <<< "$u")"; shell="$(jq -r '.shell // "/bin/bash"' <<< "$u")"; sudo="$(jq -r '.sudo // false' <<< "$u")"
	if [ "$name" = root ]; then
		home=/root
	else
		if ! in_target getent passwd "$name" >/dev/null 2>&1; then
			# (keep-data: with the ID its home directory on a kept volume has)
			uid_arg=(); [ "$keep" = 0 ] || mapfile -t uid_arg < <(kd_home_uid "$name")
			run_in_target "useradd $name" useradd -m -s "$shell" "${uid_arg[@]}" "$name"
		else
			# an account of the image (www-data, backup, daemon) has no login
			# shell: nobody would get in after the reboot
			uid="$(in_target getent passwd "$name" | cut -d: -f3)"
			if [ "$uid" -lt 1000 ] || [ "$uid" -ge 65534 ]; then
				die "user $name is a system account of the image; choose another name"
			fi
		fi
		home="$(in_target getent passwd "$name" | cut -d: -f6)"
		for g in $(jq -r '.groups // [] | .[]' <<< "$u"); do
			in_target getent group "$g" >/dev/null 2>&1 || run_in_target "groupadd $g" groupadd "$g"
			run_in_target "usermod $name" usermod -aG "$g" "$name"
		done
		if [ "$sudo" = true ]; then
			run_in_target "usermod $name" usermod -aG "$sudo_group" "$name"
			mkdir -p "$TARGET/etc/sudoers.d"
			printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$name" > "$TARGET/etc/sudoers.d/90-ump-$name"
			chmod 440 "$TARGET/etc/sudoers.d/90-ump-$name"
		fi
	fi
	[ "$keep" = 0 ] || kd_home_take "$name" "$home"
	mkdir -p -m 700 "$TARGET$home/.ssh"
	jq -r '.keys[]' <<< "$u" | awk '!seen[$0]++' > "$TARGET$home/.ssh/authorized_keys"
	chmod 600 "$TARGET$home/.ssh/authorized_keys"
	[ "$name" = root ] || in_target chown -R "$name:$name" "$home/.ssh"
done
[ "$keep" = 0 ] || kd_keys_outside
# root over SSH only when the profile lists it with keys (the image says
# no)
mkdir -p "$TARGET/etc/ssh/sshd_config.d"
rm -f "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
if jq -e '[.users[] | select(.name == "root")] | length > 0' >/dev/null <<< "$spec"; then
	printf '# UMP: the profile lists root with keys.\nPermitRootLogin prohibit-password\n' > "$TARGET/etc/ssh/sshd_config.d/10-ump-root.conf"
	log "ssh: root logs in with keys; administrator $admin"
else
	log "ssh: root cannot log in; administrator $admin (sudo)"
fi

# --- nftables, userdata -------------------------------------------------

# the network for the chroot: the resolver of the rescue system serves it,
# and no service starts there
net_on() {
	resolv_was="$(readlink "$TARGET/etc/resolv.conf" 2>/dev/null || true)"
	rm -f "$TARGET/etc/resolv.conf"
	chroot_resolv "$TARGET/etc/resolv.conf"
	printf '#!/bin/sh\nexit 101\n' > "$TARGET/usr/sbin/policy-rc.d"; chmod 755 "$TARGET/usr/sbin/policy-rc.d"
}
net_off() {
	in_target apt-get clean >/dev/null 2>&1 || true
	rm -f "$TARGET/usr/sbin/policy-rc.d" "$TARGET/etc/resolv.conf"
	[ -n "$resolv_was" ] && ln -s "$resolv_was" "$TARGET/etc/resolv.conf"
	return 0
}

if jq -e '.firewall.policy == "drop"' >/dev/null <<< "$spec" && [ ! -x "$TARGET/usr/sbin/nft" ]; then
	log "packages: nftables (the firewall needs it)"
	net_on
	run_in_target "apt-get update" apt-get update -q
	run_in_target "apt-get install" apt-get install -y -q --no-install-recommends nftables
	net_off
fi

[ "$keep" = 0 ] || kd_packages
[ "$keep" = 0 ] || kd_verify_fstab

# the userdata: each entry as a script under /etc/ump/userdata in the new
# system, run there in order as root, with network; a failing one fails
# the installation
n_userdata=$(jq -r '.userdata | length' <<< "$spec")
if [ "$n_userdata" -gt 0 ]; then
	mkdir -p "$TARGET/etc/ump/userdata"
	net_on
	for ((i = 0; i < n_userdata; i++)); do
		ud_name="$(jq -r ".userdata[$i].name" <<< "$spec")"
		ud_type="$(jq -r ".userdata[$i].type // \"bash\"" <<< "$spec")"
		[ "$ud_type" = bash ] || die "userdata $ud_name: type $ud_type is not supported by this installer (bash only)"
		ud_file="/etc/ump/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.sh"
		jq -r ".userdata[$i].script" <<< "$spec" > "$TARGET$ud_file"
		chmod 700 "$TARGET$ud_file"
		log "userdata: $ud_name ($ud_file; its output in /var/log/ump-userdata.log)"
		if ! ud_out="$(in_target env UMP_USERDATA="$ud_name" UMP_HOSTNAME="$hostname" UMP_OS="$os" UMP_PROFILE="$profile_name" UMP_ADMIN="$admin" bash "$ud_file" 2>&1)"; then
			printf '%s\n' "$ud_out" >&2; die "userdata $ud_name failed"
		fi
		printf '=== userdata %s (%s) at %s ===\n%s\n' "$ud_name" "$ud_file" "$(date -u +%FT%TZ)" "$ud_out" >> "$TARGET/var/log/ump-userdata.log"
	done
	net_off
fi

# --- firewall -----------------------------------------------------------

if jq -e '.firewall.policy == "drop"' >/dev/null <<< "$spec"; then
	log "firewall: input drop, allowing $(jq -r '[.firewall.allow[] | if .proto == "icmp" then "icmp" else "\(.proto)/\(.port)" end] | join(", ")' <<< "$spec"); the same for connections forwarded in from outside (containers' published ports)"
	nft_ruleset > "$TARGET/etc/nftables.conf"
	chmod 755 "$TARGET/etc/nftables.conf"
	if ! out="$(in_target nft -c -f /etc/nftables.conf 2>&1)"; then
		printf '%s\n' "$out" >&2
		# a syntax error is the file's fault; anything else is the rescue
		# kernel's (the check needs its nf_tables)
		case "$out" in *"Error:"*) die "the firewall rules in /etc/nftables.conf are wrong (see above)" ;; esac
		warn "could not check /etc/nftables.conf here (see above); the new system loads it at boot"
	fi
	run_in_target "enabling nftables" systemctl enable nftables.service
fi

# --- the boot loader ---------------------------------------------------

if [ "$family" = arch ]; then
	# What UMP's Ubuntu and Debian images bring from their build
	# (images/base: overlay/ and finish.sh), done here: Arch's packages
	# set nothing up and enable nothing.
	mkdir -p "$TARGET/etc/ssh/sshd_config.d" "$TARGET/etc/systemd/resolved.conf.d" "$TARGET/etc/tuned"
	cat > "$TARGET/etc/ssh/sshd_config.d/50-ump.conf" <<'SSHD'
# UMP: no passwords over SSH, and no root login. The installer writes
# 10-ump-root.conf (which wins, sshd keeps the first value it reads) with
# "PermitRootLogin prohibit-password" when the profile lists root with
# keys. The administrator is the profile's sudo user, ump by default.
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
SSHD
	grep -q '^Include /etc/ssh/sshd_config.d/\*\.conf' "$TARGET/etc/ssh/sshd_config" \
		|| sed -i '1i Include /etc/ssh/sshd_config.d/*.conf' "$TARGET/etc/ssh/sshd_config"
	resolved_conf > "$TARGET/etc/systemd/resolved.conf.d/ump.conf"
	echo throughput-performance > "$TARGET/etc/tuned/active_profile"
	echo manual > "$TARGET/etc/tuned/profile_mode"
	# the boot menu: two seconds, no other systems searched
	sed -i -e 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=2/' -e '/^#*GRUB_DISABLE_OS_PROBER=/d' "$TARGET/etc/default/grub"
	echo 'GRUB_DISABLE_OS_PROBER=true' >> "$TARGET/etc/default/grub"
	in_target passwd -l root >/dev/null 2>&1 || true
	# network, resolver, time, SSH, tuned; the activity history of sar and
	# atop, where the packages have their units
	run_in_target "enabling the services" systemctl enable systemd-networkd.service systemd-resolved.service sshd.service systemd-timesyncd.service tuned.service
	for u in sysstat.service sysstat-collect.timer sysstat-summary.timer atop.service atop-rotate.timer; do
		[ -f "$TARGET/usr/lib/systemd/system/$u" ] || continue
		in_target systemctl enable "$u" >/dev/null 2>&1 || warn "could not enable $u"
	done
fi

log "installing the boot loader"
# (keep-data: the arrays that are there, which the layout did not make)
kd_md=0; [ "$keep" = 0 ] || [ "$(jq -r .private.md <<< "$kd_plan")" != true ] || kd_md=1
if [ ${#M_LEVEL[@]} -gt 0 ] || [ "$kd_md" = 1 ]; then
	mdadm_conf=/etc/mdadm/mdadm.conf; [ "$family" = arch ] && mdadm_conf=/etc/mdadm.conf
	mkdir -p "$TARGET$(dirname "$mdadm_conf")"
	if [ "$keep" = 1 ]; then kd_mdadm_conf >> "$TARGET$mdadm_conf"; else mdadm --detail --scan >> "$TARGET$mdadm_conf"; fi
fi
need_initramfs=0
[ ${#M_LEVEL[@]} -gt 0 ] && need_initramfs=1
for k in "${!L_VG[@]}"; do [ "${L_RAID[$k]}" != none ] && need_initramfs=1; done
# keep-data: whatever the old system's storage is made of, the initramfs
# is built on it (arrays, the volumes of crypttab)
[ "$keep" = 0 ] || need_initramfs=1
if [ "$family" = arch ]; then
	# Arch's initramfs knows neither LVM nor md without its hooks, and the
	# root is always on a volume group
	hooks="lvm2"
	[ ${#M_LEVEL[@]} -gt 0 ] && hooks="mdadm_udev lvm2"
	sed -i -E "/^HOOKS=/ s/ filesystems/ $hooks filesystems/" "$TARGET/etc/mkinitcpio.conf"
	grep -q "^HOOKS=.* lvm2 filesystems" "$TARGET/etc/mkinitcpio.conf" || die "cannot add the lvm2 hook to /etc/mkinitcpio.conf"
	log "initramfs (mkinitcpio, hooks $hooks)"
	run_in_target "mkinitcpio" mkinitcpio -P
elif [ "$need_initramfs" = 1 ]; then
	log "initramfs ($([ "$keep" = 1 ] && echo "the kept storage" || echo "md or raid volumes"))"
	run_in_target "update-initramfs" update-initramfs -u -k all
fi
case "$arch" in
amd64)
	for x in "${disks[@]}"; do
		for i in "${!P_DISK[@]}"; do
			[ "${P_DISK[$i]}" = "$x" ] && [ "${P_FS[$i]}" = bios ] || continue
			run_in_target "grub-install (BIOS, $x)" grub-install --target=i386-pc --recheck "$x"
			break
		done
	done
	# keep-data: under BIOS, the disks the plan names: those /boot is on,
	# and those that carry GRUB now, where a failure is no failure of the
	# installation: the new system starts from the first; under UEFI the
	# ESP alone
	if [ "$keep" = 1 ]; then
		for x in $(jq -r '(.boot.bios_disks - .boot.bios_optional)[]' <<< "$kd_plan"); do run_in_target "grub-install (BIOS, $x)" grub-install --target=i386-pc --recheck "$x"; done
		for x in $(jq -r '.boot.bios_optional[]' <<< "$kd_plan"); do
			if ! out="$(in_target grub-install --target=i386-pc --recheck "$x" 2>&1)"; then
				printf '%s\n' "$out" | tail -n 3 >&2
				warn "grub-install on $x failed (above): it carries the old boot loader still, and a BIOS that starts from it does not find the new system"
			fi
		done
	fi
	[ "$has_efi" = 1 ] && run_in_target "grub-install (EFI)" grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
arm64)
	[ "$has_efi" = 1 ] && run_in_target "grub-install (EFI)" grub-install --target=arm64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable --no-nvram --recheck
	;;
esac
if [ "$family" = arch ]; then
	run_in_target "grub-mkconfig" grub-mkconfig -o /boot/grub/grub.cfg
else
	run_in_target "update-grub" update-grub
fi
grep -q 'vmlinuz' "$TARGET/boot/grub/grub.cfg" || die "grub.cfg names no kernel"
}

# --- NixOS: UMP's module and the profile, installed with nixos-install ----

# a swap file on the new root while Nix evaluates and builds: evaluating
# a NixOS configuration takes about a gigabyte, which a small machine's
# rescue system (in RAM) does not have
swap_on_target() {
	local avail need=2048
	avail=$(( $(awk '/^MemAvailable:/ {print $2}' /proc/meminfo) / 1024 ))
	[ "$avail" -ge "$need" ] && return 0
	target_swap="$TARGET/.ump-install-swap"
	log "memory: $avail MB free; a swap file of $need MB on the new root while NixOS is built"
	dd if=/dev/zero of="$target_swap" bs=1M count="$need" status=none && chmod 600 "$target_swap" && mkswap "$target_swap" >/dev/null && swapon "$target_swap" \
		|| { rm -f "$target_swap"; target_swap=""; warn "no swap file; building may run out of memory"; }
}

install_nixos() {
	local channel nix_url nix_sum work nixdir nixbin p chan tools log_file ud_n i ud_name

	channel="$(jq -r .channel <<< "$manifest")"
	nix_url="$(jq -r .nix.url <<< "$manifest")"
	nix_sum="$(jq -r .nix.sha256 <<< "$manifest")"
	target_swap=""

	# Nix, from its release tarball, with the store on the new system's
	# disk from the first byte: /nix here is the new /nix
	log "downloading Nix from $nix_url"
	mkdir -p "$TARGET/nix" /nix
	mount --bind "$TARGET/nix" /nix
	work="$(mktemp -d)"
	curl -fsSL -o "$work/nix.tar.xz" "$nix_url" || die "cannot download $nix_url"
	[ "$(sha256sum "$work/nix.tar.xz" | cut -d' ' -f1)" = "$nix_sum" ] || die "checksum mismatch for $nix_url"
	tar -xJf "$work/nix.tar.xz" -C "$work"
	nixdir="$(ls -d "$work"/nix-*/ | head -n 1)"
	mkdir -p /nix/store
	cp -RPp "$nixdir"/store/* /nix/store/
	for p in "$nixdir"/store/*; do [ -x "$p/bin/nix-build" ] && nixbin="/nix/store/$(basename "$p")/bin"; done
	[ -n "${nixbin:-}" ] || die "no nix-build in $nix_url"
	# single-user Nix as root: no build users, no sandbox (the rescue system
	# has neither), and everything comes from the binary cache anyway
	export HOME=/root USER=root PATH="$nixbin:$PATH" NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
	export NIX_CONFIG=$'build-users-group =\nsandbox = false\nexperimental-features = nix-command\nfallback = true'
	"$nixbin/nix-store" --load-db < "$nixdir/.reginfo"
	rm -rf "$work"

	log "channel: $channel"
	nix-channel --add "$channel" nixos >/dev/null
	nix-channel --update nixos >/dev/null 2>&1 || die "nix-channel --update failed for $channel"
	chan=/nix/var/nix/profiles/per-user/root/channels/nixos
	[ -d "$chan" ] || chan="$(nix-instantiate --eval --expr '<nixos>' 2>/dev/null)"
	nixos_rev="$(cat "$chan/.git-revision" 2>/dev/null || true)"
	export NIX_PATH="nixpkgs=$chan:nixos=$chan:nixos-config=$TARGET/etc/nixos/configuration.nix"
	tools="$(nix-build '<nixpkgs>' -A nixos-install-tools --no-out-link 2>/dev/null)" || die "cannot get nixos-install from $channel"
	export PATH="$tools/bin:$PATH"

	log "configuration: /etc/nixos (hardware-configuration.nix, ump/, userdata/)"
	"$tools/bin/nixos-generate-config" --root "$TARGET" >/dev/null 2>&1 || die "nixos-generate-config failed"
	# only now: the hardware configuration must not take the swap file
	swap_on_target
	mkdir -p "$TARGET/etc/nixos/ump" "$TARGET/etc/nixos/userdata"
	curl -fsSL -o "$TARGET/etc/nixos/ump/default.nix" "$storage/install/nixos/ump.nix" || die "cannot fetch $storage/install/nixos/ump.nix"
	curl -fsSL -o "$TARGET/etc/nixos/ump/motd.sh" "$storage/install/motd.sh" || die "cannot fetch $storage/install/motd.sh"
	curl -fsSL -o "$TARGET/etc/nixos/ump/bashrc.sh" "$storage/install/bashrc.sh" || die "cannot fetch $storage/install/bashrc.sh"
	printf '%s\n' "$spec" | jq 'del(.userdata)' > "$TARGET/etc/nixos/ump/spec.json"
	nft_ruleset > "$TARGET/etc/nixos/ump/nftables.conf"
	{
		printf '%s\n' "${IF_MAC[@]}" | paste -d'\t' - <(printf '%s\n' "${IF_NAME[@]}") <(printf '%s\n' "${IF_KIND[@]}") \
			| jq -R 'split("\t") | {mac: .[0], name: .[1], kind: .[2]}' | jq -s .
	} > "$TARGET/etc/nixos/ump/interfaces.json"
	# (true at the end: the last disk need not have a bios part)
	bios_disks="$(for x in "${disks[@]}"; do for i in "${!P_DISK[@]}"; do [ "${P_DISK[$i]}" = "$x" ] && [ "${P_FS[$i]}" = bios ] && { echo "$x"; break; }; done; done; true)"
	lvm_raid=false; for k in "${!L_VG[@]}"; do [ "${L_RAID[$k]}" != none ] && lvm_raid=true; done
	jq -n --arg os "$os" --arg state_version "$(jq -r .state_version <<< "$manifest")" \
		--slurpfile interfaces "$TARGET/etc/nixos/ump/interfaces.json" --argjson net "$(nixos_net_facts)" \
		--arg cmdline "$kernel_cmdline" --argjson rename "$([ "$rename" = yes ] && echo true || echo false)" \
		--argjson efi "$([ "$has_efi" = 1 ] && echo true || echo false)" --arg bios "$bios_disks" \
		--argjson md "$([ ${#M_LEVEL[@]} -gt 0 ] && echo true || echo false)" --arg mdadm_conf "$( [ ${#M_LEVEL[@]} -gt 0 ] && mdadm --detail --scan || true)" \
		--arg swaps "$(for sw in "${SWAPS[@]:-}"; do [ -n "$sw" ] && blkid -s UUID -o value "$sw"; done)" \
		--argjson lvm_raid "$lvm_raid" '{
		os: $os, state_version: $state_version, rename: $rename, interfaces: $interfaces[0]} + $net + {
		kernel_params: ($cmdline | split(" ") | map(select(. != ""))),
		efi: $efi, bios_disks: ($bios | split("\n") | map(select(. != ""))),
		md: $md, mdadm_conf: $mdadm_conf, lvm_raid: $lvm_raid,
		swaps: ($swaps | split("\n") | map(select(. != "")))}' > "$TARGET/etc/nixos/ump/facts.json"
	rm -f "$TARGET/etc/nixos/ump/interfaces.json"

	ud_n=$(jq -r '.userdata | length' <<< "$spec")
	for ((i = 0; i < ud_n; i++)); do
		ud_name="$(jq -r ".userdata[$i].name" <<< "$spec")"
		jq -r ".userdata[$i].script" <<< "$spec" > "$TARGET/etc/nixos/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.nix"
		log "userdata: $ud_name (/etc/nixos/userdata/$(printf '%02d' "$((i + 1))")-$ud_name.nix)"
	done
	cat > "$TARGET/etc/nixos/configuration.nix" <<'NIX'
# Written by ump server reinstall. The machine is ./ump (what UMP set up
# from the profile: users and keys, network, firewall, boot loader) plus
# the userdata modules in ./userdata, in the order of their names; NixOS
# merges them, and the userdata wins over UMP's defaults. Change the
# machine here or in a userdata module, then nixos-rebuild switch.
{ lib, ... }:
{
  imports = [ ./hardware-configuration.nix ./ump ]
    ++ map (f: ./userdata + "/${f}") (lib.attrNames (lib.filterAttrs (n: t: t == "regular" && lib.hasSuffix ".nix" n) (builtins.readDir ./userdata)));
}
NIX

	log "nixos-install (downloads from the binary cache; this takes a few minutes)"
	log_file="$TARGET/var/log/ump-nixos-install.log"
	mkdir -p "$(dirname "$log_file")"
	# once more on a failure: a transient one (a download, a bus error seen
	# once on an OVHcloud VPS) passes the second time, and nixos-install
	# carries on with what the store already has
	if ! "$tools/bin/nixos-install" --root "$TARGET" --no-root-passwd > "$log_file" 2>&1; then
		tail -n 3 "$log_file" >&2
		warn "nixos-install failed (above); trying once more"
		if ! "$tools/bin/nixos-install" --root "$TARGET" --no-root-passwd >> "$log_file" 2>&1; then
			tail -n 40 "$log_file" >&2
			die "nixos-install failed twice (the whole output is in /var/log/ump-nixos-install.log of the new system)"
		fi
	fi
	image_version="$(jq -r .version <<< "$manifest")${nixos_rev:+ (nixpkgs ${nixos_rev:0:12})}"
	image_kernel="$(basename "$(dirname "$(readlink -f "$TARGET/nix/var/nix/profiles/system/kernel")")" | sed 's/^[a-z0-9]*-linux-//')"
	[ -n "$target_swap" ] && { swapoff "$target_swap" 2>/dev/null; rm -f "$target_swap"; }
	umount /nix 2>/dev/null || umount -l /nix
}

case "$family" in
image|arch) install_image ;;
nixos) install_nixos ;;
esac

mkdir -p "$TARGET/etc/ump"
printf '%s\n' "$spec" | jq . > "$TARGET/etc/ump/spec.json"
chmod 600 "$TARGET/etc/ump/spec.json"
cat > "$TARGET/etc/ump/install" <<META
os=$os
image_version=$image_version
image_kernel=$image_kernel
installed=$(date -u +%Y-%m-%dT%H:%M:%SZ)
profile=$profile_name
layout=$layout_name
disks=${disks[*]}
boot_disk=$boot_disk
root=$root_dev
hostname=$hostname
admin=$admin
interfaces=$(for i in "${!IF_MAC[@]}"; do printf '%s=%s ' "${IF_NAME[$i]}" "${IF_MAC[$i]}"; done | sed 's/ $//')
uplink=$uplink
ipv6=$([ "$ipv6" = true ] && echo on || echo off)
mitigations=$([ "$mitigations" = true ] && echo on || echo off)
firewall=$(jq -r '.firewall.policy' <<< "$spec")
userdata=$(jq -r '.userdata | map(.name) | join(",")' <<< "$spec")
private=$private_list
private_network=$private_network
private_address=$private_address
META
[ -z "$uplink_spec" ] || net_record >> "$TARGET/etc/ump/install"
if [ "$keep" = 1 ]; then
	# what was kept, and the plan itself; the old system's files for a
	# second attempt are not needed any more
	kd_record >> "$TARGET/etc/ump/install"
	kd_public > "$TARGET/etc/ump/keep-data.json"
	kd_done
fi
chmod 644 "$TARGET/etc/ump/install"

# the record for the caller (--record): the installation and the host
# keys of the new system, read before it is unmounted
if [ -n "$record" ]; then
	# NixOS's sshd makes its host keys at the first boot; made now, they
	# can be recorded, and the first connection can check them
	if [ "$family" = nixos ]; then
		mkdir -p "$TARGET/etc/ssh"
		for t in ed25519 rsa; do
			k="$TARGET/etc/ssh/ssh_host_${t}_key"
			bits=(); [ "$t" = rsa ] && bits=(-b 4096)
			[ -f "$k" ] || ssh-keygen -q -t "$t" "${bits[@]}" -N '' -C "root@$hostname" -f "$k" || warn "could not make the SSH host key $k"
		done
	fi
	mkdir -p "$record"
	rm -f "$record/install" "$record/keep-data.json" "$record"/ssh_host_*_key.pub
	cp "$TARGET/etc/ump/install" "$record/install"
	[ "$keep" = 0 ] || cp "$TARGET/etc/ump/keep-data.json" "$record/keep-data.json"
	cp "$TARGET"/etc/ssh/ssh_host_*_key.pub "$record/" 2>/dev/null || warn "the new system has no SSH host keys to record"
	log "record: $record"
fi

sync
cleanup
trap - EXIT
vgchange -an >/dev/null 2>&1 || true
if [ "$keep" = 1 ]; then
	log "installed: $os ${image_version:-?} (kernel ${image_kernel:-?}) as $hostname, root on $root_dev ($root_mb MB; keep-data: $(jq -r '"\(.format | length) formatted, \([.keep[] | select(.device != null)] | length) kept, \(.aside | length) aside, \(.untouched | length) untouched"' <<< "$kd_plan")); reboot to start it"
else
log "installed: $os ${image_version:-?} (kernel ${image_kernel:-?}) as $hostname, root on $root_dev ($root_mb MB; layout $layout_name over ${#disks[@]} disk(s)); reboot to start it"
fi
