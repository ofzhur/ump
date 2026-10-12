package hetznercloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ofzhur/ump/internal/provider"
)

func page(id int, name string, next string) string {
	return fmt.Sprintf(`{"servers":[{"id":%d,"name":"%s","status":"running",
      "public_net":{"ipv4":{"ip":"198.51.100.%d"},"ipv6":{"ip":"2001:db8:c0ff:ee%d::/64"}},
      "server_type":{"name":"cx22","architecture":"x86"},
      "datacenter":{"name":"FSN1-DC14","location":{"name":"fsn1"}},
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
		IPv4: "198.51.100.1", IPv6: "2001:db8:c0ff:ee1::/64", Location: "fsn1-dc14", Product: "cx22", Status: "running",
		Labels: map[string]string{"env": "prod"}}
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
		  "server_type":{"name":"cax11"},"datacenter":{"name":"hel1-dc2","location":{"name":"hel1"}},"labels":{}}],
		  "meta":{"pagination":{"page":1,"next_page":null}}}`))
	}))
	defer srv.Close()
	p, _ := Type.New("hc", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	ms, err := p.(provider.Inventory).List(context.Background())
	if err != nil || len(ms) != 1 {
		t.Fatalf("got %v, %v", ms, err)
	}
	if ms[0].IPv4 != "" || ms[0].IPv6 != "" || ms[0].Labels != nil || ms[0].Status != "off" {
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
		"Location/city": "Falkenstein", "Location/network_zone": "eu-central",
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
