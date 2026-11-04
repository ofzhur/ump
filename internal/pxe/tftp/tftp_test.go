package tftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func request(op uint16, file, mode string, options ...string) []byte {
	out := []byte{0, byte(op)}
	for _, f := range append([]string{file, mode}, options...) {
		out = append(append(out, f...), 0)
	}
	return out
}

// logs collects what a server logs.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// serve runs a server of dir on a port of the loopback; it stops with
// the test.
func serve(t *testing.T, dir string, timeout time.Duration) (*net.UDPAddr, *logs, *logs) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	info, debug := &logs{}, &logs{}
	s := &Server{Dir: dir, Timeout: timeout, Retries: 3, Logf: info.logf, Debugf: debug.logf}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		pc.Close()
	})
	return pc.LocalAddr().(*net.UDPAddr), info, debug
}

// client is a TFTP client of the tests.
type client struct {
	t    *testing.T
	conn *net.UDPConn
}

func newClient(t *testing.T) *client {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn}
}

func (c *client) send(b []byte, to net.Addr) {
	c.t.Helper()
	if _, err := c.conn.WriteTo(b, to); err != nil {
		c.t.Fatal(err)
	}
}

// recv is the next packet within d, nil after it.
func (c *client) recv(d time.Duration) (*packet, net.Addr) {
	c.t.Helper()
	buf := make([]byte, 65536)
	c.conn.SetReadDeadline(time.Now().Add(d))
	n, from, err := c.conn.ReadFrom(buf)
	if err != nil {
		return nil, nil
	}
	p, err := parse(slices.Clone(buf[:n]))
	if err != nil {
		c.t.Fatalf("a packet that does not parse: %v", err)
	}
	return p, from
}

// get reads a file as a client does: the options the server took, the
// file, and the error the server sent.
func (c *client) get(server net.Addr, file string, options ...string) (map[string]string, []byte, error) {
	c.t.Helper()
	c.send(request(opRRQ, file, "octet", options...), server)
	var got []byte
	var opts map[string]string
	size := 512
	want := uint16(1)
	for {
		p, from := c.recv(2 * time.Second)
		switch {
		case p == nil:
			return opts, got, errors.New("no answer")
		case p.op == opERROR:
			return opts, got, fmt.Errorf("%d: %s", p.code, p.message)
		case p.op == opOACK:
			opts = p.options
			if v, ok := opts["blksize"]; ok {
				fmt.Sscan(v, &size)
			}
			c.send(ack(0), from)
		case p.op == opDATA && p.block == want:
			got = append(got, p.data...)
			c.send(ack(p.block), from)
			if len(p.data) < size {
				return opts, got, nil
			}
			want++
		default:
			return opts, got, fmt.Errorf("unexpected packet %+v", p)
		}
	}
}

func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "undionly.kpxe"), content(1300), 0o644)
	os.WriteFile(filepath.Join(dir, "even.efi"), content(1024), 0o644)
	os.MkdirAll(filepath.Join(dir, "arm64-efi"), 0o755)
	os.WriteFile(filepath.Join(dir, "arm64-efi", "snponly.efi"), content(3000), 0o644)
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("secret"), 0o600)
	os.Symlink(outside, filepath.Join(dir, "escape"))
	server, info, debug := serve(t, dir, time.Second)

	c := newClient(t)
	// blocks of 512 bytes without options
	opts, got, err := c.get(server, "undionly.kpxe")
	if err != nil || !bytes.Equal(got, content(1300)) || opts != nil {
		t.Errorf("undionly.kpxe: %v, %d bytes, options %v", err, len(got), opts)
	}
	// a file of whole blocks ends with an empty one
	if _, got, err := c.get(server, "even.efi"); err != nil || !bytes.Equal(got, content(1024)) {
		t.Errorf("even.efi: %v, %d bytes", err, len(got))
	}
	// the options: blksize bounded, tsize the size, timeout echoed,
	// windowsize ignored
	opts, got, err = c.get(server, "/arm64-efi/snponly.efi", "blksize", "9000", "tsize", "0", "timeout", "2", "windowsize", "4")
	if err != nil || !bytes.Equal(got, content(3000)) {
		t.Errorf("snponly.efi: %v, %d bytes", err, len(got))
	}
	if want := map[string]string{"blksize": "1468", "tsize": "3000", "timeout": "2"}; fmt.Sprint(opts) != fmt.Sprint(want) {
		t.Errorf("options %v, want %v", opts, want)
	}
	// separators of other systems, and leading ones
	if _, got, err := c.get(server, `\arm64-efi\snponly.efi`); err != nil || len(got) != 3000 {
		t.Errorf("backslashes: %v, %d bytes", err, len(got))
	}

	for _, r := range []struct{ file, mode, err string }{
		{"nope.efi", "octet", "1: file not found"},
		{"arm64-efi", "octet", "1: file not found"},
		{"../secret", "octet", "2: the path leaves the directory"},
		{"arm64-efi/../undionly.kpxe", "octet", "2: the path leaves the directory"},
		{"escape", "octet", "2: access violation"},
		{"undionly.kpxe", "netascii", "4: only octet mode is served"},
		{"undionly.kpxe", "OCTET", ""},
	} {
		c.send(request(opRRQ, r.file, r.mode), server)
		p, from := c.recv(2 * time.Second)
		switch {
		case p == nil:
			t.Errorf("%s (%s): no answer", r.file, r.mode)
		case r.err == "" && (p.op != opDATA || p.block != 1):
			t.Errorf("%s (%s): %+v", r.file, r.mode, p)
		case r.err != "" && (p.op != opERROR || fmt.Sprintf("%d: %s", p.code, p.message) != r.err):
			t.Errorf("%s (%s): %+v, want %s", r.file, r.mode, p, r.err)
		}
		if r.err == "" {
			c.send(errorPacket(errUndefined, "enough"), from)
		}
	}

	// writing is refused, from port 69 itself
	c.send(request(opWRQ, "new.efi", "octet"), server)
	if p, from := c.recv(2 * time.Second); p == nil || p.op != opERROR || p.code != errAccess || from.String() != server.String() {
		t.Errorf("a write request: %+v from %v", p, from)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.efi")); err == nil {
		t.Error("a write request wrote")
	}
	// a request that does not parse is refused; a packet that is no
	// request, or no packet, is dropped
	c.send([]byte{0, opRRQ, 'a'}, server)
	if p, _ := c.recv(2 * time.Second); p == nil || p.op != opERROR || p.code != errIllegal {
		t.Errorf("a request that does not parse: %+v", p)
	}
	for _, b := range [][]byte{ack(1), data(1, []byte("x")), errorPacket(errUndefined, "loop?"), oack([]string{"tsize", "1"}), {0, 9}, {0, opERROR}, {7}} {
		c.send(b, server)
		if p, _ := c.recv(200 * time.Millisecond); p != nil {
			t.Errorf("%q to port 69: answered %+v", b, p)
		}
	}

	// the size alone, as EDK2 asks for it: an error after the options is
	// no failure
	c.send(request(opRRQ, "undionly.kpxe", "octet", "tsize", "0"), server)
	p, from := c.recv(2 * time.Second)
	if p == nil || p.op != opOACK || p.options["tsize"] != "1300" {
		t.Fatalf("the size: %+v", p)
	}
	c.send(errorPacket(errOptionDenied, "size probe"), from)
	if p, _ := c.recv(300 * time.Millisecond); p != nil {
		t.Errorf("after the error: %+v", p)
	}
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(debug.String(), "undionly.kpxe: the client ended after the options (size probe)") {
		t.Errorf("the debug log:\n%s", debug)
	}
	log := info.String()
	for _, want := range []string{"tftp 127.0.0.1: undionly.kpxe sent (1300 bytes in ", "tftp 127.0.0.1: nope.efi: file not found", "tftp 127.0.0.1: new.efi: refused, the server is read-only"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "size probe") {
		t.Errorf("a size probe logged as a failure:\n%s", log)
	}
}

func TestServeRetransmits(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ipxe.efi"), content(1300), 0o644)
	server, info, _ := serve(t, dir, 200*time.Millisecond)
	c := newClient(t)
	c.send(request(opRRQ, "ipxe.efi", "octet"), server)
	p, tid := c.recv(time.Second)
	if p == nil || p.op != opDATA || p.block != 1 {
		t.Fatalf("block 1: %+v", p)
	}
	// unacknowledged, a block goes again after the timeout
	if p, _ := c.recv(time.Second); p == nil || p.block != 1 {
		t.Fatalf("block 1 again: %+v", p)
	}
	c.send(ack(1), tid)
	if p, _ := c.recv(time.Second); p == nil || p.block != 2 {
		t.Fatalf("block 2: %+v", p)
	}
	// the acknowledgement of block 1 again is not answered with block 2
	// again: only the timeout sends it again
	start := time.Now()
	c.send(ack(1), tid)
	p, _ = c.recv(time.Second)
	if p == nil || p.block != 2 || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("after a duplicate ACK: %+v after %s", p, time.Since(start))
	}
	// another port is not the transfer's; its error is not answered
	other := newClient(t)
	other.send(ack(2), tid)
	if p, _ := other.recv(time.Second); p == nil || p.op != opERROR || p.code != errUnknownTID {
		t.Errorf("another port: %+v", p)
	}
	other.send(errorPacket(errUnknownTID, "unknown transfer ID"), tid)
	if p, _ := other.recv(100 * time.Millisecond); p != nil {
		t.Errorf("another port's error: answered %+v", p)
	}
	c.send(ack(2), tid)
	if p, _ := c.recv(time.Second); p == nil || p.block != 3 || len(p.data) != 276 {
		t.Fatalf("block 3: %+v", p)
	}
	c.send(ack(3), tid)
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(info.String(), "ipxe.efi sent (1300 bytes") {
		t.Errorf("the log:\n%s", info)
	}

	// a client that goes: the transfer ends after the retries
	c.send(request(opRRQ, "ipxe.efi", "octet"), server)
	for range 4 {
		if p, _ := c.recv(time.Second); p == nil || p.block != 1 {
			t.Fatalf("block 1, tried again: %+v", p)
		}
	}
	if p, _ := c.recv(400 * time.Millisecond); p != nil {
		t.Errorf("a fifth try: %+v", p)
	}
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(info.String(), "ipxe.efi: no acknowledgement of block 1 after 4 tries after 0 bytes") {
		t.Errorf("the log:\n%s", info)
	}
}

// TestServeNoLoop: an error between two servers' ports, as a stranger
// can send it with the address of the one, is answered by neither (the
// loop of CVE-2024-2169).
func TestServeNoLoop(t *testing.T) {
	var conns [2]net.PacketConn
	var counts [2]*counter
	for i := range conns {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		counts[i] = &counter{PacketConn: pc}
		conns[i] = counts[i]
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- (&Server{Dir: t.TempDir()}).Serve(ctx, conns[i]) }()
		t.Cleanup(func() {
			cancel()
			<-done
			pc.Close()
		})
	}
	// as from the second server's port to the first's
	if _, err := conns[1].WriteTo(errorPacket(errIllegal, "a request is a read request here"), conns[0].LocalAddr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if a, b := counts[0].read(), counts[1].read(); a != 1 || b != 0 {
		t.Errorf("after one error the servers read %d and %d packets, want 1 and 0", a, b)
	}
}

// counter counts the packets a server reads.
type counter struct {
	net.PacketConn
	mu sync.Mutex
	n  int
}

func (c *counter) ReadFrom(b []byte) (int, net.Addr, error) {
	n, from, err := c.PacketConn.ReadFrom(b)
	if err == nil {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return n, from, err
}

func (c *counter) read() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
		err  string
	}{
		{request(opRRQ, "a.efi", "Octet", "BLKSIZE", "1468"), "1 a.efi octet map[blksize:1468]", ""},
		{append(request(opRRQ, "a.efi", "octet"), 0, 0), "1 a.efi octet map[]", ""},
		{request(opRRQ, "a.efi", "octet", "tsize", ""), "1 a.efi octet map[tsize:]", ""},
		{request(opRRQ, "a.efi", "octet", "tsize"), "", "option tsize without a value"},
		{request(opRRQ, "a.efi", "octet", "tsize", "0", "TSIZE", "0"), "", "option tsize twice"},
		{request(opRRQ, "a.efi", "octet", "", "0"), "", "an option without a name"},
		{request(opRRQ, "", "octet"), "", "a request without a file name and a mode"},
		{[]byte{0, 1, 'a'}, "", "a request that does not end in NUL"},
		{[]byte{0, 3, 0, 7, 'x'}, "3 block 7 x", ""},
		{[]byte{0, 4, 0}, "", "short packet"},
		{[]byte{0, 5, 0, 1, 'n', 'o', 0}, "5 error 1 no", ""},
		{[]byte{0, 9}, "", "opcode 9"},
		{[]byte{1}, "", "short packet"},
	} {
		p, err := parse(c.in)
		got := ""
		if p != nil {
			switch p.op {
			case opRRQ, opWRQ:
				got = fmt.Sprintf("%d %s %s %v", p.op, p.file, p.mode, sortedMap(p.options))
			case opDATA:
				got = fmt.Sprintf("%d block %d %s", p.op, p.block, p.data)
			case opERROR:
				got = fmt.Sprintf("%d error %d %s", p.op, p.code, p.message)
			}
		}
		if got != c.want || (err == nil) != (c.err == "") || err != nil && err.Error() != c.err {
			t.Errorf("parse(%q): %q, %v; want %q, %q", c.in, got, err, c.want, c.err)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"/undionly.kpxe", "undionly.kpxe"}, {`\\x\y.efi`, "x/y.efi"}, {"a/../b", ""}, {"..", ""}, {"/", ""}, {"a\x00b", ""},
	} {
		if got := clean(c.in); got != c.want {
			t.Errorf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func sortedMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// FuzzParse: no packet makes the parser panic, and what it reads it
// reads again the same once written back.
func FuzzParse(f *testing.F) {
	for _, seed := range [][]byte{
		request(opRRQ, "undionly.kpxe", "octet"),
		request(opRRQ, "snponly.efi", "octet", "blksize", "1468", "tsize", "0", "windowsize", "4"),
		request(opWRQ, "x", "netascii"),
		append(request(opRRQ, "a", "octet"), 0, 0, 0),
		data(1, []byte("hello")),
		ack(65535),
		errorPacket(errOptionDenied, "size probe"),
		oack([]string{"blksize", "1468"}),
		{0, 1}, {0, 5, 0}, {},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := parse(b)
		if err != nil {
			return
		}
		var again []byte
		switch p.op {
		case opRRQ, opWRQ:
			var opts []string
			names := make([]string, 0, len(p.options))
			for k := range p.options {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				opts = append(opts, k, p.options[k])
			}
			again = request(p.op, p.file, p.mode, opts...)
		case opDATA:
			again = data(p.block, p.data)
		case opACK:
			again = ack(p.block)
		case opERROR:
			again = errorPacket(p.code, p.message)
		case opOACK:
			return
		}
		q, err := parse(again)
		if err != nil {
			t.Fatalf("%q parses, written back as %q it does not: %v", b, again, err)
		}
		if q.op != p.op || q.file != p.file || q.mode != p.mode || q.block != p.block || !bytes.Equal(q.data, p.data) ||
			q.code != p.code || q.message != p.message || fmt.Sprint(q.options) != fmt.Sprint(p.options) {
			t.Fatalf("%q: %+v, written back and read again %+v", b, p, q)
		}
	})
}
