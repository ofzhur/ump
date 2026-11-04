package pxe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/provider"
	netboot "github.com/ofzhur/ump/internal/pxe"
)

const installSpec = `{"hostname": "web-07", "os": "debian-13", "storage": "https://storage.example.net"}`

func TestInstallDisks(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")

	// given: by stable names alone
	d, err := p.InstallDisks(ctx, web, []string{"/dev/disk/by-id/nvme-eui.0025388b91c0a1b2", "serial:S4EVNX0N123456"})
	if err != nil || d.Rule != provider.DisksPinned || strings.Join(d.Disks, ",") != "nvme-eui.0025388b91c0a1b2,serial:S4EVNX0N123456" ||
		d.Words != "the disks named, nvme-eui.0025388b91c0a1b2, serial:S4EVNX0N123456" {
		t.Errorf("pinned: %+v %v", d, err)
	}
	for disks, want := range map[string]string{
		"sda":                   "the disk sda is not named by a stable name",
		"/dev/nvme0n1":          "the disk /dev/nvme0n1 is not named by a stable name",
		"wwn-0x5,wwn-0x5":       "the disk wwn-0x5 is named twice",
		"/dev/disk/by-id/a/b-c": "the disk a/b-c is not named by a stable name",
	} {
		if _, err := p.InstallDisks(ctx, web, strings.Split(disks, ",")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", disks, err)
		}
	}

	// never seen: blank ones alone
	if d, err := p.InstallDisks(ctx, web, nil); err != nil || d.Rule != provider.DisksBlank || d.Words != "blank disks alone (no network boot of web-07 reported its disks)" {
		t.Errorf("never seen: %+v %v", d, err)
	}
	// seen: exactly those the last hello reported
	wwn, serial := "wwn:0x5000c500a1b2c3d4", "serial:S4EVNX0N123456"
	hello := func(at time.Time, disks ...netboot.GuardDisk) func(st *netboot.Status) {
		return func(st *netboot.Status) {
			st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: at, Answer: "rescue",
				Hello: &netboot.Hello{At: at, Firmware: "bios", GuardDisks: disks}}
		}
	}
	putStatusOf(t, h, "pxe0", hello(now.Add(-2*time.Hour), netboot.GuardDisk{Path: "/dev/sda", ID: &wwn}))
	putStatus(t, h, hello(now.Add(-time.Hour), netboot.GuardDisk{Path: "/dev/nvme0n1", ID: &wwn}, netboot.GuardDisk{Path: "/dev/nvme1n1", ID: &serial}))
	if d, err := p.InstallDisks(ctx, web, nil); err != nil || d.Rule != provider.DisksSeen || strings.Join(d.Disks, ",") != wwn+","+serial ||
		d.Words != "the disks its network boot of 2026-11-02 08:00 UTC reported, /dev/nvme0n1 (wwn:0x5000c500a1b2c3d4), /dev/nvme1n1 (serial:S4EVNX0N123456)" {
		t.Errorf("seen: %+v %v", d, err)
	}
	// a disk without an ID, or two alike: blank ones alone
	putStatus(t, h, hello(now, netboot.GuardDisk{Path: "/dev/nvme0n1", ID: &wwn}, netboot.GuardDisk{Path: "/dev/sdb"}))
	if d, _ := p.InstallDisks(ctx, web, nil); d.Rule != provider.DisksBlank || d.Words != "blank disks alone (/dev/sdb, seen at its last network boot, has neither a WWN nor a serial number)" {
		t.Errorf("no ID: %+v", d)
	}
	putStatus(t, h, hello(now, netboot.GuardDisk{Path: "/dev/nvme0n1", ID: &wwn}, netboot.GuardDisk{Path: "/dev/nvme1n1", ID: &wwn}))
	if d, _ := p.InstallDisks(ctx, web, nil); d.Rule != provider.DisksBlank || !strings.Contains(d.Words, "two disks seen at its last network boot have the ID wwn:0x5000c500a1b2c3d4") {
		t.Errorf("two alike: %+v", d)
	}
}

func TestArmInstall(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	putStatus(t, h, func(st *netboot.Status) {
		st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Answer: "local", Firmware: "uefi"}
	})
	until := now.Add(time.Hour)
	in := provider.InstallArm{Spec: json.RawMessage(installSpec), Keys: []provider.PublicKey{{Data: "ssh-ed25519 AAAAump ump"}},
		Disks: &provider.InstallDisks{Rule: provider.DisksSeen, Disks: []string{"wwn:0x5"}}, Until: until, Import: json.RawMessage(`{"hostname":"web-07"}`)}
	info, err := p.ArmInstall(ctx, web, in)
	if err != nil {
		t.Fatal(err)
	}
	if info.Until == nil || !info.Until.Equal(until) || info.Ready || info.Note != "PXE server pxe1 of site fra1 takes the arm at its next poll" {
		t.Errorf("info %+v", info)
	}
	a, _, err := h.Store.Arm(ctx, "fra1", "web-07")
	if err != nil || a == nil {
		t.Fatalf("arm: %v %v", a, err)
	}
	hash, _ := netboot.SpecHash(json.RawMessage(installSpec))
	if a.ID != info.ID || a.Mode != netboot.ModeInstall || a.By != "oleg@laptop" || a.SpecHash != hash || a.Firmware != "uefi" ||
		a.Disks.Rule != "seen" || strings.Join(a.Disks.IDs, ",") != "wwn:0x5" || strings.Join(a.Keys, ",") != "ssh-ed25519 AAAAump ump" || compact(a.Import) != `{"hostname":"web-07"}` {
		t.Errorf("arm %+v", a)
	}
	if sa, err := a.Installation(""); err != nil || sa.Firmware != "uefi" {
		t.Errorf("what it hands out: %+v %v", sa, err)
	}
	// one stands: refused
	if _, err := p.ArmInstall(ctx, web, in); err == nil || !strings.Contains(err.Error(), "web-07 has an armed install (") {
		t.Errorf("again: %v", err)
	}
	// a rescue may not take its place either
	if _, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err == nil || !strings.Contains(err.Error(), "has an armed install") {
		t.Errorf("rescue: %v", err)
	}
	// a disk rule that names nothing
	h.Store.Disarm(ctx, "fra1", "web-07")
	in.Disks = &provider.InstallDisks{Rule: provider.DisksPinned}
	if _, err := p.ArmInstall(ctx, web, in); err == nil || err.Error() != "the disk rule pinned names no disk" {
		t.Errorf("no disks: %v", err)
	}
	// with a wait: said once a server takes it
	in.Disks, in.Wait = &provider.InstallDisks{Rule: provider.DisksBlank}, true
	go func() {
		for i := 0; i < 1000; i++ {
			a, _, _ := h.Store.Arm(ctx, "fra1", "web-07")
			if a != nil {
				putStatus(t, h, func(st *netboot.Status) { st.Arms[a.ID] = &netboot.ArmState{Ready: true, At: now} })
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if info, err := p.ArmInstall(ctx, web, in); err != nil || !info.Ready || info.Note != "PXE server pxe1 of site fra1 is ready to boot it" {
		t.Errorf("ready: %+v %v", info, err)
	}
	// one whose outcome waits for ump keeps its arm, which ump reads it
	// from: installed, or failed
	a, _, _ = h.Store.Arm(ctx, "fra1", "web-07")
	in.Wait = false
	for phase, want := range map[string]string{
		netboot.PhaseInstalled: "web-07 installed unattended (" + netboot.ShortID(a.ID) + ") and waits to be recorded: ump provision records it first",
		netboot.PhaseFailed:    "the unattended installation of web-07 (" + netboot.ShortID(a.ID) + ") failed: ump pxe disarm web-07 clears it first",
	} {
		h.Store.Files.Delete(ctx, netboot.OutcomePath("fra1", "web-07"))
		h.Store.PutOutcome(ctx, "fra1", "web-07", &netboot.Outcome{Arm: a.ID, Phase: phase, At: now}, "")
		if _, err := p.ArmInstall(ctx, web, in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: install: %v", phase, err)
		}
		if _, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: rescue: %v", phase, err)
		}
	}
	// an interrupted one leaves nothing to ump
	h.Store.Files.Delete(ctx, netboot.OutcomePath("fra1", "web-07"))
	h.Store.PutOutcome(ctx, "fra1", "web-07", &netboot.Outcome{Arm: a.ID, Phase: netboot.PhaseInterrupted, At: now}, "")
	if _, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err != nil {
		t.Errorf("interrupted: rescue: %v", err)
	}
}

func TestInstallation(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	if st, err := p.Installation(ctx, web); st != nil || err != nil {
		t.Errorf("none: %+v %v", st, err)
	}
	// an armed rescue is none
	if _, err := p.EnableRescue(ctx, web, provider.RescueOptions{NoReboot: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.Installation(ctx, web); st != nil {
		t.Errorf("rescue: %+v", st)
	}
	h.Store.Disarm(ctx, "fra1", "web-07")
	arm := &netboot.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: netboot.ModeInstall, By: "oleg@laptop", Created: now, Until: now.Add(time.Hour), Import: json.RawMessage(`{"os":"debian-13"}`)}
	h.Store.PutArm(ctx, "fra1", arm, "")
	st, err := p.Installation(ctx, web)
	if err != nil || st == nil || !st.Pending || st.ID != arm.ID || st.By != "oleg@laptop" || st.Outcome != nil || compact(st.Import) != `{"os":"debian-13"}` || st.Claimed(now) {
		t.Errorf("armed: %+v %v", st, err)
	}
	// claimed: writing until its deadline, interrupted after it
	o := &netboot.Outcome{Arm: arm.ID, Phase: netboot.PhaseWriting, Daemon: "pxe1", Boot: "b1", At: now, Deadline: now.Add(2 * time.Hour)}
	h.Store.PutOutcome(ctx, "fra1", "web-07", o, "")
	if st, _ := p.Installation(ctx, web); st.Pending || !st.Claimed(now) || st.Outcome.By != "pxe1" {
		t.Errorf("claimed: %+v", st)
	}
	h.Now = func() time.Time { return now.Add(3 * time.Hour) }
	if st, _ := p.Installation(ctx, web); st.Pending || st.Claimed(now.Add(3*time.Hour)) || st.Outcome.Phase != provider.InstallInterrupted {
		t.Errorf("past its deadline: %+v %+v", st, st.Outcome)
	}
	h.Now = nil
	h.Now = func() time.Time { return now }
	// its outcome without the arm
	h.Store.Disarm(ctx, "fra1", "web-07")
	if st, _ := p.Installation(ctx, web); st == nil || st.ID != arm.ID || st.Import != nil || st.Outcome == nil {
		t.Errorf("without its arm: %+v", st)
	}
	// an outcome of another arm is not this one's
	other := *arm
	other.ID = "b7f3c2e1aaaabbbb"
	h.Store.PutArm(ctx, "fra1", &other, "")
	if st, _ := p.Installation(ctx, web); st.ID != other.ID || !st.Pending || st.Outcome != nil {
		t.Errorf("another arm: %+v", st)
	}
}

func TestDisarmInstall(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	arm := &netboot.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: netboot.ModeInstall, Created: now}
	put := func(phase string) {
		h.Store.PutArm(ctx, "fra1", arm, "")
		h.Store.Files.Delete(ctx, netboot.OutcomePath("fra1", "web-07"))
		if phase != "" {
			h.Store.PutOutcome(ctx, "fra1", "web-07", &netboot.Outcome{Arm: arm.ID, Phase: phase, At: now, Deadline: now.Add(time.Hour)}, "")
		}
	}
	gone := func() bool {
		a, _, _ := h.Store.Arm(ctx, "fra1", "web-07")
		o, _, _ := h.Store.Outcome(ctx, "fra1", "web-07")
		return a == nil && o == nil
	}
	for _, phase := range []string{"", netboot.PhaseInstalled, netboot.PhaseFailed} {
		put(phase)
		if err := p.Disarm(ctx, web, arm.ID); err != nil || !gone() {
			t.Errorf("%q: %v, gone %v", phase, err, gone())
		}
	}
	// one that writes stays
	put(netboot.PhaseWriting)
	if err := p.Disarm(ctx, web, arm.ID); !errors.Is(err, provider.ErrInstalling) || gone() {
		t.Errorf("writing: %v", err)
	}
	// another arm's: nothing
	put(netboot.PhaseFailed)
	if err := p.Disarm(ctx, web, "b7f3c2e1aaaabbbb"); err != nil || gone() {
		t.Errorf("another: %v", err)
	}
}

func TestWaitInstall(t *testing.T) {
	h := testHome(t)
	p := newProvider(t, h)
	ctx := context.Background()
	web := machineOf(t, p, "web-07")
	arm := &netboot.Arm{ID: "a7f3c2e1aaaabbbb", Machine: "web-07", Mode: netboot.ModeInstall, Created: now, Until: now.Add(time.Hour)}
	h.Store.PutArm(ctx, "fra1", arm, "")

	// a server boots it, its rescue system claims it and installs, then
	// reports: what they say, the output as it comes, the end of it
	progress := &lines{said: make(chan struct{}, 20)}
	go func() {
		wait := func(n int) {
			for range n {
				select {
				case <-progress.said:
				case <-time.After(10 * time.Second):
				}
			}
		}
		putStatus(t, h, func(st *netboot.Status) {
			st.Machines["0c:c4:7a:aa:bb:01"] = &netboot.Seen{Name: "web-07", At: now, Address: "192.0.2.50", Answer: "install", Arm: arm.ID}
		})
		wait(1)
		h.Store.PutOutcome(ctx, "fra1", "web-07", &netboot.Outcome{Arm: arm.ID, Phase: netboot.PhaseWriting, Daemon: "pxe1", Boot: "b1", At: now, Deadline: now.Add(2 * time.Hour)}, "")
		h.Store.Files.Write(ctx, netboot.LogPath("fra1", "web-07"), []byte("one\n"), "")
		wait(2)
		_, v, _ := h.Store.Files.Read(ctx, netboot.LogPath("fra1", "web-07"))
		h.Store.Files.Write(ctx, netboot.LogPath("fra1", "web-07"), []byte("one\ntwo\n"), v)
		wait(1)
		exit := 0
		h.Store.UpdateOutcome(ctx, "fra1", "web-07", func(o *netboot.Outcome) (*netboot.Outcome, error) {
			o.Phase, o.Exit, o.Written, o.HostKeys, o.LogTail = netboot.PhaseInstalled, &exit, true, []string{"ssh-ed25519 AAAAnew"}, "two\nthree\n"
			return o, nil
		})
	}()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	oc, err := p.WaitInstall(wctx, web, arm.ID, progress)
	if err != nil || oc.Phase != provider.InstallInstalled || !oc.Written || strings.Join(oc.HostKeys, ",") != "ssh-ed25519 AAAAnew" || oc.By != "pxe1" {
		t.Fatalf("outcome %+v %v", oc, err)
	}
	want := "web-07 boots its installation from PXE server pxe1 at 192.0.2.50.\n" +
		"one\n" +
		"PXE server pxe1 claimed the installation of web-07 for its boot b1; it installs until 2026-11-02 11:00 UTC at the latest.\n" +
		"two\n" +
		"three\n"
	if progress.String() != want {
		t.Errorf("progress:\n%s\nwant\n%s", progress.String(), want)
	}

	// the context ends after the claim: the outcome as it stands
	h.Store.UpdateOutcome(ctx, "fra1", "web-07", func(o *netboot.Outcome) (*netboot.Outcome, error) {
		o.Phase, o.Exit = netboot.PhaseWriting, nil
		return o, nil
	})
	wctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if oc, err := p.WaitInstall(wctx, web, arm.ID, progress); !errors.Is(err, context.DeadlineExceeded) || oc == nil || oc.Phase != provider.InstallWriting {
		t.Errorf("claimed: %+v %v", oc, err)
	}
	// before a claim: nothing
	h.Store.Files.Delete(ctx, netboot.OutcomePath("fra1", "web-07"))
	wctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if oc, err := p.WaitInstall(wctx, web, arm.ID, progress); !errors.Is(err, context.DeadlineExceeded) || oc != nil {
		t.Errorf("not claimed: %+v %v", oc, err)
	}
	// disarmed, or expired, before a claim
	h.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := p.WaitInstall(ctx, web, arm.ID, progress); err == nil || !strings.Contains(err.Error(), "the installation of web-07 (arm a7f3c2e1) expired before a boot claimed it") {
		t.Errorf("expired: %v", err)
	}
	h.Now = func() time.Time { return now }
	h.Store.Disarm(ctx, "fra1", "web-07")
	if _, err := p.WaitInstall(ctx, web, arm.ID, progress); err == nil || !strings.Contains(err.Error(), "was disarmed or replaced before a boot claimed it") {
		t.Errorf("disarmed: %v", err)
	}
}

// compact is a JSON document without its spaces.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	json.Compact(&b, raw)
	return b.String()
}

func TestNewPart(t *testing.T) {
	long := strings.Repeat("x", 600) + "end\n"
	for _, c := range []struct{ printed, data, want string }{
		{"", "one\n", "one\n"},
		{"one\n", "one\ntwo\n", "two\n"},
		{"one\ntwo\n", "one\ntwo\n", ""},
		{"one\ntwo\n", "two\nthree\n", "three\n"},
		{long, long[300:] + "more\n", "more\n"},
		{"one\ntwo\nthree\n", "two\n", ""},
		{"one\ntwo\n", "other\n", "other\n"},
	} {
		if got := string(newPart([]byte(c.printed), []byte(c.data))); got != c.want {
			t.Errorf("%q + %q: %q, want %q", c.printed, c.data, got, c.want)
		}
	}
	// a long output, as a server pushes it: the end of it alone, a window
	// that slides, with a line in front that says so
	var out strings.Builder
	for i := range 40000 {
		fmt.Fprintf(&out, "line %d\n", i)
	}
	full := out.String()
	window := func(end int) string {
		if end <= 100000 {
			return full[:end]
		}
		return fmt.Sprintf("[ump: the first %d KB are on the PXE server pxe1, /var/lib/ump-pxe/logs/web-07/b1.log]\n", (end-100000)/1000) + full[end-100000:end]
	}
	var printed, shown string
	for end := 30000; end <= len(full); end += 30000 {
		data := window(end)
		shown += string(newPart([]byte(printed), []byte(data)))
		printed = data
	}
	if want := full[:len(full)/30000*30000]; shown != want {
		t.Errorf("a sliding window: %d bytes shown, want %d", len(shown), len(want))
	}
}
