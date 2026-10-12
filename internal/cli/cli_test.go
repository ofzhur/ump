package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

func init() { provider.Register(fakeType) }

var fakeType = provider.Type{
	Name:        "fake",
	Kind:        provider.KindCloud,
	Description: "Fake provider for tests",
	Fields: []provider.Field{
		{Key: "login", Label: "Login", Help: "the login", Required: true},
		{Key: "password", Label: "Password", Secret: true, Required: true},
		{Key: "region", Label: "Region", Choices: []string{"eu", "us"}, Default: "eu"},
		{Key: "url", Label: "URL", Hidden: true},
	},
	New: func(account string, s map[string]string, _ provider.Options) (provider.Provider, error) {
		return &fakeProvider{account: account, settings: s}, nil
	},
}

type fakeProvider struct {
	account  string
	settings map[string]string
}

func (f *fakeProvider) Type() string    { return "fake" }
func (f *fakeProvider) Account() string { return f.account }
func (f *fakeProvider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapInventory}
}
func (f *fakeProvider) List(ctx context.Context) ([]provider.Machine, error) {
	if f.settings["password"] != "s3cret" {
		return nil, &provider.APIError{Provider: "fake", Status: 401, Code: "UNAUTHORIZED", Message: "authentication failed"}
	}
	region := f.settings["region"]
	if region == "" {
		region = "eu"
	}
	return []provider.Machine{
		{Provider: "fake", Account: f.account, ID: "1", Name: "web-01", Kind: provider.KindCloud, IPv4: "203.0.113.10",
			IPv6: "2001:db8::/64", Location: region + "-1", Product: "m1", Status: "running", Labels: map[string]string{"env": "prod"}},
		{Provider: "fake", Account: f.account, ID: "2", Name: "db-01", Kind: provider.KindCloud, IPv4: "203.0.113.11",
			Location: region + "-1", Product: "m2", Status: "off"},
	}, nil
}

const testConfig = `accounts:
  one:
    type: fake
    login: bob
    password: {env: FAKE_PASS}
  two:
    type: fake
    login: alice
    password: wrong
    region: us
`

type testEnv struct {
	t       *testing.T
	cfgPath string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("UMP_CONFIG", "")
	t.Setenv("FAKE_PASS", "s3cret")
	return &testEnv{t: t, cfgPath: filepath.Join(dir, "config.yaml")}
}

func (e *testEnv) write(text string) {
	e.t.Helper()
	if err := os.WriteFile(e.cfgPath, []byte(text), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) read() string {
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		return ""
	}
	return string(data)
}

// run executes ump with stdin and args against the test configuration.
func (e *testEnv) run(stdin string, tty bool, args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	var out, errb bytes.Buffer
	a := &app{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb}
	if tty {
		a.isTTY = func() bool { return true }
	}
	code = a.run(context.Background(), append([]string{"--config", e.cfgPath}, args...))
	return out.String(), errb.String(), code
}

func fields(s string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

func expectRows(t *testing.T, got string, want [][]string) {
	t.Helper()
	rows := fields(got)
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%s", len(rows), len(want), got)
	}
	for i := range want {
		if strings.Join(rows[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("row %d: got %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestGetServersTable(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "get", "servers", "--account", "one")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running"},
	})
	if !strings.HasPrefix(out, "ACCOUNT   PROVIDER   ID   NAME     IPV4           LOCATION   STATUS\n") {
		t.Errorf("unexpected column alignment:\n%s", out)
	}

	out, _, _ = e.run("", false, "list", "srv", "--account", "one", "--no-headers")
	if len(fields(out)) != 2 {
		t.Errorf("aliases or --no-headers broken:\n%s", out)
	}

	out, _, code = e.run("", false, "get", "servers", "--account", "one", "-o", "wide")
	if code != 0 {
		t.Fatal(out)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS", "IPV6", "PRODUCT", "KIND"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off", "<none>", "m2", "cloud"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running", "2001:db8::/64", "m1", "cloud"},
	})
}

func TestGetServersFormats(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, _, code := e.run("", false, "get", "servers", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatal(out)
	}
	var ms []provider.Machine
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(ms) != 2 || ms[1].Labels["env"] != "prod" || ms[0].Ref() != "fake/2" {
		t.Errorf("unexpected JSON: %+v", ms)
	}

	out, _, _ = e.run("", false, "get", "servers", "--account", "one", "-o", "yaml")
	if !strings.HasPrefix(out, "- provider: fake\n  account: one\n  id: \"2\"\n") {
		t.Errorf("unexpected YAML:\n%s", out)
	}

	out, _, _ = e.run("", false, "get", "servers", "--account", "one", "-o", "name")
	if out != "fake/2\nfake/1\n" {
		t.Errorf("unexpected names: %q", out)
	}

	_, errOut, code := e.run("", false, "get", "servers", "-o", "xml")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("bad format: code %d, %q", code, errOut)
	}

	e.write("defaults:\n  output: name\n" + testConfig)
	out, _, _ = e.run("", false, "get", "servers", "--account", "one")
	if out != "fake/2\nfake/1\n" {
		t.Errorf("defaults.output ignored: %q", out)
	}
}

func TestGetServersRefs(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "get", "servers", "--account", "one", "web-01", "fake/2", "203.0.113.11", "nope")
	if code != 1 {
		t.Errorf("code %d", code)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running"},
	})
	if errOut != "ump: server \"nope\" not found\nump: 1 of 4 servers not found\n" {
		t.Errorf("stderr: %q", errOut)
	}
	out, _, code = e.run("", false, "get", "servers", "--account", "one", "2001:db8::1", "-o", "name")
	if code != 0 || out != "fake/1\n" {
		t.Errorf("IPv6 prefix lookup: code %d, %q", code, out)
	}
}

func TestGetServersPartialFailure(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "get", "servers")
	if code != 4 {
		t.Errorf("code %d", code)
	}
	if rows := fields(out); len(rows) != 3 || rows[1][0] != "one" {
		t.Errorf("account one should still be listed:\n%s", out)
	}
	if !strings.Contains(errOut, "ump: account two: fake: authentication failed (UNAUTHORIZED, HTTP 401)\n") ||
		!strings.Contains(errOut, "1 of 2 accounts could not be listed") {
		t.Errorf("stderr: %q", errOut)
	}

	_, errOut, code = e.run("", false, "get", "servers", "--account", "nope")
	if code != 2 || !strings.Contains(errOut, `account "nope" is not configured (configured: one, two)`) {
		t.Errorf("unknown account: code %d, %q", code, errOut)
	}
}

func TestGetServersNoConfig(t *testing.T) {
	e := newTestEnv(t)
	_, errOut, code := e.run("", false, "get", "servers")
	if code != 1 || !strings.Contains(errOut, "run `ump config`") {
		t.Errorf("code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "get", "pods")
	if code != 2 || !strings.Contains(errOut, `unknown resource type "pods"`) {
		t.Errorf("code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "get", "servers", "--bogus")
	if code != 2 || !strings.Contains(errOut, "unknown flag") {
		t.Errorf("code %d, %q", code, errOut)
	}
}

func TestConfigAddViewTestRemove(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	out, errOut, code := e.run("", false, "config", "add", "fake", "three", "--set", "login=carol", "--set", "password=env:FAKE_PASS", "--set", "region=us")
	if code != 0 || !strings.Contains(out, "Added account three (fake)") || !strings.Contains(out, "(2 servers)") {
		t.Fatalf("add: code %d, out %q, err %q", code, out, errOut)
	}
	if text := e.read(); !strings.Contains(text, "  three:\n    type: fake\n    login: carol\n    password: {env: FAKE_PASS}\n    region: us\n") {
		t.Errorf("saved config:\n%s", text)
	}

	out, _, code = e.run("", false, "config", "view")
	if code != 0 || !strings.HasPrefix(out, "# "+e.cfgPath+"\n") {
		t.Fatalf("view: code %d\n%s", code, out)
	}
	if !strings.Contains(out, "password: <redacted>") || strings.Contains(out, "wrong") ||
		!strings.Contains(out, "password: {env: FAKE_PASS}") || !strings.Contains(out, "login: alice") {
		t.Errorf("view redaction:\n%s", out)
	}

	out, errOut, code = e.run("", false, "config", "test")
	if code != 4 {
		t.Errorf("test: code %d", code)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "TYPE", "RESULT"},
		{"one", "fake", "ok,", "2", "servers"},
		{"three", "fake", "ok,", "2", "servers"},
		{"two", "fake", "error:", "fake:", "authentication", "failed", "(UNAUTHORIZED,", "HTTP", "401)"},
	})
	if !strings.Contains(errOut, "1 of 3 accounts failed") {
		t.Errorf("test stderr: %q", errOut)
	}
	out, _, code = e.run("", false, "config", "test", "one")
	if code != 0 || len(fields(out)) != 2 {
		t.Errorf("test one: code %d\n%s", code, out)
	}

	// a secret file the wizard would have created is removed with the account
	secret := secretFilePath("three", "password")
	if err := config.WriteSecretFile(secret, "s3cret"); err != nil {
		t.Fatal(err)
	}
	e.write(e.read() + "  four:\n    type: fake\n    login: d\n    password: {file: " + secret + "}\n")
	out, _, code = e.run("", false, "config", "remove", "four")
	if code != 0 || !strings.Contains(out, "Removed account four (fake)") || !strings.Contains(out, "Removed secret file") {
		t.Errorf("remove: code %d\n%s", code, out)
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Errorf("secret file still exists: %v", err)
	}
	if strings.Contains(e.read(), "four:") {
		t.Error("account four still in config")
	}
	_, errOut, code = e.run("", false, "config", "remove", "four")
	if code != 1 || !strings.Contains(errOut, "not configured") {
		t.Errorf("remove again: code %d, %q", code, errOut)
	}
}

func TestConfigAddRejects(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	before := e.read()
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"config", "add", "nope", "x", "--set", "a=b"}, 2, "unknown provider type"},
		{[]string{"config", "add", "fake", "x", "--set", "bogus=1"}, 2, `has no setting "bogus"`},
		{[]string{"config", "add", "fake", "x", "--set", "login=a"}, 2, "password is required"},
		{[]string{"config", "add", "fake", "Bad Name", "--set", "login=a"}, 2, "invalid account name"},
		{[]string{"config", "add", "fake", "x", "--set", "login=a", "--set", "password=bad"}, 4, "not saved"},
		{[]string{"config", "add", "fake", "x", "--set", "login=a", "--set", "password=env:UMP_TEST_UNSET"}, 1, "UMP_TEST_UNSET is not set"},
		{[]string{"config", "add", "fake"}, 2, "accepts 2 arg(s)"},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	if e.read() != before {
		t.Error("rejected adds changed the configuration")
	}
	out, _, code := e.run("", false, "config", "add", "fake", "x", "--set", "login=a", "--set", "password=bad", "--no-test")
	if code != 0 || !strings.Contains(out, "Added account x") {
		t.Errorf("--no-test: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "config", "add", "fake", "x", "--set", "login=a", "--set", "password=s3cret")
	if code != 0 || !strings.Contains(out, "Replaced account x") {
		t.Errorf("replace: code %d, %q", code, out)
	}
}

func TestConfigTypesAndPath(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("", false, "config", "types")
	if code != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"hetzner-robot", "hetzner-cloud", "ovh", "fake", "required, secret", "application_key"} {
		if !strings.Contains(out, want) {
			t.Errorf("types output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimRight(line, " \t") != line {
			t.Errorf("trailing whitespace in %q", line)
		}
	}
	out, _, _ = e.run("", false, "config", "path")
	if out != e.cfgPath+"\n" {
		t.Errorf("path: %q", out)
	}
}

func TestWizardNeedsTTY(t *testing.T) {
	e := newTestEnv(t)
	_, errOut, code := e.run("", false, "config")
	if code != 3 || !strings.Contains(errOut, "needs a terminal") {
		t.Errorf("code %d, %q", code, errOut)
	}
}

func TestWizardFileStorage(t *testing.T) {
	e := newTestEnv(t)
	script := "fake\nacc\n1\ncarol\ns3cret\n\nn\n"
	out, errOut, code := e.run(script, true, "config")
	if code != 0 {
		t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"No accounts configured yet.", "Provider type", "1) fake", "Account name [fake]:",
		"Where should this account's secrets be kept?", "the login", "Login:", "Password:", "Region", "Testing acc ... ok (2 servers)",
		"Saved account acc (fake)", "Done. Try: ump get servers"} {
		if !strings.Contains(out, want) {
			t.Errorf("wizard output lacks %q:\n%s", want, out)
		}
	}
	secret := secretFilePath("acc", "password")
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "s3cret\n" {
		t.Fatalf("secret file: %q %v", data, err)
	}
	if fi, _ := os.Stat(secret); fi.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode %v", fi.Mode().Perm())
	}
	text := e.read()
	if !strings.Contains(text, "    login: carol\n") || !strings.Contains(text, "    region: eu\n") ||
		!strings.Contains(text, "    password: {file: "+config.AbbreviateHome(secret)+"}\n") || strings.Contains(text, "s3cret") {
		t.Errorf("saved config:\n%s", text)
	}
	out, _, code = e.run("", false, "get", "servers", "-o", "name")
	if code != 0 || out != "fake/2\nfake/1\n" {
		t.Errorf("listing with the wizard's config: code %d, %q", code, out)
	}

	// replacing an existing account asks first; declining asks for another name
	out, _, code = e.run("fake\nacc\nn\nacc2\n1\ncarol\ns3cret\n\nn\n", true, "config")
	if code != 0 || !strings.Contains(out, `Account "acc" (fake) exists. Replace it? [y/N]:`) || !strings.Contains(out, "Saved account acc2") {
		t.Errorf("replace prompt: code %d\n%s", code, out)
	}
}

func TestWizardEnvStorage(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("fake\nacc\n2\ncarol\nFAKE_PASS\n\nn\n", true, "config")
	if code != 0 || !strings.Contains(out, "Environment variable holding the password [UMP_ACC_PASSWORD]:") ||
		!strings.Contains(out, "Testing acc ... ok (2 servers)") || strings.Contains(out, "export") {
		t.Errorf("env storage: code %d\n%s", code, out)
	}
	if !strings.Contains(e.read(), "password: {env: FAKE_PASS}") {
		t.Errorf("config:\n%s", e.read())
	}
	// an unset variable skips the test and still saves
	out, _, code = e.run("fake\nacc2\n2\ncarol\nUMP_TEST_UNSET_VAR\n\nn\n", true, "config")
	if code != 0 || !strings.Contains(out, "UMP_TEST_UNSET_VAR is not set in this shell") ||
		!strings.Contains(out, "Skipping the connection test") || !strings.Contains(out, "Saved account acc2") ||
		!strings.Contains(out, "export UMP_TEST_UNSET_VAR=<password>") {
		t.Errorf("unset env: code %d\n%s", code, out)
	}
}

func TestWizardCommandStorage(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("fake\nacc\n3\ncarol\nfalse\ny\nprintf s3cret\n\nn\n", true, "config")
	if code != 0 || !strings.Contains(out, "Try another command? [Y/n]:") || !strings.Contains(out, "Testing acc ... ok (2 servers)") {
		t.Errorf("command storage: code %d\n%s", code, out)
	}
	if !strings.Contains(e.read(), "password: {command: printf s3cret}") {
		t.Errorf("config:\n%s", e.read())
	}
}

func TestWizardFailedTestIsNotSaved(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("fake\nacc\n1\ncarol\nbad\n\nn\nn\n", true, "config")
	if code != 0 || !strings.Contains(out, "failed\n  fake: authentication failed") ||
		!strings.Contains(out, "Save the account anyway? [y/N]:") || !strings.Contains(out, "Not saved.") {
		t.Errorf("code %d\n%s", code, out)
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Errorf("config was written: %s", e.read())
	}
	if _, err := os.Stat(secretFilePath("acc", "password")); !os.IsNotExist(err) {
		t.Error("secret file was written although the account was not saved")
	}
	// closing stdin aborts cleanly
	_, errOut, code := e.run("fake\n", true, "config")
	if code != 3 || !strings.Contains(errOut, "input closed") {
		t.Errorf("EOF: code %d, %q", code, errOut)
	}
}
