// Package config loads and saves UMP's configuration file.
//
// The file is YAML, lives at ~/.config/ump/config.yaml by default and holds
// the configured provider accounts. Credentials are stored as secret
// references ({ env: NAME }, { file: path }, { command: "..." }) rather
// than plaintext wherever possible; see Value.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the configuration file name inside the config directory.
	FileName = "config.yaml"
	// EnvConfig names the environment variable that overrides the
	// configuration location (a directory or a file).
	EnvConfig = "UMP_CONFIG"
)

// Config is the content of config.yaml.
type Config struct {
	Defaults Defaults           `yaml:"defaults,omitempty"`
	Accounts map[string]Account `yaml:"accounts,omitempty"`
	// Storage is the base URL of the storage that serves what UMP
	// fetches onto machines: the rescue image, the installer and the
	// operating system images (see images/rescue/README.md for the
	// layout). Default: DefaultStorage, the project's own.
	Storage string  `yaml:"storage,omitempty"`
	Staging Staging `yaml:"staging,omitempty"`
	// Rest keeps top-level sections this version does not know about, so
	// that saving the file does not drop them.
	Rest map[string]any `yaml:",inline"`
}

// Defaults are user-chosen defaults for CLI flags.
type Defaults struct {
	// Output is the default output format (table, wide, json, yaml, name).
	Output string `yaml:"output,omitempty"`
}

// Staging configures how UMP reaches staging environments (rescue
// systems, later its own staging image).
type Staging struct {
	// Key is the path of UMP's own SSH private key; the public key is
	// next to it with ".pub". Default: staging_ed25519 in the user's
	// configuration directory. The key is generated on first use.
	Key string `yaml:"key,omitempty"`
	// Image is the base URL of the UMP rescue image (the staging system):
	// <arch>/manifest.json and the files it names live below it. Default:
	// the rescue directory of the storage.
	Image string `yaml:"image,omitempty"`
}

// StagingKeyFile is the default name of UMP's SSH key.
const StagingKeyFile = "staging_ed25519"

// ProfilesDir is where the user's own profiles live: <name>.yaml and
// layouts.yaml in the profiles directory of the configuration directory;
// they take precedence over the published ones.
func ProfilesDir() string {
	return filepath.Join(UserConfigDir(), "profiles")
}

// DefaultStorage is where this project publishes its images and scripts.
const DefaultStorage = "https://ump.zhurko.de"

// StorageURL returns the base URL of the storage, without a trailing
// slash.
func (c *Config) StorageURL() string {
	if c.Storage != "" {
		return strings.TrimRight(c.Storage, "/")
	}
	return DefaultStorage
}

// StagingImage returns the base URL of the rescue image, without a
// trailing slash: staging.image when set, else <storage>/rescue.
func (c *Config) StagingImage() string {
	if c.Staging.Image != "" {
		return strings.TrimRight(c.Staging.Image, "/")
	}
	return c.StorageURL() + "/rescue"
}

// StagingKeyPath returns the path of UMP's SSH private key.
func (c *Config) StagingKeyPath() string {
	if c.Staging.Key != "" {
		return ExpandHome(c.Staging.Key)
	}
	return filepath.Join(UserConfigDir(), StagingKeyFile)
}

// Account is one configured provider account: a type and its settings.
type Account struct {
	Type     string           `yaml:"type"`
	Settings map[string]Value `yaml:",inline"`
}

// Resolved returns the account's settings with every secret reference
// resolved to its value.
func (a Account) Resolved() (map[string]string, error) {
	out := make(map[string]string, len(a.Settings))
	for k, v := range a.Settings {
		s, err := v.Resolve()
		if err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// New returns an empty configuration.
func New() *Config {
	return &Config{Accounts: map[string]Account{}}
}

// AccountNames returns the configured account names, sorted.
func (c *Config) AccountNames() []string {
	names := make([]string, 0, len(c.Accounts))
	for n := range c.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var accountNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// ValidateAccountName checks that a name can be used in config.yaml, on
// the command line and in file names.
func ValidateAccountName(name string) error {
	if !accountNameRe.MatchString(name) {
		return fmt.Errorf("invalid account name %q: use lower-case letters, digits, '-', '_' and '.', starting with a letter or digit", name)
	}
	return nil
}

// UserConfigDir is the per-user configuration directory, ~/.config/ump
// (honouring XDG_CONFIG_HOME).
func UserConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "ump")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "ump")
	}
	return filepath.Join(home, ".config", "ump")
}

// SecretsDir is where the setup wizard stores secret files. It is always
// under the user's configuration directory, never next to a project
// configuration that may live in a repository.
func SecretsDir() string {
	return filepath.Join(UserConfigDir(), "secrets")
}

// ResolvePath decides which configuration file to use: the --config flag,
// then $UMP_CONFIG, then the default location. A directory (existing, or a
// path without an extension) means FileName inside it.
func ResolvePath(flag string) string {
	p := flag
	if p == "" {
		p = os.Getenv(EnvConfig)
	}
	if p == "" {
		return filepath.Join(UserConfigDir(), FileName)
	}
	p = ExpandHome(p)
	if strings.HasSuffix(p, string(os.PathSeparator)) {
		return filepath.Join(p, FileName)
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return filepath.Join(p, FileName)
	}
	if filepath.Ext(p) == "" {
		return filepath.Join(p, FileName)
	}
	return p
}

// Load reads the configuration file. A missing file yields an empty
// configuration, not an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	cfg := New()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Accounts == nil {
		cfg.Accounts = map[string]Account{}
	}
	for name, a := range cfg.Accounts {
		if a.Type == "" {
			return nil, fmt.Errorf("%s: account %s has no type", path, name)
		}
		if a.Settings == nil {
			a.Settings = map[string]Value{}
			cfg.Accounts[name] = a
		}
	}
	return cfg, nil
}

// MarshalYAML renders v as YAML with two-space indentation.
func MarshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes the configuration atomically, creating the directory if
// needed. The file is created with mode 0600 because it may contain
// plaintext values.
func Save(path string, cfg *Config) error {
	data, err := MarshalYAML(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// WriteSecretFile stores a secret in a file only the user can read and
// returns the path to reference it by. Parent directories are created
// with mode 0700.
func WriteSecretFile(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ExpandHome replaces a leading "~" with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// AbbreviateHome replaces the user's home directory prefix with "~", for
// display and for paths written to the configuration.
func AbbreviateHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~/" + filepath.ToSlash(strings.TrimPrefix(p, home+string(os.PathSeparator)))
	}
	return p
}
