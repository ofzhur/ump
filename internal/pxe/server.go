package pxe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ofzhur/ump/internal/backend"
)

// The bounds of a server: a boot's token waits that long for the
// rescue system's hello, and a boot is kept that long after it; the
// boots kept per machine and in all; the unknown MACs in the status;
// a hello's size; the standing rescue's cap (that many boots served
// without a hello within the window give the disk); the local boots
// that say a disk does not boot; how long a consumed arm is
// remembered.
const (
	helloWithin     = 30 * time.Minute
	bootKept        = 30 * time.Minute
	bootsPerMachine = 8
	maxBoots        = 1024
	maxUnknown      = 256
	maxHello        = 256 << 10
	rescueCap       = 3
	rescueCapWindow = 30 * time.Minute
	diskBoots       = 5
	diskWindow      = 15 * time.Minute
	consumedKept    = 7 * 24 * time.Hour
	journalFile     = "journal.json"
)

// The rhythm of a server: its status at most every statusEvery on a
// change and every heartbeat without one, the rescue image's manifests
// every fetchEvery, a read of the backend within loadTimeout, a chunk
// of a download within sendTimeout.
var (
	statusEvery = 5 * time.Second
	heartbeat   = time.Minute
	fetchEvery  = time.Hour
	loadTimeout = 30 * time.Second
	sendTimeout = 30 * time.Second
)

const sendChunk = 1 << 20

// Server is a PXE server of one site (ump pxe serve), in chain mode:
// the site's DHCP server points the machines' firmware at an iPXE
// binary and iPXE at boot.ipxe here. It answers from what the backend
// holds for the site, read every Poll (site.json, the arms and the
// outcomes), boots the rescue image of its Cache with a boot config in
// front, takes the rescue system's hello, consumes an armed rescue once
// its initrd was sent completely, and reports what it sees in its
// status document. It reads nothing of pxe.yaml and writes nothing but
// the site's outcomes and its own status. Open it before anything else.
type Server struct {
	// Site is the site it serves, Name its name among the site's servers
	// (the host's), Version ump's.
	Site, Name, Version string
	Store               Store
	Cache               *Cache
	// URL is where the machines reach it (http://192.0.2.5:8067); empty:
	// the address and port a request came to.
	URL string
	// Data is the directory of what outlives the process (journal.json:
	// the arms it consumed, its boots); empty: nothing does.
	Data string
	// Poll is how often it reads the backend (default 10 s).
	Poll time.Duration
	// Warnings go into its status as they are.
	Warnings []string
	// Logf logs an event, Debugf a detail (-v); Notify tells the service
	// manager how it is (STATUS=...). Nil: not at all.
	Logf, Debugf func(format string, args ...any)
	Notify       func(state string)
	// Now is the clock; nil: time.Now.
	Now func() time.Time

	mu       sync.Mutex
	started  time.Time
	url      string // the address the last script request came to
	site     *SiteDoc
	siteErr  error
	arms     map[string]*Arm
	outcomes map[string]*Outcome
	versions map[string]string // of the documents read, by path
	loaded   time.Time
	downErr  error
	downAt   time.Time
	rescues  map[string]*Rescue // by architecture
	fetchErr map[string]error
	learned  map[string]bool // architectures machines booted as
	boots    map[string]*boot
	tokens   map[string]*boot // by the token's hash
	seen     map[string]*Seen // known machines, by MAC
	unknown  map[string]*Seen
	requests map[string][]time.Time // local answers, by MAC
	logged   map[string]time.Time   // unknown MACs logged, by MAC
	consumed consumedArms

	changed chan struct{}
	fetch   chan struct{}
	bg      sync.WaitGroup // the writes of consumed arms under way
}

// boot is one network boot into the rescue system: its ID names its
// kernel and initrd, the hash of its token takes its hello. It keeps
// the rescue image's build it started with (Version: its ID in the
// Cache).
type boot struct {
	ID      string    `json:"id"`
	Machine string    `json:"machine"`
	MAC     string    `json:"mac"`
	Arch    string    `json:"arch"`
	Mode    string    `json:"mode"`
	Arm     string    `json:"arm,omitempty"`
	Version string    `json:"version"`
	Token   string    `json:"token,omitempty"`
	Created time.Time `json:"created"`
	// Served is when its initrd was sent completely, Hello when its
	// rescue system said hello.
	Served time.Time `json:"served,omitzero"`
	Hello  time.Time `json:"hello,omitzero"`

	rescue *Rescue // nil after a restart: its files are not served again
	cpio   []byte
}

func (b *boot) alive(now time.Time) bool {
	return now.Before(b.Created.Add(helloWithin)) || !b.Hello.IsZero() && now.Before(b.Hello.Add(bootKept))
}

// consumedArms are the arms a server consumed, by ID: its journal,
// which holds while the backend does not take the outcome.
type consumedArms map[string]*consumedArm

type consumedArm struct {
	Machine string    `json:"machine"`
	Boot    string    `json:"boot"`
	At      time.Time `json:"at"`
	// Written says that the backend has the outcome too.
	Written bool `json:"written,omitempty"`
}

// Done says whether the arm was consumed here.
func (c consumedArms) Done(arm string) bool { return c[arm] != nil }

// journalDoc is journal.json in the data directory.
type journalDoc struct {
	Consumed consumedArms `json:"consumed,omitempty"`
	Boots    []*boot      `json:"boots,omitempty"`
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) debugf(format string, args ...any) {
	if s.Debugf != nil {
		s.Debugf(format, args...)
	}
}

// Open readies the server: its journal from the data directory, and
// what it saw before a restart from its last status document.
func (s *Server) Open(ctx context.Context) error {
	if s.Poll <= 0 {
		s.Poll = 10 * time.Second
	}
	s.started = s.now()
	s.arms, s.outcomes, s.versions = map[string]*Arm{}, map[string]*Outcome{}, map[string]string{}
	s.rescues, s.fetchErr, s.learned = map[string]*Rescue{}, map[string]error{}, map[string]bool{}
	s.boots, s.tokens = map[string]*boot{}, map[string]*boot{}
	s.seen, s.unknown, s.requests, s.logged = map[string]*Seen{}, map[string]*Seen{}, map[string][]time.Time{}, map[string]time.Time{}
	s.consumed = consumedArms{}
	s.changed, s.fetch = make(chan struct{}, 1), make(chan struct{}, 1)
	if s.Data != "" {
		if err := os.MkdirAll(s.Data, 0o700); err != nil {
			return err
		}
		path := filepath.Join(s.Data, journalFile)
		data, err := os.ReadFile(path)
		var j journalDoc
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case json.Unmarshal(data, &j) != nil:
			s.logf("%s is not a journal; starting without it", path)
			j = journalDoc{}
		}
		now := s.now()
		for id, c := range j.Consumed {
			if c != nil {
				s.consumed[id] = c
			}
		}
		for _, b := range j.Boots {
			if b != nil && b.ID != "" && b.alive(now) {
				s.boots[b.ID] = b
				if b.Token != "" {
					s.tokens[b.Token] = b
				}
			}
		}
	}
	rctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	var st Status
	if v, err := s.Store.read(rctx, StatusPath(s.Site, s.Name), &st); err != nil {
		s.debugf("its last status: %v", err)
	} else if v != "" {
		for mac, sn := range st.Machines {
			if sn != nil {
				s.seen[mac] = sn
			}
		}
		for mac, sn := range st.Unknown {
			if sn != nil {
				s.unknown[mac] = sn
			}
		}
	}
	return nil
}

// --- the backend ---------------------------------------------------------

// docOf says which document of a site a path in its directory is:
// site, arm or outcome, with the machine's name; "" for the others.
func docOf(rest string) (kind, name string) {
	if rest == "site.json" {
		return "site", ""
	}
	dir, file, ok := strings.Cut(rest, "/")
	name, isJSON := strings.CutSuffix(file, ".json")
	if !ok || !isJSON || name == "" || strings.Contains(file, "/") {
		return "", ""
	}
	switch dir {
	case "arm", "outcome":
		return dir, name
	}
	return "", ""
}

// Load reads what the backend holds for the site: one listing, and the
// documents that changed since the last one. When the backend does not
// answer, the server goes on with what it read before.
func (s *Server) Load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	entries, err := s.Store.Files.List(ctx)
	if err != nil {
		s.down(err)
		return err
	}
	prefix := backend.PXEDir + s.Site + "/"
	listed := map[string]string{}
	for _, e := range entries {
		if rest, ok := strings.CutPrefix(e.Path, prefix); ok {
			if kind, _ := docOf(rest); kind != "" {
				listed[e.Path] = e.Version
			}
		}
	}
	s.mu.Lock()
	var changed []string
	for p, v := range listed {
		if v == "" || s.versions[p] != v {
			changed = append(changed, p)
		}
	}
	s.mu.Unlock()
	sort.Strings(changed)
	type doc struct {
		data    []byte
		version string
	}
	read := map[string]doc{}
	for _, p := range changed {
		data, version, err := s.Store.Files.Read(ctx, p)
		if err != nil {
			s.down(err)
			return err
		}
		read[p] = doc{data, version}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.up()
	s.loaded = s.now()
	for p := range s.versions {
		if _, ok := listed[p]; !ok {
			s.drop(p, prefix)
		}
	}
	for _, p := range changed {
		d := read[p]
		if d.version == "" {
			s.drop(p, prefix) // gone since the listing
			continue
		}
		s.versions[p] = d.version
		kind, name := docOf(strings.TrimPrefix(p, prefix))
		switch kind {
		case "site":
			s.setSite(d.data)
		case "arm":
			var a Arm
			if err := json.Unmarshal(d.data, &a); err != nil || a.ID == "" || a.Machine != name {
				s.logf("%s is not an arm of %s; ignored", p, name)
				delete(s.arms, name)
				continue
			}
			if old := s.arms[name]; old == nil || old.ID != a.ID {
				until := "no expiry"
				if !a.Until.IsZero() {
					until = "until " + a.Until.Local().Format("15:04")
				}
				s.logf("%s: %s armed (%s, by %s, %s)", name, a.Mode, ShortID(a.ID), orUnknownWho(a.By), until)
			}
			s.arms[name] = &a
		case "outcome":
			var o Outcome
			if err := json.Unmarshal(d.data, &o); err != nil {
				s.logf("%s is not an outcome; ignored", p)
				delete(s.outcomes, name)
				continue
			}
			s.outcomes[name] = &o
		}
	}
	s.signal()
	return nil
}

func orUnknownWho(by string) string {
	if by == "" {
		return "someone"
	}
	return by
}

// drop forgets a document gone from the backend.
func (s *Server) drop(p, prefix string) {
	delete(s.versions, p)
	kind, name := docOf(strings.TrimPrefix(p, prefix))
	switch kind {
	case "site":
		if s.site != nil {
			s.logf("site.json is gone: every machine boots its disk until ump config pxe apply renders it again")
		}
		s.site, s.siteErr = nil, nil
	case "arm":
		if a := s.arms[name]; a != nil {
			s.logf("%s: the arm %s is gone", name, ShortID(a.ID))
		}
		delete(s.arms, name)
	case "outcome":
		delete(s.outcomes, name)
	}
}

// setSite takes a new site.json; one that cannot be read serves
// nothing.
func (s *Server) setSite(data []byte) {
	d, err := ParseSite(data)
	if err == nil && d.Site != s.Site {
		err = fmt.Errorf("site.json of site %s says it is site %s", s.Site, d.Site)
	}
	if err != nil {
		if s.siteErr == nil || s.siteErr.Error() != err.Error() {
			s.logf("%v; every machine boots its disk", err)
		}
		s.site, s.siteErr = nil, err
		return
	}
	if s.site == nil || s.site.Hash != d.Hash {
		s.logf("site.json %s: %s", d.Hash, plural(len(d.Machines), "machine"))
	}
	s.site, s.siteErr = d, nil
	for mac := range s.seen {
		if d.Lookup(mac) == nil {
			delete(s.seen, mac)
		}
	}
	s.wantFetch()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (s *Server) down(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downErr == nil {
		s.downAt = s.now()
		s.logf("the backend does not answer (%v); serving what it read before", err)
	}
	s.downErr = err
	s.signal()
}

func (s *Server) up() {
	if s.downErr != nil {
		s.logf("the backend answers again")
		s.downErr = nil
	}
}

// signal says that the status changed.
func (s *Server) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Server) wantFetch() {
	select {
	case s.fetch <- struct{}{}:
	default:
	}
}

// --- the rescue image ----------------------------------------------------

// wanted is the rescue image and the architectures the site needs: those
// pxe.yaml gives, amd64 for machines without one, and those machines
// booted as.
func (s *Server) wanted() (string, []string) {
	if s.site == nil {
		return "", nil
	}
	set := map[string]bool{}
	for _, m := range s.site.Machines {
		if m.Arch != "" {
			set[m.Arch] = true
		} else {
			set[ArchAMD64] = true
		}
	}
	for a := range s.learned {
		set[a] = true
	}
	var archs []string
	for a := range set {
		archs = append(archs, a)
	}
	sort.Strings(archs)
	return s.site.Image, archs
}

// Fetch brings the rescue image of every architecture the site needs
// into the cache, verified; a version cached before serves while the
// image cannot be reached.
func (s *Server) Fetch(ctx context.Context) error {
	s.mu.Lock()
	image, archs := s.wanted()
	s.mu.Unlock()
	var errs []error
	for _, arch := range archs {
		r, err := s.Cache.Rescue(ctx, image, arch)
		s.mu.Lock()
		old := s.rescues[arch]
		if err != nil && old == nil {
			if c, cerr := s.Cache.Cached(arch); cerr == nil {
				r = c
			}
		}
		if err != nil {
			if prev := s.fetchErr[arch]; prev == nil || prev.Error() != err.Error() {
				s.logf("rescue image for %s: %v", arch, err)
			}
			errs = append(errs, fmt.Errorf("rescue image for %s: %w", arch, err))
		}
		s.fetchErr[arch] = err
		if r != nil && (old == nil || old.Kernel != r.Kernel) {
			s.rescues[arch] = r
			s.logf("rescue image %s for %s: verified", r.ID, arch)
		}
		keep := []string{}
		if cur := s.rescues[arch]; cur != nil {
			keep = append(keep, cur.ID)
		}
		for _, b := range s.boots {
			if b.Arch == arch {
				keep = append(keep, b.Version)
			}
		}
		s.signal()
		s.mu.Unlock()
		if r != nil {
			if err := s.Cache.Prune(arch, keep...); err != nil {
				s.logf("rescue image for %s: %v", arch, err)
			}
		}
	}
	return errors.Join(errs...)
}

// --- running -------------------------------------------------------------

// Run serves until ctx ends: it reads the backend every Poll, keeps the
// rescue image current and writes the status; at the end it writes the
// status once more.
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.fetchLoop(ctx) }()
	go func() { defer wg.Done(); s.statusLoop(ctx) }()
	poll := time.NewTicker(s.Poll)
	defer poll.Stop()
	s.notify()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			s.bg.Wait()
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.Flush(fctx); err != nil {
				s.logf("the last status: %v", err)
			}
			cancel()
			return
		case <-poll.C:
			if s.Load(ctx) == nil {
				s.writeConsumed(ctx)
			}
			s.prune()
			s.notify()
		}
	}
}

func (s *Server) fetchLoop(ctx context.Context) {
	t := time.NewTicker(fetchEvery)
	defer t.Stop()
	for {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		s.Fetch(fctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.fetch:
		}
	}
}

func (s *Server) statusLoop(ctx context.Context) {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.changed:
		}
		if wait := statusEvery - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if err := s.Flush(ctx); err != nil && ctx.Err() == nil {
			s.debugf("status: %v", err)
		}
		last = time.Now()
	}
}

// notify tells the service manager how the server is.
func (s *Server) notify() {
	if s.Notify == nil {
		return
	}
	s.mu.Lock()
	var st string
	switch {
	case s.downErr != nil:
		st = fmt.Sprintf("%s: the backend does not answer since %s", s.Site, s.downAt.Local().Format("15:04"))
	case s.site == nil:
		st = s.Site + ": no site.json"
	default:
		st = fmt.Sprintf("%s: %s, read %s ago", s.Site, plural(len(s.site.Machines), "machine"), s.now().Sub(s.loaded).Round(time.Second))
	}
	s.mu.Unlock()
	s.Notify("STATUS=" + st)
}

// prune forgets the boots that are over and the consumed arms long
// written, once their boot is over (its hellos need them).
func (s *Server) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := len(s.boots) + len(s.consumed)
	for id, b := range s.boots {
		if !b.alive(now) {
			s.dropBoot(id)
		}
	}
	for id, c := range s.consumed {
		if c.Written && now.Sub(c.At) > consumedKept && s.boots[c.Boot] == nil {
			delete(s.consumed, id)
		}
	}
	if len(s.boots)+len(s.consumed) != n {
		s.saveJournal()
	}
}

func (s *Server) dropBoot(id string) {
	if b := s.boots[id]; b != nil && b.Token != "" {
		delete(s.tokens, b.Token)
	}
	delete(s.boots, id)
}

// saveJournal writes journal.json; the caller holds s.mu.
func (s *Server) saveJournal() {
	if s.Data == "" {
		return
	}
	j := journalDoc{Consumed: s.consumed}
	for _, b := range s.boots {
		j.Boots = append(j.Boots, b)
	}
	sort.Slice(j.Boots, func(a, b int) bool { return j.Boots[a].Created.Before(j.Boots[b].Created) })
	data, err := json.MarshalIndent(j, "", "  ")
	if err == nil {
		path := filepath.Join(s.Data, journalFile)
		if err = os.WriteFile(path+".tmp", append(data, '\n'), 0o600); err == nil {
			err = os.Rename(path+".tmp", path)
		}
	}
	if err != nil {
		s.logf("journal: %v", err)
	}
}

// writeConsumed tells the backend of the arms consumed while it did not
// answer.
func (s *Server) writeConsumed(ctx context.Context) {
	s.mu.Lock()
	type todo struct {
		arm string
		c   consumedArm
	}
	var list []todo
	for id, c := range s.consumed {
		if !c.Written {
			list = append(list, todo{id, *c})
		}
	}
	s.mu.Unlock()
	for _, t := range list {
		if err := s.putConsumed(ctx, t.c.Machine, t.arm, t.c.Boot, t.c.At); err != nil {
			return
		}
		s.mu.Lock()
		if c := s.consumed[t.arm]; c != nil {
			c.Written = true
		}
		s.saveJournal()
		s.mu.Unlock()
		s.logf("%s: the backend has it now that the arm %s is consumed", t.c.Machine, ShortID(t.arm))
	}
}

// errOutcomeThere says that the outcome of an arm is there already.
var errOutcomeThere = errors.New("the outcome is there")

// putConsumed writes the outcome of a consumed rescue arm, by a
// compare-and-swap: one there already for the arm stays.
func (s *Server) putConsumed(ctx context.Context, machine, arm, bootID string, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.Store.UpdateOutcome(ctx, s.Site, machine, func(o *Outcome) (*Outcome, error) {
		if o != nil && o.Arm == arm {
			return nil, errOutcomeThere
		}
		return &Outcome{Arm: arm, Phase: PhaseBooted, Daemon: s.Name, Boot: bootID, At: at}, nil
	})
	if errors.Is(err, errOutcomeThere) {
		return nil
	}
	return err
}

// --- the status ------------------------------------------------------------

// Status is the status document as it is now.
func (s *Server) Status() *Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status(s.now())
}

// Flush writes the status document now.
func (s *Server) Flush(ctx context.Context) error {
	s.mu.Lock()
	st := s.status(s.now())
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	return s.Store.PutStatus(ctx, st)
}

// status is the status document as it is; the caller holds s.mu.
func (s *Server) status(now time.Time) *Status {
	st := &Status{Daemon: s.Name, Site: s.Site, Version: s.Version, URL: s.URL, Started: s.started, Updated: now,
		Rescue: map[string]string{}, Machines: map[string]*Seen{}, Unknown: map[string]*Seen{}, Arms: map[string]*ArmState{}}
	if st.URL == "" {
		st.URL = s.url
	}
	if s.site != nil {
		st.SiteHash = s.site.Hash
	}
	for arch, r := range s.rescues {
		st.Rescue[arch] = r.Manifest.Version
	}
	for mac, sn := range s.seen {
		// the signals as of now: a window that passed says nothing
		c := *sn
		c.Signals = s.signals(mac, sn.Name, now)
		st.Machines[mac] = &c
	}
	for mac, sn := range s.unknown {
		c := *sn
		st.Unknown[mac] = &c
	}
	for name, a := range s.arms {
		if !a.Pending(s.outcomes[name], now) || s.consumed.Done(a.ID) {
			continue
		}
		as := &ArmState{At: now}
		if as.Cannot = s.cannot(name, a); as.Cannot == "" {
			as.Ready = true
		}
		st.Arms[a.ID] = as
	}
	st.Warnings = append(st.Warnings, s.Warnings...)
	switch {
	case s.siteErr != nil:
		st.Warnings = append(st.Warnings, s.siteErr.Error())
	case s.site == nil:
		st.Warnings = append(st.Warnings, "no site.json for site "+s.Site+": ump config pxe apply renders it")
	}
	if s.downErr != nil {
		st.Warnings = append(st.Warnings, fmt.Sprintf("the backend does not answer since %s: %v", s.downAt.UTC().Format(time.RFC3339), s.downErr))
	}
	archs := slices.Sorted(func(yield func(string) bool) {
		for a := range s.fetchErr {
			if !yield(a) {
				return
			}
		}
	})
	for _, arch := range archs {
		if err := s.fetchErr[arch]; err != nil {
			st.Warnings = append(st.Warnings, fmt.Sprintf("rescue image for %s: %v", arch, err))
		}
	}
	return st
}

// cannot says why the server cannot serve a pending arm; "" when it can.
func (s *Server) cannot(name string, a *Arm) string {
	var m *SiteMachine
	if s.site != nil {
		m = s.site.Machine(name)
	}
	switch {
	case m == nil:
		return name + " is no machine of site.json of site " + s.Site
	case a.Mode != ModeRescue:
		return s.unserved(a.Mode)
	}
	arch := m.Arch
	if arch == "" {
		for _, mac := range m.MACs {
			if sn := s.seen[mac]; sn != nil && (sn.Arch == ArchAMD64 || sn.Arch == ArchARM64) {
				arch = sn.Arch
			}
		}
	}
	if arch == "" {
		arch = ArchAMD64
	}
	return s.noRescue(arch)
}

// unserved says that the server does not serve an arm's mode.
func (s *Server) unserved(mode string) string {
	if mode == ModeInstall {
		return "this PXE server serves no unattended installations (ump " + s.Version + ")"
	}
	return fmt.Sprintf("this PXE server knows no mode %q (ump %s)", mode, s.Version)
}

// noRescue says why the rescue image of an architecture cannot boot a
// machine; "" when it can.
func (s *Server) noRescue(arch string) string {
	r := s.rescues[arch]
	switch {
	case r == nil && s.fetchErr[arch] != nil:
		return fmt.Sprintf("no rescue image for %s: %v", arch, s.fetchErr[arch])
	case r == nil:
		return "no rescue image for " + arch + " yet: it is being fetched"
	case !r.Manifest.Has(FeatureBootConfig):
		return fmt.Sprintf("the rescue image %s takes no boot config; the storage needs a newer one", r.Manifest.Version)
	}
	return ""
}
