package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `defaults:
  output: wide
accounts:
  hrobot:
    type: hetzner-robot
    login: "#ws+abc"
    password: { env: HROBOT_PASS }
  hcloud:
    type: hetzner-cloud
    token:
      file: ~/.config/ump/secrets/hcloud.token
  ovh:
    type: ovh
    endpoint: ovh-eu
    application_key: ak
    application_secret: { command: "pass show ovh/as" }
    consumer_key: plain-ck
staging:
  key: ~/.config/ump/staging_ed25519
future:
  setting: kept
`

func TestLoadAndSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Output != "wide" {
		t.Errorf("defaults: %+v", cfg.Defaults)
	}
	if got := cfg.AccountNames(); strings.Join(got, ",") != "hcloud,hrobot,ovh" {
		t.Errorf("AccountNames = %v", got)
	}
	hr := cfg.Accounts["hrobot"]
	if hr.Type != "hetzner-robot" || hr.Settings["login"] != PlainValue("#ws+abc") || hr.Settings["password"] != (Value{Env: "HROBOT_PASS"}) {
		t.Errorf("hrobot: %+v", hr)
	}
	if cfg.Accounts["hcloud"].Settings["token"] != (Value{File: "~/.config/ump/secrets/hcloud.token"}) {
		t.Errorf("hcloud: %+v", cfg.Accounts["hcloud"])
	}
	if cfg.Accounts["ovh"].Settings["application_secret"] != (Value{Command: "pass show ovh/as"}) {
		t.Errorf("ovh: %+v", cfg.Accounts["ovh"])
	}
	if _, ok := cfg.Rest["future"]; !ok {
		t.Errorf("unknown section not preserved: %v", cfg.Rest)
	}
	if cfg.Staging.Key != "~/.config/ump/staging_ed25519" || !strings.HasSuffix(cfg.StagingKeyPath(), "/.config/ump/staging_ed25519") || strings.HasPrefix(cfg.StagingKeyPath(), "~") {
		t.Errorf("staging key: %q -> %q", cfg.Staging.Key, cfg.StagingKeyPath())
	}
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	if got := New().StagingKeyPath(); got != "/tmp/xdg-test/ump/staging_ed25519" {
		t.Errorf("default staging key path: %q", got)
	}

	out := filepath.Join(dir, "sub", "out.yaml")
	if err := Save(out, cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("saved mode %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(out)
	text := string(data)
	for _, want := range []string{"password: {env: HROBOT_PASS}", "token: {file: ~/.config/ump/secrets/hcloud.token}",
		"application_secret: {command: pass show ovh/as}", "consumer_key: plain-ck", "staging:", "  key: ~/.config/ump/staging_ed25519", "future:", "output: wide", `login: '#ws+abc'`} {
		if !strings.Contains(text, want) {
			t.Errorf("saved config lacks %q:\n%s", want, text)
		}
	}
	again, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if again.Accounts["hrobot"].Settings["password"] != (Value{Env: "HROBOT_PASS"}) {
		t.Errorf("round trip lost the reference: %+v", again.Accounts["hrobot"])
	}
}

func TestLoadMissingAndBroken(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || cfg == nil || len(cfg.Accounts) != 0 {
		t.Errorf("missing file: %v %v", cfg, err)
	}
	for name, text := range map[string]string{
		"no type":      "accounts:\n  a:\n    login: x\n",
		"two refs":     "accounts:\n  a:\n    type: t\n    password: { env: A, file: b }\n",
		"unknown ref":  "accounts:\n  a:\n    type: t\n    password: { vault: A }\n",
		"empty ref":    "accounts:\n  a:\n    type: t\n    password: { env: \"\" }\n",
		"list setting": "accounts:\n  a:\n    type: t\n    password: [a, b]\n",
		"not yaml":     "accounts: [\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(path, []byte(text), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestValue(t *testing.T) {
	cases := map[string]Value{
		"plain":           {Plain: "plain"},
		"env:X":           {Env: "X"},
		"file:/tmp/f":     {File: "/tmp/f"},
		"command:pass x":  {Command: "pass x"},
		"cmd: op read y ": {Command: "op read y"},
	}
	for in, want := range cases {
		if got := ParseValue(in); got != want {
			t.Errorf("ParseValue(%q) = %+v, want %+v", in, got, want)
		}
	}
	if (Value{Env: "X"}).String() != "env:X" || (Value{Command: "c"}).String() != "command:c" || (Value{Plain: "p"}).String() != "p" {
		t.Error("String() format")
	}
	if !(Value{}).IsZero() || (Value{Plain: "x"}).IsRef() || !(Value{File: "f"}).IsRef() {
		t.Error("IsZero/IsRef")
	}

	t.Setenv("UMP_TEST_SECRET", "from-env")
	if v, err := (Value{Env: "UMP_TEST_SECRET"}).Resolve(); err != nil || v != "from-env" {
		t.Errorf("env: %q %v", v, err)
	}
	if _, err := (Value{Env: "UMP_TEST_UNSET_VARIABLE"}).Resolve(); err == nil {
		t.Error("unset env should fail")
	}
	f := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(f, []byte("from-file\n"), 0o600)
	if v, err := (Value{File: f}).Resolve(); err != nil || v != "from-file" {
		t.Errorf("file: %q %v", v, err)
	}
	if v, err := (Value{Command: "printf 'from-command\\n'"}).Resolve(); err != nil || v != "from-command" {
		t.Errorf("command: %q %v", v, err)
	}
	if _, err := (Value{Command: "echo oops >&2; exit 3"}).Resolve(); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failing command: %v", err)
	}
	if v, err := (Value{Plain: "p"}).Resolve(); err != nil || v != "p" {
		t.Errorf("plain: %q %v", v, err)
	}

	var node struct {
		V Value `yaml:"v"`
	}
	if err := yaml.Unmarshal([]byte("v: 42\n"), &node); err != nil || node.V.Plain != "42" {
		t.Errorf("numeric scalar: %+v %v", node, err)
	}
	var fresh struct {
		V Value `yaml:"v"`
	}
	if err := yaml.Unmarshal([]byte("v:\n"), &fresh); err != nil || !fresh.V.IsZero() {
		t.Errorf("null scalar: %+v %v", fresh, err)
	}
}

func TestWriteSecretFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets", "acc.password")
	if err := WriteSecretFile(p, "s3cret"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	dfi, _ := os.Stat(filepath.Dir(p))
	if dfi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", dfi.Mode().Perm())
	}
	if v, err := (Value{File: p}).Resolve(); err != nil || v != "s3cret" {
		t.Errorf("resolve: %q %v", v, err)
	}
}

func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(EnvConfig, "")
	if got, want := ResolvePath(""), filepath.Join(dir, "ump", "config.yaml"); got != want {
		t.Errorf("default: %q, want %q", got, want)
	}
	if got, want := SecretsDir(), filepath.Join(dir, "ump", "secrets"); got != want {
		t.Errorf("secrets: %q, want %q", got, want)
	}
	t.Setenv(EnvConfig, filepath.Join(dir, "proj"))
	if got, want := ResolvePath(""), filepath.Join(dir, "proj", "config.yaml"); got != want {
		t.Errorf("env dir: %q, want %q", got, want)
	}
	if got, want := ResolvePath(filepath.Join(dir, "x.yaml")), filepath.Join(dir, "x.yaml"); got != want {
		t.Errorf("flag file: %q, want %q", got, want)
	}
	os.MkdirAll(filepath.Join(dir, "existing.d"), 0o700)
	if got, want := ResolvePath(filepath.Join(dir, "existing.d")), filepath.Join(dir, "existing.d", "config.yaml"); got != want {
		t.Errorf("existing dir: %q, want %q", got, want)
	}
	home, _ := os.UserHomeDir()
	if got, want := ResolvePath("~/cfg/ump.yaml"), filepath.Join(home, "cfg", "ump.yaml"); got != want {
		t.Errorf("tilde: %q, want %q", got, want)
	}
}

func TestHomePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	p := filepath.Join(home, ".config", "ump", "secrets", "x")
	if got := AbbreviateHome(p); got != "~/.config/ump/secrets/x" {
		t.Errorf("AbbreviateHome = %q", got)
	}
	if got := ExpandHome("~/.config/ump"); got != filepath.Join(home, ".config", "ump") {
		t.Errorf("ExpandHome = %q", got)
	}
	if got := AbbreviateHome("/opt/x"); got != "/opt/x" {
		t.Errorf("AbbreviateHome(/opt/x) = %q", got)
	}
}

func TestValidateAccountName(t *testing.T) {
	for _, ok := range []string{"main", "hetzner-robot", "a1", "prod.eu", "x_y"} {
		if err := ValidateAccountName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Main", "-x", "a b", "a/b", "ü"} {
		if err := ValidateAccountName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
