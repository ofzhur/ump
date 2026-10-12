package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

func machines(status string, names ...string) []provider.Machine {
	var ms []provider.Machine
	for _, n := range names {
		ms = append(ms, provider.Machine{Provider: "fake", Account: "acc", ID: "id-" + n, Name: n, Status: status, Labels: map[string]string{}})
	}
	return ms
}

func TestStore(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	s := Open(cfgPath)
	if !strings.HasPrefix(s.Dir(), filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "listings")) {
		t.Errorf("store dir %s", s.Dir())
	}
	if Open(filepath.Join(t.TempDir(), "other.yaml")).Dir() == s.Dir() {
		t.Error("different configs share a store")
	}
	if snap, err := s.Latest("acc"); snap != nil || err != nil {
		t.Errorf("empty store: %v %v", snap, err)
	}

	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	snap, created, err := s.Record("acc", "fake", machines("running", "b", "a"), t0)
	if err != nil || !created || snap.ListedAt != t0 || len(snap.Machines) != 2 || snap.Machines[0].Name != "a" {
		t.Fatalf("first record: %+v %v %v", snap, created, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "config-path")); err != nil {
		t.Errorf("config-path marker missing: %v", err)
	}

	// same state one hour later: no new snapshot, seen_at moves
	t1 := t0.Add(time.Hour)
	ms := machines("running", "a", "b")
	ms[0].Labels = nil // nil and empty labels are the same state
	snap, created, err = s.Record("acc", "fake", ms, t1)
	if err != nil || created || snap.ListedAt != t0 || snap.SeenAt != t1 {
		t.Fatalf("unchanged record: %+v %v %v", snap, created, err)
	}
	files, _ := s.files("acc")
	if len(files) != 1 {
		t.Errorf("files after unchanged record: %v", files)
	}

	// a change creates a second snapshot
	t2 := t0.Add(2 * time.Hour)
	if _, created, err = s.Record("acc", "fake", machines("off", "a", "b"), t2); err != nil || !created {
		t.Fatalf("changed record: %v %v", created, err)
	}
	latest, _ := s.Latest("acc")
	if latest.ListedAt != t2 || latest.Machines[0].Status != "off" {
		t.Errorf("latest: %+v", latest)
	}
	if snap, _ := s.AsOf("acc", t0.Add(90*time.Minute)); snap == nil || snap.ListedAt != t0 {
		t.Errorf("AsOf between: %+v", snap)
	}
	if snap, _ := s.AsOf("acc", t2); snap == nil || snap.ListedAt != t2 {
		t.Errorf("AsOf exact: %+v", snap)
	}
	if snap, _ := s.AsOf("acc", t0.Add(-time.Minute)); snap != nil {
		t.Errorf("AsOf before history: %+v", snap)
	}
	if oldest, ok := s.Oldest("acc"); !ok || oldest != t0 {
		t.Errorf("Oldest: %v %v", oldest, ok)
	}
	if _, ok := s.Oldest("none"); ok {
		t.Error("Oldest of unknown account")
	}

	// pruning keeps the newest MaxSnapshots
	for i := 0; i < MaxSnapshots+5; i++ {
		if _, _, err := s.Record("acc", "fake", machines("s"+string(rune('a'+i%26))+string(rune('a'+i/26)), "a"), t2.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	files, _ = s.files("acc")
	if len(files) != MaxSnapshots {
		t.Errorf("%d files after pruning, want %d", len(files), MaxSnapshots)
	}

	// a corrupt file reports its path and the remedy
	os.MkdirAll(filepath.Join(s.Dir(), "bad"), 0o700)
	os.WriteFile(filepath.Join(s.Dir(), "bad", "20260101T000000Z.json"), []byte("{"), 0o600)
	if _, err := s.Latest("bad"); err == nil || !strings.Contains(err.Error(), "deleting the cache is safe") {
		t.Errorf("corrupt file: %v", err)
	}
}

func TestDiff(t *testing.T) {
	before := []provider.Machine{
		{Provider: "fake", Account: "acc", ID: "1", Name: "web-01", Status: "running", IPv4: "203.0.113.10", Labels: map[string]string{"env": "prod"}},
		{Provider: "fake", Account: "acc", ID: "2", Name: "db-01", Status: "running"},
		{Provider: "fake", Account: "acc", ID: "3", Name: "old-01", Status: "running"},
	}
	after := []provider.Machine{
		{Provider: "fake", Account: "acc", ID: "1", Name: "web-01", Status: "off", IPv4: "203.0.113.11", Labels: map[string]string{"env": "prod"}},
		{Provider: "fake", Account: "acc", ID: "2", Name: "db-01", Status: "running", Labels: map[string]string{}},
		{Provider: "fake", Account: "acc", ID: "4", Name: "new-01", Status: "initializing"},
	}
	changes, unchanged := Diff(before, after)
	if unchanged != 1 {
		t.Errorf("unchanged = %d", unchanged)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Change+" "+c.Machine.Name+" "+strings.Join(c.Fields, ","))
	}
	want := []string{"added new-01 ", "removed old-01 ", "changed web-01 ipv4,status"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("changes: %v, want %v", got, want)
	}
	if changes[2].Before == nil || changes[2].Before.Status != "running" || changes[2].Machine.Status != "off" {
		t.Errorf("before/after: %+v", changes[2])
	}
	if FieldValue(before[0], "labels") != "env=prod" || FieldValue(after[1], "labels") != "" {
		t.Error("label rendering")
	}
	if !Equal(before, before) || Equal(before, after) || Equal(before, before[:2]) {
		t.Error("Equal")
	}
}
