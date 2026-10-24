// Package state is the state document of UMP's home: what ump knows
// about the fleet beyond the providers' word (per-server settings, the
// servers added by hand, what was installed where), kept as state.yaml
// in the home and changed with read-modify-write on the backend's
// version tag, so that concurrent changes about different servers merge.
package state

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/provider"
)

// FormatVersion is the version of the document's layout: 3 since a
// server's private addresses are a list (2 had one, under private). An
// ump reads the versions up to its own and refuses a later one before it
// reads anything else.
const FormatVersion = 3

// File is the document's name in the home.
const File = "state.yaml"

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
	// Groups are the Ansible groups the server is in besides common, in
	// order: set by ump server reinstall --group (none without), changed
	// by ump server set --group and --ungroup.
	Groups []string `yaml:"groups,omitempty"`
	// Private are the server's addresses in private networks, one per
	// network, in the order its networks were given: given with an
	// installation (reserved before it starts) or by ump server set
	// --private-network and --private-ip, each one server's alone in its
	// network, and kept across reinstalls.
	Private []Private `yaml:"private_networks,omitempty"`
	// StaleFacts says that the facts a fact cache has of the server are
	// those of its old system: set when an installation is recorded,
	// taken away once a provisioning run completed on it.
	StaleFacts bool `yaml:"stale_facts,omitempty"`
	// Roles is what an older ump wrote for the groups; Parse turns it
	// into Groups, so it is never written.
	Roles []string `yaml:"roles,omitempty"`
	// OnePrivate is the one address of version 2 (ump v0.5.0); Parse
	// turns it into Private, so it is never written.
	OnePrivate *Private `yaml:"private,omitempty"`
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

// Private is a server's address in a private network of networks.yaml,
// with the network's CIDR as it was when the address was given.
type Private = provider.Private

// PrivateHolder is the server (provider/id) that has the address of p
// in a network that overlaps p's, whatever its name, leaving out the
// servers of except; empty when none has it.
func (d *Document) PrivateHolder(p Private, except ...string) string {
	want, err := netip.ParsePrefix(p.CIDR)
	if err != nil {
		return ""
	}
	for _, ref := range slices.Sorted(maps.Keys(d.Servers)) {
		if slices.Contains(except, ref) {
			continue
		}
		for _, q := range d.Servers[ref].Private {
			if q.Address != p.Address {
				continue
			}
			if c, err := netip.ParsePrefix(q.CIDR); err == nil && c.Overlaps(want) {
				return ref
			}
		}
	}
	return ""
}

// Empty returns whether the record holds nothing.
func (s Server) Empty() bool {
	return s.Profile == "" && s.Price == nil && len(s.Attributes) == 0 && s.Note == "" && s.Installed == nil && len(s.Groups) == 0 && len(s.Private) == 0 && !s.StaleFacts
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
	// the version first: a later one has keys this ump does not know
	var head struct {
		Version int `yaml:"version"`
	}
	if yaml.Unmarshal(data, &head) == nil && head.Version > FormatVersion {
		return nil, fmt.Errorf("the state is version %d; this ump reads up to %d", head.Version, FormatVersion)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(d); err != nil {
		return nil, err
	}
	if d.Version == 0 {
		d.Version = FormatVersion
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
		if p := s.OnePrivate; p != nil {
			if len(s.Private) > 0 {
				return nil, fmt.Errorf("servers.%s: private is the one address of version 2, private_networks the list of version 3; give one of them", ref)
			}
			if !validPrivate(*p) {
				return nil, fmt.Errorf("servers.%s.private: needs network, cidr and an IPv4 address in it", ref)
			}
			s.Private, s.OnePrivate = []Private{*p}, nil
			d.Servers[ref] = s
		}
		for i, p := range s.Private {
			if !validPrivate(p) {
				return nil, fmt.Errorf("servers.%s.private_networks[%d]: needs network, cidr and an IPv4 address in it", ref, i)
			}
			for _, q := range s.Private[:i] {
				if q.Network == p.Network {
					return nil, fmt.Errorf("servers.%s.private_networks: the network %s is given twice", ref, p.Network)
				}
			}
		}
		if len(s.Roles) > 0 {
			s.Groups = rolesToGroups(s.Roles, s.Groups)
			s.Roles = nil
			d.Servers[ref] = s
		}
	}
	return d, nil
}

// validPrivate reports whether a private address names its network and
// is an IPv4 address inside the CIDR.
func validPrivate(p Private) bool {
	a, aerr := netip.ParseAddr(p.Address)
	c, cerr := netip.ParsePrefix(p.CIDR)
	return p.Network != "" && aerr == nil && a.Is4() && cerr == nil && c.Contains(a)
}

// rolesToGroups adds the roles an older ump recorded to the groups, as
// group names: '-' and '.' become '_', a leading digit gets a '_' in
// front; common (implied) and Ansible's own all, ungrouped, localhost
// and _meta are left out, and no name comes twice.
func rolesToGroups(roles, groups []string) []string {
	out := append([]string{}, groups...)
	for _, r := range roles {
		g := strings.NewReplacer("-", "_", ".", "_").Replace(r)
		if g != "" && g[0] >= '0' && g[0] <= '9' {
			g = "_" + g
		}
		switch g {
		case "", "common", "all", "ungrouped", "localhost", "_meta":
			continue
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
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
	b.WriteString("# UMP state: what ump knows about the fleet beyond the providers' word\n# (ump config state --help). Edit by hand or with ump server set|add|remove;\n# a command rewrites the file in this layout.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte(b.String()), nil
}

// Store is the document in a backend.
type Store struct {
	Backend backend.Backend
}

// Read returns the document and its version.
func (s *Store) Read(ctx context.Context) (*Document, string, error) {
	data, version, err := s.Backend.Read(ctx, File)
	if err != nil {
		return nil, "", fmt.Errorf("%s in %s: %w", File, s.Backend, err)
	}
	d, err := Parse(data)
	if err != nil {
		return nil, "", fmt.Errorf("%s in %s: %w", File, s.Backend, err)
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
		if _, err := s.Backend.Write(ctx, File, data, version); err != nil {
			if errors.Is(err, backend.ErrConflict) && attempt < 5 {
				continue
			}
			return fmt.Errorf("%s in %s: %w", File, s.Backend, err)
		}
		return nil
	}
}

// Lock takes the lock of one server for an operation and returns the
// release; the caller runs the operation in between. Locks are per
// server, so operations on different servers run side by side.
func (s *Store) Lock(ctx context.Context, key, what string) (func() error, error) {
	info := backend.LockInfo{Key: key, Who: whoami(), What: what, Since: time.Now().UTC().Truncate(time.Second)}
	if err := s.Backend.Lock(ctx, key, info); err != nil {
		var locked *backend.ErrLocked
		if errors.As(err, &locked) {
			return nil, err
		}
		return nil, fmt.Errorf("%s: lock %s: %w", s.Backend, key, err)
	}
	return func() error { return s.Backend.Unlock(context.Background(), key) }, nil
}

// Locks lists the locks held, sorted by key.
func (s *Store) Locks(ctx context.Context) ([]backend.LockInfo, error) {
	locks, err := s.Backend.Locks(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Backend, err)
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
