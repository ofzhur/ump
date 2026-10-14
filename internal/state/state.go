// Package state is UMP's view of the fleet that outlives a command and
// is shared: what the user set per server (profile, price, attributes),
// the servers added by hand, and what ump installed where. It is one
// YAML document behind a backend chosen by URL (local file, a file over
// SSH, an S3 bucket, an HTTP service, Consul, Vault), read and written
// whole with a version tag, so that concurrent writers about different
// servers merge instead of overwriting each other, and locked per server
// for the operations that must not run twice at once.
package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/provider"
)

// FormatVersion is the version of the document's layout.
const FormatVersion = 1

// Document is the state.
type Document struct {
	// Version is the layout version, FormatVersion.
	Version int `yaml:"version"`
	// Updated is when the document was last written, UTC.
	Updated time.Time `yaml:"updated,omitempty"`
	// Servers holds what ump knows per server, keyed by provider/id.
	Servers map[string]Server `yaml:"servers,omitempty"`
	// Manual holds the servers added by hand, by name.
	Manual map[string]provider.ManualServer `yaml:"manual,omitempty"`
}

// Server is what ump knows about one server beyond the provider's word.
type Server struct {
	// Profile is the profile the server is reinstalled with.
	Profile string `yaml:"profile,omitempty"`
	// Price is what the server costs, set by hand; it wins over the API.
	Price *provider.Price `yaml:"price,omitempty"`
	// Attributes are the user's own facts about the server.
	Attributes map[string]string `yaml:"attributes,omitempty"`
	// Note is free text.
	Note string `yaml:"note,omitempty"`
	// Installed is what ump last installed on the server.
	Installed *Install `yaml:"installed,omitempty"`
}

// Install is the record of an installation, as ump saw it finish.
type Install struct {
	OS           string            `yaml:"os"`
	ImageVersion string            `yaml:"image_version,omitempty"`
	Kernel       string            `yaml:"kernel,omitempty"`
	Profile      string            `yaml:"profile"`
	Layout       string            `yaml:"layout,omitempty"`
	Hostname     string            `yaml:"hostname,omitempty"`
	Admin        string            `yaml:"admin,omitempty"`
	Interfaces   map[string]string `yaml:"interfaces,omitempty"`
	At           time.Time         `yaml:"at"`
}

// Empty returns whether the record holds nothing.
func (s Server) Empty() bool {
	return s.Profile == "" && s.Price == nil && len(s.Attributes) == 0 && s.Note == "" && s.Installed == nil
}

// New returns an empty document.
func New() *Document {
	return &Document{Version: FormatVersion, Servers: map[string]Server{}, Manual: map[string]provider.ManualServer{}}
}

// Parse reads a document; empty data is an empty document.
func Parse(data []byte) (*Document, error) {
	d := New()
	if len(strings.TrimSpace(string(data))) == 0 {
		return d, nil
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(d); err != nil {
		return nil, err
	}
	if d.Version == 0 {
		d.Version = FormatVersion
	}
	if d.Version > FormatVersion {
		return nil, fmt.Errorf("the state is version %d; this ump reads up to %d", d.Version, FormatVersion)
	}
	if d.Servers == nil {
		d.Servers = map[string]Server{}
	}
	if d.Manual == nil {
		d.Manual = map[string]provider.ManualServer{}
	}
	for ref, s := range d.Servers {
		if s.Price != nil {
			if s.Price.Currency == "" || s.Price.Period == "" || s.Price.Amount < 0 {
				return nil, fmt.Errorf("servers.%s.price: needs amount, currency and period (month or hour)", ref)
			}
		}
	}
	return d, nil
}

// Bytes renders the document, with a header, tidy and sorted.
func (d *Document) Bytes() ([]byte, error) {
	d.Version = FormatVersion
	for ref, s := range d.Servers {
		if s.Empty() {
			delete(d.Servers, ref)
		}
	}
	var b strings.Builder
	b.WriteString("# UMP state: what ump knows about the fleet beyond the providers' word\n# (ump state --help). Edit by hand or with ump server set|add|remove; a\n# command rewrites the file in this layout.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte(b.String()), nil
}

// ErrConflict says the state changed between the read and the write.
var ErrConflict = errors.New("the state changed since it was read")

// ErrLocked says another operation holds the lock.
type ErrLocked struct {
	Key  string
	Info LockInfo
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("%s is locked by %s since %s (%s); ump state unlock %s forces it", e.Key, e.Info.Who, e.Info.Since.Format(time.RFC3339), e.Info.What, e.Key)
}

// LockInfo says who holds a lock, and for what.
type LockInfo struct {
	Key   string    `json:"key" yaml:"key"`
	Who   string    `json:"who" yaml:"who"`
	What  string    `json:"what" yaml:"what"`
	Since time.Time `json:"since" yaml:"since"`
}

// Backend stores the document somewhere.
type Backend interface {
	// Read returns the document's bytes and a version tag; no document
	// is empty bytes and an empty tag.
	Read(ctx context.Context) ([]byte, string, error)
	// Write stores the bytes when the stored version is still ifVersion
	// (an empty one means: nothing stored yet), and returns the new tag;
	// ErrConflict otherwise.
	Write(ctx context.Context, data []byte, ifVersion string) (string, error)
	// Lock takes the advisory lock named key; *ErrLocked when held.
	Lock(ctx context.Context, key string, info LockInfo) error
	// Unlock releases it.
	Unlock(ctx context.Context, key string) error
	// Locks lists the locks held.
	Locks(ctx context.Context) ([]LockInfo, error)
	// String says where the state is, without secrets.
	String() string
}

// Store is the document behind a backend.
type Store struct {
	Backend Backend
}

// Read returns the document and its version.
func (s *Store) Read(ctx context.Context) (*Document, string, error) {
	data, version, err := s.Backend.Read(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("state %s: %w", s.Backend, err)
	}
	d, err := Parse(data)
	if err != nil {
		return nil, "", fmt.Errorf("state %s: %w", s.Backend, err)
	}
	return d, version, nil
}

// Update reads the document, lets fn change it and writes it back; when
// someone wrote in between, it reads again and retries fn, so that
// changes about different servers merge.
func (s *Store) Update(ctx context.Context, fn func(d *Document) error) error {
	for attempt := 0; ; attempt++ {
		d, version, err := s.Read(ctx)
		if err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
		d.Updated = time.Now().UTC().Truncate(time.Second)
		data, err := d.Bytes()
		if err != nil {
			return err
		}
		if _, err := s.Backend.Write(ctx, data, version); err != nil {
			if errors.Is(err, ErrConflict) && attempt < 5 {
				continue
			}
			return fmt.Errorf("state %s: %w", s.Backend, err)
		}
		return nil
	}
}

// Lock takes the lock of one server for an operation and returns the
// release; the caller runs the operation in between. Locks are per
// server, so operations on different servers run side by side.
func (s *Store) Lock(ctx context.Context, key, what string) (func() error, error) {
	info := LockInfo{Key: key, Who: whoami(), What: what, Since: time.Now().UTC().Truncate(time.Second)}
	if err := s.Backend.Lock(ctx, key, info); err != nil {
		var locked *ErrLocked
		if errors.As(err, &locked) {
			return nil, err
		}
		return nil, fmt.Errorf("state %s: lock %s: %w", s.Backend, key, err)
	}
	return func() error { return s.Backend.Unlock(context.Background(), key) }, nil
}

// Locks lists the locks held, sorted by key.
func (s *Store) Locks(ctx context.Context) ([]LockInfo, error) {
	locks, err := s.Backend.Locks(ctx)
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", s.Backend, err)
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].Key < locks[j].Key })
	return locks, nil
}

func whoami() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "ump"
	}
	host, _ := os.Hostname()
	return user + "@" + host + " (pid " + fmt.Sprint(os.Getpid()) + ")"
}

// lockKey makes a lock key safe for a file or object name.
func lockKey(key string) string {
	return strings.NewReplacer("/", "%2F", "\\", "%5C", ":", "%3A", " ", "%20").Replace(key)
}
