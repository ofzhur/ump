package pxe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/provider"
	netboot "github.com/ofzhur/ump/internal/pxe"
)

// Unattended installations: an arm of mode install with the
// specification the CLI rendered, which a PXE server of the machine's
// site hands to one boot of UMP's rescue system; the server writes what
// came of it, the outcome, and the CLI records the installation once it
// reached the new system.

// InstallDisks says which disks an unattended installation of the
// machine may erase: those given, by stable names (pinned); else
// exactly those the rescue system of its last network boot reported
// (seen), when each has a WWN or a serial number; else blank ones
// alone.
func (p *Provider) InstallDisks(ctx context.Context, m provider.Machine, disks []string) (*provider.InstallDisks, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	if len(disks) > 0 {
		var names []string
		for _, d := range disks {
			d = strings.TrimPrefix(strings.TrimSpace(d), "/dev/disk/by-id/")
			switch {
			case !netboot.StableDisk(d):
				return nil, fmt.Errorf("the disk %s is not named by a stable name: an unattended installation takes names under /dev/disk/by-id (nvme-eui..., wwn-...) or serial:SERIAL, which name the same disk at every boot", d)
			case slices.Contains(names, d):
				return nil, fmt.Errorf("the disk %s is named twice", d)
			}
			names = append(names, d)
		}
		return &provider.InstallDisks{Rule: provider.DisksPinned, Disks: names, Words: "the disks named, " + strings.Join(names, ", ")}, nil
	}
	sn, err := p.home.Store.Load(ctx, pm.Site)
	if err != nil {
		return nil, err
	}
	blank := func(why string) *provider.InstallDisks {
		return &provider.InstallDisks{Rule: provider.DisksBlank, Words: "blank disks alone (" + why + ")"}
	}
	h := lastHello(sn, pm)
	if h == nil || len(h.GuardDisks) == 0 {
		return blank("no network boot of " + pm.Name + " reported its disks"), nil
	}
	var ids, shown []string
	for _, g := range h.GuardDisks {
		switch {
		case g.ID == nil:
			return blank(fmt.Sprintf("%s, seen at its last network boot, has neither a WWN nor a serial number", g.Path)), nil
		case slices.Contains(ids, *g.ID):
			return blank(fmt.Sprintf("two disks seen at its last network boot have the ID %s", *g.ID)), nil
		}
		ids = append(ids, *g.ID)
		shown = append(shown, g.Path+" ("+*g.ID+")")
	}
	return &provider.InstallDisks{Rule: provider.DisksSeen, Disks: ids,
		Words: fmt.Sprintf("the disks its network boot of %s reported, %s", h.At.UTC().Format("2006-01-02 15:04 UTC"), strings.Join(shown, ", "))}, nil
}

// lastHello is the last hello of the machine's rescue system that a PXE
// server of its site heard, nil when none did.
func lastHello(sn *netboot.Snapshot, pm *netboot.Machine) *netboot.Hello {
	var last *netboot.Hello
	for _, st := range sn.Statuses[pm.Site] {
		for _, mac := range pm.MACs {
			if s := st.Machines[mac]; s != nil && s.Hello != nil && (last == nil || s.Hello.At.After(last.At)) {
				last = s.Hello
			}
		}
	}
	return last
}

// ArmInstall arms an unattended installation of the machine: the
// specification with its hash, the keys, the disk rule, and the firmware
// pxe.yaml gives, else the one its last network boot had. With a wait
// it waits a while for a PXE server of the site to take the arm; the
// note says whether one did. An armed boot stays, and so does an
// installation whose outcome waits for ump (unsettled).
func (p *Provider) ArmInstall(ctx context.Context, m provider.Machine, in provider.InstallArm) (*provider.ArmInfo, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	if in.Disks == nil {
		return nil, errors.New("an unattended installation needs a disk rule")
	}
	rule := &netboot.DiskRule{Rule: in.Disks.Rule}
	switch in.Disks.Rule {
	case provider.DisksPinned:
		rule.Names = slices.Clone(in.Disks.Disks)
	case provider.DisksSeen:
		rule.IDs = slices.Clone(in.Disks.Disks)
	}
	if err := rule.Check(); err != nil {
		return nil, err
	}
	hash, err := netboot.SpecHash(in.Spec)
	if err != nil {
		return nil, fmt.Errorf("the specification: %w", err)
	}
	store, now := p.home.Store, p.home.now()
	old, version, err := store.Arm(ctx, pm.Site, pm.Name)
	if err != nil {
		return nil, err
	}
	if old != nil {
		o, _, err := store.Outcome(ctx, pm.Site, pm.Name)
		if err != nil {
			return nil, err
		}
		if old.Pending(o, now) {
			return nil, fmt.Errorf("%s has an armed %s (%s, by %s); disarm it first (ump pxe disarm %s)", pm.Name, old.Mode, netboot.ShortID(old.ID), old.By, pm.Name)
		}
		if err := unsettled(pm, old, o); err != nil {
			return nil, err
		}
	}
	firmware := pm.Firmware
	if firmware == "" {
		if sn, err := store.Load(ctx, pm.Site); err == nil {
			if h := lastHello(sn, pm); h != nil {
				firmware = h.Firmware
			} else if s, _ := lastSeen(sn, pm); s != nil {
				firmware = s.Firmware
			}
		}
	}
	a := &netboot.Arm{ID: netboot.NewArmID(), Machine: pm.Name, Mode: netboot.ModeInstall, By: p.home.By, Created: now, Until: in.Until,
		Spec: in.Spec, SpecHash: hash, Firmware: firmware, Disks: rule, Import: in.Import}
	for _, k := range in.Keys {
		a.Keys = append(a.Keys, k.Data)
	}
	if _, err := store.PutArm(ctx, pm.Site, a, version); err != nil {
		return nil, fmt.Errorf("arming the installation of %s: %w", pm.Name, err)
	}
	info := &provider.ArmInfo{ID: a.ID}
	if !a.Until.IsZero() {
		until := a.Until
		info.Until = &until
	}
	wait := time.Duration(0)
	if in.Wait {
		wait = p.home.readyWait()
	}
	info.Note, info.Ready = p.ready(ctx, pm, a.ID, wait, "its installation")
	return info, nil
}

// unsettled refuses to replace the arm of an installation whose outcome
// waits for the CLI, which reads what it needs from the arm: one that
// installed, which it records, or failed, whose private addresses it
// gives back.
func unsettled(pm *netboot.Machine, a *netboot.Arm, o *netboot.Outcome) error {
	if a == nil || a.Mode != netboot.ModeInstall || o == nil || o.Arm != a.ID {
		return nil
	}
	switch o.Phase {
	case netboot.PhaseInstalled:
		return fmt.Errorf("%s installed unattended (%s) and waits to be recorded: ump provision records it first, or ump server reinstall replaces it", pm.Name, netboot.ShortID(a.ID))
	case netboot.PhaseFailed:
		return fmt.Errorf("the unattended installation of %s (%s) failed: ump pxe disarm %s clears it first, and gives its private addresses back", pm.Name, netboot.ShortID(a.ID), pm.Name)
	}
	return nil
}

// outcomeOf is an outcome as the CLI takes it: an installation that is
// still being written after its deadline is interrupted, whether a
// server wrote so yet or not.
func outcomeOf(o *netboot.Outcome, now time.Time) *provider.InstallOutcome {
	phase := o.Phase
	if phase == netboot.PhaseWriting && !now.Before(o.Deadline) {
		phase = netboot.PhaseInterrupted
	}
	return &provider.InstallOutcome{Phase: phase, At: o.At, By: o.Daemon, Deadline: o.Deadline, Exit: o.Exit, Reason: o.Reason,
		Written: o.Written, Record: o.Record, HostKeys: slices.Clone(o.HostKeys), LogTail: o.LogTail}
}

// Installation is the machine's unattended installation as its site
// holds it: its arm, and what came of it; nil when it has neither (an
// armed rescue is none).
func (p *Provider) Installation(ctx context.Context, m provider.Machine) (*provider.InstallState, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	store := p.home.Store
	a, _, err := store.Arm(ctx, pm.Site, pm.Name)
	if err != nil {
		return nil, err
	}
	o, _, err := store.Outcome(ctx, pm.Site, pm.Name)
	if err != nil {
		return nil, err
	}
	return InstallationOf(a, o, p.home.now()), nil
}

// InstallationOf is the unattended installation a machine's arm and
// outcome (nil: none) hold at now, as Installation gives it.
func InstallationOf(a *netboot.Arm, o *netboot.Outcome, now time.Time) *provider.InstallState {
	if a != nil && a.Mode != netboot.ModeInstall {
		a = nil
	}
	if o != nil && (o.Phase == netboot.PhaseBooted || a != nil && o.Arm != a.ID) {
		o = nil
	}
	switch {
	case a == nil && o == nil:
		return nil
	case a == nil:
		return &provider.InstallState{ID: o.Arm, Outcome: outcomeOf(o, now)}
	}
	st := &provider.InstallState{ID: a.ID, By: a.By, Created: a.Created, Until: a.Until, Import: a.Import,
		Pending: o == nil && !a.Expired(now)}
	if o != nil {
		st.Outcome = outcomeOf(o, now)
	}
	return st
}

// Disarm removes the installation's arm, then its outcome: nothing of
// another arm, and nothing of an installation that a boot claimed and
// writes (provider.ErrInstalling). A claim that comes between the two
// is said so too, though the arm is gone; a PXE server reads the arm
// again after its claim, and takes the claim back when the arm went
// before it.
func (p *Provider) Disarm(ctx context.Context, m provider.Machine, id string) error {
	pm, err := p.lookup(m)
	if err != nil {
		return err
	}
	store := p.home.Store
	writing := func() (*netboot.Outcome, bool, error) {
		o, _, err := store.Outcome(ctx, pm.Site, pm.Name)
		if err != nil || o == nil || o.Arm != id {
			return nil, false, err
		}
		return o, o.Phase == netboot.PhaseWriting && p.home.now().Before(o.Deadline), nil
	}
	if _, claimed, err := writing(); err != nil || claimed {
		if claimed {
			err = provider.ErrInstalling
		}
		return err
	}
	a, _, err := store.Arm(ctx, pm.Site, pm.Name)
	if err != nil {
		return err
	}
	if a != nil && a.ID == id {
		if err := store.Disarm(ctx, pm.Site, pm.Name); err != nil {
			return err
		}
	}
	o, claimed, err := writing()
	switch {
	case err != nil:
		return err
	case claimed:
		return provider.ErrInstalling
	case o != nil:
		return store.Files.Delete(ctx, netboot.OutcomePath(pm.Site, pm.Name))
	}
	return nil
}

// WaitInstall follows the installation of the arm until its outcome:
// what the PXE servers of the machine's site see of its boots, their
// claim, the installer's output as they push it, and the end of it the
// report carried when they pushed none. An arm that goes or expires
// before a boot claims it ends the wait with an error, and so does the
// context, with the outcome as it stands (writing once claimed).
func (p *Provider) WaitInstall(ctx context.Context, m provider.Machine, id string, progress io.Writer) (*provider.InstallOutcome, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	store := p.home.Store
	// the waits go by the real time, what the servers saw by the
	// documents' clock
	start, since := time.Now(), p.home.now().Add(-time.Minute)
	said := map[string]bool{}
	say := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if !said[line] {
			said[line] = true
			fmt.Fprintln(progress, line)
		}
	}
	var last *provider.InstallOutcome
	var printed []byte
	follow := func(data []byte) {
		if part := newPart(printed, data); len(part) > 0 {
			progress.Write(part)
			if part[len(part)-1] != '\n' {
				io.WriteString(progress, "\n")
			}
		}
		if len(data) > 0 {
			printed = data
		}
	}
	booted := false
	for {
		sn, err := store.Load(ctx, pm.Site)
		if err == nil {
			// a document that could not be read is not gone
			err = errors.Join(sn.Errors[netboot.ArmPath(pm.Site, pm.Name)], sn.Errors[netboot.OutcomePath(pm.Site, pm.Name)])
		}
		switch {
		case err != nil && ctx.Err() == nil:
			say("The backend does not answer (%v); asking again.", err)
		case err == nil:
			now := p.home.now()
			a, o := sn.Arm(pm.Site, pm.Name), sn.Outcome(pm.Site, pm.Name)
			for _, st := range sn.Statuses[pm.Site] {
				if as := st.Arms[id]; as != nil && as.Cannot != "" && last == nil {
					say("PXE server %s cannot install %s: %s", st.Daemon, pm.Name, as.Cannot)
				}
				for _, mac := range pm.MACs {
					s := st.Machines[mac]
					if s == nil || s.At.Before(since) && s.Arm != id {
						continue
					}
					switch {
					case s.Arm == id && s.Answer == netboot.ModeInstall:
						booted = true
						at := ""
						if s.Address != "" {
							at = " at " + s.Address
						}
						say("%s boots its installation from PXE server %s%s.", pm.Name, st.Daemon, at)
					case s.Arm == id && s.Answer == netboot.AnswerRescueBare:
						say("%s booted again while it installs: PXE server %s gave it UMP's rescue system, without the installation.", pm.Name, st.Daemon)
					case s.Reason != "" && last == nil:
						say("PXE server %s sent %s to its disk: %s", st.Daemon, pm.Name, s.Reason)
					}
					if h := s.Hello; h != nil && h.Arm == id && h.Address() != "" {
						say("The rescue system of %s reported in at %s (learned at boot).", pm.Name, h.Address())
					}
					for _, sig := range s.Signals {
						say("PXE server %s: %s", st.Daemon, sig)
					}
				}
			}
			switch {
			case o != nil && o.Arm == id && o.Phase != netboot.PhaseBooted:
				last = outcomeOf(o, now)
				if data, _, err := store.Files.Read(ctx, netboot.LogPath(pm.Site, pm.Name)); err == nil {
					follow(data)
				}
				if o.Phase == netboot.PhaseWriting {
					say("PXE server %s claimed the installation of %s for its boot %s; it installs until %s at the latest.", o.Daemon, pm.Name, o.Boot, o.Deadline.UTC().Format("2006-01-02 15:04 UTC"))
					break
				}
				follow([]byte(o.LogTail))
				return last, nil
			case last != nil:
				return last, fmt.Errorf("the outcome of the installation of %s (arm %s) went meanwhile: another ump recorded it, or it was forgotten (ump server show %s)", pm.Name, netboot.ShortID(id), m.Ref())
			case a == nil || a.ID != id:
				return nil, fmt.Errorf("the installation of %s (arm %s) was disarmed or replaced before a boot claimed it (ump pxe status %s)", pm.Name, netboot.ShortID(id), pm.Name)
			case a.Expired(now):
				return nil, fmt.Errorf("the installation of %s (arm %s) expired before a boot claimed it (ump pxe status %s)", pm.Name, netboot.ShortID(id), pm.Name)
			}
		}
		if !booted && last == nil && time.Since(start) >= noBoot {
			say("No network boot of %s seen since the arm: check its power, and that its boot order starts with the network.", pm.Name)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(p.home.poll()):
		}
	}
}

// newPart is what data holds beyond printed, the part of an output
// printed before: data goes on from printed, or from a later point of
// it when the beginning was cut off (a server pushes the end of a long
// output, without the line in front of it: netboot.LogOutput), where the
// longest end of printed that data begins with ends; nothing when
// printed holds all of data.
func newPart(printed, data []byte) []byte {
	printed, data = netboot.LogOutput(printed), netboot.LogOutput(data)
	switch {
	case bytes.HasPrefix(data, printed):
		return data[len(printed):]
	case bytes.Contains(printed, data):
		return nil
	}
	return data[overlap(printed, data):]
}

// overlap is the length of the longest end of printed that data begins
// with (the prefix function of data, run through the end of printed).
func overlap(printed, data []byte) int {
	n := min(len(printed), len(data))
	if n == 0 {
		return 0
	}
	d, p := data[:n], printed[len(printed)-n:]
	fail := make([]int32, n)
	for i, k := 1, int32(0); i < n; i++ {
		for k > 0 && d[i] != d[k] {
			k = fail[k-1]
		}
		if d[i] == d[k] {
			k++
		}
		fail[i] = k
	}
	k := int32(0)
	for _, c := range p {
		if int(k) == n {
			k = fail[k-1]
		}
		for k > 0 && c != d[k] {
			k = fail[k-1]
		}
		if c == d[k] {
			k++
		}
	}
	return int(k)
}
