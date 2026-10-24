package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/state"
)

func networksPath() string { return filepath.Join(config.ProfilesDir(), "networks.yaml") }

var networkSet = &namedSet{
	file: "networks.yaml", key: "networks", noun: "network",
	use:     "network",
	aliases: []string{"net"},
	short:   "The named private networks the profiles refer to",
	long: `The private networks live in networks.yaml next to the profiles
(` + config.AbbreviateHome(networksPath()) + `), one named entry each; a profile
names one in network.private, or none. The first use puts the published
networks there: default, 10.0.0.0/24. A network is an IPv4 network
(cidr, /8 to /30, overlapping no other) and the addresses never given
to a server (reserved); every server ump installs in it gets an address
of its own, the lowest free one unless --private-ip says which, kept
across reinstalls. docs/networks.md in the repository describes it.

On a terminal, create without a name asks for one and for the
network to start from; give the copy a cidr of its own.`,
	example: `  ump config net list
  ump config net get default
  ump config net create db --from default   # then your editor
  ump config net edit
  ump config net delete db`,
	defaultFrom: "default",
	headers:     []string{"NAME", "CIDR", "ADDRESSES", "RESERVED", "STATE", "DESCRIPTION"},
	rows: func(data []byte, names []string) ([][]string, error) {
		networks, err := profile.ParseNetworks(data)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, n := range names {
			net := networks[n]
			state, size := "ok", "-"
			if err := net.Validate(); err != nil {
				state = "invalid"
			} else {
				size = strconv.Itoa(net.Size())
			}
			reserved := strings.Join(net.Reserved, ",")
			if reserved == "" {
				reserved = "-"
			}
			rows = append(rows, []string{n, net.CIDR, size, reserved, state, net.Description})
		}
		return rows, nil
	},
	name: func(name string) error {
		if name == profile.NoPrivateNetwork {
			return errors.New("none is what a profile says for no private network")
		}
		if !profile.IsNetworkName(name) {
			return fmt.Errorf("%q: use lower-case letters, digits, '.', '_' and '-', starting with a letter or digit", name)
		}
		return nil
	},
	// every network valid, and none overlapping another: an address of
	// both would be one server's in either
	check: func(data []byte) error {
		networks, err := profile.ParseNetworks(data)
		if err != nil {
			return err
		}
		names := sortedKeys(networks)
		for i, n := range names {
			if err := networks[n].Validate(); err != nil {
				return err
			}
			for _, o := range names[:i] {
				if networks[n].Overlaps(networks[o]) {
					return fmt.Errorf("networks %s (%s) and %s (%s) overlap; give each addresses of its own", o, networks[o].CIDR, n, networks[n].CIDR)
				}
			}
		}
		return nil
	},
	keepChanged: true,
}

func (a *app) newConfigNetworkCmd() *cobra.Command { return networkSet.newCmd(a) }

// loadNetworks reads networks.yaml: the user's own in the profiles
// directory, else the published one.
func (a *app) loadNetworks(ctx context.Context, storage string) (map[string]*profile.PrivateNetwork, error) {
	data, from, err := a.loadNamed(ctx, storage, "networks.yaml")
	if err != nil {
		return nil, err
	}
	networks, err := profile.ParseNetworks(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return networks, nil
}

// checkPrivateFlags checks --private-network and --private-ip by
// themselves: a network name or none, an IPv4 address, not both none
// and an address.
func checkPrivateFlags(cmd *cobra.Command, network, address string) error {
	if network != "" && network != profile.NoPrivateNetwork && !profile.IsNetworkName(network) {
		return usageErrorf(cmd, "--private-network %q is not a network name or none", network)
	}
	if address != "" {
		if a, err := netip.ParseAddr(address); err != nil || !a.Is4() {
			return usageErrorf(cmd, "--private-ip %q is not an IPv4 address", address)
		}
		if network == profile.NoPrivateNetwork {
			return usageErrorf(cmd, "--private-ip and --private-network none exclude each other")
		}
	}
	return nil
}

// privateRequest is what one server asks of the private networks: the
// server (provider/id), the network (nil: none) and the address given by
// hand (empty: any).
type privateRequest struct {
	ref     string
	network *profile.PrivateNetwork
	address string
}

// assignPrivate gives every request its address, nil for none, in doc
// and among each other, or says which request it cannot (its index)
// and why (exit status 2). An address is one server's alone in every
// network that has it, whatever its name, and kept: first, a server
// keeps the one it has in the network it stays in, while it is still one
// the network gives (the lowest free one otherwise); then an address
// given by hand is checked against the network and refused when another
// server has it, also one of the requests that keeps it or was given it
// by hand; then the others get the lowest free addresses, in order. The
// servers of the requests leave the addresses they do not keep.
func assignPrivate(doc *state.Document, reqs []privateRequest) ([]*state.Private, int, error) {
	out := make([]*state.Private, len(reqs))
	refs := make([]string, len(reqs))
	for i, r := range reqs {
		refs[i] = r.ref
	}
	// who holds what, by address, with the network they hold it in: the
	// other servers, then the requests that keep their address
	type holder struct {
		ref, network string
		cidr         netip.Prefix
	}
	held := map[netip.Addr][]holder{}
	hold := func(a netip.Addr, ref, network, cidr string) {
		if c, err := netip.ParsePrefix(cidr); err == nil {
			held[a] = append(held[a], holder{ref, network, c})
		}
	}
	// takenIn is the holder of an address in a network that overlaps n
	takenIn := func(n *profile.PrivateNetwork, a netip.Addr) (holder, bool) {
		c := netip.MustParsePrefix(n.CIDR)
		for _, h := range held[a] {
			if h.cidr.Overlaps(c) {
				return h, true
			}
		}
		return holder{}, false
	}
	for ref, s := range doc.Servers {
		if s.Private == nil || slices.Contains(refs, ref) {
			continue
		}
		if a, err := netip.ParseAddr(s.Private.Address); err == nil {
			hold(a, ref, s.Private.Network, s.Private.CIDR)
		}
	}
	for i, r := range reqs {
		cur := doc.Servers[r.ref].Private
		if r.network == nil || r.address != "" || cur == nil || cur.Network != r.network.Name {
			continue
		}
		a, err := r.network.CheckAddress(cur.Address)
		if _, taken := takenIn(r.network, a); err != nil || taken {
			continue
		}
		hold(a, r.ref, r.network.Name, r.network.CIDR)
		out[i] = &state.Private{Network: r.network.Name, CIDR: r.network.CIDR, Address: a.String()}
	}
	// the addresses given by hand
	for i, r := range reqs {
		if r.network == nil || r.address == "" {
			continue
		}
		a, err := r.network.CheckAddress(r.address)
		if err != nil {
			return nil, i, withExit(exitUsage, fmt.Errorf("--private-ip %s: %v", r.address, err))
		}
		if h, taken := takenIn(r.network, a); taken && h.ref != r.ref {
			return nil, i, withExit(exitUsage, fmt.Errorf("--private-ip %s: %s has that address in the network %s; give another (ump server list -o wide shows the addresses)", r.address, h.ref, h.network))
		}
		hold(a, r.ref, r.network.Name, r.network.CIDR)
		out[i] = &state.Private{Network: r.network.Name, CIDR: r.network.CIDR, Address: a.String()}
	}
	// the lowest free ones for the rest
	for i, r := range reqs {
		if r.network == nil || out[i] != nil {
			continue
		}
		a, err := r.network.Free(func(a netip.Addr) bool { _, taken := takenIn(r.network, a); return taken })
		if err != nil {
			return nil, i, withExit(exitUsage, err)
		}
		hold(a, r.ref, r.network.Name, r.network.CIDR)
		out[i] = &state.Private{Network: r.network.Name, CIDR: r.network.CIDR, Address: a.String()}
	}
	return out, -1, nil
}

// describePrivate is a private address in words: 10.0.0.5 in default,
// or none.
func describePrivate(p *state.Private) string {
	if p == nil {
		return "none"
	}
	return p.Address + " in " + p.Network
}

// reservePrivate records the private addresses of planned installations
// in the state before they start, so that two installations, by this ump
// or another, never get the same one: within the state's
// read-modify-write, a server that no longer has the address it was
// planned with, or an address another server has meanwhile, stops all of
// them (exit status 2, nothing changed). A server that leaves its private
// network leaves its address at once. Each plan keeps what its server
// had, for releasePrivate.
func (a *app) reservePrivate(ctx context.Context, plans []*installPlan) error {
	refs := make([]string, len(plans))
	for i, p := range plans {
		refs[i] = p.m.Ref()
	}
	return a.updateState(ctx, func(d *state.Document) error {
		for _, p := range plans {
			if now := d.Servers[p.m.Ref()].Private; !now.Equal(p.had) {
				return withExit(exitUsage, fmt.Errorf("the private address of %s changed meanwhile, from %s to %s; nothing was changed, run it again", p.ref, describePrivate(p.had), describePrivate(now)))
			}
			if p.private == nil {
				continue
			}
			if other := d.PrivateHolder(p.private, refs...); other != "" {
				return withExit(exitUsage, fmt.Errorf("the private address %s of %s is %s's now (another ump gave it meanwhile); nothing was changed, run it again", describePrivate(p.private), p.ref, other))
			}
		}
		for _, p := range plans {
			s := d.Servers[p.m.Ref()]
			p.reserved = !s.Private.Equal(p.private)
			if p.reserved {
				p.before, s.Private = s.Private, p.private
				d.Servers[p.m.Ref()] = s
			}
		}
		return nil
	})
}

// releasePrivate gives the servers of installations whose installer did
// not write the disks the private addresses they had back, where
// reservePrivate recorded others and nobody changed them since; the
// caller holds the servers' locks. An address another server got
// meanwhile stays that one's, and the server keeps the one recorded for
// it; a failure is told, and the addresses stay as recorded.
func (a *app) releasePrivate(ctx context.Context, plans []*installPlan) {
	var back []*installPlan
	for _, p := range plans {
		if p.reserved {
			back = append(back, p)
		}
	}
	if len(back) == 0 {
		return
	}
	var kept []*installPlan
	err := a.updateState(context.WithoutCancel(ctx), func(d *state.Document) error {
		kept = nil
		for _, p := range back {
			s := d.Servers[p.m.Ref()]
			if !s.Private.Equal(p.private) {
				continue
			}
			if b := p.before; b != nil && d.PrivateHolder(b, p.m.Ref()) != "" {
				kept = append(kept, p)
				continue
			}
			s.Private = p.before
			d.Servers[p.m.Ref()] = s
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the private addresses recorded for installations that did not write the disks stay with their servers: %v\n", err)
		return
	}
	for _, p := range kept {
		keeps := "keeps the private address " + describePrivate(p.private)
		if p.private == nil {
			keeps = "stays without a private address"
		}
		fmt.Fprintf(a.stderr, "ump: %s %s: the one it had, %s, is another server's now\n", p.ref, keeps, describePrivate(p.before))
	}
	for _, p := range back {
		p.reserved = false
	}
}
