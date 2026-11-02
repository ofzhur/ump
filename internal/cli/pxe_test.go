package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/pxe"
)

// pxeHome is the local home of a test, where pxe.yaml and .pxe/ are.
func pxeHome() string { return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump") }

func readPXE(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(pxeHome(), "pxe.yaml"))
	return string(data)
}

func writePXE(t *testing.T, text string) {
	t.Helper()
	os.MkdirAll(pxeHome(), 0o700)
	if err := os.WriteFile(filepath.Join(pxeHome(), "pxe.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// siteDoc is the site.json of a site in the test's home, nil when none.
func siteDoc(t *testing.T, site string) *pxe.SiteDoc {
	t.Helper()
	d, err := pxe.Store{Files: &backend.Local{Dir: pxeHome()}}.Site(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pxeStoreOf() pxe.Store { return pxe.Store{Files: &backend.Local{Dir: pxeHome()}} }

func TestConfigPXE(t *testing.T) {
	e := newTestEnv(t)
	e.write("staging:\n  image: https://storage.example.net/rescue\n")

	out, errOut, code := e.run("", false, "config", "pxe", "list")
	if code != 0 || out != "" || !strings.Contains(errOut, "No machines in ") {
		t.Fatalf("no pxe.yaml: code %d, %q %q", code, out, errOut)
	}

	out, errOut, code = e.run("", false, "config", "px", "create", "web-07", "--mac", "0C-C4-7A-AA-BB-01",
		"--set", "network.ipv4.address=192.0.2.17/24", "--set", "profile=web", "--set", "group=[web, db]")
	if code != 0 {
		t.Fatalf("create: code %d, %q", code, errOut)
	}
	d := siteDoc(t, pxe.DefaultSite)
	if d == nil || len(d.Machines) != 1 || d.Image != "https://storage.example.net/rescue" {
		t.Fatalf("site.json: %+v", d)
	}
	for _, want := range []string{
		"Created the machine web-07 in ",
		"Applied site default: 1 machine (site.json " + d.Hash + ").\n",
		"Site default: no PXE server of it runs (ump pxe serve --site default).\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("create says %q, not %q", out, want)
		}
	}
	m := d.Machines[0]
	ump, _ := os.ReadFile(filepath.Join(pxeHome(), "staging_ed25519.pub"))
	if m.Name != "web-07" || strings.Join(m.MACs, ",") != "0c:c4:7a:aa:bb:01" || m.Mode != pxe.ModeLocal || len(m.Keys) != 1 || m.Keys[0] != strings.TrimSpace(string(ump)) {
		t.Errorf("web-07 in site.json: %+v (UMP's key %q)", m, ump)
	}
	text := readPXE(t)
	for _, want := range []string{"# The machines ump boots over the network", "machines:\n  web-07:\n    mac: 0C-C4-7A-AA-BB-01\n", "    network:\n      ipv4:\n        address: 192.0.2.17/24\n", "    profile: web\n", "    group: [web, db]\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("pxe.yaml lacks %q:\n%s", want, text)
		}
	}

	// refused: nothing is saved
	for _, c := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"web-07", "--mac", "0c:c4:7a:aa:bb:02"}, 1, "machine web-07 exists already (ump config pxe edit web-07)"},
		{[]string{"db-01"}, 2, "--mac is needed without a terminal: ump config pxe create db-01 --mac MAC"},
		{[]string{"Db_01", "--mac", "0c:c4:7a:aa:bb:02"}, 2, `invalid name "Db_01"`},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:01"}, 2, "nothing was saved: line 26: machines.db-01: mac: 0c:c4:7a:aa:bb:01 is web-07's already: a MAC boots one machine"},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:02", "--mode", "install"}, 2, "machines.db-01: mode: install: unattended installations are not supported yet; local or rescue"},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:02", "--set", "network.ipv4.address=192.0.2.18"}, 2, `network.ipv4.address: "192.0.2.18" is not an IPv4 address with its prefix length`},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:02", "--set", "vlan=40"}, 2, "machines.db-01: vlan is no key of a machine"},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:02", "--set", "novalue"}, 2, `--set needs KEY=VALUE, got "novalue"`},
		{[]string{"db-01", "--mac", "0c:c4:7a:aa:bb:02", "--set", "rescue-keys=[nokey]"}, 2, `nothing was saved: machines.db-01: rescue-keys: rescue key "nokey": `},
	} {
		_, errOut, code := e.run("", false, append([]string{"config", "pxe", "create"}, c.args...)...)
		if code != c.code || !strings.Contains(errOut, c.err) || readPXE(t) != text {
			t.Errorf("create %v: code %d, %q", c.args, code, errOut)
		}
	}

	out, errOut, code = e.run("", false, "config", "pxe", "list")
	if code != 0 || errOut != "" {
		t.Fatalf("list: code %d, %q", code, errOut)
	}
	if want := "NAME     SITE      MAC                 STATUS   PROFILE   ADDRESS      SEEN\nweb-07   default   0c:c4:7a:aa:bb:01   local    web       192.0.2.17   -\n"; out != want {
		t.Errorf("list:\n%s\nwant\n%s", out, want)
	}
	out, _, _ = e.run("", false, "config", "pxe", "get", "web-07")
	if !strings.HasPrefix(out, "web-07:\n  mac: 0C-C4-7A-AA-BB-01\n") {
		t.Errorf("get:\n%s", out)
	}
	out, _, code = e.run("", false, "config", "pxe", "get", "web-07", "--merged")
	for _, want := range []string{"web-07:\n  site: default\n  mac:\n    - 0c:c4:7a:aa:bb:01\n  mode: local\n  hostname: web-07\n", "    uplink: 0c:c4:7a:aa:bb:01\n", "    ipv6: none\n", "  profile: web\n", "  group:\n    - web\n    - db\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("get --merged lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dns") || strings.Contains(out, "null") {
		t.Errorf("get --merged: a static address keeps the public resolvers, and nothing is null:\n%s", out)
	}
	out, _, _ = e.run("", false, "config", "pxe", "get", "web-07", "--merged", "-o", "json")
	var merged map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &merged); err != nil || merged["web-07"]["site"] != "default" {
		t.Errorf("get --merged -o json: %v\n%s", err, out)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "get", "nope"); code != 1 || !strings.Contains(errOut, `no machine "nope" in `) {
		t.Errorf("get nope: code %d, %q", code, errOut)
	}

	// a hand edit reaches the servers with apply alone
	writePXE(t, text+"  db-01:\n    mac: [0c:c4:7a:aa:bb:10, 0c:c4:7a:aa:bb:11]\n    mode: rescue\n    address: 192.0.2.30\n")
	out, errOut, code = e.run("", false, "config", "pxe", "list", "-o", "wide")
	if code != 0 || errOut != "pxe.yaml changed since it was applied (site default): ump config pxe apply\n" || !strings.Contains(out, "db-01    default   0c:c4:7a:aa:bb:10,0c:c4:7a:aa:bb:11   rescue   -         192.0.2.30") {
		t.Errorf("list after a hand edit: code %d, %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "config", "pxe", "apply")
	if code != 0 || !strings.Contains(out, "Applied site default: 2 machines") || len(siteDoc(t, pxe.DefaultSite).Machines) != 2 {
		t.Errorf("apply: code %d, %q %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "config", "pxe", "apply")
	if code != 0 || out != "Site default is up to date (site.json "+siteDoc(t, pxe.DefaultSite).Hash+").\n" {
		t.Errorf("apply again: code %d, %q", code, out)
	}
	if _, errOut, _ := e.run("", false, "config", "pxe", "list"); errOut != "" {
		t.Errorf("list after apply: %q", errOut)
	}

	// what the backend holds shows: an arm, a server that saw the machine
	ctx := context.Background()
	store := pxeStoreOf()
	arm := &pxe.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: pxe.ModeRescue, By: "oleg@laptop", Created: e.now, Until: e.now.Add(15 * time.Minute)}
	if _, err := store.PutArm(ctx, pxe.DefaultSite, arm, ""); err != nil {
		t.Fatal(err)
	}
	store.PutStatus(ctx, &pxe.Status{Daemon: "pxe1", Site: pxe.DefaultSite, Started: e.now, Updated: e.now, SiteHash: siteDoc(t, pxe.DefaultSite).Hash,
		Machines: map[string]*pxe.Seen{"0c:c4:7a:aa:bb:11": {Name: "db-01", At: e.now.Add(-4 * time.Minute), Answer: pxe.ModeRescue}}})
	out, _, _ = e.run("", false, "config", "pxe", "list")
	if !strings.Contains(out, "web-07   default   0c:c4:7a:aa:bb:01      armed rescue   web       192.0.2.17   -\n") || !strings.Contains(out, "db-01    default   0c:c4:7a:aa:bb:10 +1   rescue         -         192.0.2.30   4m ago\n") {
		t.Errorf("list with an arm and a server:\n%s", out)
	}
	out, _, _ = e.run("", false, "config", "pxe", "list", "-o", "json")
	var rows []pxeRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 || rows[0].Status != "armed rescue" || rows[1].SeenBy != "pxe1" || rows[1].Mode != pxe.ModeRescue {
		t.Errorf("list -o json: %v %+v", err, rows)
	}

	// sites: the site.json of a site gone goes, but not under a
	// machine whose arm is pending there; disarm finds the arm where it
	// was written
	writePXE(t, "sites:\n  fra1:\n    jump: ump@pxe1\nmachines:\n  web-07:\n    site: fra1\n    mac: 0c:c4:7a:aa:bb:01\n")
	hash := siteDoc(t, pxe.DefaultSite).Hash
	_, errOut, code = e.run("", false, "config", "pxe", "apply")
	if code != exitAborted || errOut != "ump: web-07 has an armed rescue at site default (a7f3c2e1, by oleg@laptop); disarm it before its site or MACs change (ump pxe disarm web-07)\n" ||
		siteDoc(t, pxe.DefaultSite).Hash != hash || siteDoc(t, "fra1") != nil {
		t.Errorf("apply moving an armed machine: code %d, %q", code, errOut)
	}
	if out, errOut, code := e.run("", false, "pxe", "disarm", "web-07"); code != 0 || out != "Disarmed web-07: its armed rescue (a7f3c2e1, by oleg@laptop) does not boot.\n" {
		t.Errorf("disarm at the site before: code %d, %q %q", code, out, errOut)
	}
	out, errOut, code = e.run("", false, "config", "pxe", "apply")
	if code != 0 || !strings.Contains(out, "Applied site fra1: 1 machine") || !strings.Contains(out, "Removed site default, gone from pxe.yaml: its servers serve nothing now.\n") {
		t.Errorf("apply with a site: code %d, %q %q", code, out, errOut)
	}
	if siteDoc(t, pxe.DefaultSite) != nil || siteDoc(t, "fra1") == nil {
		t.Error("site.json of default stays, or that of fra1 is missing")
	}

	// a pxe.yaml that is not valid: exit status 2, with its line
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n    mode: sometimes\n")
	for _, args := range [][]string{{"list"}, {"apply"}, {"get", "web-07"}} {
		_, errOut, code := e.run("", false, append([]string{"config", "pxe"}, args...)...)
		if code != 2 || !strings.Contains(errOut, `pxe.yaml: line 4: machines.web-07: mode: "sometimes" is not local, rescue or install`) {
			t.Errorf("%v with an invalid pxe.yaml: code %d, %q", args, code, errOut)
		}
	}
}

func TestConfigPXEApplyWaits(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	old := pxeApplyWait
	pxeApplyWait = 50 * time.Millisecond
	defer func() { pxeApplyWait = old }()
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	ctx := context.Background()
	store := pxeStoreOf()
	hash := siteDoc(t, pxe.DefaultSite).Hash
	status := func(daemon, hash string, updated time.Time) {
		if err := store.PutStatus(ctx, &pxe.Status{Daemon: daemon, Site: pxe.DefaultSite, Started: updated, Updated: updated, SiteHash: hash}); err != nil {
			t.Fatal(err)
		}
	}
	// a server that serves it, and one that stopped long ago
	status("pxe1", hash, e.now)
	status("pxe2", "0000", e.now.Add(-time.Hour))
	store.DeleteSite(ctx, pxe.DefaultSite)
	out, errOut, code := e.run("", false, "config", "pxe", "apply")
	if code != 0 || !strings.Contains(out, "Site default: served by pxe1.\n") {
		t.Errorf("served: code %d, %q %q", code, out, errOut)
	}
	// a running server that serves the old one
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n    mode: rescue\n")
	out, errOut, code = e.run("", false, "config", "pxe", "apply")
	if code != 5 || !strings.Contains(errOut, "these PXE servers do not serve the new site.json after 50ms: default/pxe1") || strings.Contains(out, "served by") {
		t.Errorf("lagging: code %d, %q %q", code, out, errOut)
	}
}

func TestConfigPXEEdit(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	// without pxe.yaml, edit opens a new one
	e.editorHook = func(path string) error { return nil }
	out, errOut, code := e.run("", false, "config", "pxe", "edit")
	if code != 0 || !strings.Contains(out, "0 machines, all valid.\n") || !strings.Contains(readPXE(t), "# The machines ump boots over the network") {
		t.Fatalf("edit without a file: code %d, %q %q", code, out, errOut)
	}
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(data), "machines:\n", "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n", 1)), 0o600)
	}
	out, errOut, code = e.run("", false, "config", "pxe", "edit")
	if code != 0 || !strings.Contains(out, "1 machine, all valid.\n") || siteDoc(t, pxe.DefaultSite).Machine("web-07") == nil {
		t.Fatalf("edit of the file: code %d, %q %q", code, out, errOut)
	}

	// one machine
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "# The machine web-07 of ") {
			t.Errorf("the entry to edit:\n%s", data)
		}
		return os.WriteFile(path, []byte(strings.Replace(string(data), "mac: 0c:c4:7a:aa:bb:01\n", "mac: 0c:c4:7a:aa:bb:01\n  mode: rescue\n", 1)), 0o600)
	}
	out, errOut, code = e.run("", false, "config", "pxe", "edit", "web-07")
	if code != 0 || !strings.Contains(out, "Saved the machine web-07 in ") || siteDoc(t, pxe.DefaultSite).Machine("web-07").Mode != pxe.ModeRescue {
		t.Fatalf("edit web-07: code %d, %q %q", code, out, errOut)
	}
	before := readPXE(t)
	e.editorHook = func(path string) error { return nil }
	if out, _, code := e.run("", false, "config", "pxe", "edit", "web-07"); code != 0 || !strings.Contains(out, "No changes to the machine web-07.\n") {
		t.Errorf("no change: code %d, %q", code, out)
	}
	// an edit that is not valid is kept in its file, nothing saved
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(data), "mode: rescue", "mode: install", 1)), 0o600)
	}
	_, errOut, code = e.run("", false, "config", "pxe", "edit", "web-07")
	if code != 2 || !strings.Contains(errOut, "unattended installations are not supported yet") || !strings.Contains(errOut, "nothing was saved; your edit is in ") || readPXE(t) != before {
		t.Errorf("an invalid edit: code %d, %q", code, errOut)
	}
	// with a pending arm the MACs stay; other changes go through
	store := pxeStoreOf()
	arm := &pxe.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: pxe.ModeRescue, By: "oleg@laptop", Created: e.now, Until: e.now.Add(15 * time.Minute)}
	store.PutArm(context.Background(), pxe.DefaultSite, arm, "")
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(data), "bb:01", "bb:02", 1)), 0o600)
	}
	_, errOut, code = e.run("", false, "config", "pxe", "edit", "web-07")
	if code != 3 || !strings.Contains(errOut, "web-07 has an armed rescue at site default (a7f3c2e1, by oleg@laptop); disarm it before its site or MACs change (ump pxe disarm web-07)") || readPXE(t) != before {
		t.Errorf("a pending arm: code %d, %q", code, errOut)
	}
	// nor in the whole file, by a hand edit or apply
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(data), "bb:01", "bb:02", 1)), 0o600)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "edit"); code != 3 || !strings.Contains(errOut, "disarm it before its site or MACs change") || readPXE(t) != before {
		t.Errorf("a pending arm, the whole file: code %d, %q", code, errOut)
	}
	writePXE(t, strings.Replace(before, "bb:01", "bb:02", 1))
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 3 || !strings.Contains(errOut, "disarm it before its site or MACs change") || siteDoc(t, pxe.DefaultSite).Machine("web-07").MACs[0] != "0c:c4:7a:aa:bb:01" {
		t.Errorf("a pending arm, apply: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "create", "db-01", "--mac", "0c:c4:7a:aa:bb:10"); code != 3 || !strings.Contains(errOut, "nothing was saved: web-07 has an armed rescue") {
		t.Errorf("a pending arm, a later save: code %d, %q", code, errOut)
	}
	writePXE(t, before)
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(string(data)+"  note: rack 4\n"), 0o600)
	}
	if out, errOut, code := e.run("", false, "config", "pxe", "edit", "web-07"); code != 0 || !strings.Contains(readPXE(t), "note: rack 4") {
		t.Errorf("a change beside the arm: code %d, %q %q", code, out, errOut)
	}
	// an expired arm holds nothing
	e.now = e.now.Add(time.Hour)
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(data), "bb:01", "bb:02", 1)), 0o600)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "edit", "web-07"); code != 0 || !strings.Contains(readPXE(t), "bb:02") {
		t.Errorf("an expired arm: code %d, %q", code, errOut)
	}
	// the whole file, not valid: kept, nothing applied
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("machines:\n  web-07:\n    macs: x\n"), 0o600)
	}
	hash := siteDoc(t, pxe.DefaultSite).Hash
	text := readPXE(t)
	if _, errOut, code := e.run("", false, "config", "pxe", "edit"); code != 2 || !strings.Contains(errOut, "pxe.yaml is not valid: line 3: machines.web-07: macs is no key of a machine") ||
		!strings.Contains(errOut, "nothing was saved; your edit is in ") || siteDoc(t, pxe.DefaultSite).Hash != hash || readPXE(t) != text {
		t.Errorf("an invalid file: code %d, %q", code, errOut)
	}
	// and put right
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:03\n"), 0o600)
	}
	if out, errOut, code := e.run("", false, "config", "pxe", "edit"); code != 0 || !strings.Contains(out, "1 machine, all valid.") {
		t.Errorf("put right: code %d, %q %q", code, out, errOut)
	}
}

func TestConfigPXECreateInEditor(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "sites:\n  fra1: {}\nmachines: {}\n")
	e.editorHook = func(path string) error { return nil }
	out, errOut, code := e.run("", true, "config", "pxe", "create", "lab-1")
	if code != 0 || !strings.Contains(out, "No machine lab-1 created.\n") || strings.Contains(readPXE(t), "lab-1") {
		t.Fatalf("unchanged: code %d, %q %q", code, out, errOut)
	}
	e.editorHook = func(path string) error {
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "# The new machine lab-1 for ") || !strings.Contains(string(data), "site: fra1") {
			t.Errorf("the new entry:\n%s", data)
		}
		return os.WriteFile(path, []byte(strings.Replace(string(data), `mac: ""`, "mac: 52:54:00:12:34:56", 1)), 0o600)
	}
	out, errOut, code = e.run("", true, "config", "pxe", "create", "lab-1")
	if code != 0 || !strings.Contains(out, "Saved the machine lab-1 in ") || siteDoc(t, "fra1").Machine("lab-1") == nil {
		t.Fatalf("created: code %d, %q %q", code, out, errOut)
	}
	if text := readPXE(t); !strings.Contains(text, "machines:\n  lab-1:\n") {
		t.Errorf("pxe.yaml, the machines in block style:\n%s", text)
	}
}

func TestConfigPXEDelete(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n  db-01:\n    mac: 0c:c4:7a:aa:bb:10\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	e.writeState("servers:\n  pxe/web-07:\n    installed: {os: ubuntu-26.04, profile: web, hostname: web-07, at: 2026-10-01T08:00:00Z}\n    private_networks: [{network: default, cidr: 10.0.0.0/24, address: 10.0.0.17}]\n  fake/1:\n    note: kept\n")
	ctx := context.Background()
	store := pxeStoreOf()
	store.PutArm(ctx, pxe.DefaultSite, &pxe.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: pxe.ModeRescue, Created: e.now, Until: e.now.Add(time.Hour)}, "")
	store.Files.Write(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"), []byte("log\n"), "")
	text := readPXE(t)

	_, errOut, code := e.run("", false, "config", "pxe", "delete", "web-07")
	if code != 3 || !strings.Contains(errOut, "needs confirmation") || readPXE(t) != text {
		t.Fatalf("without --yes: code %d, %q", code, errOut)
	}
	out, errOut, code := e.run("y\n", true, "config", "pxe", "delete", "web-07")
	if code != 0 {
		t.Fatalf("delete: code %d, %q", code, errOut)
	}
	if !strings.Contains(errOut, "Delete the machine web-07 from ") || !strings.Contains(errOut, "It leaves the fleet with its installation record and its private address 10.0.0.17 in default. Its armed rescue (a7f3c2e1) goes with it.") {
		t.Errorf("the question: %q", errOut)
	}
	for _, want := range []string{"Deleted the machine web-07 from ", "Forgot pxe/web-07 in the state.\n", "Applied site default: 1 machine"} {
		if !strings.Contains(out, want) {
			t.Errorf("delete says %q, not %q", out, want)
		}
	}
	if strings.Contains(readPXE(t), "web-07") || !strings.Contains(readPXE(t), "db-01") {
		t.Errorf("pxe.yaml:\n%s", readPXE(t))
	}
	if st := e.readState(); strings.Contains(st, "pxe/web-07") || !strings.Contains(st, "fake/1") {
		t.Errorf("state:\n%s", st)
	}
	for _, p := range []string{pxe.ArmPath(pxe.DefaultSite, "web-07"), pxe.LogPath(pxe.DefaultSite, "web-07")} {
		if _, v, _ := store.Files.Read(ctx, p); v != "" {
			t.Errorf("%s stays", p)
		}
	}
	if d := siteDoc(t, pxe.DefaultSite); d.Machine("web-07") != nil || d.Machine("db-01") == nil {
		t.Errorf("site.json: %+v", d)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "delete", "web-07", "--yes"); code != 1 || !strings.Contains(errOut, `no machine "web-07" in `) {
		t.Errorf("again: code %d, %q", code, errOut)
	}

	// an installation that is being written is not deleted under it
	store.PutOutcome(ctx, pxe.DefaultSite, "db-01", &pxe.Outcome{Arm: "x", Phase: pxe.PhaseWriting, Daemon: "pxe1", At: e.now, Deadline: e.now.Add(2 * time.Hour)}, "")
	if _, errOut, code := e.run("", false, "config", "pxe", "delete", "db-01", "--yes"); code != 3 || !strings.Contains(errOut, "db-01 is being installed (since 2026-10-12 09:00 UTC, by default/pxe1); it ends with its report or at 2026-10-12 11:00 UTC") {
		t.Errorf("being installed: code %d, %q", code, errOut)
	}
	e.now = e.now.Add(3 * time.Hour)
	if out, errOut, code := e.run("", false, "config", "pxe", "delete", "db-01", "--yes"); code != 0 || strings.Contains(out, "Forgot") || !strings.Contains(out, "Applied site default: 0 machines") {
		t.Errorf("after the deadline: code %d, %q %q", code, out, errOut)
	}
}

// TestConfigPXEGone: a machine that leaves pxe.yaml with an
// installation record leaves the fleet only when asked: an edit of the
// whole file asks and forgets it, and after a hand edit delete does.
func TestConfigPXEGone(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n  db-01:\n    mac: 0c:c4:7a:aa:bb:10\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	installed := "    installed: {os: ubuntu-26.04, profile: web, hostname: %s, at: 2026-10-01T08:00:00Z}\n    private_networks: [{network: default, cidr: 10.0.0.0/24, address: %s}]\n"
	e.writeState("servers:\n  pxe/web-07:\n" + fmt.Sprintf(installed, "web-07", "10.0.0.17") + "  pxe/db-01:\n" + fmt.Sprintf(installed, "db-01", "10.0.0.18"))
	ctx := context.Background()
	store := pxeStoreOf()
	store.Files.Write(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"), []byte("log\n"), "")
	text := readPXE(t)

	// the whole file without web-07: asked, and needs --yes without a terminal
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("machines:\n  db-01:\n    mac: 0c:c4:7a:aa:bb:10\n"), 0o600)
	}
	_, errOut, code := e.run("", false, "config", "pxe", "edit")
	if code != 3 || !strings.Contains(errOut, "needs confirmation") || !strings.Contains(errOut, "nothing was saved, your edit is in ") || readPXE(t) != text {
		t.Errorf("without --yes: code %d, %q", code, errOut)
	}
	out, errOut, code := e.run("y\n", true, "config", "pxe", "edit")
	if code != 0 || !strings.Contains(errOut, "web-07 leaves the fleet with its installation record and its private address 10.0.0.17 in default.") {
		t.Fatalf("edit: code %d, %q %q", code, out, errOut)
	}
	if !strings.Contains(out, "Forgot pxe/web-07 in the state.\n") || strings.Contains(e.readState(), "pxe/web-07") || strings.Contains(readPXE(t), "web-07") {
		t.Errorf("after the edit: %q\n%s", out, e.readState())
	}
	if _, v, _ := store.Files.Read(ctx, pxe.LogPath(pxe.DefaultSite, "web-07")); v != "" {
		t.Error("the log of web-07 stays")
	}

	// a hand edit: apply says so, and delete forgets it
	writePXE(t, "machines: {}\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 || !strings.Contains(errOut, "ump: pxe/db-01 is in the state, but pxe.yaml has no machine db-01: ump config pxe delete db-01 forgets it\n") {
		t.Errorf("apply after a hand edit: code %d, %q", code, errOut)
	}
	out, errOut, code = e.run("y\n", true, "config", "pxe", "delete", "pxe/db-01")
	if code != 0 || !strings.Contains(errOut, "Forget pxe/db-01, which ") || !strings.Contains(errOut, "It leaves the fleet with its installation record and its private address 10.0.0.18 in default.") ||
		!strings.Contains(out, "Forgot pxe/db-01 in the state.\n") || strings.Contains(out, "Deleted the machine") || strings.Contains(e.readState(), "pxe/db-01") {
		t.Errorf("delete after a hand edit: code %d, %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "delete", "db-01", "--yes"); code != 1 || !strings.Contains(errOut, `no machine "db-01" in `) {
		t.Errorf("again: code %d, %q", code, errOut)
	}
}

func TestConfigPXEBackend(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	remote := filepath.Join(t.TempDir(), "fleet")
	t.Setenv("UMP_STATE", "file://"+remote)
	if _, errOut, code := e.run("", false, "config", "pxe", "create", "web-07", "--mac", "0c:c4:7a:aa:bb:01"); code != 0 {
		t.Fatalf("create: code %d, %q", code, errOut)
	}
	// pxe.yaml goes with the home, site.json straight into the backend
	if data, err := os.ReadFile(filepath.Join(remote, "pxe.yaml")); err != nil || !strings.Contains(string(data), "web-07") {
		t.Errorf("pxe.yaml in the backend: %v %q", err, data)
	}
	d, err := pxe.Store{Files: &backend.Local{Dir: remote}}.Site(context.Background(), pxe.DefaultSite)
	if err != nil || d == nil || d.Machine("web-07") == nil {
		t.Errorf("site.json in the backend: %+v %v", d, err)
	}
	if _, err := os.Stat(filepath.Join(pxeHome(), ".pxe")); err == nil {
		t.Error(".pxe/ in the local home")
	}
	mirrors, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "homes", "*", ".pxe"))
	if len(mirrors) != 0 {
		t.Errorf(".pxe/ in the mirror: %v", mirrors)
	}
	if out, errOut, code := e.run("", false, "config", "pxe", "list"); code != 0 || errOut != "" || !strings.Contains(out, "web-07") {
		t.Errorf("list: code %d, %q %q", code, out, errOut)
	}
}
