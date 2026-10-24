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
	// every network valid, and none overlapping another
	check: func(data []byte) error {
		networks, err := profile.ParseNetworks(data)
		if err != nil {
			return err
		}
		for _, n := range sortedKeys(networks) {
			if err := networks[n].Validate(); err != nil {
				return err
			}
		}
		return checkOverlap(networks)
	},
	keepChanged: true,
}

// checkOverlap refuses two valid networks that overlap: an address of
// both would be one server's in either.
func checkOverlap(networks map[string]*profile.PrivateNetwork) error {
	var valid []string
	for _, n := range sortedKeys(networks) {
		if networks[n].Validate() != nil {
			continue
		}
		for _, o := range valid {
			if networks[n].Overlaps(networks[o]) {
				return fmt.Errorf("networks %s (%s) and %s (%s) overlap; give each addresses of its own", o, networks[o].CIDR, n, networks[n].CIDR)
			}
		}
		valid = append(valid, n)
	}
	return nil
}

func (a *app) newConfigNetworkCmd() *cobra.Command { return networkSet.newCmd(a) }

// loadNetworks reads networks.yaml: the user's own in the profiles
// directory, else the published one; none overlapping another.
func (a *app) loadNetworks(ctx context.Context, storage string) (map[string]*profile.PrivateNetwork, error) {
	data, from, err := a.loadNamed(ctx, storage, "networks.yaml")
	if err != nil {
		return nil, err
	}
	networks, err := profile.ParseNetworks(data)
	if err == nil {
		err = checkOverlap(networks)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return networks, nil
}

// privateFlags are --private-network and --private-ip as checked by
// themselves: given says that --private-network was, networks are its
// names (none: empty), ips the addresses of --private-ip in order.
type privateFlags struct {
	given    bool
	networks []string
	ips      []privateIP
}

// privateIP is an address of --private-ip, as given (text), with its
// network (empty: an address alone).
type privateIP struct {
	text, network, address string
}

// checkPrivateFlags checks --private-network and --private-ip by
// themselves (each repeatable, or comma-separated): network names, or
// none alone; NETWORK=ADDRESS with an IPv4 address, each network once,
// or one address alone, at least one; not none and an address.
func checkPrivateFlags(cmd *cobra.Command, networks, ips []string) (privateFlags, error) {
	var f privateFlags
	if f.given = networks != nil; f.given {
		names := listFlag(networks).values()
		if len(names) == 0 {
			return f, usageErrorf(cmd, "--private-network names no network; none gives the server none")
		}
		for _, n := range names {
			switch {
			case n == profile.NoPrivateNetwork && len(names) > 1:
				return f, usageErrorf(cmd, "--private-network none stands alone: it gives the server no private network")
			case n != profile.NoPrivateNetwork && !profile.IsNetworkName(n):
				return f, usageErrorf(cmd, "--private-network %q is not a network name or none", n)
			case n != profile.NoPrivateNetwork:
				f.networks = append(f.networks, n)
			}
		}
	}
	for _, v := range ips {
		for _, text := range strings.Split(v, ",") {
			if text = strings.TrimSpace(text); text == "" {
				continue
			}
			ip := privateIP{text: text, address: text}
			if n, a, ok := strings.Cut(text, "="); ok {
				ip.network, ip.address = strings.TrimSpace(n), strings.TrimSpace(a)
				if !profile.IsNetworkName(ip.network) {
					return f, usageErrorf(cmd, "--private-ip %q: %q is not a network name", text, ip.network)
				}
			}
			if a, err := netip.ParseAddr(ip.address); err != nil || !a.Is4() {
				if ip.network == "" {
					return f, usageErrorf(cmd, "--private-ip %q is not an IPv4 address", text)
				}
				return f, usageErrorf(cmd, "--private-ip %q: %q is not an IPv4 address", text, ip.address)
			}
			for _, o := range f.ips {
				if ip.network != "" && o.network == ip.network {
					return f, usageErrorf(cmd, "--private-ip names the network %s twice", ip.network)
				}
			}
			f.ips = append(f.ips, ip)
		}
	}
	if ips != nil && len(f.ips) == 0 {
		return f, usageErrorf(cmd, "--private-ip gives no address")
	}
	if len(f.ips) > 1 {
		for _, ip := range f.ips {
			if ip.network == "" {
				return f, usageErrorf(cmd, "--private-ip %s: an address without its network stands alone; give each as NETWORK=ADDRESS", ip.text)
			}
		}
	}
	if len(f.ips) > 0 && f.given && len(f.networks) == 0 {
		return f, usageErrorf(cmd, "--private-ip and --private-network none exclude each other")
	}
	return f, nil
}

// addresses are the addresses of --private-ip by network, for a server
// in the networks names (one at least): NETWORK=ADDRESS in one of them,
// an address alone in the only one.
func (f privateFlags) addresses(names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, ip := range f.ips {
		switch {
		case ip.network == "" && len(names) > 1:
			return nil, fmt.Errorf("--private-ip %s: the server is in the private networks %s; say in which, NETWORK=ADDRESS", ip.text, strings.Join(names, ", "))
		case ip.network == "":
			out[names[0]] = ip.address
		case !slices.Contains(names, ip.network):
			return nil, fmt.Errorf("--private-ip %s: %s is no private network of the server (%s); --private-network gives it others", ip.text, ip.network, strings.Join(names, ", "))
		default:
			out[ip.network] = ip.address
		}
	}
	return out, nil
}

// privateRequest is what one server asks of the private networks: the
// server (provider/id), its networks in order (none: none) and the
// addresses given by hand, by network (absent: any).
type privateRequest struct {
	ref      string
	networks []*profile.PrivateNetwork
	given    map[string]string
}

// assignPrivate gives every request its addresses, one per network in
// its order, in doc and among each other, or says which request it
// cannot (its index) and why (exit status 2). An address is one server's
// alone in every network that has it, whatever its name, and kept:
// first, a server keeps the one it has in each network it stays in,
// while it is still one the network gives (the lowest free one
// otherwise); then an address given by hand is checked against its
// network and refused when a server has it already: another one, also
// one of the requests that keeps it or was given it by hand, or the same
// one in another network; then the others get the lowest free addresses,
// in order. The servers of the requests leave the addresses they do not
// keep.
func assignPrivate(doc *state.Document, reqs []privateRequest) ([][]state.Private, int, error) {
	out := make([][]state.Private, len(reqs))
	refs := make([]string, len(reqs))
	for i, r := range reqs {
		refs[i] = r.ref
		out[i] = make([]state.Private, len(r.networks))
	}
	// who holds what, by address, with the network they hold it in: the
	// other servers, then the requests that keep their addresses
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
	give := func(i, j int, a netip.Addr) {
		n := reqs[i].networks[j]
		hold(a, reqs[i].ref, n.Name, n.CIDR)
		out[i][j] = state.Private{Network: n.Name, CIDR: n.CIDR, Address: a.String()}
	}
	for ref, s := range doc.Servers {
		if slices.Contains(refs, ref) {
			continue
		}
		for _, p := range s.Private {
			if a, err := netip.ParseAddr(p.Address); err == nil {
				hold(a, ref, p.Network, p.CIDR)
			}
		}
	}
	for i, r := range reqs {
		for j, n := range r.networks {
			if _, byHand := r.given[n.Name]; byHand {
				continue
			}
			k := slices.IndexFunc(doc.Servers[r.ref].Private, func(p state.Private) bool { return p.Network == n.Name })
			if k < 0 {
				continue
			}
			a, err := n.CheckAddress(doc.Servers[r.ref].Private[k].Address)
			if _, taken := takenIn(n, a); err != nil || taken {
				continue
			}
			give(i, j, a)
		}
	}
	// the addresses given by hand
	for i, r := range reqs {
		for j, n := range r.networks {
			addr, byHand := r.given[n.Name]
			if !byHand {
				continue
			}
			a, err := n.CheckAddress(addr)
			if err != nil {
				return nil, i, withExit(exitUsage, fmt.Errorf("--private-ip %s: %v", addr, err))
			}
			if h, taken := takenIn(n, a); taken {
				return nil, i, withExit(exitUsage, fmt.Errorf("--private-ip %s: %s has that address in the network %s; give another (ump server list shows the addresses)", addr, h.ref, h.network))
			}
			give(i, j, a)
		}
	}
	// the lowest free ones for the rest
	for i, r := range reqs {
		for j, n := range r.networks {
			if out[i][j].Address != "" {
				continue
			}
			a, err := n.Free(func(a netip.Addr) bool { _, taken := takenIn(n, a); return taken })
			if err != nil {
				return nil, i, withExit(exitUsage, err)
			}
			give(i, j, a)
		}
	}
	for i := range out {
		if len(out[i]) == 0 {
			out[i] = nil
		}
	}
	return out, -1, nil
}

// describePrivate is private addresses in words: 10.0.0.5 in default,
// 10.0.1.3 in lab; or none.
func describePrivate(ps []state.Private) string {
	if len(ps) == 0 {
		return "none"
	}
	words := make([]string, len(ps))
	for i, p := range ps {
		words[i] = p.Address + " in " + p.Network
	}
	return strings.Join(words, ", ")
}

// privateAddresses is "private address" or "private addresses", as many
// as ps has, followed by them.
func privateAddresses(ps []state.Private) string {
	if len(ps) == 1 {
		return "private address " + describePrivate(ps)
	}
	return "private addresses " + describePrivate(ps)
}

// privateIPv4 is the addresses alone, comma-separated.
func privateIPv4(ps []state.Private) string {
	addrs := make([]string, len(ps))
	for i, p := range ps {
		addrs[i] = p.Address
	}
	return strings.Join(addrs, ",")
}

// reservePrivate records the private addresses of planned installations
// in the state before they start, so that two installations, by this ump
// or another, never get the same one: within the state's
// read-modify-write, a server that no longer has the addresses it was
// planned with, or an address another server has meanwhile, stops all of
// them (exit status 2, nothing changed). A server that leaves a private
// network leaves its address there at once. Each plan keeps what its
// server had, for releasePrivate.
func (a *app) reservePrivate(ctx context.Context, plans []*installPlan) error {
	refs := make([]string, len(plans))
	for i, p := range plans {
		refs[i] = p.m.Ref()
	}
	return a.updateState(ctx, func(d *state.Document) error {
		for _, p := range plans {
			if now := d.Servers[p.m.Ref()].Private; !slices.Equal(now, p.had) {
				return withExit(exitUsage, fmt.Errorf("the private addresses of %s changed meanwhile, from %s to %s; nothing was changed, run it again", p.ref, describePrivate(p.had), describePrivate(now)))
			}
			for _, q := range p.private {
				if other := d.PrivateHolder(q, refs...); other != "" {
					return withExit(exitUsage, fmt.Errorf("the private address %s of %s is %s's now (another ump gave it meanwhile); nothing was changed, run it again", describePrivate([]state.Private{q}), p.ref, other))
				}
			}
		}
		for _, p := range plans {
			s := d.Servers[p.m.Ref()]
			p.reserved = !slices.Equal(s.Private, p.private)
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
// caller holds the servers' locks. What one of them gives back is free
// for the others. When another server got one of them meanwhile, that
// one stays its, and the server keeps the ones recorded for it; a
// failure is told, and the addresses stay as recorded.
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
	type keep struct {
		p     *installPlan
		taken []state.Private
	}
	var kept []keep
	err := a.updateState(context.WithoutCancel(ctx), func(d *state.Document) error {
		// to a fixpoint: what one gives back may be what another had
		pending := back
		for changed := true; changed; {
			changed, kept = false, nil
			var left []*installPlan
			for _, p := range pending {
				s := d.Servers[p.m.Ref()]
				if !slices.Equal(s.Private, p.private) {
					continue
				}
				var taken []state.Private
				for _, b := range p.before {
					if d.PrivateHolder(b, p.m.Ref()) != "" {
						taken = append(taken, b)
					}
				}
				if len(taken) > 0 {
					kept = append(kept, keep{p, taken})
					left = append(left, p)
					continue
				}
				s.Private = p.before
				d.Servers[p.m.Ref()] = s
				changed = true
			}
			pending = left
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the private addresses recorded for installations that did not write the disks stay with their servers: %v\n", err)
		return
	}
	for _, k := range kept {
		keeps := "keeps the " + privateAddresses(k.p.private)
		if len(k.p.private) == 0 {
			keeps = "stays without a private address"
		}
		verb := "is another server's"
		if len(k.taken) > 1 {
			verb = "are other servers'"
		}
		fmt.Fprintf(a.stderr, "ump: %s %s: of those it had, %s %s now\n", k.p.ref, keeps, describePrivate(k.taken), verb)
	}
	for _, p := range back {
		p.reserved = false
	}
}
