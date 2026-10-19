package hetznerrobot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

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
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/server":
			w.Write([]byte(serversJSON))
		case "/reset/321":
			// the power state is asked per server (the cancelled 322 is not asked)
			w.Write([]byte(`{"reset": {"server_ip": "203.0.113.10", "server_number": 321, "type": ["sw", "hw", "man"], "operating_status": "running"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ms, err := inv.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d machines, want 2", len(ms))
	}
	want := provider.Machine{Provider: "hetzner-robot", Account: "main", ID: "321", Name: "web-01", Kind: provider.KindBareMetal,
		IPv4: "203.0.113.10", IPv6: "2001:db8:111:4221::/64", Location: "nbg1-dc1", Product: "AX41-NVMe", Status: "running"}
	if !reflect.DeepEqual(ms[0], want) {
		t.Errorf("machine 0:\n got %+v\nwant %+v", ms[0], want)
	}
	if ms[1].Name != "322" || ms[1].Status != "cancelled" || ms[1].IPv6 != "" || ms[1].Location != "hel1-dc12" {
		t.Errorf("machine 1: %+v", ms[1])
	}
}

func TestListStatusWithoutPowerState(t *testing.T) {
	for _, c := range []struct {
		name, reset string
		code        int
		want        string
	}{
		{"off", `{"reset": {"server_number": 321, "operating_status": "shut off"}}`, 200, "off"},
		{"not supported", `{"reset": {"server_number": 321, "operating_status": "not supported"}}`, 200, "ready"},
		{"call refused", `{"error":{"status":403,"code":"RATE_LIMIT_EXCEEDED","message":"limit"}}`, 403, "ready"},
	} {
		_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/reset/321" {
				w.WriteHeader(c.code)
				w.Write([]byte(c.reset))
				return
			}
			w.Write([]byte(serversJSON))
		})
		ms, err := inv.List(context.Background())
		if err != nil || ms[0].Status != c.want || ms[1].Status != "cancelled" {
			t.Errorf("%s: %v, statuses %s/%s, want %s/cancelled", c.name, err, ms[0].Status, ms[1].Status, c.want)
		}
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

func TestReverseDNS(t *testing.T) {
	entries := map[string]string{"203.0.113.10": "web-01.example.net", "2001:db8:111:4221::2": "web-01-v6.example.net", "198.51.100.9": "other.example.net"}
	var log []string
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		log = append(log, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/server/321":
			w.Write([]byte(serverDetailJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/rdns":
			var items []string
			for ip, ptr := range entries {
				items = append(items, `{"rdns":{"ip":"`+ip+`","ptr":"`+ptr+`"}}`)
			}
			w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rdns/"):
			ip := strings.TrimPrefix(r.URL.Path, "/rdns/")
			if _, ok := entries[ip]; !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":{"status":404,"code":"RDNS_NOT_FOUND","message":"rdns not found"}}`))
				return
			}
			r.ParseForm()
			entries[ip] = r.PostForm.Get("ptr")
			w.Write([]byte(`{"rdns":{"ip":"` + ip + `","ptr":"` + entries[ip] + `"}}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/rdns/"):
			ip := strings.TrimPrefix(r.URL.Path, "/rdns/")
			r.ParseForm()
			entries[ip] = r.PostForm.Get("ptr")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"rdns":{"ip":"` + ip + `","ptr":"` + entries[ip] + `"}}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rdns/"):
			ip := strings.TrimPrefix(r.URL.Path, "/rdns/")
			if _, ok := entries[ip]; !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":{"status":404,"code":"RDNS_NOT_FOUND","message":"rdns not found"}}`))
				return
			}
			delete(entries, ip)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	rd := inv.(provider.ReverseDNS)
	m := provider.Machine{ID: "321", IPv4: "203.0.113.10"}
	ptrs, err := rd.PTRs(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.PTR{{IP: "203.0.113.10", Name: "web-01.example.net"}, {IP: "2001:db8:111:4221::2", Name: "web-01-v6.example.net"}}
	if !reflect.DeepEqual(ptrs, want) {
		t.Errorf("PTRs: got %+v, want %+v", ptrs, want)
	}

	// a new entry on an additional IP goes through POST, then PUT
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("203.0.113.50"), "extra.example.net"); err != nil {
		t.Fatal(err)
	}
	if entries["203.0.113.50"] != "extra.example.net" {
		t.Errorf("entry not created: %v", entries)
	}
	// an existing entry is updated with POST alone
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("203.0.113.10"), "new.example.net"); err != nil {
		t.Fatal(err)
	}
	// removal, and removal of a missing entry
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("2001:db8:111:4221::2"), ""); err != nil {
		t.Fatal(err)
	}
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("2001:db8:111:4221::7"), ""); err != nil {
		t.Fatal(err)
	}
	if entries["203.0.113.10"] != "new.example.net" || entries["2001:db8:111:4221::2"] != "" {
		t.Errorf("entries after updates: %v", entries)
	}
	// a foreign address is refused before any call
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("198.51.100.9"), "x.example.net"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign address: %v", err)
	}
	// the entry list is fetched again after a change
	ptrs, _ = rd.PTRs(context.Background(), m)
	if ptrs[0].Name != "new.example.net" || len(ptrs) != 2 || ptrs[1].IP != "203.0.113.50" {
		t.Errorf("PTRs after changes: %+v", ptrs)
	}
	count := map[string]int{}
	for _, l := range log {
		count[l]++
	}
	if count["GET /rdns"] != 2 || count["PUT /rdns/203.0.113.50"] != 1 || count["POST /rdns/203.0.113.50"] != 1 || count["POST /rdns/203.0.113.10"] != 1 {
		t.Errorf("requests: %v", log)
	}
}

func TestPowerAndRescue(t *testing.T) {
	powerPoll = time.Millisecond
	status := "running"
	rescueActive := false
	keys := []string{"0f:1e:2d:3c:4b:5a:69:78:87:96:a5:b4:c3:d2:e1:f0"}
	var resets []string
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/reset/321":
			w.Write([]byte(`{"reset":{"server_ip":"203.0.113.10","server_number":321,"type":["sw","hw","power","power_long","man"],"operating_status":"` + status + `"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/reset/321":
			r.ParseForm()
			typ := r.PostForm.Get("type")
			resets = append(resets, typ)
			switch typ {
			case "power_long":
				status = "shut off"
			case "power":
				if status == "shut off" {
					status = "running"
				} else {
					status = "shut off"
				}
			}
			w.Write([]byte(`{"reset":{"server_ip":"203.0.113.10","server_number":321,"type":"` + typ + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/boot/321/rescue":
			w.Write([]byte(`{"rescue":{"server_ip":"203.0.113.10","server_number":321,"os":["linux","vkvm"],"arch":[64],"active":` + fmt.Sprint(rescueActive) + `,"password":null,"authorized_key":[],"host_key":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/boot/321/rescue":
			r.ParseForm()
			if r.PostForm.Get("os") != "linux" || len(r.PostForm["authorized_key[]"]) != 2 {
				t.Errorf("rescue form: %v", r.PostForm)
			}
			rescueActive = true
			w.Write([]byte(`{"rescue":{"server_ip":"203.0.113.10","server_number":321,"os":"linux","arch":64,"active":true,"password":"r3scue","authorized_key":[],
			  "host_key":[{"key":{"fingerprint":"aa:bb:cc","type":"ED25519","size":256}}]}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/boot/321/rescue":
			rescueActive = false
			w.Write([]byte(`{"rescue":{"server_ip":"203.0.113.10","server_number":321,"active":false}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/key":
			var items []string
			for _, fp := range keys {
				items = append(items, `{"key":{"name":"k","fingerprint":"`+fp+`","type":"ED25519","size":256,"data":"ssh-ed25519 AAAA"}}`)
			}
			w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/key":
			r.ParseForm()
			if r.PostForm.Get("name") != "laptop" || !strings.HasPrefix(r.PostForm.Get("data"), "ssh-ed25519 ") {
				t.Errorf("key form: %v", r.PostForm)
			}
			keys = append(keys, "d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"key":{"name":"laptop","fingerprint":"d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81","type":"ED25519","size":256,"data":"ssh-ed25519 AAAA"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	pw := inv.(provider.Power)
	m := provider.Machine{ID: "321", IPv4: "203.0.113.10"}
	if st, err := pw.PowerState(context.Background(), m); err != nil || st != provider.PowerOn {
		t.Errorf("PowerState: %v %v", st, err)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootSoft); err != nil {
		t.Fatal(err)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootHard); err != nil {
		t.Fatal(err)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootPowerCycle); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOff(context.Background(), m, true); err != nil {
		t.Fatal(err)
	}
	// off now: a hard reboot becomes a power-on, power off is a no-op
	if err := pw.Reboot(context.Background(), m, provider.RebootHard); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOff(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOn(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resets, ","); got != "sw,hw,power_long,power,power_long,power,power,power" {
		t.Errorf("resets: %s", got)
	}

	rs := inv.(provider.Rescue)
	if on, _ := rs.RescueEnabled(context.Background(), m); on {
		t.Error("rescue reported active")
	}
	info, err := rs.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: []provider.PublicKey{
		{Name: "ops", Fingerprint: "0F:1E:2D:3C:4B:5A:69:78:87:96:A5:B4:C3:D2:E1:F0", Data: "ssh-rsa AAAA"},
		{Name: "laptop", Fingerprint: "d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81", Data: "ssh-ed25519 AAAA test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Password != "r3scue" || info.Address != "203.0.113.10" || info.User != "root" || strings.Join(info.HostKeys, ",") != "ED25519 aa:bb:cc" {
		t.Errorf("rescue info: %+v", info)
	}
	if on, _ := rs.RescueEnabled(context.Background(), m); !on {
		t.Error("rescue not active")
	}
	if err := rs.DisableRescue(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := rs.DisableRescue(context.Background(), m); err != nil {
		t.Errorf("disable when inactive: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("keys registered: %v", keys)
	}
}

func TestPreflightFirewall(t *testing.T) {
	// the rules of a firewall that lets TCP answers in but no UDP ones
	tcpOnly := `{"ip_version": "ipv4", "name": "tcp_established", "tcp_flags": "ack", "action": "accept"},
	  {"ip_version": "ipv4", "name": "ssh", "dst_port": "22", "protocol": "tcp", "action": "accept"},
	  {"ip_version": "ipv4", "name": "icmp", "protocol": "icmp", "action": "accept"}`
	for _, tc := range []struct {
		name, status, rules string
		warn                bool
	}{
		{"udp answers dropped", "active", tcpOnly, true},
		{"disabled", "disabled", tcpOnly, false},
		{"source port 53", "active", tcpOnly + `, {"name": "dns", "src_port": "53", "protocol": "udp", "action": "accept"}`, false},
		{"ephemeral ports", "active", tcpOnly + `, {"name": "ephemeral", "dst_port": "32768-65535", "protocol": "udp", "action": "accept"}`, false},
		{"too few ports", "active", tcpOnly + `, {"name": "some", "dst_port": "40000-65535", "protocol": "udp", "action": "accept"}`, true},
		{"one resolver only", "active", tcpOnly + `, {"name": "dns", "src_ip": "1.1.1.1/32", "src_port": "53", "protocol": "udp", "action": "accept"}`, true},
		{"discard", "active", tcpOnly + `, {"name": "all", "action": "discard"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/firewall/321" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Write([]byte(`{"firewall": {"server_number": 321, "status": "` + tc.status + `", "rules": {"input": [` + tc.rules + `], "output": []}}}`))
			})
			p, err := Type.New("main", map[string]string{"login": "#ws+user", "password": "pass", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			m := provider.Machine{Provider: TypeName, Account: "main", ID: "321", Name: "web-01"}
			warnings, err := p.(provider.Preflight).Preflight(context.Background(), m)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(warnings) > 0; got != tc.warn {
				t.Fatalf("warnings %q, want a warning: %v", warnings, tc.warn)
			}
			if tc.warn && !strings.Contains(warnings[0], "source port 53") {
				t.Errorf("the warning names no remedy: %q", warnings[0])
			}
		})
	}
}

func TestPreflightNoFirewall(t *testing.T) {
	_, inv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"status":404,"code":"NOT_FOUND","message":"Not found"}}`))
	})
	warnings, err := inv.(provider.Preflight).Preflight(context.Background(), provider.Machine{ID: "321"})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Preflight = %q, %v; want nothing", warnings, err)
	}
}
