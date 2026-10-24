package profile

import (
	"net/netip"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublishedNetworks(t *testing.T) {
	networks, err := ParseNetworks(repoFile(t, "networks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := networks["default"]
	if len(networks) != 1 || n == nil || n.Name != "default" || n.CIDR != "10.0.0.0/24" || len(n.Reserved) != 0 {
		t.Fatalf("networks: %+v", networks)
	}
	if err := n.Validate(); err != nil || n.Size() != 254 {
		t.Errorf("default: %v, size %d", err, n.Size())
	}
	// every published profile names it
	for _, name := range []string{"ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04", "debian-13", "debian-12", "nixos-25.11", "archlinux", "cachyos"} {
		p, err := Parse(repoFile(t, name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := p.ResolvePrivate(networks); err != nil || len(got) != 1 || got[0] != n {
			t.Errorf("%s: %v, %+v", name, err, got)
		}
	}
	if _, err := ParseNetworks([]byte("networks:\n  db:\n    cidr: 10.0.1.0/24\n    gateway: 10.0.1.1\n")); err == nil || !strings.Contains(err.Error(), "field gateway not found") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := ParseNetworks([]byte("networks:\n  db:\n")); err == nil || err.Error() != "network db is empty" {
		t.Errorf("empty entry: %v", err)
	}
}

func TestNetworkValidate(t *testing.T) {
	for _, c := range []struct {
		name, cidr string
		reserved   []string
		err        string
	}{
		{"db", "10.0.1.0/24", []string{"10.0.1.1", "10.0.1.254"}, ""},
		{"big", "10.0.0.0/8", nil, ""},
		{"tiny", "192.168.7.4/30", nil, ""},
		{"none", "10.0.0.0/24", nil, "network none: none is what a profile says for no private network"},
		{"v6", "fd00::/64", nil, `network v6: cidr "fd00::/64" is not an IPv4 network like 10.0.0.0/24`},
		{"bare", "10.0.0.0", nil, `network bare: cidr "10.0.0.0" is not an IPv4 network like 10.0.0.0/24`},
		{"wide", "10.0.0.0/7", nil, "network wide: cidr 10.0.0.0/7: the prefix length is /8 to /30"},
		{"narrow", "10.0.0.0/31", nil, "network narrow: cidr 10.0.0.0/31: the prefix length is /8 to /30"},
		{"host", "10.0.0.5/24", nil, "network host: cidr 10.0.0.5/24 is not a network address (10.0.0.0/24 is)"},
		{"word", "10.0.0.0/24", []string{"gateway"}, `network word: reserved "gateway" is not an IPv4 address`},
		{"out", "10.0.0.0/24", []string{"10.0.1.1"}, "network out: reserved 10.0.1.1 is not in 10.0.0.0/24"},
		{"twice", "10.0.0.0/24", []string{"10.0.0.1", "10.0.0.1"}, "network twice: reserved 10.0.0.1 is listed twice"},
		{"own", "10.0.0.0/24", []string{"10.0.0.0"}, "network own: reserved 10.0.0.0 is the address of the network itself, which no server gets anyway"},
		{"bcast", "10.0.0.0/24", []string{"10.0.0.255"}, "network bcast: reserved 10.0.0.255 is the broadcast address of the network, which no server gets anyway"},
		{"Db", "10.0.0.0/24", nil, `network "Db": a network name is lower-case letters, digits, '.', '_' and '-', starting with a letter or digit`},
		{"db net", "10.0.0.0/24", nil, `network "db net": a network name is`},
		{"-db", "10.0.0.0/24", nil, `network "-db": a network name is`},
		{"db_1.a-b", "10.0.0.0/24", nil, ""},
	} {
		err := (&PrivateNetwork{Name: c.name, CIDR: c.cidr, Reserved: c.reserved}).Validate()
		if (c.err == "" && err != nil) || (c.err != "" && (err == nil || !strings.HasPrefix(err.Error(), c.err))) {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
	if s := (&PrivateNetwork{CIDR: "10.0.1.0/24", Reserved: []string{"10.0.1.1", "10.0.1.254"}}).Size(); s != 252 {
		t.Errorf("size with reserved: %d", s)
	}
	if s := (&PrivateNetwork{CIDR: "10.0.1.0/24", Reserved: []string{"10.0.1.0", "10.0.1.1", "10.0.1.255", "10.0.2.1"}}).Size(); s != 253 {
		t.Errorf("size with the network's own, the broadcast and an outside address reserved: %d", s)
	}
	if s := (&PrivateNetwork{CIDR: "192.168.7.4/30"}).Size(); s != 2 {
		t.Errorf("size of a /30: %d", s)
	}
	// overlapping networks share addresses, whatever their names
	def := &PrivateNetwork{Name: "default", CIDR: "10.0.0.0/24"}
	for _, c := range []struct {
		cidr string
		want bool
	}{{"10.0.0.0/24", true}, {"10.0.0.0/16", true}, {"10.0.0.128/25", true}, {"10.0.1.0/24", false}, {"192.168.0.0/24", false}} {
		if got := def.Overlaps(&PrivateNetwork{Name: "lan", CIDR: c.cidr}); got != c.want {
			t.Errorf("default and %s overlap: %v", c.cidr, got)
		}
	}
}

func TestNetworkAddresses(t *testing.T) {
	n := &PrivateNetwork{Name: "db", CIDR: "10.0.1.0/24", Reserved: []string{"10.0.1.1"}}
	for _, c := range []struct{ addr, err string }{
		{"10.0.1.5", ""},
		{"10.0.1.254", ""},
		{"10.0.1.300", `"10.0.1.300" is not an IPv4 address`},
		{"fd00::5", `"fd00::5" is not an IPv4 address`},
		{"10.0.2.5", "10.0.2.5 is not in the network db (10.0.1.0/24)"},
		{"10.0.1.0", "10.0.1.0 is the address of the network db itself"},
		{"10.0.1.255", "10.0.1.255 is the broadcast address of the network db"},
		{"10.0.1.1", "10.0.1.1 is reserved in the network db"},
	} {
		_, err := n.CheckAddress(c.addr)
		if (c.err == "" && err != nil) || (c.err != "" && (err == nil || err.Error() != c.err)) {
			t.Errorf("%s: %v, want %q", c.addr, err, c.err)
		}
	}
	// the lowest address no server has, the network's own, the broadcast
	// and the reserved ones left out
	taken := map[netip.Addr]bool{netip.MustParseAddr("10.0.1.2"): true, netip.MustParseAddr("10.0.1.4"): true}
	if a, err := n.Free(func(a netip.Addr) bool { return taken[a] }); err != nil || a.String() != "10.0.1.3" {
		t.Errorf("free: %v %v", a, err)
	}
	if a, err := n.Free(func(netip.Addr) bool { return false }); err != nil || a.String() != "10.0.1.2" {
		t.Errorf("free, reserved first: %v %v", a, err)
	}
	tiny := &PrivateNetwork{Name: "tiny", CIDR: "192.168.7.4/30"}
	if a, err := tiny.Free(func(a netip.Addr) bool { return a.String() == "192.168.7.5" }); err != nil || a.String() != "192.168.7.6" {
		t.Errorf("free in a /30: %v %v", a, err)
	}
	if _, err := tiny.Free(func(netip.Addr) bool { return true }); err == nil || err.Error() != "the network tiny (192.168.7.4/30) has no free address left" {
		t.Errorf("full: %v", err)
	}
}

func TestProfilePrivate(t *testing.T) {
	networks := map[string]*PrivateNetwork{"default": {Name: "default", CIDR: "10.0.0.0/24"}, "db": {Name: "db", CIDR: "10.0.1.0/24"}}
	base := "name: p\nos: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\n"
	for _, c := range []struct {
		yaml, want, err string
	}{
		{base, "", ""},
		{base + "network:\n  rename: false\n", "", ""},
		{base + "network:\n  private: none\n", "", ""},
		{base + "network:\n  private: []\n", "", ""},
		{base + "network:\n  private: db\n", "db", ""},
		// several, in order
		{base + "network:\n  private: [db, default]\n", "db,default", ""},
		{base + "network:\n  private:\n    - default\n    - db\n", "default,db", ""},
		{base + "network:\n  private: [none]\n", "", ""},
		{base + "network:\n  private: nope\n", "", `profile p: private network "nope" is not in networks.yaml (known: db, default; ump config os update fetches the published ones)`},
		{base + "network:\n  private: [default, nope]\n", "", `profile p: private network "nope" is not in networks.yaml (known: db, default; ump config os update fetches the published ones)`},
		{base + "network:\n  private: Bad Net\n", "", `profile p: network.private "Bad Net" is not a network name or none`},
		{base + "network:\n  private: [db, Bad]\n", "", `profile p: network.private "Bad" is not a network name or none`},
		{base + "network:\n  private: [db, none]\n", "", "profile p: network.private: none is no network; it stands alone, for no private network"},
		{base + "network:\n  private: [db, db]\n", "", "profile p: network.private names db twice"},
	} {
		p, err := Parse([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if err = p.Validate(); err == nil {
			var ns []*PrivateNetwork
			ns, err = p.ResolvePrivate(networks)
			for _, n := range ns {
				got += "," + n.Name
			}
			got = strings.TrimPrefix(got, ",")
		}
		if got != c.want || (c.err == "" && err != nil) || (c.err != "" && (err == nil || err.Error() != c.err)) {
			t.Errorf("%q: %q, %v; want %q, %q", c.yaml, got, err, c.want, c.err)
		}
	}
	for _, bad := range []string{"{a: b}", "[[db]]", "[{a: b}]"} {
		if _, err := Parse([]byte(base + "network:\n  private: " + bad + "\n")); err == nil || !strings.Contains(err.Error(), "network.private: a ") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// --set reaches it, like any field
	p, _ := Parse([]byte(base + "network:\n  private: default\n"))
	q, err := p.Set([]string{"network.private=none"})
	if err != nil || q.PrivateNetworkNames() != nil || strings.Join(p.PrivateNetworkNames(), ",") != "default" {
		t.Errorf("--set network.private=none: %v, %q", err, q.Network.Private)
	}
	if q, err = p.Set([]string{"network.private=[default, db]"}); err != nil || strings.Join(q.PrivateNetworkNames(), ",") != "default,db" {
		t.Errorf("--set network.private=[default, db]: %v, %q", err, q.Network.Private)
	}
	// written back as given: one name alone, several as a list, none
	// absent
	for _, c := range []struct{ private, want string }{
		{"default", "network:\n    private: default\n"},
		{"[default, db]", "network:\n    private:\n        - default\n        - db\n"},
		{"none", "network:\n    private: none\n"},
	} {
		p, _ := Parse([]byte(base + "network:\n  private: " + c.private + "\n"))
		data, err := yaml.Marshal(p)
		if err != nil || !strings.Contains(string(data), c.want) {
			t.Errorf("%s written:\n%s", c.private, data)
		}
	}
	p, _ = Parse([]byte(base))
	if data, _ := yaml.Marshal(p); strings.Contains(string(data), "private") {
		t.Errorf("absent written:\n%s", data)
	}
	if s := (PrivateNetworks{"default", "db"}).String(); s != "default,db" {
		t.Errorf("String: %q", s)
	}
	if s := (PrivateNetworks{"none"}).String() + " " + PrivateNetworks(nil).String(); s != "none none" {
		t.Errorf("String of none: %q", s)
	}
}
