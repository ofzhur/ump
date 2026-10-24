package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/provider"
)

func TestStoreUpdateMerges(t *testing.T) {
	// two writers about different servers: the second's write conflicts
	// and is retried on the fresh document, so both changes land
	dir := t.TempDir()
	s := &Store{Backend: &backend.Local{Dir: dir}}
	ctx := context.Background()
	var once sync.Once
	err := s.Update(ctx, func(d *Document) error {
		d.Servers["fake/1"] = Server{Profile: "web"}
		once.Do(func() {
			if err := s.Update(ctx, func(d *Document) error {
				d.Servers["fake/2"] = Server{Price: &provider.Price{Amount: 5, Currency: "EUR", Period: "month"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.Read(ctx)
	if err != nil || d.Servers["fake/1"].Profile != "web" || d.Servers["fake/2"].Price == nil {
		t.Fatalf("merged state: %+v %v", d, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, File))
	if !strings.HasPrefix(string(data), "# UMP state") || !strings.Contains(string(data), "version: 3\n") {
		t.Errorf("file:\n%s", data)
	}
	if err := s.Update(ctx, func(d *Document) error { d.Servers["fake/1"] = Server{}; return nil }); err != nil {
		t.Fatal(err)
	}
	d, _, _ = s.Read(ctx)
	if _, ok := d.Servers["fake/1"]; ok {
		t.Error("empty record kept")
	}
	unlock, err := s.Lock(ctx, "fake/1", "reinstall")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, "fake/1", "rescue"); err == nil || !strings.Contains(err.Error(), "fake/1 is locked by") || !strings.Contains(err.Error(), "ump config state unlock fake/1 forces it") {
		t.Errorf("second lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse([]byte("version: 7\n")); err == nil || !strings.Contains(err.Error(), "version 7") {
		t.Errorf("newer version: %v", err)
	}
	// the version comes before the keys a later one may have
	if _, err := Parse([]byte("version: 4\nservers:\n  fake/1:\n    mesh: {peers: 3}\n")); err == nil || err.Error() != "the state is version 4; this ump reads up to 3" {
		t.Errorf("newer version with new keys: %v", err)
	}
	// a state of version 1 loads as it is, and is written as version 3
	d, err := Parse([]byte("version: 1\nservers:\n  fake/1:\n    profile: web\n    groups: [web]\n    stale_facts: true\n"))
	if err != nil || d.Version != 1 || d.Servers["fake/1"].Profile != "web" || d.Servers["fake/1"].Groups[0] != "web" || !d.Servers["fake/1"].StaleFacts {
		t.Fatalf("version 1: %+v %v", d, err)
	}
	if data, _ := d.Bytes(); !strings.Contains(string(data), "\nversion: 3\n") || !strings.Contains(string(data), "    profile: web\n") {
		t.Errorf("version 1 written:\n%s", data)
	}
	if _, err := Parse([]byte("servers:\n  fake/1:\n    price: {amount: 1}\n")); err == nil || !strings.Contains(err.Error(), "servers.fake/1.price") {
		t.Errorf("bad price: %v", err)
	}
	if _, err := Parse([]byte("colour: blue\n")); err == nil {
		t.Error("unknown field accepted")
	}
	if d, err := Parse(nil); err != nil || d.Version != FormatVersion || d.Servers == nil {
		t.Errorf("empty: %+v %v", d, err)
	}
}

func TestParseRolesBecomeGroups(t *testing.T) {
	// an older ump wrote roles; they load as groups and are written as such
	d, err := Parse([]byte("servers:\n  fake/1:\n    roles: [web-1, base.x, web_1, common, localhost, _meta, 2fa]\n    groups: [db]\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := d.Servers["fake/1"]
	if strings.Join(s.Groups, ",") != "db,web_1,base_x,_2fa" || s.Roles != nil {
		t.Errorf("groups %v, roles %v", s.Groups, s.Roles)
	}
	data, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "    groups:\n      - db\n      - web_1\n      - base_x\n      - _2fa\n") || strings.Contains(string(data), "roles") {
		t.Errorf("written:\n%s", data)
	}
}

func TestPrivate(t *testing.T) {
	// version 3: a list per server, in the order of its networks
	d, err := Parse([]byte("version: 3\nservers:\n  fake/1:\n    private_networks:\n      - {network: default, cidr: 10.0.0.0/24, address: 10.0.0.5}\n      - {network: lab, cidr: 10.0.2.0/24, address: 10.0.2.9}\n  fake/2:\n    private_networks: [{network: db, cidr: 10.0.1.0/24, address: 10.0.1.5}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := d.Servers["fake/1"].Private; len(p) != 2 || p[0] != (Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.5"}) || p[1] != (Private{Network: "lab", CIDR: "10.0.2.0/24", Address: "10.0.2.9"}) || d.Servers["fake/1"].Empty() {
		t.Errorf("fake/1: %+v", d.Servers["fake/1"])
	}
	// an address is one server's in every network that has it, whatever
	// its name
	at := func(network, cidr, address string) Private {
		return Private{Network: network, CIDR: cidr, Address: address}
	}
	for _, c := range []struct {
		p      Private
		except []string
		want   string
	}{
		{at("default", "10.0.0.0/24", "10.0.0.5"), nil, "fake/1"},
		{at("lab", "10.0.2.0/24", "10.0.2.9"), nil, "fake/1"},
		{at("default", "10.0.0.0/24", "10.0.0.5"), []string{"fake/1"}, ""},
		{at("lan", "10.0.0.0/24", "10.0.0.5"), nil, "fake/1"},
		{at("big", "10.0.0.0/16", "10.0.0.5"), nil, "fake/1"},
		{at("big", "10.0.0.0/16", "10.0.1.5"), nil, "fake/2"},
		{at("db", "10.0.1.0/24", "10.0.1.6"), nil, ""},
		{at("other", "10.0.3.0/24", "10.0.0.5"), nil, ""},
	} {
		if r := d.PrivateHolder(c.p, c.except...); r != c.want {
			t.Errorf("holder of %+v but %v: %q, want %q", c.p, c.except, r, c.want)
		}
	}
	data, _ := d.Bytes()
	if !strings.Contains(string(data), "\nversion: 3\n") || !strings.Contains(string(data), "  fake/1:\n    private_networks:\n      - network: default\n        cidr: 10.0.0.0/24\n        address: 10.0.0.5\n      - network: lab\n        cidr: 10.0.2.0/24\n        address: 10.0.2.9\n") {
		t.Errorf("written:\n%s", data)
	}
	// a state of version 2 (ump v0.5.0) has one address under private: it
	// loads as the first of the list, and is written as version 3
	d, err = Parse([]byte("version: 2\nservers:\n  fake/1:\n    private: {network: default, cidr: 10.0.0.0/24, address: 10.0.0.5}\n    stale_facts: true\n"))
	if err != nil || d.Version != 2 {
		t.Fatalf("version 2: %+v %v", d, err)
	}
	if s := d.Servers["fake/1"]; len(s.Private) != 1 || s.Private[0] != at("default", "10.0.0.0/24", "10.0.0.5") || s.OnePrivate != nil || !s.StaleFacts {
		t.Errorf("version 2: %+v", s)
	}
	if data, _ := d.Bytes(); !strings.Contains(string(data), "\nversion: 3\n") || !strings.Contains(string(data), "    private_networks:\n      - network: default\n        cidr: 10.0.0.0/24\n        address: 10.0.0.5\n    stale_facts: true\n") || strings.Contains(string(data), "private:") {
		t.Errorf("version 2 written:\n%s", data)
	}
	if s := (Server{Private: []Private{at("default", "10.0.0.0/24", "10.0.0.5")}}); s.Empty() {
		t.Error("a server with a private address is empty")
	}
	for _, bad := range []string{
		"{network: default, cidr: 10.0.0.0/24}",
		"{cidr: 10.0.0.0/24, address: 10.0.0.5}",
		"{network: default, cidr: 10.0.0.0/24, address: 10.0.1.5}",
		"{network: default, cidr: 10.0.0.0/24, address: fd00::5}",
		"{network: default, address: 10.0.0.5}",
	} {
		if _, err := Parse([]byte("servers:\n  fake/1:\n    private: " + bad + "\n")); err == nil || err.Error() != "servers.fake/1.private: needs network, cidr and an IPv4 address in it" {
			t.Errorf("%s: %v", bad, err)
		}
		if _, err := Parse([]byte("servers:\n  fake/1:\n    private_networks:\n      - {network: db, cidr: 10.0.1.0/24, address: 10.0.1.5}\n      - " + bad + "\n")); err == nil || err.Error() != "servers.fake/1.private_networks[1]: needs network, cidr and an IPv4 address in it" {
			t.Errorf("%s in the list: %v", bad, err)
		}
	}
	for _, c := range []struct{ yaml, err string }{
		{"    private_networks:\n      - {network: db, cidr: 10.0.1.0/24, address: 10.0.1.5}\n      - {network: db, cidr: 10.0.1.0/24, address: 10.0.1.6}\n", "servers.fake/1.private_networks: the network db is given twice"},
		{"    private: {network: db, cidr: 10.0.1.0/24, address: 10.0.1.5}\n    private_networks: [{network: lab, cidr: 10.0.2.0/24, address: 10.0.2.5}]\n", "servers.fake/1: private is the one address of version 2, private_networks the list of version 3; give one of them"},
		{"    private_networks: [{network: db, cidr: 10.0.1.0/24, address: 10.0.1.5, gateway: 10.0.1.1}]\n", "field gateway not found"},
	} {
		if _, err := Parse([]byte("servers:\n  fake/1:\n" + c.yaml)); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%q: %v", c.yaml, err)
		}
	}
}
