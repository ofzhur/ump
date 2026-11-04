package profile

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// The modes of an uplink's address family.
const (
	UplinkStatic = "static" // the address of UplinkAddress
	UplinkDHCP   = "dhcp"   // IPv4 by DHCP, the MAC as the client identifier
	UplinkAuto   = "auto"   // IPv6 from router advertisements (SLAAC, EUI-64)
	UplinkNone   = "none"   // none of this family; also when empty
)

// UplinkSpec is the installed system's uplink where the provider knows
// it (a PXE machine's, from pxe.yaml), provider-neutral: the interface by
// MAC (empty: the one the rescue system reaches the network through),
// its IPv4 and IPv6, and its resolvers: DNS, or with DNSFromDHCP those of
// the lease, else 1.1.1.1 and 8.8.8.8. Without one the installer
// configures the uplink as the rescue system has it.
type UplinkSpec struct {
	MAC         string        `json:"mac,omitempty"`
	IPv4        UplinkAddress `json:"ipv4"`
	IPv6        UplinkAddress `json:"ipv6"`
	DNS         []string      `json:"dns,omitempty"`
	DNSFromDHCP bool          `json:"dns_from_dhcp,omitempty"`
}

// UplinkAddress is how the uplink gets an address of one family: Mode
// static with Address (ADDRESS/PREFIX) and Gateway (on-link; none: no
// default route), dhcp (IPv4), auto (IPv6) or none.
type UplinkAddress struct {
	Mode    string `json:"mode"`
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// mode is the mode, none when empty.
func (a UplinkAddress) mode() string {
	if a.Mode == "" {
		return UplinkNone
	}
	return a.Mode
}

// ValidateUplink checks the specification's uplink, when it has one: a
// MAC of an interface, in lower case with colons; per family a mode it
// knows, for static an address with its prefix length that is a host of
// its network and a gateway of the family other than the address, and
// neither for the other modes; IPv4 or IPv6 at all, and IPv6 only with
// the kernel's IPv6 on; resolvers of a family the uplink has, each once,
// or those of a lease it takes; and no static address in one of the
// server's private networks.
func (s *Spec) ValidateUplink() error {
	u := s.Network.Uplink
	if u == nil {
		return nil
	}
	if u.MAC != "" {
		hw, err := net.ParseMAC(u.MAC)
		switch {
		case err != nil || len(hw) != 6 || hw.String() != u.MAC:
			return fmt.Errorf("network.uplink: mac %q is not a MAC address like 0c:c4:7a:aa:bb:01 (lower case, colons)", u.MAC)
		case hw[0]&1 == 1:
			return fmt.Errorf("network.uplink: mac %s is a multicast address, no interface's", u.MAC)
		case u.MAC == "00:00:00:00:00:00":
			return fmt.Errorf("network.uplink: mac %s is no interface's", u.MAC)
		}
	}
	v4, err := u.IPv4.check("ipv4", UplinkDHCP)
	if err != nil {
		return err
	}
	v6, err := u.IPv6.check("ipv6", UplinkAuto)
	if err != nil {
		return err
	}
	m4, m6 := u.IPv4.mode(), u.IPv6.mode()
	switch {
	case m4 == UplinkNone && m6 == UplinkNone:
		return fmt.Errorf("network.uplink has neither IPv4 nor IPv6; the new system would be unreachable")
	case m6 != UplinkNone && !s.Kernel.IPv6:
		return fmt.Errorf("network.uplink: ipv6 is %s, and the kernel's IPv6 is off (kernel.ipv6 in the profile)", m6)
	}
	var seen []netip.Addr
	for _, d := range u.DNS {
		a, err := netip.ParseAddr(d)
		switch {
		case err != nil || a.Zone() != "":
			return fmt.Errorf("network.uplink: dns %q is not an IP address", d)
		case a.Is4() && m4 == UplinkNone:
			return fmt.Errorf("network.uplink: dns %s is an IPv4 address, and the uplink has no IPv4", d)
		case a.Is6() && m6 == UplinkNone:
			return fmt.Errorf("network.uplink: dns %s is an IPv6 address, and the uplink has no IPv6", d)
		case slices.Contains(seen, a):
			return fmt.Errorf("network.uplink: dns %s is listed twice", d)
		}
		seen = append(seen, a)
	}
	if u.DNSFromDHCP {
		switch {
		case len(u.DNS) > 0:
			return fmt.Errorf("network.uplink: dns and dns_from_dhcp exclude each other")
		case m4 != UplinkDHCP && m6 != UplinkAuto:
			return fmt.Errorf("network.uplink: dns_from_dhcp, and no lease to take resolvers from (ipv4 %s, ipv6 %s)", m4, m6)
		}
	}
	privates := s.Network.PrivateNetworks
	if len(privates) == 0 && s.Network.Private != nil {
		privates = []PrivateSpec{*s.Network.Private}
	}
	for _, p := range []netip.Prefix{v4, v6} {
		if !p.IsValid() {
			continue
		}
		for _, n := range privates {
			if c, err := netip.ParsePrefix(n.CIDR); err == nil && c.Overlaps(p.Masked()) {
				return fmt.Errorf("network.uplink: address %s overlaps the private network %s (%s); the uplink needs a network of its own", p, n.Network, n.CIDR)
			}
		}
	}
	return nil
}

// check checks one family's configuration, whose dynamic mode is
// dynamic (dhcp, auto), and returns its static address.
func (a UplinkAddress) check(family, dynamic string) (netip.Prefix, error) {
	at := "network.uplink: " + family
	mode := a.mode()
	switch mode {
	case UplinkStatic:
	case dynamic, UplinkNone:
		if a.Address != "" || a.Gateway != "" {
			return netip.Prefix{}, fmt.Errorf("%s is %s: an address and a gateway only with static", at, mode)
		}
		return netip.Prefix{}, nil
	default:
		return netip.Prefix{}, fmt.Errorf("%s: mode %q is not %s, %s or %s", at, a.Mode, UplinkStatic, dynamic, UplinkNone)
	}
	name, example := "IPv4", "192.0.2.17/24"
	if family == "ipv6" {
		name, example = "IPv6", "2001:db8::17/64"
	}
	ok := func(addr netip.Addr) bool {
		if family == "ipv4" {
			return addr.Is4()
		}
		return addr.Is6() && !addr.Is4In6() && addr.Zone() == ""
	}
	p, err := netip.ParsePrefix(a.Address)
	if err != nil || !ok(p.Addr()) || p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("%s: address %q is not an %s address with its prefix length, like %s", at, a.Address, name, example)
	}
	if family == "ipv4" && p.Bits() <= 30 {
		host := ^uint32(0) >> p.Bits()
		b := p.Addr().As4()
		switch binary.BigEndian.Uint32(b[:]) & host {
		case 0:
			return netip.Prefix{}, fmt.Errorf("%s: address %s is the address of its network, no host's", at, a.Address)
		case host:
			return netip.Prefix{}, fmt.Errorf("%s: address %s is the broadcast address of its network, no host's", at, a.Address)
		}
	}
	if a.Gateway != "" {
		g, err := netip.ParseAddr(a.Gateway)
		switch {
		case err != nil || !ok(g):
			return netip.Prefix{}, fmt.Errorf("%s: gateway %q is not an %s address", at, a.Gateway, name)
		case g == p.Addr():
			return netip.Prefix{}, fmt.Errorf("%s: gateway %s is the uplink's own address", at, a.Gateway)
		}
	}
	return p, nil
}
