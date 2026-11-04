package pxe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/backend"
)

// The documents of a site, under .pxe/<site>/ in the home's backend:
//
//	site.json          what its servers serve (the CLI renders it)
//	arm/NAME.json      a boot armed for a machine (the CLI)
//	outcome/NAME.json  what came of it (a server, by compare-and-swap)
//	status/HOST.json   what a server sees (that server alone)
//	logs/NAME.log      the end of an installation's output (a server)
//
// The CLI writes what decides, the servers what they report, so that a
// backend policy can keep a server from arming anything.

// SitePath and the others are the documents' paths in the backend.
func SitePath(site string) string { return backend.PXEDir + site + "/site.json" }
func ArmPath(site, name string) string {
	return backend.PXEDir + site + "/arm/" + name + ".json"
}
func OutcomePath(site, name string) string {
	return backend.PXEDir + site + "/outcome/" + name + ".json"
}
func StatusPath(site, host string) string {
	return backend.PXEDir + site + "/status/" + host + ".json"
}
func LogPath(site, name string) string { return backend.PXEDir + site + "/logs/" + name + ".log" }

// Arm is a network boot armed for a machine: one boot into the rescue
// system, or one unattended installation. Its ID ties the boot, the
// outcome and the CLI that waits for it together; it expires at Until
// (zero: never).
type Arm struct {
	ID      string    `json:"id"`
	Machine string    `json:"machine"`
	Mode    string    `json:"mode"`
	By      string    `json:"by"`
	Created time.Time `json:"created"`
	Until   time.Time `json:"until,omitzero"`
	// Keys are root's keys on the way, as key lines, besides the site's
	// for the machine.
	Keys []string `json:"keys,omitempty"`
	// An installation's: the specification the CLI rendered and an
	// operator confirmed, its hash, the firmware it must boot with, the
	// disks it may erase, and what the CLI reads back to record it.
	Spec     json.RawMessage `json:"spec,omitempty"`
	SpecHash string          `json:"spec_hash,omitempty"`
	Firmware string          `json:"firmware,omitempty"`
	Disks    *DiskRule       `json:"disks,omitempty"`
	Import   json.RawMessage `json:"import,omitempty"`
}

// DiskRule is the disks an unattended installation may erase, as the
// rescue system's disk guard takes them: pinned (Names, stable names),
// seen (IDs, those a hello reported) or blank.
type DiskRule struct {
	Rule  string   `json:"rule"`
	Names []string `json:"names,omitempty"`
	IDs   []string `json:"ids,omitempty"`
}

// The rules of a DiskRule.
const (
	DisksPinned = "pinned"
	DisksSeen   = "seen"
	DisksBlank  = "blank"
)

// Check says what is wrong with the rule; nil when nothing is.
func (d *DiskRule) Check() error {
	switch {
	case d == nil:
		return errors.New("no disk rule")
	case d.Rule == DisksPinned && len(d.Names) == 0:
		return errors.New("the disk rule pinned names no disk")
	case d.Rule == DisksSeen && len(d.IDs) == 0:
		return errors.New("the disk rule seen records no disk")
	case d.Rule != DisksPinned && d.Rule != DisksSeen && d.Rule != DisksBlank:
		return fmt.Errorf("the disk rule %q is not pinned, seen or blank", d.Rule)
	}
	return nil
}

// SpecHash is the hash an arm carries of its specification: the
// SHA-256 of its compact JSON, so that indenting changes nothing.
func SpecHash(spec json.RawMessage) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, spec); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// Installation is what an armed installation hands its boot (the
// rescue system's ump-pxe install, at /api/v1/spec): the arm's
// specification, the firmware the machine must boot with, and the
// disks it may erase; an error says why the arm cannot install.
func (a *Arm) Installation(firmware string) (*SpecAnswer, error) {
	var obj map[string]json.RawMessage
	if len(a.Spec) == 0 || json.Unmarshal(a.Spec, &obj) != nil {
		return nil, errors.New("the arm holds no specification")
	}
	if a.SpecHash != "" {
		if h, err := SpecHash(a.Spec); err != nil || h != a.SpecHash {
			return nil, errors.New("the arm's specification does not match its hash")
		}
	}
	if err := a.Disks.Check(); err != nil {
		return nil, fmt.Errorf("the arm: %w", err)
	}
	if a.Firmware != "" {
		firmware = a.Firmware
	}
	return &SpecAnswer{Spec: a.Spec, Firmware: firmware, Disks: a.Disks}, nil
}

// SpecAnswer is the answer at /api/v1/spec.
type SpecAnswer struct {
	Spec     json.RawMessage `json:"spec"`
	Firmware string          `json:"firmware,omitempty"`
	Disks    *DiskRule       `json:"disks"`
}

// NewArmID is a new arm's ID: 16 hex digits, random.
func NewArmID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ShortID is an ID as people read it: its first 8 digits.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Expired says whether the arm's window has passed.
func (a *Arm) Expired(now time.Time) bool {
	return !a.Until.IsZero() && !now.Before(a.Until)
}

// Pending says whether the arm still stands: not expired, and no
// outcome of it but an installation that is being written.
func (a *Arm) Pending(o *Outcome, now time.Time) bool {
	if a.Expired(now) {
		return false
	}
	return o == nil || o.Arm != a.ID || a.Mode == ModeInstall && o.Phase == PhaseWriting
}

// The phases of an outcome.
const (
	PhaseBooted      = "booted"      // a rescue's boot was served: the arm is consumed
	PhaseWriting     = "writing"     // an installation claimed its specification
	PhaseInstalled   = "installed"   // and succeeded
	PhaseFailed      = "failed"      // or failed
	PhaseInterrupted = "interrupted" // or did not report by its deadline
)

// Outcome is what came of an arm, written by a server with a
// compare-and-swap, so that two servers never serve one arm twice.
type Outcome struct {
	Arm    string    `json:"arm"`
	Phase  string    `json:"phase"`
	Daemon string    `json:"daemon,omitempty"`
	Boot   string    `json:"boot,omitempty"`
	At     time.Time `json:"at"`
	// Deadline ends an installation that is being written.
	Deadline time.Time `json:"deadline,omitzero"`
	// An installation's report: install.sh's exit status, why it
	// refused, whether it wrote the disks, its record, the new system's
	// host keys and the end of its output.
	Exit     *int     `json:"exit,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Written  bool     `json:"written,omitempty"`
	Record   string   `json:"record,omitempty"`
	HostKeys []string `json:"host_keys,omitempty"`
	LogTail  string   `json:"log_tail,omitempty"`
}

// Status is what one PXE server of a site sees, written by that server
// alone, at most every few seconds on a change and every minute as a
// heartbeat.
type Status struct {
	Daemon  string    `json:"daemon"`
	Site    string    `json:"site"`
	Version string    `json:"version"`
	URL     string    `json:"url,omitempty"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	// SiteHash is the hash of the site.json it serves.
	SiteHash string `json:"site_hash,omitempty"`
	// Rescue is the rescue image's version it has, by architecture.
	Rescue map[string]string `json:"rescue,omitempty"`
	// Machines are the machines it saw, by MAC; Unknown the MACs without
	// an entry (a few).
	Machines map[string]*Seen `json:"machines,omitempty"`
	Unknown  map[string]*Seen `json:"unknown,omitempty"`
	// Arms are the arms it can serve (ready) or cannot, by ID.
	Arms map[string]*ArmState `json:"arms,omitempty"`
	// Warnings are what it wants people to know.
	Warnings []string `json:"warnings,omitempty"`
}

// Seen is a machine as a server last saw it.
type Seen struct {
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
	// Address is the one iPXE had: learned, never where ump connects
	// but for the session of an arm.
	Address  string `json:"address,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Platform string `json:"platform,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	// Answer is what it got (local, rescue, install), Reason why it got
	// the disk although its arm or its mode says otherwise.
	Answer string `json:"answer"`
	Reason string `json:"reason,omitempty"`
	Arm    string `json:"arm,omitempty"`
	Boot   string `json:"boot,omitempty"`
	// Hello is its rescue system's last hello.
	Hello   *Hello   `json:"hello,omitempty"`
	Signals []string `json:"signals,omitempty"`
}

// ArmState says whether a server can serve an arm.
type ArmState struct {
	Ready  bool      `json:"ready,omitempty"`
	Cannot string    `json:"cannot,omitempty"`
	At     time.Time `json:"at"`
}

// Hello is what the rescue system of a network boot posts about the
// machine (ump-pxe hello), with the arm and the time the server adds.
type Hello struct {
	Boot       string            `json:"boot"`
	Arm        string            `json:"arm,omitempty"`
	At         time.Time         `json:"at,omitzero"`
	Rescue     string            `json:"rescue"`
	Arch       string            `json:"arch"`
	Hostname   string            `json:"hostname"`
	Firmware   string            `json:"firmware"`
	BootMAC    string            `json:"boot_mac"`
	Interfaces []Interface       `json:"interfaces"`
	HostKeys   []string          `json:"host_keys"`
	DMI        map[string]string `json:"dmi,omitempty"`
	Disks      json.RawMessage   `json:"disks,omitempty"`
	GuardDisks []GuardDisk       `json:"guard_disks,omitempty"`
	MemoryMB   int               `json:"memory_mb"`
	EFIBootMgr string            `json:"efibootmgr,omitempty"`
	IPMILan    string            `json:"ipmi_lan,omitempty"`
}

// Interface is a network interface of a hello.
type Interface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	Up        bool     `json:"up"`
	Addresses []string `json:"addresses"`
}

// GuardDisk is a disk the disk guard counts, with its ID (wwn:WWN,
// serial:SERIAL; nil without either).
type GuardDisk struct {
	Path string  `json:"path"`
	ID   *string `json:"id"`
}

// Address is the boot interface's first IPv4 address, without its
// prefix length; another interface's when the boot's has none.
func (h *Hello) Address() string {
	var other string
	for _, i := range h.Interfaces {
		for _, a := range i.Addresses {
			p, err := netip.ParsePrefix(a)
			if err != nil || !p.Addr().Is4() {
				continue
			}
			if strings.EqualFold(i.MAC, h.BootMAC) {
				return p.Addr().String()
			}
			if other == "" {
				other = p.Addr().String()
			}
		}
	}
	return other
}

// Report is what an unattended installation reports at its end
// (ump-pxe install).
type Report struct {
	Boot     string   `json:"boot"`
	Exit     int      `json:"exit"`
	Reason   string   `json:"reason,omitempty"`
	Record   string   `json:"record,omitempty"`
	HostKeys []string `json:"host_keys,omitempty"`
	LogTail  string   `json:"log_tail,omitempty"`
}

// Files is where the documents are: the home's backend (a
// backend.Backend is one), or its local directory.
type Files interface {
	Read(ctx context.Context, path string) ([]byte, string, error)
	Write(ctx context.Context, path string, data []byte, ifVersion string) (string, error)
	Delete(ctx context.Context, path string) error
	List(ctx context.Context) ([]backend.Entry, error)
}

// Store reads and writes the documents of the sites, each with the
// version it was read at, so that a write over a changed one is refused
// (backend.ErrConflict).
type Store struct {
	Files Files
}

// read decodes the document at p into v; no document is version "".
func (s Store) read(ctx context.Context, p string, v any) (string, error) {
	data, version, err := s.Files.Read(ctx, p)
	if err != nil || version == "" {
		return "", err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return version, nil
}

// write writes v to p over ifVersion ("": none there yet).
func (s Store) write(ctx context.Context, p string, v any, ifVersion string) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return s.Files.Write(ctx, p, append(data, '\n'), ifVersion)
}

// Site reads a site's site.json; nil when there is none.
func (s Store) Site(ctx context.Context, site string) (*SiteDoc, error) {
	data, version, err := s.Files.Read(ctx, SitePath(site))
	if err != nil || version == "" {
		return nil, err
	}
	return ParseSite(data)
}

// PutSite writes site.json, and says whether it changed: the CLI's
// rendering replaces whatever another one wrote.
func (s Store) PutSite(ctx context.Context, d *SiteDoc) (bool, error) {
	for range 3 {
		data, version, err := s.Files.Read(ctx, SitePath(d.Site))
		if err != nil {
			return false, err
		}
		if old, err := ParseSite(data); version != "" && err == nil && old.Hash == d.Hash {
			return false, nil
		}
		_, err = s.Files.Write(ctx, SitePath(d.Site), d.JSON(), version)
		if !errors.Is(err, backend.ErrConflict) {
			return err == nil, err
		}
	}
	return false, fmt.Errorf("%s: %w", SitePath(d.Site), backend.ErrConflict)
}

// Sites are the sites with a site.json in the backend.
func (s Store) Sites(ctx context.Context) ([]string, error) {
	entries, err := s.Files.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, backend.PXEDir)
		if site, file, ok2 := strings.Cut(rest, "/"); ok && ok2 && file == "site.json" {
			out = append(out, site)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeleteSite removes a site's site.json: its servers serve nothing.
func (s Store) DeleteSite(ctx context.Context, site string) error {
	return s.Files.Delete(ctx, SitePath(site))
}

// Arm reads a machine's arm; nil and "" when there is none.
func (s Store) Arm(ctx context.Context, site, name string) (*Arm, string, error) {
	var a Arm
	v, err := s.read(ctx, ArmPath(site, name), &a)
	if err != nil || v == "" {
		return nil, "", err
	}
	return &a, v, nil
}

// PutArm writes a machine's arm over ifVersion ("": only when it has
// none).
func (s Store) PutArm(ctx context.Context, site string, a *Arm, ifVersion string) (string, error) {
	return s.write(ctx, ArmPath(site, a.Machine), a, ifVersion)
}

// Disarm removes a machine's arm.
func (s Store) Disarm(ctx context.Context, site, name string) error {
	return s.Files.Delete(ctx, ArmPath(site, name))
}

// Outcome reads a machine's outcome; nil and "" when there is none.
func (s Store) Outcome(ctx context.Context, site, name string) (*Outcome, string, error) {
	var o Outcome
	v, err := s.read(ctx, OutcomePath(site, name), &o)
	if err != nil || v == "" {
		return nil, "", err
	}
	return &o, v, nil
}

// PutOutcome writes a machine's outcome over ifVersion: with "" it
// claims the arm, and only one writer gets it.
func (s Store) PutOutcome(ctx context.Context, site, name string, o *Outcome, ifVersion string) (string, error) {
	return s.write(ctx, OutcomePath(site, name), o, ifVersion)
}

// UpdateOutcome changes a machine's outcome by a compare-and-swap:
// change gets the outcome there (nil: none) and returns the new one, or
// an error to write nothing; it runs again when another writer came
// first.
func (s Store) UpdateOutcome(ctx context.Context, site, name string, change func(o *Outcome) (*Outcome, error)) (*Outcome, error) {
	for range 5 {
		o, version, err := s.Outcome(ctx, site, name)
		if err != nil {
			return nil, err
		}
		next, err := change(o)
		if err != nil {
			return nil, err
		}
		_, err = s.PutOutcome(ctx, site, name, next, version)
		if errors.Is(err, backend.ErrConflict) {
			continue
		}
		return next, err
	}
	return nil, fmt.Errorf("%s: %w", OutcomePath(site, name), backend.ErrConflict)
}

// PutStatus writes a server's status over whatever is there: it is the
// document's only writer.
func (s Store) PutStatus(ctx context.Context, st *Status) error {
	p := StatusPath(st.Site, st.Daemon)
	_, version, err := s.Files.Read(ctx, p)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, p, st, version)
	return err
}

// Forget removes what the site holds of a machine: its arm, outcome and
// log, the arm first. Whoever removes an arm and its outcome removes the
// arm first: a server puts back the outcome of an arm it consumed or
// claimed while that arm stands, so that it never boots twice.
func (s Store) Forget(ctx context.Context, site, name string) error {
	for _, p := range []string{ArmPath(site, name), OutcomePath(site, name), LogPath(site, name)} {
		if err := s.Files.Delete(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Holding are the sites that hold an arm, an outcome or a log of a
// machine.
func (s Store) Holding(ctx context.Context, name string) ([]string, error) {
	entries, err := s.Files.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, backend.PXEDir)
		site, file, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || slices.Contains(out, site) {
			continue
		}
		if file == "arm/"+name+".json" || file == "outcome/"+name+".json" || file == "logs/"+name+".log" {
			out = append(out, site)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Pending are the arms of every site that still stand at now, by site
// and machine; an arm or outcome that cannot be read is an error, not
// none.
func (s Store) Pending(ctx context.Context, now time.Time) (map[string]map[string]*Arm, error) {
	entries, err := s.Files.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]*Arm{}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, backend.PXEDir)
		site, file, ok2 := strings.Cut(rest, "/")
		name, isArm := strings.CutPrefix(file, "arm/")
		name, isJSON := strings.CutSuffix(name, ".json")
		if !ok || !ok2 || !isArm || !isJSON || strings.Contains(name, "/") {
			continue
		}
		a, _, err := s.Arm(ctx, site, name)
		if err != nil {
			return nil, err
		}
		if a == nil {
			continue // gone since the listing
		}
		o, _, err := s.Outcome(ctx, site, name)
		if err != nil {
			return nil, err
		}
		if !a.Pending(o, now) {
			continue
		}
		if out[site] == nil {
			out[site] = map[string]*Arm{}
		}
		out[site][name] = a
	}
	return out, nil
}

// Snapshot is the documents of some sites as read at one time, with one
// listing: what a server serves from and the CLI shows.
type Snapshot struct {
	Sites    map[string]*SiteDoc
	Arms     map[string]map[string]*Arm
	Outcomes map[string]map[string]*Outcome
	Statuses map[string][]*Status
	// Versions are the documents' versions, by path.
	Versions map[string]string
	// Errors are documents that could not be read, by path; the others
	// are there all the same.
	Errors map[string]error
}

// Arm is the arm of a machine in the snapshot, nil when none.
func (sn *Snapshot) Arm(site, name string) *Arm { return sn.Arms[site][name] }

// Outcome is the outcome of a machine in the snapshot, nil when none.
func (sn *Snapshot) Outcome(site, name string) *Outcome { return sn.Outcomes[site][name] }

// Load reads the documents of the sites (all when none is named) that
// the backend lists.
func (s Store) Load(ctx context.Context, sites ...string) (*Snapshot, error) {
	entries, err := s.Files.List(ctx)
	if err != nil {
		return nil, err
	}
	sn := &Snapshot{Sites: map[string]*SiteDoc{}, Arms: map[string]map[string]*Arm{}, Outcomes: map[string]map[string]*Outcome{},
		Statuses: map[string][]*Status{}, Versions: map[string]string{}, Errors: map[string]error{}}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Path, backend.PXEDir)
		if !ok {
			continue
		}
		site, file, ok := strings.Cut(rest, "/")
		if !ok || len(sites) > 0 && !slices.Contains(sites, site) {
			continue
		}
		dir, base := path.Split(file)
		name, isJSON := strings.CutSuffix(base, ".json")
		var v any
		switch {
		case file == "site.json":
			data, version, err := s.Files.Read(ctx, e.Path)
			if err == nil && version != "" {
				var d *SiteDoc
				if d, err = ParseSite(data); err == nil {
					sn.Sites[site] = d
					sn.Versions[e.Path] = version
				}
			}
			if err != nil {
				sn.Errors[e.Path] = err
			}
			continue
		case dir == "arm/" && isJSON:
			a := &Arm{}
			if sn.Arms[site] == nil {
				sn.Arms[site] = map[string]*Arm{}
			}
			sn.Arms[site][name], v = a, a
		case dir == "outcome/" && isJSON:
			o := &Outcome{}
			if sn.Outcomes[site] == nil {
				sn.Outcomes[site] = map[string]*Outcome{}
			}
			sn.Outcomes[site][name], v = o, o
		case dir == "status/" && isJSON:
			st := &Status{}
			sn.Statuses[site], v = append(sn.Statuses[site], st), st
		default:
			continue
		}
		version, err := s.read(ctx, e.Path, v)
		if err != nil {
			sn.Errors[e.Path] = err
		}
		if err == nil && version != "" {
			sn.Versions[e.Path] = version
			continue
		}
		// what could not be read, or is gone since the listing, is not there
		switch dir {
		case "arm/":
			delete(sn.Arms[site], name)
		case "outcome/":
			delete(sn.Outcomes[site], name)
		case "status/":
			sn.Statuses[site] = sn.Statuses[site][:len(sn.Statuses[site])-1]
		}
	}
	return sn, nil
}
