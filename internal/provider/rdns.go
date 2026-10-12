package provider

import (
	"context"
	"net/netip"
)

// CapReverseDNS is implemented by providers that manage the reverse DNS
// (PTR) entries of a machine's public addresses (see ReverseDNS).
const CapReverseDNS Capability = "rdns"

// PTR is the reverse DNS entry of one address. An empty Name means the
// address has no entry.
type PTR struct {
	IP   string `json:"ip" yaml:"ip"`
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}

// ReverseDNS is implemented by providers that manage PTR records.
type ReverseDNS interface {
	Provider
	// PTRs returns the reverse DNS entries of the machine's public
	// addresses: the primary IPv4 first (with an empty name when it has
	// none), then every other address of the machine that has an entry.
	PTRs(ctx context.Context, m Machine) ([]PTR, error)
	// SetPTR sets the reverse DNS entry of one of the machine's addresses.
	// An empty name removes the entry; providers with a default entry
	// then show that again. An address that does not belong to the
	// machine is refused.
	SetPTR(ctx context.Context, m Machine, ip netip.Addr, name string) error
}

// Contains reports whether addr is one of the single addresses or inside
// one of the prefixes ("a.b.c.d/nn", "2001:db8::/64").
func Contains(addr netip.Addr, singles []string, prefixes []string) bool {
	for _, s := range singles {
		if a, err := netip.ParseAddr(s); err == nil && a == addr {
			return true
		}
	}
	for _, p := range prefixes {
		if pfx, err := netip.ParsePrefix(p); err == nil && pfx.Contains(addr) {
			return true
		}
	}
	return false
}
