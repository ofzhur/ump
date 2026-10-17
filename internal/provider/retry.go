package provider

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// handshakeWaits are the pauses before sending a request again whose
// connection failed in the TLS handshake.
var handshakeWaits = []time.Duration{time.Second, 3 * time.Second}

// handshakeRetry sends a request again when its connection failed in the
// TLS handshake: the request had not gone out, so any method may be sent
// again. OVHcloud's API front end now and then refuses a handshake with
// "remote error: tls: record overflow" and takes the next one.
type handshakeRetry struct {
	next  http.RoundTripper
	waits []time.Duration
}

func (t *handshakeRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	for i := 0; ; i++ {
		var failed atomic.Bool
		trace := &httptrace.ClientTrace{TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				failed.Store(true)
			}
		}}
		resp, err := t.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
		if err == nil || !failed.Load() || i >= len(t.waits) {
			return resp, err
		}
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return resp, err
			}
			body, berr := req.GetBody()
			if berr != nil {
				return resp, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
		select {
		case <-req.Context().Done():
			return nil, err
		case <-time.After(t.waits[i]):
		}
	}
}
