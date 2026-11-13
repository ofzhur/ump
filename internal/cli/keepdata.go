package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
)

// ump server reinstall --keep-data: the old system is replaced and its
// data stays. The installer does it (install/README.md, Keeping the
// data) in two runs, both in the rescue system: one that finds the old
// system, reading only, and makes a plan with a hash, and one that
// installs when the plan it makes then has the hash that was confirmed.
// ump asks between them. Nothing is written to a disk before the second
// run says so.

// keepLayout is the layout a keep-data installation is recorded and
// shown with: none is applied.
const keepLayout = "keep-data"

// keepPlanFile is where the installer writes its plan in the rescue
// system, as JSON; keepWriting is the line it prints before its first
// write.
const (
	keepPlanFile = "/tmp/ump-keep-data.json"
	keepWriting  = "==> keep-data: writing begins"
)

// keepFirst is what the first question of a keep-data installation says
// after what is installed where.
const keepFirst = "Its data is kept: no disk is wiped. The running system goes down and the server boots the rescue system, where the installer finds the old system and plans what it formats and what it keeps; nothing is written to a disk until you confirm that plan."

// keepData is a keep-data installation: its options, and once the plan
// run is done the server in the rescue system and the installer's plan.
type keepData struct {
	// root is --root, keepRoot --keep-root, format the mountpoints of
	// --format
	root     string
	keepRoot bool
	format   []string
	entry    *installEntry
	plan     *keepPlan
}

// args are the installer's arguments of the installation, the same for
// its two runs.
func (k *keepData) args() []string {
	args := []string{"--keep-data"}
	if k.root != "" {
		args = append(args, "--root", k.root)
	}
	if k.keepRoot {
		args = append(args, "--keep-root")
	}
	for _, f := range k.format {
		args = append(args, "--format", f)
	}
	return args
}

// keepPlan is the installer's plan as ump reads it: a refusal, or the
// old root, how it was found, and the hash; doc is the whole of it, as
// the result of the command carries it.
type keepPlan struct {
	Refused string `json:"refused"`
	Hash    string `json:"hash"`
	Root    struct {
		Device    string `json:"device"`
		Subvolume string `json:"subvolume"`
		By        string `json:"by"`
	} `json:"root"`
	Candidates []struct {
		Device    string `json:"device"`
		Subvolume string `json:"subvolume"`
		System    string `json:"system"`
		Hostname  string `json:"hostname"`
	} `json:"candidates"`
	doc any
}

// root is the old root as --root names one: the device, with :SUBVOLUME
// on btrfs.
func (k *keepPlan) root() string {
	if k.Root.Subvolume != "" {
		return k.Root.Device + ":" + k.Root.Subvolume
	}
	return k.Root.Device
}

// candidates are the old systems the installer found, each as --root
// names it, with its system and hostname.
func (k *keepPlan) candidates() []string {
	var out []string
	for _, c := range k.Candidates {
		name := c.Device
		if c.Subvolume != "" {
			name += ":" + c.Subvolume
		}
		if what := strings.Join(slices.DeleteFunc([]string{c.System, c.Hostname}, func(s string) bool { return s == "" }), ", "); what != "" {
			name += " (" + what + ")"
		}
		out = append(out, name)
	}
	return out
}

// parseKeepPlan reads the plan file of the installer.
func parseKeepPlan(data []byte) (*keepPlan, error) {
	k := &keepPlan{}
	if err := json.Unmarshal(data, k); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &k.doc); err != nil {
		return nil, err
	}
	return k, nil
}

// checkKeepFlags checks --keep-data and its flags by themselves (exit
// status 2): they go together, and with no layout, no choice of disks
// and no unattended installation.
func checkKeepFlags(cmd *cobra.Command, o installOptions) (*keepData, error) {
	if !o.keepData {
		if o.root != "" || o.keepRoot || len(o.format) > 0 {
			return nil, usageErrorf(cmd, "--root, --keep-root and --format belong to --keep-data")
		}
		return nil, nil
	}
	switch {
	case o.layout != "":
		return nil, usageErrorf(cmd, "--keep-data and --layout %s: a keep-data installation applies no layout, it takes the volumes as the old system has them", o.layout)
	case o.disks != "":
		return nil, usageErrorf(cmd, "--keep-data and --disks %s: a keep-data installation chooses no disks, it finds the old system and wipes none", o.disks)
	case o.unattended:
		return nil, usageErrorf(cmd, "--keep-data and --unattended: a keep-data installation shows its plan and asks before it writes, which a machine that installs by itself cannot")
	}
	k := &keepData{root: strings.TrimSpace(o.root), keepRoot: o.keepRoot}
	if o.root != "" && (k.root == "" || strings.ContainsAny(k.root, " \t\r\n")) {
		return nil, usageErrorf(cmd, "--root %q: the old root is a device (/dev/vg0/root), UUID=..., or DEVICE:SUBVOLUME on btrfs", o.root)
	}
	for _, f := range o.format {
		mp := strings.TrimSpace(f)
		if len(mp) > 1 {
			mp = strings.TrimRight(mp, "/")
		}
		switch {
		case !strings.HasPrefix(mp, "/") || strings.ContainsAny(mp, " \t\r\n\\"):
			return nil, usageErrorf(cmd, "--format %q: a mountpoint of the old system's fstab (/var, /srv/cache)", f)
		case mp == "/" && o.keepRoot:
			return nil, usageErrorf(cmd, "--format / and --keep-root contradict each other")
		case slices.Contains(k.format, mp):
			return nil, usageErrorf(cmd, "--format %s is given twice", mp)
		}
		k.format = append(k.format, mp)
	}
	return k, nil
}

// checkKeepOS refuses a keep-data installation of a system the installer
// does not keep data for (exit status 2), before the server is touched:
// NixOS, Arch Linux and CachyOS by their names, and whatever the
// storage's manifest says is of another family than the unpacked images
// (Ubuntu, Debian). A storage that cannot be read passes: the installer
// checks again, before it writes.
func (a *app) checkKeepOS(ctx context.Context, m provider.Machine, os, storage string) error {
	family := profile.Family(os)
	if strings.HasPrefix(os, "archlinux") || strings.HasPrefix(os, "cachyos") {
		family = "arch"
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if family != "image" {
			break
		}
		data, err := a.fetch(ctx, storage+"/os/"+os+"/"+arch+"/manifest.json")
		if err != nil {
			continue
		}
		var mf struct {
			Family string `json:"family"`
		}
		if json.Unmarshal(data, &mf) == nil && mf.Family != "" {
			family = mf.Family
		}
	}
	if family == "image" {
		return nil
	}
	return withExit(exitUsage, fmt.Errorf("--keep-data installs the images unpacked onto the old system's volumes (Ubuntu, Debian); %s is of the family %s, whose installation makes its own file systems, and is not supported with it yet. %s was not touched", os, family, m.Name))
}

// keepNoWay is the refusal of a keep-data installation on a server ump
// could reach only through its provider's stock installation (why says
// what stands in the other ways): that erases the disks.
func keepNoWay(addr, why string) error {
	return fmt.Errorf("no root access to %s over SSH, and %s: the only other way in is the provider's reinstall with a stock Linux, which erases the disks that --keep-data keeps. ump keeps the data of such a server through its running system alone: let UMP in there as root or a sudo user (with UMP's key, ump config key show, or one of yours) and run this again. Nothing was changed", addr, why)
}

// planKeep makes the plan of a keep-data installation: it brings the
// server into the UMP rescue system and runs the installer there in its
// plan mode, which finds the old system reading only and writes nothing;
// the plan is printed as the installer prints it, and read back as JSON
// (p.keep.plan). A machine the installer refuses, and whatever else
// keeps it from planning, is an error that says that nothing was
// written and how the old system starts again (exit status 3 for a
// refusal).
func (a *app) planKeep(cmd *cobra.Command, p *installPlan, r installRun) (installResult, error) {
	ctx := cmd.Context()
	m := p.m
	result := p.result(r.dryRun)
	// never through a provider's stock installation, which erases the
	// machine
	e, err := a.enterInstall(cmd, p, false)
	if err != nil {
		return result, err
	}
	p.keep.entry = e
	result.Via = e.via
	fmt.Fprintf(a.stderr, "Keep-data plan for %s on %s (the installer looks, and writes nothing):\n", p.what(), m.Name)
	code, out, err := a.runInstaller(ctx, p, e, append(p.keep.args(), "--plan", keepPlanFile), "rm -f "+keepPlanFile+"; ")
	if err != nil {
		return result, err
	}
	ran := installerRan(out)
	switch {
	case code == 255:
		return result, withExit(exitUnreachable, fmt.Errorf("the connection to %s ended while the installer planned, which writes nothing; the server is in the UMP rescue system, look there (%s)", e.addr, e.staging))
	case code != 0 && !ran && code != 1 && code != 2:
		return result, a.noInstaller(p, e, code)
	}
	// the plan as JSON; a machine the installer refuses has one too, with
	// the old systems it found
	plan, perr := a.readKeepPlan(ctx, p, e)
	switch {
	case code == 2:
		names := ""
		if plan != nil && len(plan.Candidates) > 1 && p.keep.root == "" {
			names = fmt.Sprintf(" --root names the old system to replace: %s.", strings.Join(plan.candidates(), ", "))
		}
		return result, withExit(exitAborted, fmt.Errorf("the installer refused to keep the data of %s (exit status 2, see above).%s %s", m.Name, names, a.keepUnwritten(ctx, p)))
	case code != 0:
		return result, fmt.Errorf("the installer's plan for %s failed (exit status %d, see above). %s", m.Name, code, a.keepUnwritten(ctx, p))
	case perr != nil:
		return result, fmt.Errorf("the installer's plan for %s could not be read back from the rescue system (%s): %w. %s", m.Name, keepPlanFile, perr, a.keepUnwritten(ctx, p))
	case plan.Refused != "" || len(plan.Hash) != 64:
		return result, fmt.Errorf("the installer's plan for %s (%s) has no hash to confirm. %s", m.Name, keepPlanFile, a.keepUnwritten(ctx, p))
	}
	p.keep.plan = plan
	result.KeepData = plan.doc
	if r.dryRun {
		fmt.Fprintf(a.stderr, "Dry run done; nothing was written to the disks of %s, which stays in the UMP rescue system (%s). %s.\n", m.Name, e.staging, a.oldSystemBack(ctx, p))
	}
	return result, nil
}

// readKeepPlan reads the plan file the installer left in the rescue
// system.
func (a *app) readKeepPlan(ctx context.Context, p *installPlan, e *installEntry) (*keepPlan, error) {
	argv := withSSHOptions(a.sshTo(e.user, e.addr, []string{p.keyPath}, true, true, []string{"cat " + keepPlanFile}), batchOptions(10*time.Second)...)
	var out, errb bytes.Buffer
	code, err := a.sshExec(ctx, argv, nil, &out, &errb)
	switch {
	case err != nil:
		return nil, err
	case code != 0:
		return nil, errors.New(nonEmpty(strings.TrimSpace(errb.String()), fmt.Sprintf("exit status %d", code)))
	}
	return parseKeepPlan(out.Bytes())
}

// keepUnwritten says, as the end of an error, that a keep-data
// installation that stopped wrote nothing, where its server is and how
// its old system starts again.
func (a *app) keepUnwritten(ctx context.Context, p *installPlan) string {
	return fmt.Sprintf("Nothing was written to the disks of %s, which stays in the UMP rescue system (%s); %s", p.m.Name, p.keep.entry.staging, lowerFirst(a.oldSystemBack(ctx, p)))
}

// oldSystemBack says how the old system of a server in the rescue system
// starts again while nothing was written: a reboot from disk (a reset:
// the rescue system runs from memory and has no disk mounted), once the
// provider's rescue system is disabled where it would boot again.
func (a *app) oldSystemBack(ctx context.Context, p *installPlan) string {
	if rs, ok := p.prov.(provider.Rescue); ok {
		if on, err := rs.RescueEnabled(ctx, p.m); err == nil && on {
			return "ump server unrescue " + p.ref + " --reboot starts the old system again"
		}
	}
	if _, ok := p.prov.(provider.Power); ok {
		return "ump server reboot " + p.ref + " --hard starts the old system again"
	}
	return "A reboot starts the old system again (ump ssh " + p.ref + " --staging -- systemctl reboot)"
}

// lowerFirst is s with its first letter in lower case.
func lowerFirst(s string) string {
	if s == "" || strings.HasPrefix(s, "ump ") {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// keepAccepted says whether --yes accepts the plan of a keep-data
// installation: the installer found the one old system by itself, or
// the operator named it with --root.
func (p *installPlan) keepAccepted() error {
	if plan := p.keep.plan; plan.Root.By != "detection" && p.keep.root == "" {
		return withExit(exitAborted, fmt.Errorf("--yes accepts a keep-data plan whose old system the installer found by itself, or that --root names; the plan for %s has its root on %s by %s", p.m.Name, plan.root(), nonEmpty(plan.Root.By, "no word of the installer")))
	}
	return nil
}

// confirmKeep is the second question of keep-data installations, about
// the plans the installer made for the servers of plans (shown above
// it): yes skips it for the plans it accepts (keepAccepted). An answer
// that is no, like a plan --yes does not accept, leaves the disks as
// they are, which is said with how the old systems start again.
func (a *app) confirmKeep(cmd *cobra.Command, yes bool, plans []*installPlan) error {
	ctx := cmd.Context()
	var err error
	if yes {
		for _, p := range plans {
			if err = p.keepAccepted(); err != nil {
				break
			}
		}
	} else {
		question := fmt.Sprintf("Install %s on %s as the plan above says (%s)? What it lists under FORMAT and LOST is lost.", plans[0].spec.OS, plans[0].m.Name, shortHash(plans[0].keep.plan.Hash))
		if len(plans) > 1 {
			names := make([]string, len(plans))
			for i, p := range plans {
				names[i] = p.ref
			}
			question = fmt.Sprintf("Install on %s as their plans above say? What each lists under FORMAT and LOST is lost.", strings.Join(names, ", "))
		}
		err = a.confirm(cmd, false, question)
	}
	if err == nil {
		return nil
	}
	for _, p := range plans {
		fmt.Fprintf(a.stderr, "Nothing was written to the disks of %s, which stays in the UMP rescue system (%s). %s.\n", p.m.Name, p.keep.entry.staging, a.oldSystemBack(ctx, p))
	}
	return err
}

// shortHash is the start of a plan's hash, to tell plans apart by.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return "plan " + hash[:12]
	}
	return "plan " + hash
}

// installKept runs the installer on a server whose keep-data plan was
// confirmed, with the plan's hash: the installer finds everything again
// and writes only when its plan is still that one. Its exit status says
// whether it wrote: 2 is every end before the first write, 1 a failure
// after it, when the old root is gone or moved aside and the data
// volumes are as they were; a second run then plans from what the first
// saved of the old system. result says whether the disks were written.
func (a *app) installKept(ctx context.Context, p *installPlan, r installRun, e *installEntry, result *installResult) error {
	m := p.m
	fmt.Fprintf(a.stderr, "Installing %s on %s as %s, as planned (%s) ...\n", p.what(), m.Name, p.hostname, shortHash(p.keep.plan.Hash))
	if r.after != "" {
		fmt.Fprintf(a.stderr, "Provisioning after the first boot: %s.\n", r.after)
	}
	code, out, err := a.runInstaller(ctx, p, e, append(p.keep.args(), "--confirm", p.keep.plan.Hash), "")
	if err != nil {
		return err
	}
	ran, began := installerRan(out), strings.Contains(out, keepWriting)
	result.written = code == 0 || began || code != 2 && ran
	again := "run the same command again: the installer finds what it saved of the old system's fstab on the new root, plans anew, and ump asks again"
	switch {
	case code == 0:
		return nil
	case code == 255 && began:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation, after the installer began to write: the old root is gone or moved aside, the data volumes are untouched. The server is in the UMP rescue system, look there (%s); when the installer is not running any more, %s", e.addr, e.staging, again))
	case code == 255:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation, before the installer said that it writes; the server is in the UMP rescue system, look there (%s)", e.addr, e.staging))
	case !ran && code != 1 && code != 2:
		return a.noInstaller(p, e, code)
	case code == 2:
		return withExit(exitAborted, fmt.Errorf("the installer stopped before it wrote anything (exit status 2, see above). %s", a.keepUnwritten(ctx, p)))
	}
	return fmt.Errorf("the installation on %s failed after the installer began to write (exit status %d, see above): the old root is gone or moved aside, the data volumes are untouched. The server stays in the UMP rescue system (%s); %s", m.Name, code, e.staging, again)
}

// keepNames are the servers of the plans that keep their data, as named.
func keepNames(plans []*installPlan) []string {
	var names []string
	for _, p := range plans {
		if p.keep != nil {
			names = append(names, p.ref)
		}
	}
	return names
}
