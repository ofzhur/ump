package pxe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handler is the server's HTTP side: boot.ipxe, a machine's script, its
// boot's kernel and initrd, and the rescue system's hello.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot.ipxe", s.bootScript)
	mux.HandleFunc("GET /ipxe", s.machineScript)
	mux.HandleFunc("GET /b/{boot}/{file}", s.bootFile)
	mux.HandleFunc("POST /api/v1/hello", s.hello)
	if s.Debugf == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logWriter{ResponseWriter: w, code: http.StatusOK}
		mux.ServeHTTP(lw, r)
		s.debugf("http %s %s %s: %d, %s in %s", clientIP(r), r.Method, r.URL.Path, lw.code, size(lw.n), time.Since(start).Round(time.Millisecond))
	})
}

// logWriter counts what a response writes, and keeps io.ReaderFrom
// (sendfile) and http.ResponseController working.
type logWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *logWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *logWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.n += n
		return n, err
	}
	return io.Copy(struct{ io.Writer }{w}, r)
}

func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func clientIP(r *http.Request) string {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return r.RemoteAddr
}

// size is a number of bytes as people read it.
func size(n int64) string {
	switch {
	case n >= 10<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 10<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// base is where the machine reaches the server: URL, else the address
// and port its request came to.
func (s *Server) base(r *http.Request) string {
	if s.URL != "" {
		return strings.TrimSuffix(s.URL, "/")
	}
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if ap, err := netip.ParseAddrPort(a.String()); err == nil && !ap.Addr().IsUnspecified() {
			return "http://" + netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
		}
	}
	return "http://" + r.Host
}

func script(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	io.WriteString(w, text)
}

func (s *Server) bootScript(w http.ResponseWriter, r *http.Request) {
	script(w, BootScript(s.base(r)))
}

func (s *Server) machineScript(w http.ResponseWriter, r *http.Request) {
	c, err := ParseClient(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return // iPXE asks with GET; a HEAD boots nothing
	}
	text, line := s.answer(c, s.base(r), s.now())
	if line != "" {
		s.logf("%s", line)
	}
	script(w, text)
}

// describe is a client as the log names it: its MAC, how it booted and
// the address iPXE had.
func describe(c Client) string {
	var w []string
	if a := c.Arch(); a != "" {
		w = append(w, a)
	} else if c.BuildArch != "" {
		w = append(w, c.BuildArch)
	}
	if f := c.Firmware(); f != "" {
		w = append(w, f)
	} else if c.Platform != "" {
		w = append(w, c.Platform)
	}
	d := strings.Join(w, " ")
	if c.IP != "" {
		if d != "" {
			d += ", "
		}
		d += c.IP
	}
	if d == "" {
		return c.MAC
	}
	return c.MAC + " (" + d + ")"
}

// answer is the script of a machine's request, and the line that logs
// it ("" for none): from site.json, its arm and outcome and the journal
// (Answer), turned into the disk by what stands in the way (Refusal, the
// rescue image, the standing rescue's cap).
func (s *Server) answer(c Client, base string, now time.Time) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.url = base
	who := describe(c)
	if s.site == nil {
		return ExitScript("ump: site "+s.Site+" has nothing to serve; booting from disk", true), who + ": the disk, since there is no site.json"
	}
	m := s.site.Lookup(c.MAC)
	if m == nil {
		line := ""
		if s.logged[c.MAC].IsZero() || now.Sub(s.logged[c.MAC]) > 10*time.Minute {
			if len(s.logged) > maxUnknown {
				clear(s.logged)
			}
			s.logged[c.MAC] = now
			line = who + ": no machine of pxe.yaml, the disk"
		}
		if s.site.Unknown == UnknownLocal {
			s.seeUnknown(c, now)
		}
		return ExitScript("ump: "+c.MAC+" is no machine of site "+s.Site, false), line
	}
	a, o := s.arms[m.Name], s.outcomes[m.Name]
	mode := Answer(m.Mode, a, o, s.consumed, now)
	armed := a != nil && !s.consumed.Done(a.ID) && (o == nil || o.Arm != a.ID) && !a.Expired(now)
	arch := c.Arch()
	reason := ""
	switch mode {
	case ModeLocal:
	case ModeRescue, AnswerRescueBare:
		need := 0
		if r := s.rescues[arch]; r != nil {
			need = r.Manifest.MemoryMB
		}
		reason = Refusal(m, c, mode, need)
		if reason == "" && !s.learned[arch] {
			s.learned[arch] = true
			if s.rescues[arch] == nil {
				s.wantFetch()
			}
		}
		if reason == "" {
			reason = s.noRescue(arch)
		}
		if reason == "" && mode == ModeRescue && !armed && s.capped(m.Name, now) {
			reason = "the rescue system does not come up: " + strconv.Itoa(rescueCap) + " boots without a hello within 30 minutes (NIC driver or firmware? see the console)"
		}
	default:
		reason = s.unserved(mode)
	}
	armID := ""
	if armed || mode == AnswerRescueBare {
		armID = a.ID
	}
	sn := s.see(c, m.Name, now)
	sn.Answer, sn.Reason, sn.Arm, sn.Boot = mode, reason, armID, ""
	line := who + ": " + m.Name + ": "
	if reason != "" || mode == ModeLocal {
		sn.Answer = ModeLocal
		if reason == "" {
			s.requests[c.MAC] = append(s.requests[c.MAC], now)
			if r := s.requests[c.MAC]; len(r) > diskBoots {
				s.requests[c.MAC] = slices.Clone(r[len(r)-diskBoots:])
			}
		}
		sn.Signals = s.signals(c.MAC, m.Name, now)
		s.signal()
		if reason != "" {
			return ExitScript("ump: "+m.Name+": local: "+reason, true), line + "local (" + reason + ")"
		}
		return ExitScript("ump: "+m.Name+": local", false), line + "local"
	}

	// the rescue system: a boot of its own, with the version of now
	b := &boot{ID: randomID(), Machine: m.Name, MAC: c.MAC, Arch: arch, Mode: mode, Version: s.rescues[arch].ID, Created: now, rescue: s.rescues[arch]}
	cfg := BootConfig{Hostname: m.Hostname, BootMAC: c.MAC, DNS: "dhcp", Time: now, Keys: m.Keys}
	if mode == ModeRescue {
		token := randomID()
		b.Token = tokenHash(token)
		cfg.URL, cfg.Boot, cfg.Token = base, b.ID, token
		if armed {
			b.Arm = a.ID
			cfg.Keys = unique(append(slices.Clone(m.Keys), a.Keys...))
		}
	}
	b.cpio = cfg.Cpio()
	s.addBoot(b)
	sn.Boot = b.ID
	sn.Signals = s.signals(c.MAC, m.Name, now)
	s.saveJournal()
	s.signal()
	switch {
	case mode == AnswerRescueBare:
		line += "rescue without a token (its installation is being written, arm " + ShortID(a.ID) + ")"
	case armed:
		line += "rescue (arm " + ShortID(a.ID) + ")"
	default:
		line += "rescue"
	}
	return KernelScript(m.Name, ModeRescue, base, b.ID, KernelArgs(m.Console, arch)), line
}

// see is the machine's entry in the status, updated with the request.
func (s *Server) see(c Client, name string, now time.Time) *Seen {
	sn := s.seen[c.MAC]
	if sn == nil {
		sn = &Seen{}
		s.seen[c.MAC] = sn
	}
	sn.Name, sn.At, sn.Address, sn.Platform, sn.Firmware = name, now, c.IP, c.Platform, c.Firmware()
	if sn.Arch = c.Arch(); sn.Arch == "" {
		sn.Arch = c.BuildArch
	}
	delete(s.unknown, c.MAC)
	return sn
}

// seeUnknown lists a MAC without a machine in the status, the latest
// few.
func (s *Server) seeUnknown(c Client, now time.Time) {
	sn := s.unknown[c.MAC]
	if sn == nil {
		if len(s.unknown) >= maxUnknown {
			var oldest string
			for mac, u := range s.unknown {
				if oldest == "" || u.At.Before(s.unknown[oldest].At) {
					oldest = mac
				}
			}
			delete(s.unknown, oldest)
		}
		sn = &Seen{}
		s.unknown[c.MAC] = sn
	}
	sn.At, sn.Address, sn.Platform, sn.Firmware, sn.Answer = now, c.IP, c.Platform, c.Firmware(), ModeLocal
	if sn.Arch = c.Arch(); sn.Arch == "" {
		sn.Arch = c.BuildArch
	}
	s.signal()
}

// capped says whether the standing rescue of a machine came up too
// seldom: rescueCap boots whose initrd was sent and that said no hello,
// since its last hello and within the window.
func (s *Server) capped(name string, now time.Time) bool {
	var hello time.Time
	for _, b := range s.boots {
		if b.Machine == name && b.Hello.After(hello) {
			hello = b.Hello
		}
	}
	n := 0
	for _, b := range s.boots {
		if b.Machine == name && b.Mode == ModeRescue && b.Arm == "" && !b.Served.IsZero() && b.Hello.IsZero() &&
			b.Served.After(hello) && now.Sub(b.Served) < rescueCapWindow {
			n++
		}
	}
	return n >= rescueCap
}

// signals are what the status says of a machine beyond its answer.
func (s *Server) signals(mac, name string, now time.Time) []string {
	var out []string
	if s.capped(name, now) {
		out = append(out, "the rescue system does not come up (NIC driver or firmware? see the console)")
	}
	if r := s.requests[mac]; len(r) >= diskBoots && now.Sub(r[len(r)-diskBoots]) <= diskWindow {
		out = append(out, fmt.Sprintf("its disk does not boot: %d network boots within %d minutes", diskBoots, int(diskWindow.Minutes())))
	}
	if sn := s.seen[mac]; sn != nil && sn.Hello != nil {
		if w := BootOrder(sn.Hello); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// BootOrder says when a UEFI machine's boot order does not start with
// the network boot its rescue system came from, as its hello's
// efibootmgr -v has it: the next boot may skip the PXE server. Empty
// when it does, or when the hello does not tell.
func BootOrder(h *Hello) string {
	if h.Firmware != FirmwareUEFI {
		return ""
	}
	var current, first string
	entries := map[string]string{}
	for _, l := range strings.Split(h.EFIBootMgr, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "BootCurrent:"):
			current = strings.TrimSpace(strings.TrimPrefix(l, "BootCurrent:"))
		case strings.HasPrefix(l, "BootOrder:"):
			first, _, _ = strings.Cut(strings.TrimSpace(strings.TrimPrefix(l, "BootOrder:")), ",")
		case len(l) > 8 && strings.HasPrefix(l, "Boot") && (l[8] == '*' || l[8] == ' '):
			desc, _, _ := strings.Cut(strings.TrimSpace(l[9:]), "\t")
			entries[l[4:8]] = strings.TrimSpace(desc)
		}
	}
	if current == "" || first == "" || strings.EqualFold(current, first) {
		return ""
	}
	desc := ""
	if d := entries[first]; d != "" {
		desc = " (" + d + ")"
	}
	return fmt.Sprintf("the boot order starts with Boot%s%s, not with Boot%s, the network boot it came from", first, desc, current)
}

// addBoot keeps a new boot, and forgets the oldest beyond the bounds.
func (s *Server) addBoot(b *boot) {
	s.boots[b.ID] = b
	if b.Token != "" {
		s.tokens[b.Token] = b
	}
	var mine, all []*boot
	for _, o := range s.boots {
		all = append(all, o)
		if o.Machine == b.Machine {
			mine = append(mine, o)
		}
	}
	oldest := func(l []*boot) {
		sort.Slice(l, func(i, j int) bool { return l[i].Created.Before(l[j].Created) })
	}
	if len(mine) > bootsPerMachine {
		oldest(mine)
		for _, o := range mine[:len(mine)-bootsPerMachine] {
			s.dropBoot(o.ID)
		}
	}
	if len(s.boots) > maxBoots {
		oldest(all)
		for _, o := range all[:len(all)-maxBoots] {
			s.dropBoot(o.ID)
		}
	}
}

func unique(l []string) []string {
	var out []string
	for _, v := range l {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// randomID is 128 random bits, as 22 characters of URL-safe base64: a
// boot's ID, a token.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenHash is what a server keeps of a token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bootFile serves a boot's kernel, or its initrd: the boot config, then
// the rescue image's initrd. An armed rescue is consumed once its
// initrd was sent completely.
func (s *Server) bootFile(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	b := s.boots[r.PathValue("boot")]
	if b != nil && (!b.alive(s.now()) || b.rescue == nil) {
		b = nil
	}
	var path string
	var prefix []byte
	if b != nil {
		switch r.PathValue("file") {
		case "vmlinuz":
			path = b.rescue.Kernel
		case "initrd":
			path, prefix = b.rescue.Initrd, b.cpio
		}
	}
	s.mu.Unlock()
	if path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.logf("%s: %v", b.Machine, err)
		http.Error(w, "the file is not there", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "the file is not there", http.StatusInternalServerError)
		return
	}
	total := int64(len(prefix)) + st.Size()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	start := time.Now()
	n, err := send(w, prefix, f, func(n int64) {
		if n == total && prefix != nil {
			s.served(b, clientIP(r), n, time.Since(start))
		}
	})
	if err != nil {
		s.debugf("%s: %s of boot %s: %v after %s", b.Machine, r.PathValue("file"), b.ID, err, size(n))
	}
}

// send writes prefix and then f, each chunk within sendTimeout, so that
// a slow machine gets its initrd and a dead one does not hold the
// connection; written learns that all of it was written, before the
// last of it leaves the buffer.
func send(w http.ResponseWriter, prefix []byte, f *os.File, written func(n int64)) (int64, error) {
	rc := http.NewResponseController(w)
	deadline := func(t time.Time) { _ = rc.SetWriteDeadline(t) } // not every writer has one
	defer deadline(time.Time{})
	var n int64
	if len(prefix) > 0 {
		deadline(time.Now().Add(sendTimeout))
		m, err := w.Write(prefix)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	for {
		deadline(time.Now().Add(sendTimeout))
		m, err := io.CopyN(w, f, sendChunk)
		n += m
		if errors.Is(err, io.EOF) {
			written(n)
			deadline(time.Now().Add(sendTimeout))
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return n, err
			}
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// served notes that a boot's initrd was sent completely, and consumes
// its arm: in the journal at once, in the backend in the background.
func (s *Server) served(b *boot, to string, n int64, took time.Duration) {
	now := s.now()
	s.mu.Lock()
	b.Served = now
	consume := b.Arm != "" && b.Mode == ModeRescue && !s.consumed.Done(b.Arm)
	if consume {
		s.consumed[b.Arm] = &consumedArm{Machine: b.Machine, Boot: b.ID, At: now}
	}
	if sn := s.seen[b.MAC]; sn != nil {
		sn.Signals = s.signals(b.MAC, b.Machine, now)
	}
	s.saveJournal()
	s.signal()
	s.mu.Unlock()
	s.logf("%s: rescue %s sent to %s (%s in %s)", b.Machine, b.Version, to, size(n), took.Round(time.Second))
	if !consume {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		if err := s.putConsumed(context.Background(), b.Machine, b.Arm, b.ID, now); err != nil {
			s.logf("%s: the arm %s is consumed, and the backend did not take it (%v); the journal holds it until it does", b.Machine, ShortID(b.Arm), err)
			return
		}
		s.mu.Lock()
		if c := s.consumed[b.Arm]; c != nil {
			c.Written = true
		}
		s.saveJournal()
		s.mu.Unlock()
		s.logf("%s: the arm %s is consumed", b.Machine, ShortID(b.Arm))
	}()
}

// hello takes the rescue system's hello (ump-pxe hello) with its boot's
// token, and puts it into the status, where the CLI that armed the boot
// waits for it. Only a boot whose initrd was sent completely says
// hello, and an armed rescue only when it consumed the arm: anyone on
// the network can start a boot of a known MAC and read its token from
// the start of the initrd, but not take a rescue session without
// consuming its arm, which the CLI then sees.
func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "a boot's token is needed", http.StatusUnauthorized)
		return
	}
	h, err := ParseHello(http.MaxBytesReader(w, r.Body, maxHello))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "the hello is too big", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := s.now()
	s.mu.Lock()
	b := s.tokens[tokenHash(token)]
	switch {
	case b == nil || !b.alive(now):
		s.mu.Unlock()
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "no boot of this server has that token, or it expired", http.StatusUnauthorized)
		return
	case h.Boot != "" && h.Boot != b.ID:
		s.mu.Unlock()
		http.Error(w, "the token is another boot's", http.StatusForbidden)
		return
	case b.Served.IsZero():
		s.mu.Unlock()
		http.Error(w, "the initrd of this boot was not sent completely", http.StatusForbidden)
		return
	case b.Arm != "" && b.Mode == ModeRescue && (s.consumed[b.Arm] == nil || s.consumed[b.Arm].Boot != b.ID):
		s.mu.Unlock()
		s.logf("%s: a hello of boot %s refused: another boot consumed its arm %s", b.Machine, b.ID, ShortID(b.Arm))
		http.Error(w, "another boot consumed the arm of this boot", http.StatusForbidden)
		return
	}
	first := b.Hello.IsZero()
	b.Hello = now
	h.Boot, h.Arm, h.At = b.ID, b.Arm, now
	sn := s.seen[b.MAC]
	if sn == nil {
		sn = &Seen{Name: b.Machine, At: now, Answer: ModeRescue}
		s.seen[b.MAC] = sn
	}
	sn.Hello = h
	sn.Signals = s.signals(b.MAC, b.Machine, now)
	s.saveJournal()
	s.signal()
	s.mu.Unlock()
	if first {
		at := h.Address()
		if at == "" {
			at = "an unknown address"
		}
		arm := ""
		if b.Arm != "" {
			arm = " (arm " + ShortID(b.Arm) + ")"
		}
		s.logf("%s: rescue system up at %s%s", b.Machine, at, arm)
	}
	io.WriteString(w, "taken\n")
}

// ParseHello reads a hello as the rescue system posts it.
func ParseHello(r io.Reader) (*Hello, error) {
	var h Hello
	dec := json.NewDecoder(r)
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	if dec.More() {
		return nil, errors.New("hello: more than one JSON value")
	}
	if h.BootMAC != "" {
		mac, err := NormalizeMAC(h.BootMAC)
		if err != nil {
			return nil, fmt.Errorf("hello: boot_mac: %w", err)
		}
		h.BootMAC = mac
	}
	return &h, nil
}
