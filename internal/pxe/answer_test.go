package pxe

import (
	"net/url"
	"testing"
	"time"
)

type journal map[string]bool

func (j journal) Done(arm string) bool { return j[arm] }

func TestAnswer(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	rescue := &Arm{ID: "a1", Mode: ModeRescue, Until: now.Add(time.Minute)}
	install := &Arm{ID: "a2", Mode: ModeInstall}
	expired := &Arm{ID: "a3", Mode: ModeRescue, Until: now}
	for _, c := range []struct {
		name     string
		standing string
		a        *Arm
		o        *Outcome
		j        Journal
		want     string
	}{
		{"local", ModeLocal, nil, nil, nil, ModeLocal},
		{"standing rescue", ModeRescue, nil, nil, nil, ModeRescue},
		{"standing install is the arm's", ModeInstall, nil, nil, nil, ModeLocal},
		{"armed rescue", ModeLocal, rescue, nil, nil, ModeRescue},
		{"armed rescue, consumed", ModeLocal, rescue, &Outcome{Arm: "a1", Phase: PhaseBooted}, nil, ModeLocal},
		{"armed rescue, consumed by the journal", ModeLocal, rescue, nil, journal{"a1": true}, ModeLocal},
		{"armed rescue, another arm's outcome", ModeLocal, rescue, &Outcome{Arm: "a0", Phase: PhaseInstalled}, nil, ModeRescue},
		{"armed rescue, expired", ModeLocal, expired, nil, nil, ModeLocal},
		{"armed rescue, expired, standing rescue", ModeRescue, expired, nil, nil, ModeRescue},
		{"armed install without expiry", ModeInstall, install, nil, nil, ModeInstall},
		{"install being written", ModeLocal, install, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now.Add(time.Hour)}, nil, AnswerRescueBare},
		{"install being written, claimed here", ModeLocal, install, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now.Add(time.Hour)}, journal{"a2": true}, AnswerRescueBare},
		{"install being written, its arm gone", ModeRescue, nil, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now.Add(time.Hour)}, nil, AnswerRescueBare},
		{"install being written, another arm", ModeLocal, rescue, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now.Add(time.Hour)}, nil, AnswerRescueBare},
		{"install interrupted", ModeLocal, install, &Outcome{Arm: "a2", Phase: PhaseInterrupted}, nil, ModeLocal},
		{"install being written, deadline passed", ModeLocal, install, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now}, nil, ModeLocal},
		{"install written", ModeInstall, install, &Outcome{Arm: "a2", Phase: PhaseInstalled}, nil, ModeLocal},
		{"install failed, standing rescue", ModeRescue, install, &Outcome{Arm: "a2", Phase: PhaseFailed}, nil, ModeRescue},
		{"install claimed, journal only", ModeLocal, install, nil, journal{"a2": true}, ModeLocal},
	} {
		if got := Answer(c.standing, c.a, c.o, c.j, now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestStatusWords(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	rescue := &Arm{ID: "a1", Mode: ModeRescue, Until: now.Add(time.Minute)}
	install := &Arm{ID: "a2", Mode: ModeInstall}
	for _, c := range []struct {
		standing  string
		a         *Arm
		o         *Outcome
		installed bool
		want      string
	}{
		{ModeLocal, nil, nil, false, "local"},
		{"", nil, nil, false, "local"},
		{ModeRescue, nil, nil, true, "rescue"},
		{ModeLocal, rescue, nil, false, "armed rescue"},
		{ModeLocal, rescue, &Outcome{Arm: "a1", Phase: PhaseBooted}, false, "local"},
		{ModeInstall, install, nil, false, "armed install"},
		{ModeInstall, install, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now.Add(time.Hour)}, false, "installing"},
		{ModeInstall, install, &Outcome{Arm: "a2", Phase: PhaseInstalled}, false, "installed (unverified)"},
		{ModeInstall, install, &Outcome{Arm: "a2", Phase: PhaseFailed}, false, "failed"},
		{ModeInstall, install, &Outcome{Arm: "a2", Phase: PhaseInterrupted}, false, "interrupted"},
		{ModeLocal, nil, &Outcome{Arm: "a2", Phase: PhaseWriting, Deadline: now}, false, "interrupted"},
		{ModeInstall, nil, nil, false, "install (not armed)"},
		{ModeInstall, nil, nil, true, "install (done)"},
	} {
		if got := StatusWords(c.standing, c.a, c.o, c.installed, now); got != c.want {
			t.Errorf("%s %+v %+v %v: %q, want %q", c.standing, c.a, c.o, c.installed, got, c.want)
		}
	}
}

func TestClient(t *testing.T) {
	q, _ := url.ParseQuery("mac=0C:C4:7A:AA:BB:01&ip=192.0.2.17&buildarch=i386&platform=pcbios&uuid=x&mem=2048&lm=1")
	c, err := ParseClient(q)
	if err != nil || c.MAC != "0c:c4:7a:aa:bb:01" || c.IP != "192.0.2.17" || c.MemoryMB != 2048 || c.Arch() != ArchAMD64 || c.Firmware() != FirmwareBIOS {
		t.Errorf("%+v %v", c, err)
	}
	// what iPXE could not tell is empty
	q, _ = url.ParseQuery("mac=0c:c4:7a:aa:bb:01&ip=&buildarch=arm64&platform=efi&mem=&lm=")
	if c, err := ParseClient(q); err != nil || c.IP != "" || c.MemoryMB != 0 || c.Arch() != ArchARM64 || c.Firmware() != FirmwareUEFI {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := ParseClient(url.Values{"mac": {"nope"}}); err == nil {
		t.Error("a request without a MAC")
	}
	for _, x := range []struct{ buildarch, platform, arch string }{
		{"x86_64", "efi", ArchAMD64}, {"i386", "efi", ""}, {"arm32", "efi", ""}, {"riscv64", "efi", ""}, {"", "", ""},
	} {
		if got := (Client{BuildArch: x.buildarch, Platform: x.platform}).Arch(); got != x.arch {
			t.Errorf("%s on %s: %q", x.buildarch, x.platform, got)
		}
	}
}

func TestRefusal(t *testing.T) {
	m := &SiteMachine{Name: "web-07"}
	bios := Client{BuildArch: "i386", Platform: "pcbios", LongMode: "1", MemoryMB: 4096}
	efi := Client{BuildArch: "x86_64", Platform: "efi"}
	for _, c := range []struct {
		m    SiteMachine
		c    Client
		mode string
		want string
	}{
		{*m, bios, ModeRescue, ""},
		{*m, efi, ModeRescue, ""},
		{*m, Client{BuildArch: "i386", Platform: "pcbios"}, ModeRescue, ""},
		{*m, Client{BuildArch: "i386", Platform: "pcbios", LongMode: "0"}, ModeRescue, "the CPU has no 64-bit mode (long mode)"},
		{*m, Client{BuildArch: "i386", Platform: "efi"}, ModeRescue, "ump boots amd64 and arm64 machines, not i386 on efi"},
		{*m, Client{}, ModeRescue, "ump boots amd64 and arm64 machines, not an unknown platform on an unknown platform"},
		{SiteMachine{Arch: ArchARM64}, efi, ModeRescue, "pxe.yaml says arm64, and the machine boots as amd64"},
		{*m, Client{BuildArch: "x86_64", Platform: "efi", MemoryMB: 256}, ModeRescue, "256 MB of memory, and the rescue image needs 412 MB"},
		{SiteMachine{Firmware: FirmwareUEFI}, bios, ModeRescue, ""},
		{SiteMachine{Firmware: FirmwareUEFI}, bios, ModeInstall, "pxe.yaml says uefi, and the machine boots with bios"},
	} {
		if got := Refusal(&c.m, c.c, c.mode, 412); got != c.want {
			t.Errorf("%+v %+v %s: %q, want %q", c.m, c.c, c.mode, got, c.want)
		}
	}
}
