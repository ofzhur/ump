package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// remote is a local backend that answers like one over the network,
// a little later.
type remote struct{ *backend.Local }

func (r remote) Read(ctx context.Context, p string) ([]byte, string, error) {
	time.Sleep(time.Millisecond)
	return r.Local.Read(ctx, p)
}

func (r remote) List(ctx context.Context) ([]backend.Entry, error) {
	time.Sleep(time.Millisecond)
	return r.Local.List(ctx)
}

// Several ump processes on one machine share a mirror: each writes
// state.yaml to the backend directly and then pulls, as a reinstall does,
// and pushes what it changed. Whatever the interleaving, the mirror ends
// up as the backend is, with no conflict to report.
func TestMirrorSharedByProcesses(t *testing.T) {
	ctx := context.Background()
	b := remote{&backend.Local{Dir: t.TempDir()}}
	dir := t.TempDir()
	if _, err := b.Write(ctx, "state.yaml", []byte("round 0\n"), ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			m := &Mirror{Backend: b, Dir: dir}
			for i := 0; i < 15; i++ {
				// the direct write of state.Store.Update: read, write over
				// that version, again when someone came in between
				for {
					_, version, err := b.Read(ctx, "state.yaml")
					if err != nil {
						t.Error(err)
						return
					}
					_, err = b.Write(ctx, "state.yaml", []byte(fmt.Sprintf("process %d round %d\n", p, i)), version)
					if err == nil {
						break
					}
					if !errors.Is(err, backend.ErrConflict) {
						t.Error(err)
						return
					}
				}
				if _, _, err := m.Pull(ctx); err != nil {
					t.Error(err)
					return
				}
				if _, err := m.Push(ctx); err != nil {
					t.Error(err)
					return
				}
			}
		}(p)
	}
	wg.Wait()
	m := &Mirror{Backend: b, Dir: dir}
	if _, _, err := m.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	changes, err := m.Push(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range changes {
		t.Errorf("after the processes: %s %s, want nothing to push", ch.Path, ch.Kind)
	}
	remote, _, _ := b.Read(ctx, "state.yaml")
	if local, _ := os.ReadFile(filepath.Join(dir, "state.yaml")); string(local) != string(remote) {
		t.Errorf("the mirror holds %q, the backend %q", local, remote)
	}
}
