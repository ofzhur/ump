// Package profile reads the YAML profiles that say what a server becomes
// when ump installs it (operating system, disk layout, users, packages,
// firewall, the provision step), validates them and renders the
// specification the installer takes.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile is a server profile as written in YAML.
type Profile struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description,omitempty"`
	OS          string    `yaml:"os"`
	Timezone    string    `yaml:"timezone,omitempty"`
	Kernel      Kernel    `yaml:"kernel,omitempty"`
	Disks       Disks     `yaml:"disks"`
	Users       []User    `yaml:"users"`
	Packages    []string  `yaml:"packages,omitempty"`
	Firewall    Firewall  `yaml:"firewall,omitempty"`
	Provision   Provision `yaml:"provision,omitempty"`
	Network     Network   `yaml:"network,omitempty"`
}

// Network holds the network choices. Rename (unset: on) gives the
// interfaces uniform names: ext01, ext02, ... for the ones with a public
// address and int01, ... for the others, instead of the kernel's.
type Network struct {
	Rename *bool `yaml:"rename,omitempty"`
}

// Kernel holds the kernel command line choices; unset means off for the
// two switches, which is the default of ump install.
type Kernel struct {
	Mitigations *bool  `yaml:"mitigations,omitempty"`
	IPv6        *bool  `yaml:"ipv6,omitempty"`
	Extra       string `yaml:"extra,omitempty"`
}

// Disks is either the name of a layout from layouts.yaml or an inline
// layout.
type Disks struct {
	Name   string
	Layout *Layout
}

func (d *Disks) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&d.Name)
	case yaml.MappingNode:
		d.Layout = &Layout{}
		return n.Decode(d.Layout)
	}
	return fmt.Errorf("disks: a layout name or a layout, not a %s", kindName(n.Kind))
}

func (d Disks) MarshalYAML() (any, error) {
	if d.Layout != nil {
		return d.Layout, nil
	}
	return d.Name, nil
}

// Layout is a disk layout: parts applied in order.
type Layout struct {
	Name        string `yaml:"-" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Parts       []Part `yaml:"parts" json:"parts"`
}

// Part is one step of a layout: a slice (a partition on every matching
// disk), an md array, or a logical volume.
type Part struct {
	Type       string   `yaml:"type" json:"type"`
	Target     []string `yaml:"target,omitempty" json:"target,omitempty"`
	Index      int      `yaml:"index,omitempty" json:"index,omitempty"`
	Size       string   `yaml:"size" json:"size"`
	Filesystem string   `yaml:"filesystem" json:"filesystem"`
	Mountpoint string   `yaml:"mountpoint,omitempty" json:"mountpoint,omitempty"`
	Label      string   `yaml:"label,omitempty" json:"label,omitempty"`
	VG         string   `yaml:"vg,omitempty" json:"vg,omitempty"`
	Name       string   `yaml:"name,omitempty" json:"name,omitempty"`
	Raid       *int     `yaml:"raid,omitempty" json:"raid,omitempty"`
}

// User is an account of the new system.
type User struct {
	Name   string   `yaml:"name" json:"name"`
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	Sudo   bool     `yaml:"sudo,omitempty" json:"sudo,omitempty"`
	Shell  string   `yaml:"shell,omitempty" json:"shell,omitempty"`
	Keys   []string `yaml:"keys" json:"keys"`
}

// Firewall is the input policy and what it lets in.
type Firewall struct {
	Policy string  `yaml:"policy,omitempty" json:"policy"`
	Allow  []Allow `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// Allow is one rule: a protocol, a port or range for tcp and udp, and
// optionally the source. In YAML it is "icmp", "ssh", "tcp/80",
// "udp/53", "tcp/8000-8100", or a mapping with proto, port and from.
type Allow struct {
	Proto string `yaml:"proto" json:"proto"`
	Port  string `yaml:"port,omitempty" json:"port,omitempty"`
	From  string `yaml:"from,omitempty" json:"from,omitempty"`
}

func (a *Allow) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		type plain Allow
		var p plain
		if err := n.Decode(&p); err != nil {
			return err
		}
		*a = Allow(p)
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	switch s {
	case "icmp", "ping":
		*a = Allow{Proto: "icmp"}
	case "ssh":
		*a = Allow{Proto: "tcp", Port: "22"}
	default:
		proto, port, ok := strings.Cut(s, "/")
		if !ok {
			return fmt.Errorf("firewall: %q is not icmp, ssh, tcp/PORT or udp/PORT", s)
		}
		*a = Allow{Proto: proto, Port: port}
	}
	return nil
}

// Provision is the step after the first boot. Only the type is known to
// ump yet; the other fields are kept for the step to read.
type Provision struct {
	Type    string            `yaml:"type,omitempty" json:"type"`
	Options map[string]string `yaml:",inline" json:"options,omitempty"`
}

// Parse reads a profile from YAML and fills in the defaults.
func Parse(data []byte) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if p.Firewall.Policy == "" {
		p.Firewall.Policy = "accept"
	}
	if p.Provision.Type == "" {
		p.Provision.Type = "none"
	}
	return &p, nil
}

// Load reads a profile file.
func Load(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParseLayouts reads layouts.yaml: named layouts.
func ParseLayouts(data []byte) (map[string]*Layout, error) {
	var file struct {
		Layouts map[string]*Layout `yaml:"layouts"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	for name, l := range file.Layouts {
		if l == nil {
			return nil, fmt.Errorf("layout %s is empty", name)
		}
		l.Name = name
	}
	return file.Layouts, nil
}

var (
	userName   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	vgName     = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
	sizeSpec   = regexp.MustCompile(`^([0-9]+)([KMGT]|%)$`)
	portSpec   = regexp.MustCompile(`^([0-9]{1,5})(-([0-9]{1,5}))?$`)
	keyLine    = regexp.MustCompile(`^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/=]+( .*)?$`)
	mountPoint = regexp.MustCompile(`^/([A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*)?$`)
)

// filesystems the installer can make, and the special ones.
var filesystems = map[string]bool{"ext4": true, "xfs": true, "btrfs": true, "vfat": true, "swap": true, "efi": true, "bios": true, "lvm": true, "none": true}

// Validate checks a layout: types, sizes, file systems, mountpoints, the
// volume groups, the raid levels, and that the root and the boot
// partitions are where the boot loader can use them.
func (l *Layout) Validate() error {
	if len(l.Parts) == 0 {
		return fmt.Errorf("layout %s has no parts", l.Name)
	}
	vgs := map[string]bool{}
	mounts := map[string]bool{}
	rootOn := ""
	for i, p := range l.Parts {
		at := fmt.Sprintf("layout %s, part %d (%s)", l.Name, i+1, p.Type)
		if !filesystems[p.Filesystem] {
			return fmt.Errorf("%s: unknown filesystem %q", at, p.Filesystem)
		}
		if p.Size != "rest" && p.Size != "all" && !sizeSpec.MatchString(p.Size) {
			return fmt.Errorf("%s: size %q is not like 1M, 512M, 1G, 20%% or rest", at, p.Size)
		}
		if p.Mountpoint != "" {
			if !mountPoint.MatchString(p.Mountpoint) {
				return fmt.Errorf("%s: mountpoint %q is not an absolute path", at, p.Mountpoint)
			}
			if p.Filesystem == "swap" || p.Filesystem == "bios" || p.Filesystem == "lvm" || p.Filesystem == "none" {
				return fmt.Errorf("%s: a %s part has no mountpoint", at, p.Filesystem)
			}
			if mounts[p.Mountpoint] {
				return fmt.Errorf("%s: mountpoint %s is used twice", at, p.Mountpoint)
			}
			mounts[p.Mountpoint] = true
			if p.Mountpoint == "/" {
				rootOn = p.Type
			}
		}
		if p.Filesystem == "efi" && p.Mountpoint == "" {
			return fmt.Errorf("%s: an efi part needs mountpoint /boot/efi", at)
		}
		if p.Filesystem == "lvm" {
			if p.VG == "" || !vgName.MatchString(p.VG) {
				return fmt.Errorf("%s: a part with filesystem lvm needs the vg it joins", at)
			}
			vgs[p.VG] = true
		}
		switch p.Type {
		case "slice", "md":
			if len(p.Target) == 0 {
				return fmt.Errorf("%s: no target (first, or a regular expression on the disk path)", at)
			}
			for _, t := range p.Target {
				if t == "first" {
					continue
				}
				if _, err := regexp.Compile(t); err != nil {
					return fmt.Errorf("%s: target %q: %v", at, t, err)
				}
			}
			if p.Type == "slice" && (p.Index < 0 || p.Index > 128) {
				return fmt.Errorf("%s: index %d is not between 1 and 128", at, p.Index)
			}
			if p.Type == "md" {
				if p.Raid == nil {
					return fmt.Errorf("%s: an md part needs a raid level", at)
				}
				switch *p.Raid {
				case 0, 1, 4, 5, 6, 10:
				default:
					return fmt.Errorf("%s: raid %d is not an md level (0, 1, 4, 5, 6, 10)", at, *p.Raid)
				}
				if p.Filesystem == "efi" || p.Filesystem == "bios" {
					return fmt.Errorf("%s: the boot loader cannot use %s on md", at, p.Filesystem)
				}
			}
		case "lvm":
			if p.VG == "" || !vgs[p.VG] {
				return fmt.Errorf("%s: vg %q is not made by an earlier slice or md part", at, p.VG)
			}
			if p.Name == "" || !vgName.MatchString(p.Name) {
				return fmt.Errorf("%s: a logical volume needs a name", at)
			}
			if p.Raid != nil {
				switch *p.Raid {
				case 0, 1, 5, 6, 10:
				default:
					return fmt.Errorf("%s: raid %d is not an lvm level (0, 1, 5, 6, 10)", at, *p.Raid)
				}
			}
			switch p.Filesystem {
			case "efi", "bios", "lvm":
				return fmt.Errorf("%s: a logical volume cannot be %s", at, p.Filesystem)
			}
		default:
			return fmt.Errorf("%s: unknown part type %q (slice, md, lvm)", at, p.Type)
		}
	}
	if rootOn == "" {
		return fmt.Errorf("layout %s: no part is mounted at /", l.Name)
	}
	if rootOn != "slice" && !mounts["/boot"] {
		return fmt.Errorf("layout %s: with root on %s, /boot must be a slice of its own (the boot loader reads plain partitions only)", l.Name, rootOn)
	}
	return nil
}

// Validate checks the profile apart from the keys, which Resolve checks.
func (p *Profile) Validate() error {
	if p.OS == "" || strings.ContainsAny(p.OS, "/ ") {
		return fmt.Errorf("profile %s: os %q is not an image name", p.Name, p.OS)
	}
	if p.Disks.Name == "" && p.Disks.Layout == nil {
		return fmt.Errorf("profile %s: disks names no layout", p.Name)
	}
	if p.Disks.Layout != nil {
		if p.Disks.Layout.Name == "" {
			p.Disks.Layout.Name = "inline"
		}
		if err := p.Disks.Layout.Validate(); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	if len(p.Users) == 0 {
		return fmt.Errorf("profile %s: no users; a sudo user (or root) with a key is needed to reach the new system", p.Name)
	}
	seen := map[string]bool{}
	for _, u := range p.Users {
		if !userName.MatchString(u.Name) {
			return fmt.Errorf("profile %s: user name %q is not valid", p.Name, u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("profile %s: user %s is listed twice", p.Name, u.Name)
		}
		seen[u.Name] = true
		for _, g := range u.Groups {
			if !userName.MatchString(g) {
				return fmt.Errorf("profile %s: user %s: group name %q is not valid", p.Name, u.Name, g)
			}
		}
		if u.Shell != "" && !strings.HasPrefix(u.Shell, "/") {
			return fmt.Errorf("profile %s: user %s: shell %q is not a path", p.Name, u.Name, u.Shell)
		}
		if len(u.Keys) == 0 {
			return fmt.Errorf("profile %s: user %s has no keys (and no passwords are set)", p.Name, u.Name)
		}
	}
	if p.Admin() == "" {
		return fmt.Errorf("profile %s: no user has sudo and root is not listed; ump could not administer the new system", p.Name)
	}
	for _, pkg := range p.Packages {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`).MatchString(pkg) {
			return fmt.Errorf("profile %s: %q is not a package name", p.Name, pkg)
		}
	}
	switch p.Firewall.Policy {
	case "accept", "drop":
	default:
		return fmt.Errorf("profile %s: firewall policy %q is not accept or drop", p.Name, p.Firewall.Policy)
	}
	for _, a := range p.Firewall.Allow {
		switch a.Proto {
		case "icmp":
			if a.Port != "" {
				return fmt.Errorf("profile %s: firewall: icmp has no port", p.Name)
			}
		case "tcp", "udp":
			m := portSpec.FindStringSubmatch(a.Port)
			if m == nil {
				return fmt.Errorf("profile %s: firewall: %s needs a port or a range like 8000-8100, not %q", p.Name, a.Proto, a.Port)
			}
			for _, s := range []string{m[1], m[3]} {
				if s == "" {
					continue
				}
				if n, _ := strconv.Atoi(s); n < 1 || n > 65535 {
					return fmt.Errorf("profile %s: firewall: port %s is out of range", p.Name, s)
				}
			}
		default:
			return fmt.Errorf("profile %s: firewall: protocol %q is not icmp, tcp or udp", p.Name, a.Proto)
		}
		if a.From != "" && !regexp.MustCompile(`^[0-9a-fA-F.:]+(/[0-9]+)?$`).MatchString(a.From) {
			return fmt.Errorf("profile %s: firewall: from %q is not an address or a network", p.Name, a.From)
		}
	}
	switch p.Provision.Type {
	case "none", "ansible-pull", "ansible-bootstrap":
	default:
		return fmt.Errorf("profile %s: provision type %q is not none, ansible-pull or ansible-bootstrap", p.Name, p.Provision.Type)
	}
	return nil
}

// Admin is the user ump logs in as on the installed system: the first
// user with sudo, else root when it is listed, else nothing.
func (p *Profile) Admin() string {
	return admin(p.Users)
}

func admin(users []User) string {
	for _, u := range users {
		if u.Sudo && u.Name != "root" {
			return u.Name
		}
	}
	for _, u := range users {
		if u.Name == "root" {
			return "root"
		}
	}
	return ""
}

// KeyResolver turns a key reference of a profile (ump, local, agent, a
// file path, or a key line) into key lines.
type KeyResolver func(ref string) ([]string, error)

// IsKeyLine reports whether s is an authorized_keys line.
func IsKeyLine(s string) bool { return keyLine.MatchString(strings.TrimSpace(s)) }

// Spec is what the installer takes: the profile with the layout inlined,
// the keys resolved and the machine's own values filled in.
type Spec struct {
	OS        string      `json:"os"`
	Hostname  string      `json:"hostname"`
	Timezone  string      `json:"timezone"`
	Storage   string      `json:"storage"`
	Kernel    KernelSpec  `json:"kernel"`
	Layout    *Layout     `json:"layout"`
	Disks     []string    `json:"disks,omitempty"`
	Users     []User      `json:"users"`
	Packages  []string    `json:"packages"`
	Firewall  Firewall    `json:"firewall"`
	Provision Provision   `json:"provision"`
	Network   NetworkSpec `json:"network"`
	Profile   ProfileInfo `json:"profile"`
}

// NetworkSpec is Network with the default filled in.
type NetworkSpec struct {
	Rename bool `json:"rename"`
}

// Admin is the user ump logs in as on the installed system.
func (s *Spec) Admin() string { return admin(s.Users) }

type KernelSpec struct {
	Mitigations bool   `json:"mitigations"`
	IPv6        bool   `json:"ipv6"`
	Extra       string `json:"extra,omitempty"`
}

type ProfileInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Resolve validates the profile, picks the layout (the profile's inline
// one, or by name from layouts) and resolves the key references, and
// returns the specification. The caller fills in the hostname, storage
// and disks.
func (p *Profile) Resolve(layouts map[string]*Layout, resolve KeyResolver) (*Spec, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	layout := p.Disks.Layout
	if layout == nil {
		l, ok := layouts[p.Disks.Name]
		if !ok {
			names := make([]string, 0, len(layouts))
			for n := range layouts {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("profile %s: layout %q is not in layouts.yaml (known: %s)", p.Name, p.Disks.Name, strings.Join(names, ", "))
		}
		if err := l.Validate(); err != nil {
			return nil, err
		}
		layout = l
	}
	spec := &Spec{
		OS:       p.OS,
		Timezone: p.Timezone,
		Kernel: KernelSpec{
			Mitigations: p.Kernel.Mitigations != nil && *p.Kernel.Mitigations,
			IPv6:        p.Kernel.IPv6 != nil && *p.Kernel.IPv6,
			Extra:       strings.TrimSpace(p.Kernel.Extra),
		},
		Layout:    layout,
		Packages:  append([]string{}, p.Packages...),
		Firewall:  p.Firewall,
		Provision: p.Provision,
		Network:   NetworkSpec{Rename: p.Network.Rename == nil || *p.Network.Rename},
		Profile:   ProfileInfo{Name: p.Name, Description: p.Description},
	}
	if spec.Packages == nil {
		spec.Packages = []string{}
	}
	if spec.Firewall.Allow == nil {
		spec.Firewall.Allow = []Allow{}
	}
	for _, u := range p.Users {
		var keys []string
		seen := map[string]bool{}
		for _, ref := range u.Keys {
			lines, err := resolve(ref)
			if err != nil {
				return nil, fmt.Errorf("profile %s: user %s: key %q: %w", p.Name, u.Name, ref, err)
			}
			for _, k := range lines {
				k = strings.TrimSpace(k)
				if k == "" || strings.HasPrefix(k, "#") || seen[k] {
					continue
				}
				if !IsKeyLine(k) {
					return nil, fmt.Errorf("profile %s: user %s: key %q: %q is not a public key line", p.Name, u.Name, ref, truncate(k))
				}
				seen[k] = true
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("profile %s: user %s: the key references %v gave no key", p.Name, u.Name, u.Keys)
		}
		shell := u.Shell
		if shell == "" {
			shell = "/bin/bash"
		}
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		spec.Users = append(spec.Users, User{Name: u.Name, Groups: groups, Sudo: u.Sudo, Shell: shell, Keys: keys})
	}
	return spec, nil
}

// JSON renders the specification for the installer.
func (s *Spec) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "list"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	}
	return "node"
}
