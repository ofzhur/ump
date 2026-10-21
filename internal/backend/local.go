package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local is a directory on this machine: the home itself when no backend
// is configured, or any directory (file://...), for example one in the
// fleet's git repository. A file's version is its content's hash; a
// write checks it under a short-lived lock. The advisory locks are
// directories under .locks/, made atomically.
type Local struct {
	Dir string
}

func (l *Local) String() string { return "file://" + l.Dir }

// Excluded says which names of a home directory are never files of the
// backend: locks, temporary files, the mirror's bookkeeping, and the
// pointer to the backend with its own secrets (backend.yaml and
// backend.*, next to the local configuration, which is the home when
// the state is local).
func Excluded(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasPrefix(rel, ".locks") || !strings.Contains(rel, "/") && strings.HasPrefix(rel, "backend.") || strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".lock") || base == ".mirror.json" || base == ".DS_Store"
}

func (l *Local) file(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.Dir, filepath.FromSlash(p)), nil
}

func (l *Local) Read(ctx context.Context, p string) ([]byte, string, error) {
	f, err := l.file(p)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(f)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return data, HashVersion(data), nil
}

func (l *Local) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	f, err := l.file(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return "", err
	}
	release, err := l.writeLock(ctx, f)
	if err != nil {
		return "", err
	}
	defer release()
	_, version, err := l.Read(ctx, p)
	if err != nil {
		return "", err
	}
	if version != ifVersion {
		return "", ErrConflict
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, f); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return HashVersion(data), nil
}

func (l *Local) Delete(ctx context.Context, p string) error {
	f, err := l.file(p)
	if err != nil {
		return err
	}
	if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) List(ctx context.Context) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(l.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(l.Dir, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && Excluded(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if Excluded(rel) || !ValidPath(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, Entry{Path: rel, Version: HashVersion(data)})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// writeLock serialises writers of one file for the moment of the
// compare-and-write; it is not an advisory lock.
func (l *Local) writeLock(ctx context.Context, f string) (func(), error) {
	path := f + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			h.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > time.Minute {
			os.Remove(path) // a writer that died
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("another ump is writing " + f + " (stale " + path + "?)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (l *Local) locksDir() string { return filepath.Join(l.Dir, ".locks") }

func (l *Local) Lock(ctx context.Context, key string, info LockInfo) error {
	dir := filepath.Join(l.locksDir(), lockKey(key))
	if err := os.MkdirAll(l.locksDir(), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		held := LockInfo{Key: key}
		if data, err := os.ReadFile(filepath.Join(dir, "info.json")); err == nil {
			json.Unmarshal(data, &held)
		}
		return &ErrLocked{Key: key, Info: held}
	}
	data, _ := json.Marshal(info)
	return os.WriteFile(filepath.Join(dir, "info.json"), data, 0o600)
}

func (l *Local) Unlock(ctx context.Context, key string) error {
	err := os.RemoveAll(filepath.Join(l.locksDir(), lockKey(key)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (l *Local) Locks(ctx context.Context) ([]LockInfo, error) {
	entries, err := os.ReadDir(l.locksDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LockInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info := LockInfo{Key: unlockKey(e.Name())}
		if data, err := os.ReadFile(filepath.Join(l.locksDir(), e.Name(), "info.json")); err == nil {
			json.Unmarshal(data, &info)
		}
		out = append(out, info)
	}
	return out, nil
}

// HashVersion is the version tag of content: the first 16 hex characters
// of its SHA-256 (what sha256sum | cut -c1-16 gives on a remote shell).
func HashVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
