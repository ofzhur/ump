package pxe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
)

// flaky is a local backend that can stop answering; onRead, when set,
// runs before each read (another writer that comes in between).
type flaky struct {
	*backend.Local
	down   atomic.Bool
	onRead atomic.Pointer[func(p string)]
}

var errDown = errors.New("connection refused")

func (f *flaky) Read(ctx context.Context, p string) ([]byte, string, error) {
	if f.down.Load() {
		return nil, "", errDown
	}
	if fn := f.onRead.Load(); fn != nil {
		(*fn)(p)
	}
	return f.Local.Read(ctx, p)
}

func (f *flaky) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	if f.down.Load() {
		return "", errDown
	}
	return f.Local.Write(ctx, p, data, ifVersion)
}

func (f *flaky) Delete(ctx context.Context, p string) error {
	if f.down.Load() {
		return errDown
	}
	return f.Local.Delete(ctx, p)
}

func (f *flaky) List(ctx context.Context) ([]backend.Entry, error) {
	if f.down.Load() {
		return nil, errDown
	}
	return f.Local.List(ctx)
}

// lines collects a server's log.
type lines struct {
	mu sync.Mutex
	l  []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.l = append(l.l, fmt.Sprintf(format, args...))
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.l, "\n")
}

const servedYAML = `unknown: local
machines:
  web-07:
    mac: 0c:c4:7a:aa:bb:01
  lab-1:
    mac: 52:54:00:12:34:56
    mode: rescue
    console: ttyS1,115200n8
`

// fixture is a server of site default over a local backend, with a
// storage that has a rescue image for amd64.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	files *flaky
	store Store
	st    *storage
	image string
	now   time.Time
	log   *lines
	srv   *Server
	http  *httptest.Server
}

func newFixture(t *testing.T, yaml string) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), files: &flaky{Local: &backend.Local{Dir: t.TempDir()}},
		st: &storage{files: map[string][]byte{}, hits: map[string]int{}}, now: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), log: &lines{}}
	f.store = Store{Files: f.files}
	img := httptest.NewServer(f.st)
	t.Cleanup(img.Close)
	f.image = img.URL + "/rescue"
	f.st.publish(ArchAMD64, "2026.11.01", []byte("bzImage"), []byte("initrd one"), "")
	f.render(yaml)
	f.srv = f.open(t.TempDir(), t.TempDir())
	f.http = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.http.Close)
	return f
}

// open is a server of the fixture's backend with its own data and
// cache, loaded and with the rescue image fetched.
func (f *fixture) open(data, cache string) *Server {
	f.t.Helper()
	s := &Server{Site: DefaultSite, Name: "pxe1", Version: "v0.8.0", Store: f.store, Cache: &Cache{Dir: cache}, Data: data,
		Logf: f.log.logf, Now: func() time.Time { return f.now }}
	if err := s.Open(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	// what it writes in the background ends before its directories go
	f.t.Cleanup(s.bg.Wait)
	s.Load(f.ctx)
	s.Fetch(f.ctx)
	return s
}

func (f *fixture) render(yaml string) *SiteDoc {
	f.t.Helper()
	c := parse(f.t, yaml)
	if err := c.Validate(); err != nil {
		f.t.Fatal(err)
	}
	d, err := Render(c, DefaultSite, f.image, testKeys)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.PutSite(f.ctx, d); err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) load() {
	f.t.Helper()
	if err := f.srv.Load(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// get is a request to the server: its status and body; for an initrd
// once the server noted it and wrote what it writes to the backend.
func (f *fixture) get(method, path string) (int, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.http.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if id, ok := strings.CutSuffix(strings.TrimPrefix(path, "/b/"), "/initrd"); ok && method == "GET" && resp.StatusCode == 200 {
		// the last bytes may reach the client before the server notes them
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			f.srv.mu.Lock()
			b := f.srv.boots[id]
			served := b != nil && !b.Served.IsZero()
			f.srv.mu.Unlock()
			if served {
				break
			}
		}
		f.srv.bg.Wait()
	}
	return resp.StatusCode, string(body)
}

// boot asks for a machine's script as boot.ipxe does, with query
// beyond the MAC.
func (f *fixture) boot(mac, query string) string {
	f.t.Helper()
	code, body := f.get("GET", "/ipxe?mac="+mac+"&"+query)
	if code != 200 {
		f.t.Fatalf("script of %s: HTTP %d %s", mac, code, body)
	}
	return body
}

var bootRe = regexp.MustCompile(`/b/([A-Za-z0-9_-]{22})/vmlinuz`)

// bootID is the boot of a kernel script.
func bootID(t *testing.T, script string) string {
	t.Helper()
	m := bootRe.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no kernel in the script:\n%s", script)
	}
	return m[1]
}

// bootConfig is what the boot config of an initrd holds, by name.
func bootConfig(t *testing.T, initrd string) map[string]string {
	t.Helper()
	files, err := ReadCpio([]byte(initrd))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Name] = string(f.Data)
	}
	return out
}

func (f *fixture) status() *Status {
	f.t.Helper()
	if err := f.srv.Flush(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	var st Status
	if v, err := f.store.read(f.ctx, StatusPath(DefaultSite, "pxe1"), &st); err != nil || v == "" {
		f.t.Fatalf("status: %q %v", v, err)
	}
	return &st
}

func (f *fixture) hello(token, boot string, extra string) (int, string) {
	f.t.Helper()
	body := `{"boot": "` + boot + `", "boot_mac": "52:54:00:12:34:56", "firmware": "uefi", "rescue": "2026.11.01",
		"interfaces": [{"name": "eth0", "mac": "52:54:00:12:34:56", "up": true, "addresses": ["192.0.2.17/24"]}],
		"host_keys": ["ssh-ed25519 AAAAhost"]` + extra + `}`
	req, _ := http.NewRequest("POST", f.http.URL+"/api/v1/hello", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const efi = "platform=efi&buildarch=x86_64&ip=192.0.2.17&mem=4096&lm="

func TestServerScripts(t *testing.T) {
	f := newFixture(t, servedYAML)
	code, body := f.get("GET", "/boot.ipxe")
	if code != 200 || !strings.Contains(body, "chain --autofree "+f.http.URL+"/ipxe?mac=${netX/mac}&ip=${netX/ip}&") {
		t.Errorf("boot.ipxe: %d\n%s", code, body)
	}
	if code, _ := f.get("GET", "/ipxe"); code != 400 {
		t.Errorf("no MAC: %d", code)
	}
	if code, _ := f.get("POST", "/ipxe?mac=52:54:00:12:34:56"); code != 405 {
		t.Errorf("POST: %d", code)
	}
	if got := f.boot("0C-C4-7A-AA-BB-01", efi); got != "#!ipxe\necho ump: web-07: local\nexit\n" {
		t.Errorf("local:\n%s", got)
	}
	if got := f.boot("52:54:00:99:99:99", efi); got != "#!ipxe\necho ump: 52:54:00:99:99:99 is no machine of site default\nexit\n" {
		t.Errorf("unknown:\n%s", got)
	}
	st := f.status()
	if u := st.Unknown["52:54:00:99:99:99"]; u == nil || u.Address != "192.0.2.17" || u.Arch != ArchAMD64 {
		t.Errorf("unknown in the status: %+v", st.Unknown)
	}
	if s := st.Machines["0c:c4:7a:aa:bb:01"]; s == nil || s.Name != "web-07" || s.Answer != ModeLocal || s.Firmware != FirmwareUEFI || s.At != f.now {
		t.Errorf("web-07 in the status: %+v", s)
	}
	if st.SiteHash == "" || st.Version != "v0.8.0" || st.Rescue[ArchAMD64] != "2026.11.01" || st.URL != f.http.URL || len(st.Warnings) != 0 {
		t.Errorf("status: %+v", st)
	}
	for _, want := range []string{
		"site.json " + st.SiteHash + ": 2 machines",
		"rescue image 2026.11.01 for amd64: verified",
		"0c:c4:7a:aa:bb:01 (amd64 uefi, 192.0.2.17): web-07: local",
		"52:54:00:99:99:99 (amd64 uefi, 192.0.2.17): no machine of pxe.yaml, the disk",
	} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}

	// the disk five times in a row: it does not boot
	for range 4 {
		f.now = f.now.Add(time.Minute)
		f.boot("0c:c4:7a:aa:bb:01", efi)
	}
	if s := f.status().Machines["0c:c4:7a:aa:bb:01"]; len(s.Signals) != 1 || s.Signals[0] != "its disk does not boot: 5 network boots within 15 minutes" {
		t.Errorf("signals: %v", s.Signals)
	}
	// once the disk boots, the machine asks no more, and the window passes
	f.now = f.now.Add(15 * time.Minute)
	if s := f.status().Machines["0c:c4:7a:aa:bb:01"]; len(s.Signals) != 0 {
		t.Errorf("signals after the window: %v", s.Signals)
	}
	f.now = f.now.Add(-15 * time.Minute)

	// what stands in the way of the rescue system turns it into the disk
	for _, c := range []struct{ query, reason string }{
		{"platform=efi&buildarch=x86_64&mem=256", "256 MB of memory, and the rescue image needs 412 MB"},
		{"platform=pcbios&buildarch=i386&lm=0", "the CPU has no 64-bit mode (long mode)"},
		{"platform=efi&buildarch=i386", "ump boots amd64 and arm64 machines, not i386 on efi"},
		{"platform=efi&buildarch=arm64", "no rescue image for arm64 yet: it is being fetched"},
	} {
		if got := f.boot("52:54:00:12:34:56", c.query); got != "#!ipxe\necho ump: lab-1: local: "+c.reason+"\nsleep 5\nexit\n" {
			t.Errorf("%s:\n%s", c.query, got)
		}
		if s := f.status().Machines["52:54:00:12:34:56"]; s.Answer != ModeLocal || s.Reason != c.reason {
			t.Errorf("%s in the status: %+v", c.query, s)
		}
	}
	// arm64 is fetched once a machine boots as one
	select {
	case <-f.srv.fetch:
	default:
		t.Error("no fetch of arm64")
	}
	f.st.publish(ArchARM64, "2026.11.01", gzipped([]byte("arm64 Image")), []byte("initrd arm64"), "")
	if err := f.srv.Fetch(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.boot("52:54:00:12:34:56", "platform=efi&buildarch=arm64")
	id := bootID(t, got)
	if !strings.Contains(got, "/b/"+id+"/vmlinuz initrd=initrd console=ttyS1,115200n8 panic=30 || goto retry") {
		t.Errorf("arm64:\n%s", got)
	}
	if _, kernel := f.get("GET", "/b/"+id+"/vmlinuz"); kernel != "arm64 Image" {
		t.Errorf("arm64 kernel %q", kernel)
	}

	// site.json gone: nothing is served
	f.store.DeleteSite(f.ctx, DefaultSite)
	f.load()
	if got := f.boot("52:54:00:12:34:56", efi); got != "#!ipxe\necho ump: site default has nothing to serve; booting from disk\nsleep 5\nexit\n" {
		t.Errorf("no site.json:\n%s", got)
	}
	if st := f.status(); st.SiteHash != "" || len(st.Warnings) != 1 || st.Warnings[0] != "no site.json for site default: ump config pxe apply renders it" {
		t.Errorf("status without site.json: %+v", st)
	}
	// one of a newer ump is refused
	d := f.render(servedYAML)
	newer := *d
	newer.Format = SiteFormat + 1
	newer.Hash = newer.hash()
	_, v, _ := f.files.Read(f.ctx, SitePath(DefaultSite))
	if _, err := f.files.Write(f.ctx, SitePath(DefaultSite), newer.JSON(), v); err != nil {
		t.Fatal(err)
	}
	f.load()
	if !strings.Contains(f.boot("52:54:00:12:34:56", efi), "has nothing to serve") || !strings.Contains(strings.Join(f.status().Warnings, "\n"), "newer than this ump's") {
		t.Errorf("a newer site.json is served, or not said:\n%s", f.log)
	}
}

func TestServerRescue(t *testing.T) {
	f := newFixture(t, servedYAML)
	script := f.boot("52:54:00:12:34:56", efi)
	id := bootID(t, script)
	if want := "kernel " + f.http.URL + "/b/" + id + "/vmlinuz initrd=initrd console=ttyS1,115200n8 console=tty0 panic=30 || goto retry\n"; !strings.Contains(script, want) || !strings.HasPrefix(script, "#!ipxe\necho ump: lab-1: rescue\n") {
		t.Errorf("the script:\n%s", script)
	}
	if _, kernel := f.get("GET", "/b/"+id+"/vmlinuz"); kernel != "bzImage" {
		t.Errorf("kernel %q", kernel)
	}
	req, _ := http.NewRequest("HEAD", f.http.URL+"/b/"+id+"/initrd", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, initrd := f.get("GET", "/b/"+id+"/initrd")
	if !strings.HasSuffix(initrd, "initrd one") || (len(initrd)-len("initrd one"))%512 != 0 || resp.ContentLength != int64(len(initrd)) {
		t.Fatalf("initrd: %d bytes, HEAD says %d", len(initrd), resp.ContentLength)
	}
	cfg := bootConfig(t, initrd)
	want := fmt.Sprintf("ump.hostname=lab-1 ump.boot-mac=52:54:00:12:34:56 ump.dns=dhcp ump.time=%d ump.pxe=%s ump.pxe.boot=%s\n", f.now.Unix(), f.http.URL, id)
	if cfg["ump/cmdline"] != want || cfg["ump/authorized_keys"] != "ssh-ed25519 AAAAump ump\n" || len(cfg["ump/token"]) != 23 {
		t.Errorf("boot config: %q", cfg)
	}
	token := strings.TrimSpace(cfg["ump/token"])
	for _, p := range []string{"/b/" + id + "/other", "/b/nope/initrd"} {
		if code, _ := f.get("GET", p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}

	// the hello: its token, its boot
	for _, c := range []struct {
		token, boot, extra string
		code               int
	}{
		{"", id, "", 401},
		{"wrong", id, "", 401},
		{token, "another", "", 403},
		{token, id, `, "disks": "` + strings.Repeat("x", maxHello) + `"`, 413},
		{token, id, `, "memory_mb": "lots"`, 400},
	} {
		if code, body := f.hello(c.token, c.boot, c.extra); code != c.code {
			t.Errorf("hello %+v: %d %s", c, code, body)
		}
	}
	order := `, "efibootmgr": "BootCurrent: 0003\nBootOrder: 0001,0003\nBoot0001* ubuntu\tHD(1,GPT)\nBoot0003* UEFI PXEv4 (MAC:525400123456)\tPciRoot(0x0)\n"`
	if code, body := f.hello(token, id, order); code != 200 || body != "taken\n" {
		t.Fatalf("hello: %d %s", code, body)
	}
	// taken again, as the rescue system sends it again without an answer
	if code, _ := f.hello(token, id, order); code != 200 {
		t.Errorf("hello again: %d", code)
	}
	s := f.status().Machines["52:54:00:12:34:56"]
	if s == nil || s.Hello == nil || s.Hello.Boot != id || s.Hello.Arm != "" || s.Hello.Address() != "192.0.2.17" || s.Hello.At != f.now || s.Boot != id {
		t.Fatalf("the hello in the status: %+v", s)
	}
	if len(s.Signals) != 1 || s.Signals[0] != "the boot order starts with Boot0001 (ubuntu), not with Boot0003, the network boot it came from" {
		t.Errorf("signals: %q", s.Signals)
	}
	for _, want := range []string{"52:54:00:12:34:56 (amd64 uefi, 192.0.2.17): lab-1: rescue", "lab-1: rescue 2026.11.01 sent to 127.0.0.1 (", "lab-1: rescue system up at 192.0.2.17"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}
	// a restarted server takes the hello of a boot of the one before
	again := f.open(f.srv.Data, f.srv.Cache.Dir)
	f.http.Config.Handler = again.Handler()
	if code, body := f.hello(token, id, ""); code != 200 {
		t.Errorf("hello after a restart: %d %s", code, body)
	}
	if code, _ := f.get("GET", "/b/"+id+"/initrd"); code != 404 {
		t.Errorf("a boot of the server before is served: %d", code)
	}
	// the token's time is up
	f.now = f.now.Add(2 * time.Hour)
	if code, _ := f.hello(token, id, ""); code != 401 {
		t.Errorf("an old token: %d", code)
	}
}

func TestServerRescueCap(t *testing.T) {
	f := newFixture(t, servedYAML)
	for range rescueCap {
		id := bootID(t, f.boot("52:54:00:12:34:56", efi))
		f.get("GET", "/b/"+id+"/initrd")
		f.now = f.now.Add(2 * time.Minute)
	}
	got := f.boot("52:54:00:12:34:56", efi)
	if !strings.Contains(got, "echo ump: lab-1: local: the rescue system does not come up: 3 boots without a hello within 30 minutes") {
		t.Errorf("the cap:\n%s", got)
	}
	if s := f.status().Machines["52:54:00:12:34:56"]; len(s.Signals) != 1 || !strings.HasPrefix(s.Signals[0], "the rescue system does not come up") {
		t.Errorf("signals: %v", s.Signals)
	}
	// half an hour later it gets another chance
	f.now = f.now.Add(30 * time.Minute)
	bootID(t, f.boot("52:54:00:12:34:56", efi))
}

func TestServerArmedRescue(t *testing.T) {
	f := newFixture(t, servedYAML)
	arm := &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: ModeRescue, By: "oleg@laptop", Created: f.now, Until: f.now.Add(15 * time.Minute), Keys: []string{"ssh-ed25519 AAAAoleg oleg", "ssh-ed25519 AAAAump ump"}}
	if _, err := f.store.PutArm(f.ctx, DefaultSite, arm, ""); err != nil {
		t.Fatal(err)
	}
	f.load()
	if st := f.status(); st.Arms[arm.ID] == nil || !st.Arms[arm.ID].Ready {
		t.Errorf("the arm is not ready: %+v", st.Arms)
	}
	id := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	// the kernel alone consumes nothing, nor does a second script
	f.get("GET", "/b/"+id+"/vmlinuz")
	id2 := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	_, initrd := f.get("GET", "/b/"+id2+"/initrd")
	if cfg := bootConfig(t, initrd); cfg["ump/authorized_keys"] != "ssh-ed25519 AAAAump ump\nssh-ed25519 AAAAoleg oleg\n" || !strings.Contains(cfg["ump/cmdline"], "ump.hostname=web-07 ") {
		t.Errorf("boot config: %q", cfg)
	}
	o, _, err := f.store.Outcome(f.ctx, DefaultSite, "web-07")
	if err != nil || o == nil || o.Arm != arm.ID || o.Phase != PhaseBooted || o.Daemon != "pxe1" || o.Boot != id2 || o.At != f.now {
		t.Fatalf("outcome: %+v %v", o, err)
	}
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); got != "#!ipxe\necho ump: web-07: local\nexit\n" {
		t.Errorf("after the boot:\n%s", got)
	}
	if st := f.status(); len(st.Arms) != 0 {
		t.Errorf("a consumed arm in the status: %+v", st.Arms)
	}
	for _, want := range []string{"web-07: rescue armed (a7f3c2e1, by oleg@laptop, until ", "0c:c4:7a:aa:bb:01 (amd64 uefi, 192.0.2.17): web-07: rescue (arm a7f3c2e1)", "web-07: the arm a7f3c2e1 is consumed"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}

	// the hello of the boot carries the arm, for the CLI that waits
	token := strings.TrimSpace(bootConfig(t, initrd)["ump/token"])
	body := `{"boot": "` + id2 + `", "boot_mac": "0c:c4:7a:aa:bb:01", "interfaces": [{"mac": "0c:c4:7a:aa:bb:01", "addresses": ["192.0.2.30/24"]}]}`
	req, _ := http.NewRequest("POST", f.http.URL+"/api/v1/hello", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("hello: %v %v", resp, err)
	}
	if h := f.status().Machines["0c:c4:7a:aa:bb:01"].Hello; h == nil || h.Arm != arm.ID || h.Address() != "192.0.2.30" {
		t.Errorf("hello: %+v", h)
	}
	// the other boot that got the arm, its initrd read to the end later,
	// takes no rescue session: its hello is refused, the status keeps
	// the arm's
	_, other := f.get("GET", "/b/"+id+"/initrd")
	otherToken := strings.TrimSpace(bootConfig(t, other)["ump/token"])
	body = `{"boot": "` + id + `", "boot_mac": "0c:c4:7a:aa:bb:01", "interfaces": [{"mac": "0c:c4:7a:aa:bb:01", "addresses": ["192.0.2.66/24"]}]}`
	req, _ = http.NewRequest("POST", f.http.URL+"/api/v1/hello", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+otherToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("the hello of the other boot: %v %v", resp, err)
	}
	if h := f.status().Machines["0c:c4:7a:aa:bb:01"].Hello; h == nil || h.Boot != id2 || h.Address() != "192.0.2.30" {
		t.Errorf("hello after the other boot's: %+v", h)
	}
	if !strings.Contains(f.log.String(), "web-07: a hello of boot "+id+" refused: another boot consumed its arm a7f3c2e1") {
		t.Errorf("the log:\n%s", f.log)
	}

	// an expired arm boots nothing, and an installation is not served
	f.store.Forget(f.ctx, DefaultSite, "web-07")
	arm2 := &Arm{ID: "b0b0b0b0cccccccc", Machine: "web-07", Mode: ModeRescue, Created: f.now, Until: f.now.Add(time.Minute)}
	f.store.PutArm(f.ctx, DefaultSite, arm2, "")
	f.load()
	f.now = f.now.Add(time.Minute)
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("an expired arm:\n%s", got)
	}
	f.store.Disarm(f.ctx, DefaultSite, "web-07")
	f.store.PutArm(f.ctx, DefaultSite, &Arm{ID: "c0c0c0c0dddddddd", Machine: "web-07", Mode: ModeInstall, Created: f.now}, "")
	f.load()
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "local: the arm holds no specification") {
		t.Errorf("an armed installation without a specification:\n%s", got)
	}
	if as := f.status().Arms["c0c0c0c0dddddddd"]; as == nil || as.Ready || as.Cannot != "the arm holds no specification" {
		t.Errorf("an armed installation without a specification in the status: %+v", as)
	}
	f.store.Disarm(f.ctx, DefaultSite, "web-07")
	f.load()
	for _, want := range []string{"web-07: install armed (c0c0c0c0, by someone, no expiry)", "web-07: the arm c0c0c0c0 is gone"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}
}

// TestServerTwoConsume: two servers of a site that both boot the armed
// rescue of a machine both consume the arm, and the backend has one
// outcome of it: the first server's; the second takes it as written.
func TestServerTwoConsume(t *testing.T) {
	f := newFixture(t, servedYAML)
	arm := &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: ModeRescue, Created: f.now, Until: f.now.Add(time.Hour)}
	if _, err := f.store.PutArm(f.ctx, DefaultSite, arm, ""); err != nil {
		t.Fatal(err)
	}
	f.load()
	one, oneHTTP := f.srv, f.http
	two := f.open(t.TempDir(), t.TempDir())
	two.Name = "pxe2"
	twoHTTP := httptest.NewServer(two.Handler())
	defer twoHTTP.Close()
	// both answer before either consumed it
	id1 := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	f.srv, f.http = two, twoHTTP
	id2 := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	f.srv, f.http = one, oneHTTP
	f.get("GET", "/b/"+id1+"/initrd")
	f.srv, f.http = two, twoHTTP
	f.get("GET", "/b/"+id2+"/initrd")
	f.srv, f.http = one, oneHTTP

	o, _, err := f.store.Outcome(f.ctx, DefaultSite, "web-07")
	if err != nil || o == nil || o.Arm != arm.ID || o.Phase != PhaseBooted || o.Daemon != "pxe1" || o.Boot != id1 {
		t.Errorf("outcome: %+v %v", o, err)
	}
	for _, s := range []*Server{one, two} {
		s.mu.Lock()
		c := s.consumed[arm.ID]
		s.mu.Unlock()
		if c == nil || !c.Written {
			t.Errorf("%s: the consumed arm %+v", s.Name, c)
		}
	}
	if strings.Contains(f.log.String(), "did not take it") {
		t.Errorf("the log:\n%s", f.log)
	}
}

func TestServerBackendDown(t *testing.T) {
	f := newFixture(t, servedYAML)
	arm := &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: ModeRescue, Created: f.now, Until: f.now.Add(15 * time.Minute)}
	f.store.PutArm(f.ctx, DefaultSite, arm, "")
	f.load()
	f.files.down.Store(true)
	if err := f.srv.Load(f.ctx); err == nil {
		t.Fatal("a backend that does not answer loads")
	}
	// what it read serves; the arm is consumed in the journal
	id := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	f.get("GET", "/b/"+id+"/initrd")
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("after the boot, without the backend:\n%s", got)
	}
	data, _ := os.ReadFile(filepath.Join(f.srv.Data, journalFile))
	var j journalDoc
	if err := json.Unmarshal(data, &j); err != nil || j.Consumed[arm.ID] == nil || j.Consumed[arm.ID].Written || len(j.Boots) != 1 {
		t.Fatalf("journal: %v\n%s", err, data)
	}
	// so does a restarted server
	again := f.open(f.srv.Data, f.srv.Cache.Dir)
	if !again.consumed.Done(arm.ID) {
		t.Error("the journal is lost at a restart")
	}
	for _, want := range []string{"the backend does not answer (connection refused); serving what it read before", "web-07: the arm a7f3c2e1 is consumed, and the backend did not take it (connection refused); the journal holds it until it does"} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}
	// back, it learns of it
	f.files.down.Store(false)
	f.load()
	f.srv.writeConsumed(f.ctx)
	if o, _, _ := f.store.Outcome(f.ctx, DefaultSite, "web-07"); o == nil || o.Arm != arm.ID || o.Phase != PhaseBooted {
		t.Errorf("outcome: %+v", o)
	}
	if !f.srv.consumed[arm.ID].Written || !strings.Contains(f.log.String(), "the backend answers again") {
		t.Errorf("not written: %+v\n%s", f.srv.consumed[arm.ID], f.log)
	}
}

// TestServerLostOutcome: the outcome of a rescue arm consumed here that
// the backend lost while the arm stands is put back, once.
func TestServerLostOutcome(t *testing.T) {
	f := newFixture(t, servedYAML)
	arm := &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: ModeRescue, Created: f.now, Until: f.now.Add(time.Hour)}
	f.store.PutArm(f.ctx, DefaultSite, arm, "")
	f.load()
	id := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	f.get("GET", "/b/"+id+"/initrd")
	f.load()
	f.srv.writeConsumed(f.ctx)
	if strings.Contains(f.log.String(), "lost") {
		t.Errorf("an outcome there is put back:\n%s", f.log)
	}
	f.files.Delete(f.ctx, OutcomePath(DefaultSite, "web-07"))
	f.load()
	f.srv.writeConsumed(f.ctx)
	if o, _, _ := f.store.Outcome(f.ctx, DefaultSite, "web-07"); o == nil || o.Arm != arm.ID || o.Phase != PhaseBooted || o.Boot != id {
		t.Errorf("outcome: %+v", o)
	}
	if !strings.Contains(f.log.String(), "web-07: the backend lost the outcome of the arm a7f3c2e1, which was consumed here: put back") {
		t.Errorf("the log:\n%s", f.log)
	}
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("after:\n%s", got)
	}
	// removed with its arm, the arm first: not put back, though the
	// server read the outcome gone before it read the arm gone
	f.files.Delete(f.ctx, OutcomePath(DefaultSite, "web-07"))
	f.load()
	f.store.Disarm(f.ctx, DefaultSite, "web-07")
	f.srv.writeConsumed(f.ctx)
	if o, _, _ := f.store.Outcome(f.ctx, DefaultSite, "web-07"); o != nil {
		t.Errorf("a settled outcome put back: %+v", o)
	}
}

func TestServerPinsVersion(t *testing.T) {
	f := newFixture(t, servedYAML)
	old := bootID(t, f.boot("52:54:00:12:34:56", efi))
	f.st.publish(ArchAMD64, "2026.11.02", []byte("bzImage 2"), []byte("initrd two"), "")
	if err := f.srv.Fetch(f.ctx); err != nil {
		t.Fatal(err)
	}
	cur := bootID(t, f.boot("52:54:00:12:34:56", efi))
	for id, want := range map[string]string{old: "initrd one", cur: "initrd two"} {
		if _, initrd := f.get("GET", "/b/"+id+"/initrd"); !strings.HasSuffix(initrd, want) {
			t.Errorf("boot %s: the initrd ends %q", id, initrd[len(initrd)-10:])
		}
	}
	if v, _ := f.srv.Cache.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.01,2026.11.02" {
		t.Errorf("versions while a boot keeps the old one: %v", v)
	}
	// once its boots are over, the old version goes
	f.now = f.now.Add(time.Hour)
	f.srv.prune()
	f.srv.Fetch(f.ctx)
	if v, _ := f.srv.Cache.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.02" {
		t.Errorf("versions: %v", v)
	}
}

// failing is a response writer that fails after n bytes.
type failing struct {
	header http.Header
	n      int
}

func (w *failing) Header() http.Header { return w.header }
func (w *failing) WriteHeader(int)     {}
func (w *failing) Write(b []byte) (int, error) {
	if len(b) > w.n {
		m := w.n
		w.n = 0
		return m, errors.New("broken pipe")
	}
	w.n -= len(b)
	return len(b), nil
}

func TestServerIncompleteInitrd(t *testing.T) {
	f := newFixture(t, servedYAML)
	f.store.PutArm(f.ctx, DefaultSite, &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: ModeRescue, Created: f.now, Until: f.now.Add(time.Hour)}, "")
	f.load()
	id := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	r := httptest.NewRequest("GET", "/b/"+id+"/initrd", nil)
	f.srv.Handler().ServeHTTP(&failing{header: http.Header{}, n: 600}, r)
	if o, _, _ := f.store.Outcome(f.ctx, DefaultSite, "web-07"); o != nil || f.srv.consumed.Done("a7f3c2e1aaaabbbb") {
		t.Errorf("an initrd sent in part consumed the arm: %+v", o)
	}
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: rescue\n") {
		t.Errorf("after a broken download:\n%s", got)
	}
	// its token, at the start of the initrd, takes no hello
	f.srv.mu.Lock()
	cpio := string(f.srv.boots[id].cpio)
	f.srv.mu.Unlock()
	if code, body := f.hello(strings.TrimSpace(bootConfig(t, cpio)["ump/token"]), id, ""); code != 403 || body != "the initrd of this boot was not sent completely\n" {
		t.Errorf("the hello of a boot whose initrd was sent in part: %d %s", code, body)
	}
}

func TestServerRun(t *testing.T) {
	oldEvery, oldBeat := statusEvery, heartbeat
	statusEvery, heartbeat = 10*time.Millisecond, 50*time.Millisecond
	defer func() { statusEvery, heartbeat = oldEvery, oldBeat }()
	f := newFixture(t, servedYAML)
	f.srv.Poll = 20 * time.Millisecond
	var notes atomic.Value
	f.srv.Notify = func(s string) { notes.Store(s) }
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { f.srv.Run(ctx); close(done) }()
	arm := &Arm{ID: "a7f3c2e1aaaabbbb", Machine: "lab-1", Mode: ModeRescue, Created: f.now, Until: f.now.Add(time.Hour)}
	f.store.PutArm(f.ctx, DefaultSite, arm, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var st Status
		f.store.read(f.ctx, StatusPath(DefaultSite, "pxe1"), &st)
		if as := st.Arms[arm.ID]; as != nil && as.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready for the arm in the status: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n, _ := notes.Load().(string); n != "STATUS=default: 2 machines, read 0s ago" {
		t.Errorf("notify: %q", n)
	}
}

func TestBootOrder(t *testing.T) {
	for _, c := range []struct{ firmware, efibootmgr, want string }{
		{"uefi", "BootCurrent: 0003\nBootOrder: 0003,0001\nBoot0001* ubuntu\n", ""},
		{"uefi", "BootCurrent: 0003\nBootOrder: 0001,0003\nBoot0001  Windows Boot Manager\tHD()\n", "the boot order starts with Boot0001 (Windows Boot Manager), not with Boot0003, the network boot it came from"},
		{"uefi", "BootOrder: 0001,0003\n", ""},
		{"bios", "BootCurrent: 0003\nBootOrder: 0001\n", ""},
		{"uefi", "BootCurrent: 0003\nBootOrder: 0002\n", "the boot order starts with Boot0002, not with Boot0003, the network boot it came from"},
	} {
		if got := BootOrder(&Hello{Firmware: c.firmware, EFIBootMgr: c.efibootmgr}); got != c.want {
			t.Errorf("%q: %q, want %q", c.efibootmgr, got, c.want)
		}
	}
}

// FuzzParseHello: no hello makes the parser or Address panic.
func FuzzParseHello(f *testing.F) {
	f.Add([]byte(`{"boot": "x", "boot_mac": "52:54:00:12:34:56", "interfaces": [{"mac": "52:54:00:12:34:56", "addresses": ["192.0.2.17/24", "2001:db8::1/64"]}]}`))
	f.Add([]byte(`{"interfaces": [{"addresses": ["nonsense"]}]} {}`))
	f.Add([]byte(`{"boot_mac": "ff:ff:ff:ff:ff:ff"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHello(bytes.NewReader(b))
		if err != nil {
			return
		}
		h.Address()
		BootOrder(h)
	})
}
