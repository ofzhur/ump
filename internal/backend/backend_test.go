package backend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// exercise runs the contract every backend must keep.
func exercise(t *testing.T, b Backend) {
	t.Helper()
	ctx := context.Background()
	data, version, err := b.Read(ctx, "config.yaml")
	if err != nil || len(data) != 0 || version != "" {
		t.Fatalf("empty read: %q %q %v", data, version, err)
	}
	if entries, err := b.List(ctx); err != nil || len(entries) != 0 {
		t.Fatalf("empty list: %v %v", entries, err)
	}
	if _, err := b.Write(ctx, "config.yaml", []byte("one"), "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("write with a wrong version: %v", err)
	}
	v1, err := b.Write(ctx, "config.yaml", []byte("one"), "")
	if err != nil || v1 == "" {
		t.Fatalf("first write: %q %v", v1, err)
	}
	if _, err := b.Write(ctx, "config.yaml", []byte("again"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second first write: %v", err)
	}
	data, version, err = b.Read(ctx, "config.yaml")
	if err != nil || string(data) != "one" || version != v1 {
		t.Fatalf("read after write: %q %q (want %q) %v", data, version, v1, err)
	}
	v2, err := b.Write(ctx, "config.yaml", []byte("two"), v1)
	if err != nil || v2 == "" || v2 == v1 {
		t.Fatalf("conditional write: %q %v", v2, err)
	}
	if _, err := b.Write(ctx, "config.yaml", []byte("three"), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("write with the old version: %v", err)
	}
	// files in subdirectories, binary content, listing
	if _, err := b.Write(ctx, "secrets/acc.token", []byte{0, 1, 2, 255}, ""); err != nil {
		t.Fatalf("binary write: %v", err)
	}
	if _, err := b.Write(ctx, "profiles/web.yaml", []byte("os: x\n"), ""); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	if _, err := b.Write(ctx, "../escape", []byte("no"), ""); err == nil {
		t.Fatal("a path outside the home was accepted")
	}
	got, _, err := b.Read(ctx, "secrets/acc.token")
	if err != nil || string(got) != string([]byte{0, 1, 2, 255}) {
		t.Fatalf("binary read: %q %v", got, err)
	}
	entries, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	versions := map[string]string{}
	for _, e := range entries {
		paths = append(paths, e.Path)
		versions[e.Path] = e.Version
	}
	sort.Strings(paths)
	if strings.Join(paths, ",") != "config.yaml,profiles/web.yaml,secrets/acc.token" {
		t.Fatalf("list: %v", paths)
	}
	if versions["config.yaml"] != v2 {
		t.Fatalf("list version of config.yaml: %q, want %q", versions["config.yaml"], v2)
	}
	if err := b.Delete(ctx, "profiles/web.yaml"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := b.Delete(ctx, "profiles/web.yaml"); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	if d, v, err := b.Read(ctx, "profiles/web.yaml"); err != nil || d != nil || v != "" {
		t.Fatalf("read after delete: %q %q %v", d, v, err)
	}
	if entries, _ := b.List(ctx); len(entries) != 2 {
		t.Fatalf("list after delete: %v", entries)
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
	if entries, _ := b.List(ctx); len(entries) != 2 {
		t.Fatalf("locks show up as files: %v", entries)
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
	exercise(t, &Local{Dir: filepath.Join(t.TempDir(), "home")})
}

func TestSSH(t *testing.T) {
	// the runner executes the command locally, which is what ssh would do
	// remotely: the scripts must work with a plain sh
	for _, tool := range []string{"sha256sum", "find"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
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
	exercise(t, &SSH{Host: "ops@jump", Dir: filepath.Join(t.TempDir(), "home"), Run: run})
}

type obj struct {
	body []byte
	etag string
}

// uriEncode is SigV4's encoding as the S3 documentation describes it,
// written apart from the client's so that the two check each other.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || strings.IndexByte("-_.~", c) >= 0 || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// checkSigV4 verifies a request's signature the way S3 and R2 do: from
// the path and the query as the server decoded them. It returns what is
// wrong, or "".
func checkSigV4(r *http.Request, secret string) string {
	auth := r.Header.Get("Authorization")
	var cred, signedHeaders, sig string
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ", ") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signedHeaders = v
		case "Signature":
			sig = v
		}
	}
	scopeParts := strings.SplitN(cred, "/", 2)
	if len(scopeParts) != 2 || sig == "" {
		return "no SigV4 authorization"
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != r.Header.Get("x-amz-content-sha256") {
		return "payload hash mismatch"
	}
	var pairs []string
	for k, vs := range r.URL.Query() {
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	sort.Strings(pairs)
	var headers strings.Builder
	for _, h := range strings.Split(signedHeaders, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		headers.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	canonical := strings.Join([]string{r.Method, uriEncode(r.URL.Path, false), strings.Join(pairs, "&"), headers.String(), signedHeaders, r.Header.Get("x-amz-content-sha256")}, "\n")
	scope := scopeParts[1]
	date := strings.SplitN(scope, "/", 2)[0]
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("x-amz-date") + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := []byte("AWS4" + secret)
	for _, part := range []string{date, strings.Split(scope, "/")[1], "s3", "aws4_request"} {
		k = mac(k, part)
	}
	if want := hex.EncodeToString(mac(k, toSign)); want != sig {
		return "SignatureDoesNotMatch for the canonical request:\n" + canonical
	}
	return ""
}

// fakeS3 is enough of S3 for a backend: GET, PUT with If-Match and
// If-None-Match, DELETE and list-type=2 with a prefix.
func fakeS3(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	objects := map[string]obj{}
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if msg := checkSigV4(r, "SK"); msg != "" {
			t.Errorf("%s %s: %s", r.Method, r.URL.RequestURI(), msg)
			w.WriteHeader(403)
			fmt.Fprintf(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>%s</Message></Error>", msg)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if r.URL.Query().Get("list-type") == "2" {
			prefix := r.URL.Query().Get("prefix")
			fmt.Fprint(w, "<ListBucketResult>")
			for k, o := range objects {
				if strings.HasPrefix(k, prefix) {
					fmt.Fprintf(w, "<Contents><Key>%s</Key><ETag>&quot;%s&quot;</ETag></Contents>", k, o.etag)
				}
			}
			fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		cur, exists := objects[key]
		switch r.Method {
		case http.MethodGet:
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", `"`+cur.etag+`"`)
			w.Write(cur.body)
		case http.MethodPut:
			if r.Header.Get("If-None-Match") == "*" && exists {
				w.WriteHeader(412)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && (!exists || `"`+cur.etag+`"` != m) {
				w.WriteHeader(412)
				return
			}
			body, _ := io.ReadAll(r.Body)
			n++
			etag := fmt.Sprintf("etag%d", n)
			objects[key] = obj{body: body, etag: etag}
			w.Header().Set("ETag", `"`+etag+`"`)
			w.WriteHeader(200)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
}

func TestS3(t *testing.T) {
	srv := fakeS3(t)
	defer srv.Close()
	exercise(t, &S3{Endpoint: srv.URL, Bucket: "bucket", Prefix: "fleet/ump", Region: "auto", AccessKey: "AK", SecretKey: "SK", Client: srv.Client()})
}

func TestHTTP(t *testing.T) {
	var mu sync.Mutex
	files := map[string]obj{}
	n := 0
	locks := map[string]LockInfo{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/fleet"), "/")
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
			list := []LockInfo{}
			for _, l := range locks {
				list = append(list, l)
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && r.URL.Query().Has("list"):
			list := []map[string]string{}
			for p, o := range files {
				list = append(list, map[string]string{"path": p, "version": o.etag})
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet:
			o, ok := files[path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", `"`+o.etag+`"`)
			w.Write(o.body)
		case r.Method == http.MethodPut:
			o, exists := files[path]
			if r.Header.Get("If-None-Match") == "*" && exists {
				w.WriteHeader(412)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && (!exists || `"`+o.etag+`"` != m) {
				w.WriteHeader(412)
				return
			}
			body, _ := io.ReadAll(r.Body)
			n++
			etag := fmt.Sprintf("v%d", n)
			files[path] = obj{body: body, etag: etag}
			w.Header().Set("ETag", `"`+etag+`"`)
			w.WriteHeader(200)
		case r.Method == http.MethodDelete:
			delete(files, path)
			w.WriteHeader(204)
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
	exercise(t, &Consul{Address: srv.URL, Prefix: "ump/home", Token: "ctok", Client: srv.Client()})
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
			seen := map[string]bool{}
			var keys []string
			for k := range secrets {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				rest := strings.TrimPrefix(k, prefix)
				if i := strings.Index(rest, "/"); i >= 0 {
					rest = rest[:i+1]
				}
				if !seen[rest] {
					seen[rest] = true
					keys = append(keys, rest)
				}
			}
			if keys == nil {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/metadata/"):
			s, ok := secrets[strings.TrimPrefix(r.URL.Path, "/v1/kv/metadata/")]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"current_version": s.version}})
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
	b := &Vault{Address: srv.URL, Mount: "kv", Prefix: "ump/home", Token: "vtok", Client: srv.Client()}
	exercise(t, b)
	// the value is base64 in the secret
	b.Write(context.Background(), "x", []byte("hi"), "")
	if s := secrets["ump/home/x"]; s == nil || s.data["data"] != base64.StdEncoding.EncodeToString([]byte("hi")) {
		t.Errorf("stored secret: %+v", secrets["ump/home/x"])
	}
}

func TestOpen(t *testing.T) {
	creds := Credentials{AccessKey: "a", SecretKey: "b", Token: "t"}
	for _, c := range []struct{ url, want string }{
		{"/tmp/x", "file:///tmp/x"},
		{"file:///srv/fleet/ump", "file:///srv/fleet/ump"},
		{"ssh://ops@jump/srv/ump", "ssh://ops@jump/srv/ump"},
		{"s3://bucket/fleet/ump", "s3://bucket/fleet/ump"},
		{"s3://bucket", "s3://bucket/"},
		{"r2://acct/bucket/fleet", "r2://acct/bucket/fleet"},
		{"https://state.example.net/ump", "https://state.example.net/ump"},
		{"consul://consul.example.net:8500/ump", "consul://consul.example.net:8500/ump"},
		{"vault://vault.example.net:8200/kv/ump", "vault://vault.example.net:8200/kv/ump"},
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
	for _, bad := range []string{"", "r2://acct", "ftp://x/y", "vault://h/kv", "ssh://host"} {
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
