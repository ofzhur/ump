package hetznercloud

import (
	"context"
	"encoding/json"
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

func page(id int, name string, next string) string {
	return fmt.Sprintf(`{"servers":[{"id":%d,"name":"%s","status":"running",
      "public_net":{"ipv4":{"ip":"198.51.100.%d"},"ipv6":{"ip":"2001:db8:c0ff:ee%d::/64"}},
      "server_type":{"name":"cx22","architecture":"x86","deprecated":false,"prices":[{"location":"fsn1","price_monthly":{"net":"3.7900000000","gross":"4.5101000000"}},{"location":"nbg1","price_monthly":{"net":"3.7900000000","gross":"4.5101000000"}}]},
      "datacenter":null,"location":{"name":"fsn1","description":"Falkenstein DC Park 1","city":"Falkenstein","country":"DE","network_zone":"eu-central"},
      "labels":{"env":"prod"},"rescue_enabled":false}],
      "meta":{"pagination":{"page":%d,"per_page":50,"next_page":%s,"total_entries":2}}}`, id, name, id, id, id, next)
}

func TestListServersPaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"unauthorized","message":"unable to authenticate"}}`))
			return
		}
		if r.URL.Path != "/servers" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		pages = append(pages, r.URL.Query().Get("page"))
		switch r.URL.Query().Get("page") {
		case "1":
			w.Write([]byte(page(1, "edge-01", "2")))
		case "2":
			w.Write([]byte(page(2, "edge-02", "null")))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()
	p, err := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := p.(provider.Inventory).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || len(ms) != 2 {
		t.Fatalf("pages %v, machines %d", pages, len(ms))
	}
	want := provider.Machine{Provider: "hetzner-cloud", Account: "hc", ID: "1", Name: "edge-01", Kind: provider.KindCloud,
		IPv4: "198.51.100.1", IPv6: "2001:db8:c0ff:ee1::/64", Location: "fsn1", Product: "cx22", Status: "running",
		Labels: map[string]string{"env": "prod"}, Price: &provider.Price{Amount: 3.79, Currency: "EUR", Period: "month", Source: "provider", Note: "net"}}
	got := ms[0]
	if got.Labels["env"] != "prod" {
		t.Errorf("labels not mapped: %v", got.Labels)
	}
	got.Labels, want.Labels = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("machine 0:\n got %+v\nwant %+v", got, want)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"unauthorized","message":"unable to authenticate"}}`))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "bad", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	_, err := p.(provider.Inventory).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.Code != "unauthorized" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNoPublicNet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"servers":[{"id":7,"name":"private","status":"off","public_net":{"ipv4":null,"ipv6":null},
		  "server_type":{"name":"cax11"},"datacenter":{"name":"hel1-dc2","location":{"name":"hel1"}},"location":{"name":"hel1"},"labels":{}}],
		  "meta":{"pagination":{"page":1,"next_page":null}}}`))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	ms, err := p.(provider.Inventory).List(context.Background())
	if err != nil || len(ms) != 1 {
		t.Fatalf("got %v, %v", ms, err)
	}
	if ms[0].IPv4 != "" || ms[0].IPv6 != "" || ms[0].Labels != nil || ms[0].Status != "off" || ms[0].Location != "hel1-dc2" {
		t.Errorf("unexpected machine: %+v", ms[0])
	}
}

const serverDetailJSON = `{"server":{"id":42,"name":"edge-01","status":"running","created":"2026-01-02T03:04:05+00:00",
  "public_net":{"ipv4":{"ip":"198.51.100.1","blocked":false,"dns_ptr":"edge-01.example.net"},
    "ipv6":{"ip":"2001:db8:c0ff:ee1::/64","blocked":false,"dns_ptr":[{"ip":"2001:db8:c0ff:ee1::1","dns_ptr":"edge-01.example.net"}]},
    "floating_ips":[7],"firewalls":[{"id":3,"status":"applied"}]},
  "private_net":[{"network":99,"ip":"10.0.1.5","alias_ips":[],"mac_address":"86:00:00:2a:7d:e0"}],
  "server_type":{"name":"cx22","description":"CX22","cores":2,"memory":4.0,"disk":40,"cpu_type":"shared","architecture":"x86"},
  "datacenter":{"name":"fsn1-dc14","description":"Falkenstein 1 virtual DC 14","location":{"name":"fsn1","city":"Falkenstein","country":"DE","network_zone":"eu-central"}},
  "image":{"id":1,"name":"debian-12","description":"Debian 12","os_flavor":"debian","os_version":"12","type":"system"},
  "iso":null,"volumes":[11,12],"protection":{"delete":true,"rebuild":true},"labels":{"env":"prod"},
  "rescue_enabled":false,"locked":false,"backup_window":"22-02","placement_group":null,"primary_disk_size":40,
  "included_traffic":21990232555520,"outgoing_traffic":123,"ingoing_traffic":null}}`

func TestDescribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/servers/42" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(serverDetailJSON))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	d, err := p.(provider.Describer).Describe(context.Background(), provider.Machine{ID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "edge-01" || d.Labels["env"] != "prod" || d.Location != "fsn1-dc14" {
		t.Errorf("machine: %+v", d.Machine)
	}
	want := map[string]any{
		"Server/backup_window": "22-02", "Server/placement_group": nil, "Server/volumes": []string{"11", "12"},
		"Server/primary_disk": provider.Bytes(40 << 30),
		"Type/memory":         provider.Bytes(4 << 30), "Type/cores": 2, "Type/cpu_type": "shared",
		"Image/name": "Debian 12", "Image/os_version": "12", "Image/iso": nil,
		"Location/location": "fsn1", "Location/datacenter": "fsn1-dc14", "Location/city": "Falkenstein", "Location/network_zone": "eu-central",
		"Network/ipv4_dns_ptr": "edge-01.example.net", "Network/ipv6_dns_ptr": []string{"2001:db8:c0ff:ee1::1 edge-01.example.net"},
		"Network/floating_ips": []string{"7"}, "Network/private_networks": []string{"10.0.1.5 on network 99 (86:00:00:2a:7d:e0)"},
		"Network/firewalls": []string{"3 (applied)"},
		"Traffic/included":  provider.Bytes(21990232555520), "Traffic/ingoing": nil,
		"Protection/delete": true,
	}
	for path, w := range want {
		if got := field(d, path); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %#v (%T), want %#v", path, got, got, w)
		}
	}
	if _, err := p.(provider.Describer).Describe(context.Background(), provider.Machine{ID: "x"}); err == nil {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/servers/42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name != "prod-edge-01" {
			t.Errorf("body: %+v %v", body, err)
		}
		if body.Name == "prod-edge-01" && r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		w.Write([]byte(strings.Replace(serverDetailJSON, `"name":"edge-01"`, `"name":"prod-edge-01"`, 1)))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	m, err := p.(provider.Renamer).Rename(context.Background(), provider.Machine{ID: "42"}, "prod-edge-01")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "prod-edge-01" || m.ID != "42" || m.Labels["env"] != "prod" {
		t.Errorf("renamed machine: %+v", m)
	}
}

func TestRenameConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"code":"uniqueness_error","message":"server name is already used"}}`))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	_, err := p.(provider.Renamer).Rename(context.Background(), provider.Machine{ID: "42"}, "taken")
	if err == nil || err.Error() != "hetzner-cloud: the name is already used in this project (uniqueness_error, HTTP 409)" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReverseDNS(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers/42":
			w.Write([]byte(serverDetailJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/servers/42/actions/change_dns_ptr":
			var body struct {
				IP     string  `json:"ip"`
				DNSPtr *string `json:"dns_ptr"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			ptr := "<null>"
			if body.DNSPtr != nil {
				ptr = *body.DNSPtr
			}
			got = append(got, body.IP+"="+ptr)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"action":{"id":1,"status":"success"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	rd := p.(provider.ReverseDNS)
	m := provider.Machine{ID: "42"}
	ptrs, err := rd.PTRs(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.PTR{{IP: "198.51.100.1", Name: "edge-01.example.net"}, {IP: "2001:db8:c0ff:ee1::1", Name: "edge-01.example.net"}}
	if !reflect.DeepEqual(ptrs, want) {
		t.Errorf("PTRs: got %+v, want %+v", ptrs, want)
	}
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("198.51.100.1"), "new.example.net"); err != nil {
		t.Fatal(err)
	}
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("2001:db8:c0ff:ee1::5"), ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "198.51.100.1=new.example.net,2001:db8:c0ff:ee1::5=<null>" {
		t.Errorf("actions: %v", got)
	}
	if err := rd.SetPTR(context.Background(), m, netip.MustParseAddr("203.0.113.9"), "x"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign address: %v", err)
	}
}

func TestPowerAndRescue(t *testing.T) {
	actionPoll = time.Millisecond
	var log []string
	rescue := false
	keys := map[string]int64{"0f:1e:2d:3c:4b:5a:69:78:87:96:a5:b4:c3:d2:e1:f0": 7}
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log = append(log, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/servers/42":
			body := serverDetailJSON
			if rescue {
				body = strings.Replace(body, `"rescue_enabled":false`, `"rescue_enabled":true`, 1)
			}
			w.Write([]byte(body))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/servers/actions/"):
			polls++
			status := "running"
			if polls >= 2 {
				status = "success"
			}
			w.Write([]byte(`{"action":{"id":900,"command":"reset_server","status":"` + status + `","progress":50}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/ssh_keys":
			fp := r.URL.Query().Get("fingerprint")
			if id, ok := keys[fp]; ok {
				w.Write([]byte(fmt.Sprintf(`{"ssh_keys":[{"id":%d,"name":"k","fingerprint":"%s"}]}`, id, fp)))
				return
			}
			w.Write([]byte(`{"ssh_keys":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/ssh_keys":
			var body struct {
				Name, PublicKey string `json:"-"`
				N               string `json:"name"`
				P               string `json:"public_key"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.N == "laptop" {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error":{"code":"uniqueness_error","message":"name taken"}}`))
				return
			}
			keys["d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81"] = 8
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"ssh_key":{"id":8,"name":"` + body.N + `","fingerprint":"d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81"}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/servers/42/actions/"):
			action := strings.TrimPrefix(r.URL.Path, "/servers/42/actions/")
			switch action {
			case "enable_rescue":
				var body struct {
					Type    string  `json:"type"`
					SSHKeys []int64 `json:"ssh_keys"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Type != "linux64" || len(body.SSHKeys) != 2 {
					t.Errorf("enable_rescue body: %+v", body)
				}
				rescue = true
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"action":{"id":901,"command":"enable_rescue","status":"success","progress":100},"root_password":"s3cret"}`))
			case "disable_rescue":
				rescue = false
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"action":{"id":902,"command":"disable_rescue","status":"success","progress":100}}`))
			case "reset":
				polls = 0
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"action":{"id":900,"command":"reset_server","status":"running","progress":0}}`))
			case "reboot", "poweroff", "poweron", "shutdown":
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"action":{"id":903,"command":"` + action + `","status":"success","progress":100}}`))
			default:
				t.Errorf("unexpected action %s", action)
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	pw := p.(provider.Power)
	m := provider.Machine{ID: "42", IPv4: "198.51.100.1"}

	if st, err := pw.PowerState(context.Background(), m); err != nil || st != provider.PowerOn {
		t.Errorf("PowerState: %v %v", st, err)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootHard); err != nil {
		t.Fatal(err)
	}
	if polls < 2 {
		t.Errorf("reset action not polled to completion: %d polls", polls)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootSoft); err != nil {
		t.Fatal(err)
	}
	if err := pw.Reboot(context.Background(), m, provider.RebootPowerCycle); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOff(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOn(context.Background(), m); err != nil {
		t.Fatal(err)
	}

	rs := p.(provider.Rescue)
	if on, _ := rs.RescueEnabled(context.Background(), m); on {
		t.Error("rescue reported enabled")
	}
	info, err := rs.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: []provider.PublicKey{
		{Name: "ops", Fingerprint: "0f:1e:2d:3c:4b:5a:69:78:87:96:a5:b4:c3:d2:e1:f0", Data: "ssh-rsa AAAA..."},
		{Name: "laptop", Fingerprint: "d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81", Data: "ssh-ed25519 AAAA..."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Password != "s3cret" || info.User != "root" || info.Address != "198.51.100.1" {
		t.Errorf("rescue info: %+v", info)
	}
	if on, _ := rs.RescueEnabled(context.Background(), m); !on {
		t.Error("rescue not enabled")
	}
	if err := rs.DisableRescue(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := rs.DisableRescue(context.Background(), m); err != nil {
		t.Errorf("disable when already disabled: %v", err)
	}
	joined := strings.Join(log, "\n")
	for _, want := range []string{"POST /servers/42/actions/reset", "POST /servers/42/actions/reboot", "POST /servers/42/actions/poweroff", "POST /servers/42/actions/poweron",
		"POST /servers/42/actions/shutdown", "GET /ssh_keys?fingerprint=0f%3A1e", "POST /ssh_keys", "POST /servers/42/actions/enable_rescue", "POST /servers/42/actions/disable_rescue"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing request %q in:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "POST /ssh_keys") != 2 {
		t.Errorf("the clashing key name should be retried once:\n%s", joined)
	}
	if strings.Count(joined, "POST /servers/42/actions/disable_rescue") != 1 {
		t.Errorf("disable_rescue should run once:\n%s", joined)
	}
}
