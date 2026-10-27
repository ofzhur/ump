package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/provider"
)

// fakeVultr serves the parts of the API the provider uses and records
// the writes.
type fakeVultr struct {
	mu     sync.Mutex
	writes []string
}

func (f *fakeVultr) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Invalid API token.","status":401}`)
			return
		}
		if r.Method != http.MethodGet {
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Query().Get("per_page") == "" && !strings.Contains(r.URL.Path, "/instances/") && !strings.Contains(r.URL.Path, "/bare-metals/") {
			t.Errorf("list without per_page: %s", r.URL)
		}
		switch r.URL.Path {
		case "/instances":
			if r.URL.Query().Get("cursor") == "" {
				io.WriteString(w, `{"instances":[{"id":"cb676a46-66fd-4dfb-b839-443f2e6c0b60","os":"Ubuntu 24.04 LTS x64","ram":1024,"disk":25,"main_ip":"198.51.100.10","vcpu_count":1,"region":"fra","plan":"vc2-1c-1gb","date_created":"2026-10-01T10:00:00+00:00","status":"active","power_status":"running","server_status":"ok","v6_network":"2001:db8:10::","v6_main_ip":"2001:db8:10::1","v6_network_size":64,"label":"web-01","hostname":"web-01","tags":["prod","web"]}],"meta":{"total":2,"links":{"next":"c2","prev":""}}}`)
				return
			}
			io.WriteString(w, `{"instances":[{"id":"aa11","os":"Debian 12","ram":2048,"disk":55,"main_ip":"198.51.100.11","vcpu_count":2,"region":"ams","plan":"retired-plan","status":"active","power_status":"stopped","v6_network":"","v6_main_ip":"","v6_network_size":0,"label":"","hostname":"db-01","tags":[]}],"meta":{"total":2,"links":{"next":"","prev":"c1"}}}`)
		case "/bare-metals":
			io.WriteString(w, `{"bare_metals":[{"id":"bm-1","os":"Ubuntu 24.04","ram":"65536 MB","disk":"2x 960 GB SSD","main_ip":"203.0.113.5","cpu_count":8,"region":"ewr","status":"active","plan":"vbm-8c-64gb","v6_network":"2001:db8:5::","v6_main_ip":"2001:db8:5::1","v6_network_size":64,"mac_address":2199023255553,"label":"metal-01","tags":["prod"]}],"meta":{"total":1,"links":{"next":""}}}`)
		case "/plans":
			if r.URL.Query().Get("type") != "all" {
				t.Errorf("plans without type=all: %s", r.URL)
			}
			io.WriteString(w, `{"plans":[{"id":"vc2-1c-1gb","vcpu_count":1,"ram":1024,"disk":25,"monthly_cost":5,"type":"vc2"}],"meta":{"links":{"next":""}}}`)
		case "/plans-metal":
			io.WriteString(w, `{"plans_metal":[{"id":"vbm-8c-64gb","cpu_count":8,"monthly_cost":350,"type":"SSD"}],"meta":{"links":{"next":""}}}`)
		case "/ssh-keys":
			io.WriteString(w, `{"ssh_keys":[{"id":"k1","name":"laptop","ssh_key":"ssh-ed25519 AAAAexisting me@laptop"}],"meta":{"links":{"next":""}}}`)
		case "/regions":
			io.WriteString(w, `{"regions":[{"id":"fra","city":"Frankfurt","country":"DE","continent":"Europe"}],"meta":{"links":{"next":""}}}`)
		case "/instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60":
			io.WriteString(w, `{"instance":{"id":"cb676a46-66fd-4dfb-b839-443f2e6c0b60","os":"Ubuntu 24.04 LTS x64","ram":1024,"disk":25,"main_ip":"198.51.100.10","vcpu_count":1,"region":"fra","plan":"vc2-1c-1gb","status":"active","power_status":"running","server_status":"ok","allowed_bandwidth":1000,"v6_network":"2001:db8:10::","v6_main_ip":"2001:db8:10::1","v6_network_size":64,"label":"renamed","hostname":"web-01","tags":["web"]}}`)
		case "/bare-metals/bm-1":
			io.WriteString(w, `{"bare_metal":{"id":"bm-1","main_ip":"203.0.113.5","region":"ewr","status":"active","plan":"vbm-8c-64gb","label":"metal-renamed"}}`)
		case "/instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv4":
			io.WriteString(w, `{"ipv4s":[{"ip":"198.51.100.10","netmask":"255.255.254.0","gateway":"198.51.100.1","type":"main_ip","reverse":"198.51.100.10.vultrusercontent.com"}],"meta":{"links":{"next":""}}}`)
		case "/instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv6":
			io.WriteString(w, `{"ipv6s":[{"ip":"2001:db8:10::1","network":"2001:db8:10::","network_size":64,"type":"main_ip"}],"meta":{"links":{"next":""}}}`)
		case "/instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv6/reverse":
			io.WriteString(w, `{"reverse_ipv6s":[{"ip":"2001:db8:10::1","reverse":"web-01.example.net"}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"Not found","status":404}`)
		}
	})
}

func newTest(t *testing.T) (*Provider, *fakeVultr) {
	t.Helper()
	f := &fakeVultr{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	p, err := Type.New("vu", map[string]string{"api_key": "key", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
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
	if len(ms) != 3 {
		t.Fatalf("machines: %+v", ms)
	}
	web, db, metal := ms[0], ms[1], ms[2]
	if web.Ref() != "vultr/cb676a46-66fd-4dfb-b839-443f2e6c0b60" || web.Name != "web-01" || web.Kind != provider.KindCloud || web.IPv4 != "198.51.100.10" ||
		web.IPv6 != "2001:db8:10::/64" || web.Location != "fra" || web.Product != "vc2-1c-1gb" || web.Status != "running" || web.Account != "vu" {
		t.Errorf("web: %+v", web)
	}
	if _, ok := web.Labels["prod"]; !ok || len(web.Labels) != 2 {
		t.Errorf("tags: %v", web.Labels)
	}
	if web.Price == nil || web.Price.String() != "5.00 USD/mo" || web.Price.Source != "provider" {
		t.Errorf("price: %+v", web.Price)
	}
	if db.Name != "db-01" || db.Status != "stopped" || db.IPv6 != "" || db.Price != nil || db.Labels != nil {
		t.Errorf("db (hostname as the name, no IPv6, a plan without a price): %+v", db)
	}
	if metal.Kind != provider.KindBareMetal || metal.Name != "metal-01" || metal.Status != "active" || metal.Price == nil || metal.Price.Amount != 350 || metal.IPv6 != "2001:db8:5::/64" {
		t.Errorf("metal: %+v", metal)
	}
	if !web.Matches("2001:db8:10::abcd") {
		t.Error("an address in the IPv6 network does not match")
	}
}

func TestPowerRenameAndRDNS(t *testing.T) {
	p, f := newTest(t)
	ctx := context.Background()
	ms, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	web, metal := ms[0], ms[2]
	if st, err := p.PowerState(ctx, web); err != nil || st != provider.PowerOn {
		t.Errorf("power state: %v %v", st, err)
	}
	if st, _ := p.PowerState(ctx, metal); st != provider.PowerUnknown {
		t.Errorf("bare metal power state: %v", st)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.Reboot(ctx, web, provider.RebootSoft))
	must(p.Reboot(ctx, metal, provider.RebootPowerCycle))
	must(p.PowerOff(ctx, web, false))
	must(p.PowerOn(ctx, web))
	renamed, err := p.Rename(ctx, web, "renamed")
	if err != nil || renamed.Name != "renamed" || renamed.Price == nil {
		t.Errorf("rename: %+v %v", renamed, err)
	}
	if renamed, err := p.Rename(ctx, metal, "metal-renamed"); err != nil || renamed.Name != "metal-renamed" {
		t.Errorf("rename bare metal: %+v %v", renamed, err)
	}
	ptrs, err := p.PTRs(ctx, web)
	if err != nil || len(ptrs) != 2 || ptrs[0].Name != "198.51.100.10.vultrusercontent.com" || ptrs[1].IP != "2001:db8:10::1" || ptrs[1].Name != "web-01.example.net" {
		t.Errorf("ptrs: %+v %v", ptrs, err)
	}
	must(p.SetPTR(ctx, web, netip.MustParseAddr("198.51.100.10"), "web-01.example.net"))
	must(p.SetPTR(ctx, web, netip.MustParseAddr("198.51.100.10"), ""))
	must(p.SetPTR(ctx, web, netip.MustParseAddr("2001:db8:10::5"), "v6.example.net"))
	must(p.SetPTR(ctx, web, netip.MustParseAddr("2001:db8:10::5"), ""))
	if err := p.SetPTR(ctx, web, netip.MustParseAddr("192.0.2.1"), "x"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign address: %v", err)
	}
	if _, err := p.PTRs(ctx, metal); err == nil || !strings.Contains(err.Error(), "instances only") {
		t.Errorf("bare metal rdns: %v", err)
	}
	want := []string{
		"POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/reboot ",
		"POST /bare-metals/bm-1/halt ",
		"POST /bare-metals/bm-1/start ",
		"POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/halt ",
		"POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/start ",
		`PATCH /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60 {"label":"renamed"}`,
		`PATCH /bare-metals/bm-1 {"label":"metal-renamed"}`,
		`POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv4/reverse {"ip":"198.51.100.10","reverse":"web-01.example.net"}`,
		`POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv4/reverse/default {"ip":"198.51.100.10"}`,
		`POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv6/reverse {"ip":"2001:db8:10::5","reverse":"v6.example.net"}`,
		"DELETE /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/ipv6/reverse/2001:db8:10::5 ",
	}
	if strings.Join(f.writes, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(f.writes, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallStock(t *testing.T) {
	p, f := newTest(t)
	ctx := context.Background()
	ms, _ := p.List(ctx)
	web := ms[0]
	keys := []provider.PublicKey{
		{Name: "ump", Data: "ssh-ed25519 AAAAump ump@desk"},
		{Name: "mine", Data: "ssh-ed25519 AAAAexisting other comment"}, // in the account already
	}
	st, err := p.InstallStock(ctx, web, keys)
	if err != nil {
		t.Fatal(err)
	}
	if st.Address != "198.51.100.10" || st.Image != "Ubuntu 24.04 LTS x64" || st.Wait != 15*time.Minute {
		t.Errorf("stock install: %+v", st)
	}
	if len(f.writes) != 3 || f.writes[0] != `POST /ssh-keys {"name":"ump","ssh_key":"ssh-ed25519 AAAAump ump@desk"}` ||
		!strings.HasPrefix(f.writes[1], "PATCH /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60 ") ||
		f.writes[2] != "POST /instances/cb676a46-66fd-4dfb-b839-443f2e6c0b60/reinstall {}" {
		t.Fatalf("writes:\n%s", strings.Join(f.writes, "\n"))
	}
	var patch struct {
		UserData string `json:"user_data"`
	}
	json.Unmarshal([]byte(strings.SplitN(f.writes[1], " ", 3)[2]), &patch)
	raw, err := base64.StdEncoding.DecodeString(patch.UserData)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		DisableRoot bool `yaml:"disable_root"`
		WriteFiles  []struct {
			Path    string `yaml:"path"`
			Append  bool   `yaml:"append"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
		Runcmd []any `yaml:"runcmd"`
	}
	if !strings.HasPrefix(string(raw), "#cloud-config\n") || yaml.Unmarshal(raw, &cfg) != nil {
		t.Fatalf("user data:\n%s", raw)
	}
	if cfg.DisableRoot || len(cfg.WriteFiles) != 2 || cfg.WriteFiles[0].Path != "/root/.ssh/authorized_keys" || !cfg.WriteFiles[0].Append ||
		cfg.WriteFiles[0].Content != "ssh-ed25519 AAAAump ump@desk\nssh-ed25519 AAAAexisting other comment\n" ||
		cfg.WriteFiles[1].Content != "PermitRootLogin prohibit-password\n" || len(cfg.Runcmd) != 1 {
		t.Errorf("cloud-config: %+v\n%s", cfg, raw)
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
	for _, want := range []string{`"name":"renamed"`, `"power_status":"running"`, `"city":"Frankfurt"`, `"price":"5.00 USD/mo"`, `"ipv6_network":"2001:db8:10::/64"`, `"vcpus":1`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("describe lacks %s:\n%s", want, data)
		}
	}
}

func TestUnauthorized(t *testing.T) {
	f := &fakeVultr{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p, _ := Type.New("vu", map[string]string{"api_key": "bad", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	_, err := p.(provider.Inventory).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || !strings.Contains(apiErr.Message, "access control") {
		t.Errorf("error: %v", err)
	}
	// Vultr names the refused address: the message says so and how to fix it
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"Unauthorized IP address: 192.0.2.7","status":401}`)
	}))
	defer ipSrv.Close()
	p, _ = Type.New("vu", map[string]string{"api_key": "key", "url": ipSrv.URL}, provider.Options{HTTPClient: ipSrv.Client()})
	_, err = p.(provider.Inventory).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unauthorized IP address: 192.0.2.7 (the API key's access control does not allow it: customer portal → Account → API → Access Control, add the address)") {
		t.Errorf("unauthorized address: %v", err)
	}
	if errs := Type.Validate(map[string]string{}); len(errs) != 1 || !strings.Contains(errs[0].Error(), "api_key is required") {
		t.Errorf("validate: %v", errs)
	}
}

// TestDescribeGone: asked for a server it no longer has, the API answers
// 404, which tells ump that the server is gone (provider.IsNotFound);
// another failure does not.
func TestDescribeGone(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/instances/abc") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			w.Write([]byte(`{"error":"Not found","status":404}`))
		}
	}))
	defer srv.Close()
	p, err := Type.New("a", map[string]string{"api_key": "key", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	m := provider.Machine{ID: "abc", Kind: provider.KindCloud}
	if _, err := p.(provider.Describer).Describe(context.Background(), m); !provider.IsNotFound(err) {
		t.Errorf("404: %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := p.(provider.Describer).Describe(context.Background(), m); err == nil || provider.IsNotFound(err) {
		t.Errorf("500: %v", err)
	}
}
