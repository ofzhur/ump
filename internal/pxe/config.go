// Package pxe is UMP's network boot. pxe.yaml in the home says which
// machines ump boots over the network, how, and from which site; the
// CLI renders from it what the PXE servers of a site serve (site.json)
// and arms single boots (arms), and the servers (ump pxe serve) report
// what came of them (outcomes) and what they see (status). The CLI
// renders and arms, a server serves, claims and reports; they meet in
// the backend, under .pxe/<site>/, never directly. The iPXE scripts, the
// boot config in front of the rescue image, the cache of the rescue
// image and the server itself (Server; its TFTP side is
// internal/pxe/tftp) are here too.
//
// A site is a private network with its own DHCP server, which points
// the machines' firmware at an iPXE binary and iPXE at the site's PXE
// server (chain mode); ump hands out no addresses.
package pxe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/plan"
	"github.com/ofzhur/ump/internal/profile"
)

// FileName is pxe.yaml, in the home next to config.yaml.
const FileName = "pxe.yaml"

// TypeName is the provider type of the machines of pxe.yaml: a machine
// is the server pxe/NAME.
const TypeName = "pxe"

// Ref is the server reference of the machine name.
func Ref(name string) string { return TypeName + "/" + name }

// DefaultSite is the one site of a pxe.yaml without sites.
const DefaultSite = "default"

// The modes of a machine: how it boots when nothing is armed (standing),
// or what an arm boots it into once.
const (
	ModeLocal   = "local"   // its disk: iPXE exits to the next boot entry
	ModeRescue  = "rescue"  // UMP's rescue system
	ModeInstall = "install" // an unattended installation, armed once
)

// The policies for a MAC without an entry; in chain mode both answer
// with an exit to the next boot entry, local also lists the MAC in the
// status.
const (
	UnknownIgnore = "ignore"
	UnknownLocal  = "local"
)

// Firmware and architectures.
const (
	FirmwareUEFI = "uefi"
	FirmwareBIOS = "bios"
	ArchAMD64    = "amd64"
	ArchARM64    = "arm64"
)

// ConsoleNone, as a machine's console, passes no console= at all.
const ConsoleNone = "none"

// Config is pxe.yaml as written: the values of every level, not merged
// (Machine merges them), with the lines they came from.
type Config struct {
	// Unknown is the policy for MACs without an entry (ignore, local).
	Unknown  string
	Defaults Defaults
	Sites    map[string]*Site
	Machines map[string]*Entry

	// sites and machines are the names in the file's order, lines the
	// line of each key by its path (machines/NAME/network/ipv4)
	sites, machines []string
	lines           map[string]int
}

// Boot is what defaults, a site and a machine may give: how a machine
// boots, and its installed system's network.
type Boot struct {
	// Mode is local or rescue, and install for a machine alone.
	Mode *string `yaml:"mode"`
	// RescueKeys are root's keys in a standing rescue system, besides
	// UMP's and staging.keys: named keys, files, key lines.
	RescueKeys plan.List `yaml:"rescue-keys"`
	// Console is the kernel's serial console (console=ttyS1,115200n8),
	// none for none.
	Console *string `yaml:"console"`
	Network Network `yaml:"network"`
}

// Defaults are the values under every machine.
type Defaults struct {
	Boot `yaml:",inline"`
	// Jump is the jump host for SSH to the machines: an ump server's
	// name or [user@]host[:port].
	Jump *string `yaml:"jump"`
}

// Site is a private network with a DHCP server that chains to ump, and
// the PXE servers that serve it; its values come under its machines'.
type Site struct {
	Boot        `yaml:",inline"`
	Description string  `yaml:"description"`
	Jump        *string `yaml:"jump"`
	Unknown     string  `yaml:"unknown"`
}

// Entry is a machine as pxe.yaml has it: what PXE adds, and reinstall's
// values for it (a standing plan, as multiinstall's for a server).
type Entry struct {
	Boot        `yaml:",inline"`
	plan.Values `yaml:",inline"`
	Site        string    `yaml:"site"`
	MAC         plan.List `yaml:"mac"`
	Arch        string    `yaml:"arch"`
	Firmware    string    `yaml:"firmware"`
	Address     string    `yaml:"address"`
	Note        string    `yaml:"note"`
}

// Network is the installed system's uplink as a level gives it; the
// levels merge per field.
type Network struct {
	// Uplink is the uplink by MAC, a machine's alone (default: its first
	// MAC).
	Uplink string      `yaml:"uplink"`
	IPv4   *Addressing `yaml:"ipv4"`
	IPv6   *Addressing `yaml:"ipv6"`
	DNS    *DNS        `yaml:"dns"`
}

// Addressing is how the uplink gets an address of one family: a mode
// alone (dhcp, auto, none), or a static address and gateway, which a
// machine and its site may give one each (the site the gateway, the
// machine its address).
type Addressing struct {
	Mode    string
	Address string
	Gateway string
}

func (a *Addressing) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*a = Addressing{Mode: n.Value}
		return nil
	case yaml.MappingNode:
		var v struct {
			Address string `yaml:"address"`
			Gateway string `yaml:"gateway"`
		}
		if err := n.Decode(&v); err != nil {
			return err
		}
		*a = Addressing{Address: v.Address, Gateway: v.Gateway}
		return nil
	}
	return fmt.Errorf("line %d: dhcp, auto, none, or {address: ADDRESS/PREFIX, gateway: ADDRESS}", n.Line)
}

// DNS is the uplink's resolvers: those of the lease (dhcp) or a list.
type DNS struct {
	DHCP    bool
	Servers []string
}

func (d *DNS) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.ScalarNode && n.Value == "dhcp":
		*d = DNS{DHCP: true}
		return nil
	case n.Kind == yaml.ScalarNode:
		*d = DNS{Servers: []string{n.Value}}
		return nil
	case n.Kind == yaml.SequenceNode:
		var l plan.List
		if err := l.UnmarshalYAML(n); err != nil {
			return err
		}
		*d = DNS{Servers: l}
		return nil
	}
	return fmt.Errorf("line %d: dns is dhcp or a list of addresses", n.Line)
}

// The keys of each level, and those refused there with a reason.
var (
	bootKeys     = []string{"mode", "rescue-keys", "console", "network"}
	defaultsKeys = append([]string{"jump"}, bootKeys...)
	siteKeys     = append([]string{"description", "jump", "unknown"}, bootKeys...)
	machineKeys  = append(append([]string{"site", "mac", "arch", "firmware", "address", "note"}, bootKeys...), machinePlanKeys()...)
	networkKeys  = []string{"uplink", "ipv4", "ipv6", "dns"}
	// reserved for later versions: refused with a word, not as a typo
	networkReserved = []string{"interfaces", "bond", "vlans", "routes", "search", "mtu", "ntp", "proxy", "boot-network"}
)

// machinePlanKeys are reinstall's values a machine takes: all but via,
// which its mode says.
func machinePlanKeys() []string {
	var keys []string
	for _, k := range plan.Keys() {
		if k != "via" {
			keys = append(keys, k)
		}
	}
	return keys
}

// refusedAt says why a key is not one of a level.
func refusedAt(level, key string) string {
	switch {
	case key == "via" && level == "machine":
		return "via is no key here: the mode says how the machine boots"
	case key == "unattended":
		return "unattended is no key here: mode install says it"
	case key == "jump" && level == "machine":
		return "jump is a site's (sites.NAME.jump) or for all (defaults.jump)"
	case key == "ipxe":
		return "ipxe is no key here: the site's DHCP server names the iPXE file (docs/pxe.md)"
	case slices.Contains(plan.Keys(), key) && level != "machine":
		return key + " is a machine's alone: reinstall's values are per machine"
	case slices.Contains([]string{"site", "mac", "arch", "firmware", "address", "note"}, key):
		return key + " is a machine's alone"
	case key == "description" || key == "unknown":
		return key + " is a site's"
	}
	return ""
}

// Parse reads pxe.yaml, strictly: one document, no key it does not know
// or twice, no key without a value, values of the right kind. Validate
// checks what they mean. An empty file is no machines.
func Parse(data []byte) (*Config, error) {
	c := &Config{Sites: map[string]*Site{}, Machines: map[string]*Entry{}, lines: map[string]int{}}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc, more yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		return nil, err
	}
	switch err := dec.Decode(&more); {
	case err == nil:
		return nil, fmt.Errorf("line %d: pxe.yaml is one YAML document", more.Line)
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	if len(doc.Content) == 0 {
		return c, nil
	}
	root := resolveAlias(doc.Content[0])
	if isNull(root) {
		return c, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: pxe.yaml is a mapping of unknown, defaults, sites and machines", root.Line)
	}
	err := eachKey(root, "", func(k, v *yaml.Node) error {
		c.lines[k.Value] = k.Line
		switch k.Value {
		case "unknown":
			if v.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: unknown is ignore or local", v.Line)
			}
			c.Unknown = v.Value
			return nil
		case "defaults":
			if err := c.checkLevel(v, "defaults", "defaults", defaultsKeys); err != nil {
				return err
			}
			return decode(v, "defaults", &c.Defaults)
		case "sites":
			return c.parseNamed(v, "sites", func(name string, n *yaml.Node) error {
				s := &Site{}
				if err := c.checkLevel(n, "site", "sites/"+name, siteKeys); err != nil {
					return err
				}
				if err := decode(n, "sites."+name, s); err != nil {
					return err
				}
				c.Sites[name] = s
				c.sites = append(c.sites, name)
				return nil
			})
		case "machines":
			return c.parseNamed(v, "machines", func(name string, n *yaml.Node) error {
				e := &Entry{}
				if err := c.checkLevel(n, "machine", "machines/"+name, machineKeys); err != nil {
					return err
				}
				if err := decode(n, "machines."+name, e); err != nil {
					return err
				}
				c.Machines[name] = e
				c.machines = append(c.machines, name)
				return nil
			})
		}
		return fmt.Errorf("line %d: %s is no part of pxe.yaml (unknown, defaults, sites, machines)", k.Line, k.Value)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// parseNamed reads sites or machines: a mapping of names to entries.
func (c *Config) parseNamed(n *yaml.Node, what string, fn func(name string, n *yaml.Node) error) error {
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: %s is a mapping of names to their values", n.Line, what)
	}
	return eachKey(n, what+".", func(k, v *yaml.Node) error {
		if k.Kind != yaml.ScalarNode || strings.TrimSpace(k.Value) == "" {
			return fmt.Errorf("line %d: %s are named", k.Line, what)
		}
		c.lines[what+"/"+k.Value] = k.Line
		return fn(k.Value, v)
	})
}

// checkLevel checks the keys of defaults, a site or a machine (level)
// and of its network, and records their lines under path.
func (c *Config) checkLevel(n *yaml.Node, level, path string, keys []string) error {
	where := strings.ReplaceAll(path, "/", ".")
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: %s is a mapping of keys to values", n.Line, where)
	}
	return eachKey(n, where+".", func(k, v *yaml.Node) error {
		c.lines[path+"/"+k.Value] = k.Line
		if !slices.Contains(keys, k.Value) {
			if why := refusedAt(level, k.Value); why != "" {
				return fmt.Errorf("line %d: %s: %s", k.Line, where, why)
			}
			return fmt.Errorf("line %d: %s: %s is no key of a %s (%s)", k.Line, where, k.Value, level, strings.Join(keys, ", "))
		}
		if isNull(v) || v.Kind == yaml.ScalarNode && v.Value == "" && v.Style == 0 {
			return fmt.Errorf("line %d: %s: %s has no value", k.Line, where, k.Value)
		}
		if k.Value != "network" {
			return nil
		}
		if v.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: %s.network is a mapping of %s", v.Line, where, strings.Join(networkKeys, ", "))
		}
		return eachKey(v, where+".network.", func(nk, nv *yaml.Node) error {
			c.lines[path+"/network/"+nk.Value] = nk.Line
			switch {
			case slices.Contains(networkReserved, nk.Value):
				return fmt.Errorf("line %d: %s.network: %s is reserved for a later version of ump", nk.Line, where, nk.Value)
			case !slices.Contains(networkKeys, nk.Value):
				return fmt.Errorf("line %d: %s.network: %s is no key of the network (%s)", nk.Line, where, nk.Value, strings.Join(networkKeys, ", "))
			case isNull(nv):
				return fmt.Errorf("line %d: %s.network: %s has no value", nk.Line, where, nk.Value)
			case (nk.Value == "ipv4" || nk.Value == "ipv6") && nv.Kind == yaml.MappingNode:
				return eachKey(nv, where+".network."+nk.Value+".", func(ak, av *yaml.Node) error {
					c.lines[path+"/network/"+nk.Value+"/"+ak.Value] = ak.Line
					if ak.Value != "address" && ak.Value != "gateway" {
						return fmt.Errorf("line %d: %s.network.%s: %s is no key here (address, gateway)", ak.Line, where, nk.Value, ak.Value)
					}
					if av.Kind != yaml.ScalarNode || av.Value == "" {
						return fmt.Errorf("line %d: %s.network.%s: %s is an address", ak.Line, where, nk.Value, ak.Value)
					}
					return nil
				})
			}
			return nil
		})
	})
}

// eachKey calls fn for the pairs of a mapping, refusing a key twice.
func eachKey(n *yaml.Node, where string, fn func(k, v *yaml.Node) error) error {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolveAlias(n.Content[i+1])
		if seen[k.Value] {
			return fmt.Errorf("line %d: %s%s is given twice", k.Line, where, k.Value)
		}
		seen[k.Value] = true
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return nil
}

// decode decodes a level, with yaml's type errors said as "line N:
// where: ...".
func decode(n *yaml.Node, where string, v any) error {
	if isNull(n) {
		return nil
	}
	err := n.Decode(v)
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		msg := te.Errors[0]
		if rest, ok := strings.CutPrefix(msg, "line "); ok {
			if line, more, ok := strings.Cut(rest, ": "); ok {
				return fmt.Errorf("line %s: %s: %s", line, where, more)
			}
		}
		return fmt.Errorf("%s: %s", where, msg)
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "line ") {
			line, more, _ := strings.Cut(strings.TrimPrefix(err.Error(), "line "), ": ")
			return fmt.Errorf("line %s: %s: %s", line, where, more)
		}
		return fmt.Errorf("%s: %w", where, err)
	}
	return nil
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// line is the line of the key at path (machines/web-07/mac), else of the
// nearest key above it that has one; 0 when none.
func (c *Config) line(path string) int {
	for path != "" {
		if l, ok := c.lines[path]; ok {
			return l
		}
		i := strings.LastIndex(path, "/")
		if i < 0 {
			break
		}
		path = path[:i]
	}
	return 0
}

// at is an error at the key of path, "line N: where: ...".
func (c *Config) at(path, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if l := c.line(path); l > 0 {
		return fmt.Errorf("line %d: %s: %s", l, describePath(path), msg)
	}
	return fmt.Errorf("%s: %s", describePath(path), msg)
}

// describePath is a path as people read it: machines.web-07: network.ipv4.
func describePath(path string) string {
	parts := strings.Split(path, "/")
	switch {
	case len(parts) >= 2 && (parts[0] == "machines" || parts[0] == "sites"):
		head := parts[0] + "." + parts[1]
		if len(parts) == 2 {
			return head
		}
		return head + ": " + strings.Join(parts[2:], ".")
	case len(parts) >= 2:
		return parts[0] + ": " + strings.Join(parts[1:], ".")
	}
	return path
}

// Names are the machines, in the file's order.
func (c *Config) Names() []string { return slices.Clone(c.machines) }

// SiteNames are the sites, in the file's order: default alone when the
// file has none.
func (c *Config) SiteNames() []string {
	if len(c.sites) == 0 {
		return []string{DefaultSite}
	}
	return slices.Clone(c.sites)
}

// Machine is a machine of pxe.yaml with its values merged: the defaults,
// then its site's, then its own, each replacing the one below it (lists
// too); the network merges per field, so that a site gives the gateway
// and the resolvers and a machine its address.
type Machine struct {
	Name     string
	Site     string
	MACs     []string
	Mode     string
	Hostname string
	Arch     string
	Firmware string
	// Console is the kernel's serial console, ConsoleNone, or empty for
	// the default (KernelArgs).
	Console    string
	RescueKeys []string
	Jump       string
	Address    string
	Note       string
	// Network is the installed system's uplink.
	Network profile.UplinkSpec
	// Plan is reinstall's values for the machine.
	Plan plan.Values
}

// SiteOf is the site a machine's entry names, default without sites.
func (c *Config) SiteOf(e *Entry) string {
	if e.Site == "" && len(c.Sites) == 0 {
		return DefaultSite
	}
	return e.Site
}

// Machine merges the entry of a machine; nil when there is none. The
// values are taken as they are: Validate checks them.
func (c *Config) Machine(name string) *Machine {
	e := c.Machines[name]
	if e == nil {
		return nil
	}
	site := c.SiteOf(e)
	s := c.Sites[site]
	if s == nil {
		s = &Site{}
	}
	m := &Machine{Name: name, Site: site, Arch: e.Arch, Firmware: e.Firmware, Address: e.Address, Note: e.Note, Plan: e.Values}
	for _, mac := range e.MAC {
		if n, err := NormalizeMAC(mac); err == nil {
			mac = n
		}
		m.MACs = append(m.MACs, mac)
	}
	m.Mode = first(ModeLocal, e.Mode, s.Mode, c.Defaults.Mode)
	m.Hostname = name
	if e.Hostname != nil {
		m.Hostname = *e.Hostname
	}
	m.Console = first("", e.Console, s.Console, c.Defaults.Console)
	m.Jump = first("", s.Jump, c.Defaults.Jump)
	for _, l := range []plan.List{e.RescueKeys, s.RescueKeys, c.Defaults.RescueKeys} {
		if l != nil {
			m.RescueKeys = slices.Clone([]string(l))
			break
		}
	}
	m.Network = c.uplink(m, e, s)
	return m
}

// Merged are every machine merged, in the file's order.
func (c *Config) Merged() []*Machine {
	var out []*Machine
	for _, n := range c.machines {
		out = append(out, c.Machine(n))
	}
	return out
}

// first is the value of the first level that gives one, def otherwise.
func first(def string, levels ...*string) string {
	for _, v := range levels {
		if v != nil {
			return *v
		}
	}
	return def
}

// uplink merges the network of the levels into the installed system's
// uplink: by default the first MAC, IPv4 by DHCP, no IPv6, and the
// lease's resolvers where there is a lease, else (static) ump's public
// ones.
func (c *Config) uplink(m *Machine, e *Entry, s *Site) profile.UplinkSpec {
	var u profile.UplinkSpec
	if mac := e.Network.Uplink; mac != "" {
		if n, err := NormalizeMAC(mac); err == nil {
			mac = n
		}
		u.MAC = mac
	} else if len(m.MACs) > 0 {
		u.MAC = m.MACs[0]
	}
	levels := []Network{c.Defaults.Network, s.Network, e.Network}
	var v4, v6 *Addressing
	var dns *DNS
	for _, l := range levels {
		v4 = mergeAddressing(v4, l.IPv4)
		v6 = mergeAddressing(v6, l.IPv6)
		if l.DNS != nil {
			dns = l.DNS
		}
	}
	u.IPv4 = resolveAddressing(v4, profile.UplinkDHCP)
	u.IPv6 = resolveAddressing(v6, profile.UplinkNone)
	switch {
	case dns != nil && dns.DHCP:
		u.DNSFromDHCP = true
	case dns != nil:
		u.DNS = slices.Clone(dns.Servers)
	case u.IPv4.Mode == profile.UplinkDHCP || u.IPv6.Mode == profile.UplinkAuto:
		u.DNSFromDHCP = true
	}
	return u
}

// mergeAddressing puts the addressing of a level (upper) over the one
// below: a mode replaces it, and so does an address over a mode; an
// address and a gateway over an address and a gateway merge per field.
func mergeAddressing(lower, upper *Addressing) *Addressing {
	switch {
	case upper == nil:
		return lower
	case upper.Mode != "" || lower == nil || lower.Mode != "":
		u := *upper
		return &u
	}
	m := *lower
	if upper.Address != "" {
		m.Address = upper.Address
	}
	if upper.Gateway != "" {
		m.Gateway = upper.Gateway
	}
	return &m
}

// resolveAddressing is the merged addressing of a family as the
// specification has it: static with an address (or a gateway alone,
// which Validate refuses), else the mode, def when none is given.
func resolveAddressing(a *Addressing, def string) profile.UplinkAddress {
	switch {
	case a == nil:
		return profile.UplinkAddress{Mode: def}
	case a.Mode != "":
		return profile.UplinkAddress{Mode: a.Mode}
	}
	return profile.UplinkAddress{Mode: profile.UplinkStatic, Address: a.Address, Gateway: a.Gateway}
}

// NormalizeMAC writes a MAC address as ump does, lower case with colons
// (0c:c4:7a:aa:bb:01); hyphens, dots and bare hex are taken too. It
// refuses what is no interface's: multicast, all zeros.
func NormalizeMAC(s string) (string, error) {
	t := strings.TrimSpace(s)
	if len(t) == 12 && !strings.ContainsAny(t, ":-.") {
		var b strings.Builder
		for i := 0; i < 12; i += 2 {
			if i > 0 {
				b.WriteByte(':')
			}
			b.WriteString(t[i : i+2])
		}
		t = b.String()
	}
	hw, err := net.ParseMAC(t)
	switch {
	case err != nil || len(hw) != 6:
		return "", fmt.Errorf("%q is not a MAC address like 0c:c4:7a:aa:bb:01", s)
	case hw[0]&1 == 1:
		return "", fmt.Errorf("%s is a multicast address, no interface's", hw)
	case hw.String() == "00:00:00:00:00:00":
		return "", fmt.Errorf("%s is no interface's", hw)
	}
	return hw.String(), nil
}

var (
	siteName      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	consoleValue  = regexp.MustCompile(`^[A-Za-z0-9_./:,-]+$`)
)

// ValidSite says whether a name is one a site may have: a DNS label.
func ValidSite(name string) bool { return siteName.MatchString(name) }

// validHostname says whether a name is a hostname: DNS labels of lower
// case letters, digits and hyphens, joined by dots, as a server's name
// is (it becomes the rescue system's and the installed system's).
func validHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, l := range strings.Split(name, ".") {
		if !hostnameLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// validJump says whether a jump host is one ssh takes as ProxyJump: an
// ump server's name or [user@]host[:port], several separated by commas.
func validJump(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\n\"'") {
		return false
	}
	for _, hop := range strings.Split(s, ",") {
		if hop == "" || strings.Count(hop, "@") > 1 || strings.HasSuffix(hop, "@") || strings.HasPrefix(hop, "@") {
			return false
		}
	}
	return true
}

// StableDisk says whether a disk name stays the same across boots: a
// name under /dev/disk/by-id (nvme-eui..., wwn-..., ata-...), which the
// kernel's names (sda, nvme0n1) are not, or serial:SERIAL.
func StableDisk(d string) bool {
	if s, ok := strings.CutPrefix(d, "serial:"); ok {
		return s != ""
	}
	return strings.Contains(d, "-") && !strings.ContainsAny(d, "/ \t")
}

// Validate checks what the values mean: names, sites, modes, MACs (each
// once in the file), addresses (each once), the network of every
// machine merged, keys and jump hosts. It returns the first problem in
// the file's order, with its line.
func (c *Config) Validate() error {
	if c.Unknown != "" && c.Unknown != UnknownIgnore && c.Unknown != UnknownLocal {
		return c.at("unknown", "%q is not ignore or local", c.Unknown)
	}
	if err := c.checkBoot("defaults", &c.Defaults.Boot, false); err != nil {
		return err
	}
	if c.Defaults.Jump != nil && !validJump(*c.Defaults.Jump) {
		return c.at("defaults/jump", "%q is not an ump server's name or [user@]host[:port]", *c.Defaults.Jump)
	}
	for _, name := range c.sites {
		s, path := c.Sites[name], "sites/"+name
		switch {
		case !siteName.MatchString(name):
			return c.at(path, "a site's name is lower-case letters, digits and hyphens (it names the server's unit, ump-pxe@%s)", name)
		case s.Unknown != "" && s.Unknown != UnknownIgnore && s.Unknown != UnknownLocal:
			return c.at(path+"/unknown", "%q is not ignore or local", s.Unknown)
		case s.Jump != nil && !validJump(*s.Jump):
			return c.at(path+"/jump", "%q is not an ump server's name or [user@]host[:port]", *s.Jump)
		}
		if err := c.checkBoot(path, &s.Boot, false); err != nil {
			return err
		}
	}
	macs := map[string]string{}
	privateIPs := map[netip.Addr]string{}
	addresses := map[netip.Addr]string{}
	for _, name := range c.machines {
		e, path := c.Machines[name], "machines/"+name
		if !validHostname(name) {
			return c.at(path, "a machine's name is a hostname: lower-case letters, digits and hyphens, with dots between labels")
		}
		switch site := c.SiteOf(e); {
		case len(c.Sites) > 0 && e.Site == "":
			return c.at(path, "site is missing; with sites, every machine names its own (%s)", strings.Join(c.sites, ", "))
		case len(c.Sites) > 0 && c.Sites[site] == nil:
			return c.at(path+"/site", "%s is no site of pxe.yaml (%s)", site, strings.Join(c.sites, ", "))
		case len(c.Sites) == 0 && site != DefaultSite:
			return c.at(path+"/site", "%s is no site: pxe.yaml has none, so every machine is in %s (sites: names them)", site, DefaultSite)
		}
		if len(e.MAC) == 0 {
			return c.at(path, "mac is missing: the MACs it boots from, the first its uplink")
		}
		for _, raw := range e.MAC {
			mac, err := NormalizeMAC(raw)
			if err != nil {
				return c.at(path+"/mac", "%v", err)
			}
			if other, ok := macs[mac]; ok {
				if other == name {
					return c.at(path+"/mac", "%s is given twice", mac)
				}
				return c.at(path+"/mac", "%s is %s's already: a MAC boots one machine", mac, other)
			}
			macs[mac] = name
		}
		if err := c.checkBoot(path, &e.Boot, true); err != nil {
			return err
		}
		switch {
		case e.Arch != "" && e.Arch != ArchAMD64 && e.Arch != ArchARM64:
			return c.at(path+"/arch", "%q is not amd64 or arm64", e.Arch)
		case e.Firmware != "" && e.Firmware != FirmwareUEFI && e.Firmware != FirmwareBIOS:
			return c.at(path+"/firmware", "%q is not uefi or bios", e.Firmware)
		case e.Hostname != nil && !validHostname(*e.Hostname):
			return c.at(path+"/hostname", "%q is not a hostname", *e.Hostname)
		}
		m := c.Machine(name)
		if m.Mode == ModeInstall && m.Network.IPv4.Mode != profile.UplinkStatic && m.Network.IPv6.Mode != profile.UplinkStatic && e.Address == "" {
			return c.at(path+"/mode", "install: ump records an installation once it reaches the new system, at a static network.ipv4 or at address (the DHCP server's reservation); give one")
		}
		for _, d := range e.Disks {
			if m.Mode == ModeInstall && !StableDisk(d) {
				return c.at(path+"/disks", "%s is not a stable name; an unattended installation takes names under /dev/disk/by-id (nvme-eui..., wwn-...) or serial:SERIAL", d)
			}
		}
		for _, ip := range e.PrivateIP {
			text := strings.TrimSpace(ip)
			if _, a, ok := strings.Cut(text, "="); ok {
				text = strings.TrimSpace(a)
			}
			a, err := netip.ParseAddr(text)
			if err != nil || !a.Is4() {
				return c.at(path+"/private-ip", "%q is not NETWORK=ADDRESS or an IPv4 address", ip)
			}
			if other, ok := privateIPs[a]; ok && other != name {
				return c.at(path+"/private-ip", "%s is %s's already", a, other)
			}
			privateIPs[a] = name
		}
		if err := c.checkUplink(path, m); err != nil {
			return err
		}
		var own []netip.Addr
		for _, fam := range []profile.UplinkAddress{m.Network.IPv4, m.Network.IPv6} {
			if fam.Mode == profile.UplinkStatic {
				p, _ := netip.ParsePrefix(fam.Address)
				own = append(own, p.Addr())
			}
		}
		if e.Address != "" {
			a, err := netip.ParseAddr(e.Address)
			if err != nil || a.Zone() != "" {
				return c.at(path+"/address", "%q is not an IP address (without a prefix length)", e.Address)
			}
			if len(own) > 0 && !slices.Contains(own, a) {
				return c.at(path+"/address", "%s is not the static address of its network (%s); give one of them, or address alone with DHCP", a, own[0])
			}
			if !slices.Contains(own, a) {
				own = append(own, a)
			}
		}
		for _, a := range own {
			if other, ok := addresses[a]; ok && other != name {
				return c.at(path, "the address %s is %s's already", a, other)
			}
			addresses[a] = name
		}
	}
	return nil
}

// checkBoot checks the values of a level as it gives them, so that a
// problem is said where it is written: the mode (install for a machine
// alone), the keys, the console, and the network's values.
func (c *Config) checkBoot(path string, b *Boot, machine bool) error {
	if b.Mode != nil {
		switch *b.Mode {
		case ModeLocal, ModeRescue:
		case ModeInstall:
			if !machine {
				return c.at(path+"/mode", "install is armed per machine alone, with a yes (ump config pxe apply); here local or rescue")
			}
		default:
			if machine {
				return c.at(path+"/mode", "%q is not local, rescue or install", *b.Mode)
			}
			return c.at(path+"/mode", "%q is not local or rescue", *b.Mode)
		}
	}
	for _, k := range b.RescueKeys {
		switch strings.TrimSpace(k) {
		case "local", "agent":
			return c.at(path+"/rescue-keys", "%s would be another key for every operator who applies pxe.yaml; name the key (ump config key add)", k)
		case "", "ump":
			return c.at(path+"/rescue-keys", "%q: name your keys; UMP's own is always there", k)
		}
	}
	if b.Console != nil && !consoleValue.MatchString(*b.Console) {
		return c.at(path+"/console", "%q is not a console (ttyS1,115200n8, ttyAMA0, none)", *b.Console)
	}
	n := b.Network
	if n.Uplink != "" {
		if !machine {
			return c.at(path+"/network/uplink", "the uplink is a machine's, by one of its MACs")
		}
		if _, err := NormalizeMAC(n.Uplink); err != nil {
			return c.at(path+"/network/uplink", "%v", err)
		}
	}
	for _, f := range []struct {
		name    string
		a       *Addressing
		dynamic string
	}{{"ipv4", n.IPv4, profile.UplinkDHCP}, {"ipv6", n.IPv6, profile.UplinkAuto}} {
		if f.a == nil {
			continue
		}
		at := path + "/network/" + f.name
		switch f.a.Mode {
		case "":
		case f.dynamic, profile.UplinkNone:
			continue
		default:
			return c.at(at, "%q is not %s, none or {address: ADDRESS/PREFIX, gateway: ADDRESS}", f.a.Mode, f.dynamic)
		}
		v6 := f.name == "ipv6"
		if f.a.Address != "" {
			p, err := netip.ParsePrefix(f.a.Address)
			if err != nil || p.Addr().Is4() == v6 || p.Addr().Zone() != "" {
				return c.at(at+"/address", "%q is not an %s address with its prefix length, like %s", f.a.Address, family(v6), example(v6))
			}
		}
		if f.a.Gateway != "" {
			g, err := netip.ParseAddr(f.a.Gateway)
			if err != nil || g.Is4() == v6 {
				return c.at(at+"/gateway", "%q is not an %s address", f.a.Gateway, family(v6))
			}
		}
	}
	if n.DNS != nil {
		for _, d := range n.DNS.Servers {
			if a, err := netip.ParseAddr(d); err != nil || a.Zone() != "" {
				return c.at(path+"/network/dns", "%q is not an IP address, nor dhcp", d)
			}
		}
	}
	return nil
}

func family(v6 bool) string {
	if v6 {
		return "IPv6"
	}
	return "IPv4"
}

func example(v6 bool) string {
	if v6 {
		return "2001:db8::17/64"
	}
	return "192.0.2.17/24"
}

// checkUplink checks a machine's merged network as an installation will
// (profile's ValidateUplink), with the kernel's IPv6 taken as on (the
// profile, which decides, is checked when the machine is installed).
func (c *Config) checkUplink(path string, m *Machine) error {
	u := m.Network
	if u.IPv4.Mode == profile.UplinkStatic && u.IPv4.Address == "" {
		return c.at(path+"/network/ipv4", "a gateway (%s) and no address; give the machine's address, or dhcp", u.IPv4.Gateway)
	}
	if u.IPv6.Mode == profile.UplinkStatic && u.IPv6.Address == "" {
		return c.at(path+"/network/ipv6", "a gateway (%s) and no address; give the machine's address, or auto or none", u.IPv6.Gateway)
	}
	spec := &profile.Spec{Kernel: profile.KernelSpec{IPv6: true}, Network: profile.NetworkSpec{Uplink: &u}}
	if err := spec.ValidateUplink(); err != nil {
		msg := strings.TrimPrefix(err.Error(), "network.uplink: ")
		msg = strings.ReplaceAll(msg, "network.uplink", "the network")
		sub := "/network"
		for _, f := range []string{"ipv4", "ipv6", "dns"} {
			if rest, ok := strings.CutPrefix(msg, f+": "); ok {
				sub, msg = sub+"/"+f, rest
			} else if strings.HasPrefix(msg, f+" ") {
				sub += "/" + f
			}
		}
		return c.at(path+sub, "%s", msg)
	}
	return nil
}
