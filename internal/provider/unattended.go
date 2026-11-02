package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// An unattended installation: the machine installs a specification by
// itself at its next boot, without UMP's SSH session (PXE: a network
// boot of UMP's rescue system claims it from the PXE server of its
// site). The caller renders and confirms the specification as for any
// installation; the provider hands it to one boot and reports what came
// of it, the outcome, which the caller records once it reached the new
// system.

// The rules of InstallDisks.
const (
	// DisksPinned: the disks named, by stable names.
	DisksPinned = "pinned"
	// DisksSeen: exactly the disks a boot of the machine reported.
	DisksSeen = "seen"
	// DisksBlank: every disk, when none has a partition or a signature.
	DisksBlank = "blank"
)

// InstallDisks are the disks an unattended installation may erase, as
// the machine's disk guard takes them: by a rule, the disks it names
// (stable names for pinned, the IDs a boot reported for seen, none for
// blank), and the rule in words, for the question.
type InstallDisks struct {
	Rule  string
	Disks []string
	Words string
}

// InstallArm is an unattended installation to arm: the specification
// the caller rendered and the operator confirmed, root's keys in the
// rescue system on the way, the disks it may erase (as InstallDisks
// said them), until when it stands (zero: until a boot claims it or it
// is disarmed), and what the caller reads back to record it (Import),
// which the provider keeps as it is. Wait says that the caller waits
// for the installation, so ArmInstall may wait a while to say whether
// the machine can boot it.
type InstallArm struct {
	Spec   json.RawMessage
	Keys   []PublicKey
	Disks  *InstallDisks
	Until  time.Time
	Import json.RawMessage
	Wait   bool
}

// ArmInfo is an armed unattended installation: its ID, until when it
// stands (nil: until claimed), whether the machine boots it at its next
// boot (Ready: a server took it), and a note for the operator, such as
// why not.
type ArmInfo struct {
	ID    string
	Until *time.Time
	Ready bool
	Note  string
}

// The phases of an unattended installation's outcome.
const (
	InstallWriting     = "writing"     // a boot claimed it and installs
	InstallInstalled   = "installed"   // and reported success
	InstallFailed      = "failed"      // or failure
	InstallInterrupted = "interrupted" // or did not report in time
)

// InstallOutcome is what came of an unattended installation, as the
// machine reported it: the phase, when, who wrote it, until when it may
// write, the installer's exit status, why it refused, whether it wrote
// the disks, its record (the new system's /etc/ump/install), the new
// system's SSH host keys (type and key) and the end of its output.
type InstallOutcome struct {
	Phase    string
	At       time.Time
	By       string
	Deadline time.Time
	Exit     *int
	Reason   string
	Written  bool
	Record   string
	HostKeys []string
	LogTail  string
}

// InstallState is a machine's unattended installation as its provider
// holds it: the arm (ID, by whom, when, until when; Pending while no
// boot claimed it and it has not expired), what the caller gave it to
// read back (Import; nil when the arm is gone but its outcome stays),
// and the outcome (nil before a claim).
type InstallState struct {
	ID      string
	By      string
	Created time.Time
	Until   time.Time
	Pending bool
	Import  json.RawMessage
	Outcome *InstallOutcome
}

// Claimed says that a boot claimed the installation and writes it.
func (s *InstallState) Claimed(now time.Time) bool {
	return s != nil && s.Outcome != nil && s.Outcome.Phase == InstallWriting && now.Before(s.Outcome.Deadline)
}

// ErrInstalling is UnattendedInstaller.Disarm's answer for an
// installation that a boot claimed and writes: it ends with its report
// or at its deadline.
var ErrInstalling = errors.New("the installation is being written")

// UnattendedInstaller is implemented by providers that can install a
// machine without UMP's SSH session (PXE). InstallDisks says which
// disks an installation of the machine may erase: those given (stable
// names), else those its last boot reported, else blank ones alone.
// ArmInstall arms an installation, WaitInstall waits for its outcome
// (writing what it learns meanwhile to progress, the installer's output
// among it) until the context ends, then returning the outcome as it
// stands with the context's error. Installation is the machine's
// installation as it stands, nil without one. Disarm removes the arm of
// that ID and its outcome, the arm first, and nothing of another arm;
// ErrInstalling when a boot claimed it.
type UnattendedInstaller interface {
	InstallDisks(ctx context.Context, m Machine, disks []string) (*InstallDisks, error)
	ArmInstall(ctx context.Context, m Machine, a InstallArm) (*ArmInfo, error)
	WaitInstall(ctx context.Context, m Machine, id string, progress io.Writer) (*InstallOutcome, error)
	Installation(ctx context.Context, m Machine) (*InstallState, error)
	Disarm(ctx context.Context, m Machine, id string) error
}
