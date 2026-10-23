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

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// Provisioning by push: ump runs Ansible from the infrastructure
// repository, the current directory. Every server ump installs is in the
// group common and in the groups given with --group; its run list is
// those groups in order, and each group is a playbook: playbooks/<group>.yml
// of the repository when it has one, else the collection's
// ofzhur.ansible.<group>. ump writes the server's host file into
// inventory/hosts/ (read by the collection's inventory plugin
// ofzhur.ansible.host) and runs the run list for that host alone.

const (
	// commonGroup is the group of every server ump installs; its run
	// list starts with it.
	commonGroup = "common"
	// collection is the Ansible collection with the inventory plugin and
	// the playbooks of the groups the repository has none of.
	collection = "ofzhur.ansible"
	// hostsDir is where the host files go, in the repository.
	hostsDir = "inventory/hosts"
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
	if fi, err := os.Stat(filepath.Join(dir, "inventory")); err != nil || !fi.IsDir() {
		missing = append(missing, "inventory/")
	}
	return &infraRepo{dir: dir}, missing, nil
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

// syntaxCheck runs ansible-playbook --syntax-check on the run list in
// the repository, which also reads the inventory: a group without a
// playbook, a playbook that does not parse and a broken inventory stop
// ump before it changes anything. The output is shown when it fails.
func (a *app) syntaxCheck(ctx context.Context, repo *infraRepo, rl runList) error {
	var out bytes.Buffer
	args := append([]string{"--syntax-check"}, rl.playbooks...)
	code, err := a.ansible(ctx, repo.dir, "ansible-playbook", args, repo.ansibleEnv(), nil, &out, &out)
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
// hosts with the ump_ref of each (empty where a host has none).
type inventory struct {
	groups map[string]bool
	hosts  map[string]string
}

// readInventory reads the repository's inventory with ansible-inventory
// --list; its output is shown when it fails.
func (a *app) readInventory(ctx context.Context, repo *infraRepo) (*inventory, error) {
	var out, errb bytes.Buffer
	code, err := a.ansible(ctx, repo.dir, "ansible-inventory", []string{"--list"}, repo.ansibleEnv(), nil, &out, &errb)
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
// when the new one is written). retry is what the user can do about the
// hostname.
func (inv *inventory) check(hostname, ref string, rl runList, retry string) error {
	if inv.groups[hostname] {
		return withExit(exitUsage, fmt.Errorf("the hostname %s is also a group of the inventory: %s; %s", hostname, sameName, retry))
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

type hostVars struct {
	AnsibleHost    string            `yaml:"ansible_host,omitempty"`
	AnsibleUser    string            `yaml:"ansible_user,omitempty"`
	UMPName        string            `yaml:"ump_name,omitempty"`
	UMPRef         string            `yaml:"ump_ref,omitempty"`
	UMPProvider    string            `yaml:"ump_provider,omitempty"`
	UMPAccount     string            `yaml:"ump_account,omitempty"`
	UMPLocation    string            `yaml:"ump_location,omitempty"`
	UMPProduct     string            `yaml:"ump_product,omitempty"`
	UMPOS          string            `yaml:"ump_os,omitempty"`
	UMPProfile     string            `yaml:"ump_profile,omitempty"`
	UMPInstalledAt string            `yaml:"ump_installed_at,omitempty"`
	UMPAttributes  map[string]string `yaml:"ump_attributes,omitempty"`
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
// without ump_ref, or with another server's.
func (r *infraRepo) checkHostFiles(hostname, ref, retry string) error {
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

// putInInventory writes the host file and says so.
func (a *app) putInInventory(repo *infraRepo, hostname string, groups []string, v hostVars) (string, error) {
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
	return a.ansible(ctx, repo.dir, "ansible-playbook", args, repo.ansibleEnv(), stdin, a.stderr, a.stderr)
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
