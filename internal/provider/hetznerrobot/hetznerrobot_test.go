package hetznerrobot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

const serverDetailJSON = `{"server": {"server_ip": "203.0.113.10", "server_ipv6_net": "2001:db8:111:4221::", "server_number": 321,
  "server_name": "web-01", "product": "AX41-NVMe", "dc": "NBG1-DC1", "traffic": "unlimited", "status": "ready",
  "cancelled": false, "paid_until": "2026-11-01", "ip": ["203.0.113.10", "203.0.113.50"],
  "subnet": [{"ip": "2001:db8:111:4221::", "mask": "64"}], "reset": true, "rescue": true, "vnc": false,
  "windows": false, "plesk": false, "cpanel": false, "wol": true, "hot_swap": true, "linked_storagebox": 12345}}`

func TestDescribe(t *testing.T) {
	resetStatus := http.StatusOK
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/321":
			w.Write([]byte(serverDetailJSON))
		case "/reset/321":
			if resetStatus != http.StatusOK {
				w.WriteHeader(resetStatus)
				w.Write([]byte(`{"error":{"status":404,"code":"RESET_NOT_AVAILABLE","message":"Reset not available"}}`))
				return
			}
			w.Write([]byte(`{"reset":{"server_ip":"203.0.113.10","server_number":321,"type":["sw","hw","man"],"operating_status":"running"}}`))
		case "/boot/321/rescue":
			w.Write([]byte(`{"rescue":{"server_ip":"203.0.113.10","server_number":321,"os":["linux","linuxold","vkvm"],"arch":[64],
			  "active":true,"password":"secret","authorized_key":[{"key":{"name":"laptop","fingerprint":"aa:bb","type":"ED25519","size":256}}],"host_key":[]}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	d, err := inv.(provider.Describer).Describe(context.Background(), provider.Machine{ID: "321"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "web-01" || d.IPv6 != "2001:db8:111:4221::/64" || d.Account != "main" {
		t.Errorf("machine: %+v", d.Machine)
	}
	names := []string{}
	for _, s := range d.Sections {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "Contract,Addresses,Features,Power,Rescue" {
		t.Fatalf("sections: %v", names)
	}
	want := map[string]any{
		"Contract/paid_until": "2026-11-01", "Contract/linked_storagebox": 12345,
		"Addresses/additional_ips": []string{"203.0.113.50"}, "Addresses/subnets": []string{"2001:db8:111:4221::/64"},
		"Features/wol": true, "Features/vnc": false,
		"Power/state": "running", "Power/reset_types": []string{"sw", "hw", "man"},
		"Rescue/active": true, "Rescue/architectures": []string{"64"}, "Rescue/authorized_keys": []string{"laptop (aa:bb)"},
	}
	for path, w := range want {
		if got := field(d, path); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %#v, want %#v", path, got, w)
		}
	}
	for _, s := range d.Sections {
		for _, it := range s.Items {
			if strings.Contains(strings.ToLower(it.Key), "password") {
				t.Errorf("rescue password must not be described: %s/%s", s.Name, it.Key)
			}
		}
	}

	resetStatus = http.StatusNotFound
	d, err = inv.(provider.Describer).Describe(context.Background(), provider.Machine{ID: "321"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sections {
		if s.Name == "Power" {
			t.Error("Power section present although reset is not available")
		}
	}
	if _, err := inv.(provider.Describer).Describe(context.Background(), provider.Machine{ID: "web"}); err == nil {
		t.Error("non-numeric ID accepted")
	}
}

func field(d *provider.Details, path string) any {
	section, key, _ := strings.Cut(path, "/")
	for _, s := range d.Sections {
		if s.Name != section {
			continue
		}
		for _, it := range s.Items {
			if it.Key == key {
				return it.Value
			}
		}
	}
	return nil
}

func TestRename(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/server/321" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type %q", ct)
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("server_name") != "prod-web-01" {
			t.Errorf("form: %v %v", r.PostForm, err)
		}
		w.Write([]byte(strings.Replace(serverDetailJSON, `"server_name": "web-01"`, `"server_name": "prod-web-01"`, 1)))
	})
	m, err := inv.(provider.Renamer).Rename(context.Background(), provider.Machine{ID: "321"}, "prod-web-01")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "prod-web-01" || m.ID != "321" || m.Account != "main" {
		t.Errorf("renamed machine: %+v", m)
	}
	if _, err := inv.(provider.Renamer).Rename(context.Background(), provider.Machine{ID: "x"}, "n"); err == nil {
		t.Error("non-numeric ID accepted")
	}
}

func TestRenameRejected(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"status":400,"code":"INVALID_INPUT","message":"invalid input","invalid":["server_name"]}}`))
	})
	_, err := inv.(provider.Renamer).Rename(context.Background(), provider.Machine{ID: "321"}, "bad name")
	if err == nil || err.Error() != "hetzner-robot: the Robot API rejected the input: invalid input (INVALID_INPUT, HTTP 400)" {
		t.Errorf("unexpected error: %v", err)
	}
}
