package profile

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUplinkJSON(t *testing.T) {
	// what install.sh reads (network.uplink): the field names are its
	s := &Spec{Network: NetworkSpec{Rename: true}}
	data, err := json.Marshal(s.Network)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"rename":true}` {
		t.Errorf("without an uplink: %s", data)
	}
	s.Network.Uplink = &UplinkSpec{
		MAC:  "0c:c4:7a:aa:bb:11",
		IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24", Gateway: "192.0.2.1"},
		IPv6: UplinkAddress{Mode: UplinkNone},
		DNS:  []string{"192.0.2.53", "192.0.2.54"},
	}
	data, _ = json.Marshal(s.Network)
	if want := `{"rename":true,"uplink":{"mac":"0c:c4:7a:aa:bb:11","ipv4":{"mode":"static","address":"192.0.2.17/24","gateway":"192.0.2.1"},"ipv6":{"mode":"none"},"dns":["192.0.2.53","192.0.2.54"]}}`; string(data) != want {
		t.Errorf("static:\n got %s\nwant %s", data, want)
	}
	s.Network.Uplink = &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkDHCP}, DNSFromDHCP: true}
	data, _ = json.Marshal(s.Network.Uplink)
	if want := `{"ipv4":{"mode":"dhcp"},"ipv6":{"mode":""},"dns_from_dhcp":true}`; string(data) != want {
		t.Errorf("dhcp:\n got %s\nwant %s", data, want)
	}
	var back UplinkSpec
	if err := json.Unmarshal(data, &back); err != nil || back.IPv4.Mode != UplinkDHCP || !back.DNSFromDHCP || back.IPv6.mode() != UplinkNone {
		t.Errorf("round trip: %v, %+v", err, back)
	}
}

func TestValidateUplink(t *testing.T) {
	static4 := UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24", Gateway: "192.0.2.1"}
	dhcp := UplinkAddress{Mode: UplinkDHCP}
	auto := UplinkAddress{Mode: UplinkAuto}
	none := UplinkAddress{Mode: UplinkNone}
	private := []PrivateSpec{{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.17"}}
	for _, c := range []struct {
		name    string
		uplink  *UplinkSpec
		ipv6    bool
		private []PrivateSpec
		err     string
	}{
		{"absent", nil, false, nil, ""},
		{"static", &UplinkSpec{MAC: "0c:c4:7a:aa:bb:11", IPv4: static4, IPv6: none, DNS: []string{"192.0.2.53"}}, false, private, ""},
		{"static without gateway or mac", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24"}}, false, nil, ""},
		{"a /32 with an on-link gateway", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "203.0.113.10/32", Gateway: "172.31.1.1"}}, false, nil, ""},
		{"a /31", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.0/31", Gateway: "192.0.2.1"}}, false, nil, ""},
		{"dhcp", &UplinkSpec{IPv4: dhcp, DNSFromDHCP: true}, false, private, ""},
		{"dhcp with resolvers", &UplinkSpec{IPv4: dhcp, DNS: []string{"192.0.2.53"}}, false, nil, ""},
		{"dhcp and auto", &UplinkSpec{IPv4: dhcp, IPv6: auto, DNSFromDHCP: true}, true, nil, ""},
		{"ipv6 static alone", &UplinkSpec{IPv4: none, IPv6: UplinkAddress{Mode: UplinkStatic, Address: "2001:db8::17/64", Gateway: "fe80::1"}, DNS: []string{"2001:db8::53"}}, true, nil, ""},
		{"ipv6 auto alone, resolvers from it", &UplinkSpec{IPv6: auto, DNSFromDHCP: true}, true, nil, ""},
		{"empty", &UplinkSpec{}, false, nil, "network.uplink has neither IPv4 nor IPv6; the new system would be unreachable"},
		{"none", &UplinkSpec{IPv4: none, IPv6: none}, true, nil, "network.uplink has neither IPv4 nor IPv6"},
		{"upper-case mac", &UplinkSpec{MAC: "0C:C4:7A:AA:BB:11", IPv4: dhcp}, false, nil, `network.uplink: mac "0C:C4:7A:AA:BB:11" is not a MAC address like 0c:c4:7a:aa:bb:01 (lower case, colons)`},
		{"dashed mac", &UplinkSpec{MAC: "0c-c4-7a-aa-bb-11", IPv4: dhcp}, false, nil, `network.uplink: mac "0c-c4-7a-aa-bb-11" is not a MAC address`},
		{"short mac", &UplinkSpec{MAC: "0c:c4:7a:aa:bb", IPv4: dhcp}, false, nil, `network.uplink: mac "0c:c4:7a:aa:bb" is not a MAC address`},
		{"long mac", &UplinkSpec{MAC: "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01", IPv4: dhcp}, false, nil, "is not a MAC address"},
		{"multicast mac", &UplinkSpec{MAC: "01:00:5e:00:00:01", IPv4: dhcp}, false, nil, "network.uplink: mac 01:00:5e:00:00:01 is a multicast address, no interface's"},
		{"zero mac", &UplinkSpec{MAC: "00:00:00:00:00:00", IPv4: dhcp}, false, nil, "network.uplink: mac 00:00:00:00:00:00 is no interface's"},
		{"ipv4 auto", &UplinkSpec{IPv4: auto}, false, nil, `network.uplink: ipv4: mode "auto" is not static, dhcp or none`},
		{"ipv6 dhcp", &UplinkSpec{IPv4: dhcp, IPv6: dhcp}, true, nil, `network.uplink: ipv6: mode "dhcp" is not static, auto or none`},
		{"dhcp with an address", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkDHCP, Address: "192.0.2.17/24"}}, false, nil, "network.uplink: ipv4 is dhcp: an address and a gateway only with static"},
		{"none with a gateway", &UplinkSpec{IPv4: dhcp, IPv6: UplinkAddress{Gateway: "fe80::1"}}, true, nil, "network.uplink: ipv6 is none: an address and a gateway only with static"},
		{"no prefix", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17"}}, false, nil, `network.uplink: ipv4: address "192.0.2.17" is not an IPv4 address with its prefix length, like 192.0.2.17/24`},
		{"prefix 0", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/0"}}, false, nil, "is not an IPv4 address with its prefix length"},
		{"ipv6 as ipv4", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "2001:db8::17/64"}}, false, nil, "is not an IPv4 address with its prefix length"},
		{"ipv4 as ipv6", &UplinkSpec{IPv4: dhcp, IPv6: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24"}}, true, nil, `network.uplink: ipv6: address "192.0.2.17/24" is not an IPv6 address with its prefix length, like 2001:db8::17/64`},
		{"mapped ipv4 as ipv6", &UplinkSpec{IPv4: dhcp, IPv6: UplinkAddress{Mode: UplinkStatic, Address: "::ffff:192.0.2.17/120"}}, true, nil, "is not an IPv6 address"},
		{"network address", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.0/24"}}, false, nil, "network.uplink: ipv4: address 192.0.2.0/24 is the address of its network, no host's"},
		{"broadcast address", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.255/24"}}, false, nil, "network.uplink: ipv4: address 192.0.2.255/24 is the broadcast address of its network, no host's"},
		{"gateway of the other family", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24", Gateway: "fe80::1"}}, false, nil, `network.uplink: ipv4: gateway "fe80::1" is not an IPv4 address`},
		{"gateway with a zone", &UplinkSpec{IPv4: dhcp, IPv6: UplinkAddress{Mode: UplinkStatic, Address: "2001:db8::17/64", Gateway: "fe80::1%eth0"}}, true, nil, `network.uplink: ipv6: gateway "fe80::1%eth0" is not an IPv6 address`},
		{"gateway the address", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "192.0.2.17/24", Gateway: "192.0.2.17"}}, false, nil, "network.uplink: ipv4: gateway 192.0.2.17 is the uplink's own address"},
		{"ipv6 with the kernel's off", &UplinkSpec{IPv4: dhcp, IPv6: auto}, false, nil, "network.uplink: ipv6 is auto, and the kernel's IPv6 is off (kernel.ipv6 in the profile)"},
		{"resolver no address", &UplinkSpec{IPv4: dhcp, DNS: []string{"dns.example.net"}}, false, nil, `network.uplink: dns "dns.example.net" is not an IP address`},
		{"resolver twice", &UplinkSpec{IPv4: dhcp, DNS: []string{"192.0.2.53", "192.0.2.53"}}, false, nil, "network.uplink: dns 192.0.2.53 is listed twice"},
		{"ipv6 resolver without ipv6", &UplinkSpec{IPv4: dhcp, DNS: []string{"2001:db8::53"}}, false, nil, "network.uplink: dns 2001:db8::53 is an IPv6 address, and the uplink has no IPv6"},
		{"ipv4 resolver without ipv4", &UplinkSpec{IPv6: auto, DNS: []string{"192.0.2.53"}}, true, nil, "network.uplink: dns 192.0.2.53 is an IPv4 address, and the uplink has no IPv4"},
		{"resolvers and from dhcp", &UplinkSpec{IPv4: dhcp, DNS: []string{"192.0.2.53"}, DNSFromDHCP: true}, false, nil, "network.uplink: dns and dns_from_dhcp exclude each other"},
		{"from dhcp without a lease", &UplinkSpec{IPv4: static4, DNSFromDHCP: true}, false, nil, "network.uplink: dns_from_dhcp, and no lease to take resolvers from (ipv4 static, ipv6 none)"},
		{"in a private network", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "10.0.0.200/24", Gateway: "10.0.0.1"}}, false, private, "network.uplink: address 10.0.0.200/24 overlaps the private network default (10.0.0.0/24); the uplink needs a network of its own"},
		{"around a private network", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "10.0.8.1/16"}}, false, []PrivateSpec{{Network: "db", CIDR: "10.0.1.0/24"}}, "overlaps the private network db (10.0.1.0/24)"},
		{"next to a private network", &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "10.0.1.17/24"}}, false, private, ""},
	} {
		s := &Spec{Kernel: KernelSpec{IPv6: c.ipv6}, Network: NetworkSpec{Uplink: c.uplink, PrivateNetworks: c.private}}
		err := s.ValidateUplink()
		if (c.err == "" && err != nil) || (c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err))) {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
	// a specification of one private network (Private alone) is checked
	// the same
	s := &Spec{Network: NetworkSpec{Private: &private[0], Uplink: &UplinkSpec{IPv4: UplinkAddress{Mode: UplinkStatic, Address: "10.0.0.5/24"}}}}
	if err := s.ValidateUplink(); err == nil || !strings.Contains(err.Error(), "overlaps the private network default") {
		t.Errorf("private alone: %v", err)
	}
}
