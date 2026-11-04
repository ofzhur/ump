package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// Machines on a private network are reached through a jump host (a PXE
// site's jump): ump's own SSH goes through it as ssh's ProxyJump, and so
// does Ansible's (ansible_ssh_common_args in the inventory). ssh offers
// the jump host the keys of your own SSH setup (the agent, ~/.ssh,
// ~/.ssh/config), not those of the command line.

// sshRoute is how ump reaches an address over SSH: through a jump host
// (ProxyJump), and for the rescue system of a network boot with the
// host keys it reported pinned (a known_hosts file of the run's)
// instead of no host key check.
type sshRoute struct {
	jump       string
	knownHosts string
}

// sshRoutes are the routes of a run, by address: the machines behind a
// jump host, from their listing, and the rescue systems of network
// boots; tmp is a directory of the run's for the pinned host keys,
// removed at its end (clean). An address that servers reached in
// different ways share (shared: their jumps by ref, "" for none) has
// the route of the server the run acts on (reach; claimed: its ref).
type sshRoutes struct {
	mu      sync.Mutex
	routes  map[string]sshRoute
	shared  map[string]map[string]string
	claimed map[string]string
	tmp     string
}

// clean removes the run's directory.
func (s *sshRoutes) clean() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tmp != "" {
		os.RemoveAll(s.tmp)
		s.tmp = ""
	}
}

// setRoute sets how ump reaches an address.
func (a *app) setRoute(addr string, r sshRoute) {
	if addr == "" {
		return
	}
	if a.routes == nil {
		a.routes = &sshRoutes{}
	}
	a.routes.mu.Lock()
	defer a.routes.mu.Unlock()
	if a.routes.routes == nil {
		a.routes.routes = map[string]sshRoute{}
	}
	a.routes.routes[addr] = r
}

// route is how ump reaches an address: directly, unless setRoute said
// otherwise.
func (a *app) route(addr string) sshRoute {
	if a.routes == nil {
		return sshRoute{}
	}
	a.routes.mu.Lock()
	defer a.routes.mu.Unlock()
	return a.routes.routes[addr]
}

// setJump sets the jump host of an address, keeping its pinned host
// keys.
func (a *app) setJump(addr, jump string) {
	r := a.route(addr)
	r.jump = jump
	a.setRoute(addr, r)
}

// reach makes the addresses of m, the server a command acts on, go m's
// way (its jump host, or none) for the rest of the run where servers
// reached in other ways share them (resolveJumps). A run acts on only
// one of such servers.
func (a *app) reach(m provider.Machine) error {
	if a.routes == nil {
		return nil
	}
	for _, addr := range []string{m.IPv4, m.IPv6} {
		a.routes.mu.Lock()
		jumps, shared := a.routes.shared[addr]
		other, claimed := a.routes.claimed[addr]
		if shared && !claimed {
			if a.routes.claimed == nil {
				a.routes.claimed = map[string]string{}
			}
			a.routes.claimed[addr] = m.Ref()
		}
		a.routes.mu.Unlock()
		switch {
		case addr == "" || !shared:
		case claimed && other != m.Ref() && jumps[other] != m.Jump:
			return withExit(exitUsage, fmt.Errorf("%s and %s share the address %s, but ump reaches them %s and %s: act on one of them per run", other, m.Ref(), addr, reachedHow(jumps[other]), reachedHow(m.Jump)))
		default:
			a.setJump(addr, m.Jump)
		}
	}
	return nil
}

// reachedHow says how ump reaches a server with jump.
func reachedHow(jump string) string {
	if jump == "" {
		return "directly"
	}
	return "through " + jump
}

// unpin drops the host keys pinned for an address: the system they were
// pinned for is gone (a rescue system rebooted into the new system).
func (a *app) unpin(addr string) {
	if r := a.route(addr); r.knownHosts != "" {
		r.knownHosts = ""
		a.setRoute(addr, r)
	}
}

// sshTo is the ssh command line ump runs to reach addr (sshArgs), by its
// route.
func (a *app) sshTo(user, addr string, identities []string, identitiesOnly, insecureHostKey bool, command []string) []string {
	return sshArgs(a.route(addr), user, addr, identities, identitiesOnly, insecureHostKey, command)
}

// sshShown is the ssh command line ump prints for people to run, which
// outlives the run: through the jump host, without the run's pinned
// host keys.
func (a *app) sshShown(user, addr string, identities []string, identitiesOnly, insecureHostKey bool, command []string) []string {
	r := a.route(addr)
	r.knownHosts = ""
	return sshArgs(r, user, addr, identities, identitiesOnly, insecureHostKey, command)
}

// answers reports whether SSH answers at addr: a connection to its SSH
// port, made through the jump host when the address has one (ssh -W,
// which prints the server's greeting).
func (a *app) answers(ctx context.Context, addr string) bool {
	jump := a.route(addr).jump
	if jump == "" || addr == "" {
		return a.probe(ctx, addr)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	code, err := a.sshExec(ctx, jumpProbe(jump, addr), nil, &out, io.Discard)
	return err == nil && code == 0 && strings.HasPrefix(out.String(), "SSH-")
}

// jumpProbe is the ssh command line that connects to the SSH port of
// addr through the jump host and prints what answers there.
func jumpProbe(jump, addr string) []string {
	hops := strings.Split(jump, ",")
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if len(hops) > 1 {
		argv = append(argv, "-o", "ProxyJump="+strings.Join(hops[:len(hops)-1], ","))
	}
	user, host, port := splitHop(hops[len(hops)-1])
	if port != "" {
		argv = append(argv, "-p", port)
	}
	argv = append(argv, "-W", net.JoinHostPort(addr, strconv.Itoa(sshPort)))
	if user != "" {
		host = user + "@" + host
	}
	return append(argv, host)
}

// splitHop splits a hop of a jump, [user@]host[:port], with an IPv6
// host in brackets when it has a port.
func splitHop(hop string) (user, host, port string) {
	if i := strings.LastIndex(hop, "@"); i >= 0 {
		user, hop = hop[:i], hop[i+1:]
	}
	if h, p, err := net.SplitHostPort(hop); err == nil {
		return user, h, p
	}
	return user, strings.Trim(hop, "[]"), ""
}

// resolveJumps makes the machines' jump hosts what ssh takes as
// ProxyJump: a hop whose host names a server of the listings (its name
// or ref) becomes its address, and its administrator the login when the
// hop names none, as ump ssh logs in there; any other host, an address
// among them, stays as it is, for ssh. The machines' addresses go
// through their jump host from then on (route), but for an address that
// servers reached in other ways share (a private address behind the
// jump host, and the same address of another network): it goes the way
// of the server a command acts on (reach), and no way before.
func (a *app) resolveJumps(cfg *config.Config, doc *state.Document, results []listResult) {
	var all []provider.Machine
	for _, r := range results {
		all = append(all, r.machines...)
	}
	jumps := map[string]map[string]string{} // by address: each server's jump, by ref
	for _, r := range results {
		for i := range r.machines {
			m := &r.machines[i]
			if m.Jump != "" {
				hops := strings.Split(m.Jump, ",")
				for j, hop := range hops {
					hops[j] = a.resolveHop(cfg, doc, all, m.Ref(), hop)
				}
				m.Jump = strings.Join(hops, ",")
			}
			for _, addr := range []string{m.IPv4, m.IPv6} {
				if addr == "" {
					continue
				}
				if jumps[addr] == nil {
					jumps[addr] = map[string]string{}
				}
				jumps[addr][m.Ref()] = m.Jump
			}
		}
	}
	for addr, byRef := range jumps {
		var jump string
		ways := map[string]bool{}
		for _, j := range byRef {
			jump, ways[j] = j, true
		}
		switch {
		case len(ways) > 1:
			if a.routes == nil {
				a.routes = &sshRoutes{}
			}
			a.routes.mu.Lock()
			if a.routes.shared == nil {
				a.routes.shared = map[string]map[string]string{}
			}
			a.routes.shared[addr] = byRef
			_, claimed := a.routes.claimed[addr]
			a.routes.mu.Unlock()
			if !claimed {
				a.setJump(addr, "")
			}
		case jump != "":
			a.setJump(addr, jump)
		}
	}
}

// resolveHop is a hop of the jump of the server self, resolved.
func (a *app) resolveHop(cfg *config.Config, doc *state.Document, all []provider.Machine, self, hop string) string {
	user, host, port := splitHop(hop)
	if _, err := netip.ParseAddr(host); err == nil {
		return hop
	}
	ms := matching(all, host)
	if len(ms) != 1 || ms[0].Ref() == self || primaryAddress(ms[0]) == "" {
		return hop
	}
	j := ms[0]
	addr := primaryAddress(j)
	if user == "" {
		var s state.Server
		if doc != nil {
			s = doc.Servers[j.Ref()]
		}
		if s.Installed != nil && s.Installed.Admin != "" {
			user = s.Installed.Admin
		} else {
			user = a.profileAdmin(cfg, s.Profile)
		}
	}
	switch {
	case port != "":
		addr = net.JoinHostPort(addr, port)
	case strings.Contains(addr, ":"):
		// ProxyJump takes an IPv6 address in brackets
		addr = "[" + addr + "]"
	}
	return user + "@" + addr
}

// pinHostKeys pins the host keys (type and key) of the system at addr
// for the run: ump's connections there check them instead of none.
func (a *app) pinHostKeys(addr string, keys []string) (string, error) {
	dir, err := a.tempDir()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, k := range keys {
		if f := strings.Fields(k); len(f) >= 2 {
			b.WriteString(addr + " " + f[0] + " " + f[1] + "\n")
		}
	}
	path := filepath.Join(dir, "known_hosts-"+strings.NewReplacer(":", "_", "%", "_").Replace(addr))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// tempDir is a directory of the run's, removed at its end.
func (a *app) tempDir() (string, error) {
	if a.routes == nil {
		a.routes = &sshRoutes{}
	}
	a.routes.mu.Lock()
	defer a.routes.mu.Unlock()
	if a.routes.tmp == "" {
		dir, err := os.MkdirTemp("", "ump-")
		if err != nil {
			return "", err
		}
		a.routes.tmp = dir
	}
	return a.routes.tmp, nil
}
