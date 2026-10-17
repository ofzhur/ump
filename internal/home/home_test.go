package home

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ofzhur/ump/internal/backend"
)

// Two machines share a backend: each has its mirror. A file both change
// is a conflict to be named, except the listing cache, where the newer
// listing wins without a word.
func TestPushConflictsAndTheCache(t *testing.T) {
	ctx := context.Background()
	b := &backend.Local{Dir: t.TempDir()}
	a := &Mirror{Backend: b, Dir: t.TempDir()}
	c := &Mirror{Backend: b, Dir: t.TempDir()}
	write := func(m *Mirror, path, data string) {
		t.Helper()
		p := filepath.Join(m.Dir, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const snap = "cache/listings/x/acct/20261017T120000Z.json"

	write(a, "config.yaml", "one")
	write(a, snap, "seen 12:00")
	if _, err := a.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Pull(ctx); err != nil {
		t.Fatal(err)
	}

	// both change both files; a pushes first
	write(a, "config.yaml", "a's")
	write(a, snap, "seen 12:01 by a")
	write(c, "config.yaml", "c's")
	write(c, snap, "seen 12:02 by c")
	if _, err := a.Push(ctx); err != nil {
		t.Fatal(err)
	}
	changes, err := c.Push(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var conflicts []string
	for _, ch := range changes {
		if ch.Kind == "conflict" {
			conflicts = append(conflicts, ch.Path)
		}
	}
	if len(conflicts) != 1 || conflicts[0] != "config.yaml" {
		t.Errorf("conflicts %q, want config.yaml alone", conflicts)
	}
	if data, _, _ := b.Read(ctx, snap); string(data) != "seen 12:02 by c" {
		t.Errorf("the cache holds %q, want the newer listing", data)
	}
	if data, _, _ := b.Read(ctx, "config.yaml"); string(data) != "a's" {
		t.Errorf("config.yaml is %q, want a's kept", data)
	}
	// c's next push does not stumble over its own cache write
	write(c, snap, "seen 12:03 by c")
	if changes, err := c.Push(ctx); err != nil || len(changes) != 2 {
		t.Errorf("next push: %v %+v", err, changes)
	}
}
