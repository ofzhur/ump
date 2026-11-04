package pxe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const installYAML = `machines:
  web-07:
    mac: 0c:c4:7a:aa:bb:01
    firmware: uefi
  lab-1:
    mac: 52:54:00:12:34:56
    mode: rescue
`

const testSpec = `{"hostname": "web-07", "os": "debian-13", "storage": "https://storage.example.net"}`

// armInstall arms an installation of web-07 without expiry.
func (f *fixture) armInstall(id string) *Arm {
	f.t.Helper()
	hash, err := SpecHash(json.RawMessage(testSpec))
	if err != nil {
		f.t.Fatal(err)
	}
	a := &Arm{ID: id, Machine: "web-07", Mode: ModeInstall, By: "oleg@laptop", Created: f.now, Keys: []string{"ssh-ed25519 AAAAoleg oleg"},
		Spec: json.RawMessage(testSpec), SpecHash: hash, Disks: &DiskRule{Rule: DisksBlank}}
	if _, err := f.store.PutArm(f.ctx, DefaultSite, a, ""); err != nil {
		f.t.Fatal(err)
	}
	f.load()
	return a
}

// do is a request of a rescue system: its status, body and headers.
func (f *fixture) do(method, path, token, body string) (int, string, http.Header) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String(), resp.Header
}

// installBoot boots web-07 as its armed installation and says its
// hello: the boot, its token and its boot config.
func (f *fixture) installBoot() (string, string, map[string]string) {
	f.t.Helper()
	script := f.boot("0c:c4:7a:aa:bb:01", efi)
	if !strings.HasPrefix(script, "#!ipxe\necho ump: web-07: install\n") {
		f.t.Fatalf("not an installation:\n%s", script)
	}
	id := bootID(f.t, script)
	_, initrd := f.get("GET", "/b/"+id+"/initrd")
	cfg := bootConfig(f.t, initrd)
	token := strings.TrimSpace(cfg["ump/token"])
	body := `{"boot": "` + id + `", "boot_mac": "0c:c4:7a:aa:bb:01", "firmware": "uefi",
		"interfaces": [{"mac": "0c:c4:7a:aa:bb:01", "addresses": ["192.0.2.17/24"]}], "host_keys": ["ssh-ed25519 AAAArescue"]}`
	if code, b, _ := f.do("POST", "/api/v1/hello", token, body); code != 200 {
		f.t.Fatalf("hello: %d %s", code, b)
	}
	return id, token, cfg
}

func (f *fixture) outcome() *Outcome {
	f.t.Helper()
	o, _, err := f.store.Outcome(f.ctx, DefaultSite, "web-07")
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

const wantSpec = `{"spec":{"hostname":"web-07","os":"debian-13","storage":"https://storage.example.net"},"firmware":"uefi","disks":{"rule":"blank"}}`

func TestInstall(t *testing.T) {
	f := newFixture(t, installYAML)
	arm := f.armInstall("a7f3c2e1aaaabbbb")
	if as := f.status().Arms[arm.ID]; as == nil || !as.Ready {
		t.Errorf("the arm is not ready: %+v", as)
	}
	// the boot: a token, the arm's keys, the mode
	id, token, cfg := f.installBoot()
	if !strings.Contains(cfg["ump/cmdline"], " ump.pxe.boot="+id+" ump.pxe.mode=install\n") || cfg["ump/authorized_keys"] != "ssh-ed25519 AAAAump ump\nssh-ed25519 AAAAoleg oleg\n" {
		t.Errorf("boot config: %q", cfg)
	}
	if code, _, _ := f.do("GET", "/api/v1/spec", "", ""); code != 401 {
		t.Errorf("spec without a token: %d", code)
	}

	// the claim: the outcome first, then the answer
	code, body, _ := f.do("GET", "/api/v1/spec", token, "")
	if code != 200 || body != wantSpec {
		t.Fatalf("spec: %d %s", code, body)
	}
	o := f.outcome()
	if o == nil || o.Arm != arm.ID || o.Phase != PhaseWriting || o.Boot != id || o.Daemon != "pxe1" || !o.Deadline.Equal(f.now.Add(DefaultInstallTimeout)) {
		t.Fatalf("outcome: %+v", o)
	}
	_, v1, _ := f.files.Read(f.ctx, OutcomePath(DefaultSite, "web-07"))
	// a lost answer: the same again, and nothing written
	if code, again, _ := f.do("GET", "/api/v1/spec", token, ""); code != 200 || again != body {
		t.Errorf("spec again: %d %s", code, again)
	}
	if _, v2, _ := f.files.Read(f.ctx, OutcomePath(DefaultSite, "web-07")); v2 != v1 {
		t.Error("the outcome was written again")
	}
	if fi, err := os.Stat(filepath.Join(f.srv.Data, "specs", id+".json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the specification in the data directory: %v %v", fi, err)
	}
	if st := f.status(); len(st.Arms) != 0 {
		t.Errorf("a claimed arm in the status: %+v", st.Arms)
	}

	// a boot while it writes: the rescue system without a token
	bare := f.boot("0c:c4:7a:aa:bb:01", efi)
	_, initrd := f.get("GET", "/b/"+bootID(t, bare)+"/initrd")
	if c := bootConfig(t, initrd); c["ump/token"] != "" || strings.Contains(c["ump/cmdline"], "ump.pxe") || c["ump/authorized_keys"] != "ssh-ed25519 AAAAump ump\nssh-ed25519 AAAAoleg oleg\n" {
		t.Errorf("the bare rescue: %q", c)
	}
	if s := f.status().Machines["0c:c4:7a:aa:bb:01"]; s.Answer != AnswerRescueBare || s.Arm != arm.ID {
		t.Errorf("the bare rescue in the status: %+v", s)
	}

	// the log, in parts: sent again, overlapping, beyond the end
	for _, c := range []struct {
		offset, part string
		code         int
	}{
		{"0", "hello\n", 200},
		{"0", "hello\n", 200},
		{"3", "lo\nworld\n", 200},
		{"20", "gap\n", 409},
		{"x", "", 400},
		{"5", "\n", 200},
	} {
		code, b, h := f.do("POST", "/api/v1/log?offset="+c.offset, token, c.part)
		if code != c.code {
			t.Errorf("log at %s: %d %s", c.offset, code, b)
		}
		if code == 409 && h.Get("Ump-Log-Offset") != "12" {
			t.Errorf("log beyond the end: %q", h.Get("Ump-Log-Offset"))
		}
	}
	if code, _, _ := f.do("POST", "/api/v1/log?offset=12", token, strings.Repeat("x", maxLogPart+1)); code != 413 {
		t.Errorf("a part beyond 1 MB: %d", code)
	}
	f.srv.pushLogs(f.ctx)
	if data, _, _ := f.files.Read(f.ctx, LogPath(DefaultSite, "web-07")); string(data) != "hello\nworld\n" {
		t.Errorf("the log in the backend: %q", data)
	}

	// the report: once in the outcome, 200, and again
	report := `{"boot": "` + id + `", "exit": 0, "record": "os=debian-13\n", "host_keys": ["ssh-ed25519 AAAAnew"], "log_tail": "done\n"}`
	if code, b, _ := f.do("POST", "/api/v1/report", token, report); code != 200 || b != "taken\n" {
		t.Fatalf("report: %d %s", code, b)
	}
	o = f.outcome()
	if o.Phase != PhaseInstalled || o.Exit == nil || *o.Exit != 0 || !o.Written || o.Record != "os=debian-13\n" || len(o.HostKeys) != 1 || o.LogTail != "done\n" || o.Boot != id {
		t.Errorf("outcome after the report: %+v", o)
	}
	_, v3, _ := f.files.Read(f.ctx, OutcomePath(DefaultSite, "web-07"))
	if code, _, _ := f.do("POST", "/api/v1/report", token, report); code != 200 {
		t.Errorf("report again: %d", code)
	}
	if _, v4, _ := f.files.Read(f.ctx, OutcomePath(DefaultSite, "web-07")); v4 != v3 {
		t.Error("the outcome was written again")
	}
	if code, _, _ := f.do("GET", "/api/v1/spec", token, ""); code != 409 {
		t.Errorf("spec after the report: %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.srv.Data, "specs", id+".json")); !os.IsNotExist(err) {
		t.Errorf("the specification stays after the report: %v", err)
	}
	// every later answer: the disk
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); got != "#!ipxe\necho ump: web-07: local\nexit\n" {
		t.Errorf("after the installation:\n%s", got)
	}
	f.load()
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); got != "#!ipxe\necho ump: web-07: local\nexit\n" {
		t.Errorf("after the installation, loaded again:\n%s", got)
	}
	for _, want := range []string{
		"web-07: install armed (a7f3c2e1, by oleg@laptop, no expiry)",
		"0c:c4:7a:aa:bb:01 (amd64 uefi, 192.0.2.17): web-07: install (arm a7f3c2e1, boot 1 of 3)",
		"web-07: rescue system up at 192.0.2.17 for the installation (arm a7f3c2e1)",
		"web-07: the installation (arm a7f3c2e1) claimed by boot " + id,
		"web-07: rescue without a token (its installation is being written, arm a7f3c2e1)",
		"web-07: installed (arm a7f3c2e1); it boots its disk from now on",
	} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, f.log)
		}
	}
}

// TestInstallFailed: a failed installation is an outcome too, and the
// machine boots its disk afterwards; a refusal (exit status 2) wrote
// nothing.
func TestInstallFailed(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	_, token, _ := f.installBoot()
	if code, _, _ := f.do("GET", "/api/v1/spec", token, ""); code != 200 {
		t.Fatal(code)
	}
	for _, bad := range []string{`{"exit": 300}`, `{"exit": 0} {}`, `{"exit": 0, "record": "` + strings.Repeat("x", maxRecord+1) + `"}`, `{"boot": "another", "exit": 0}`} {
		if code, _, _ := f.do("POST", "/api/v1/report", token, bad); code != 400 && code != 403 {
			t.Errorf("report %.40s: %d", bad, code)
		}
	}
	if code, _, _ := f.do("POST", "/api/v1/report", token, `{"exit": 2, "reason": "sda is not blank", "log_tail": "`+strings.Repeat("y", maxLogTail+10)+`"}`); code != 200 {
		t.Fatal(code)
	}
	o := f.outcome()
	if o.Phase != PhaseFailed || *o.Exit != 2 || o.Written || o.Reason != "sda is not blank" || len(o.LogTail) != maxLogTail {
		t.Errorf("outcome: %+v", o)
	}
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("after the failure:\n%s", got)
	}
	if !strings.Contains(f.log.String(), "web-07: the installation (arm a7f3c2e1) failed with exit status 2: sda is not blank") {
		t.Errorf("the log:\n%s", f.log)
	}
}

// TestInstallTwoBoots: two boots of the armed installation, at one
// server or at two, both say hello and ask for the specification; one
// gets it, the other 409, and the outcome is the first's.
func TestInstallTwoBoots(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	id1, token1, _ := f.installBoot()
	id2, token2, _ := f.installBoot()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, token := range []string{token1, token2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				code, _, _ := f.do("GET", "/api/v1/spec", token, "")
				if code != 503 {
					codes[i] = code
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	winner, loser := id1, token2
	switch {
	case codes[0] == 200 && codes[1] == 409:
	case codes[0] == 409 && codes[1] == 200:
		winner, loser = id2, token1
	default:
		t.Fatalf("two claims: %v", codes)
	}
	if o := f.outcome(); o.Boot != winner || o.Phase != PhaseWriting {
		t.Errorf("outcome: %+v", o)
	}
	// the loser has nothing to report
	if code, _, _ := f.do("POST", "/api/v1/report", loser, `{"exit": 0}`); code != 403 {
		t.Errorf("report of the loser: %d", code)
	}

	// a second server of the site booted it too before the claim: its
	// claim fails at the backend
	g := newFixture(t, installYAML)
	g.armInstall("b0b0b0b0cccccccc")
	_, tokenA, _ := g.installBoot()
	one, oneHTTP := g.srv, g.http
	two := g.open(t.TempDir(), t.TempDir())
	two.Name = "pxe2"
	twoHTTP := httptest.NewServer(two.Handler())
	defer twoHTTP.Close()
	g.srv, g.http = two, twoHTTP
	idB, tokenB, _ := g.installBoot()
	if code, _, _ := g.do("GET", "/api/v1/spec", tokenB, ""); code != 200 {
		t.Fatalf("the claim at the second server: %d", code)
	}
	g.srv, g.http = one, oneHTTP
	if code, body, _ := g.do("GET", "/api/v1/spec", tokenA, ""); code != 409 || !strings.Contains(body, "boot "+idB+" of pxe2 claimed the installation") {
		t.Errorf("the claim at the first server: %d %s", code, body)
	}
	// once it read the outcome, the first server answers the bare rescue
	g.load()
	if got := g.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: rescue\n") {
		t.Errorf("at the first server while the second's installation writes:\n%s", got)
	}
}

// TestInstallAttempts: an armed installation boots three times at a
// server without a claim, then the disk; the count outlives a restart.
func TestInstallAttempts(t *testing.T) {
	f := newFixture(t, installYAML)
	arm := f.armInstall("a7f3c2e1aaaabbbb")
	for range installAttempts {
		bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	}
	want := "local: 3 boots, and none claimed the installation (does the rescue system come up? see the console)"
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, want) {
		t.Errorf("the fourth boot:\n%s", got)
	}
	if as := f.status().Arms[arm.ID]; as == nil || as.Ready || !strings.HasPrefix(as.Cannot, "3 boots") {
		t.Errorf("the arm in the status: %+v", as)
	}
	again := f.open(f.srv.Data, f.srv.Cache.Dir)
	f.http.Config.Handler = again.Handler()
	f.srv = again
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, want) {
		t.Errorf("after a restart:\n%s", got)
	}
	// a new arm counts anew, and the old one's count goes with it
	f.store.Disarm(f.ctx, DefaultSite, "web-07")
	f.load()
	f.srv.prune()
	if f.srv.attempts[arm.ID] != 0 {
		t.Errorf("attempts of an arm that is gone: %v", f.srv.attempts)
	}
	f.armInstall("b0b0b0b0cccccccc")
	f.installBoot()
}

// TestInstallRestart: a server restarted while an installation writes
// takes the rest of its log, a lost answer and its report.
func TestInstallRestart(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	id, token, _ := f.installBoot()
	_, spec, _ := f.do("GET", "/api/v1/spec", token, "")
	f.do("POST", "/api/v1/log?offset=0", token, "one\n")

	again := f.open(f.srv.Data, f.srv.Cache.Dir)
	f.http.Config.Handler = again.Handler()
	f.srv = again
	if code, body, _ := f.do("GET", "/api/v1/spec", token, ""); code != 200 || body != spec {
		t.Errorf("spec after a restart: %d %s", code, body)
	}
	for _, part := range []struct{ offset, text string }{{"0", "one\n"}, {"4", "two\n"}} {
		if code, b, _ := f.do("POST", "/api/v1/log?offset="+part.offset, token, part.text); code != 200 {
			t.Errorf("log at %s after a restart: %d %s", part.offset, code, b)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.srv.Data, "logs", "web-07", id+".log")); string(data) != "one\ntwo\n" {
		t.Errorf("the log: %q", data)
	}
	// a boot while it writes: the bare rescue, from the journal and the backend
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: rescue\n") {
		t.Errorf("a boot after the restart:\n%s", got)
	}
	if code, _, _ := f.do("POST", "/api/v1/report", token, `{"exit": 0}`); code != 200 {
		t.Fatalf("report after a restart: %d", code)
	}
	if o := f.outcome(); o.Phase != PhaseInstalled || o.Boot != id {
		t.Errorf("outcome: %+v", o)
	}
	f.srv.bg.Wait()
	if data, _, _ := f.files.Read(f.ctx, LogPath(DefaultSite, "web-07")); string(data) != "one\ntwo\n" {
		t.Errorf("the log in the backend: %q", data)
	}
}

// TestInstallBackendDown: without the backend an armed installation
// boots nothing, its claim and its report are refused with 503, which
// the rescue system asks again, and its log is kept; back, all goes on.
func TestInstallBackendDown(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	f.files.down.Store(true)
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local: the arm could not be read again (connection refused), and an armed installation boots only then") {
		t.Errorf("an armed installation without the backend:\n%s", got)
	}
	f.files.down.Store(false)
	id, token, _ := f.installBoot()
	f.files.down.Store(true)
	if code, _, h := f.do("GET", "/api/v1/spec", token, ""); code != 503 || h.Get("Retry-After") == "" {
		t.Errorf("a claim without the backend: %d", code)
	}
	f.files.down.Store(false)
	if o := f.outcome(); o != nil {
		t.Errorf("an outcome without the backend: %+v", o)
	}
	if code, _, _ := f.do("GET", "/api/v1/spec", token, ""); code != 200 {
		t.Fatalf("the claim, back: %d", code)
	}
	f.files.down.Store(true)
	if code, _, _ := f.do("POST", "/api/v1/log?offset=0", token, "output\n"); code != 200 {
		t.Errorf("log without the backend: %d", code)
	}
	if code, _, _ := f.do("POST", "/api/v1/report", token, `{"exit": 0}`); code != 503 {
		t.Errorf("a report without the backend: %d", code)
	}
	// a boot of it meanwhile: the bare rescue, as the server knows it
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: rescue\n") {
		t.Errorf("a boot while it writes, without the backend:\n%s", got)
	}
	f.files.down.Store(false)
	if o := f.outcome(); o.Phase != PhaseWriting {
		t.Errorf("outcome before the report: %+v", o)
	}
	if code, _, _ := f.do("POST", "/api/v1/report", token, `{"exit": 0}`); code != 200 {
		t.Fatalf("the report, back: %d", code)
	}
	if o := f.outcome(); o.Phase != PhaseInstalled || o.Boot != id {
		t.Errorf("outcome: %+v", o)
	}
	if !strings.Contains(f.log.String(), "web-07: the claim of the arm a7f3c2e1: the backend did not take it (connection refused); the rescue system asks again") {
		t.Errorf("the log:\n%s", f.log)
	}
}

// TestInstallDeadline: an installation that does not report by its
// deadline is interrupted, by any server of the site, and its machine
// boots its disk; a late report still counts.
func TestInstallDeadline(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	_, token, _ := f.installBoot()
	f.do("GET", "/api/v1/spec", token, "")
	two := f.open(t.TempDir(), t.TempDir())
	two.Name = "pxe2"
	f.now = f.now.Add(DefaultInstallTimeout - time.Minute)
	two.interrupt(f.ctx)
	if o := f.outcome(); o.Phase != PhaseWriting {
		t.Errorf("interrupted before the deadline: %+v", o)
	}
	f.now = f.now.Add(time.Minute)
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("at the deadline:\n%s", got)
	}
	two.Load(f.ctx)
	two.interrupt(f.ctx)
	if o := f.outcome(); o.Phase != PhaseInterrupted || o.Daemon != "pxe1" {
		t.Errorf("after the deadline: %+v", o)
	}
	f.now = f.now.Add(3 * time.Hour)
	if code, _, _ := f.do("POST", "/api/v1/report", token, `{"exit": 0}`); code != 200 {
		t.Errorf("a late report: %d", code)
	}
	if o := f.outcome(); o.Phase != PhaseInstalled {
		t.Errorf("after a late report: %+v", o)
	}
	if !strings.Contains(f.log.String(), "did not report by its deadline") {
		t.Errorf("the log:\n%s", f.log)
	}
}

// TestInstallNeverTwice: an installation is not served again when the
// backend lost its outcome while the arm stands (the server puts it
// back), nor when the arm was disarmed or expired before the claim.
func TestInstallNeverTwice(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	id, token, _ := f.installBoot()
	f.do("GET", "/api/v1/spec", token, "")
	f.do("POST", "/api/v1/report", token, `{"exit": 0}`)
	f.files.Delete(f.ctx, OutcomePath(DefaultSite, "web-07"))
	f.load()
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("with the outcome lost:\n%s", got)
	}
	f.srv.writeConsumed(f.ctx)
	if o := f.outcome(); o == nil || o.Phase != PhaseInstalled || o.Boot != id {
		t.Errorf("the outcome put back: %+v", o)
	}
	if !strings.Contains(f.log.String(), "web-07: the backend lost the outcome of the arm a7f3c2e1, which was claimed here: put back (installed)") {
		t.Errorf("the log:\n%s", f.log)
	}
	// settled by the CLI, the arm first: not put back, though the server
	// read the outcome gone before it read the arm gone
	f.files.Delete(f.ctx, OutcomePath(DefaultSite, "web-07"))
	f.load()
	f.store.Disarm(f.ctx, DefaultSite, "web-07")
	f.srv.writeConsumed(f.ctx)
	if o := f.outcome(); o != nil {
		t.Errorf("a settled outcome put back: %+v", o)
	}
	// a report finds its outcome lost while the arm stands: written anew;
	// gone with its arm: not recorded, and the rescue system asks again
	h := newFixture(t, installYAML)
	h.armInstall("e0e0e0e0ffffffff")
	_, token, _ = h.installBoot()
	h.do("GET", "/api/v1/spec", token, "")
	h.files.Delete(h.ctx, OutcomePath(DefaultSite, "web-07"))
	if code, _, _ := h.do("POST", "/api/v1/report", token, `{"exit": 1}`); code != 200 {
		t.Errorf("a report whose outcome was lost: %d", code)
	}
	if o := h.outcome(); o == nil || o.Phase != PhaseFailed {
		t.Errorf("the outcome written anew: %+v", o)
	}
	h.store.Forget(h.ctx, DefaultSite, "web-07")
	h.armInstall("f0f0f0f0aaaaaaaa")
	_, token, _ = h.installBoot()
	h.do("GET", "/api/v1/spec", token, "")
	h.store.Forget(h.ctx, DefaultSite, "web-07")
	if code, body, _ := h.do("POST", "/api/v1/report", token, `{"exit": 0}`); code != 409 || !strings.Contains(body, "gone with its arm") {
		t.Errorf("a report whose outcome is gone with its arm: %d %s", code, body)
	}
	if o := h.outcome(); o != nil {
		t.Errorf("an outcome written for a settled installation: %+v", o)
	}

	// disarmed between the boot and the claim: nothing to claim
	g := newFixture(t, installYAML)
	g.armInstall("b0b0b0b0cccccccc")
	_, token, _ = g.installBoot()
	g.store.Disarm(g.ctx, DefaultSite, "web-07")
	if code, body, _ := g.do("GET", "/api/v1/spec", token, ""); code != 410 || !strings.Contains(body, "the arm b0b0b0b0 is gone") {
		t.Errorf("the claim of a disarmed installation: %d %s", code, body)
	}
	if o, _, _ := g.store.Outcome(g.ctx, DefaultSite, "web-07"); o != nil {
		t.Errorf("an outcome of a disarmed installation: %+v", o)
	}
	// disarmed a moment ago, before the server read it again: the disk
	g.armInstall("c0c0c0c0dddddddd")
	g.store.Disarm(g.ctx, DefaultSite, "web-07")
	if got := g.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("an installation disarmed before the poll:\n%s", got)
	}
	// expired between the boot and the claim
	a := g.armInstall("d0d0d0d0eeeeeeee")
	a.Until = g.now.Add(time.Minute)
	g.store.Disarm(g.ctx, DefaultSite, "web-07")
	g.store.PutArm(g.ctx, DefaultSite, a, "")
	g.load()
	_, token, _ = g.installBoot()
	g.now = g.now.Add(time.Minute)
	if code, body, _ := g.do("GET", "/api/v1/spec", token, ""); code != 410 || !strings.Contains(body, "expired") {
		t.Errorf("the claim of an expired installation: %d %s", code, body)
	}
}

// TestInstallDisarmRace: a disarm that removes the arm after the claim
// read it, and reads the outcome before the claim wrote it, sees no
// claim; the claim then sees the arm gone, takes itself back and hands
// out nothing.
func TestInstallDisarmRace(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	_, token, _ := f.installBoot()
	saw := "?"
	race := func(p string) {
		if p != OutcomePath(DefaultSite, "web-07") {
			return
		}
		f.files.onRead.Store(nil)
		// as the provider's Disarm: the arm, then the outcome
		f.store.Disarm(f.ctx, DefaultSite, "web-07")
		o, _, _ := f.store.Outcome(f.ctx, DefaultSite, "web-07")
		saw = fmt.Sprint(o)
	}
	f.files.onRead.Store(&race)
	code, body, _ := f.do("GET", "/api/v1/spec", token, "")
	f.files.onRead.Store(nil)
	if saw != "<nil>" {
		t.Fatalf("the disarm saw %s", saw)
	}
	if code != 410 || !strings.Contains(body, "the arm a7f3c2e1 is gone") {
		t.Errorf("the claim after the disarm: %d %s", code, body)
	}
	if o := f.outcome(); o != nil {
		t.Errorf("the claim stayed: %+v", o)
	}
	if !strings.Contains(f.log.String(), "web-07: the arm a7f3c2e1 went while boot ") {
		t.Errorf("the log:\n%s", f.log)
	}
	// it boots its disk, not the rescue system of an installation that writes
	f.load()
	if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local\n") {
		t.Errorf("after the claim went:\n%s", got)
	}
}

// TestInstallCannot: what keeps a server from an armed installation is
// in its status, and the machine gets its disk.
func TestInstallCannot(t *testing.T) {
	f := newFixture(t, installYAML)
	a := f.armInstall("a7f3c2e1aaaabbbb")
	cannot := func(want string) {
		t.Helper()
		f.load()
		if as := f.status().Arms[a.ID]; as == nil || as.Ready || as.Cannot != want {
			t.Errorf("the arm in the status: %+v, want %q", as, want)
		}
		if got := f.boot("0c:c4:7a:aa:bb:01", efi); !strings.Contains(got, "web-07: local: "+want) {
			t.Errorf("the boot:\n%s", got)
		}
		f.srv.mu.Lock()
		clear(f.srv.attempts)
		f.srv.mu.Unlock()
	}
	put := func() {
		t.Helper()
		_, v, _ := f.files.Read(f.ctx, ArmPath(DefaultSite, "web-07"))
		if _, err := f.store.PutArm(f.ctx, DefaultSite, a, v); err != nil {
			t.Fatal(err)
		}
	}
	a.SpecHash = "0000"
	put()
	cannot("the arm's specification does not match its hash")
	a.SpecHash, a.Disks = "", nil
	put()
	cannot("the arm: no disk rule")
	a.Disks = &DiskRule{Rule: DisksPinned}
	put()
	cannot("the arm: the disk rule pinned names no disk")
	a.Disks, a.Firmware = &DiskRule{Rule: DisksSeen, IDs: []string{"wwn:0x5000c500a1b2c3d4"}}, FirmwareBIOS
	put()
	cannot("the installation is armed for bios, and pxe.yaml says uefi")
	a.Firmware = ""
	put()
	f.srv.mu.Lock()
	r := *f.srv.rescues[ArchAMD64]
	r.Manifest.Features = []string{FeatureBootConfig}
	f.srv.rescues[ArchAMD64] = &r
	f.srv.mu.Unlock()
	cannot("the rescue image 2026.11.01 installs nothing unattended; the storage needs a newer one")
	f.srv.Fetch(f.ctx)
	f.srv.Data = ""
	cannot("this PXE server has no data directory, which an unattended installation needs")
}

// TestInstallFirmware: an installation armed for UEFI is not booted by
// a machine that boots with BIOS.
func TestInstallFirmware(t *testing.T) {
	f := newFixture(t, strings.Replace(installYAML, "    firmware: uefi\n", "", 1))
	a := f.armInstall("a7f3c2e1aaaabbbb")
	a.Firmware = FirmwareUEFI
	_, v, _ := f.files.Read(f.ctx, ArmPath(DefaultSite, "web-07"))
	f.store.PutArm(f.ctx, DefaultSite, a, v)
	f.load()
	if got := f.boot("0c:c4:7a:aa:bb:01", "platform=pcbios&buildarch=i386&lm=1"); !strings.Contains(got, "local: the installation is armed for uefi, and the machine boots with bios") {
		t.Errorf("BIOS:\n%s", got)
	}
	_, token, _ := f.installBoot()
	if _, body, _ := f.do("GET", "/api/v1/spec", token, ""); !strings.Contains(body, `"firmware":"uefi"`) {
		t.Errorf("spec: %s", body)
	}
}

// TestInstallRequests: what the endpoints refuse.
func TestInstallRequests(t *testing.T) {
	f := newFixture(t, installYAML)
	// a rescue boot's token
	rescue := bootID(t, f.boot("52:54:00:12:34:56", efi))
	_, initrd := f.get("GET", "/b/"+rescue+"/initrd")
	rt := strings.TrimSpace(bootConfig(t, initrd)["ump/token"])
	if code, body, _ := f.do("GET", "/api/v1/spec", rt, ""); code != 403 || body != "no installation is armed for this boot\n" {
		t.Errorf("spec of a rescue boot: %d %s", code, body)
	}
	for _, p := range []string{"/api/v1/log?offset=0", "/api/v1/report"} {
		if code, _, _ := f.do("POST", p, rt, `{"exit": 0}`); code != 403 {
			t.Errorf("%s of a rescue boot: %d", p, code)
		}
	}
	// before the hello, and from another address than the hello's
	f.armInstall("a7f3c2e1aaaabbbb")
	id := bootID(t, f.boot("0c:c4:7a:aa:bb:01", efi))
	_, initrd = f.get("GET", "/b/"+id+"/initrd")
	token := strings.TrimSpace(bootConfig(t, initrd)["ump/token"])
	if code, body, _ := f.do("GET", "/api/v1/spec", token, ""); code != 403 || body != "the rescue system of this boot said no hello\n" {
		t.Errorf("spec before the hello: %d %s", code, body)
	}
	_, token, _ = f.installBoot()
	f.srv.mu.Lock()
	f.srv.tokens[tokenHash(token)].Addr = "192.0.2.66"
	f.srv.mu.Unlock()
	if code, body, _ := f.do("GET", "/api/v1/spec", token, ""); code != 403 || body != "the specification goes to the address of the hello, 192.0.2.66\n" {
		t.Errorf("spec from another address: %d %s", code, body)
	}
}

// TestInstallLogBound: a log keeps its first 8 MB and says so; parts
// beyond that are taken all the same; the backend gets the end.
func TestInstallLogBound(t *testing.T) {
	f := newFixture(t, installYAML)
	f.armInstall("a7f3c2e1aaaabbbb")
	id, token, _ := f.installBoot()
	f.do("GET", "/api/v1/spec", token, "")
	part := strings.Repeat("x", maxLogPart-1) + "\n"
	for i := range maxLog/maxLogPart + 2 {
		if code, b, _ := f.do("POST", "/api/v1/log?offset="+strconv.Itoa(i*maxLogPart), token, part); code != 200 {
			t.Fatalf("part %d: %d %s", i, code, b)
		}
	}
	data, _ := os.ReadFile(filepath.Join(f.srv.Data, "logs", "web-07", id+".log"))
	if len(data) != maxLog+len(logCut) || !strings.HasSuffix(string(data), logCut) {
		t.Errorf("the log: %d bytes, ends %q", len(data), data[len(data)-80:])
	}
	f.srv.pushLogs(f.ctx)
	pushed, _, _ := f.files.Read(f.ctx, LogPath(DefaultSite, "web-07"))
	if !strings.HasPrefix(string(pushed), "[ump: the first ") || !strings.HasSuffix(string(pushed), logCut) || len(pushed) > maxLogPushed+512 {
		t.Errorf("the log in the backend: %d bytes, starts %q", len(pushed), pushed[:80])
	}
	// the last few logs of a machine stay
	dir := filepath.Join(f.srv.Data, "logs", "web-07")
	for i := range 6 {
		p := filepath.Join(dir, "old"+strconv.Itoa(i)+".log")
		os.WriteFile(p, nil, 0o600)
		old := time.Now().Add(-time.Duration(i+1) * time.Hour)
		os.Chtimes(p, old, old)
	}
	f.srv.pruneLogs("web-07", logsKept)
	entries, _ := os.ReadDir(dir)
	if len(entries) != logsKept || !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == id+".log" }) || slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "old5.log" }) {
		t.Errorf("the logs kept: %v", entries)
	}
}

func TestParseReport(t *testing.T) {
	// the end of the log is cut to its last 64 KB, at a character
	tail := "é" + strings.Repeat("a", maxLogTail-1)
	r, err := ParseReport(strings.NewReader(`{"boot": "b", "exit": 1, "reason": "", "record": "a=b\n", "host_keys": ["k"], "log_tail": "` + tail + `"}`))
	if err != nil || r.Exit != 1 || r.LogTail != tail[2:] || r.Record != "a=b\n" {
		t.Errorf("%.80v %v", r, err)
	}
	for _, bad := range []string{``, `[]`, `{"exit": -1}`, `{"exit": 256}`, `{} {}`, `{"exit": "1"}`} {
		if _, err := ParseReport(strings.NewReader(bad)); err == nil {
			t.Errorf("%q is a report", bad)
		}
	}
}

// FuzzParseReport: no report makes the parser panic, and what it takes
// stays within the bounds.
func FuzzParseReport(f *testing.F) {
	f.Add([]byte(`{"boot": "x", "exit": 0, "record": "os=debian-13\n", "host_keys": ["ssh-ed25519 AAAA"], "log_tail": "done"}`))
	f.Add([]byte(`{"exit": 255} {}`))
	f.Add([]byte(`{"log_tail": "\xff\xfe"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseReport(bytes.NewReader(b))
		if err != nil {
			return
		}
		if r.Exit < 0 || r.Exit > 255 || len(r.Record) > maxRecord || len(r.LogTail) > maxLogTail {
			t.Errorf("%+v", r)
		}
	})
}
