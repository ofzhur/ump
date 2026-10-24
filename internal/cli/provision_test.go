package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

func TestGroupFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	got, err := groupFlags{"web", "base,web, monitoring", "common", "_x"}.list(cmd)
	if err != nil || strings.Join(got, ",") != "web,base,monitoring,_x" {
		t.Errorf("list = %v, %v", got, err)
	}
	if got, err := (groupFlags{}).list(cmd); err != nil || got != nil {
		t.Errorf("none = %v, %v", got, err)
	}
	for _, bad := range []string{"Web", "web-01", "1web", "a b", "base.x"} {
		if _, err := (groupFlags{"ok," + bad}).list(cmd); err == nil || exitCodeFor(err) != exitUsage || !strings.Contains(err.Error(), "a group name is lower-case letters, digits and '_', not starting with a digit") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, own := range []string{"all", "ungrouped", "localhost", "_meta"} {
		if _, err := (groupFlags{own}).list(cmd); err == nil || exitCodeFor(err) != exitUsage || !strings.Contains(err.Error(), "group \""+own+"\": all, ungrouped, localhost and _meta are Ansible's own names") {
			t.Errorf("%q: %v", own, err)
		}
	}
}

func TestRunList(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "playbooks"), 0o755)
	os.WriteFile(filepath.Join(dir, "playbooks", "web.yml"), []byte("- hosts: web\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "playbooks", "db.yaml"), []byte("- hosts: db\n"), 0o644)
	rl := (&infraRepo{dir: dir}).runList([]string{"web", "db"})
	if strings.Join(rl.groups, ",") != "common,web,db" || strings.Join(rl.playbooks, ",") != "ofzhur.ansible.common,playbooks/web.yml,ofzhur.ansible.db" {
		t.Errorf("run list %+v", rl)
	}
	if rl.String() != "common, web, db (ofzhur.ansible.common, playbooks/web.yml, ofzhur.ansible.db)" {
		t.Errorf("String() = %q", rl.String())
	}
	// a repository playbook for common wins over the collection's
	os.WriteFile(filepath.Join(dir, "playbooks", "common.yml"), []byte("- hosts: common\n"), 0o644)
	if rl := (&infraRepo{dir: dir}).runList(nil); strings.Join(rl.playbooks, ",") != "playbooks/common.yml" {
		t.Errorf("local common: %+v", rl)
	}
}

func TestRenderHostFile(t *testing.T) {
	m := provider.Machine{Provider: "hetzner-cloud", Account: "main", ID: "123", Name: "web-01", IPv4: "203.0.113.10", IPv6: "2001:db8::/64",
		Location: "fsn1", Product: "cx22", Attributes: map[string]string{"owner": "alice", "on_call": "yes", "note": "a: b", "cc": "4711"}}
	rec := state.Install{OS: "ubuntu-26.04", Profile: "web", At: time.Date(2026, 10, 12, 11, 58, 30, 0, time.FixedZone("CEST", 7200))}
	data, err := renderHostFile("web-01", []string{"common", "web", "db"}, hostVarsFor(m, rec, "ump"))
	if err != nil {
		t.Fatal(err)
	}
	want := `# web-01: written by ump (ump server reinstall, ump server provision).
# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.
plugin: ofzhur.ansible.host
groups: [common, web, db]
vars:
  ansible_host: 203.0.113.10
  ansible_user: ump
  ump_name: web-01
  ump_ref: hetzner-cloud/123
  ump_provider: hetzner-cloud
  ump_account: main
  ump_location: fsn1
  ump_product: cx22
  ump_os: ubuntu-26.04
  ump_profile: web
  ump_installed_at: "2026-10-12T09:58:30Z"
  ump_attributes:
    cc: "4711"
    note: 'a: b'
    on_call: "yes"
    owner: alice
`
	if string(data) != want {
		t.Errorf("host file:\n%s\nwant:\n%s", data, want)
	}
	// empty values are left out; IPv6 when there is no IPv4
	data, err = renderHostFile("rack-07", []string{"common"}, hostVarsFor(provider.Machine{Provider: "manual", ID: "rack-07", Name: "rack-07", IPv6: "2001:db8::7"}, state.Install{}, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ansible_host: 2001:db8::7\n  ansible_user: ops\n  ump_name: rack-07\n  ump_ref: manual/rack-07\n  ump_provider: manual\n") {
		t.Errorf("minimal host file:\n%s", data)
	}
}

func TestWriteHostFile(t *testing.T) {
	repo := &infraRepo{dir: t.TempDir()}
	hosts := filepath.Join(repo.dir, "inventory", "hosts")
	file := func(ref string) []byte {
		return []byte("plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ump_ref: " + ref + "\n")
	}
	rl := repo.runList([]string{"web"})

	// no file yet: nothing in the way; the directory is created
	if err := repo.checkHost("web-01", "fake/1", rl, "give another --hostname"); err != nil {
		t.Errorf("no file: %v", err)
	}
	written, removed, err := repo.writeHostFile("web-01", "fake/1", file("fake/1"))
	if err != nil || written != "inventory/hosts/web-01.yml" || len(removed) != 0 {
		t.Fatalf("first write: %q %v %v", written, removed, err)
	}
	// the server's own file is no conflict; another server's is
	if err := repo.checkHost("web-01", "fake/1", rl, "give another --hostname"); err != nil {
		t.Errorf("own file: %v", err)
	}
	err = repo.checkHost("web-01", "fake/2", rl, "give another --hostname")
	if err == nil || exitCodeFor(err) != exitUsage || err.Error() != "inventory/hosts/web-01.yml is the host file of fake/1 (its ump_ref); give another --hostname, or remove the file if that server is gone" {
		t.Errorf("conflict: %v", err)
	}
	os.WriteFile(filepath.Join(hosts, "db-01.yaml"), file("fake/2"), 0o644)
	if err := repo.checkHost("db-01", "fake/3", rl, "give another --hostname"); err == nil || !strings.Contains(err.Error(), "inventory/hosts/db-01.yaml is the host file of fake/2") {
		t.Errorf("conflict with .yaml: %v", err)
	}
	// a hostname that is a group of the run list, or Ansible's own
	for _, name := range []string{"web", "common"} {
		if err := repo.checkHost(name, "fake/1", rl, "give another --hostname"); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "the hostname "+name+" is also a group of its run list: a host and a group with the same name make the group's plays match only the host; give another --hostname" {
			t.Errorf("hostname %s: %v", name, err)
		}
	}
	if err := repo.checkHost("all", "fake/1", rl, "give another --hostname"); err == nil || !strings.Contains(err.Error(), "the hostname all is one of Ansible's own groups: a host and a group") {
		t.Errorf("hostname all: %v", err)
	}
	// a host of the controller's own names would take its plays
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if err := repo.checkHost(name, "fake/1", rl, "give another --hostname"); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "the hostname "+name+" is one of Ansible's own names: plays and tasks for localhost would run on the server; give another --hostname" {
			t.Errorf("hostname %s: %v", name, err)
		}
	}
	// a file of the hostname without ump_ref (by hand, or not even YAML)
	// is neither overwritten nor joined by ump's
	for _, f := range []struct{ name, content string }{
		{"hand-01.yml", "plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ansible_host: 192.0.2.1\n"},
		{"hand-01.yaml", "hand-01:\n  ansible_host: 192.0.2.1\n"},
		{"hand-01.yml", "{not yaml"},
	} {
		os.WriteFile(filepath.Join(hosts, f.name), []byte(f.content), 0o644)
		want := "inventory/hosts/" + f.name + " has no ump_ref: written by hand or by another tool; remove it, ump writes inventory/hosts/hand-01.yml"
		if err := repo.checkHost("hand-01", "fake/1", rl, "give another --hostname"); err == nil || exitCodeFor(err) != exitUsage || err.Error() != want {
			t.Errorf("%s: %v", f.name, err)
		}
		if _, _, err := repo.writeHostFile("hand-01", "fake/1", file("fake/1")); err == nil || err.Error() != want {
			t.Errorf("%s, written: %v", f.name, err)
		}
		if data, _ := os.ReadFile(filepath.Join(hosts, f.name)); string(data) != f.content {
			t.Errorf("%s overwritten: %q", f.name, data)
		}
		os.Remove(filepath.Join(hosts, f.name))
	}
	if _, err := os.Stat(filepath.Join(hosts, "hand-01.yml")); err == nil {
		t.Errorf("hand-01.yml written")
	}
	// the file of another server is not overwritten either
	if _, _, err := repo.writeHostFile("web-01", "fake/2", file("fake/2")); err == nil || !strings.Contains(err.Error(), "is the host file of fake/1") {
		t.Errorf("written over another server's: %v", err)
	}

	// a new hostname: the old file of the same server goes, others stay
	os.WriteFile(filepath.Join(hosts, "old-name.yaml"), file("fake/1"), 0o644)
	os.WriteFile(filepath.Join(hosts, ".hidden.yml"), file("fake/1"), 0o644)
	os.WriteFile(filepath.Join(hosts, "notes.txt"), file("fake/1"), 0o644)
	written, removed, err = repo.writeHostFile("web-02", "fake/1", file("fake/1"))
	if err != nil || written != "inventory/hosts/web-02.yml" || strings.Join(removed, ",") != "inventory/hosts/old-name.yaml,inventory/hosts/web-01.yml" {
		t.Errorf("rename: %q %v %v", written, removed, err)
	}
	var names []string
	entries, _ := os.ReadDir(hosts)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ".hidden.yml,db-01.yaml,notes.txt,web-02.yml" {
		t.Errorf("files after the rename: %v", names)
	}
	if data, _ := os.ReadFile(filepath.Join(hosts, "web-02.yml")); !bytes.Equal(data, file("fake/1")) {
		t.Errorf("content: %s", data)
	}
}

func TestAnsibleEnv(t *testing.T) {
	repo := &infraRepo{dir: t.TempDir()}
	cfg := filepath.Join(repo.dir, "ansible.cfg")
	os.WriteFile(cfg, []byte("[ssh_connection]\nssh_args = -o StrictHostKeyChecking=accept-new\n"), 0o644)
	for _, v := range []string{"ANSIBLE_SSH_COMMON_ARGS", "ANSIBLE_SSH_EXTRA_ARGS", "ANSIBLE_SFTP_EXTRA_ARGS", "ANSIBLE_SCP_EXTRA_ARGS"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	identities := "ANSIBLE_SSH_EXTRA_ARGS=-o IdentitiesOnly=yes|ANSIBLE_SFTP_EXTRA_ARGS=-o IdentitiesOnly=yes|ANSIBLE_SCP_EXTRA_ARGS=-o IdentitiesOnly=yes"
	if env := repo.ansibleEnv(); strings.Join(env, "|") != "ANSIBLE_CONFIG="+cfg+"|ANSIBLE_SSH_COMMON_ARGS=-o StrictHostKeyChecking=accept-new|"+identities {
		t.Errorf("env: %v", env)
	}
	// arguments of the repository's own, or of the environment, stay
	os.WriteFile(cfg, []byte("[ssh_connection]\n  ssh_common_args = -o ProxyJump=bastion\nscp_extra_args: -l 8192\n"), 0o644)
	if env := repo.ansibleEnv(); strings.Join(env, "|") != "ANSIBLE_CONFIG="+cfg+"|ANSIBLE_SSH_EXTRA_ARGS=-o IdentitiesOnly=yes|ANSIBLE_SFTP_EXTRA_ARGS=-o IdentitiesOnly=yes" {
		t.Errorf("env with ssh_common_args and scp_extra_args in ansible.cfg: %v", env)
	}
	os.WriteFile(cfg, []byte("[defaults]\n"), 0o644)
	t.Setenv("ANSIBLE_SSH_COMMON_ARGS", "-o ProxyJump=bastion")
	t.Setenv("ANSIBLE_SSH_EXTRA_ARGS", "-o IdentityAgent=none")
	if env := repo.ansibleEnv(); strings.Join(env, "|") != "ANSIBLE_CONFIG="+cfg+"|ANSIBLE_SFTP_EXTRA_ARGS=-o IdentitiesOnly=yes|ANSIBLE_SCP_EXTRA_ARGS=-o IdentitiesOnly=yes" {
		t.Errorf("env with ANSIBLE_SSH_COMMON_ARGS and ANSIBLE_SSH_EXTRA_ARGS set: %v", env)
	}
}

// TestRunAnsible runs the real runner against a script that stands in
// for ansible-playbook: it runs in the directory, with the environment
// added, its output where it was sent and its exit status.
func TestRunAnsible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stands in for ansible-playbook")
	}
	bin, dir := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\necho \"args $* in $(pwd) with $ANSIBLE_CONFIG\"\necho oops >&2\nexit 4\n"
	if err := os.WriteFile(filepath.Join(bin, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errb bytes.Buffer
	code, err := runAnsible(context.Background(), dir, "ansible-playbook", []string{"ofzhur.ansible.common", "--limit", "web-01"}, []string{"ANSIBLE_CONFIG=/x/ansible.cfg"}, nil, &out, &errb)
	if err != nil || code != 4 || out.String() != "args ofzhur.ansible.common --limit web-01 in "+dir+" with /x/ansible.cfg\n" || errb.String() != "oops\n" {
		t.Errorf("code %d, %v, stdout %q, stderr %q", code, err, out.String(), errb.String())
	}
	t.Setenv("PATH", t.TempDir())
	for _, prog := range []string{"ansible-playbook", "ansible-inventory"} {
		if _, err := runAnsible(context.Background(), dir, prog, nil, nil, nil, &out, &errb); err == nil || err.Error() != "no "+prog+" found in PATH" {
			t.Errorf("without %s: %v", prog, err)
		}
	}
}

// noTouch says what ump did to servers and providers: nothing, when the
// layout or the run list stopped it.
func (e *testEnv) noTouch(t *testing.T, what string) {
	t.Helper()
	if len(e.remoteCalls) != 0 || len(e.sshCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("%s: touched the server: remote %v, ssh %v, provider %v", what, e.remoteCalls, e.sshCalls, fakePower.actions)
	}
	if e.hostFile("web-01.yml") != "" {
		t.Errorf("%s: host file written", what)
	}
}

func TestProvisionNeedsTheLayout(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	listed := false
	listGate = func(string, bool) { listed = true }
	defer func() { listGate = nil }()

	// no ansible.cfg, no inventory/, no ansible-playbook: refused before
	// anything is listed, asked or touched
	os.Remove(filepath.Join(e.repo, "ansible.cfg"))
	os.Remove(filepath.Join(e.repo, "inventory"))
	e.noAnsible = true
	resetFakePower()
	for _, args := range [][]string{
		{"server", "reinstall", "web-01", "--account", "one"},
		{"server", "reinstall", "web-01", "--yes", "--dry-run", "--account", "one"},
		{"server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one"},
	} {
		_, errOut, code := e.run("", false, args...)
		if code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": no ansible.cfg, no inventory/, no ansible-playbook in PATH; run ump from the root of the infrastructure repository, or give --no-provision\n") {
			t.Errorf("%v: code %d, %q", args, code, errOut)
		}
	}
	_, errOut, code := e.run("", false, "server", "provision", "web-01", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: cannot provision from "+e.repo+": no ansible.cfg, no inventory/, no ansible-playbook in PATH; run ump from the root of the infrastructure repository\n") {
		t.Errorf("provision: code %d, %q", code, errOut)
	}
	// one missing piece is named alone; without ansible-playbook, the
	// layout is right where it is
	os.WriteFile(filepath.Join(e.repo, "ansible.cfg"), nil, 0o644)
	os.Mkdir(filepath.Join(e.repo, "inventory"), 0o755)
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": no ansible-playbook in PATH; install ansible-core, or give --no-provision\n") {
		t.Errorf("no ansible-playbook: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "provision", "web-01", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": no ansible-playbook in PATH; install ansible-core\n") {
		t.Errorf("provision, no ansible-playbook: code %d, %q", code, errOut)
	}
	e.noAnsible = false
	// Ansible takes no ansible.cfg from a world-writable directory, and
	// ANSIBLE_CONFIG must not get past that
	if runtime.GOOS != "windows" {
		os.Chmod(e.repo, 0o777)
		for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes"}, {"server", "provision", "web-01"}} {
			if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": it is a world-writable directory, which Ansible ignores as an ansible.cfg source (anyone could have put one there); chmod o-w "+e.repo+"\n") {
				t.Errorf("%v, world-writable: code %d, %q", args, code, errOut)
			}
		}
		os.Chmod(e.repo, 0o755)
	}
	os.Remove(filepath.Join(e.repo, "ansible.cfg"))
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 2 || !strings.Contains(errOut, ": no ansible.cfg; run ump") {
		t.Errorf("no ansible.cfg: code %d, %q", code, errOut)
	}
	e.noTouch(t, "no layout")
	if listed || len(e.ansibleCalls) != 0 {
		t.Errorf("listed %v, ansible %v", listed, e.ansibleCalls)
	}

	// --no-provision needs no layout: the server is installed, and is in
	// no inventory
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "web-01 is up:") || !strings.Contains(errOut, "No host file for web-01 (no Ansible layout here): ump server provision web-01, run from the infrastructure repository, writes it; where ump is the inventory, web-01 is in it from its record on.\n") {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if len(e.ansibleCalls) != 0 || e.hostFile("web-01.yml") != "" {
		t.Errorf("--no-provision: ansible %v, host file %q", e.ansibleCalls, e.hostFile("web-01.yml"))
	}
}

func TestReinstallProvisions(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.MkdirAll(filepath.Join(e.repo, "playbooks"), 0o755)
	os.WriteFile(filepath.Join(e.repo, "playbooks", "web.yml"), []byte("- hosts: web\n"), 0o644)
	if out, _, code := e.run("", false, "server", "set", "web-01", "--attr", "owner=alice", "--account", "one"); code != 0 {
		t.Fatalf("server set: %s", out)
	}

	// the run list is checked before anything happens, shown with the
	// plan, and run once the new system is up and recorded
	resetFakePower()
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--group", "db,web, common", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("reinstall: code %d, stderr %q\n%s", code, errOut, out)
	}
	if len(e.ansibleCalls) != 3 {
		t.Fatalf("ansible runs: %v", e.ansibleCalls)
	}
	check, inv, run := e.ansibleCalls[0], e.ansibleCalls[1], e.ansibleCalls[2]
	if check.dir != e.repo || check.prog != "ansible-playbook" || strings.Join(check.args, " ") != "--syntax-check ofzhur.ansible.common playbooks/web.yml ofzhur.ansible.db" {
		t.Errorf("syntax check: %+v", check)
	}
	if inv.dir != e.repo || inv.prog != "ansible-inventory" || strings.Join(inv.args, " ") != "--list" || !slices.Equal(inv.env, check.env) {
		t.Errorf("inventory: %+v", inv)
	}
	keyPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")
	if run.dir != e.repo || run.prog != "ansible-playbook" || strings.Join(run.args, " ") != "ofzhur.ansible.common playbooks/web.yml ofzhur.ansible.db --limit web-01 --private-key "+keyPath {
		t.Errorf("run: %+v", run)
	}
	for _, want := range []string{"ANSIBLE_CONFIG=" + filepath.Join(e.repo, "ansible.cfg"), "ANSIBLE_SSH_COMMON_ARGS=-o StrictHostKeyChecking=accept-new", "ANSIBLE_SSH_EXTRA_ARGS=-o IdentitiesOnly=yes"} {
		if !slices.Contains(run.env, want) {
			t.Errorf("run env lacks %s: %v", want, run.env)
		}
	}
	plan := strings.Index(errOut, "Run list after the first boot: common, web, db (ofzhur.ansible.common, playbooks/web.yml, ofzhur.ansible.db).\n")
	up := strings.Index(errOut, "web-01 is up:")
	wrote := strings.Index(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, web, db.\n")
	prov := strings.Index(errOut, "Provisioning web-01: common, web, db (ofzhur.ansible.common, playbooks/web.yml, ofzhur.ansible.db) ...\nPLAY RECAP")
	done := strings.Index(errOut, "PLAY RECAP *********\nweb-01 is provisioned: common, web, db.\n")
	if plan < 0 || up < plan || wrote < up || prov < wrote || done < prov {
		t.Errorf("progress (%d %d %d %d %d):\n%s", plan, up, wrote, prov, done, errOut)
	}
	for _, want := range []string{`"installed_at": "2026-10-12T09:00:00Z"`, `"groups": [
    "common",
    "web",
    "db"
  ],`, `"run_list": [
    "common",
    "web",
    "db"
  ],`, `"playbooks": [
    "ofzhur.ansible.common",
    "playbooks/web.yml",
    "ofzhur.ansible.db"
  ],`, `"provisioned": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s:\n%s", want, out)
		}
	}
	host := e.hostFile("web-01.yml")
	if !strings.HasPrefix(host, "# web-01: written by ump (ump server reinstall, ump server provision).\n# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.\nplugin: ofzhur.ansible.host\ngroups: [common, web, db]\nvars:\n  ansible_host: 203.0.113.10\n  ansible_user: ump\n  ump_name: web-01\n  ump_ref: fake/1\n  ump_provider: fake\n  ump_account: one\n  ump_location: eu-1\n  ump_product: m1\n  ump_os: ubuntu-26.04\n  ump_profile: ubuntu-26.04\n  ump_installed_at: \"2026-10-12T09:00:00Z\"\n  ump_attributes:\n    owner: alice\n") {
		t.Errorf("host file:\n%s", host)
	}
	if st := e.readState(); !strings.Contains(st, "    groups:\n      - web\n      - db\n") {
		t.Errorf("state:\n%s", st)
	}
	// listed as in the inventory: common first; the servers ump did not
	// install have none
	out, _, code = e.run("", false, "server", "list", "--account", "one")
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "NAME", "IPV4", "LOCATION", "STATUS", "OS", "GROUPS", "PRICE"},
		{"one", "fake", "db-01", "203.0.113.11", "eu-1", "off", "<none>", "<none>", "<none>"},
		{"one", "fake", "web-01", "203.0.113.10", "eu-1", "running", "ubuntu-26.04", "common,web,db", "<none>"},
	})
	if out, _, code := e.run("", false, "server", "list", "--account", "one", "-o", "json"); code != 0 || strings.Count(out, `"groups"`) != 1 || !strings.Contains(out, `"groups": [
      "common",
      "web",
      "db"
    ],
    "profile": "ubuntu-26.04",
    "layout": "lvm-linear"
  }`) || strings.Count(out, `"profile"`) != 1 {
		t.Errorf("list -o json: code %d\n%s", code, out)
	}
	// -o wide adds the profile and the layout of the installation
	out, _, _ = e.run("", false, "server", "list", "--account", "one", "-o", "wide")
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS", "OS", "GROUPS", "PROFILE", "LAYOUT", "IPV6", "PRODUCT", "KIND", "PRICE", "ATTRIBUTES"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off", "<none>", "<none>", "<none>", "<none>", "<none>", "m2", "cloud", "<none>", "<none>"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running", "ubuntu-26.04", "common,web,db", "ubuntu-26.04", "lvm-linear", "2001:db8::/64", "m1", "cloud", "<none>", "owner=alice"},
	})
	if out, _, code := e.run("", false, "server", "show", "web-01", "--account", "one"); code != 0 || !strings.Contains(out, "Groups:") || !strings.Contains(out, "common, web, db") {
		t.Errorf("show: code %d\n%s", code, out)
	}
	// the result as YAML: the groups once, those of this installation
	if out, _, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "-o", "yaml"); code != 0 || !strings.Contains(out, "groups:\n  - common\nprofile:") || !strings.Contains(out, "run_list:\n  - common\n") || !strings.Contains(out, "provisioned: true\n") {
		t.Errorf("-o yaml: code %d\n%s", code, out)
	}

	// a dry run checks and shows the run list and writes nothing
	os.RemoveAll(filepath.Join(e.repo, "inventory", "hosts"))
	e.ansibleCalls = nil
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--dry-run", "--group", "cache", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "Run list after the first boot: common, cache (ofzhur.ansible.common, ofzhur.ansible.cache).\n") || !strings.Contains(out, `"provisioned": false`) || !strings.Contains(out, `"groups": [
    "common",
    "cache"
  ],`) || strings.Contains(out, "installed_at") {
		t.Errorf("dry run: code %d, %q\n%s", code, errOut, out)
	}
	if len(e.ansibleCalls) != 2 || e.ansibleCalls[0].args[0] != "--syntax-check" || e.ansibleCalls[1].prog != "ansible-inventory" || e.hostFile("web-01.yml") != "" || strings.Contains(e.readState(), "cache") {
		t.Errorf("dry run: ansible %v, host file %q\n%s", e.ansibleCalls, e.hostFile("web-01.yml"), e.readState())
	}

	// --no-provision with the layout: the names checked, in the
	// inventory, nothing runs
	e.ansibleCalls = nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--group", "web", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, web.\nNot provisioned (--no-provision); ump server provision web-01 runs its run list.\n") || strings.Contains(errOut, "Run list") {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if len(e.ansibleCalls) != 1 || e.ansibleCalls[0].prog != "ansible-inventory" || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") {
		t.Errorf("--no-provision: ansible %v, host file %q", e.ansibleCalls, e.hostFile("web-01.yml"))
	}

	// another hostname: the server's old host file goes
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-01.example.net", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Removed inventory/hosts/web-01.yml: fake/1 has the hostname web-01.example.net now.\n") || e.hostFile("web-01.yml") != "" || !strings.Contains(e.hostFile("web-01.example.net.yml"), "ump_ref: fake/1\n") {
		t.Errorf("new hostname: code %d, %q", code, errOut)
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || !strings.HasSuffix(runs[0], "ofzhur.ansible.common --limit web-01.example.net --private-key "+keyPath) {
		t.Errorf("new hostname, runs: %v", runs)
	}
}

func TestReinstallProvisionFailures(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()

	// a run list that does not pass the syntax check stops ump first
	e.syntaxExit = 4
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "nope", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "[ERROR]: the playbook: ofzhur.ansible.nope could not be found\nump: the run list common, nope (ofzhur.ansible.common, ofzhur.ansible.nope) does not pass ansible-playbook --syntax-check in "+e.repo+" (exit status 4, see above); nothing was changed\n") {
		t.Errorf("syntax check: code %d, %q", code, errOut)
	}
	e.noTouch(t, "syntax check")
	e.syntaxExit = 0

	// the host file of another server is in the way, or the hostname is a group
	hosts := filepath.Join(e.repo, "inventory", "hosts")
	os.MkdirAll(hosts, 0o755)
	os.WriteFile(filepath.Join(hosts, "web-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/2\n"), 0o644)
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: inventory/hosts/web-01.yml is the host file of fake/2 (its ump_ref); give another --hostname, or remove the file if that server is gone\n") {
		t.Errorf("conflict: code %d, %q", code, errOut)
	}
	if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("conflict: remote %v, provider %v", e.remoteCalls, fakePower.actions)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"); code != 2 || !strings.Contains(errOut, "is the host file of fake/2") {
		t.Errorf("conflict with --no-provision: code %d, %q", code, errOut)
	}
	os.Remove(filepath.Join(hosts, "web-01.yml"))
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web", "--group", "web", "--account", "one"); code != 2 || !strings.Contains(errOut, "the hostname web is also a group of its run list") {
		t.Errorf("hostname is a group: code %d, %q", code, errOut)
	}
	if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("hostname is a group: remote %v, provider %v", e.remoteCalls, fakePower.actions)
	}
	// a host file of the hostname that ump did not write is in the way
	os.WriteFile(filepath.Join(hosts, "web-01.yaml"), []byte("plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ansible_host: 192.0.2.1\n"), 0o644)
	for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--no-provision"}} {
		if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || !strings.HasSuffix(errOut, "ump: inventory/hosts/web-01.yaml has no ump_ref: written by hand or by another tool; remove it, ump writes inventory/hosts/web-01.yml\n") {
			t.Errorf("%v, hand-written host file: code %d, %q", args, code, errOut)
		}
	}
	e.noTouch(t, "hand-written host file")
	os.Remove(filepath.Join(hosts, "web-01.yaml"))

	// the inventory has the hostname as a group, or a host of the name of
	// a group of the run list: refused before anything happens
	e.inventory = `{"_meta": {"hostvars": {"db": {"ansible_host": "192.0.2.5"}}}, "all": {"children": ["ungrouped", "common", "prod"]}, "prod": {"children": ["web-01"]}, "ungrouped": {"hosts": ["db"]}}`
	for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--no-provision"}} {
		if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || !strings.HasSuffix(errOut, "ump: the hostname web-01 is also a group of the inventory: a host and a group with the same name make the group's plays match only the host; give another --hostname\n") {
			t.Errorf("%v, hostname is a group of the inventory: code %d, %q", args, code, errOut)
		}
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-02", "--group", "db", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: the group db of the run list is also a host of the inventory: a host and a group with the same name make the group's plays match only the host; rename the host or use another group\n") {
		t.Errorf("group is a host of the inventory: code %d, %q", code, errOut)
	}
	e.noTouch(t, "inventory names")
	// --no-provision where the names cannot be checked: installed, and
	// in no inventory
	e.noAnsible, e.ansibleCalls = true, nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "web-01 is up:") || !strings.HasSuffix(errOut, "No host file for web-01 (no ansible-inventory in PATH to check the names): ump server provision web-01, run from the infrastructure repository, writes it; where ump is the inventory, web-01 is in it from its record on.\n") {
		t.Errorf("--no-provision without ansible-inventory: code %d, %q", code, errOut)
	}
	e.noAnsible = false
	if runtime.GOOS != "windows" {
		os.Chmod(e.repo, 0o777)
		resetFakePower()
		_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
		if code != 0 || !strings.HasSuffix(errOut, "No host file for web-01 ("+e.repo+" is world-writable, Ansible takes no ansible.cfg from it): ump server provision web-01, run from the infrastructure repository, writes it; where ump is the inventory, web-01 is in it from its record on.\n") {
			t.Errorf("--no-provision, world-writable: code %d, %q", code, errOut)
		}
		os.Chmod(e.repo, 0o755)
	}
	if len(e.ansibleCalls) != 0 || e.hostFile("web-01.yml") != "" {
		t.Errorf("--no-provision unchecked: ansible %v, host file %q", e.ansibleCalls, e.hostFile("web-01.yml"))
	}
	e.remoteCalls = nil
	resetFakePower()
	e.inventory, e.inventoryExit = "", 1
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "[ERROR]: inventory/groups.yml: not a mapping\nump: the inventory of "+e.repo+" does not pass ansible-inventory --list (exit status 1, see above); nothing was changed\n") {
		t.Errorf("broken inventory: code %d, %q", code, errOut)
	}
	e.noTouch(t, "broken inventory")
	e.inventoryExit = 0

	// a run list that fails: exit status 6, the server installed and
	// recorded, in the inventory
	e.ansibleExit = 2
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one", "-o", "json")
	if code != 6 || !strings.Contains(errOut, "web-01 : ok=2 changed=0 unreachable=0 failed=1\nump: provisioning failed: the server is installed and recorded, its run list did not complete (ansible-playbook exit status 2, see above); fix that and run ump server provision web-01\n") {
		t.Errorf("failed run: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, `"provisioned": false`) || !strings.Contains(out, `"rebooted": true`) || !strings.Contains(e.readState(), "    installed:\n") || !strings.Contains(e.readState(), "      - web\n") || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") {
		t.Errorf("failed run: stdout %q, host file %q\n%s", out, e.hostFile("web-01.yml"), e.readState())
	}
}

func TestProvisionCommand(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()

	// a server ump did not install has nothing to go into the inventory
	_, errOut, code := e.run("", false, "server", "provision", "db-01", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "ump: ump did not install db-01: nothing to put into the inventory; ump server reinstall db-01\n") || len(e.ansibleCalls) != 0 {
		t.Errorf("not installed: code %d, %q, ansible %v", code, errOut, e.ansibleCalls)
	}

	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--group", "web", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	// --group adds groups, in order, once; then the run list runs
	e.ansibleCalls, e.remoteCalls = nil, nil
	out, errOut, code := e.run("", false, "server", "provision", "web-01", "--group", "monitoring,web", "--group", "db", "--account", "one")
	if code != 0 || out != "fake/1 (web-01, 203.0.113.10): provisioned, run list common, web, monitoring, db.\n" {
		t.Errorf("provision: code %d, %q\n%s", code, out, errOut)
	}
	keyPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")
	if len(e.ansibleCalls) != 3 || strings.Join(e.ansibleCalls[0].args, " ") != "--syntax-check ofzhur.ansible.common ofzhur.ansible.web ofzhur.ansible.monitoring ofzhur.ansible.db" || e.ansibleCalls[1].prog != "ansible-inventory" || strings.Join(e.ansibleCalls[2].args, " ") != "ofzhur.ansible.common ofzhur.ansible.web ofzhur.ansible.monitoring ofzhur.ansible.db --limit web-01 --private-key "+keyPath {
		t.Errorf("ansible: %+v", e.ansibleCalls)
	}
	if !strings.Contains(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, web, monitoring, db.\nProvisioning web-01: common, web, monitoring, db (") || len(e.remoteCalls) != 0 {
		t.Errorf("provision: stderr %q, remote %v", errOut, e.remoteCalls)
	}
	if !strings.Contains(e.readState(), "    groups:\n      - web\n      - monitoring\n      - db\n") || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web, monitoring, db]\n") || !strings.Contains(e.hostFile("web-01.yml"), "  ansible_user: ump\n") {
		t.Errorf("state\n%s\nhost file\n%s", e.readState(), e.hostFile("web-01.yml"))
	}

	// without --group: the same run list again; a failure is exit status 6
	e.ansibleCalls = nil
	e.ansibleExit = 1
	_, errOut, code = e.run("", false, "server", "provision", "web-01", "--account", "one")
	if code != 6 || !strings.Contains(errOut, "ump: provisioning failed: the run list of web-01 did not complete (ansible-playbook exit status 1, see above); fix that and run ump server provision web-01 again\n") {
		t.Errorf("failed converge: code %d, %q", code, errOut)
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || !strings.HasPrefix(runs[0], "ofzhur.ansible.common ofzhur.ansible.web ofzhur.ansible.monitoring ofzhur.ansible.db --limit web-01 ") {
		t.Errorf("converge: %v", runs)
	}
	e.ansibleExit = 0

	// the server is locked while its run list runs
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	if err := b.Lock(context.Background(), "fake/1", backend.LockInfo{Key: "fake/1", Who: "bob@desk", What: "reinstall"}); err != nil {
		t.Fatal(err)
	}
	e.ansibleCalls = nil
	if _, errOut, code := e.run("", false, "server", "provision", "web-01", "--account", "one"); code != 3 || !strings.Contains(errOut, "fake/1 is locked by bob@desk") || len(e.ansibleCalls) != 0 {
		t.Errorf("locked: code %d, %q, ansible %v", code, errOut, e.ansibleCalls)
	}
	b.Unlock(context.Background(), "fake/1")

	// usage errors
	for _, args := range [][]string{
		{"server", "provision", "web-01", "--group", "Web"},
		{"server", "provision", "web-01", "--group", "base,all"},
		{"server", "reinstall", "web-01", "--yes", "--group", "a b"},
		{"server", "reinstall", "web-01", "--yes", "--role", "web"},
	} {
		if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || (!strings.Contains(errOut, "a group name is lower-case") && !strings.Contains(errOut, "Ansible's own names") && !strings.Contains(errOut, "unknown flag: --role")) {
			t.Errorf("%v: code %d, %q", args, code, errOut)
		}
	}
}

// TestInventoryNames reads what ansible-inventory --list of ansible-core
// 2.20 prints for a host file of ump (its values marked unsafe), the
// group tree of groups.yml with an empty group, and hosts written by
// hand.
func TestInventoryNames(t *testing.T) {
	inv, err := parseInventory([]byte(`{
    "_meta": {
        "hostvars": {
            "db": {
                "ansible_host": "192.0.2.5",
                "ump_ref": 42
            },
            "web-01": {
                "ansible_host": {
                    "__ansible_unsafe": "203.0.113.10"
                },
                "ump_ref": {
                    "__ansible_unsafe": "fake/1"
                }
            },
            "web-03": {
                "ump_ref": "fake/3"
            }
        },
        "profile": "inventory_legacy"
    },
    "all": {
        "children": [
            "ungrouped",
            "common",
            "prod"
        ]
    },
    "common": {
        "hosts": [
            "web-01",
            "web-03"
        ]
    },
    "prod": {
        "children": [
            "web",
            "dbs"
        ]
    },
    "ungrouped": {
        "hosts": [
            "db",
            "lb-01"
        ]
    },
    "web": {
        "hosts": [
            "web-01"
        ]
    }
}`))
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	for g := range inv.groups {
		groups = append(groups, g)
	}
	slices.Sort(groups)
	if strings.Join(groups, ",") != "all,common,dbs,prod,ungrouped,web" || len(inv.hosts) != 4 || inv.hosts["web-01"] != "fake/1" || inv.hosts["web-03"] != "fake/3" || inv.hosts["db"] != "" || inv.hosts["lb-01"] != "" {
		t.Errorf("groups %v, hosts %v", groups, inv.hosts)
	}
	repo := &infraRepo{dir: t.TempDir()}
	for _, c := range []struct {
		hostname, ref string
		groups        []string
		err           string
	}{
		{"web-01", "fake/1", []string{"web"}, ""},
		// an empty group is a group too
		{"dbs", "fake/2", nil, "the hostname dbs is also a group of the inventory"},
		{"prod", "fake/2", nil, "the hostname prod is also a group of the inventory"},
		{"web-02", "fake/2", []string{"web", "db"}, "the group db of the run list is also a host of the inventory"},
		{"web-02", "fake/2", []string{"lb_01", "web-01"}, "the group web-01 of the run list is also a host of the inventory"},
		// the server's own host under its old hostname goes when the new one is written
		{"web-02", "fake/1", []string{"web-01"}, ""},
	} {
		err := inv.check(c.hostname, c.ref, repo.runList(c.groups), "give another --hostname")
		if c.err == "" && err != nil || c.err != "" && (err == nil || exitCodeFor(err) != exitUsage || !strings.HasPrefix(err.Error(), c.err+": a host and a group with the same name make the group's plays match only the host; ")) {
			t.Errorf("%s (%s) in %v: %v", c.hostname, c.ref, c.groups, err)
		}
	}
	for _, bad := range []string{"", "[]", `{"web": {"hosts": "web-01"}}`, `{"_meta": {"hostvars": []}}`} {
		if _, err := parseInventory([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// TestNoRebootThenProvision: --no-reboot records the installation, so
// that ump server provision after the reboot puts in the new system: its
// hostname, groups and administrator.
func TestNoRebootThenProvision(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	e.ansibleCalls, e.forgotHosts = nil, nil
	e.now = e.now.Add(time.Hour)
	resetFakePower()
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-reboot", "--hostname", "web-02", "--group", "db", "--set", "users=[{name: ops, sudo: true, keys: [ump]}]", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "Not provisioned: after the reboot, ump server provision web-01, run from the infrastructure repository, puts web-02 into the inventory and runs its run list (where ump is the inventory, web-02 is in it from its record on).\n") {
		t.Fatalf("--no-reboot: code %d, %q", code, errOut)
	}
	for _, want := range []string{`"installed_at": "2026-10-12T10:00:00Z"`, `"groups": [
    "common",
    "db"
  ],`, `"rebooted": false`, `"provisioned": false`, `"admin": "ops"`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s:\n%s", want, out)
		}
	}
	if st := e.readState(); !strings.Contains(st, "      hostname: web-02\n      admin: ops\n      at: 2026-10-12T10:00:00Z\n    groups:\n      - db\n") {
		t.Errorf("state:\n%s", st)
	}
	if strings.Join(e.forgotHosts, ",") != "203.0.113.10" || !strings.Contains(errOut, "Removed the old host key of 203.0.113.10 from ~/.ssh/known_hosts") {
		t.Errorf("host keys forgotten: %v", e.forgotHosts)
	}
	// the new system is not up: nothing ran, the host file is the old one
	if len(e.ansibleRuns()) != 0 || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") || e.hostFile("web-02.yml") != "" {
		t.Errorf("--no-reboot: runs %v, host files %q %q", e.ansibleRuns(), e.hostFile("web-01.yml"), e.hostFile("web-02.yml"))
	}

	// after the reboot
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "server", "provision", "web-01", "--account", "one")
	if code != 0 || out != "fake/1 (web-01, 203.0.113.10): provisioned, run list common, db.\n" || !strings.Contains(errOut, "Removed inventory/hosts/web-01.yml: fake/1 has the hostname web-02 now.\nWrote inventory/hosts/web-02.yml: web-02 in common, db.\n") {
		t.Errorf("provision: code %d, %q\n%s", code, out, errOut)
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || !strings.HasPrefix(runs[0], "ofzhur.ansible.common ofzhur.ansible.db --limit web-02 ") {
		t.Errorf("provision runs: %v", runs)
	}
	host := e.hostFile("web-02.yml")
	if !strings.Contains(host, "groups: [common, db]\n") || !strings.Contains(host, "  ansible_user: ops\n") || !strings.Contains(host, "  ump_installed_at: \"2026-10-12T10:00:00Z\"\n") || e.hostFile("web-01.yml") != "" {
		t.Errorf("host file:\n%s", host)
	}
}

// TestStateRolesBecomeGroups: a state an older ump wrote with roles is
// read as groups and written back as such.
func TestStateRolesBecomeGroups(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "state.yaml"), []byte("version: 1\nservers:\n  fake/1:\n    installed: {os: ubuntu-24.04, profile: web, at: 2026-01-02T03:04:05Z}\n    roles: [web-app, base]\n"), 0o600)
	if out, _, code := e.run("", false, "server", "list", "web-01", "--account", "one", "-o", "json"); code != 0 || !strings.Contains(out, "\"groups\": [\n      \"common\",\n      \"web_app\",\n      \"base\"\n    ]") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	if _, _, code := e.run("", false, "server", "set", "web-01", "--attr", "owner=alice", "--account", "one"); code != 0 || !strings.Contains(e.readState(), "    groups:\n      - web_app\n      - base\n") || strings.Contains(e.readState(), "roles") {
		t.Errorf("written: code %d\n%s", code, e.readState())
	}
}

// TestUmpInventoryFile: a YAML mapping with plugin: ofzhur.ansible.ump
// directly in inventory/ makes ump the inventory; nothing else does.
func TestUmpInventoryFile(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inventory")
	os.MkdirAll(filepath.Join(inv, "sub"), 0o755)
	write := func(name, content string) {
		os.WriteFile(filepath.Join(inv, name), []byte(content), 0o644)
	}
	write("groups.yml", "all:\n  children:\n    common:\n")
	write("list.yml", "- plugin: ofzhur.ansible.ump\n")
	write("host.yml", "plugin: ofzhur.ansible.host\n")
	write(".ump.yml", "plugin: ofzhur.ansible.ump\n")
	write("ump.txt", "plugin: ofzhur.ansible.ump\n")
	write("sub/ump.yml", "plugin: ofzhur.ansible.ump\n")
	write("broken.yml", "{not yaml")
	if got := umpInventory(dir); got != "" {
		t.Errorf("no plugin file: %q", got)
	}
	write("ump.yaml", "# ump is the inventory\nplugin: ofzhur.ansible.ump\n")
	if got := umpInventory(dir); got != "inventory/ump.yaml" {
		t.Errorf("plugin file: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "ansible.cfg"), nil, 0o644)
	a := &app{repoDir: dir}
	if repo, missing, err := a.infraLayout(); err != nil || len(missing) != 0 || repo.plugin != "inventory/ump.yaml" {
		t.Errorf("layout: %+v %v %v", repo, missing, err)
	}
}

// TestServerListAnsible: -o ansible is the inventory of the servers ump
// installed, with the hosts, groups and variables of their host files.
func TestServerListAnsible(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	if _, _, code := e.run("", false, "server", "set", "web-01", "--attr", "owner=alice", "--attr", "note=a: b", "--account", "one"); code != 0 {
		t.Fatal("server set failed")
	}
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web,db", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	out, errOut, code := e.run("", false, "server", "list", "-o", "ansible", "--account", "one")
	want := `{
  "_meta": {
    "hostvars": {
      "web-01": {
        "ansible_host": "203.0.113.10",
        "ansible_user": "ump",
        "ump_name": "web-01",
        "ump_ref": "fake/1",
        "ump_provider": "fake",
        "ump_account": "one",
        "ump_location": "eu-1",
        "ump_product": "m1",
        "ump_os": "ubuntu-26.04",
        "ump_profile": "ubuntu-26.04",
        "ump_installed_at": "2026-10-12T09:00:00Z",
        "ump_attributes": {
          "note": "a: b",
          "owner": "alice"
        }
      }
    }
  },
  "all": {
    "children": [
      "common",
      "db",
      "web"
    ]
  },
  "common": {
    "hosts": [
      "web-01"
    ]
  },
  "db": {
    "hosts": [
      "web-01"
    ]
  },
  "web": {
    "hosts": [
      "web-01"
    ]
  }
}
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("-o ansible: code %d, %q\n%s", code, errOut, out)
	}
	// the same as the host file: its groups, its variables
	var file struct {
		Groups []string       `yaml:"groups"`
		Vars   map[string]any `yaml:"vars"`
	}
	if err := yaml.Unmarshal([]byte(e.hostFile("web-01.yml")), &file); err != nil {
		t.Fatal(err)
	}
	var listed map[string]json.RawMessage
	json.Unmarshal([]byte(out), &listed)
	var meta struct {
		Hostvars map[string]map[string]any `json:"hostvars"`
	}
	json.Unmarshal(listed["_meta"], &meta)
	fromFile, _ := json.Marshal(file.Vars)
	fromList, _ := json.Marshal(meta.Hostvars["web-01"])
	if string(fromFile) != string(fromList) {
		t.Errorf("variables differ:\nhost file %s\nlisted    %s", fromFile, fromList)
	}
	for _, g := range file.Groups {
		if !strings.Contains(string(listed[g]), `"web-01"`) {
			t.Errorf("group %s of the host file: %s", g, listed[g])
		}
	}
	// what ump reads back through ansible-inventory --list
	if inv, err := parseInventory([]byte(out)); err != nil || inv.hosts["web-01"] != "fake/1" || !inv.groups["db"] || len(inv.hosts) != 1 {
		t.Errorf("parsed: %+v, %v", inv, err)
	}
	// --cached serves the same; a server ump did not install is not in it
	if cached, errOut, code := e.run("", false, "server", "list", "-o", "ansible", "--cached", "--account", "one"); code != 0 || cached != out || errOut != "" {
		t.Errorf("--cached: code %d, %q\n%s", code, errOut, cached)
	}
	empty := "{\n  \"_meta\": {\n    \"hostvars\": {}\n  },\n  \"all\": {\n    \"children\": []\n  }\n}\n"
	if out, _, code := e.run("", false, "server", "list", "db-01", "-o", "ansible", "--account", "one"); code != 0 || out != empty {
		t.Errorf("db-01: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "server", "list", "-o", "ansible", "--diff", "--account", "one"); code != 2 || !strings.Contains(errOut, "-o ansible cannot be combined with --diff or --since") {
		t.Errorf("--diff: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "list", "-o", "ini", "--account", "one"); code != 2 || !strings.Contains(errOut, `unknown output format "ini" (use one of table, wide, json, yaml, name, ansible)`) {
		t.Errorf("-o ini: code %d, %q", code, errOut)
	}

	// two servers with one hostname: the later installation has it
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
	setDB := func(at time.Time) {
		data, _ := os.ReadFile(path)
		d, err := state.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		d.Servers["fake/2"] = state.Server{Installed: &state.Install{OS: "debian-13", Profile: "debian-13", Hostname: "web-01", Admin: "ops", At: at}, Groups: []string{"db"}}
		data, _ = d.Bytes()
		os.WriteFile(path, data, 0o600)
	}
	setDB(e.now.Add(-time.Hour))
	out, errOut, code = e.run("", false, "server", "list", "-o", "ansible", "--account", "one")
	if code != 0 || !strings.Contains(out, `"ump_ref": "fake/1"`) || strings.Contains(out, "fake/2") || errOut != "ump: fake/2 (db-01, 203.0.113.11) is left out of the inventory: its hostname web-01 is that of fake/1, installed later\n" {
		t.Errorf("earlier db-01: code %d, %q\n%s", code, errOut, out)
	}
	setDB(e.now.Add(time.Hour))
	out, errOut, code = e.run("", false, "server", "list", "-o", "ansible", "--account", "one")
	if code != 0 || !strings.Contains(out, `"ump_ref": "fake/2"`) || !strings.Contains(out, `"ansible_user": "ops"`) || strings.Contains(out, `"fake/1"`) || errOut != "ump: fake/1 (web-01, 203.0.113.10) is left out of the inventory: its hostname web-01 is that of fake/2, installed later\n" {
		t.Errorf("later db-01: code %d, %q\n%s", code, errOut, out)
	}

	// a group of a state edited by hand that would break the inventory
	data, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(data), "    groups:\n      - db\n", "    groups:\n      - db\n      - _meta\n      - all\n      - Web\n", 1)), 0o600)
	out, errOut, code = e.run("", false, "server", "list", "-o", "ansible", "--account", "one")
	if inv, err := parseInventory([]byte(out)); code != 0 || err != nil || inv.hosts["web-01"] != "fake/2" || inv.groups["Web"] || !strings.Contains(out, `"db": {`) || !strings.Contains(errOut, "ump: the group \"_meta\" of fake/2 (db-01, 203.0.113.11) in the state is left out: not a group name ump gives\nump: the group \"all\" of fake/2") || !strings.Contains(errOut, "ump: the group \"Web\" of fake/2") {
		t.Errorf("groups edited by hand: code %d, %q\n%s", code, errOut, out)
	}
}

// TestUmpIsTheInventory: with inventory/ump.yml, reinstall and provision
// write no host file and check and run as before; a host file of the
// server beside it, or its hostname as another host of the inventory,
// stops them first.
func TestUmpIsTheInventory(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(e.repo, "inventory", "ump.yml"), []byte("plugin: ofzhur.ansible.ump\n"), 0o644)
	hosts := filepath.Join(e.repo, "inventory", "hosts")

	resetFakePower()
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"provisioned": true`) {
		t.Fatalf("reinstall: code %d, %q\n%s", code, errOut, out)
	}
	up := strings.Index(errOut, "web-01 is up:")
	none := strings.Index(errOut, "No host file: inventory/ump.yml makes ump the inventory, with web-01 in common, web.\nProvisioning web-01: common, web (ofzhur.ansible.common, ofzhur.ansible.web) ...\n")
	if up < 0 || none < up || strings.Contains(errOut, "Wrote ") {
		t.Errorf("progress:\n%s", errOut)
	}
	if _, err := os.Stat(hosts); err == nil {
		t.Errorf("inventory/hosts/ written")
	}
	if len(e.ansibleCalls) != 3 || e.ansibleCalls[0].args[0] != "--syntax-check" || e.ansibleCalls[1].prog != "ansible-inventory" || len(e.ansibleRuns()) != 1 {
		t.Errorf("ansible: %+v", e.ansibleCalls)
	}
	// an ump the runs call in turn is part of this one, with its configuration
	for _, c := range e.ansibleCalls {
		if !slices.Contains(c.env, "UMP_PARENT=local") || !slices.Contains(c.env, "UMP_CONFIG="+e.cfgPath) {
			t.Errorf("%s env: %v", c.prog, c.env)
		}
	}
	if st := e.readState(); !strings.Contains(st, "      hostname: web-01\n") || !strings.Contains(st, "    groups:\n      - web\n") {
		t.Errorf("state:\n%s", st)
	}

	// provision: no host file either, the run list runs
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "server", "provision", "web-01", "--group", "monitoring", "--account", "one")
	if code != 0 || out != "fake/1 (web-01, 203.0.113.10): provisioned, run list common, web, monitoring.\n" || !strings.Contains(errOut, "No host file: inventory/ump.yml makes ump the inventory, with web-01 in common, web, monitoring.\nProvisioning web-01: ") || len(e.ansibleRuns()) != 1 {
		t.Errorf("provision: code %d, %q\n%s", code, errOut, out)
	}
	// --no-provision: in the inventory through ump, nothing runs
	resetFakePower()
	e.ansibleCalls = nil
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "No host file: inventory/ump.yml makes ump the inventory, with web-01 in common.\nNot provisioned (--no-provision); ump server provision web-01 runs its run list.\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	// another server's host file is no concern of this one
	os.MkdirAll(hosts, 0o755)
	os.WriteFile(filepath.Join(hosts, "db-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/2\n"), 0o644)
	if _, errOut, code := e.run("", false, "server", "provision", "web-01", "--account", "one"); code != 0 {
		t.Errorf("another server's host file: code %d, %q", code, errOut)
	}

	// a host file of the hostname, or one of the server under another
	// name, beside the plugin: refused before anything happens
	for _, f := range []struct{ name, content, want string }{
		{"web-01.yml", "plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/1\n", "inventory/hosts/web-01.yml defines the host web-01 too, and inventory/ump.yml makes ump the inventory: the host would be defined twice; remove the host file"},
		{"web-01.yaml", "web-01:\n", "inventory/hosts/web-01.yaml defines the host web-01 too, and inventory/ump.yml makes ump the inventory: the host would be defined twice; remove the host file"},
		{"old-name.yml", "plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/1\n", "inventory/hosts/old-name.yml is the host file of fake/1, and inventory/ump.yml makes ump the inventory: the server would be in it twice; remove the host file"},
	} {
		os.WriteFile(filepath.Join(hosts, f.name), []byte(f.content), 0o644)
		for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--no-provision"}, {"server", "provision", "web-01"}} {
			e.remoteCalls, e.ansibleCalls = nil, nil
			resetFakePower()
			if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || !strings.HasSuffix(errOut, "ump: "+f.want+"\n") {
				t.Errorf("%s, %v: code %d, %q", f.name, args, code, errOut)
			}
			if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 || len(e.ansibleRuns()) != 0 {
				t.Errorf("%s, %v: remote %v, provider %v, runs %v", f.name, args, e.remoteCalls, fakePower.actions, e.ansibleRuns())
			}
		}
		os.Remove(filepath.Join(hosts, f.name))
	}

	// the hostname as another host of the inventory
	e.inventory = `{"_meta": {"hostvars": {"web-01": {"ump_ref": "fake/1"}, "web-02": {"ump_ref": {"__ansible_unsafe": "fake/2"}}, "lb": {"ansible_host": "192.0.2.9"}}}, "all": {"children": ["ungrouped", "common"]}, "common": {"hosts": ["web-01", "web-02"]}, "ungrouped": {"hosts": ["lb"]}}`
	for host, want := range map[string]string{
		"web-02": "the hostname web-02 is the host of fake/2 in the inventory (its ump_ref); give another --hostname",
		"lb":     "the hostname lb is a host of the inventory without ump_ref; give another --hostname",
	} {
		resetFakePower()
		if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", host, "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: "+want+"\n") || len(fakePower.actions) != 0 {
			t.Errorf("hostname %s: code %d, %q", host, code, errOut)
		}
	}
	// the server's own host is no conflict
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 0 {
		t.Errorf("own host: code %d, %q", code, errOut)
	}
	e.inventory = ""

	// --no-reboot: in the inventory from its record on
	resetFakePower()
	e.ansibleCalls = nil
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-reboot", "--hostname", "web-02", "--account", "one")
	if code != 0 || !strings.HasSuffix(errOut, "Not provisioned: inventory/ump.yml makes ump the inventory, which has web-02 from its record on; after the reboot, ump server provision web-01 runs its run list.\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("--no-reboot: code %d, %q", code, errOut)
	}

	// the installation cannot be recorded: nothing runs against the old
	// record, which ump as the inventory would list
	home := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")
	e.installHook = func() { os.Chmod(home, 0o500) }
	t.Cleanup(func() { os.Chmod(home, 0o700) })
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		return // root writes anyway
	}
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--group", "db"}, 6, "ump: provisioning failed: web-03 is installed but not recorded, and inventory/ump.yml makes ump the inventory, which lists the old record; record it and run ump server provision web-01\n"},
		{[]string{"--no-provision"}, 0, "Not provisioned (--no-provision), and not in the inventory as installed: inventory/ump.yml makes ump the inventory, which lists the old record; record it.\n"},
	} {
		resetFakePower()
		e.ansibleCalls = nil
		out, errOut, code := e.run("", false, append([]string{"server", "reinstall", "web-01", "--yes", "--hostname", "web-03", "--account", "one", "-o", "json"}, c.args...)...)
		os.Chmod(home, 0o700)
		if code != c.code || !strings.HasSuffix(errOut, c.want) || len(e.ansibleRuns()) != 0 || !strings.Contains(out, `"provisioned": false`) {
			t.Errorf("%v, not recorded: code %d, %q\n%s", c.args, code, errOut, out)
		}
		if !strings.Contains(errOut, "ump: the installation is not recorded in the state: ") || !strings.Contains(errOut, "Record it by hand: in state.yaml (ump config state view says where), the server's installed and groups are these:\nservers:\n  fake/1:\n    installed:\n      os: ubuntu-26.04\n") || !strings.Contains(errOut, "      hostname: web-03\n      admin: ump\n") {
			t.Errorf("%v, the record to add:\n%s", c.args, errOut)
		}
		if strings.Contains(e.readState(), "web-03") {
			t.Errorf("%v: recorded\n%s", c.args, e.readState())
		}
	}
}

// TestMain runs the test binary as ump when UMP_TEST_MAIN is set: the
// ump that Ansible runs in TestNestedUmp, as the inventory plugin
// ofzhur.ansible.ump does, with the fake provider type of the tests.
func TestMain(m *testing.M) {
	if os.Getenv("UMP_TEST_MAIN") != "" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// nestedAnsible stands in for ansible-playbook and ansible-inventory with
// the inventory plugin ofzhur.ansible.ump: every run lists the inventory
// with ump, the test binary, a child of the ump that runs Ansible. It
// notes per run the child's UMP_PARENT and exit status, whether the
// mirror's lock was held and whether the child changed the mirror's
// manifest (a pull or a push), and keeps the child's output; the
// inventory goes to ansible-inventory's stdout. UMP_TEST_BACKEND, when
// set, is a backend directory to make readable again first;
// UMP_TEST_HOLD holds the mirror's lock while the child runs, as a
// parent that kept it would.
const nestedAnsible = `#!/bin/sh
n=$(cat "$UMP_TEST_LOG/n" 2>/dev/null || echo 0); n=$((n + 1)); echo $n >"$UMP_TEST_LOG/n"
log="$UMP_TEST_LOG/$n"
if [ -n "$UMP_TEST_BACKEND" ]; then chmod 700 "$UMP_TEST_BACKEND"; fi
lock=free
if [ -n "$UMP_TEST_MIRROR" ]; then
	if [ -e "$UMP_TEST_MIRROR/.mirror.lock" ]; then lock=held; fi
	cp "$UMP_TEST_MIRROR/.mirror.json" "$log.before"
fi
if [ -n "$UMP_TEST_HOLD" ]; then touch "$UMP_TEST_MIRROR/.mirror.lock"; fi
UMP_TEST_MAIN=1 "$UMP_TEST_BIN" server list -o ansible >"$log.json" 2>"$log.err"
code=$?
if [ -n "$UMP_TEST_HOLD" ]; then rm -f "$UMP_TEST_MIRROR/.mirror.lock"; fi
mirror=kept
if [ -n "$UMP_TEST_MIRROR" ] && ! cmp -s "$UMP_TEST_MIRROR/.mirror.json" "$log.before"; then mirror=changed; fi
echo "$(basename "$0") $UMP_PARENT $code lock=$lock mirror=$mirror" >"$log"
if [ "$(basename "$0")" = ansible-inventory ]; then cat "$log.json"; fi
exit $code
`

// nestedRun is what nestedAnsible noted of one run.
type nestedRun struct {
	note, stderr string
	inv          *inventory
	groups       map[string][]string
	vars         map[string]map[string]any
}

// nestedRuns reads the runs nestedAnsible noted since the last call.
func nestedRuns(t *testing.T, log string) []nestedRun {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(log, "n"))
	var runs []nestedRun
	for i := 1; ; i++ {
		base := filepath.Join(log, strconv.Itoa(i))
		note, err := os.ReadFile(base)
		if err != nil {
			break
		}
		out, _ := os.ReadFile(base + ".json")
		errOut, _ := os.ReadFile(base + ".err")
		r := nestedRun{note: strings.TrimSpace(string(note)), stderr: string(errOut), groups: map[string][]string{}}
		r.inv, _ = parseInventory(out)
		var raw map[string]json.RawMessage
		json.Unmarshal(out, &raw)
		for g, v := range raw {
			var group struct {
				Hosts    []string                  `json:"hosts"`
				Hostvars map[string]map[string]any `json:"hostvars"`
			}
			json.Unmarshal(v, &group)
			if g == "_meta" {
				r.vars = group.Hostvars
			} else if len(group.Hosts) > 0 {
				r.groups[g] = group.Hosts
			}
		}
		runs = append(runs, r)
		for _, f := range []string{"", ".json", ".err", ".before"} {
			os.Remove(base + f)
		}
	}
	if len(data) > 0 {
		os.Remove(filepath.Join(log, "n"))
	}
	return runs
}

// TestNestedUmp runs ump as the inventory the way Ansible does under ump
// server reinstall and provision: ump runs Ansible (the scripts above,
// with the real runner), and Ansible runs ump. The child sees the server
// its parent recorded before the run, with the parent's configuration
// and state, waits for no lock, and leaves the mirror alone: with the
// local home, with a backend, and with a backend that did not answer the
// parent but answers the child, which must still read the mirror the
// parent wrote to.
func TestNestedUmp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stands in for Ansible")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t)
	e.write("accounts:\n  one:\n    type: fake\n    login: bob\n    password: {env: FAKE_PASS}\n")
	e.writeProfiles()
	e.realAnsible = true
	os.WriteFile(filepath.Join(e.repo, "inventory", "ump.yml"), []byte("plugin: ofzhur.ansible.ump\n"), 0o644)
	bin, log := t.TempDir(), t.TempDir()
	for _, prog := range []string{"ansible-playbook", "ansible-inventory"} {
		os.WriteFile(filepath.Join(bin, prog), []byte(nestedAnsible), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("UMP_TEST_BIN", self)
	t.Setenv("UMP_TEST_LOG", log)
	t.Setenv("UMP_TEST_MIRROR", "")
	t.Setenv("UMP_TEST_BACKEND", "")
	t.Setenv("UMP_TEST_HOLD", "")

	// check runs what reinstall and provision ran: the syntax check, the
	// inventory and the run list, each with a child that answered
	// without a word, under parent, and the run list's child listing
	// hostname in groups, as the server ref
	check := func(what, parent, hostname, ref string, groups []string, extra string) {
		t.Helper()
		runs := nestedRuns(t, log)
		if len(runs) != 3 || !strings.HasPrefix(runs[0].note, "ansible-playbook ") || !strings.HasPrefix(runs[1].note, "ansible-inventory ") || !strings.HasPrefix(runs[2].note, "ansible-playbook ") {
			t.Fatalf("%s: runs %+v", what, runs)
		}
		for _, r := range runs {
			if want := parent + " 0 lock=free mirror=kept"; !strings.HasSuffix(r.note, " "+want) || r.stderr != "" || r.inv == nil {
				t.Errorf("%s: %q, want %q; stderr %q", what, r.note, want, r.stderr)
			}
		}
		run := runs[2]
		if run.inv == nil || run.inv.hosts[hostname] != ref {
			t.Fatalf("%s: the run list's inventory lacks %s: %+v", what, hostname, run.inv)
		}
		for _, g := range append([]string{"common"}, groups...) {
			if !slices.Contains(run.groups[g], hostname) {
				t.Errorf("%s: %s not in %s: %v", what, hostname, g, run.groups)
			}
		}
		if len(run.groups) != len(groups)+1 || run.vars[hostname]["ansible_host"] != "203.0.113.10" || run.vars[hostname]["ansible_user"] != "ump" {
			t.Errorf("%s: groups %v, vars %v", what, run.groups, run.vars)
		}
		if extra != "" && !strings.Contains(run.note+run.stderr, extra) {
			t.Errorf("%s: %q lacks %q", what, run.note, extra)
		}
	}

	// the local home
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web"); code != 0 || !strings.Contains(errOut, "No host file: inventory/ump.yml makes ump the inventory, with web-01 in common, web.\n") {
		t.Fatalf("reinstall, local: code %d, %q", code, errOut)
	}
	check("local", "local", "web-01", "fake/1", []string{"web"}, "")

	// a backend: the state the parent records goes there, the child
	// reads it there and leaves the mirror, which it shares, to the parent
	backend := filepath.Join(t.TempDir(), "backend")
	os.MkdirAll(filepath.Join(backend, "profiles"), 0o700)
	for _, name := range profileFiles {
		data, _ := os.ReadFile(filepath.Join("..", "..", "profiles", name))
		os.WriteFile(filepath.Join(backend, "profiles", name), data, 0o600)
	}
	t.Setenv("UMP_STATE", "file://"+backend)
	if _, errOut, code := e.run("", false, "server", "list"); code != 0 {
		t.Fatalf("list, backend: code %d, %q", code, errOut)
	}
	mirrors, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "homes", "*"))
	if len(mirrors) != 1 {
		t.Fatalf("mirrors: %v", mirrors)
	}
	t.Setenv("UMP_TEST_MIRROR", mirrors[0])
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web"); code != 0 {
		t.Fatalf("reinstall, backend: code %d, %q", code, errOut)
	}
	check("backend", "backend", "web-01", "fake/1", []string{"web"}, "")
	if data, _ := os.ReadFile(filepath.Join(backend, "state.yaml")); !strings.Contains(string(data), "hostname: web-01") {
		t.Errorf("backend state:\n%s", data)
	}
	// a group the parent adds before the run is in the child's inventory;
	// a lock of the mirror held meanwhile does not stop the child
	t.Setenv("UMP_TEST_HOLD", "1")
	if _, errOut, code := e.run("", false, "server", "provision", "web-01", "--group", "monitoring"); code != 0 {
		t.Fatalf("provision, backend: code %d, %q", code, errOut)
	}
	check("provision", "backend", "web-01", "fake/1", []string{"web", "monitoring"}, "")
	t.Setenv("UMP_TEST_HOLD", "")

	// the backend does not answer the parent's pull: the parent records
	// on the mirror; it answers again by the time Ansible runs, and the
	// child still reads the mirror, where the new hostname and groups are
	if os.Geteuid() == 0 {
		return // root reads the directory anyway
	}
	e.now = e.now.Add(time.Minute)
	os.Chmod(backend, 0)
	t.Cleanup(func() { os.Chmod(backend, 0o700) })
	t.Setenv("UMP_TEST_BACKEND", backend)
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-09", "--group", "db")
	if code != 0 || !strings.Contains(errOut, "ump: the state could not be pulled from file://"+backend) {
		t.Fatalf("reinstall, backend down: code %d, %q", code, errOut)
	}
	check("mirror", "mirror", "web-09", "fake/1", []string{"db"}, "")
	if data, _ := os.ReadFile(filepath.Join(backend, "state.yaml")); strings.Contains(string(data), "web-09") {
		t.Errorf("the mirror's state reached the backend:\n%s", data)
	}
}
