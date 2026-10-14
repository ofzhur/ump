package state

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

// Local is a file on this machine: the default state under
// ~/.local/state/ump, or any path (file://...), for example one in the
// fleet's git repository. The version tag is the content's hash; a
// write checks it under a short-lived file lock. The per-server locks
// are directories next to the file, made atomically.
type Local struct {
	Path string
}

func (l *Local) String() string { return "file://" + l.Path }

func (l *Local) locksDir() string { return l.Path + ".locks" }

func (l *Local) Read(ctx context.Context) ([]byte, string, error) {
	data, err := os.ReadFile(l.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return data, hashVersion(data), nil
}

func (l *Local) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return "", err
	}
	release, err := l.writeLock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	current, version, err := l.Read(ctx)
	if err != nil {
		return "", err
	}
	if version != ifVersion {
		_ = current
		return "", ErrConflict
	}
	tmp := l.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, l.Path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hashVersion(data), nil
}

// writeLock serialises writers of the file itself for the moment of
// the compare-and-write; it is not the per-server lock.
func (l *Local) writeLock(ctx context.Context) (func(), error) {
	path := l.Path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
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
			return nil, errors.New("another ump is writing the state (stale " + path + "?)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

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

func hashVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func unlockKey(name string) string {
	return strings.NewReplacer("%2F", "/", "%5C", "\\", "%3A", ":", "%20", " ").Replace(name)
}
