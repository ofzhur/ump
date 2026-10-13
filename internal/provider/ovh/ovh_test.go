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
	"net/netip"
	"net/url"
	"reflect"
	"sort"
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
	denyVPS      bool
	vpsActions   []string
	vpsNetboot   string
	vpsName      string
	renamed      string
	reverse      map[string]map[string]string
	calls        atomic.Int32

	bootID          int
	rescueKey       string
	rebooted        int
	instanceRescue  bool
	instanceActions []string
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
	if strings.HasPrefix(r.URL.Path, "/ip/") {
		f.handleIP(w, r, body)
		return
	}
	switch r.URL.Path {
	case "/dedicated/server":
		if f.denyDedicate {
			f.deny(w)
			return
		}
		w.Write([]byte(`["ns1.ip-203-0-113.eu","ns2.ip-203-0-113.eu"]`))
	case "/dedicated/server/ns1.ip-203-0-113.eu":
		if r.Method == http.MethodPut {
			var fields map[string]any
			json.Unmarshal(body, &fields)
			if v, ok := fields["bootId"]; ok {
				f.bootID = int(v.(float64))
			}
			if v, ok := fields["rescueSshKey"]; ok {
				f.rescueKey = v.(string)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if f.bootID == 0 {
			f.bootID = 1
		}
		w.Write([]byte(`{"name":"ns1.ip-203-0-113.eu","ip":"203.0.113.20","datacenter":"gra2","region":"eu-west-gra","state":"ok",
		  "powerState":"poweredOn","commercialRange":"rise-1","os":"debian12_64","serverId":1001,"iam":{"displayName":"db-01","id":"x"},
		  "bootId":` + fmt.Sprint(f.bootID) + `,"rootDevice":"/dev/sda","rescueMail":"ops@example.net","monitoring":true,"supportLevel":"pro","linkSpeed":1000,
		  "availabilityZone":"eu-west-gra-a","rack":"R42"}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/reboot":
		f.rebooted++
		w.Write([]byte(`{"taskId":555,"function":"hardReboot","status":"todo","comment":"Reboot asked"}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/boot":
		switch r.URL.Query().Get("bootType") {
		case "rescue":
			w.Write([]byte(`[1122]`))
		case "harddisk":
			w.Write([]byte(`[1]`))
		default:
			w.Write([]byte(`[1,1122]`))
		}
	case "/dedicated/server/ns1.ip-203-0-113.eu/boot/1122":
		w.Write([]byte(`{"bootId":1122,"bootType":"rescue","description":"Rescue 64 pro","kernel":"rescue64-pro"}`))
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
	case "/cloud/project/p1/instance/uuid-1/reboot":
		var req struct {
			Type string `json:"type"`
		}
		json.Unmarshal(body, &req)
		f.instanceActions = append(f.instanceActions, "reboot:"+req.Type)
		w.WriteHeader(http.StatusOK)
	case "/cloud/project/p1/instance/uuid-1/start", "/cloud/project/p1/instance/uuid-1/stop":
		f.instanceActions = append(f.instanceActions, strings.TrimPrefix(r.URL.Path, "/cloud/project/p1/instance/uuid-1/"))
		w.WriteHeader(http.StatusOK)
	case "/cloud/project/p1/instance/uuid-1/rescueMode":
		var req struct {
			Rescue bool `json:"rescue"`
		}
		json.Unmarshal(body, &req)
		f.instanceRescue = req.Rescue
		f.instanceActions = append(f.instanceActions, fmt.Sprintf("rescue:%v", req.Rescue))
		w.Write([]byte(`{"adminPassword":"adm1n"}`))
	case "/cloud/project/p1/instance/uuid-1":
		if r.Method == http.MethodPut {
			var req struct {
				InstanceName string `json:"instanceName"`
			}
			if err := json.Unmarshal(body, &req); err != nil || req.InstanceName == "" {
				f.t.Errorf("bad instance update: %s", body)
			}
			f.renamed = req.InstanceName
			w.WriteHeader(http.StatusOK)
			return
		}
		name := "vm-01"
		if f.renamed != "" {
			name = f.renamed
		}
		status := "ACTIVE"
		if f.instanceRescue {
			status = "RESCUE"
		}
		w.Write([]byte(`{"id":"uuid-1","name":"` + name + `","status":"` + status + `","region":"GRA11","created":"2026-03-01T10:00:00Z","planCode":"b3-8.consumption",
		  "flavor":{"name":"b3-8","type":"ovh.b3","vcpus":2,"ram":8192,"disk":50,"osType":"linux"},
		  "image":{"name":"Debian 12","type":"linux","user":"debian","visibility":"public"},"sshKey":{"name":"laptop"},"monthlyBilling":null,
		  "ipAddresses":[{"ip":"10.0.0.5","type":"private","version":4,"networkId":"net-1"},{"ip":"198.51.100.7","type":"public","version":4},{"ip":"2001:db8:7::1","type":"public","version":6}]}`))
	case "/dedicated/server/ns1.ip-203-0-113.eu/ips":
		w.Write([]byte(`["203.0.113.20/32","2001:db8:5:600::/64"]`))
	case "/dedicated/server/ns2.ip-203-0-113.eu":
		w.Write([]byte(`{"name":"ns2.ip-203-0-113.eu","ip":"203.0.113.21","datacenter":"rbx8","state":"ok","powerState":"poweredOff","commercialRange":"advance-2"}`))
	case "/dedicated/server/ns2.ip-203-0-113.eu/ips":
		w.Write([]byte(`["203.0.113.21/32"]`))
	case "/vps":
		if f.denyVPS {
			f.deny(w)
			return
		}
		w.Write([]byte(`["vps-00000000.vps.ovh.net"]`))
	case "/vps/vps-00000000.vps.ovh.net":
		if r.Method == http.MethodPut {
			var req map[string]string
			json.Unmarshal(body, &req)
			if v, ok := req["netbootMode"]; ok {
				f.vpsNetboot = v
				f.vpsActions = append(f.vpsActions, "netboot:"+v)
			}
			if v, ok := req["displayName"]; ok {
				f.vpsName = v
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		netboot, name := f.vpsNetboot, f.vpsName
		if netboot == "" {
			netboot = "local"
		}
		if name == "" {
			name = "edge-01"
		}
		w.Write([]byte(`{"name":"vps-00000000.vps.ovh.net","displayName":"` + name + `","state":"running","zone":"Region OpenStack: gra","cluster":"c-42","netbootMode":"` + netboot + `","offerType":"ssd","vcore":2,"memoryLimit":4096,
		  "model":{"name":"vps-2024-le-2","offer":"VPS Le-2","version":"2024v1","vcore":2,"memory":4096,"disk":80},"iam":{"displayName":"ignored"}}`))
	case "/vps/vps-00000000.vps.ovh.net/ips":
		w.Write([]byte(`["2001:db8:9::1","203.0.113.99"]`))
	case "/vps/vps-00000000.vps.ovh.net/ips/203.0.113.99":
		w.Write([]byte(`{"ipAddress":"203.0.113.99","version":4,"type":"primary","reverse":"edge-01.example.net.","gateway":"203.0.113.1"}`))
	case "/vps/vps-00000000.vps.ovh.net/ips/2001:db8:9::1":
		w.Write([]byte(`{"ipAddress":"2001:db8:9::1","version":6,"type":"additional","reverse":""}`))
	case "/vps/vps-00000000.vps.ovh.net/serviceInfos":
		w.Write([]byte(`{"creation":"2026-09-01","expiration":"2026-11-01","status":"ok","renewalType":"automaticV2016"}`))
	case "/vps/vps-00000000.vps.ovh.net/reboot", "/vps/vps-00000000.vps.ovh.net/start", "/vps/vps-00000000.vps.ovh.net/stop":
		f.vpsActions = append(f.vpsActions, strings.TrimPrefix(r.URL.Path, "/vps/vps-00000000.vps.ovh.net/"))
		w.Write([]byte(`{"id":7,"type":"rebootVm","state":"todo"}`))
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

// handleIP serves /ip/{block}/reverse for the blocks of ns1 and vm-01.
func (f *fakeAPI) handleIP(w http.ResponseWriter, r *http.Request, body []byte) {
	if f.reverse == nil {
		f.reverse = map[string]map[string]string{
			"203.0.113.20/32":     {"203.0.113.20": "db-01.example.net."},
			"2001:db8:5:600::/64": {"2001:db8:5:600::1": "db-01-v6.example.net."},
			"198.51.100.7/32":     {},
			"2001:db8:7::/64":     {},
		}
	}
	// path: /ip/<escaped block>/reverse[/<ip>]
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/ip/")
	blockEsc, tail, _ := strings.Cut(rest, "/reverse")
	block, _ := url.PathUnescape(blockEsc)
	entries, ok := f.reverse[block]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"The requested object (ip = ` + block + `) does not exist","errorCode":"NOT_FOUND"}`))
		return
	}
	ip, _ := url.PathUnescape(strings.TrimPrefix(tail, "/"))
	switch {
	case r.Method == http.MethodGet && ip == "":
		ips := []string{}
		for k := range entries {
			ips = append(ips, k)
		}
		sort.Strings(ips)
		json.NewEncoder(w).Encode(ips)
	case r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(ReverseIP{IPReverse: ip, Reverse: entries[ip]})
	case r.Method == http.MethodPost:
		var req ReverseIP
		json.Unmarshal(body, &req)
		if !strings.HasSuffix(req.Reverse, ".") {
			f.t.Errorf("reverse without trailing dot: %q", req.Reverse)
		}
		entries[req.IPReverse] = req.Reverse
		json.NewEncoder(w).Encode(req)
	case r.Method == http.MethodDelete:
		if _, ok := entries[ip]; !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"not found","errorCode":"NOT_FOUND"}`))
			return
		}
		delete(entries, ip)
		w.WriteHeader(http.StatusOK)
	default:
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
	if len(ms) != 4 {
		t.Fatalf("got %d machines, want 4: %+v", len(ms), ms)
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
	if len(ms) != 3 {
		t.Errorf("got %d machines, want the 2 dedicated ones and the VPS", len(ms))
	}
	f.denyVPS = true
	if ms, err = f.provider(t).List(context.Background()); err != nil || len(ms) != 2 {
		t.Errorf("VPS denied: %v, %d machines", err, len(ms))
	}
	f.denyDedicate = true
	_, err = f.provider(t).List(context.Background())
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "NOT_GRANTED_CALL" {
		t.Errorf("all families denied should fail, got %v", err)
	}
}

func TestVPS(t *testing.T) {
	f := newFakeAPI(t)
	p := f.provider(t)
	ms, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var vps provider.Machine
	for _, m := range ms {
		if m.ID == "vps-00000000.vps.ovh.net" {
			vps = m
		}
	}
	want := provider.Machine{Provider: "ovh", Account: "ovh", ID: "vps-00000000.vps.ovh.net", Name: "edge-01", Kind: provider.KindCloud,
		IPv4: "203.0.113.99", IPv6: "2001:db8:9::1", Location: "gra", Product: "vps-2024-le-2", Status: "running", Labels: map[string]string{"family": "vps"}}
	if !reflect.DeepEqual(vps, want) {
		t.Errorf("vps:\n got %+v\nwant %+v", vps, want)
	}

	pw := p.(provider.Power)
	if st, err := pw.PowerState(context.Background(), vps); err != nil || st != provider.PowerOn {
		t.Errorf("power state: %v %v", st, err)
	}
	if err := pw.Reboot(context.Background(), vps, provider.RebootPowerCycle); err != nil {
		t.Fatal(err)
	}
	if err := pw.Reboot(context.Background(), vps, provider.RebootSoft); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOff(context.Background(), vps, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.vpsActions, ","); got != "stop,start,reboot,stop" {
		t.Errorf("actions: %s", got)
	}

	rs := p.(provider.Rescue)
	f.vpsActions = nil
	info, err := rs.EnableRescue(context.Background(), vps, provider.RescueOptions{Keys: []provider.PublicKey{{Data: "ssh-ed25519 AAAA x"}}})
	if err != nil || info.Address != "203.0.113.99" || info.User != "root" || !strings.Contains(info.Note, "mails the rescue password") || info.Reboots {
		t.Errorf("enable rescue: %+v %v", info, err)
	}
	if on, err := rs.RescueEnabled(context.Background(), vps); err != nil || !on {
		t.Errorf("rescue enabled: %v %v", on, err)
	}
	if err := rs.DisableRescue(context.Background(), vps); err != nil {
		t.Fatal(err)
	}
	if on, _ := rs.RescueEnabled(context.Background(), vps); on || strings.Join(f.vpsActions, ",") != "netboot:rescue,netboot:local" {
		t.Errorf("rescue off: on=%v actions %v", on, f.vpsActions)
	}

	got, err := p.(provider.Renamer).Rename(context.Background(), vps, "edge-02")
	if err != nil || got.Name != "edge-02" || got.ID != vps.ID || got.IPv4 != "203.0.113.99" || !isVPS(got) {
		t.Errorf("rename: %+v %v", got, err)
	}

	d, err := p.(provider.Describer).Describe(context.Background(), vps)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("%+v", d.Sections)
	for _, want := range []string{"Server", "Model", "Addresses", "Service", "vps-2024-le-2", "4096 MB", "80 GB", "203.0.113.99", "reverse edge-01.example.net.", "2026-11-01"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe lacks %q:\n%s", want, text)
		}
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

func TestRenameInstance(t *testing.T) {
	f := newFakeAPI(t)
	renamer := f.provider(t).(provider.Renamer)
	m := provider.Machine{ID: "uuid-1", Kind: provider.KindCloud, Labels: map[string]string{"project": "Production", "project_id": "p1"}}
	got, err := renamer.Rename(context.Background(), m, "prod-vm-01")
	if err != nil {
		t.Fatal(err)
	}
	if f.renamed != "prod-vm-01" || got.Name != "prod-vm-01" || got.ID != "uuid-1" || got.Labels["project"] != "Production" {
		t.Errorf("renamed: sent %q, got %+v", f.renamed, got)
	}
	if _, err := renamer.Rename(context.Background(), provider.Machine{ID: "uuid-1", Kind: provider.KindCloud}, "x"); err == nil {
		t.Error("instance without project_id accepted")
	}
	_, err = renamer.Rename(context.Background(), provider.Machine{ID: "ns1.ip-203-0-113.eu", Kind: provider.KindBareMetal}, "db-02")
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(err.Error(), "dedicated servers cannot be renamed through the OVHcloud API") {
		t.Errorf("dedicated rename: %v", err)
	}
	if f.calls.Load() != 3 {
		t.Errorf("%d API calls, want 3 (time, update, get); the dedicated rename must not call the API", f.calls.Load())
	}
}

func TestDefaultAccessRulesCoverRename(t *testing.T) {
	found := false
	for _, r := range DefaultAccessRules {
		if r.Method == "PUT" && r.Path == "/cloud/project/*/instance/*" {
			found = true
		}
	}
	if !found {
		t.Error("the wizard's consumer key must allow renaming instances")
	}
}

func TestReverseDNS(t *testing.T) {
	f := newFakeAPI(t)
	rd := f.provider(t).(provider.ReverseDNS)
	ded := provider.Machine{ID: "ns1.ip-203-0-113.eu", Kind: provider.KindBareMetal}
	ptrs, err := rd.PTRs(context.Background(), ded)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.PTR{{IP: "203.0.113.20", Name: "db-01.example.net"}, {IP: "2001:db8:5:600::1", Name: "db-01-v6.example.net"}}
	if !reflect.DeepEqual(ptrs, want) {
		t.Errorf("dedicated PTRs: got %+v, want %+v", ptrs, want)
	}
	if err := rd.SetPTR(context.Background(), ded, netip.MustParseAddr("2001:db8:5:600::2"), "second.example.net"); err != nil {
		t.Fatal(err)
	}
	if f.reverse["2001:db8:5:600::/64"]["2001:db8:5:600::2"] != "second.example.net." {
		t.Errorf("entry not set: %v", f.reverse)
	}
	if err := rd.SetPTR(context.Background(), ded, netip.MustParseAddr("203.0.113.20"), ""); err != nil {
		t.Fatal(err)
	}
	if err := rd.SetPTR(context.Background(), ded, netip.MustParseAddr("203.0.113.20"), ""); err != nil {
		t.Errorf("deleting a missing entry: %v", err)
	}
	if _, ok := f.reverse["203.0.113.20/32"]["203.0.113.20"]; ok {
		t.Error("entry not deleted")
	}
	if err := rd.SetPTR(context.Background(), ded, netip.MustParseAddr("203.0.113.99"), "x.example.net"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("foreign address: %v", err)
	}

	vm := provider.Machine{ID: "uuid-1", Kind: provider.KindCloud, Labels: map[string]string{"project_id": "p1"}}
	ptrs, err = rd.PTRs(context.Background(), vm)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ptrs, []provider.PTR{{IP: "198.51.100.7"}}) {
		t.Errorf("instance PTRs: %+v", ptrs)
	}
	if err := rd.SetPTR(context.Background(), vm, netip.MustParseAddr("198.51.100.7"), "vm-01.example.net."); err != nil {
		t.Fatal(err)
	}
	ptrs, _ = rd.PTRs(context.Background(), vm)
	if len(ptrs) != 1 || ptrs[0].Name != "vm-01.example.net" {
		t.Errorf("instance PTRs after set: %+v", ptrs)
	}
}

func TestPowerAndRescue(t *testing.T) {
	f := newFakeAPI(t)
	p := f.provider(t)
	pw, rs := p.(provider.Power), p.(provider.Rescue)
	ded := provider.Machine{ID: "ns1.ip-203-0-113.eu", Kind: provider.KindBareMetal, IPv4: "203.0.113.20"}
	vm := provider.Machine{ID: "uuid-1", Kind: provider.KindCloud, IPv4: "198.51.100.7", Labels: map[string]string{"project_id": "p1"}}

	if st, err := pw.PowerState(context.Background(), ded); err != nil || st != provider.PowerOn {
		t.Errorf("dedicated PowerState: %v %v", st, err)
	}
	if err := pw.Reboot(context.Background(), ded, provider.RebootSoft); err != nil || f.rebooted != 1 {
		t.Errorf("dedicated reboot: %v, %d", err, f.rebooted)
	}
	if err := pw.PowerOff(context.Background(), ded, true); err == nil || !strings.Contains(err.Error(), "cannot be switched on or off") {
		t.Errorf("dedicated power off: %v", err)
	}
	if on, err := rs.RescueEnabled(context.Background(), ded); err != nil || on {
		t.Errorf("dedicated rescue state: %v %v", on, err)
	}
	info, err := rs.EnableRescue(context.Background(), ded, provider.RescueOptions{Keys: []provider.PublicKey{{Data: "ssh-ed25519 AAAA one"}, {Data: "ssh-ed25519 BBBB two"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f.bootID != 1122 || f.rescueKey != "ssh-ed25519 AAAA one" || info.Reboots || !strings.Contains(info.Note, "one rescue SSH key") || info.Address != "203.0.113.20" {
		t.Errorf("dedicated rescue: bootId %d key %q info %+v", f.bootID, f.rescueKey, info)
	}
	if on, err := rs.RescueEnabled(context.Background(), ded); err != nil || !on {
		t.Errorf("dedicated rescue state after enable: %v %v", on, err)
	}
	if err := rs.DisableRescue(context.Background(), ded); err != nil || f.bootID != 1 {
		t.Errorf("dedicated unrescue: %v, bootId %d", err, f.bootID)
	}

	if st, err := pw.PowerState(context.Background(), vm); err != nil || st != provider.PowerOn {
		t.Errorf("instance PowerState: %v %v", st, err)
	}
	for _, mode := range []provider.RebootMode{provider.RebootSoft, provider.RebootHard, provider.RebootPowerCycle} {
		if err := pw.Reboot(context.Background(), vm, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := pw.PowerOff(context.Background(), vm, false); err != nil {
		t.Fatal(err)
	}
	if err := pw.PowerOn(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	info, err = rs.EnableRescue(context.Background(), vm, provider.RescueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Password != "adm1n" || !info.Reboots {
		t.Errorf("instance rescue: %+v", info)
	}
	if on, _ := rs.RescueEnabled(context.Background(), vm); !on {
		t.Error("instance rescue not reported")
	}
	if st, _ := pw.PowerState(context.Background(), vm); st != provider.PowerOn {
		t.Errorf("instance in rescue should count as on, got %s", st)
	}
	if err := rs.DisableRescue(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.instanceActions, ","); got != "reboot:soft,reboot:hard,stop,start,stop,start,rescue:true,rescue:false" {
		t.Errorf("instance actions: %s", got)
	}
}
