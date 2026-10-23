package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// Provisioning: ump runs Ansible from the infrastructure repository, the
// current directory. Every server ump installs is in the group common and
// in the groups given with --group; its run list is those groups in
// order, and each group is a playbook: playbooks/<group>.yml of the
// repository when it has one, else the collection's
// ofzhur.ansible.<group>. The server goes into the inventory one of two
// ways: ump writes its host file into inventory/hosts/ (read by the
// collection's inventory plugin ofzhur.ansible.host), or, when a file of
// inventory/ configures the plugin ofzhur.ansible.ump, ump itself is the
// inventory (ump server list -o ansible) and writes nothing. Either way
// the inventory has the same hosts, groups and variables, and ump runs
// the run list for that host alone.

const (
	// commonGroup is the group of every server ump installs; its run
	// list starts with it.
	commonGroup = "common"
	// collection is the Ansible collection with the inventory plugins and
	// the playbooks of the groups the repository has none of.
	collection = "ofzhur.ansible"
	// hostsDir is where the host files go, in the repository.
	hostsDir = "inventory/hosts"
	// umpPlugin is the inventory plugin that makes ump the inventory.
	umpPlugin = collection + ".ump"
	// parentEnv names, in the environment of the Ansible ump runs, where
	// ump reads the state (backend, mirror or local): an ump that Ansible
	// runs in turn is part of the same run.
	parentEnv = "UMP_PARENT"
)

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
	// the environment with PWD set to dir; later entries win
	cmd.Env = append(cmd.Environ(), env...)
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

// requireRepo returns the infrastructure repository to run Ansible in,
// or an error (exit status 2) that says what is missing; hint is what
// else the user can do.
func (a *app) requireRepo(hint string) (*infraRepo, error) {
	repo, missing, err := a.infraLayout()
	if err != nil {
		return nil, err
	}
	_, noAnsible := a.lookPath("ansible-playbook")
	switch {
	case len(missing) > 0:
		if noAnsible != nil {
			missing = append(missing, "ansible-playbook in PATH")
		}
		return nil, withExit(exitUsage, fmt.Errorf("cannot provision from %s: no %s; run ump from the root of the infrastructure repository%s", repo.dir, strings.Join(missing, ", no "), hint))
	case noAnsible != nil:
		return nil, withExit(exitUsage, fmt.Errorf("cannot provision from %s: no ansible-playbook in PATH; install ansible-core%s", repo.dir, hint))
	}
	if repo.worldWritable() {
		return nil, withExit(exitUsage, fmt.Errorf("cannot provision from %s: it is a world-writable directory, which Ansible ignores as an ansible.cfg source (anyone could have put one there); chmod o-w %s", repo.dir, repo.dir))
	}
	return repo, nil
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

// runList is a host's groups in order, common first, with the playbook
// each resolves to.
type runList struct {
	groups    []string
	playbooks []string
}

// runList resolves the groups: playbooks/<group>.yml of the repository
// when it is there, else the collection's playbook of that name.
func (r *infraRepo) runList(groups []string) runList {
	var rl runList
	for _, g := range append([]string{commonGroup}, groups...) {
		pb := collection + "." + g
		local := filepath.ToSlash(filepath.Join("playbooks", g+".yml"))
		if fi, err := os.Stat(filepath.Join(r.dir, local)); err == nil && fi.Mode().IsRegular() {
			pb = local
		}
		rl.groups = append(rl.groups, g)
		rl.playbooks = append(rl.playbooks, pb)
	}
	return rl
}

// String is how ump shows a run list: common, web (ofzhur.ansible.common, playbooks/web.yml).
func (rl runList) String() string {
	return strings.Join(rl.groups, ", ") + " (" + strings.Join(rl.playbooks, ", ") + ")"
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

// syntaxCheck runs ansible-playbook --syntax-check on the run list in
// the repository, which also reads the inventory: a group without a
// playbook, a playbook that does not parse and a broken inventory stop
// ump before it changes anything. The output is shown when it fails.
func (a *app) syntaxCheck(ctx context.Context, repo *infraRepo, rl runList) error {
	var out bytes.Buffer
	args := append([]string{"--syntax-check"}, rl.playbooks...)
	code, err := a.ansible(ctx, repo.dir, "ansible-playbook", args, a.ansibleEnv(repo), nil, &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		a.stderr.Write(out.Bytes())
		return withExit(exitUsage, fmt.Errorf("the run list %s does not pass ansible-playbook --syntax-check in %s (exit status %d, see above); nothing was changed", rl, repo.dir, code))
	}
	return nil
}

// inventory is what ansible-inventory --list says: the groups, and the
// hosts with the ump_ref of each (empty where a host has none); plugin
// is the repository's, when ump is the inventory.
type inventory struct {
	groups map[string]bool
	hosts  map[string]string
	plugin string
}

// readInventory reads the repository's inventory with ansible-inventory
// --list; its output is shown when it fails.
func (a *app) readInventory(ctx context.Context, repo *infraRepo) (*inventory, error) {
	var out, errb bytes.Buffer
	code, err := a.ansible(ctx, repo.dir, "ansible-inventory", []string{"--list"}, a.ansibleEnv(repo), nil, &out, &errb)
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
	inv := &inventory{groups: map[string]bool{}, hosts: map[string]string{}}
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

// sameName is why a host and a group must not share a name: Ansible
// resolves a pattern that names a host as that host.
const sameName = "a host and a group with the same name make the group's plays match only the host"

// check refuses names the inventory already has the other way round:
// the hostname as a group, or a group of the run list as a host other
// than the server's own (its host file of another hostname, which goes
// when the new one is written); when ump is the inventory, also the
// hostname as a host other than the server's. retry is what the user can
// do about the hostname.
func (inv *inventory) check(hostname, ref string, rl runList, retry string) error {
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
	for _, g := range rl.groups {
		if r, ok := inv.hosts[g]; ok && g != hostname && r != ref {
			return withExit(exitUsage, fmt.Errorf("the group %s of the run list is also a host of the inventory: %s; rename the host or use another group", g, sameName))
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
// server list -o ansible.
type hostVars struct {
	AnsibleHost    string            `json:"ansible_host,omitempty" yaml:"ansible_host,omitempty"`
	AnsibleUser    string            `json:"ansible_user,omitempty" yaml:"ansible_user,omitempty"`
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
// admin is the user Ansible logs in as.
func hostVarsFor(m provider.Machine, rec state.Install, admin string) hostVars {
	v := hostVars{
		AnsibleHost: primaryAddress(m), AnsibleUser: admin,
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
// logging in as the administrator of the installation (the profile's
// for a record without one).
func (a *app) inventoryVars(cfg *config.Config, m provider.Machine, rec state.Install) hostVars {
	admin := rec.Admin
	if admin == "" {
		admin = a.profileAdmin(cfg, rec.Profile)
	}
	return hostVarsFor(m, rec, admin)
}

// ansibleInventory is the inventory of the servers ump installed among
// ms, in the JSON of an inventory script: every group with its hosts,
// all with the groups as children, and the variables of the hosts under
// _meta, as the host files have them. A server without an address or a
// valid hostname is left out with a note, and so is one whose hostname
// a later installation took.
func (a *app) ansibleInventory(cfg *config.Config, doc *state.Document, ms []provider.Machine) map[string]any {
	type entry struct {
		m provider.Machine
		s state.Server
	}
	hosts := map[string]entry{}
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
		if other, taken := hosts[host]; taken {
			kept, left := other, entry{m, s}
			if s.Installed.At.After(other.s.Installed.At) {
				kept, left = left, kept
			}
			hosts[host] = kept
			fmt.Fprintf(a.stderr, "ump: %s is left out of the inventory: its hostname %s is that of %s, installed later\n", describeRef(left.m), host, kept.m.Ref())
			continue
		}
		hosts[host] = entry{m, s}
	}
	hostvars := map[string]hostVars{}
	members := map[string][]string{}
	for host, e := range hosts {
		hostvars[host] = a.inventoryVars(cfg, e.m, *e.s.Installed)
		for _, g := range append([]string{commonGroup}, e.s.Groups...) {
			// a group of a state edited by hand that ump would not give
			if !groupName.MatchString(g) || ansibleOwnName(g) {
				fmt.Fprintf(a.stderr, "ump: the group %q of %s in the state is left out: not a group name ump gives\n", g, describeRef(e.m))
				continue
			}
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
	fmt.Fprintf(&b, "# %s: written by ump (ump server reinstall, ump server provision).\n# Edits are overwritten; overrides go to inventory/host_vars/%s.yml.\n", hostname, hostname)
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
// name is not its own. retry is what the user can do about the name.
func (r *infraRepo) checkHost(hostname, ref string, rl runList, retry string) error {
	switch {
	case slices.Contains(rl.groups, hostname):
		return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of its run list: %s; %s", hostname, sameName, retry))
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
// which Ansible ignores, and renames it into place; the files of other
// names that were written for the same server (an old hostname) go. A
// file of the hostname that is not the server's stops it, as up front.
// It returns the paths written and removed, relative to the repository.
func (r *infraRepo) writeHostFile(hostname, ref string, data []byte) (written string, removed []string, err error) {
	if err := r.checkHostFiles(hostname, ref, "give another hostname"); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(r.dir, hostsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	name := hostname + ".yml"
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if n == name || strings.HasPrefix(n, ".") || e.IsDir() || !(strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")) {
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
	return hostsDir + "/" + name, removed, nil
}

// putInInventory writes the host file and says so; when ump is the
// inventory, the state has the server already, and it says that.
func (a *app) putInInventory(repo *infraRepo, hostname string, groups []string, v hostVars) (string, error) {
	if repo.plugin != "" {
		if err := repo.checkNoHostFile(hostname, v.UMPRef); err != nil {
			return "", err
		}
		fmt.Fprintf(a.stderr, "No host file: %s makes ump the inventory, with %s in %s.\n", repo.plugin, hostname, strings.Join(groups, ", "))
		return "", nil
	}
	data, err := renderHostFile(hostname, groups, v)
	if err != nil {
		return "", err
	}
	written, removed, err := repo.writeHostFile(hostname, v.UMPRef, data)
	if err != nil {
		return "", fmt.Errorf("the host file %s/%s.yml: %w", hostsDir, hostname, err)
	}
	for _, f := range removed {
		fmt.Fprintf(a.stderr, "Removed %s: %s has the hostname %s now.\n", f, v.UMPRef, hostname)
	}
	fmt.Fprintf(a.stderr, "Wrote %s: %s in %s.\n", written, hostname, strings.Join(groups, ", "))
	return written, nil
}

// runRunList runs the host's run list with ansible-playbook in the
// repository, limited to the host, logging in with UMP's key; Ansible's
// output goes to stderr, stdout stays for results. It returns the exit
// status.
func (a *app) runRunList(ctx context.Context, repo *infraRepo, rl runList, hostname, keyPath string) (int, error) {
	fmt.Fprintf(a.stderr, "Provisioning %s: %s ...\n", hostname, rl)
	args := append(append([]string{}, rl.playbooks...), "--limit", hostname, "--private-key", keyPath)
	var stdin io.Reader
	if a.isTTY != nil && a.isTTY() {
		stdin = a.stdin
	}
	return a.ansible(ctx, repo.dir, "ansible-playbook", args, a.ansibleEnv(repo), stdin, a.stderr, a.stderr)
}

// provisionHost puts the host into the inventory and runs its run list.
func (a *app) provisionHost(ctx context.Context, repo *infraRepo, rl runList, hostname string, v hostVars, keyPath string) error {
	if _, err := a.putInInventory(repo, hostname, rl.groups, v); err != nil {
		return err
	}
	code, err := a.runRunList(ctx, repo, rl, hostname, keyPath)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("ansible-playbook exit status %d, see above", code)
	}
	return nil
}

// provisionFailed is the error of a run list that did not complete,
// exit status 6, unless the user interrupted it.
func (a *app) provisionFailed(ctx context.Context, err error, what, next string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return withExit(exitProvision, fmt.Errorf("provisioning failed: %s (%v); fix that and %s", what, err, next))
}
