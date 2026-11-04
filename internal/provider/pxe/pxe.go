// Package pxe is the provider type of the machines ump boots over the
// network: those of pxe.yaml in the home (internal/pxe), each the
// server pxe/NAME. No API knows them. ump lists them from pxe.yaml with
// what the PXE servers of their sites report (.pxe/<site>/ in the
// backend), and their rescue system is a network boot ump arms: the
// machine boots UMP's rescue system at its next boot, a power cycle by
// the operator or a reboot over SSH, and reports in to the PXE server,
// where ump waits for it. An unattended installation is such a boot
// too, which installs the specification of its arm by itself.
package pxe

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
	netboot "github.com/ofzhur/ump/internal/pxe"
)

// TypeName is the provider type name, and AccountName the account every
// machine of pxe.yaml belongs to.
const (
	TypeName    = netboot.TypeName
	AccountName = "pxe"
)

// Type registers the pxe provider type.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindBareMetal,
	Description:  "machines booted over the network (pxe.yaml, ump config pxe): UMP's rescue system from the PXE server of their site",
	Capabilities: []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRescue},
	HomeListed:   true,
	AddHint:      "ump config pxe create NAME --mac MAC puts one into pxe.yaml",
	New: func(account string, settings map[string]string, opts provider.Options) (provider.Provider, error) {
		h, _ := opts.PXE.(*Home)
		if h == nil {
			h = &Home{}
		}
		return &Provider{account: account, home: h}, nil
	},
}

// Home is what the CLI read for the pxe provider type, handed over as
// provider.Options.PXE.
type Home struct {
	// Config is pxe.yaml, valid; nil without one. Err says why it is
	// not valid, which the listing reports.
	Config *netboot.Config
	Err    error
	// Store is the documents of the sites, in the home's backend.
	Store netboot.Store
	// Installed says which machines ump has an installation of, by
	// name (the state).
	Installed map[string]bool
	// By names who arms a boot, user@host.
	By string
	// Now is the clock of the documents; nil means time.Now. Waits go
	// by the real time.
	Now func() time.Time
	// Poll is how often the status documents are read while waiting,
	// ReadyWait how long EnableRescue waits for a PXE server to take an
	// arm; zero means 3 s and 30 s.
	Poll      time.Duration
	ReadyWait time.Duration
}

func (h *Home) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Home) poll() time.Duration {
	if h.Poll > 0 {
		return h.Poll
	}
	return 3 * time.Second
}

func (h *Home) readyWait() time.Duration {
	if h.ReadyWait > 0 {
		return h.ReadyWait
	}
	return 30 * time.Second
}

// live is how recent a PXE server's status must be for it to run, and
// noBoot how long after an arm a machine that did not boot from the
// network is said.
const (
	live   = 3 * time.Minute
	noBoot = 3 * time.Minute
	// armedFor is the window of an arm nobody waits for (--no-reboot,
	// --no-wait).
	armedFor = 24 * time.Hour
)

// Provider lists the machines of pxe.yaml and boots them into UMP's
// rescue system over the network.
type Provider struct {
	account  string
	home     *Home
	warnings []string
}

func (p *Provider) Type() string    { return TypeName }
func (p *Provider) Account() string { return p.account }
func (p *Provider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRescue}
}

// List returns the machines of pxe.yaml, with the status their arms,
// outcomes and the PXE servers' reports give. When those cannot be
// read, the machines are listed all the same, with their addresses,
// which are pxe.yaml's, and the status unknown (Warnings says why).
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	p.warnings = nil
	if p.home.Err != nil {
		return nil, p.home.Err
	}
	c := p.home.Config
	if c == nil || len(c.Names()) == 0 {
		return []provider.Machine{}, nil
	}
	sn, err := p.home.Store.Load(ctx, c.SiteNames()...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		p.warnings = append(p.warnings, fmt.Sprintf("what the sites hold could not be read (%v); the machines are pxe.yaml's, their status unknown", err))
	}
	now := p.home.now()
	out := make([]provider.Machine, 0, len(c.Names()))
	for _, m := range c.Merged() {
		out = append(out, p.machine(m, sn, now))
	}
	return out, nil
}

// Warnings says what the last List lacked.
func (p *Provider) Warnings() []string { return p.warnings }

// Addresses are where ump reaches a machine's installed system: the
// static addresses of its network, else address (a DHCP reservation),
// never one learned at boot.
func Addresses(m *netboot.Machine) (ipv4, ipv6 string) {
	if m.Network.IPv4.Mode == profile.UplinkStatic {
		ipv4, _, _ = strings.Cut(m.Network.IPv4.Address, "/")
	}
	if m.Network.IPv6.Mode == profile.UplinkStatic {
		ipv6, _, _ = strings.Cut(m.Network.IPv6.Address, "/")
	}
	switch {
	case m.Address == "":
	case strings.Contains(m.Address, ":") && ipv6 == "":
		ipv6 = m.Address
	case !strings.Contains(m.Address, ":") && ipv4 == "":
		ipv4 = m.Address
	}
	return ipv4, ipv6
}

// machine is a machine of pxe.yaml as ump lists it, from the documents
// of its site (sn; nil: they could not be read).
func (p *Provider) machine(m *netboot.Machine, sn *netboot.Snapshot, now time.Time) provider.Machine {
	out := provider.Machine{Provider: TypeName, Account: p.account, ID: m.Name, Name: m.Name, Kind: provider.KindBareMetal,
		Arch: m.Arch, Location: m.Site, Jump: m.Jump}
	out.IPv4, out.IPv6 = Addresses(m)
	out.Labels = map[string]string{"mac": strings.Join(m.MACs, ",")}
	if m.Note != "" {
		out.Labels["note"] = m.Note
	}
	if sn == nil {
		out.Status = "unknown"
		return out
	}
	out.Status = netboot.StatusWords(m.Mode, sn.Arm(m.Site, m.Name), sn.Outcome(m.Site, m.Name), p.home.Installed[m.Name], now)
	if s, _ := lastSeen(sn, m); s != nil {
		if out.Arch == "" {
			out.Arch = s.Arch
		}
		if h := s.Hello; h != nil {
			if out.Arch == "" {
				out.Arch = h.Arch
			}
			out.Product = strings.TrimSpace(h.DMI["sys_vendor"] + " " + h.DMI["product_name"])
		}
	}
	return out
}

// lastSeen is what the PXE servers of the machine's site saw of it
// last, by any of its MACs, and which server; nil when none did.
func lastSeen(sn *netboot.Snapshot, m *netboot.Machine) (*netboot.Seen, string) {
	var seen *netboot.Seen
	var by string
	for _, st := range sn.Statuses[m.Site] {
		for _, mac := range m.MACs {
			if s := st.Machines[mac]; s != nil && (seen == nil || s.At.After(seen.At)) {
				seen, by = s, st.Daemon
			}
		}
	}
	return seen, by
}

// lookup is the machine of pxe.yaml that m is.
func (p *Provider) lookup(m provider.Machine) (*netboot.Machine, error) {
	if p.home.Err != nil {
		return nil, p.home.Err
	}
	if p.home.Config != nil {
		if pm := p.home.Config.Machine(m.ID); pm != nil {
			return pm, nil
		}
	}
	return nil, fmt.Errorf("%s is no machine of pxe.yaml", m.Ref())
}

// Describe says what pxe.yaml, the arm, the outcome and the PXE
// servers know of the machine.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	sn, err := p.home.Store.Load(ctx, pm.Site)
	if err != nil {
		return nil, err
	}
	now := p.home.now()
	d := &provider.Details{Machine: m}

	boot := provider.Section{Name: "PXE"}
	site := pm.Site
	if s := p.home.Config.Sites[pm.Site]; s != nil && s.Description != "" {
		site += " (" + s.Description + ")"
	}
	boot.AddLabeled("macs", "MACs", pm.MACs).Add("site", site).Add("mode", pm.Mode)
	if pm.Jump != "" {
		boot.Add("jump", pm.Jump)
	}
	if pm.Arch != "" {
		boot.Add("arch", pm.Arch)
	}
	if pm.Firmware != "" {
		boot.Add("firmware", pm.Firmware)
	}
	if a := sn.Arm(pm.Site, pm.Name); a != nil {
		v := fmt.Sprintf("%s %s, by %s, %s", a.Mode, netboot.ShortID(a.ID), a.By, a.Created.UTC().Format(time.RFC3339))
		switch o := sn.Outcome(pm.Site, pm.Name); {
		case !a.Pending(o, now):
			v += " (done)"
		case !a.Until.IsZero():
			v += ", until " + a.Until.UTC().Format(time.RFC3339)
		}
		boot.Add("arm", v)
	}
	if pm.Note != "" {
		boot.Add("note", pm.Note)
	}
	d.Sections = append(d.Sections, boot)

	n := pm.Network
	network := provider.Section{Name: "Network"}
	network.AddLabeled("uplink", "Uplink", n.MAC).AddLabeled("ipv4", "IPv4", uplinkWords(n.IPv4)).AddLabeled("ipv6", "IPv6", uplinkWords(n.IPv6))
	switch {
	case n.DNSFromDHCP:
		network.AddLabeled("dns", "DNS", "the lease's")
	case len(n.DNS) > 0:
		network.AddLabeled("dns", "DNS", n.DNS)
	default:
		network.AddLabeled("dns", "DNS", "1.1.1.1, 8.8.8.8")
	}
	if pm.Address != "" {
		network.Add("address", pm.Address)
	}
	d.Sections = append(d.Sections, network)

	if s, by := lastSeen(sn, pm); s != nil {
		seen := provider.Section{Name: "Seen"}
		seen.Add("by", by).Add("at", s.At.UTC().Format(time.RFC3339))
		if s.Address != "" {
			seen.AddLabeled("address", "Address", s.Address+" (learned at boot)")
		}
		if s.Platform != "" || s.Firmware != "" {
			seen.Add("boot", strings.TrimSpace(s.Arch+" "+s.Firmware+" "+s.Platform))
		}
		answer := s.Answer
		if s.Reason != "" {
			answer += ": " + s.Reason
		}
		seen.Add("answer", answer)
		if h := s.Hello; h != nil {
			if a := h.Address(); a != "" {
				seen.AddLabeled("rescue", "Rescue", fmt.Sprintf("%s at %s (learned at boot), %s", h.Rescue, a, h.At.UTC().Format(time.RFC3339)))
			}
			if v := strings.TrimSpace(h.DMI["sys_vendor"] + " " + h.DMI["product_name"]); v != "" {
				seen.Add("product", v)
			}
			if v := h.DMI["product_serial"]; v != "" {
				seen.Add("serial", v)
			}
			if h.MemoryMB > 0 {
				seen.AddLabeled("memory", "Memory", fmt.Sprintf("%d MB", h.MemoryMB))
			}
			var disks []string
			for _, g := range h.GuardDisks {
				if g.ID != nil {
					disks = append(disks, g.Path+" ("+*g.ID+")")
				} else {
					disks = append(disks, g.Path)
				}
			}
			if len(disks) > 0 {
				seen.Add("disks", disks)
			}
			if w := netboot.BootOrder(h); w != "" {
				seen.AddLabeled("boot_order", "Boot order", w)
			}
			if ip := bmcAddress(h.IPMILan); ip != "" {
				seen.AddLabeled("bmc", "BMC", ip)
			}
		}
		if len(s.Signals) > 0 {
			seen.Add("signals", s.Signals)
		}
		d.Sections = append(d.Sections, seen)
	}

	if o := sn.Outcome(pm.Site, pm.Name); o != nil && o.Phase != netboot.PhaseBooted {
		last := provider.Section{Name: "Last install"}
		last.Add("outcome", o.Phase).Add("at", o.At.UTC().Format(time.RFC3339))
		if o.Exit != nil {
			last.Add("exit", *o.Exit)
		}
		if o.Reason != "" {
			last.Add("reason", o.Reason)
		}
		d.Sections = append(d.Sections, last)
	}
	return d, nil
}

// uplinkWords is how the installed system gets an address of a family.
func uplinkWords(a profile.UplinkAddress) string {
	switch a.Mode {
	case profile.UplinkStatic:
		if a.Gateway != "" {
			return a.Address + " via " + a.Gateway
		}
		return a.Address
	case "":
		return profile.UplinkNone
	}
	return a.Mode
}

// bmcAddress is the IP address of ipmitool lan print's output.
func bmcAddress(lan string) string {
	for _, l := range strings.Split(lan, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.TrimSpace(k) == "IP Address" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// RescueEnabled says whether the machine boots UMP's rescue system at
// its next boot: by its mode, or by an arm not yet taken.
func (p *Provider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return false, err
	}
	if pm.Mode == netboot.ModeRescue {
		return true, nil
	}
	a, _, err := p.home.Store.Arm(ctx, pm.Site, pm.Name)
	if err != nil || a == nil || a.Mode != netboot.ModeRescue {
		return false, err
	}
	o, _, err := p.home.Store.Outcome(ctx, pm.Site, pm.Name)
	if err != nil {
		return false, err
	}
	return a.Pending(o, p.home.now()), nil
}

// EnableRescue arms a boot of UMP's rescue system with the keys, for as
// long as the caller waits (24 hours with NoReboot), and waits a while
// for a PXE server of the machine's site to take it. The note says
// whether one did. The machine boots it at its next boot. An armed
// installation stays, and so does one whose outcome waits for ump
// (unsettled).
func (p *Provider) EnableRescue(ctx context.Context, m provider.Machine, opts provider.RescueOptions) (*provider.RescueInfo, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	store, now := p.home.Store, p.home.now()
	old, version, err := store.Arm(ctx, pm.Site, pm.Name)
	if err != nil {
		return nil, err
	}
	if old != nil && old.Mode != netboot.ModeRescue {
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
	window := opts.Wait
	if opts.NoReboot || window <= 0 {
		window = armedFor
	}
	a := &netboot.Arm{ID: netboot.NewArmID(), Machine: pm.Name, Mode: netboot.ModeRescue, By: p.home.By, Created: now, Until: now.Add(window)}
	for _, k := range opts.Keys {
		a.Keys = append(a.Keys, k.Data)
	}
	if _, err := store.PutArm(ctx, pm.Site, a, version); err != nil {
		return nil, fmt.Errorf("arming the rescue boot of %s: %w", pm.Name, err)
	}
	until := a.Until
	info := &provider.RescueInfo{User: "root", Arm: a.ID, Until: &until}
	if opts.NoReboot {
		info.Note = p.servers(ctx, pm, a.ID, 0, "the rescue system")
	} else {
		info.Note = p.servers(ctx, pm, a.ID, p.home.readyWait(), "the rescue system")
	}
	return info, nil
}

// servers says whether a PXE server of the machine's site can serve
// the arm, which boots what, waiting up to wait for one that runs to
// take it.
func (p *Provider) servers(ctx context.Context, pm *netboot.Machine, arm string, wait time.Duration, what string) string {
	note, _ := p.ready(ctx, pm, arm, wait, what)
	return note
}

// ready is servers' note, and whether a server took the arm.
func (p *Provider) ready(ctx context.Context, pm *netboot.Machine, arm string, wait time.Duration, what string) (string, bool) {
	start := time.Now()
	for {
		sn, err := p.home.Store.Load(ctx, pm.Site)
		if err != nil {
			return fmt.Sprintf("the PXE servers of site %s could not be asked: %v", pm.Site, err), false
		}
		now := p.home.now()
		var running, cannot []string
		var heard time.Time
		for _, st := range sn.Statuses[pm.Site] {
			if st.Updated.After(heard) {
				heard = st.Updated
			}
			if now.Sub(st.Updated) > live {
				continue
			}
			running = append(running, st.Daemon)
			if as := st.Arms[arm]; as != nil && as.Ready || taken(st, pm, arm) {
				return fmt.Sprintf("PXE server %s of site %s is ready to boot it", st.Daemon, pm.Site), true
			} else if as != nil && as.Cannot != "" {
				cannot = append(cannot, st.Daemon+": "+as.Cannot)
			}
		}
		if len(running) == 0 {
			since := "never heard of"
			if !heard.IsZero() {
				since = "last heard " + now.Sub(heard).Round(time.Second).String() + " ago"
			}
			return fmt.Sprintf("no PXE server of site %s runs (%s); one must, for the machine to boot %s (ump pxe serve --site %s)", pm.Site, since, what, pm.Site), false
		}
		switch {
		case wait == 0:
			return fmt.Sprintf("PXE server %s of site %s takes the arm at its next poll", strings.Join(running, ", "), pm.Site), false
		case time.Since(start) >= wait:
			if len(cannot) > 0 {
				return fmt.Sprintf("the PXE servers of site %s cannot boot it yet: %s", pm.Site, strings.Join(cannot, "; ")), false
			}
			return fmt.Sprintf("no PXE server of site %s has taken the arm yet (running: %s); see ump pxe status %s", pm.Site, strings.Join(running, ", "), pm.Name), false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(p.home.poll()):
		}
	}
}

// taken says that the PXE server booted the machine for the arm, or
// heard from its rescue system.
func taken(st *netboot.Status, pm *netboot.Machine, arm string) bool {
	for _, mac := range pm.MACs {
		if s := st.Machines[mac]; s != nil && (s.Arm == arm || s.Hello != nil && s.Hello.Arm == arm) {
			return true
		}
	}
	return false
}

// DisableRescue removes the arm of a rescue boot. A machine whose mode
// is rescue boots it at every boot: pxe.yaml says so, not ump.
func (p *Provider) DisableRescue(ctx context.Context, m provider.Machine) error {
	pm, err := p.lookup(m)
	if err != nil {
		return err
	}
	if pm.Mode == netboot.ModeRescue {
		return fmt.Errorf("%s boots UMP's rescue system at every boot (mode: rescue in pxe.yaml); set mode: local there (ump config pxe edit %s)", pm.Name, pm.Name)
	}
	a, _, err := p.home.Store.Arm(ctx, pm.Site, pm.Name)
	if err != nil || a == nil {
		return err
	}
	if a.Mode != netboot.ModeRescue {
		return fmt.Errorf("%s has an armed %s, no rescue boot (ump pxe disarm %s)", pm.Name, a.Mode, pm.Name)
	}
	return p.home.Store.Disarm(ctx, pm.Site, pm.Name)
}

// WaitRescue waits for the rescue system of the arm to report in to a
// PXE server of the machine's site, saying what the servers see
// meanwhile, and returns the address and host keys it reported. When
// the context ends first, the arm goes.
func (p *Provider) WaitRescue(ctx context.Context, m provider.Machine, info *provider.RescueInfo, progress io.Writer) (string, []string, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return "", nil, err
	}
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
	booted := false
	for {
		sn, err := p.home.Store.Load(ctx, pm.Site)
		if err == nil {
			// an arm that could not be read is not gone
			err = sn.Errors[netboot.ArmPath(pm.Site, pm.Name)]
		}
		switch {
		case err != nil && ctx.Err() == nil:
			say("The backend does not answer (%v); asking again.", err)
		case err == nil:
			if addr, keys, ok := hello(sn, pm, info.Arm); ok {
				if addr == "" {
					return "", nil, fmt.Errorf("the rescue system of %s reported in without an IPv4 address; see its console", pm.Name)
				}
				return addr, keys, nil
			}
			if a := sn.Arm(pm.Site, pm.Name); a == nil || a.ID != info.Arm {
				return "", nil, fmt.Errorf("the rescue boot of %s (arm %s) was disarmed or replaced meanwhile (ump pxe status %s)", pm.Name, netboot.ShortID(info.Arm), pm.Name)
			}
			for _, st := range sn.Statuses[pm.Site] {
				if as := st.Arms[info.Arm]; as != nil && as.Cannot != "" {
					say("PXE server %s cannot boot %s: %s", st.Daemon, pm.Name, as.Cannot)
				}
				for _, mac := range pm.MACs {
					s := st.Machines[mac]
					if s == nil || s.At.Before(since) && s.Arm != info.Arm {
						continue
					}
					switch {
					case s.Arm == info.Arm && s.Answer == netboot.ModeRescue:
						booted = true
						at := ""
						if s.Address != "" {
							at = " at " + s.Address
						}
						say("%s boots UMP's rescue system from PXE server %s%s.", pm.Name, st.Daemon, at)
					case s.Reason != "":
						say("PXE server %s sent %s to its disk: %s", st.Daemon, pm.Name, s.Reason)
					}
					for _, sig := range s.Signals {
						say("PXE server %s: %s", st.Daemon, sig)
					}
				}
			}
		}
		if !booted && time.Since(start) >= noBoot {
			say("No network boot of %s seen since the arm: check its power, and that its boot order starts with the network.", pm.Name)
		}
		select {
		case <-ctx.Done():
			p.disarm(ctx, pm, info.Arm)
			return "", nil, fmt.Errorf("the rescue system of %s did not report in; the arm is gone (ump pxe status %s shows what the PXE servers saw): %w", pm.Name, pm.Name, ctx.Err())
		case <-time.After(p.home.poll()):
		}
	}
}

// hello is the address and host keys of the rescue system of the arm,
// when it reported in.
func hello(sn *netboot.Snapshot, pm *netboot.Machine, arm string) (string, []string, bool) {
	for _, st := range sn.Statuses[pm.Site] {
		for _, mac := range pm.MACs {
			if s := st.Machines[mac]; s != nil && s.Hello != nil && s.Hello.Arm == arm {
				return s.Hello.Address(), slices.Clone(s.Hello.HostKeys), true
			}
		}
	}
	return "", nil, false
}

// disarm removes the arm when it is still the one of the wait and no
// PXE server took it, for a wait that gave up.
func (p *Provider) disarm(ctx context.Context, pm *netboot.Machine, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	a, _, err := p.home.Store.Arm(ctx, pm.Site, pm.Name)
	if err != nil || a == nil || a.ID != id {
		return
	}
	_ = p.home.Store.Disarm(ctx, pm.Site, pm.Name)
}

// Preflight says what stands in the way of the rescue system: no PXE
// server of the site running, or a machine its servers do not serve
// yet.
func (p *Provider) Preflight(ctx context.Context, m provider.Machine) ([]string, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	sn, err := p.home.Store.Load(ctx, pm.Site)
	if err != nil {
		return nil, err
	}
	var out []string
	switch d := sn.Sites[pm.Site]; {
	case d == nil:
		out = append(out, fmt.Sprintf("site %s has no site.json, so its PXE servers boot nothing: ump config pxe apply", pm.Site))
	case d.Machine(pm.Name) == nil:
		out = append(out, fmt.Sprintf("the PXE servers of site %s do not know %s yet: ump config pxe apply", pm.Site, pm.Name))
	}
	now := p.home.now()
	if !slices.ContainsFunc(sn.Statuses[pm.Site], func(st *netboot.Status) bool { return now.Sub(st.Updated) <= live }) {
		out = append(out, fmt.Sprintf("no PXE server of site %s runs (ump pxe serve --site %s): the rescue system of %s boots from one", pm.Site, pm.Site, m.Ref()))
	}
	return out, nil
}

// CheckInstall refuses an installation on a machine whose mode is
// rescue: it would boot UMP's rescue system again instead of the new
// system, and ump could not record it.
func (p *Provider) CheckInstall(ctx context.Context, m provider.Machine) error {
	pm, err := p.lookup(m)
	if err != nil || pm.Mode != netboot.ModeRescue {
		return nil // a machine pxe.yaml lacks is refused on the way
	}
	return fmt.Errorf("%s boots UMP's rescue system at every boot (mode: rescue in pxe.yaml), so a new system would never start: set mode: local first (ump config pxe edit %s)", m.Ref(), pm.Name)
}

// Hostname is the machine's hostname as pxe.yaml gives it (its name by
// default), the rescue system's and the installed system's; "" for a
// machine pxe.yaml lacks.
func (p *Provider) Hostname(m provider.Machine) string {
	pm, err := p.lookup(m)
	if err != nil {
		return ""
	}
	return pm.Hostname
}

// Uplink is the installed system's uplink as pxe.yaml gives it.
func (p *Provider) Uplink(ctx context.Context, m provider.Machine) (*profile.UplinkSpec, error) {
	pm, err := p.lookup(m)
	if err != nil {
		return nil, err
	}
	u := pm.Network
	u.DNS = slices.Clone(u.DNS)
	return &u, nil
}
