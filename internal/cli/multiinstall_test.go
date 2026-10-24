package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/state"
)

func TestParsePlan(t *testing.T) {
	// the example of the help, in the file's order; a server's values
	// replace those of defaults, key by key, lists as a whole
	p, err := parsePlan([]byte(`defaults:                  # for every server
  profile: web
  group: [web]
  ipv6: true
servers:                   # each over the defaults
  web-01: {}
  web-02:
    layout: lvm-raid1
  db-01:
    profile: db
    group: [db, backup]
    hostname: db-01.example.net
    private-network: [default, db]
    private-ip: [default=10.0.0.12, db=10.0.1.12]
    user: [oleg:local, deploy]
    ipv6: false
  203.0.113.12:
    private-network: none
    private-ip: 10.0.0.13
    group: []
    ssh-key: local
    disks: [/dev/sda, /dev/sdb]
    set: [kernel.extra=quiet]
    via: direct
`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.servers {
		names = append(names, s.name)
	}
	if strings.Join(names, " ") != "web-01 web-02 db-01 203.0.113.12" {
		t.Errorf("servers: %v", names)
	}
	o := p.defaults.over(p.servers[0].values).options()
	if o.profile != "web" || strings.Join(o.groups, ",") != "web" || !o.ipv6 || o.layout != "" || o.via != "auto" {
		t.Errorf("web-01: %+v", o)
	}
	if o := p.defaults.over(p.servers[1].values).options(); o.profile != "web" || o.layout != "lvm-raid1" {
		t.Errorf("web-02: %+v", o)
	}
	o = p.defaults.over(p.servers[2].values).options()
	if o.profile != "db" || strings.Join(o.groups, ",") != "db,backup" || o.hostname != "db-01.example.net" || strings.Join(o.privateNetworks, " ") != "default db" || strings.Join(o.privateIPs, " ") != "default=10.0.0.12 db=10.0.1.12" || strings.Join(o.users, " ") != "oleg:local deploy" || o.ipv6 {
		t.Errorf("db-01: %+v", o)
	}
	o = p.defaults.over(p.servers[3].values).options()
	if len(o.groups) != 0 || strings.Join(o.keyFiles, ",") != "local" || o.disks != "/dev/sda,/dev/sdb" || strings.Join(o.sets, ",") != "kernel.extra=quiet" || o.via != "direct" || o.profile != "web" || strings.Join(o.privateNetworks, " ") != "none" || strings.Join(o.privateIPs, " ") != "10.0.0.13" {
		t.Errorf("203.0.113.12: %+v", o)
	}
	if o := p.defaults.over(p.servers[0].values).options(); o.privateNetworks != nil || o.privateIPs != nil {
		t.Errorf("web-01, private: %+v", o)
	}
	if p, err := parsePlan([]byte("servers:\n  web-01:\n")); err != nil || len(p.servers) != 1 || p.defaults.over(p.servers[0].values).options().profile != "" {
		t.Errorf("a server without values: %v %+v", err, p)
	}
	if p, err := parsePlan([]byte("---\ndefaults:\nservers:\n  web-01:\n...\n")); err != nil || len(p.servers) != 1 {
		t.Errorf("one document, marked: %v %+v", err, p)
	}

	// strictly: one document, no other key, nothing twice, no key
	// without a value, no value of the wrong kind
	for _, c := range []struct{ plan, err string }{
		{"", "empty; a plan has servers, and defaults for them"},
		{"# nothing yet\n", "empty; a plan has servers, and defaults for them"},
		{"servers:\n  web-01:\n---\nservers:\n  db-01:\n", "line 3: a plan is one YAML document"},
		{"servers:\n  web-01:\n---\n", "line 3: a plan is one YAML document"},
		{"servers:\n  web-01:\n    profile:\n", "line 3: servers.web-01: profile has no value"},
		{"defaults:\n  group: ~\nservers:\n  web-01:\n", "line 2: defaults: group has no value"},
		{"servers:\n  web-01:\n    hostname: \"\"\n", "line 3: servers.web-01: hostname has no value"},
		{"- web-01\n", "line 1: a plan is a mapping of defaults and servers"},
		{"defaults: {}\n", "no servers"},
		{"servers: {}\n", "line 1: no servers"},
		{"servers: [web-01]\n", "line 1: servers is a mapping of the servers' names to their values"},
		{"server:\n  web-01: {}\n", "line 1: server is no part of a plan (defaults, servers)"},
		{"servers:\n  web-01: {}\nservers:\n  db-01: {}\n", "line 3: servers is given twice"},
		{"servers:\n  web-01: {}\n  web-01: {}\n", "line 3: server web-01 is given twice"},
		{"servers:\n  web-01:\n    profil: web\n", "line 3: servers.web-01: profil is no flag of a server (profile, layout, firewall, os, hostname, private-network, private-ip, timezone, disks, ssh-key, set, user, userdata, group, ipv6, mitigations, via, storage)"},
		{"defaults:\n  no-reboot: true\nservers:\n  web-01:\n", "line 2: defaults: no-reboot is no flag of a server"},
		{"defaults:\n  profile: web\n  private-ip: 10.0.0.5\nservers:\n  web-01:\n", "line 3: defaults: private-ip would give every server the same address; give it per server"},
		{"defaults:\n  private-ip: [db=10.0.1.5]\nservers:\n  web-01:\n", "line 2: defaults: private-ip would give every server the same address; give it per server"},
		{"servers:\n  web-01:\n    private-network: [[db]]\n", "servers.web-01: line 3: a list of values, one per line or in [ ]"},
		{"servers:\n  web-01:\n    layout: a\n    layout: b\n", "line 4: servers.web-01: layout is given twice"},
		{"servers:\n  web-01: lvm-raid1\n", "line 2: servers.web-01 is a mapping of reinstall's flags to their values"},
		{"servers:\n  web-01:\n    profile: [web]\n", "servers.web-01: yaml: unmarshal errors:\n  line 3: cannot unmarshal !!seq into string"},
		{"servers:\n  web-01:\n    ipv6: sure\n", "servers.web-01: yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `sure` into bool"},
		{"servers:\n  web-01:\n    group: {web: 1}\n", "servers.web-01: line 3: a value, or a list of values"},
		{"servers:\n  web-01:\n    user: [[oleg]]\n", "servers.web-01: line 3: a list of values, one per line or in [ ]"},
		{"servers:\n  web-01: {\n", "yaml: line 2: did not find expected node content"},
	} {
		_, err := parsePlan([]byte(c.plan))
		if err == nil || !strings.HasPrefix(err.Error(), c.err) {
			t.Errorf("%q: %v, want %s", c.plan, err, c.err)
		}
	}
}

func TestLinePrefix(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	a := &linePrefix{mu: &mu, w: &out, prefix: "[a] "}
	b := &linePrefix{mu: &mu, w: &out, prefix: "[b] "}
	a.Write([]byte("Waiting for SSH ..."))
	b.Write([]byte("one\ntwo"))
	a.Write([]byte(" up after 3s.\nnext\n"))
	b.Flush()
	a.Flush()
	if out.String() != "[b] one\n[a] Waiting for SSH ... up after 3s.\n[a] next\n[b] two\n" {
		t.Errorf("%q", out.String())
	}
}

// multiEnv is the test environment with two machines that let UMP in as
// root, each with its own state behind the fake ssh.
func multiEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	e.directRoot = true
	e.machines = map[string]*fakeMachine{}
	return e
}

// writePlan writes a plan file and returns its path.
func writePlan(t *testing.T, plan string) string {
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := os.WriteFile(path, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// specOf is the specification the installer got on a machine, last.
func (e *testEnv) specOf(addr string) map[string]any {
	e.t.Helper()
	for i := len(e.remoteCalls) - 1; i >= 0; i-- {
		argv := e.remoteCalls[i]
		if sshAddr(argv) == addr && strings.Contains(argv[len(argv)-1], "/install/install.sh") {
			var spec map[string]any
			if err := json.Unmarshal([]byte(e.remoteStdin[i]), &spec); err != nil {
				e.t.Fatalf("spec: %v", err)
			}
			return spec
		}
	}
	e.t.Fatalf("no installer run on %s", addr)
	return nil
}

// installsOn are the addresses the installer ran on, in order.
func (e *testEnv) installsOn() []string {
	var out []string
	for _, argv := range e.remoteCalls {
		if strings.Contains(argv[len(argv)-1], "/install/install.sh") {
			out = append(out, sshAddr(argv))
		}
	}
	return out
}

// installLines are the lines of stderr from the installations: after
// the line that starts them, up to the first that is no server's.
func installLines(errOut string) []string {
	_, after, _ := strings.Cut(errOut, " at once ...\n")
	var lines []string
	for _, l := range strings.Split(after, "\n") {
		if !strings.HasPrefix(l, "[") {
			break
		}
		lines = append(lines, l)
	}
	return lines
}

func TestMultiinstall(t *testing.T) {
	e := multiEnv(t)
	plan := writePlan(t, `defaults:
  group: web
  layout: lvm-striped
servers:
  web-01: {}
  db-01:
    profile: debian-13
    layout: lvm-raid1
    group: [db, backup]
    hostname: db-01.example.net
`)
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	var held []backend.LockInfo
	var before []string
	e.onRun = func() {
		held, _ = b.Locks(context.Background())
		before = append(e.installsOn(), e.hostFile("web-01.yml"), e.hostFile("db-01.example.net.yml"))
	}
	e.hostRecaps = map[string]hostRecap{"web-01": {OK: 4, Changed: 1}}
	out, errOut, code := e.run("", false, "server", "multiinstall", "--plan", plan, "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("multiinstall: code %d, %q\n%s", code, errOut, out)
	}
	// one table and the installations side by side, every line whole and
	// with its server in front
	profiles := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")
	for _, want := range []string{
		"SERVER   PROVIDER   OS             LAYOUT        HOSTNAME            PRIVATE-IPV4   GROUPS\nweb-01   fake/1     ubuntu-26.04   lvm-striped   web-01              10.0.0.1       common,web\ndb-01    fake/2     debian-13      lvm-raid1     db-01.example.net   10.0.0.2       common,db,backup\nProvisioning after the installations: 00-base on the fleet.\nInstalling 2 servers, all at once ...\n",
		"[web-01] Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 26.04 LTS).\n",
		"[db-01] Checking root access to 203.0.113.11 over SSH ... yes (Ubuntu 26.04 LTS).\n",
		"[web-01] Installing ubuntu-26.04 (profile ubuntu-26.04 from " + filepath.Join(profiles, "ubuntu-26.04.yaml") + ", layout lvm-striped, private address 10.0.0.1 in default) on web-01 as web-01 ...\n",
		"[db-01] Installing debian-13 (profile debian-13 from " + filepath.Join(profiles, "debian-13.yaml") + ", layout lvm-raid1, private address 10.0.0.2 in default) on db-01 as db-01.example.net ...\n",
		"[db-01] Waiting for SSH on 203.0.113.11 to go down ... still up; assuming it rebooted quickly.\n",
		"[web-01] web-01 is up: Ubuntu 26.04.1 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).\n",
		"[db-01] db-01 is up: Ubuntu 26.04.1 LTS, hostname db-01.example.net, root on /dev/mapper/vg0-root; log in as ump (ump ssh db-01).\n",
		"\nWrote inventory/hosts/db-01.example.net.yml: db-01.example.net in common, db, backup.\nWrote inventory/hosts/web-01.yml: web-01 in common, web.\nProvisioning: 00-base on the fleet (2 hosts) ...\n",
		"\nProvisioned 2 hosts, 1 changed.\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress lacks %q:\n%s", want, errOut)
		}
	}
	lines := installLines(errOut)
	if len(lines) < 20 {
		t.Errorf("installation lines:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "[web-01] ") && !strings.HasPrefix(l, "[db-01] ") {
			t.Errorf("line without its server: %q", l)
		}
	}
	if strings.Count(errOut, "WARNING: only UMP's own key opens the rescue system") != 1 || strings.Count(errOut, "Generated UMP's SSH key") != 1 {
		t.Errorf("said more than once:\n%s", errOut)
	}
	// each server its own specification
	web, db := e.specOf("203.0.113.10"), e.specOf("203.0.113.11")
	if web["os"] != "ubuntu-26.04" || web["hostname"] != "web-01" || web["layout"].(map[string]any)["name"] != "lvm-striped" {
		t.Errorf("web-01's spec: %v", web)
	}
	if db["os"] != "debian-13" || db["hostname"] != "db-01.example.net" || db["layout"].(map[string]any)["name"] != "lvm-raid1" {
		t.Errorf("db-01's spec: %v", db)
	}
	// one run, after every installation, on both new hosts, with their
	// locks and the fleet's held
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs(importing()) {
		t.Errorf("runs: %v", runs)
	}
	// the new systems start without the old ones' facts
	if e.imported != "# The playbooks of a run of ump provision, which removes this file after it.\n"+clearFacts("db-01.example.net,web-01")+"- import_playbook: \"00-base.yml\"\n" {
		t.Errorf("imported:\n%s", e.imported)
	}
	if !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") || !strings.Contains(e.hostFile("db-01.example.net.yml"), "groups: [common, db, backup]\n") {
		t.Errorf("host files:\n%s\n%s", e.hostFile("web-01.yml"), e.hostFile("db-01.example.net.yml"))
	}
	if len(before) != 4 || before[2] == "" || before[3] == "" {
		t.Errorf("before the run: %q", before)
	}
	if len(held) != 3 || held[0].Key != "fake/1" || held[0].What != "multiinstall" || held[1].Key != "fake/2" || held[2].Key != "fleet" || held[2].What != "provision after installing web-01, db-01" {
		t.Errorf("locks while provisioning: %+v", held)
	}
	if locks, _ := b.Locks(context.Background()); len(locks) != 0 {
		t.Errorf("locks left: %+v", locks)
	}
	st := e.readState()
	for _, want := range []string{"  fake/1:\n    installed:\n      os: ubuntu-26.04\n", "      layout: lvm-striped\n      hostname: web-01\n", "    groups:\n      - web\n", "  fake/2:\n    installed:\n      os: debian-13\n", "      hostname: db-01.example.net\n", "    groups:\n      - db\n      - backup\n"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q:\n%s", want, st)
		}
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "yes", "yes"},
		{"db-01", "db-01.example.net", "yes", "yes"},
	})

	// -o json: per server, with the run
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "server", "multiinstall", "--plan", plan, "--yes", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("-o json: code %d, %q", code, errOut)
	}
	var res struct {
		Servers []struct {
			Name        string   `json:"name"`
			Hostname    string   `json:"hostname"`
			OS          string   `json:"os"`
			Layout      string   `json:"layout"`
			Groups      []string `json:"groups"`
			Via         string   `json:"via"`
			Rebooted    bool     `json:"rebooted"`
			Installed   bool     `json:"installed"`
			Provisioned bool     `json:"provisioned"`
			Playbooks   []string `json:"playbooks"`
			InstalledAt string   `json:"installed_at"`
			Reason      string   `json:"reason"`
		} `json:"servers"`
		Provision *provisionResult `json:"provision"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(res.Servers) != 2 || res.Provision == nil || len(res.Provision.Hosts) != 2 || res.Provision.Hosts["web-01"].Changed != 1 {
		t.Fatalf("json:\n%s", out)
	}
	s := res.Servers[1]
	if s.Name != "db-01" || s.Hostname != "db-01.example.net" || s.OS != "debian-13" || s.Layout != "lvm-raid1" || strings.Join(s.Groups, ",") != "common,db,backup" || s.Via != "direct" || !s.Rebooted || !s.Installed || !s.Provisioned || strings.Join(s.Playbooks, ",") != "00-base" || s.InstalledAt != "2026-10-12T09:00:00Z" || s.Reason != "" {
		t.Errorf("db-01 in json: %+v", s)
	}

	// the same flags for every server on the command line; -o yaml
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--profile", "debian-13", "--layout", "lvm-striped", "--group", "web", "--yes", "--account", "one", "-o", "yaml")
	if code != 0 || strings.Count(out, "    installed: true\n") != 2 || !strings.Contains(out, "provision:\n  playbooks:\n    - 00-base\n") {
		t.Fatalf("shared flags: code %d, %q\n%s", code, errOut, out)
	}
	for _, addr := range []string{"203.0.113.10", "203.0.113.11"} {
		if spec := e.specOf(addr); spec["os"] != "debian-13" || spec["layout"].(map[string]any)["name"] != "lvm-striped" {
			t.Errorf("%s with shared flags: %v", addr, spec)
		}
	}
	if !strings.Contains(e.hostFile("db-01.yml"), "groups: [common, web]\n") || e.hostFile("db-01.example.net.yml") != "" || !strings.Contains(errOut, "Removed inventory/hosts/db-01.example.net.yml: fake/2 has the hostname db-01 now.\n") {
		t.Errorf("shared flags, host files: %q\n%s", errOut, e.hostFile("db-01.yml"))
	}
}

func TestMultiinstallChecksFirst(t *testing.T) {
	e := multiEnv(t)
	hosts := filepath.Join(e.repo, "inventory", "hosts")
	os.MkdirAll(hosts, 0o755)
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	cases := []struct {
		name  string
		args  []string
		plan  string
		setup func()
		code  int
		msg   string
	}{
		{"no servers", nil, "", nil, 2, "ump: give the servers, NAME..., or a plan, --plan FILE\nRun 'ump server multiinstall --help' for usage.\n"},
		{"names and a plan", []string{"web-01"}, "servers:\n  db-01:\n", nil, 2, "ump: give the servers as NAME... or in the plan, not both\n"},
		{"a flag of the plan's", []string{"--profile", "debian-13"}, "servers:\n  db-01:\n", nil, 2, "ump: --profile and --plan: the plan has the servers' values, and its defaults those for every server\n"},
		{"a plan not as it should be", nil, "servers:\n  db-01:\n    profil: x\n", nil, 2, ": line 3: servers.db-01: profil is no flag of a server (profile, layout,"},
		{"a value of the plan", nil, "servers:\n  db-01:\n    group: [All]\n", nil, 2, `: servers.db-01: group "All": a group name is lower-case letters, digits and '_', not starting with a digit` + "\n"},
		{"--parallel 0", []string{"web-01", "--parallel", "0"}, "", nil, 2, "ump: --parallel 0: give 1 or more (default: all at once)\n"},
		{"narrowing without provisioning", []string{"web-01", "--no-provision", "--hosts", "web-01"}, "", nil, 2, "ump: --hosts and --playbook narrow the provisioning, which --no-provision leaves out\n"},
		{"no ansible-playbook", []string{"web-01", "db-01"}, "", func() { e.noAnsible = true }, 2, "ump: cannot provision from " + e.repo + ": no ansible-playbook in PATH; install ansible-core, or give --no-provision\n"},
		{"an unknown server", []string{"web-01", "web-09"}, "", nil, 2, "ump: server \"web-09\" not found\n"},
		{"a server twice", []string{"web-01", "203.0.113.10"}, "", nil, 2, "ump: web-01 and 203.0.113.10 are the same server, fake/1; name it once\n"},
		{"a layout of one server", nil, "defaults:\n  group: web\nservers:\n  web-01:\n  db-01:\n    layout: nope\n", nil, 2, ": servers.db-01: profile ubuntu-26.04: layout \"nope\" is not in layouts.yaml (known: lvm-example, lvm-linear, lvm-raid1, lvm-striped; ump config os update fetches the published ones)\n"},
		{"a userdata of one server", []string{"web-01", "db-01", "--userdata", "nope"}, "", nil, 2, "ump: web-01: --userdata nope: not in userdata.yaml (known: "},
		{"a hostname twice", []string{"web-01", "db-01", "--hostname", "web"}, "", nil, 2, "ump: web-01 and db-01 would both have the hostname web; give one of them another (hostname: in a plan)\n"},
		{"a hostname as another's group", nil, "servers:\n  web-01:\n    group: [db]\n  db-01:\n    hostname: db\n", nil, 2, "ump: the hostname db of db-01 is also a group of web-01: a host and a group with the same name make the group's plays match only the host; give one of them another\n"},
		{"a host file of another server", []string{"web-01", "db-01"}, "", func() {
			os.WriteFile(filepath.Join(hosts, "db-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/9\n"), 0o644)
		}, 2, "ump: inventory/hosts/db-01.yml is the host file of fake/9 (its ump_ref); give db-01 another hostname, or remove the file if that server is gone\n"},
		{"--hosts matching no new host", []string{"web-01", "db-01", "--group", "web", "--hosts", "lab*"}, "", nil, 2, "ump: --hosts lab* matches no host of the inventory with web-01 in common, web; db-01 in common, web; nothing was changed\n"},
		{"a server locked", []string{"web-01", "db-01"}, "", func() {
			b.Lock(context.Background(), "fake/2", backend.LockInfo{Key: "fake/2", Who: "bob@desk", What: "rescue", Since: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)})
		}, 2, "ump: db-01: fake/2 is locked by bob@desk since 2026-10-12T08:00:00Z (rescue); ump config state unlock fake/2 forces it; nothing was changed\n"},
		{"no confirmation", []string{"web-01", "db-01"}, "", nil, 3, "web-01   fake/1     ubuntu-26.04   lvm-linear   web-01     10.0.0.1       common\ndb-01    fake/2     ubuntu-26.04   lvm-linear   db-01      10.0.0.2       common\nump: ump server multiinstall needs confirmation: run it in a terminal or pass --yes\n"},
	}
	for _, c := range cases {
		args := append([]string{"server", "multiinstall"}, c.args...)
		if c.plan != "" {
			args = append(args, "--plan", writePlan(t, c.plan))
		}
		if !slices.Contains(c.args, "--no-provision") && c.code != 3 {
			args = append(args, "--yes")
		}
		args = append(args, "--account", "one")
		if c.setup != nil {
			c.setup()
		}
		_, errOut, code := e.run("", false, args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%s: code %d (want %d), %q", c.name, code, c.code, errOut)
		}
		e.noAnsible = false
		os.Remove(filepath.Join(hosts, "db-01.yml"))
		b.Unlock(context.Background(), "fake/2")
	}
	// answered no on a terminal: the one question for all of them
	_, errOut, code := e.run("n\n", true, "server", "multiinstall", "web-01", "db-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "Install on these 2 servers? Every disk of each is erased. Then ump provision runs 00-base on the fleet. [y/N]") {
		t.Errorf("answered no: code %d, %q", code, errOut)
	}
	// nothing touched: no remote call, no provider, no record, no host
	// file, no run
	if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 || strings.Contains(e.readState(), "installed") || e.hostFile("web-01.yml") != "" || len(e.ansibleRuns()) != 0 {
		t.Errorf("touched: remote %v, provider %v, state %q, runs %v", e.remoteCalls, fakePower.actions, e.readState(), e.ansibleRuns())
	}

	// a hostname that is a group of another server in the state
	d := state.New()
	d.Servers["fake/2"] = state.Server{Installed: &state.Install{OS: "debian-13", Profile: "debian-13", Hostname: "db-01", At: e.now}, Groups: []string{"web1"}}
	data, _ := d.Bytes()
	os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"), data, 0o600)
	_, errOut, code = e.run("", false, "server", "multiinstall", "--plan", writePlan(t, "servers:\n  web-01:\n    hostname: web1\n"), "--yes", "--account", "one")
	if code != 2 || !strings.HasSuffix(errOut, "ump: the hostname web1 is also a group of fake/2 in the state: "+sameName+"; give web-01 another hostname, or ump server set fake/2 --ungroup web1\n") || len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("a group of the state: code %d, %q", code, errOut)
	}
}

// overlapGate holds every installer run until the other comes, up to
// wait, and keeps how many ran at once at most.
type overlapGate struct {
	mu       sync.Mutex
	inflight int
	peak     int
}

func (g *overlapGate) gate(wait time.Duration) func(string, []string) {
	return func(addr string, argv []string) {
		if !strings.Contains(argv[len(argv)-1], "/install/install.sh") {
			return
		}
		g.mu.Lock()
		g.inflight++
		g.peak = max(g.peak, g.inflight)
		g.mu.Unlock()
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			g.mu.Lock()
			met := g.peak >= 2
			g.mu.Unlock()
			if met {
				break
			}
		}
		g.mu.Lock()
		g.inflight--
		g.mu.Unlock()
	}
}

func TestMultiinstallSideBySide(t *testing.T) {
	e := multiEnv(t)
	// all at once: both installers run at the same time
	g := &overlapGate{}
	e.remoteGate = g.gate(5 * time.Second)
	if _, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one"); code != 0 || g.peak != 2 {
		t.Errorf("all at once: code %d, peak %d, %q", code, g.peak, errOut)
	}
	// --parallel 1: one after the other, in order
	g = &overlapGate{}
	e.remoteGate = g.gate(200 * time.Millisecond)
	e.remoteCalls = nil
	_, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--parallel", "1", "--yes", "--account", "one")
	if code != 0 || g.peak != 1 || strings.Join(e.installsOn(), " ") != "203.0.113.10 203.0.113.11" || !strings.Contains(errOut, "Installing 2 servers, 1 at a time ...\n") {
		t.Errorf("--parallel 1: code %d, peak %d, %v, %q", code, g.peak, e.installsOn(), errOut)
	}
	// a dry run side by side too: the plans, nothing written, no run
	g = &overlapGate{}
	e.remoteGate = g.gate(5 * time.Second)
	os.RemoveAll(filepath.Join(e.repo, "inventory", "hosts"))
	os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"))
	e.remoteCalls, e.ansibleCalls, e.installs = nil, nil, nil
	out, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--dry-run", "--yes", "--account", "one")
	if code != 0 || g.peak != 2 || len(e.installs) != 2 || !strings.HasSuffix(e.installs[0], " --dry-run") || !strings.HasSuffix(e.installs[1], " --dry-run") {
		t.Errorf("dry run: code %d, peak %d, %q, %q", code, g.peak, e.installs, errOut)
	}
	for _, want := range []string{"Installation plans for 2 servers, all at once ...\n", "[web-01] Installation plan for ubuntu-26.04", "[db-01] ==> dry run: stopping here, no disk was touched\n", "[db-01] Dry run done; db-01 stays in the UMP rescue system (ump ssh db-01 --staging).\n"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("dry run lacks %q:\n%s", want, errOut)
		}
	}
	if e.reboots != 0 || strings.Contains(e.readState(), "installed") || e.hostFile("web-01.yml") != "" || len(e.ansibleRuns()) != 0 || strings.Contains(errOut, "Wrote ") {
		t.Errorf("dry run wrote or ran: reboots %d, state %q, runs %v", e.reboots, e.readState(), e.ansibleRuns())
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "dry-run", "-"},
		{"db-01", "db-01", "dry-run", "-"},
	})
	out, _, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--dry-run", "--yes", "--account", "one", "-o", "json")
	if code != 0 || strings.Count(out, `"dry_run": true`) != 2 || strings.Count(out, `"installed": false`) != 2 || strings.Contains(out, `"provision"`) {
		t.Errorf("dry run, json: code %d\n%s", code, out)
	}
	// with --verbose, the API calls of an installation are lines of its
	// progress too
	_, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--no-provision", "--yes", "--account", "one", "--verbose")
	if code != 0 || !strings.Contains(errOut, "\n[web-01] fake: GET /servers/1/rescue\n") || !strings.Contains(errOut, "\n[db-01] fake: GET /servers/2/rescue\n") || strings.Contains(errOut, "\nfake: ") {
		t.Errorf("--verbose: code %d, %q", code, errOut)
	}
}

func TestMultiinstallFailures(t *testing.T) {
	e := multiEnv(t)
	// db-01's installation fails; web-01's goes on, and is provisioned,
	// by the one run after both
	e.installExitAt = map[string]int{"203.0.113.11": 1}
	out, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one")
	if code != 1 || !strings.HasSuffix(errOut, "ump: the installation failed on db-01, see above\n") {
		t.Fatalf("one failing: code %d, %q", code, errOut)
	}
	for _, want := range []string{
		"[db-01] install.sh: no disk found\n",
		"[db-01] ump: the installation on db-01 failed (exit status 1, see above); the server stays in the UMP rescue system (ump ssh db-01 --staging)\n",
		"[web-01] web-01 is up:",
		"\nWrote inventory/hosts/web-01.yml: web-01 in common.\nProvisioning: 00-base on the fleet (1 host) ...\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("one failing lacks %q:\n%s", want, errOut)
		}
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || e.reboots != 1 || e.hostFile("db-01.yml") != "" || strings.Contains(e.readState(), "fake/2") {
		t.Errorf("one failing: runs %v, reboots %d, state\n%s", runs, e.reboots, e.readState())
	}
	if !strings.Contains(out, "web-01   web-01     yes         yes\n") || !strings.Contains(out, "db-01    db-01      no          -             the installation on db-01 failed (exit status 1, see above); the server stays in the UMP rescue system (ump ssh db-01 --staging)\n") {
		t.Errorf("one failing, summary:\n%s", out)
	}
	out, _, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one", "-o", "json")
	if code != 1 || !strings.Contains(out, `"installed": false,
      "reason": "the installation on db-01 failed (exit status 1, see above); the server stays in the UMP rescue system (ump ssh db-01 --staging)"`) {
		t.Errorf("one failing, json: code %d\n%s", code, out)
	}

	// both installed, db-01 fails in the run: exit status 6, web-01 is
	// provisioned by its own line
	e.installExitAt = nil
	e.hostRecaps = map[string]hostRecap{"db-01": {OK: 1, Failed: 1}}
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one")
	if code != 6 || !strings.HasSuffix(errOut, "ump: provisioning failed on db-01 (failed), see above; fix that and run ump provision --hosts db-01\n") {
		t.Errorf("failed in the run: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, "web-01   web-01     yes         yes\n") || !strings.Contains(out, "db-01    db-01      yes         no            failed in the run (see above)\n") {
		t.Errorf("failed in the run, summary:\n%s", out)
	}
	// web-01 alone, provisioned; the run failed on db-01, which is not new
	e.hostRecaps = map[string]hostRecap{"db-01": {Unreachable: 1}}
	_, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "--yes", "--account", "one")
	if code != 6 || !strings.HasSuffix(errOut, "ump: web-01 is provisioned; the run failed on db-01 (unreachable), see above; fix that and run ump provision --hosts db-01\n") {
		t.Errorf("provisioned, the run failed: code %d, %q", code, errOut)
	}
	// an installation that failed comes first
	e.installExitAt = map[string]int{"203.0.113.11": 2}
	e.hostRecaps = map[string]hostRecap{"web-01": {Failed: 1}}
	_, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one")
	if code != 1 || !strings.HasSuffix(errOut, "ump: the installation failed on db-01, see above; provisioning failed on web-01 (failed), see above; fix that and run ump provision --hosts web-01\n") {
		t.Errorf("both: code %d, %q", code, errOut)
	}
	// every installation failed: nothing to provision
	e.installExitAt = map[string]int{"203.0.113.10": 1, "203.0.113.11": 1}
	e.ansibleCalls = nil
	_, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "Not provisioned: no server was installed.\n") || len(e.ansibleRuns()) != 0 || !strings.HasSuffix(errOut, "ump: the installation failed on web-01, db-01, see above\n") {
		t.Errorf("all failing: code %d, %q", code, errOut)
	}
}

func TestMultiinstallProvisioning(t *testing.T) {
	e := multiEnv(t)
	// --hosts narrows the one run; a new server without a line of the
	// recap is not provisioned
	out, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--group", "web", "--hosts", "web-01", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Provisioning after the installations: 00-base on web-01.\n") || !strings.Contains(errOut, "Provisioning: 00-base on web-01 (1 host) ...\n") {
		t.Errorf("--hosts: code %d, %q", code, errOut)
	}
	// the facts of the new hosts of the run are cleared; db-01, outside
	// it, keeps the mark of stale facts for the next run
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs(importing(), "--limit", "web-01") || !strings.Contains(e.imported, clearFacts("web-01")) {
		t.Errorf("--hosts, runs: %v\n%s", runs, e.imported)
	}
	if d, err := state.Parse([]byte(e.readState())); err != nil || d.Servers["fake/1"].StaleFacts || !d.Servers["fake/2"].StaleFacts {
		t.Errorf("--hosts, state: %v\n%s", err, e.readState())
	}
	if !strings.Contains(out, "web-01   web-01     yes         yes\n") || !strings.Contains(out, "db-01    db-01      yes         no            no play of the run matched it\n") {
		t.Errorf("--hosts, summary:\n%s", out)
	}
	// --no-provision: the host files, under the fleet's lock, no run
	os.RemoveAll(filepath.Join(e.repo, "inventory", "hosts"))
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--group", "db", "--no-provision", "--yes", "--account", "one")
	if code != 0 || !strings.HasSuffix(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, db.\nWrote inventory/hosts/db-01.yml: db-01 in common, db.\nNot provisioned (--no-provision); the next ump provision includes the servers.\n") || strings.Contains(errOut, "\nProvisioning") {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if len(e.ansibleRuns()) != 0 {
		t.Errorf("--no-provision, runs: %v", e.ansibleRuns())
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "yes", "-"},
		{"db-01", "db-01", "yes", "-"},
	})
}

// TestMultiinstallInterrupted: an interrupt leaves the installations not
// started yet; those that completed are recorded, not provisioned, and
// say so, and the next ump provision includes them.
func TestMultiinstallInterrupted(t *testing.T) {
	e := multiEnv(t)
	e.interruptAt = "203.0.113.10"
	out, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--parallel", "1", "--yes", "--account", "one")
	if code != 3 || !strings.HasSuffix(errOut, "Interrupted: web-01 is installed and recorded; the next ump provision includes it.\nump: interrupted\n") || strings.Join(e.installsOn(), " ") != "203.0.113.10" || len(e.ansibleRuns()) != 0 || e.hostFile("web-01.yml") != "" {
		t.Errorf("interrupted: code %d, installs %v, runs %v, %q", code, e.installsOn(), e.ansibleRuns(), errOut)
	}
	if !strings.Contains(out, "web-01   web-01     yes         no            interrupted before the provisioning; run ump provision\n") || !strings.Contains(out, "db-01    db-01      no          -             interrupted\n") {
		t.Errorf("interrupted, summary:\n%s", out)
	}
	if st := e.readState(); !strings.Contains(st, "fake/1:") || strings.Contains(st, "fake/2:") {
		t.Errorf("interrupted, state:\n%s", st)
	}
	// the next run includes it, without its old system's facts
	e.interruptAt = ""
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || !strings.Contains(e.hostFile("web-01.yml"), "ump_ref: fake/1\n") || !strings.Contains(e.imported, clearFacts("web-01")) || strings.Contains(e.readState(), "stale_facts") {
		t.Errorf("the next provision: code %d, %q\n%s", code, errOut, e.imported)
	}

	// --no-provision: no host files either
	os.RemoveAll(filepath.Join(e.repo, "inventory", "hosts"))
	e.interruptAt, e.remoteCalls = "203.0.113.10", nil
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--parallel", "1", "--no-provision", "--yes", "--account", "one", "-o", "json")
	if code != 3 || !strings.Contains(errOut, "Interrupted: web-01 is installed and recorded; the next ump provision includes it.\n") || e.hostFile("web-01.yml") != "" {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, `"installed": true,
      "reason": "interrupted; the host files are not written"`) || !strings.Contains(out, `"installed": false,
      "reason": "interrupted"`) {
		t.Errorf("--no-provision, json:\n%s", out)
	}
}
