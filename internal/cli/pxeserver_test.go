package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/pxe"
)

func TestDaemonCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"pxe", "serve"}, true},
		{[]string{"--config", "x.yaml", "pxe", "serve", "--site", "fra1"}, true},
		{[]string{"-v", "pxe", "serve"}, true},
		{[]string{"pxe", "status"}, false},
		{[]string{"config", "pxe", "serve"}, false},
		{[]string{"--config", "pxe", "serve"}, false},
		{[]string{"--", "pxe", "serve"}, false},
		{[]string{"pxe"}, false},
	} {
		if got := daemonCommand(c.args); got != c.want {
			t.Errorf("%v: %v", c.args, got)
		}
	}
}

// rescueStorage serves a rescue image for amd64 as the storage does.
func rescueStorage(t *testing.T) *httptest.Server {
	t.Helper()
	kernel, initrd := []byte("bzImage"), []byte("initrd")
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	manifest := fmt.Sprintf(`{"version": "2026.11.01", "arch": "amd64", "features": ["boot-config", "pxe-install"], "memory_mb": 412,
		"files": {"vmlinuz": {"sha256": "%s", "size": %d}, "initrd.zst": {"sha256": "%s", "size": %d}}}`, sum(kernel), len(kernel), sum(initrd), len(initrd))
	files := map[string][]byte{"/rescue/amd64/manifest.json": []byte(manifest), "/rescue/amd64/vmlinuz": kernel, "/rescue/amd64/initrd.zst": initrd}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// freePort is a TCP address on the loopback that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestPXEServeCheck(t *testing.T) {
	e := newTestEnv(t)
	storage := rescueStorage(t)
	e.write("staging:\n  image: " + storage.URL + "/rescue\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n")
	data, cache, ipxe := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(ipxe, "snponly.efi"), []byte("efi"), 0o644)
	os.WriteFile(filepath.Join(ipxe, "undionly.kpxe"), []byte("pxe"), 0o644)
	os.WriteFile(filepath.Join(ipxe, "README"), []byte("not iPXE"), 0o644)
	dirs := []string{"--data", data, "--cache", cache}

	// before apply there is nothing to serve
	_, errOut, code := e.run("", false, append([]string{"pxe", "serve", "--check", "--http", "127.0.0.1:0"}, dirs...)...)
	if code != 1 || !strings.Contains(errOut, "site default cannot be served: no site.json for site default: ump config pxe apply renders it") {
		t.Errorf("before apply: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	out, errOut, code := e.run("", false, append([]string{"pxe", "serve", "--check", "--http", "127.0.0.1:0", "--tftp", "127.0.0.1:0", "--ipxe-dir", ipxe}, dirs...)...)
	hash := siteDoc(t, pxe.DefaultSite).Hash
	for _, want := range []string{
		"Site default: site.json " + hash + ", 1 machine.\n",
		"Rescue image 2026.11.01 for amd64: verified, in " + cache + ".\n",
		"HTTP 127.0.0.1:0: bound.\n",
		"TFTP 127.0.0.1:0: bound, 2 iPXE binaries in " + ipxe + ": snponly.efi, undionly.kpxe.\n",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("check: code %d, %q lacks %q (%q)", code, out, want, errOut)
		}
	}
	if !strings.Contains(errOut, "pxe: default: site.json "+hash+": 1 machine\n") || !strings.Contains(errOut, "pxe: default: rescue image 2026.11.01 for amd64: verified\n") {
		t.Errorf("the log: %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(cache, "rescue", "2026.11.01", "amd64", "initrd.zst")); err != nil {
		t.Errorf("the cache: %v", err)
	}

	// the flags from the environment, the flag winning
	t.Setenv("UMP_PXE_SITE", "fra1")
	if _, errOut, code := e.run("", false, append([]string{"pxe", "serve", "--check", "--http", "127.0.0.1:0"}, dirs...)...); code != 1 || !strings.Contains(errOut, "no site.json for site fra1") {
		t.Errorf("UMP_PXE_SITE: code %d, %q", code, errOut)
	}
	if _, _, code := e.run("", false, append([]string{"pxe", "serve", "--check", "--site", "default", "--http", "127.0.0.1:0"}, dirs...)...); code != 0 {
		t.Errorf("--site over UMP_PXE_SITE: code %d", code)
	}
	t.Setenv("UMP_PXE_SITE", "")

	// a port in use
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, errOut, code := e.run("", false, append([]string{"pxe", "serve", "--check", "--http", busy.Addr().String()}, dirs...)...); code != 1 || !strings.Contains(errOut, "--http "+busy.Addr().String()+": the address is in use (does the service run already?)") {
		t.Errorf("a port in use: code %d, %q", code, errOut)
	}
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"--site", "Fra_1"}, `--site "Fra_1" is not a site's name`},
		{[]string{"--ipxe-dir", ipxe}, "--ipxe-dir names the files of the TFTP server: it needs --tftp"},
		{[]string{"--url", "http://192.0.2.5:8067/pxe"}, `--url "http://192.0.2.5:8067/pxe" is not where machines reach the server`},
		{[]string{"--poll", "10ms"}, "--poll 10ms: at least 1s"},
		{[]string{"--install-timeout", "5m"}, "--install-timeout 5m0s: at least 10m"},
		{[]string{"extra"}, "unknown command \"extra\""},
	} {
		if _, errOut, code := e.run("", false, append([]string{"pxe", "serve", "--check"}, c.args...)...); code != 2 || !strings.Contains(errOut, c.err) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	t.Setenv("UMP_PXE_POLL", "soon")
	if _, errOut, code := e.run("", false, "pxe", "serve", "--check"); code != 2 || !strings.Contains(errOut, "UMP_PXE_POLL: invalid argument \"soon\"") {
		t.Errorf("UMP_PXE_POLL: code %d, %q", code, errOut)
	}
}

func TestPXEServeBackend(t *testing.T) {
	e := newTestEnv(t)
	storage := rescueStorage(t)
	e.write("staging:\n  image: " + storage.URL + "/rescue\n")
	remote := filepath.Join(t.TempDir(), "fleet")
	t.Setenv("UMP_STATE", "file://"+remote)
	if _, errOut, code := e.run("", false, "config", "pxe", "create", "web-07", "--mac", "0c:c4:7a:aa:bb:01"); code != 0 {
		t.Fatalf("create: code %d, %q", code, errOut)
	}
	os.RemoveAll(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "homes"))
	_, errOut, code := e.run("", false, "pxe", "serve", "--check", "--http", "127.0.0.1:0", "--data", t.TempDir(), "--cache", t.TempDir())
	if code != 0 || !strings.Contains(errOut, "the backend file://"+remote) {
		t.Errorf("check with a backend: code %d, %q", code, errOut)
	}
	// the server reads the backend itself: no mirror
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ump", "homes")); err == nil {
		t.Error("the server pulled the home into a mirror")
	}
}

// syncBuffer is a buffer the server's goroutines write to at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPXEServe(t *testing.T) {
	e := newTestEnv(t)
	storage := rescueStorage(t)
	e.write("staging:\n  image: " + storage.URL + "/rescue\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n    mode: rescue\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	addr := freePort(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tftpAddr := udp.LocalAddr().String()
	udp.Close()
	ipxe := t.TempDir()
	os.WriteFile(filepath.Join(ipxe, "snponly.efi"), []byte("efi"), 0o644)
	var out, errb syncBuffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, httpClient: localOnlyClient}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int)
	go func() {
		done <- a.run(ctx, []string{"--config", e.cfgPath, "pxe", "serve", "--http", addr, "--tftp", tftpAddr, "--ipxe-dir", ipxe, "--poll", "1s", "--data", t.TempDir(), "--cache", t.TempDir()})
	}()
	get := func(path string) string {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(get("/boot.ipxe"), "#!ipxe") {
		if time.Now().After(deadline) {
			t.Fatalf("the server does not answer:\n%s", errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// the rescue image comes in the background
	var script string
	for {
		script = get("/ipxe?mac=0c:c4:7a:aa:bb:01&platform=efi&buildarch=x86_64")
		if strings.Contains(script, "kernel http://"+addr+"/b/") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(script, "echo ump: web-07: rescue\n") {
		t.Errorf("the script:\n%s\n%s", script, errb.String())
	}
	// the iPXE binary over TFTP
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	to, _ := net.ResolveUDPAddr("udp", tftpAddr)
	c.WriteTo([]byte("\x00\x01snponly.efi\x00octet\x00"), to)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 600)
	n, from, err := c.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "\x00\x03\x00\x01efi" {
		t.Errorf("TFTP: %v %q", err, buf[:n])
	} else {
		c.WriteTo([]byte{0, 4, 0, 1}, from)
	}
	stop()
	if code := <-done; code != 0 {
		t.Errorf("stopped: code %d\n%s", code, errb.String())
	}
	log := errb.String()
	for _, want := range []string{"pxe: default: serving site default: HTTP " + addr + ", TFTP " + tftpAddr + " (" + ipxe + ")\n", "pxe: default: tftp 127.0.0.1: snponly.efi sent (3 bytes in ",
		"pxe: default: stopping after the transfers under way\n", "pxe: default: stopped\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	// its last status is in the backend
	var st pxe.Status
	data, _ := os.ReadFile(filepath.Join(pxeHome(), ".pxe", "default", "status", pxeName()+".json"))
	if err := json.Unmarshal(data, &st); err != nil || st.Machines["0c:c4:7a:aa:bb:01"] == nil || st.Rescue["amd64"] != "2026.11.01" {
		t.Errorf("status: %v\n%s", err, data)
	}
}

func TestPXEStatus(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "unknown: local\nmachines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n    address: 192.0.2.30\n  db-01:\n    mac: [0c:c4:7a:aa:bb:10, 0c:c4:7a:aa:bb:11]\n    mode: rescue\n")
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	out, errOut, code := e.run("", false, "pxe", "status")
	if code != 0 || !strings.Contains(errOut, "No PXE server of default has written its status (ump pxe serve --site SITE).") ||
		!strings.Contains(out, "web-07   default   0c:c4:7a:aa:bb:01   local    -        -      -    -\n") {
		t.Errorf("no server: code %d, %q\n%s", code, errOut, out)
	}
	ctx := context.Background()
	store := pxeStoreOf()
	hash := siteDoc(t, pxe.DefaultSite).Hash
	hello := &pxe.Hello{Boot: "b1", Arm: "a7f3c2e1aaaabbbb", BootMAC: "0c:c4:7a:aa:bb:11", Interfaces: []pxe.Interface{{MAC: "0c:c4:7a:aa:bb:11", Addresses: []string{"192.0.2.41/24"}}}}
	store.PutStatus(ctx, &pxe.Status{Daemon: "pxe1", Site: pxe.DefaultSite, Version: "v0.8.0", URL: "http://192.0.2.5:8067", Started: e.now.Add(-time.Hour), Updated: e.now.Add(-8 * time.Second),
		SiteHash: hash, Rescue: map[string]string{"amd64": "2026.11.01"},
		Machines: map[string]*pxe.Seen{
			"0c:c4:7a:aa:bb:01": {Name: "web-07", At: e.now.Add(-2 * time.Minute), Address: "192.0.2.17", Arch: "amd64", Firmware: "uefi", Answer: "local", Signals: []string{"its disk does not boot: 5 network boots within 15 minutes"}},
			"0c:c4:7a:aa:bb:11": {Name: "db-01", At: e.now.Add(-time.Minute), Address: "192.0.2.40", Arch: "amd64", Firmware: "bios", Answer: "rescue", Arm: "a7f3c2e1aaaabbbb", Hello: hello},
		},
		Unknown:  map[string]*pxe.Seen{"52:54:00:99:99:99": {At: e.now.Add(-30 * time.Second), Address: "192.0.2.99", Arch: "amd64", Firmware: "uefi", Answer: "local"}},
		Warnings: []string{"rescue image for arm64: HTTP 404"}})
	store.PutStatus(ctx, &pxe.Status{Daemon: "pxe2", Site: pxe.DefaultSite, Version: "v0.7.9", Started: e.now.Add(-48 * time.Hour), Updated: e.now.Add(-5 * time.Hour), SiteHash: "0000",
		Machines: map[string]*pxe.Seen{"0c:c4:7a:aa:bb:01": {Name: "web-07", At: e.now.Add(-5 * time.Hour), Answer: "rescue", Reason: "old"}},
		Warnings: []string{"not shown: it stopped"}})

	out, errOut, code = e.run("", false, "pxe", "status")
	want := `SERVER   SITE      VERSION   URL                     UPDATED             SITE.JSON
pxe1     default   v0.8.0    http://192.0.2.5:8067   8s ago              ` + hash + ` (current)
pxe2     default   v0.7.9    -                       5h ago (stopped?)   0000 (old)

NAME     SITE      MAC                 STATUS   ANSWER   SEEN     BY     ADDRESS
web-07   default   0c:c4:7a:aa:bb:01   local    local    2m ago   pxe1   192.0.2.17
db-01    default   0c:c4:7a:aa:bb:11   rescue   rescue   1m ago   pxe1   192.0.2.40

default/pxe1: rescue image for arm64: HTTP 404
web-07: its disk does not boot: 5 network boots within 15 minutes
`
	if code != 0 || errOut != "" || out != want {
		t.Errorf("status: code %d, %q\n%s\nwant\n%s", code, errOut, out, want)
	}
	out, _, _ = e.run("", false, "pxe", "status", "db-01", "-o", "wide")
	if !strings.Contains(out, "db-01   default   0c:c4:7a:aa:bb:11   rescue   rescue   1m ago   pxe1   192.0.2.40   amd64 bios   192.0.2.41   a7f3c2e1\n") || strings.Contains(out, "SERVER") || strings.Contains(out, "web-07") {
		t.Errorf("status db-01 -o wide:\n%s", out)
	}
	out, _, code = e.run("", false, "pxe", "status", "52-54-00-99-99-99")
	if code != 0 || !strings.Contains(out, "52:54:00:99:99:99   default   30s ago   pxe1   192.0.2.99   amd64 uefi\n") {
		t.Errorf("status of an unknown MAC: code %d\n%s", code, out)
	}
	out, _, _ = e.run("", false, "pxe", "status", "--unknown", "--no-headers")
	if out != "52:54:00:99:99:99   default   30s ago   pxe1   192.0.2.99   amd64 uefi\n" {
		t.Errorf("--unknown:\n%s", out)
	}
	for _, c := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"nope"}, 1, `no machine "nope" in `},
		{[]string{"52:54:00:00:00:01"}, 1, "no machine of ~/.config/ump/pxe.yaml has the MAC 52:54:00:00:00:01, and no PXE server saw it"},
		{[]string{"-o", "name"}, 2, `unknown output format "name"`},
		{[]string{"--log", "web-07"}, 1, "no installation log of web-07: none of its installations reported one yet"},
	} {
		_, errOut, code := e.run("", false, append([]string{"pxe", "status"}, c.args...)...)
		if code != c.code || !strings.Contains(strings.ReplaceAll(errOut, pxeHome(), "~/.config/ump"), c.err) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	store.Files.Write(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"), []byte("installing\ndone\n"), "")
	if out, _, code := e.run("", false, "pxe", "status", "--log", "pxe/web-07"); code != 0 || out != "installing\ndone\n" {
		t.Errorf("--log: code %d, %q", code, out)
	}
	out, _, _ = e.run("", false, "pxe", "status", "-o", "json")
	var doc pxeStatusDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Servers) != 2 || len(doc.Machines) != 2 || len(doc.Unknown) != 1 ||
		doc.Machines[1].Hello == nil || doc.Machines[1].Rescue != "192.0.2.41" || !doc.Servers[0].Running || doc.Servers[1].Running || doc.Servers[1].Current {
		t.Errorf("-o json: %v\n%s", err, out)
	}
}

// TestPXEStatusLog: --log prints the log a server pushed, says how the
// installation stands, and falls back to the end its report carried.
func TestPXEStatusLog(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n")
	ctx := context.Background()
	store := pxeStoreOf()
	o := &pxe.Outcome{Arm: "a7f3c2e1aaaabbbb", Phase: pxe.PhaseWriting, Daemon: "pxe1", Boot: "b1", At: e.now, Deadline: e.now.Add(2 * time.Hour)}
	store.PutOutcome(ctx, pxe.DefaultSite, "web-07", o, "")
	store.Files.Write(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"), []byte{}, "")
	out, errOut, code := e.run("", false, "pxe", "status", "--log", "web-07")
	if code != 0 || out != "" || errOut != "The installation of web-07 (arm a7f3c2e1) is under way at default/pxe1 since 2026-10-12 09:00 UTC; it reports by 2026-10-12 11:00 UTC.\nIts log has no output yet.\n" {
		t.Errorf("under way: code %d, %q %q", code, out, errOut)
	}
	// the report's end of it when the backend has no log
	exit := 1
	o.Phase, o.Exit, o.LogTail = pxe.PhaseFailed, &exit, "install.sh: no disk\n"
	_, v, _ := store.Files.Read(ctx, pxe.OutcomePath(pxe.DefaultSite, "web-07"))
	store.PutOutcome(ctx, pxe.DefaultSite, "web-07", o, v)
	store.Files.Delete(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"))
	out, errOut, code = e.run("", false, "pxe", "status", "--log", "web-07")
	if code != 0 || out != "install.sh: no disk\n" || !strings.Contains(errOut, "(arm a7f3c2e1, default/pxe1): failed at 2026-10-12 09:00 UTC, exit status 1.\nIts log is not in the backend;") {
		t.Errorf("the report's end: code %d, %q %q", code, out, errOut)
	}
	// a machine a hand edit moved to another site: where its log is
	writePXE(t, "sites:\n  fra1: {}\nmachines:\n  web-07:\n    site: fra1\n    mac: 0c:c4:7a:aa:bb:01\n")
	store.Files.Write(ctx, pxe.LogPath(pxe.DefaultSite, "web-07"), []byte("==> partitioning\n"), "")
	if out, _, code := e.run("", false, "pxe", "status", "--log", "web-07"); code != 0 || out != "==> partitioning\n" {
		t.Errorf("moved: code %d, %q", code, out)
	}
}

func TestPXEDisarm(t *testing.T) {
	e := newTestEnv(t)
	e.write("{}\n")
	writePXE(t, "machines:\n  web-07:\n    mac: 0c:c4:7a:aa:bb:01\n  db-01:\n    mac: 0c:c4:7a:aa:bb:10\n")
	ctx := context.Background()
	store := pxeStoreOf()
	arm := &pxe.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: pxe.ModeRescue, By: "oleg@laptop", Created: e.now, Until: e.now.Add(15 * time.Minute)}
	store.PutArm(ctx, pxe.DefaultSite, arm, "")
	out, errOut, code := e.run("", false, "pxe", "disarm", "pxe/web-07", "db-01")
	if code != 0 || out != "Disarmed web-07: its armed rescue (a7f3c2e1, by oleg@laptop) does not boot.\ndb-01 has no arm.\n" {
		t.Errorf("disarm: code %d, %q %q", code, out, errOut)
	}
	if a, _, _ := store.Arm(ctx, pxe.DefaultSite, "web-07"); a != nil {
		t.Errorf("the arm stays: %+v", a)
	}
	// one that is over, as json
	store.PutArm(ctx, pxe.DefaultSite, arm, "")
	store.PutOutcome(ctx, pxe.DefaultSite, "web-07", &pxe.Outcome{Arm: arm.ID, Phase: pxe.PhaseBooted, At: e.now}, "")
	out, _, code = e.run("", false, "pxe", "disarm", "web-07", "-o", "json")
	var done []pxeDisarmed
	if err := json.Unmarshal([]byte(out), &done); err != nil || code != 0 || len(done) != 1 || done[0].Pending || done[0].Arm != arm.ID {
		t.Errorf("-o json: code %d, %v %s", code, err, out)
	}
	// an installation that is being written stays
	install := &pxe.Arm{ID: "b0b0b0b0cccccccc", Machine: "db-01", Mode: pxe.ModeInstall, Created: e.now}
	store.PutArm(ctx, pxe.DefaultSite, install, "")
	store.PutOutcome(ctx, pxe.DefaultSite, "db-01", &pxe.Outcome{Arm: install.ID, Phase: pxe.PhaseWriting, Daemon: "pxe1", At: e.now, Deadline: e.now.Add(2 * time.Hour)}, "")
	if _, errOut, code := e.run("", false, "pxe", "disarm", "db-01"); code != 3 || !strings.Contains(errOut, "db-01 is being installed (since 2026-10-12 09:00 UTC, by default/pxe1); it ends with its report or at 2026-10-12 11:00 UTC") {
		t.Errorf("being installed: code %d, %q", code, errOut)
	}
	if a, _, _ := store.Arm(ctx, pxe.DefaultSite, "db-01"); a == nil {
		t.Error("an installation that is being written was disarmed")
	}
	// one that installed and waits to be recorded keeps what ump records
	store.UpdateOutcome(ctx, pxe.DefaultSite, "db-01", func(o *pxe.Outcome) (*pxe.Outcome, error) {
		o.Phase = pxe.PhaseInstalled
		return o, nil
	})
	if out, _, code := e.run("", false, "pxe", "disarm", "db-01"); code != 0 || out != "db-01 is installed (b0b0b0b0, at 2026-10-12 09:00 UTC) and waits to be recorded: the next ump provision records it, and its arm goes with it.\n" {
		t.Errorf("installed: code %d, %q", code, out)
	}
	if a, _, _ := store.Arm(ctx, pxe.DefaultSite, "db-01"); a == nil {
		t.Error("an installation that waits to be recorded was disarmed")
	}
	if _, errOut, code := e.run("", false, "pxe", "disarm", "nope"); code != 1 || !strings.Contains(errOut, `no machine "nope" in `) {
		t.Errorf("nope: code %d, %q", code, errOut)
	}
}
