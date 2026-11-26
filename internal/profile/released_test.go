package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The files of profiles/ are published, and every released ump reads
// them: ump config os update puts layouts.yaml, firewalls.yaml,
// networks.yaml, users.yaml and userdata.yaml beside the profiles, and a
// profile that is not there is fetched by its name. They are decoded
// with KnownFields, so one field a released ump does not know fails the
// whole file there: with layouts.yaml every reinstall, whatever its
// layout.
//
// The types below are what ump v0.9.0 decodes these files into
// (internal/profile of that tag: the fields with their YAML names and
// types, and the forms its UnmarshalYAML methods take), kept as they
// were. TestPublishedFilesReadByReleasedUmp decodes the published files
// into them. Nothing is added here for a new field: the types change
// when no ump older than the one that knows the field is in use.

type v090Profile struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	OS          string            `yaml:"os"`
	Timezone    string            `yaml:"timezone,omitempty"`
	Kernel      v090Kernel        `yaml:"kernel,omitempty"`
	Disks       v090Disks         `yaml:"disks"`
	Users       []v090UserRef     `yaml:"users"`
	Userdata    []v090UserdataRef `yaml:"userdata,omitempty"`
	Firewall    v090FirewallRef   `yaml:"firewall,omitempty"`
	Provision   v090Ignored       `yaml:"provision,omitempty"`
	Network     v090Network       `yaml:"network,omitempty"`
}

type v090Ignored struct{}

func (*v090Ignored) UnmarshalYAML(*yaml.Node) error { return nil }

type v090Kernel struct {
	Mitigations *bool  `yaml:"mitigations,omitempty"`
	IPv6        *bool  `yaml:"ipv6,omitempty"`
	Extra       string `yaml:"extra,omitempty"`
}

type v090Network struct {
	Rename  *bool               `yaml:"rename,omitempty"`
	Private v090PrivateNetworks `yaml:"private,omitempty"`
}

type v090PrivateNetworks []string

func (l *v090PrivateNetworks) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = nil
		if n.Value != "" {
			*l = v090PrivateNetworks{n.Value}
		}
		return nil
	case yaml.SequenceNode:
		out := v090PrivateNetworks{}
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

type v090Disks struct {
	Name   string
	Layout *v090Layout
}

func (d *v090Disks) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&d.Name)
	case yaml.MappingNode:
		d.Layout = &v090Layout{}
		return decodeStrict(n, d.Layout)
	}
	return fmt.Errorf("disks: a layout name or a layout, not a %s", kindName(n.Kind))
}

type v090Layout struct {
	Name        string     `yaml:"-"`
	Description string     `yaml:"description,omitempty"`
	Parts       []v090Part `yaml:"parts"`
}

type v090Part struct {
	Type       string   `yaml:"type"`
	Target     []string `yaml:"target,omitempty"`
	Index      int      `yaml:"index,omitempty"`
	Size       string   `yaml:"size"`
	Filesystem string   `yaml:"filesystem"`
	Mountpoint string   `yaml:"mountpoint,omitempty"`
	Label      string   `yaml:"label,omitempty"`
	VG         string   `yaml:"vg,omitempty"`
	Name       string   `yaml:"name,omitempty"`
	Raid       *int     `yaml:"raid,omitempty"`
}

type v090User struct {
	Name        string   `yaml:"name,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Groups      []string `yaml:"groups,omitempty"`
	Sudo        bool     `yaml:"sudo,omitempty"`
	Shell       string   `yaml:"shell,omitempty"`
	Keys        []string `yaml:"keys"`
}

type v090UserRef struct {
	Name string
	User *v090User
}

func (r *v090UserRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Name)
	case yaml.MappingNode:
		r.User = &v090User{}
		return decodeStrict(n, r.User)
	}
	return fmt.Errorf("users: a user name or a user, not a %s", kindName(n.Kind))
}

type v090Userdata struct {
	Name        string `yaml:"name,omitempty"`
	Description string `yaml:"description,omitempty"`
	Type        string `yaml:"type,omitempty"`
	Script      string `yaml:"script"`
}

type v090UserdataRef struct {
	Name string
	Data *v090Userdata
}

func (r *v090UserdataRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Name)
	case yaml.MappingNode:
		r.Data = &v090Userdata{}
		return decodeStrict(n, r.Data)
	}
	return fmt.Errorf("userdata: a userdata name or a userdata, not a %s", kindName(n.Kind))
}

type v090Firewall struct {
	Name        string      `yaml:"-"`
	Description string      `yaml:"description,omitempty"`
	Policy      string      `yaml:"policy,omitempty"`
	Allow       []v090Allow `yaml:"allow,omitempty"`
}

type v090FirewallRef struct {
	Name  string
	Rules *v090Firewall
}

func (f *v090FirewallRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&f.Name)
	case yaml.MappingNode:
		f.Rules = &v090Firewall{}
		return decodeStrict(n, f.Rules)
	}
	return fmt.Errorf("firewall: a firewall name or a firewall, not a %s", kindName(n.Kind))
}

type v090Allow struct {
	Proto string `yaml:"proto"`
	Port  string `yaml:"port,omitempty"`
	From  string `yaml:"from,omitempty"`
}

func (a *v090Allow) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		type plain v090Allow
		var p plain
		if err := decodeStrict(n, &p); err != nil {
			return err
		}
		*a = v090Allow(p)
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	switch s {
	case "icmp", "ping":
		*a = v090Allow{Proto: "icmp"}
	case "ssh":
		*a = v090Allow{Proto: "tcp", Port: "22"}
	default:
		proto, port, ok := strings.Cut(s, "/")
		if !ok {
			return fmt.Errorf("firewall: %q is not icmp, ssh, tcp/PORT or udp/PORT", s)
		}
		*a = v090Allow{Proto: proto, Port: port}
	}
	return nil
}

type v090PrivateNetwork struct {
	Name        string   `yaml:"-"`
	Description string   `yaml:"description,omitempty"`
	CIDR        string   `yaml:"cidr"`
	Reserved    []string `yaml:"reserved,omitempty"`
}

// v090Decode reads a published file as ump v0.9.0 does: the files of
// named entries into their maps, every other file as a profile, with
// KnownFields.
func v090Decode(file string, data []byte) error {
	var v any
	switch file {
	case "layouts.yaml":
		v = &struct {
			Layouts map[string]*v090Layout `yaml:"layouts"`
		}{}
	case "firewalls.yaml":
		v = &struct {
			Firewalls map[string]*v090Firewall `yaml:"firewalls"`
		}{}
	case "networks.yaml":
		v = &struct {
			Networks map[string]*v090PrivateNetwork `yaml:"networks"`
		}{}
	case "users.yaml":
		v = &struct {
			Users map[string]*v090User `yaml:"users"`
		}{}
	case "userdata.yaml":
		v = &struct {
			Userdata map[string]*v090Userdata `yaml:"userdata"`
		}{}
	default:
		v = &v090Profile{}
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// TestPublishedFilesReadByReleasedUmp: every published file has the
// fields of ump v0.9.0 and no others, in the forms it takes, so that a
// released ump still reads the file when a newer one is published.
func TestPublishedFilesReadByReleasedUmp(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "profiles", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no published files: %v", err)
	}
	sets := map[string]bool{"layouts.yaml": false, "firewalls.yaml": false, "networks.yaml": false, "users.yaml": false, "userdata.yaml": false}
	profiles := 0
	for _, path := range files {
		file := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sets[file]; ok {
			sets[file] = true
		} else {
			profiles++
		}
		if err := v090Decode(file, data); err != nil {
			t.Errorf("profiles/%s: ump v0.9.0 does not read it: %v\n"+
				"The file is published, and released ump reads it strictly: a field or a form it does not know fails the whole file there, "+
				"for layouts.yaml every reinstall with any layout. Write the new thing with the fields v0.9.0 has (a zfs part's pool is name, its level raid), "+
				"or in a file of its own, which a released ump never fetches. A new field can come to a published file only when no ump from before it is in use; "+
				"the v090 types of this test change then, and not before.", file, err)
		}
	}
	for file, seen := range sets {
		if !seen {
			t.Errorf("profiles/%s is not there", file)
		}
	}
	if profiles == 0 {
		t.Error("no published profile")
	}

	// the check itself: a field from after v0.9.0 is refused wherever a
	// published file can carry one, as v0.9.0 refuses it
	for _, c := range []struct{ file, data, want string }{
		{"layouts.yaml", "layouts:\n  z:\n    parts:\n      - {type: zfs, target: [first], size: rest, pool: zroot, mountpoint: /}\n", "field pool not found in type profile.v090Part"},
		{"layouts.yaml", "layouts:\n  z:\n    parts:\n      - {type: zfs, target: [first], size: rest, raid: mirror, mountpoint: /}\n", "cannot unmarshal !!str `mirror` into int"},
		{"layouts.yaml", "layouts:\n  z:\n    encrypted: true\n    parts: []\n", "field encrypted not found in type profile.v090Layout"},
		{"freebsd-15.1.yaml", "name: z\nos: freebsd-15.1\nusers: [ump]\ndisks:\n  parts:\n    - {type: zfs, target: [first], size: rest, vdev: mirror, mountpoint: /}\n", "field vdev not found in type profile.v090Part"},
		{"freebsd-15.1.yaml", "name: z\nos: freebsd-15.1\nusers: [ump]\ndisks: zfs\nboot_environment: default\n", "field boot_environment not found in type profile.v090Profile"},
		{"ubuntu-26.04.yaml", "name: z\nos: ubuntu-26.04\nusers: [ump]\ndisks: lvm-linear\nkernel: {livepatch: true}\n", "field livepatch not found in type profile.v090Kernel"},
		{"ubuntu-26.04.yaml", "name: z\nos: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: ump, sudo: true, keys: [ump], password: x}\n", "field password not found in type profile.v090User"},
		{"firewalls.yaml", "firewalls:\n  web:\n    allow: [{proto: tcp, port: 80, to: 10.0.0.1}]\n", "field to not found in type profile.plain"},
		{"firewalls.yaml", "firewalls:\n  web:\n    allow: [http]\n", `"http" is not icmp, ssh, tcp/PORT or udp/PORT`},
		{"networks.yaml", "networks:\n  default:\n    cidr: 10.0.0.0/24\n    ipv6: fd00::/64\n", "field ipv6 not found in type profile.v090PrivateNetwork"},
		{"users.yaml", "users:\n  ump:\n    keys: [ump]\n    uid: 1000\n", "field uid not found in type profile.v090User"},
		{"userdata.yaml", "userdata:\n  docker:\n    script: \"true\"\n    os: [ubuntu-26.04]\n", "field os not found in type profile.v090Userdata"},
	} {
		if err := v090Decode(c.file, []byte(c.data)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s with what v0.9.0 does not know: %v, want %q", c.file, err, c.want)
		}
	}
}
