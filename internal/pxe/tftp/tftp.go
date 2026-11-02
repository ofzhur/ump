// Package tftp is a read-only TFTP server (RFC 1350) of the files under
// one directory, with the options blksize, tsize and timeout (RFC 2347,
// 2348, 2349): what a machine's firmware loads iPXE with when the
// site's DHCP server names no TFTP server of its own. It writes
// nothing, refuses write requests and every path outside its
// directory, answers each request from a port of its own, as RFC 1350
// has it, and never answers an error.
package tftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The opcodes.
const (
	opRRQ   = 1
	opWRQ   = 2
	opDATA  = 3
	opACK   = 4
	opERROR = 5
	opOACK  = 6
)

// The error codes.
const (
	errUndefined    = 0
	errNotFound     = 1
	errAccess       = 2
	errIllegal      = 4
	errUnknownTID   = 5
	errOptionDenied = 8
)

// The block sizes: RFC 1350's, the largest RFC 2348 allows, and the
// default bound, which keeps a block within an Ethernet frame (1500
// bytes less the IP, UDP and TFTP headers).
const (
	defaultBlockSize = 512
	maxBlockSize     = 65464
	DefaultMaxBlock  = 1468
)

// packet is a TFTP packet.
type packet struct {
	op uint16
	// a request's
	file, mode string
	options    map[string]string
	// DATA's and ACK's
	block uint16
	data  []byte
	// ERROR's
	code    uint16
	message string
}

// parse reads a packet. Option names are taken in lower case, a
// request's mode too; NUL bytes after a request's last field are
// padding.
func parse(b []byte) (*packet, error) {
	if len(b) < 2 {
		return nil, errors.New("short packet")
	}
	p := &packet{op: binary.BigEndian.Uint16(b)}
	rest := b[2:]
	switch p.op {
	case opRRQ, opWRQ, opOACK:
		fields, err := strs(rest)
		if err != nil {
			return nil, err
		}
		if p.op != opOACK {
			if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
				return nil, errors.New("a request without a file name and a mode")
			}
			p.file, p.mode = fields[0], strings.ToLower(fields[1])
			fields = fields[2:]
		}
		for i := 0; i < len(fields); i += 2 {
			name := strings.ToLower(fields[i])
			if name == "" {
				if strings.Join(fields[i:], "") == "" {
					break // padding
				}
				return nil, errors.New("an option without a name")
			}
			if i+1 == len(fields) {
				return nil, fmt.Errorf("option %s without a value", name)
			}
			if p.options == nil {
				p.options = map[string]string{}
			}
			if _, ok := p.options[name]; ok {
				return nil, fmt.Errorf("option %s twice", name)
			}
			p.options[name] = fields[i+1]
		}
	case opDATA, opACK:
		if len(rest) < 2 {
			return nil, errors.New("short packet")
		}
		p.block = binary.BigEndian.Uint16(rest)
		if p.op == opDATA {
			if len(rest)-2 > maxBlockSize {
				return nil, errors.New("a block larger than any block size")
			}
			p.data = rest[2:]
		}
	case opERROR:
		if len(rest) < 2 {
			return nil, errors.New("short packet")
		}
		p.code = binary.BigEndian.Uint16(rest)
		msg, _, _ := bytes.Cut(rest[2:], []byte{0})
		p.message = string(msg)
	default:
		return nil, fmt.Errorf("opcode %d", p.op)
	}
	return p, nil
}

// strs splits NUL-terminated strings; the last one must end too.
func strs(b []byte) ([]string, error) {
	if len(b) == 0 || b[len(b)-1] != 0 {
		return nil, errors.New("a request that does not end in NUL")
	}
	return strings.Split(string(b[:len(b)-1]), "\x00"), nil
}

func data(block uint16, b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint16(out, opDATA)
	binary.BigEndian.PutUint16(out[2:], block)
	return append(out, b...)
}

func ack(block uint16) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint16(out, opACK)
	binary.BigEndian.PutUint16(out[2:], block)
	return out
}

func errorPacket(code uint16, message string) []byte {
	out := make([]byte, 4, 5+len(message))
	binary.BigEndian.PutUint16(out, opERROR)
	binary.BigEndian.PutUint16(out[2:], code)
	return append(append(out, message...), 0)
}

// oack is an OACK of the options, in their order (name, value, ...).
func oack(options []string) []byte {
	out := []byte{0, opOACK}
	for _, o := range options {
		out = append(append(out, o...), 0)
	}
	return out
}

// Server serves the files under Dir.
type Server struct {
	Dir string
	// MaxBlock bounds the block size a client may ask for (default
	// DefaultMaxBlock); Timeout is the wait for an acknowledgement before
	// a block goes again (default 1 s; a client's timeout option wins),
	// Retries how often it goes again (default 5); MaxTransfers bounds
	// the transfers at a time (default 64).
	MaxBlock     int
	Timeout      time.Duration
	Retries      int
	MaxTransfers int
	// Logf logs a transfer, Debugf a detail; nil: not at all.
	Logf, Debugf func(format string, args ...any)

	root *os.Root
	busy chan struct{}
	wg   sync.WaitGroup
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

// Serve answers the requests that reach conn until ctx ends, then waits
// for the transfers under way to end (they are short, and bounded by
// the retries). The transfers go from the address conn
// listens on, at ports of their own.
func (s *Server) Serve(ctx context.Context, conn net.PacketConn) error {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	s.root = root
	if s.MaxBlock <= 0 || s.MaxBlock > maxBlockSize {
		s.MaxBlock = DefaultMaxBlock
	}
	if s.Timeout <= 0 {
		s.Timeout = time.Second
	}
	if s.Retries <= 0 {
		s.Retries = 5
	}
	if s.MaxTransfers <= 0 {
		s.MaxTransfers = 64
	}
	s.busy = make(chan struct{}, s.MaxTransfers)
	var local net.IP
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsUnspecified() {
		local = a.IP
	}
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 65536)
	for {
		n, from, err := conn.ReadFrom(buf)
		if ctx.Err() != nil {
			s.wg.Wait()
			return nil
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.wg.Wait()
			return err
		}
		// only a request is answered: no TFTP host answers anything with
		// a request, so no two hosts can answer each other's answers for
		// ever (an ERROR answered with an ERROR is CVE-2024-2169); the
		// rest is dropped
		p, err := parse(buf[:n])
		if err != nil {
			s.debugf("tftp %s: %v", host(from), err)
			if op := opcode(buf[:n]); op == opRRQ || op == opWRQ {
				conn.WriteTo(errorPacket(errIllegal, err.Error()), from)
			}
			continue
		}
		switch p.op {
		case opRRQ:
		case opWRQ:
			s.logf("tftp %s: %s: refused, the server is read-only", host(from), p.file)
			conn.WriteTo(errorPacket(errAccess, "this server is read-only"), from)
			continue
		default:
			s.debugf("tftp %s: opcode %d dropped, no request", host(from), p.op)
			continue
		}
		// a request sent again starts a transfer of its own, which the
		// client ends with an error (unknown transfer ID), as RFC 1350
		// has it
		select {
		case s.busy <- struct{}{}:
		default:
			conn.WriteTo(errorPacket(errUndefined, "too many transfers; try again"), from)
			continue
		}
		s.wg.Add(1)
		go func(from net.Addr) {
			defer func() {
				<-s.busy
				s.wg.Done()
			}()
			s.transfer(context.WithoutCancel(ctx), p, from, local)
		}(from)
	}
}

// opcode is a packet's opcode, 0 when it has none.
func opcode(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

func host(a net.Addr) string {
	if u, ok := a.(*net.UDPAddr); ok {
		return u.IP.String()
	}
	return a.String()
}

// clean is a requested path as a path under the directory: separators
// as slashes, without leading ones; empty when it leaves the directory.
func clean(name string) string {
	name = strings.TrimLeft(strings.ReplaceAll(name, `\`, "/"), "/")
	if name == "" || strings.ContainsRune(name, 0) {
		return ""
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return ""
		}
	}
	return name
}

// transfer sends a file to a client from a port of its own.
func (s *Server) transfer(ctx context.Context, req *packet, to net.Addr, local net.IP) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: local})
	if err != nil {
		s.logf("tftp %s: %v", host(to), err)
		return
	}
	defer conn.Close()
	t := &transfer{s: s, conn: conn, to: to, timeout: s.Timeout, size: defaultBlockSize}
	name := clean(req.file)
	if req.mode != "octet" {
		t.fail(errIllegal, "only octet mode is served")
		s.logf("tftp %s: %s: refused, mode %s", host(to), req.file, req.mode)
		return
	}
	if name == "" {
		t.fail(errAccess, "the path leaves the directory")
		s.logf("tftp %s: %s: refused, the path leaves the directory", host(to), req.file)
		return
	}
	f, err := s.root.Open(name)
	var st os.FileInfo
	if err == nil {
		if st, err = f.Stat(); err == nil && !st.Mode().IsRegular() {
			err = fs.ErrNotExist
		}
		if err != nil {
			f.Close()
		}
	}
	if err != nil {
		code, why := uint16(errAccess), "access violation"
		if errors.Is(err, fs.ErrNotExist) {
			code, why = errNotFound, "file not found"
		}
		t.fail(code, why)
		s.logf("tftp %s: %s: %s", host(to), name, why)
		return
	}
	defer f.Close()
	start := time.Now()
	var reply []string
	for _, o := range []string{"blksize", "tsize", "timeout"} { // RFC 2347's order is the client's; any is fine
		v, ok := req.options[o]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
		case o == "blksize" && n >= 8 && n <= maxBlockSize:
			t.size = min(n, s.MaxBlock)
			reply = append(reply, o, strconv.Itoa(t.size))
		case o == "tsize" && n >= 0:
			reply = append(reply, o, strconv.FormatInt(st.Size(), 10))
		case o == "timeout" && n >= 1 && n <= 255:
			t.timeout = time.Duration(n) * time.Second
			reply = append(reply, o, v)
		}
	}
	if len(reply) > 0 {
		// the options first; an error in answer is a client that only
		// asked for the size (EDK2 does), not a failure
		if err := t.exchange(ctx, oack(reply), 0); err != nil {
			var pe *peerError
			if errors.As(err, &pe) {
				s.debugf("tftp %s: %s: the client ended after the options (%s)", host(to), name, pe.message)
				return
			}
			s.logf("tftp %s: %s: %v", host(to), name, err)
			return
		}
	}
	buf := make([]byte, t.size)
	var sent int64
	for block := uint16(1); ; block++ {
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.fail(errUndefined, "cannot read the file")
			s.logf("tftp %s: %s: %v", host(to), name, err)
			return
		}
		if err := t.exchange(ctx, data(block, buf[:n]), block); err != nil {
			s.logf("tftp %s: %s: %v after %d bytes", host(to), name, err, sent)
			return
		}
		sent += int64(n)
		if n < t.size {
			break
		}
	}
	s.logf("tftp %s: %s sent (%d bytes in %s)", host(to), name, sent, time.Since(start).Round(time.Millisecond))
}

// transfer is one transfer's side of the server.
type transfer struct {
	s       *Server
	conn    *net.UDPConn
	to      net.Addr // the client's address: its transfer ID
	timeout time.Duration
	size    int
}

// peerError is an ERROR the client sent.
type peerError struct {
	code    uint16
	message string
}

func (e *peerError) Error() string {
	return fmt.Sprintf("the client ended the transfer (%d: %s)", e.code, e.message)
}

func (t *transfer) fail(code uint16, message string) {
	t.conn.WriteTo(errorPacket(code, message), t.to)
}

// exchange sends a packet until the client acknowledges block: again
// after each timeout, never again for an acknowledgement of the block
// before (that would double every block from there on, the Sorcerer's
// Apprentice bug).
func (t *transfer) exchange(ctx context.Context, pkt []byte, block uint16) error {
	buf := make([]byte, 65536)
	for try := 0; try <= t.s.Retries; try++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := t.conn.WriteTo(pkt, t.to); err != nil {
			return err
		}
		t.conn.SetReadDeadline(time.Now().Add(t.timeout))
		for {
			n, from, err := t.conn.ReadFrom(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // send it again
				}
				return err
			}
			if from.String() != t.to.String() {
				// RFC 1350 answers a stranger with an error, but never
				// an error: two transfers would answer each other
				if opcode(buf[:n]) != opERROR {
					t.conn.WriteTo(errorPacket(errUnknownTID, "unknown transfer ID"), from)
				}
				continue
			}
			p, err := parse(buf[:n])
			if err != nil {
				continue
			}
			switch {
			case p.op == opERROR:
				return &peerError{code: p.code, message: p.message}
			case p.op == opACK && p.block == block:
				return nil
			}
			// an acknowledgement of an earlier block, or something else:
			// waited out, not answered
		}
	}
	return fmt.Errorf("no acknowledgement of block %d after %d tries", block, t.s.Retries+1)
}
