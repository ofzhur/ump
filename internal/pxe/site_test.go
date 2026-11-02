package pxe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
)

// testKeys resolves a machine's rescue keys as the CLI does: UMP's own
// first, then its own.
func testKeys(m *Machine) ([]string, error) {
	keys := []string{"ssh-ed25519 AAAAump ump"}
	for _, k := range m.RescueKeys {
		if k == "broken" {
			return nil, errors.New("no key broken")
		}
		keys = append(keys, "ssh-ed25519 AAAA"+k+" "+k)
	}
	return keys, nil
}

func TestRender(t *testing.T) {
	c := parse(t, sample)
	d, err := Render(c, "fra1", "https://storage.example.net/rescue", testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if d.Format != SiteFormat || d.Site != "fra1" || d.Unknown != UnknownIgnore || d.Image != "https://storage.example.net/rescue" || len(d.Hash) != 16 {
		t.Errorf("site.json: %+v", d)
	}
	if len(d.Machines) != 2 || d.Machines[0].Name != "db-01" || d.Machines[1].Name != "web-07" {
		t.Fatalf("machines of fra1, by name: %+v", d.Machines)
	}
	db := d.Machines[0]
	if strings.Join(db.MACs, ",") != "0c:c4:7a:aa:bb:10,0c:c4:7a:aa:bb:11" || db.Mode != ModeLocal || db.Arch != ArchAMD64 ||
		strings.Join(db.Keys, "|") != "ssh-ed25519 AAAAump ump|ssh-ed25519 AAAAlaptop laptop" {
		t.Errorf("db-01: %+v", db)
	}
	if d.Lookup("0c:c4:7a:aa:bb:11").Name != "db-01" || d.Lookup("0c:c4:7a:aa:bb:99") != nil || d.Machine("web-07").Firmware != FirmwareUEFI {
		t.Error("lookups")
	}
	lab, err := Render(c, "lab", "https://storage.example.net/rescue", testKeys)
	if err != nil || lab.Unknown != UnknownLocal || len(lab.Machines) != 1 || lab.Machines[0].Mode != ModeRescue || lab.Machines[0].Console != ConsoleNone {
		t.Errorf("lab: %+v %v", lab, err)
	}
	// the same pxe.yaml renders the same, byte for byte
	again, _ := Render(parse(t, sample), "fra1", "https://storage.example.net/rescue", testKeys)
	if string(again.JSON()) != string(d.JSON()) {
		t.Error("two renderings differ")
	}
	parsed, err := ParseSite(d.JSON())
	if err != nil || parsed.Hash != d.Hash || len(parsed.Machines) != 2 {
		t.Errorf("parsed: %+v %v", parsed, err)
	}
	// a change changes the hash
	c.Machines["web-07"].Mode = ptr(ModeRescue)
	changed, _ := Render(c, "fra1", "https://storage.example.net/rescue", testKeys)
	if changed.Hash == d.Hash {
		t.Error("a change kept the hash")
	}
	c.Machines["web-07"].RescueKeys = []string{"broken"}
	if _, err := Render(c, "fra1", "x", testKeys); err == nil || err.Error() != "web-07: no key broken" {
		t.Errorf("a key that does not resolve: %v", err)
	}
	empty, err := Render(parse(t, ""), DefaultSite, "x", testKeys)
	if err != nil || len(empty.Machines) != 0 || !strings.Contains(string(empty.JSON()), `"machines": []`) {
		t.Errorf("no machines: %s %v", empty.JSON(), err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestParseSiteRefuses(t *testing.T) {
	d, _ := Render(parse(t, sample), "fra1", "x", testKeys)
	newer := *d
	newer.Format = SiteFormat + 1
	newer.Hash = newer.hash()
	if _, err := ParseSite(newer.JSON()); err == nil || !strings.Contains(err.Error(), "newer than this ump's") {
		t.Errorf("newer: %v", err)
	}
	edited := strings.Replace(string(d.JSON()), `"mode": "local"`, `"mode": "rescue"`, 1)
	if _, err := ParseSite([]byte(edited)); err == nil || !strings.Contains(err.Error(), "changed by hand") {
		t.Errorf("edited: %v", err)
	}
	if _, err := ParseSite([]byte(`{"site": "x"}`)); err == nil || !strings.Contains(err.Error(), "no format") {
		t.Errorf("no format: %v", err)
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	s := Store{Files: &backend.Local{Dir: t.TempDir()}}
	for _, p := range []string{SitePath("fra1"), ArmPath("fra1", "web-07"), OutcomePath("fra1", "web-07"), StatusPath("fra1", "pxe1"), LogPath("fra1", "web-07")} {
		if !strings.HasPrefix(p, backend.PXEDir) || !backend.ValidPath(p) {
			t.Errorf("%s is no path of .pxe/", p)
		}
	}
	d, _ := Render(parse(t, sample), "fra1", "x", testKeys)
	if changed, err := s.PutSite(ctx, d); !changed || err != nil {
		t.Fatalf("first rendering: %v %v", changed, err)
	}
	if changed, err := s.PutSite(ctx, d); changed || err != nil {
		t.Errorf("the same again: %v %v", changed, err)
	}
	if got, err := s.Site(ctx, "fra1"); err != nil || got.Hash != d.Hash {
		t.Errorf("read back: %+v %v", got, err)
	}
	if got, err := s.Site(ctx, "lab"); got != nil || err != nil {
		t.Errorf("no site.json: %+v %v", got, err)
	}

	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	a := &Arm{ID: NewArmID(), Machine: "web-07", Mode: ModeRescue, By: "oleg@laptop", Created: now, Until: now.Add(15 * time.Minute)}
	if len(a.ID) != 16 || ShortID(a.ID) != a.ID[:8] || NewArmID() == a.ID {
		t.Errorf("arm IDs: %s", a.ID)
	}
	v, err := s.PutArm(ctx, "fra1", a, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutArm(ctx, "fra1", a, ""); !errors.Is(err, backend.ErrConflict) {
		t.Errorf("a second arm over the first: %v", err)
	}
	got, gv, err := s.Arm(ctx, "fra1", "web-07")
	if err != nil || gv != v || got.ID != a.ID || !got.Until.Equal(a.Until) {
		t.Errorf("arm read back: %+v %s %v", got, gv, err)
	}

	// two servers claim the arm: one gets it
	claim := func(daemon string) error {
		_, err := s.PutOutcome(ctx, "fra1", "web-07", &Outcome{Arm: a.ID, Phase: PhaseBooted, Daemon: daemon, At: now}, "")
		return err
	}
	if err := claim("pxe1"); err != nil {
		t.Fatal(err)
	}
	if err := claim("pxe2"); !errors.Is(err, backend.ErrConflict) {
		t.Errorf("the second claim: %v", err)
	}
	o, err := s.UpdateOutcome(ctx, "fra1", "web-07", func(o *Outcome) (*Outcome, error) {
		if o == nil || o.Daemon != "pxe1" {
			t.Errorf("the outcome to change: %+v", o)
		}
		n := *o
		n.Phase = PhaseInstalled
		return &n, nil
	})
	if err != nil || o.Phase != PhaseInstalled {
		t.Errorf("update: %+v %v", o, err)
	}
	if _, err := s.UpdateOutcome(ctx, "fra1", "web-07", func(*Outcome) (*Outcome, error) { return nil, errors.New("no") }); err == nil || err.Error() != "no" {
		t.Errorf("a change that writes nothing: %v", err)
	}

	st := &Status{Daemon: "pxe1", Site: "fra1", Version: "v0.8.0", Started: now, Updated: now, SiteHash: d.Hash,
		Machines: map[string]*Seen{"0c:c4:7a:aa:bb:01": {Name: "web-07", At: now, Answer: ModeRescue}}}
	if err := s.PutStatus(ctx, st); err != nil {
		t.Fatal(err)
	}
	st.Updated = now.Add(time.Minute)
	if err := s.PutStatus(ctx, st); err != nil {
		t.Fatalf("the heartbeat over the status: %v", err)
	}
	s.Files.Write(ctx, ArmPath("lab", "bench"), []byte("{not json"), "")

	sn, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sn.Sites["fra1"].Hash != d.Hash || sn.Arm("fra1", "web-07").ID != a.ID || sn.Outcome("fra1", "web-07").Phase != PhaseInstalled {
		t.Errorf("snapshot: %+v", sn)
	}
	if len(sn.Statuses["fra1"]) != 1 || !sn.Statuses["fra1"][0].Updated.Equal(now.Add(time.Minute)) || sn.Statuses["fra1"][0].Machines["0c:c4:7a:aa:bb:01"].Name != "web-07" {
		t.Errorf("statuses: %+v", sn.Statuses)
	}
	if sn.Arm("lab", "bench") != nil || sn.Errors[ArmPath("lab", "bench")] == nil {
		t.Errorf("an arm that does not parse: %+v %v", sn.Arm("lab", "bench"), sn.Errors)
	}
	if sn.Versions[ArmPath("fra1", "web-07")] != v {
		t.Errorf("versions: %v", sn.Versions)
	}
	only, err := s.Load(ctx, "lab")
	if err != nil || len(only.Sites) != 0 || len(only.Arms["fra1"]) != 0 {
		t.Errorf("one site: %+v %v", only, err)
	}

	s.Files.Write(ctx, LogPath("fra1", "web-07"), []byte("log\n"), "")
	if err := s.Forget(ctx, "fra1", "web-07"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ArmPath("fra1", "web-07"), OutcomePath("fra1", "web-07"), LogPath("fra1", "web-07")} {
		if _, v, _ := s.Files.Read(ctx, p); v != "" {
			t.Errorf("%s is still there", p)
		}
	}
	if sites, err := s.Sites(ctx); err != nil || strings.Join(sites, ",") != "fra1" {
		t.Errorf("sites: %v %v", sites, err)
	}
	if err := s.DeleteSite(ctx, "fra1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Site(ctx, "fra1"); got != nil {
		t.Error("site.json is still there")
	}
}

// racing is a backend where another writer writes a path once, between
// a writer's read of it and its write (meanwhile).
type racing struct {
	*backend.Local
	path      string
	meanwhile func()
}

func (r *racing) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	if p == r.path && r.meanwhile != nil {
		m := r.meanwhile
		r.meanwhile = nil
		m()
	}
	return r.Local.Write(ctx, p, data, ifVersion)
}

// TestStoreConflicts: a compare-and-swap that another writer came
// before runs again on what that one wrote.
func TestStoreConflicts(t *testing.T) {
	ctx := context.Background()
	r := &racing{Local: &backend.Local{Dir: t.TempDir()}}
	s := Store{Files: r}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	// an outcome changed meanwhile: the change runs again on it
	if _, err := s.PutOutcome(ctx, "fra1", "web-07", &Outcome{Arm: "a1", Phase: PhaseBooted, Daemon: "pxe1", At: now}, ""); err != nil {
		t.Fatal(err)
	}
	r.path = OutcomePath("fra1", "web-07")
	r.meanwhile = func() {
		s.UpdateOutcome(ctx, "fra1", "web-07", func(o *Outcome) (*Outcome, error) {
			n := *o
			n.Daemon = "pxe2"
			return &n, nil
		})
	}
	var seen []string
	o, err := s.UpdateOutcome(ctx, "fra1", "web-07", func(o *Outcome) (*Outcome, error) {
		seen = append(seen, o.Daemon)
		n := *o
		n.Reason = "seen by " + o.Daemon
		return &n, nil
	})
	if err != nil || strings.Join(seen, ",") != "pxe1,pxe2" || o.Reason != "seen by pxe2" {
		t.Errorf("update over a change: %+v %v, the change saw %v", o, err, seen)
	}
	if got, _, _ := s.Outcome(ctx, "fra1", "web-07"); got.Daemon != "pxe2" || got.Reason != "seen by pxe2" {
		t.Errorf("the outcome: %+v", got)
	}

	// site.json rendered meanwhile by another ump: this rendering wins
	c := parse(t, sample)
	mine, _ := Render(c, "fra1", "mine", testKeys)
	theirs, _ := Render(c, "fra1", "theirs", testKeys)
	r.path = SitePath("fra1")
	r.meanwhile = func() { s.PutSite(ctx, theirs) }
	if changed, err := s.PutSite(ctx, mine); !changed || err != nil {
		t.Errorf("a rendering over another: %v %v", changed, err)
	}
	if got, _ := s.Site(ctx, "fra1"); got.Hash != mine.Hash {
		t.Errorf("site.json: %s, want %s", got.Hash, mine.Hash)
	}
}

func TestHelloAddress(t *testing.T) {
	h := &Hello{BootMAC: "0c:c4:7a:aa:bb:11", Interfaces: []Interface{
		{Name: "eno1", MAC: "0c:c4:7a:aa:bb:10", Addresses: []string{"10.1.0.5/24"}},
		{Name: "eno2", MAC: "0c:c4:7a:aa:bb:11", Addresses: []string{"2001:db8::5/64", "192.0.2.17/24"}},
	}}
	if got := h.Address(); got != "192.0.2.17" {
		t.Errorf("the boot interface's: %s", got)
	}
	h.BootMAC = "0c:c4:7a:aa:bb:99"
	if got := h.Address(); got != "10.1.0.5" {
		t.Errorf("another's: %s", got)
	}
	if got := (&Hello{}).Address(); got != "" {
		t.Errorf("none: %s", got)
	}
}
