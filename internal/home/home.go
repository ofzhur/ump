// Package home finds UMP's home, the directory with everything ump
// keeps, and mirrors it when it lives behind a backend. Where the home
// is: UMP_STATE in the environment names a backend URL (with the
// credential variables), else ~/.config/ump/backend.yaml names one, else
// the home is ~/.config/ump itself and nothing is mirrored. Behind a
// backend, a mirror of it on this machine is what ump reads and writes:
// pulled before a command, pushed after it, file by file with version
// tags, so that every file stays editable by hand and nothing is lost
// to a concurrent writer without a word.
package home

import (
	"context"
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

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/config"
)

// PointerFile is the file in the local configuration directory that
// names the backend; written by ump config state.
const PointerFile = "backend.yaml"

// Pointer says where the home is and how to reach it. The secrets are
// references like an account's.
type Pointer struct {
	URL       string       `yaml:"url"`
	Endpoint  string       `yaml:"endpoint,omitempty"`
	Region    string       `yaml:"region,omitempty"`
	AccessKey config.Value `yaml:"access_key,omitempty"`
	SecretKey config.Value `yaml:"secret_key,omitempty"`
	Token     config.Value `yaml:"token,omitempty"`
	User      config.Value `yaml:"user,omitempty"`
	Password  config.Value `yaml:"password,omitempty"`
}

// PointerPath is where the pointer lives: next to the local
// configuration, never in the home itself.
func PointerPath() string { return filepath.Join(config.LocalDir(), PointerFile) }

// LoadPointer reads the pointer; none is a nil pointer.
func LoadPointer() (*Pointer, error) {
	data, err := os.ReadFile(PointerPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pointer
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", PointerPath(), err)
	}
	if strings.TrimSpace(p.URL) == "" {
		return nil, nil
	}
	return &p, nil
}

// SavePointer writes the pointer; a nil one removes it.
func SavePointer(p *Pointer) error {
	if p == nil {
		err := os.Remove(PointerPath())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := config.MarshalYAML(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(PointerPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(PointerPath(), append([]byte("# Where UMP's home lives (ump config state); the home itself is mirrored under\n# the cache directory. Secrets are references, never values.\n"), data...), 0o600)
}

// Resolved is where the home is for this run.
type Resolved struct {
	// Backend is nil when the home is local.
	Backend backend.Backend
	// Dir is the directory ump reads and writes: the local home, or the
	// mirror of the backend.
	Dir string
	// Source says what named the backend: env, file or nothing.
	Source string
	// URL is the backend's URL as given.
	URL string
}

// Resolve finds the home. env wins over the pointer file; creds come
// from the environment where set, else from the pointer's references.
func Resolve(runner backend.Runner, client Client) (*Resolved, error) {
	return resolve(runner, client, true)
}

// ResolveLocal finds the home's directory without the backend: no
// credentials resolved (a reference may run a password manager), nothing
// opened, Backend nil. For the commands that must not wait on anything:
// shell completion, help, the version.
func ResolveLocal() (*Resolved, error) {
	return resolve(nil, nil, false)
}

func resolve(runner backend.Runner, client Client, withBackend bool) (*Resolved, error) {
	url := strings.TrimSpace(os.Getenv("UMP_STATE"))
	source := "env"
	var p *Pointer
	if url == "" {
		var err error
		if p, err = LoadPointer(); err != nil {
			return nil, err
		}
		if p == nil {
			return &Resolved{Dir: config.LocalDir()}, nil
		}
		url, source = p.URL, "file"
	}
	if p == nil {
		p = &Pointer{}
	}
	var creds backend.Credentials
	if withBackend {
		var err error
		if creds, err = credentials(p); err != nil {
			return nil, err
		}
	} else {
		// what names the backend (and so the mirror's directory) only;
		// placeholders stand in for the secrets, which only a connection
		// needs
		creds = location(p)
		creds.AccessKey, creds.SecretKey, creds.Token = "-", "-", "-"
	}
	creds.Runner = runner
	creds.Client = client
	b, err := backend.Open(url, creds)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(b.String()))
	r := &Resolved{Backend: b, Dir: filepath.Join(config.CacheDir(), "homes", hex.EncodeToString(sum[:6])), Source: source, URL: url}
	if !withBackend {
		r.Backend = nil
	}
	return r, nil
}

// Client is the HTTP client type, named here so that callers need not
// import net/http for a nil.
type Client = backend.HTTPClient

// OpenPointer opens the backend a pointer names, with the environment's
// credentials winning over the pointer's.
func OpenPointer(p *Pointer, runner backend.Runner, client Client) (backend.Backend, error) {
	creds, err := credentials(p)
	if err != nil {
		return nil, err
	}
	creds.Runner = runner
	creds.Client = client
	return backend.Open(p.URL, creds)
}

// credentials gathers what the backend needs: the environment wins
// over the pointer, so that CI needs no file.
func credentials(p *Pointer) (backend.Credentials, error) {
	c := location(p)
	resolve := func(v config.Value, env ...string) (string, error) {
		for _, e := range env {
			if x := os.Getenv(e); x != "" {
				return x, nil
			}
		}
		if v.IsZero() {
			return "", nil
		}
		return v.Resolve()
	}
	var err error
	if c.AccessKey, err = resolve(p.AccessKey, "AWS_ACCESS_KEY_ID"); err != nil {
		return c, fmt.Errorf("access_key: %w", err)
	}
	if c.SecretKey, err = resolve(p.SecretKey, "AWS_SECRET_ACCESS_KEY"); err != nil {
		return c, fmt.Errorf("secret_key: %w", err)
	}
	if c.Token, err = resolve(p.Token, "UMP_STATE_TOKEN", "VAULT_TOKEN", "CONSUL_HTTP_TOKEN"); err != nil {
		return c, fmt.Errorf("token: %w", err)
	}
	if c.User, err = resolve(p.User, "UMP_STATE_USER"); err != nil {
		return c, fmt.Errorf("user: %w", err)
	}
	if c.Password, err = resolve(p.Password, "UMP_STATE_PASSWORD"); err != nil {
		return c, fmt.Errorf("password: %w", err)
	}
	return c, nil
}

// location is where the backend is: the endpoint and the region, which
// need no secrets.
func location(p *Pointer) backend.Credentials {
	c := backend.Credentials{}
	if c.Endpoint = os.Getenv("UMP_STATE_ENDPOINT"); c.Endpoint == "" {
		c.Endpoint = p.Endpoint
	}
	if c.Region = os.Getenv("AWS_REGION"); c.Region == "" {
		if c.Region = os.Getenv("AWS_DEFAULT_REGION"); c.Region == "" {
			c.Region = p.Region
		}
	}
	return c
}

// Mirror is the local copy of a backend's home.
type Mirror struct {
	Backend backend.Backend
	Dir     string
	// Now is the clock of the pull times (time.Now when nil).
	Now func() time.Time
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// manifest is what the mirror knows of the backend: per file, the
// version pulled and the hash of the content at that moment.
type manifest struct {
	Files map[string]manifestEntry `json:"files"`
	// Pulled is when the last pull succeeded, Failed when the last one
	// failed (the backend did not answer).
	Pulled time.Time `json:"pulled,omitzero"`
	Failed time.Time `json:"failed,omitzero"`
}

type manifestEntry struct {
	Version string `json:"version"`
	Hash    string `json:"hash"`
}

func (m *Mirror) manifestPath() string { return filepath.Join(m.Dir, ".mirror.json") }

func (m *Mirror) loadManifest() (*manifest, error) {
	mf := &manifest{Files: map[string]manifestEntry{}}
	data, err := os.ReadFile(m.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return mf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, mf); err != nil {
		return nil, fmt.Errorf("%s: %w", m.manifestPath(), err)
	}
	if mf.Files == nil {
		mf.Files = map[string]manifestEntry{}
	}
	return mf, nil
}

func (m *Mirror) saveManifest(mf *manifest) error {
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(m.manifestPath(), data, 0o600)
}

// Pull brings the mirror up to date with the backend: files whose
// version changed are downloaded, files the backend no longer has are
// removed (unless changed locally, which Push reports), new ones are
// added. It returns what it did.
func (m *Mirror) Pull(ctx context.Context) (pulled, removed int, err error) {
	mf, err := m.loadManifest()
	if err != nil {
		return 0, 0, err
	}
	entries, err := m.Backend.List(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", m.Backend, err)
	}
	remote := map[string]string{}
	for _, e := range entries {
		if backend.ValidPath(e.Path) {
			remote[e.Path] = e.Version
		}
	}
	for path, version := range remote {
		local := filepath.Join(m.Dir, filepath.FromSlash(path))
		if known, ok := mf.Files[path]; ok {
			if known.Version == version {
				if _, err := os.Stat(local); err == nil {
					continue
				}
			} else if data, err := os.ReadFile(local); err == nil && hash(data) != known.Hash {
				continue // changed on both sides: Push reports the conflict
			}
		}
		data, _, err := m.Backend.Read(ctx, path)
		if err != nil {
			return pulled, removed, fmt.Errorf("%s: %w", m.Backend, err)
		}
		if err := writeFile(local, data); err != nil {
			return pulled, removed, err
		}
		mf.Files[path] = manifestEntry{Version: version, Hash: hash(data)}
		pulled++
	}
	mf.Pulled = m.now()
	for path, known := range mf.Files {
		if _, still := remote[path]; still {
			continue
		}
		local := filepath.Join(m.Dir, filepath.FromSlash(path))
		if data, err := os.ReadFile(local); err == nil && hash(data) != known.Hash {
			continue // changed here: Push decides
		}
		os.Remove(local)
		delete(mf.Files, path)
		removed++
	}
	return pulled, removed, m.saveManifest(mf)
}

// LastPull says when the last pull succeeded and when the last one
// failed (zero when never).
func (m *Mirror) LastPull() (pulled, failed time.Time) {
	mf, err := m.loadManifest()
	if err != nil {
		return time.Time{}, time.Time{}
	}
	return mf.Pulled, mf.Failed
}

// PullFailed records that a pull failed now.
func (m *Mirror) PullFailed() {
	mf, err := m.loadManifest()
	if err != nil {
		return
	}
	mf.Failed = m.now()
	m.saveManifest(mf)
}

// Change is one difference between the mirror and the backend.
type Change struct {
	Path string
	// Kind is added, changed, deleted or conflict (changed on both sides).
	Kind string
}

// Push writes what changed in the mirror to the backend, each file over
// the version it was pulled at; a file that changed on both sides is
// reported as a conflict and left as it is, locally and remotely.
// disposable says whether a file may be overwritten whatever happened to
// it meanwhile: the listing cache, which every listing rewrites and which
// is always safe to lose.
func disposable(path string) bool {
	return strings.HasPrefix(path, "cache/")
}

func (m *Mirror) Push(ctx context.Context) ([]Change, error) {
	mf, err := m.loadManifest()
	if err != nil {
		return nil, err
	}
	local := map[string][]byte{}
	err = filepath.WalkDir(m.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(m.Dir, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && backend.Excluded(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if backend.Excluded(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		local[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	var changes []Change
	paths := make([]string, 0, len(local))
	for p := range local {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data := local[path]
		known, pulled := mf.Files[path]
		if pulled && known.Hash == hash(data) {
			continue
		}
		version, err := m.Backend.Write(ctx, path, data, known.Version)
		if errors.Is(err, backend.ErrConflict) && disposable(path) {
			// someone else listed meanwhile: the newer listing wins
			// without a word, the cache is only a cache
			if _, current, rerr := m.Backend.Read(ctx, path); rerr == nil {
				version, err = m.Backend.Write(ctx, path, data, current)
			}
			if err != nil {
				continue
			}
		}
		if errors.Is(err, backend.ErrConflict) {
			changes = append(changes, Change{Path: path, Kind: "conflict"})
			continue
		}
		if err != nil {
			return changes, fmt.Errorf("%s: %s: %w", m.Backend, path, err)
		}
		kind := "changed"
		if !pulled {
			kind = "added"
		}
		changes = append(changes, Change{Path: path, Kind: kind})
		mf.Files[path] = manifestEntry{Version: version, Hash: hash(data)}
	}
	for path := range mf.Files {
		if _, exists := local[path]; exists {
			continue
		}
		if err := m.Backend.Delete(ctx, path); err != nil {
			return changes, fmt.Errorf("%s: %s: %w", m.Backend, path, err)
		}
		delete(mf.Files, path)
		changes = append(changes, Change{Path: path, Kind: "deleted"})
	}
	return changes, m.saveManifest(mf)
}

// Copy writes every file of a directory to a backend (ump config state
// migrate); the backend must not hold those files yet unless force.
func Copy(ctx context.Context, dir string, b backend.Backend, force bool) (int, error) {
	src := &backend.Local{Dir: dir}
	entries, err := src.List(ctx)
	if err != nil {
		return 0, err
	}
	existing := map[string]string{}
	remote, err := b.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", b, err)
	}
	for _, e := range remote {
		existing[e.Path] = e.Version
	}
	if len(existing) > 0 && !force {
		return 0, fmt.Errorf("%s holds %d files already; --force overwrites them", b, len(existing))
	}
	n := 0
	for _, e := range entries {
		data, _, err := src.Read(ctx, e.Path)
		if err != nil {
			return n, err
		}
		if _, err := b.Write(ctx, e.Path, data, existing[e.Path]); err != nil {
			return n, fmt.Errorf("%s: %s: %w", b, e.Path, err)
		}
		n++
	}
	return n, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
