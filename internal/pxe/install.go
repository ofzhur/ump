package pxe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/backend"
)

// An unattended installation at a server, armed by the CLI (an arm of
// mode install with the specification it rendered and an operator
// confirmed):
//
//  1. Each script request of the machine before a claim is a boot of
//     its own, with a token, up to installAttempts per arm; the arm and
//     the outcome are read again first, within recheckTimeout, and
//     without them the machine gets its disk.
//  2. The boot's rescue system says hello, then asks for the
//     specification (/api/v1/spec). The server claims the arm first:
//     it creates the outcome (writing, with the deadline) by a
//     compare-and-swap, and answers once the backend has it. The same
//     token gets the same answer again until its report; another boot
//     gets 409, at any server of the site.
//  3. The output comes in parts (/api/v1/log?offset=N), kept in Data
//     and pushed to the backend, the end of it, every logEvery.
//  4. The report (/api/v1/report) turns the outcome into installed or
//     failed, by a compare-and-swap; the rescue system reboots only
//     after the 200. A report sent again gets 200 again for reportKept.
//  5. Every later answer is the machine's standing mode: the new
//     system boots from its disk. A boot of it while the installation
//     writes gets the rescue system without a token; at the deadline any
//     server of the site writes interrupted, and a late report still
//     replaces that.
//
// The journal holds the claims and the boots, Data the specifications
// handed out (0600) and the logs, so that a restarted server takes the
// parts and the report of an installation under way.

// The bounds of an installation: the boots of an armed installation a
// server answers before one claims it; how long the arm and the outcome
// may take to read again before such a boot; a part of the output, the
// output kept, and the end of it the backend gets; a report, its
// record and the end of the output it carries; how long a report is
// taken again; the logs kept per machine.
const (
	installAttempts = 3
	recheckTimeout  = 2 * time.Second
	maxLogPart      = 1 << 20
	maxLog          = 8 << 20
	maxLogPushed    = 1 << 20
	maxReport       = 256 << 10
	maxRecord       = 16 << 10
	maxLogTail      = 64 << 10
	reportKept      = 24 * time.Hour
	logsKept        = 5
)

// DefaultInstallTimeout is how long an installation may write, from its
// claim to its report, unless the server says otherwise.
const DefaultInstallTimeout = 2 * time.Hour

func (s *Server) installTimeout() time.Duration {
	if s.InstallTimeout > 0 {
		return s.InstallTimeout
	}
	return DefaultInstallTimeout
}

// outcome is the outcome of a machine's arm as the server knows it: the
// backend's, or the journal's when the backend has none of that arm
// (it lost it, or has not read it yet); the caller holds s.mu.
func (s *Server) outcome(name string) *Outcome {
	o := s.outcomes[name]
	if a := s.arms[name]; a != nil && (o == nil || o.Arm != a.ID) {
		if c := s.consumed[a.ID]; c != nil && c.Outcome != nil {
			return c.Outcome
		}
	}
	return o
}

// noInstall says why the server cannot serve an armed installation of a
// machine whose rescue image (of arch) boots; "" when it can. The
// caller holds s.mu.
func (s *Server) noInstall(m *SiteMachine, a *Arm, arch string) string {
	if s.Data == "" {
		return "this PXE server has no data directory, which an unattended installation needs"
	}
	if r := s.rescues[arch]; r != nil && !r.Manifest.Has(FeaturePXEInstall) {
		return fmt.Sprintf("the rescue image %s installs nothing unattended; the storage needs a newer one", r.Manifest.Version)
	}
	if _, err := a.Installation(m.Firmware); err != nil {
		return err.Error()
	}
	if a.Firmware != "" && m.Firmware != "" && a.Firmware != m.Firmware {
		return fmt.Sprintf("the installation is armed for %s, and pxe.yaml says %s", a.Firmware, m.Firmware)
	}
	if n := s.attempts[a.ID]; n >= installAttempts {
		return fmt.Sprintf("%d boots, and none claimed the installation (does the rescue system come up? see the console)", n)
	}
	return ""
}

// recheck reads the arm and the outcome of the machine with the MAC
// again when the server would answer it with an armed installation, so
// that a disarm of a moment ago holds; an error when the backend did not
// answer within recheckTimeout.
func (s *Server) recheck(ctx context.Context, mac string, now time.Time) error {
	s.mu.Lock()
	var name string
	if s.site != nil {
		if m := s.site.Lookup(mac); m != nil && Answer(m.Mode, s.arms[m.Name], s.outcome(m.Name), s.consumed, now) == ModeInstall {
			name = m.Name
		}
	}
	s.mu.Unlock()
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	a, av, err := s.Store.Arm(ctx, s.Site, name)
	if err != nil {
		return err
	}
	o, ov, err := s.Store.Outcome(ctx, s.Site, name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range []struct {
		path, version string
	}{{ArmPath(s.Site, name), av}, {OutcomePath(s.Site, name), ov}} {
		if d.version == "" {
			delete(s.versions, d.path)
		} else {
			s.versions[d.path] = d.version
		}
	}
	if a == nil {
		if old := s.arms[name]; old != nil {
			s.logf("%s: the arm %s is gone", name, ShortID(old.ID))
		}
		delete(s.arms, name)
	} else {
		s.arms[name] = a
	}
	if o == nil {
		delete(s.outcomes, name)
	} else {
		s.outcomes[name] = o
	}
	return nil
}

// --- the requests ----------------------------------------------------------

// httpError is a refusal as the HTTP status it answers with.
type httpError struct {
	code int
	text string
}

func (e *httpError) Error() string { return e.text }

func refuse(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

// fail answers a request with err: its status, else 503 for a backend
// that did not answer, which the rescue system asks again.
func fail(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		if he.code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		http.Error(w, he.text, he.code)
		return
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, "the backend does not answer: "+err.Error(), http.StatusServiceUnavailable)
}

// errKeep says that what the backend has is right: nothing is written.
var errKeep = errors.New("kept")

// authorize is the boot of a request's token (Authorization: Bearer),
// while it counts; nil when there is none, with the answer written.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) *boot {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	var b *boot
	if ok && token != "" {
		b = s.tokens[tokenHash(token)]
	}
	if b != nil && !b.alive(s.now()) {
		b = nil
	}
	s.mu.Unlock()
	if b == nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "no boot of this server has that token, or it expired", http.StatusUnauthorized)
	}
	return b
}

// spec hands the boot of an armed installation its specification
// (ump-pxe install), once: the server claims the arm first, and answers
// only once the backend has the claim. Asked again with the same token,
// it answers the same until the boot's report; another boot of the arm
// gets 409, and so does this one after its report. Only the boot whose
// rescue system said hello asks, from the address of its hello.
func (s *Server) spec(w http.ResponseWriter, r *http.Request) {
	b := s.authorize(w, r)
	if b == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	var err error
	switch c := s.consumed[b.Arm]; {
	case b.Mode != ModeInstall:
		err = refuse(http.StatusForbidden, "no installation is armed for this boot")
	case b.Hello.IsZero():
		err = refuse(http.StatusForbidden, "the rescue system of this boot said no hello")
	case b.Addr != "" && clientIP(r) != b.Addr:
		err = refuse(http.StatusForbidden, "the specification goes to the address of the hello, %s", b.Addr)
	case !b.Reported.IsZero():
		err = refuse(http.StatusConflict, "this boot reported its installation already")
	case c != nil && c.Boot != b.ID:
		err = refuse(http.StatusConflict, "boot %s claimed the installation (arm %s)", c.Boot, ShortID(b.Arm))
	case !b.Claimed.IsZero():
		// the same again: its answer got lost
		s.mu.Unlock()
		sa, err := s.handedOut(r.Context(), b)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, sa)
		return
	case s.claiming[b.Arm]:
		err = refuse(http.StatusServiceUnavailable, "the installation is being claimed")
	}
	if err != nil {
		s.mu.Unlock()
		fail(w, err)
		return
	}
	s.claiming[b.Arm] = true
	firmware := ""
	if s.site != nil {
		if m := s.site.Machine(b.Machine); m != nil {
			firmware = m.Firmware
		}
	}
	s.mu.Unlock()

	// a claim that began ends, whether the rescue system waits or not
	sa, o, err := s.claim(context.WithoutCancel(r.Context()), b, firmware, now)
	s.mu.Lock()
	delete(s.claiming, b.Arm)
	if err != nil {
		s.mu.Unlock()
		var he *httpError
		if errors.As(err, &he) {
			s.logf("%s: boot %s asked for the specification of the arm %s: %v", b.Machine, b.ID, ShortID(b.Arm), err)
		} else {
			s.logf("%s: the claim of the arm %s: the backend did not take it (%v); the rescue system asks again", b.Machine, ShortID(b.Arm), err)
		}
		fail(w, err)
		return
	}
	b.Claimed, b.Deadline = now, o.Deadline
	s.consumed[b.Arm] = &consumedArm{Machine: b.Machine, Boot: b.ID, At: now, Written: true, Outcome: o}
	s.outcomes[b.Machine] = o
	s.pushed[b.ID] = -1 // the backend's log is another installation's
	keep := s.saveSpec(b, sa)
	s.saveJournal()
	s.signal()
	s.mu.Unlock()
	if keep != nil {
		s.logf("%s: the specification of the arm %s is not kept in the data directory (%v); a lost answer is read from the arm again", b.Machine, ShortID(b.Arm), keep)
	}
	s.pruneLogs(b.Machine, logsKept-1)
	s.logf("%s: the installation (arm %s) claimed by boot %s; it reports by %s", b.Machine, ShortID(b.Arm), b.ID, o.Deadline.Local().Format("15:04"))
	writeJSON(w, sa)
}

// claim claims the arm of an installation's boot: its outcome, writing
// until the deadline, created by a compare-and-swap after the arm was
// read again. An outcome of the arm there already is another boot's
// claim, or this boot's own that a restart cost the answer of.
//
// A disarm removes the arm, then reads the outcome, and takes back
// nothing that is being written: so the claim reads the arm once more
// after it wrote the outcome, and takes the outcome back when the arm
// went meanwhile. Either the disarm sees the claim, or the claim sees
// the disarm.
func (s *Server) claim(ctx context.Context, b *boot, firmware string, now time.Time) (*SpecAnswer, *Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a, err := s.claimable(ctx, b, now)
	if err != nil {
		return nil, nil, err
	}
	sa, err := a.Installation(firmware)
	if err != nil {
		return nil, nil, refuse(http.StatusGone, "%v", err)
	}
	o := &Outcome{Arm: a.ID, Phase: PhaseWriting, Daemon: s.Name, Boot: b.ID, At: now, Deadline: now.Add(s.installTimeout())}
	written, done := "", false
	for range 5 {
		old, version, err := s.Store.Outcome(ctx, s.Site, b.Machine)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case old != nil && old.Arm != a.ID && old.Phase == PhaseWriting && now.Before(old.Deadline):
			return nil, nil, refuse(http.StatusConflict, "the installation of the arm %s is being written", ShortID(old.Arm))
		case old == nil || old.Arm != a.ID:
		case old.Boot == b.ID && old.Phase == PhaseWriting:
			// its own, which the arm read since stands for
			return sa, old, nil
		case old.Boot == b.ID:
			return nil, nil, refuse(http.StatusConflict, "the installation of this boot is %s already", old.Phase)
		case old.Phase == PhaseBooted:
			return nil, nil, refuse(http.StatusConflict, "the arm %s booted a rescue system at %s", ShortID(a.ID), old.Daemon)
		default:
			return nil, nil, refuse(http.StatusConflict, "boot %s of %s claimed the installation (a machine with %s's MAC?)", old.Boot, old.Daemon, b.Machine)
		}
		written, err = s.Store.PutOutcome(ctx, s.Site, b.Machine, o, version)
		if errors.Is(err, backend.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		done = true
		break
	}
	if !done {
		return nil, nil, fmt.Errorf("%s: %w", OutcomePath(s.Site, b.Machine), backend.ErrConflict)
	}
	// a backend that does not answer now leaves the claim to the boot's
	// next ask, which reads the arm first
	if _, err := s.claimable(ctx, b, s.now()); err != nil {
		var he *httpError
		if errors.As(err, &he) {
			s.unclaim(ctx, b, written)
		}
		return nil, nil, err
	}
	return sa, o, nil
}

// claimable is the arm of an installation's boot as the backend has it
// now, while it stands for that boot; a refusal (410) when it went, and
// then the boot's own claim of it goes too.
func (s *Server) claimable(ctx context.Context, b *boot, now time.Time) (*Arm, error) {
	a, _, err := s.Store.Arm(ctx, s.Site, b.Machine)
	switch {
	case err != nil:
		return nil, err
	case a == nil || a.ID != b.Arm || a.Machine != b.Machine:
		err = refuse(http.StatusGone, "the arm %s is gone: it was disarmed, or another took its place", ShortID(b.Arm))
	case a.Mode != ModeInstall:
		err = refuse(http.StatusGone, "the arm %s is no installation", ShortID(b.Arm))
	case a.Expired(now):
		err = refuse(http.StatusGone, "the arm %s expired at %s", ShortID(b.Arm), a.Until.Local().Format("15:04"))
	default:
		return a, nil
	}
	s.unclaim(ctx, b, "")
	return nil, err
}

// unclaim takes back the claim a boot wrote of an arm that went before
// the boot had its answer: the outcome goes, while it is that claim (at
// the version written, when given), and the machine boots as if its arm
// had never been claimed. Nobody else writes an outcome that is being
// written but its boot's report, which a boot without its specification
// never sends.
func (s *Server) unclaim(ctx context.Context, b *boot, version string) {
	o, v, err := s.Store.Outcome(ctx, s.Site, b.Machine)
	switch {
	case err != nil || o == nil || o.Arm != b.Arm || o.Boot != b.ID || o.Phase != PhaseWriting:
		return
	case version != "" && v != version:
		return
	}
	if err := s.Store.Files.Delete(ctx, OutcomePath(s.Site, b.Machine)); err != nil {
		s.logf("%s: the claim of the arm %s, which went meanwhile, stays: %v; it ends at its deadline", b.Machine, ShortID(b.Arm), err)
		return
	}
	s.logf("%s: the arm %s went while boot %s claimed it: the claim is taken back", b.Machine, ShortID(b.Arm), b.ID)
}

// specFile is where the server keeps the specification it handed a
// boot, until its report.
func (s *Server) specFile(b *boot) string {
	return filepath.Join(s.Data, "specs", b.ID+".json")
}

// saveSpec keeps the specification handed to a boot; the caller holds
// s.mu.
func (s *Server) saveSpec(b *boot, sa *SpecAnswer) error {
	data, err := json.Marshal(sa)
	if err != nil {
		return err
	}
	p := s.specFile(b)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// removeSpec forgets the specification handed to a boot; the caller
// holds s.mu.
func (s *Server) removeSpec(b *boot) {
	if s.Data != "" {
		os.Remove(s.specFile(b))
	}
}

// handedOut is the specification a boot claimed, again: as kept, else
// from its arm, if that still stands.
func (s *Server) handedOut(ctx context.Context, b *boot) (*SpecAnswer, error) {
	if data, err := os.ReadFile(s.specFile(b)); err == nil {
		var sa SpecAnswer
		if json.Unmarshal(data, &sa) == nil {
			return &sa, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a, _, err := s.Store.Arm(ctx, s.Site, b.Machine)
	if err != nil {
		return nil, err
	}
	if a == nil || a.ID != b.Arm {
		return nil, refuse(http.StatusGone, "the specification of this boot is not kept, and its arm %s is gone", ShortID(b.Arm))
	}
	s.mu.Lock()
	firmware := ""
	if s.site != nil {
		if m := s.site.Machine(b.Machine); m != nil {
			firmware = m.Firmware
		}
	}
	s.mu.Unlock()
	sa, err := a.Installation(firmware)
	if err != nil {
		return nil, refuse(http.StatusGone, "%v", err)
	}
	return sa, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// --- the log -----------------------------------------------------------------

// logFile is where the server keeps a boot's output: Data/logs/NAME/.
func (s *Server) logFile(b *boot) string {
	return filepath.Join(s.Data, "logs", b.Machine, b.ID+".log")
}

// logCut ends a log the server kept no more of.
const logCut = "\n[ump: the log is cut off here, at 8 MB; the report has its end]\n"

// logPart takes a part of an installation's output (ump-pxe install) at
// its offset in the output: what the server has already changes
// nothing, a part that starts beyond it is refused (409, with the offset
// it has in Ump-Log-Offset), so that the log is the output as it came,
// however often a part was sent. The server keeps the first maxLog
// bytes.
func (s *Server) logPart(w http.ResponseWriter, r *http.Request) {
	b := s.authorize(w, r)
	if b == nil {
		return
	}
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || off < 0 {
		http.Error(w, "offset: not a number of bytes", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogPart))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "a part of the log is at most 1 MB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.mu.Lock()
	claimed, end := !b.Claimed.IsZero(), b.LogEnd
	s.mu.Unlock()
	switch {
	case !claimed:
		http.Error(w, "this boot claimed no installation", http.StatusForbidden)
		return
	case off > end:
		w.Header().Set("Ump-Log-Offset", strconv.FormatInt(end, 10))
		http.Error(w, fmt.Sprintf("the log has %d bytes; a part starts there or before", end), http.StatusConflict)
		return
	case off+int64(len(data)) <= end:
		io.WriteString(w, "taken\n")
		return
	}
	if err := s.appendLog(b, end, data[end-off:]); err != nil {
		s.logf("%s: the log of boot %s: %v", b.Machine, b.ID, err)
		http.Error(w, "the log cannot be kept", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	b.LogEnd = off + int64(len(data))
	s.saveJournal()
	s.mu.Unlock()
	io.WriteString(w, "taken\n")
}

// appendLog writes part to a boot's log at end, the offset the server
// has; what a crash left beyond end goes. Of what goes beyond maxLog it
// keeps a line that says so, once. The caller holds s.logMu.
func (s *Server) appendLog(b *boot, end int64, part []byte) error {
	if end > maxLog {
		return nil // cut off already
	}
	p := s.logFile(b)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(end); err != nil {
		return err
	}
	if room := maxLog - end; int64(len(part)) > room {
		part = append(part[:room:room], logCut...)
	}
	_, err = f.WriteAt(part, end)
	return err
}

// pruneLogs keeps the newest n logs of a machine.
func (s *Server) pruneLogs(name string, n int) {
	dir := filepath.Join(s.Data, "logs", name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type log struct {
		name string
		at   time.Time
	}
	var logs []log
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, log{e.Name(), fi.ModTime()})
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].at.After(logs[j].at) })
	for i := n; i < len(logs); i++ {
		os.Remove(filepath.Join(dir, logs[i].name))
	}
}

// logLoop pushes the installation logs that grew every logEvery.
func (s *Server) logLoop(ctx context.Context) {
	t := time.NewTicker(logEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pushLogs(ctx)
		}
	}
}

// pushLogs puts the end of each installation log that grew since into
// the backend (logs/NAME.log, replacing what was there), at most
// maxLogPushed of it.
func (s *Server) pushLogs(ctx context.Context) {
	s.mu.Lock()
	var list []*boot
	for _, b := range s.boots {
		if !b.Claimed.IsZero() && b.LogEnd > s.pushed[b.ID] {
			list = append(list, b)
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Claimed.Before(list[j].Claimed) })
	for _, b := range list {
		s.logMu.Lock()
		s.mu.Lock()
		end := b.LogEnd
		s.mu.Unlock()
		data, err := s.logTail(b)
		s.logMu.Unlock()
		if err != nil {
			s.logf("%s: the log of boot %s: %v", b.Machine, b.ID, err)
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, loadTimeout)
		err = s.putLog(wctx, b.Machine, data)
		cancel()
		if err != nil {
			s.debugf("%s: the log in the backend: %v", b.Machine, err)
			return
		}
		s.mu.Lock()
		s.pushed[b.ID] = end
		s.mu.Unlock()
	}
}

// logHead begins the line in front of a log in the backend that holds
// only the end of the output (logTail).
const logHead = "[ump: the first "

// LogOutput is a log as the backend has it without the line in front
// that says where the beginning of a long output is: the output alone,
// or the end of it.
func LogOutput(data []byte) []byte {
	if !bytes.HasPrefix(data, []byte(logHead)) {
		return data
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return data[i+1:]
	}
	return nil
}

// logTail is the end of a boot's log as the backend gets it. The caller
// holds s.logMu.
func (s *Server) logTail(b *boot) ([]byte, error) {
	f, err := os.Open(s.logFile(b))
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var head string
	start := int64(0)
	if n := fi.Size(); n > maxLogPushed {
		start = n - maxLogPushed
		head = fmt.Sprintf(logHead+"%s are on the PXE server %s, %s]\n", size(start), s.Name, s.logFile(b))
	}
	data := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return append([]byte(head), data...), nil
}

// putLog writes a machine's log into the backend, over whatever is
// there: the server of its installation is the only writer.
func (s *Server) putLog(ctx context.Context, name string, data []byte) error {
	p := LogPath(s.Site, name)
	for range 3 {
		_, version, err := s.Store.Files.Read(ctx, p)
		if err != nil {
			return err
		}
		_, err = s.Store.Files.Write(ctx, p, data, version)
		if !errors.Is(err, backend.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", p, backend.ErrConflict)
}

// --- the report ----------------------------------------------------------------

// ParseReport reads a report as the rescue system posts it.
func ParseReport(r io.Reader) (*Report, error) {
	var rep Report
	dec := json.NewDecoder(r)
	if err := dec.Decode(&rep); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	if dec.More() {
		return nil, errors.New("report: more than one JSON value")
	}
	switch {
	case rep.Exit < 0 || rep.Exit > 255:
		return nil, fmt.Errorf("report: exit status %d", rep.Exit)
	case len(rep.Record) > maxRecord:
		return nil, errors.New("report: the record is bigger than 16 KB")
	}
	if len(rep.LogTail) > maxLogTail {
		rep.LogTail = rep.LogTail[len(rep.LogTail)-maxLogTail:]
	}
	rep.LogTail = strings.ToValidUTF8(rep.LogTail, "")
	return &rep, nil
}

// report takes the result of an installation (ump-pxe install): the
// outcome of its claim becomes installed or failed by a
// compare-and-swap, and the answer is 200 only once the backend has it,
// so that the rescue system reboots into the new system only then. A
// report sent again gets 200 again; while the backend does not answer,
// 503, and the rescue system asks again.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	b := s.authorize(w, r)
	if b == nil {
		return
	}
	rep, err := ParseReport(http.MaxBytesReader(w, r.Body, maxReport))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "the report is too big", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := s.now()
	s.mu.Lock()
	switch {
	case rep.Boot != "" && rep.Boot != b.ID:
		err = refuse(http.StatusForbidden, "the token is another boot's")
	case b.Claimed.IsZero():
		err = refuse(http.StatusForbidden, "this boot claimed no installation")
	case !b.Reported.IsZero():
		s.mu.Unlock()
		io.WriteString(w, "taken\n")
		return
	case s.reports[b.ID]:
		err = refuse(http.StatusServiceUnavailable, "the report is being written")
	}
	if err != nil {
		s.mu.Unlock()
		fail(w, err)
		return
	}
	s.reports[b.ID] = true
	s.mu.Unlock()

	o, err := s.putReport(context.WithoutCancel(r.Context()), b, rep, now)
	s.mu.Lock()
	delete(s.reports, b.ID)
	if err != nil {
		if s.refused[b.ID] != err.Error() {
			s.refused[b.ID] = err.Error()
			s.logf("%s: the report of the installation (arm %s, exit status %d) is not recorded: %v", b.Machine, ShortID(b.Arm), rep.Exit, err)
		}
		s.mu.Unlock()
		fail(w, err)
		return
	}
	b.Reported = now
	if c := s.consumed[b.Arm]; c != nil {
		// the journal keeps it without the end of the log, which the
		// backend's log has
		j := *o
		j.LogTail = ""
		c.Outcome = &j
	}
	if cur := s.outcomes[b.Machine]; cur == nil || cur.Arm == o.Arm {
		s.outcomes[b.Machine] = o
	}
	s.removeSpec(b)
	s.saveJournal()
	s.signal()
	s.mu.Unlock()
	switch {
	case o.Phase == PhaseInstalled:
		s.logf("%s: installed (arm %s); it boots its disk from now on", b.Machine, ShortID(b.Arm))
	case o.Reason != "":
		s.logf("%s: the installation (arm %s) failed with exit status %d: %s", b.Machine, ShortID(b.Arm), rep.Exit, o.Reason)
	default:
		s.logf("%s: the installation (arm %s) failed with exit status %d", b.Machine, ShortID(b.Arm), rep.Exit)
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.pushLogs(context.Background())
	}()
	io.WriteString(w, "taken\n")
}

// putReport writes a report into the outcome of its boot's claim:
// writing, or interrupted by its deadline, becomes installed or failed.
// An outcome that holds the report already stays; one of another arm or
// of another boot takes none, nor does one that is gone with its arm (ump
// settled it); one the backend lost while the arm stands is written anew.
func (s *Server) putReport(ctx context.Context, b *boot, rep *Report, now time.Time) (*Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a, _, err := s.Store.Arm(ctx, s.Site, b.Machine)
	if err != nil {
		return nil, err
	}
	stands := a != nil && a.ID == b.Arm
	exit := rep.Exit
	next := &Outcome{Arm: b.Arm, Phase: PhaseFailed, Daemon: s.Name, Boot: b.ID, At: now, Exit: &exit, Reason: rep.Reason,
		Written: exit == 0, Record: rep.Record, HostKeys: rep.HostKeys, LogTail: rep.LogTail}
	if exit == 0 {
		next.Phase = PhaseInstalled
	}
	var kept *Outcome
	_, err = s.Store.UpdateOutcome(ctx, s.Site, b.Machine, func(old *Outcome) (*Outcome, error) {
		switch {
		case old == nil && stands:
			return next, nil
		case old == nil:
			return nil, refuse(http.StatusConflict, "the outcome of the installation is gone with its arm: ump settled it since")
		case old.Arm != b.Arm:
			return nil, refuse(http.StatusConflict, "%s was armed again since (arm %s)", b.Machine, ShortID(old.Arm))
		case old.Boot != b.ID:
			return nil, refuse(http.StatusConflict, "boot %s claimed the installation, not this one", old.Boot)
		case old.Phase == PhaseInstalled || old.Phase == PhaseFailed:
			kept = old
			return nil, errKeep
		}
		return next, nil
	})
	if errors.Is(err, errKeep) {
		return kept, nil
	}
	if err != nil {
		return nil, err
	}
	return next, nil
}

// --- deadlines and lost outcomes -------------------------------------------------

// interrupt ends the installations of the site whose deadline passed
// without a report: whichever server sees it first writes interrupted,
// by a compare-and-swap, and the machine gets its standing mode again.
func (s *Server) interrupt(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	var due []string
	for name, o := range s.outcomes {
		if o.Phase == PhaseWriting && !now.Before(o.Deadline) {
			due = append(due, name)
		}
	}
	s.mu.Unlock()
	sort.Strings(due)
	for _, name := range due {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var next *Outcome
		_, err := s.Store.UpdateOutcome(wctx, s.Site, name, func(old *Outcome) (*Outcome, error) {
			if old == nil || old.Phase != PhaseWriting || now.Before(old.Deadline) {
				return nil, errKeep
			}
			c := *old
			c.Phase, c.At = PhaseInterrupted, now
			next = &c
			return next, nil
		})
		cancel()
		if errors.Is(err, errKeep) {
			continue
		}
		if err != nil {
			s.debugf("%s: interrupted: %v", name, err)
			return
		}
		s.mu.Lock()
		s.outcomes[name] = next
		s.signal()
		s.mu.Unlock()
		s.logf("%s: the installation (arm %s, boot %s of %s) did not report by its deadline, %s: interrupted; it boots its disk", name, ShortID(next.Arm), next.Boot, next.Daemon, next.Deadline.Local().Format("15:04"))
	}
}

// errArmGone says that the arm of an outcome to put back is gone from
// the backend, or another took its place.
var errArmGone = errors.New("the arm is gone")

// armStands reads the machine's arm from the backend: nil when it is
// still the arm id, errArmGone when not. An outcome is put back only
// for an arm that stands, read after the outcome was found missing:
// whoever removes an arm and its outcome removes the arm first, so an
// arm read afterwards is gone when the outcome was settled, whatever
// the server read before.
func (s *Server) armStands(ctx context.Context, name, id string) error {
	a, _, err := s.Store.Arm(ctx, s.Site, name)
	switch {
	case err != nil:
		return err
	case a == nil || a.ID != id:
		return errArmGone
	}
	return nil
}

// restoreOutcome puts back the outcome of an installation claimed here
// that the backend lost while its arm stands.
func (s *Server) restoreOutcome(ctx context.Context, name string, o *Outcome) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.Store.UpdateOutcome(ctx, s.Site, name, func(old *Outcome) (*Outcome, error) {
		if old != nil && old.Arm == o.Arm {
			return nil, errKeep
		}
		return o, s.armStands(ctx, name, o.Arm)
	})
	if errors.Is(err, errKeep) || errors.Is(err, errArmGone) {
		return nil
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	c := *o
	s.outcomes[name] = &c
	s.mu.Unlock()
	s.logf("%s: the backend lost the outcome of the arm %s, which was claimed here: put back (%s)", name, ShortID(o.Arm), o.Phase)
	return nil
}
