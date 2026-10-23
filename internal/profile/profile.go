// Package profile reads the YAML profiles that say what a server becomes
// when ump installs it (operating system, disk layout, users, userdata,
// firewall, the reserved provision type), validates them and renders the
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
	Name        string        `yaml:"name"`
	Description string        `yaml:"description,omitempty"`
	OS          string        `yaml:"os"`
	Timezone    string        `yaml:"timezone,omitempty"`
	Kernel      Kernel        `yaml:"kernel,omitempty"`
	Disks       Disks         `yaml:"disks"`
	Users       []UserRef     `yaml:"users"`
	Userdata    []UserdataRef `yaml:"userdata,omitempty"`
	Firewall    FirewallRef   `yaml:"firewall,omitempty"`
	Provision   Provision     `yaml:"provision,omitempty"`
	Network     Network       `yaml:"network,omitempty"`
}

// FirewallRef is either the name of a firewall from firewalls.yaml or an
// inline one; neither means no firewall.
type FirewallRef struct {
	Name  string
	Rules *Firewall
}

func (f *FirewallRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&f.Name)
	case yaml.MappingNode:
		f.Rules = &Firewall{}
		return decodeStrict(n, f.Rules)
	}
	return fmt.Errorf("firewall: a firewall name or a firewall, not a %s", kindName(n.Kind))
}

func (f FirewallRef) MarshalYAML() (any, error) {
	if f.Rules != nil {
		return f.Rules, nil
	}
	return f.Name, nil
}

// IsZero makes an absent firewall stay absent when a profile is written.
func (f FirewallRef) IsZero() bool { return f.Name == "" && f.Rules == nil }

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
		return decodeStrict(n, d.Layout)
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

// User is an account of the new system: inline in a profile (with its
// name), or a named entry of users.yaml (the name is the key there).
type User struct {
	Name        string   `yaml:"name,omitempty" json:"name"`
	Description string   `yaml:"description,omitempty" json:"-"`
	Groups      []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	Sudo        bool     `yaml:"sudo,omitempty" json:"sudo,omitempty"`
	Shell       string   `yaml:"shell,omitempty" json:"shell,omitempty"`
	Keys        []string `yaml:"keys" json:"keys"`
}

// Validate checks a user: the name, the groups, the shell, and that it
// has key references (no passwords are set).
func (u *User) Validate() error {
	if !userName.MatchString(u.Name) {
		return fmt.Errorf("user name %q is not valid", u.Name)
	}
	for _, g := range u.Groups {
		if !userName.MatchString(g) {
			return fmt.Errorf("user %s: group name %q is not valid", u.Name, g)
		}
	}
	if u.Shell != "" && !strings.HasPrefix(u.Shell, "/") {
		return fmt.Errorf("user %s: shell %q is not a path", u.Name, u.Shell)
	}
	if len(u.Keys) == 0 {
		return fmt.Errorf("user %s has no keys (and no passwords are set)", u.Name)
	}
	return nil
}

// UserRef is either the name of a user from users.yaml or an inline
// user.
type UserRef struct {
	Name string
	User *User
}

func (r *UserRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Name)
	case yaml.MappingNode:
		r.User = &User{}
		return decodeStrict(n, r.User)
	}
	return fmt.Errorf("users: a user name or a user, not a %s", kindName(n.Kind))
}

func (r UserRef) MarshalYAML() (any, error) {
	if r.User != nil {
		return r.User, nil
	}
	return r.Name, nil
}

// ParseUsers reads users.yaml: named users.
func ParseUsers(data []byte) (map[string]*User, error) {
	var file struct {
		Users map[string]*User `yaml:"users"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	for name, u := range file.Users {
		if u == nil {
			return nil, fmt.Errorf("user %s is empty", name)
		}
		u.Name = name
	}
	return file.Users, nil
}

// Userdata is what runs in the freshly installed system before its
// first boot: a script for Ubuntu (type bash), later a declarative
// configuration for systems that take one (cloud-init, Talos, NixOS).
type Userdata struct {
	Name        string `yaml:"name,omitempty" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Type        string `yaml:"type,omitempty" json:"type"`
	Script      string `yaml:"script" json:"script"`
}

// UserdataTypes are the kinds of userdata ump knows how to apply: bash
// scripts for systems unpacked from an image (Ubuntu), NixOS modules for
// NixOS.
var UserdataTypes = []string{"bash", "nix"}

// Family is the kind of system an image name installs: nixos for the
// nixos-* images, image (unpacked and configured in place) for the rest.
func Family(os string) string {
	if strings.HasPrefix(os, "nixos") {
		return "nixos"
	}
	return "image"
}

// UserdataType is the userdata type a system family takes.
func UserdataType(family string) string {
	if family == "nixos" {
		return "nix"
	}
	return "bash"
}

// Validate checks a userdata entry.
func (u *Userdata) Validate() error {
	t := u.Type
	if t == "" {
		t = "bash"
	}
	known := false
	for _, k := range UserdataTypes {
		if k == t {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("userdata %s: type %q is not %s", u.Name, u.Type, strings.Join(UserdataTypes, ", "))
	}
	if strings.TrimSpace(u.Script) == "" {
		return fmt.Errorf("userdata %s: the script is empty", u.Name)
	}
	return nil
}

// UserdataRef is either the name of an entry of userdata.yaml or an
// inline one.
type UserdataRef struct {
	Name string
	Data *Userdata
}

func (r *UserdataRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Name)
	case yaml.MappingNode:
		r.Data = &Userdata{}
		return decodeStrict(n, r.Data)
	}
	return fmt.Errorf("userdata: a userdata name or a userdata, not a %s", kindName(n.Kind))
}

func (r UserdataRef) MarshalYAML() (any, error) {
	if r.Data != nil {
		return r.Data, nil
	}
	return r.Name, nil
}

// ParseUserdata reads userdata.yaml: named userdata.
func ParseUserdata(data []byte) (map[string]*Userdata, error) {
	var file struct {
		Userdata map[string]*Userdata `yaml:"userdata"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	for name, u := range file.Userdata {
		if u == nil {
			return nil, fmt.Errorf("userdata %s is empty", name)
		}
		u.Name = name
		if u.Type == "" {
			u.Type = "bash"
		}
	}
	return file.Userdata, nil
}

// IsUserdataName reports whether s can name an entry of userdata.yaml
// in a profile.
func IsUserdataName(s string) bool { return nameRe.MatchString(s) }

// HasUserdataRefs reports whether the profile names entries of
// userdata.yaml.
func HasUserdataRefs(p *Profile) bool {
	for _, r := range p.Userdata {
		if r.Data == nil {
			return true
		}
	}
	return false
}

// ResolveUserdata turns the profile's userdata references into entries:
// inline ones as they are, names looked up in userdata.
func (p *Profile) ResolveUserdata(userdata map[string]*Userdata) ([]Userdata, error) {
	out := []Userdata{}
	seen := map[string]bool{}
	for i, r := range p.Userdata {
		var u Userdata
		switch {
		case r.Data != nil:
			u = *r.Data
			if u.Name == "" {
				u.Name = fmt.Sprintf("inline-%d", i+1)
			}
			if u.Type == "" {
				u.Type = "bash"
			}
		default:
			known, ok := userdata[r.Name]
			if !ok {
				return nil, fmt.Errorf("profile %s: userdata %q is not in userdata.yaml (known: %s; ump config os update fetches the published ones)", p.Name, r.Name, strings.Join(sortedKeys(userdata), ", "))
			}
			u = *known
		}
		if err := u.Validate(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("profile %s: userdata %s is listed twice", p.Name, u.Name)
		}
		seen[u.Name] = true
		out = append(out, u)
	}
	return out, nil
}

// HasUserRefs reports whether the profile names users from users.yaml.
func HasUserRefs(p *Profile) bool {
	for _, r := range p.Users {
		if r.User == nil {
			return true
		}
	}
	return false
}

// ResolveUsers turns the profile's user references into users: inline
// ones as they are, names looked up in users.
func (p *Profile) ResolveUsers(users map[string]*User) ([]User, error) {
	var out []User
	seen := map[string]bool{}
	for i, r := range p.Users {
		var u User
		switch {
		case r.User != nil:
			u = *r.User
		case r.Name != "":
			def, ok := users[r.Name]
			if !ok {
				return nil, fmt.Errorf("profile %s: user %q is not in users.yaml (known: %s; ump config os update fetches the published ones)", p.Name, r.Name, strings.Join(sortedKeys(users), ", "))
			}
			u = *def
		default:
			return nil, fmt.Errorf("profile %s: user %d is empty", p.Name, i+1)
		}
		if err := u.Validate(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("profile %s: user %s is listed twice", p.Name, u.Name)
		}
		seen[u.Name] = true
		out = append(out, u)
	}
	return out, nil
}

// Firewall is the input policy and what it lets in, on the host and
// forwarded in from outside (containers' published ports) alike.
type Firewall struct {
	Name        string  `yaml:"-" json:"name,omitempty"`
	Description string  `yaml:"description,omitempty" json:"-"`
	Policy      string  `yaml:"policy,omitempty" json:"policy"`
	Allow       []Allow `yaml:"allow,omitempty" json:"allow"`
}

// Validate checks the policy and the allowed entries.
func (f *Firewall) Validate() error {
	at := "firewall"
	if f.Name != "" {
		at = "firewall " + f.Name
	}
	switch f.Policy {
	case "accept", "drop":
	default:
		return fmt.Errorf("%s: policy %q is not accept or drop", at, f.Policy)
	}
	for _, a := range f.Allow {
		switch a.Proto {
		case "icmp":
			if a.Port != "" {
				return fmt.Errorf("%s: icmp has no port", at)
			}
		case "tcp", "udp":
			m := portSpec.FindStringSubmatch(a.Port)
			if m == nil {
				return fmt.Errorf("%s: %s needs a port or a range like 8000-8100, not %q", at, a.Proto, a.Port)
			}
			for _, s := range []string{m[1], m[3]} {
				if s == "" {
					continue
				}
				if n, _ := strconv.Atoi(s); n < 1 || n > 65535 {
					return fmt.Errorf("%s: port %s is out of range", at, s)
				}
			}
		default:
			return fmt.Errorf("%s: protocol %q is not icmp, tcp or udp", at, a.Proto)
		}
		if a.From != "" && !regexp.MustCompile(`^[0-9a-fA-F.:]+(/[0-9]+)?$`).MatchString(a.From) {
			return fmt.Errorf("%s: from %q is not an address or a network", at, a.From)
		}
	}
	return nil
}

// ParseFirewalls reads firewalls.yaml: named firewalls.
func ParseFirewalls(data []byte) (map[string]*Firewall, error) {
	var file struct {
		Firewalls map[string]*Firewall `yaml:"firewalls"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	for name, f := range file.Firewalls {
		if f == nil {
			return nil, fmt.Errorf("firewall %s is empty", name)
		}
		f.Name = name
		if f.Policy == "" {
			f.Policy = "drop"
		}
	}
	return file.Firewalls, nil
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
		if err := decodeStrict(n, &p); err != nil {
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

// Provision is reserved for ansible-pull, the server converging itself
// later. The type and the other fields are recorded on the server and
// change nothing else: ump provisions by push (internal/cli/ansible.go).
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
	if p.Firewall.Rules != nil && p.Firewall.Rules.Policy == "" {
		p.Firewall.Rules.Policy = "drop"
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
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
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
	for _, r := range p.Users {
		if r.User != nil {
			if err := r.User.Validate(); err != nil {
				return fmt.Errorf("profile %s: %w", p.Name, err)
			}
		}
	}
	for _, r := range p.Userdata {
		if r.Data == nil && !IsUserdataName(r.Name) {
			return fmt.Errorf("profile %s: %q is not a userdata name", p.Name, r.Name)
		}
	}
	if p.Firewall.Rules != nil {
		if err := p.Firewall.Rules.Validate(); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
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
// user with sudo, else root when it is listed, else nothing. Named
// users are looked up in users; an unknown name is skipped.
func (p *Profile) Admin(users map[string]*User) string {
	var list []User
	for _, r := range p.Users {
		switch {
		case r.User != nil:
			list = append(list, *r.User)
		case users[r.Name] != nil:
			list = append(list, *users[r.Name])
		}
	}
	return admin(list)
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
	Userdata  []Userdata  `json:"userdata"`
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

// Resolve validates the profile, picks the layout and the firewall (the
// profile's inline ones, or by name from layouts and firewalls) and
// resolves the key references, and returns the specification. The
// caller fills in the hostname, storage and disks.
func (p *Profile) Resolve(layouts map[string]*Layout, firewalls map[string]*Firewall, users map[string]*User, userdata map[string]*Userdata, resolve KeyResolver) (*Spec, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	resolvedUsers, err := p.ResolveUsers(users)
	if err != nil {
		return nil, err
	}
	resolvedUserdata, err := p.ResolveUserdata(userdata)
	if err != nil {
		return nil, err
	}
	want := UserdataType(Family(p.OS))
	for _, u := range resolvedUserdata {
		if u.Type != want {
			return nil, fmt.Errorf("profile %s: userdata %s is %s, and %s takes %s userdata (see docs/userdata.md)", p.Name, u.Name, u.Type, p.OS, want)
		}
	}
	if admin(resolvedUsers) == "" {
		return nil, fmt.Errorf("profile %s: no user has sudo and root is not listed; ump could not administer the new system", p.Name)
	}
	layout := p.Disks.Layout
	if layout == nil {
		l, ok := layouts[p.Disks.Name]
		if !ok {
			return nil, fmt.Errorf("profile %s: layout %q is not in layouts.yaml (known: %s; ump config os update fetches the published ones)", p.Name, p.Disks.Name, strings.Join(sortedKeys(layouts), ", "))
		}
		if err := l.Validate(); err != nil {
			return nil, err
		}
		layout = l
	}
	firewall := Firewall{Policy: "accept", Allow: []Allow{}}
	switch {
	case p.Firewall.Rules != nil:
		firewall = *p.Firewall.Rules
	case p.Firewall.Name != "":
		f, ok := firewalls[p.Firewall.Name]
		if !ok {
			return nil, fmt.Errorf("profile %s: firewall %q is not in firewalls.yaml (known: %s; ump config os update fetches the published ones)", p.Name, p.Firewall.Name, strings.Join(sortedKeys(firewalls), ", "))
		}
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		firewall = *f
	}
	if firewall.Allow == nil {
		firewall.Allow = []Allow{}
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
		Userdata:  resolvedUserdata,
		Firewall:  firewall,
		Provision: p.Provision,
		Network:   NetworkSpec{Rename: p.Network.Rename == nil || *p.Network.Rename},
		Profile:   ProfileInfo{Name: p.Name, Description: p.Description},
	}
	for _, u := range resolvedUsers {
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

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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

// Set applies assignments of the form FIELD=VALUE to a copy of the
// profile and returns it, parsed and checked as a profile again. FIELD
// is a path into the YAML (kernel.ipv6, users[0].shell, users[ump].keys
// for the user named ump, users[+] to append one); VALUE is YAML
// (true, 8, a word, [a, b], {name: ops, sudo: true}). An unknown field
// is an error, as in a profile file.
func (p *Profile) Set(assignments []string) (*Profile, error) {
	data, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	for _, a := range assignments {
		path, value, ok := strings.Cut(a, "=")
		if !ok || strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("--set %q: expected FIELD=VALUE", a)
		}
		var v any
		if err := yaml.Unmarshal([]byte(value), &v); err != nil {
			return nil, fmt.Errorf("--set %s: value %q is not YAML: %w", path, value, err)
		}
		if err := setPath(doc, strings.TrimSpace(path), v); err != nil {
			return nil, fmt.Errorf("--set %s: %w", path, err)
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	q, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("after --set: %w", err)
	}
	return q, nil
}

var pathSegment = regexp.MustCompile(`^([A-Za-z0-9_-]+)(\[([^\]]*)\])?$`)

// setPath sets value at a dotted path in a YAML document.
func setPath(doc map[string]any, path string, value any) error {
	segments := strings.Split(path, ".")
	var cur any = doc
	for i, seg := range segments {
		m := pathSegment.FindStringSubmatch(seg)
		if m == nil {
			return fmt.Errorf("%q is not a field name", seg)
		}
		key, indexed, index := m[1], m[2] != "", m[3]
		last := i == len(segments)-1
		node, ok := cur.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not a mapping", strings.Join(segments[:i], "."))
		}
		if !indexed {
			if last {
				node[key] = value
				return nil
			}
			next, ok := node[key].(map[string]any)
			if !ok {
				if node[key] != nil {
					return fmt.Errorf("%s is not a mapping", key)
				}
				next = map[string]any{}
				node[key] = next
			}
			cur = next
			continue
		}
		list, _ := node[key].([]any)
		pos := -1
		switch {
		case index == "+":
			list = append(list, nil)
			pos = len(list) - 1
		default:
			if n, err := strconv.Atoi(index); err == nil {
				pos = n
			} else {
				for j, item := range list {
					if im, ok := item.(map[string]any); ok && im["name"] == index {
						pos = j
					}
				}
				if pos < 0 {
					return fmt.Errorf("no entry of %s named %q", key, index)
				}
			}
		}
		if pos < 0 || pos >= len(list) {
			return fmt.Errorf("%s has %d entries, no index %d", key, len(list), pos)
		}
		node[key] = list
		if last {
			list[pos] = value
			return nil
		}
		next, ok := list[pos].(map[string]any)
		if !ok {
			if list[pos] != nil {
				return fmt.Errorf("%s[%s] is not a mapping", key, index)
			}
			next = map[string]any{}
			list[pos] = next
		}
		cur = next
	}
	return nil
}
