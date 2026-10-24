package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
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

// TestPlaybooks: the playbooks are playbooks/*.yml in file-name order,
// files or links to files; --playbook narrows them to those it names, in
// that order still.
func TestPlaybooks(t *testing.T) {
	repo := &infraRepo{dir: t.TempDir()}
	if got := repo.playbooks(); got != nil {
		t.Errorf("no playbooks/: %v", got)
	}
	dir := filepath.Join(repo.dir, "playbooks")
	os.MkdirAll(filepath.Join(dir, "tasks"), 0o755)
	os.MkdirAll(filepath.Join(dir, "dir.yml"), 0o755)
	for _, n := range []string{"20-db.yml", "00-base.yml", "10-web.yml", "30-cache.yaml", ".hidden.yml", "notes.txt", "tasks/users.yml"} {
		os.WriteFile(filepath.Join(dir, n), []byte("- hosts: all\n"), 0o644)
	}
	want := "00-base,10-web,20-db"
	if runtime.GOOS != "windows" {
		os.Symlink("00-base.yml", filepath.Join(dir, "15-link.yml"))
		os.Symlink("dir.yml", filepath.Join(dir, "16-dir-link.yml"))
		os.Symlink("gone.yml", filepath.Join(dir, "17-dangling.yml"))
		want = "00-base,10-web,15-link,20-db"
	}
	if got := repo.playbooks(); strings.Join(got, ",") != want {
		t.Errorf("playbooks: %v", got)
	}
	all := []string{"00-base", "10-web", "20-db"}
	if got := playbookPaths(all); strings.Join(got, " ") != "playbooks/00-base.yml playbooks/10-web.yml playbooks/20-db.yml" {
		t.Errorf("paths: %v", got)
	}
	if got, err := choosePlaybooks(all, nil); err != nil || !slices.Equal(got, all) {
		t.Errorf("every one: %v, %v", got, err)
	}
	if got, err := choosePlaybooks(all, []string{"20-db", "00-base"}); err != nil || strings.Join(got, ",") != "00-base,20-db" {
		t.Errorf("chosen: %v, %v", got, err)
	}
	if _, err := choosePlaybooks(all, []string{"10-web", "web"}); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "--playbook web: there is no playbooks/web.yml (the playbooks: 00-base, 10-web, 20-db)" {
		t.Errorf("unknown: %v", err)
	}
	cmd := &cobra.Command{Use: "x"}
	if got, err := playbookNames(cmd, listFlag{"10-web.yml,00-base", "10-web"}); err != nil || strings.Join(got, ",") != "10-web,00-base" {
		t.Errorf("names: %v, %v", got, err)
	}
	for _, bad := range []string{"tasks/users", ".hidden", ".yml"} {
		if _, err := playbookNames(cmd, listFlag{bad}); err == nil || exitCodeFor(err) != exitUsage {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// TestAnsibleArgs: what goes to ansible-playbook after -- may not name
// the hosts or the inventory, in any of the forms ansible-playbook takes:
// a long option by a prefix, a short one in a cluster; the value of a
// short option is no option.
func TestAnsibleArgs(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	for _, args := range [][]string{nil, {"--check", "--diff"}, {"-vvv", "-C"}, {"-e", "x=1"}, {"-eansible_python_interpreter=/usr/bin/python3"}, {"-tlinux,web"}, {"-uops"},
		{"--list-tasks"}, {"--list-hosts"}, {"--tags", "users"}, {"--extra-vars=limit=1"}, {"-"}} {
		if err := ansibleArgs(cmd, args); err != nil {
			t.Errorf("%q: %v", args, err)
		}
	}
	for _, arg := range []string{"-l", "-lweb", "-vl", "-vvl", "-Dl", "--l", "--li", "--limit", "--limit=web", "-limit", "-i", "-ihosts", "-Ci", "--i", "--inv", "--inventory", "--inventory-file=x", "--inventory-f"} {
		if err := ansibleArgs(cmd, []string{"--check", arg}); err == nil || exitCodeFor(err) != exitUsage || !strings.HasPrefix(err.Error(), arg+" after --: ") {
			t.Errorf("%q: %v", arg, err)
		}
	}
}

func TestRenderHostFile(t *testing.T) {
	m := provider.Machine{Provider: "hetzner-cloud", Account: "main", ID: "123", Name: "web-01", IPv4: "203.0.113.10", IPv6: "2001:db8::/64",
		Location: "fsn1", Product: "cx22", Attributes: map[string]string{"owner": "alice", "on_call": "yes", "note": "a: b", "cc": "4711"}}
	rec := state.Install{OS: "ubuntu-26.04", Profile: "web", At: time.Date(2026, 10, 12, 11, 58, 30, 0, time.FixedZone("CEST", 7200))}
	data, err := renderHostFile("web-01", []string{"common", "web", "db"}, hostVarsFor(m, rec, "ump", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `# web-01: written by ump (ump server reinstall, ump provision).
# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.
plugin: ofzhur.ansible.host
groups: [common, web, db]
vars:
  ansible_host: 203.0.113.10
  ump_admin: ump
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
	data, err = renderHostFile("rack-07", []string{"common"}, hostVarsFor(provider.Machine{Provider: "manual", ID: "rack-07", Name: "rack-07", IPv6: "2001:db8::7"}, state.Install{}, "ops", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ansible_host: 2001:db8::7\n  ump_admin: ops\n  ump_name: rack-07\n  ump_ref: manual/rack-07\n  ump_provider: manual\n") || strings.Contains(string(data), "ansible_user") {
		t.Errorf("minimal host file:\n%s", data)
	}
}

func TestWriteHostFile(t *testing.T) {
	ctx := context.Background()
	repo := &infraRepo{dir: t.TempDir()}
	hosts := filepath.Join(repo.dir, "inventory", "hosts")
	file := func(ref string) []byte {
		return []byte("plugin: ofzhur.ansible.host\ngroups: [common]\nvars:\n  ump_ref: " + ref + "\n")
	}
	groups := []string{"common", "web"}

	// no file yet: nothing in the way; the directory is created
	if err := repo.checkHost(ctx, "web-01", "fake/1", groups, "give another --hostname", ""); err != nil {
		t.Errorf("no file: %v", err)
	}
	written, removed, err := repo.writeHostFile(ctx, "web-01", "fake/1", file("fake/1"))
	if err != nil || written != "inventory/hosts/web-01.yml" || len(removed) != 0 {
		t.Fatalf("first write: %q %v %v", written, removed, err)
	}
	// the same again is no write
	fi, _ := os.Stat(filepath.Join(hosts, "web-01.yml"))
	os.Chtimes(filepath.Join(hosts, "web-01.yml"), time.Unix(1, 0), time.Unix(1, 0))
	if written, removed, err := repo.writeHostFile(ctx, "web-01", "fake/1", file("fake/1")); err != nil || written != "" || len(removed) != 0 {
		t.Errorf("unchanged: %q %v %v", written, removed, err)
	}
	if after, _ := os.Stat(filepath.Join(hosts, "web-01.yml")); fi == nil || after.ModTime().Unix() != 1 {
		t.Errorf("unchanged file rewritten")
	}
	// the server's own file is no conflict; another server's is
	if err := repo.checkHost(ctx, "web-01", "fake/1", groups, "give another --hostname", ""); err != nil {
		t.Errorf("own file: %v", err)
	}
	err = repo.checkHost(ctx, "web-01", "fake/2", groups, "give another --hostname", "")
	if err == nil || exitCodeFor(err) != exitUsage || err.Error() != "inventory/hosts/web-01.yml is the host file of fake/1 (its ump_ref); give another --hostname, or remove the file if that server is gone" {
		t.Errorf("conflict: %v", err)
	}
	os.WriteFile(filepath.Join(hosts, "db-01.yaml"), file("fake/2"), 0o644)
	if err := repo.checkHost(ctx, "db-01", "fake/3", groups, "give another --hostname", ""); err == nil || !strings.Contains(err.Error(), "inventory/hosts/db-01.yaml is the host file of fake/2") {
		t.Errorf("conflict with .yaml: %v", err)
	}
	// a hostname that is a group of the server, or Ansible's own
	for _, name := range []string{"web", "common"} {
		if err := repo.checkHost(ctx, name, "fake/1", groups, "give another --hostname", ""); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "the hostname "+name+" is also a group of the server: a host and a group with the same name make the group's plays match only the host; give another --hostname" {
			t.Errorf("hostname %s: %v", name, err)
		}
	}
	if err := repo.checkHost(ctx, "all", "fake/1", groups, "give another --hostname", ""); err == nil || !strings.Contains(err.Error(), "the hostname all is one of Ansible's own groups: a host and a group") {
		t.Errorf("hostname all: %v", err)
	}
	// a host of the controller's own names would take its plays
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if err := repo.checkHost(ctx, name, "fake/1", groups, "give another --hostname", ""); err == nil || exitCodeFor(err) != exitUsage || err.Error() != "the hostname "+name+" is one of Ansible's own names: plays and tasks for localhost would run on the server; give another --hostname" {
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
		if err := repo.checkHost(ctx, "hand-01", "fake/1", groups, "give another --hostname", ""); err == nil || exitCodeFor(err) != exitUsage || err.Error() != want {
			t.Errorf("%s: %v", f.name, err)
		}
		if _, _, err := repo.writeHostFile(ctx, "hand-01", "fake/1", file("fake/1")); err == nil || err.Error() != want {
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
	if _, _, err := repo.writeHostFile(ctx, "web-01", "fake/2", file("fake/2")); err == nil || !strings.Contains(err.Error(), "is the host file of fake/1") {
		t.Errorf("written over another server's: %v", err)
	}

	// a new hostname: the old file of the same server goes, others stay
	os.WriteFile(filepath.Join(hosts, "old-name.yaml"), file("fake/1"), 0o644)
	os.WriteFile(filepath.Join(hosts, ".hidden.yml"), file("fake/1"), 0o644)
	os.WriteFile(filepath.Join(hosts, "notes.txt"), file("fake/1"), 0o644)
	written, removed, err = repo.writeHostFile(ctx, "web-02", "fake/1", file("fake/1"))
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
// added but for an inventory of its own, its output where it was sent
// and its exit status.
func TestRunAnsible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stands in for ansible-playbook")
	}
	bin, dir := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\necho \"args $* in $(pwd) with $ANSIBLE_CONFIG, ${ANSIBLE_INVENTORY-no inventory}, ${ANSIBLE_HOSTS-no hosts}, $ANSIBLE_FORKS\"\necho oops >&2\nexit 4\n"
	if err := os.WriteFile(filepath.Join(bin, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANSIBLE_INVENTORY", "/elsewhere/hosts.ini")
	t.Setenv("ANSIBLE_HOSTS", "/elsewhere/hosts")
	t.Setenv("ANSIBLE_FORKS", "7")
	var out, errb bytes.Buffer
	code, err := runAnsible(context.Background(), dir, "ansible-playbook", []string{"playbooks/00-base.yml", "--limit", "web-01"}, []string{"ANSIBLE_CONFIG=/x/ansible.cfg"}, nil, &out, &errb)
	if err != nil || code != 4 || out.String() != "args playbooks/00-base.yml --limit web-01 in "+dir+" with /x/ansible.cfg, no inventory, no hosts, 7\n" || errb.String() != "oops\n" {
		t.Errorf("code %d, %v, stdout %q, stderr %q", code, err, out.String(), errb.String())
	}
	t.Setenv("PATH", t.TempDir())
	for _, prog := range []string{"ansible-playbook", "ansible-inventory"} {
		if _, err := runAnsible(context.Background(), dir, prog, nil, nil, nil, &out, &errb); err == nil || err.Error() != "no "+prog+" found in PATH" {
			t.Errorf("without %s: %v", prog, err)
		}
	}
}

// TestRecap reads the PLAY RECAP of ansible-playbook as it passes, in
// pieces, with Ansible's colors: the last one counts, as ansible-playbook
// prints one per playbook with the counts so far.
func TestRecap(t *testing.T) {
	out := "PLAY [all] ****\n\nTASK [ping] ****\nok: [web-01]\n\nPLAY RECAP *********************************************************************\n" +
		"web-01                     : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0   \n\n" +
		"PLAY [db] ****\n\nPLAY RECAP *********************************************************************\n" +
		"\x1b[0;31mdb-01\x1b[0m                      : \x1b[0;32mok=3   \x1b[0m changed=\x1b[0;33m2   \x1b[0m unreachable=0    \x1b[0;31mfailed=1   \x1b[0m skipped=4    rescued=5    ignored=6   \n" +
		"\x1b[0;32mweb-01\x1b[0m                     : \x1b[0;32mok=2   \x1b[0m changed=0    \x1b[1;31munreachable=1   \x1b[0m failed=0    skipped=0    rescued=0    ignored=0\n" +
		"\nweb-01 : ok=9 changed=9 unreachable=9 failed=9\nCUSTOM STATS: ****\n"
	for _, size := range []int{1, 7, len(out)} {
		w := &recapWriter{}
		for i := 0; i < len(out); i += size {
			w.Write([]byte(out[i:min(i+size, len(out))]))
		}
		want := map[string]hostRecap{
			"db-01":  {OK: 3, Changed: 2, Failed: 1, Skipped: 4, Rescued: 5, Ignored: 6},
			"web-01": {OK: 2, Unreachable: 1},
		}
		if len(w.hosts) != 2 || w.hosts["db-01"] != want["db-01"] || w.hosts["web-01"] != want["web-01"] {
			t.Errorf("pieces of %d: %+v", size, w.hosts)
		}
	}
	// no recap: no hosts; a last line without its newline is read at the end
	w := &recapWriter{}
	w.Write([]byte("ERROR! the playbook could not be found\n"))
	if w.hosts != nil {
		t.Errorf("no recap: %+v", w.hosts)
	}
	w.Write([]byte("PLAY RECAP ***\nlb : ok=1 changed=1 unreachable=0 failed=0"))
	w.Write([]byte("\n"))
	if w.hosts["lb"] != (hostRecap{OK: 1, Changed: 1}) {
		t.Errorf("last line: %+v", w.hosts)
	}
}

// noTouch says what ump did to servers and providers: nothing, when the
// layout or the checks stopped it.
func (e *testEnv) noTouch(t *testing.T, what string) {
	t.Helper()
	if len(e.remoteCalls) != 0 || len(e.sshCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("%s: touched the server: remote %v, ssh %v, provider %v", what, e.remoteCalls, e.sshCalls, fakePower.actions)
	}
	if e.hostFile("web-01.yml") != "" {
		t.Errorf("%s: host file written", what)
	}
}

// keyPath is UMP's key in the test home.
func keyPath() string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")
}

// importing is the playbook that imports the playbooks of a run of
// several.
func importing() string {
	return "playbooks/.ump-provision-" + strconv.Itoa(os.Getpid()) + ".yml"
}

// clearFacts is the play that clears the facts of the hosts of the
// pattern, the servers just installed, in the playbook of the run.
func clearFacts(pattern string) string {
	return "- name: New systems, without the old ones' facts\n  hosts: \"" + pattern + "\"\n  gather_facts: false\n  tasks:\n    - ansible.builtin.meta: clear_facts\n"
}

// runArgs are the arguments of ump's ansible-playbook run of the
// playbooks: theirs, UMP's key, the login, then the rest.
func runArgs(playbooks string, rest ...string) string {
	return strings.Join(append([]string{playbooks, "--private-key", keyPath(), "--extra-vars", umpLogin}, rest...), " ")
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
	_, errOut, code := e.run("", false, "provision", "--yes")
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
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": no ansible-playbook in PATH; install ansible-core\n") {
		t.Errorf("provision, no ansible-playbook: code %d, %q", code, errOut)
	}
	e.noAnsible = false
	// no playbook: the hint is the collection's base
	pbs := filepath.Join(e.repo, "playbooks")
	os.Rename(pbs, pbs+".off")
	os.MkdirAll(filepath.Join(pbs, "tasks"), 0o755)
	os.WriteFile(filepath.Join(pbs, "web.yaml"), []byte("- hosts: web\n"), 0o644)
	os.WriteFile(filepath.Join(pbs, "tasks", "x.yml"), []byte("- debug: {}\n"), 0o644)
	for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes", "--account", "one"}, {"provision", "--yes"}} {
		hint := ""
		if args[0] == "server" {
			hint = ", or give --no-provision"
		}
		if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": no playbooks/*.yml; create playbooks/00-base.yml with the line \"- import_playbook: ofzhur.ansible.base\""+hint+"\n") {
			t.Errorf("%v, no playbook: code %d, %q", args, code, errOut)
		}
	}
	os.RemoveAll(pbs)
	os.Rename(pbs+".off", pbs)
	// Ansible takes no ansible.cfg from a world-writable directory, and
	// ANSIBLE_CONFIG must not get past that
	if runtime.GOOS != "windows" {
		os.Chmod(e.repo, 0o777)
		for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes", "--account", "one"}, {"provision", "--yes"}} {
			if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, "ump: cannot provision from "+e.repo+": it is a world-writable directory, which Ansible ignores as an ansible.cfg source (anyone could have put one there); chmod o-w "+e.repo+"\n") {
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

	// --no-provision needs no layout: the server is installed, and has no
	// host file
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "web-01 is up:") || !strings.HasSuffix(errOut, "Not provisioned (--no-provision), and no host file for web-01 (no Ansible layout here): ump provision, run from the infrastructure repository, writes it and includes the server; where ump is the inventory, it is in it already.\n") {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if len(e.ansibleCalls) != 0 || e.hostFile("web-01.yml") != "" {
		t.Errorf("--no-provision: ansible %v, host file %q", e.ansibleCalls, e.hostFile("web-01.yml"))
	}
	// and it takes no narrowing of a provisioning
	for _, flag := range []string{"--hosts=web-01", "--playbook=00-base"} {
		if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", flag, "--account", "one"); code != 2 || !strings.Contains(errOut, "--hosts and --playbook narrow the provisioning, which --no-provision leaves out") {
			t.Errorf("--no-provision %s: code %d, %q", flag, code, errOut)
		}
	}
}

func TestReinstallProvisions(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(e.repo, "playbooks", "10-web.yml"), []byte("- hosts: web\n"), 0o644)
	if out, _, code := e.run("", false, "server", "set", "web-01", "--attr", "owner=alice", "--account", "one"); code != 0 {
		t.Fatalf("server set: %s", out)
	}

	// the playbooks are checked before anything happens, shown with the
	// plan, and run on the fleet once the new system is up and recorded
	resetFakePower()
	e.hostRecaps = map[string]hostRecap{"web-01": {OK: 5, Changed: 2}}
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--group", "db,web, common", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("reinstall: code %d, stderr %q\n%s", code, errOut, out)
	}
	var progs []string
	for _, c := range e.ansibleCalls {
		progs = append(progs, c.prog+" "+strings.Join(c.args, " "))
	}
	if strings.Join(progs, "\n") != strings.Join([]string{
		"ansible-playbook --syntax-check playbooks/00-base.yml playbooks/10-web.yml",
		"ansible-inventory --list",
		"ansible-inventory --list",
		"ansible-inventory --list",
		"ansible-playbook " + runArgs(importing()),
	}, "\n") {
		t.Fatalf("ansible runs:\n%s", strings.Join(progs, "\n"))
	}
	// the new system starts without the old one's facts
	if e.imported != "# The playbooks of a run of ump provision, which removes this file after it.\n"+clearFacts("web-01")+"- import_playbook: \"00-base.yml\"\n- import_playbook: \"10-web.yml\"\n" {
		t.Errorf("imported:\n%s", e.imported)
	}
	if _, err := os.Stat(filepath.Join(e.repo, importing())); err == nil {
		t.Errorf("%s left behind", importing())
	}
	check, run := e.ansibleCalls[0], e.ansibleCalls[4]
	if check.dir != e.repo || run.dir != e.repo || !slices.Equal(e.ansibleCalls[1].env, check.env) {
		t.Errorf("syntax check: %+v", check)
	}
	for _, want := range []string{"ANSIBLE_CONFIG=" + filepath.Join(e.repo, "ansible.cfg"), "ANSIBLE_SSH_COMMON_ARGS=-o StrictHostKeyChecking=accept-new", "ANSIBLE_SSH_EXTRA_ARGS=-o IdentitiesOnly=yes"} {
		if !slices.Contains(run.env, want) {
			t.Errorf("run env lacks %s: %v", want, run.env)
		}
	}
	plan := strings.Index(errOut, "Provisioning after the first boot: 00-base, 10-web on the fleet.\n")
	up := strings.Index(errOut, "web-01 is up:")
	wrote := strings.Index(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, web, db.\n")
	prov := strings.Index(errOut, "Provisioning: 00-base, 10-web on the fleet (1 host) ...\n")
	done := strings.Index(errOut, "PLAY RECAP *********\nweb-01                     : ok=5    changed=2    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0   \nweb-01 is provisioned. Provisioned 1 host, 1 changed.\n")
	if plan < 0 || up < plan || wrote < up || prov < wrote || done < prov {
		t.Errorf("progress (%d %d %d %d %d):\n%s", plan, up, wrote, prov, done, errOut)
	}
	for _, want := range []string{`"installed_at": "2026-10-12T09:00:00Z"`, `"groups": [
    "common",
    "web",
    "db"
  ],`, `"playbooks": [
    "00-base",
    "10-web"
  ],
  "provisioned": true,
  "provision": {
    "playbooks": [
      "00-base",
      "10-web"
    ],
    "hosts": {
      "web-01": {
        "ok": 5,
        "changed": 2,
        "unreachable": 0,
        "failed": 0,
        "skipped": 0,
        "rescued": 0,
        "ignored": 0
      }
    },
    "exit_status": 0
  },`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "run_list") {
		t.Errorf("result has a run list:\n%s", out)
	}
	host := e.hostFile("web-01.yml")
	if !strings.HasPrefix(host, "# web-01: written by ump (ump server reinstall, ump provision).\n# Edits are overwritten; overrides go to inventory/host_vars/web-01.yml.\nplugin: ofzhur.ansible.host\ngroups: [common, web, db]\nvars:\n  ansible_host: 203.0.113.10\n  ump_admin: ump\n  ump_name: web-01\n  ump_ref: fake/1\n  ump_provider: fake\n  ump_account: one\n  ump_location: eu-1\n  ump_product: m1\n  ump_os: ubuntu-26.04\n  ump_profile: ubuntu-26.04\n  ump_installed_at: \"2026-10-12T09:00:00Z\"\n  ump_private_address: 10.0.0.1\n  ump_private_network: default\n  ump_private_cidr: 10.0.0.0/24\n  ump_attributes:\n    owner: alice\n") {
		t.Errorf("host file:\n%s", host)
	}
	if st := e.readState(); !strings.Contains(st, "    groups:\n      - web\n      - db\n") {
		t.Errorf("state:\n%s", st)
	}
	// listed as in the inventory: common first; the servers ump did not
	// install have none
	out, _, _ = e.run("", false, "server", "list", "--account", "one")
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "NAME", "IPV4", "LOCATION", "STATUS", "OS", "GROUPS", "PRICE"},
		{"one", "fake", "db-01", "203.0.113.11", "eu-1", "off", "<none>", "<none>", "<none>"},
		{"one", "fake", "web-01", "203.0.113.10", "eu-1", "running", "ubuntu-26.04", "common,web,db", "<none>"},
	})
	if out, _, code := e.run("", false, "server", "show", "web-01", "--account", "one"); code != 0 || !strings.Contains(out, "Groups:") || !strings.Contains(out, "common, web, db") {
		t.Errorf("show: code %d\n%s", code, out)
	}

	// another server, another administrator: the fleet is both, each
	// logging in as its own (ump_admin), in one run
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
	data, _ := os.ReadFile(path)
	d, _ := state.Parse(data)
	d.Servers["fake/2"] = state.Server{Installed: &state.Install{OS: "debian-13", Profile: "debian-13", Hostname: "db-01", Admin: "ops", At: e.now.Add(-time.Hour)}, Groups: []string{"db"}}
	data, _ = d.Bytes()
	os.WriteFile(path, data, 0o600)
	e.ansibleCalls, e.hostRecaps = nil, nil
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.Contains(out, "groups:\n  - common\nprofile:") || !strings.Contains(out, "provisioned: true\n") || !strings.Contains(out, "  hosts:\n    db-01:\n      ok: 2\n") || !strings.Contains(out, "    web-01:\n      ok: 2\n") {
		t.Errorf("-o yaml, two servers: code %d, %q\n%s", code, errOut, out)
	}
	if !strings.Contains(errOut, "Wrote inventory/hosts/db-01.yml: db-01 in common, db.\nWrote inventory/hosts/web-01.yml: web-01 in common.\nProvisioning: 00-base, 10-web on the fleet (2 hosts) ...\n") || !strings.Contains(e.hostFile("db-01.yml"), "  ump_admin: ops\n  ump_name: db-01\n  ump_ref: fake/2\n") || !strings.Contains(e.hostFile("web-01.yml"), "  ump_admin: ump\n") {
		t.Errorf("two servers: %q\n%s", errOut, e.hostFile("db-01.yml"))
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs(importing()) || !strings.Contains(e.imported, clearFacts("web-01")) {
		t.Errorf("two servers, runs: %v\n%s", runs, e.imported)
	}

	// --hosts and --playbook narrow the provisioning
	e.ansibleCalls = nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--hosts", "web-01", "--playbook", "10-web", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Provisioning after the first boot: 10-web on web-01.\n") || !strings.Contains(errOut, "Provisioning: 10-web on web-01 (1 host) ...\n") {
		t.Errorf("narrowed: code %d, %q", code, errOut)
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs(importing(), "--limit", "web-01") || strings.Join(e.ansibleCalls[0].args, " ") != "--syntax-check playbooks/10-web.yml" {
		t.Errorf("narrowed, runs: %v", e.ansibleCalls)
	}
	if e.imported != "# The playbooks of a run of ump provision, which removes this file after it.\n"+clearFacts("web-01")+"- import_playbook: \"10-web.yml\"\n" {
		t.Errorf("narrowed, imported:\n%s", e.imported)
	}

	// a dry run checks and shows the playbooks and writes nothing
	os.RemoveAll(filepath.Join(e.repo, "inventory", "hosts"))
	e.ansibleCalls = nil
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--dry-run", "--group", "cache", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "Provisioning after the first boot: 00-base, 10-web on the fleet.\n") || !strings.Contains(out, `"provisioned": false`) || !strings.Contains(out, `"groups": [
    "common",
    "cache"
  ],`) || strings.Contains(out, "installed_at") || strings.Contains(out, `"provision"`) {
		t.Errorf("dry run: code %d, %q\n%s", code, errOut, out)
	}
	if len(e.ansibleCalls) != 2 || e.ansibleCalls[0].args[0] != "--syntax-check" || e.ansibleCalls[1].prog != "ansible-inventory" || e.hostFile("web-01.yml") != "" || strings.Contains(e.readState(), "cache") {
		t.Errorf("dry run: ansible %v, host file %q\n%s", e.ansibleCalls, e.hostFile("web-01.yml"), e.readState())
	}

	// --no-provision with the layout: the names checked, its host file
	// written, nothing runs
	e.ansibleCalls = nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--group", "web", "--account", "one")
	if code != 0 || !strings.HasSuffix(errOut, "Wrote inventory/hosts/web-01.yml: web-01 in common, web.\nNot provisioned (--no-provision); the next ump provision includes it (ump provision --hosts web-01 for it alone).\n") || strings.Contains(errOut, "Provisioning") {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	if len(e.ansibleCalls) != 1 || e.ansibleCalls[0].prog != "ansible-inventory" || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") || e.hostFile("db-01.yml") != "" {
		t.Errorf("--no-provision: ansible %v, host file %q", e.ansibleCalls, e.hostFile("web-01.yml"))
	}
	// the host file is written under the fleet's lock, and what stops it
	// keeps its exit status: the lock held by another run, a host file of
	// another server written meanwhile
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	b.Lock(context.Background(), "fleet", backend.LockInfo{Key: "fleet", Who: "bob@desk", What: "provision"})
	t.Setenv("UMP_WAIT", "100ms")
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--group", "db", "--account", "one", "-o", "json")
	if code != 3 || !strings.Contains(errOut, "ump: web-01 is installed and recorded, but not written into the inventory: waited 100ms: fleet is locked by bob@desk") || !strings.Contains(out, `"rebooted": true`) || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") {
		t.Errorf("--no-provision, fleet locked: code %d, %q\n%s", code, errOut, out)
	}
	b.Unlock(context.Background(), "fleet")
	e.installHook = func() {
		os.WriteFile(filepath.Join(e.repo, "inventory", "hosts", "web-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/2\n"), 0o644)
	}
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: web-01 is installed and recorded, but not written into the inventory: inventory/hosts/web-01.yml is the host file of fake/2 (its ump_ref); give another hostname, or remove the file if that server is gone\n") {
		t.Errorf("--no-provision, a host file in the way: code %d, %q", code, errOut)
	}
	e.installHook = nil
	os.WriteFile(filepath.Join(e.repo, "inventory", "hosts", "web-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/1\n"), 0o644)

	// another hostname: the server's old host file goes
	e.ansibleCalls = nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-01.example.net", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Removed inventory/hosts/web-01.yml: fake/1 has the hostname web-01.example.net now.\n") || e.hostFile("web-01.yml") != "" || !strings.Contains(e.hostFile("web-01.example.net.yml"), "ump_ref: fake/1\n") {
		t.Errorf("new hostname: code %d, %q", code, errOut)
	}
}

func TestReinstallProvisionFailures(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()

	// playbooks that do not pass the syntax check stop ump first, and so
	// does an unknown --playbook
	e.syntaxExit = 4
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "[ERROR]: the playbook: ofzhur.ansible.nope could not be found\nump: the playbooks 00-base do not pass ansible-playbook --syntax-check in "+e.repo+" (exit status 4, see above); nothing was changed\n") {
		t.Errorf("syntax check: code %d, %q", code, errOut)
	}
	e.noTouch(t, "syntax check")
	e.syntaxExit = 0
	e.ansibleCalls = nil
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--playbook", "web", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: --playbook web: there is no playbooks/web.yml (the playbooks: 00-base)\n") || len(e.ansibleCalls) != 0 {
		t.Errorf("unknown --playbook: code %d, %q", code, errOut)
	}
	e.noTouch(t, "unknown --playbook")

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
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web", "--group", "web", "--account", "one"); code != 2 || !strings.Contains(errOut, "the hostname web is also a group of the server") {
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
	// a group of the server: refused before anything happens
	e.inventory = `{"_meta": {"hostvars": {"db": {"ansible_host": "192.0.2.5"}}}, "all": {"children": ["ungrouped", "common", "prod"]}, "prod": {"children": ["web-01"]}, "ungrouped": {"hosts": ["db"]}}`
	for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--no-provision"}} {
		if _, errOut, code := e.run("", false, append(args, "--account", "one")...); code != 2 || !strings.HasSuffix(errOut, "ump: the hostname web-01 is also a group of the inventory: a host and a group with the same name make the group's plays match only the host; give another --hostname\n") {
			t.Errorf("%v, hostname is a group of the inventory: code %d, %q", args, code, errOut)
		}
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-02", "--group", "db", "--account", "one"); code != 2 || !strings.HasSuffix(errOut, "ump: the group db of web-02 is also a host of the inventory: a host and a group with the same name make the group's plays match only the host; rename the host, or use another group\n") {
		t.Errorf("group is a host of the inventory: code %d, %q", code, errOut)
	}
	e.noTouch(t, "inventory names")
	// --no-provision where the names cannot be checked: installed, with no
	// host file
	e.noAnsible, e.ansibleCalls = true, nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "web-01 is up:") || !strings.HasSuffix(errOut, "Not provisioned (--no-provision), and no host file for web-01 (no ansible-inventory in PATH to check the names): ump provision, run from the infrastructure repository, writes it and includes the server; where ump is the inventory, it is in it already.\n") {
		t.Errorf("--no-provision without ansible-inventory: code %d, %q", code, errOut)
	}
	e.noAnsible = false
	if runtime.GOOS != "windows" {
		os.Chmod(e.repo, 0o777)
		resetFakePower()
		_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
		if code != 0 || !strings.Contains(errOut, "and no host file for web-01 ("+e.repo+" is world-writable, Ansible takes no ansible.cfg from it)") {
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

	// the playbooks fail on the server: exit status 6, the server installed
	// and recorded, in the inventory, named
	e.hostRecaps = map[string]hostRecap{"web-01": {OK: 2, Failed: 1}}
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one", "-o", "json")
	if code != 6 || !strings.HasSuffix(errOut, "ump: web-01 is installed and recorded, but not provisioned: provisioning failed on web-01 (failed), see above; fix that and run ump provision --hosts web-01\n") {
		t.Errorf("failed run: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, `"provisioned": false`) || !strings.Contains(out, `"failed": [
      "web-01"
    ],
    "exit_status": 2`) || !strings.Contains(out, `"rebooted": true`) || !strings.Contains(e.readState(), "    installed:\n") || !strings.Contains(e.readState(), "      - web\n") || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") {
		t.Errorf("failed run: stdout %q, host file %q\n%s", out, e.hostFile("web-01.yml"), e.readState())
	}

	// the server is judged by its own line of the recap: provisioned,
	// while the run failed on another host (exit status 6, naming it), or
	// without a host to blame
	os.WriteFile(filepath.Join(hosts, "db-01.yml"), []byte("plugin: ofzhur.ansible.host\ngroups: [common, db]\nvars:\n  ansible_host: 192.0.2.5\n  ump_ref: fake/2\n"), 0o644)
	e.hostRecaps = map[string]hostRecap{"web-01": {OK: 4, Changed: 1}, "db-01": {OK: 1, Unreachable: 1}}
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one", "-o", "json")
	if code != 6 || !strings.HasSuffix(errOut, "ump: web-01 is provisioned; the run failed on db-01 (unreachable), see above; fix that and run ump provision --hosts db-01\n") || strings.Contains(errOut, "not provisioned") {
		t.Errorf("another host failed: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, `"provisioned": true`) || !strings.Contains(out, `"unreachable": [
      "db-01"
    ],
    "exit_status": 4`) {
		t.Errorf("another host failed: stdout %q", out)
	}
	e.hostRecaps, e.ansibleExit = nil, 4
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one"); code != 6 || !strings.HasSuffix(errOut, "ump: web-01 is provisioned; the run failed: ansible-playbook exit status 4, see above; fix that and run ump provision\n") {
		t.Errorf("no host to blame: code %d, %q", code, errOut)
	}
	e.ansibleExit = 0
	// not in the run: not provisioned, and the run did not fail
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--hosts", "db-01", "--account", "one", "-o", "json")
	if code != 0 || !strings.HasSuffix(errOut, "web-01 is not provisioned: no play of the run matched it. Provisioned 1 host, 0 changed.\n") || !strings.Contains(out, `"provisioned": false`) {
		t.Errorf("not in the run: code %d, %q\n%s", code, errOut, out)
	}
	os.Remove(filepath.Join(hosts, "db-01.yml"))

	// --hosts is checked against the inventory as it will be, with the
	// server in its new groups, before the question and the rescue system:
	// matching nothing, nothing is changed
	e.ansibleCalls, e.remoteCalls, e.sources = nil, nil, nil
	resetFakePower()
	_, errOut, code = e.run("y\n", true, "server", "reinstall", "web-01", "--hosts", "db*", "--group", "cache", "--account", "one")
	if code != 2 || !strings.HasSuffix(errOut, "ump: --hosts db* matches no host of the inventory with web-01 in common, cache; nothing was changed\n") || strings.Contains(errOut, "[y/N]") {
		t.Errorf("--hosts matching nothing: code %d, %q", code, errOut)
	}
	if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 || len(e.ansibleRuns()) != 0 || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, web]\n") || strings.Contains(e.readState(), "      - cache\n") {
		t.Errorf("--hosts matching nothing: remote %v, provider %v, runs %v, host file %q", e.remoteCalls, fakePower.actions, e.ansibleRuns(), e.hostFile("web-01.yml"))
	}
	// a group the server is not in yet matches it
	e.ansibleCalls, e.sources = nil, nil
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "cache", "--hosts", "cache", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Provisioning: 00-base on cache (1 host) ...\n") || !strings.Contains(errOut, "web-01 is provisioned. Provisioned 1 host, 0 changed.\n") {
		t.Errorf("--hosts of a new group: code %d, %q", code, errOut)
	}
	if len(e.sources) != 2 || !strings.Contains(e.sources[0], `"cache":{"hosts":{"web-01":null}}`) || !strings.Contains(e.sources[0], `"web":{"hosts":{"web-01":null}}`) || !strings.Contains(e.sources[1], `"cache":{"hosts":{"web-01":null}}`) {
		t.Errorf("--hosts of a new group: the inventories as they will be:\n%s", strings.Join(e.sources, "\n"))
	}
}

// TestMatchHostsAnsible matches patterns with the real ansible-inventory,
// where it is in PATH: in the inventory ump hands it, with the plugins of
// the repository's ansible.cfg left aside.
func TestMatchHostsAnsible(t *testing.T) {
	if _, err := exec.LookPath("ansible-inventory"); err != nil {
		t.Skip("no ansible-inventory in PATH")
	}
	t.Setenv("HOME", t.TempDir())
	repo := &infraRepo{dir: t.TempDir()}
	os.WriteFile(filepath.Join(repo.dir, "ansible.cfg"), []byte("[inventory]\nenable_plugins = host_list\n"), 0o644)
	inv, err := parseInventory([]byte(`{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped", "common", "prod"]},
		"common": {"hosts": ["db-01", "web-01"]}, "prod": {"children": ["web"]}, "web": {"hosts": ["web-01"]}, "ungrouped": {"hosts": ["lb"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	inv = inv.with(map[string]inventoryHost{"cache-01": {m: provider.Machine{Provider: "fake", ID: "3"}, groups: []string{"common", "cache"}}})
	var errb bytes.Buffer
	a := &app{stderr: &errb, ansible: runAnsible}
	for _, pattern := range []string{"prod", "all:!common", "cache", "~db-.*", "web[0]", "common:&prod", "*-01:!web*"} {
		if err := a.matchHosts(context.Background(), repo, inv, pattern, "none"); err != nil {
			t.Errorf("%s: %v\n%s", pattern, err, errb.String())
		}
	}
	for pattern, want := range map[string]string{
		"db":                "none",
		"prod:!web-01":      "none",
		"web[3]":            "--hosts web[3] does not pass ansible-inventory --limit (exit status 1, see above); nothing was changed",
		"lb:&common,!lb-01": "none",
	} {
		if err := a.matchHosts(context.Background(), repo, inv, pattern, "none"); err == nil || exitCodeFor(err) != exitUsage || err.Error() != want {
			t.Errorf("%s: %v", pattern, err)
		}
	}
}

// TestInventoryWith: the inventory as it will be is the current one plus
// the hosts in their groups, without the old hostname of a server among
// them; Ansible's yaml plugin reads it as a group tree.
func TestInventoryWith(t *testing.T) {
	inv, err := parseInventory([]byte(`{"_meta": {"hostvars": {"web-01": {"ump_ref": "fake/1"}, "old-02": {"ump_ref": "fake/2"}, "lb": {}}},
		"all": {"children": ["ungrouped", "common", "prod"]}, "common": {"hosts": ["old-02", "web-01"]}, "prod": {"children": ["web"]},
		"web": {"hosts": ["web-01"]}, "ungrouped": {"hosts": ["lb"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	future := inv.with(map[string]inventoryHost{
		"db-02":  {m: provider.Machine{Provider: "fake", ID: "2"}, groups: []string{"common", "db"}},
		"web-01": {m: provider.Machine{Provider: "fake", ID: "1"}, groups: []string{"common", "cache"}},
	})
	src, err := future.source()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"all":{"children":{"cache":null,"common":null,"db":null,"prod":null,"ungrouped":null}},"cache":{"hosts":{"web-01":null}},"common":{"hosts":{"db-02":null,"web-01":null}},"db":{"hosts":{"db-02":null}},"prod":{"children":{"web":null}},"ungrouped":{"hosts":{"lb":null}},"web":{"hosts":{"web-01":null}}}`
	if string(src) != want {
		t.Errorf("source:\n%s\nwant\n%s", src, want)
	}
	if _, ok := future.hosts["old-02"]; ok || future.hosts["db-02"] != "fake/2" || len(inv.members["common"]) != 2 || inv.groups["db"] {
		t.Errorf("hosts %v; the current inventory %v %v", future.hosts, inv.members, inv.groups)
	}
}

// fleetEnv is a test environment with two servers ump installed, web-01
// (by ump, administrator ump, in web) and db-01 (administrator ops, in
// db), and a playbook of the repository beside 00-base.
func fleetEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(e.repo, "playbooks", "10-web.yml"), []byte("- hosts: web\n"), 0o644)
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--group", "web", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
	data, _ := os.ReadFile(path)
	d, err := state.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d.Servers["fake/2"] = state.Server{Installed: &state.Install{OS: "debian-13", Profile: "debian-13", Hostname: "db-01", Admin: "ops", At: e.now.Add(-time.Hour)}, Groups: []string{"db"}}
	data, _ = d.Bytes()
	os.WriteFile(path, data, 0o600)
	e.ansibleCalls, e.remoteCalls = nil, nil
	resetFakePower()
	return e
}

func TestProvisionCommand(t *testing.T) {
	e := fleetEnv(t)

	// the whole fleet takes a confirmation, or --yes
	_, errOut, code := e.run("", false, "provision")
	if code != 3 || !strings.Contains(errOut, "ump provision needs confirmation: run it in a terminal or pass --yes") || len(e.ansibleRuns()) != 0 {
		t.Errorf("no --yes: code %d, %q", code, errOut)
	}
	e.ansibleCalls = nil
	_, errOut, code = e.run("n\n", true, "provision")
	if code != 3 || !strings.Contains(errOut, "Run 00-base, 10-web on every host of the inventory, the whole fleet? [y/N]") || len(e.ansibleRuns()) != 0 {
		t.Errorf("answered no: code %d, %q", code, errOut)
	}
	e.ansibleCalls = nil
	e.hostRecaps = map[string]hostRecap{"db-01": {OK: 3, Changed: 1}}
	out, errOut, code := e.run("y\n", true, "provision")
	if code != 0 || out != "Provisioned 2 hosts, 1 changed.\n" {
		t.Fatalf("provision: code %d, %q\n%s", code, out, errOut)
	}
	var progs []string
	for _, c := range e.ansibleCalls {
		progs = append(progs, c.prog+" "+strings.Join(c.args, " "))
	}
	if strings.Join(progs, "\n") != strings.Join([]string{
		"ansible-playbook --syntax-check playbooks/00-base.yml playbooks/10-web.yml",
		"ansible-inventory --list",
		"ansible-inventory --list",
		"ansible-playbook " + runArgs(importing()),
	}, "\n") {
		t.Errorf("ansible runs:\n%s", strings.Join(progs, "\n"))
	}
	// web-01, installed without provisioning, starts without its old
	// system's facts; db-01, whose record has no mark, keeps its own
	if e.imported != "# The playbooks of a run of ump provision, which removes this file after it.\n"+clearFacts("web-01")+"- import_playbook: \"00-base.yml\"\n- import_playbook: \"10-web.yml\"\n" {
		t.Errorf("imported:\n%s", e.imported)
	}
	// the host files of both, each with its administrator; no login of
	// the inventory's
	if !strings.Contains(errOut, "Wrote inventory/hosts/db-01.yml: db-01 in common, db.\nProvisioning: 00-base, 10-web on the fleet (2 hosts) ...\n") || strings.Contains(errOut, "Wrote inventory/hosts/web-01.yml") {
		t.Errorf("progress: %q", errOut)
	}
	for host, admin := range map[string]string{"web-01": "ump", "db-01": "ops"} {
		if f := e.hostFile(host + ".yml"); !strings.Contains(f, "  ump_admin: "+admin+"\n") || strings.Contains(f, "ansible_user") {
			t.Errorf("%s:\n%s", host, f)
		}
	}
	if len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("touched the servers: remote %v, provider %v", e.remoteCalls, fakePower.actions)
	}

	// --hosts runs on its hosts alone, without a question; -o json is
	// the recap per host; arguments after -- go to ansible-playbook
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "provision", "--hosts", "db", "--playbook", "00-base", "-o", "json", "--", "--check", "--diff")
	if code != 0 || out != `{
  "playbooks": [
    "00-base"
  ],
  "limit": "db",
  "hosts": {
    "db-01": {
      "ok": 3,
      "changed": 1,
      "unreachable": 0,
      "failed": 0,
      "skipped": 0,
      "rescued": 0,
      "ignored": 0
    }
  },
  "exit_status": 0
}
` {
		t.Errorf("--hosts db: code %d, %q\n%s", code, errOut, out)
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs("playbooks/00-base.yml", "--check", "--diff", "--limit", "db") || !strings.Contains(errOut, "Provisioning: 00-base on db (1 host) ...\n") {
		t.Errorf("--hosts db: runs %v, %q", runs, errOut)
	}
	// the hosts of --hosts, before the host files are written (in the
	// inventory as it will be, from a file of ump's, with the yaml plugin
	// alone) and after
	match := e.ansibleCalls[2]
	if len(match.args) != 5 || match.args[0] != "-i" || strings.HasPrefix(match.args[1], e.repo) || strings.Join(match.args[2:], " ") != "--list --limit db" || !slices.Contains(match.env, "ANSIBLE_INVENTORY_ENABLED=yaml") || strings.Join(e.ansibleCalls[3].args, " ") != "--list --limit db" {
		t.Errorf("--hosts db: the matches %v %v", match, e.ansibleCalls[3].args)
	}
	if _, err := os.Stat(match.args[1]); err == nil {
		t.Errorf("%s left behind", match.args[1])
	}

	// failures per host: exit status 6 naming them, the result still printed
	e.ansibleCalls = nil
	e.hostRecaps = map[string]hostRecap{"db-01": {OK: 1, Failed: 1}, "web-01": {Unreachable: 1}}
	out, errOut, code = e.run("", false, "provision", "--yes", "--playbook", "10-web,00-base", "-o", "yaml")
	if code != 6 || !strings.HasSuffix(errOut, "ump: provisioning failed on db-01 (failed), web-01 (unreachable), see above; fix that and run ump provision --hosts db-01,web-01 --playbook 00-base,10-web\n") {
		t.Errorf("failures: code %d, %q", code, errOut)
	}
	if !strings.Contains(out, "failed:\n  - db-01\nunreachable:\n  - web-01\nexit_status: 2\n") || !strings.Contains(out, "  web-01:\n    ok: 0\n    changed: 0\n    unreachable: 1\n") {
		t.Errorf("failures: -o yaml\n%s", out)
	}
	// a failure without a host to blame
	e.hostRecaps, e.ansibleExit = nil, 4
	if _, errOut, code := e.run("", false, "provision", "--hosts", "web*"); code != 6 || !strings.HasSuffix(errOut, "ump: provisioning failed: ansible-playbook exit status 4, see above; fix that and run ump provision --hosts 'web*'\n") {
		t.Errorf("exit status 4: code %d, %q", code, errOut)
	}
	e.ansibleExit = 0

	// --hosts that matches nothing: nothing is written, nothing runs
	e.ansibleCalls = nil
	os.Remove(filepath.Join(e.repo, "inventory", "hosts", "db-01.yml"))
	if _, errOut, code := e.run("", false, "provision", "--hosts", "lb-*,!db"); code != 2 || !strings.HasSuffix(errOut, "ump: --hosts lb-*,!db matches no host of the inventory; nothing ran\n") || len(e.ansibleRuns()) != 0 || e.hostFile("db-01.yml") != "" {
		t.Errorf("--hosts matching nothing: code %d, %q", code, errOut)
	}
	// it matches the hosts the run would write
	if _, errOut, code := e.run("", false, "provision", "--hosts", "db-01"); code != 0 || e.hostFile("db-01.yml") == "" {
		t.Errorf("--hosts of a host to write: code %d, %q", code, errOut)
	}

	// usage: an unknown playbook, an empty --hosts, arguments, and what
	// would go around the checks
	e.ansibleCalls = nil
	for args, want := range map[string]string{
		"--yes --playbook web":       "--playbook web: there is no playbooks/web.yml (the playbooks: 00-base, 10-web)",
		"--yes --playbook tasks/x":   `--playbook "tasks/x": a playbook is named by its file in playbooks/, without .yml`,
		"--hosts=":                   "--hosts is empty: give a host pattern, or leave it out for the whole fleet",
		"web-01":                     "ump provision takes no arguments; ansible-playbook's go after --",
		"web-01 -- -v":               "ump provision takes no arguments; ansible-playbook's go after --",
		"--yes -- -l web-01":         "-l after --: give the hosts with --hosts, which ump checks first",
		"--yes -- --limit=web-01":    "--limit=web-01 after --: give the hosts with --hosts, which ump checks first",
		"--yes -- -vl web-01":        "-vl after --: give the hosts with --hosts, which ump checks first",
		"--yes -- --lim web-01":      "--lim after --: give the hosts with --hosts, which ump checks first",
		"--yes -- -Cvi x.yml":        "-Cvi after --: ump runs on the repository's inventory, which it checks first",
		"--yes -- --inv=x.yml":       "--inv=x.yml after --: ump runs on the repository's inventory, which it checks first",
		"--yes -- -i hosts.ini":      "-i after --: ump runs on the repository's inventory, which it checks first",
		"--yes -- --inventory=x.yml": "--inventory=x.yml after --: ump runs on the repository's inventory, which it checks first",
		"--yes -o table":             `unknown output format "table" (use one of json, yaml)`,
	} {
		if _, errOut, code := e.run("", false, append([]string{"provision"}, strings.Fields(args)...)...); code != 2 || !strings.Contains(errOut, "ump: "+want+"\n") {
			t.Errorf("%s: code %d, %q", args, code, errOut)
		}
	}
	if len(e.ansibleCalls) != 0 {
		t.Errorf("usage errors ran Ansible: %v", e.ansibleCalls)
	}

	// a host file in the way of one server stops the run before a file is
	// written or a playbook runs
	os.Remove(filepath.Join(e.repo, "inventory", "hosts", "db-01.yml"))
	os.WriteFile(filepath.Join(e.repo, "inventory", "hosts", "db-01.yaml"), []byte("db-01:\n"), 0o644)
	os.WriteFile(filepath.Join(e.repo, "inventory", "hosts", "web-01.yml"), []byte("stale\n"), 0o644)
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: inventory/hosts/db-01.yaml has no ump_ref: written by hand or by another tool; remove it, ump writes inventory/hosts/db-01.yml\n") || len(e.ansibleRuns()) != 0 || e.hostFile("web-01.yml") != "stale\n" {
		t.Errorf("host file in the way: code %d, %q", code, errOut)
	}
	// ump server provision is gone
	if _, errOut, code := e.run("", false, "server", "provision", "web-01"); code != 2 || !strings.Contains(errOut, "ump: ump server provision is ump provision now: it runs the playbooks on the fleet, or with --hosts HOSTNAME on one server; ump server set NAME --group GROUP adds a group\n") {
		t.Errorf("server provision: code %d, %q", code, errOut)
	}
}

// TestFleetLock: one provisioning at a time. The fleet's lock is held
// while Ansible runs; another run waits for it up to the configured
// wait, and gives up with exit status 3, as for a server's lock.
func TestFleetLock(t *testing.T) {
	e := fleetEnv(t)
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	var held []backend.LockInfo
	e.onRun = func() { held, _ = b.Locks(context.Background()) }
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || len(held) != 1 || held[0].Key != "fleet" || held[0].What != "provision" {
		t.Fatalf("lock while running: code %d, %q, %+v", code, errOut, held)
	}
	if locks, _ := b.Locks(context.Background()); len(locks) != 0 {
		t.Errorf("lock left: %+v", locks)
	}
	if out, _, code := e.run("", false, "config", "state", "locks"); code != 0 || out != "LOCK   OPERATION   HELD BY   SINCE\n" {
		t.Errorf("locks: %q", out)
	}
	// a reinstall's provisioning takes it too, and the server's lock
	held = nil
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 0 || len(held) != 2 || held[0].Key != "fake/1" || held[0].What != "reinstall" || held[1].Key != "fleet" || held[1].What != "provision after reinstalling web-01" {
		t.Fatalf("reinstall: code %d, %q, %+v", code, errOut, held)
	}

	// held by another: waited for up to the configured wait
	t.Setenv("UMP_WAIT", "300ms")
	since := time.Now().UTC().Truncate(time.Second)
	if err := b.Lock(context.Background(), "fleet", backend.LockInfo{Key: "fleet", Who: "bob@desk (pid 7)", What: "provision", Since: since}); err != nil {
		t.Fatal(err)
	}
	e.ansibleCalls = nil
	_, errOut, code := e.run("", false, "provision", "--hosts", "web-01")
	if code != 3 || !strings.Contains(errOut, "Waiting for the fleet's lock, held by bob@desk (pid 7) since ") || !strings.Contains(errOut, " (provision), up to 300ms ...\nump: waited 300ms: fleet is locked by bob@desk (pid 7) since "+since.Format(time.RFC3339)+" (provision); ump config state unlock fleet forces it\n") {
		t.Errorf("held: code %d, %q", code, errOut)
	}
	if len(e.ansibleRuns()) != 0 || len(e.ansibleCalls) != 1 {
		t.Errorf("held: ansible %v", e.ansibleCalls)
	}
	if out, _, code := e.run("", false, "config", "state", "locks", "--no-headers"); code != 0 || !strings.HasPrefix(out, "fleet   provision   bob@desk (pid 7)   ") {
		t.Errorf("locks: %q", out)
	}
	// released while waiting: the run goes on
	t.Setenv("UMP_WAIT", "1m")
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.Unlock(context.Background(), "fleet")
	}()
	if out, errOut, code := e.run("", false, "provision", "--hosts", "web-01"); code != 0 || !strings.Contains(errOut, " (provision), up to 1m ... free after 0s.\n") || out != "Provisioned 1 host, 0 changed.\n" {
		t.Errorf("released: code %d, %q, %q", code, out, errOut)
	}
	// ump config state unlock takes the fleet's lock too
	b.Lock(context.Background(), "fleet", backend.LockInfo{Key: "fleet", Who: "bob@desk", What: "provision"})
	if out, _, code := e.run("", false, "config", "state", "unlock", "fleet"); code != 0 || out != "Unlocked fleet.\n" {
		t.Errorf("unlock: code %d, %q", code, out)
	}
	if locks, _ := b.Locks(context.Background()); len(locks) != 0 {
		t.Errorf("unlocked: %+v", locks)
	}
}

func TestServerSetGroups(t *testing.T) {
	e := fleetEnv(t)
	set := func(args ...string) (string, string, int) {
		return e.run("", false, append(append([]string{"server", "set"}, args...), "--account", "one")...)
	}
	out, errOut, code := set("web-01", "--group", "monitoring,web", "--group", "backup")
	if code != 0 || out != "fake/1 (web-01, 203.0.113.10): groups common, web, monitoring, backup (ump provision applies them).\n" {
		t.Errorf("--group: code %d, %q, %q", code, out, errOut)
	}
	out, _, code = set("web-01", "--ungroup", "web", "--ungroup", "backup", "--attr", "owner=alice")
	if code != 0 || out != "fake/1 (web-01, 203.0.113.10): attributes owner=alice; groups common, monitoring (ump provision applies them).\n" {
		t.Errorf("--ungroup: code %d, %q", code, out)
	}
	if st := e.readState(); !strings.Contains(st, "    groups:\n      - monitoring\n") {
		t.Errorf("state:\n%s", st)
	}
	for args, want := range map[string]string{
		"web-01 --ungroup db":            "ump: web-01 is not in the group db (its groups: common, monitoring)\n",
		"web-01 --ungroup common":        "ump: --ungroup common: every server ump installed is in common\n",
		"web-01 --group db --ungroup db": "ump: --group and --ungroup both name db\n",
		"web-01 --group all":             "ump: group \"all\": all, ungrouped, localhost and _meta are Ansible's own names\n",
		"web-01 --ungroup Web":           "ump: group \"Web\": a group name is lower-case letters, digits and '_', not starting with a digit\n",
	} {
		if _, errOut, code := set(strings.Fields(args)...); code != 2 || !strings.Contains(errOut, want) {
			t.Errorf("%s: code %d, %q", args, code, errOut)
		}
	}
	// a server ump did not install has no groups to change, nor anything
	// else of the same call
	if _, errOut, code := e.run("", false, "server", "add", "lab-01", "--ipv4", "192.0.2.7"); code != 0 {
		t.Fatalf("add: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "set", "lab-01", "--group", "web", "--attr", "owner=bob"); code != 2 || !strings.HasSuffix(errOut, "ump: ump did not install lab-01: its groups come with the installation (ump server reinstall lab-01 --group GROUP)\n") {
		t.Errorf("not installed: code %d, %q", code, errOut)
	}
	if st := e.readState(); !strings.Contains(st, "    groups:\n      - monitoring\n") || strings.Contains(st, "bob") {
		t.Errorf("state after the refusals:\n%s", st)
	}
	if len(e.ansibleRuns()) != 0 {
		t.Errorf("Ansible ran: %v", e.ansibleRuns())
	}

	// a group is no host's name: not another host of the inventory
	// (ansible-inventory, where the repository is at hand), nor the
	// server's hostname
	lb := filepath.Join(e.repo, "inventory", "hosts", "lb.yml")
	os.WriteFile(lb, []byte("plugin: ofzhur.ansible.host\ngroups: [edge]\n"), 0o644)
	e.ansibleCalls = nil
	if _, errOut, code := set("web-01", "--group", "backup,lb"); code != 2 || !strings.HasSuffix(errOut, "ump: --group lb is a host of the inventory: a host and a group with the same name make the group's plays match only the host; rename the host, or use another group\n") {
		t.Errorf("a host of the inventory: code %d, %q", code, errOut)
	}
	if len(e.ansibleCalls) != 1 || e.ansibleCalls[0].prog != "ansible-inventory" {
		t.Errorf("a host of the inventory: ansible %v", e.ansibleCalls)
	}
	hostname := func(name string) {
		path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
		data, _ := os.ReadFile(path)
		d, _ := state.Parse(data)
		d.Servers["fake/2"].Installed.Hostname = name
		data, _ = d.Bytes()
		os.WriteFile(path, data, 0o600)
	}
	hostname("db")
	if _, errOut, code := set("db-01", "--group", "backup,db"); code != 2 || !strings.HasSuffix(errOut, "ump: --group db is the hostname of db-01: a host and a group with the same name make the group's plays match only the host\n") {
		t.Errorf("the hostname: code %d, %q", code, errOut)
	}
	// without the repository, the hostname alone
	e.noAnsible, e.ansibleCalls = true, nil
	if _, errOut, code := set("db-01", "--group", "db"); code != 2 || !strings.Contains(errOut, "ump: --group db is the hostname of db-01: ") {
		t.Errorf("the hostname, no repository: code %d, %q", code, errOut)
	}
	if out, errOut, code := set("web-01", "--group", "lb"); code != 0 || out != "fake/1 (web-01, 203.0.113.10): groups common, monitoring, lb (ump provision applies them).\n" || len(e.ansibleCalls) != 0 {
		t.Errorf("no repository: code %d, %q, %q", code, out, errOut)
	}
	e.noAnsible = false
	// a group of the state that cannot be stops the next run, which says
	// how to take it away
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: the hostname db is also a group of the server: a host and a group with the same name make the group's plays match only the host; ump server set db-01 --ungroup db, or reinstall db-01 with another --hostname\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("the hostname as a group of the state: code %d, %q", code, errOut)
	}
	hostname("db-01")
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: the group lb of web-01 is also a host of the inventory: a host and a group with the same name make the group's plays match only the host; rename the host, or ump server set web-01 --ungroup lb\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("a host as a group of the state: code %d, %q", code, errOut)
	}
	os.Remove(lb)
	if _, errOut, code := set("web-01", "--ungroup", "lb"); code != 0 {
		t.Fatalf("--ungroup lb: code %d, %q", code, errOut)
	}

	// the next provisioning puts it into them
	if _, _, code := e.run("", false, "provision", "--hosts", "web-01"); code != 0 || !strings.Contains(e.hostFile("web-01.yml"), "groups: [common, monitoring]\n") {
		t.Errorf("provision: code %d\n%s", code, e.hostFile("web-01.yml"))
	}
}

// TestStaleFacts: an installation marks the server's facts stale in the
// state; a run clears the facts of the stale hosts it has, and takes the
// mark away from those it completed on. The others keep it.
func TestStaleFacts(t *testing.T) {
	e := fleetEnv(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
	stale := func() (out []string) {
		data, _ := os.ReadFile(path)
		d, err := state.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range slices.Sorted(maps.Keys(d.Servers)) {
			if d.Servers[ref].StaleFacts {
				out = append(out, ref)
			}
		}
		return out
	}
	data, _ := os.ReadFile(path)
	d, _ := state.Parse(data)
	db := d.Servers["fake/2"]
	db.StaleFacts = true
	d.Servers["fake/2"] = db
	data, _ = d.Bytes()
	os.WriteFile(path, data, 0o600)
	if s := stale(); strings.Join(s, " ") != "fake/1 fake/2" {
		t.Fatalf("stale: %v", s)
	}
	header := "# The playbooks of a run of ump provision, which removes this file after it.\n"
	imports := "- import_playbook: \"00-base.yml\"\n- import_playbook: \"10-web.yml\"\n"

	// --hosts: the stale hosts of the run alone
	if _, errOut, code := e.run("", false, "provision", "--hosts", "db-01"); code != 0 || e.imported != header+clearFacts("db-01")+imports {
		t.Errorf("--hosts db-01: code %d, %q\n%s", code, errOut, e.imported)
	}
	if s := stale(); strings.Join(s, " ") != "fake/1" {
		t.Errorf("after --hosts db-01, stale: %v", s)
	}
	// a host that failed keeps the mark
	e.hostRecaps = map[string]hostRecap{"web-01": {OK: 1, Failed: 1}}
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 6 || e.imported != header+clearFacts("web-01")+imports {
		t.Errorf("failed: code %d, %q\n%s", code, errOut, e.imported)
	}
	if s := stale(); strings.Join(s, " ") != "fake/1" {
		t.Errorf("after a failure, stale: %v", s)
	}
	// and loses it once a run completed on it
	e.hostRecaps = nil
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || e.imported != header+clearFacts("web-01")+imports {
		t.Errorf("completed: code %d, %q\n%s", code, errOut, e.imported)
	}
	if s := stale(); len(s) != 0 {
		t.Errorf("after the run, stale: %v", s)
	}
	if _, _, code := e.run("", false, "provision", "--yes"); code != 0 || e.imported != header+imports {
		t.Errorf("nothing stale: code %d\n%s", code, e.imported)
	}
}

// TestHostnameGroupOfAnother: a hostname that is a group of another
// server in the state stops a reinstall before the question, and ump
// provision before a host file is written, naming both servers.
func TestHostnameGroupOfAnother(t *testing.T) {
	e := fleetEnv(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml")
	edit := func(f func(d *state.Document)) {
		data, _ := os.ReadFile(path)
		d, err := state.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		f(d)
		data, _ = d.Bytes()
		os.WriteFile(path, data, 0o600)
	}
	edit(func(d *state.Document) {
		db := d.Servers["fake/2"]
		db.Groups = []string{"db", "web1"}
		d.Servers["fake/2"] = db
	})
	_, errOut, code := e.run("y\n", true, "server", "reinstall", "web-01", "--hostname", "web1", "--no-provision", "--account", "one")
	if code != 2 || !strings.HasSuffix(errOut, "ump: the hostname web1 is also a group of fake/2 in the state: "+sameName+"; give another --hostname, or ump server set fake/2 --ungroup web1\n") || strings.Contains(errOut, "[y/N]") {
		t.Errorf("reinstall: code %d, %q", code, errOut)
	}
	if len(e.remoteCalls) != 0 || len(e.sshCalls) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("reinstall: touched the server: remote %v, ssh %v, provider %v", e.remoteCalls, e.sshCalls, fakePower.actions)
	}

	// a hostname recorded before the group
	edit(func(d *state.Document) { d.Servers["fake/1"].Installed.Hostname = "web1" })
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: the hostname web1 of web-01 is also a group of db-01: "+sameName+"; ump server set db-01 --ungroup web1, or reinstall web-01 with another --hostname\n") || len(e.ansibleRuns()) != 0 || e.hostFile("web1.yml") != "" || e.hostFile("db-01.yml") != "" {
		t.Errorf("provision: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "set", "db-01", "--ungroup", "web1", "--account", "one"); code != 0 {
		t.Fatalf("--ungroup web1: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || e.hostFile("web1.yml") == "" || !strings.Contains(e.hostFile("db-01.yml"), "groups: [common, db]\n") {
		t.Errorf("provision after --ungroup: code %d, %q", code, errOut)
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
	for _, c := range []struct {
		hostname, ref string
		groups        []string
		err           string
	}{
		{"web-01", "fake/1", []string{"common", "web"}, ""},
		// an empty group is a group too
		{"dbs", "fake/2", []string{"common"}, "the hostname dbs is also a group of the inventory"},
		{"prod", "fake/2", []string{"common"}, "the hostname prod is also a group of the inventory"},
		{"web-02", "fake/2", []string{"common", "web", "db"}, "the group db of web-02 is also a host of the inventory"},
		{"web-02", "fake/2", []string{"common", "lb_01", "web-01"}, "the group web-01 of web-02 is also a host of the inventory"},
		// the server's own host under its old hostname goes when the new one is written
		{"web-02", "fake/1", []string{"common", "web-01"}, ""},
	} {
		err := inv.check(c.hostname, c.ref, c.groups, "give another --hostname", "")
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
// that ump provision after the reboot puts in the new system: its
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
	if code != 0 || !strings.HasSuffix(errOut, "Not provisioned: after the reboot, ump provision --hosts web-02 runs the playbooks on it. It is in the inventory from its record on: in ump's, and in the host files from the next ump provision, which finds it unreachable until then.\n") {
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
	if st := e.readState(); !strings.Contains(st, "      hostname: web-02\n      admin: ops\n      at: 2026-10-12T10:00:00Z\n    groups:\n      - db\n    private:\n      network: default\n      cidr: 10.0.0.0/24\n      address: 10.0.0.1\n    stale_facts: true\n") {
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
	out, errOut, code = e.run("", false, "provision", "--hosts", "web-02")
	if code != 0 || out != "Provisioned 1 host, 0 changed.\n" || !strings.Contains(errOut, "Removed inventory/hosts/web-01.yml: fake/1 has the hostname web-02 now.\nWrote inventory/hosts/web-02.yml: web-02 in common, db.\n") {
		t.Errorf("provision: code %d, %q\n%s", code, out, errOut)
	}
	// the new system starts without the old one's facts, which are not
	// stale once the run completed on it
	if runs := e.ansibleRuns(); len(runs) != 1 || runs[0] != runArgs(importing(), "--limit", "web-02") {
		t.Errorf("provision runs: %v", runs)
	}
	if e.imported != "# The playbooks of a run of ump provision, which removes this file after it.\n"+clearFacts("web-02")+"- import_playbook: \"00-base.yml\"\n" {
		t.Errorf("imported:\n%s", e.imported)
	}
	if st := e.readState(); strings.Contains(st, "stale_facts") {
		t.Errorf("state after the run:\n%s", st)
	}
	host := e.hostFile("web-02.yml")
	if !strings.Contains(host, "groups: [common, db]\n") || !strings.Contains(host, "  ump_admin: ops\n") || !strings.Contains(host, "  ump_installed_at: \"2026-10-12T10:00:00Z\"\n") || e.hostFile("web-01.yml") != "" {
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
	if plugins, pointers := inventoryFiles(dir); len(plugins) != 0 || len(pointers) != 0 {
		t.Errorf("no plugin file: %v %v", plugins, pointers)
	}
	write("ump.yaml", "# ump is the inventory\nplugin: ofzhur.ansible.ump\n")
	if plugins, pointers := inventoryFiles(dir); !slices.Equal(plugins, []string{"inventory/ump.yaml"}) || len(pointers) != 0 {
		t.Errorf("plugin file: %v %v", plugins, pointers)
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
        "ump_admin": "ump",
        "ump_name": "web-01",
        "ump_ref": "fake/1",
        "ump_provider": "fake",
        "ump_account": "one",
        "ump_location": "eu-1",
        "ump_product": "m1",
        "ump_os": "ubuntu-26.04",
        "ump_profile": "ubuntu-26.04",
        "ump_installed_at": "2026-10-12T09:00:00Z",
        "ump_private_address": "10.0.0.1",
        "ump_private_network": "default",
        "ump_private_cidr": "10.0.0.0/24",
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
	// --cached serves the same, without a note; a server ump did not
	// install is not in it
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
	if code != 0 || !strings.Contains(out, `"ump_ref": "fake/2"`) || !strings.Contains(out, `"ump_admin": "ops"`) || strings.Contains(out, `"fake/1"`) || errOut != "ump: fake/1 (web-01, 203.0.113.10) is left out of the inventory: its hostname web-01 is that of fake/2, installed later\n" {
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
// write no host file and check and run as before; a host file of a
// server beside it, or its hostname as another host of the inventory,
// stops them first.
func TestUmpIsTheInventory(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(e.repo, "inventory", "ump.yml"), []byte("plugin: ofzhur.ansible.ump\n"), 0o644)
	hosts := filepath.Join(e.repo, "inventory", "hosts")
	// the fake ansible-inventory lists what ump would
	e.inventory = `{"_meta": {"hostvars": {"web-01": {"ump_ref": "fake/1", "ump_admin": "ump"}}}, "all": {"children": ["ungrouped", "common", "web"]}, "common": {"hosts": ["web-01"]}, "web": {"hosts": ["web-01"]}}`

	resetFakePower()
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"provisioned": true`) {
		t.Fatalf("reinstall: code %d, %q\n%s", code, errOut, out)
	}
	up := strings.Index(errOut, "web-01 is up:")
	none := strings.Index(errOut, "No host files: inventory/ump.yml makes ump the inventory.\nProvisioning: 00-base on the fleet (1 host) ...\n")
	if up < 0 || none < up || strings.Contains(errOut, "Wrote ") {
		t.Errorf("progress:\n%s", errOut)
	}
	if _, err := os.Stat(hosts); err == nil {
		t.Errorf("inventory/hosts/ written")
	}
	if len(e.ansibleCalls) != 5 || e.ansibleCalls[0].args[0] != "--syntax-check" || e.ansibleCalls[1].prog != "ansible-inventory" || len(e.ansibleRuns()) != 1 {
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

	// provision: no host file either, the playbooks run
	e.ansibleCalls = nil
	out, errOut, code = e.run("", false, "provision", "--hosts", "web")
	if code != 0 || out != "Provisioned 1 host, 0 changed.\n" || !strings.Contains(errOut, "No host files: inventory/ump.yml makes ump the inventory.\nProvisioning: 00-base on web (1 host) ...\n") || len(e.ansibleRuns()) != 1 {
		t.Errorf("provision: code %d, %q\n%s", code, errOut, out)
	}
	// --no-provision: in the inventory through ump, nothing runs
	resetFakePower()
	e.ansibleCalls = nil
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "No host file: inventory/ump.yml makes ump the inventory, with web-01 in common.\nNot provisioned (--no-provision); the next ump provision includes it (ump provision --hosts web-01 for it alone).\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}
	// a host file of another name and server is no concern
	os.MkdirAll(hosts, 0o755)
	os.WriteFile(filepath.Join(hosts, "lb-01.yml"), []byte("plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/9\n"), 0o644)
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 {
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
		for _, args := range [][]string{{"server", "reinstall", "web-01", "--yes", "--account", "one"}, {"server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"}, {"provision", "--yes"}} {
			e.remoteCalls, e.ansibleCalls = nil, nil
			resetFakePower()
			if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, "ump: "+f.want+"\n") {
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

	// the installation cannot be recorded: nothing runs against the old
	// record, which the inventory would have
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
		{[]string{"--group", "db"}, 6, "ump: provisioning failed: web-03 is installed but not recorded, and the inventory comes from the record; record it and run ump provision --hosts web-03\n"},
		{[]string{"--no-provision"}, 0, "Not provisioned (--no-provision), nor in the inventory as installed: the inventory comes from the record; record it.\n"},
	} {
		resetFakePower()
		e.ansibleCalls = nil
		out, errOut, code := e.run("", false, append([]string{"server", "reinstall", "web-01", "--yes", "--hostname", "web-03", "--account", "one", "-o", "json"}, c.args...)...)
		os.Chmod(home, 0o700)
		if code != c.code || !strings.HasSuffix(errOut, c.want) || len(e.ansibleRuns()) != 0 || !strings.Contains(out, `"provisioned": false`) {
			t.Errorf("%v, not recorded: code %d, %q\n%s", c.args, code, errOut, out)
		}
		if !strings.Contains(errOut, "ump: the installation is not recorded in the state: ") || !strings.Contains(errOut, "Record it by hand: in state.yaml (ump config state view says where), the server's installed, groups and stale_facts are these:\nservers:\n  fake/1:\n    installed:\n      os: ubuntu-26.04\n") || !strings.Contains(errOut, "      hostname: web-03\n      admin: ump\n") || !strings.Contains(errOut, "    stale_facts: true\n") {
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
// server reinstall and ump provision: ump runs Ansible (the scripts
// above, with the real runner), and Ansible runs ump. The child sees the
// server its parent recorded before the run, with the parent's
// configuration and state, waits for no lock, and leaves the mirror
// alone: with the local home, with a backend, and with a backend that
// did not answer the parent but answers the child, which must still read
// the mirror the parent wrote to.
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
	// inventory (before the installation, for a reinstall), the inventory
	// under the fleet's lock, the hosts of --hosts in the inventory as it
	// will be (limited), its hosts, and the playbooks, each with a child
	// that answered without a word, under parent, and the playbooks' child
	// listing hostname in groups, as the server ref
	check := func(what, parent, hostname, ref string, groups []string, reinstall, limited bool) {
		t.Helper()
		runs := nestedRuns(t, log)
		want := []string{"ansible-playbook", "ansible-inventory", "ansible-inventory", "ansible-playbook"}
		if limited {
			want = []string{"ansible-playbook", "ansible-inventory", "ansible-inventory", "ansible-inventory", "ansible-playbook"}
		}
		if reinstall {
			want = append([]string{"ansible-playbook", "ansible-inventory"}, want[1:]...)
		}
		if len(runs) != len(want) {
			t.Fatalf("%s: runs %+v", what, runs)
		}
		for i, r := range runs {
			if !strings.HasPrefix(r.note, want[i]+" ") {
				t.Errorf("%s: run %d is %q, want %s", what, i, r.note, want[i])
			}
			if want := parent + " 0 lock=free mirror=kept"; !strings.HasSuffix(r.note, " "+want) || r.stderr != "" || r.inv == nil {
				t.Errorf("%s: %q, want %q; stderr %q", what, r.note, want, r.stderr)
			}
		}
		run := runs[len(runs)-1]
		if run.inv == nil || run.inv.hosts[hostname] != ref {
			t.Fatalf("%s: the playbooks' inventory lacks %s: %+v", what, hostname, run.inv)
		}
		for _, g := range append([]string{"common"}, groups...) {
			if !slices.Contains(run.groups[g], hostname) {
				t.Errorf("%s: %s not in %s: %v", what, hostname, g, run.groups)
			}
		}
		if len(run.groups) != len(groups)+1 || run.vars[hostname]["ansible_host"] != "203.0.113.10" || run.vars[hostname]["ump_admin"] != "ump" {
			t.Errorf("%s: groups %v, vars %v", what, run.groups, run.vars)
		}
	}

	// the local home
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web"); code != 0 || !strings.Contains(errOut, "No host files: inventory/ump.yml makes ump the inventory.\n") {
		t.Fatalf("reinstall, local: code %d, %q", code, errOut)
	}
	check("local", "local", "web-01", "fake/1", []string{"web"}, true, false)

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
	check("backend", "backend", "web-01", "fake/1", []string{"web"}, true, false)
	if data, _ := os.ReadFile(filepath.Join(backend, "state.yaml")); !strings.Contains(string(data), "hostname: web-01") {
		t.Errorf("backend state:\n%s", data)
	}
	// a group set before the run is in the child's inventory; a lock of
	// the mirror held meanwhile does not stop the child
	if _, errOut, code := e.run("", false, "server", "set", "web-01", "--group", "monitoring"); code != 0 {
		t.Fatalf("set, backend: code %d, %q", code, errOut)
	}
	if runs := nestedRuns(t, log); len(runs) != 1 || runs[0].note != "ansible-inventory backend 0 lock=free mirror=kept" {
		t.Errorf("set, backend: runs %+v", runs)
	}
	t.Setenv("UMP_TEST_HOLD", "1")
	if _, errOut, code := e.run("", false, "provision", "--hosts", "web-01"); code != 0 {
		t.Fatalf("provision, backend: code %d, %q", code, errOut)
	}
	check("provision", "backend", "web-01", "fake/1", []string{"web", "monitoring"}, false, true)
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
	check("mirror", "mirror", "web-09", "fake/1", []string{"db"}, true, false)
	if data, _ := os.ReadFile(filepath.Join(backend, "state.yaml")); strings.Contains(string(data), "web-09") {
		t.Errorf("the mirror's state reached the backend:\n%s", data)
	}
}
