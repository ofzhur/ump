package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/pxe"
	"github.com/ofzhur/ump/internal/state"
)

// The unattended installations end to end: the CLI against PXE server
// pxe1 of site fra1, a pxe.Server over HTTP on the test's backend as
// ump pxe serve runs it, and the rescue side of the machines played by
// fakeRescue, which boots an armed installation, says hello, claims the
// specification, posts its output and reports, as ump-pxe install does.

const newHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINewSystemOfWeb07"

// rescueMachine is a machine at its network boots: its MAC, the address
// its rescue system gets and the one of its new system, whether it boots
// an armed installation at all, the exit status and reason it reports,
// and the disks its rescue system says it has.
type rescueMachine struct {
	mac     string
	address string
	system  string
	boots   bool
	exit    int
	reason  string
	disks   []pxe.GuardDisk
}

// fakeRescue is the PXE server and the machines of site fra1.
type fakeRescue struct {
	t    *testing.T
	e    *testEnv
	srv  *pxe.Server
	http *httptest.Server

	mu       sync.Mutex
	machines map[string]*rescueMachine // by name
	done     map[string]bool           // the arms handled
	// specs are the answers the claims got, by machine; installed the
	// addresses whose new system runs; failures what went wrong
	specs     map[string]string
	installed map[string]bool
	failures  []string
	// pinned are the known_hosts files of the ssh runs to the machines'
	// addresses, as they were
	pinned map[string][]string
}

// startFakeRescue runs pxe1 for site fra1 of the test's home and the
// machines of the map; the fake ssh finds the new system of a machine
// once it reported success.
func startFakeRescue(t *testing.T, e *testEnv, machines map[string]*rescueMachine) *fakeRescue {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &pxe.Server{Site: "fra1", Name: "pxe1", Version: "v0.8.0", Store: pxeStoreOf(), Cache: &pxe.Cache{Dir: t.TempDir(), Client: localOnlyClient},
		Data: t.TempDir(), Poll: 5 * time.Millisecond, Now: func() time.Time { return e.now }}
	if err := srv.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	// it runs once its status is there
	if err := srv.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fakeRescue{t: t, e: e, srv: srv, http: httptest.NewServer(srv.Handler()), machines: machines, done: map[string]bool{},
		specs: map[string]string{}, installed: map[string]bool{}, pinned: map[string][]string{}}
	e.remoteGate = f.gate
	ran, looped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ran)
		srv.Run(ctx)
	}()
	go func() {
		defer close(looped)
		for ctx.Err() == nil {
			srv.Flush(ctx)
			f.step(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-looped
		<-ran
		f.http.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, msg := range f.failures {
			t.Error(msg)
		}
	})
	return f
}

// gate sees every ssh run of the CLI: it records the host keys pinned
// for a machine's address, and lets the new system answer there once
// the machine reported success.
func (f *fakeRescue) gate(addr string, argv []string) {
	f.mu.Lock()
	installed := f.installed[addr]
	for _, arg := range argv {
		if file, ok := strings.CutPrefix(arg, "UserKnownHostsFile="); ok && file != "/dev/null" {
			data, _ := os.ReadFile(file)
			f.pinned[addr] = append(f.pinned[addr], string(data))
		}
	}
	f.mu.Unlock()
	if !installed {
		return
	}
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	if f.e.machines == nil {
		f.e.booted = true
		return
	}
	fm := f.e.machines[addr]
	if fm == nil {
		fm = &fakeMachine{}
		f.e.machines[addr] = fm
	}
	fm.booted = true
}

// step boots each machine's armed installation once its server took
// the arm.
func (f *fakeRescue) step(ctx context.Context) {
	st := f.srv.Status()
	for name, m := range f.snapshot() {
		a, _, err := pxeStoreOf().Arm(ctx, "fra1", name)
		if err != nil || a == nil || a.Mode != pxe.ModeInstall || !m.boots {
			continue
		}
		f.mu.Lock()
		done := f.done[a.ID]
		f.done[a.ID] = true
		f.mu.Unlock()
		if done {
			continue
		}
		if as := st.Arms[a.ID]; as == nil || !as.Ready {
			f.mu.Lock()
			delete(f.done, a.ID)
			f.mu.Unlock()
			continue
		}
		if err := f.install(name, m); err != nil {
			f.mu.Lock()
			f.failures = append(f.failures, fmt.Sprintf("%s: %v", name, err))
			f.mu.Unlock()
		}
	}
}

func (f *fakeRescue) snapshot() map[string]*rescueMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*rescueMachine{}
	for k, v := range f.machines {
		c := *v
		out[k] = &c
	}
	return out
}

func (f *fakeRescue) get(path string) []byte {
	resp, err := http.Get(f.http.URL + path)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

// do is a request of the rescue system with the boot's token.
func (f *fakeRescue) do(method, path, token string, body []byte) (int, string) {
	req, _ := http.NewRequest(method, f.http.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// install plays one boot of the machine's armed installation: the
// script, the boot config, the hello, the claim, the output and the
// report.
func (f *fakeRescue) install(name string, m *rescueMachine) error {
	script := string(f.get("/ipxe?mac=" + m.mac + "&ip=" + m.address + "&buildarch=x86_64&platform=efi&mem=65536"))
	if !strings.Contains(script, "echo ump: "+name+": install\n") {
		return fmt.Errorf("not an installation:\n%s", script)
	}
	initrd := regexp.MustCompile(`initrd --name initrd (\S+)`).FindStringSubmatch(script)
	if initrd == nil {
		return fmt.Errorf("no initrd:\n%s", script)
	}
	files, err := pxe.ReadCpio(f.get(strings.TrimPrefix(initrd[1], f.http.URL)))
	if err != nil {
		return err
	}
	var token, boot string
	for _, file := range files {
		switch file.Name {
		case "ump/token":
			token = strings.TrimSpace(string(file.Data))
		case "ump/cmdline":
			if b := regexp.MustCompile(`ump\.pxe\.boot=(\S+)`).FindStringSubmatch(string(file.Data)); b != nil {
				boot = b[1]
			}
		}
	}
	hello, _ := json.Marshal(pxe.Hello{Boot: boot, Rescue: "2026.11.01", Arch: "amd64", Firmware: "uefi", BootMAC: m.mac, HostKeys: []string{rescueHostKey},
		GuardDisks: m.disks, Interfaces: []pxe.Interface{{Name: "eno1", MAC: m.mac, Up: true, Addresses: []string{m.address + "/24"}}}})
	if code, body := f.do("POST", "/api/v1/hello", token, hello); code != 200 {
		return fmt.Errorf("hello: %d %s", code, body)
	}
	code, spec := f.do("GET", "/api/v1/spec", token, nil)
	if code != 200 {
		return fmt.Errorf("spec: %d %s", code, spec)
	}
	f.mu.Lock()
	f.specs[name] = spec
	f.mu.Unlock()
	out := "==> machine: amd64, UEFI firmware, 8 CPUs, 65536 MB RAM\n"
	if m.exit == 0 {
		out += "==> installed: ubuntu-26.04 2026.10.12 as " + name + "; reboot to start it\n"
	}
	if code, body := f.do("POST", "/api/v1/log?offset=0", token, []byte(out)); code != 200 {
		return fmt.Errorf("log: %d %s", code, body)
	}
	rep := pxe.Report{Boot: boot, Exit: m.exit, Reason: m.reason, LogTail: out}
	if m.exit == 0 {
		rep.Record, rep.HostKeys = "os=ubuntu-26.04\nhostname="+name+"\n", []string{newHostKey}
	}
	report, _ := json.Marshal(rep)
	if code, body := f.do("POST", "/api/v1/report", token, report); code != 200 {
		return fmt.Errorf("report: %d %s", code, body)
	}
	if m.exit == 0 {
		f.mu.Lock()
		f.installed[m.system] = true
		f.mu.Unlock()
	}
	return nil
}

// spec is the specification a machine's claim got.
func (f *fakeRescue) spec(name string) map[string]any {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var answer struct {
		Spec     map[string]any `json:"spec"`
		Firmware string         `json:"firmware"`
		Disks    map[string]any `json:"disks"`
	}
	if err := json.Unmarshal([]byte(f.specs[name]), &answer); err != nil {
		f.t.Fatalf("the claim of %s: %v (%q)", name, err, f.specs[name])
	}
	answer.Spec["_disks"], answer.Spec["_firmware"] = answer.Disks, answer.Firmware
	return answer.Spec
}

// waitInstalled waits until the new system at addr runs: the machine
// reported success.
func (f *fakeRescue) waitInstalled(addr string) {
	f.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		done := f.installed[addr]
		f.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s did not install", addr)
		}
	}
}

func (f *fakeRescue) pinnedAt(addr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pinned[addr])
}

// setupUnattended is the fleet's site behind its jump host with web-07,
// the rescue image of the storage (with the feature pxe-install), the
// repository's profiles, and short waits.
func setupUnattended(t *testing.T, pxeYAML string) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	storage := rescueStorage(t)
	e.write("staging:\n  image: " + storage.URL + "/rescue\n")
	e.writeProfiles()
	writePXE(t, pxeYAML)
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	oldPoll, oldReady, oldApply := pxeRescuePoll, pxeReadyWait, pxeApplyPoll
	pxeRescuePoll, pxeReadyWait, pxeApplyPoll = 5*time.Millisecond, 10*time.Second, 5*time.Millisecond
	t.Cleanup(func() { pxeRescuePoll, pxeReadyWait, pxeApplyPoll = oldPoll, oldReady, oldApply })
	return e
}

func web07(boots bool) map[string]*rescueMachine {
	return map[string]*rescueMachine{"web-07": {mac: "0c:c4:7a:aa:bb:01", address: "192.0.2.50", system: "192.0.2.17", boots: boots}}
}

// outcomeOf is the outcome of a machine at fra1, nil when none.
func outcomeOf(t *testing.T, name string) *pxe.Outcome {
	t.Helper()
	o, _, err := pxeStoreOf().Outcome(context.Background(), "fra1", name)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func armOf(t *testing.T, name string) *pxe.Arm {
	t.Helper()
	a, _, err := pxeStoreOf().Arm(context.Background(), "fra1", name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUnattendedReinstall(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	f := startFakeRescue(t, e, web07(true))
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision", "-o", "json")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"The installation on web-07 may erase blank disks alone (no network boot of web-07 reported its disks).\n",
		"Installation armed for pxe/web-07 (",
		"); it installs at its next boot.\nNote: PXE server pxe1 of site fra1 is ready to boot it\n",
		"Checking root access to 192.0.2.17 over SSH, for the reboot ... no (tried root, ump).\n",
		"Power-cycle web-07 now (or reset it): it installs at its next boot.\n",
		"Waiting for the installation of web-07 (up to 1h) ...\n",
		"==> installed: ubuntu-26.04 2026.10.12 as web-07; reboot to start it\n",
		"Waiting for the new system of web-07 at 192.0.2.17 (up to 15m) ... up after ",
		"web-07 is up: Ubuntu 26.04.1 LTS, hostname web-07, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-07).\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	var res installResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Via != "unattended" || !res.Rebooted || res.System == nil || res.InstalledAt == nil || res.Admin != "ump" {
		t.Errorf("result: %v\n%s", err, out)
	}
	// what the machine claimed is what the CLI rendered: the hostname, its
	// network, its private address, the disks it may erase
	spec := f.spec("web-07")
	uplink, _ := json.Marshal(spec["network"].(map[string]any)["uplink"])
	if spec["hostname"] != "web-07" || spec["os"] != "ubuntu-26.04" || string(uplink) != `{"dns":["192.0.2.53"],"ipv4":{"address":"192.0.2.17/24","gateway":"192.0.2.1","mode":"static"},"ipv6":{"mode":"none"},"mac":"0c:c4:7a:aa:bb:01"}` {
		t.Errorf("the specification: %v %s", spec["hostname"], uplink)
	}
	if d, _ := json.Marshal(spec["_disks"]); string(d) != `{"rule":"blank"}` || spec["_firmware"] != "" {
		t.Errorf("disks %s, firmware %v", d, spec["_firmware"])
	}
	private := spec["network"].(map[string]any)["private"].(map[string]any)
	// the new system was checked at its own address, through the jump
	// host, with the host key it reported pinned
	if pins := f.pinnedAt("192.0.2.17"); len(pins) == 0 || pins[len(pins)-1] != "192.0.2.17 "+newHostKey+"\n" {
		t.Errorf("pinned: %q", pins)
	}
	var check []string
	for _, argv := range e.remoteCalls {
		if strings.HasPrefix(argv[len(argv)-1], "sed -n 's/^PRETTY_NAME=//p'") {
			check = argv
		}
	}
	if !hasOptions(check, "ProxyJump=ump@198.51.100.7", "StrictHostKeyChecking=yes") || check[len(check)-3] != "ump@192.0.2.17" {
		t.Errorf("the check of the new system: %v", check)
	}
	// recorded with its private address; the arm and its outcome are gone,
	// and the machine boots its disk
	st := e.readState()
	if !strings.Contains(st, "pxe/web-07:") || !strings.Contains(st, "os: ubuntu-26.04") || !strings.Contains(st, "address: "+private["address"].(string)) {
		t.Errorf("state:\n%s", st)
	}
	if a, o := armOf(t, "web-07"), outcomeOf(t, "web-07"); a != nil || o != nil {
		t.Errorf("left: %+v %+v", a, o)
	}
	if !slices.Contains(e.forgotHosts, "192.0.2.17") {
		t.Errorf("forgot %v", e.forgotHosts)
	}
	if hf := e.hostFile("web-07.yml"); !strings.Contains(hf, "ansible_host: 192.0.2.17\n") {
		t.Errorf("host file:\n%s", hf)
	}
	if script := string(f.get("/ipxe?mac=0c:c4:7a:aa:bb:01&buildarch=x86_64&platform=efi")); !strings.Contains(script, "echo ump: web-07: local\n") {
		t.Errorf("after the installation:\n%s", script)
	}
}

// TestUnattendedFailed: a refused installation writes nothing, records
// nothing and gives the private address back; its outcome stays.
func TestUnattendedFailed(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	machines := web07(true)
	machines["web-07"].exit, machines["web-07"].reason = 2, "/dev/sda is not blank (gpt), and the arm takes blank disks only"
	f := startFakeRescue(t, e, machines)
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision")
	if code != 1 || !strings.Contains(errOut, "ump: the installation of web-07 was refused (exit status 2: /dev/sda is not blank (gpt), and the arm takes blank disks only); nothing was written, nothing is recorded, and the machine stays in UMP's rescue system (ump pxe status --log web-07 has its log)\n") {
		t.Fatalf("code %d\n%s", code, errOut)
	}
	if st := e.readState(); strings.Contains(st, "installed:") || strings.Contains(st, "10.0.0.") {
		t.Errorf("state:\n%s", st)
	}
	if o := outcomeOf(t, "web-07"); o == nil || o.Phase != pxe.PhaseFailed {
		t.Errorf("outcome %+v", o)
	}
	if out, _, _ := e.run("", false, "server", "list", "-o", "json"); !strings.Contains(out, `"status": "failed"`) {
		t.Errorf("list:\n%s", out)
	}
	// the next one takes its place, and installs
	failed := outcomeOf(t, "web-07").Arm
	f.mu.Lock()
	f.machines["web-07"].exit, f.machines["web-07"].reason = 0, ""
	f.mu.Unlock()
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision"); code != 0 {
		t.Fatalf("again: code %d\n%s", code, errOut)
	}
	if st := e.readState(); !strings.Contains(st, "os: ubuntu-26.04") || armOf(t, "web-07") != nil || outcomeOf(t, "web-07") != nil {
		t.Errorf("after %s failed:\n%s", failed, st)
	}
}

// TestUnattendedTimeout: an installation no boot claimed is disarmed,
// and the private address goes back.
func TestUnattendedTimeout(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	startFakeRescue(t, e, web07(false))
	old := installWait
	installWait = 300 * time.Millisecond
	t.Cleanup(func() { installWait = old })
	t.Setenv("UMP_WAIT", "100ms")
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision")
	if code != exitUnreachable || !strings.Contains(errOut, "ump: no boot of web-07 claimed its installation within 300ms; it is disarmed (ump pxe status web-07 shows what the PXE servers saw)\n") {
		t.Fatalf("code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a != nil {
		t.Errorf("the arm stayed: %+v", a)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.") {
		t.Errorf("the private address stayed:\n%s", st)
	}
}

// TestUnattendedNoWait: --no-wait arms and returns; the machine installs
// by itself, and the next ump provision records it and provisions it.
func TestUnattendedNoWait(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	machines := web07(false)
	f := startFakeRescue(t, e, machines)
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "Power-cycle web-07 (or reset it) for it to install; ump pxe status --log web-07 follows it, and the next ump provision records it and provisions it.\n") {
		t.Fatalf("reinstall: code %d\n%s\n%s", code, out, errOut)
	}
	a := armOf(t, "web-07")
	if a == nil || a.Mode != pxe.ModeInstall || !a.Until.Equal(e.now.Add(2*time.Hour)) || !strings.Contains(out, `"armed": "`+a.ID+`"`) {
		t.Fatalf("arm %+v\n%s", a, out)
	}
	if strings.Contains(e.readState(), "installed:") || !strings.Contains(e.readState(), "10.0.0.") {
		t.Errorf("state:\n%s", e.readState())
	}
	if out, _, _ := e.run("", false, "server", "list", "-o", "json"); !strings.Contains(out, `"status": "armed install"`) {
		t.Errorf("list:\n%s", out)
	}
	// a provision before the installation records nothing
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || strings.Contains(errOut, "recorded") {
		t.Errorf("provision before: code %d\n%s", code, errOut)
	}
	f.mu.Lock()
	f.machines["web-07"].boots = true
	f.mu.Unlock()
	f.waitInstalled("192.0.2.17")
	if out, _, _ := e.run("", false, "server", "list", "-o", "json"); !strings.Contains(out, `"status": "installed (unverified)"`) {
		t.Errorf("list:\n%s", out)
	}
	e.ansibleCalls = nil
	_, errOut, code = e.run("", false, "provision", "--yes")
	if code != 0 || !strings.Contains(errOut, "web-07 installed unattended: Ubuntu 26.04.1 LTS, hostname web-07, root on /dev/mapper/vg0-root; recorded.\n") {
		t.Fatalf("provision: code %d\n%s", code, errOut)
	}
	if !strings.Contains(e.readState(), "os: ubuntu-26.04") || armOf(t, "web-07") != nil || outcomeOf(t, "web-07") != nil {
		t.Errorf("not settled:\n%s", e.readState())
	}
	if runs := e.ansibleRuns(); len(runs) != 1 || !strings.Contains(e.hostFile("web-07.yml"), "ansible_host: 192.0.2.17\n") {
		t.Errorf("runs %v, host file:\n%s", runs, e.hostFile("web-07.yml"))
	}
	if pins := f.pinnedAt("192.0.2.17"); len(pins) == 0 || pins[len(pins)-1] != "192.0.2.17 "+newHostKey+"\n" {
		t.Errorf("pinned: %q", pins)
	}
}

// TestUnattendedStanding: a machine with mode: install is armed by
// apply, with a yes, once; the next ump provision records it.
func TestUnattendedStanding(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	f := startFakeRescue(t, e, web07(false))
	writePXE(t, strings.Replace(fleetPXE, "    site: fra1\n", "    site: fra1\n    mode: install\n    profile: ubuntu-26.04\n    group: [web]\n    disks: [nvme-eui.0025388b91c0a1b2]\n", 1))
	// without a terminal and --yes nothing is armed
	_, errOut, code := e.run("", false, "config", "pxe", "apply")
	if code != exitAborted || !strings.Contains(errOut, "  web-07: ubuntu-26.04 (profile ubuntu-26.04 from ") ||
		!strings.Contains(errOut, "as web-07; it may erase the disks named, nvme-eui.0025388b91c0a1b2\n") ||
		!strings.Contains(errOut, "ump: the installation of web-07 is not armed: that needs a yes, on a terminal or by ump config pxe apply --yes\n") {
		t.Fatalf("apply: code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a != nil {
		t.Fatalf("armed: %+v", a)
	}
	out, errOut, code := e.run("", false, "config", "pxe", "apply", "--yes")
	if code != 0 || !strings.Contains(out, "Armed the installation of web-07 (") {
		t.Fatalf("apply --yes: code %d\n%s\n%s", code, out, errOut)
	}
	a := armOf(t, "web-07")
	if a == nil || !a.Until.IsZero() || a.Disks.Rule != pxe.DisksPinned || !slices.Equal(a.Disks.Names, []string{"nvme-eui.0025388b91c0a1b2"}) {
		t.Fatalf("arm %+v", a)
	}
	// armed once: apply again changes nothing
	if out, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 || strings.Contains(out+errOut, "Standing") {
		t.Errorf("apply again: code %d\n%s\n%s", code, out, errOut)
	}
	if b := armOf(t, "web-07"); b == nil || b.ID != a.ID {
		t.Errorf("arm again %+v", b)
	}
	f.mu.Lock()
	f.machines["web-07"].boots = true
	f.mu.Unlock()
	f.waitInstalled("192.0.2.17")
	if d, _ := json.Marshal(f.spec("web-07")["_disks"]); string(d) != `{"names":["nvme-eui.0025388b91c0a1b2"],"rule":"pinned"}` {
		t.Errorf("disks %s", d)
	}
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || !strings.Contains(errOut, "web-07 installed unattended:") {
		t.Fatalf("provision: code %d\n%s", code, errOut)
	}
	st := e.readState()
	if !strings.Contains(st, "os: ubuntu-26.04") || !strings.Contains(st, "groups:\n            - web\n") && !strings.Contains(st, "- web") {
		t.Errorf("state:\n%s", st)
	}
	// done: listed so, and not armed again
	if out, _, _ := e.run("", false, "config", "pxe", "list"); !strings.Contains(out, "install (done)") {
		t.Errorf("list:\n%s", out)
	}
	if _, _, code := e.run("", false, "config", "pxe", "apply"); code != 0 || armOf(t, "web-07") != nil {
		t.Errorf("armed again: code %d", code)
	}
}

// TestUnattendedVoided: another installation takes the place of an
// armed one, and a recorded installation leaves no arm behind; one that
// a boot claimed refuses it.
func TestUnattendedVoided(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	startFakeRescue(t, e, web07(false))
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"); code != 0 {
		t.Fatalf("arm: code %d\n%s", code, errOut)
	}
	armed := armOf(t, "web-07")
	// its private addresses stay what it installs
	if _, errOut, code := e.run("", false, "server", "set", "pxe/web-07", "--private-ip", "10.0.0.9"); code != exitAborted ||
		!strings.Contains(errOut, "ump: pxe/web-07 has an unattended installation armed ("+pxe.ShortID(armed.ID)+", by ") {
		t.Errorf("set: code %d\n%s", code, errOut)
	}
	// a reinstall through the running system disarms it first
	e.directRoot = true
	e.onAsk = func() {}
	_, errOut, code := e.run("y\n", true, "server", "reinstall", "pxe/web-07", "--via", "direct", "--no-provision")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, " The unattended installation armed for it ("+pxe.ShortID(armed.ID)+", by ") || !strings.Contains(errOut, "Disarmed the unattended installation of web-07 ("+pxe.ShortID(armed.ID)) {
		t.Errorf("stderr:\n%s", errOut)
	}
	if a := armOf(t, "web-07"); a != nil {
		t.Errorf("still armed: %+v", a)
	}
	// an arm that comes during an installation goes with its record
	e.installHook = func() {
		a := *armed
		a.ID = "b7f3c2e1aaaabbbb"
		pxeStoreOf().PutArm(context.Background(), "fra1", &a, "")
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--via", "direct", "--no-provision", "--yes"); code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a != nil {
		t.Errorf("the arm stayed: %+v", a)
	}
	e.installHook = nil
	// multiinstall disarms one too, which its question says
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"); code != 0 {
		t.Fatalf("arm again: code %d\n%s", code, errOut)
	}
	again := armOf(t, "web-07")
	_, errOut, code = e.run("y\n", true, "server", "multiinstall", "pxe/web-07", "--via", "direct", "--no-provision")
	if code != 0 || !strings.Contains(errOut, " The unattended installation armed for pxe/web-07 ("+pxe.ShortID(again.ID)+", by ") ||
		!strings.Contains(errOut, "Disarmed the unattended installation of web-07 ("+pxe.ShortID(again.ID)) {
		t.Errorf("multiinstall: code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a != nil {
		t.Errorf("still armed after multiinstall: %+v", a)
	}
	// an arm that comes while the question waits, which it did not say:
	// refused, and left as it is
	e.onAsk = func() {
		a := *armed
		a.ID = "c7f3c2e1aaaabbbb"
		pxeStoreOf().PutArm(context.Background(), "fra1", &a, "")
	}
	_, errOut, code = e.run("y\n", true, "server", "reinstall", "pxe/web-07", "--via", "direct", "--no-provision")
	if code != exitAborted || !strings.Contains(errOut, "ump: pxe/web-07 got an unattended installation meanwhile (c7f3c2e1, by ") {
		t.Errorf("armed meanwhile: code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a == nil || a.ID != "c7f3c2e1aaaabbbb" {
		t.Errorf("the arm that came: %+v", a)
	}
	e.onAsk = func() {}
	pxeStoreOf().Forget(context.Background(), "fra1", "web-07")
	// one that installs: refused
	claimed := *armed
	pxeStoreOf().PutArm(context.Background(), "fra1", &claimed, "")
	pxeStoreOf().PutOutcome(context.Background(), "fra1", "web-07", &pxe.Outcome{Arm: claimed.ID, Phase: pxe.PhaseWriting, Daemon: "pxe1", Boot: "b1", At: e.now, Deadline: e.now.Add(time.Hour)}, "")
	for _, args := range [][]string{
		{"server", "reinstall", "pxe/web-07", "--via", "direct", "--no-provision", "--yes"},
		{"server", "reinstall", "pxe/web-07", "--unattended", "--no-provision", "--yes"},
		{"server", "multiinstall", "pxe/web-07", "--no-provision", "--yes"},
	} {
		_, errOut, code := e.run("", false, args...)
		if code != exitAborted || !strings.Contains(errOut, "pxe/web-07 installs unattended (since 2026-10-12 09:00 UTC, by pxe1): it ends with its report or at 2026-10-12 10:00 UTC; ump pxe status --log web-07 follows it\n") {
			t.Errorf("%v: code %d\n%s", args, code, errOut)
		}
	}
}

func TestUnattendedRefused(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	for _, c := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"--via", "provider"}, 2, "--unattended takes no --via: the machine boots its installation by itself"},
		{[]string{"--no-reboot"}, 2, "--unattended takes no --no-reboot"},
		{[]string{"--armed-for", "3h"}, 2, "--armed-for is how long the arm of --no-wait stands"},
		{[]string{"--no-wait", "--armed-for", "10s"}, 2, "--armed-for 10s: at least 1m"},
		{[]string{"--no-wait", "--hosts", "web"}, 2, "--hosts and --playbook narrow the provisioning after the installation, which --no-wait leaves to the next ump provision"},
		{[]string{"--disks", "sda"}, 2, "ump: pxe/web-07: the disk sda is not named by a stable name: an unattended installation takes names under /dev/disk/by-id"},
	} {
		_, errOut, code := e.run("", false, append([]string{"server", "reinstall", "pxe/web-07", "--unattended", "--yes"}, c.args...)...)
		if code != c.code || !strings.Contains(errOut, c.err) {
			t.Errorf("%v: code %d\n%s", c.args, code, errOut)
		}
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--no-wait", "--yes"); code != 2 || !strings.Contains(errOut, "--no-wait and --armed-for are for --unattended") {
		t.Errorf("--no-wait alone: code %d\n%s", code, errOut)
	}
	// --dry-run arms nothing
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--dry-run", "--no-provision")
	if code != 0 || !strings.Contains(errOut, "Dry run: web-07 would install ubuntu-26.04 (profile ubuntu-26.04 from ") || armOf(t, "web-07") != nil {
		t.Errorf("dry run: code %d\n%s", code, errOut)
	}
	// a provider without unattended installations
	e2 := newTestEnv(t)
	e2.write(testConfig)
	e2.writeProfiles()
	_, errOut, code = e2.run("", false, "server", "reinstall", "fake/1", "--unattended", "--yes", "--no-provision")
	if code != 2 || !strings.Contains(errOut, "--unattended: provider type fake has no unattended installations; without --unattended, ump installs fake/1 through a rescue system") {
		t.Errorf("fake: code %d\n%s", code, errOut)
	}
}

// TestUnattendedSeenDisks: without --disks, an installation may erase
// exactly the disks a boot of the machine reported.
func TestUnattendedSeenDisks(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	wwn, serial := "wwn:0x5000c500a1b2c3d4", "serial:S4EVNX0N123456"
	st := &pxe.Status{Daemon: "pxe0", Site: "fra1", Version: "v0.8.0", Started: e.now.Add(-2 * time.Hour), Updated: e.now.Add(-time.Hour),
		Machines: map[string]*pxe.Seen{"0c:c4:7a:aa:bb:01": {Name: "web-07", At: e.now.Add(-time.Hour), Answer: pxe.ModeRescue,
			Hello: &pxe.Hello{At: e.now.Add(-time.Hour), Rescue: "2026.11.01", GuardDisks: []pxe.GuardDisk{{Path: "/dev/nvme0n1", ID: &wwn}, {Path: "/dev/nvme1n1", ID: &serial}}}}}}
	if err := pxeStoreOf().PutStatus(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	f := startFakeRescue(t, e, web07(true))
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision")
	if code != 0 || !strings.Contains(errOut, "The installation on web-07 may erase the disks its network boot of 2026-10-12 08:00 UTC reported, /dev/nvme0n1 (wwn:0x5000c500a1b2c3d4), /dev/nvme1n1 (serial:S4EVNX0N123456).\n") {
		t.Fatalf("code %d\n%s", code, errOut)
	}
	if d, _ := json.Marshal(f.spec("web-07")["_disks"]); string(d) != `{"ids":["wwn:0x5000c500a1b2c3d4","serial:S4EVNX0N123456"],"rule":"seen"}` {
		t.Errorf("disks %s", d)
	}
}

// TestUnattendedMulti: multiinstall --unattended arms every server,
// waits for them side by side, and provisions them in one run.
func TestUnattendedMulti(t *testing.T) {
	two := fleetPXE + "  web-08:\n    site: fra1\n    mac: 0c:c4:7a:aa:bb:02\n    network:\n      ipv4: {address: 192.0.2.18/24}\n"
	e := setupUnattended(t, two)
	e.machines = map[string]*fakeMachine{}
	machines := web07(true)
	machines["web-08"] = &rescueMachine{mac: "0c:c4:7a:aa:bb:02", address: "192.0.2.51", system: "192.0.2.18", boots: true}
	startFakeRescue(t, e, machines)
	out, errOut, code := e.run("", false, "server", "multiinstall", "pxe/web-07", "pxe/web-08", "--unattended", "--yes")
	if code != 0 {
		t.Fatalf("multiinstall: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"[pxe/web-07] web-07 is up: Ubuntu 26.04.1 LTS",
		"[pxe/web-08] web-08 is up: Ubuntu 26.04.1 LTS",
		"Waiting for the installations of 2 servers ...\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"pxe/web-07", "web-07", "yes", "yes"},
		{"pxe/web-08", "web-08", "yes", "yes"},
	})
	if runs := e.ansibleRuns(); len(runs) != 1 {
		t.Errorf("runs %v", runs)
	}
	st := e.readState()
	if !strings.Contains(st, "pxe/web-07:") || !strings.Contains(st, "pxe/web-08:") || strings.Count(st, "os: ubuntu-26.04") != 2 {
		t.Errorf("state:\n%s", st)
	}
}

// TestUnattendedNoWaitFailed: the next ump provision gives the private
// addresses of a failed installation back, records nothing, and runs.
func TestUnattendedNoWaitFailed(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	machines := web07(true)
	machines["web-07"].exit = 1
	startFakeRescue(t, e, machines)
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"); code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	for deadline := time.Now().Add(10 * time.Second); outcomeOf(t, "web-07") == nil || outcomeOf(t, "web-07").Phase != pxe.PhaseFailed; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no outcome")
		}
	}
	if !strings.Contains(e.readState(), "10.0.0.") {
		t.Fatalf("not reserved:\n%s", e.readState())
	}
	_, errOut, code := e.run("", false, "provision", "--yes")
	if code != 0 || !strings.Contains(errOut, "ump: the installation of web-07 failed (exit status 1); nothing is recorded, and the machine stays in UMP's rescue system (ump pxe status --log web-07 has its log)\n") {
		t.Fatalf("provision: code %d\n%s", code, errOut)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.") || strings.Contains(st, "installed:") {
		t.Errorf("state:\n%s", st)
	}
	if len(e.ansibleRuns()) != 1 || outcomeOf(t, "web-07") == nil {
		t.Errorf("runs %v, outcome %+v", e.ansibleRuns(), outcomeOf(t, "web-07"))
	}
}

// TestUnattendedGiveBack: an armed installation that ends without a
// claim gives the private addresses it reserved back, whoever ends it:
// ump pxe disarm, its expiry, another installation that takes its
// place and does not write.
func TestUnattendedGiveBack(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	startFakeRescue(t, e, web07(false))
	reserved := func() bool { return strings.Contains(e.readState(), "10.0.0.") }
	arm := func(args ...string) *pxe.Arm {
		t.Helper()
		if _, errOut, code := e.run("", false, append([]string{"server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"}, args...)...); code != 0 || !reserved() {
			t.Fatalf("arm: code %d\n%s\n%s", code, errOut, e.readState())
		}
		return armOf(t, "web-07")
	}

	// ump pxe disarm
	a := arm()
	out, errOut, code := e.run("", false, "pxe", "disarm", "web-07")
	if code != 0 || !strings.Contains(out, "Disarmed web-07: its armed install ("+pxe.ShortID(a.ID)) {
		t.Fatalf("disarm: code %d\n%s\n%s", code, out, errOut)
	}
	if reserved() || armOf(t, "web-07") != nil {
		t.Errorf("after the disarm:\n%s", e.readState())
	}

	// its expiry: the next ump provision disarms it
	arm("--armed-for", "1m")
	a, v, _ := pxeStoreOf().Arm(context.Background(), "fra1", "web-07")
	a.Until = e.now.Add(-time.Second)
	if _, err := pxeStoreOf().PutArm(context.Background(), "fra1", a, v); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = e.run("", false, "provision", "--yes")
	if code != 0 || !strings.Contains(errOut, "The unattended installation of web-07 ("+pxe.ShortID(a.ID)+") expired before a boot claimed it: disarmed, and its private addresses given back.\n") {
		t.Fatalf("provision: code %d\n%s", code, errOut)
	}
	if reserved() || armOf(t, "web-07") != nil {
		t.Errorf("after the expiry:\n%s", e.readState())
	}
	if out, _, _ := e.run("", false, "server", "list", "-o", "json"); strings.Contains(out, "10.0.0.") {
		t.Errorf("list:\n%s", out)
	}

	// another installation takes its place and writes nothing: the
	// server gets back what it had before both
	arm()
	e.directRoot = true
	e.entryExit = 1
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--via", "direct", "--no-provision", "--yes"); code == 0 || !strings.Contains(errOut, "Disarmed the unattended installation of web-07") {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	if reserved() || armOf(t, "web-07") != nil {
		t.Errorf("after a failed reinstall in its place:\n%s", e.readState())
	}
}

// TestUnattendedFailedCleared: a failed installation gives its private
// addresses back once, is said at each ump provision with how to clear
// it, and ump pxe disarm clears it, with its outcome.
func TestUnattendedFailedCleared(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	machines := web07(true)
	machines["web-07"].exit = 1
	startFakeRescue(t, e, machines)
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"); code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	for deadline := time.Now().Add(10 * time.Second); outcomeOf(t, "web-07") == nil || outcomeOf(t, "web-07").Phase != pxe.PhaseFailed; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no outcome")
		}
	}
	id := pxe.ShortID(armOf(t, "web-07").ID)
	// a rescue cannot take its place
	if _, errOut, code := e.run("", false, "server", "rescue", "pxe/web-07", "--no-reboot", "--yes"); code == 0 || !strings.Contains(errOut, "the unattended installation of web-07 ("+id+") failed: ump pxe disarm web-07 clears it first") {
		t.Errorf("rescue: code %d\n%s", code, errOut)
	}
	if _, errOut, _ := e.run("", false, "provision", "--yes"); !strings.Contains(errOut, "ump: the installation of web-07 failed (exit status 1)") {
		t.Errorf("provision:\n%s", errOut)
	}
	_, errOut, code := e.run("", false, "provision", "--yes")
	if code != 0 || strings.Contains(errOut, "ump: the installation of web-07 failed") ||
		!strings.Contains(errOut, "ump: the unattended installation of pxe/web-07 ("+id+") failed at 2026-10-12 09:00 UTC: ump pxe status --log web-07 has its log, ump pxe disarm web-07 clears it\n") {
		t.Errorf("provision again: code %d\n%s", code, errOut)
	}
	out, errOut, code := e.run("", false, "pxe", "disarm", "web-07")
	if code != 0 || !strings.Contains(out, "Removed the arm of web-07 (install "+id+", by ") || !strings.Contains(out, "which failed, and its outcome.\n") {
		t.Fatalf("disarm: code %d\n%s\n%s", code, out, errOut)
	}
	if armOf(t, "web-07") != nil || outcomeOf(t, "web-07") != nil {
		t.Errorf("left: %+v %+v", armOf(t, "web-07"), outcomeOf(t, "web-07"))
	}
	if _, errOut, _ := e.run("", false, "provision", "--yes"); strings.Contains(errOut, "web-07") {
		t.Errorf("provision after the disarm:\n%s", errOut)
	}
	if out, _, _ := e.run("", false, "pxe", "disarm", "web-07"); out != "web-07 has no arm.\n" {
		t.Errorf("disarm again: %q", out)
	}
}

// TestUnattendedHostKeys: an installation that reported no host keys is
// not recorded; one that did has them in ~/.ssh/known_hosts for the
// provisioning.
func TestUnattendedHostKeys(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	f := startFakeRescue(t, e, web07(false))
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--no-wait", "--yes"); code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	a := armOf(t, "web-07")
	f.mu.Lock()
	f.installed["192.0.2.17"] = true
	f.mu.Unlock()
	exit := 0
	report := func(keys []string) {
		pxeStoreOf().Files.Delete(context.Background(), pxe.OutcomePath("fra1", "web-07"))
		pxeStoreOf().PutOutcome(context.Background(), "fra1", "web-07", &pxe.Outcome{Arm: a.ID, Phase: pxe.PhaseInstalled, Daemon: "pxe1", Boot: "b1", At: e.now, Exit: &exit, Written: true, HostKeys: keys}, "")
	}
	report(nil)
	_, errOut, _ := e.run("", false, "provision", "--yes")
	if !strings.Contains(errOut, "ump: the installation of web-07 reported no host keys of its new system, which ump checks it with; nothing is recorded") || strings.Contains(e.readState(), "installed:") {
		t.Fatalf("without host keys:\n%s", errOut)
	}
	report([]string{"ssh-ed25519 not-a-key!", newHostKey + " root@web-07"})
	_, errOut, code := e.run("", false, "provision", "--yes")
	if code != 0 || !strings.Contains(errOut, "web-07 installed unattended:") || !strings.Contains(errOut, "The host keys of the new system at 192.0.2.17 are in ~/.ssh/known_hosts now, in place of the old system's.\n") {
		t.Fatalf("provision: code %d\n%s", code, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")); string(data) != "192.0.2.17 "+newHostKey+"\n" {
		t.Errorf("known_hosts: %q", data)
	}
}

// TestUnattendedMultiAddresses: multiinstall --unattended reserves the
// private addresses of its servers together, as it planned them: one
// takes the address another gives up.
func TestUnattendedMultiAddresses(t *testing.T) {
	two := fleetPXE + "  web-08:\n    site: fra1\n    mac: 0c:c4:7a:aa:bb:02\n    network:\n      ipv4: {address: 192.0.2.18/24}\n"
	e := setupUnattended(t, two)
	machines := web07(false)
	machines["web-08"] = &rescueMachine{mac: "0c:c4:7a:aa:bb:02", address: "192.0.2.51", system: "192.0.2.18"}
	startFakeRescue(t, e, machines)
	store := &state.Store{Backend: &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}}
	if err := store.Update(context.Background(), func(d *state.Document) error {
		d.Servers["pxe/web-08"] = state.Server{Private: []state.Private{{Network: "default", CIDR: "10.0.0.0/24", Address: "10.0.0.5"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan := writePlan(t, "servers:\n  pxe/web-07:\n    private-ip: 10.0.0.5\n  pxe/web-08:\n    private-ip: 10.0.0.6\n")
	_, errOut, code := e.run("", false, "server", "multiinstall", "--plan", plan, "--unattended", "--no-wait", "--yes")
	if code != 0 {
		t.Fatalf("multiinstall: code %d\n%s", code, errOut)
	}
	if armOf(t, "web-07") == nil || armOf(t, "web-08") == nil {
		t.Errorf("arms %+v %+v", armOf(t, "web-07"), armOf(t, "web-08"))
	}
	st := e.readState()
	if i, j := strings.Index(st, "pxe/web-07:"), strings.Index(st, "pxe/web-08:"); i < 0 || j < i || !strings.Contains(st[i:j], "10.0.0.5") || !strings.Contains(st[j:], "10.0.0.6") {
		t.Errorf("state:\n%s", st)
	}
}

// TestUnattendedReplaced: an installation replaced while ump waits for
// it leaves the private addresses to the one that took its place.
func TestUnattendedReplaced(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	f := startFakeRescue(t, e, web07(false))
	const other = "d7f3c2e1aaaabbbb"
	e.remoteGate = func(addr string, argv []string) {
		f.gate(addr, argv)
		if a := armOf(t, "web-07"); a != nil && a.ID != other {
			b := *a
			b.ID = other
			pxeStoreOf().Disarm(context.Background(), "fra1", "web-07")
			pxeStoreOf().PutArm(context.Background(), "fra1", &b, "")
		}
	}
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--unattended", "--yes", "--no-provision")
	if code != 1 || !strings.Contains(errOut, "was disarmed or replaced before a boot claimed it") {
		t.Fatalf("code %d\n%s", code, errOut)
	}
	if a := armOf(t, "web-07"); a == nil || a.ID != other || !strings.Contains(e.readState(), "10.0.0.") {
		t.Errorf("arm %+v, state:\n%s", a, e.readState())
	}
}
