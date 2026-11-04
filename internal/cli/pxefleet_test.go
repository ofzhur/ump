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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/pxe"
)

// fleetPXE is a site behind a jump host with one machine, which has a
// static address.
const fleetPXE = `sites:
  fra1:
    jump: ump@198.51.100.7
    network:
      ipv4: {gateway: 192.0.2.1}
      dns: [192.0.2.53]
machines:
  web-07:
    site: fra1
    mac: 0c:c4:7a:aa:bb:01
    network:
      ipv4: {address: 192.0.2.17/24}
`

const rescueHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIRescueHostKeyOfWeb07"

// startFakePXE plays PXE server pxe1 of site fra1 for the CLI's network
// boots: every pending arm is ready as soon as it is written, and with
// boot the machine boots it at once, its rescue system reporting in at
// 192.0.2.50 with rescueHostKey; the fake ssh finds UMP's rescue system
// from then on. Its boots are the arms it booted.
func startFakePXE(t *testing.T, e *testEnv, boot bool) *[]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, up := make(chan struct{}), make(chan struct{})
	booted := &[]string{}
	store := pxeStoreOf()
	go func() {
		defer close(done)
		served, running := map[string]bool{}, false
		for ctx.Err() == nil {
			if running {
				time.Sleep(2 * time.Millisecond)
			}
			sn, err := store.Load(ctx, "fra1")
			if err != nil {
				continue
			}
			st := &pxe.Status{Daemon: "pxe1", Site: "fra1", Version: "v0.8.0", Started: e.now, Updated: e.now,
				Machines: map[string]*pxe.Seen{}, Arms: map[string]*pxe.ArmState{}}
			if d := sn.Sites["fra1"]; d != nil {
				st.SiteHash = d.Hash
			}
			for name, a := range sn.Arms["fra1"] {
				if !a.Pending(sn.Outcome("fra1", name), e.now) && !served[a.ID] {
					continue
				}
				st.Arms[a.ID] = &pxe.ArmState{Ready: true, At: e.now}
				if !boot {
					continue
				}
				st.Machines["0c:c4:7a:aa:bb:01"] = &pxe.Seen{Name: name, At: e.now, Address: "192.0.2.50", Answer: pxe.ModeRescue, Arm: a.ID,
					Hello: &pxe.Hello{Arm: a.ID, At: e.now, Rescue: "2026.11.01", BootMAC: "0c:c4:7a:aa:bb:01", HostKeys: []string{rescueHostKey},
						Interfaces: []pxe.Interface{{Name: "eno1", MAC: "0c:c4:7a:aa:bb:01", Up: true, Addresses: []string{"192.0.2.50/24"}}}}}
				if !served[a.ID] {
					served[a.ID] = true
					*booted = append(*booted, a.ID)
					store.UpdateOutcome(ctx, "fra1", name, func(*pxe.Outcome) (*pxe.Outcome, error) {
						return &pxe.Outcome{Arm: a.ID, Phase: pxe.PhaseBooted, Daemon: "pxe1", At: e.now}, nil
					})
					e.mu.Lock()
					e.inImage = true
					e.mu.Unlock()
				}
			}
			if store.PutStatus(ctx, st) == nil && !running {
				running = true
				close(up)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// it runs once its status is there
	<-up
	return booted
}

// pinned records the host keys each ssh run to addr pinned, by the
// file its command line names.
func pinned(e *testEnv, addr string) *[]string {
	keys := &[]string{}
	e.remoteGate = func(a string, argv []string) {
		if a != addr {
			return
		}
		for _, arg := range argv {
			if f, ok := strings.CutPrefix(arg, "UserKnownHostsFile="); ok && f != "/dev/null" {
				data, _ := os.ReadFile(f)
				*keys = append(*keys, string(data))
			}
		}
	}
	return keys
}

// hasOptions says that argv has each of the -o options.
func hasOptions(argv []string, opts ...string) bool {
	for _, o := range opts {
		i := slices.Index(argv, o)
		if i < 1 || argv[i-1] != "-o" {
			return false
		}
	}
	return true
}

func setupFleetPXE(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.write("")
	writePXE(t, fleetPXE)
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	oldPoll, oldReady := pxeRescuePoll, pxeReadyWait
	pxeRescuePoll, pxeReadyWait = 5*time.Millisecond, 10*time.Second
	t.Cleanup(func() { pxeRescuePoll, pxeReadyWait = oldPoll, oldReady })
	return e
}

func TestPXEServerRescue(t *testing.T) {
	e := setupFleetPXE(t)
	booted := startFakePXE(t, e, true)
	keys := pinned(e, "192.0.2.50")
	out, errOut, code := e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--", "uptime")
	if code != 0 {
		t.Fatalf("rescue: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"Checking root access to 192.0.2.17 over SSH ... no (tried root, ump).\n",
		"Arming a network boot of UMP's rescue system for pxe/web-07 (web-07, 192.0.2.17) ...\n",
		"Power-cycle web-07 now (or reset it): it boots UMP's rescue system at its next boot.\n",
		"Waiting for the rescue system of web-07 to report in (up to 15m) ...\n",
		"The rescue system of web-07 is up at 192.0.2.50 after ",
		"(ump-rescue 2026.10.12); its host keys are pinned for this run.\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	for _, want := range []string{
		"Rescue system armed for pxe/web-07 (web-07, 192.0.2.17); it boots at the next boot.\n",
		"Note: PXE server pxe1 of site fra1 is ready to boot it\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if len(*booted) != 1 {
		t.Fatalf("boots: %v", *booted)
	}
	// the arm: a rescue boot with UMP's key, for the wait
	a, _, err := pxeStoreOf().Arm(context.Background(), "fra1", "web-07")
	if err != nil || a == nil {
		t.Fatalf("arm: %v, %v", a, err)
	}
	if a.ID != (*booted)[0] || a.Mode != pxe.ModeRescue || len(a.Keys) != 1 || !strings.HasPrefix(a.Keys[0], "ssh-ed25519 ") || !a.Until.Equal(e.now.Add(16*time.Minute)) {
		t.Errorf("arm %+v", a)
	}
	// every ssh run goes through the jump host: the probes of the running
	// system, those of the rescue system with its host key pinned, and
	// the session
	for _, argv := range e.remoteCalls {
		if !hasOptions(argv, "ProxyJump=ump@198.51.100.7") {
			t.Errorf("no jump: %v", argv)
		}
	}
	if len(*keys) == 0 || (*keys)[0] != "192.0.2.50 "+rescueHostKey+"\n" {
		t.Errorf("pinned: %q", *keys)
	}
	session := e.sshCalls[len(e.sshCalls)-1]
	if !hasOptions(session, "ProxyJump=ump@198.51.100.7", "StrictHostKeyChecking=yes") || slices.Contains(session, "UserKnownHostsFile=/dev/null") ||
		session[len(session)-3] != "root@192.0.2.50" || session[len(session)-1] != "uptime" {
		t.Errorf("session %v", session)
	}

	// --no-connect prints what outlives the run: the jump, no pinned keys
	e.inImage = false
	out, errOut, code = e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--no-connect")
	if code != 0 || !strings.Contains(out, "Connect: ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o ProxyJump=ump@198.51.100.7 -i ") ||
		!strings.Contains(out, " -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@192.0.2.50\n") {
		t.Errorf("--no-connect: code %d\n%s\n%s", code, out, errOut)
	}

	// --via provider reboots a running system that lets UMP in
	e.inImage, e.directRoot = false, true
	e.remoteCalls = nil
	if out, errOut, code = e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--via", "provider", "--", "true"); code != 0 {
		t.Fatalf("--via provider: code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Checking root access to 192.0.2.17 over SSH, for the reboot ... yes; rebooting it as root.\n") || strings.Contains(errOut, "Power-cycle") || e.reboots != 1 {
		t.Errorf("reboot over SSH: %d reboots\n%s", e.reboots, errOut)
	}
}

func TestPXEServerRescueArmOnly(t *testing.T) {
	e := setupFleetPXE(t)
	startFakePXE(t, e, false)
	out, errOut, code := e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--no-reboot")
	if code != 0 {
		t.Fatalf("--no-reboot: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"Rescue system armed for pxe/web-07 (web-07, 192.0.2.17) (until 2026-10-13 09:00 UTC); it boots at the next boot.\n",
		"Power-cycle web-07 (or reset it) to boot it; ump pxe status web-07 then says where its rescue system is up, and ump server unrescue pxe/web-07 disarms it.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if out, _, code := e.run("", false, "server", "list", "-o", "json"); code != 0 || !strings.Contains(out, `"status": "armed rescue"`) {
		t.Errorf("list: code %d\n%s", code, out)
	}
	if out, errOut, code := e.run("", false, "server", "unrescue", "pxe/web-07"); code != 0 || !strings.Contains(out, "Rescue system disabled for pxe/web-07") {
		t.Errorf("unrescue: code %d\n%s\n%s", code, out, errOut)
	}
	if a, _, _ := pxeStoreOf().Arm(context.Background(), "fra1", "web-07"); a != nil {
		t.Errorf("still armed: %+v", a)
	}
}

func TestPXEServerRescueTimeout(t *testing.T) {
	e := setupFleetPXE(t)
	t.Setenv("UMP_WAIT", "100ms")
	out, errOut, code := e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--", "true")
	if code != exitUnreachable {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"Note: no PXE server of site fra1 runs (never heard of); one must, for the machine to boot the rescue system (ump pxe serve --site fra1)\n",
		"ump: account pxe: the rescue system of web-07 did not report in; the arm is gone (ump pxe status web-07 shows what the PXE servers saw): context deadline exceeded; a slow machine may need a longer wait",
	} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("lacks %q:\n%s\n%s", want, out, errOut)
		}
	}
	if a, _, _ := pxeStoreOf().Arm(context.Background(), "fra1", "web-07"); a != nil {
		t.Errorf("the arm stayed: %+v", a)
	}
}

func TestPXEServerReinstall(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	startFakePXE(t, e, true)
	keys := pinned(e, "192.0.2.50")
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"Power-cycle web-07 now (or reset it): it boots UMP's rescue system at its next boot.\n",
		"Installing ubuntu-26.04 (profile ubuntu-26.04 from ",
		"Rebooting web-07 into ubuntu-26.04 ...\n",
		"Waiting for SSH on 192.0.2.17 (up to 15m) ... up after ",
		"web-07 is up: Ubuntu 26.04.1 LTS, hostname web-07",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	// the installer ran in the rescue system, through the jump host, with
	// its host key pinned, and got the machine's uplink
	var install []string
	for _, argv := range e.remoteCalls {
		if strings.Contains(argv[len(argv)-1], "/install/install.sh") {
			install = argv
		}
	}
	if !hasOptions(install, "ProxyJump=ump@198.51.100.7", "StrictHostKeyChecking=yes") || install[len(install)-3] != "root@192.0.2.50" {
		t.Errorf("installer: %v", install)
	}
	if len(*keys) == 0 {
		t.Error("no host key pinned")
	}
	uplink, _ := json.Marshal(e.lastSpec()["network"].(map[string]any)["uplink"])
	if string(uplink) != `{"dns":["192.0.2.53"],"ipv4":{"address":"192.0.2.17/24","gateway":"192.0.2.1","mode":"static"},"ipv6":{"mode":"none"},"mac":"0c:c4:7a:aa:bb:01"}` {
		t.Errorf("uplink %s", uplink)
	}
	// the new system at the machine's own address: its old host key goes,
	// and the installation is recorded
	if !slices.Contains(e.forgotHosts, "192.0.2.17") || slices.Contains(e.forgotHosts, "192.0.2.50") {
		t.Errorf("forgot %v", e.forgotHosts)
	}
	if st := e.readState(); !strings.Contains(st, "pxe/web-07:") || !strings.Contains(st, "os: ubuntu-26.04") {
		t.Errorf("state:\n%s", st)
	}
	// Ansible goes through the jump host too
	hf := e.hostFile("web-07.yml")
	if !strings.Contains(hf, "ansible_host: 192.0.2.17\n  ansible_ssh_common_args: -o ProxyJump=ump@198.51.100.7 -o StrictHostKeyChecking=accept-new\n") {
		t.Errorf("host file:\n%s", hf)
	}
	out, _, code = e.run("", false, "server", "list", "-o", "ansible")
	if code != 0 || !strings.Contains(out, `"ansible_ssh_common_args": "-o ProxyJump=ump@198.51.100.7 -o StrictHostKeyChecking=accept-new"`) {
		t.Errorf("-o ansible: code %d\n%s", code, out)
	}

	// a failed installation leaves the rescue system where it reported in
	e.installExit = 1
	_, errOut, code = e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != 1 || !strings.Contains(errOut, "the server stays in the UMP rescue system (ssh -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o ProxyJump=ump@198.51.100.7 -i ") ||
		!strings.Contains(errOut, "-o StrictHostKeyChecking=no -o LogLevel=ERROR root@192.0.2.50)\n") {
		t.Errorf("failed: code %d\n%s", code, errOut)
	}
}

// TestPXEServerReinstallReserved: the DHCP server's reservation is the
// address of the rescue system and of the new system alike, whose host
// key is not the rescue system's.
func TestPXEServerReinstallReserved(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	writePXE(t, strings.Replace(fleetPXE, "      ipv4: {address: 192.0.2.17/24}\n", "      ipv4: dhcp\n    address: 192.0.2.50\n", 1))
	startFakePXE(t, e, true)
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s\n%s", code, out, errOut)
	}
	var check []string
	for _, argv := range e.remoteCalls {
		if strings.HasPrefix(argv[len(argv)-1], "sed -n 's/^PRETTY_NAME=//p'") {
			check = argv
		}
	}
	if !hasOptions(check, "ProxyJump=ump@198.51.100.7", "UserKnownHostsFile=/dev/null") || check[len(check)-3] != "ump@192.0.2.50" {
		t.Errorf("the check of the new system: %v", check)
	}
	if spec := e.lastSpec()["network"].(map[string]any)["uplink"].(map[string]any); spec["ipv4"].(map[string]any)["mode"] != "dhcp" || spec["dns"].([]any)[0] != "192.0.2.53" {
		t.Errorf("uplink %v", spec)
	}
}

// TestPXEServerReinstallHostname: pxe.yaml's hostname names the new
// system as it names the rescue system; --hostname still wins.
func TestPXEServerReinstallHostname(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	writePXE(t, strings.Replace(fleetPXE, "    site: fra1\n", "    site: fra1\n    hostname: db-primary\n", 1))
	startFakePXE(t, e, true)
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != 0 || e.lastSpec()["hostname"] != "db-primary" || !strings.Contains(e.readState(), "hostname: db-primary") {
		t.Fatalf("reinstall: code %d, hostname %v\n%s\n%s", code, e.lastSpec()["hostname"], out, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision", "--hostname", "web-07b"); code != 0 || e.lastSpec()["hostname"] != "web-07b" {
		t.Errorf("--hostname: code %d, hostname %v\n%s", code, e.lastSpec()["hostname"], errOut)
	}
}

func TestPXEServerUplinkOverlaps(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	writePXE(t, strings.ReplaceAll(fleetPXE, "192.0.2.", "10.0.0."))
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != exitUsage || !strings.Contains(errOut, "ump: pxe/web-07: network.uplink: address 10.0.0.17/24 overlaps the private network default (10.0.0.0/24); the uplink needs a network of its own\n") {
		t.Errorf("code %d\n%s", code, errOut)
	}
	if a, _, _ := pxeStoreOf().Arm(context.Background(), "fra1", "web-07"); a != nil {
		t.Errorf("armed: %+v", a)
	}
}

// TestPXEServerReinstallStandingRescue: a machine that boots the rescue
// system at every boot would never start a new system, so reinstall
// refuses before it asks or arms anything.
func TestPXEServerReinstallStandingRescue(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	writePXE(t, strings.Replace(fleetPXE, "    site: fra1\n", "    site: fra1\n    mode: rescue\n", 1))
	_, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--yes", "--no-provision")
	if code != exitAborted || !strings.Contains(errOut, "ump: pxe/web-07 boots UMP's rescue system at every boot (mode: rescue in pxe.yaml), so a new system would never start: set mode: local first (ump config pxe edit web-07)\n") {
		t.Errorf("code %d\n%s", code, errOut)
	}
	if a, _, _ := pxeStoreOf().Arm(context.Background(), "fra1", "web-07"); a != nil {
		t.Errorf("armed: %+v", a)
	}
}

func TestPXEInFleet(t *testing.T) {
	e := newTestEnv(t)
	e.write(goneConfig)
	// the site's jump is a server of the fleet: its administrator at its address
	writePXE(t, `sites:
  fra1:
    jump: web-01
  lab:
    jump: ops@[2001:db8::7]:2222,root@lab-gw
machines:
  web-07:
    site: fra1
    mac: 0c:c4:7a:aa:bb:01
    network:
      ipv4: dhcp
    address: 192.0.2.17
  lab-01:
    site: lab
    mac: 0c:c4:7a:aa:bb:20
    network:
      ipv4: dhcp
`)
	out, errOut, code := e.run("", false, "server", "list", "-o", "wide")
	if code != 0 {
		t.Fatalf("list: code %d\n%s", code, errOut)
	}
	rows := fields(out)
	var pxeRows []string
	for _, r := range rows {
		if r[1] == "pxe" {
			pxeRows = append(pxeRows, strings.Join(r[:8], " "))
		}
	}
	if strings.Join(pxeRows, "\n") != "pxe pxe lab-01 lab-01 <none> <none> lab local\npxe pxe web-07 web-07 192.0.2.17 <none> fra1 local" {
		t.Errorf("pxe machines in the listing:\n%s", out)
	}
	var ms []struct {
		Ref  string `json:"ref"`
		ID   string `json:"id"`
		Jump string `json:"jump"`
		IPv4 string `json:"ipv4"`
	}
	out, _, _ = e.run("", false, "server", "list", "-o", "json")
	json.Unmarshal([]byte(out), &ms)
	jumps := map[string]string{}
	for _, m := range ms {
		jumps[m.ID] = m.Jump
	}
	if jumps["web-07"] != "ump@203.0.113.10" || jumps["lab-01"] != "ops@[2001:db8::7]:2222,root@lab-gw" || jumps["1"] != "" {
		t.Errorf("jumps %v", jumps)
	}

	// ump ssh goes through it
	if _, errOut, code := e.run("", false, "ssh", "pxe/web-07", "--", "true"); code != 0 {
		t.Fatalf("ssh: code %d, %s", code, errOut)
	}
	argv := e.sshCalls[len(e.sshCalls)-1]
	if !hasOptions(argv, "ProxyJump=ump@203.0.113.10") || argv[len(argv)-3] != "ump@192.0.2.17" {
		t.Errorf("ssh %v", argv)
	}
	// a machine ump has no address for: no ssh, no inventory
	if _, errOut, code := e.run("", false, "ssh", "pxe/lab-01", "--", "true"); code != 1 || !strings.Contains(errOut, "pxe/lab-01 has no address ump reaches it at: give it address: (the DHCP server's reservation for it) or a static network.ipv4 in pxe.yaml (ump config pxe edit lab-01)") {
		t.Errorf("ssh without an address: code %d, %s", code, errOut)
	}
	if !strings.Contains(e.runShow(t, "pxe/web-07"), "Jump:      ump@203.0.113.10\n") {
		t.Errorf("show lacks the jump")
	}

	// installed by ump: in the inventory, through the jump; the one without an address left out
	e.writeState("servers:\n  pxe/web-07:\n    installed: {os: debian-13, profile: debian-13, hostname: web-07, admin: ump, at: 2026-10-01T10:00:00Z}\n  pxe/lab-01:\n    installed: {os: debian-13, profile: debian-13, hostname: lab-01, admin: ump, at: 2026-10-01T10:00:00Z}\n")
	out, errOut, code = e.run("", false, "server", "list", "-o", "ansible")
	if code != 0 || !strings.Contains(out, `"ansible_ssh_common_args": "-o ProxyJump=ump@203.0.113.10 -o StrictHostKeyChecking=accept-new"`) || strings.Contains(out, "lab-01") {
		t.Errorf("-o ansible: code %d\n%s", code, out)
	}
	if !strings.Contains(errOut, "ump: pxe/lab-01 (lab-01) is left out of the inventory: pxe.yaml gives it no address (address: or a static network.ipv4), and ump takes none it was given at boot\n") {
		t.Errorf("left out: %s", errOut)
	}

	// pxe.yaml lists the machines, never the listing cache, and a machine
	// gone from it is not forgotten by a listing (ump config pxe delete does that)
	writePXE(t, "machines:\n  web-08:\n    mac: 0c:c4:7a:aa:bb:08\n")
	out, _, _ = e.run("", false, "server", "list", "-o", "name")
	if !strings.Contains(out, "pxe/web-08\n") || strings.Contains(out, "pxe/web-07") {
		t.Errorf("listing after the edit:\n%s", out)
	}
	if st := e.readState(); !strings.Contains(st, "pxe/web-07:") {
		t.Errorf("a machine gone from pxe.yaml was forgotten:\n%s", st)
	}

	// a pxe.yaml that is not valid: the account says why
	writePXE(t, "machines:\n  web-08:\n    mac: nope\n")
	_, errOut, code = e.run("", false, "server", "list")
	if code != exitProvider || !strings.Contains(errOut, "ump: account pxe: ") || !strings.Contains(errOut, "pxe.yaml: line 3") {
		t.Errorf("invalid pxe.yaml: code %d, %s", code, errOut)
	}
}

// runShow is ump server show REF, which must succeed.
func (e *testEnv) runShow(t *testing.T, ref string) string {
	t.Helper()
	out, errOut, code := e.run("", false, "server", "show", ref)
	if code != 0 {
		t.Fatalf("show %s: code %d, %s", ref, code, errOut)
	}
	return out
}

// TestJumpSharedAddress: a private address behind a site's jump host
// that a server of another network also has goes the way of the server
// ump acts on, never through the other's jump host.
func TestJumpSharedAddress(t *testing.T) {
	e := newTestEnv(t)
	e.write("")
	writePXE(t, fleetPXE)
	e.writeState("manual:\n  office-nas: {ipv4: 192.0.2.17}\n")
	for _, c := range []struct{ ref, jump string }{{"manual/office-nas", ""}, {"pxe/web-07", "ProxyJump=ump@198.51.100.7"}, {"office-nas", ""}} {
		if _, errOut, code := e.run("", false, "ssh", c.ref, "--", "true"); code != 0 {
			t.Fatalf("ssh %s: code %d, %s", c.ref, code, errOut)
		}
		argv := e.sshCalls[len(e.sshCalls)-1]
		jumps := slices.ContainsFunc(argv, func(s string) bool { return strings.HasPrefix(s, "ProxyJump=") })
		if c.jump == "" && jumps || c.jump != "" && !hasOptions(argv, c.jump) || !strings.HasSuffix(argv[len(argv)-3], "@192.0.2.17") {
			t.Errorf("ssh %s: %v", c.ref, argv)
		}
	}

	// one run acts on one of them
	a := &app{}
	ms := []provider.Machine{
		{Provider: "manual", Account: "manual", ID: "office-nas", Name: "office-nas", IPv4: "192.0.2.17"},
		{Provider: "pxe", Account: "pxe", ID: "web-07", Name: "web-07", IPv4: "192.0.2.17", Jump: "ump@198.51.100.7"},
		{Provider: "pxe", Account: "pxe", ID: "web-08", Name: "web-08", IPv4: "192.0.2.18", Jump: "ump@198.51.100.7"},
	}
	a.resolveJumps(nil, nil, []listResult{{machines: ms}})
	if r := a.route("192.0.2.17"); r.jump != "" {
		t.Errorf("the shared address goes %+v before a server is picked", r)
	}
	if r := a.route("192.0.2.18"); r.jump != "ump@198.51.100.7" {
		t.Errorf("the address of web-08 goes %+v", r)
	}
	if err := a.reach(ms[1]); err != nil || a.route("192.0.2.17").jump != "ump@198.51.100.7" {
		t.Errorf("reach web-07: %v, %+v", err, a.route("192.0.2.17"))
	}
	err := a.reach(ms[0])
	if err == nil || err.Error() != "pxe/web-07 and manual/office-nas share the address 192.0.2.17, but ump reaches them through ump@198.51.100.7 and directly: act on one of them per run" {
		t.Errorf("reach office-nas after web-07: %v", err)
	}
	if a.route("192.0.2.17").jump != "ump@198.51.100.7" {
		t.Errorf("the refused server changed the route")
	}
	if err := a.reach(ms[2]); err != nil {
		t.Errorf("reach web-08: %v", err)
	}
}

func TestJumpHops(t *testing.T) {
	for _, c := range []struct {
		hop, user, host, port string
	}{
		{"pxe1", "", "pxe1", ""},
		{"ump@198.51.100.7", "ump", "198.51.100.7", ""},
		{"ops@jump.example.com:2222", "ops", "jump.example.com", "2222"},
		{"[2001:db8::7]:22", "", "2001:db8::7", "22"},
		{"ump@[2001:db8::7]", "ump", "2001:db8::7", ""},
	} {
		if u, h, p := splitHop(c.hop); u != c.user || h != c.host || p != c.port {
			t.Errorf("%s: %q %q %q", c.hop, u, h, p)
		}
	}
	for _, c := range []struct {
		jump, addr, want string
	}{
		{"ump@198.51.100.7", "192.0.2.17", "ssh -o BatchMode=yes -o ConnectTimeout=10 -W 192.0.2.17:22 ump@198.51.100.7"},
		{"ops@gw:2222", "2001:db8::17", "ssh -o BatchMode=yes -o ConnectTimeout=10 -p 2222 -W [2001:db8::17]:22 ops@gw"},
		{"a@outer,b@inner", "192.0.2.17", "ssh -o BatchMode=yes -o ConnectTimeout=10 -o ProxyJump=a@outer -W 192.0.2.17:22 b@inner"},
	} {
		if got := strings.Join(jumpProbe(c.jump, c.addr), " "); got != c.want {
			t.Errorf("%s %s: %s", c.jump, c.addr, got)
		}
	}
}

// TestPXERescueWithServer runs ump server rescue against the PXE server
// itself (pxe.Server), with the machine played over its HTTP side as
// iPXE and the rescue system would: once the server takes the arm, the
// machine asks for its script, loads the kernel and the initrd, and its
// rescue system says hello with the token of the boot config. The CLI
// connects where the hello said, with its host key pinned.
func TestPXERescueWithServer(t *testing.T) {
	e := newTestEnv(t)
	storage := rescueStorage(t)
	e.write("staging:\n  image: " + storage.URL + "/rescue\n")
	writePXE(t, fleetPXE)
	if _, errOut, code := e.run("", false, "config", "pxe", "apply"); code != 0 {
		t.Fatalf("apply: code %d, %q", code, errOut)
	}
	oldPoll, oldReady := pxeRescuePoll, pxeReadyWait
	pxeRescuePoll, pxeReadyWait = 5*time.Millisecond, 10*time.Second
	t.Cleanup(func() { pxeRescuePoll, pxeReadyWait = oldPoll, oldReady })

	ctx, cancel := context.WithCancel(context.Background())
	srv := &pxe.Server{Site: "fra1", Name: "pxe1", Version: "v0.8.0", Store: pxeStoreOf(), Cache: &pxe.Cache{Dir: t.TempDir(), Client: localOnlyClient},
		Data: t.TempDir(), Now: func() time.Time { return e.now }}
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
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	get := func(url string) []byte {
		resp, err := http.Get(url)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	// the machine: it boots the rescue system once its script says so
	boot := func() error {
		script := string(get(hs.URL + "/ipxe?mac=0c:c4:7a:aa:bb:01&ip=192.0.2.50&buildarch=x86_64&platform=efi&mem=65536"))
		m := regexp.MustCompile(`initrd --name initrd (\S+)`).FindStringSubmatch(script)
		if m == nil {
			return fmt.Errorf("no rescue: %s", script)
		}
		files, err := pxe.ReadCpio(get(m[1]))
		if err != nil {
			return err
		}
		token := ""
		for _, f := range files {
			if f.Name == "ump/token" {
				token = strings.TrimSpace(string(f.Data))
			}
		}
		hello, _ := json.Marshal(pxe.Hello{Rescue: "2026.11.01", Arch: "amd64", Firmware: "uefi", BootMAC: "0c:c4:7a:aa:bb:01", HostKeys: []string{rescueHostKey},
			Interfaces: []pxe.Interface{{Name: "eno1", MAC: "0c:c4:7a:aa:bb:01", Up: true, Addresses: []string{"192.0.2.50/24"}}}})
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/hello", bytes.NewReader(hello))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("hello: %s", resp.Status)
		}
		e.mu.Lock()
		e.inImage = true
		e.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		booted := false
		for ctx.Err() == nil {
			srv.Load(ctx)
			if a, _, _ := pxeStoreOf().Arm(ctx, "fra1", "web-07"); a != nil && !booted {
				booted = boot() == nil
			}
			srv.Flush(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer func() {
		cancel()
		<-done
	}()

	keys := pinned(e, "192.0.2.50")
	out, errOut, code := e.run("", false, "server", "rescue", "pxe/web-07", "--yes", "--", "uptime")
	if code != 0 {
		t.Fatalf("rescue: code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Note: PXE server pxe1 of site fra1 is ready to boot it\n") || !strings.Contains(errOut, "The rescue system of web-07 is up at 192.0.2.50 after ") {
		t.Errorf("rescue:\n%s\n%s", out, errOut)
	}
	if len(*keys) == 0 || (*keys)[0] != "192.0.2.50 "+rescueHostKey+"\n" {
		t.Errorf("pinned: %q", *keys)
	}
	session := e.sshCalls[len(e.sshCalls)-1]
	if !hasOptions(session, "ProxyJump=ump@198.51.100.7", "StrictHostKeyChecking=yes") || session[len(session)-3] != "root@192.0.2.50" {
		t.Errorf("session %v", session)
	}
	// the server consumed the arm: the next boot is the disk's
	if script := string(get(hs.URL + "/ipxe?mac=0c:c4:7a:aa:bb:01&buildarch=x86_64&platform=efi")); !strings.Contains(script, "echo ump: web-07: local\n") {
		t.Errorf("after the rescue boot:\n%s", script)
	}
}
