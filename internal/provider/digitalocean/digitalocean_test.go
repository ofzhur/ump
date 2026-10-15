package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ofzhur/ump/internal/provider"
)

// fakeDO serves the parts of the API the provider uses; an action is in
// progress at first and completed when polled.
type fakeDO struct {
	mu     sync.Mutex
	writes []string
	polls  int
	name   string
}

const droplet = `{"id":101,"name":%q,"memory":1024,"vcpus":1,"disk":25,"locked":false,"status":"active","created_at":"2026-10-01T10:00:00Z","size_slug":"s-1vcpu-1gb",
 "region":{"slug":"fra1","name":"Frankfurt 1"},"image":{"name":"24.04 (LTS) x64","distribution":"Ubuntu","slug":"ubuntu-24-04-x64"},
 "size":{"slug":"s-1vcpu-1gb","price_monthly":6,"transfer":1},
 "networks":{"v4":[{"ip_address":"10.114.0.2","netmask":"255.255.240.0","gateway":"10.114.0.1","type":"private"},{"ip_address":"203.0.113.20","netmask":"255.255.240.0","gateway":"203.0.113.1","type":"public"}],
  "v6":[{"ip_address":"2a03:b0c0:3:d0::1","netmask":64,"gateway":"2a03:b0c0:3:d0::1:1","type":"public"}]},
 "tags":["web","prod"],"features":["ipv6","monitoring"],"volume_ids":[],"vpc_uuid":"vpc-1"}`

func (f *fakeDO) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"id":"unauthorized","message":"Unable to authenticate you"}`)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/droplets/101/actions":
			body, _ := io.ReadAll(r.Body)
			f.writes = append(f.writes, strings.TrimSpace(string(body)))
			var req map[string]string
			json.Unmarshal(body, &req)
			if req["type"] == "rename" {
				f.name = req["name"]
			}
			io.WriteString(w, `{"action":{"id":9,"status":"in-progress","type":"`+req["type"]+`"}}`)
		case r.URL.Path == "/actions/9":
			f.polls++
			io.WriteString(w, `{"action":{"id":9,"status":"completed","type":"x"}}`)
		case r.URL.Path == "/droplets" && r.URL.Query().Get("page") == "1":
			if r.URL.Query().Get("per_page") != "200" {
				t.Errorf("list without per_page: %s", r.URL)
			}
			io.WriteString(w, `{"droplets":[`+sprintf(droplet, "web-01.example.net")+`],"links":{"pages":{"next":"https://api.digitalocean.com/v2/droplets?page=2"}},"meta":{"total":2}}`)
		case r.URL.Path == "/droplets" && r.URL.Query().Get("page") == "2":
			io.WriteString(w, `{"droplets":[{"id":102,"name":"db-01","status":"off","size_slug":"s-old","region":{"slug":"nyc3"},"networks":{"v4":[],"v6":[]},"tags":[]}],"links":{},"meta":{"total":2}}`)
		case r.URL.Path == "/droplets/101":
			io.WriteString(w, `{"droplet":`+sprintf(droplet, f.name)+`}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"id":"not_found","message":"The resource you were accessing could not be found."}`)
		}
	})
}

func sprintf(format, name string) string {
	data, _ := json.Marshal(name)
	return strings.Replace(format, "%q", string(data), 1)
}

func newTest(t *testing.T) (*Provider, *fakeDO) {
	t.Helper()
	f := &fakeDO{name: "web-01.example.net"}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	old := actionPoll
	actionPoll = 0
	t.Cleanup(func() { actionPoll = old })
	p, err := Type.New("do", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider), f
}

func TestList(t *testing.T) {
	p, _ := newTest(t)
	ms, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("machines: %+v", ms)
	}
	web, db := ms[0], ms[1]
	if web.Ref() != "digitalocean/101" || web.Name != "web-01.example.net" || web.IPv4 != "203.0.113.20" || web.IPv6 != "2a03:b0c0:3:d0::1" ||
		web.Location != "fra1" || web.Product != "s-1vcpu-1gb" || web.Status != "active" || web.Kind != provider.KindCloud {
		t.Errorf("web (the private IPv4 skipped): %+v", web)
	}
	if web.Price == nil || web.Price.String() != "6.00 USD/mo" || len(web.Labels) != 2 {
		t.Errorf("price, tags: %+v %v", web.Price, web.Labels)
	}
	if db.Status != "off" || db.IPv4 != "" || db.Price != nil || db.Labels != nil {
		t.Errorf("db: %+v", db)
	}
}

func TestPowerAndRename(t *testing.T) {
	p, f := newTest(t)
	ctx := context.Background()
	ms, _ := p.List(ctx)
	web := ms[0]
	if st, err := p.PowerState(ctx, web); err != nil || st != provider.PowerOn {
		t.Errorf("power state: %v %v", st, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.Reboot(ctx, web, provider.RebootSoft))
	must(p.Reboot(ctx, web, provider.RebootHard))
	must(p.PowerOff(ctx, web, false))
	must(p.PowerOff(ctx, web, true))
	must(p.PowerOn(ctx, web))
	renamed, err := p.Rename(ctx, web, "web-02.example.net")
	if err != nil || renamed.Name != "web-02.example.net" {
		t.Errorf("rename: %+v %v", renamed, err)
	}
	want := `{"type":"reboot"}
{"type":"power_cycle"}
{"type":"shutdown"}
{"type":"power_off"}
{"type":"power_on"}
{"name":"web-02.example.net","type":"rename"}`
	if strings.Join(f.writes, "\n") != want {
		t.Errorf("actions:\n%s", strings.Join(f.writes, "\n"))
	}
	if f.polls != 6 {
		t.Errorf("each action is followed to completion: %d polls", f.polls)
	}
}

func TestDescribe(t *testing.T) {
	p, _ := newTest(t)
	ctx := context.Background()
	ms, _ := p.List(ctx)
	d, err := p.Describe(ctx, ms[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(d)
	for _, want := range []string{`"image":"Ubuntu 24.04 (LTS) x64"`, `"name":"Frankfurt 1"`, `"price":"6.00 USD/mo"`, `"tags":["prod","web"]`, `"ipv6":["2a03:b0c0:3:d0::1/64 (gateway 2a03:b0c0:3:d0::1:1)"]`, `"transfer":"1 TB"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("describe lacks %s:\n%s", want, data)
		}
	}
}

func TestErrors(t *testing.T) {
	f := &fakeDO{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p, _ := Type.New("do", map[string]string{"token": "bad", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	_, err := p.(provider.Inventory).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || !strings.Contains(apiErr.Message, "Unable to authenticate you") {
		t.Errorf("error: %v", err)
	}
	if e := parseError(403, []byte(`{"id":"forbidden","message":"You do not have access for the attempted action."}`)); !strings.Contains(e.Error(), "read-only token") {
		t.Errorf("forbidden: %v", e)
	}
	ctx := context.Background()
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
	if err := c.WaitAction(ctx, Action{ID: 1, Status: "errored", Type: "power_on"}); err == nil || !strings.Contains(err.Error(), "action power_on failed") {
		t.Errorf("errored action: %v", err)
	}
}
