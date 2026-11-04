package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	pxeprovider "github.com/ofzhur/ump/internal/provider/pxe"
	"github.com/ofzhur/ump/internal/pxe"
	"github.com/ofzhur/ump/internal/state"
)

// Unattended installations (provider.UnattendedInstaller; PXE): ump
// renders and confirms the specification as reinstall does, reserves
// the private addresses under the server's lock and arms it; the
// machine installs it by itself at its next boot, and its provider
// writes the outcome. ump records the installation only once it reached
// the new system at the server's own address, as its administrator with
// UMP's key and the host keys the installation reported pinned: the ump
// that waits for it, else the next ump provision.

// installWait is how long reinstall --unattended waits for the
// outcome, unless the configured wait is longer, and unattendedArmedFor
// how long the arm of --no-wait stands by default.
var (
	installWait        = time.Hour
	unattendedArmedFor = 2 * time.Hour
)

// unattendedImport is what an armed installation carries for the ump
// that records it (provider.InstallArm.Import): the installation as the
// question showed it, and the private addresses its arm reserved, with
// those the server had before, which it gets back when nothing was
// written.
type unattendedImport struct {
	Hostname string          `json:"hostname"`
	OS       string          `json:"os"`
	Profile  string          `json:"profile"`
	Layout   string          `json:"layout,omitempty"`
	Admin    string          `json:"admin"`
	Groups   []string        `json:"groups,omitempty"`
	Private  []state.Private `json:"private,omitempty"`
	Before   []state.Private `json:"before,omitempty"`
	Reserved bool            `json:"reserved,omitempty"`
}

// importOf is the import of a planned installation whose private
// addresses reservePrivate recorded.
func importOf(p *installPlan) json.RawMessage {
	imp := unattendedImport{Hostname: p.hostname, OS: p.spec.OS, Profile: p.profile, Layout: p.spec.Layout.Name, Admin: p.spec.Admin(),
		Groups: p.groups, Private: p.private, Before: p.before, Reserved: p.reserved}
	data, _ := json.Marshal(imp)
	return data
}

// parseImport reads an arm's import; nil when the arm is gone.
func parseImport(raw json.RawMessage) (*unattendedImport, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var imp unattendedImport
	if err := json.Unmarshal(raw, &imp); err != nil {
		return nil, fmt.Errorf("the arm's import: %w", err)
	}
	return &imp, nil
}

// planned is the installation an import says, as a plan whose private
// addresses releasePrivate can give back.
func (imp *unattendedImport) planned(m provider.Machine) *installPlan {
	return &installPlan{ref: m.Ref(), m: m, hostname: imp.Hostname, groups: imp.Groups, private: imp.Private, before: imp.Before, reserved: imp.Reserved}
}

// addUnattendedFlags adds the flags of an unattended installation.
func addUnattendedFlags(sub *cobra.Command, o *installOptions) {
	sub.Flags().BoolVar(&o.unattended, "unattended", false, "the machine installs by itself at its next boot (a PXE machine's network boot), without UMP's SSH session")
	sub.Flags().BoolVar(&o.noWait, "no-wait", false, "with --unattended: arm the installation and return; the next ump provision records it")
	sub.Flags().DurationVar(&o.armedFor, "armed-for", unattendedArmedFor, "with --no-wait: how long the installation stays armed")
}

// checkUnattendedFlags checks the flags of an unattended installation
// (exit status 2).
func checkUnattendedFlags(cmd *cobra.Command, o installOptions, pattern string, playbooks []string) error {
	switch {
	case !o.unattended && (o.noWait || cmd.Flags().Changed("armed-for")):
		return usageErrorf(cmd, "--no-wait and --armed-for are for --unattended")
	case !o.unattended:
		return nil
	case cmd.Flags().Changed("via"):
		return usageErrorf(cmd, "--unattended takes no --via: the machine boots its installation by itself")
	case o.noReboot:
		return usageErrorf(cmd, "--unattended takes no --no-reboot: the machine reboots into the new system once it reported")
	case cmd.Flags().Changed("armed-for") && !o.noWait:
		return usageErrorf(cmd, "--armed-for is how long the arm of --no-wait stands; without --no-wait it stands as long as ump waits")
	case o.armedFor < time.Minute:
		return usageErrorf(cmd, "--armed-for %s: at least 1m", o.armedFor)
	case o.noWait && (pattern != "" || len(playbooks) > 0):
		return usageErrorf(cmd, "--hosts and --playbook narrow the provisioning after the installation, which --no-wait leaves to the next ump provision")
	}
	return nil
}

// unattendedOf is the server's provider as an UnattendedInstaller, and
// its installation as it stands (nil: none); a provider without them
// has none.
func unattendedOf(ctx context.Context, m provider.Machine, prov provider.Provider) (provider.UnattendedInstaller, *provider.InstallState, error) {
	ui, ok := prov.(provider.UnattendedInstaller)
	if !ok {
		return nil, nil, nil
	}
	st, err := ui.Installation(ctx, m)
	if err != nil {
		return nil, nil, fmt.Errorf("account %s: %w", m.Account, err)
	}
	return ui, st, nil
}

// beingInstalled refuses an installation of a server that a boot claimed
// unattended and installs (exit status 3).
func (a *app) beingInstalled(m provider.Machine, st *provider.InstallState) error {
	if !st.Claimed(a.now()) {
		return nil
	}
	return withExit(exitAborted, fmt.Errorf("%s installs unattended (since %s, by %s): it ends with its report or at %s; ump pxe status --log %s follows it", m.Ref(), a.fmtTime(st.Outcome.At), st.Outcome.By, a.fmtTime(st.Outcome.Deadline), m.ID))
}

// unrecorded says that the installation installed and waits for ump to
// record it from its arm.
func unrecorded(st *provider.InstallState) bool {
	return st != nil && st.Import != nil && st.Outcome != nil && st.Outcome.Phase == provider.InstallInstalled
}

// voids says, for the question, which installation of the server (it:
// how the question names it) the planned one takes the place of: one
// armed, or one that installed and waits to be recorded; "" when none.
func (a *app) voids(st *provider.InstallState, it string) string {
	switch {
	case st == nil:
	case st.Pending:
		until := ""
		if !st.Until.IsZero() {
			until = ", until " + a.fmtTime(st.Until)
		}
		return fmt.Sprintf(" The unattended installation armed for %s (%s, by %s%s) is disarmed.", it, pxe.ShortID(st.ID), st.By, until)
	case unrecorded(st):
		return fmt.Sprintf(" The unattended installation of %s that installed at %s (%s), and that ump has not recorded, is discarded.", it, a.fmtTime(st.Outcome.At), pxe.ShortID(st.ID))
	}
	return ""
}

// armedMeanwhile refuses an installation whose server's unattended
// installation, read again under its lock (now), is not what its
// question said (asked): one that a boot claimed, one armed after the
// question, or one that installed since (exit status 3).
func (a *app) armedMeanwhile(m provider.Machine, asked, now *provider.InstallState) error {
	if err := a.beingInstalled(m, now); err != nil || now == nil {
		return err
	}
	switch {
	case asked == nil || now.ID != asked.ID:
		return withExit(exitAborted, fmt.Errorf("%s got an unattended installation meanwhile (%s, by %s), which the question did not say; nothing was changed, run it again", m.Ref(), pxe.ShortID(now.ID), nonEmpty(now.By, "a PXE server")))
	case unrecorded(now) && !unrecorded(asked):
		return withExit(exitAborted, fmt.Errorf("%s installed unattended meanwhile (%s); nothing was changed: ump provision records it, or run it again", m.Ref(), pxe.ShortID(now.ID)))
	}
	return nil
}

// voidArmed disarms the unattended installation of the planned server
// that the planned one takes the place of, under the server's lock,
// which the caller holds: read again, as its question said (p.armed),
// one armed or one that ended goes with its outcome. The private
// addresses its arm reserved, when it wrote nothing, the plan takes over
// (p.voided, reservePrivate), so that the server gets back those it had
// before either. One that a boot claimed meanwhile, or that the question
// did not say, stops it (exit status 3).
func (a *app) voidArmed(ctx context.Context, p *installPlan) error {
	if p.ui == nil {
		return nil
	}
	m := p.m
	st, err := p.ui.Installation(ctx, m)
	if err != nil {
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	if err := a.armedMeanwhile(m, p.armed, st); err != nil || st == nil {
		return err
	}
	imp, err := parseImport(st.Import)
	if err != nil {
		return fmt.Errorf("%s: %w", m.Ref(), err)
	}
	err = p.ui.Disarm(ctx, m, st.ID)
	switch {
	case errors.Is(err, provider.ErrInstalling):
		return withExit(exitAborted, fmt.Errorf("%s began to install unattended meanwhile, and is not installed again (ump pxe status --log %s follows it)", m.Ref(), m.ID))
	case err != nil:
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	if imp != nil && imp.Reserved && (st.Outcome == nil || st.Outcome.Phase == provider.InstallFailed) {
		p.voided = imp
	}
	switch {
	case st.Pending:
		fmt.Fprintf(a.stderr, "Disarmed the unattended installation of %s (%s, by %s).\n", m.Name, pxe.ShortID(st.ID), st.By)
	case unrecorded(st):
		fmt.Fprintf(a.stderr, "Discarded the unattended installation of %s (%s), which installed unrecorded.\n", m.Name, pxe.ShortID(st.ID))
	}
	return nil
}

// reinstallUnattended is reinstall --unattended once the installation is
// planned and checked: the disks it may erase, one question, the arm
// (under the server's lock, with the private addresses), then the wait
// for the outcome, the check of the new system, the record and the
// provisioning, as reinstall does them. --no-wait returns once armed,
// --dry-run says what it would arm.
func (a *app) reinstallUnattended(cmd *cobra.Command, cfg *config.Config, p *installPlan, o installOptions, tail installTail) error {
	ctx := cmd.Context()
	m := p.m
	var err error
	if p.disks, err = p.ui.InstallDisks(ctx, m, p.spec.Disks); err != nil {
		return withExit(exitUsage, fmt.Errorf("%s: %w", m.Ref(), err))
	}
	if err := a.checkArch(ctx, cmd, m, p.spec.OS, p.storage, p.logins, p.keyPath, o.yes); err != nil {
		return err
	}
	result := p.result(o.dryRun)
	result.Via = "unattended"
	result.Playbooks = tail.run.playbooks
	fmt.Fprintf(a.stderr, "The installation on %s may erase %s.\n", m.Name, p.disks.Words)
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Dry run: %s would install %s unattended as %s at its next boot; nothing is armed.\n", m.Name, p.what(), p.hostname)
		return a.installReport(result, tail.format)
	}
	question := fmt.Sprintf("Erase %s at its next boot and install %s on it unattended as %s?", describeRef(m), p.what(), p.hostname) + a.voids(p.armed, "it")
	switch {
	case o.noWait:
		question += " The next ump provision records it and provisions it."
	case !tail.noProvision:
		question += " Then ump provision runs " + tail.run.String() + "."
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
	unlock, err := a.lockServer(ctx, m, "reinstall")
	if err != nil {
		return err
	}
	info, err := a.armUnattended(ctx, p, a.armedUntil(o), !o.noWait)
	unlock()
	if err != nil {
		return err
	}
	a.saidArmed(m, info)
	if o.noWait {
		result.Armed = info.ID
		fmt.Fprintln(a.stderr, powerCycle([]provider.Machine{m}))
		return a.installReport(result, tail.format)
	}
	oc := a.unattendedOne(ctx, p, info, "reinstall")
	if oc.err != nil {
		return oc.err
	}
	defer oc.unlock()
	return a.finishInstall(ctx, cfg, p, oc.record.record, oc.result, tail)
}

// armedUntil is until when an unattended installation stays armed: as
// long as ump waits for it, or --armed-for with --no-wait.
func (a *app) armedUntil(o installOptions) time.Time {
	if o.noWait {
		return a.now().Add(o.armedFor)
	}
	return a.now().Add(a.unattendedWait() + time.Minute)
}

// unattendedOne waits for the outcome of an armed installation and, when
// it installed, checks the new system under the server's lock (what
// names the operation that holds it), which a completed one keeps
// (installOutcome.unlock) for its record and provisioning; another ump
// may have recorded or replaced it before the lock.
func (a *app) unattendedOne(ctx context.Context, p *installPlan, info *provider.ArmInfo, what string) installOutcome {
	m := p.m
	result := p.result(false)
	result.Via = "unattended"
	oc, err := a.waitUnattended(ctx, p, info, what)
	if err == nil {
		err = a.unattendedFailed(ctx, m, oc, &unattendedImport{Private: p.private, Before: p.before, Reserved: p.reserved}, false)
	}
	if err != nil {
		result.written = oc != nil && oc.Written
		return installOutcome{result: result, err: err}
	}
	result.Rebooted, result.written = true, true
	unlock, err := a.lockServer(ctx, m, what)
	if err != nil {
		return installOutcome{result: result, err: fmt.Errorf("%s is installed, but not recorded: %w; the next ump provision records it", m.Name, err)}
	}
	if st, err := p.ui.Installation(ctx, m); err != nil || st == nil || st.ID != info.ID || !unrecorded(st) {
		unlock()
		if err != nil {
			err = fmt.Errorf("account %s: %w", m.Account, err)
		} else {
			err = fmt.Errorf("the installation of %s (%s) went meanwhile: another ump recorded it, or replaced it (ump server show %s)", m.Name, pxe.ShortID(info.ID), m.Ref())
		}
		return installOutcome{result: result, err: err}
	}
	admin := p.spec.Admin()
	sys, err := a.verifyUnattended(ctx, m, oc, admin, p.keyPath, true)
	if err != nil {
		unlock()
		return installOutcome{result: result, err: err}
	}
	record := installRecord(p.spec.OS, p.profile, p.spec.Layout.Name, p.hostname, admin, sys.Details, a.now())
	result.System, result.Admin, result.InstalledAt = sys, admin, &record.At
	a.trustHostKeys(primaryAddress(m), oc.HostKeys)
	fmt.Fprintf(a.stderr, "%s is up: %s, hostname %s, root on %s; log in as %s (ump ssh %s).\n", m.Name, sys.Name, p.hostname, sys.Root, admin, m.Name)
	return installOutcome{result: result, record: &serverRecord{m, record, p.groups, p.private}, unlock: unlock}
}

// unattendedWait is how long ump waits for an unattended installation:
// installWait, or the configured wait when that is longer.
func (a *app) unattendedWait() time.Duration {
	return max(installWait, a.waitLimit(waitTimeout))
}

// armUnattended arms the planned installation, under the server's lock,
// which the caller holds, as armPlans arms several.
func (a *app) armUnattended(ctx context.Context, p *installPlan, until time.Time, wait bool) (*provider.ArmInfo, error) {
	infos, errs := a.armPlans(ctx, []*installPlan{p}, until, wait)
	return infos[0], errs[0]
}

// armPlans arms the planned installations under their servers' locks,
// which the caller holds: the installation that stood for each is
// disarmed (voidArmed), the private addresses of all are reserved
// together, as they were planned together, and each arm carries the
// specification and what ump needs to record it. One that fails leaves
// the others; a failed arm gives its addresses back.
func (a *app) armPlans(ctx context.Context, plans []*installPlan, until time.Time, wait bool) ([]*provider.ArmInfo, []error) {
	infos, errs := make([]*provider.ArmInfo, len(plans)), make([]error, len(plans))
	var void []*installPlan
	for i, p := range plans {
		if errs[i] = a.voidArmed(ctx, p); errs[i] == nil {
			void = append(void, p)
		}
	}
	if len(void) == 0 {
		return infos, errs
	}
	if err := a.reservePrivate(ctx, void); err != nil {
		for i := range plans {
			if errs[i] == nil {
				errs[i] = err
			}
		}
		return infos, errs
	}
	for i, p := range plans {
		if errs[i] != nil {
			continue
		}
		info, err := p.ui.ArmInstall(ctx, p.m, provider.InstallArm{Spec: p.specJSON, Keys: p.keys, Disks: p.disks, Until: until, Import: importOf(p), Wait: wait})
		if err != nil {
			a.releasePrivate(ctx, []*installPlan{p})
			errs[i] = fmt.Errorf("account %s: %w", p.m.Account, err)
			continue
		}
		infos[i] = info
	}
	return infos, errs
}

// saidArmed says that an installation was armed.
func (a *app) saidArmed(m provider.Machine, info *provider.ArmInfo) {
	until := ""
	if info.Until != nil {
		until = ", until " + a.fmtTime(*info.Until)
	}
	fmt.Fprintf(a.stderr, "Installation armed for %s (%s%s); it installs at its next boot.\n", m.Ref(), pxe.ShortID(info.ID), until)
	if info.Note != "" {
		fmt.Fprintf(a.stderr, "Note: %s\n", info.Note)
	}
}

// waitUnattended waits for the outcome of the armed installation (up to
// unattendedWait): the machine's running system is rebooted over SSH
// when it lets UMP in and a server took the arm, else the operator is
// told to power-cycle it. An installation that no boot claimed in time,
// or whose arm expired, is disarmed and gives its private addresses back
// (dropUnclaimed); one that a boot claimed is left to its end, which the
// next ump provision records. what names the operation for the server's
// lock.
func (a *app) waitUnattended(ctx context.Context, p *installPlan, info *provider.ArmInfo, what string) (*provider.InstallOutcome, error) {
	m := p.m
	if !info.Ready || !a.rebootOverSSH(ctx, m, p.logins, p.keyPath) {
		fmt.Fprintf(a.stderr, "Power-cycle %s now (or reset it): it installs at its next boot.\n", m.Name)
	}
	limit := a.unattendedWait()
	fmt.Fprintf(a.stderr, "Waiting for the installation of %s (up to %s) ...\n", m.Name, fmtWait(limit))
	wctx, cancel := context.WithTimeout(ctx, limit)
	oc, err := p.ui.WaitInstall(wctx, m, info.ID, a.stderr)
	cancel()
	if err == nil {
		return oc, nil
	}
	follow := fmt.Sprintf("ump pxe status --log %s follows it, and the next ump provision records it", m.ID)
	claimed := oc != nil && oc.Phase == provider.InstallWriting
	if oc == nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		derr := a.dropUnclaimed(dctx, p, info.ID, what)
		cancel()
		switch {
		case errors.Is(derr, provider.ErrInstalling):
			claimed = true
		case derr != nil:
			fmt.Fprintf(a.stderr, "ump: the installation of %s could not be disarmed: %v; ump pxe disarm %s does it\n", m.Name, derr, m.ID)
		}
	}
	switch {
	case ctx.Err() != nil && claimed:
		fmt.Fprintf(a.stderr, "The installation of %s goes on without ump: %s.\n", m.Name, follow)
		return nil, ctx.Err()
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case claimed:
		return nil, withExit(exitUnreachable, fmt.Errorf("the installation of %s did not end within %s; it goes on without ump: %s", m.Name, fmtWait(limit), follow))
	case errors.Is(err, context.DeadlineExceeded):
		return nil, withExit(exitUnreachable, fmt.Errorf("no boot of %s claimed its installation within %s; it is disarmed (ump pxe status %s shows what the PXE servers saw)", m.Name, fmtWait(limit), m.ID))
	}
	return nil, fmt.Errorf("account %s: %w", m.Account, err)
}

// dropUnclaimed disarms the installation of the plan's arm id, which no
// boot claimed, and gives the private addresses the arm reserved back,
// under the server's lock (what names the operation): whoever removes
// an arm gives its addresses back, so nothing is done when another ump
// removed or replaced it meanwhile. provider.ErrInstalling when a boot
// claimed it after all.
func (a *app) dropUnclaimed(ctx context.Context, p *installPlan, id, what string) error {
	unlock, err := a.lockServer(ctx, p.m, what)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := p.ui.Installation(ctx, p.m)
	switch {
	case err != nil:
		return err
	case st == nil || st.ID != id || st.Import == nil:
		return nil
	case st.Outcome != nil:
		return provider.ErrInstalling
	}
	if err := p.ui.Disarm(ctx, p.m, id); err != nil {
		return err
	}
	a.releasePrivate(ctx, []*installPlan{p})
	return nil
}

// releaseUnattended gives the private addresses of an installation that
// wrote nothing back, under the server's lock, which it takes unless
// the caller holds it (locked).
func (a *app) releaseUnattended(ctx context.Context, p *installPlan, locked bool) {
	if !p.reserved {
		return
	}
	if locked {
		a.releasePrivate(ctx, []*installPlan{p})
		return
	}
	unlock, err := a.lockServer(context.WithoutCancel(ctx), p.m, "reinstall")
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the private addresses reserved for the installation of %s stay recorded: %v\n", p.m.Name, err)
		return
	}
	defer unlock()
	a.releasePrivate(ctx, []*installPlan{p})
}

// unattendedFailed says why an installation that did not install
// failed, giving its private addresses back when it did not write the
// new system, as reinstall gives them back after a failed installer
// (imp: what its arm reserved; locked: the caller holds the server's
// lock); nil for an installed one.
func (a *app) unattendedFailed(ctx context.Context, m provider.Machine, oc *provider.InstallOutcome, imp *unattendedImport, locked bool) error {
	if oc.Phase == provider.InstallInstalled {
		return nil
	}
	if !oc.Written && oc.Phase == provider.InstallFailed && imp != nil {
		a.releaseUnattended(ctx, imp.planned(m), locked)
	}
	return a.outcomeError(m, oc)
}

// outcomeError is an installation that failed or did not report, in
// words: nothing is recorded.
func (a *app) outcomeError(m provider.Machine, oc *provider.InstallOutcome) error {
	var parts []string
	if oc.Exit != nil {
		parts = append(parts, fmt.Sprintf("exit status %d", *oc.Exit))
	}
	if oc.Reason != "" {
		parts = append(parts, oc.Reason)
	}
	why := ""
	if len(parts) > 0 {
		why = " (" + strings.Join(parts, ": ") + ")"
	}
	switch {
	case oc.Phase == provider.InstallInterrupted:
		return fmt.Errorf("the installation of %s did not report by %s; the machine boots its disk, which it may have written. Nothing is recorded (ump pxe status --log %s has its log)", m.Name, a.fmtTime(oc.Deadline), m.ID)
	case oc.Exit != nil && *oc.Exit == 2:
		// the installer's and the disk guard's refusals
		return fmt.Errorf("the installation of %s was refused%s; nothing was written, nothing is recorded, and the machine stays in UMP's rescue system (ump pxe status --log %s has its log)", m.Name, why, m.ID)
	}
	return fmt.Errorf("the installation of %s failed%s; nothing is recorded, and the machine stays in UMP's rescue system (ump pxe status --log %s has its log)", m.Name, why, m.ID)
}

// verifyUnattended checks the new system of an unattended installation
// before ump records it: SSH at the server's own address (through its
// jump host) answers as the installation's administrator with UMP's
// key, with the host keys the installation reported pinned; one that
// reported none is not checked, nor recorded. With wait, it tries until
// the configured wait ends, as after a reboot.
func (a *app) verifyUnattended(ctx context.Context, m provider.Machine, oc *provider.InstallOutcome, admin, keyPath string, wait bool) (*installedSystem, error) {
	addr := primaryAddress(m)
	if addr == "" {
		return nil, noAddress(m)
	}
	keys := reportedHostKeys(oc.HostKeys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("the installation of %s reported no host keys of its new system, which ump checks it with; nothing is recorded (ump server reinstall %s installs it again)", m.Name, m.Ref())
	}
	file, err := a.pinHostKeys(addr, keys)
	if err != nil {
		return nil, err
	}
	r := a.route(addr)
	r.knownHosts = file
	a.setRoute(addr, r)
	fail := withExit(exitUnreachable, fmt.Errorf("the new system of %s does not answer at %s as %s with UMP's key and the host keys its installation reported; nothing is recorded (the next ump provision tries again)", m.Name, addr, admin))
	if !wait {
		if sys, ok := a.installedSystem(ctx, addr, keyPath, admin); ok {
			return sys, nil
		}
		return nil, fail
	}
	limit := a.waitLimit(waitTimeout)
	start := time.Now()
	fmt.Fprintf(a.stderr, "Waiting for the new system of %s at %s (up to %s) ...", m.Name, addr, fmtWait(limit))
	tick := a.waitTicker(start)
	for {
		if a.answers(ctx, addr) {
			if sys, ok := a.installedSystem(ctx, addr, keyPath, admin); ok {
				fmt.Fprintf(a.stderr, " up after %s.\n", time.Since(start).Round(time.Second))
				return sys, nil
			}
		}
		if time.Since(start) >= limit {
			fmt.Fprintln(a.stderr)
			return nil, fail
		}
		if err := sleepCtx(ctx, waitPoll); err != nil {
			fmt.Fprintln(a.stderr)
			return nil, err
		}
		tick()
	}
}

// hostKeyType and hostKeyData are what a host key's type and its key
// look like.
var (
	hostKeyType = regexp.MustCompile(`^(ssh|ecdsa|sk)-[a-z0-9@.-]+$`)
	hostKeyData = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
)

// reportedHostKeys are the host keys an installation reported that look
// like host keys, each as its type and key.
func reportedHostKeys(keys []string) []string {
	var out []string
	for _, k := range keys {
		if f := strings.Fields(k); len(f) >= 2 && hostKeyType.MatchString(f[0]) && hostKeyData.MatchString(f[1]) {
			out = append(out, f[0]+" "+f[1])
		}
	}
	return out
}

// trustHostKeys puts the host keys an unattended installation reported
// into ~/.ssh/known_hosts for the address, in place of the old system's:
// the provisioning that follows (StrictHostKeyChecking=accept-new) finds
// them there, and takes no other key that answers at the address.
func (a *app) trustHostKeys(addr string, keys []string) {
	knownHosts.Lock()
	defer knownHosts.Unlock()
	if _, err := a.forgetOldHostKey(addr); err != nil {
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
	}
	if err := addKnownHosts(addr, reportedHostKeys(keys)); err != nil {
		fmt.Fprintf(a.stderr, "ump: the host keys of the new system at %s are not in ~/.ssh/known_hosts: %v\n", addr, err)
		return
	}
	fmt.Fprintf(a.stderr, "The host keys of the new system at %s are in ~/.ssh/known_hosts now, in place of the old system's.\n", addr)
}

// addKnownHosts adds the host keys (type and key) of the address to
// ~/.ssh/known_hosts.
func addKnownHosts(addr string, keys []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "known_hosts")
	var b strings.Builder
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteString("\n")
	}
	for _, k := range keys {
		b.WriteString(addr + " " + k + "\n")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// What ump provision does with an unattended installation (settling).
const (
	settleNothing = iota
	// it installed: checked and recorded
	settleRecord
	// it failed, and the private addresses its arm reserved are still
	// the server's: given back
	settleRelease
	// its arm expired before a boot claimed it: disarmed, and the
	// addresses given back
	settleExpired
	// it failed or did not report, or installed and lost its arm: said,
	// with how to clear it
	settleSay
)

// settling is what ump provision does with the unattended installation
// of a server (s, as the state has it).
func settling(st *provider.InstallState, s state.Server) int {
	switch {
	case st == nil || st.Pending || st.Outcome != nil && st.Outcome.Phase == provider.InstallWriting:
		return settleNothing
	case st.Outcome == nil && st.Import != nil:
		return settleExpired
	case st.Outcome == nil:
		return settleNothing
	case st.Import == nil:
		return settleSay
	case st.Outcome.Phase == provider.InstallInstalled:
		return settleRecord
	case st.Outcome.Phase == provider.InstallFailed:
		if imp, err := parseImport(st.Import); err == nil && imp.Reserved && slices.Equal(s.Private, imp.Private) {
			return settleRelease
		}
	}
	return settleSay
}

// unsettled says, of an installation that ump cannot settle, what came
// of it and how to clear it.
func (a *app) unsettled(name string, st *provider.InstallState) {
	ref, id := pxe.Ref(name), pxe.ShortID(st.ID)
	switch {
	case st.Outcome.Phase == provider.InstallInstalled:
		fmt.Fprintf(a.stderr, "ump: the unattended installation of %s (%s) installed, but its arm is gone with what ump records of it: ump server reinstall %s installs it again, ump pxe disarm %s forgets it\n", ref, id, ref, name)
	case st.Outcome.Phase == provider.InstallInterrupted:
		fmt.Fprintf(a.stderr, "ump: the unattended installation of %s (%s) did not report by %s, and is not recorded: ump pxe status --log %s has its log, ump pxe disarm %s clears it\n", ref, id, a.fmtTime(st.Outcome.Deadline), name, name)
	default:
		fmt.Fprintf(a.stderr, "ump: the unattended installation of %s (%s) failed at %s: ump pxe status --log %s has its log, ump pxe disarm %s clears it\n", ref, id, a.fmtTime(st.Outcome.At), name, name)
	}
}

// settleInstalls settles the unattended installations whose outcome
// waits, before ump provision runs, each under its server's lock and as
// it stands then: one that installed and that ump reaches as its
// administrator at its own address, with the host keys it reported
// pinned, is recorded (and its arm goes with it); one that failed gives
// the private addresses its arm reserved back, once; one whose arm
// expired before a boot claimed it is disarmed and gives them back. A
// server whose lock another ump holds is left to it. It says what it
// did, what not, and which installations failed or did not report;
// nothing of it stops the run.
func (a *app) settleInstalls(cmd *cobra.Command, cfg *config.Config) {
	ctx := cmd.Context()
	doc, err := a.loadState(ctx, cfg)
	if err != nil || a.pxeConf == nil || len(a.pxeConf.Names()) == 0 {
		return
	}
	sn, err := a.pxeStore().Load(ctx, a.pxeConf.SiteNames()...)
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the outcomes of unattended installations could not be read: %v\n", err)
		return
	}
	waiting := false
	for _, pm := range a.pxeConf.Merged() {
		st := pxeprovider.InstallationOf(sn.Arm(pm.Site, pm.Name), sn.Outcome(pm.Site, pm.Name), a.now())
		switch settling(st, doc.Servers[pxe.Ref(pm.Name)]) {
		case settleNothing:
		case settleSay:
			a.unsettled(pm.Name, st)
		default:
			waiting = true
		}
	}
	if !waiting {
		return
	}
	machines, byAccount, _, _, err := a.listedMachines(cmd)
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the unattended installations waiting cannot be settled: %v\n", err)
		return
	}
	_, keyPath, err := a.stagingKey(cfg)
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
		return
	}
	var records []serverRecord
	var unlocks []func()
	defer func() {
		for _, u := range unlocks {
			u()
		}
	}()
	for _, m := range machines {
		r, ok := byAccount[m.Account]
		if m.Provider != pxe.TypeName || !ok {
			continue
		}
		ui, st, err := unattendedOf(ctx, m, r.prov)
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
			continue
		}
		if k := settling(st, doc.Servers[m.Ref()]); k == settleNothing || k == settleSay {
			continue
		}
		if err := a.reach(m); err != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
			continue
		}
		unlock, err := a.lockServer(ctx, m, "provision")
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: the unattended installation of %s is left to the ump that holds its lock: %v\n", m.Ref(), err)
			continue
		}
		unlocks = append(unlocks, unlock)
		// as it stands under the lock: another ump may have settled or
		// replaced it since
		if st, err = ui.Installation(ctx, m); err != nil || st == nil {
			if err != nil {
				fmt.Fprintf(a.stderr, "ump: account %s: %v\n", m.Account, err)
			}
			continue
		}
		imp, err := parseImport(st.Import)
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: %s: %v\n", m.Ref(), err)
			continue
		}
		cur, _, err := a.stateStore().Read(ctx)
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
			continue
		}
		switch settling(st, cur.Servers[m.Ref()]) {
		case settleExpired:
			err := ui.Disarm(ctx, m, st.ID)
			switch {
			case errors.Is(err, provider.ErrInstalling):
				continue
			case err != nil:
				fmt.Fprintf(a.stderr, "ump: account %s: %v\n", m.Account, err)
				continue
			}
			a.releasePrivate(ctx, []*installPlan{imp.planned(m)})
			fmt.Fprintf(a.stderr, "The unattended installation of %s (%s) expired before a boot claimed it: disarmed, and its private addresses given back.\n", m.Name, pxe.ShortID(st.ID))
		case settleRelease:
			fmt.Fprintf(a.stderr, "ump: %v\n", a.unattendedFailed(ctx, m, st.Outcome, imp, true))
		case settleRecord:
			sys, err := a.verifyUnattended(ctx, m, st.Outcome, imp.Admin, keyPath, false)
			if err != nil {
				fmt.Fprintf(a.stderr, "ump: %v\n", err)
				continue
			}
			record := installRecord(imp.OS, imp.Profile, imp.Layout, imp.Hostname, imp.Admin, sys.Details, a.now())
			a.trustHostKeys(primaryAddress(m), st.Outcome.HostKeys)
			records = append(records, serverRecord{m: m, record: record, groups: imp.Groups, private: imp.Private})
			fmt.Fprintf(a.stderr, "%s installed unattended: %s, hostname %s, root on %s; recorded.\n", m.Name, sys.Name, imp.Hostname, sys.Root)
		}
	}
	if len(records) > 0 {
		a.recordInstallations(context.WithoutCancel(ctx), records)
	}
}

// voidPXE removes what the sites hold of the arms of PXE machines whose
// installation ump just recorded, the arm first, then its outcome: no
// armed boot erases the system ump recorded, and the outcome is
// settled. An installation that a boot claimed and writes keeps its
// outcome until its report or its deadline; the logs stay.
func (a *app) voidPXE(ctx context.Context, records []serverRecord) {
	store := a.pxeStore()
	for _, r := range records {
		if r.m.Provider != pxe.TypeName {
			continue
		}
		name := r.m.ID
		sites, err := store.Holding(ctx, name)
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: the arms of %s: %v\n", r.m.Ref(), err)
			continue
		}
		for _, site := range sites {
			if err := voidSite(ctx, store, site, name, a.now()); err != nil {
				fmt.Fprintf(a.stderr, "ump: the arm of %s at site %s: %v (ump pxe disarm %s)\n", r.m.Ref(), site, err, name)
			}
		}
	}
}

// voidSite removes a machine's arm at a site, then its outcome but one
// that is being written.
func voidSite(ctx context.Context, store pxe.Store, site, name string, now time.Time) error {
	if err := store.Disarm(ctx, site, name); err != nil {
		return err
	}
	o, _, err := store.Outcome(ctx, site, name)
	if err != nil || o == nil || o.Phase == pxe.PhaseWriting && now.Before(o.Deadline) {
		return err
	}
	return store.Files.Delete(ctx, pxe.OutcomePath(site, name))
}

// armAll arms the planned installations of multiinstall --unattended
// (armPlans), under the servers' locks, which the caller holds, and says
// what it armed and what not.
func (a *app) armAll(ctx context.Context, plans []*installPlan, until time.Time, wait bool) ([]*provider.ArmInfo, []error) {
	infos, errs := a.armPlans(ctx, plans, until, wait)
	for i, p := range plans {
		if errs[i] != nil {
			fmt.Fprintf(a.stderr, "ump: %s: %v\n", p.ref, errs[i])
			continue
		}
		a.saidArmed(p.m, infos[i])
	}
	return infos, errs
}

// unattendedAll waits for the armed installations side by side, each as
// a copy of the app whose stderr puts [NAME] in front of every line (as
// installAll runs installations), and checks the new systems; a
// completed one keeps its server's lock (installOutcome.unlock).
func (a *app) unattendedAll(cmd *cobra.Command, plans []*installPlan, infos []*provider.ArmInfo, errs []error) []installOutcome {
	ctx := cmd.Context()
	// what the copies share: the state's store, a bad wait said once
	a.stateStore()
	a.waitLimit(waitTimeout)
	outcomes := make([]installOutcome, len(plans))
	var lines sync.Mutex
	var wg sync.WaitGroup
	for i, p := range plans {
		if errs[i] != nil {
			outcomes[i] = installOutcome{result: p.result(false), err: errs[i]}
			continue
		}
		wg.Go(func() {
			w := &linePrefix{mu: &lines, w: a.stderr, prefix: "[" + p.ref + "] "}
			sub := *a
			sub.stderr, sub.stdout, sub.isTTY = w, w, nil
			outcomes[i] = sub.unattendedOne(ctx, p, infos[i], "multiinstall")
			switch err := outcomes[i].err; {
			case errors.Is(err, context.Canceled):
				fmt.Fprintln(w, "ump: interrupted")
			case err != nil:
				fmt.Fprintf(w, "ump: %v\n", err)
			}
			w.Flush()
		})
	}
	wg.Wait()
	return outcomes
}

// multiArmed reports what multiinstall --unattended --no-wait armed:
// exit status 1 when an arm failed.
func (a *app) multiArmed(plans []*installPlan, infos []*provider.ArmInfo, errs []error, format outputFormat) error {
	res := multiResult{Servers: make([]multiServer, len(plans))}
	var failed []string
	var armed []provider.Machine
	for i, p := range plans {
		s := multiServer{installResult: p.result(false), ref: p.ref}
		s.Via = "unattended"
		if errs[i] != nil {
			s.Reason = errs[i].Error()
			failed = append(failed, p.ref)
		} else {
			s.Armed = infos[i].ID
			armed = append(armed, p.m)
		}
		res.Servers[i] = s
	}
	if format != "" {
		if err := printEncoded(a.stdout, res, format); err != nil {
			return err
		}
	} else if err := a.multiSummary(res, false, false); err != nil {
		return err
	}
	if len(armed) > 0 {
		fmt.Fprintln(a.stderr, powerCycle(armed))
	}
	if len(failed) > 0 {
		return withExit(exitGeneric, fmt.Errorf("the installation of %s is not armed, see above", strings.Join(failed, ", ")))
	}
	return nil
}

// multiDryUnattended is multiinstall --unattended --dry-run: what each
// server would install, and nothing armed.
func (a *app) multiDryUnattended(plans []*installPlan, format outputFormat) error {
	res := multiResult{Servers: make([]multiServer, len(plans))}
	for i, p := range plans {
		s := multiServer{installResult: p.result(true), ref: p.ref}
		s.Via = "unattended"
		res.Servers[i] = s
	}
	fmt.Fprintf(a.stderr, "Dry run: these %s would install unattended at their next boot; nothing is armed.\n", plural(len(plans), "server"))
	if format != "" {
		return printEncoded(a.stdout, res, format)
	}
	return a.multiSummary(res, true, false)
}

// powerCycle says what to do about installations armed and not waited
// for.
func powerCycle(ms []provider.Machine) string {
	if len(ms) == 1 {
		return fmt.Sprintf("Power-cycle %s (or reset it) for it to install; ump pxe status --log %s follows it, and the next ump provision records it and provisions it.", ms[0].Name, ms[0].ID)
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return fmt.Sprintf("Power-cycle %s (or reset them) for them to install; ump pxe status --log NAME follows one, and the next ump provision records them and provisions them.", strings.Join(names, ", "))
}
