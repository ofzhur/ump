package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

// ump server multiinstall: installations on several servers side by
// side, each as ump server reinstall makes one (runInstall), after one
// round of checks and one question, and one provisioning run for all of
// them at the end.

type multiOptions struct {
	// server are the flags of serverFlags, for every server
	server      installOptions
	plan        string
	parallel    int
	noProvision bool
	dryRun      bool
	yes         bool
	output      string
	hosts       string
	playbooks   listFlag
}

func (a *app) newServerMultiinstallCmd() *cobra.Command {
	var o multiOptions
	sub := &cobra.Command{
		Use:     "multiinstall NAME... | --plan FILE",
		Aliases: []string{"parallel-install"},
		Short:   "Install operating systems on several servers at once",
		Long: `Install several servers at once, each the way ump server reinstall
installs one, and provision them with one run. The servers are named on
the command line, all with the same values of reinstall's flags for a
server (--profile, --layout, --group and the others), or in a plan file
(--plan), each with values of its own:

  defaults:                  # for every server
    profile: web
    group: [web]
  servers:                   # each over the defaults
    web-01: {}
    web-02:
      layout: lvm-raid1
    db-01:
      profile: db
      group: [db, backup]
      hostname: db-01.example.net
      user: [oleg:local, deploy]

The keys are the flags' names (profile, layout, firewall, os, hostname,
timezone, disks, ssh-key, set, user, userdata, group, ipv6, mitigations,
via, storage), with a list for a repeatable one; a server's value
replaces that of defaults. Any other key, a key without a value and a
second YAML document are refused, and so is one of these flags on the
command line with --plan.

Every check comes first, and a failure stops ump (exit status 2) before
any server is touched: each server is resolved, once; its profile,
layout, firewall, users, userdata and keys, as reinstall resolves them,
and its architecture where the provider tells it; the hostnames, each
once among the servers, none a group of another server (new, or in the
state) and none another's in the inventory; the Ansible layout, the
playbooks, and --hosts against the inventory with every new host in it;
and no server may be locked. A table shows the servers, and one question
covers them all (without a terminal, --yes is required).

The installations run side by side, --parallel N at a time (default:
all), each under its server's lock, their progress line by line, each
line with [NAME] in front. A failed installation does not stop the
others; its server stays where it stopped, in the UMP rescue system as a
rule, as with reinstall. Once all are done, ump records every installed
server and runs ump provision once, under the fleet's lock: the host
files, then the playbooks on the fleet (--hosts PATTERN and --playbook
NAME narrow it); each server is provisioned by its own line of the
recap. --no-provision runs nothing (where the layout is there, the host
files are still written). --dry-run brings every server into the rescue
system and prints each installer's plan, side by side; nothing is
written, nothing provisioned.

A summary says per server what was installed, what failed and why, and
what was provisioned; -o json|yaml prints that per server, with the
run's result. Exit status 1 means an installation failed, 6 that the
provisioning did not complete (on any host), 0 that all went well.
docs/cli.md describes the plan file.`,
		Example: `  ump server multiinstall web-01 web-02 web-03 --group web --dry-run
  ump server multiinstall web-01 web-02 web-03 --profile web --group web
  ump server multiinstall --plan fleet.yaml --parallel 4
  ump server multiinstall --plan fleet.yaml --hosts db --yes -o json`,
		Args: usageArgs(func(cmd *cobra.Command, args []string) error {
			switch {
			case o.plan != "" && len(args) > 0:
				return errors.New("give the servers as NAME... or in the plan, not both")
			case o.plan == "" && len(args) == 0:
				return errors.New("give the servers, NAME..., or a plan, --plan FILE")
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.multiInstall(cmd, args, o)
		},
	}
	addServerFlags(sub, &o.server)
	sub.Flags().StringVar(&o.plan, "plan", "", "a plan file (YAML): the servers, each with values of its own for reinstall's flags, over defaults")
	sub.Flags().IntVar(&o.parallel, "parallel", 0, "how many installations run at a time (default: all)")
	sub.Flags().BoolVar(&o.dryRun, "dry-run", false, "print each installer's plan instead of installing")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	sub.Flags().BoolVar(&o.noProvision, "no-provision", false, "do not provision: no Ansible layout needed, no playbook runs")
	hostsFlag(sub, &o.hosts, "an Ansible host pattern: provision the hosts it matches alone (default: the whole fleet)")
	sub.Flags().StringArrayVar((*[]string)(&o.playbooks), "playbook", nil, "a playbook of playbooks/, without .yml: provision with those alone (repeatable, or comma-separated; default: every one)")
	return sub
}

// multiEntry is one server of a multiinstall: as named, with its flags
// and their checked values; where names it in a plan.
type multiEntry struct {
	ref   string
	o     installOptions
	f     installFlags
	where string
}

// fail names the entry in an error about it.
func (e multiEntry) fail(err error) error {
	if e.where != "" {
		return fmt.Errorf("%s: %w", e.where, err)
	}
	return fmt.Errorf("%s: %w", e.ref, err)
}

// inPlan names the entry of a plan in an error that names the server.
func (e multiEntry) inPlan(err error) error {
	if e.where != "" {
		return fmt.Errorf("%s: %w", e.where, err)
	}
	return err
}

// multiServer is what multiinstall reports of one server: its
// installation's result, whether it was installed, and why not or why it
// was not provisioned.
type multiServer struct {
	installResult `yaml:",inline"`
	Installed     bool   `json:"installed" yaml:"installed"`
	Reason        string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// ref is the server as named
	ref string
}

// multiResult is what multiinstall reports: the servers in order, and
// the provisioning run.
type multiResult struct {
	Servers   []multiServer    `json:"servers" yaml:"servers"`
	Provision *provisionResult `json:"provision,omitempty" yaml:"provision,omitempty"`
}

// installOutcome is how an installation of multiinstall ended: its
// result, the record when it completed, and the error when not; unlock
// releases the server's lock, which a completed installation keeps until
// its provisioning is done.
type installOutcome struct {
	result installResult
	record *serverRecord
	err    error
	unlock func()
}

func (a *app) multiInstall(cmd *cobra.Command, refs []string, o multiOptions) error {
	format, err := resultFormat(cmd, o.output)
	if err != nil {
		return err
	}
	if cmd.Flags().Changed("parallel") && o.parallel < 1 {
		return usageErrorf(cmd, "--parallel %d: give 1 or more (default: all at once)", o.parallel)
	}
	named, err := playbookNames(cmd, o.playbooks)
	if err != nil {
		return err
	}
	pattern, err := hostsPattern(cmd, o.hosts)
	if err != nil {
		return err
	}
	if o.noProvision && (pattern != "" || len(named) > 0) {
		return usageErrorf(cmd, "--hosts and --playbook narrow the provisioning, which --no-provision leaves out")
	}
	entries, err := multiEntries(cmd, refs, o)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	repo, run, inv, unchecked, err := a.installRepo(ctx, o.noProvision, named, pattern)
	if err != nil {
		return err
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	plans, warnings, err := a.planInstalls(cmd, cfg, repo, inv, entries, pattern)
	if err != nil {
		// a check that fails is a validation error, whatever it is
		// elsewhere; an aborted question or a provider's error keep theirs
		if exitCodeFor(err) == exitGeneric && ctx.Err() == nil {
			err = withExit(exitUsage, err)
		}
		return err
	}
	accounts := map[string]bool{}
	for _, p := range plans {
		accounts[p.m.Account] = true
	}
	defer func() {
		for acct := range accounts {
			a.invalidateListing(acct)
		}
	}()

	// one table, one question
	var noRescue []string
	for _, p := range plans {
		a.warnNoRescue(p.m, p.prov)
		a.warnPreflight(ctx, p.m, p.prov)
		if noRescueWarning(p.m, p.prov) != "" {
			noRescue = append(noRescue, p.ref)
		}
	}
	for _, w := range warnings {
		fmt.Fprintln(a.stderr, w)
	}
	rows := make([][]string, 0, len(plans))
	for _, p := range plans {
		rows = append(rows, []string{p.ref, p.m.Ref(), p.spec.OS, p.spec.Layout.Name, p.hostname, strings.Join(p.inGroups(), ",")})
	}
	if err := writeTable(a.stderr, []string{"SERVER", "PROVIDER", "OS", "LAYOUT", "HOSTNAME", "GROUPS"}, rows, false); err != nil {
		return err
	}
	question := fmt.Sprintf("Install on these %s? Every disk of each is erased.", plural(len(plans), "server"))
	switch len(noRescue) {
	case 0:
	case 1:
		question += " " + noRescue[0] + " has no rescue system to fall back on: a failed installation is yours to recover."
	default:
		question += " " + strings.Join(noRescue, ", ") + " have no rescue system to fall back on: a failed installation is yours to recover."
	}
	if !o.noProvision {
		question += " Then ump provision runs " + run.String() + "."
	}
	if o.dryRun {
		question = fmt.Sprintf("Bring these %s into the rescue system and show their installation plans? (dry run: nothing is erased)", plural(len(plans), "server"))
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}

	// the installations, side by side
	parallel := o.parallel
	if parallel <= 0 || parallel > len(plans) {
		parallel = len(plans)
	}
	if !o.noProvision {
		fmt.Fprintf(a.stderr, "Provisioning after the installations: %s.\n", run)
	}
	at := "all at once"
	if parallel < len(plans) {
		at = fmt.Sprintf("%d at a time", parallel)
	}
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Installation plans for %s, %s ...\n", plural(len(plans), "server"), at)
	} else {
		fmt.Fprintf(a.stderr, "Installing %s, %s ...\n", plural(len(plans), "server"), at)
	}
	outcomes := a.installAll(cmd, cfg, plans, installRun{dryRun: o.dryRun}, parallel)
	defer func() {
		for _, oc := range outcomes {
			if oc.unlock != nil {
				oc.unlock()
			}
		}
	}()

	res := multiResult{Servers: make([]multiServer, len(plans))}
	var failed, records []string
	var done []serverRecord
	for i, p := range plans {
		oc := outcomes[i]
		s := multiServer{installResult: oc.result, Installed: oc.err == nil && !o.dryRun, ref: p.ref}
		s.Playbooks = run.playbooks
		if oc.err != nil {
			s.Reason = oc.err.Error()
			if errors.Is(oc.err, context.Canceled) {
				s.Reason = "interrupted"
			}
			failed = append(failed, p.ref)
		} else if oc.record != nil {
			done = append(done, *oc.record)
			records = append(records, p.ref)
		}
		res.Servers[i] = s
	}
	interrupted := ctx.Err()
	var provErr error
	switch {
	case o.dryRun:
	case len(done) == 0:
		if !o.noProvision {
			fmt.Fprintln(a.stderr, "Not provisioned: no server was installed.")
		}
	// what was installed is recorded, also after an interrupt
	case !a.recordInstallations(context.WithoutCancel(ctx), done):
		for i, s := range res.Servers {
			if s.Installed {
				res.Servers[i].Reason = "installed but not recorded, and the inventory comes from the record; record it (see above)"
			}
		}
		if o.noProvision {
			fmt.Fprintln(a.stderr, "Not provisioned (--no-provision), nor in the inventory as installed: the inventory comes from the record; record it.")
			break
		}
		provErr = withExit(exitProvision, fmt.Errorf("provisioning failed: %s installed but not recorded, and the inventory comes from the record; record it and run ump provision", namesAre(records)))
	case interrupted != nil:
		why := "interrupted before the provisioning; run ump provision"
		if o.noProvision {
			why = "interrupted; the host files are not written"
		}
		for i, s := range res.Servers {
			if s.Installed {
				res.Servers[i].Reason = why
			}
		}
		them := "them"
		if len(records) == 1 {
			them = "it"
		}
		fmt.Fprintf(a.stderr, "Interrupted: %s installed and recorded; the next ump provision includes %s.\n", namesAre(records), them)
	case o.noProvision && repo != nil:
		hosts := make([]hostEntry, len(done))
		for i, r := range done {
			hosts[i] = hostEntry{r.record.Hostname, append([]string{commonGroup}, r.groups...), a.inventoryVars(cfg, r.m, r.record)}
		}
		if err := a.addToInventory(ctx, repo, "installing "+strings.Join(records, ", "), hosts); err != nil {
			provErr = withExit(exitCodeFor(err), fmt.Errorf("%s installed and recorded, but not all written into the inventory: %w", namesAre(records), err))
			break
		}
		fmt.Fprintln(a.stderr, "Not provisioned (--no-provision); the next ump provision includes the servers.")
	case o.noProvision:
		why := "no Ansible layout here"
		if unchecked != "" {
			why = unchecked
		}
		fmt.Fprintf(a.stderr, "Not provisioned (--no-provision), and no host files (%s): ump provision, run from the infrastructure repository, writes them and includes the servers; where ump is the inventory, they are in it already.\n", why)
	default:
		known := make([]provider.Machine, len(done))
		for i, r := range done {
			known[i] = r.m
		}
		prov, err := a.provision(ctx, cfg, repo, run, "provision after installing "+strings.Join(records, ", "), known)
		res.Provision = prov
		all := true
		for i, s := range res.Servers {
			if !s.Installed {
				continue
			}
			own, ran := recapOf(prov, s.Hostname)
			res.Servers[i].Provisioned = ran && own.Failed == 0 && own.Unreachable == 0
			switch {
			case prov == nil:
				res.Servers[i].Reason = "the provisioning did not run (see below)"
			case !ran:
				res.Servers[i].Reason = "no play of the run matched it"
			case own.Failed > 0:
				res.Servers[i].Reason = "failed in the run (see above)"
			case own.Unreachable > 0:
				res.Servers[i].Reason = "unreachable in the run (see above)"
			}
			all = all && res.Servers[i].Provisioned
		}
		var rf *runFailure
		switch {
		case err != nil && ctx.Err() != nil:
			interrupted = ctx.Err()
		case err != nil && all && errors.As(err, &rf):
			provErr = withExit(exitProvision, fmt.Errorf("%s provisioned; the run failed%s", namesAre(records), rf.detail()))
		case errors.As(err, &rf):
			provErr = err
		case err != nil:
			provErr = withExit(exitCodeFor(err), fmt.Errorf("%s installed and recorded, but not provisioned: %w", namesAre(records), err))
		default:
			fmt.Fprintln(a.stderr, prov.summary())
		}
	}

	if format != "" {
		if err := printEncoded(a.stdout, res, format); err != nil {
			return err
		}
	} else if err := a.multiSummary(res, o.dryRun, !o.noProvision); err != nil {
		return err
	}
	if interrupted != nil {
		return interrupted
	}
	var msgs []string
	code := exitOK
	if len(failed) > 0 {
		code = exitGeneric
		msgs = append(msgs, "the installation failed on "+strings.Join(failed, ", ")+", see above")
	}
	if provErr != nil {
		if code == exitOK {
			code = exitCodeFor(provErr)
		}
		msgs = append(msgs, provErr.Error())
	}
	if len(msgs) > 0 {
		return withExit(code, errors.New(strings.Join(msgs, "; ")))
	}
	return nil
}

// namesAre is servers' names as the subject of a sentence: web-01 is,
// web-01, web-02 are.
func namesAre(names []string) string {
	if len(names) == 1 {
		return names[0] + " is"
	}
	return strings.Join(names, ", ") + " are"
}

// recapOf is the line of a host in a run's recap.
func recapOf(res *provisionResult, host string) (hostRecap, bool) {
	if res == nil {
		return hostRecap{}, false
	}
	r, ok := res.Hosts[host]
	return r, ok
}

// multiSummary prints the result as a table: per server whether it was
// installed (dry-run for a plan alone) and provisioned (- where nothing
// ran), and why not.
func (a *app) multiSummary(res multiResult, dryRun, provisioning bool) error {
	rows := make([][]string, 0, len(res.Servers))
	for _, s := range res.Servers {
		installed, provisioned := "no", "-"
		switch {
		case s.Installed:
			installed = "yes"
		case dryRun && s.Reason == "":
			installed = "dry-run"
		}
		if s.Installed && provisioning {
			provisioned = "no"
			if s.Provisioned {
				provisioned = "yes"
			}
		}
		rows = append(rows, []string{s.ref, s.Hostname, installed, provisioned, s.Reason})
	}
	return writeTable(a.stdout, []string{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"}, rows, false)
}

// multiEntries are the servers of a multiinstall with their flags: those
// named, each with the flags given, or those of the plan, whose values
// none of the flags may compete with.
func multiEntries(cmd *cobra.Command, refs []string, o multiOptions) ([]multiEntry, error) {
	var entries []multiEntry
	if o.plan == "" {
		for _, r := range refs {
			entries = append(entries, multiEntry{ref: r, o: o.server})
		}
	} else {
		for _, name := range serverFlags {
			if cmd.Flags().Changed(name) {
				return nil, usageErrorf(cmd, "--%s and --plan: the plan has the servers' values, and its defaults those for every server", name)
			}
		}
		data, err := os.ReadFile(expandHome(o.plan))
		if err != nil {
			return nil, withExit(exitUsage, err)
		}
		p, err := parsePlan(data)
		if err != nil {
			return nil, withExit(exitUsage, fmt.Errorf("%s: %w", o.plan, err))
		}
		for _, s := range p.servers {
			entries = append(entries, multiEntry{ref: s.name, o: p.defaults.over(s.values).options(), where: o.plan + ": servers." + s.name})
		}
	}
	for i := range entries {
		f, err := checkServerFlags(cmd, entries[i].o)
		if err != nil {
			return nil, entries[i].inPlan(err)
		}
		entries[i].f = f
	}
	return entries, nil
}

// planInstalls makes every check of a multiinstall that needs no server
// touched, and returns the plans, in order, with the warnings the
// question covers. Each server is resolved against one listing, once.
func (a *app) planInstalls(cmd *cobra.Command, cfg *config.Config, repo *infraRepo, inv *inventory, entries []multiEntry, pattern string) ([]*installPlan, []string, error) {
	ctx := cmd.Context()
	machines, byAccount, failed, total, err := a.listedMachines(cmd)
	if err != nil {
		return nil, nil, err
	}
	var plans []*installPlan
	for _, e := range entries {
		m, prov, err := a.resolveListed(cmd, machines, byAccount, failed, total, e.ref)
		if err != nil {
			return nil, nil, e.inPlan(err)
		}
		for _, q := range plans {
			if q.m.Ref() == m.Ref() {
				return nil, nil, withExit(exitUsage, fmt.Errorf("%s and %s are the same server, %s; name it once", q.ref, e.ref, m.Ref()))
			}
		}
		p, err := newInstallPlan(e.ref, m, prov, e.o, e.f)
		if err != nil {
			return nil, nil, e.inPlan(err)
		}
		plans = append(plans, p)
	}
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	// the profiles as reinstall resolves them, and the architectures
	// where the provider tells them; the storage is asked once per OS
	var warnings []string
	warned := false
	archs := map[string]map[string]string{}
	for i, p := range plans {
		e := entries[i]
		if p.keys, p.keyPaths, p.keyPath, err = a.rescueKeys(cfg, e.o.keyFiles); err != nil {
			return nil, nil, e.fail(err)
		}
		if len(p.keys) == 1 && !warned {
			a.warnUMPKeyOnly(p.ref)
			warned = true
		}
		if err := a.resolveInstall(ctx, cmd, cfg, doc, p, e.o, e.f); err != nil {
			return nil, nil, e.fail(err)
		}
		key := p.storage + " " + p.spec.OS
		bad, ok := archs[key]
		if !ok {
			bad = a.osArchs(ctx, p.spec.OS, p.storage)
			archs[key] = bad
		}
		switch {
		case len(bad) == 0:
		case p.m.Arch != "":
			if err := archRefusal(p.m, p.spec.OS, p.m.Arch, "as the provider says", bad); err != nil {
				return nil, nil, err
			}
		default:
			warnings = append(warnings, fmt.Sprintf("Warning: %s exists for %s only, and the provider of %s does not say its architecture. On another architecture the installation stops in the rescue system, after the server was rebooted into it or, without a rescue system, reinstalled by its provider: a wiped server and no new system.", p.spec.OS, strings.Join(archsWith(bad), " and "), p.ref))
		}
	}
	// the hostnames: each once, none a group of another new host or of
	// another server in the state, and in the inventory where no other
	// server's host is
	for i, p := range plans {
		for _, q := range plans[:i] {
			if q.hostname == p.hostname {
				return nil, nil, withExit(exitUsage, fmt.Errorf("%s and %s would both have the hostname %s; give one of them another (hostname: in a plan)", q.ref, p.ref, p.hostname))
			}
		}
		for _, q := range plans {
			if q != p && slices.Contains(q.groups, p.hostname) {
				return nil, nil, withExit(exitUsage, fmt.Errorf("the hostname %s of %s is also a group of %s: %s; give one of them another", p.hostname, p.ref, q.ref, sameName))
			}
		}
		if err := checkStateGroups(doc, p.hostname, p.m.Ref(), "give "+p.ref+" another hostname"); err != nil {
			return nil, nil, err
		}
	}
	if repo != nil {
		files, err := repo.readHostFiles(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range plans {
			retry := "give " + p.ref + " another hostname"
			if err := checkHostName(p.hostname, p.inGroups(), retry, ""); err != nil {
				return nil, nil, err
			}
			if err := files.check(p.hostname, p.m.Ref(), retry); err != nil {
				return nil, nil, err
			}
			if err := inv.check(p.hostname, p.m.Ref(), p.inGroups(), retry, ""); err != nil {
				return nil, nil, err
			}
		}
	}
	// --hosts matches a host of the inventory as it will be, with every
	// new host in it
	if pattern != "" {
		hosts := map[string]inventoryHost{}
		var with []string
		for _, p := range plans {
			hosts[p.hostname] = inventoryHost{m: p.m, groups: p.inGroups()}
			with = append(with, p.hostname+" in "+strings.Join(p.inGroups(), ", "))
		}
		if err := a.matchHosts(ctx, repo, inv.with(hosts), pattern, fmt.Sprintf("--hosts %s matches no host of the inventory with %s; nothing was changed", pattern, strings.Join(with, "; "))); err != nil {
			return nil, nil, err
		}
	}
	// no server is locked: each installation takes its server's lock,
	// and one held now would fail it after the question
	locks, err := a.stateStore().Locks(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range locks {
		for _, p := range plans {
			if l.Key == p.m.Ref() {
				return nil, nil, withExit(exitUsage, fmt.Errorf("%s: %w; nothing was changed", p.ref, &backend.ErrLocked{Key: l.Key, Info: l}))
			}
		}
	}
	return plans, warnings, nil
}

// installAll runs the planned installations side by side, parallel at
// a time, in order, each under its server's lock and as a copy of the
// app whose stderr puts [NAME] in front of every line; with --verbose,
// the copy builds its server's provider, so that the API calls are such
// lines too. A completed installation keeps its lock
// (installOutcome.unlock); an interrupt leaves the installations not
// started yet with its error.
func (a *app) installAll(cmd *cobra.Command, cfg *config.Config, plans []*installPlan, r installRun, parallel int) []installOutcome {
	ctx := cmd.Context()
	// what the copies share: the state's store, a bad wait said once, and
	// with --verbose the settings of the servers' accounts, resolved once
	// for all of them
	a.stateStore()
	a.waitLimit(waitTimeout)
	settings := map[string]map[string]string{}
	for _, p := range plans {
		acct, ok := cfg.Accounts[p.m.Account]
		if _, done := settings[p.m.Account]; !a.verbose || !ok || done {
			continue
		}
		if s, err := acct.Resolved(); err == nil {
			settings[p.m.Account] = s
		}
	}
	outcomes := make([]installOutcome, len(plans))
	next := make(chan int)
	go func() {
		for i := range plans {
			next <- i
		}
		close(next)
	}()
	var lines sync.Mutex
	var wg sync.WaitGroup
	for range parallel {
		wg.Go(func() {
			for i := range next {
				p := plans[i]
				if err := ctx.Err(); err != nil {
					outcomes[i] = installOutcome{result: p.result(r.dryRun), err: err}
					continue
				}
				w := &linePrefix{mu: &lines, w: a.stderr, prefix: "[" + p.ref + "] "}
				sub := *a
				sub.stderr, sub.stdout, sub.isTTY = w, w, nil
				if s, ok := settings[p.m.Account]; ok {
					if prov, err := provider.Build(cfg.Accounts[p.m.Account].Type, p.m.Account, s, sub.providerOptions()); err == nil {
						own := *p
						own.prov = prov
						p = &own
					}
				}
				outcomes[i] = sub.installOne(cmd, p, r)
				w.Flush()
			}
		})
	}
	wg.Wait()
	return outcomes
}

// installOne runs one installation of multiinstall under its server's
// lock, which it keeps when the installation completed, for its
// provisioning.
func (a *app) installOne(cmd *cobra.Command, p *installPlan, r installRun) installOutcome {
	unlock, err := a.lockServer(cmd.Context(), p.m, "multiinstall")
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
		return installOutcome{result: p.result(r.dryRun), err: err}
	}
	result, record, err := a.runInstall(cmd, p, r)
	oc := installOutcome{result: result, err: err}
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.stderr, "ump: interrupted")
	case err != nil:
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
	}
	if record == nil {
		unlock()
		return oc
	}
	oc.record, oc.unlock = &serverRecord{p.m, *record, p.groups}, unlock
	return oc
}

// linePrefix passes what is written to w a whole line at a time, with
// the prefix in front: the progress of installations side by side, each
// line whole. mu is shared by the writers to w.
type linePrefix struct {
	mu      *sync.Mutex
	w       io.Writer
	prefix  string
	partial []byte
}

func (l *linePrefix) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.w.Write(append([]byte(l.prefix), l.partial[:i+1]...))
		l.partial = l.partial[i+1:]
	}
}

// Flush passes on a last line without a newline, with one.
func (l *linePrefix) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.w.Write(append(append([]byte(l.prefix), l.partial...), '\n'))
		l.partial = nil
	}
}

// installPlanFile is a plan of multiinstall as read: the values for
// every server, and the servers in the file's order, each with its own.
type installPlanFile struct {
	defaults planValues
	servers  []planServer
}

type planServer struct {
	name   string
	values planValues
}

// planValues are the values of a plan's defaults or of one of its
// servers, under the names of the flags (serverFlags); nil is not given.
type planValues struct {
	Profile     *string  `yaml:"profile"`
	Layout      *string  `yaml:"layout"`
	Firewall    *string  `yaml:"firewall"`
	OS          *string  `yaml:"os"`
	Hostname    *string  `yaml:"hostname"`
	Timezone    *string  `yaml:"timezone"`
	Disks       planList `yaml:"disks"`
	SSHKey      planList `yaml:"ssh-key"`
	Set         planList `yaml:"set"`
	User        planList `yaml:"user"`
	Userdata    planList `yaml:"userdata"`
	Group       planList `yaml:"group"`
	IPv6        *bool    `yaml:"ipv6"`
	Mitigations *bool    `yaml:"mitigations"`
	Via         *string  `yaml:"via"`
	Storage     *string  `yaml:"storage"`
}

// planList is the value of a repeatable flag: a list, or one value.
type planList []string

func (l *planList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = planList{n.Value}
		return nil
	case yaml.SequenceNode:
		out := planList{}
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a list of values, one per line or in [ ]", c.Line)
			}
			out = append(out, c.Value)
		}
		*l = out
		return nil
	}
	return fmt.Errorf("line %d: a value, or a list of values", n.Line)
}

// over is the values of a server over the defaults: each it gives
// replaces theirs.
func (v planValues) over(server planValues) planValues {
	out := v
	dst, src := reflect.ValueOf(&out).Elem(), reflect.ValueOf(server)
	for i := range src.NumField() {
		if !src.Field(i).IsNil() {
			dst.Field(i).Set(src.Field(i))
		}
	}
	return out
}

// options are the values as the flags of one installation.
func (v planValues) options() installOptions {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	o := installOptions{
		profile: str(v.Profile), layout: str(v.Layout), firewall: str(v.Firewall), os: str(v.OS),
		hostname: str(v.Hostname), timezone: str(v.Timezone), storage: str(v.Storage), via: str(v.Via),
		disks: strings.Join(v.Disks, ","), keyFiles: v.SSHKey, sets: v.Set, users: v.User, userdata: v.Userdata,
		groups: groupFlags(v.Group), ipv6: v.IPv6 != nil && *v.IPv6, mitigations: v.Mitigations != nil && *v.Mitigations,
	}
	if o.via == "" {
		o.via = "auto"
	}
	return o
}

// parsePlan reads a plan: a mapping of defaults (the values for every
// server) and servers (each server's name with its values), strictly: one
// document, no other key, no key twice or without a value, no value of
// the wrong kind.
func parsePlan(data []byte) (*installPlanFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc, more yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty; a plan has servers, and defaults for them")
	}
	switch err := dec.Decode(&more); {
	case err == nil:
		return nil, fmt.Errorf("line %d: a plan is one YAML document", more.Line)
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	root := resolveAlias(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: a plan is a mapping of defaults and servers", root.Line)
	}
	p := &installPlanFile{}
	var servers *yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], resolveAlias(root.Content[i+1])
		if seen[k.Value] {
			return nil, fmt.Errorf("line %d: %s is given twice", k.Line, k.Value)
		}
		seen[k.Value] = true
		switch k.Value {
		case "defaults":
			var err error
			if p.defaults, err = planEntry(v, "defaults"); err != nil {
				return nil, err
			}
		case "servers":
			servers = v
		default:
			return nil, fmt.Errorf("line %d: %s is no part of a plan (defaults, servers)", k.Line, k.Value)
		}
	}
	switch {
	case servers == nil:
		return nil, errors.New("no servers")
	case servers.Kind != yaml.MappingNode:
		return nil, fmt.Errorf("line %d: servers is a mapping of the servers' names to their values", servers.Line)
	case len(servers.Content) == 0:
		return nil, fmt.Errorf("line %d: no servers", servers.Line)
	}
	names := map[string]bool{}
	for i := 0; i+1 < len(servers.Content); i += 2 {
		k, v := servers.Content[i], resolveAlias(servers.Content[i+1])
		name := strings.TrimSpace(k.Value)
		switch {
		case k.Kind != yaml.ScalarNode || name == "":
			return nil, fmt.Errorf("line %d: a server is named by its name, ID, IP address or provider/id", k.Line)
		case names[name]:
			return nil, fmt.Errorf("line %d: server %s is given twice", k.Line, name)
		}
		names[name] = true
		values, err := planEntry(v, "servers."+name)
		if err != nil {
			return nil, err
		}
		p.servers = append(p.servers, planServer{name, values})
	}
	return p, nil
}

// planEntry reads the values of defaults or of a server (where): a
// mapping of the flags' names, or nothing.
func planEntry(n *yaml.Node, where string) (planValues, error) {
	var v planValues
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" {
		return v, nil
	}
	if n.Kind != yaml.MappingNode {
		return v, fmt.Errorf("line %d: %s is a mapping of reinstall's flags to their values", n.Line, where)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], resolveAlias(n.Content[i+1])
		switch {
		case !slices.Contains(serverFlags, k.Value):
			return v, fmt.Errorf("line %d: %s: %s is no flag of a server (%s)", k.Line, where, k.Value, strings.Join(serverFlags, ", "))
		case seen[k.Value]:
			return v, fmt.Errorf("line %d: %s: %s is given twice", k.Line, where, k.Value)
		case val.Kind == yaml.ScalarNode && (val.ShortTag() == "!!null" || val.Value == ""):
			return v, fmt.Errorf("line %d: %s: %s has no value", k.Line, where, k.Value)
		}
		seen[k.Value] = true
	}
	if err := n.Decode(&v); err != nil {
		return v, fmt.Errorf("%s: %w", where, err)
	}
	return v, nil
}

// resolveAlias is the node an alias stands for.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}
