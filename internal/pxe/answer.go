package pxe

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AnswerRescueBare is the answer to a machine whose installation is
// being written and that booted again: the rescue system with UMP's key
// and no token, never a second installation and never the half-written
// disk.
const AnswerRescueBare = "rescue-bare"

// Journal is a server's own record of the arms it consumed or claimed,
// which holds while the backend does not answer.
type Journal interface {
	Done(arm string) bool
}

// Answer is what a known machine gets at /ipxe: standing is its mode in
// site.json, a and o its arm and outcome (nil: none), j the server's
// journal (nil: empty). An arm boots once: a rescue until its boot was
// served (an outcome of it), an installation until its outcome; a
// machine whose installation is being written gets the bare rescue
// until the deadline. Otherwise the standing mode: the rescue system,
// or the disk (local, also for a standing install, which an arm
// carries).
func Answer(standing string, a *Arm, o *Outcome, j Journal, now time.Time) string {
	if a != nil && (j == nil || !j.Done(a.ID)) {
		mine := o != nil && o.Arm == a.ID
		switch {
		case mine && o.Phase == PhaseWriting && now.Before(o.Deadline):
			return AnswerRescueBare
		case !mine && !a.Expired(now):
			return a.Mode
		}
	}
	if standing == ModeRescue {
		return ModeRescue
	}
	return ModeLocal
}

// Client is what iPXE says of a machine when it asks for its script
// (boot.ipxe passes it): the MAC and address of the interface it booted
// from, the build architecture and platform of the iPXE binary, the
// SMBIOS UUID, the memory in MB and whether an x86 BIOS machine has a
// 64-bit CPU (long mode). Values iPXE could not tell are empty or 0.
type Client struct {
	MAC       string
	IP        string
	BuildArch string
	Platform  string
	UUID      string
	MemoryMB  int
	LongMode  string
}

// ParseClient reads the query of a script request; the MAC is required.
func ParseClient(q url.Values) (Client, error) {
	mac, err := NormalizeMAC(q.Get("mac"))
	if err != nil {
		return Client{}, fmt.Errorf("mac: %w", err)
	}
	c := Client{MAC: mac, BuildArch: q.Get("buildarch"), Platform: q.Get("platform"), UUID: q.Get("uuid"), LongMode: q.Get("lm")}
	if a, err := netip.ParseAddr(q.Get("ip")); err == nil {
		c.IP = a.String()
	}
	if n, err := strconv.Atoi(q.Get("mem")); err == nil && n > 0 {
		c.MemoryMB = n
	}
	return c, nil
}

// Firmware is how the machine booted: uefi, bios, or empty when iPXE
// did not say.
func (c Client) Firmware() string {
	switch c.Platform {
	case "efi":
		return FirmwareUEFI
	case "pcbios":
		return FirmwareBIOS
	}
	return ""
}

// Arch is the architecture the machine runs the rescue system in: amd64
// for 64-bit x86 (a BIOS machine's iPXE says i386 even on a 64-bit CPU),
// arm64, or empty for what ump does not boot.
func (c Client) Arch() string {
	switch {
	case c.Platform == "pcbios":
		return ArchAMD64
	case c.Platform == "efi" && c.BuildArch == "x86_64":
		return ArchAMD64
	case c.Platform == "efi" && c.BuildArch == "arm64":
		return ArchARM64
	}
	return ""
}

// Refusal says why a machine is not booted into the rescue system or an
// installation (mode) although it would be: a platform ump does not
// boot, another architecture than pxe.yaml says, an x86 BIOS machine
// without a 64-bit CPU, too little memory for the image (needMB, the
// manifest's memory_mb), or for an installation another firmware than
// pxe.yaml says. Empty when nothing stands in the way; what iPXE could
// not tell stands in no way.
func Refusal(m *SiteMachine, c Client, mode string, needMB int) string {
	arch := c.Arch()
	switch {
	case arch == "":
		return fmt.Sprintf("ump boots amd64 and arm64 machines, not %s on %s", orUnknown(c.BuildArch), orUnknown(c.Platform))
	case m.Arch != "" && m.Arch != arch:
		return fmt.Sprintf("pxe.yaml says %s, and the machine boots as %s", m.Arch, arch)
	case c.Platform == "pcbios" && c.LongMode == "0":
		return "the CPU has no 64-bit mode (long mode)"
	case needMB > 0 && c.MemoryMB > 0 && c.MemoryMB < needMB:
		return fmt.Sprintf("%d MB of memory, and the rescue image needs %d MB", c.MemoryMB, needMB)
	case mode == ModeInstall && m.Firmware != "" && c.Firmware() != "" && m.Firmware != c.Firmware():
		return fmt.Sprintf("pxe.yaml says %s, and the machine boots with %s", m.Firmware, c.Firmware())
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown platform"
	}
	return s
}

// StatusWords is a machine's status in words, as ump server list shows
// it: from its standing mode, its arm and outcome (nil: none), and
// whether ump has an installation of it in the state.
func StatusWords(standing string, a *Arm, o *Outcome, installed bool, now time.Time) string {
	if a != nil && a.Pending(o, now) {
		if a.Mode == ModeInstall && o != nil && o.Arm == a.ID && o.Phase == PhaseWriting {
			return "installing"
		}
		return "armed " + a.Mode
	}
	if o != nil && (a == nil || o.Arm == a.ID) {
		switch o.Phase {
		case PhaseInstalled:
			return "installed (unverified)"
		case PhaseFailed, PhaseInterrupted:
			return o.Phase
		case PhaseWriting:
			if now.Before(o.Deadline) {
				return "installing"
			}
			return PhaseInterrupted
		}
	}
	switch {
	case standing == ModeInstall && installed:
		return "install (done)"
	case standing == ModeInstall:
		return "install (not armed)"
	case standing == "":
		return ModeLocal
	}
	return strings.ToLower(standing)
}
