package linode

import (
	"context"
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

	"github.com/ofzhur/ump/internal/provider"
)

// fakeLinode serves the parts of the API the provider uses and records
// the writes.
type fakeLinode struct {
	mu     sync.Mutex
	writes []string
}

func (f *fakeLinode) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"reason":"Invalid Token"}]}`)
			return
		}
		if r.Method != http.MethodGet {
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
			f.mu.Unlock()
			if r.Method == http.MethodPut && r.URL.Path == "/linode/instances/42" {
				io.WriteString(w, `{"id":42,"label":"renamed","region":"de-fra-2","type":"g6-nanode-1","status":"running","ipv4":["172.105.1.2"],"ipv6":"2a01:7e01::f03c:94ff:fe11:2233/128"}`)
				return
			}
			io.WriteString(w, `{}`)
			return
		}
		page := r.URL.Query().Get("page")
		switch r.URL.Path {
		case "/linode/instances":
			if r.URL.Query().Get("page_size") != "500" {
				t.Errorf("list without page_size: %s", r.URL)
			}
			if page == "1" {
				io.WriteString(w, `{"data":[{"id":42,"label":"web-01","region":"de-fra-2","type":"g6-nanode-1","status":"running","image":"linode/ubuntu24.04","ipv4":["192.168.140.5","172.105.1.2"],"ipv6":"2a01:7e01::f03c:94ff:fe11:2233/128","tags":["prod"],"specs":{"disk":25600,"memory":1024,"vcpus":1,"transfer":1000},"created":"2026-10-01T10:00:00"}],"page":1,"pages":2,"results":2}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":43,"label":"db-01","region":"us-east","type":"g6-retired-9","status":"offline","ipv4":["172.105.9.9"],"ipv6":"2600:3c00::1/128","tags":[]}],"page":2,"pages":2,"results":2}`)
		case "/linode/types":
			io.WriteString(w, `{"data":[{"id":"g6-nanode-1","price":{"hourly":0.0075,"monthly":5},"region_prices":[{"id":"id-cgk","hourly":0.009,"monthly":6}]}],"page":1,"pages":1}`)
		case "/linode/instances/42":
			io.WriteString(w, `{"id":42,"label":"web-01","region":"de-fra-2","type":"g6-nanode-1","status":"running","image":"linode/ubuntu24.04","ipv4":["172.105.1.2"],"ipv6":"2a01:7e01::f03c:94ff:fe11:2233/128","tags":["web","prod"],"specs":{"disk":25600,"memory":1024,"vcpus":1,"transfer":1000},"hypervisor":"kvm","watchdog_enabled":true,"backups":{"enabled":false}}`)
		case "/linode/instances/42/ips":
			io.WriteString(w, `{"ipv4":{"public":[{"address":"172.105.1.2","rdns":"172-105-1-2.ip.linodeusercontent.com"}],"private":[{"address":"192.168.140.5","rdns":null}]},"ipv6":{"slaac":{"address":"2a01:7e01::f03c:94ff:fe11:2233","rdns":null},"global":[{"range":"2a01:7e01:e001:1::","prefix":64}]}}`)
		case "/regions":
			io.WriteString(w, `{"data":[{"id":"de-fra-2","label":"Frankfurt 2, DE","country":"de"}],"page":1,"pages":1}`)
		case "/images":
			io.WriteString(w, `{"data":[
				{"id":"linode/debian12","label":"Debian 12","vendor":"Debian","is_public":true,"deprecated":false,"status":"available"},
				{"id":"linode/debian13","label":"Debian 13","vendor":"Debian","is_public":true,"deprecated":false,"status":"available"},
				{"id":"linode/debian9","label":"Debian 9","vendor":"Debian","is_public":true,"deprecated":true,"status":"available"},
				{"id":"linode/ubuntu26.04","label":"Ubuntu 26.04","vendor":"Ubuntu","is_public":true,"deprecated":false,"status":"available"},
				{"id":"private/123","label":"Debian 99","vendor":"","is_public":false,"deprecated":false,"status":"available"}],"page":1,"pages":1}`)
		case "/profile/sshkeys":
			io.WriteString(w, `{"data":[{"id":1,"label":"laptop","ssh_key":"ssh-ed25519 AAAAexisting me@laptop"}],"page":1,"pages":1}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errors":[{"reason":"Not found"}]}`)
		}
	})
}

func newTest(t *testing.T) (*Provider, *fakeLinode) {
	t.Helper()
	f := &fakeLinode{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	p, err := Type.New("li", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
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
	if web.Ref() != "linode/42" || web.Name != "web-01" || web.Kind != provider.KindCloud || web.IPv4 != "172.105.1.2" ||
		web.IPv6 != "2a01:7e01::f03c:94ff:fe11:2233" || web.Location != "de-fra-2" || web.Product != "g6-nanode-1" || web.Status != "running" || web.Account != "li" {
		t.Errorf("web (the private IPv4 skipped): %+v", web)
	}
	if _, ok := web.Labels["prod"]; !ok {
		t.Errorf("tags: %v", web.Labels)
	}
	if web.Price == nil || web.Price.String() != "5.00 USD/mo" || web.Price.Source != "provider" {
		t.Errorf("price: %+v", web.Price)
	}
	if db.Status != "offline" || db.Price != nil || db.Labels != nil {
		t.Errorf("db (a type without a price): %+v", db)
	}
	types := map[string]LinodeType{"g6-nanode-1": {ID: "g6-nanode-1", Price: &struct {
		Monthly float64 `json:"monthly"`
	}{5}, RegionPrices: []struct {
		ID      string  `json:"id"`
		Monthly float64 `json:"monthly"`
	}{{ID: "id-cgk", Monthly: 6}}}}
	if pr := price(types, "g6-nanode-1", "id-cgk"); pr == nil || pr.Amount != 6 {
		t.Errorf("region price: %+v", pr)
	}
}

func TestPowerRenameAndRDNS(t *testing.T) {
	p, f := newTest(t)
	ctx := context.Background()
	ms, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	must(p.Reboot(ctx, web, provider.RebootHard))
	must(p.Reboot(ctx, web, provider.RebootPowerCycle))
	must(p.PowerOff(ctx, web, true))
	must(p.PowerOn(ctx, web))
	renamed, err := p.Rename(ctx, web, "renamed")
	if err != nil || renamed.Name != "renamed" || renamed.Price == nil {
		t.Errorf("rename: %+v %v", renamed, err)
	}
	ptrs, err := p.PTRs(ctx, web)
	if err != nil || len(ptrs) != 2 || ptrs[0].Name != "172-105-1-2.ip.linodeusercontent.com" || ptrs[1].IP != "2a01:7e01::f03c:94ff:fe11:2233" {
		t.Errorf("ptrs: %+v %v", ptrs, err)
	}
	must(p.SetPTR(ctx, web, netip.MustParseAddr("172.105.1.2"), "web-01.example.net"))
	must(p.SetPTR(ctx, web, netip.MustParseAddr("172.105.1.2"), ""))
	must(p.SetPTR(ctx, web, netip.MustParseAddr("2a01:7e01:e001:1::5"), "v6.example.net"))
	if err := p.SetPTR(ctx, web, netip.MustParseAddr("192.0.2.1"), "x"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign address: %v", err)
	}
	want := []string{
		"POST /linode/instances/42/reboot {}",
		"POST /linode/instances/42/shutdown {}",
		"POST /linode/instances/42/boot {}",
		"POST /linode/instances/42/shutdown {}",
		"POST /linode/instances/42/boot {}",
		`PUT /linode/instances/42 {"label":"renamed"}`,
		`PUT /networking/ips/172.105.1.2 {"rdns":"web-01.example.net"}`,
		`PUT /networking/ips/172.105.1.2 {"rdns":null}`,
		`PUT /networking/ips/2a01:7e01:e001:1::5 {"rdns":"v6.example.net"}`,
	}
	if strings.Join(f.writes, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(f.writes, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallStock(t *testing.T) {
	p, f := newTest(t)
	ctx := context.Background()
	ms, _ := p.List(ctx)
	keys := []provider.PublicKey{
		{Name: "ump", Data: "ssh-ed25519 AAAAump ump@desk"},
		{Name: "mine", Data: "ssh-ed25519 AAAAexisting other comment"}, // in the profile already
	}
	st, err := p.InstallStock(ctx, ms[0], keys)
	if err != nil {
		t.Fatal(err)
	}
	if st.Image != "Debian 13" || st.Address != "172.105.1.2" || st.Wait != 15*time.Minute {
		t.Errorf("stock install: %+v", st)
	}
	if len(f.writes) != 2 || f.writes[0] != `POST /profile/sshkeys {"label":"ump","ssh_key":"ssh-ed25519 AAAAump ump@desk"}` || !strings.HasPrefix(f.writes[1], "POST /linode/instances/42/rebuild ") {
		t.Fatalf("writes:\n%s", strings.Join(f.writes, "\n"))
	}
	var body struct {
		Image          string   `json:"image"`
		RootPass       string   `json:"root_pass"`
		AuthorizedKeys []string `json:"authorized_keys"`
		Booted         bool     `json:"booted"`
	}
	json.Unmarshal([]byte(strings.SplitN(f.writes[1], " ", 3)[2]), &body)
	if body.Image != "linode/debian13" || len(body.RootPass) < 20 || !body.Booted || len(body.AuthorizedKeys) != 2 || body.AuthorizedKeys[0] != "ssh-ed25519 AAAAump ump@desk" {
		t.Errorf("rebuild: %+v", body)
	}
	if a, b := mustPass(t), mustPass(t); a == b {
		t.Error("the root password is not random")
	}
}

func mustPass(t *testing.T) string {
	t.Helper()
	s, err := rootPassword()
	if err != nil {
		t.Fatal(err)
	}
	return s
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
	for _, want := range []string{`"hypervisor":"kvm"`, `"label":"Frankfurt 2, DE"`, `"country":"DE"`, `"price":"5.00 USD/mo"`, `"ipv6_ranges":["2a01:7e01:e001:1::/64"]`, `"vcpus":1`, `"tags":["prod","web"]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("describe lacks %s:\n%s", want, data)
		}
	}
}

func TestErrors(t *testing.T) {
	f := &fakeLinode{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p, _ := Type.New("li", map[string]string{"token": "bad", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	_, err := p.(provider.Inventory).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || !strings.Contains(apiErr.Message, "Invalid Token") {
		t.Errorf("error: %v", err)
	}
	if e := parseError(400, []byte(`{"errors":[{"reason":"Label must be unique","field":"label"}]}`)); !strings.Contains(e.Error(), "label: Label must be unique") {
		t.Errorf("field error: %v", e)
	}
}

// TestDescribeGone: asked for a server it no longer has, the API answers
// 404, which tells ump that the server is gone (provider.IsNotFound);
// another failure does not.
func TestDescribeGone(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/linode/instances/42") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			w.Write([]byte(`{"errors":[{"reason":"Not found"}]}`))
		}
	}))
	defer srv.Close()
	p, err := Type.New("a", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	m := provider.Machine{ID: "42"}
	if _, err := p.(provider.Describer).Describe(context.Background(), m); !provider.IsNotFound(err) {
		t.Errorf("404: %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := p.(provider.Describer).Describe(context.Background(), m); err == nil || provider.IsNotFound(err) {
		t.Errorf("500: %v", err)
	}
}
