package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
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

// fakeKeyless makes the fake rescue system one that takes no key.
var fakeKeyless bool

func (f *fakeProvider) RescueTakesNoKey(m provider.Machine) bool { return fakeKeyless }

// fakeStock makes the fake provider able to reinstall a machine with a
// stock image; the machine then lets UMP in as root. fakeStockKeys is
// what the stock install says root takes, when not UMP's key.
var (
	fakeStock     bool
	fakeStockKeys string
)

func (f *fakeProvider) InstallStock(ctx context.Context, m provider.Machine, keys []provider.PublicKey) (*provider.StockInstall, error) {
	if !fakeStock {
		return nil, &provider.APIError{Provider: "fake", Message: "no stock images"}
	}
	fakePower.actions = append(fakePower.actions, "stock:"+keys[0].Name)
	return &provider.StockInstall{Image: "Debian 13", Address: m.IPv4, Keys: fakeStockKeys}, nil
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

// listGate, when set, is told when a listing of a type starts and ends, so
// that a test can see what runs at the same time.
var listGate func(typ string, start bool)

func (f *fakeProvider) List(ctx context.Context) ([]provider.Machine, error) {
	if listGate != nil {
		listGate(f.Type(), true)
		defer listGate(f.Type(), false)
	}
	return f.listMachines()
}

func (f *fakeProvider) listMachines() ([]provider.Machine, error) {
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

	// The fake machine behind the non-interactive ssh runs (probes, the
	// entry script, version checks); remoteCalls records the command lines
	// and remoteStdin what each got on stdin.
	remoteCalls [][]string
	remoteStdin []string
	// agentKeys is what the fake ssh-agent lists.
	agentKeys []string
	// editorHook plays the user's editor; nil edits nothing.
	editorHook func(path string) error
	// directRoot: the running system lets UMP in as root; inImage: it runs
	// the UMP rescue system already; rescueNoKey: the provider's rescue
	// system does not take UMP's key; entryExit: the entry script fails
	// with this status; entryNoMarker: the connection drops before the
	// script reports the jump; entryNoScript: the server cannot fetch the
	// entry script; versionAfter: how many version checks the kexec'd
	// system needs before it answers.
	directRoot bool
	// directSudo: the running system refuses root, but lets UMP in as
	// ump, who has sudo without a password
	directSudo bool
	stockRoot  bool
	// umpKeyDenied: UMP's key alone opens nothing on the running system
	// (the user's key does); keyAdds are the accounts UMP's key was added to
	umpKeyDenied  bool
	keyAdds       []string
	inImage       bool
	rescueNoKey   bool
	entryExit     int
	entryNoMarker bool
	entryNoScript bool
	versionAfter  int
	kexeced       bool
	versionPolls  int
	// the installer: installs records its command lines; installExit
	// makes it fail, installNoScript makes the fetch fail; reboots counts
	// the reboots asked over SSH; booted: the installed system answers
	// (bootFails: nothing with UMP's key answers after the reboot)
	installs        []string
	installExit     int
	installNoScript bool
	reboots         int
	installed       bool
	booted          bool
	bootFails       bool
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	os.MkdirAll(filepath.Join(dir, "home"), 0o700)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("UMP_STATE", "")
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

// readState returns the state file of the local home.
func (e *testEnv) readState() string {
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"))
	if err != nil {
		return ""
	}
	return string(data)
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
		editor: func(path string) error {
			if e.editorHook == nil {
				return nil
			}
			return e.editorHook(path)
		},
		agentKeys: func() ([]string, error) {
			if e.agentKeys == nil {
				return nil, errors.New("ssh-add -L lists no keys")
			}
			return e.agentKeys, nil
		},
		sshExec: func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			in := ""
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				in = string(b)
			}
			e.remoteCalls = append(e.remoteCalls, argv)
			e.remoteStdin = append(e.remoteStdin, in)
			out, errOut, code := e.remote(argv)
			io.WriteString(stdout, out)
			io.WriteString(stderr, errOut)
			return code, nil
		},
	}
	e.kexeced, e.versionPolls = false, 0
	e.installed, e.booted, e.reboots = false, false, 0
	if tty {
		a.isTTY = func() bool { return true }
	}
	code = a.run(context.Background(), append([]string{"--config", e.cfgPath}, args...))
	return out.String(), errb.String(), code
}

// remote plays the machine at the other end of a non-interactive ssh run.
func (e *testEnv) remote(argv []string) (stdout, stderr string, code int) {
	cmd := argv[len(argv)-1]
	inProviderRescue := fakePower.rescue["fake/1"]
	umpKeyOnly := strings.Contains(strings.Join(argv, " "), "IdentitiesOnly=yes")
	switch {
	case strings.HasPrefix(cmd, "umask 077; mkdir -p ~/.ssh"):
		for _, a := range argv {
			if strings.HasSuffix(a, "@203.0.113.10") {
				e.keyAdds = append(e.keyAdds, strings.TrimSuffix(a, "@203.0.113.10"))
			}
		}
		e.umpKeyDenied = false
		return "", "", 0
	case strings.HasPrefix(cmd, "id -u;") && umpKeyOnly && e.umpKeyDenied && !e.kexeced && !e.inImage:
		return "", "root@203.0.113.10: Permission denied (publickey).\n", 255
	case strings.HasPrefix(cmd, "id -u;"):
		switch {
		case e.kexeced || e.inImage:
			return "0\n2026.10.12\nDebian GNU/Linux 13 (trixie)\n", "", 0
		case inProviderRescue:
			if e.rescueNoKey {
				return "", "root@203.0.113.10: Permission denied (publickey,password).\n", 255
			}
			return "0\n-\nDebian GNU/Linux 12 (bookworm)\n", "", 0
		case e.directRoot:
			return "0\n-\nUbuntu 26.04 LTS\n", "", 0
		case e.directSudo && strings.Contains(strings.Join(argv, " "), " ump@"):
			return "1000\n-\nUbuntu 26.04 LTS\nsudo\n", "", 0
		case e.stockRoot && strings.Contains(strings.Join(fakePower.actions, ","), "stock:"):
			return "0\n-\nDebian GNU/Linux 13 (trixie)\n", "", 0
		}
		return "", "root@203.0.113.10: Permission denied (publickey,password).\n", 255
	case strings.Contains(cmd, "/kexec-into-rescue.sh"):
		if e.entryNoScript {
			return "", "curl: (22) The requested URL returned error: 403\n", 22
		}
		if e.entryExit != 0 {
			return "==> downloading the rescue image for amd64 from https://nowhere.invalid\n", "download failed: https://nowhere.invalid/amd64/manifest.json (HTTP 403)\n", e.entryExit
		}
		if e.entryNoMarker {
			return "==> downloading the rescue image for amd64\n", "Connection reset by peer\n", 255
		}
		e.kexeced = true
		return "==> downloading the rescue image for amd64\n==> booting into the UMP rescue system; reconnect as root@203.0.113.10\n", "Connection reset by peer\n", 255
	case strings.Contains(cmd, "/install/install.sh"):
		e.installs = append(e.installs, cmd)
		if e.installNoScript {
			return "", "curl: (22) The requested URL returned error: 404\n", 22
		}
		if e.installExit == 2 {
			return "", "install.sh: layout lvm-striped, part 4 (slice): no disk matches [\"nvme.*\"] (disks: /dev/sda)\n", 2
		}
		if e.installExit != 0 {
			return "==> machine: amd64, BIOS firmware, 2 CPUs, 937 MB RAM\n", "install.sh: no disk found\n", e.installExit
		}
		if strings.Contains(cmd, "--dry-run") {
			return "==> machine: amd64, BIOS firmware, 2 CPUs, 937 MB RAM\n==> dry run: stopping here, no disk was touched\n", "", 0
		}
		e.installed = true
		return "==> machine: amd64, BIOS firmware, 2 CPUs, 937 MB RAM\n==> installed: ubuntu-26.04 2026.10.12 (kernel 7.0.0-39-generic) as web-01 on vg0/root (17000 MB over 1 disk(s)); reboot to start it\n", "", 0
	case cmd == "systemctl reboot":
		e.reboots++
		if e.installed && !e.bootFails {
			e.kexeced, e.inImage, e.booted = false, false, true
		}
		return "", "Connection reset by peer\n", 255
	case strings.HasPrefix(cmd, "sed -n 's/^PRETTY_NAME=//p' /etc/os-release"):
		if e.booted {
			return "Ubuntu 26.04.1 LTS\n/dev/mapper/vg0-root\nos=ubuntu-26.04 hostname=web-01 \n", "", 0
		}
		return "", "root@203.0.113.10: Permission denied (publickey).\n", 255
	case cmd == "cat /etc/ump-rescue-version":
		if e.kexeced || e.inImage {
			e.versionPolls++
			if e.versionPolls > e.versionAfter {
				return "2026.10.12\n", "", 0
			}
		}
		return "", "cat: /etc/ump-rescue-version: No such file or directory\n", 1
	}
	return "", "unexpected command: " + cmd + "\n", 127
}

// entryCommands returns the entry script runs among the remote calls.
func (e *testEnv) entryCommands() []string {
	var out []string
	for _, argv := range e.remoteCalls {
		if cmd := argv[len(argv)-1]; strings.Contains(cmd, "/kexec-into-rescue.sh") {
			out = append(out, cmd)
		}
	}
	return out
}

// entryPrefix is how the entry command for a base URL starts: the server
// fetches the published script and runs it with the URL.
func entryPrefix(url string) string {
	return `f=/tmp/kexec-into-rescue.sh; if command -v curl >/dev/null; then curl -fsSL ` + url + `/kexec-into-rescue.sh -o "$f"; else wget -q -O "$f" ` + url + `/kexec-into-rescue.sh; fi && bash "$f" -u ` + url + ` `
}

// writeProfiles puts the repository's profiles into the configuration
// directory, where ump looks first.
func (e *testEnv) writeProfiles() {
	e.t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")
	os.MkdirAll(dir, 0o700)
	for _, name := range profileFiles {
		data, err := os.ReadFile(filepath.Join("..", "..", "profiles", name))
		if err != nil {
			e.t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
}

// lastSpec returns the specification the last installer run got.
func (e *testEnv) lastSpec() map[string]any {
	e.t.Helper()
	for i := len(e.remoteCalls) - 1; i >= 0; i-- {
		if strings.Contains(e.remoteCalls[i][len(e.remoteCalls[i])-1], "/install/install.sh") {
			var spec map[string]any
			if err := json.Unmarshal([]byte(e.remoteStdin[i]), &spec); err != nil {
				e.t.Fatalf("spec: %v\n%s", err, e.remoteStdin[i])
			}
			return spec
		}
	}
	e.t.Fatal("no installer run")
	return nil
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
	out, errOut, code := e.run("", false, "server", "list", "--account", "one")
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

	out, _, _ = e.run("", false, "server", "ls", "--account", "one", "--no-headers")
	if len(fields(out)) != 2 {
		t.Errorf("aliases or --no-headers broken:\n%s", out)
	}

	out, _, code = e.run("", false, "server", "list", "--account", "one", "-o", "wide")
	if code != 0 {
		t.Fatal(out)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS", "IPV6", "PRODUCT", "KIND", "PRICE", "ATTRIBUTES"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off", "<none>", "m2", "cloud", "<none>", "<none>"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running", "2001:db8::/64", "m1", "cloud", "<none>", "<none>"},
	})
}

func TestGetServersFormats(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, _, code := e.run("", false, "server", "list", "--account", "one", "-o", "json")
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

	out, _, _ = e.run("", false, "server", "list", "--account", "one", "-o", "yaml")
	if !strings.HasPrefix(out, "- provider: fake\n  account: one\n  id: \"2\"\n") {
		t.Errorf("unexpected YAML:\n%s", out)
	}

	out, _, _ = e.run("", false, "server", "list", "--account", "one", "-o", "name")
	if out != "fake/2\nfake/1\n" {
		t.Errorf("unexpected names: %q", out)
	}

	_, errOut, code := e.run("", false, "server", "list", "-o", "xml")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("bad format: code %d, %q", code, errOut)
	}

	e.write("defaults:\n  output: name\n" + testConfig)
	out, _, _ = e.run("", false, "server", "list", "--account", "one")
	if out != "fake/2\nfake/1\n" {
		t.Errorf("defaults.output ignored: %q", out)
	}
}

func TestGetServersRefs(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "server", "list", "--account", "one", "web-01", "fake/2", "203.0.113.11", "nope")
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
	out, _, code = e.run("", false, "server", "list", "--account", "one", "2001:db8::1", "-o", "name")
	if code != 0 || out != "fake/1\n" {
		t.Errorf("IPv6 prefix lookup: code %d, %q", code, out)
	}
}

func TestGetServersPartialFailure(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "server", "list")
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

	_, errOut, code = e.run("", false, "server", "list", "--account", "nope")
	if code != 2 || !strings.Contains(errOut, `account "nope" is not configured (configured: one, two)`) {
		t.Errorf("unknown account: code %d, %q", code, errOut)
	}
}

func TestGetServersNoConfig(t *testing.T) {
	e := newTestEnv(t)
	_, errOut, code := e.run("", false, "server", "list")
	if code != 1 || !strings.Contains(errOut, "run `ump config isp`") {
		t.Errorf("code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "pods")
	if code != 2 || !strings.Contains(errOut, `unknown command "pods" for ump server`) {
		t.Errorf("code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "list", "--bogus")
	if code != 2 || !strings.Contains(errOut, "unknown flag") {
		t.Errorf("code %d, %q", code, errOut)
	}
}

func TestConfigAddViewTestRemove(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	out, errOut, code := e.run("", false, "config", "isp", "add", "fake", "three", "--set", "login=carol", "--set", "password=env:FAKE_PASS", "--set", "region=us")
	if code != 0 || !strings.Contains(out, "Added account three (fake)") || !strings.Contains(out, "(2 servers)") {
		t.Fatalf("add: code %d, out %q, err %q", code, out, errOut)
	}
	if text := e.read(); !strings.Contains(text, "  three:\n    type: fake\n    login: carol\n    password: {env: FAKE_PASS}\n    region: us\n") {
		t.Errorf("saved config:\n%s", text)
	}

	out, _, code = e.run("", false, "config", "isp", "view")
	if code != 0 || !strings.HasPrefix(out, "# "+e.cfgPath+"\n") {
		t.Fatalf("view: code %d\n%s", code, out)
	}
	if !strings.Contains(out, "password: <redacted>") || strings.Contains(out, "wrong") ||
		!strings.Contains(out, "password: {env: FAKE_PASS}") || !strings.Contains(out, "login: alice") {
		t.Errorf("view redaction:\n%s", out)
	}

	out, errOut, code = e.run("", false, "config", "isp", "test")
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
	out, _, code = e.run("", false, "config", "isp", "test", "one")
	if code != 0 || len(fields(out)) != 2 {
		t.Errorf("test one: code %d\n%s", code, out)
	}

	// a secret file the wizard would have created is removed with the account
	secret := secretFilePath("three", "password")
	if err := config.WriteSecretFile(secret, "s3cret"); err != nil {
		t.Fatal(err)
	}
	e.write(e.read() + "  four:\n    type: fake\n    login: d\n    password: {file: " + secret + "}\n")
	out, _, code = e.run("", false, "config", "isp", "remove", "four")
	if code != 0 || !strings.Contains(out, "Removed account four (fake)") || !strings.Contains(out, "Removed secret file") {
		t.Errorf("remove: code %d\n%s", code, out)
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Errorf("secret file still exists: %v", err)
	}
	if strings.Contains(e.read(), "four:") {
		t.Error("account four still in config")
	}
	_, errOut, code = e.run("", false, "config", "isp", "remove", "four")
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
		{[]string{"config", "isp", "add", "nope", "x", "--set", "a=b"}, 2, "unknown provider type"},
		{[]string{"config", "isp", "add", "fake", "x", "--set", "bogus=1"}, 2, `has no setting "bogus"`},
		{[]string{"config", "isp", "add", "fake", "x", "--set", "login=a"}, 2, "password is required"},
		{[]string{"config", "isp", "add", "fake", "Bad Name", "--set", "login=a"}, 2, "invalid account name"},
		{[]string{"config", "isp", "add", "fake", "x", "--set", "login=a", "--set", "password=bad"}, 4, "not saved"},
		{[]string{"config", "isp", "add", "fake", "x", "--set", "login=a", "--set", "password=env:UMP_TEST_UNSET"}, 1, "UMP_TEST_UNSET is not set"},
		{[]string{"config", "isp", "add", "fake"}, 2, "either no arguments (the wizard) or TYPE NAME"},
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
	out, _, code := e.run("", false, "config", "isp", "add", "fake", "x", "--set", "login=a", "--set", "password=bad", "--no-test")
	if code != 0 || !strings.Contains(out, "Added account x") {
		t.Errorf("--no-test: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "config", "isp", "add", "fake", "x", "--set", "login=a", "--set", "password=s3cret")
	if code != 0 || !strings.Contains(out, "Replaced account x") {
		t.Errorf("replace: code %d, %q", code, out)
	}
}

func TestConfigTypesAndPath(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("", false, "config", "isp", "types")
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
}

func TestWizardNeedsTTY(t *testing.T) {
	e := newTestEnv(t)
	_, errOut, code := e.run("", false, "config", "isp", "add")
	if code != 3 || !strings.Contains(errOut, "needs a terminal") {
		t.Errorf("code %d, %q", code, errOut)
	}
	// bare, it is the help; the short forms work
	if out, _, code := e.run("", true, "config", "isp"); code != 0 || !strings.Contains(out, "Available Commands:") {
		t.Errorf("bare: code %d, %q", code, out)
	}
	if out, _, code := e.run("", true, "c", "i", "v"); code != 0 || !strings.Contains(out, "does not exist yet") {
		t.Errorf("short form: code %d, %q", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "isp", "add", "--set", "a=b"); code != 2 || !strings.Contains(errOut, "the flags need TYPE and NAME") {
		t.Errorf("flags without arguments: code %d, %q", code, errOut)
	}
}

func TestWizardFileStorage(t *testing.T) {
	e := newTestEnv(t)
	script := "\nfake\nacc\ncarol\ns3cret\n\nn\n"
	out, errOut, code := e.run(script, true, "config", "isp", "add")
	if code != 0 {
		t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"No accounts configured yet.", "Provider type", "2) fake", "Account name [fake]:",
		"The secrets are kept as files in the state (", "the login", "Login:", "Password:", "Region", "Testing acc ... ok (2 servers)",
		"Saved account acc (fake)", "Done. Try: ump server list"} {
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
	out, _, code = e.run("", false, "server", "list", "-o", "name")
	if code != 0 || out != "fake/2\nfake/1\n" {
		t.Errorf("listing with the wizard's config: code %d, %q", code, out)
	}

	// replacing an existing account asks first; declining asks for another name
	out, _, code = e.run("fake\nacc\nn\nacc2\ncarol\ns3cret\n\nn\n", true, "config", "isp", "add")
	if code != 0 || !strings.Contains(out, `Account "acc" (fake) exists. Replace it? [y/N]:`) || !strings.Contains(out, "Saved account acc2") {
		t.Errorf("replace prompt: code %d\n%s", code, out)
	}
}

func TestWizardFailedTestIsNotSaved(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("\nfake\nacc\ncarol\nbad\n\nn\nn\n", true, "config", "isp", "add")
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
	_, errOut, code := e.run("\nfake\n", true, "config", "isp", "add")
	if code != 3 || !strings.Contains(errOut, "input closed") {
		t.Errorf("EOF: code %d, %q", code, errOut)
	}
}

func TestWizardQuit(t *testing.T) {
	e := newTestEnv(t)
	// q at the provider menu: nothing to add
	out, errOut, code := e.run("\nq\n", true, "config", "isp", "add")
	if code != 0 || errOut != "" || !strings.Contains(out, "  q) quit\n") || !strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit at menu: code %d, stderr %q\n%s", code, errOut, out)
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}
	// q in the middle of adding an account (at the region choice): nothing saved
	out, _, code = e.run("\nfake\nacc\ncarol\ns3cret\nq\n", true, "config", "isp", "add")
	if code != 0 || !strings.Contains(out, "Nothing saved for this account.") || !strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit mid-account: code %d\n%s", code, out)
	}
	if _, err := os.Stat(e.cfgPath); !os.IsNotExist(err) {
		t.Error("config file was created")
	}
	// "exit" at a choice field, after the secret was typed
	out, _, code = e.run("\nfake\nacc\ncarol\ns3cret\nexit\n", true, "config", "isp", "add")
	if code != 0 || !strings.Contains(out, "Nothing saved for this account.") {
		t.Errorf("exit at choice: code %d\n%s", code, out)
	}
	if _, err := os.Stat(secretFilePath("acc", "password")); !os.IsNotExist(err) {
		t.Error("secret file was written although the account was not saved")
	}
	// q at the yes/no question after a save keeps what was saved
	out, _, code = e.run("\nfake\nacc\ncarol\ns3cret\n\nq\n", true, "config", "isp", "add")
	if code != 0 || !strings.Contains(out, "Saved account acc (fake)") || !strings.Contains(out, "Done. Try: ump server list") ||
		strings.Contains(out, "Nothing changed.") {
		t.Errorf("quit after save: code %d\n%s", code, out)
	}
	if !strings.Contains(e.read(), "  acc:\n    type: fake\n") {
		t.Errorf("config:\n%s", e.read())
	}
	// q at the replace question
	out, _, code = e.run("fake\nacc\nq\n", true, "config", "isp", "add")
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
	code := a.run(ctx, []string{"--config", e.cfgPath, "config", "isp", "add"})
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
		stdin:  strings.NewReader("\nfake\nacc\ncarol\n"),
		stdout: &out,
		stderr: &errOut,
		isTTY:  func() bool { return true },
		readSecret: func() (string, error) {
			cancel()  // "Ctrl-C" arrives while the read is pending
			select {} // and the read stays blocked forever
		},
		saveTerm: func() func() { return func() { restored = true } },
	}
	code = a.run(ctx, []string{"--config", e.cfgPath, "config", "isp", "add"})
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
	code = a.run(ctx, []string{"--config", e.cfgPath, "server", "list"})
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
	if listGate != nil {
		listGate("listonly", true)
		defer listGate("listonly", false)
	}
	ms, err := l.inner.listMachines()
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

// noRescueProvider is a fake without any rescue system that can install
// a stock image, as at Vultr.
type noRescueProvider struct{ inner *fakeProvider }

func (n *noRescueProvider) Type() string    { return "norescue" }
func (n *noRescueProvider) Account() string { return n.inner.account }
func (n *noRescueProvider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapInventory, provider.CapPower}
}
func (n *noRescueProvider) List(ctx context.Context) ([]provider.Machine, error) {
	ms, err := n.inner.List(ctx)
	for i := range ms {
		ms[i].Provider = "norescue"
	}
	return ms, err
}
func (n *noRescueProvider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	return n.inner.PowerState(ctx, m)
}
func (n *noRescueProvider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	return n.inner.Reboot(ctx, m, mode)
}
func (n *noRescueProvider) PowerOn(ctx context.Context, m provider.Machine) error {
	return n.inner.PowerOn(ctx, m)
}
func (n *noRescueProvider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	return n.inner.PowerOff(ctx, m, hard)
}
func (n *noRescueProvider) InstallStock(ctx context.Context, m provider.Machine, keys []provider.PublicKey) (*provider.StockInstall, error) {
	return n.inner.InstallStock(ctx, m, keys)
}

func init() {
	provider.Register(provider.Type{
		Name: "norescue", Kind: provider.KindCloud, Description: "Fake provider without a rescue system",
		Fields: []provider.Field{{Key: "password", Secret: true, Required: true}},
		New: func(account string, s map[string]string, _ provider.Options) (provider.Provider, error) {
			return &noRescueProvider{inner: &fakeProvider{account: account, settings: s}}, nil
		},
	})
}

// keylessProvider is a fake whose rescue system takes no key and that
// cannot install a stock image either: nothing but the refusal is left.
type keylessProvider struct{ inner *fakeProvider }

func (k *keylessProvider) Type() string                        { return "keyless" }
func (k *keylessProvider) Account() string                     { return k.inner.account }
func (k *keylessProvider) Capabilities() []provider.Capability { return k.inner.Capabilities() }
func (k *keylessProvider) List(ctx context.Context) ([]provider.Machine, error) {
	ms, err := k.inner.List(ctx)
	for i := range ms {
		ms[i].Provider = "keyless"
	}
	return ms, err
}
func (k *keylessProvider) RescueTakesNoKey(m provider.Machine) bool { return true }
func (k *keylessProvider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	return k.inner.RescueEnabled(ctx, m)
}
func (k *keylessProvider) EnableRescue(ctx context.Context, m provider.Machine, o provider.RescueOptions) (*provider.RescueInfo, error) {
	return k.inner.EnableRescue(ctx, m, o)
}
func (k *keylessProvider) DisableRescue(ctx context.Context, m provider.Machine) error {
	return k.inner.DisableRescue(ctx, m)
}
func (k *keylessProvider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	return k.inner.PowerState(ctx, m)
}
func (k *keylessProvider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	return k.inner.Reboot(ctx, m, mode)
}
func (k *keylessProvider) PowerOn(ctx context.Context, m provider.Machine) error {
	return k.inner.PowerOn(ctx, m)
}
func (k *keylessProvider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	return k.inner.PowerOff(ctx, m, hard)
}

func init() {
	provider.Register(provider.Type{
		Name: "keyless", Kind: provider.KindCloud, Description: "Fake provider whose rescue takes no key",
		Fields: []provider.Field{{Key: "password", Secret: true, Required: true}},
		New: func(account string, s map[string]string, _ provider.Options) (provider.Provider, error) {
			return &keylessProvider{inner: &fakeProvider{account: account, settings: s}}, nil
		},
	})
}

func TestDescribeServerText(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, errOut, code := e.run("", false, "server", "show", "web-01", "--account", "one")
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
	out, _, code = e.run("", false, "server", "show", "web-01", "fake/1", "--account", "one")
	if code != 0 || strings.Count(out, "Name:      web-01") != 2 || !strings.Contains(out, "\n\nName:") {
		t.Errorf("two refs: code %d\n%s", code, out)
	}
}

func TestDescribeServerFormats(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, _, code := e.run("", false, "server", "show", "web-01", "--account", "one", "-o", "json")
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

	out, _, code = e.run("", false, "server", "show", "web-01", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.HasPrefix(out, "- provider: fake\n  account: one\n") || !strings.Contains(out, "  details:\n    - name: Power\n      fields:\n        state: \"on\"\n        reset_types:\n          - soft\n") {
		t.Errorf("yaml: code %d\n%s", code, out)
	}

	_, errOut, code := e.run("", false, "server", "show", "web-01", "-o", "wide")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("bad format: code %d, %q", code, errOut)
	}
}

func TestDescribeServerErrors(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	// not found
	out, errOut, code := e.run("", false, "server", "show", "nope", "--account", "one")
	if code != 1 || out != "" || errOut != "ump: server \"nope\" not found\nump: 0 of 1 servers described\n" {
		t.Errorf("not found: code %d, out %q, stderr %q", code, out, errOut)
	}
	// JSON output stays an array even when nothing matched
	out, _, _ = e.run("", false, "server", "show", "nope", "--account", "one", "-o", "json")
	if out != "[]\n" {
		t.Errorf("json for nothing: %q", out)
	}

	// ambiguous without a terminal: the same names exist in accounts one and three
	e.write(testConfig + "  three:\n    type: fake\n    login: carol\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "server", "show", "web-01", "--account", "one,three")
	if code != 1 || !strings.Contains(errOut, `ump: server "web-01" is ambiguous: fake/1 (account one), fake/1 (account three); use provider/id or --account`) {
		t.Errorf("ambiguous: code %d, %q", code, errOut)
	}

	// provider error while describing
	_, errOut, code = e.run("", false, "server", "show", "db-01", "--account", "one")
	if code != 4 || !strings.Contains(errOut, "ump: account one: fake: describe failed (HTTP 500)") {
		t.Errorf("provider error: code %d, %q", code, errOut)
	}

	// a failing account is reported, the lookup in the others still works, exit code 4
	out, errOut, code = e.run("", false, "server", "show", "web-01", "--account", "one,two")
	if code != 4 || !strings.Contains(out, "Name:      web-01") || !strings.Contains(errOut, "account two: fake: authentication failed") {
		t.Errorf("failing account: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "server", "show", "nope", "--account", "one,two")
	if code != 4 || !strings.Contains(errOut, `server "nope" not found (1 of 2 accounts could not be listed)`) {
		t.Errorf("not found with failing account: code %d, %q", code, errOut)
	}

	// usage
	_, errOut, code = e.run("", false, "server", "show")
	if code != 2 || !strings.Contains(errOut, "requires at least 1 arg") {
		t.Errorf("no ref: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "show")
	if code != 2 || !strings.Contains(errOut, "requires at least 1 arg") {
		t.Errorf("unknown resource: code %d, %q", code, errOut)
	}
}

func TestDescribeServerWithoutDescriber(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	out, errOut, code := e.run("", false, "server", "show", "web-01")
	if code != 0 || errOut != "" || !strings.Contains(out, "Ref:       listonly/1\n") || strings.Contains(out, "Power:") {
		t.Errorf("fallback: code %d, stderr %q\n%s", code, errOut, out)
	}
}

const variantB = "    variant: b\n"

func TestGetServersDiff(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	// first listing: nothing to compare, but the state is recorded
	out, errOut, code := e.run("", false, "server", "list", "--diff", "--account", "one")
	if code != 0 || out != "" || errOut != "account one: first listing, nothing to compare yet (2 servers recorded)\n" {
		t.Errorf("first diff: code %d, out %q, stderr %q", code, out, errOut)
	}

	// an hour later nothing changed
	e.now = e.now.Add(time.Hour)
	out, errOut, code = e.run("", false, "server", "list", "--diff", "--account", "one")
	if code != 0 || out != "" || errOut != "No changes since 2026-10-12 09:00 UTC (2 servers).\n" {
		t.Errorf("unchanged diff: code %d, out %q, stderr %q", code, out, errOut)
	}

	// then the world moves on
	e.write(strings.Replace(testConfig, "    password: {env: FAKE_PASS}\n", "    password: {env: FAKE_PASS}\n"+variantB, 1))
	e.now = e.now.Add(time.Hour)
	out, errOut, code = e.run("", false, "server", "list", "--diff", "--account", "one")
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
	out, _, code = e.run("", false, "server", "list", "--diff", "--account", "one", "-o", "json")
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
	out, errOut, code = e.run("", false, "server", "list", "--since", "3h", "--account", "one", "-o", "name")
	if code != 0 || out != "+ fake/3\n- fake/2\n~ fake/1\n" || errOut != "1 added, 1 removed, 1 changed, 0 unchanged since 2026-10-12 09:00 UTC.\n" {
		t.Errorf("--since 3h: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "server", "list", "--since", "2026-10-12T10:30:00Z", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"change": "added"`) {
		t.Errorf("--since timestamp: code %d\n%s", code, out)
	}
	// before the history starts: not compared
	_, errOut, code = e.run("", false, "server", "list", "--since", "1w", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "account one: no listing from before 2026-10-05 12:00 UTC (the cache starts 2026-10-12 09:00 UTC)") {
		t.Errorf("--since before history: code %d, %q", code, errOut)
	}
	// the JSON report explains a skipped account too
	out, _, _ = e.run("", false, "server", "list", "--since", "1w", "--account", "one", "-o", "json")
	if !strings.Contains(out, `"compared": false`) || !strings.Contains(out, `"reason": "no listing from before`) {
		t.Errorf("json for skipped account:\n%s", out)
	}

	// a failing account is reported and the others are still compared
	_, errOut, code = e.run("", false, "server", "list", "--diff")
	if code != 4 || !strings.Contains(errOut, "account two: fake: authentication failed") || !strings.Contains(errOut, "No changes since") {
		t.Errorf("failing account: code %d, %q", code, errOut)
	}

	// usage
	for _, args := range [][]string{
		{"server", "list", "--diff", "web-01"},
		{"server", "list", "--cached", "--diff"},
		{"server", "list", "--cached", "--since", "1h"},
		{"server", "list", "--since", "yesterday"},
	} {
		if _, _, code := e.run("", false, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func TestGetServersCached(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	_, errOut, code := e.run("", false, "server", "list", "--cached", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "account one: not in the cache yet") {
		t.Errorf("empty cache: code %d, %q", code, errOut)
	}
	if _, _, code := e.run("", false, "server", "list", "--account", "one"); code != 0 {
		t.Fatal("listing failed")
	}
	// the provider now answers differently, the cache still has the old listing
	e.write(strings.Replace(testConfig, "    password: {env: FAKE_PASS}\n", "    password: {env: FAKE_PASS}\n"+variantB, 1))
	e.now = e.now.Add(time.Hour)
	out, errOut, code := e.run("", false, "server", "list", "--cached", "--account", "one")
	if code != 0 || errOut != "From cache: one (2026-10-12 09:00 UTC)\n" {
		t.Errorf("cached: code %d, stderr %q", code, errOut)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "PROVIDER", "ID", "NAME", "IPV4", "LOCATION", "STATUS"},
		{"one", "fake", "2", "db-01", "203.0.113.11", "eu-1", "off"},
		{"one", "fake", "1", "web-01", "203.0.113.10", "eu-1", "running"},
	})
	out, _, code = e.run("", false, "server", "list", "--cached", "--account", "one", "db-01", "-o", "name")
	if code != 0 || out != "fake/2\n" {
		t.Errorf("cached lookup: code %d, %q", code, out)
	}
	// describe records listings as well
	if _, _, code := e.run("", false, "server", "show", "web-01", "--account", "one"); code != 0 {
		t.Fatal("describe failed")
	}
	out, _, _ = e.run("", false, "server", "list", "--cached", "--account", "one", "-o", "name")
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
	out, errOut, code := e.run("", false, "server", "list", "web-01", "--account", "one,three")
	if code != 0 || len(fields(out)) != 3 || errOut != "Note: \"web-01\" matches 2 servers: fake/1 (account one), fake/1 (account three)\n" {
		t.Errorf("get: code %d, stderr %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "server", "list", "web-01", "--account", "one,three", "-o", "json")
	if code != 0 || strings.Count(out, `"name": "web-01"`) != 2 || !strings.HasPrefix(errOut, "Note:") || strings.Contains(out, "Note:") {
		t.Errorf("get json: code %d, stderr %q\n%s", code, errOut, out)
	}

	// on a terminal, describe asks which one; the question goes to stderr
	out, errOut, code = e.run("2\n", true, "server", "show", "web-01", "--account", "one,three")
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
	out, _, code = e.run("1\n", true, "server", "show", "web-01", "--account", "one,three", "-o", "json")
	if code != 0 || !strings.HasPrefix(out, "[\n  {\n    \"provider\": \"fake\",") || !strings.Contains(out, `"account": "one"`) {
		t.Errorf("json with prompt: code %d\n%s", code, out)
	}
	// an empty answer asks again; q aborts without describing anything
	out, errOut, code = e.run("\n1\n", true, "server", "show", "web-01", "--account", "one,three")
	if code != 0 || strings.Count(errOut, "Choice: ") != 2 || !strings.Contains(errOut, "Please enter a number between 1 and 2, or q to quit.") || !strings.Contains(out, "Account:   one\n") {
		t.Errorf("empty answer: code %d, stderr %q", code, errOut)
	}
	out, errOut, code = e.run("q\n", true, "server", "show", "web-01", "--account", "one,three")
	if code != 3 || out != "" || !strings.HasSuffix(errOut, "ump: aborted\n") {
		t.Errorf("quit: code %d, out %q, stderr %q", code, out, errOut)
	}
	// a unique reference never asks
	out, errOut, code = e.run("", true, "server", "show", "db-01", "--account", "one")
	if code != 4 || strings.Contains(errOut, "Which one") {
		t.Errorf("unique ref: code %d, stderr %q", code, errOut)
	}
	_ = out
}

func TestRenameServer(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)

	out, errOut, code := e.run("", false, "server", "rename", "web-01", "prod-web-01", "--account", "one")
	if code != 0 || out != "Renamed fake/1 (account one) from web-01 to prod-web-01\n" ||
		errOut != "The hostname of the installed system is unchanged; ump install applies the new name.\n" {
		t.Errorf("rename: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "server", "rename", "fake/1", "prod-web-01.example.net", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"name": "prod-web-01.example.net"`) || !strings.HasPrefix(out, "[\n") {
		t.Errorf("rename json: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "server", "rename", "203.0.113.10", "x-01", "--account", "one", "-o", "name")
	if code != 0 || out != "fake/1\n" {
		t.Errorf("rename name output: code %d, %q", code, out)
	}

	// nothing to do when the name is already set
	out, errOut, code = e.run("", false, "server", "rename", "web-01", "web-01", "--account", "one")
	if code != 0 || out != "" || errOut != "fake/1 is already named web-01; nothing to do.\n" {
		t.Errorf("same name: code %d, out %q, stderr %q", code, out, errOut)
	}

	// a name another server already has is refused unless forced
	_, errOut, code = e.run("", false, "server", "rename", "web-01", "db-01", "--account", "one")
	if code != 1 || !strings.Contains(errOut, `the name "db-01" is already used by fake/2 (account one); pick another name or use --force`) {
		t.Errorf("duplicate refused: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "server", "rename", "web-01", "db-01", "--account", "one", "--force")
	if code != 0 || out != "Renamed fake/1 (account one) from web-01 to db-01\n" {
		t.Errorf("forced: code %d, %q", code, out)
	}

	// invalid names are rejected before any API call
	for _, bad := range []string{"Web-01", "web_01", ".web", "web-", "web..01", "", "a b"} {
		_, errOut, code = e.run("", false, "server", "rename", "web-01", bad, "--account", "one")
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
	_, errOut, code = e.run("", false, "server", "rename", "db-01", "new-01", "--account", "one")
	if code != 4 || !strings.Contains(errOut, "ump: account one: fake: name taken (uniqueness_error, HTTP 409)") {
		t.Errorf("provider error: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "rename", "nope", "new-01", "--account", "one")
	if code != 1 || !strings.Contains(errOut, `server "nope" not found`) {
		t.Errorf("not found: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "rename", "web-01")
	if code != 2 || !strings.Contains(errOut, "accepts 2 arg(s)") {
		t.Errorf("usage: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "rename", "web-01", "x-01", "-o", "table", "--account", "one")
	if code != 2 || !strings.Contains(errOut, "unknown output format") {
		t.Errorf("table output: code %d, %q", code, errOut)
	}

	// duplicates ask on a terminal and fail without one
	e.write(testConfig + "  three:\n    type: fake\n    login: carol\n    password: s3cret\n")
	out, errOut, code = e.run("2\n", true, "server", "rename", "web-01", "new-01", "--account", "one,three")
	if code != 0 || out != "Renamed fake/1 (account three) from web-01 to new-01\n" || !strings.Contains(errOut, "Which one?") {
		t.Errorf("prompted rename: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "server", "rename", "web-01", "new-01", "--account", "one,three")
	if code != 1 || !strings.Contains(errOut, `server "web-01" is ambiguous`) {
		t.Errorf("ambiguous: code %d, %q", code, errOut)
	}

	// a provider without rename support
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "server", "rename", "web-01", "new-01")
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
	out, errOut, code := e.run("", false, "server", "rdns", "get", "--account", "one")
	if code != 0 || errOut != "" {
		t.Fatalf("get rdns: code %d, stderr %q\n%s", code, errOut, out)
	}
	expectRows(t, out, [][]string{
		{"ACCOUNT", "SERVER", "IP", "PTR"},
		{"one", "db-01", "203.0.113.11", "<none>"},
		{"one", "web-01", "203.0.113.10", "<none>"},
	})

	// set with the server name as default, by name; then explicitly, by provider/id with an IPv6 address
	out, errOut, code = e.run("", false, "server", "rdns", "set", "web-01", "--account", "one")
	if code != 0 || out != "Set reverse DNS of 203.0.113.10 (fake/1, account one) to web-01\n" || errOut != "" {
		t.Errorf("set default: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, _, code = e.run("", false, "server", "ptr", "set", "fake/1", "web-01.example.net.", "--ip", "2001:db8::1", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"ip": "2001:db8::1"`) || !strings.Contains(out, `"ptr": "web-01.example.net"`) {
		t.Errorf("set with --ip json: code %d\n%s", code, out)
	}
	out, _, _ = e.run("", false, "server", "rdns", "get", "web-01", "--account", "one")
	expectRows(t, out, [][]string{
		{"ACCOUNT", "SERVER", "IP", "PTR"},
		{"one", "web-01", "203.0.113.10", "web-01"},
		{"one", "web-01", "2001:db8::1", "web-01.example.net"},
	})
	out, _, code = e.run("", false, "server", "rdns", "get", "web-01", "--account", "one", "-o", "json")
	if code != 0 || !strings.HasPrefix(out, "[\n  {\n    \"provider\": \"fake\",") || !strings.Contains(out, `"server": "web-01"`) {
		t.Errorf("get rdns json: code %d\n%s", code, out)
	}

	// delete, by default the primary IPv4; then the IPv6 entry
	out, _, code = e.run("", false, "server", "rdns", "delete", "web-01", "--account", "one")
	if code != 0 || out != "Removed reverse DNS of 203.0.113.10 (fake/1, account one)\n" {
		t.Errorf("delete: code %d, %q", code, out)
	}
	if _, _, code = e.run("", false, "server", "reverse-dns", "rm", "web-01", "--ip", "2001:db8::1", "--account", "one"); code != 0 {
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
		{[]string{"server", "rdns", "set", "web-01", "x.example.net", "--ip", "203.0.113.99", "--account", "one"}, 1, "does not belong to server"},
		{[]string{"server", "rdns", "set", "web-01", "Bad_Name", "--account", "one"}, 2, "invalid name"},
		{[]string{"server", "rdns", "set", "web-01", "--ip", "nope", "--account", "one"}, 2, "invalid address"},
		{[]string{"server", "rdns", "set", "nope", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"server", "rdns", "set"}, 2, "accepts between 1 and 2 arg(s)"},
		{[]string{"server", "rdns", "delete", "web-01", "x", "--account", "one"}, 2, "accepts 1 arg(s)"},
		{[]string{"server", "rdns", "get", "--account", "two", "-o", "name"}, 2, "unknown output format"},
		{[]string{"server", "rdns", "pods", "x"}, 2, `unknown command "pods"`},
		{[]string{"server", "rdns", "get", "nope", "--account", "one"}, 1, `server "nope" not found`},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	// a server whose lookup fails is reported, the others are listed, exit 4
	e.write(strings.Replace(testConfig, "    password: wrong\n", "    password: {env: FAKE_PASS}\n", 1))
	out, errOut, code = e.run("", false, "server", "rdns", "get", "--account", "two")
	if code != 4 || !strings.Contains(errOut, "ump: account two: fake/2: fake: ptr lookup failed") || len(fields(out)) != 2 {
		t.Errorf("partial failure: code %d, stderr %q\n%s", code, errOut, out)
	}

	// a provider without reverse DNS support
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	out, errOut, code = e.run("", false, "server", "rdns", "get")
	if code != 0 || out != "" || errOut != "account lo: provider type listonly cannot manage reverse DNS\n" {
		t.Errorf("unsupported get: code %d, out %q, stderr %q", code, out, errOut)
	}
	_, errOut, code = e.run("", false, "server", "rdns", "set", "web-01")
	if code != 1 || !strings.Contains(errOut, "provider type listonly cannot manage reverse DNS") {
		t.Errorf("unsupported set: code %d, %q", code, errOut)
	}
}

func TestConfigTypesCapabilities(t *testing.T) {
	e := newTestEnv(t)
	out, _, code := e.run("", false, "config", "isp", "types")
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

	out, errOut, code := e.run("", false, "server", "reboot", "web-01", "--yes", "--account", "one")
	if code != 0 || out != "Rebooting fake/1 (web-01, 203.0.113.10) (soft)\n" || errOut != "" {
		t.Errorf("reboot: code %d, out %q, stderr %q", code, out, errOut)
	}
	if _, _, code = e.run("", false, "server", "reboot", "fake/1", "--hard", "--yes", "--account", "one"); code != 0 {
		t.Errorf("reboot --hard: code %d", code)
	}
	if _, _, code = e.run("", false, "server", "reboot", "203.0.113.10", "--power-cycle", "--yes", "--account", "one"); code != 0 {
		t.Errorf("reboot --power-cycle: code %d", code)
	}
	out, _, code = e.run("", false, "server", "start", "web-01", "--account", "one")
	if code != 0 || out != "Switching on fake/1 (web-01, 203.0.113.10)\n" {
		t.Errorf("start: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "server", "stop", "web-01", "--yes", "--account", "one")
	if code != 0 || out != "Switching off fake/1 (web-01, 203.0.113.10) (shutdown)\n" {
		t.Errorf("stop: code %d, %q", code, out)
	}
	out, _, code = e.run("", false, "server", "stop", "web-01", "--hard", "--yes", "--account", "one")
	if code != 0 || out != "Switching off fake/1 (web-01, 203.0.113.10) (power off)\n" {
		t.Errorf("stop --hard: code %d, %q", code, out)
	}
	if got := strings.Join(fakePower.actions, ","); got != "reboot:soft,reboot:hard,reboot:power-cycle,on,off:false,off:true" {
		t.Errorf("actions: %s", got)
	}

	// confirmation: none without a terminal and --yes; a question on a terminal
	_, errOut, code = e.run("", false, "server", "reboot", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "ump server reboot needs confirmation: run it in a terminal or pass --yes") {
		t.Errorf("no --yes: code %d, %q", code, errOut)
	}
	out, errOut, code = e.run("y\n", true, "server", "reboot", "web-01", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Reboot fake/1 (web-01, 203.0.113.10) (soft)? [y/N]: ") || !strings.HasPrefix(out, "Rebooting") {
		t.Errorf("confirmed: code %d, out %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = e.run("n\n", true, "server", "stop", "web-01", "--account", "one")
	if code != 3 || out != "" || !strings.HasSuffix(errOut, "ump: aborted\n") {
		t.Errorf("declined: code %d, out %q, stderr %q", code, out, errOut)
	}
	if _, _, code = e.run("", false, "server", "start", "web-01", "--account", "one"); code != 0 {
		t.Error("start must not ask for confirmation")
	}

	// errors
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"server", "reboot", "web-01", "--hard", "--power-cycle", "--yes", "--account", "one"}, 2, "exclude each other"},
		{[]string{"server", "reboot", "db-01", "--yes", "--account", "one"}, 4, "fake: server is locked (locked, HTTP 423)"},
		{[]string{"server", "reboot", "nope", "--yes", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"server", "bogus", "x"}, 2, `unknown command "bogus" for ump server`},
		{[]string{"server", "reboot"}, 2, "accepts 1 arg(s)"},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "server", "reboot", "web-01", "--yes")
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
	out, errOut, code := e.run("", false, "server", "rescue", "web-01", "--ssh-key", keyFile, "--yes", "--account", "one")
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
	// ... and the provider's rescue system was only the first hop
	for _, want := range []string{
		"Checking root access to 203.0.113.10 over SSH ... no (tried root, ump).\nGoing through the provider's rescue system.\n",
		"Provider rescue system reached (Debian GNU/Linux 12 (bookworm)); loading the UMP rescue image from https://ump.zhurko.de/dl/rescue.\n",
		"==> booting into the UMP rescue system",
		"Waiting for the UMP rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.10.12).\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress line missing: %q in\n%s", want, errOut)
		}
	}
	entries := e.entryCommands()
	wantEntry := entryPrefix("https://ump.zhurko.de/dl/rescue") + "-H web-01 -K 'ssh-ed25519 "
	if len(entries) != 1 || !strings.HasPrefix(entries[0], wantEntry) || !strings.HasSuffix(entries[0], "' -K 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 ops@example'") {
		t.Errorf("entry script runs: %q", entries)
	}
	if argv := e.remoteCalls[len(e.remoteCalls)-2]; !strings.Contains(strings.Join(argv, " "), "-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@203.0.113.10") {
		t.Errorf("entry ssh argv: %v", argv)
	}

	// on a terminal the terminal is handed over to ssh, once the image answers
	resetFakePower()
	out, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 || len(e.sshCalls) != 1 || !e.sshInteractive[0] || !strings.Contains(errOut, "Connecting to the UMP rescue system of web-01 as root") {
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
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one", "--", "lsblk", "-d")
	if code != 7 || strings.Contains(errOut, "ump:") || len(e.sshCalls) != 1 || e.sshInteractive[0] {
		t.Errorf("command: code %d, stderr %q, calls %v", code, errOut, e.sshCalls)
	}
	if argv := e.sshCalls[0]; strings.Join(argv[len(argv)-4:], " ") != "root@203.0.113.10 -- lsblk -d" {
		t.Errorf("command argv: %v", argv)
	}
	e.sshExit = 0

	// --no-connect, --no-reboot, --no-wait print the command for later; -o json carries it
	e.sshCalls, e.sshInteractive = nil, nil
	out, _, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--no-connect", "--account", "one")
	if code != 0 || len(e.sshCalls) != 0 || !strings.Contains(out, "Connect: ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i ") {
		t.Errorf("--no-connect: code %d, calls %v\n%s", code, e.sshCalls, out)
	}
	resetFakePower()
	out, _, code = e.run("", true, "server", "rescue", "web-01", "--no-reboot", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(out, "it boots at the next reboot.") || !strings.Contains(out, "Connect once it is up: ssh -o ServerAliveInterval=5") || strings.Join(fakePower.actions, ",") != "rescue" || len(e.sshCalls) != 0 {
		t.Errorf("--no-reboot: code %d, actions %v, calls %v\n%s", code, fakePower.actions, e.sshCalls, out)
	}
	out, _, code = e.run("", true, "server", "rescue", "web-01", "--no-wait", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(out, "Connect once it is up: ssh -o ServerAliveInterval=5") || len(e.sshCalls) != 0 {
		t.Errorf("--no-wait: code %d\n%s", code, out)
	}
	resetFakePower()
	out, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one", "-o", "json")
	if code != 0 || len(e.sshCalls) != 0 || !strings.Contains(out, `"password": "r3scue"`) || !strings.Contains(out, `"ssh": "ssh -o ServerAliveInterval=5`) || !strings.Contains(out, umpKey+".pub") {
		t.Errorf("rescue json: code %d, calls %v\n%s", code, e.sshCalls, out)
	}
	if !strings.HasPrefix(out, "{") || !strings.Contains(out, `"image": {`) || !strings.Contains(out, `"version": "2026.10.12"`) || !strings.Contains(out, `"via": "provider"`) {
		t.Errorf("rescue json image details:\n%s", out)
	}
	if !strings.Contains(errOut, "Rescue system enabled for fake/1") {
		t.Errorf("with -o json the progress must go to stderr: %q", errOut)
	}

	// unrescue, with and without reboot
	out, _, code = e.run("", false, "server", "unrescue", "web-01", "--account", "one")
	if code != 0 || out != "Rescue system disabled for fake/1 (web-01, 203.0.113.10); the next boot is from disk.\n" {
		t.Errorf("unrescue: code %d, %q", code, out)
	}
	resetFakePower()
	out, _, code = e.run("", false, "server", "unrescue", "web-01", "--reboot", "--yes", "--account", "one")
	if code != 0 || out != "Rescue system disabled for fake/1 (web-01, 203.0.113.10); rebooting from disk.\n" || strings.Join(fakePower.actions, ",") != "unrescue,reboot:hard" {
		t.Errorf("unrescue --reboot: code %d, %q, actions %v", code, out, fakePower.actions)
	}
	if _, errOut, code := e.run("", false, "server", "unrescue", "web-01", "--reboot", "--account", "one"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("unrescue --reboot without --yes: code %d, %q", code, errOut)
	}

	// errors
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"server", "rescue", "web-01", "--yes", "--account", "one", "--ssh-key", "/nonexistent.pub"}, 1, "nonexistent.pub"},
		{[]string{"server", "rescue", "web-01", "--account", "one"}, 3, "needs confirmation"},
		{[]string{"server", "rescue", "web-01", "--yes", "-o", "table", "--account", "one"}, 2, "unknown output format"},
		{[]string{"server", "rescue", "nope", "--yes", "--account", "one"}, 1, `server "nope" not found`},
		{[]string{"server", "rescue", "web-01", "db-01", "--yes", "--account", "one"}, 2, "expected one server reference, got 2"},
	}
	for _, c := range cases {
		_, errOut, code := e.run("", false, c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), stderr %q (want %q)", c.args, code, c.code, errOut, c.msg)
		}
	}
	e.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes")
	if code != 1 || !strings.Contains(errOut, "provider type listonly has no rescue system") {
		t.Errorf("no rescue: code %d, %q", code, errOut)
	}
}

func TestSSHCommandAndKey(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	umpKey := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")

	// config key list generates once and lists it; show prints the public key
	out, errOut, code := e.run("", false, "config", "key", "list")
	if code != 0 || !strings.Contains(errOut, "Generated UMP's SSH key") || !strings.Contains(out, "ump    ssh-ed25519") || !strings.Contains(out, "UMP's own key (") {
		t.Errorf("config key: code %d, stderr %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "config", "key", "show")
	if code != 0 || errOut != "" || !strings.Contains(out, "Public key:   ssh-ed25519 AAAA") || !strings.Contains(out, "Fingerprint:  SHA256:") {
		t.Errorf("config key show: code %d, stderr %q\n%s", code, errOut, out)
	}
	out, errOut, code = e.run("", false, "config", "key", "show", "-o", "json")
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
	_, errOut, code = e.run("", false, "ssh", "web-01", "--account", "one", "--", "uname", "-a")
	if code != 3 || errOut != "" || len(e.sshCalls) != 1 || e.sshInteractive[0] {
		t.Errorf("ssh command: code %d, stderr %q, calls %v", code, errOut, e.sshCalls)
	}
	if argv := strings.Join(e.sshCalls[0], " "); argv != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -i "+filepath.Join(home, ".ssh", "id_ed25519")+" ump@203.0.113.10 -- uname -a" {
		t.Errorf("ssh argv: %q", argv)
	}
	e.sshExit = 0
	// --staging uses UMP's key without host key checks; --user changes the login
	e.sshCalls, e.sshInteractive = nil, nil
	_, _, code = e.run("", false, "ssh", "fake/1", "--staging", "--user", "deploy", "--account", "one", "--", "id")
	if code != 0 || len(e.sshCalls) != 1 {
		t.Fatalf("ssh --staging: code %d, calls %v", code, e.sshCalls)
	}
	if argv := strings.Join(e.sshCalls[0], " "); argv != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR deploy@203.0.113.10 -- id" {
		t.Errorf("ssh --staging argv: %q", argv)
	}
	// interactive needs a terminal
	e.sshCalls, e.sshInteractive = nil, nil
	_, errOut, code = e.run("", false, "ssh", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "needs a terminal") || len(e.sshCalls) != 0 {
		t.Errorf("interactive without terminal: code %d, %q, calls %v", code, errOut, e.sshCalls)
	}
	_, _, code = e.run("", true, "ssh", "web-01", "--account", "one")
	if code != 0 || len(e.sshCalls) != 1 || !e.sshInteractive[0] || strings.Join(e.sshCalls[0], " ") != "ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -i "+umpKey+" -i "+filepath.Join(home, ".ssh", "id_ed25519")+" ump@203.0.113.10" {
		t.Errorf("interactive: code %d, calls %v %v", code, e.sshCalls, e.sshInteractive)
	}
	if strings.Contains(strings.Join(e.sshCalls[0], " "), "IdentitiesOnly") {
		t.Error("the default session must not exclude agent keys")
	}
	_, errOut, code = e.run("", false, "ssh", "nope", "--account", "one", "--", "id")
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

func TestRescueDirectAndRunning(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()
	umpKey := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "staging_ed25519")

	// the running system lets UMP in as root: the image is loaded over SSH
	// and the provider is not touched
	e.directRoot, e.versionAfter = true, 2
	out, errOut, code := e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("direct: code %d, stderr %q\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 26.04 LTS).\n",
		"Loading the UMP rescue image from https://ump.zhurko.de/dl/rescue on web-01, without the provider's rescue system.\n",
		"==> booting into the UMP rescue system",
		"Waiting for the UMP rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.10.12).\n",
		"Connecting to the UMP rescue system of web-01 as root; exit the shell to return.\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress line missing: %q in\n%s", want, errOut)
		}
	}
	if len(fakePower.actions) != 0 {
		t.Errorf("the provider was used: %v", fakePower.actions)
	}
	if out != "" {
		t.Errorf("stdout: %q", out)
	}
	entries := e.entryCommands()
	if len(entries) != 1 || !strings.HasPrefix(entries[0], entryPrefix("https://ump.zhurko.de/dl/rescue")+"-H web-01 -K 'ssh-ed25519 ") {
		t.Errorf("entry: %q", entries)
	}
	// the probe and the entry offer UMP's key and the user's own, the
	// version checks UMP's key alone
	probe := strings.Join(e.remoteCalls[0], " ")
	if !strings.Contains(probe, "-o BatchMode=yes -o ConnectTimeout=10") || !strings.Contains(probe, "-i "+umpKey+" ") || strings.Contains(probe, "IdentitiesOnly") {
		t.Errorf("probe argv: %v", e.remoteCalls[0])
	}
	if e.versionPolls != 3 {
		t.Errorf("version checks: %d", e.versionPolls)
	}
	last := strings.Join(e.remoteCalls[len(e.remoteCalls)-1], " ")
	if !strings.Contains(last, "-i "+umpKey+" -o IdentitiesOnly=yes") || !strings.HasSuffix(last, "root@203.0.113.10 -- cat /etc/ump-rescue-version") {
		t.Errorf("version check argv: %v", e.remoteCalls[len(e.remoteCalls)-1])
	}
	if len(e.sshCalls) != 1 || !e.sshInteractive[0] || e.sshCalls[0][len(e.sshCalls[0])-1] != "root@203.0.113.10" {
		t.Errorf("hand-over: %v", e.sshCalls)
	}

	// the image URL from the configuration and from --image; -o json names
	// the way the server got there
	e.sshCalls, e.sshInteractive = nil, nil
	e.write(testConfig + "staging:\n  image: https://files.example.net/ump/\n")
	out, _, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"url": "https://files.example.net/ump"`) || !strings.Contains(out, `"via": "direct"`) || strings.Contains(out, `"rescue"`) {
		t.Errorf("json after a direct entry: code %d\n%s", code, out)
	}
	if entries := e.entryCommands(); len(entries) != 2 || !strings.HasPrefix(entries[1], entryPrefix("https://files.example.net/ump")+"-H web-01 ") {
		t.Errorf("entry with the configured image: %q", entries)
	}
	_, _, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one", "--image", "http://10.0.0.1/r/", "--", "lsblk")
	if entries := e.entryCommands(); code != 0 || len(entries) != 3 || !strings.HasPrefix(entries[2], entryPrefix("http://10.0.0.1/r")+"-H web-01 ") {
		t.Errorf("entry with --image: code %d, %q", code, entries)
	}
	if argv := e.sshCalls[len(e.sshCalls)-1]; strings.Join(argv[len(argv)-3:], " ") != "root@203.0.113.10 -- lsblk" {
		t.Errorf("command after the direct entry: %v", argv)
	}

	// a server that runs the image already is connected to as it is
	e.directRoot, e.inImage = false, true
	e.sshCalls, e.sshInteractive, e.remoteCalls = nil, nil, nil
	out, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Checking root access to 203.0.113.10 over SSH ... web-01 already runs the UMP rescue system 2026.10.12.\n") || !strings.HasPrefix(out, "Connect: ssh ") {
		t.Errorf("already running: code %d, stderr %q\n%s", code, errOut, out)
	}
	if len(e.remoteCalls) != 1 || len(fakePower.actions) != 0 || len(e.entryCommands()) != 0 {
		t.Errorf("already running must only probe: calls %d, actions %v", len(e.remoteCalls), fakePower.actions)
	}
	out, _, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.Contains(out, "via: running") {
		t.Errorf("yaml when running: code %d\n%s", code, out)
	}

	// the direct way asks before it kexecs the running system
	e.inImage, e.directRoot = false, true
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "needs confirmation") || len(e.entryCommands()) != 0 {
		t.Errorf("confirmation: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("n\n", true, "server", "rescue", "web-01", "--account", "one")
	if code != 3 || !strings.Contains(errOut, "aborted") || len(e.entryCommands()) != 0 {
		t.Errorf("declined: code %d, %q", code, errOut)
	}
}

func TestRescueDirectWithSudo(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()

	// root cannot log in (a UMP-installed system), but ump can and has
	// sudo: the entry script runs through sudo, and the provider is not
	// touched
	e.directSudo = true
	out, errOut, code := e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("direct with sudo: code %d, stderr %q\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 26.04 LTS, as ump with sudo).\n",
		"Loading the UMP rescue image from https://ump.zhurko.de/dl/rescue on web-01, without the provider's rescue system.\n",
		"Connecting to the UMP rescue system of web-01 as root; exit the shell to return.\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress line missing: %q in\n%s", want, errOut)
		}
	}
	if len(fakePower.actions) != 0 {
		t.Errorf("the provider was used: %v", fakePower.actions)
	}
	entries := e.entryCommands()
	if len(entries) != 1 || !strings.HasPrefix(entries[0], "sudo -n sh -c '"+entryPrefix("https://ump.zhurko.de/dl/rescue")+"-H web-01 -K '\\''ssh-ed25519 ") {
		t.Errorf("entry: %q", entries)
	}
	users := []string{}
	for _, call := range e.remoteCalls {
		if strings.HasPrefix(call[len(call)-1], "id -u;") && !strings.Contains(strings.Join(call, " "), "IdentitiesOnly=yes") {
			for _, a := range call {
				if strings.HasSuffix(a, "@203.0.113.10") {
					users = append(users, strings.TrimSuffix(a, "@203.0.113.10"))
				}
			}
		}
	}
	if strings.Join(users, ",") != "root,ump" {
		t.Errorf("probed as %v, want root then ump", users)
	}

	// a user without sudo is not root access
	e.directSudo = false
	e.remoteCalls = nil
	e.inImage = false
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--via", "direct", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "no (tried root, ump)") {
		t.Errorf("without sudo: code %d, %q", code, errOut)
	}
}

func TestRescueWaysAndFailures(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()

	// --via direct without root access does not touch the provider
	_, errOut, code := e.run("", false, "server", "rescue", "web-01", "--yes", "--via", "direct", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "no root access to 203.0.113.10 over SSH (as root, or a sudo user with UMP's key); --via provider") || len(fakePower.actions) != 0 {
		t.Errorf("--via direct: code %d, %q, actions %v", code, errOut, fakePower.actions)
	}

	// --via provider skips the probe even when the system would let UMP in
	e.directRoot = true
	e.remoteCalls = nil
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--via", "provider", "--account", "one")
	if code != 0 || strings.Contains(errOut, "Checking root access") || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard" || len(e.entryCommands()) != 1 {
		t.Errorf("--via provider: code %d, %q, actions %v", code, errOut, fakePower.actions)
	}
	if cmd := e.remoteCalls[0][len(e.remoteCalls[0])-1]; !strings.HasPrefix(cmd, "id -u;") || !strings.Contains(strings.Join(e.remoteCalls[0], " "), "IdentitiesOnly=yes") {
		t.Errorf("the first remote run must probe the provider's rescue system with UMP's key: %v", e.remoteCalls[0])
	}
	e.directRoot = false

	// --no-image stops in the provider's rescue system, as before
	resetFakePower()
	e.remoteCalls, e.sshCalls, e.sshInteractive = nil, nil, nil
	_, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--no-image", "--account", "one")
	if code != 0 || len(e.remoteCalls) != 0 || !strings.Contains(errOut, "Connecting to the rescue system of web-01 as root") || len(e.sshCalls) != 1 {
		t.Errorf("--no-image: code %d, remote %v, stderr %q", code, e.remoteCalls, errOut)
	}

	// the provider's rescue system does not take UMP's key: say how to go on by hand
	resetFakePower()
	e.rescueNoKey = true
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "did not accept UMP's key; connect by hand (ssh ") || !strings.Contains(errOut, "bash /tmp/kexec-into-rescue.sh -u https://ump.zhurko.de/dl/rescue") {
		t.Errorf("rescue without key: code %d, %q", code, errOut)
	}
	e.rescueNoKey = false

	// the entry script fails: its output was shown, the status is ump's
	resetFakePower()
	e.entryExit = 1
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "download failed: https://nowhere.invalid") || !strings.Contains(errOut, "ump: loading the rescue image on 203.0.113.10 failed (exit status 1)") {
		t.Errorf("entry failure: code %d, %q", code, errOut)
	}
	e.entryExit = 0

	// the server cannot fetch the script: the base URL is the thing to look at
	resetFakePower()
	e.entryNoScript = true
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "curl: (22)") || !strings.Contains(errOut, "ump: 203.0.113.10 could not fetch the entry script from https://ump.zhurko.de/dl/rescue/kexec-into-rescue.sh (exit status 22); the server must reach the image's base URL") {
		t.Errorf("no script: code %d, %q", code, errOut)
	}
	e.entryNoScript = false

	// the connection drops before the script reported the jump
	resetFakePower()
	e.entryNoMarker = true
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "ended before the rescue image was loaded") {
		t.Errorf("dropped entry: code %d, %q", code, errOut)
	}
	e.entryNoMarker = false

	// the image never answers
	resetFakePower()
	oldTimeout := imageWaitTimeout
	imageWaitTimeout = 30 * time.Millisecond
	t.Cleanup(func() { imageWaitTimeout = oldTimeout })
	e.versionAfter = 1 << 30
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "did not answer within 30ms") {
		t.Errorf("image timeout: code %d, %q", code, errOut)
	}
	e.versionAfter = 0

	// usage
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"server", "rescue", "web-01", "--yes", "--via", "magic", "--account", "one"}, `unknown --via "magic"`},
		{[]string{"server", "rescue", "web-01", "--yes", "--via", "direct", "--no-image", "--account", "one"}, "--via direct cannot be combined"},
	} {
		if _, errOut, code := e.run("", false, c.args...); code != 2 || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
}

func installerPrefix(storage string) string {
	return `cat > /tmp/ump-install.json && f=/tmp/ump-install.sh; if command -v curl >/dev/null; then curl -fsSL ` + storage + `/install/install.sh -o "$f"; else wget -q -O "$f" ` + storage + `/install/install.sh; fi && bash "$f" --spec /tmp/ump-install.json`
}

func TestInstallServer(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()

	// through the provider's rescue system, the installer, a reboot, and
	// the new system answering with UMP's key
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("install: code %d, stderr %q\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"Checking root access to 203.0.113.10 over SSH ... no (tried root, ump).\n",
		"Waiting for the UMP rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.10.12).\n",
		"Installing ubuntu-26.04 (profile ubuntu-26.04 from " + filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles", "ubuntu-26.04.yaml") + ", layout lvm-linear) on web-01 as web-01 ...\n",
		"==> installed: ubuntu-26.04 2026.10.12",
		"Rebooting web-01 into ubuntu-26.04 ...\n",
		"Waiting for SSH on 203.0.113.10 to go down ...",
		"web-01 is up: Ubuntu 26.04.1 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress line missing: %q in\n%s", want, errOut)
		}
	}
	if out != "" {
		t.Errorf("stdout: %q", out)
	}
	if len(e.installs) != 1 || !strings.HasSuffix(e.installs[0], installerPrefix("https://ump.zhurko.de/dl")) {
		t.Errorf("installer command: %q", e.installs)
	}
	spec := e.lastSpec()
	if spec["os"] != "ubuntu-26.04" || spec["hostname"] != "web-01" || spec["storage"] != "https://ump.zhurko.de/dl" || spec["timezone"] != "UTC" {
		t.Errorf("spec: %v", spec)
	}
	if k := spec["kernel"].(map[string]any); k["mitigations"] != false || k["ipv6"] != false {
		t.Errorf("spec kernel: %v", k)
	}
	if l := spec["layout"].(map[string]any); l["name"] != "lvm-linear" || len(l["parts"].([]any)) != 5 {
		t.Errorf("spec layout: %v", l)
	}
	users := spec["users"].([]any)
	if len(users) != 1 || users[0].(map[string]any)["name"] != "ump" || users[0].(map[string]any)["sudo"] != true || users[0].(map[string]any)["shell"] != "/bin/bash" || len(users[0].(map[string]any)["keys"].([]any)) != 1 || !strings.HasPrefix(users[0].(map[string]any)["keys"].([]any)[0].(string), "ssh-ed25519 ") {
		t.Errorf("spec users: %v", users)
	}
	if fw := spec["firewall"].(map[string]any); fw["policy"] != "drop" || len(fw["allow"].([]any)) != 2 {
		t.Errorf("spec firewall: %v", fw)
	}
	if ud := spec["userdata"].([]any); spec["provision"].(map[string]any)["type"] != "none" || len(ud) != 1 || ud[0].(map[string]any)["name"] != "ubuntu" || ud[0].(map[string]any)["type"] != "bash" || !strings.Contains(ud[0].(map[string]any)["script"].(string), "set -euo pipefail") {
		t.Errorf("spec provision/userdata: %v %v", spec["provision"], spec["userdata"])
	}
	// root's keys of the new system are put into the rescue system first
	if !strings.HasPrefix(e.installs[0], `mkdir -p -m 700 /root/.ssh && touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && for k in 'ssh-ed25519 `) || !strings.Contains(e.installs[0], `; do grep -qxF "$k" /root/.ssh/authorized_keys || echo "$k" >> /root/.ssh/authorized_keys; done; cat > /tmp/ump-install.json && f=/tmp/ump-install.sh;`) {
		t.Errorf("keys not ensured before the installer: %q", e.installs[0])
	}
	if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard,unrescue" {
		t.Errorf("provider actions: %s", got)
	}
	if e.reboots != 1 || len(e.sshCalls) != 0 {
		t.Errorf("reboots %d, ssh hand-overs %v", e.reboots, e.sshCalls)
	}

	// the options reach the installer; --storage moves the rescue image too;
	// the user's own public key goes along
	resetFakePower()
	e.directRoot = true
	sshDir := filepath.Join(os.Getenv("HOME"), ".ssh")
	os.MkdirAll(sshDir, 0o700)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519"), []byte("private\n"), 0o600)
	os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 me@laptop\n"), 0o644)
	out, _, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--hostname", "db-01.example.net", "--ipv6", "--mitigations", "--disks", "/dev/sda,/dev/sdb", "--timezone", "Europe/Berlin", "--storage", "https://files.example.net/ump/", "-o", "json")
	if code != 0 {
		t.Fatalf("install with options: code %d\n%s", code, out)
	}
	if !strings.HasSuffix(e.installs[1], installerPrefix("https://files.example.net/ump")) || !strings.Contains(e.installs[1], "for k in 'ssh-ed25519 ") {
		t.Errorf("installer command with options: %q", e.installs[1])
	}
	spec = e.lastSpec()
	if spec["hostname"] != "db-01.example.net" || spec["timezone"] != "Europe/Berlin" || spec["storage"] != "https://files.example.net/ump" {
		t.Errorf("spec with options: %v", spec)
	}
	if k := spec["kernel"].(map[string]any); k["mitigations"] != true || k["ipv6"] != true {
		t.Errorf("spec kernel with options: %v", k)
	}
	if d := spec["disks"].([]any); len(d) != 2 || d[0] != "/dev/sda" || d[1] != "/dev/sdb" {
		t.Errorf("spec disks: %v", spec["disks"])
	}
	if keys := spec["users"].([]any)[0].(map[string]any)["keys"].([]any); len(keys) != 2 || !strings.HasSuffix(keys[1].(string), " me@laptop") {
		t.Errorf("root keys with the local key: %v", keys)
	}
	if entries := e.entryCommands(); len(entries) != 2 || !strings.HasPrefix(entries[1], entryPrefix("https://files.example.net/ump/rescue")) || !strings.Contains(entries[1], "-K 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 me@laptop'") {
		t.Errorf("rescue entry with --storage and the user's key: %q", entries)
	}
	os.RemoveAll(sshDir)
	if len(fakePower.actions) != 0 {
		t.Errorf("direct installation must not touch the provider: %v", fakePower.actions)
	}
	for _, want := range []string{`"os": "ubuntu-26.04"`, `"hostname": "db-01.example.net"`, `"via": "direct"`, `"rebooted": true`, `"name": "Ubuntu 26.04.1 LTS"`, `"root": "/dev/mapper/vg0-root"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json: %q missing in\n%s", want, out)
		}
	}
	e.directRoot = false

	// the storage from the configuration
	resetFakePower()
	e.write(testConfig + "storage: https://mirror.example.org/ump\n")
	_, _, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 0 || !strings.HasSuffix(e.installs[2], installerPrefix("https://mirror.example.org/ump")) || !strings.HasPrefix(e.entryCommands()[2], entryPrefix("https://mirror.example.org/ump/rescue")) {
		t.Errorf("configured storage: code %d, %q, %q", code, e.installs[2], e.entryCommands()[2])
	}
	e.write(testConfig)

	// --dry-run plans and leaves the server in the rescue system; --no-reboot installs and leaves it there
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--dry-run", "--account", "one")
	if code != 0 || !strings.HasSuffix(e.installs[3], " --dry-run") || e.reboots != 0 || !strings.Contains(errOut, "Dry run done; web-01 stays in the UMP rescue system (ump ssh web-01 --staging).") || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard" {
		t.Errorf("--dry-run: code %d, reboots %d, actions %v, stderr %q", code, e.reboots, fakePower.actions, errOut)
	}
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-reboot", "--account", "one")
	if code != 0 || e.reboots != 0 || !strings.Contains(errOut, "Installed; web-01 stays in the UMP rescue system, a reboot starts ubuntu-26.04.") || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" {
		t.Errorf("--no-reboot: code %d, reboots %d, actions %v, stderr %q", code, e.reboots, fakePower.actions, errOut)
	}

	// failures: the installer fails, the installer cannot be fetched, the new system does not answer
	resetFakePower()
	e.installExit = 1
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "install.sh: no disk found") || !strings.Contains(errOut, "ump: the installation on web-01 failed (exit status 1, see above); the server stays in the UMP rescue system (ump ssh web-01 --staging)") || e.reboots != 0 {
		t.Errorf("failed installation: code %d, reboots %d, %q", code, e.reboots, errOut)
	}
	resetFakePower()
	e.installExit = 2
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "no disk matches") || !strings.Contains(errOut, "ump: the installer refused the specification or the machine (exit status 2, see above); nothing was changed, web-01 stays in the UMP rescue system") {
		t.Errorf("refused specification: code %d, %q", code, errOut)
	}
	e.installExit = 0
	resetFakePower()
	e.installNoScript = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "ump: 203.0.113.10 could not fetch the installer from https://ump.zhurko.de/dl/install/install.sh (exit status 22); the server must reach the storage") {
		t.Errorf("no installer: code %d, %q", code, errOut)
	}
	e.installNoScript = false
	resetFakePower()
	e.bootFails = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "SSH answers on 203.0.113.10, but not as ump with UMP's key; the new system may not have booted") {
		t.Errorf("boot failure: code %d, %q", code, errOut)
	}
	e.bootFails = false

	// usage and confirmation
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"server", "reinstall", "web-01", "--account", "one"}, 3, "needs confirmation"},
		{[]string{"server", "reinstall", "web-01", "--yes", "--hostname", "bad host", "--account", "one"}, 2, "invalid name"},
		{[]string{"server", "reinstall", "web-01", "--yes", "--via", "magic", "--account", "one"}, 2, `unknown --via "magic"`},
		{[]string{"server", "reinstall", "web-01", "--yes", "--os", "../x", "--account", "one"}, 2, "invalid --os"},
		{[]string{"server", "reinstall", "web-01", "--yes", "--layout", "nope", "--account", "one"}, 1, `layout "nope" is not in layouts.yaml`},
		{[]string{"server", "reinstall", "web-01", "--yes", "--profile", "/nonexistent/p.yaml", "--account", "one"}, 1, "nonexistent"},
		{[]string{"server", "reinstall", "web-01", "--yes", "-o", "table", "--account", "one"}, 2, "unknown output format"},
	} {
		if _, errOut, code := e.run("", false, c.args...); code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d (want %d), %q", c.args, code, c.code, errOut)
		}
	}
}

func TestInstallProfiles(t *testing.T) {
	e := newTestEnv(t)
	resetFakePower()
	e.directRoot = true

	// a profile file of its own: users with sudo and agent keys, inline
	// userdata, an inline layout, a firewall, a provision step
	dir := t.TempDir()
	prof := filepath.Join(dir, "db.yaml")
	os.WriteFile(prof, []byte(`os: ubuntu-26.04
timezone: Europe/Berlin
kernel: {ipv6: true}
disks:
  parts:
    - {type: slice, target: [first], size: 1M, filesystem: bios}
    - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
    - {type: slice, target: ["nvme.*"], size: rest, filesystem: lvm, vg: data}
    - {type: lvm, vg: data, name: swap, size: 8G, filesystem: swap}
    - {type: lvm, vg: data, name: root, size: rest, raid: 0, filesystem: xfs, mountpoint: /}
users:
  - {name: root, keys: [ump, agent]}
  - {name: dba, sudo: true, groups: [adm], keys: [agent]}
userdata: [{name: pg, script: "apt-get install -y postgresql\n"}]
firewall: {policy: drop, allow: [icmp, ssh, tcp/5432, {proto: tcp, port: 9100, from: 10.0.0.0/8}]}
provision: {type: ansible-pull, repo: https://git.example.net/infra.git}
`), 0o600)
	e.write(testConfig)
	e.agentKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 me@agent"}
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", prof, "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("install with a profile file: code %d, stderr %q\n%s", code, errOut, out)
	}
	if !strings.Contains(errOut, "Installing ubuntu-26.04 (profile db from "+prof+", layout inline) on web-01 as web-01 ...") || !strings.Contains(errOut, "The provision step ansible-pull is recorded in /etc/ump/install on the server; running it is not implemented yet.") {
		t.Errorf("stderr: %q", errOut)
	}
	spec := e.lastSpec()
	if spec["timezone"] != "Europe/Berlin" || spec["kernel"].(map[string]any)["ipv6"] != true || spec["kernel"].(map[string]any)["mitigations"] != false {
		t.Errorf("spec: %v", spec)
	}
	if l := spec["layout"].(map[string]any); l["name"] != "inline" || len(l["parts"].([]any)) != 6 {
		t.Errorf("layout: %v", l)
	}
	users := spec["users"].([]any)
	if len(users) != 2 || users[1].(map[string]any)["name"] != "dba" || users[1].(map[string]any)["sudo"] != true || len(users[0].(map[string]any)["keys"].([]any)) != 2 || len(users[1].(map[string]any)["keys"].([]any)) != 1 {
		t.Errorf("users: %v", users)
	}
	if ud := spec["userdata"].([]any); len(ud) != 1 || ud[0].(map[string]any)["name"] != "pg" || ud[0].(map[string]any)["type"] != "bash" || ud[0].(map[string]any)["script"] != "apt-get install -y postgresql\n" {
		t.Errorf("userdata: %v", ud)
	}
	if fw := spec["firewall"].(map[string]any); len(fw["allow"].([]any)) != 4 || fw["allow"].([]any)[3].(map[string]any)["from"] != "10.0.0.0/8" {
		t.Errorf("firewall: %v", fw)
	}
	if pv := spec["provision"].(map[string]any); pv["type"] != "ansible-pull" || pv["options"].(map[string]any)["repo"] != "https://git.example.net/infra.git" {
		t.Errorf("provision: %v", pv)
	}
	if !strings.Contains(out, `"profile": "db"`) || !strings.Contains(out, `"layout": "inline"`) {
		t.Errorf("json result:\n%s", out)
	}

	// the published profile and layouts, from the storage, when there is no
	// local copy: the first run downloads them into the profiles directory
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/profiles/ubuntu-26.04.yaml", "/profiles/ubuntu-24.04.yaml", "/profiles/ubuntu-22.04.yaml", "/profiles/layouts.yaml", "/profiles/firewalls.yaml", "/profiles/users.yaml", "/profiles/userdata.yaml":
			data, _ := os.ReadFile(filepath.Join("..", "..", "profiles", filepath.Base(r.URL.Path)))
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	profilesDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")
	os.RemoveAll(profilesDir)
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--layout", "lvm-striped", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Downloaded the published profiles, layouts, firewalls and users into "+profilesDir+" (ump config os)") || !strings.Contains(errOut, "(profile ubuntu-26.04 from "+filepath.Join(profilesDir, "ubuntu-26.04.yaml")+", layout lvm-striped)") {
		t.Errorf("published profile: code %d, %q", code, errOut)
	}
	for _, f := range profileFiles {
		if _, err := os.Stat(filepath.Join(profilesDir, f)); err != nil {
			t.Errorf("first run did not download %s: %v", f, err)
		}
	}
	if l := e.lastSpec()["layout"].(map[string]any); l["name"] != "lvm-striped" || len(l["parts"].([]any)) != 6 {
		t.Errorf("layout from the storage: %v", l)
	}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "nope", "--account", "one")
	if code != 1 || !strings.Contains(errOut, `profile "nope": not in `+filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")+`, and `+srv.URL+`/profiles/nope.yaml: HTTP 404`) {
		t.Errorf("missing profile: code %d, %q", code, errOut)
	}

	// a key reference that gives nothing is an error before anything happens
	e.agentKeys = nil
	os.WriteFile(prof, []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: root, keys: [agent]}\n"), 0o600)
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", prof, "--account", "one")
	if code != 1 || !strings.Contains(errOut, `user root: key "agent": ssh-add -L lists no keys`) || len(e.entryCommands()) != 2 {
		t.Errorf("agent without keys: code %d, %q, entries %d", code, errOut, len(e.entryCommands()))
	}
}

func TestConfigOSAndServerSet(t *testing.T) {
	e := newTestEnv(t)
	resetFakePower()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join("..", "..", "profiles", filepath.Base(r.URL.Path)))
		if err != nil || !strings.HasPrefix(r.URL.Path, "/profiles/") {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	// the first run downloads the published profile and layouts
	out, errOut, code := e.run("", false, "config", "os", "list")
	if code != 0 || !strings.Contains(errOut, "Downloaded the published profiles, layouts, firewalls and users into "+dir) || !strings.Contains(out, "NAME") || !strings.Contains(strings.Join(strings.Fields(out), " "), "* ubuntu-26.04 ubuntu-26.04 lvm-linear ump default published") || !strings.Contains(strings.Join(strings.Fields(out), " "), " ubuntu-22.04 ubuntu-22.04 lvm-linear ump default published") || !strings.Contains(out, "* the default (ump config os default NAME changes it)") {
		t.Errorf("first list: code %d, stderr %q\n%s", code, errOut, out)
	}
	for _, f := range profileFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not downloaded: %v", f, err)
		}
	}
	_, errOut, _ = e.run("", false, "config", "os", "list")
	if strings.Contains(errOut, "Downloaded") {
		t.Error("downloaded again although the directory exists")
	}

	// get prints the file; create copies it with the name changed and, without a terminal, does not open an editor
	out, _, code = e.run("", false, "config", "os", "get", "ubuntu-26.04")
	if code != 0 || !strings.HasPrefix(out, "# The profile for Ubuntu 26.04, the default") {
		t.Errorf("get: code %d\n%s", code, out)
	}
	edited := []string{}
	out, _, code = e.run("", false, "config", "os", "create", "db")
	if code != 0 || !strings.Contains(out, "Created "+filepath.Join(dir, "db.yaml")+" from ubuntu-26.04.") || len(edited) != 0 {
		t.Errorf("create: code %d\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "db.yaml"))
	if !strings.Contains(string(data), "\nname: db\n") {
		t.Errorf("created profile keeps the old name:\n%s", data)
	}
	if _, errOut, code = e.run("", false, "config", "os", "create", "db"); code != 1 || !strings.Contains(errOut, "exists already") {
		t.Errorf("create twice: code %d, %q", code, errOut)
	}
	if _, errOut, code = e.run("", false, "config", "os", "get", "nope"); code != 1 || !strings.Contains(errOut, `no profile "nope"`) {
		t.Errorf("get missing: code %d, %q", code, errOut)
	}

	// edit opens the editor and checks the result; a broken edit is kept and named
	e.editorHook = func(path string) error {
		edited = append(edited, path)
		return os.WriteFile(path, []byte("os: ubuntu-26.04\ndisks: lvm-raid1\nusers:\n  - {name: root, keys: [ump]}\n"), 0o600)
	}
	out, _, code = e.run("", true, "config", "os", "edit", "db")
	if code != 0 || len(edited) != 1 || edited[0] != filepath.Join(dir, "db.yaml") || !strings.Contains(out, "Profile db is valid (ubuntu-26.04, layout lvm-raid1).") {
		t.Errorf("edit: code %d, edited %v\n%s", code, edited, out)
	}
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("os: ubuntu-26.04\ndisks: nope\nusers: []\n"), 0o600)
	}
	_, errOut, code = e.run("", true, "config", "os", "edit", "db")
	if code != 1 || !strings.Contains(errOut, "the profile is kept, but it is not valid") {
		t.Errorf("broken edit: code %d, %q", code, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "db.yaml")); !strings.Contains(string(data), "disks: nope") {
		t.Error("the broken edit was not kept")
	}

	// server set remembers the profile, reinstall uses it, --clear forgets it
	e.editorHook = nil
	os.WriteFile(filepath.Join(dir, "db.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-raid1\nusers:\n  - {name: root, keys: [ump]}\n"), 0o600)
	out, _, code = e.run("", false, "server", "set", "web-01", "--profile", "db", "--account", "one")
	if code != 0 || !strings.Contains(out, "fake/1 (web-01, 203.0.113.10): profile db; ump server reinstall web-01 uses it.") {
		t.Errorf("set: code %d\n%s", code, out)
	}
	if st := e.readState(); !strings.Contains(st, "servers:\n  fake/1:\n    profile: db\n") || strings.Contains(e.read(), "profile: db") {
		t.Errorf("state after set:\n%s\nconfig:\n%s", st, e.read())
	}
	if _, errOut, code = e.run("", false, "server", "set", "web-01", "--profile", "nope", "--account", "one"); code != 1 || !strings.Contains(errOut, `profile "nope"`) {
		t.Errorf("set unknown profile: code %d, %q", code, errOut)
	}
	if _, errOut, code = e.run("", false, "server", "set", "web-01", "--account", "one"); code != 2 || !strings.Contains(errOut, "give at least one of --profile, --clear, --price") {
		t.Errorf("set without a choice: code %d, %q", code, errOut)
	}
	e.directRoot = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "(profile db from "+filepath.Join(dir, "db.yaml")+", layout lvm-raid1)") {
		t.Errorf("reinstall with the set profile: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "ubuntu-26.04", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "(profile ubuntu-26.04 from ") {
		t.Errorf("--profile wins: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "server", "set", "web-01", "--clear", "--account", "one")
	if code != 0 || !strings.Contains(out, "no profile set; reinstall uses the default profile (ubuntu-26.04).") || strings.Contains(e.readState(), "profile: db") {
		t.Errorf("clear: code %d\n%s\n%s", code, out, e.read())
	}

	// delete asks, then removes
	if _, errOut, code = e.run("", false, "config", "os", "delete", "db"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("delete without --yes: code %d, %q", code, errOut)
	}
	if out, _, code = e.run("", false, "config", "os", "delete", "db", "--yes"); code != 0 || !strings.Contains(out, "Deleted ") {
		t.Errorf("delete: code %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "db.yaml")); err == nil {
		t.Error("db.yaml still there")
	}

	// bare, it is the help; create without a name needs a terminal
	if out, _, code = e.run("", false, "config", "os"); code != 0 || !strings.Contains(out, "Available Commands:") {
		t.Errorf("bare: code %d, %q", code, out)
	}
	if out, _, code = e.run("", false, "c", "o", "ls"); code != 0 || !strings.Contains(out, "NAME") {
		t.Errorf("short form: code %d, %q", code, out)
	}
	if _, errOut, code = e.run("", false, "config", "os", "create"); code != 2 || !strings.Contains(errOut, "a profile name is needed without a terminal") {
		t.Errorf("create without a name: code %d, %q", code, errOut)
	}
	if out, _, code = e.run("", false, "config"); code != 0 || !strings.Contains(out, "isp") || !strings.Contains(out, "os") {
		t.Errorf("config help: code %d\n%s", code, out)
	}
}

func TestConfigDiskLayout(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	out, _, code := e.run("", false, "config", "dl", "list")
	if code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(strings.Join(strings.Fields(out), " "), "lvm-linear 5 (slice,slice,slice,slice,lvm) ok") || !strings.Contains(out, "lvm-raid1") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "config", "disk-layout", "get", "lvm-raid1")
	if code != 0 || !strings.HasPrefix(out, "lvm-raid1:\n") || !strings.Contains(out, "raid: 1") {
		t.Errorf("get: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "dl", "get", "nope"); code != 1 || !strings.Contains(errOut, `no layout "nope"`) {
		t.Errorf("get missing: code %d, %q", code, errOut)
	}

	// create copies an entry; the comments of the file survive
	out, _, code = e.run("", false, "config", "dl", "create", "nvme-raid1", "--from", "lvm-raid1")
	if code != 0 || !strings.Contains(out, "Created the layout nvme-raid1 from lvm-raid1") {
		t.Errorf("create: code %d\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "layouts.yaml"))
	if !strings.Contains(string(data), "# Named disk layouts") || !strings.Contains(string(data), "\n  nvme-raid1:\n") {
		t.Errorf("file after create:\n%s", data)
	}
	if _, errOut, code := e.run("", false, "config", "dl", "create", "nvme-raid1"); code != 1 || !strings.Contains(errOut, "exists already") {
		t.Errorf("create twice: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "config", "dl", "list", "--no-headers")
	if code != 0 || !strings.Contains(out, "nvme-raid1") {
		t.Errorf("list after create:\n%s", out)
	}

	// the new layout is usable by a profile
	os.WriteFile(filepath.Join(dir, "p.yaml"), []byte("os: ubuntu-26.04\ndisks: nvme-raid1\nusers:\n  - {name: root, keys: [ump]}\n"), 0o600)
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--account", "one"); code != 0 || !strings.Contains(errOut, "layout nvme-raid1)") {
		t.Errorf("reinstall with the new layout: code %d, %q", code, errOut)
	}

	// edit opens the file and checks it; a broken file is kept and named
	edited := 0
	e.editorHook = func(path string) error {
		edited++
		if path != filepath.Join(dir, "layouts.yaml") {
			t.Errorf("edited %s", path)
		}
		return nil
	}
	out, _, code = e.run("", true, "config", "dl", "edit")
	if code != 0 || edited != 1 || !strings.Contains(out, "5 layouts, all valid: lvm-example, lvm-linear, lvm-raid1, lvm-striped, nvme-raid1.") {
		t.Errorf("edit: code %d, edited %d\n%s", code, edited, out)
	}
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("layouts:\n  bad:\n    parts:\n      - {type: slice, target: [first], size: rest, filesystem: ext4}\n"), 0o600)
	}
	if _, errOut, code := e.run("", true, "config", "dl", "edit"); code != 1 || !strings.Contains(errOut, "the file is kept, but it is not valid") || !strings.Contains(errOut, "no part is mounted at /") {
		t.Errorf("broken edit: code %d, %q", code, errOut)
	}
	if out, _, code := e.run("", false, "config", "dl", "list"); code != 0 || !strings.Contains(out, "bad") || !strings.Contains(out, "invalid") {
		t.Errorf("list of the broken file:\n%s", out)
	}
	e.editorHook = nil

	// delete
	e.writeProfiles()
	if _, errOut, code := e.run("", false, "config", "dl", "delete", "lvm-example"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("delete without --yes: code %d, %q", code, errOut)
	}
	if out, _, code := e.run("", false, "config", "dl", "delete", "lvm-example", "--yes"); code != 0 || !strings.Contains(out, "Deleted the layout lvm-example.") {
		t.Errorf("delete: code %d\n%s", code, out)
	}
	if out, _, _ := e.run("", false, "config", "dl", "list", "--no-headers"); strings.Contains(out, "lvm-example") {
		t.Errorf("still listed:\n%s", out)
	}
	if out, _, code := e.run("", false, "config", "dl"); code != 0 || !strings.Contains(out, "Available Commands:") {
		t.Errorf("bare: code %d, %q", code, out)
	}
	if out, _, code := e.run("", false, "cfg", "layout", "l"); code != 0 || !strings.Contains(out, "NAME") {
		t.Errorf("short form: code %d, %q", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "dl", "create"); code != 2 || !strings.Contains(errOut, "a layout name is needed without a terminal") {
		t.Errorf("create without a name: code %d, %q", code, errOut)
	}
	// on a terminal, create asks for the name and what to start from
	e.editorHook = func(path string) error { return nil }
	if out, errOut, code := e.run("asked\n1\n", true, "config", "dl", "create"); code != 0 || !strings.Contains(out+errOut, "Name of the new layout:") || !strings.Contains(out+errOut, "Start from") || !strings.Contains(out, "Created the layout asked from ") {
		t.Errorf("create asks: code %d\n%s%s", code, out, errOut)
	}
}

func TestListAccountsSameTypeInSequence(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  a:\n    type: fake\n    login: a\n    password: s3cret\n  b:\n    type: fake\n    login: b\n    password: s3cret\n  c:\n    type: fake\n    login: c\n    password: s3cret\n  lo:\n    type: listonly\n    password: s3cret\n")
	var (
		mu       sync.Mutex
		running  = map[string]int{}
		maxSame  = map[string]int{}
		overlap  bool
		fakeSeen bool
	)
	listGate = func(typ string, start bool) {
		mu.Lock()
		defer mu.Unlock()
		if start {
			running[typ]++
			if running[typ] > maxSame[typ] {
				maxSame[typ] = running[typ]
			}
			for other, n := range running {
				if other != typ && n > 0 {
					overlap = true
				}
			}
			if typ == "fake" {
				fakeSeen = true
			}
			// hold the listing for a moment, so that overlaps would show
			mu.Unlock()
			time.Sleep(15 * time.Millisecond)
			mu.Lock()
		} else {
			running[typ]--
		}
	}
	defer func() { listGate = nil }()
	out, _, code := e.run("", false, "server", "list", "-o", "name")
	if code != 0 || strings.Count(out, "\n") != 8 {
		t.Fatalf("list: code %d\n%s", code, out)
	}
	if maxSame["fake"] != 1 || !fakeSeen {
		t.Errorf("accounts of one type must run one after the other, saw %d at once", maxSame["fake"])
	}
	if !overlap {
		t.Error("different types should run at the same time, but did not overlap")
	}
}

func TestReinstallWithoutRescueTakesTheStockWay(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	// no root access, and the provider refuses the stock image: its error
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes")
	if code != 4 || !strings.Contains(errOut, "Checking root access to 203.0.113.10 over SSH ... no") || !strings.Contains(errOut, "ump: account nr: fake: no stock images") {
		t.Errorf("stock image refused: code %d, %q", code, errOut)
	}
	// with one: the provider reinstalls its own system (here with the keys
	// the machine was created with, as at DigitalOcean), and the
	// installation goes on from there
	fakeStock, fakeStockKeys = true, "the SSH keys the machine was created with"
	defer func() { fakeStock, fakeStockKeys = false, "" }()
	resetFakePower()
	e.stockRoot = true
	defer func() { e.stockRoot = false }()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes")
	if code != 0 {
		t.Fatalf("stock way: code %d, %q", code, errOut)
	}
	for _, want := range []string{
		"Provider type norescue has no rescue system and the running system does not let UMP in; the provider reinstalls web-01 with a stock Linux first",
		"web-01 runs the provider's Debian 13 now, with the SSH keys the machine was created with; waiting for root access over SSH (up to 15m) ... yes",
		"web-01 is up: Ubuntu 26.04.1 LTS",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stock way lacks %q in\n%s", want, errOut)
		}
	}
	if got := strings.Join(fakePower.actions, ","); !strings.HasPrefix(got, "stock:ump") {
		t.Errorf("actions: %s", got)
	}
	// rescue never reinstalls
	resetFakePower()
	_, _, code = e.run("", false, "server", "rescue", "web-01", "--yes")
	if code == 0 || len(fakePower.actions) != 0 {
		t.Errorf("rescue must not reinstall: code %d, actions %v", code, fakePower.actions)
	}
}

func TestRescueKeylessIsRefusedEarly(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  kl:\n    type: keyless\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes")
	if code != 5 || !strings.Contains(errOut, "the provider's rescue system of web-01 takes no SSH key (the password goes by mail)") || !strings.Contains(errOut, "ump server rescue --no-image") {
		t.Errorf("keyless: code %d, %q", code, errOut)
	}
	if len(fakePower.actions) != 0 {
		t.Errorf("nothing may be enabled or rebooted: %v", fakePower.actions)
	}
	e.write(testConfig)
	fakeKeyless = true
	defer func() { fakeKeyless = false }()

	// with a provider that can install its stock image, the reinstall takes
	// that way: stock image with UMP's key, root over SSH, then the direct way
	fakeStock = true
	defer func() { fakeStock = false }()
	resetFakePower()
	e.stockRoot = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("stock way: code %d, %q", code, errOut)
	}
	for _, want := range []string{
		"The provider's rescue system of web-01 takes no SSH key; the provider reinstalls web-01 with a stock Linux first",
		"web-01 runs the provider's Debian 13 now, with UMP's key for root; waiting for root access over SSH (up to 15m) ... yes after 0s (Debian GNU/Linux 13 (trixie)).",
		"Loading the UMP rescue image from https://ump.zhurko.de/dl/rescue on web-01.",
		"web-01 is up: Ubuntu 26.04.1 LTS",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stock way lacks %q in\n%s", want, errOut)
		}
	}
	if got := strings.Join(fakePower.actions, ","); !strings.HasPrefix(got, "stock:ump") || strings.Contains(got, "rescue") {
		t.Errorf("actions: %s", got)
	}
	// rescue itself never takes that way
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.Contains(errOut, "takes no SSH key") || len(fakePower.actions) != 0 {
		t.Errorf("rescue must not reinstall: code %d, actions %v, %q", code, fakePower.actions, errOut)
	}
	e.stockRoot = false

	// --no-image still works, it is the way in with the mailed password
	_, _, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--no-image", "--account", "one")
	if code != 0 || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard" {
		t.Errorf("--no-image: code %d, actions %v", code, fakePower.actions)
	}
}

func TestConfigFirewall(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	out, _, code := e.run("", false, "config", "fw", "list")
	if code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(strings.Join(strings.Fields(out), " "), "default drop icmp, ssh ok ICMP and SSH") || !strings.Contains(strings.Join(strings.Fields(out), " "), "open accept everything ok") || !strings.Contains(out, "tcp/80, tcp/443") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "config", "firewall", "get", "web")
	if code != 0 || !strings.HasPrefix(out, "web:\n") || !strings.Contains(out, "tcp/443") {
		t.Errorf("get: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "fw", "get", "nope"); code != 1 || !strings.Contains(errOut, `no firewall "nope"`) || !strings.Contains(errOut, "ump config firewall list") {
		t.Errorf("get missing: code %d, %q", code, errOut)
	}

	// create copies an entry; the comments of the file survive
	out, _, code = e.run("", false, "config", "fw", "create", "mail")
	if code != 0 || !strings.Contains(out, "Created the firewall mail from default") {
		t.Errorf("create: code %d\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "firewalls.yaml"))
	if !strings.Contains(string(data), "# Named firewalls") || !strings.Contains(string(data), "\n  mail:\n") {
		t.Errorf("file after create:\n%s", data)
	}

	// a profile names the new firewall; --firewall picks another
	os.WriteFile(filepath.Join(dir, "p.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nfirewall: mail\nusers:\n  - {name: root, keys: [ump]}\n"), 0o600)
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--account", "one"); code != 0 {
		t.Errorf("reinstall with the new firewall: code %d, %q", code, errOut)
	}
	if fw := e.lastSpec()["firewall"].(map[string]any); fw["name"] != "mail" || fw["policy"] != "drop" || len(fw["allow"].([]any)) != 2 {
		t.Errorf("spec firewall: %v", fw)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--firewall", "open", "--account", "one"); code != 0 {
		t.Errorf("reinstall --firewall: code %d, %q", code, errOut)
	}
	if fw := e.lastSpec()["firewall"].(map[string]any); fw["name"] != "open" || fw["policy"] != "accept" {
		t.Errorf("spec firewall with --firewall open: %v", fw)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--firewall", "nope", "--account", "one"); code != 1 || !strings.Contains(errOut, `firewall "nope" is not in firewalls.yaml (known: default, mail, open, web)`) {
		t.Errorf("reinstall with an unknown firewall: code %d, %q", code, errOut)
	}

	// edit checks the file; a broken one is kept and named
	e.editorHook = func(path string) error {
		return os.WriteFile(path, []byte("firewalls:\n  bad:\n    allow: [tcp/70000]\n"), 0o600)
	}
	if _, errOut, code := e.run("", true, "config", "fw", "edit"); code != 1 || !strings.Contains(errOut, "the file is kept, but it is not valid") || !strings.Contains(errOut, "firewall bad: port 70000 is out of range") {
		t.Errorf("broken edit: code %d, %q", code, errOut)
	}
	if out, _, code := e.run("", false, "config", "fw", "list"); code != 0 || !strings.Contains(out, "invalid") {
		t.Errorf("list of the broken file:\n%s", out)
	}
	e.editorHook = nil

	// delete
	e.writeProfiles()
	if out, _, code := e.run("", false, "config", "fw", "delete", "web", "--yes"); code != 0 || !strings.Contains(out, "Deleted the firewall web.") {
		t.Errorf("delete: code %d\n%s", code, out)
	}
	if out, _, _ := e.run("", false, "config", "fw", "list", "--no-headers"); strings.Contains(out, "web") {
		t.Errorf("still listed:\n%s", out)
	}
}

func TestUpdateHint(t *testing.T) {
	e := newTestEnv(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bin/latest/VERSION" {
			hits++
			io.WriteString(w, "v0.2.0\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	was := Version
	Version = "v0.1.0"
	defer func() { Version = was }()

	// a successful command checks once and says what is new
	_, errOut, code := e.run("", false, "config", "isp", "view")
	if code != 0 || !strings.Contains(errOut, "ump v0.2.0 is available (this is v0.1.0): curl -fsSL "+srv.URL+"/get.sh | sh") || hits != 1 {
		t.Errorf("first run: code %d, hits %d, %q", code, hits, errOut)
	}
	// not again the same day; again the next day
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); strings.Contains(errOut, "available") || hits != 1 {
		t.Errorf("second run: hits %d, %q", hits, errOut)
	}
	e.now = e.now.Add(25 * time.Hour)
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); !strings.Contains(errOut, "available") || hits != 2 {
		t.Errorf("next day: hits %d, %q", hits, errOut)
	}
	// nothing when up to date, off, for version, or for a development build
	e.now = e.now.Add(25 * time.Hour)
	Version = "v0.2.0"
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); strings.Contains(errOut, "available") || hits != 3 {
		t.Errorf("up to date: hits %d, %q", hits, errOut)
	}
	e.now = e.now.Add(25 * time.Hour)
	Version = "v0.1.0"
	t.Setenv("UMP_NO_UPDATE_CHECK", "1")
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); errOut != "" || hits != 3 {
		t.Errorf("env off: hits %d, %q", hits, errOut)
	}
	t.Setenv("UMP_NO_UPDATE_CHECK", "")
	if _, errOut, _ := e.run("", false, "version"); errOut != "" || hits != 3 {
		t.Errorf("version: hits %d, %q", hits, errOut)
	}
	e.write(testConfig + "storage: " + srv.URL + "\nupdates: false\n")
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); errOut != "" || hits != 3 {
		t.Errorf("config off: hits %d, %q", hits, errOut)
	}
	e.write(testConfig + "storage: " + srv.URL + "\n")
	Version = "dev"
	if _, errOut, _ := e.run("", false, "config", "isp", "view"); errOut != "" || hits != 3 {
		t.Errorf("dev: hits %d, %q", hits, errOut)
	}
	// a failed command does not check
	Version = "v0.1.0"
	if _, _, code := e.run("", false, "config", "os", "get", "nope"); code == 0 || hits != 3 {
		t.Errorf("failed command: code %d, hits %d", code, hits)
	}

	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true}, {"v0.1.0", "v0.2.0", false}, {"v1.0.0", "v0.9.9", true}, {"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0-rc1", true}, {"v0.1.0-rc1", "v0.1.0", false}, {"v0.1.0-rc2", "v0.1.0-rc1", true}, {"v0.1.10", "v0.1.9", true},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%s, %s) = %v", c.latest, c.current, got)
		}
	}
}

func TestConfigOSUpdate(t *testing.T) {
	e := newTestEnv(t)
	published := map[string]string{}
	for _, name := range profileFiles {
		data, _ := os.ReadFile(filepath.Join("..", "..", "profiles", name))
		published[name] = string(data)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := published[strings.TrimPrefix(r.URL.Path, "/profiles/")]; ok && strings.HasPrefix(r.URL.Path, "/profiles/") {
			io.WriteString(w, data)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	if _, _, code := e.run("", false, "config", "os", "list"); code != 0 {
		t.Fatal("first list failed")
	}
	if out, _, code := e.run("", false, "config", "os", "update"); code != 0 || !strings.Contains(out, "ubuntu-26.04.yaml: up to date\nubuntu-24.04.yaml: up to date\nubuntu-22.04.yaml: up to date\nlayouts.yaml: updated\nfirewalls.yaml: updated\nusers.yaml: updated\n") {
		t.Errorf("update when current: code %d\n%s", code, out)
	}

	// a published profile is not for editing; the user's own profile and
	// entries survive an update, a changed published profile is replaced
	if _, errOut, code := e.run("", false, "config", "os", "edit", "ubuntu-24.04"); code != 1 || !strings.Contains(errOut, "ubuntu-24.04 is a published profile, which ump config os update replaces; make your own from it: ump config os create NAME --from ubuntu-24.04") {
		t.Errorf("edit of a published profile: code %d, %q", code, errOut)
	}
	os.WriteFile(filepath.Join(dir, "ubuntu-26.04.yaml"), []byte(published["ubuntu-26.04.yaml"]+"timezone: Europe/Berlin\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "mine.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	if _, _, code := e.run("", false, "config", "dl", "create", "mine", "--from", "lvm-linear"); code != 0 {
		t.Fatal("layout create failed")
	}
	if _, _, code := e.run("", false, "config", "user", "create", "ops", "--from", "ump"); code != 0 {
		t.Fatal("user create failed")
	}
	for name := range published {
		published[name] += "# newer\n"
	}
	out, _, code := e.run("", false, "config", "os", "update")
	if code != 0 || !strings.Contains(out, "ubuntu-26.04.yaml: updated\n") || !strings.Contains(out, "layouts.yaml: updated; your layouts kept: mine\n") || !strings.Contains(out, "users.yaml: updated; your users kept: ops\n") || !strings.Contains(out, "firewalls.yaml: updated\n") {
		t.Errorf("update: code %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "ubuntu-26.04.yaml")); string(data) != published["ubuntu-26.04.yaml"] {
		t.Errorf("the published profile was not replaced:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); err != nil {
		t.Error("mine.yaml was touched")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "layouts.yaml"))
	if !strings.Contains(string(data), "# newer") || !strings.Contains(string(data), "\n  mine:\n") || !strings.Contains(string(data), "\n  lvm-linear:\n") {
		t.Errorf("layouts.yaml after update:\n%s", data)
	}
	if out, _, _ := e.run("", false, "config", "user", "list", "--no-headers"); !strings.Contains(out, "ops") || !strings.Contains(out, "ump") {
		t.Errorf("users after update:\n%s", out)
	}
	if out, _, _ := e.run("", false, "config", "os", "list"); !strings.Contains(out, "mine") || strings.Contains(out, "users") {
		t.Errorf("list after update:\n%s", out)
	}

	// the default profile can be changed, and is marked
	if out, _, code := e.run("", false, "config", "os", "default"); code != 0 || out != "ubuntu-26.04\n" {
		t.Errorf("default: code %d, %q", code, out)
	}
	if out, _, code := e.run("", false, "config", "os", "default", "mine"); code != 0 || !strings.Contains(out, "The default profile is mine now") {
		t.Errorf("set default: code %d, %q", code, out)
	}
	if out, _, _ := e.run("", false, "config", "os", "list"); !strings.Contains(strings.Join(strings.Fields(out), " "), "* mine ubuntu-26.04") || strings.Contains(strings.Join(strings.Fields(out), " "), "* ubuntu-26.04") {
		t.Errorf("list with the new default:\n%s", out)
	}
	if !strings.Contains(e.read(), "default_profile: mine\n") {
		t.Errorf("config after default:\n%s", e.read())
	}
	if _, errOut, code := e.run("", false, "config", "os", "default", "nope"); code != 1 || !strings.Contains(errOut, `no profile "nope"`) {
		t.Errorf("default to a missing profile: code %d, %q", code, errOut)
	}
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 0 || !strings.Contains(errOut, "(profile mine from ") {
		t.Errorf("reinstall takes the new default: code %d, %q", code, errOut)
	}
}

func TestProfileWizard(t *testing.T) {
	e := newTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/os/index.json":
			io.WriteString(w, `{"images": ["ubuntu-22.04", "ubuntu-24.04", "ubuntu-26.04"]}`)
		case strings.HasSuffix(r.URL.Path, "/amd64/manifest.json"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/os/"), "/amd64/manifest.json")
			fmt.Fprintf(w, `{"version": "2026.10.13", "os": %q, "kernel": "7.0.0-14-generic"}`, name)
		case strings.HasPrefix(r.URL.Path, "/profiles/"):
			data, err := os.ReadFile(filepath.Join("..", "..", "profiles", filepath.Base(r.URL.Path)))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles")

	// image 2 (24.04), the default layout, the web firewall, admin ump with
	// bash and UMP's key alone, mitigations off, IPv6 on, UTC, the
	// userdata ubuntu (an unknown name first, refused), renamed
	// interfaces, no provision step, a description, no editor
	script := "2\n\nweb\nump, ops\ny\n\nump\nOps people\n\ny\n\nnope\nubuntu\n\n\nWeb servers\n\n"
	out, errOut, code := e.run(script, true, "config", "os", "create", "web")
	if code != 0 {
		t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"Operating system image (from " + srv.URL + "/os/)", "1) ubuntu-22.04  (kernel 7.0.0-14, built 2026.10.13)", "Choice [3]:",
		"Disk layout (ump config disk-layout)", "1) lvm-example", "Firewall (ump config firewall)", "2) open  (no firewall)", "Users come from users.yaml (ump config user): ump.",
		"Users (comma-separated; a new name is defined next) [ump]:", "ops is not in users.yaml yet; defining it now.", "ops: sudo without a password (an administrator)? [Y/n]:", "Shell of ops", "Keys of ops", "Description of ops", "Added ops to " + dir + "/users.yaml.", "mitigations on?", "IPv6 on?", "Time zone [UTC]:", "Userdata comes from userdata.yaml (ump config userdata): ubuntu.", "Userdata (comma-separated, in order; none for nothing) [ubuntu]:", "nope is not in userdata.yaml; ump config userdata create nope makes it.", "Uniform interface names", "Provision step", "Description",
		"Saved " + dir + "/web.yaml. Use it with ump server reinstall NAME --profile web", "Open it in your editor now? [y/N]:"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("wizard output lacks %q:\n%s", want, errOut)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "web.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# Profile web, made by ump config os create on 2026-10-12", "name: web\n", "description: Web servers\n", "os: ubuntu-24.04\n", "timezone: UTC\n",
		"kernel:\n  mitigations: false\n  ipv6: true\n", "disks: lvm-linear\n", "users:\n  - ump\n  - ops\n",
		"userdata:\n  - ubuntu\n", "firewall: web\n", "provision:\n  type: none\n", "network:\n  rename: true\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("profile lacks %q:\n%s", want, text)
		}
	}
	if out, _, code := e.run("", false, "config", "os", "list"); code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "web ubuntu-24.04 lvm-linear ump,ops web yours Web servers") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	// without a name, the wizard asks for it first
	if _, errOut, code := e.run("web2\n2\n\nweb\nump\n\ny\n\nubuntu\n\n\nMore web\n\n", true, "config", "os", "create"); code != 0 || !strings.Contains(errOut, "Profiles in "+dir+": ") || !strings.Contains(errOut, "Name of the new profile:") || !strings.Contains(errOut, "Saved "+dir+"/web2.yaml") {
		t.Errorf("create without a name: code %d\n%s", code, errOut)
	}
	if out, _, code := e.run("", false, "config", "user", "list"); code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "ops yes /bin/bash ump ok Ops people") {
		t.Errorf("user list: code %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "users.yaml")); !strings.Contains(string(data), "# Named users") || !strings.Contains(string(data), "\n  ops:\n") {
		t.Errorf("users.yaml after the wizard:\n%s", data)
	}
	// the profile installs: the spec carries the choices
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "web", "--account", "one"); code != 0 {
		t.Errorf("reinstall with the wizard's profile: code %d, %q", code, errOut)
	}
	spec := e.lastSpec()
	if spec["os"] != "ubuntu-24.04" || spec["kernel"].(map[string]any)["ipv6"] != true || spec["firewall"].(map[string]any)["name"] != "web" || len(spec["userdata"].([]any)) != 1 {
		t.Errorf("spec: %v", spec)
	}
	if users := spec["users"].([]any); len(users) != 2 || users[0].(map[string]any)["name"] != "ump" || users[1].(map[string]any)["name"] != "ops" || users[1].(map[string]any)["shell"] != "/bin/bash" {
		t.Errorf("spec users: %v", spec["users"])
	}

	// another name for the image, and the copy with --from stays
	script = "4\ndebian-13\n\n\n\n\n\n\n\n\n\n\n\n"
	if _, errOut, code := e.run(script, true, "config", "os", "create", "deb"); code != 0 || !strings.Contains(errOut, "Image name (os/<name>/ on the storage) [ubuntu-26.04]:") {
		t.Errorf("another image: code %d\n%s", code, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "deb.yaml")); !strings.Contains(string(data), "os: debian-13\n") {
		t.Errorf("deb profile:\n%s", data)
	}
	if _, errOut, code := e.run("", false, "config", "os", "create", "copy", "--from", "web"); code != 0 || strings.Contains(errOut, "Operating system image") {
		t.Errorf("--from: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "os", "create", "plain"); code != 0 || strings.Contains(errOut, "Operating system image") {
		t.Errorf("without a terminal: code %d, %q", code, errOut)
	}
}

func TestReinstallSet(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	e.directRoot = true
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one",
		"--set", "kernel.extra=quiet", "--set", "users[0]={name: ump, sudo: true, keys: [ump]}", "--set", "users[ump].shell=/bin/bash", "--set", "userdata=[{name: x, script: 'echo x'}]", "--set", "network.rename=false")
	if code != 0 {
		t.Fatalf("reinstall --set: code %d, %q", code, errOut)
	}
	spec := e.lastSpec()
	if spec["kernel"].(map[string]any)["extra"] != "quiet" || spec["users"].([]any)[0].(map[string]any)["shell"] != "/bin/bash" || len(spec["userdata"].([]any)) != 1 || spec["network"].(map[string]any)["rename"] != false {
		t.Errorf("spec: %v", spec)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--set", "kernel.colour=blue"); code != 2 || !strings.Contains(errOut, "field colour not found") {
		t.Errorf("unknown field: code %d, %q", code, errOut)
	}
}

func TestConfigKeyNamed(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")
	const line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTeN oleg@laptop"
	pub := filepath.Join(dir, "work.pub")
	os.WriteFile(pub, []byte("# two keys\n"+line+"\n"+strings.Replace(line, "laptop", "desk", 1)+"\n"), 0o600)

	if out, _, code := e.run("", false, "config", "key", "add", "laptop", "--line", line); code != 0 || !strings.Contains(out, "Added the key laptop (ssh-ed25519 oleg@laptop).") {
		t.Errorf("add --line: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "key", "add", "work", "--file", pub); code != 0 || !strings.Contains(out, "Added the key work (ssh-ed25519 oleg@laptop).\nAdded the key work-2 (ssh-ed25519 oleg@desk).") {
		t.Errorf("add --file: code %d\n%s", code, out)
	}
	e.agentKeys = []string{line}
	if out, _, code := e.run("", false, "config", "key", "add", "agent-key", "--agent"); code != 0 || !strings.Contains(out, "Added the key agent-key") {
		t.Errorf("add --agent: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "key", "add", "laptop", "--line", line); code != 1 || !strings.Contains(errOut, "key laptop exists already") {
		t.Errorf("add twice: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "key", "add", "ump", "--line", line); code != 2 || !strings.Contains(errOut, "not ump, local or agent") {
		t.Errorf("reserved name: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "key", "add", "bad", "--line", "not a key"); code != 1 || !strings.Contains(errOut, "is not a public key line") {
		t.Errorf("bad line: code %d, %q", code, errOut)
	}
	out, _, code := e.run("", false, "config", "key", "list")
	flat := strings.Join(strings.Fields(out), " ")
	if code != 0 || !strings.Contains(flat, "NAME TYPE COMMENT FINGERPRINT NOTE ump ssh-ed25519") || !strings.Contains(flat, "laptop ssh-ed25519 oleg@laptop SHA256:") || !strings.Contains(flat, "work-2 ssh-ed25519 oleg@desk") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "keys.yaml")); !strings.Contains(string(data), "  laptop: "+line+"\n") {
		t.Errorf("keys.yaml:\n%s", data)
	}

	// a user names the keys; the spec carries the lines
	os.WriteFile(filepath.Join(dir, "profiles", "p.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: ops, sudo: true, keys: [laptop, work-2, ump]}\n"), 0o600)
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--account", "one"); code != 0 {
		t.Fatalf("reinstall with named keys: code %d, %q", code, errOut)
	}
	keys := e.lastSpec()["users"].([]any)[0].(map[string]any)["keys"].([]any)
	if len(keys) != 3 || keys[0] != line || !strings.HasSuffix(keys[1].(string), "oleg@desk") {
		t.Errorf("spec keys: %v", keys)
	}
	os.WriteFile(filepath.Join(dir, "profiles", "p.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: ops, sudo: true, keys: [nothing]}\n"), 0o600)
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--profile", "p", "--account", "one"); code != 1 || !strings.Contains(errOut, `key "nothing": not a key reference (ump, local, agent, a named key from ump config key list, a file, or a public key line)`) {
		t.Errorf("unknown key name: code %d, %q", code, errOut)
	}

	if out, _, code := e.run("", false, "config", "key", "remove", "work-2"); code != 0 || !strings.Contains(out, "Removed the key work-2.") {
		t.Errorf("remove: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "key", "remove", "work-2"); code != 1 || !strings.Contains(errOut, `no key "work-2"`) {
		t.Errorf("remove twice: code %d, %q", code, errOut)
	}
	if out, _, _ := e.run("", false, "c", "k", "ls"); !strings.Contains(out, "laptop      ssh-ed25519   oleg@laptop") {
		t.Errorf("short form lists the keys: %q", out)
	}
}

func TestConfigKeyAddAsks(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	const line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTeN oleg@laptop"
	// add without arguments on a terminal: name, where from, the line
	out, errOut, code := e.run("laptop\n2\n"+line+"\n", true, "config", "key", "add")
	if code != 0 {
		t.Fatalf("code %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"Name for the key:", "Where from?", "1) a public key file", "Public key line", "Added the key laptop (ssh-ed25519 oleg@laptop)."} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("output lacks %q:\n%s%s", want, out, errOut)
		}
	}
	// a flag given: only the name is asked
	pub := filepath.Join(t.TempDir(), "k.pub")
	os.WriteFile(pub, []byte(line+"\n"), 0o600)
	if out, errOut, code := e.run("desk\n", true, "config", "key", "add", "--file", pub); code != 0 || strings.Contains(out+errOut, "Where from?") || !strings.Contains(out, "Added the key desk") {
		t.Errorf("with --file: code %d\n%s%s", code, out, errOut)
	}
	// a reserved name is refused
	if _, errOut, code := e.run("ump\n2\n"+line+"\n", true, "config", "key", "add"); code != 2 || !strings.Contains(errOut, `key name "ump"`) {
		t.Errorf("reserved name: code %d, %q", code, errOut)
	}
	// without a terminal the name is required
	if _, errOut, code := e.run("", false, "config", "key", "add"); code != 2 || !strings.Contains(errOut, "a key name is needed without a terminal") {
		t.Errorf("without a terminal: code %d, %q", code, errOut)
	}
	if out, _, code := e.run("", true, "config", "key", "show", "-o", "json"); code != 0 || !strings.Contains(out, `"public_key": "ssh-ed25519`) {
		t.Errorf("show -o json: code %d, %q", code, out)
	}
}

func TestServerSetPriceAndAttributes(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	out, _, code := e.run("", false, "server", "set", "web-01", "--price", "39,90", "--attr", "owner=alice", "--attr", "role=web", "--account", "one")
	if code != 0 || !strings.Contains(out, "fake/1 (web-01, 203.0.113.10): price 39.90 EUR/mo; attributes owner=alice,role=web.") {
		t.Errorf("set: code %d\n%s", code, out)
	}
	if st := e.readState(); !strings.Contains(st, "    price:\n      amount: 39.9\n      currency: EUR\n      period: month\n") || !strings.Contains(st, "    attributes:\n      owner: alice\n      role: web\n") {
		t.Errorf("state after set:\n%s", st)
	}
	out, _, code = e.run("", false, "server", "list", "--account", "one", "-o", "wide")
	if code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "web-01 203.0.113.10 eu-1 running 2001:db8::/64 m1 cloud 39.90 EUR/mo owner=alice,role=web") {
		t.Errorf("wide: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "server", "list", "web-01", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"price": {`) || !strings.Contains(out, `"source": "manual"`) || !strings.Contains(out, `"owner": "alice"`) {
		t.Errorf("json: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "server", "show", "web-01", "--account", "one")
	if code != 0 || !strings.Contains(out, "\nUMP:\n  Price:       39.90 EUR/mo (manual)\n  Attributes:  owner=alice\n               role=web\n") {
		t.Errorf("show: code %d\n%s", code, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--price", "abc"}, `--price "abc" is not an amount`},
		{[]string{"--price", "5", "--currency", "euros"}, `--currency "euros" is not a three-letter code`},
		{[]string{"--price", "5", "--period", "year"}, `--period "year" is not month or hour`},
		{[]string{"--currency", "EUR"}, "--currency and --period go with --price"},
		{[]string{"--attr", "bad key=1"}, `--attr "bad key=1": expected KEY=VALUE`},
		{[]string{"--price", "5", "--clear-price"}, "--price and --clear-price exclude each other"},
	} {
		if _, errOut, code := e.run("", false, append([]string{"server", "set", "web-01", "--account", "one"}, c.args...)...); code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	out, _, code = e.run("", false, "server", "set", "web-01", "--price", "2", "--currency", "usd", "--period", "hour", "--unset-attr", "role", "--account", "one")
	if code != 0 || !strings.Contains(out, "price 2.00 USD/h; attributes owner=alice.") {
		t.Errorf("set again: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "server", "set", "web-01", "--clear-price", "--unset-attr", "owner", "--account", "one")
	if code != 0 || !strings.Contains(out, "no price set; no attributes.") || strings.Contains(e.readState(), "fake/1") {
		t.Errorf("clear all: code %d\n%s\n%s", code, out, e.readState())
	}
}

func TestManualServers(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	if _, errOut, code := e.run("", false, "server", "add", "rack-07"); code != 2 || !strings.Contains(errOut, "give the server's address") {
		t.Errorf("add without address: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "add", "rack-07", "--ipv4", "nope"); code != 2 || !strings.Contains(errOut, `--ipv4 "nope" is not an address`) {
		t.Errorf("add bad address: code %d, %q", code, errOut)
	}
	out, _, code := e.run("", false, "server", "add", "rack-07", "--ipv4", "203.0.113.10", "--location", "basement", "--product", "Dell R640", "--note", "KVM at 192.0.2.250")
	if code != 0 || !strings.Contains(out, "Added manual/rack-07 (rack-07, 203.0.113.10). No provider API stands behind it") {
		t.Errorf("add: code %d\n%s", code, out)
	}
	if st := e.readState(); !strings.Contains(st, "manual:\n  rack-07:\n    ipv4: 203.0.113.10\n    location: basement\n    product: Dell R640\n    kind: bare-metal\n    note: KVM at 192.0.2.250\n") || strings.Contains(e.read(), "manual") {
		t.Errorf("state after add:\n%s\nconfig:\n%s", st, e.read())
	}
	if _, errOut, code := e.run("", false, "server", "add", "rack-07", "--ipv4", "203.0.113.10"); code != 1 || !strings.Contains(errOut, "manual server rack-07 exists already") {
		t.Errorf("add twice: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "server", "list", "--account", "manual")
	if code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "manual manual rack-07 rack-07 203.0.113.10 basement manual") {
		t.Errorf("list: code %d\n%s", code, out)
	}
	out, _, code = e.run("", false, "server", "show", "rack-07", "--account", "manual")
	if code != 0 || !strings.Contains(out, "Product:   Dell R640") || !strings.Contains(out, "\nManual:\n  Note:    KVM at 192.0.2.250\n") || !strings.Contains(out, "Rescue:  none:") {
		t.Errorf("show: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "isp", "types"); code != 0 || !strings.Contains(out, "manual") {
		t.Errorf("types: code %d\n%s", code, out)
	}
	// prices and attributes work on it too
	if out, _, code := e.run("", false, "server", "set", "rack-07", "--price", "89", "--account", "manual"); code != 0 || !strings.Contains(out, "price 89.00 EUR/mo") {
		t.Errorf("set price: code %d\n%s", code, out)
	}
	// reinstall and rescue warn: no rescue system; the direct way works when the system lets UMP in
	e.writeProfiles()
	e.directRoot = true
	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "rack-07", "--yes", "--account", "manual")
	if code != 0 || !strings.Contains(errOut, "Warning: manual/rack-07 (rack-07, 203.0.113.10) has no provider rescue system behind it (provider type manual). ump reaches it over SSH alone; if the installation fails or the new system does not boot, the machine is yours to recover") || !strings.Contains(errOut, "rack-07 is up:") {
		t.Errorf("reinstall: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "rack-07", "--account", "manual"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("reinstall without --yes: code %d, %q", code, errOut)
	}
	e.directRoot = false
	e.inImage = false
	_, errOut, code = e.run("", false, "server", "rescue", "rack-07", "--yes", "--account", "manual")
	if code != 1 || !strings.Contains(errOut, "Warning: manual/rack-07") || !strings.Contains(errOut, "no root access to 203.0.113.10 over SSH, and provider type manual has no rescue system") {
		t.Errorf("rescue without access: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "reboot", "rack-07", "--yes", "--account", "manual"); code == 0 || !strings.Contains(errOut, "cannot") {
		t.Errorf("reboot: code %d, %q", code, errOut)
	}
	// remove
	if _, errOut, code := e.run("", false, "server", "remove", "web-01"); code != 1 || !strings.Contains(errOut, `no manual server "web-01"`) {
		t.Errorf("remove a provider's server: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "remove", "rack-07"); code != 3 || !strings.Contains(errOut, "needs confirmation") {
		t.Errorf("remove without --yes: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "server", "remove", "rack-07", "--yes")
	if code != 0 || !strings.Contains(out, "Removed manual/rack-07 (rack-07, 203.0.113.10) from the state.") {
		t.Errorf("remove: code %d\n%s", code, out)
	}
	if st := e.readState(); strings.Contains(st, "rack-07") {
		t.Errorf("state after remove:\n%s", st)
	}
}

func TestConfigState(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles", "db.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	local := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")

	// no backend: the home is the local directory
	out, _, code := e.run("", false, "config", "state", "view")
	if code != 0 || !strings.Contains(out, "Backend:   none, the state is in "+local) || !strings.Contains(out, "Accounts:  2") {
		t.Errorf("view: code %d\n%s", code, out)
	}
	if short, _, code := e.run("", false, "c", "st", "v"); code != 0 || short != out {
		t.Errorf("short form is not the view: code %d\n%s", code, short)
	}
	if _, errOut, code := e.run("", false, "config", "state", "pull"); code != 1 || !strings.Contains(errOut, "no backend configured") {
		t.Errorf("pull without a backend: code %d, %q", code, errOut)
	}
	if _, _, code := e.run("", false, "server", "set", "web-01", "--profile", "db", "--attr", "owner=alice", "--account", "one"); code != 0 {
		t.Fatal("set failed")
	}
	if _, _, code := e.run("", false, "server", "add", "rack-07", "--ipv4", "203.0.113.77"); code != 0 {
		t.Fatal("add failed")
	}

	// migrate the home to a directory backend: everything goes there,
	// the pointer is written, the next commands work on the mirror
	remote := filepath.Join(t.TempDir(), "fleet", "ump")
	if _, errOut, code := e.run("", false, "config", "state", "migrate"); code != 2 || !strings.Contains(errOut, "--to URL is required") {
		t.Errorf("migrate without --to: code %d, %q", code, errOut)
	}
	out, _, code = e.run("", false, "config", "state", "migrate", "--to", "file://"+remote)
	if code != 0 || !strings.Contains(out, "files to file://"+remote+" and wrote "+filepath.Join(local, "backend.yaml")) {
		t.Errorf("migrate: code %d\n%s", code, out)
	}
	for _, f := range []string{"state.yaml", "profiles/db.yaml", "profiles/ubuntu-26.04.yaml", "profiles/users.yaml"} {
		if _, err := os.Stat(filepath.Join(remote, f)); err != nil {
			t.Errorf("%s not in the backend: %v", f, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(local, "backend.yaml")); !strings.Contains(string(data), "url: file://"+remote+"\n") {
		t.Errorf("backend.yaml:\n%s", data)
	}
	out, _, code = e.run("", false, "config", "state", "view")
	mirror := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Mirror:") {
			mirror = strings.TrimSpace(strings.TrimPrefix(line, "Mirror:"))
		}
	}
	if code != 0 || !strings.Contains(out, "Backend:   file://"+remote+" (backend.yaml)") || !strings.HasPrefix(mirror, filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "homes")) || !strings.Contains(out, "Servers:   1 with settings, 0 with an installation recorded, 1 added by hand") {
		t.Errorf("view after migrate: code %d\n%s", code, out)
	}
	if out, errOut, code := e.run("", false, "server", "list", "-o", "name", "--account", "one", "--account", "manual"); code != 0 || !strings.Contains(out, "fake/1\n") || !strings.Contains(out, "manual/rack-07\n") {
		t.Errorf("list through the mirror: code %d, %q, %q", code, out, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "state", "migrate", "--to", "file://"+remote); code != 1 || !strings.Contains(errOut, "the state is at file://"+remote+" already") {
		t.Errorf("migrate to the same: code %d, %q", code, errOut)
	}

	// a change goes to the backend directly (the state) or with the push
	// after the command (a profile)
	if _, _, code := e.run("", false, "server", "set", "db-01", "--price", "5", "--account", "one"); code != 0 {
		t.Fatal("set through the backend failed")
	}
	if data, _ := os.ReadFile(filepath.Join(remote, "state.yaml")); !strings.Contains(string(data), "fake/2") {
		t.Errorf("state in the backend:\n%s", data)
	}
	if _, _, code := e.run("", false, "config", "os", "create", "web", "--from", "db"); code != 0 {
		t.Fatal("create failed")
	}
	if _, err := os.Stat(filepath.Join(remote, "profiles", "web.yaml")); err != nil {
		t.Errorf("the new profile was not pushed: %v", err)
	}
	// an installation is recorded and locks go to the backend
	e.directRoot = true
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(remote, "state.yaml")); !strings.Contains(string(data), "    installed:\n      os: ubuntu-26.04\n") || !strings.Contains(string(data), "      profile: db\n") {
		t.Errorf("install record in the backend:\n%s", data)
	}
	if out, _, _ := e.run("", false, "server", "show", "web-01", "--account", "one"); !strings.Contains(out, "  Installed:   ubuntu-26.04 (profile db, layout lvm-linear, administrator ump) on 2026-10-12 09:00 UTC") {
		t.Errorf("show after reinstall:\n%s", out)
	}
	b := &backend.Local{Dir: remote}
	if err := b.Lock(context.Background(), "fake/1", backend.LockInfo{Key: "fake/1", Who: "bob@desk", What: "reinstall"}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 3 || !strings.Contains(errOut, "fake/1 is locked by bob@desk") || !strings.Contains(errOut, "ump config state unlock fake/1 forces it") {
		t.Errorf("reinstall of a locked server: code %d, %q", code, errOut)
	}
	if out, _, code := e.run("", false, "config", "state", "locks"); code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "fake/1 reinstall bob@desk") {
		t.Errorf("locks: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "state", "unlock", "fake/1"); code != 0 || !strings.Contains(out, "Unlocked fake/1.") {
		t.Errorf("unlock: code %d\n%s", code, out)
	}
	if _, _, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one"); code != 0 {
		t.Errorf("reinstall after unlock: code %d", code)
	}

	// someone else changes a file in the backend: pull brings it; a file
	// changed on both sides is a conflict until push --force
	os.WriteFile(filepath.Join(remote, "profiles", "web.yaml"), []byte("os: ubuntu-24.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	// every command pulls first, so the explicit pull finds nothing left
	if out, _, code := e.run("", false, "config", "state", "pull"); code != 0 || !strings.Contains(out, "Pulled 0 files from file://"+remote) {
		t.Errorf("pull: code %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(mirror, "profiles", "web.yaml")); !strings.Contains(string(data), "ubuntu-24.04") {
		t.Errorf("mirror after pull:\n%s", data)
	}
	os.WriteFile(filepath.Join(mirror, "profiles", "web.yaml"), []byte("os: ubuntu-22.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	os.WriteFile(filepath.Join(remote, "profiles", "web.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	if out, errOut, code := e.run("", false, "config", "state", "push"); code != 1 || !strings.Contains(out, "profiles/web.yaml: conflict") || !strings.Contains(errOut, "1 files in conflict") {
		t.Errorf("push with a conflict: code %d\n%s\n%s", code, out, errOut)
	}
	if out, _, code := e.run("", false, "config", "state", "push", "--force"); code != 0 || !strings.Contains(out, "profiles/web.yaml: forced") {
		t.Errorf("push --force: code %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(remote, "profiles", "web.yaml")); !strings.Contains(string(data), "ubuntu-22.04") {
		t.Errorf("backend after push --force:\n%s", data)
	}

	// the environment wins over the pointer; test, set and clear
	other := filepath.Join(t.TempDir(), "other")
	t.Setenv("UMP_STATE", "file://"+other)
	if out, _, code := e.run("", false, "config", "state", "view"); code != 0 || !strings.Contains(out, "Backend:   file://"+other+" (UMP_STATE in the environment)") || !strings.Contains(out, "Servers:   0 with settings") {
		t.Errorf("view from the environment:\n%s", out)
	}
	if out, _, code := e.run("", false, "config", "state", "test"); code != 0 || !strings.Contains(out, "file://"+other+" answers (0 files there)") {
		t.Errorf("test: code %d\n%s", code, out)
	}
	t.Setenv("UMP_STATE", "")
	if out, _, code := e.run("", false, "config", "state", "set", "--url", "file://"+other); code != 0 || !strings.Contains(out, "The state backend is file://"+other+" now") {
		t.Errorf("set: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "state", "clear"); code != 0 || !strings.Contains(out, "No backend configured; the state is in "+local) {
		t.Errorf("clear: code %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(local, "backend.yaml")); err == nil {
		t.Error("backend.yaml still there after clear")
	}
	if out, _, code := e.run("", false, "config", "state", "view"); code != 0 || !strings.Contains(out, "Backend:   none, the state is in "+local) {
		t.Errorf("view after clear:\n%s", out)
	}
	// a secret given as a value is kept in a file and referenced
	if _, _, code := e.run("", false, "config", "state", "set", "--url", "file://"+other, "--token", "t0ken", "--no-test"); code != 0 {
		t.Fatal("set with a token failed")
	}
	if data, _ := os.ReadFile(filepath.Join(local, "backend.yaml")); strings.Contains(string(data), "t0ken") || !strings.Contains(string(data), "token:") {
		t.Errorf("backend.yaml with a secret:\n%s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(local, "backend.token")); string(data) != "t0ken\n" {
		t.Errorf("secret file: %q", data)
	}
	if _, errOut, code := e.run("", false, "config", "state", "set"); code != 2 || !strings.Contains(errOut, "--url is required (ump config state setup asks for it on a terminal)") {
		t.Errorf("set without --url: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "state", "setup"); code != 3 || !strings.Contains(errOut, "needs a terminal") {
		t.Errorf("setup without a terminal: code %d, %q", code, errOut)
	}
}

func TestStateSetup(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	local := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")
	os.MkdirAll(filepath.Join(local, "profiles"), 0o700)
	os.WriteFile(filepath.Join(local, "profiles", "db.yaml"), []byte("os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump]\n"), 0o600)
	if _, _, code := e.run("", false, "server", "set", "fake/1", "--attr", "owner=alice"); code != 0 {
		t.Fatal("server set failed")
	}
	// not a clean start: the wizard says what the state holds and offers
	// the migration to an empty backend
	remote := filepath.Join(t.TempDir(), "fleet")
	out, errOut, code := e.run("1\nfile://"+remote+"\ny\n", true, "config", "state", "setup")
	if code != 0 {
		t.Fatalf("setup: code %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"No state backend is configured; the state is in " + local + " on this machine and holds 2 accounts, 1 profile files, 1 servers with settings.", "Where should the state live?", "file://" + remote + " answers (0 files there).", "Migrate this machine's state (2 accounts, 1 profile files, 1 servers with settings) there? [Y/n]:", "Copied ", "The state backend is file://" + remote + " now"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("setup lacks %q:\n%s%s", want, out, errOut)
		}
	}
	for _, f := range []string{"profiles/db.yaml", "state.yaml"} { // the test's config.yaml lives outside the home (--config)
		if _, err := os.Stat(filepath.Join(remote, f)); err != nil {
			t.Errorf("%s did not reach the backend: %v", f, err)
		}
	}
	if out, _, code := e.run("", false, "config", "state", "view"); code != 0 || !strings.Contains(out, "Backend:   file://"+remote+" (backend.yaml)") {
		t.Errorf("view after setup:\n%s", out)
	}
	// the same backend again: nothing to do
	if out, errOut, code := e.run("1\nfile://"+remote+"\n", true, "config", "state", "setup"); code != 0 || !strings.Contains(out+errOut, "it is the state backend already") {
		t.Errorf("same backend: code %d\n%s%s", code, out, errOut)
	}
	// another backend that holds state: used as it is by default
	other := filepath.Join(t.TempDir(), "other")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "config.yaml"), []byte("accounts: {}\n"), 0o600)
	out, errOut, code = e.run("1\nfile://"+other+"\n\n", true, "config", "state", "setup")
	if code != 0 || !strings.Contains(out+errOut, "It holds state already, and so does this machine (") || !strings.Contains(out+errOut, "1) use the state in the backend") || strings.Contains(out+errOut, "Copied ") || !strings.Contains(out+errOut, "The state backend is file://"+other+" now") {
		t.Errorf("backend with state: code %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(other, "profiles", "db.yaml")); err == nil {
		t.Error("the state was copied although the backend's was chosen")
	}
	// none: back to the local directory
	if out, errOut, code := e.run("8\n", true, "config", "state", "setup"); code != 0 || !strings.Contains(out+errOut, "No backend; the state stays in "+local) {
		t.Errorf("none: code %d\n%s%s", code, out, errOut)
	}
}

func TestWizardHomeQuestion(t *testing.T) {
	e := newTestEnv(t)
	// a fresh system: the wizard asks about the home first; "n" runs the
	// backend wizard, where "none" keeps the local directory
	out, _, code := e.run("n\n8\nfake\nacc\ncarol\ns3cret\n\nn\n", true, "config", "isp", "add")
	if code != 0 || !strings.Contains(out, "No state backend is configured: everything ump keeps, this account's secrets included, stays in") || !strings.Contains(out, "Keep it on this machine? [Y/n]:") || !strings.Contains(out, "Where should the state live?") || !strings.Contains(out, "No backend; the state stays in") || !strings.Contains(out, "Saved account acc (fake)") {
		t.Errorf("fresh system: code %d\n%s", code, out)
	}
	if strings.Contains(out, "3) manual") || strings.Contains(out, "manual         servers added by hand") {
		t.Errorf("manual offered as a provider type:\n%s", out)
	}
	// with a file there, no question
	if out, _, code := e.run("fake\nacc2\ncarol\ns3cret\n\nn\n", true, "config", "isp", "add"); code != 0 || strings.Contains(out, "Keep it on this machine?") || !strings.Contains(out, "Home:      "+filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")) == false && !strings.Contains(out, "Home: ") {
		t.Errorf("second run: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "config", "isp", "add", "manual", "x"); code != 2 || !strings.Contains(errOut, "manual servers are not an account to add") {
		t.Errorf("add manual: code %d, %q", code, errOut)
	}

	// behind a backend the secrets go into the home without a question
	remote := filepath.Join(t.TempDir(), "fleet")
	t.Setenv("UMP_STATE", "file://"+remote)
	out, _, code = e.run("fake\nacc3\ncarol\ns3cret\n\nn\n", true, "config", "isp", "add")
	if code != 0 || strings.Contains(out, "Where should this account's secrets be kept?") || !strings.Contains(out, "The secrets are kept as files in the state (file://"+remote+")") || !strings.Contains(out, "Home: file://"+remote) || !strings.Contains(out, "Saved account acc3 (fake)") {
		t.Errorf("behind a backend: code %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(remote, "secrets", "acc3.password")); err != nil {
		t.Errorf("the secret did not reach the backend: %v", err)
	}
}

func TestWaitLimit(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	var errOut bytes.Buffer
	a := &app{stderr: &errOut, configFlag: e.cfgPath}
	if d := a.waitLimit(15 * time.Minute); d != 15*time.Minute {
		t.Errorf("default: %s", d)
	}
	e.write(testConfig + "wait: 40m\n")
	if d := a.waitLimit(15 * time.Minute); d != 40*time.Minute {
		t.Errorf("from the configuration: %s", d)
	}
	t.Setenv("UMP_WAIT", "1h")
	if d := a.waitLimit(15 * time.Minute); d != time.Hour {
		t.Errorf("UMP_WAIT wins: %s", d)
	}
	t.Setenv("UMP_WAIT", "soon")
	if d := a.waitLimit(15 * time.Minute); d != 15*time.Minute || !strings.Contains(errOut.String(), `UMP_WAIT is "soon", not a duration such as 20m; waiting up to 15m`) {
		t.Errorf("a bad value: %s, %q", d, errOut.String())
	}
	a.waitLimit(15 * time.Minute)
	if strings.Count(errOut.String(), "not a duration") != 1 {
		t.Errorf("said more than once: %q", errOut.String())
	}
	for d, want := range map[time.Duration]string{15 * time.Minute: "15m", 10 * time.Minute: "10m", 90 * time.Minute: "1h30m", time.Hour: "1h", 45 * time.Second: "45s", 30 * time.Millisecond: "30ms"} {
		if got := fmtWait(d); got != want {
			t.Errorf("fmtWait(%s) = %q, want %q", d, got, want)
		}
	}
	// the ticker: a dot per ten seconds, the minutes at each full minute
	var out bytes.Buffer
	b := &app{stderr: &out}
	start := time.Now().Add(-65 * time.Second)
	tick := b.waitTicker(start)
	tick()
	if out.String() != "1m" {
		t.Errorf("at 65s: %q", out.String())
	}
	tick()
	if out.String() != "1m" {
		t.Errorf("no second mark within ten seconds: %q", out.String())
	}
	out.Reset()
	b.waitTicker(time.Now().Add(-25 * time.Second))()
	if out.String() != "." {
		t.Errorf("at 25s: %q", out.String())
	}
}

func TestRescueWithoutProviderRescueNeedsForce(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	// the running system lets UMP in, but nothing stands behind a failed
	// jump: refused without --force, before anything happens
	e.directRoot, e.versionAfter = true, 2
	_, errOut, code := e.run("", true, "server", "rescue", "web-01", "--yes")
	if code != 3 || !strings.Contains(errOut, "has no provider rescue system to fall back on") || !strings.Contains(errOut, "ump server rescue --force takes that risk") {
		t.Errorf("without --force: code %d, %q", code, errOut)
	}
	if entries := e.entryCommands(); len(entries) != 0 || len(fakePower.actions) != 0 {
		t.Errorf("something happened: entries %v, actions %v", entries, fakePower.actions)
	}
	// with --force the jump is made
	_, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--force")
	if code != 0 || !strings.Contains(errOut, "Waiting for the UMP rescue system on 203.0.113.10") {
		t.Errorf("with --force: code %d, %q", code, errOut)
	}
	// a server that runs the UMP rescue system already is connected to
	e.directRoot, e.inImage = false, true
	if _, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes"); code != 0 || !strings.Contains(errOut, "already runs the UMP rescue system") {
		t.Errorf("running already: code %d, %q", code, errOut)
	}
	// reinstall needs no --force: its confirmation covers the wipe
	e.inImage, e.directRoot, e.versionPolls = false, true, 0
	if _, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes"); code != 0 || !strings.Contains(errOut, "web-01 is up:") {
		t.Errorf("reinstall: code %d, %q", code, errOut)
	}
}

func TestDirectEntrySetsUpUMPKey(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	resetFakePower()
	// the running system lets the user's key in, not UMP's: UMP's key is
	// added to root's authorized_keys over that connection, once
	e.directRoot, e.versionAfter, e.umpKeyDenied = true, 2, true
	_, errOut, code := e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("code %d, %q", code, errOut)
	}
	if !strings.Contains(errOut, "UMP's key did not open root@web-01, one of yours did; added UMP's key to that account's authorized_keys") || strings.Join(e.keyAdds, ",") != "root" {
		t.Errorf("key setup: %v, %q", e.keyAdds, errOut)
	}
	// UMP's key opens it already: nothing is added
	e.keyAdds, e.kexeced, e.versionPolls = nil, false, 0
	_, errOut, code = e.run("", true, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 || len(e.keyAdds) != 0 || strings.Contains(errOut, "added UMP's key") {
		t.Errorf("UMP's key works already: code %d, %v, %q", code, e.keyAdds, errOut)
	}
}
