package linode

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ofzhur/ump/internal/provider"
)

// fakeFinnix is the console of an instance in rescue mode, as Finnix
// has it: boot messages, the login asking the terminal for its size, and
// the prompt only once that was answered. A command typed before the
// prompt is swallowed half and leaves an open quote, as on the real
// console, and Ctrl-C leaves it taking nothing at all.
type fakeFinnix struct {
	mu       sync.Mutex
	typed    strings.Builder
	prompted bool
	broken   bool
	tries    int
	out      *io.PipeWriter
	in       *io.PipeReader
	closed   bool
}

func newFakeFinnix() *fakeFinnix {
	r, w := io.Pipe()
	f := &fakeFinnix{in: r, out: w}
	go w.Write([]byte("[  OK  ] Started Network Configuration.\r\n\nFinnix 126 (ttyS0)\r\n\x1b[24;200H\x1b[6n"))
	return f
}

func (f *fakeFinnix) Read(p []byte) (int, error) { return f.in.Read(p) }

func (f *fakeFinnix) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.typed.Write(p)
	var reply string
	switch s := string(p); {
	case strings.Contains(s, "\x03"):
		f.broken = true
		reply = "^C\r\n"
	case s == "\x1b[24;200R":
		f.prompted = true
		reply = "root@finnix:~# "
	case s == "\r":
		if f.prompted && !f.broken {
			reply = "\r\nroot@finnix:~# "
		}
	case strings.Contains(s, "systemctl start ssh"):
		f.tries++
		switch {
		case f.broken:
		case !f.prompted:
			reply = "\r\n> "
			f.broken = true
		default:
			// the shell echoes the command, then runs it
			reply = strings.TrimSpace(s) + "\r\n" + readyMark + "\r\nroot@finnix:~# "
		}
	}
	f.mu.Unlock()
	if reply != "" {
		go f.out.Write([]byte(reply))
	}
	return len(p), nil
}

func (f *fakeFinnix) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.out.Close()
	return nil
}

func testIdentity(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "staging_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, sshPub
}

func TestEnableRescue(t *testing.T) {
	oldPoll, oldBoot, oldConsole := rescuePoll, rescueBoot, rescueConsole
	rescuePoll, rescueBoot, rescueConsole = 5*time.Millisecond, 5*time.Second, 5*time.Second
	defer func() { rescuePoll, rescueBoot, rescueConsole = oldPoll, oldBoot, oldConsole }()

	identity, pub := testIdentity(t)
	var mu sync.Mutex
	var writes []string
	var profileKeys []string
	rescued := false
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "GET /linode/instances/42":
			status := "running"
			if rescued {
				polls++
				if polls == 1 {
					status = "rebooting"
				}
			}
			io.WriteString(w, `{"id":42,"label":"web-01","region":"de-fra-2","status":"`+status+`","ipv4":["172.105.1.2"]}`)
		case "GET /profile":
			keys, _ := json.Marshal(profileKeys)
			io.WriteString(w, `{"username":"jdoe","lish_auth_method":"password_keys","authorized_keys":`+string(keys)+`}`)
		case "PUT /profile":
			var in struct {
				AuthorizedKeys []string `json:"authorized_keys"`
			}
			json.Unmarshal(body, &in)
			profileKeys = in.AuthorizedKeys
			writes = append(writes, "PUT /profile")
			io.WriteString(w, `{}`)
		case "GET /linode/instances/42/configs":
			io.WriteString(w, `{"data":[{"id":7,"devices":{"sda":{"disk_id":100,"volume_id":null},"sdb":{"disk_id":101,"volume_id":null},"sdc":null}}],"page":1,"pages":1}`)
		case "POST /linode/instances/42/rescue":
			writes = append(writes, "POST rescue "+strings.TrimSpace(string(body)))
			rescued = true
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, err := newProvider("lin", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lp := p.(*Provider)
	profileKeys = []string{"ssh-rsa AAAAother laptop"}
	console := newFakeFinnix()
	var dialed string
	dials := 0
	lp.console = func(ctx context.Context, region, user, label string, signer ssh.Signer) (io.ReadWriteCloser, error) {
		dialed = region + " " + user + " " + label + " " + ssh.FingerprintSHA256(signer.PublicKey())
		dials++
		if dials == 1 {
			// Lish drops the console when the reboot comes after the dial
			dropped := newFakeFinnix()
			dropped.Close()
			return dropped, nil
		}
		return console, nil
	}
	m := provider.Machine{Provider: TypeName, Account: "lin", ID: "42", Name: "web-01", IPv4: "172.105.1.2"}
	keys := []provider.PublicKey{{Name: "ump", Data: "ssh-ed25519 AAAAump ump@host"}, {Name: "me", Data: "ssh-ed25519 AAAAme me@laptop"}}

	if _, err := lp.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: keys, NoReboot: true, Identity: identity}); err == nil || !strings.Contains(err.Error(), "starts with a reboot") {
		t.Errorf("--no-reboot: %v", err)
	}
	if _, err := lp.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: keys}); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("without the identity: %v", err)
	}

	info, err := lp.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: keys, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	if info.Address != "172.105.1.2" || info.User != "root" || !info.Reboots || !info.Ready {
		t.Errorf("info %+v", info)
	}
	if dials != 2 {
		t.Errorf("console dialed %d times, want a second time after the drop", dials)
	}
	if want := "de-fra-2 jdoe web-01 " + ssh.FingerprintSHA256(pub); dialed != want {
		t.Errorf("console dialed as %q, want %q", dialed, want)
	}
	// UMP's key joins the profile's Lish keys; the other key stays
	if len(profileKeys) != 2 || profileKeys[0] != "ssh-rsa AAAAother laptop" || !strings.HasPrefix(profileKeys[1], strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))) {
		t.Errorf("Lish keys %q", profileKeys)
	}
	if len(writes) != 2 || writes[1] != `POST rescue {"devices":{"sda":{"disk_id":100},"sdb":{"disk_id":101}}}` {
		t.Errorf("writes %q", writes)
	}
	typed := console.typed.String()
	if console.tries != 1 || !console.prompted || console.broken || !strings.Contains(typed, "printf '%s\\n' 'ssh-ed25519 AAAAump ump@host' 'ssh-ed25519 AAAAme me@laptop' > /root/.ssh/authorized_keys") || !console.closed {
		t.Errorf("console: %d tries, closed %v, typed %q", console.tries, console.closed, typed)
	}

	// a second rescue finds the key in the profile and leaves it alone
	writes, rescued, polls = nil, false, 0
	lp.console = func(ctx context.Context, region, user, label string, signer ssh.Signer) (io.ReadWriteCloser, error) {
		return newFakeFinnix(), nil
	}
	if _, err := lp.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: keys, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "POST rescue") {
		t.Errorf("second rescue writes %q", writes)
	}

	// a key that cannot be typed is refused before anything happens
	writes = nil
	if _, err := lp.EnableRescue(context.Background(), m, provider.RescueOptions{Keys: []provider.PublicKey{{Name: "odd", Data: "ssh-ed25519 AAAA it's"}}, Identity: identity}); err == nil || len(writes) != 0 {
		t.Errorf("odd key: %v, writes %q", err, writes)
	}
	if on, err := lp.RescueEnabled(context.Background(), m); on || err != nil {
		t.Errorf("RescueEnabled = %v, %v", on, err)
	}
}

func TestEnableRescueLishDisabled(t *testing.T) {
	identity, _ := testIdentity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/linode/instances/42":
			io.WriteString(w, `{"id":42,"label":"web-01","region":"de-fra-2","status":"running"}`)
		case "/profile":
			io.WriteString(w, `{"username":"jdoe","lish_auth_method":"disabled","authorized_keys":null}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	p, _ := newProvider("lin", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{})
	m := provider.Machine{Provider: TypeName, Account: "lin", ID: "42", IPv4: "172.105.1.2"}
	_, err := p.(provider.Rescue).EnableRescue(context.Background(), m, provider.RescueOptions{Keys: []provider.PublicKey{{Data: "ssh-ed25519 AAAAump"}}, Identity: identity})
	if err == nil || !strings.Contains(err.Error(), "Lish is disabled") {
		t.Errorf("err %v", err)
	}
}

func TestBootFromDisk(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "GET /linode/instances/42/configs":
			io.WriteString(w, `{"data":[
				{"id":7,"kernel":"linode/grub2","helpers":{"updatedb_disabled":true,"distro":true,"modules_dep":true,"network":null,"devtmpfs_automount":true}},
				{"id":8,"kernel":"linode/direct-disk","helpers":{"updatedb_disabled":true,"distro":false,"modules_dep":false,"network":null,"devtmpfs_automount":false}}],"page":1,"pages":1}`)
		case "PUT /linode/instances/42/configs/7":
			writes = append(writes, r.URL.Path+" "+strings.TrimSpace(string(body)))
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p, _ := newProvider("lin", map[string]string{"token": "tok", "url": srv.URL}, provider.Options{})
	m := provider.Machine{Provider: TypeName, Account: "lin", ID: "42"}
	if err := p.(provider.DiskBooter).BootFromDisk(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := `/linode/instances/42/configs/7 {"helpers":{"devtmpfs_automount":false,"distro":false,"modules_dep":false,"updatedb_disabled":true},"kernel":"linode/direct-disk"}`
	if len(writes) != 1 || writes[0] != want {
		t.Errorf("writes %q", writes)
	}
}
