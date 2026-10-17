package provider

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// alertingListener answers the first n connections' ClientHello with a
// TLS record_overflow alert, as OVHcloud's front end now and then does.
type alertingListener struct {
	net.Listener
	mu sync.Mutex
	n  int
}

func (l *alertingListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		refuse := l.n > 0
		if refuse {
			l.n--
		}
		l.mu.Unlock()
		if !refuse {
			return c, nil
		}
		go func() {
			buf := make([]byte, 4096)
			c.Read(buf)
			// alert record: fatal (2), record_overflow (22)
			c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x16})
			c.Close()
		}()
	}
}

func TestHandshakeRetry(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	al := &alertingListener{Listener: srv.Listener, n: 1}
	srv.Listener = al
	srv.StartTLS()
	defer srv.Close()

	base := srv.Client()
	base.Transport.(*http.Transport).DisableKeepAlives = true
	var logged []string
	c := Options{HTTPClient: base, Logf: func(f string, a ...any) { logged = append(logged, f) }}.Client()
	c.Transport.(*handshakeRetry).waits = []time.Duration{10 * time.Millisecond}

	// a POST whose handshake failed goes again, with its body
	resp, err := c.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST after a refused handshake: %v", err)
	}
	resp.Body.Close()
	if calls.Load() != 1 || bodies[0] != "hello" || len(logged) != 2 {
		t.Errorf("calls %d, bodies %q, logged tries %d", calls.Load(), bodies, len(logged))
	}

	// refused more often than there are retries: the error comes through
	al.mu.Lock()
	al.n = 2
	al.mu.Unlock()
	if _, err := c.Get(srv.URL); err == nil || !strings.Contains(err.Error(), "record overflow") {
		t.Errorf("GET refused twice: %v", err)
	}

	// an error after the handshake is not a reason to send again
	calls.Store(0)
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv2.Close()
	c2 := Options{HTTPClient: srv2.Client()}.Client()
	c2.Transport.(*handshakeRetry).waits = []time.Duration{10 * time.Millisecond}
	if _, err := c2.Post(srv2.URL, "text/plain", strings.NewReader("once")); err == nil {
		t.Error("a dropped connection after the request went out came back fine")
	}
	if calls.Load() != 1 {
		t.Errorf("a request that went out was sent %d times", calls.Load())
	}
}
