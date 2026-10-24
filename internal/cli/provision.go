package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// Provisioning: what a server becomes after its operating system is
// installed. ump puts every server it installs into the inventory of the
// infrastructure repository, in the group common and the groups given
// with --group, and runs the repository's playbooks over the inventory,
// the fleet (see ansible.go); docs/provisioning.md describes the model.

// groupName is what Ansible takes as a group name; ump keeps to lower
// case.
var groupName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ansibleOwnName says that a name is Ansible's own: its groups all and
// ungrouped, localhost, and _meta, the host variables in the JSON of an
// inventory script.
func ansibleOwnName(name string) bool {
	switch name {
	case "all", "ungrouped", "localhost", "_meta":
		return true
	}
	return false
}

// listFlag is a repeatable flag whose values are comma-separated lists.
type listFlag []string

// values are the names given, in order, without blanks and duplicates.
func (f listFlag) values() []string {
	var out []string
	for _, v := range f {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// groupFlags is --group (or --ungroup), repeatable, each a
// comma-separated list.
type groupFlags listFlag

func (f *groupFlags) add(cmd *cobra.Command, name, what string) {
	cmd.Flags().StringArrayVar((*[]string)(f), name, nil, what+" (repeatable, or comma-separated)")
}

// list returns the groups in the order given, without duplicates and
// without common, which every server is in anyway.
func (f groupFlags) list(cmd *cobra.Command) ([]string, error) {
	var out []string
	for _, g := range listFlag(f).values() {
		switch {
		case g == commonGroup:
			continue
		case ansibleOwnName(g):
			return nil, usageErrorf(cmd, "group %q: all, ungrouped, localhost and _meta are Ansible's own names", g)
		case !groupName.MatchString(g):
			return nil, usageErrorf(cmd, "group %q: a group name is lower-case letters, digits and '_', not starting with a digit", g)
		}
		out = append(out, g)
	}
	return out, nil
}

// playbookNames reads --playbook: the names of playbooks/NAME.yml (a
// .yml given along is taken off).
func playbookNames(cmd *cobra.Command, f listFlag) ([]string, error) {
	var names []string
	for _, n := range f.values() {
		name := strings.TrimSuffix(n, ".yml")
		if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
			return nil, usageErrorf(cmd, "--playbook %q: a playbook is named by its file in %s/, without .yml", n, playbooksDir)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// hostsFlag adds --hosts.
func hostsFlag(cmd *cobra.Command, p *string, what string) {
	cmd.Flags().StringVar(p, "hosts", "", what)
}

// hostsPattern is --hosts, refused when given empty.
func hostsPattern(cmd *cobra.Command, value string) (string, error) {
	pattern := strings.TrimSpace(value)
	if cmd.Flags().Changed("hosts") && pattern == "" {
		return "", usageErrorf(cmd, "--hosts is empty: give a host pattern, or leave it out for the whole fleet")
	}
	return pattern, nil
}

// fleetLock is the lock of a provisioning run in the state backend,
// beside those of the servers (provider/id): one run at a time.
const fleetLock = "fleet"

// lockFleet takes the fleet's lock, waiting for another run's up to the
// configured wait, and returns the release; still held then is exit
// status 3, as a server's lock is.
func (a *app) lockFleet(ctx context.Context, what string) (func(), error) {
	st := a.stateStore()
	unlock, err := st.Lock(ctx, fleetLock, what)
	var locked *backend.ErrLocked
	if errors.As(err, &locked) {
		limit := a.waitLimit(waitTimeout)
		fmt.Fprintf(a.stderr, "Waiting for the fleet's lock, held by %s since %s (%s), up to %s ...", locked.Info.Who, a.fmtTime(locked.Info.Since), locked.Info.What, fmtWait(limit))
		start := time.Now()
		deadline := start.Add(limit)
		tick := a.waitTicker(start)
		for errors.As(err, &locked) {
			if time.Now().After(deadline) {
				fmt.Fprintln(a.stderr)
				return nil, withExit(exitAborted, fmt.Errorf("waited %s: %w", fmtWait(limit), err))
			}
			if err := sleepCtx(ctx, waitPoll); err != nil {
				fmt.Fprintln(a.stderr)
				return nil, err
			}
			tick()
			unlock, err = st.Lock(ctx, fleetLock, what)
		}
		if err == nil {
			fmt.Fprintf(a.stderr, " free after %s.\n", time.Since(start).Round(time.Second))
		} else {
			fmt.Fprintln(a.stderr)
		}
	}
	if err != nil {
		return nil, err
	}
	return func() {
		if err := unlock(); err != nil {
			fmt.Fprintf(a.stderr, "ump: releasing the fleet's lock: %v\n", err)
		}
	}, nil
}

type provisionOptions struct {
	hosts     string
	playbooks listFlag
	yes       bool
	output    string
}

func (a *app) newProvisionCmd() *cobra.Command {
	var o provisionOptions
	cmd := &cobra.Command{
		Use:   "provision [--hosts PATTERN] [--playbook NAME]... [--yes] [-o json|yaml] [-- ANSIBLE-ARGS...]",
		Short: "Run the playbooks of the infrastructure repository on the fleet",
		Long: `Converge the fleet: run the playbooks of the infrastructure repository
on every host of its inventory. ump runs from the root of that
repository: it needs ansible.cfg, inventory/ and playbooks/ there, and
ansible-playbook in PATH.

Every playbooks/*.yml runs, in file-name order, in one ansible-playbook,
on the hosts its plays name (hosts: web, hosts: all): a server's groups
say which plays it gets. --playbook NAME (the file name without .yml;
repeatable, or comma-separated) runs those alone, --hosts PATTERN (an
Ansible host pattern: web-01, web, 'web:&prod', 'all:!lab-*') runs on
the hosts it matches alone. Arguments after -- go to ansible-playbook
(--check --diff, -v, --tags; not --limit or --inventory).

ump checks the playbooks with ansible-playbook --syntax-check. Then,
under the fleet's lock (one run at a time: another waits for it up to
the configured wait), it checks the names of the inventory
(ansible-inventory) and that --hosts matches a host of it as it will
be, writes the host files of every server it installed (into
inventory/hosts/, or the storage inventory/hosts.yml points to, with
the credentials of [ofzhur_storage] in ansible.cfg; none when ump is
the inventory), and runs the playbooks, logging in to every host as its
administrator (ump_admin) with UMP's key only.

Exit status 6 names the hosts that failed or were unreachable: fix that
and run ump provision --hosts with them. Without --hosts, asks for
confirmation on a terminal (the whole fleet); without one, --yes is
required. docs/provisioning.md describes the repository.`,
		Example: `  ump provision
  ump provision --hosts web-01
  ump provision --hosts web --playbook 10-web
  ump provision --yes -- --check --diff`,
		Args: usageArgs(func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 && len(args) > 0 {
				return errors.New("ump provision takes no arguments; ansible-playbook's go after --")
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.provisionFleet(cmd, o, args)
		},
	}
	hostsFlag(cmd, &o.hosts, "an Ansible host pattern: run on the hosts it matches alone (default: every host of the inventory)")
	cmd.Flags().StringArrayVar((*[]string)(&o.playbooks), "playbook", nil, "a playbook of playbooks/, without .yml: run those alone (repeatable, or comma-separated; default: every one)")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	return cmd
}

// shortValued are the short options of ansible-playbook that take a
// value: in a cluster (-vl), what follows one is its value.
const shortValued = "ceilMtTu"

// ansibleArgs refuses the arguments for ansible-playbook that would go
// around ump's checks: the inventory is the repository's, and the hosts
// are those of --hosts. ansible-playbook takes a long option by a
// prefix (--lim) and short ones in a cluster (-vl), and so does this.
func ansibleArgs(cmd *cobra.Command, args []string) error {
	for _, arg := range args {
		var limit, inventory bool
		name, _, _ := strings.Cut(arg, "=")
		switch {
		case strings.HasPrefix(name, "--") && len(name) > 2:
			limit = strings.HasPrefix("--limit", name)
			inventory = strings.HasPrefix("--inventory-file", name)
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--"):
			for _, c := range arg[1:] {
				limit, inventory = c == 'l', c == 'i'
				if strings.ContainsRune(shortValued, c) {
					break
				}
			}
		}
		switch {
		case limit:
			return usageErrorf(cmd, "%s after --: give the hosts with --hosts, which ump checks first", arg)
		case inventory:
			return usageErrorf(cmd, "%s after --: ump runs on the repository's inventory, which it checks first", arg)
		}
	}
	return nil
}

// resultFormat is -o of a command that prints its result as json or
// yaml, or nothing.
func resultFormat(cmd *cobra.Command, output string) (outputFormat, error) {
	if output == "" {
		return "", nil
	}
	f, err := parseFormat(output)
	if err != nil || (f != formatJSON && f != formatYAML) {
		return "", usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
	}
	return f, nil
}

func (a *app) provisionFleet(cmd *cobra.Command, o provisionOptions, extra []string) error {
	format, err := resultFormat(cmd, o.output)
	if err != nil {
		return err
	}
	named, err := playbookNames(cmd, o.playbooks)
	if err != nil {
		return err
	}
	pattern, err := hostsPattern(cmd, o.hosts)
	if err != nil {
		return err
	}
	if err := ansibleArgs(cmd, extra); err != nil {
		return err
	}
	repo, all, err := a.requireRepo("")
	if err != nil {
		return err
	}
	playbooks, err := choosePlaybooks(all, named)
	if err != nil {
		return err
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := a.syntaxCheck(ctx, repo, playbooks); err != nil {
		return err
	}
	if pattern == "" {
		if err := a.confirm(cmd, o.yes, fmt.Sprintf("Run %s on every host of the inventory, the whole fleet?", strings.Join(playbooks, ", "))); err != nil {
			return err
		}
	}
	run := fleetRun{playbooks: playbooks, narrowed: len(named) > 0, pattern: pattern, extra: extra}
	res, err := a.provision(ctx, cfg, repo, run, "provision", nil)
	if res != nil && format != "" {
		if perr := printEncoded(a.stdout, res, format); perr != nil {
			return perr
		}
	}
	if err != nil {
		return err
	}
	if format == "" {
		fmt.Fprintln(a.stdout, res.summary())
	}
	return nil
}

// fleetRun is a provisioning run: the playbooks (narrowed: chosen with
// --playbook), the pattern of the hosts (--hosts; empty: every host),
// and the user's arguments for ansible-playbook.
type fleetRun struct {
	playbooks []string
	narrowed  bool
	pattern   string
	extra     []string
}

// String is how ump shows the run: 00-base, 10-web on the fleet.
func (r fleetRun) String() string {
	on := "the fleet"
	if r.pattern != "" {
		on = r.pattern
	}
	return strings.Join(r.playbooks, ", ") + " on " + on
}

// retry is the command that runs the run again, on hosts when given.
func (r fleetRun) retry(hosts []string) string {
	cmd := "ump provision"
	switch {
	case len(hosts) > 0:
		cmd += " --hosts " + strings.Join(hosts, ",")
	case r.pattern != "":
		cmd += " --hosts " + shellJoin([]string{r.pattern})
	}
	if r.narrowed {
		cmd += " --playbook " + strings.Join(r.playbooks, ",")
	}
	return cmd
}

// provisionResult is what a run reports: per host the counts of
// Ansible's recap, and the hosts that failed or were unreachable.
type provisionResult struct {
	Playbooks   []string             `json:"playbooks" yaml:"playbooks"`
	Limit       string               `json:"limit,omitempty" yaml:"limit,omitempty"`
	Hosts       map[string]hostRecap `json:"hosts" yaml:"hosts"`
	Failed      []string             `json:"failed,omitempty" yaml:"failed,omitempty"`
	Unreachable []string             `json:"unreachable,omitempty" yaml:"unreachable,omitempty"`
	ExitStatus  int                  `json:"exit_status" yaml:"exit_status"`
}

// summary is the result in a line: Provisioned 3 hosts, 1 changed.
func (r *provisionResult) summary() string {
	if len(r.Hosts) == 0 {
		return "Provisioned: no play matched a host."
	}
	changed := 0
	for _, h := range r.Hosts {
		if h.Changed > 0 {
			changed++
		}
	}
	return fmt.Sprintf("Provisioned %s, %d changed.", plural(len(r.Hosts), "host"), changed)
}

// runFailure is a run that did not complete: the hosts that failed or
// were unreachable, each named with why, else Ansible's exit status;
// retry runs it again.
type runFailure struct {
	names []string
	code  int
	retry string
}

// detail says where the run failed and what to do about it.
func (f *runFailure) detail() string {
	if len(f.names) == 0 {
		return fmt.Sprintf(": ansible-playbook exit status %d, see above; fix that and run %s", f.code, f.retry)
	}
	return fmt.Sprintf(" on %s, see above; fix that and run %s", strings.Join(f.names, ", "), f.retry)
}

func (f *runFailure) Error() string { return "provisioning failed" + f.detail() }

// provision is the run of ump provision after its checks, and of ump
// server reinstall after the installation: under the fleet's lock, the
// names of the inventory and the hosts of the pattern, checked against
// the inventory as it will be, then the host files of every server ump
// installed, and the playbooks on the hosts. known are machines at hand
// (those just installed), which win over the listing. The result is
// there once Ansible ran; a run that failed is exit status 6, naming the
// hosts (a runFailure).
func (a *app) provision(ctx context.Context, cfg *config.Config, repo *infraRepo, run fleetRun, what string, known []provider.Machine) (*provisionResult, error) {
	unlock, err := a.lockFleet(ctx, what)
	if err != nil {
		return nil, err
	}
	defer unlock()
	inv, err := a.readInventory(ctx, repo, "")
	if err != nil {
		return nil, err
	}
	refresh, files, err := a.refreshHosts(ctx, cfg, repo, inv, known)
	if err != nil {
		return nil, err
	}
	if run.pattern != "" {
		if err := a.matchHosts(ctx, repo, inv.with(refresh), run.pattern, fmt.Sprintf("--hosts %s matches no host of the inventory; nothing ran", run.pattern)); err != nil {
			return nil, err
		}
	}
	if err := a.writeHostFiles(ctx, repo, files, refresh); err != nil {
		return nil, err
	}
	matched, err := a.readInventory(ctx, repo, run.pattern)
	if err != nil {
		return nil, err
	}
	if run.pattern != "" && len(matched.hosts) == 0 {
		return nil, withExit(exitUsage, fmt.Errorf("--hosts %s matches no host of the inventory; nothing ran", run.pattern))
	}
	_, keyPath, err := a.stagingKey(cfg)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(a.stderr, "Provisioning: %s (%s) ...\n", run, plural(len(matched.hosts), "host"))
	// the servers of the run installed since a run last completed on them
	// start without the old systems' facts
	var stale []string
	for _, host := range slices.Sorted(maps.Keys(refresh)) {
		if _, ok := matched.hosts[host]; ok && refresh[host].stale {
			stale = append(stale, host)
		}
	}
	code, recap, err := a.runPlaybooks(ctx, repo, run.playbooks, stale, run.pattern, run.extra, keyPath)
	if err != nil {
		return nil, err
	}
	a.freshFacts(ctx, refresh, stale, recap)
	res := &provisionResult{Playbooks: run.playbooks, Limit: run.pattern, Hosts: recap, ExitStatus: code}
	if res.Hosts == nil {
		res.Hosts = map[string]hostRecap{}
	}
	for host, r := range recap {
		if r.Failed > 0 {
			res.Failed = append(res.Failed, host)
		}
		if r.Unreachable > 0 {
			res.Unreachable = append(res.Unreachable, host)
		}
	}
	slices.Sort(res.Failed)
	slices.Sort(res.Unreachable)
	switch {
	case code == 0:
		return res, nil
	case ctx.Err() != nil:
		return res, ctx.Err()
	}
	var names, hosts []string
	for _, h := range res.Failed {
		names, hosts = append(names, h+" (failed)"), append(hosts, h)
	}
	for _, h := range res.Unreachable {
		if !slices.Contains(hosts, h) {
			names, hosts = append(names, h+" (unreachable)"), append(hosts, h)
		}
	}
	return res, withExit(exitProvision, &runFailure{names: names, code: code, retry: run.retry(hosts)})
}

// freshFacts takes the mark of stale facts away from the stale hosts the
// run completed on (no host failed or unreachable in the recap): their
// facts are the new system's now. A host installed again meanwhile keeps
// the mark of its new installation.
func (a *app) freshFacts(ctx context.Context, hosts map[string]inventoryHost, stale []string, recap map[string]hostRecap) {
	var done []string
	for _, host := range stale {
		if r, ok := recap[host]; ok && r.Failed == 0 && r.Unreachable == 0 {
			done = append(done, host)
		}
	}
	if len(done) == 0 {
		return
	}
	err := a.updateState(context.WithoutCancel(ctx), func(d *state.Document) error {
		for _, host := range done {
			h := hosts[host]
			if s, ok := d.Servers[h.m.Ref()]; ok && s.Installed != nil && s.Installed.At.Equal(h.installed) {
				s.StaleFacts = false
				d.Servers[h.m.Ref()] = s
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the facts of %s stay marked stale in the state, and the next run clears them again: %v\n", strings.Join(done, ", "), err)
	}
}

// refreshHosts are the hosts of every server ump installed, as the state
// and the providers have it now, once all of them pass the checks (a
// hostname that is a group of another host, a host file in the way, a
// name the inventory has the other way round), with the host files they
// go into, as read (inventory/hosts/, or the storage): none when one
// does not. When ump is the inventory, the checks are those of the host
// files beside it. The servers of an account that could not be listed
// are none: their host files stay as they are.
func (a *app) refreshHosts(ctx context.Context, cfg *config.Config, repo *infraRepo, inv *inventory, known []provider.Machine) (map[string]inventoryHost, *hostFiles, error) {
	_, path, err := a.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	// the state as it is now, under the lock
	a.doc = nil
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	isKnown := func(m provider.Machine) bool {
		return slices.ContainsFunc(known, func(k provider.Machine) bool { return k.Ref() == m.Ref() })
	}
	ms := slices.Clone(known)
	for _, r := range a.listAccounts(ctx, cfg, cfg.AccountNames(), cache.Open(path), a.cachedListing(cfg)) {
		if r.err != nil {
			fmt.Fprintf(a.stderr, "ump: %v; its servers stay in the inventory as they were\n", r.err)
			continue
		}
		for _, m := range r.machines {
			if !isKnown(m) {
				ms = append(ms, m)
			}
		}
	}
	hosts := a.inventoryHosts(cfg, doc, ms)
	names := slices.Sorted(maps.Keys(hosts))
	// no hostname is a group of another host; common, a group of every
	// host, is refused below
	for _, host := range names {
		for _, other := range names {
			h, o := hosts[host], hosts[other]
			if other != host && host != commonGroup && slices.Contains(o.groups, host) {
				return nil, nil, withExit(exitUsage, fmt.Errorf("the hostname %s of %s is also a group of %s: %s; %s, or reinstall %s with another --hostname", host, h.m.Name, o.m.Name, sameName, ungroup(o.m.Name, host), h.m.Name))
			}
		}
	}
	files, err := repo.readHostFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, host := range names {
		h := hosts[host]
		retry := "reinstall " + h.m.Name + " with another --hostname"
		if err := checkHostName(host, h.groups, retry, h.m.Name); err != nil {
			return nil, nil, err
		}
		if err := files.check(host, h.m.Ref(), retry); err != nil {
			return nil, nil, err
		}
		if err := inv.check(host, h.m.Ref(), h.groups, retry, h.m.Name); err != nil {
			return nil, nil, err
		}
	}
	return hosts, files, nil
}

// writeHostFiles writes the host files of the hosts into files; when ump
// is the inventory there is nothing to write.
func (a *app) writeHostFiles(ctx context.Context, repo *infraRepo, files *hostFiles, hosts map[string]inventoryHost) error {
	if repo.plugin != "" {
		fmt.Fprintf(a.stderr, "No host files: %s makes ump the inventory.\n", repo.plugin)
		return nil
	}
	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[host]
		if err := a.writeHost(ctx, files, host, h.groups, h.vars); err != nil {
			return err
		}
	}
	return nil
}

// hostEntry is a host for the inventory: its name, its groups, its
// variables.
type hostEntry struct {
	hostname string
	groups   []string
	vars     hostVars
}

// addToInventory writes the host files of servers installed without
// provisioning, under the fleet's lock; what names the installation.
func (a *app) addToInventory(ctx context.Context, repo *infraRepo, what string, hosts []hostEntry) error {
	unlock, err := a.lockFleet(ctx, "inventory after "+what)
	if err != nil {
		return err
	}
	defer unlock()
	for _, h := range hosts {
		if err := a.putInInventory(ctx, repo, h.hostname, h.groups, h.vars); err != nil {
			return err
		}
	}
	return nil
}

// notProvisioned is the error of the provisioning after an
// installation: the server is installed and recorded, and the error
// keeps its exit status (6: Ansible failed; 2, 3: it did not run).
func notProvisioned(ctx context.Context, err error, hostname string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return withExit(exitCodeFor(err), fmt.Errorf("%s is installed and recorded, but not provisioned: %w", hostname, err))
}
