package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

func init() { provider.Register(fakeType) }

var fakeType = provider.Type{
	Name:         "fake",
	Kind:         provider.KindCloud,
	Description:  "Fake provider for tests",
	Capabilities: []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapReverseDNS},
	Fields: []provider.Field{
		{Key: "login", Label: "Login", Help: "the login", Required: true},
		{Key: "password", Label: "Password", Secret: true, Required: true},
		{Key: "region", Label: "Region", Choices: []string{"eu", "us"}, Default: "eu"},
		{Key: "url", Label: "URL", Hidden: true},
		{Key: "variant", Label: "Variant", Hidden: true},
	},
	New: func(account string, s map[string]string, _ provider.Options) (provider.Provider, error) {
		return &fakeProvider{account: account, settings: s}, nil
	},
}

type fakeProvider struct {
	account  string
	settings map[string]string
}

// fakePTRs is the reverse DNS state shared by all fake providers in a test.
var fakePTRs = map[string]string{}

// fakePower records power and rescue actions of the fake providers.
var fakePower = struct {
	actions []string
	rescue  map[string]bool
	keys    []string
}{rescue: map[string]bool{}}

func (f *fakeProvider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	return provider.PowerOn, nil
}

func (f *fakeProvider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	if m.ID == "2" {
		return &provider.APIError{Provider: "fake", Status: 423, Code: "locked", Message: "server is locked"}
	}
	fakePower.actions = append(fakePower.actions, "reboot:"+string(mode))
	return nil
}

func (f *fakeProvider) PowerOn(ctx context.Context, m provider.Machine) error {
	fakePower.actions = append(fakePower.actions, "on")
	return nil
}

func (f *fakeProvider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	fakePower.actions = append(fakePower.actions, fmt.Sprintf("off:%v", hard))
	return nil
}

func (f *fakeProvider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	return fakePower.rescue[m.Ref()], nil
}

func (f *fakeProvider) EnableRescue(ctx context.Context, m provider.Machine, opts provider.RescueOptions) (*provider.RescueInfo, error) {
	fakePower.rescue[m.Ref()] = true
	fakePower.keys = nil
	for _, k := range opts.Keys {
		fakePower.keys = append(fakePower.keys, k.Name+" "+k.Fingerprint)
	}
	fakePower.actions = append(fakePower.actions, "rescue")
	return &provider.RescueInfo{Address: m.IPv4, User: "root", Password: "r3scue"}, nil
}

func (f *fakeProvider) DisableRescue(ctx context.Context, m provider.Machine) error {
	fakePower.rescue[m.Ref()] = false
	fakePower.actions = append(fakePower.actions, "unrescue")
	return nil
}

// PTRs reports the primary IPv4 and every other address with an entry.
func (f *fakeProvider) PTRs(ctx context.Context, m provider.Machine) ([]provider.PTR, error) {
	if m.ID == "2" && f.settings["region"] == "us" {
		return nil, &provider.APIError{Provider: "fake", Status: 500, Message: "ptr lookup failed"}
	}
	out := []provider.PTR{{IP: m.IPv4, Name: fakePTRs[m.Account+"/"+m.IPv4]}}
	for key, name := range fakePTRs {
		acct, ip, _ := strings.Cut(key, "/")
		if acct != m.Account || ip == m.IPv4 {
			continue
		}
		if addr, err := netip.ParseAddr(ip); err == nil && m.Matches(addr.String()) {
			out = append(out, provider.PTR{IP: ip, Name: name})
		}
	}
	return out, nil
}

func (f *fakeProvider) SetPTR(ctx context.Context, m provider.Machine, ip netip.Addr, name string) error {
	if !m.Matches(ip.String()) {
		return fmt.Errorf("fake: address %s does not belong to server %s", ip, m.ID)
	}
	if name == "" {
		delete(fakePTRs, m.Account+"/"+ip.String())
		return nil
	}
	fakePTRs[m.Account+"/"+ip.String()] = name
	return nil
}

// Describe adds a section; it fails for db-01 to exercise the error path.
func (f *fakeProvider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	if m.ID == "2" {
		return nil, &provider.APIError{Provider: "fake", Status: 500, Message: "describe failed"}
	}
	s := provider.Section{Name: "Power"}
	s.Add("state", "on").Add("reset_types", []string{"soft", "hard"}).Add("locked", false).Add("disk", provider.Bytes(40<<30)).Add("iso", nil)
	return &provider.Details{Machine: m, Sections: []provider.Section{s}}, nil
}

// Rename succeeds for every machine but db-01, which the provider refuses.
func (f *fakeProvider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	if m.ID == "2" {
		return provider.Machine{}, &provider.APIError{Provider: "fake", Status: 409, Code: "uniqueness_error", Message: "name taken"}
	}
	m.Name = name
	return m, nil
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
	if f.settings["variant"] == "b" {
		// the world has moved on: web-01 changed, db-01 is gone, cache-01 is new
		return []provider.Machine{
			{Provider: "fake", Account: f.account, ID: "1", Name: "web-01", Kind: provider.KindCloud, IPv4: "203.0.113.12",
				IPv6: "2001:db8::/64", Location: region + "-1", Product: "m1", Status: "off", Labels: map[string]string{"env": "prod"}},
			{Provider: "fake", Account: f.account, ID: "3", Name: "cache-01", Kind: provider.KindCloud, IPv4: "203.0.113.13",
				Location: region + "-1", Product: "m3", Status: "initializing"},
		}, nil
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
	// now is the clock the CLI sees; tests move it forward.
	now time.Time
	// sshCalls records the ssh command lines the CLI would have run.
	sshCalls [][]string
	// sshInteractive records whether each call handed the terminal over.
	sshInteractive []bool
	// sshExit is the exit status the fake ssh returns.
	sshExit int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	os.MkdirAll(filepath.Join(dir, "home"), 0o700)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("UMP_CONFIG", "")
	t.Setenv("FAKE_PASS", "s3cret")
	// the fake probe always answers, so the "wait for SSH to go down" phase
	// would otherwise last its full real-world duration
	oldDown, oldPoll := waitDownTime, waitPoll
	waitDownTime, waitPoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { waitDownTime, waitPoll = oldDown, oldPoll })
	return &testEnv{t: t, cfgPath: filepath.Join(dir, "config.yaml"), now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
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
	a := &app{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, now: func() time.Time { return e.now }, tz: time.UTC,
		ssh: func(argv []string, interactive bool) (int, error) {
			e.sshCalls = append(e.sshCalls, argv)
			e.sshInteractive = append(e.sshInteractive, interactive)
			return e.sshExit, nil
		},
		probe: func(ctx context.Context, addr string) bool { return true },
	}
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

func TestWizardQuit(t *testing.T) {
	e := newTestEnv(t)
	// q at the provider menu: nothing to add
	out, errOut, code := e.run("q\n", true, "config")
	if code != 0 || errOut != "" || !strings.Contains(out, "  q) quit\n") || !strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit at menu: code %d, stderr %q\n%s", code, errOut, out)
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}
	// q in the middle of adding an account: nothing saved
	out, _, code = e.run("fake\nacc\nq\n", true, "config")
	if code != 0 || !strings.Contains(out, "Nothing saved for this account.") || !strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit mid-account: code %d\n%s", code, out)
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}
	// "exit" at a choice field, after the secret was typed
	out, _, code = e.run("fake\nacc\n1\ncarol\ns3cret\nexit\n", true, "config")
	if code != 0 || !strings.Contains(out, "Nothing saved for this account.") {
		t.Errorf("exit at choice: code %d\n%s", code, out)
	}
	if _, err := os.Stat(secretFilePath("acc", "password")); !os.IsNotExist(err) {
		t.Error("secret file was written although the account was not saved")
	}
	// q at the yes/no question after a save keeps what was saved
	out, _, code = e.run("fake\nacc\n1\ncarol\ns3cret\n\nq\n", true, "config")
	if code != 0 || !strings.Contains(out, "Saved account acc (fake)") || !strings.Contains(out, "Done. Try: ump get servers") ||
		strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit after save: code %d\n%s", code, out)
	}
	if !strings.Contains(e.read(), "  acc:\n    type: fake\n") {
		t.Errorf("config:\n%s", e.read())
	}
	// q at the replace question
	out, _, code = e.run("fake\nacc\nq\n", true, "config")
	if code != 0 || !strings.Contains(out, "Replace it? [y/N]: ") || !strings.Contains(out, "Nothing saved for this account.") {
		t.Errorf("quit at replace: code %d\n%s", code, out)
	}
}

func TestWizardInterrupted(t *testing.T) {
	e := newTestEnv(t)

	// Ctrl-C while a prompt waits: stdin never answers, the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out, errOut bytes.Buffer
	a := &app{stdin: pr, stdout: &out, stderr: &errOut, isTTY: func() bool { return true }}
	code := a.run(ctx, []string{"--config", e.cfgPath, "config"})
	if code != 3 || errOut.String() != "ump: interrupted\n" {
		t.Errorf("interrupted at prompt: code %d, stderr %q\n%s", code, errOut.String(), out.String())
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}

	// Ctrl-C during hidden input: the abandoned read never returns, so the
	// terminal state captured before it must be restored by the wizard.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	restored := false
	out.Reset()
	errOut.Reset()
	a = &app{
		stdin:  strings.NewReader("fake\nacc\n1\ncarol\n"),
		stdout: &out,
		stderr: &errOut,
		isTTY:  func() bool { return true },
		readSecret: func() (string, error) {
			cancel()  // "Ctrl-C" arrives while the read is pending
			select {} // and the read stays blocked forever
		},
		saveTerm: func() func() { return func() { restored = true } },
	}
	code = a.run(ctx, []string{"--config", e.cfgPath, "config"})
	if code != 3 || errOut.String() != "ump: interrupted\n" || !restored {
		t.Errorf("interrupted in secret: code %d, stderr %q, restored %v\n%s", code, errOut.String(), restored, out.String())
	}
	if !strings.HasSuffix(out.String(), "Password: \n") {
		t.Errorf("prompt line not closed before the error:\n%q", out.String())
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}

	// a cancelled listing is "interrupted" too, not a provider error
	e.write(testConfig)
	out.Reset()
	errOut.Reset()
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	a = &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	code = a.run(ctx, []string{"--config", e.cfgPath, "get", "servers"})
	if code != 3 || errOut.String() != "ump: interrupted\n" {
		t.Errorf("interrupted listing: code %d, stderr %q", code, errOut.String())
	}
}

// listOnlyProvider cannot describe machines; describe falls back to the listing.
type listOnlyProvider struct{ inner *fakeProvider }

func (l *listOnlyProvider) Type() string                        { return "listonly" }
func (l *listOnlyProvider) Account() string                     { return l.inner.account }
func (l *listOnlyProvider) Capabilities() []provider.Capability { return l.inner.Capabilities() }
func (l *listOnlyProvider) List(ctx context.Context) ([]provider.Machine, error) {
	ms, err := l.inner.List(ctx)
	for i := range ms {
		ms[i].Provider = "listonly"
	}
	return ms, err
}

func init() {
	provider.Register(provider.Type{
		Name: "listonly", Kind: provider.KindCloud, Description: "Fake provider without describe",
		Fields: []provider.Field{{Key: "password", Secret: true, Required: true}},
		New: func(account string, s map[string]string, _ provider.Options) (provider.Provider, error) {
			return &listOnlyProvider{inner: &fakeProvider{account: account, settings: s}}, nil
		},
	})
}

func TestDescribeServerText(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "describe", "server", "web-01", "--account", "one")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q\n%s", code, errOut, out)
	}
	want := `Name:      web-01
Ref:       fake/1
Account:   one
Provider:  fake
Kind:      cloud
Status:    running
Location:  eu-1
Product:   m1
IPv4:      203.0.113.10
IPv6:      2001:db8::/64
Labels:    env=prod

Power:
  State:        on
  Reset types:  soft
                hard
  Locked:       no
  Disk:         40 GB
  ISO:          <none>
`
	if out != want {
		t.Errorf("text output:\n got %q\nwant %q", out, want)
	}

	// two servers, separated by a blank line; aliases work
	out, _, code = e.run("", false, "describe", "srv", "web-01", "fake/1", "--account", "one")
	if code != 0 || strings.Count(out, "Name:      web-01") != 2 || !strings.Contains(out, "\n\nName:") {
		t.Errorf("two refs: code %d\n%s", code, out)
	}
}

func TestDescribeServerFormats(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, _, code := e.run("", false, "describe", "server", "web-01", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatal(out)
	}
	var ds []struct {
		provider.Machine
		Details []struct {
			Name   string         `json:"name"`
			Fields map[string]any `json:"fields"`
		} `json:"details"`
	}
	if err := json.Unmarshal([]byte(out), &ds); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(ds) != 1 || ds[0].Ref() != "fake/1" || len(ds[0].Details) != 1 || ds[0].Details[0].Name != "Power" ||
		ds[0].Details[0].Fields["state"] != "on" || ds[0].Details[0].Fields["disk"] != float64(40<<30) || ds[0].Details[0].Fields["locked"] != false {
		t.Errorf("unexpected JSON: %s", out)
	}
	if !strings.Contains(out, "\"disk\": 42949672960") {
		t.Errorf("sizes must be plain numbers in JSON:\n%s", out)
	}

	out, _, code = e.run("", false, "describe", "server", "web-01", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.HasPrefix(out, "- provider: fake\n  account: one\n") || !strings.Contains(out, "  details:\n    - name: Power\n      fields:\n        state: \"on\"\n        reset_types:\n          - soft\n") {
		t.Errorf("yaml: code %d\n%s", code, out)
	}

	_, errOut, code := e.run("", false, "describe", "server", "web-01", "-o", "wide")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("bad format: code %d, %q", code, errOut)
	}
}

func TestDescribeServerErrors(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	// not found
	out, errOut, code := e.run("", false, "describe", "server", "nope", "--account", "one")
	if code != 1 || out != "" || errOut != "ump: server \"nope\" not found\nump: 0 of 1 servers described\n" {
		t.Errorf("not found: code %d, out %q, stderr %q", code, out, errOut)
	}
	// JSON output stays an array even when nothing matched
	out, _, _ = e.run("", false, "describe", "server", "nope", "--account", "one", "-o", "json")
	if out != "[]\n" {
		t.Errorf("json for nothing: %q", out)
	}

	// ambiguous without a terminal: the same names exist in accounts one and three
	e.write(testConfig + "  three:\n    type: fake\n    login: carol\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "describe", "server", "web-01", "--account", "one,three")
	if code != 1 || !strings.Contains(errOut, `ump: server "web-01" is ambiguous: fake/1 (account one), fake/1 (account three); use provider/id or --account`) {
		t.Errorf("ambiguous: code %d, %q", code, errOut)
	}

	// provider error while describing
	_, errOut, code = e.run("", false, "describe", "server", "db-01", "--account", "one")
	if code != 4 || !strings.Contains(errOut, "ump: account one: fake: describe failed (HTTP 500)") {
		t.Errorf("provider error: code %d, %q", code, errOut)
	}

	// a failing account is reported, the lookup in the others still works, exit code 4
	out, errOut, code = e.run("", false, "describe", "server", "web-01", "--account", "one,two")
	if code != 4 || !strings.Contains(out, "Name:      web-01") || !strings.Contains(errOut, "account two: fake: authentication failed") {
		t.Errorf("failing account: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "describe", "server", "nope", "--account", "one,two")
	if code != 4 || !strings.Contains(errOut, `server "nope" not found (1 of 2 accounts could not be listed)`) {
		t.Errorf("not found with failing account: code %d, %q", code, errOut)
	}

	// usage
	_, errOut, code = e.run("", false, "describe", "server")
	if code != 2 || !strings.Contains(errOut, "requires at least 1 arg") {
		t.Errorf("no ref: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "describe", "pod", "x")
	if code != 2 || !strings.Contains(errOut, `unknown resource type "pod"`) {
		t.Errorf("unknown resource: code %d, %q", code, errOut)
	}
}

func TestDescribeServerWithoutDescriber(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	out, errOut, code := e.run("", false, "describe", "server", "web-01")
	if code != 0 || errOut != "" || !strings.Contains(out, "Ref:       listonly/1\n") || strings.Contains(out, "Power:") {
		t.Errorf("fallback: code %d, stderr %q\n%s", code, errOut, out)
	}
}

const variantB = "    variant: b\n"

func TestGetServersDiff(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	// first listing: nothing to compare, but the state is recorded
	out, errOut, code := e.run("", false, "get", "servers", "--diff", "--account", "one")
	if code != 0 || out != "" || errOut != "account one: first listing, nothing to compare yet (2 servers recorded)\n" {
		t.Errorf("first diff: code %d, out %q, stderr %q", code, out, errOut)
	}

	// an hour later nothing changed
	e.now = e.now.Add(time.Hour)
	out, errOut, code = e.run("", false, "get", "servers", "--diff", "--account", "one")
	if code != 0 || out != "" || errOut != "No changes since 2026-10-12 09:00 UTC (2 servers).\n" {
		t.Errorf("unchanged diff: code %d, out %q, stderr %q", code, out, errOut)
	}

	// then the world moves on
	e.write(strings.Replace(testConfig, "    password: {env: FAKE_PASS}\n", "    password: {env: FAKE_PASS}\n"+variantB, 1))
	e.now = e.now.Add(time.Hour)
	out, errOut, code = e.run("", false, "get", "servers", "--diff", "--account", "one")
	if code != 0 {
		t.Fatalf("diff: code %d, stderr %q\n%s", code, errOut, out)
	}
	want := `CHANGE    ACCOUNT   PROVIDER   ID   NAME       IPV4           LOCATION   STATUS         DETAILS
added     one       fake       3    cache-01   203.0.113.13   eu-1       initializing
removed   one       fake       2    db-01      203.0.113.11   eu-1       off
changed   one       fake       1    web-01     203.0.113.12   eu-1       off            ipv4: 203.0.113.10 → 203.0.113.12; status: running → off
`
	if out != want {
		t.Errorf("diff table:\n got %q\nwant %q", out, want)
	}
	if errOut != "1 added, 1 removed, 1 changed, 0 unchanged since 2026-10-12 10:00 UTC.\n" {
		t.Errorf("summary: %q", errOut)
	}

	// right after, nothing changed again; the JSON report says so
	e.now = e.now.Add(time.Hour)
	out, _, code = e.run("", false, "get", "servers", "--diff", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatal(out)
	}
	var report struct {
		Accounts  []accountDiff  `json:"accounts"`
		Changes   []cache.Change `json:"changes"`
		Unchanged int            `json:"unchanged"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(report.Accounts) != 1 || !report.Accounts[0].Compared || report.Accounts[0].Since == nil ||
		!report.Accounts[0].Since.Equal(time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)) || len(report.Changes) != 0 || report.Unchanged != 2 {
		t.Errorf("report: %s", out)
	}

	// --since reaches back to the earlier state; -o name marks the changes
	out, errOut, code = e.run("", false, "get", "servers", "--since", "3h", "--account", "one", "-o", "name")
	if code != 0 || out != "+ fake/3\n- fake/2\n~ fake/1\n" || errOut != "1 added, 1 removed, 1 changed, 0 unchanged since 2026-10-12 09:00 UTC.\n" {
		t.Errorf("--since 3h: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "get", "servers", "--since", "2026-10-12T10:30:00Z", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"change": "added"`) {
		t.Errorf("--since timestamp: code %d\n%s", code, out)
	}
	// before the history starts: not compared
	_, errOut, code = e.run("", false, "get", "servers", "--since", "1w", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "account one: no listing from before 2026-10-05 12:00 UTC (the cache starts 2026-10-12 09:00 UTC)") {
		t.Errorf("--since before history: code %d, %q", code, errOut)
	}
	// the JSON report explains a skipped account too
	out, _, _ = e.run("", false, "get", "servers", "--since", "1w", "--account", "one", "-o", "json")
	if !strings.Contains(out, `"compared": false`) || !strings.Contains(out, `"reason": "no listing from before`) {
		t.Errorf("json for skipped account:\n%s", out)
	}

	// a failing account is reported and the others are still compared
	_, errOut, code = e.run("", false, "get", "servers", "--diff")
	if code != 4 || !strings.Contains(errOut, "account two: fake: authentication failed") || !strings.Contains(errOut, "No changes since") {
		t.Errorf("failing account: code %d, %q", code, errOut)
	}

	// usage
	for _, args := range [][]string{
		{"get", "servers", "--diff", "web-01"},
		{"get", "servers", "--cached", "--diff"},
		{"get", "servers", "--cached", "--since", "1h"},
		{"get", "servers", "--since", "yesterday"},
	} {
		if _, _, code := e.run("", false, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func TestGetServersCached(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	_, errOut, code := e.run("", false, "get", "servers", "--cached", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "account one: not in the cache yet") {
		t.Errorf("empty cache: code %d, %q", code, errOut)
	}
	if _, _, code := e.run("", false, "get", "servers", "--account", "one"); code != 0 {
		t.Fatal("listing failed")
	}
	// the provider now answers differently, the cache still has the old listing
	e.write(strings.Replace(testConfig, "    password: {env: FAKE_PASS}\n", "    password: {env: FAKE_PASS}\n"+variantB, 1))
	e.now = e.now.Add(time.Hour)
	out, errOut, code := e.run("", false, "get", "servers", "--cached", "--account", "one")
	if code != 0 || errOut != "From cache: one (2026-10-12 09:00 UTC)\n" {
		t.Errorf("cached: code %d, stderr %q", code, errOut)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running"},
	})
	out, _, code = e.run("", false, "get", "servers", "--cached", "--account", "one", "db-01", "-o", "name")
	if code != 0 || out != "fake/2\n" {
		t.Errorf("cached lookup: code %d, %q", code, out)
	}
	// describe records listings as well
	if _, _, code := e.run("", false, "describe", "server", "web-01", "--account", "one"); code != 0 {
		t.Fatal("describe failed")
	}
	out, _, _ = e.run("", false, "get", "servers", "--cached", "--account", "one", "-o", "name")
	if out != "fake/3\nfake/1\n" {
		t.Errorf("cache after describe: %q", out)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"30m": now.Add(-30 * time.Minute), "12h": now.Add(-12 * time.Hour), "7d": now.AddDate(0, 0, -7), "2w": now.AddDate(0, 0, -14),
		"1h30m": now.Add(-90 * time.Minute), "2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01 10:30": time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC), "2026-10-01T10:30:00+02:00": time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseSince(in, now, time.UTC)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-1h", "7 days"} {
		if _, err := parseSince(bad, now, time.UTC); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDuplicateNames(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig + "  three:\n    type: fake\n    login: carol\n    password: s3cret\n")

	// a listing shows every match and says so
	out, errOut, code := e.run("", false, "get", "servers", "web-01", "--account", "one,three")
	if code != 0 || len(fields(out)) != 3 || errOut != "Note: \"web-01\" matches 2 servers: fake/1 (account one), fake/1 (account three)\n" {
		t.Errorf("get: code %d, stderr %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "get", "servers", "web-01", "--account", "one,three", "-o", "json")
	if code != 0 || strings.Count(out, `"name": "web-01"`) != 2 || !strings.HasPrefix(errOut, "Note:") || strings.Contains(out, "Note:") {
		t.Errorf("get json: code %d, stderr %q\n%s", code, errOut, out)
	}

	// on a terminal, describe asks which one; the question goes to stderr
	out, errOut, code = e.run("2\n", true, "describe", "server", "web-01", "--account", "one,three")
	if code != 0 {
		t.Fatalf("describe with choice: code %d, stderr %q\n%s", code, errOut, out)
	}
	for _, want := range []string{"Server \"web-01\" exists 2 times. Which one?\n", "  1) fake/1", "account one", "  2) fake/1", "account three", "  q) quit\n", "Choice: "} {
		if !strings.Contains(errOut, want) {
			t.Errorf("prompt lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out, "Choice") || !strings.HasPrefix(out, "Name:      web-01\nRef:       fake/1\nAccount:   three\n") {
		t.Errorf("stdout after choosing 2:\n%s", out)
	}
	// JSON stays clean when stdin is a terminal but stdout is piped
	out, _, code = e.run("1\n", true, "describe", "server", "web-01", "--account", "one,three", "-o", "json")
	if code != 0 || !strings.HasPrefix(out, "[\n  {\n    \"provider\": \"fake\",") || !strings.Contains(out, `"account": "one"`) {
		t.Errorf("json with prompt: code %d\n%s", code, out)
	}
	// an empty answer asks again; q aborts without describing anything
	out, errOut, code = e.run("\n1\n", true, "describe", "server", "web-01", "--account", "one,three")
	if code != 0 || strings.Count(errOut, "Choice: ") != 2 || !strings.Contains(errOut, "Please enter a number between 1 and 2, or q to quit.") || !strings.Contains(out, "Account:   one\n") {
		t.Errorf("empty answer: code %d, stderr %q", code, errOut)
	}
	out, errOut, code = e.run("q\n", true, "describe", "server", "web-01", "--account", "one,three")
	if code != 3 || out != "" || !strings.HasSuffix(errOut, "ump: aborted\n") {
		t.Errorf("quit: code %d, out %q, stderr %q", code, out, errOut)
	}
	// a unique reference never asks
	out, errOut, code = e.run("", true, "describe", "server", "db-01", "--account", "one")
	if code != 4 || strings.Contains(errOut, "Which one") {
		t.Errorf("unique ref: code %d, stderr %q", code, errOut)
	}
	_ = out
}

func TestRenameServer(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	out, errOut, code := e.run("", false, "rename", "server", "web-01", "prod-web-01", "--account", "one")
	if code != 0 || out != "Renamed fake/1 (account one) from web-01 to prod-web-01\n" ||
		errOut != "The hostname of the installed system is unchanged; ump install applies the new name.\n" {
		t.Errorf("rename: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "rename", "server", "fake/1", "prod-web-01.example.net", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"name": "prod-web-01.example.net"`) || !strings.HasPrefix(out, "[\n") {
		t.Errorf("rename json: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "rename", "server", "203.0.113.10", "x-01", "--account", "one", "-o", "name")
	if code != 0 || out != "fake/1\n" {
		t.Errorf("rename name output: code %d, %q", code, out)
	}

	// nothing to do when the name is already set
	out, errOut, code = e.run("", false, "rename", "server", "web-01", "web-01", "--account", "one")
	if code != 0 || out != "" || errOut != "fake/1 is already named web-01; nothing to do.\n" {
		t.Errorf("same name: code %d, out %q, stderr %q", code, out, errOut)
	}

	// a name another server already has is refused unless forced
	_, errOut, code = e.run("", false, "rename", "server", "web-01", "db-01", "--account", "one")
	if code != 1 || !strings.Contains(errOut, `the name "db-01" is already used by fake/2 (account one); pick another name or use --force`) {
		t.Errorf("duplicate refused: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "rename", "server", "web-01", "db-01", "--account", "one", "--force")
	if code != 0 || out != "Renamed fake/1 (account one) from web-01 to db-01\n" {
		t.Errorf("forced: code %d, %q", code, out)
	}

	// invalid names are rejected before any API call
	for _, bad := range []string{"Web-01", "web_01", ".web", "web-", "web..01", "", "a b"} {
		_, errOut, code = e.run("", false, "rename", "server", "web-01", bad, "--account", "one")
		if code != 2 || !strings.Contains(errOut, "invalid name") {
			t.Errorf("name %q: code %d, %q", bad, code, errOut)
		}
	}
	for _, good := range []string{"a", "web-01", "prod-web-01.example.net", "x1-y2"} {
		if err := validateServerName(good); err != nil {
			t.Errorf("name %q rejected: %v", good, err)
		}
	}
	if err := validateServerName(strings.Repeat("a", 64)); err == nil {
		t.Error("64-character label accepted")
	}

	// provider refusal, not found, usage
	_, errOut, code = e.run("", false, "rename", "server", "db-01", "new-01", "--account", "one")
	if code != 4 || !strings.Contains(errOut, "ump: account one: fake: name taken (uniqueness_error, HTTP 409)") {
		t.Errorf("provider error: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "rename", "server", "nope", "new-01", "--account", "one")
	if code != 1 || !strings.Contains(errOut, `server "nope" not found`) {
		t.Errorf("not found: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "rename", "server", "web-01")
	if code != 2 || !strings.Contains(errOut, "accepts 2 arg(s)") {
		t.Errorf("usage: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "rename", "server", "web-01", "x-01", "-o", "table", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("table output: code %d, %q", code, errOut)
	}

	// duplicates ask on a terminal and fail without one
	e.write(testConfig + "  three:\n    type: fake\n    login: carol\n    password: s3cret\n")
	out, errOut, code = e.run("2\n", true, "rename", "server", "web-01", "new-01", "--account", "one,three")
	if code != 0 || out != "Renamed fake/1 (account three) from web-01 to new-01\n" || !strings.Contains(errOut, "Which one?") {
		t.Errorf("prompted rename: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "rename", "server", "web-01", "new-01", "--account", "one,three")
	if code != 1 || !strings.Contains(errOut, `server "web-01" is ambiguous`) {
		t.Errorf("ambiguous: code %d, %q", code, errOut)
	}

	// a provider without rename support
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "rename", "server", "web-01", "new-01")
	if code != 1 || !strings.Contains(errOut, "provider type listonly cannot rename servers") {
		t.Errorf("no renamer: code %d, %q", code, errOut)
	}
}

func TestReverseDNSCommands(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	for k := range fakePTRs {
		delete(fakePTRs, k)
	}

	// nothing set yet: the primary IPv4 of every server is listed without an entry
	out, errOut, code := e.run("", false, "get", "rdns", "--account", "one")
	if code != 0 || errOut != "" {
		t.Fatalf("get rdns: code %d, stderr %q\n%s", code, errOut, out)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "SERVER", "IP", "PTR"},
		{"one", "db-01", "203.0.113.11", "<none>"},
		{"one", "web-01", "203.0.113.10", "<none>"},
	})

	// set with the server name as default, by name; then explicitly, by provider/id with an IPv6 address
	out, errOut, code = e.run("", false, "set", "rdns", "web-01", "--account", "one")
	if code != 0 || out != "Set reverse DNS of 203.0.113.10 (fake/1, account one) to web-01\n" || errOut != "" {
		t.Errorf("set default: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "set", "ptr", "fake/1", "web-01.example.net.", "--ip", "2001:db8::1", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"ip": "2001:db8::1"`) || !strings.Contains(out, `"ptr": "web-01.example.net"`) {
		t.Errorf("set with --ip json: code %d\n%s", code, out)
	}
	out, _, _ = e.run("", false, "get", "rdns", "web-01", "--account", "one")
	expectRows(t, out, [][]string{
		{"ACCOUNT", "SERVER", "IP", "PTR"},
		{"one", "web-01", "203.0.113.10", "web-01"},
		{"one", "web-01", "2001:db8::1", "web-01.example.net"},
	})
	out, _, code = e.run("", false, "get", "rdns", "web-01", "--account", "one", "-o", "json")
	if code != 0 || !strings.HasPrefix(out, "[\n  {\n    \"provider\": \"fake\",") || !strings.Contains(out, `"server": "web-01"`) {
		t.Errorf("get rdns json: code %d\n%s", code, out)
	}

	// delete, by default the primary IPv4; then the IPv6 entry
	out, _, code = e.run("", false, "delete", "rdns", "web-01", "--account", "one")
	if code != 0 || out != "Removed reverse DNS of 203.0.113.10 (fake/1, account one)\n" {
		t.Errorf("delete: code %d, %q", code, out)
	}
	if _, _, code = e.run("", false, "delete", "reverse-dns", "web-01", "--ip", "2001:db8::1", "--account", "one"); code != 0 {
		t.Errorf("delete --ip: code %d", code)
	}
	if len(fakePTRs) != 0 {
		t.Errorf("entries left: %v", fakePTRs)
	}

	// errors: foreign address, invalid name, invalid address, not found, provider failure, usage
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"set", "rdns", "web-01", "x.example.net", "--ip", "203.0.113.99", "--account", "one"}, 1, "does not belong to server"},
		{[]string{"set", "rdns", "web-01", "Bad_Name", "--account", "one"}, 2, "invalid name"},
		{[]string{"set", "rdns", "web-01", "--ip", "nope", "--account", "one"}, 2, "invalid address"},
		{[]string{"set", "rdns", "nope", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"set", "rdns"}, 2, "accepts between 1 and 2 arg(s)"},
		{[]string{"delete", "rdns", "web-01", "x", "--account", "one"}, 2, "accepts 1 arg(s)"},
		{[]string{"get", "rdns", "--account", "two", "-o", "name"}, 2, "unknown output format"},
		{[]string{"set", "pods", "x"}, 2, `unknown resource type "pods"`},
		{[]string{"delete", "pods", "x"}, 2, `unknown resource type "pods"`},
		{[]string{"get", "rdns", "nope", "--account", "one"}, 1, `server "nope" not found`},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	// a server whose lookup fails is reported, the others are listed, exit 4
	e.write(strings.Replace(testConfig, "    password: wrong\n", "    password: {env: FAKE_PASS}\n", 1))
	out, errOut, code = e.run("", false, "get", "rdns", "--account", "two")
	if code != 4 || !strings.Contains(errOut, "ump: account two: fake/2: fake: ptr lookup failed") || len(fields(out)) != 2 {
		t.Errorf("partial failure: code %d, stderr %q\n%s", code, errOut, out)
	}

	// a provider without reverse DNS support
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	out, errOut, code = e.run("", false, "get", "rdns")
	if code != 0 || out != "" || errOut != "account lo: provider type listonly cannot manage reverse DNS\n" {
		t.Errorf("unsupported get: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "set", "rdns", "web-01")
	if code != 1 || !strings.Contains(errOut, "provider type listonly cannot manage reverse DNS") {
		t.Errorf("unsupported set: code %d, %q", code, errOut)
	}
}

func TestConfigTypesCapabilities(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("", false, "config", "types")
	if code != 0 || !strings.Contains(out, "CAPABILITIES") {
		t.Fatalf("code %d\n%s", code, out)
	}
	table, _, _ := strings.Cut(out, "\n\n") // the settings section below repeats the type names
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "hetzner-robot", "hetzner-cloud", "ovh":
			if !strings.Contains(line, "inventory, describe, rename, rdns") {
				t.Errorf("%s: capabilities missing in %q", f[0], line)
			}
		case "listonly":
			if !strings.Contains(line, "<none>") {
				t.Errorf("listonly: %q", line)
			}
		}
	}
}

func resetFakePower() {
	fakePower.actions = nil
	fakePower.rescue = map[string]bool{}
	fakePower.keys = nil
}

func TestPowerCommands(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()

	out, errOut, code := e.run("", false, "reboot", "server", "web-01", "--yes", "--account", "one")
	if code != 0 || out != "Rebooting fake/1 (web-01, 203.0.113.10) (soft)\n" || errOut != "" {
		t.Errorf("reboot: code %d, out %q, stderr %q", code, out, errOut)
	}
	if _, _, code = e.run("", false, "reboot", "server", "fake/1", "--hard", "--yes", "--account", "one"); code != 0 {
		t.Errorf("reboot --hard: code %d", code)
	}
	if _, _, code = e.run("", false, "reboot", "server", "203.0.113.10", "--power-cycle", "--yes", "--account", "one"); code != 0 {
		t.Errorf("reboot --power-cycle: code %d", code)
	}
	out, _, code = e.run("", false, "start", "server", "web-01", "--account", "one")
	if code != 0 || out != "Switching on fake/1 (web-01, 203.0.113.10)\n" {
		t.Errorf("start: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "stop", "server", "web-01", "--yes", "--account", "one")
	if code != 0 || out != "Switching off fake/1 (web-01, 203.0.113.10) (shutdown)\n" {
		t.Errorf("stop: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "stop", "server", "web-01", "--hard", "--yes", "--account", "one")
	if code != 0 || out != "Switching off fake/1 (web-01, 203.0.113.10) (power off)\n" {
		t.Errorf("stop --hard: code %d, %q", code, out)
	}
	if got := strings.Join(fakePower.actions, ","); got != "reboot:soft,reboot:hard,reboot:power-cycle,on,off:false,off:true" {
		t.Errorf("actions: %s", got)
	}

	// confirmation: none without a terminal and --yes; a question on a terminal
	_, errOut, code = e.run("", false, "reboot", "server", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "ump reboot server needs confirmation: run it in a terminal or pass --yes") {
		t.Errorf("no --yes: code %d, %q", code, errOut)
	}
	out, errOut, code = e.run("y\n", true, "reboot", "server", "web-01", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Reboot fake/1 (web-01, 203.0.113.10) (soft)? [y/N]: ") || !strings.HasPrefix(out, "Rebooting") {
		t.Errorf("confirmed: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = e.run("n\n", true, "stop", "server", "web-01", "--account", "one")
	if code != 3 || out != "" || !strings.HasSuffix(errOut, "ump: aborted\n") {
		t.Errorf("declined: code %d, out %q, stderr %q", code, out, errOut)
	}
	if _, _, code = e.run("", false, "start", "server", "web-01", "--account", "one"); code != 0 {
		t.Error("start must not ask for confirmation")
	}

	// errors
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"reboot", "server", "web-01", "--hard", "--power-cycle", "--yes", "--account", "one"}, 2, "exclude each other"},
		{[]string{"reboot", "server", "db-01", "--yes", "--account", "one"}, 4, "fake: server is locked (locked, HTTP 423)"},
		{[]string{"reboot", "server", "nope", "--yes", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"reboot", "pods", "x"}, 2, `unknown resource type "pods"`},
		{[]string{"reboot", "server"}, 2, "accepts 1 arg(s)"},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "reboot", "server", "web-01", "--yes")
	if code != 1 || !strings.Contains(errOut, "provider type listonly cannot control power") {
		t.Errorf("no power: code %d, %q", code, errOut)
	}
}

func TestRescueCommands(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()
	keyFile := filepath.Join(t.TempDir(), "ops.pub")
	os.WriteFile(keyFile, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 ops@example\n"), 0o600)
	umpKey := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")

	// without a terminal and without a command: the key is generated, the
	// rescue system armed with it, the server reset, and the ssh command printed
	out, errOut, code := e.run("", false, "rescue", "server", "web-01", "--ssh-key", keyFile, "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("rescue: code %d, stderr %q\n%s", code, errOut, out)
	}
	if !strings.Contains(errOut, "Generated UMP's SSH key at ") {
		t.Errorf("key generation not reported: %q", errOut)
	}
	if fi, err := os.Stat(umpKey); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("ump key: %v %v", fi, err)
	}
	wantPrefix := "Rescue system enabled for fake/1 (web-01, 203.0.113.10); rebooting into it.\nSSH keys: " + umpKey + ".pub, " + keyFile + "\nPassword: r3scue\n"
	if !strings.HasPrefix(out, wantPrefix) {
		t.Errorf("rescue output:\n got %q\nwant prefix %q", out, wantPrefix)
	}
	if !strings.Contains(out, "Connect: ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@203.0.113.10\n") {
		t.Errorf("connect line missing:\n%s", out)
	}
	if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard" {
		t.Errorf("actions: %s", got)
	}
	if len(fakePower.keys) != 2 || !strings.HasPrefix(fakePower.keys[0], "ump") || fakePower.keys[1] != "ops@example d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81" {
		t.Errorf("keys sent to the provider: %v", fakePower.keys)
	}
	if len(e.sshCalls) != 0 {
		t.Errorf("ssh must not run without a terminal: %v", e.sshCalls)
	}

	// on a terminal the terminal is handed over to ssh
	out, errOut, code = e.run("", true, "rescue", "server", "web-01", "--yes", "--account", "one")
	if code != 0 || len(e.sshCalls) != 1 || !e.sshInteractive[0] || !strings.Contains(errOut, "Connecting to the rescue system of web-01 as root") {
		t.Errorf("hand-over: code %d, calls %v, interactive %v, stderr %q", code, e.sshCalls, e.sshInteractive, errOut)
	}
	if argv := e.sshCalls[0]; argv[0] != "ssh" || argv[len(argv)-1] != "root@203.0.113.10" || !strings.Contains(strings.Join(argv, " "), "-i "+umpKey+" -o IdentitiesOnly=yes") {
		t.Errorf("ssh argv: %v", argv)
	}
	if strings.Contains(out, "Connect:") {
		t.Errorf("connect line printed although connected:\n%s", out)
	}
	if strings.Contains(errOut, "Generated UMP's SSH key") {
		t.Error("the key was generated twice")
	}

	// a command after -- runs in the rescue system, its status becomes ump's
	e.sshCalls, e.sshInteractive, e.sshExit = nil, nil, 7
	_, errOut, code = e.run("", false, "rescue", "server", "web-01", "--yes", "--account", "one", "--", "lsblk", "-d")
	if code != 7 || strings.Contains(errOut, "ump:") || len(e.sshCalls) != 1 || e.sshInteractive[0] {
		t.Errorf("command: code %d, stderr %q, calls %v", code, errOut, e.sshCalls)
	}
	if argv := e.sshCalls[0]; strings.Join(argv[len(argv)-4:], " ") != "root@203.0.113.10 -- lsblk -d" {
		t.Errorf("command argv: %v", argv)
	}
	e.sshExit = 0

	// --no-connect, --no-reboot, --no-wait print the command for later; -o json carries it
	e.sshCalls, e.sshInteractive = nil, nil
	out, _, code = e.run("", true, "rescue", "server", "web-01", "--yes", "--no-connect", "--account", "one")
	if code != 0 || len(e.sshCalls) != 0 || !strings.Contains(out, "Connect: ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i ") {
		t.Errorf("--no-connect: code %d, calls %v\n%s", code, e.sshCalls, out)
	}
	resetFakePower()
	out, _, code = e.run("", true, "rescue", "server", "web-01", "--no-reboot", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(out, "it boots at the next reboot.") || !strings.Contains(out, "Connect once it is up: ssh -o ServerAliveInterval=5") || strings.Join(fakePower.actions, ",") != "rescue" || len(e.sshCalls) != 0 {
		t.Errorf("--no-reboot: code %d, actions %v, calls %v\n%s", code, fakePower.actions, e.sshCalls, out)
	}
	out, _, code = e.run("", true, "rescue", "server", "web-01", "--no-wait", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(out, "Connect once it is up: ssh -o ServerAliveInterval=5") || len(e.sshCalls) != 0 {
		t.Errorf("--no-wait: code %d\n%s", code, out)
	}
	out, _, code = e.run("", true, "rescue", "server", "web-01", "--yes", "--account", "one", "-o", "json")
	if code != 0 || len(e.sshCalls) != 0 || !strings.Contains(out, `"password": "r3scue"`) || !strings.Contains(out, `"ssh": "ssh -o ServerAliveInterval=5`) || !strings.Contains(out, umpKey+".pub") {
		t.Errorf("rescue json: code %d, calls %v\n%s", code, e.sshCalls, out)
	}

	// unrescue, with and without reboot
	out, _, code = e.run("", false, "unrescue", "server", "web-01", "--account", "one")
	if code != 0 || out != "Rescue system disabled for fake/1 (web-01, 203.0.113.10); the next boot is from disk.\n" {
		t.Errorf("unrescue: code %d, %q", code, out)
	}
	resetFakePower()
	out, _, code = e.run("", false, "unrescue", "server", "web-01", "--reboot", "--yes", "--account", "one")
	if code != 0 || out != "Rescue system disabled for fake/1 (web-01, 203.0.113.10); rebooting from disk.\n" || strings.Join(fakePower.actions, ",") != "unrescue,reboot:hard" {
		t.Errorf("unrescue --reboot: code %d, %q, actions %v", code, out, fakePower.actions)
	}
	if _, errOut, code := e.run("", false, "unrescue", "server", "web-01", "--reboot", "--account", "one"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("unrescue --reboot without --yes: code %d, %q", code, errOut)
	}

	// errors
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"rescue", "server", "web-01", "--yes", "--account", "one", "--ssh-key", "/nonexistent.pub"}, 1, "nonexistent.pub"},
		{[]string{"rescue", "server", "web-01", "--account", "one"}, 3, "needs confirmation"},
		{[]string{"rescue", "server", "web-01", "--yes", "-o", "table", "--account", "one"}, 2, "unknown output format"},
		{[]string{"rescue", "server", "nope", "--yes", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"rescue", "server", "web-01", "db-01", "--yes", "--account", "one"}, 2, "expected one server reference, got 2"},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "rescue", "server", "web-01", "--yes")
	if code != 1 || !strings.Contains(errOut, "provider type listonly has no rescue system") {
		t.Errorf("no rescue: code %d, %q", code, errOut)
	}
}

func TestSSHCommandAndKey(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	umpKey := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")

	// config key generates once and prints the public key
	out, errOut, code := e.run("", false, "config", "key")
	if code != 0 || !strings.Contains(errOut, "Generated UMP's SSH key") || !strings.Contains(out, "Public key:   ssh-ed25519 AAAA") || !strings.Contains(out, "Fingerprint:  SHA256:") {
		t.Errorf("config key: code %d, stderr %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "config", "key", "-o", "json")
	if code != 0 || errOut != "" || !strings.Contains(out, `"path": "`+umpKey+`"`) || !strings.Contains(out, `"fingerprint_md5"`) {
		t.Errorf("config key json: code %d, stderr %q\n%s", code, errOut, out)
	}
	pub, _ := os.ReadFile(umpKey + ".pub")
	if !strings.Contains(out, strings.TrimSpace(string(pub))) {
		t.Errorf("json lacks the public key line:\n%s", out)
	}

	// ssh server: a command without a terminal; UMP's key is offered next to
	// the user's existing default keys, without excluding the agent's
	home, _ := os.UserHomeDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("private"), 0o600)
	e.sshExit = 3
	_, errOut, code = e.run("", false, "ssh", "server", "web-01", "--account", "one", "--", "uname", "-a")
	if code != 3 || errOut != "" || len(e.sshCalls) != 1 || e.sshInteractive[0] {
		t.Errorf("ssh command: code %d, stderr %q, calls %v", code, errOut, e.sshCalls)
	}
	if argv := strings.Join(e.sshCalls[0], " "); argv != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -i "+filepath.Join(home, ".ssh", "id_ed25519")+" root@203.0.113.10 -- uname -a" {
		t.Errorf("ssh argv: %q", argv)
	}
	e.sshExit = 0
	// --staging uses UMP's key without host key checks; --user changes the login
	e.sshCalls, e.sshInteractive = nil, nil
	_, _, code = e.run("", false, "ssh", "server", "fake/1", "--staging", "--user", "deploy", "--account", "one", "--", "id")
	if code != 0 || len(e.sshCalls) != 1 {
		t.Fatalf("ssh --staging: code %d, calls %v", code, e.sshCalls)
	}
	if argv := strings.Join(e.sshCalls[0], " "); argv != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR deploy@203.0.113.10 -- id" {
		t.Errorf("ssh --staging argv: %q", argv)
	}
	// interactive needs a terminal
	e.sshCalls, e.sshInteractive = nil, nil
	_, errOut, code = e.run("", false, "ssh", "server", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "needs a terminal") || len(e.sshCalls) != 0 {
		t.Errorf("interactive without terminal: code %d, %q, calls %v", code, errOut, e.sshCalls)
	}
	_, _, code = e.run("", true, "ssh", "server", "web-01", "--account", "one")
	if code != 0 || len(e.sshCalls) != 1 || !e.sshInteractive[0] || strings.Join(e.sshCalls[0], " ") != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -i "+filepath.Join(home, ".ssh", "id_ed25519")+" root@203.0.113.10" {
		t.Errorf("interactive: code %d, calls %v %v", code, e.sshCalls, e.sshInteractive)
	}
	if strings.Contains(strings.Join(e.sshCalls[0], " "), "IdentitiesOnly") {
		t.Error("the default session must not exclude agent keys")
	}
	_, errOut, code = e.run("", false, "ssh", "server", "nope", "--account", "one", "--", "id")
	if code != 1 || !strings.Contains(errOut, `server "nope" not found`) {
		t.Errorf("not found: code %d, %q", code, errOut)
	}
	if got := shellJoin([]string{"ssh", "-o", "a b", "x'y", "root@h"}); got != `ssh -o 'a b' 'x'\''y' root@h` {
		t.Errorf("shellJoin = %q", got)
	}
}

func TestWaitForSSH(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	oldPort, oldTimeout, oldDown, oldPoll := sshPort, waitTimeout, waitDownTime, waitPoll
	defer func() { sshPort, waitTimeout, waitDownTime, waitPoll = oldPort, oldTimeout, oldDown, oldPoll }()
	sshPort = ln.Addr().(*net.TCPAddr).Port
	waitTimeout, waitDownTime, waitPoll = 300*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond

	var errOut bytes.Buffer
	a := &app{stdout: &bytes.Buffer{}, stderr: &errOut, probe: sshAnswers}
	if err := a.waitForSSH(context.Background(), "127.0.0.1", false); err != nil || !strings.Contains(errOut.String(), " up after ") {
		t.Errorf("listening: %v, %q", err, errOut.String())
	}
	errOut.Reset()
	if err := a.waitForSSH(context.Background(), "127.0.0.1", true); err != nil || !strings.Contains(errOut.String(), "still up; assuming it rebooted quickly") {
		t.Errorf("still up: %v, %q", err, errOut.String())
	}
	ln.Close()
	errOut.Reset()
	err = a.waitForSSH(context.Background(), "127.0.0.1", false)
	if err == nil || exitCodeFor(err) != exitUnreachable || !strings.Contains(err.Error(), "did not answer within") {
		t.Errorf("closed port: %v", err)
	}
	if err := a.waitForSSH(context.Background(), "", false); err == nil {
		t.Error("empty address accepted")
	}
}
