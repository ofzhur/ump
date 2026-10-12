package hetznerrobot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ofzhur/ump/internal/provider"
)

const serversJSON = `[
  {"server": {"server_ip": "203.0.113.10", "server_ipv6_net": "2001:db8:111:4221::", "server_number": 321,
    "server_name": "web-01", "product": "AX41-NVMe", "dc": "NBG1-DC1", "traffic": "unlimited", "status": "ready",
    "cancelled": false, "paid_until": "2026-11-01", "ip": ["203.0.113.10"],
    "subnet": [{"ip": "2001:db8:111:4221::", "mask": "64"}], "linked_storagebox": null}},
  {"server": {"server_ip": "203.0.113.11", "server_ipv6_net": "", "server_number": 322,
    "server_name": "", "product": "Server Auction", "dc": "HEL1-DC12", "traffic": "unlimited", "status": "in process",
    "cancelled": true, "paid_until": "2026-10-09", "ip": ["203.0.113.11"], "subnet": [], "linked_storagebox": null}}
]`

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, provider.Inventory) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p, err := Type.New("main", map[string]string{"login": "#ws+user", "password": "pass", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p.(provider.Inventory)
}

func TestListServers(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "#ws+user" || pass != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"status":401,"code":"UNAUTHORIZED","message":"Unauthorized"}}`))
			return
		}
		if r.URL.Path != "/server" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(serversJSON))
	})
	ms, err := inv.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d machines, want 2", len(ms))
	}
	want := provider.Machine{Provider: "hetzner-robot", Account: "main", ID: "321", Name: "web-01", Kind: provider.KindBareMetal,
		IPv4: "203.0.113.10", IPv6: "2001:db8:111:4221::/64", Location: "nbg1-dc1", Product: "AX41-NVMe", Status: "ready"}
	if !reflect.DeepEqual(ms[0], want) {
		t.Errorf("machine 0:\n got %+v\nwant %+v", ms[0], want)
	}
	if ms[1].Name != "322" || ms[1].Status != "cancelled" || ms[1].IPv6 != "" || ms[1].Location != "hel1-dc12" {
		t.Errorf("machine 1: %+v", ms[1])
	}
}

func TestUnauthorized(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"status":401,"code":"UNAUTHORIZED","message":"Unauthorized"}}`))
	})
	_, err := inv.List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Status != 401 || apiErr.Code != "UNAUTHORIZED" {
		t.Errorf("unexpected error: %+v", apiErr)
	}
	if want := "hetzner-robot: authentication failed (check the web service login and password) (UNAUTHORIZED, HTTP 401)"; err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

func TestNoServers(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"status":404,"code":"SERVER_NOT_FOUND","message":"No servers found"}}`))
	})
	ms, err := inv.List(context.Background())
	if err != nil || len(ms) != 0 {
		t.Errorf("got %v, %v; want empty list without error", ms, err)
	}
}

func TestRateLimit(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"status":403,"code":"RATE_LIMIT_EXCEEDED","message":"rate limit exceeded"}}`))
	})
	_, err := inv.List(context.Background())
	if err == nil || err.Error() != "hetzner-robot: rate limit exceeded (the Robot API allows 200 requests per hour) (RATE_LIMIT_EXCEEDED, HTTP 403)" {
		t.Errorf("unexpected error: %v", err)
	}
}
