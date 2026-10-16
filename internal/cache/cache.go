// Package cache keeps UMP's disposable listing cache under cache/ in
// its home. Every successful listing of an account is recorded as a
// snapshot, but a new file is written only when the listing changed, so
// the store holds the history of observed states: `ump get servers
// --diff` compares the current listing with the previous one (or with the
// state as of an earlier time), and --cached answers without calling the
// providers. A listing confirmed less than the cache TTL ago is served
// without calling the provider (Fresh), unless a command changed one of
// the account's servers since (Invalidate). Deleting the cache is always
// safe.
//
// Layout:
//
//	<home>/cache/listings/<config id>/config-path
//	<home>/cache/listings/<config id>/<account>/<listed at, UTC>.json
//	<home>/cache/listings/<config id>/<account>/invalidated
//
// The config id is derived from the configuration file's path, because
// account names are only unique within one file.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

const (
	// MaxSnapshots is how many observed states are kept per account.
	MaxSnapshots = 50
	nameLayout   = "20060102T150405Z"
)

// Snapshot is one observed state of an account's listing.
type Snapshot struct {
	Account  string `json:"account"`
	Provider string `json:"provider"`
	// ListedAt is when this state was first observed.
	ListedAt time.Time `json:"listed_at"`
	// SeenAt is when this state was last confirmed by a listing.
	SeenAt   time.Time          `json:"seen_at"`
	Machines []provider.Machine `json:"machines"`
}

// Dir is the cache directory: cache/ in UMP's home, so that a home
// behind a backend carries its listings along.
func Dir() string {
	return filepath.Join(config.UserConfigDir(), "cache")
}

// Store holds the listing snapshots of one configuration file.
type Store struct {
	dir        string
	configPath string
}

// Open returns the listing store for a configuration file. Nothing is
// created until the first snapshot is recorded.
func Open(configPath string) *Store {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	sum := sha256.Sum256([]byte(abs))
	return &Store{dir: filepath.Join(Dir(), "listings", hex.EncodeToString(sum[:6])), configPath: abs}
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// files returns the snapshot file names of an account, oldest first.
func (s *Store) files(account string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, account))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if _, err := time.Parse(nameLayout, strings.TrimSuffix(e.Name(), ".json")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) load(account, file string) (*Snapshot, error) {
	path := filepath.Join(s.dir, account, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("%s: %w (deleting the cache is safe)", path, err)
	}
	if snap.Machines == nil {
		snap.Machines = []provider.Machine{}
	}
	return &snap, nil
}

// Latest returns the newest snapshot of an account, or nil.
func (s *Store) Latest(account string) (*Snapshot, error) {
	files, err := s.files(account)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	return s.load(account, files[len(files)-1])
}

// AsOf returns the account's state at time t: the newest snapshot
// observed at or before t, or nil when the history does not reach back
// that far.
func (s *Store) AsOf(account string, t time.Time) (*Snapshot, error) {
	files, err := s.files(account)
	if err != nil {
		return nil, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		ts, _ := time.Parse(nameLayout, strings.TrimSuffix(files[i], ".json"))
		if !ts.After(t) {
			return s.load(account, files[i])
		}
	}
	return nil, nil
}

// Oldest returns when the account's history begins.
func (s *Store) Oldest(account string) (time.Time, bool) {
	files, err := s.files(account)
	if err != nil || len(files) == 0 {
		return time.Time{}, false
	}
	ts, err := time.Parse(nameLayout, strings.TrimSuffix(files[0], ".json"))
	return ts, err == nil
}

// invalidatedFile holds the time until which listings of the account
// are not trusted, because a command changed one of its servers.
const invalidatedFile = "invalidated"

// Fresh returns the newest snapshot of an account when a listing
// confirmed it less than ttl ago and no command changed one of the
// account's servers since; nil otherwise, and always when ttl is not
// positive.
func (s *Store) Fresh(account string, ttl time.Duration, now time.Time) (*Snapshot, error) {
	if ttl <= 0 {
		return nil, nil
	}
	snap, err := s.Latest(account)
	if err != nil || snap == nil {
		return nil, err
	}
	if age := now.Sub(snap.SeenAt); age < 0 || age >= ttl {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, account, invalidatedFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return snap, nil
	case err != nil:
		return nil, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil || !snap.SeenAt.After(t) {
		return nil, nil
	}
	return snap, nil
}

// Invalidate records that a command changed one of the account's
// servers: Fresh serves no listing seen before until. A change takes a
// while to show at the provider (a machine switched off still runs for a
// minute), so until is usually some time ahead.
func (s *Store) Invalidate(account string, until time.Time) error {
	dir := filepath.Join(s.dir, account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, invalidatedFile), []byte(until.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// Record stores a listing. When it equals the newest snapshot, only that
// snapshot's SeenAt is updated; otherwise a new snapshot is written and
// the oldest ones beyond MaxSnapshots are removed. It returns the
// snapshot now holding the state and whether it is new.
func (s *Store) Record(account, prov string, machines []provider.Machine, now time.Time) (*Snapshot, bool, error) {
	now = now.UTC().Truncate(time.Second)
	ms := append([]provider.Machine{}, machines...)
	provider.SortMachines(ms)
	latest, err := s.Latest(account)
	if err != nil {
		return nil, false, err
	}
	if latest != nil && Equal(latest.Machines, ms) {
		latest.SeenAt = now
		return latest, false, s.write(latest)
	}
	snap := &Snapshot{Account: account, Provider: prov, ListedAt: now, SeenAt: now, Machines: ms}
	if err := s.write(snap); err != nil {
		return nil, false, err
	}
	return snap, true, s.prune(account)
}

func (s *Store) write(snap *Snapshot) error {
	dir := filepath.Join(s.dir, snap.Account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(s.dir, "config-path")
	if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
		_ = os.WriteFile(marker, []byte(s.configPath+"\n"), 0o600)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, snap.ListedAt.UTC().Format(nameLayout)+".json")
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (s *Store) prune(account string) error {
	files, err := s.files(account)
	if err != nil {
		return err
	}
	for len(files) > MaxSnapshots {
		if err := os.Remove(filepath.Join(s.dir, account, files[0])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		files = files[1:]
	}
	return nil
}

// Change kinds reported by Diff.
const (
	Added   = "added"
	Removed = "removed"
	Changed = "changed"
)

// Change is one difference between two listings.
type Change struct {
	// Change is added, removed or changed.
	Change string `json:"change" yaml:"change"`
	// Fields names what changed, for changed machines.
	Fields []string `json:"fields,omitempty" yaml:"fields,omitempty"`
	// Before is the previous state of a changed machine.
	Before *provider.Machine `json:"before,omitempty" yaml:"before,omitempty"`
	// Machine is the current state, or the last known state of a removed one.
	Machine provider.Machine `json:"machine" yaml:"machine"`
}

// Diff compares two listings of the same account by machine reference and
// returns the changes in listing order plus the number of unchanged
// machines.
func Diff(before, after []provider.Machine) ([]Change, int) {
	prev := make(map[string]provider.Machine, len(before))
	for _, b := range before {
		prev[b.Ref()] = b
	}
	seen := map[string]bool{}
	var changes []Change
	unchanged := 0
	for _, m := range after {
		seen[m.Ref()] = true
		b, ok := prev[m.Ref()]
		if !ok {
			changes = append(changes, Change{Change: Added, Machine: m})
			continue
		}
		if fields := ChangedFields(b, m); len(fields) > 0 {
			bb := b
			changes = append(changes, Change{Change: Changed, Fields: fields, Before: &bb, Machine: m})
		} else {
			unchanged++
		}
	}
	for _, b := range before {
		if !seen[b.Ref()] {
			changes = append(changes, Change{Change: Removed, Machine: b})
		}
	}
	SortChanges(changes)
	return changes, unchanged
}

// SortChanges orders changes like a listing: by provider, account, name
// and ID of the machine.
func SortChanges(changes []Change) {
	sort.SliceStable(changes, func(i, j int) bool {
		a, b := changes[i].Machine, changes[j].Machine
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

// Fields that ChangedFields compares, in reporting order. Provider and ID
// identify a machine and the account is the listing's; they never change.
var comparedFields = []string{"name", "kind", "ipv4", "ipv6", "location", "product", "status", "labels"}

// ChangedFields names the fields that differ between two states of a
// machine.
func ChangedFields(a, b provider.Machine) []string {
	var out []string
	for _, f := range comparedFields {
		if FieldValue(a, f) != FieldValue(b, f) {
			out = append(out, f)
		}
	}
	return out
}

// FieldValue renders one of the compared fields as text; labels become
// "k=v, k=v" in key order, so nil and empty label maps compare equal.
func FieldValue(m provider.Machine, field string) string {
	switch field {
	case "name":
		return m.Name
	case "kind":
		return string(m.Kind)
	case "ipv4":
		return m.IPv4
	case "ipv6":
		return m.IPv6
	case "location":
		return m.Location
	case "product":
		return m.Product
	case "status":
		return m.Status
	case "labels":
		keys := make([]string, 0, len(m.Labels))
		for k := range m.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + m.Labels[k]
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// Equal reports whether two listings describe the same machines in the
// same state, regardless of order.
func Equal(a, b []provider.Machine) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]provider.Machine, len(a))
	for _, m := range a {
		index[m.Ref()] = m
	}
	for _, m := range b {
		o, ok := index[m.Ref()]
		if !ok || len(ChangedFields(o, m)) > 0 {
			return false
		}
	}
	return true
}
