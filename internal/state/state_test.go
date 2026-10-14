package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ofzhur/ump/internal/provider"
)

// exercise runs the contract every backend must keep.
func exercise(t *testing.T, b Backend) {
	t.Helper()
	ctx := context.Background()
	data, version, err := b.Read(ctx)
	if err != nil || len(data) != 0 || version != "" {
		t.Fatalf("empty read: %q %q %v", data, version, err)
	}
	if _, err := b.Write(ctx, []byte("one"), "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("write with a wrong version: %v", err)
	}
	v1, err := b.Write(ctx, []byte("one"), "")
	if err != nil || v1 == "" {
		t.Fatalf("first write: %q %v", v1, err)
	}
	if _, err := b.Write(ctx, []byte("again"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second first write: %v", err)
	}
	data, version, err = b.Read(ctx)
	if err != nil || string(data) != "one" || version != v1 {
		t.Fatalf("read after write: %q %q (want %q) %v", data, version, v1, err)
	}
	v2, err := b.Write(ctx, []byte("two"), v1)
	if err != nil || v2 == "" || v2 == v1 {
		t.Fatalf("conditional write: %q %v", v2, err)
	}
	if _, err := b.Write(ctx, []byte("three"), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("write with the old version: %v", err)
	}
	// locks are per key
	info := LockInfo{Key: "fake/1", Who: "alice@laptop", What: "reinstall"}
	if err := b.Lock(ctx, "fake/1", info); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := b.Lock(ctx, "fake/2", LockInfo{Key: "fake/2", Who: "bob"}); err != nil {
		t.Fatalf("lock of another key: %v", err)
	}
	var locked *ErrLocked
	if err := b.Lock(ctx, "fake/1", LockInfo{Key: "fake/1", Who: "bob"}); !errors.As(err, &locked) || locked.Info.Who != "alice@laptop" || locked.Info.What != "reinstall" {
		t.Fatalf("lock held: %v", err)
	}
	locks, err := b.Locks(ctx)
	if err != nil || len(locks) != 2 {
		t.Fatalf("locks: %+v %v", locks, err)
	}
	if err := b.Unlock(ctx, "fake/1"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := b.Unlock(ctx, "fake/1"); err != nil {
		t.Fatalf("unlock twice: %v", err)
	}
	if err := b.Lock(ctx, "fake/1", info); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	b.Unlock(ctx, "fake/1")
	b.Unlock(ctx, "fake/2")
	if locks, _ := b.Locks(ctx); len(locks) != 0 {
		t.Fatalf("locks left: %+v", locks)
	}
}

func TestLocal(t *testing.T) {
	exercise(t, &Local{Path: filepath.Join(t.TempDir(), "sub", "state.yaml")})
}

func TestSSH(t *testing.T) {
	// the runner executes the command locally, which is what ssh would do
	// remotely: the scripts must work with a plain sh
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	dir := t.TempDir()
	run := func(ctx context.Context, host, command string, stdin []byte) ([]byte, int, error) {
		if host != "ops@jump" {
			t.Errorf("host %q", host)
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = strings.NewReader(string(stdin))
		out, err := cmd.Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, exit.ExitCode(), nil
		}
		return out, 0, err
	}
	exercise(t, &SSH{Host: "ops@jump", Path: filepath.Join(dir, "state.yaml"), Run: run})
}

// fakeS3 is enough of S3 for the state: GET, PUT with If-Match and
// If-None-Match, DELETE and list-type=2 with a prefix.
func fakeS3(t *testing.T) (*httptest.Server, *sync.Map) {
	var objects sync.Map // key -> {body, etag}
	type obj struct {
		body []byte
		etag string
	}
	n := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("x-amz-content-sha256") == "" {
			w.WriteHeader(403)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if r.URL.Query().Get("list-type") == "2" {
			prefix := r.URL.Query().Get("prefix")
			var keys []string
			objects.Range(func(k, _ any) bool {
				if strings.HasPrefix(k.(string), prefix) {
					keys = append(keys, k.(string))
				}
				return true
			})
			fmt.Fprint(w, "<ListBucketResult>")
			for _, k := range keys {
				fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
			}
			fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		cur, exists := objects.Load(key)
		switch r.Method {
		case http.MethodGet:
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", `"`+cur.(obj).etag+`"`)
			w.Write(cur.(obj).body)
		case http.MethodPut:
			if r.Header.Get("If-None-Match") == "*" && exists {
				w.WriteHeader(412)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && (!exists || `"`+cur.(obj).etag+`"` != m) {
				w.WriteHeader(412)
				return
			}
			body, _ := io.ReadAll(r.Body)
			n++
			etag := fmt.Sprintf("etag%d", n)
			objects.Store(key, obj{body: body, etag: etag})
			w.Header().Set("ETag", `"`+etag+`"`)
			w.WriteHeader(200)
		case http.MethodDelete:
			objects.Delete(key)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	return srv, &objects
}

func TestS3(t *testing.T) {
	srv, _ := fakeS3(t)
	defer srv.Close()
	exercise(t, &S3{Endpoint: srv.URL, Bucket: "bucket", Key: "fleet/state.yaml", Region: "auto", AccessKey: "AK", SecretKey: "SK", Client: srv.Client()})
}

func TestHTTP(t *testing.T) {
	var mu sync.Mutex
	var doc []byte
	etag := ""
	n := 0
	locks := map[string]LockInfo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Query().Get("key")
		switch {
		case r.Method == "LOCK":
			if held, ok := locks[key]; ok {
				w.WriteHeader(423)
				json.NewEncoder(w).Encode(held)
				return
			}
			var info LockInfo
			json.NewDecoder(r.Body).Decode(&info)
			locks[key] = info
			w.WriteHeader(200)
		case r.Method == "UNLOCK":
			delete(locks, key)
			w.WriteHeader(204)
		case r.Method == http.MethodGet && r.URL.Query().Has("locks"):
			var list []LockInfo
			for _, l := range locks {
				list = append(list, l)
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet:
			if doc == nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", `"`+etag+`"`)
			w.Write(doc)
		case r.Method == http.MethodPut:
			if r.Header.Get("If-None-Match") == "*" && doc != nil {
				w.WriteHeader(412)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && (doc == nil || `"`+etag+`"` != m) {
				w.WriteHeader(412)
				return
			}
			doc, _ = io.ReadAll(r.Body)
			n++
			etag = fmt.Sprintf("v%d", n)
			w.Header().Set("ETag", `"`+etag+`"`)
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	exercise(t, &HTTP{URL: srv.URL + "/fleet", Token: "tok", Client: srv.Client()})
}

func TestConsul(t *testing.T) {
	var mu sync.Mutex
	type entry struct {
		value []byte
		index uint64
	}
	kv := map[string]entry{}
	var counter uint64 = 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Consul-Token") != "ctok" {
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Has("recurse") {
				var out []map[string]any
				for k, e := range kv {
					if strings.HasPrefix(k, key) {
						out = append(out, map[string]any{"Key": k, "Value": e.value, "ModifyIndex": e.index})
					}
				}
				if out == nil {
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(out)
				return
			}
			e, ok := kv[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{"Key": key, "Value": e.value, "ModifyIndex": e.index}})
		case http.MethodPut:
			cas := r.URL.Query().Get("cas")
			e, ok := kv[key]
			if cas == "0" && ok || cas != "0" && (!ok || fmt.Sprint(e.index) != cas) {
				fmt.Fprint(w, "false")
				return
			}
			body, _ := io.ReadAll(r.Body)
			counter++
			kv[key] = entry{value: body, index: counter}
			fmt.Fprint(w, "true")
		case http.MethodDelete:
			delete(kv, key)
			fmt.Fprint(w, "true")
		}
	}))
	defer srv.Close()
	exercise(t, &Consul{Address: srv.URL, Path: "ump/state", Token: "ctok", Client: srv.Client()})
}

func TestVault(t *testing.T) {
	var mu sync.Mutex
	type secret struct {
		data    map[string]string
		version int
	}
	secrets := map[string]*secret{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "vtok" {
			w.WriteHeader(403)
			w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "LIST" && strings.HasPrefix(r.URL.Path, "/v1/kv/metadata/"):
			prefix := strings.TrimPrefix(r.URL.Path, "/v1/kv/metadata/")
			var keys []string
			for k := range secrets {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, strings.TrimPrefix(k, prefix))
				}
			}
			if keys == nil {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/kv/metadata/"):
			delete(secrets, strings.TrimPrefix(r.URL.Path, "/v1/kv/metadata/"))
			w.WriteHeader(204)
		case r.Method == http.MethodGet:
			s, ok := secrets[strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")]
			if !ok {
				w.WriteHeader(404)
				w.Write([]byte(`{"errors":[]}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": s.data, "metadata": map[string]any{"version": s.version}}})
		case r.Method == http.MethodPost:
			path := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")
			var req struct {
				Data    map[string]string `json:"data"`
				Options struct {
					CAS int `json:"cas"`
				} `json:"options"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			s := secrets[path]
			cur := 0
			if s != nil {
				cur = s.version
			}
			if req.Options.CAS != cur {
				w.WriteHeader(400)
				w.Write([]byte(`{"errors":["check-and-set parameter did not match the current version"]}`))
				return
			}
			secrets[path] = &secret{data: req.Data, version: cur + 1}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": cur + 1}})
		}
	}))
	defer srv.Close()
	exercise(t, &Vault{Address: srv.URL, Mount: "kv", Path: "ump/state", Token: "vtok", Client: srv.Client()})
}

func TestStoreUpdateMerges(t *testing.T) {
	// two writers about different servers: the second's write conflicts
	// and is retried on the fresh document, so both changes land
	b := &Local{Path: filepath.Join(t.TempDir(), "state.yaml")}
	s := &Store{Backend: b}
	ctx := context.Background()
	var once sync.Once
	err := s.Update(ctx, func(d *Document) error {
		d.Servers["fake/1"] = Server{Profile: "web"}
		once.Do(func() {
			if err := s.Update(ctx, func(d *Document) error {
				d.Servers["fake/2"] = Server{Price: &provider.Price{Amount: 5, Currency: "EUR", Period: "month"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.Read(ctx)
	if err != nil || d.Servers["fake/1"].Profile != "web" || d.Servers["fake/2"].Price == nil {
		t.Fatalf("merged state: %+v %v", d, err)
	}
	data, _ := os.ReadFile(b.Path)
	if !strings.HasPrefix(string(data), "# UMP state") || !strings.Contains(string(data), "version: 1\n") {
		t.Errorf("file:\n%s", data)
	}
	// an empty record disappears
	if err := s.Update(ctx, func(d *Document) error { d.Servers["fake/1"] = Server{}; return nil }); err != nil {
		t.Fatal(err)
	}
	d, _, _ = s.Read(ctx)
	if _, ok := d.Servers["fake/1"]; ok {
		t.Error("empty record kept")
	}
	// the lock through the store
	unlock, err := s.Lock(ctx, "fake/1", "reinstall")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, "fake/1", "rescue"); err == nil || !strings.Contains(err.Error(), "fake/1 is locked by") || !strings.Contains(err.Error(), "ump state unlock fake/1 forces it") {
		t.Errorf("second lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestParseAndOpen(t *testing.T) {
	if _, err := Parse([]byte("version: 7\n")); err == nil || !strings.Contains(err.Error(), "version 7") {
		t.Errorf("newer version: %v", err)
	}
	if _, err := Parse([]byte("servers:\n  fake/1:\n    price: {amount: 1}\n")); err == nil || !strings.Contains(err.Error(), "servers.fake/1.price") {
		t.Errorf("bad price: %v", err)
	}
	if _, err := Parse([]byte("colour: blue\n")); err == nil {
		t.Error("unknown field accepted")
	}
	creds := Credentials{AccessKey: "a", SecretKey: "b", Token: "t"}
	for _, c := range []struct{ url, want string }{
		{"", "file://" + DefaultPath()},
		{"local", "file://" + DefaultPath()},
		{"/tmp/x.yaml", "file:///tmp/x.yaml"},
		{"file:///tmp/x.yaml", "file:///tmp/x.yaml"},
		{"ssh://ops@jump/srv/ump/state.yaml", "ssh://ops@jump/srv/ump/state.yaml"},
		{"s3://bucket/fleet/state.yaml", "s3://bucket/fleet/state.yaml"},
		{"r2://acct/bucket/fleet/state.yaml", "r2://acct/bucket/fleet/state.yaml"},
		{"https://state.example.net/ump", "https://state.example.net/ump"},
		{"consul://consul.example.net:8500/ump/state", "consul://consul.example.net:8500/ump/state"},
		{"vault://vault.example.net:8200/kv/ump/state", "vault://vault.example.net:8200/kv/ump/state"},
	} {
		b, err := Open(c.url, creds)
		if err != nil || b.String() != c.want {
			t.Errorf("Open(%q) = %v, %v; want %s", c.url, b, err, c.want)
		}
	}
	if b, _ := Open("s3://bucket/k", Credentials{AccessKey: "a", SecretKey: "b", Region: "eu-central-1"}); b.(*S3).Endpoint != "https://s3.eu-central-1.amazonaws.com" {
		t.Errorf("s3 endpoint: %s", b.(*S3).Endpoint)
	}
	if b, _ := Open("r2://acct/bucket/k", creds); b.(*S3).Endpoint != "https://acct.r2.cloudflarestorage.com" || b.(*S3).Region != "auto" {
		t.Errorf("r2: %+v", b)
	}
	if b, _ := Open("http+consul://c:8500/p", creds); b.(*Consul).Address != "http://c:8500" {
		t.Errorf("http+consul: %+v", b)
	}
	for _, bad := range []string{"s3://bucket", "r2://acct/bucket", "ftp://x/y", "vault://h/kv", "ssh://host"} {
		if _, err := Open(bad, creds); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}
	if _, err := Open("s3://b/k", Credentials{}); err == nil || !strings.Contains(err.Error(), "access key") {
		t.Errorf("s3 without keys: %v", err)
	}
	if _, err := Open("vault://h:8200/kv/p", Credentials{}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("vault without token: %v", err)
	}
}
