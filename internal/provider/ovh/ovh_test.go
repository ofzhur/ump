package ovh

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

const (
	testAK = "app-key"
	testAS = "app-secret"
	testCK = "consumer-key"
)

// fakeAPI is an OVH API stand-in that checks request signatures the way
// the real one does.
type fakeAPI struct {
	t            *testing.T
	srv          *httptest.Server
	denyCloud    bool
	denyDedicate bool
	calls        atomic.Int32
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) deny(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"message":"This call has not been granted","errorCode":"NOT_GRANTED_CALL","httpCode":"403 Forbidden"}`))
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Ovh-Application") != testAK {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Invalid application key","errorCode":"INVALID_KEY"}`))
		return
	}
	switch r.URL.Path {
	case "/auth/time":
		w.Write([]byte("1700000000"))
		return
	case "/auth/credential":
		var req struct {
			AccessRules []AccessRule `json:"accessRules"`
			Redirection string       `json:"redirection"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.AccessRules) == 0 {
			f.t.Errorf("bad credential request: %s", body)
		}
		if r.Header.Get("X-Ovh-Signature") != "" {
			f.t.Error("credential request must not be signed")
		}
		w.Write([]byte(`{"validationUrl":"https://eu.api.ovh.com/auth/?credentialToken=abc","consumerKey":"new-ck","state":"pendingValidation"}`))
		return
	}
	// everything else must be signed
	ts := r.Header.Get("X-Ovh-Timestamp")
	h := sha1.New()
	io.WriteString(h, testAS+"+"+testCK+"+"+r.Method+"+"+f.srv.URL+r.URL.RequestURI()+"+"+string(body)+"+"+ts)
	want := "$1$" + hex.EncodeToString(h.Sum(nil))
	if r.Header.Get("X-Ovh-Consumer") != testCK || r.Header.Get("X-Ovh-Signature") != want {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"Invalid signature","errorCode":"INVALID_SIGNATURE"}`))
		return
	}
	if ts != "1700000000" && ts != "1700000001" {
		f.t.Errorf("timestamp %s not derived from /auth/time", ts)
	}
	switch r.URL.Path {
	case "/dedicated/server":
		if f.denyDedicate {
			f.deny(w)
			return
		}
		w.Write([]byte(`["ns1.ip-203-0-113.eu","ns2.ip-203-0-113.eu"]`))
	case "/dedicated/server/ns1.ip-203-0-113.eu":
		w.Write([]byte(`{"name":"ns1.ip-203-0-113.eu","ip":"203.0.113.20","datacenter":"gra2","region":"eu-west-gra","state":"ok",
		  "powerState":"poweredOn","commercialRange":"rise-1","os":"debian12_64","serverId":1001,"iam":{"displayName":"db-01","id":"x"},
		  "bootId":1,"rootDevice":"/dev/sda","rescueMail":"ops@example.net","monitoring":true,"supportLevel":"pro","linkSpeed":1000,
		  "availabilityZone":"eu-west-gra-a","rack":"R42"}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/serviceInfos":
		w.Write([]byte(`{"creation":"2025-01-15","expiration":"2026-11-15","status":"ok","renewalType":"automaticV2016",
		  "renew":{"automatic":true,"deleteAtExpiration":false,"forced":false}}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/boot/1":
		w.Write([]byte(`{"bootId":1,"bootType":"harddisk","description":"Boot on disk","kernel":"hd"}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/specifications/hardware":
		w.Write([]byte(`{"description":"RISE-1","memorySize":{"unit":"MB","value":32768},"numberOfProcessors":1,
		  "processorName":"Intel Xeon E-2386G","coresPerProcessor":6,"threadsPerProcessor":12,
		  "diskGroups":[{"description":"","numberOfDisks":2,"diskSize":{"unit":"GB","value":512},"diskType":"NVME","raidController":"soft"}],
		  "bootMode":"uefi","formFactor":"1u"}`))
	case "/cloud/project/p1/instance/uuid-1":
		w.Write([]byte(`{"id":"uuid-1","name":"vm-01","status":"ACTIVE","region":"GRA11","created":"2026-03-01T10:00:00Z","planCode":"b3-8.consumption",
		  "flavor":{"name":"b3-8","type":"ovh.b3","vcpus":2,"ram":8192,"disk":50,"osType":"linux"},
		  "image":{"name":"Debian 12","type":"linux","user":"debian","visibility":"public"},"sshKey":{"name":"laptop"},"monthlyBilling":null,
		  "ipAddresses":[{"ip":"10.0.0.5","type":"private","version":4,"networkId":"net-1"},{"ip":"198.51.100.7","type":"public","version":4},{"ip":"2001:db8:7::1","type":"public","version":6}]}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/ips":
		w.Write([]byte(`["203.0.113.20/32","2001:db8:5:600::/64"]`))
	case "/dedicated/server/ns2.ip-203-0-113.eu":
		w.Write([]byte(`{"name":"ns2.ip-203-0-113.eu","ip":"203.0.113.21","datacenter":"rbx8","state":"ok","powerState":"poweredOff","commercialRange":"advance-2"}`))
	case "/dedicated/server/ns2.ip-203-0-113.eu/ips":
		w.Write([]byte(`["203.0.113.21/32"]`))
	case "/cloud/project":
		if f.denyCloud {
			f.deny(w)
			return
		}
		w.Write([]byte(`["p1"]`))
	case "/cloud/project/p1":
		w.Write([]byte(`{"project_id":"p1","description":"Production","status":"ok"}`))
	case "/cloud/project/p1/instance":
		w.Write([]byte(`[{"id":"uuid-1","name":"vm-01","status":"ACTIVE","region":"GRA11","flavorId":"flavor-uuid","planCode":"b3-8.consumption",
		  "ipAddresses":[{"ip":"10.0.0.5","type":"private","version":4},{"ip":"198.51.100.7","type":"public","version":4},{"ip":"2001:db8:7::1","type":"public","version":6}]}]`))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) provider(t *testing.T) provider.Inventory {
	t.Helper()
	p, err := Type.New("ovh", map[string]string{
		"endpoint": f.srv.URL, "application_key": testAK, "application_secret": testAS, "consumer_key": testCK,
	}, provider.Options{HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return p.(provider.Inventory)
}

func TestList(t *testing.T) {
	f := newFakeAPI(t)
	ms, err := f.provider(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("got %d machines, want 3: %+v", len(ms), ms)
	}
	byID := map[string]provider.Machine{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	ded := byID["ns1.ip-203-0-113.eu"]
	want := provider.Machine{Provider: "ovh", Account: "ovh", ID: "ns1.ip-203-0-113.eu", Name: "db-01", Kind: provider.KindBareMetal,
		IPv4: "203.0.113.20", IPv6: "2001:db8:5:600::/64", Location: "gra2", Product: "rise-1", Status: "ok"}
	if !reflect.DeepEqual(ded, want) {
		t.Errorf("dedicated:\n got %+v\nwant %+v", ded, want)
	}
	if off := byID["ns2.ip-203-0-113.eu"]; off.Status != "off" || off.Name != "ns2.ip-203-0-113.eu" || off.IPv6 != "" {
		t.Errorf("powered-off server: %+v", off)
	}
	vm := byID["uuid-1"]
	if vm.Kind != provider.KindCloud || vm.Name != "vm-01" || vm.IPv4 != "198.51.100.7" || vm.IPv6 != "2001:db8:7::1" ||
		vm.Location != "gra11" || vm.Product != "b3-8" || vm.Status != "active" ||
		vm.Labels["project"] != "Production" || vm.Labels["project_id"] != "p1" {
		t.Errorf("instance: %+v", vm)
	}
}

func TestNotGrantedFamilyIsSkipped(t *testing.T) {
	f := newFakeAPI(t)
	f.denyCloud = true
	ms, err := f.provider(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Errorf("got %d machines, want the 2 dedicated ones", len(ms))
	}
	f.denyDedicate = true
	_, err = f.provider(t).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "NOT_GRANTED_CALL" {
		t.Errorf("both families denied should fail, got %v", err)
	}
}

func TestBadSecret(t *testing.T) {
	f := newFakeAPI(t)
	p, _ := Type.New("ovh", map[string]string{
		"endpoint": f.srv.URL, "application_key": testAK, "application_secret": "wrong", "consumer_key": testCK,
	}, provider.Options{HTTPClient: f.srv.Client()})
	_, err := p.(provider.Inventory).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signature rejected") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequestConsumerKey(t *testing.T) {
	f := newFakeAPI(t)
	c := &Client{BaseURL: f.srv.URL, AppKey: testAK, HTTP: f.srv.Client()}
	cred, err := c.RequestConsumerKey(context.Background(), DefaultAccessRules, "")
	if err != nil {
		t.Fatal(err)
	}
	if cred.ConsumerKey != "new-ck" || !strings.HasPrefix(cred.ValidationURL, "https://") {
		t.Errorf("unexpected credential: %+v", cred)
	}
}

type scriptedUI struct {
	lines []string
	out   strings.Builder
}

func (s *scriptedUI) Printf(format string, args ...any) { fmt.Fprintf(&s.out, format, args...) }
func (s *scriptedUI) Line(label, def string) (string, error) {
	if len(s.lines) == 0 {
		return def, nil
	}
	l := s.lines[0]
	s.lines = s.lines[1:]
	return l, nil
}
func (s *scriptedUI) Secret(label string) (string, error)          { return s.Line(label, "") }
func (s *scriptedUI) Confirm(label string, def bool) (bool, error) { return def, nil }

func TestCompleteRequestsKey(t *testing.T) {
	f := newFakeAPI(t)
	ui := &scriptedUI{lines: []string{""}}
	settings := map[string]string{"endpoint": f.srv.URL, "application_key": testAK, "application_secret": testAS}
	if err := Type.Complete(context.Background(), ui, settings, provider.Options{HTTPClient: f.srv.Client()}); err != nil {
		t.Fatal(err)
	}
	if settings["consumer_key"] != "new-ck" {
		t.Errorf("consumer key not filled in: %v", settings)
	}
	if !strings.Contains(ui.out.String(), "credentialToken=abc") {
		t.Errorf("validation URL not shown: %s", ui.out.String())
	}
	// an existing key is left alone
	settings["consumer_key"] = "keep"
	if err := Type.Complete(context.Background(), ui, settings, provider.Options{}); err != nil || settings["consumer_key"] != "keep" {
		t.Errorf("Complete changed an existing key: %v %v", settings, err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	if u, err := ResolveEndpoint(""); err != nil || u != "https://eu.api.ovh.com/1.0" {
		t.Errorf("default: %q %v", u, err)
	}
	if u, err := ResolveEndpoint("ovh-ca"); err != nil || u != "https://ca.api.ovh.com/1.0" {
		t.Errorf("ovh-ca: %q %v", u, err)
	}
	if u, err := ResolveEndpoint("https://api.example.test/1.0/"); err != nil || u != "https://api.example.test/1.0" {
		t.Errorf("url: %q %v", u, err)
	}
	if _, err := ResolveEndpoint("ovh-moon"); err == nil {
		t.Error("unknown endpoint accepted")
	}
}

func TestTimestampUsesServerTime(t *testing.T) {
	f := newFakeAPI(t)
	c := &Client{BaseURL: f.srv.URL, AppKey: testAK, AppSecret: testAS, ConsumerKey: testCK, HTTP: f.srv.Client()}
	ts, err := c.timestamp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts < 1700000000 || ts > 1700000001 {
		t.Errorf("timestamp %d not aligned with server time", ts)
	}
	if time.Unix(ts, 0).Year() != 2023 {
		t.Errorf("unexpected year for %d", ts)
	}
	if _, err := c.timestamp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Errorf("server time fetched %d times, want once", f.calls.Load())
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

func sectionNames(d *provider.Details) string {
	names := []string{}
	for _, s := range d.Sections {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

func TestDescribeDedicated(t *testing.T) {
	f := newFakeAPI(t)
	d, err := f.provider(t).(provider.Describer).Describe(context.Background(), provider.Machine{ID: "ns1.ip-203-0-113.eu", Kind: provider.KindBareMetal})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "db-01" || d.IPv6 != "2001:db8:5:600::/64" {
		t.Errorf("machine: %+v", d.Machine)
	}
	if got := sectionNames(d); got != "Server,Location,Boot,Addresses,Service,Hardware" {
		t.Fatalf("sections: %s", got)
	}
	want := map[string]any{
		"Server/display_name": "db-01", "Server/server_id": int64(1001), "Server/monitoring": true, "Server/link_speed_mbps": 1000,
		"Location/rack": "R42", "Boot/boot_type": "harddisk", "Boot/root_device": "/dev/sda",
		"Addresses/ip_blocks": []string{"203.0.113.20/32", "2001:db8:5:600::/64"},
		"Service/renewal":     "automatic", "Service/expiration": "2026-11-15",
		"Hardware/memory": provider.Bytes(32 << 30), "Hardware/cores": 6, "Hardware/threads": 12,
		"Hardware/disks": []string{"2 x 512 GB NVME (soft)"}, "Hardware/boot_mode": "uefi",
	}
	for path, w := range want {
		if got := field(d, path); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %#v (%T), want %#v", path, got, got, w)
		}
	}
}

func TestDescribeInstance(t *testing.T) {
	f := newFakeAPI(t)
	m := provider.Machine{ID: "uuid-1", Kind: provider.KindCloud, Labels: map[string]string{"project": "Production", "project_id": "p1"}}
	d, err := f.provider(t).(provider.Describer).Describe(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "vm-01" || d.Product != "b3-8" || d.Labels["project"] != "Production" {
		t.Errorf("machine: %+v", d.Machine)
	}
	if got := sectionNames(d); got != "Instance,Flavor,Image,Addresses,Access,Project" {
		t.Fatalf("sections: %s", got)
	}
	want := map[string]any{
		"Instance/billing": "hourly", "Instance/created": "2026-03-01T10:00:00Z",
		"Flavor/vcpus": 2, "Flavor/ram": provider.Bytes(8 << 30), "Flavor/disk": provider.Bytes(50 << 30),
		"Image/name": "Debian 12", "Image/user": "debian",
		"Addresses/public_ipv4": []string{"198.51.100.7"}, "Addresses/public_ipv6": []string{"2001:db8:7::1"},
		"Addresses/private": []string{"10.0.0.5 (network net-1)"},
		"Access/ssh_key":    "laptop", "Project/id": "p1",
	}
	for path, w := range want {
		if got := field(d, path); !reflect.DeepEqual(got, w) {
			t.Errorf("%s: got %#v (%T), want %#v", path, got, got, w)
		}
	}
	if _, err := f.provider(t).(provider.Describer).Describe(context.Background(), provider.Machine{ID: "uuid-1", Kind: provider.KindCloud}); err == nil {
		t.Error("instance without project_id label accepted")
	}
}
