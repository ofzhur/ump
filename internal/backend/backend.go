// Package backend stores UMP's home, the directory with everything ump
// keeps (the configuration, the secrets, the profiles, the keys, the
// state, the listing cache), somewhere shared: a directory on this or
// another machine, an S3 bucket, an HTTP service, Consul or Vault. A
// backend is a flat store of files by path, each read and written with
// a version tag so that a write over a changed file is refused, plus
// advisory locks by name, which ump takes per server.
package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPClient is the client type the backends take (tests hand one in).
type HTTPClient = *http.Client

// ErrConflict says a file changed between the read and the write.
var ErrConflict = errors.New("changed since it was read")

// ErrLocked says another operation holds the lock.
type ErrLocked struct {
	Key  string
	Info LockInfo
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("%s is locked by %s since %s (%s); ump config state unlock %s forces it", e.Key, e.Info.Who, e.Info.Since.Format(time.RFC3339), e.Info.What, e.Key)
}

// LockInfo says who holds a lock, and for what.
type LockInfo struct {
	Key   string    `json:"key" yaml:"key"`
	Who   string    `json:"who" yaml:"who"`
	What  string    `json:"what" yaml:"what"`
	Since time.Time `json:"since" yaml:"since"`
}

// Entry is a file the backend holds.
type Entry struct {
	Path    string
	Version string
}

// Backend stores files by path.
type Backend interface {
	// Read returns a file and its version tag; no file is empty bytes
	// and an empty tag.
	Read(ctx context.Context, path string) ([]byte, string, error)
	// Write stores a file when its stored version is still ifVersion
	// (an empty one means: not there yet), and returns the new tag;
	// ErrConflict otherwise.
	Write(ctx context.Context, path string, data []byte, ifVersion string) (string, error)
	// Delete removes a file; a missing one is no error.
	Delete(ctx context.Context, path string) error
	// List returns every file with its version.
	List(ctx context.Context) ([]Entry, error)
	// Lock takes the advisory lock named key; *ErrLocked when held.
	Lock(ctx context.Context, key string, info LockInfo) error
	// Unlock releases it.
	Unlock(ctx context.Context, key string) error
	// Locks lists the locks held.
	Locks(ctx context.Context) ([]LockInfo, error)
	// String says where the backend is, without secrets.
	String() string
}

// lockKey makes a lock key safe for a file or object name.
func lockKey(key string) string {
	return strings.NewReplacer("/", "%2F", "\\", "%5C", ":", "%3A", " ", "%20").Replace(key)
}

func unlockKey(name string) string {
	return strings.NewReplacer("%2F", "/", "%5C", "\\", "%3A", ":", "%20", " ").Replace(name)
}

// cleanPath rejects paths that leave the home.
func cleanPath(p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" || strings.Contains(p, "..") || strings.HasPrefix(p, ".") && !strings.HasPrefix(p, ".published") {
		return "", fmt.Errorf("bad path %q", p)
	}
	return p, nil
}
