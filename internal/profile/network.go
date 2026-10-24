package profile

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// NoPrivateNetwork is what a profile, a flag or a plan says for no
// private network.
const NoPrivateNetwork = "none"

// PrivateNetwork is a named private network of networks.yaml: an IPv4
// network from which every server ump installs in it gets an address of
// its own, for the traffic between the servers.
type PrivateNetwork struct {
	Name        string   `yaml:"-" json:"name"`
	Description string   `yaml:"description,omitempty" json:"-"`
	CIDR        string   `yaml:"cidr" json:"cidr"`
	Reserved    []string `yaml:"reserved,omitempty" json:"reserved,omitempty"`
}

// ParseNetworks reads networks.yaml: named private networks.
func ParseNetworks(data []byte) (map[string]*PrivateNetwork, error) {
	var file struct {
		Networks map[string]*PrivateNetwork `yaml:"networks"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	for name, n := range file.Networks {
		if n == nil {
			return nil, fmt.Errorf("network %s is empty", name)
		}
		n.Name = name
	}
	return file.Networks, nil
}

// IsNetworkName reports whether s can name an entry of networks.yaml.
func IsNetworkName(s string) bool { return nameRe.MatchString(s) && s != NoPrivateNetwork }

// Validate checks a network: a name of lower-case letters, digits, '.',
// '_' and '-' (not none), an IPv4 network address with a prefix of /8 to
// /30, and reserved addresses inside it, each once, neither its own
// address nor its broadcast address.
func (n *PrivateNetwork) Validate() error {
	at := "network " + n.Name
	if n.Name == NoPrivateNetwork {
		return fmt.Errorf("network none: none is what a profile says for no private network; give the network another name")
	}
	if !IsNetworkName(n.Name) {
		return fmt.Errorf("network %q: a network name is lower-case letters, digits, '.', '_' and '-', starting with a letter or digit", n.Name)
	}
	p, err := netip.ParsePrefix(n.CIDR)
	if err != nil || !p.Addr().Is4() {
		return fmt.Errorf("%s: cidr %q is not an IPv4 network like 10.0.0.0/24", at, n.CIDR)
	}
	if p.Bits() < 8 || p.Bits() > 30 {
		return fmt.Errorf("%s: cidr %s: the prefix length is /8 to /30", at, n.CIDR)
	}
	if p.Masked() != p {
		return fmt.Errorf("%s: cidr %s is not a network address (%s is)", at, n.CIDR, p.Masked())
	}
	first, last := n.bounds()
	var seen []netip.Addr
	for _, r := range n.Reserved {
		a, err := netip.ParseAddr(r)
		if err != nil || !a.Is4() {
			return fmt.Errorf("%s: reserved %q is not an IPv4 address", at, r)
		}
		switch {
		case !p.Contains(a):
			return fmt.Errorf("%s: reserved %s is not in %s", at, r, n.CIDR)
		case a == first:
			return fmt.Errorf("%s: reserved %s is the address of the network itself, which no server gets anyway", at, r)
		case a == last:
			return fmt.Errorf("%s: reserved %s is the broadcast address of the network, which no server gets anyway", at, r)
		}
		if slices.Contains(seen, a) {
			return fmt.Errorf("%s: reserved %s is listed twice", at, r)
		}
		seen = append(seen, a)
	}
	return nil
}

// bounds are the network's own address and its broadcast address; the
// network is valid.
func (n *PrivateNetwork) bounds() (first, last netip.Addr) {
	p := netip.MustParsePrefix(n.CIDR).Masked()
	b := p.Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | (1<<(32-p.Bits()) - 1)
	binary.BigEndian.PutUint32(b[:], v)
	return p.Addr(), netip.AddrFrom4(b)
}

// reserved are the reserved addresses.
func (n *PrivateNetwork) reserved() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	for _, r := range n.Reserved {
		if a, err := netip.ParseAddr(r); err == nil {
			out[a] = true
		}
	}
	return out
}

// Size is how many addresses the network gives servers: all but its own
// address, its broadcast address and the reserved ones.
func (n *PrivateNetwork) Size() int {
	p := netip.MustParsePrefix(n.CIDR)
	first, last := n.bounds()
	size := 1<<(32-p.Bits()) - 2
	for a := range n.reserved() {
		if p.Contains(a) && a != first && a != last {
			size--
		}
	}
	return size
}

// Overlaps reports whether two networks share addresses. Both are
// valid.
func (n *PrivateNetwork) Overlaps(o *PrivateNetwork) bool {
	return netip.MustParsePrefix(n.CIDR).Overlaps(netip.MustParsePrefix(o.CIDR))
}

// CheckAddress checks an address for a server of the network: an IPv4
// address inside it, neither its own address nor its broadcast address
// nor a reserved one. The network is valid.
func (n *PrivateNetwork) CheckAddress(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Addr{}, fmt.Errorf("%q is not an IPv4 address", s)
	}
	first, last := n.bounds()
	switch {
	case !netip.MustParsePrefix(n.CIDR).Contains(a):
		return a, fmt.Errorf("%s is not in the network %s (%s)", s, n.Name, n.CIDR)
	case a == first:
		return a, fmt.Errorf("%s is the address of the network %s itself", s, n.Name)
	case a == last:
		return a, fmt.Errorf("%s is the broadcast address of the network %s", s, n.Name)
	case n.reserved()[a]:
		return a, fmt.Errorf("%s is reserved in the network %s", s, n.Name)
	}
	return a, nil
}

// Free is the lowest address of the network that a server can have and
// taken does not hold. The network is valid.
func (n *PrivateNetwork) Free(taken func(netip.Addr) bool) (netip.Addr, error) {
	first, last := n.bounds()
	reserved := n.reserved()
	for a := first.Next(); a.IsValid() && a.Less(last); a = a.Next() {
		if !reserved[a] && !taken(a) {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("the network %s (%s) has no free address left", n.Name, n.CIDR)
}

// PrivateNetworks is network.private of a profile: the names of private
// networks, in order, written as one name or a list, or none, alone.
type PrivateNetworks []string

func (l *PrivateNetworks) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = nil
		if n.Value != "" {
			*l = PrivateNetworks{n.Value}
		}
		return nil
	case yaml.SequenceNode:
		out := PrivateNetworks{}
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: network.private: a list of network names", c.Line)
			}
			out = append(out, c.Value)
		}
		*l = out
		return nil
	}
	return fmt.Errorf("network.private: a network name, a list of them, or none, not a %s", kindName(n.Kind))
}

// MarshalYAML writes one name as it is, several as a list.
func (l PrivateNetworks) MarshalYAML() (any, error) {
	if len(l) == 1 {
		return l[0], nil
	}
	return []string(l), nil
}

// IsZero makes an absent network.private stay absent when a profile is
// written.
func (l PrivateNetworks) IsZero() bool { return len(l) == 0 }

// Names are the networks named, in order; none for none.
func (l PrivateNetworks) Names() []string {
	if len(l) == 1 && l[0] == NoPrivateNetwork {
		return nil
	}
	return l
}

// Validate checks the names: each a network name, once, or none alone.
func (l PrivateNetworks) Validate() error {
	for i, n := range l {
		switch {
		case n == NoPrivateNetwork && len(l) > 1:
			return fmt.Errorf("network.private: none is no network; it stands alone, for no private network")
		case n != NoPrivateNetwork && !IsNetworkName(n):
			return fmt.Errorf("network.private %q is not a network name or none", n)
		case slices.Contains(l[:i], n):
			return fmt.Errorf("network.private names %s twice", n)
		}
	}
	return nil
}

// String is the names, comma-separated, or none.
func (l PrivateNetworks) String() string {
	if len(l.Names()) == 0 {
		return NoPrivateNetwork
	}
	return strings.Join(l, ",")
}

// PrivateNetworkNames are the private networks the profile names, in
// order; none for none.
func (p *Profile) PrivateNetworkNames() []string { return p.Network.Private.Names() }

// ResolvePrivate is the private networks the profile names, looked up in
// networks, in order; none for none.
func (p *Profile) ResolvePrivate(networks map[string]*PrivateNetwork) ([]*PrivateNetwork, error) {
	var out []*PrivateNetwork
	for _, name := range p.PrivateNetworkNames() {
		n, ok := networks[name]
		if !ok {
			return nil, fmt.Errorf("profile %s: private network %q is not in networks.yaml (known: %s; ump config os update fetches the published ones)", p.Name, name, strings.Join(sortedKeys(networks), ", "))
		}
		if err := n.Validate(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		out = append(out, n)
	}
	return out, nil
}
