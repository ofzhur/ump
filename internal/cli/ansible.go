package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// Provisioning: ump runs Ansible from the infrastructure repository, the
// current directory. Every server ump installs is in the group common and
// in the groups given with --group. Every playbook of the repository's
// playbooks/ runs, in file-name order, on the hosts its plays name: a
// group needs no playbook, and a playbook picks its hosts. The servers
// go into the inventory one of two ways: ump writes a host file per
// server into inventory/hosts/ (read by the collection's inventory plugin
// ofzhur.ansible.host), or, when a file of inventory/ configures the
// plugin ofzhur.ansible.ump, ump itself is the inventory (ump server list
// -o ansible) and writes nothing. Either way the inventory has the same
// hosts, groups and variables, and ump runs the playbooks over all of
// it, or over the hosts of a pattern.

const (
	// commonGroup is the group of every server ump installs.
	commonGroup = "common"
	// collection is the Ansible collection with the inventory plugins and
	// the playbooks the repository imports.
	collection = "ofzhur.ansible"
	// hostsDir is where the host files go, in the repository.
	hostsDir = "inventory/hosts"
	// playbooksDir holds the playbooks ump runs, in the repository.
	playbooksDir = "playbooks"
	// basePlaybook is how a repository without playbooks starts.
	basePlaybook = `playbooks/00-base.yml with the line "- import_playbook: ofzhur.ansible.base"`
	// umpPlugin is the inventory plugin that makes ump the inventory.
	umpPlugin = collection + ".ump"
	// parentEnv names, in the environment of the Ansible ump runs, where
	// ump reads the state (backend, mirror or local): an ump that Ansible
	// runs in turn is part of the same run.
	parentEnv = "UMP_PARENT"
)

// umpLogin is the extra variable that makes ump's runs log in to every
// host as its administrator, ump_admin, and to a host without one (ump
// did not install it) as the inventory says. ssh takes ansible_ssh_user
// over ansible_user, so it can fall back to ansible_user, which
// ansible_user itself could not; none leaves the login to Ansible.
// People's own runs, without it, log in as themselves.
const umpLogin = `{"ansible_ssh_user": "{{ ump_admin | default(ansible_user) | default(none) }}"}`

// ansibleRunner runs an Ansible program (ansible-playbook,
// ansible-inventory) with args in dir, with env added to the
// environment, and returns its exit status; the context ends it.
type ansibleRunner func(ctx context.Context, dir, prog string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// runAnsible is the real ansibleRunner.
func runAnsible(ctx context.Context, dir, prog string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	path, err := exec.LookPath(prog)
	if err != nil {
		return 0, fmt.Errorf("no %s found in PATH", prog)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	// the environment with PWD set to dir, but for an inventory of its
	// own: the repository's ansible.cfg names it; later entries win
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "ANSIBLE_INVENTORY" || name == "ANSIBLE_HOSTS"
	})
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// infraRepo is the infrastructure repository ump provisions from.
type infraRepo struct {
	// dir is its root, absolute: where ump runs.
	dir string
	// plugin is the file of inventory/ that makes ump the inventory
	// (inventory/ump.yml), relative to dir; empty: host files.
	plugin string
}

// infraLayout looks for the Ansible layout in the current directory:
// ansible.cfg and inventory/. A layout that is not there is no error:
// missing says what is missing.
func (a *app) infraLayout() (repo *infraRepo, missing []string, err error) {
	dir := a.repoDir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, nil, err
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, nil, err
	}
	if fi, err := os.Stat(filepath.Join(dir, "ansible.cfg")); err != nil || !fi.Mode().IsRegular() {
		missing = append(missing, "ansible.cfg")
	}
	repo = &infraRepo{dir: dir}
	if fi, err := os.Stat(filepath.Join(dir, "inventory")); err != nil || !fi.IsDir() {
		missing = append(missing, "inventory/")
	} else {
		repo.plugin = umpInventory(dir)
	}
	return repo, missing, nil
}

// umpInventory returns the file directly in the repository's inventory/
// that makes ump the inventory, a YAML mapping with plugin:
// ofzhur.ansible.ump, relative to the repository; empty when there is
// none. Ansible skips dot names, and so does ump.
func umpInventory(dir string) string {
	entries, err := os.ReadDir(filepath.Join(dir, "inventory"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !isYAMLFile(n) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "inventory", n))
		if err != nil {
			continue
		}
		var f map[string]any
		if yaml.Unmarshal(data, &f) != nil {
			continue
		}
		if p, _ := f["plugin"].(string); p == umpPlugin {
			return "inventory/" + n
		}
	}
	return ""
}

// isYAMLFile says whether a file name is one Ansible reads as YAML.
func isYAMLFile(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// playbooks are the names of the repository's playbooks, the files
// playbooks/NAME.yml (or links to files), in file-name order; dot files,
// other names and subdirectories (what the playbooks include) are none.
func (r *infraRepo) playbooks() []string {
	entries, err := os.ReadDir(filepath.Join(r.dir, playbooksDir))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".yml") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(r.dir, playbooksDir, n)); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".yml"))
	}
	return names
}

// playbookPaths are the playbooks' paths in the repository, as
// ansible-playbook takes them.
func playbookPaths(names []string) []string {
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = playbooksDir + "/" + n + ".yml"
	}
	return paths
}

// requireRepo returns the infrastructure repository to run Ansible in,
// with its playbooks, or an error (exit status 2) that says what is
// missing; hint is what else the user can do.
func (a *app) requireRepo(hint string) (*infraRepo, []string, error) {
	repo, missing, err := a.infraLayout()
	if err != nil {
		return nil, nil, err
	}
	what := "run ump from the root of the infrastructure repository"
	playbooks := repo.playbooks()
	if len(playbooks) == 0 {
		if len(missing) == 0 {
			what = "create " + basePlaybook
		}
		missing = append(missing, playbooksDir+"/*.yml")
	}
	if _, err := a.lookPath("ansible-playbook"); err != nil {
		if len(missing) == 0 {
			what = "install ansible-core"
		}
		missing = append(missing, "ansible-playbook in PATH")
	}
	if len(missing) > 0 {
		return nil, nil, withExit(exitUsage, fmt.Errorf("cannot provision from %s: no %s; %s%s", repo.dir, strings.Join(missing, ", no "), what, hint))
	}
	if repo.worldWritable() {
		return nil, nil, withExit(exitUsage, fmt.Errorf("cannot provision from %s: it is a world-writable directory, which Ansible ignores as an ansible.cfg source (anyone could have put one there); chmod o-w %s", repo.dir, repo.dir))
	}
	return repo, playbooks, nil
}

// choosePlaybooks narrows the repository's playbooks to those named
// with --playbook, which must be there; the order stays the
// repository's. None named is every one.
func choosePlaybooks(all, named []string) ([]string, error) {
	for _, n := range named {
		if !slices.Contains(all, n) {
			return nil, withExit(exitUsage, fmt.Errorf("--playbook %s: there is no %s/%s.yml (the playbooks: %s)", n, playbooksDir, n, strings.Join(all, ", ")))
		}
	}
	if len(named) == 0 {
		return all, nil
	}
	var out []string
	for _, n := range all {
		if slices.Contains(named, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// worldWritable says that Ansible takes no ansible.cfg from the
// repository, where anyone could have put it; ANSIBLE_CONFIG would get
// past that, so ump runs no Ansible there.
func (r *infraRepo) worldWritable() bool {
	fi, err := os.Stat(r.dir)
	return err == nil && fi.Mode().Perm()&0o002 != 0 && runtime.GOOS != "windows"
}

// inventoryRepo is the repository a server installed with --no-provision
// goes into: the one ump runs in, when it has the layout and
// ansible-inventory can check the names there first. Without the layout
// it is nil; without the check, nil and why not.
func (a *app) inventoryRepo() (repo *infraRepo, unchecked string) {
	repo, missing, err := a.infraLayout()
	switch {
	case err != nil || len(missing) > 0:
		return nil, ""
	case repo.worldWritable():
		return nil, repo.dir + " is world-writable, Ansible takes no ansible.cfg from it"
	}
	if _, err := a.lookPath("ansible-inventory"); err != nil {
		return nil, "no ansible-inventory in PATH to check the names"
	}
	return repo, ""
}

// ansibleSSHArgs are the SSH arguments ump's runs need, each with the
// setting of ansible.cfg it gives way to.
var ansibleSSHArgs = []struct{ env, key, value string }{
	// a freshly installed system has a host key nobody knows yet: a new
	// one is accepted, never a changed one (ump forgets the old key when
	// it reinstalls)
	{"ANSIBLE_SSH_COMMON_ARGS", "ssh_common_args", "-o StrictHostKeyChecking=accept-new"},
	// the keys given (UMP's, the inventory's ansible_ssh_private_key_file)
	// are offered, not every key of the agent before them: sshd's
	// MaxAuthTries would end the login first
	{"ANSIBLE_SSH_EXTRA_ARGS", "ssh_extra_args", "-o IdentitiesOnly=yes"},
	{"ANSIBLE_SFTP_EXTRA_ARGS", "sftp_extra_args", "-o IdentitiesOnly=yes"},
	{"ANSIBLE_SCP_EXTRA_ARGS", "scp_extra_args", "-o IdentitiesOnly=yes"},
}

// ansibleEnv is what ump adds to Ansible's environment. ANSIBLE_CONFIG
// is the repository's ansible.cfg, whatever the environment says. The
// SSH arguments are set where neither the environment nor ansible.cfg
// set them: Ansible puts them after ansible.cfg's ssh_args and
// host_key_checking, and ssh takes the first value of an option, so the
// repository's own choice stays in force.
func (r *infraRepo) ansibleEnv() []string {
	env := []string{"ANSIBLE_CONFIG=" + filepath.Join(r.dir, "ansible.cfg")}
	cfg, _ := os.ReadFile(filepath.Join(r.dir, "ansible.cfg"))
	for _, s := range ansibleSSHArgs {
		if _, set := os.LookupEnv(s.env); set {
			continue
		}
		if regexp.MustCompile(`(?m)^[ \t]*` + s.key + `[ \t]*[=:]`).Match(cfg) {
			continue
		}
		env = append(env, s.env+"="+s.value)
	}
	return env
}

// ansibleEnv is what ump adds to the environment of an Ansible run: the
// repository's settings, and for an ump that Ansible runs in turn (the
// inventory plugin ofzhur.ansible.ump) where this one reads the state
// and its configuration file, so that it lists what this one recorded,
// from the same accounts, and leaves the mirror alone (resolveHome).
func (a *app) ansibleEnv(repo *infraRepo) []string {
	where := "local"
	if a.home != nil && a.home.Backend != nil {
		where = "backend"
		if a.pullFailed {
			where = "mirror"
		}
	}
	env := append(repo.ansibleEnv(), parentEnv+"="+where)
	if path, err := filepath.Abs(config.ResolvePath(a.configFlag)); err == nil {
		env = append(env, config.EnvConfig+"="+path)
	}
	return env
}

// syntaxCheck runs ansible-playbook --syntax-check on the playbooks in
// the repository, which also reads the inventory: a playbook that does
// not parse and a broken inventory stop ump before it changes anything.
// The output is shown when it fails.
func (a *app) syntaxCheck(ctx context.Context, repo *infraRepo, playbooks []string) error {
	var out bytes.Buffer
	args := append([]string{"--syntax-check"}, playbookPaths(playbooks)...)
	code, err := a.ansible(ctx, repo.dir, "ansible-playbook", args, a.ansibleEnv(repo), nil, &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		a.stderr.Write(out.Bytes())
		return withExit(exitUsage, fmt.Errorf("the playbooks %s do not pass ansible-playbook --syntax-check in %s (exit status %d, see above); nothing was changed", strings.Join(playbooks, ", "), repo.dir, code))
	}
	return nil
}

// inventory is what ansible-inventory --list says: the groups, each
// with its own hosts and children, and the hosts with the ump_ref of
// each (empty where a host has none); plugin is the repository's, when
// ump is the inventory.
type inventory struct {
	groups   map[string]bool
	members  map[string][]string
	children map[string][]string
	hosts    map[string]string
	plugin   string
}

// readInventory reads the repository's inventory with ansible-inventory
// --list, the hosts of a pattern (--limit) when one is given; its output
// is shown when it fails.
func (a *app) readInventory(ctx context.Context, repo *infraRepo, pattern string) (*inventory, error) {
	var out, errb bytes.Buffer
	args := []string{"--list"}
	if pattern != "" {
		args = append(args, "--limit", pattern)
	}
	code, err := a.ansible(ctx, repo.dir, "ansible-inventory", args, a.ansibleEnv(repo), nil, &out, &errb)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		a.stderr.Write(errb.Bytes())
		return nil, withExit(exitUsage, fmt.Errorf("the inventory of %s does not pass ansible-inventory --list (exit status %d, see above); nothing was changed", repo.dir, code))
	}
	inv, err := parseInventory(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ansible-inventory --list in %s: %w", repo.dir, err)
	}
	inv.plugin = repo.plugin
	return inv, nil
}

// parseInventory reads the JSON of ansible-inventory --list: every key
// but _meta is a group with its hosts and children (a group without
// either is only among its parent's children), and _meta has the
// variables of the hosts.
func parseInventory(data []byte) (*inventory, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	inv := &inventory{groups: map[string]bool{}, members: map[string][]string{}, children: map[string][]string{}, hosts: map[string]string{}}
	for name, v := range raw {
		if name == "_meta" {
			continue
		}
		var g struct {
			Hosts    []string `json:"hosts"`
			Children []string `json:"children"`
		}
		if err := json.Unmarshal(v, &g); err != nil {
			return nil, fmt.Errorf("group %s: %w", name, err)
		}
		inv.groups[name] = true
		inv.members[name], inv.children[name] = g.Hosts, g.Children
		for _, c := range g.Children {
			inv.groups[c] = true
		}
		for _, h := range g.Hosts {
			inv.hosts[h] = ""
		}
	}
	var meta struct {
		Hostvars map[string]map[string]any `json:"hostvars"`
	}
	if v, ok := raw["_meta"]; ok {
		if err := json.Unmarshal(v, &meta); err != nil {
			return nil, fmt.Errorf("_meta: %w", err)
		}
	}
	for h, vars := range meta.Hostvars {
		inv.hosts[h] = inventoryString(vars["ump_ref"])
	}
	return inv, nil
}

// inventoryString is a string value of ansible-inventory --list: plain,
// or marked unsafe ({"__ansible_unsafe": "..."}), as the collection's
// inventory plugin hands its values over; empty for anything else.
func inventoryString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		s, _ := v["__ansible_unsafe"].(string)
		return s
	}
	return ""
}

// with is the inventory once the hosts are in it, each in its groups:
// the current one plus the hosts, and without a host the servers among
// them have under another name (an old hostname, whose host file goes).
// What else gives a host its groups stays, so a group a host leaves is
// still its group here.
func (inv *inventory) with(hosts map[string]inventoryHost) *inventory {
	out := &inventory{groups: maps.Clone(inv.groups), members: map[string][]string{}, children: maps.Clone(inv.children), hosts: maps.Clone(inv.hosts), plugin: inv.plugin}
	named := map[string]string{}
	for host, h := range hosts {
		named[h.m.Ref()] = host
	}
	gone := func(h string) bool {
		host, ok := named[inv.hosts[h]]
		return ok && host != h
	}
	for g, hs := range inv.members {
		out.members[g] = slices.DeleteFunc(slices.Clone(hs), gone)
	}
	out.children["all"] = slices.Clone(inv.children["all"])
	for h := range inv.hosts {
		if gone(h) {
			delete(out.hosts, h)
		}
	}
	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		out.hosts[host] = hosts[host].m.Ref()
		for _, g := range hosts[host].groups {
			if !out.groups[g] {
				out.groups[g] = true
				out.children["all"] = append(out.children["all"], g)
			}
			if !slices.Contains(out.members[g], host) {
				out.members[g] = append(out.members[g], host)
			}
		}
	}
	return out
}

// source is the inventory as Ansible's yaml inventory plugin reads it
// (in JSON, which it reads too): every group with its hosts and
// children.
func (inv *inventory) source() ([]byte, error) {
	set := func(names []string) map[string]any {
		m := map[string]any{}
		for _, n := range names {
			m[n] = nil
		}
		return m
	}
	groups := map[string]map[string]any{}
	for g := range inv.groups {
		groups[g] = map[string]any{}
		if hs := inv.members[g]; len(hs) > 0 {
			groups[g]["hosts"] = set(hs)
		}
		if cs := inv.children[g]; len(cs) > 0 {
			groups[g]["children"] = set(cs)
		}
	}
	return json.Marshal(groups)
}

// matchHosts checks that a pattern (--hosts) matches a host of the
// inventory as it will be, before anything is written:
// ansible-inventory --limit takes that inventory from a temporary file,
// outside the repository, and matches the pattern as Ansible does.
// Matching no host is exit status 2, with refusal as the message.
func (a *app) matchHosts(ctx context.Context, repo *infraRepo, inv *inventory, pattern, refusal string) error {
	src, err := inv.source()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ump-inventory-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(file, src, 0o600); err != nil {
		return err
	}
	var out, errb bytes.Buffer
	env := append(a.ansibleEnv(repo), "ANSIBLE_INVENTORY_ENABLED=yaml")
	code, err := a.ansible(ctx, repo.dir, "ansible-inventory", []string{"-i", file, "--list", "--limit", pattern}, env, nil, &out, &errb)
	if err != nil {
		return err
	}
	if code != 0 {
		a.stderr.Write(errb.Bytes())
		return withExit(exitUsage, fmt.Errorf("--hosts %s does not pass ansible-inventory --limit (exit status %d, see above); nothing was changed", pattern, code))
	}
	matched, err := parseInventory(out.Bytes())
	if err != nil {
		return fmt.Errorf("ansible-inventory --limit %s: %w", pattern, err)
	}
	if len(matched.hosts) == 0 {
		return withExit(exitUsage, errors.New(refusal))
	}
	return nil
}

// sameName is why a host and a group must not share a name: Ansible
// resolves a pattern that names a host as that host.
const sameName = "a host and a group with the same name make the group's plays match only the host"

// ungroup is what the user can do about a group of a server that cannot
// be: give another, or, for a group of the state (server is the server's
// name), take it away.
func ungroup(server, group string) string {
	if server == "" {
		return "use another group"
	}
	return "ump server set " + server + " --ungroup " + group
}

// check refuses names the inventory already has the other way round:
// the hostname as a group, or a group of the server (common and its
// own) as a host other than the server's (its host file of another
// hostname, which goes when the new one is written); when ump is the
// inventory, also the hostname as a host other than the server's. retry
// is what the user can do about the hostname; server is the server's
// name when its groups are those of the state (empty: --group).
func (inv *inventory) check(hostname, ref string, groups []string, retry, server string) error {
	if inv.groups[hostname] {
		return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of the inventory: %s; %s", hostname, sameName, retry))
	}
	if r, ok := inv.hosts[hostname]; ok && inv.plugin != "" && r != ref {
		other := "a host of the inventory without ump_ref"
		if r != "" {
			other = "the host of " + r + " in the inventory (its ump_ref)"
		}
		return withExit(exitUsage, fmt.Errorf("the hostname %s is %s; %s", hostname, other, retry))
	}
	for _, g := range groups {
		if r, ok := inv.hosts[g]; ok && g != hostname && r != ref {
			return withExit(exitUsage, fmt.Errorf("the group %s of %s is also a host of the inventory: %s; rename the host, or %s", g, hostname, sameName, ungroup(server, g)))
		}
	}
	return nil
}

// hostFile is the content of inventory/hosts/<hostname>.yml, for the
// collection's inventory plugin; the fields are in the order written.
type hostFile struct {
	Plugin string   `yaml:"plugin"`
	Groups []string `yaml:"groups,flow"`
	Vars   hostVars `yaml:"vars"`
}

// hostVars are the variables of a host, in the host file and in ump
// server list -o ansible. ump_admin is the administrator, whom ump's
// runs log in as; there is no login of the inventory's, so that people
// log in as themselves.
type hostVars struct {
	AnsibleHost    string            `json:"ansible_host,omitempty" yaml:"ansible_host,omitempty"`
	UMPAdmin       string            `json:"ump_admin,omitempty" yaml:"ump_admin,omitempty"`
	UMPName        string            `json:"ump_name,omitempty" yaml:"ump_name,omitempty"`
	UMPRef         string            `json:"ump_ref,omitempty" yaml:"ump_ref,omitempty"`
	UMPProvider    string            `json:"ump_provider,omitempty" yaml:"ump_provider,omitempty"`
	UMPAccount     string            `json:"ump_account,omitempty" yaml:"ump_account,omitempty"`
	UMPLocation    string            `json:"ump_location,omitempty" yaml:"ump_location,omitempty"`
	UMPProduct     string            `json:"ump_product,omitempty" yaml:"ump_product,omitempty"`
	UMPOS          string            `json:"ump_os,omitempty" yaml:"ump_os,omitempty"`
	UMPProfile     string            `json:"ump_profile,omitempty" yaml:"ump_profile,omitempty"`
	UMPInstalledAt string            `json:"ump_installed_at,omitempty" yaml:"ump_installed_at,omitempty"`
	UMPAttributes  map[string]string `json:"ump_attributes,omitempty" yaml:"ump_attributes,omitempty"`
}

// primaryAddress is the address ump reaches a server at: the IPv4, else
// the IPv6.
func primaryAddress(m provider.Machine) string {
	if m.IPv4 != "" {
		return m.IPv4
	}
	return m.IPv6
}

// hostVarsFor is what the host file says about a server ump installed;
// admin is the administrator of the installation.
func hostVarsFor(m provider.Machine, rec state.Install, admin string) hostVars {
	v := hostVars{
		AnsibleHost: primaryAddress(m), UMPAdmin: admin,
		UMPName: m.Name, UMPRef: m.Ref(), UMPProvider: m.Provider, UMPAccount: m.Account,
		UMPLocation: m.Location, UMPProduct: m.Product, UMPOS: rec.OS, UMPProfile: rec.Profile,
		UMPAttributes: m.Attributes,
	}
	if !rec.At.IsZero() {
		v.UMPInstalledAt = rec.At.UTC().Format(time.RFC3339)
	}
	return v
}

// inventoryHostname is the host name of a server ump installed: the
// hostname of its installation, else its name where that is a valid one;
// empty when neither.
func inventoryHostname(m provider.Machine, rec state.Install) string {
	if rec.Hostname != "" {
		return rec.Hostname
	}
	return rescueHostname(m.Name)
}

// inventoryVars is what the inventory says about a server ump installed,
// with the administrator of the installation (the profile's for a
// record without one).
func (a *app) inventoryVars(cfg *config.Config, m provider.Machine, rec state.Install) hostVars {
	admin := rec.Admin
	if admin == "" {
		admin = a.profileAdmin(cfg, rec.Profile)
	}
	return hostVarsFor(m, rec, admin)
}

// inventoryHost is a server ump installed as the inventory has it: in
// common and its groups, with its variables; stale says that a fact
// cache has the facts of its old system, installed is when ump
// installed the new one.
type inventoryHost struct {
	m         provider.Machine
	groups    []string
	vars      hostVars
	stale     bool
	installed time.Time
}

// inventoryHosts are the servers ump installed among ms, by host name.
// A server without an address or a valid hostname is left out with a
// note, and so is one whose hostname a later installation took, and a
// group of a state edited by hand that ump would not give.
func (a *app) inventoryHosts(cfg *config.Config, doc *state.Document, ms []provider.Machine) map[string]inventoryHost {
	type entry struct {
		m provider.Machine
		s state.Server
	}
	found := map[string]entry{}
	for _, m := range ms {
		s, ok := doc.Servers[m.Ref()]
		if !ok || s.Installed == nil {
			continue
		}
		host := inventoryHostname(m, *s.Installed)
		switch {
		case primaryAddress(m) == "":
			fmt.Fprintf(a.stderr, "ump: %s is left out of the inventory: it has no public address\n", describeRef(m))
			continue
		case host == "":
			fmt.Fprintf(a.stderr, "ump: %s is left out of the inventory: its installation has no hostname, and its name is not a valid one\n", describeRef(m))
			continue
		}
		if other, taken := found[host]; taken {
			kept, left := other, entry{m, s}
			if s.Installed.At.After(other.s.Installed.At) {
				kept, left = left, kept
			}
			found[host] = kept
			fmt.Fprintf(a.stderr, "ump: %s is left out of the inventory: its hostname %s is that of %s, installed later\n", describeRef(left.m), host, kept.m.Ref())
			continue
		}
		found[host] = entry{m, s}
	}
	hosts := map[string]inventoryHost{}
	for host, e := range found {
		h := inventoryHost{m: e.m, groups: []string{commonGroup}, vars: a.inventoryVars(cfg, e.m, *e.s.Installed), stale: e.s.StaleFacts, installed: e.s.Installed.At}
		for _, g := range e.s.Groups {
			if !givenGroup(g) {
				fmt.Fprintf(a.stderr, "ump: the group %q of %s in the state is left out: not a group name ump gives\n", g, describeRef(e.m))
				continue
			}
			h.groups = append(h.groups, g)
		}
		hosts[host] = h
	}
	return hosts
}

// givenGroup says that ump gives g as a group besides common: what the
// inventory keeps of a server's groups in the state.
func givenGroup(g string) bool {
	return groupName.MatchString(g) && !ansibleOwnName(g) && g != commonGroup
}

// checkStateGroups refuses a new hostname that is a group of another
// server ump installed, in the state: the next ump provision would
// refuse the two. retry is what the user can do about the hostname.
func checkStateGroups(doc *state.Document, hostname, ref, retry string) error {
	if !givenGroup(hostname) {
		return nil
	}
	for _, r := range slices.Sorted(maps.Keys(doc.Servers)) {
		s := doc.Servers[r]
		if r != ref && s.Installed != nil && slices.Contains(s.Groups, hostname) {
			return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of %s in the state: %s; %s, or %s", hostname, r, sameName, retry, ungroup(r, hostname)))
		}
	}
	return nil
}

// ansibleInventory is the inventory of the servers ump installed among
// ms, in the JSON of an inventory script: every group with its hosts,
// all with the groups as children, and the variables of the hosts under
// _meta, as the host files have them.
func (a *app) ansibleInventory(cfg *config.Config, doc *state.Document, ms []provider.Machine) map[string]any {
	hostvars := map[string]hostVars{}
	members := map[string][]string{}
	for host, h := range a.inventoryHosts(cfg, doc, ms) {
		hostvars[host] = h.vars
		for _, g := range h.groups {
			members[g] = append(members[g], host)
		}
	}
	inv := map[string]any{"_meta": map[string]any{"hostvars": hostvars}}
	groups := []string{}
	for g, hs := range members {
		slices.Sort(hs)
		inv[g] = map[string][]string{"hosts": hs}
		groups = append(groups, g)
	}
	slices.Sort(groups)
	inv["all"] = map[string][]string{"children": groups}
	return inv
}

// renderHostFile is the host file of a host in the groups (common
// first), with a header that says who writes it.
func renderHostFile(hostname string, groups []string, v hostVars) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s: written by ump (ump server reinstall, ump provision).\n# Edits are overwritten; overrides go to inventory/host_vars/%s.yml.\n", hostname, hostname)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(hostFile{Plugin: collection + ".host", Groups: groups, Vars: v}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// hostFileRef is the ump_ref of a host file: the server it was written
// for; empty when the file has none or is no YAML. The error is that of
// reading it (fs.ErrNotExist: there is none).
func hostFileRef(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f struct {
		Vars struct {
			UMPRef string `yaml:"ump_ref"`
		} `yaml:"vars"`
	}
	if yaml.Unmarshal(data, &f) != nil {
		return "", nil
	}
	return f.Vars.UMPRef, nil
}

// checkHost refuses a host that cannot go into the inventory as it is:
// its name is one of its groups or Ansible's own, or a host file of its
// name is not its own. retry is what the user can do about the name;
// server is the server's name when its groups are those of the state.
func (r *infraRepo) checkHost(hostname, ref string, groups []string, retry, server string) error {
	switch {
	case slices.Contains(groups, hostname) && server != "":
		return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of the server: %s; %s, or %s", hostname, sameName, ungroup(server, hostname), retry))
	case slices.Contains(groups, hostname):
		return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of the server: %s; %s", hostname, sameName, retry))
	case hostname == "all" || hostname == "ungrouped":
		return withExit(exitUsage, fmt.Errorf("the hostname %s is one of Ansible's own groups: %s; %s", hostname, sameName, retry))
	case hostname == "localhost" || hostname == "127.0.0.1":
		return withExit(exitUsage, fmt.Errorf("the hostname %s is one of Ansible's own names: plays and tasks for localhost would run on the server; %s", hostname, retry))
	}
	return r.checkHostFiles(hostname, ref, retry)
}

// checkHostFiles refuses the host files of the hostname that ump would
// overwrite or write its own beside: HOSTNAME.yml or HOSTNAME.yaml
// without ump_ref, or with another server's. When ump is the inventory,
// it refuses every host file of the hostname or of the server instead:
// the host would be defined twice.
func (r *infraRepo) checkHostFiles(hostname, ref, retry string) error {
	if r.plugin != "" {
		return r.checkNoHostFile(hostname, ref)
	}
	for _, ext := range []string{".yml", ".yaml"} {
		rel := hostsDir + "/" + hostname + ext
		other, err := hostFileRef(filepath.Join(r.dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case other == "":
			return withExit(exitUsage, fmt.Errorf("%s has no ump_ref: written by hand or by another tool; remove it, ump writes %s/%s.yml", rel, hostsDir, hostname))
		case other != ref:
			return withExit(exitUsage, fmt.Errorf("%s is the host file of %s (its ump_ref); %s, or remove the file if that server is gone", rel, other, retry))
		}
	}
	return nil
}

// checkNoHostFile refuses a host file of the hostname, or one written
// for the server under another name, beside the plugin that makes ump
// the inventory.
func (r *infraRepo) checkNoHostFile(hostname, ref string) error {
	dir := filepath.Join(r.dir, hostsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !isYAMLFile(n) {
			continue
		}
		rel := hostsDir + "/" + n
		if n == hostname+".yml" || n == hostname+".yaml" {
			return withExit(exitUsage, fmt.Errorf("%s defines the host %s too, and %s makes ump the inventory: the host would be defined twice; remove the host file", rel, hostname, r.plugin))
		}
		if other, err := hostFileRef(filepath.Join(dir, n)); err == nil && other == ref {
			return withExit(exitUsage, fmt.Errorf("%s is the host file of %s, and %s makes ump the inventory: the server would be in it twice; remove the host file", rel, ref, r.plugin))
		}
	}
	return nil
}

// writeHostFile writes inventory/hosts/<hostname>.yml under a dot name,
// which Ansible ignores, and renames it into place, unless the file says
// that already; the files of other names that were written for the same
// server (an old hostname) go. A file of the hostname that is not the
// server's stops it, as up front. It returns the path written (empty:
// unchanged) and those removed, relative to the repository.
func (r *infraRepo) writeHostFile(hostname, ref string, data []byte) (written string, removed []string, err error) {
	if err := r.checkHostFiles(hostname, ref, "give another hostname"); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(r.dir, hostsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	name := hostname + ".yml"
	if old, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(old, data) {
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			os.Remove(tmp)
			return "", nil, err
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			os.Remove(tmp)
			return "", nil, err
		}
		written = hostsDir + "/" + name
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if n == name || strings.HasPrefix(n, ".") || e.IsDir() || !isYAMLFile(n) {
			continue
		}
		if other, err := hostFileRef(filepath.Join(dir, n)); err != nil || other != ref {
			continue
		}
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return "", nil, err
		}
		removed = append(removed, hostsDir+"/"+n)
	}
	return written, removed, nil
}

// putInInventory writes the host file and says what changed; when ump
// is the inventory, the state has the server already, and it says that.
func (a *app) putInInventory(repo *infraRepo, hostname string, groups []string, v hostVars) error {
	if repo.plugin != "" {
		if err := repo.checkNoHostFile(hostname, v.UMPRef); err != nil {
			return err
		}
		fmt.Fprintf(a.stderr, "No host file: %s makes ump the inventory, with %s in %s.\n", repo.plugin, hostname, strings.Join(groups, ", "))
		return nil
	}
	data, err := renderHostFile(hostname, groups, v)
	if err != nil {
		return err
	}
	written, removed, err := repo.writeHostFile(hostname, v.UMPRef, data)
	if err != nil {
		return fmt.Errorf("the host file %s/%s.yml: %w", hostsDir, hostname, err)
	}
	for _, f := range removed {
		fmt.Fprintf(a.stderr, "Removed %s: %s has the hostname %s now.\n", f, v.UMPRef, hostname)
	}
	if written != "" {
		fmt.Fprintf(a.stderr, "Wrote %s: %s in %s.\n", written, hostname, strings.Join(groups, ", "))
	}
	return nil
}

// hostRecap is what the PLAY RECAP of ansible-playbook says of a host.
type hostRecap struct {
	OK          int `json:"ok" yaml:"ok"`
	Changed     int `json:"changed" yaml:"changed"`
	Unreachable int `json:"unreachable" yaml:"unreachable"`
	Failed      int `json:"failed" yaml:"failed"`
	Skipped     int `json:"skipped" yaml:"skipped"`
	Rescued     int `json:"rescued" yaml:"rescued"`
	Ignored     int `json:"ignored" yaml:"ignored"`
}

var (
	// ansiEscape is a color of Ansible's output.
	ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")
	// recapLine is a host's line of the recap, as
	// web-01 : ok=3 changed=1 unreachable=0 failed=0 skipped=0 rescued=0 ignored=0
	recapLine  = regexp.MustCompile(`^(\S+)\s+:\s+((?:[a-z]+=\d+\s*)+)$`)
	recapCount = regexp.MustCompile(`([a-z]+)=(\d+)`)
)

// recapWriter reads Ansible's output as it passes and keeps the last
// PLAY RECAP: ansible-playbook prints one after every playbook, each
// with the counts so far.
type recapWriter struct {
	line   []byte
	inside bool
	hosts  map[string]hostRecap
}

func (w *recapWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c != '\n' {
			if len(w.line) < 4096 {
				w.line = append(w.line, c)
			}
			continue
		}
		w.take(string(ansiEscape.ReplaceAll(w.line, nil)))
		w.line = w.line[:0]
	}
	return len(p), nil
}

// take reads a line: the recap's header starts a recap, and the first
// line after its hosts' lines ends it.
func (w *recapWriter) take(line string) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "PLAY RECAP") {
		w.inside, w.hosts = true, map[string]hostRecap{}
		return
	}
	if !w.inside {
		return
	}
	m := recapLine.FindStringSubmatch(line)
	if m == nil {
		w.inside = line == "" && len(w.hosts) == 0
		return
	}
	var r hostRecap
	for _, c := range recapCount.FindAllStringSubmatch(m[2], -1) {
		n, _ := strconv.Atoi(c[2])
		switch c[1] {
		case "ok":
			r.OK = n
		case "changed":
			r.Changed = n
		case "unreachable":
			r.Unreachable = n
		case "failed":
			r.Failed = n
		case "skipped":
			r.Skipped = n
		case "rescued":
			r.Rescued = n
		case "ignored":
			r.Ignored = n
		}
	}
	w.hosts[m[1]] = r
}

// runPlaybooks runs the playbooks with ansible-playbook in the
// repository, on the hosts of the pattern (every host of the inventory
// without one), each logging in as its administrator with UMP's key,
// with the user's arguments after ump's, and --limit last, where no
// argument of the user's can take its place. Ansible's output goes to
// stderr, stdout stays for results. It returns the exit status and the
// recap.
//
// Several playbooks run as one that imports them, written into
// playbooks/ for the run (a dot file, which is no playbook of ump's):
// given one by one, ansible-playbook stops at the first in which a host
// failed or was unreachable, for every host; one playbook leaves those
// hosts out of the plays after and goes on with the others. stale are
// hosts whose facts in a fact cache are those of their old systems: that
// playbook, also for one, starts with a play that clears them, so that a
// new system starts without the old one's facts (its Python, for one).
func (a *app) runPlaybooks(ctx context.Context, repo *infraRepo, playbooks, stale []string, pattern string, extra []string, keyPath string) (int, map[string]hostRecap, error) {
	run := playbookPaths(playbooks)
	if len(playbooks) > 1 || len(stale) > 0 {
		name := fmt.Sprintf(".ump-provision-%d.yml", os.Getpid())
		var b strings.Builder
		b.WriteString("# The playbooks of a run of ump provision, which removes this file after it.\n")
		if len(stale) > 0 {
			fmt.Fprintf(&b, "- name: New systems, without the old ones' facts\n  hosts: %s\n  gather_facts: false\n  tasks:\n    - ansible.builtin.meta: clear_facts\n", strconv.Quote(strings.Join(stale, ",")))
		}
		for _, n := range playbooks {
			fmt.Fprintf(&b, "- import_playbook: %s\n", strconv.Quote(n+".yml"))
		}
		path := filepath.Join(repo.dir, playbooksDir, name)
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			return 0, nil, err
		}
		defer os.Remove(path)
		run = []string{playbooksDir + "/" + name}
	}
	args := append(append(run, "--private-key", keyPath, "--extra-vars", umpLogin), extra...)
	if pattern != "" {
		args = append(args, "--limit", pattern)
	}
	var stdin io.Reader
	if a.isTTY != nil && a.isTTY() {
		stdin = a.stdin
	}
	env := a.ansibleEnv(repo)
	// Ansible drops its colors when its output goes through ump
	if a.stderrTTY != nil && a.stderrTTY() && os.Getenv("ANSIBLE_FORCE_COLOR") == "" && os.Getenv("ANSIBLE_NOCOLOR") == "" && os.Getenv("NO_COLOR") == "" {
		env = append(env, "ANSIBLE_FORCE_COLOR=1")
	}
	recap := &recapWriter{}
	out := io.MultiWriter(a.stderr, recap)
	code, err := a.ansible(ctx, repo.dir, "ansible-playbook", args, env, stdin, out, out)
	recap.Write([]byte("\n"))
	return code, recap.hosts, err
}
