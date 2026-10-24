package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

func TestAssignPrivate(t *testing.T) {
	def := &profile.PrivateNetwork{Name: "default", CIDR: "10.0.0.0/24", Reserved: []string{"10.0.0.3"}}
	db := &profile.PrivateNetwork{Name: "db", CIDR: "10.0.1.0/24"}
	tiny := &profile.PrivateNetwork{Name: "tiny", CIDR: "192.168.7.4/30"}
	// default renamed, and a network over default and db
	lan := &profile.PrivateNetwork{Name: "lan", CIDR: "10.0.0.0/24"}
	big := &profile.PrivateNetwork{Name: "big", CIDR: "10.0.0.0/16"}
	at := func(network *profile.PrivateNetwork, addr string) *state.Private {
		return &state.Private{Network: network.Name, CIDR: network.CIDR, Address: addr}
	}
	doc := state.New()
	doc.Servers["fake/1"] = state.Server{Private: at(def, "10.0.0.1")}
	doc.Servers["fake/2"] = state.Server{Private: at(def, "10.0.0.7")}
	doc.Servers["fake/3"] = state.Server{Private: at(db, "10.0.1.1")}
	doc.Servers["fake/4"] = state.Server{Private: at(def, "10.0.0.255")}
	doc.Servers["fake/5"] = state.Server{Private: at(tiny, "192.168.7.5")}
	got := func(reqs ...privateRequest) string {
		ps, i, err := assignPrivate(doc, reqs)
		if err != nil {
			if exitCodeFor(err) != exitUsage {
				t.Errorf("%v: exit status %d", err, exitCodeFor(err))
			}
			return reqs[i].ref + ": " + err.Error()
		}
		var out []string
		for _, p := range ps {
			out = append(out, describePrivate(p))
		}
		return strings.Join(out, "; ")
	}
	for _, c := range []struct {
		name string
		reqs []privateRequest
		want string
	}{
		// the lowest free address: neither another server's, nor reserved,
		// nor the network's own or its broadcast address
		{"new", []privateRequest{{ref: "fake/9", network: def}}, "10.0.0.2 in default"},
		{"new, in order", []privateRequest{{ref: "fake/8", network: def}, {ref: "fake/9", network: def}}, "10.0.0.2 in default; 10.0.0.4 in default"},
		{"another network", []privateRequest{{ref: "fake/9", network: db}}, "10.0.1.2 in db"},
		{"none", []privateRequest{{ref: "fake/9"}}, "none"},
		// a server keeps its address in the network it stays in, and
		// leaves it otherwise, for the others
		{"kept", []privateRequest{{ref: "fake/2", network: def}}, "10.0.0.7 in default"},
		{"moved", []privateRequest{{ref: "fake/3", network: def}}, "10.0.0.2 in default"},
		{"left", []privateRequest{{ref: "fake/1"}, {ref: "fake/9", network: def}}, "none; 10.0.0.1 in default"},
		{"no longer one the network gives", []privateRequest{{ref: "fake/4", network: def}}, "10.0.0.2 in default"},
		// by hand: in the network, and no other server's, also not one of
		// the same batch that keeps it or was given it; the others go
		// around them
		{"by hand", []privateRequest{{ref: "fake/9", network: def, address: "10.0.0.200"}}, "10.0.0.200 in default"},
		{"by hand, its own", []privateRequest{{ref: "fake/1", network: def, address: "10.0.0.1"}}, "10.0.0.1 in default"},
		{"by hand before the lowest free", []privateRequest{{ref: "fake/8", network: def}, {ref: "fake/9", network: def, address: "10.0.0.2"}}, "10.0.0.4 in default; 10.0.0.2 in default"},
		{"by hand, another network's address", []privateRequest{{ref: "fake/9", network: def, address: "10.0.1.1"}}, "fake/9: --private-ip 10.0.1.1: 10.0.1.1 is not in the network default (10.0.0.0/24)"},
		{"by hand, another server's", []privateRequest{{ref: "fake/9", network: def, address: "10.0.0.7"}}, "fake/9: --private-ip 10.0.0.7: fake/2 has that address in the network default; give another (ump server list -o wide shows the addresses)"},
		{"by hand, one the batch keeps", []privateRequest{{ref: "fake/2", network: def}, {ref: "fake/9", network: def, address: "10.0.0.7"}}, "fake/9: --private-ip 10.0.0.7: fake/2 has that address in the network default"},
		{"by hand, twice in the batch", []privateRequest{{ref: "fake/8", network: def, address: "10.0.0.9"}, {ref: "fake/9", network: def, address: "10.0.0.9"}}, "fake/9: --private-ip 10.0.0.9: fake/8 has that address in the network default"},
		{"by hand, one the batch leaves", []privateRequest{{ref: "fake/2"}, {ref: "fake/9", network: def, address: "10.0.0.7"}}, "none; 10.0.0.7 in default"},
		{"by hand, reserved", []privateRequest{{ref: "fake/9", network: def, address: "10.0.0.3"}}, "fake/9: --private-ip 10.0.0.3: 10.0.0.3 is reserved in the network default"},
		{"by hand, the network's", []privateRequest{{ref: "fake/9", network: def, address: "10.0.0.0"}}, "fake/9: --private-ip 10.0.0.0: 10.0.0.0 is the address of the network default itself"},
		{"by hand, the broadcast", []privateRequest{{ref: "fake/9", network: def, address: "10.0.0.255"}}, "fake/9: --private-ip 10.0.0.255: 10.0.0.255 is the broadcast address of the network default"},
		// a full network
		{"full", []privateRequest{{ref: "fake/8", network: tiny}, {ref: "fake/9", network: tiny}}, "fake/9: the network tiny (192.168.7.4/30) has no free address left"},
		// an address is taken in every network that has it, whatever its
		// name: a renamed network, one that overlaps
		{"renamed", []privateRequest{{ref: "fake/9", network: lan}}, "10.0.0.2 in lan"},
		{"renamed, by hand", []privateRequest{{ref: "fake/9", network: lan, address: "10.0.0.7"}}, "fake/9: --private-ip 10.0.0.7: fake/2 has that address in the network default"},
		{"renamed, moved", []privateRequest{{ref: "fake/2", network: lan}}, "10.0.0.2 in lan"},
		{"overlapping", []privateRequest{{ref: "fake/9", network: big, address: "10.0.1.1"}}, "fake/9: --private-ip 10.0.1.1: fake/3 has that address in the network db"},
		{"overlapping, in the batch", []privateRequest{{ref: "fake/2", network: def}, {ref: "fake/9", network: big, address: "10.0.0.7"}}, "fake/9: --private-ip 10.0.0.7: fake/2 has that address in the network default"},
		{"overlapping, the lowest free", []privateRequest{{ref: "fake/8", network: def}, {ref: "fake/9", network: big}}, "10.0.0.2 in default; 10.0.0.3 in big"},
	} {
		if g := got(c.reqs...); !strings.HasPrefix(g, c.want) {
			t.Errorf("%s: %s\nwant %s", c.name, g, c.want)
		}
	}
}

// privateOf is the private address of a server in the state file, as
// written there, or "".
func privateOf(t *testing.T, e *testEnv, ref string) string {
	t.Helper()
	d, err := state.Parse([]byte(e.readState()))
	if err != nil {
		t.Fatal(err)
	}
	p := d.Servers[ref].Private
	if p == nil {
		return ""
	}
	return p.Address + " " + p.Network + " " + p.CIDR
}

func TestReinstallPrivate(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	e.directRoot = true
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")
	reinstall := func(args ...string) (string, string, int) {
		resetFakePower()
		return e.run("", false, append(append([]string{"server", "reinstall"}, args...), "--yes", "--no-provision", "--account", "one")...)
	}

	// the profile's network, default; the lowest free address goes into
	// the specification, the state and the host file
	_, errOut, code := reinstall("web-01")
	if code != 0 || !strings.Contains(errOut, ", layout lvm-linear, private address 10.0.0.1 in default) on web-01 as web-01 ...\n") {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	if p := e.lastSpec()["network"].(map[string]any); p["rename"] != true || p["private"] == nil || p["private"].(map[string]any)["network"] != "default" || p["private"].(map[string]any)["cidr"] != "10.0.0.0/24" || p["private"].(map[string]any)["address"] != "10.0.0.1" {
		t.Errorf("spec network: %v", p)
	}
	if got := privateOf(t, e, "fake/1"); got != "10.0.0.1 default 10.0.0.0/24" {
		t.Errorf("state: %q", got)
	}
	if h := e.hostFile("web-01.yml"); !strings.Contains(h, "  ump_private_address: 10.0.0.1\n  ump_private_network: default\n  ump_private_cidr: 10.0.0.0/24\n") {
		t.Errorf("host file:\n%s", h)
	}
	// the next server the next address; a reinstall keeps the address,
	// and -o json says it
	if _, errOut, code := reinstall("db-01"); code != 0 || privateOf(t, e, "fake/2") != "10.0.0.2 default 10.0.0.0/24" {
		t.Errorf("db-01: code %d, %q\n%s", code, errOut, e.readState())
	}
	out, errOut, code := reinstall("web-01", "-o", "json")
	if code != 0 || !strings.Contains(out, `"private_network": "default",`) || !strings.Contains(out, `"private_address": "10.0.0.1"`) || privateOf(t, e, "fake/1") != "10.0.0.1 default 10.0.0.0/24" {
		t.Errorf("again: code %d, %q\n%s", code, errOut, out)
	}

	// given by hand: checked before anything is touched
	os.WriteFile(filepath.Join(dir, "networks.yaml"), []byte("networks:\n  default:\n    cidr: 10.0.0.0/24\n    reserved: [10.0.0.3]\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "p.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\nnetwork:\n  private: none\n"), 0o600)
	before, installs := e.readState(), len(e.installs)
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"db-01", "--private-ip", "10.0.0.1"}, 2, "ump: --private-ip 10.0.0.1: fake/1 has that address in the network default; give another (ump server list -o wide shows the addresses)\n"},
		{[]string{"db-01", "--private-ip", "10.0.1.5"}, 2, "ump: --private-ip 10.0.1.5: 10.0.1.5 is not in the network default (10.0.0.0/24)\n"},
		{[]string{"db-01", "--private-ip", "10.0.0.0"}, 2, "ump: --private-ip 10.0.0.0: 10.0.0.0 is the address of the network default itself\n"},
		{[]string{"db-01", "--private-ip", "10.0.0.255"}, 2, "ump: --private-ip 10.0.0.255: 10.0.0.255 is the broadcast address of the network default\n"},
		{[]string{"db-01", "--private-ip", "10.0.0.3"}, 2, "ump: --private-ip 10.0.0.3: 10.0.0.3 is reserved in the network default\n"},
		{[]string{"db-01", "--private-ip", "10.0.0.300"}, 2, `ump: --private-ip "10.0.0.300" is not an IPv4 address`},
		{[]string{"db-01", "--private-network", "nope"}, 2, "ump: --private-network nope: not in networks.yaml (known: default; ump config network create nope makes it)\n"},
		{[]string{"db-01", "--private-network", "Bad"}, 2, `ump: --private-network "Bad" is not a network name or none`},
		{[]string{"db-01", "--private-network", "none", "--private-ip", "10.0.0.9"}, 2, "ump: --private-ip and --private-network none exclude each other\n"},
		{[]string{"db-01", "--profile", "p", "--private-ip", "10.0.0.9"}, 2, "ump: --private-ip 10.0.0.9: profile p gives the server no private network; --private-network NAME gives it one\n"},
		{[]string{"db-01", "--set", "network.private=nope"}, 1, `ump: profile ubuntu-26.04: private network "nope" is not in networks.yaml (known: default; ump config os update fetches the published ones)` + "\n"},
	} {
		if _, errOut, code := reinstall(c.args...); code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), %q", c.args, code, c.code, errOut)
		}
	}
	if e.readState() != before || len(e.installs) != installs || e.kexeced {
		t.Errorf("touched: installs %d, state\n%s", len(e.installs)-installs, e.readState())
	}

	// by hand; the old address is free again, and the next server gets
	// the lowest free one, the reserved one left out
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.9"); code != 0 || privateOf(t, e, "fake/2") != "10.0.0.9 default 10.0.0.0/24" {
		t.Errorf("by hand: code %d, %q", code, errOut)
	}
	// none, by flag or by profile: no address in the specification, the
	// state or the host file, and the address is free for another
	if _, errOut, code := reinstall("web-01", "--private-network", "none"); code != 0 || privateOf(t, e, "fake/1") != "" || !strings.Contains(errOut, ", layout lvm-linear) on web-01 as web-01 ...\n") {
		t.Errorf("none: code %d, %q", code, errOut)
	}
	if n := e.lastSpec()["network"].(map[string]any); n["private"] != nil {
		t.Errorf("spec network with none: %v", n)
	}
	if h := e.hostFile("web-01.yml"); h == "" || strings.Contains(h, "ump_private") {
		t.Errorf("host file with none:\n%s", h)
	}
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.1"); code != 0 || privateOf(t, e, "fake/2") != "10.0.0.1 default 10.0.0.0/24" {
		t.Errorf("a freed address by hand: code %d, %q", code, errOut)
	}
	if _, errOut, code := reinstall("web-01", "--profile", "p"); code != 0 || privateOf(t, e, "fake/1") != "" {
		t.Errorf("profile without a network: code %d, %q", code, errOut)
	}
	if _, errOut, code := reinstall("web-01"); code != 0 || privateOf(t, e, "fake/1") != "10.0.0.2 default 10.0.0.0/24" {
		t.Errorf("the lowest free: code %d, %q\n%s", code, errOut, e.readState())
	}

	// --dry-run shows the address and records nothing; an installation
	// that fails gives its new address back
	before = e.readState()
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.20", "--dry-run"); code != 0 || !strings.Contains(errOut, "Installation plan for ubuntu-26.04 (profile ubuntu-26.04 from "+filepath.Join(dir, "ubuntu-26.04.yaml")+", layout lvm-linear, private address 10.0.0.20 in default) on db-01:\n") || e.readState() != before {
		t.Errorf("--dry-run: code %d, %q\n%s", code, errOut, e.readState())
	}
	e.installExit = 1
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.20"); code != 1 || privateOf(t, e, "fake/2") != "10.0.0.1 default 10.0.0.0/24" {
		t.Errorf("failed: code %d, %q\n%s", code, errOut, e.readState())
	}
	// once the installer wrote the disks, the address is the new system's,
	// whatever fails after: the boot, or the connection while it ran
	e.installExit = 255
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.21"); code != 5 || !strings.Contains(errOut, "the connection to 203.0.113.11 ended during the installation") || privateOf(t, e, "fake/2") != "10.0.0.21 default 10.0.0.0/24" {
		t.Errorf("connection ended: code %d, %q\n%s", code, errOut, e.readState())
	}
	e.installExit = 0
	e.bootFails = true
	if _, errOut, code := reinstall("db-01", "--private-ip", "10.0.0.22"); code != 5 || !strings.Contains(errOut, "but not as ump with UMP's key") || privateOf(t, e, "fake/2") != "10.0.0.22 default 10.0.0.0/24" {
		t.Errorf("boot fails: code %d, %q\n%s", code, errOut, e.readState())
	}
	e.bootFails = false
}

// TestReservePrivate: the address is recorded before the installation
// starts, within the state's read-modify-write, so that another ump that
// gave it meanwhile stops the installation before anything is touched.
func TestReservePrivate(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	e.directRoot = true
	e.machines = map[string]*fakeMachine{}
	store := &state.Store{Backend: &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}}
	// another ump gives 10.0.0.1 to a server of another account while ump
	// asks the storage for the image, after its plan and before the
	// installation
	var once sync.Once
	change := func(d *state.Document) {
		d.Servers["fake/7"] = state.Server{Installed: &state.Install{OS: "ubuntu-26.04", Profile: "ubuntu-26.04", Hostname: "lab-07", At: e.now}, Private: &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.1"}}
	}
	meanwhile := func() {
		once.Do(func() {
			if err := store.Update(context.Background(), func(d *state.Document) error { change(d); return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/os/ubuntu-26.04/") && strings.HasSuffix(r.URL.Path, "/manifest.json") {
			meanwhile()
			io.WriteString(w, `{"family": "image"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer storage.Close()
	e.write(testConfig + "storage: " + storage.URL + "\n")

	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: the private address 10.0.0.1 in default of web-01 is fake/7's now (another ump gave it meanwhile); nothing was changed, run it again\n") {
		t.Errorf("reinstall: code %d, %q", code, errOut)
	}
	if len(e.installs) != 0 || len(e.remoteCalls) != 0 || privateOf(t, e, "fake/1") != "" {
		t.Errorf("touched: installs %v, remote %v\n%s", e.installs, e.remoteCalls, e.readState())
	}
	if locks, _ := store.Locks(context.Background()); len(locks) != 0 {
		t.Errorf("locks left: %+v", locks)
	}
	// the next run plans around it
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"); code != 0 || privateOf(t, e, "fake/1") != "10.0.0.2 default 10.0.0.0/24" {
		t.Errorf("again: code %d, %q", code, errOut)
	}

	// multiinstall alike: all or nothing
	once = sync.Once{}
	store.Update(context.Background(), func(d *state.Document) error { delete(d.Servers, "fake/7"); return nil })
	e.remoteCalls = nil
	_, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--no-provision", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: the private address 10.0.0.1 in default of db-01 is fake/7's now (another ump gave it meanwhile); nothing was changed, run it again\n") || len(e.remoteCalls) != 0 || privateOf(t, e, "fake/2") != "" || privateOf(t, e, "fake/1") != "10.0.0.2 default 10.0.0.0/24" {
		t.Errorf("multiinstall: code %d, %q, remote %d\n%s", code, errOut, len(e.remoteCalls), e.readState())
	}

	// a server whose address changed meanwhile (ump server set by another
	// ump) stops it too, also when it keeps its address
	once = sync.Once{}
	change = func(d *state.Document) {
		s := d.Servers["fake/1"]
		s.Private = &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.30"}
		d.Servers["fake/1"] = s
	}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: the private address of web-01 changed meanwhile, from 10.0.0.2 in default to 10.0.0.30 in default; nothing was changed, run it again\n") || len(e.remoteCalls) != 0 || privateOf(t, e, "fake/1") != "10.0.0.30 default 10.0.0.0/24" {
		t.Errorf("changed meanwhile: code %d, %q, remote %d\n%s", code, errOut, len(e.remoteCalls), e.readState())
	}
}

func TestReleasePrivate(t *testing.T) {
	dir := t.TempDir()
	var errb bytes.Buffer
	a := &app{stderr: &errb, store: &state.Store{Backend: &backend.Local{Dir: dir}}}
	ctx := context.Background()
	old := &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.5"}
	a.store.Update(ctx, func(d *state.Document) error {
		d.Servers["fake/1"] = state.Server{Private: old}
		return nil
	})
	m1 := provider.Machine{Provider: "fake", ID: "1", Name: "web-01"}
	m2 := provider.Machine{Provider: "fake", ID: "2", Name: "db-01"}
	p1 := &installPlan{ref: "web-01", m: m1, had: old, private: &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.9"}}
	p2 := &installPlan{ref: "db-01", m: m2, private: &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.5"}}
	// web-01 leaves 10.0.0.5 for 10.0.0.9, and db-01 takes it, both in
	// one change
	if err := a.reservePrivate(ctx, []*installPlan{p1, p2}); err != nil {
		t.Fatal(err)
	}
	d, _, _ := a.store.Read(ctx)
	if !d.Servers["fake/1"].Private.Equal(p1.private) || !d.Servers["fake/2"].Private.Equal(p2.private) || !p1.reserved || !p1.before.Equal(old) || p2.before != nil {
		t.Fatalf("reserved: %+v", d.Servers)
	}
	// db-01's installation failed: it gives its address back; web-01's
	// was changed meanwhile, and stays
	a.store.Update(ctx, func(d *state.Document) error {
		s := d.Servers["fake/1"]
		s.Private = &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.10"}
		d.Servers["fake/1"] = s
		return nil
	})
	a.releasePrivate(ctx, []*installPlan{p1, p2})
	d, _, _ = a.store.Read(ctx)
	if d.Servers["fake/1"].Private.Address != "10.0.0.10" || d.Servers["fake/2"].Private != nil || p2.reserved || errb.Len() != 0 {
		t.Errorf("released: %+v, %q", d.Servers, errb.String())
	}
	// lab-01 left 10.0.0.20, which another server got meanwhile: it
	// stays without
	m3 := provider.Machine{Provider: "fake", ID: "3", Name: "lab-01"}
	lab := &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.20"}
	a.store.Update(ctx, func(d *state.Document) error {
		d.Servers["fake/3"] = state.Server{Private: lab}
		return nil
	})
	p3 := &installPlan{ref: "lab-01", m: m3, had: lab}
	if err := a.reservePrivate(ctx, []*installPlan{p3}); err != nil || !p3.reserved {
		t.Fatalf("lab-01: %v", err)
	}
	a.store.Update(ctx, func(d *state.Document) error {
		d.Servers["fake/9"] = state.Server{Private: lab}
		return nil
	})
	a.releasePrivate(ctx, []*installPlan{p3})
	d, _, _ = a.store.Read(ctx)
	if d.Servers["fake/3"].Private != nil || errb.String() != "ump: lab-01 stays without a private address: the one it had, 10.0.0.20 in default, is another server's now\n" {
		t.Errorf("taken meanwhile: %+v, %q", d.Servers["fake/3"], errb.String())
	}
	// nothing to record; a plan whose server's address changed since stops
	cur := &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.10"}
	p := &installPlan{ref: "web-01", m: m1, had: cur, private: cur}
	if err := a.reservePrivate(ctx, []*installPlan{p}); err != nil || p.reserved {
		t.Errorf("unchanged: %v %v", err, p.reserved)
	}
	p = &installPlan{ref: "web-01", m: m1, had: old, private: old}
	if err := a.reservePrivate(ctx, []*installPlan{p}); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "the private address of web-01 changed meanwhile, from 10.0.0.5 in default to 10.0.0.10 in default; nothing was changed, run it again" || p.reserved {
		t.Errorf("stale: %v %v", err, p.reserved)
	}
	if d, _, _ = a.store.Read(ctx); !d.Servers["fake/1"].Private.Equal(cur) {
		t.Errorf("stale, state: %+v", d.Servers["fake/1"])
	}
}

func TestMultiinstallPrivate(t *testing.T) {
	e := multiEnv(t)
	run := func(args ...string) (string, string, int) {
		resetFakePower()
		return e.run("", false, append(append([]string{"server", "multiinstall"}, args...), "--yes", "--no-provision", "--account", "one")...)
	}
	// by hand first, then the lowest free addresses, around those of the
	// other servers of the run
	_, errOut, code := run("--plan", writePlan(t, "servers:\n  web-01: {}\n  db-01:\n    private-ip: 10.0.0.1\n"))
	if code != 0 || !strings.Contains(errOut, "SERVER   PROVIDER   OS             LAYOUT       HOSTNAME   PRIVATE    GROUPS\nweb-01   fake/1     ubuntu-26.04   lvm-linear   web-01     10.0.0.2   common\ndb-01    fake/2     ubuntu-26.04   lvm-linear   db-01      10.0.0.1   common\n") {
		t.Fatalf("multiinstall: code %d, %q", code, errOut)
	}
	if web, db := e.specOf("203.0.113.10")["network"].(map[string]any)["private"], e.specOf("203.0.113.11")["network"].(map[string]any)["private"]; web.(map[string]any)["address"] != "10.0.0.2" || db.(map[string]any)["address"] != "10.0.0.1" {
		t.Errorf("specs: %v %v", web, db)
	}
	if privateOf(t, e, "fake/1") != "10.0.0.2 default 10.0.0.0/24" || privateOf(t, e, "fake/2") != "10.0.0.1 default 10.0.0.0/24" {
		t.Errorf("state:\n%s", e.readState())
	}
	if !strings.Contains(e.hostFile("web-01.yml"), "  ump_private_address: 10.0.0.2\n") || !strings.Contains(e.hostFile("db-01.yml"), "  ump_private_address: 10.0.0.1\n") {
		t.Errorf("host files:\n%s\n%s", e.hostFile("web-01.yml"), e.hostFile("db-01.yml"))
	}

	// refused before anything is touched: an address another server of
	// the run keeps, one address for several servers
	plan := writePlan(t, "servers:\n  web-01: {}\n  db-01:\n    private-ip: 10.0.0.2\n")
	e.remoteCalls = nil
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--plan", plan}, "ump: " + plan + ": servers.db-01: --private-ip 10.0.0.2: fake/1 has that address in the network default; give another (ump server list -o wide shows the addresses)\n"},
		{[]string{"web-01", "db-01", "--private-ip", "10.0.0.5"}, "ump: --private-ip gives one server its address, not 2 the same one; give each its own in a plan (private-ip:)\n"},
		{[]string{"--plan", writePlan(t, "defaults:\n  private-ip: 10.0.0.5\nservers:\n  web-01:\n")}, ": line 2: defaults: private-ip would give every server the same address; give it per server\n"},
		{[]string{"--plan", writePlan(t, "servers:\n  web-01:\n    private-network: [a]\n")}, "cannot unmarshal !!seq into string"},
		{[]string{"web-01", "db-01", "--private-network", "nope"}, "ump: web-01: --private-network nope: not in networks.yaml (known: default; ump config network create nope makes it)\n"},
	} {
		if _, errOut, code := run(c.args...); code != 2 || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	if len(e.remoteCalls) != 0 {
		t.Errorf("touched: %v", e.remoteCalls)
	}
	// one server by itself, with its address on the command line
	if _, errOut, code := run("db-01", "--private-ip", "10.0.0.5"); code != 0 || privateOf(t, e, "fake/2") != "10.0.0.5 default 10.0.0.0/24" {
		t.Errorf("one server by hand: code %d, %q", code, errOut)
	}

	// none for one, a new address for the other, whose installation
	// fails: it gives the address back, the other's is gone
	e.installExitAt = map[string]int{"203.0.113.11": 1}
	_, errOut, code = run("--plan", writePlan(t, "servers:\n  web-01:\n    private-network: none\n  db-01:\n    private-ip: 10.0.0.7\n"))
	if code != 1 || !strings.Contains(errOut, "web-01   fake/1     ubuntu-26.04   lvm-linear   web-01     none       common\n") || privateOf(t, e, "fake/1") != "" || privateOf(t, e, "fake/2") != "10.0.0.5 default 10.0.0.0/24" {
		t.Errorf("one fails: code %d, %q\n%s", code, errOut, e.readState())
	}
	e.installExitAt = nil

	// the new systems do not boot: they keep the addresses the installer
	// wrote
	e.bootFails = true
	_, errOut, code = run("--plan", writePlan(t, "servers:\n  web-01: {}\n  db-01:\n    private-ip: 10.0.0.6\n"))
	e.bootFails = false
	if code != 1 || privateOf(t, e, "fake/1") != "10.0.0.1 default 10.0.0.0/24" || privateOf(t, e, "fake/2") != "10.0.0.6 default 10.0.0.0/24" {
		t.Errorf("boot fails: code %d, %q\n%s", code, errOut, e.readState())
	}

	// every server's lock is the run's from the question on: ump server
	// set beside it on a server that has not started yet is refused
	store := &state.Store{Backend: &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}}
	var once sync.Once
	e.remoteGate = func(addr string, argv []string) {
		if addr != "203.0.113.10" || !strings.Contains(argv[len(argv)-1], "/install/install.sh") {
			return
		}
		once.Do(func() {
			if _, errOut, code := e.runBeside("server", "set", "db-01", "--private-ip", "10.0.0.30", "--account", "one"); code != 3 || !strings.Contains(errOut, "ump: fake/2 is locked by ") || !strings.Contains(errOut, "(multiinstall)") {
				t.Errorf("server set beside: code %d, %q", code, errOut)
			}
		})
	}
	e.remoteCalls, e.remoteStdin = nil, nil
	_, errOut, code = run("--plan", writePlan(t, "servers:\n  web-01: {}\n  db-01:\n    private-ip: 10.0.0.8\n"), "--parallel", "1")
	e.remoteGate = nil
	if code != 0 || privateOf(t, e, "fake/1") != "10.0.0.1 default 10.0.0.0/24" || privateOf(t, e, "fake/2") != "10.0.0.8 default 10.0.0.0/24" || e.specOf("203.0.113.11")["network"].(map[string]any)["private"].(map[string]any)["address"] != "10.0.0.8" {
		t.Errorf("set beside: code %d, %q\n%s", code, errOut, e.readState())
	}

	// a lock another ump took while ump asked stops the run: the locks
	// taken are let go, nothing is reserved or touched
	release := func() error { return nil }
	e.onAsk = func() {
		var err error
		if release, err = store.Lock(context.Background(), "fake/2", "reinstall"); err != nil {
			t.Error(err)
		}
	}
	before, calls := e.readState(), len(e.remoteCalls)
	resetFakePower()
	_, errOut, code = e.run("y\n", true, "server", "multiinstall", "--plan", writePlan(t, "servers:\n  web-01:\n    private-ip: 10.0.0.40\n  db-01:\n    private-ip: 10.0.0.41\n"), "--no-provision", "--account", "one")
	e.onAsk = nil
	locks, _ := store.Locks(context.Background())
	release()
	if code != 3 || !strings.Contains(errOut, "ump: db-01: fake/2 is locked by ") || !strings.Contains(errOut, "; nothing was changed\n") || e.readState() != before || len(e.remoteCalls) != calls || len(locks) != 1 || locks[0].Key != "fake/2" {
		t.Errorf("locked while asking: code %d, %q, locks %+v\n%s", code, errOut, locks, e.readState())
	}

	// an installation an interrupt leaves out gives its address back
	e.interruptAt = "203.0.113.10"
	_, errOut, code = run("--plan", writePlan(t, "servers:\n  web-01: {}\n  db-01:\n    private-ip: 10.0.0.9\n"), "--parallel", "1")
	e.interruptAt = ""
	if code != 3 || privateOf(t, e, "fake/1") != "10.0.0.1 default 10.0.0.0/24" || privateOf(t, e, "fake/2") != "10.0.0.8 default 10.0.0.0/24" {
		t.Errorf("interrupted: code %d, %q\n%s", code, errOut, e.readState())
	}
	if locks, _ := store.Locks(context.Background()); len(locks) != 0 {
		t.Errorf("locks left: %+v", locks)
	}
}

// runBeside runs another ump beside the one e runs, in the same home,
// with e's clock and none of its other fakes: what another terminal
// does meanwhile.
func (e *testEnv) runBeside(args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, now: func() time.Time { return e.now }, tz: time.UTC, httpClient: localOnlyClient}
	code := a.run(context.Background(), append([]string{"--config", e.cfgPath}, args...))
	return out.String(), errb.String(), code
}

func TestServerSetPrivate(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	set := func(args ...string) (string, string, int) {
		return e.run("", false, append(append([]string{"server", "set"}, args...), "--account", "one")...)
	}
	if _, errOut, code := set("db-01", "--private-network", "default"); code != 2 || !strings.Contains(errOut, "ump: ump did not install db-01: its private address comes with the installation (ump server reinstall db-01 --private-network NAME)\n") {
		t.Errorf("not installed: code %d, %q", code, errOut)
	}
	d := state.New()
	d.Servers["fake/1"] = state.Server{Installed: &state.Install{OS: "ubuntu-26.04", Profile: "ubuntu-26.04", Hostname: "web-01", Admin: "ump", At: e.now}, Private: &state.Private{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.1"}}
	d.Servers["fake/2"] = state.Server{Installed: &state.Install{OS: "ubuntu-26.04", Profile: "ubuntu-26.04", Hostname: "db-01", Admin: "ump", At: e.now}}
	data, _ := d.Bytes()
	os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"), data, 0o600)

	before := e.readState()
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"db-01", "--private-ip", "10.0.0.5"}, "ump: --private-ip 10.0.0.5: db-01 has no private network; give one, --private-network NAME\n"},
		{[]string{"db-01", "--private-network", "default", "--private-ip", "10.0.0.1"}, "ump: --private-ip 10.0.0.1: fake/1 has that address in the network default; give another (ump server list -o wide shows the addresses)\n"},
		{[]string{"web-01", "--private-ip", "10.0.1.1"}, "ump: --private-ip 10.0.1.1: 10.0.1.1 is not in the network default (10.0.0.0/24)\n"},
		{[]string{"db-01", "--private-network", "nope"}, "ump: --private-network nope: not in networks.yaml (known: default; ump config network create nope makes it)\n"},
		{[]string{"db-01", "--private-ip", "ten"}, `ump: --private-ip "ten" is not an IPv4 address`},
	} {
		if _, errOut, code := set(c.args...); code != 2 || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	if e.readState() != before {
		t.Errorf("state changed:\n%s", e.readState())
	}
	if out, errOut, _ := e.run("", false, "server", "show", "web-01", "--account", "one"); !strings.Contains(out, "  Private:    10.0.0.1 in default (10.0.0.0/24)\n") {
		t.Errorf("show: %q\n%s", errOut, out)
	}

	// a network gives the lowest free address, an address is given, and
	// the network alone keeps it; none takes it away
	for _, c := range []struct {
		args       []string
		out, state string
	}{
		{[]string{"db-01", "--private-network", "default"}, "fake/2 (db-01, 203.0.113.11): private address 10.0.0.2 in default (ump provision puts it into the host file).\n", "10.0.0.2 default 10.0.0.0/24"},
		{[]string{"db-01", "--private-ip", "10.0.0.9"}, "fake/2 (db-01, 203.0.113.11): private address 10.0.0.9 in default (ump provision puts it into the host file).\n", "10.0.0.9 default 10.0.0.0/24"},
		{[]string{"db-01", "--private-network", "default"}, "private address 10.0.0.9 in default", "10.0.0.9 default 10.0.0.0/24"},
	} {
		if out, errOut, code := set(c.args...); code != 0 || !strings.Contains(out, c.out) || privateOf(t, e, "fake/2") != c.state {
			t.Errorf("%v: code %d, %q %q\n%s", c.args, code, out, errOut, e.readState())
		}
	}
	if out, _, code := set("web-01", "--private-network", "none"); code != 0 || !strings.Contains(out, "fake/1 (web-01, 203.0.113.10): no private address (ump provision takes it out of the host file).\n") || privateOf(t, e, "fake/1") != "" {
		t.Errorf("none: code %d, %q", code, out)
	}
	// under the server's lock: not while an installation holds it
	store := &state.Store{Backend: &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}}
	release, err := store.Lock(context.Background(), "fake/2", "reinstall")
	if err != nil {
		t.Fatal(err)
	}
	before = e.readState()
	if _, errOut, code := set("db-01", "--private-ip", "10.0.0.20"); code != 3 || !strings.Contains(errOut, "ump: fake/2 is locked by") || e.readState() != before {
		t.Errorf("locked: code %d, %q", code, errOut)
	}
	if _, errOut, code := set("db-01", "--attr", "owner=alice"); code != 0 {
		t.Errorf("locked, an attribute: code %d, %q", code, errOut)
	}
	release()

	// where it shows: the wide list, JSON, the inventory
	out, _, _ := e.run("", false, "server", "list", "--account", "one", "-o", "wide", "--no-headers")
	if rows := fields(out); len(rows) != 2 || rows[0][3] != "db-01" || rows[0][12] != "10.0.0.9" || rows[1][12] != "<none>" {
		t.Errorf("wide:\n%s", out)
	}
	if out, _, _ := e.run("", false, "server", "list", "db-01", "--account", "one", "-o", "json"); !strings.Contains(out, `"private_network": "default",
    "private_address": "10.0.0.9"`) {
		t.Errorf("json:\n%s", out)
	}
	out, _, _ = e.run("", false, "server", "list", "--account", "one", "-o", "ansible")
	if !strings.Contains(out, `"ump_private_address": "10.0.0.9",
        "ump_private_network": "default",
        "ump_private_cidr": "10.0.0.0/24"`) || strings.Count(out, "ump_private_address") != 1 {
		t.Errorf("-o ansible:\n%s", out)
	}
}

func TestConfigNetwork(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	out, _, code := e.run("", false, "config", "net", "list")
	if code != 0 || out != "NAME      CIDR          ADDRESSES   RESERVED   STATE   DESCRIPTION\ndefault   10.0.0.0/24   254         -          ok      the private network of every server ump installs\n" {
		t.Errorf("list: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "network", "get", "default"); code != 0 || out != "default:\n  description: the private network of every server ump installs\n  cidr: 10.0.0.0/24\n" {
		t.Errorf("get: code %d\n%s", code, out)
	}
	if out, errOut, code := e.run("", false, "config", "net", "create", "db"); code != 0 || !strings.Contains(out, "Created the network db from default") || errOut != "Warning: networks db (10.0.0.0/24) and default (10.0.0.0/24) overlap; give each addresses of its own; ump config network edit db changes it.\n" {
		t.Errorf("create: code %d, %q\n%s", code, errOut, out)
	}
	if _, errOut, code := e.run("", false, "config", "net", "create", "none"); code != 2 || !strings.Contains(errOut, "network name: none is what a profile says for no private network") {
		t.Errorf("create none: code %d, %q", code, errOut)
	}

	// edit checks the entry: a host address is no network
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("db:\n  cidr: 10.0.1.5/24\n"), 0o600)
	}
	if _, errOut, code := e.run("", false, "config", "net", "edit", "db"); code != 1 || !strings.Contains(errOut, "ump: the network db is not valid: network db: cidr 10.0.1.5/24 is not a network address (10.0.1.0/24 is)\n") {
		t.Errorf("invalid edit: code %d, %q", code, errOut)
	}
	// nor one that overlaps another, or reserves the broadcast address
	for _, c := range []struct{ entry, msg string }{
		{"db:\n  cidr: 10.0.0.0/16\n", "ump: the network db is not valid: networks db (10.0.0.0/16) and default (10.0.0.0/24) overlap; give each addresses of its own\n"},
		{"db:\n  cidr: 10.0.1.0/24\n  reserved: [10.0.1.255]\n", "ump: the network db is not valid: network db: reserved 10.0.1.255 is the broadcast address of the network, which no server gets anyway\n"},
	} {
		e.editorHook = func(path string) error { return os.WriteFile(path, []byte(c.entry), 0o600) }
		if _, errOut, code := e.run("", false, "config", "net", "edit", "db"); code != 1 || !strings.Contains(errOut, c.msg) {
			t.Errorf("invalid edit %q: code %d, %q", c.entry, code, errOut)
		}
	}
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("db:\n  description: the databases\n  cidr: 10.0.1.0/24\n  reserved: [10.0.1.1]\n"), 0o600)
	}
	if out, errOut, code := e.run("", false, "config", "net", "edit", "db"); code != 0 || !strings.Contains(out, "Saved the network db") {
		t.Errorf("edit: code %d, %q %q", code, out, errOut)
	}
	e.editorHook = nil
	if out, _, _ := e.run("", false, "config", "net", "list", "--no-headers"); !strings.Contains(strings.Join(strings.Fields(out), " "), "db 10.0.1.0/24 253 10.0.1.1 ok the databases") {
		t.Errorf("list after edit:\n%s", out)
	}
	if out, _, _ := e.run("", false, "config", "net", "list", "-o", "json"); !strings.Contains(out, `"db": {
    "cidr": "10.0.1.0/24",
    "description": "the databases",
    "reserved": [
      "10.0.1.1"
    ]`) {
		t.Errorf("-o json:\n%s", out)
	}

	// a server in it gets the lowest address it gives; ump config os list
	// shows a profile's network
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--private-network", "db", "--yes", "--no-provision", "--account", "one"); code != 0 || privateOf(t, e, "fake/1") != "10.0.1.2 db 10.0.1.0/24" {
		t.Errorf("reinstall in db: code %d, %q", code, errOut)
	}
	os.WriteFile(filepath.Join(dir, "q.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\nnetwork:\n  private: db\n"), 0o600)
	if out, _, _ := e.run("", false, "config", "os", "list", "-o", "json"); !strings.Contains(out, `"name": "q",
    "default": false,
    "os": "ubuntu-26.04",
    "disks": "lvm-linear",
    "users": [
      "ump"
    ],
    "firewall": "none",
    "private": "db",`) {
		t.Errorf("os list -o json:\n%s", out)
	}
	// a profile that names a network that is not there is not valid
	if out, _, code := e.run("", false, "config", "os", "edit", "q"); code != 0 || !strings.Contains(out, "Profile q is valid") {
		t.Errorf("os edit q: code %d, %q", code, out)
	}
	if _, _, code := e.run("", false, "config", "net", "delete", "db", "--yes"); code != 0 {
		t.Errorf("delete: code %d", code)
	}
	if _, errOut, code := e.run("", false, "config", "os", "edit", "q"); code != 1 || !strings.Contains(errOut, `the profile is kept, but it is not valid: profile q: private network "db" is not in networks.yaml (known: default; ump config os update fetches the published ones)`) {
		t.Errorf("os edit q without db: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "os", "create", "networks", "--from", "ubuntu-26.04"); code != 2 || !strings.Contains(errOut, "networks.yaml holds the named networks entries") {
		t.Errorf("a profile named networks: code %d, %q", code, errOut)
	}

	// a profiles directory from before networks.yaml: the first command
	// downloads the published one
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/profiles/networks.yaml" {
			data, _ := os.ReadFile(filepath.Join("..", "..", "profiles", "networks.yaml"))
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer storage.Close()
	e.write(testConfig + "storage: " + storage.URL + "\n")
	os.Remove(filepath.Join(dir, "networks.yaml"))
	out, errOut, code := e.run("", false, "config", "net", "list", "--no-headers")
	if code != 0 || errOut != "Downloaded the published networks.yaml into "+dir+".\n" || !strings.HasPrefix(out, "default   10.0.0.0/24") {
		t.Errorf("download: code %d, %q\n%s", code, errOut, out)
	}
}
