package pxe

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ofzhur/ump/internal/profile"
)

// sample is the pxe.yaml of docs/pxe.md, more or less.
const sample = `# the machines ump boots over the network
unknown: ignore
defaults:
  mode: local
  rescue-keys: [oleg]
  console: ttyS1,115200n8
  network:
    ipv4: dhcp
    dns: dhcp
sites:
  fra1:
    description: Frankfurt, rack 1, VLAN 40
    jump: ump@pxe1
    network:
      ipv4: {gateway: 192.0.2.1}
      dns: [192.0.2.53, 192.0.2.54]
  lab:
    unknown: local
    mode: rescue
machines:
  web-07:
    site: fra1
    mac: 0C-C4-7A-AA-BB-01
    firmware: uefi
    profile: web
    group: [web]
    private-ip: 10.0.0.17
    disks: [nvme-eui.0025388b91c0a1b2]
    network:
      ipv4: {address: 192.0.2.17/24}
  db-01:
    site: fra1
    mac: [0c:c4:7a:aa:bb:10, 0cc47aaabb11]
    arch: amd64
    layout: raid10
    rescue-keys: [laptop]
    network:
      uplink: 0c:c4:7a:aa:bb:11
      ipv4: dhcp
    address: 192.0.2.30
  bench:
    site: lab
    mac: 52:54:00:12:34:56
    hostname: bench.lab.example.net
    console: none
    note: the one under the desk
`

func parse(t *testing.T, text string) *Config {
	t.Helper()
	c, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseAndMerge(t *testing.T) {
	c := parse(t, sample)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); strings.Join(got, ",") != "web-07,db-01,bench" {
		t.Errorf("names in the file's order: %v", got)
	}
	if got := c.SiteNames(); strings.Join(got, ",") != "fra1,lab" {
		t.Errorf("sites: %v", got)
	}
	web := c.Machine("web-07")
	want := &Machine{
		Name: "web-07", Site: "fra1", MACs: []string{"0c:c4:7a:aa:bb:01"}, Mode: ModeLocal, Hostname: "web-07",
		Firmware: FirmwareUEFI, Console: "ttyS1,115200n8", RescueKeys: []string{"oleg"}, Jump: "ump@pxe1",
		Network: profile.UplinkSpec{
			MAC:  "0c:c4:7a:aa:bb:01",
			IPv4: profile.UplinkAddress{Mode: profile.UplinkStatic, Address: "192.0.2.17/24", Gateway: "192.0.2.1"},
			IPv6: profile.UplinkAddress{Mode: profile.UplinkNone},
			DNS:  []string{"192.0.2.53", "192.0.2.54"},
		},
		Plan: c.Machines["web-07"].Values,
	}
	if !reflect.DeepEqual(web, want) {
		t.Errorf("web-07:\n got %+v\nwant %+v", web, want)
	}
	if *web.Plan.Profile != "web" || strings.Join(web.Plan.Group, ",") != "web" || strings.Join(web.Plan.PrivateIP, ",") != "10.0.0.17" {
		t.Errorf("web-07's plan: %+v", web.Plan)
	}
	db := c.Machine("db-01")
	if strings.Join(db.MACs, ",") != "0c:c4:7a:aa:bb:10,0c:c4:7a:aa:bb:11" {
		t.Errorf("db-01's MACs, normalized: %v", db.MACs)
	}
	// its own keys replace the defaults' (a list replaces a list), its
	// DHCP the site's gateway, and the site's resolvers stay
	wantNet := profile.UplinkSpec{MAC: "0c:c4:7a:aa:bb:11", IPv4: profile.UplinkAddress{Mode: profile.UplinkDHCP},
		IPv6: profile.UplinkAddress{Mode: profile.UplinkNone}, DNS: []string{"192.0.2.53", "192.0.2.54"}}
	if strings.Join(db.RescueKeys, ",") != "laptop" || !reflect.DeepEqual(db.Network, wantNet) || db.Address != "192.0.2.30" {
		t.Errorf("db-01: %+v", db)
	}
	// the site's mode, the defaults' DHCP with the lease's resolvers
	bench := c.Machine("bench")
	if bench.Mode != ModeRescue || bench.Hostname != "bench.lab.example.net" || bench.Console != ConsoleNone || bench.Jump != "" {
		t.Errorf("bench: %+v", bench)
	}
	if bench.Network.IPv4.Mode != profile.UplinkDHCP || !bench.Network.DNSFromDHCP || bench.Network.MAC != "52:54:00:12:34:56" {
		t.Errorf("bench's network: %+v", bench.Network)
	}
	if c.Machine("nope") != nil {
		t.Error("a machine without an entry")
	}
}

func TestDefaultsWithoutSites(t *testing.T) {
	c := parse(t, "machines:\n  m1:\n    mac: 0c:c4:7a:aa:bb:01\n  m2:\n    mac: 0c:c4:7a:aa:bb:02\n    site: default\n    network:\n      ipv4: {address: 192.0.2.5/24}\n")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.SiteNames(); strings.Join(got, ",") != DefaultSite {
		t.Errorf("sites: %v", got)
	}
	m1, m2 := c.Machine("m1"), c.Machine("m2")
	if m1.Site != DefaultSite || m1.Mode != ModeLocal || m1.Network.IPv4.Mode != profile.UplinkDHCP || !m1.Network.DNSFromDHCP {
		t.Errorf("m1: %+v", m1)
	}
	// a static address without resolvers keeps ump's public ones
	if m2.Network.IPv4.Mode != profile.UplinkStatic || m2.Network.DNSFromDHCP || m2.Network.DNS != nil {
		t.Errorf("m2: %+v", m2.Network)
	}
	for _, text := range []string{"", "# nothing yet\n", "machines:\n", "~\n"} {
		c := parse(t, text)
		if err := c.Validate(); err != nil || len(c.Names()) != 0 {
			t.Errorf("%q: %v %v", text, c.Names(), err)
		}
	}
}

func TestAddressingMerge(t *testing.T) {
	text := `defaults:
  network:
    ipv4: {gateway: 192.0.2.1}
    ipv6: auto
sites:
  s:
    network:
      ipv4: {gateway: 192.0.2.254}
      ipv6: {gateway: "fe80::1"}
machines:
  a:
    site: s
    mac: 0c:c4:7a:aa:bb:01
    network:
      ipv4: {address: 192.0.2.10/24}
      ipv6: {address: "2001:db8::10/64"}
  b:
    site: s
    mac: 0c:c4:7a:aa:bb:02
    network:
      ipv4: none
      ipv6: auto
      dns: "2001:db8::53"
`
	c := parse(t, text)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	a, b := c.Machine("a").Network, c.Machine("b").Network
	if a.IPv4 != (profile.UplinkAddress{Mode: profile.UplinkStatic, Address: "192.0.2.10/24", Gateway: "192.0.2.254"}) ||
		a.IPv6 != (profile.UplinkAddress{Mode: profile.UplinkStatic, Address: "2001:db8::10/64", Gateway: "fe80::1"}) {
		t.Errorf("a: %+v", a)
	}
	if b.IPv4.Mode != profile.UplinkNone || b.IPv6.Mode != profile.UplinkAuto || strings.Join(b.DNS, ",") != "2001:db8::53" || b.DNSFromDHCP {
		t.Errorf("b: %+v", b)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct{ text, err string }{
		{"machines: {}\n---\nmachines: {}\n", "line 2: pxe.yaml is one YAML document"},
		{"- a\n", "line 1: pxe.yaml is a mapping of unknown, defaults, sites and machines"},
		{"servers: {}\n", "line 1: servers is no part of pxe.yaml (unknown, defaults, sites, machines)"},
		{"machines: {}\nmachines: {}\n", "line 2: machines is given twice"},
		{"machines:\n  a:\n    mac: 0c:c4:7a:aa:bb:01\n  a:\n    mac: 0c:c4:7a:aa:bb:02\n", "line 4: machines.a is given twice"},
		{"machines:\n  a:\n    mac: 0c:c4:7a:aa:bb:01\n    mac: 0c:c4:7a:aa:bb:02\n", "line 4: machines.a.mac is given twice"},
		{"machines:\n  a:\n    macs: 0c:c4:7a:aa:bb:01\n", "line 3: machines.a: macs is no key of a machine (site, mac, arch, firmware, address, note, mode, rescue-keys, console, network, profile, layout, firewall, os, hostname, private-network, private-ip, timezone, disks, ssh-key, set, user, userdata, group, ipv6, mitigations, storage)"},
		{"machines:\n  a:\n    mac:\n", "line 3: machines.a: mac has no value"},
		{"machines:\n  a:\n    via: provider\n", "line 3: machines.a: via is no key here: the mode says how the machine boots"},
		{"machines:\n  a:\n    jump: pxe1\n", "line 3: machines.a: jump is a site's (sites.NAME.jump) or for all (defaults.jump)"},
		{"machines:\n  a:\n    ipxe: undionly.kkpxe\n", "line 3: machines.a: ipxe is no key here: the site's DHCP server names the iPXE file (docs/pxe.md)"},
		{"defaults:\n  profile: web\n", "line 2: defaults: profile is a machine's alone: reinstall's values are per machine"},
		{"sites:\n  s:\n    mac: 0c:c4:7a:aa:bb:01\n", "line 3: sites.s: mac is a machine's alone"},
		{"defaults:\n  description: x\n", "line 2: defaults: description is a site's"},
		{"defaults:\n  network:\n    vlans: [40]\n", "line 3: defaults.network: vlans is reserved for a later version of ump"},
		{"defaults:\n  network:\n    mask: 24\n", "line 3: defaults.network: mask is no key of the network (uplink, ipv4, ipv6, dns)"},
		{"defaults:\n  network:\n    ipv4: {address: 192.0.2.1/24, prefix: 24}\n", "line 3: defaults.network.ipv4: prefix is no key here (address, gateway)"},
		{"defaults:\n  network: dhcp\n", "line 2: defaults.network is a mapping of uplink, ipv4, ipv6, dns"},
		{"machines:\n  a:\n    mac: {x: 1}\n", "line 3: machines.a: a value, or a list of values"},
		{"machines:\n  a:\n    ipv6: maybe\n", "line 3: machines.a: cannot unmarshal !!str `maybe` into bool"},
		{"machines:\n  a:\n    network:\n      dns: {a: b}\n", "line 4: machines.a: dns is dhcp or a list of addresses"},
		{"machines: [a]\n", "line 1: machines is a mapping of names to their values"},
		{"unknown: [x]\n", "line 1: unknown is ignore or local"},
	} {
		_, err := Parse([]byte(c.text))
		if err == nil || err.Error() != c.err {
			t.Errorf("%q:\n got %v\nwant %s", c.text, err, c.err)
		}
	}
}

func TestValidate(t *testing.T) {
	const m = "machines:\n  a:\n    mac: 0c:c4:7a:aa:bb:01\n"
	for _, c := range []struct{ text, err string }{
		{"unknown: rescue\n" + m, `line 1: unknown: "rescue" is not ignore or local`},
		{"defaults:\n  mode: install\n" + m, "line 2: defaults: mode: install is armed per machine alone, with a yes (ump config pxe apply); here local or rescue"},
		{"defaults:\n  mode: off\n" + m, `line 2: defaults: mode: "off" is not local or rescue`},
		{"defaults:\n  jump: -oProxyCommand=x\n" + m, `line 2: defaults: jump: "-oProxyCommand=x" is not an ump server's name or [user@]host[:port]`},
		{"defaults:\n  rescue-keys: [agent]\n" + m, "line 2: defaults: rescue-keys: agent would be another key for every operator who applies pxe.yaml; name the key (ump config key add)"},
		{"defaults:\n  console: tty S0\n" + m, `line 2: defaults: console: "tty S0" is not a console (ttyS1,115200n8, ttyAMA0, none)`},
		{"defaults:\n  network:\n    uplink: 0c:c4:7a:aa:bb:01\n" + m, "line 3: defaults: network.uplink: the uplink is a machine's, by one of its MACs"},
		{"defaults:\n  network:\n    ipv4: auto\n" + m, `line 3: defaults: network.ipv4: "auto" is not dhcp, none or {address: ADDRESS/PREFIX, gateway: ADDRESS}`},
		{"defaults:\n  network:\n    ipv4: {gateway: 2001:db8::1}\n" + m, `line 3: defaults: network.ipv4.gateway: "2001:db8::1" is not an IPv4 address`},
		{"defaults:\n  network:\n    dns: [192.0.2.53, x]\n" + m, `line 3: defaults: network.dns: "x" is not an IP address, nor dhcp`},
		{"sites:\n  Fra1: {}\n" + m, "line 2: sites.Fra1: a site's name is lower-case letters, digits and hyphens (it names the server's unit, ump-pxe@Fra1)"},
		{"sites:\n  s:\n    unknown: rescue\n" + m, `line 3: sites.s: unknown: "rescue" is not ignore or local`},
		{"sites:\n  s:\n    jump: a b\n" + m, `line 3: sites.s: jump: "a b" is not an ump server's name or [user@]host[:port]`},
		{"machines:\n  Web_7:\n    mac: 0c:c4:7a:aa:bb:01\n", "line 2: machines.Web_7: a machine's name is a hostname: lower-case letters, digits and hyphens, with dots between labels"},
		{"sites:\n  s: {}\n" + m, "line 4: machines.a: site is missing; with sites, every machine names its own (s)"},
		{"sites:\n  s: {}\nmachines:\n  a:\n    site: t\n    mac: 0c:c4:7a:aa:bb:01\n", "line 5: machines.a: site: t is no site of pxe.yaml (s)"},
		{"machines:\n  a:\n    site: fra1\n    mac: 0c:c4:7a:aa:bb:01\n", "line 3: machines.a: site: fra1 is no site: pxe.yaml has none, so every machine is in default (sites: names them)"},
		{"machines:\n  a:\n    note: x\n", "line 2: machines.a: mac is missing: the MACs it boots from, the first its uplink"},
		{"machines:\n  a:\n    mac: 0c:c4:7a:aa:bb\n", `line 3: machines.a: mac: "0c:c4:7a:aa:bb" is not a MAC address like 0c:c4:7a:aa:bb:01`},
		{"machines:\n  a:\n    mac: 01:00:5e:00:00:01\n", "line 3: machines.a: mac: 01:00:5e:00:00:01 is a multicast address, no interface's"},
		{"machines:\n  a:\n    mac: [0c:c4:7a:aa:bb:01, 0C-C4-7A-AA-BB-01]\n", "line 3: machines.a: mac: 0c:c4:7a:aa:bb:01 is given twice"},
		{m + "  b:\n    mac: 0cc47aaabb01\n", "line 5: machines.b: mac: 0c:c4:7a:aa:bb:01 is a's already: a MAC boots one machine"},
		{m + "    mode: install\n", "line 4: machines.a: mode: install: ump records an installation once it reaches the new system, at a static network.ipv4 or at address (the DHCP server's reservation); give one"},
		{m + "    mode: install\n    address: 192.0.2.17\n    disks: [sda]\n", "line 6: machines.a: disks: sda is not a stable name; an unattended installation takes names under /dev/disk/by-id (nvme-eui..., wwn-...) or serial:SERIAL"},
		{m + "    mode: on\n", `line 4: machines.a: mode: "on" is not local, rescue or install`},
		{m + "    arch: x86_64\n", `line 4: machines.a: arch: "x86_64" is not amd64 or arm64`},
		{m + "    firmware: efi\n", `line 4: machines.a: firmware: "efi" is not uefi or bios`},
		{m + "    hostname: a_b\n", `line 4: machines.a: hostname: "a_b" is not a hostname`},
		{m + "    private-ip: db=10.0.0.300\n", `line 4: machines.a: private-ip: "db=10.0.0.300" is not NETWORK=ADDRESS or an IPv4 address`},
		{m + "    private-ip: 10.0.0.5\n  b:\n    mac: 0c:c4:7a:aa:bb:02\n    private-ip: default=10.0.0.5\n", "line 7: machines.b: private-ip: 10.0.0.5 is a's already"},
		{m + "    network:\n      ipv4: {address: 192.0.2.17}\n", `line 5: machines.a: network.ipv4.address: "192.0.2.17" is not an IPv4 address with its prefix length, like 192.0.2.17/24`},
		{m + "    network:\n      ipv4: {address: 192.0.2.0/24}\n", "line 5: machines.a: network.ipv4: address 192.0.2.0/24 is the address of its network, no host's"},
		{m + "    network:\n      ipv4: {gateway: 192.0.2.1}\n", "line 5: machines.a: network.ipv4: a gateway (192.0.2.1) and no address; give the machine's address, or dhcp"},
		{"sites:\n  s:\n    network:\n      ipv4: {gateway: 192.0.2.1}\nmachines:\n  a:\n    site: s\n    mac: 0c:c4:7a:aa:bb:01\n", "line 6: machines.a: network.ipv4: a gateway (192.0.2.1) and no address; give the machine's address, or dhcp"},
		{m + "    network:\n      ipv4: none\n", "line 4: machines.a: network: the network has neither IPv4 nor IPv6; the new system would be unreachable"},
		{m + "    network:\n      dns: [2001:db8::53]\n", "line 5: machines.a: network.dns: dns 2001:db8::53 is an IPv6 address, and the uplink has no IPv6"},
		{m + "    network:\n      ipv4: none\n      ipv6: auto\n      dns: [192.0.2.53]\n", "line 7: machines.a: network.dns: dns 192.0.2.53 is an IPv4 address, and the uplink has no IPv4"},
		{m + "    address: 192.0.2.300\n", `line 4: machines.a: address: "192.0.2.300" is not an IP address (without a prefix length)`},
		{m + "    address: 192.0.2.18\n    network:\n      ipv4: {address: 192.0.2.17/24}\n", "line 4: machines.a: address: 192.0.2.18 is not the static address of its network (192.0.2.17); give one of them, or address alone with DHCP"},
		{m + "    address: 192.0.2.17\n  b:\n    mac: 0c:c4:7a:aa:bb:02\n    network:\n      ipv4: {address: 192.0.2.17/24}\n", "line 5: machines.b: the address 192.0.2.17 is a's already"},
		{m + "    rescue-keys: ump\n", `line 4: machines.a: rescue-keys: "ump": name your keys; UMP's own is always there`},
	} {
		cfg, err := Parse([]byte(c.text))
		if err != nil {
			t.Errorf("%q: %v", c.text, err)
			continue
		}
		if err := cfg.Validate(); err == nil || err.Error() != c.err {
			t.Errorf("%q:\n got %v\nwant %s", c.text, err, c.err)
		}
	}
}

func TestNormalizeMAC(t *testing.T) {
	for in, want := range map[string]string{
		"0c:c4:7a:aa:bb:01":  "0c:c4:7a:aa:bb:01",
		"0C-C4-7A-AA-BB-01":  "0c:c4:7a:aa:bb:01",
		"0cc47aaabb01":       "0c:c4:7a:aa:bb:01",
		"0cc4.7aaa.bb01":     "0c:c4:7a:aa:bb:01",
		" 52:54:00:12:34:56": "52:54:00:12:34:56",
	} {
		if got, err := NormalizeMAC(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0c:c4:7a:aa:bb", "ff:ff:ff:ff:ff:ff", "00:00:00:00:00:00", "0c:c4:7a:aa:bb:01:02:03", "zzc47aaabb01"} {
		if got, err := NormalizeMAC(in); err == nil {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestStableDisk(t *testing.T) {
	for d, want := range map[string]bool{
		"nvme-eui.0025388b91c0a1b2": true, "wwn-0x5000c500a1b2c3d4": true, "ata-ST4000NM0035_ZC1A2B3C": true, "serial:ZC1A2B3C": true,
		"sda": false, "nvme0n1": false, "/dev/disk/by-id/wwn-0x5": false, "serial:": false,
	} {
		if StableDisk(d) != want {
			t.Errorf("%s: %v", d, !want)
		}
	}
}

func TestValidJump(t *testing.T) {
	for j, want := range map[string]bool{
		"pxe1": true, "ump@pxe1": true, "oleg@203.0.113.5:2222": true, "a@b,c@d": true, "[2001:db8::1]:22": true,
		"": false, "-J x": false, "a b": false, "@host": false, "user@": false, "a@b@c": false, "a,,b": false,
	} {
		if validJump(j) != want {
			t.Errorf("%q: %v", j, !want)
		}
	}
}
