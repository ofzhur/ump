package pxe

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
	netboot "github.com/ofzhur/ump/internal/pxe"
)

const testPXE = `
sites:
  fra1:
    jump: ump@198.51.100.7
    network:
      ipv4: {gateway: 192.0.2.1}
      dns: [192.0.2.53]
machines:
  web-07:
    site: fra1
    mac: 0c:c4:7a:aa:bb:01
    network:
      ipv4: {address: 192.0.2.17/24}
  db-01:
    site: fra1
    mac: [0c:c4:7a:aa:bb:10, 0c:c4:7a:aa:bb:11]
    network:
      ipv4: dhcp
      dns: dhcp
    address: 192.0.2.30
    note: rack 1, unit 7
  lab-01:
    site: fra1
    mac: 0c:c4:7a:aa:bb:20
    mode: rescue
    network:
      ipv4: dhcp
`

var now = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// testHome is a home with testPXE and its documents in a directory.
func testHome(t *testing.T) *Home {
	t.Helper()
	c, err := netboot.Parse([]byte(testPXE))
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		t.Fatal(err)
	}
	return &Home{Config: c, Store: netboot.Store{Files: &backend.Local{Dir: t.TempDir()}}, By: "oleg@laptop",
		Now: func() time.Time { return now }, Poll: 5 * time.Millisecond, ReadyWait: 200 * time.Millisecond}
}

func newProvider(t *testing.T, h *Home) *Provider {
	t.Helper()
	p, err := Type.New(AccountName, nil, provider.Options{PXE: h})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider)
}

func machineOf(t *testing.T, p *Provider, name string) provider.Machine {
	t.Helper()
	ms, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("%s is not listed", name)
	return provider.Machine{}
}

// putStatus writes the status of PXE server pxe1 of fra1, as of now.
func putStatus(t *testing.T, h *Home, change func(st *netboot.Status)) {
	t.Helper()
	putStatusOf(t, h, "pxe1", change)
}

// putStatusOf writes the status of a PXE server of fra1, as of now.
func putStatusOf(t *testing.T, h *Home, daemon string, change func(st *netboot.Status)) {
	t.Helper()
	st := &netboot.Status{Daemon: daemon, Site: "fra1", Version: "v0.8.0", Started: now.Add(-time.Hour), Updated: now,
		Machines: map[string]*netboot.Seen{}, Arms: map[string]*netboot.ArmState{}}
	if change != nil {
		change(st)
	}
	if err := h.Store.PutStatus(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func TestList(t *testing.T) {
	h := testHome(t)
	ctx := context.Background()
	h.Installed = map[string]bool{"db-01": true}
	if _, err := h.Store.PutArm(ctx, "fra1", &netboot.Arm{ID: "a7f3c2e1a7f3c2e1", Machine: "web-07", Mode: netboot.ModeRescue, Created: now, Until: now.Add(time.Hour)}, ""); err != nil {
		t.Fatal(err)
	}
	putStatus(t, h, func(st *netboot.Status) {
		st.Machines["0c:c4:7a:aa:bb:11"] = &netboot.Seen{Name: "db-01", At: now.Add(-time.Minute), Address: "192.0.2.99", Arch: "amd64", Answer: "local",
			Hello: &netboot.Hello{Arch: "amd64", DMI: map[string]string{"sys_vendor": "Supermicro", "product_name": "SYS-5019C-M"}}}
	})
	p := newProvider(t, h)
	ms, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("%d machines: %+v", len(ms), ms)
	}
	web, db, lab := ms[0], ms[1], ms[2]
	for _, c := range []struct {
		got, want string
	}{
		{web.Ref(), "pxe/web-07"}, {web.Account, "pxe"}, {web.IPv4, "192.0.2.17"}, {web.Location, "fra1"},
		{web.Status, "armed rescue"}, {web.Jump, "ump@198.51.100.7"}, {web.Labels["mac"], "0c:c4:7a:aa:bb:01"},
		{db.IPv4, "192.0.2.30"}, {db.Arch, "amd64"}, {db.Product, "Supermicro SYS-5019C-M"}, {db.Status, "local"},
		{db.Labels["mac"], "0c:c4:7a:aa:bb:10,0c:c4:7a:aa:bb:11"}, {db.Labels["note"], "rack 1, unit 7"},
		// learned addresses never: lab-01 has none
		{lab.IPv4, ""}, {lab.Status, "rescue"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if web.Kind != provider.KindBareMetal {
		t.Errorf("kind %q", web.Kind)
	}

	// a backend that does not answer: the machines of pxe.yaml with
	// their addresses all the same, the status unknown, and why
	h.Store.Files = &faulty{Files: h.Store.Files, down: true}
	p = newProvider(t, h)
	ms, err = p.List(ctx)
	if err != nil || len(ms) != 3 || ms[0].IPv4 != "192.0.2.17" || ms[0].Status != "unknown" || ms[0].Jump != "ump@198.51.100.7" ||
		len(p.Warnings()) != 1 || p.Warnings()[0] != "what the sites hold could not be read (HTTP 503); the machines are pxe.yaml's, their status unknown" {
		t.Errorf("backend down: %v, %+v, %q", err, ms, p.Warnings())
	}

	// a pxe.yaml that is not valid lists nothing but why
	p = newProvider(t, &Home{Err: errors.New("pxe.yaml: line 3: bad")})
	if _, err := p.List(ctx); err == nil || err.Error() != "pxe.yaml: line 3: bad" {
		t.Errorf("invalid: %v", err)
	}
	// none lists nothing
	if ms, err := newProvider(t, &Home{}).List(ctx); err != nil || len(ms) != 0 {
		t.Errorf("none: %v, %v", ms, err)
	}
}

// faulty is a backend that does not answer while down, and whose
// reads of a path fail as often as failRead says.
type faulty struct {
	netboot.Files
	mu       sync.Mutex
	down     bool
	failRead map[string]int
}

func (f *faulty) List(ctx context.Context) ([]backend.Entry, error) {
	if f.down {
		return nil, errors.New("HTTP 503")
	}
	return f.Files.List(ctx)
}

func (f *faulty) Read(ctx context.Context, p string) ([]byte, string, error) {
	f.mu.Lock()
	n := f.failRead[p]
	if n > 0 {
		f.failRead[p] = n - 1
	}
	f.mu.Unlock()
	if f.down || n > 0 {
		return nil, "", errors.New("HTTP 503")
	}
	return f.Files.Read(ctx, p)
}

func TestDescribe(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	putStatus(t, h, func(st *netboot.Status) {
		st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Address: "192.0.2.50", Arch: "amd64", Firmware: "uefi", Platform: "efi", Answer: "rescue",
			Hello: &netboot.Hello{Rescue: "2026.11.01", At: now, BootMAC: "0c:c4:7a:aa:bb:01", MemoryMB: 65536, IPMILan: "IP Address Source : DHCP\nIP Address              : 192.0.2.250\n",
				Interfaces: []netboot.Interface{{MAC: "0c:c4:7a:aa:bb:01", Addresses: []string{"192.0.2.50/24"}}}}}
	})
	d, err := p.Describe(context.Background(), machineOf(t, p, "web-07"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, s := range d.Sections {
		b.WriteString(s.Name + ":")
		for _, it := range s.Items {
			b.WriteString(" " + it.Key)
		}
		b.WriteString("\n")
	}
	want := "PXE: macs site mode jump\nNetwork: uplink ipv4 ipv6 dns\nSeen: by at address boot answer rescue memory bmc\n"
	if b.String() != want {
		t.Errorf("sections:\n%s\nwant\n%s", b.String(), want)
	}
	if v := d.Sections[1].Items[1].Value; v != "192.0.2.17/24 via 192.0.2.1" {
		t.Errorf("ipv4 %v", v)
	}
	if v := d.Sections[2].Items[5].Value; v != "2026.11.01 at 192.0.2.50 (learned at boot), 2026-11-02T09:00:00Z" {
		t.Errorf("rescue %v", v)
	}
	if v := d.Sections[2].Items[7].Value; v != "192.0.2.250" {
		t.Errorf("bmc %v", v)
	}
}

func TestEnableRescue(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	keys := []provider.PublicKey{{Data: "ssh-ed25519 AAAAump ump"}, {Data: "ssh-ed25519 AAAAoleg oleg"}}

	// no PXE server runs: said at once
	h.ReadyWait = time.Minute
	start := time.Now()
	info, err := p.EnableRescue(ctx, web, provider.RescueOptions{Keys: keys, Wait: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second || info.Note != "no PXE server of site fra1 runs (never heard of); one must, for the machine to boot the rescue system (ump pxe serve --site fra1)" {
		t.Errorf("no server: %q", info.Note)
	}
	a, _, err := h.Store.Arm(ctx, "fra1", "web-07")
	if err != nil || a == nil {
		t.Fatalf("arm: %v, %v", a, err)
	}
	if a.ID != info.Arm || a.Mode != "rescue" || a.By != "oleg@laptop" || !a.Until.Equal(now.Add(15*time.Minute)) || strings.Join(a.Keys, "|") != "ssh-ed25519 AAAAump ump|ssh-ed25519 AAAAoleg oleg" {
		t.Errorf("arm %+v", a)
	}
	if info.User != "root" || info.Reboots || info.Until == nil || !info.Until.Equal(a.Until) {
		t.Errorf("info %+v", info)
	}
	if on, err := p.RescueEnabled(ctx, web); err != nil || !on {
		t.Errorf("enabled: %v, %v", on, err)
	}

	// a server that runs and does not take it: said after the wait
	h.ReadyWait = 50 * time.Millisecond
	putStatus(t, h, nil)
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{Keys: keys, Wait: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if info.Note != "no PXE server of site fra1 has taken the arm yet (running: pxe1); see ump pxe status web-07" {
		t.Errorf("not taken: %q", info.Note)
	}
	// one that cannot
	putStatus(t, h, func(st *netboot.Status) {
		st.Arms[info.Arm] = &netboot.ArmState{Cannot: "no rescue image for amd64 yet: it is being fetched", At: now}
	})
	if note := p.servers(ctx, h.Config.Machine("web-07"), info.Arm, 20*time.Millisecond); note != "the PXE servers of site fra1 cannot boot it yet: pxe1: no rescue image for amd64 yet: it is being fetched" {
		t.Errorf("cannot: %q", note)
	}

	// one that takes it: said as soon as it does
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10000 {
			if a, _, _ := h.Store.Arm(ctx, "fra1", "web-07"); a != nil && a.ID != info.Arm {
				putStatus(t, h, func(st *netboot.Status) { st.Arms[a.ID] = &netboot.ArmState{Ready: true, At: now} })
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	h.ReadyWait = time.Minute
	start = time.Now()
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{Keys: keys}); err != nil {
		t.Fatal(err)
	}
	<-done
	if time.Since(start) > 30*time.Second || info.Note != "PXE server pxe1 of site fra1 is ready to boot it" {
		t.Errorf("ready: %q after %s", info.Note, time.Since(start))
	}
	if a, _, _ = h.Store.Arm(ctx, "fra1", "web-07"); !a.Until.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("without a wait the arm is for a day: %v", a.Until)
	}

	// --no-reboot: a day, said at once
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{Keys: keys, NoReboot: true, Wait: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if info.Note != "PXE server pxe1 of site fra1 takes the arm at its next poll" || !info.Until.Equal(now.Add(24*time.Hour)) {
		t.Errorf("no reboot: %q, %v", info.Note, info.Until)
	}

	// an armed installation stands in the way
	if _, err := h.Store.PutArm(ctx, "fra1", &netboot.Arm{ID: "b1b1b1b1b1b1b1b1", Machine: "web-07", Mode: netboot.ModeInstall, By: "ci@runner", Created: now}, versionOf(t, h, "web-07")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EnableRescue(ctx, web, provider.RescueOptions{}); err == nil || err.Error() != "web-07 has an armed install (b1b1b1b1, by ci@runner); disarm it first (ump pxe disarm web-07)" {
		t.Errorf("install armed: %v", err)
	}
	if err := p.DisableRescue(ctx, web); err == nil || !strings.Contains(err.Error(), "an armed install, no rescue boot") {
		t.Errorf("unrescue an install: %v", err)
	}
}

func versionOf(t *testing.T, h *Home, name string) string {
	t.Helper()
	_, v, err := h.Store.Arm(context.Background(), "fra1", name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRescueEnabledAndDisable(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web, lab := machineOf(t, p, "web-07"), machineOf(t, p, "lab-01")

	// the mode rescue: always, and pxe.yaml's to change
	if on, err := p.RescueEnabled(ctx, lab); err != nil || !on {
		t.Errorf("standing: %v, %v", on, err)
	}
	if err := p.DisableRescue(ctx, lab); err == nil || err.Error() != "lab-01 boots UMP's rescue system at every boot (mode: rescue in pxe.yaml); set mode: local there (ump config pxe edit lab-01)" {
		t.Errorf("disable standing: %v", err)
	}

	if on, err := p.RescueEnabled(ctx, web); err != nil || on {
		t.Errorf("nothing armed: %v, %v", on, err)
	}
	info, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true})
	if err != nil {
		t.Fatal(err)
	}
	// taken by a boot: no longer
	if _, err := h.Store.PutOutcome(ctx, "fra1", "web-07", &netboot.Outcome{Arm: info.Arm, Phase: netboot.PhaseBooted, At: now}, ""); err != nil {
		t.Fatal(err)
	}
	if on, err := p.RescueEnabled(ctx, web); err != nil || on {
		t.Errorf("taken: %v, %v", on, err)
	}
	if err := p.DisableRescue(ctx, web); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := h.Store.Arm(ctx, "fra1", "web-07"); a != nil {
		t.Errorf("disarmed: %+v", a)
	}
	// nothing to disarm is no error
	if err := p.DisableRescue(ctx, web); err != nil {
		t.Error(err)
	}
}

// lines is the progress of a wait: the lines said, each signalled.
type lines struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	said chan struct{}
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case l.said <- struct{}{}:
	default:
	}
	return l.buf.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestWaitRescue(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	info, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true})
	if err != nil {
		t.Fatal(err)
	}

	// the machine is refused by one server and booted by the other, and
	// its rescue system reports in once that was said
	progress := &lines{said: make(chan struct{}, 10)}
	go func() {
		putStatusOf(t, h, "pxe0", func(st *netboot.Status) {
			st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Address: "192.0.2.50", Answer: "local", Reason: "4096 MB of memory, and the rescue image needs 8192 MB"}
		})
		putStatus(t, h, func(st *netboot.Status) {
			st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Address: "192.0.2.50", Answer: "rescue", Arm: info.Arm, Signals: []string{"the boot order starts with Boot0003"}}
		})
		for range 3 {
			select {
			case <-progress.said:
			case <-time.After(10 * time.Second):
			}
		}
		putStatus(t, h, func(st *netboot.Status) {
			st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Address: "192.0.2.50", Answer: "rescue", Arm: info.Arm,
				Hello: &netboot.Hello{Arm: info.Arm, BootMAC: "0c:c4:7a:aa:bb:01", HostKeys: []string{"ssh-ed25519 AAAAhost"},
					Interfaces: []netboot.Interface{{MAC: "0c:c4:7a:aa:bb:02", Addresses: []string{"10.9.9.9/24"}}, {MAC: "0c:c4:7a:aa:bb:01", Addresses: []string{"192.0.2.51/24"}}}}}
		})
	}()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	addr, keys, err := p.WaitRescue(wctx, web, info, progress)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "192.0.2.51" || strings.Join(keys, "|") != "ssh-ed25519 AAAAhost" {
		t.Errorf("got %s, %v", addr, keys)
	}
	want := "PXE server pxe0 sent web-07 to its disk: 4096 MB of memory, and the rescue image needs 8192 MB\n" +
		"web-07 boots UMP's rescue system from PXE server pxe1 at 192.0.2.50.\n" +
		"PXE server pxe1: the boot order starts with Boot0003\n"
	if progress.String() != want {
		t.Errorf("progress:\n%s\nwant\n%s", progress.String(), want)
	}

	// nobody reports in: the wait ends with its context, and the arm goes
	putStatus(t, h, nil)
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err != nil {
		t.Fatal(err)
	}
	wctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, _, err = p.WaitRescue(wctx, web, info, progress)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the rescue system of web-07 did not report in; the arm is gone") {
		t.Errorf("timeout: %v", err)
	}
	if a, _, _ := h.Store.Arm(ctx, "fra1", "web-07"); a != nil {
		t.Errorf("the arm stayed: %+v", a)
	}

	// a read of the arm that fails once is no disarm
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err != nil {
		t.Fatal(err)
	}
	f := &faulty{Files: h.Store.Files, failRead: map[string]int{netboot.ArmPath("fra1", "web-07"): 1}}
	h.Store.Files = f
	wctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	progress = &lines{said: make(chan struct{}, 10)}
	_, _, err = p.WaitRescue(wctx, web, info, progress)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(progress.String(), "The backend does not answer (HTTP 503); asking again.\n") {
		t.Errorf("a failed read of the arm: %v\n%s", err, progress.String())
	}
	h.Store.Files = f.Files

	// someone disarms meanwhile
	if info, err = p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Store.Disarm(ctx, "fra1", "web-07"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.WaitRescue(ctx, web, info, progress); err == nil || !strings.Contains(err.Error(), "was disarmed or replaced meanwhile") {
		t.Errorf("disarmed: %v", err)
	}
}

func TestPreflight(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	lab := machineOf(t, p, "lab-01")
	ws, err := p.Preflight(ctx, lab)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"site fra1 has no site.json, so its PXE servers boot nothing: ump config pxe apply",
		"no PXE server of site fra1 runs (ump pxe serve --site fra1): the rescue system of pxe/lab-01 boots from one",
	}
	if strings.Join(ws, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings:\n%s", strings.Join(ws, "\n"))
	}
	// the mode rescue would boot the rescue system again, not a new system
	if err := p.CheckInstall(ctx, lab); err == nil || err.Error() != "pxe/lab-01 boots UMP's rescue system at every boot (mode: rescue in pxe.yaml), so a new system would never start: set mode: local first (ump config pxe edit lab-01)" {
		t.Errorf("an installation in mode rescue: %v", err)
	}
	if err := p.CheckInstall(ctx, machineOf(t, p, "web-07")); err != nil {
		t.Errorf("an installation in mode local: %v", err)
	}
	d, err := netboot.Render(h.Config, "fra1", "https://storage.example/rescue", func(m *netboot.Machine) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.PutSite(ctx, d); err != nil {
		t.Fatal(err)
	}
	putStatus(t, h, nil)
	if ws, err := p.Preflight(ctx, machineOf(t, p, "web-07")); err != nil || len(ws) != 0 {
		t.Errorf("nothing in the way: %v, %v", ws, err)
	}
}

func TestUplink(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	u, err := p.Uplink(context.Background(), machineOf(t, p, "web-07"))
	if err != nil {
		t.Fatal(err)
	}
	want := profile.UplinkSpec{MAC: "0c:c4:7a:aa:bb:01", IPv4: profile.UplinkAddress{Mode: "static", Address: "192.0.2.17/24", Gateway: "192.0.2.1"},
		IPv6: profile.UplinkAddress{Mode: "none"}, DNS: []string{"192.0.2.53"}}
	if u.MAC != want.MAC || u.IPv4 != want.IPv4 || u.IPv6 != want.IPv6 || strings.Join(u.DNS, ",") != "192.0.2.53" || u.DNSFromDHCP {
		t.Errorf("uplink %+v", u)
	}
	if _, err := p.Uplink(context.Background(), provider.Machine{Provider: "pxe", ID: "gone"}); err == nil || err.Error() != "pxe/gone is no machine of pxe.yaml" {
		t.Errorf("gone: %v", err)
	}
	for _, c := range []struct {
		m          netboot.Machine
		ipv4, ipv6 string
	}{
		{netboot.Machine{Address: "192.0.2.30"}, "192.0.2.30", ""},
		{netboot.Machine{Address: "2001:db8::30"}, "", "2001:db8::30"},
		{netboot.Machine{Network: profile.UplinkSpec{IPv6: profile.UplinkAddress{Mode: "static", Address: "2001:db8::17/64"}}, Address: "192.0.2.30"}, "192.0.2.30", "2001:db8::17"},
		{netboot.Machine{Network: profile.UplinkSpec{IPv4: profile.UplinkAddress{Mode: "dhcp"}}}, "", ""},
	} {
		if v4, v6 := Addresses(&c.m); v4 != c.ipv4 || v6 != c.ipv6 {
			t.Errorf("%+v: %s %s", c.m, v4, v6)
		}
	}
}
