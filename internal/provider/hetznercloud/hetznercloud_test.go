package hetznercloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
