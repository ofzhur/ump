package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// exerciseObjects runs the contract every storage keeps: objects by
// name, written as a whole, and All with the host files directly under
// the prefix.
func exerciseObjects(t *testing.T, o Objects) {
	t.Helper()
	ctx := context.Background()
	if all, err := o.All(ctx); err != nil || len(all) != 0 {
		t.Fatalf("empty: %v %v", all, err)
	}
	if data, ok, err := o.Get(ctx, "web-01.yml"); err != nil || ok || data != nil {
		t.Fatalf("no object: %q %v %v", data, ok, err)
	}
	file := "# web-01\nplugin: ofzhur.ansible.host\ngroups: [common, web]\nvars:\n  ump_ref: fake/1\n"
	for name, data := range map[string]string{"web-01.yml": file, "db-01.yaml": "plugin: ofzhur.ansible.host\n", ".hidden.yml": "x", "notes.txt": "x", "empty.yml": ""} {
		if err := o.Put(ctx, name, []byte(data)); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
	if data, ok, err := o.Get(ctx, "web-01.yml"); err != nil || !ok || string(data) != file {
		t.Fatalf("get: %q %v %v", data, ok, err)
	}
	all, err := o.All(ctx)
	if err != nil || strings.Join(slices.Sorted(maps.Keys(all)), ",") != "db-01.yaml,empty.yml,web-01.yml" || string(all["web-01.yml"]) != file || len(all["empty.yml"]) != 0 {
		t.Fatalf("all: %q %v", all, err)
	}
	if err := o.Put(ctx, "web-01.yml", []byte("plugin: ofzhur.ansible.host\n")); err != nil {
		t.Fatal(err)
	}
	if data, _, _ := o.Get(ctx, "web-01.yml"); string(data) != "plugin: ofzhur.ansible.host\n" {
		t.Errorf("replaced: %q", data)
	}
	for _, name := range []string{"web-01.yml", "web-01.yml", "gone.yml"} {
		if err := o.Remove(ctx, name); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
	if all, err := o.All(ctx); err != nil || strings.Join(slices.Sorted(maps.Keys(all)), ",") != "db-01.yaml,empty.yml" {
		t.Fatalf("after remove: %q %v", all, err)
	}
}

func TestDirectoryObjects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fleet", "inventory")
	o, err := OpenStorage("file://"+dir, StorageOptions{})
	if err != nil || o.String() != "file://"+dir {
		t.Fatalf("open: %v %v", o, err)
	}
	exerciseObjects(t, o)
	// the collection's modes; nothing left behind
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "db-01.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file: %v %v", fi.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ".hidden.yml,db-01.yaml,empty.yml,notes.txt" {
		t.Errorf("files: %v", names)
	}
	// a directory, and a link to a file
	os.Mkdir(filepath.Join(dir, "sub.yml"), 0o700)
	os.Symlink("db-01.yaml", filepath.Join(dir, "link.yml"))
	if all, err := o.All(context.Background()); err != nil || strings.Join(slices.Sorted(maps.Keys(all)), ",") != "db-01.yaml,empty.yml,link.yml" {
		t.Errorf("all: %v %v", slices.Sorted(maps.Keys(all)), err)
	}
	// modes of the repository's inventory/hosts/
	hosts := &Directory{Dir: filepath.Join(t.TempDir(), "hosts"), DirMode: 0o755, FileMode: 0o644}
	if err := hosts.Put(context.Background(), "web-01.yml", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(hosts.Dir, "web-01.yml")); fi.Mode().Perm() != 0o644 {
		t.Errorf("host file: %v", fi.Mode())
	}
}

func TestSSHObjects(t *testing.T) {
	// the runner executes the command locally, which is what ssh would do
	// remotely: the scripts must work with a plain sh
	var argv []string
	open := func(target, port string, args []string) SSHRunner {
		argv = append([]string{target, port}, args...)
		return func(ctx context.Context, command string, stdin []byte) ([]byte, []byte, int, error) {
			cmd := exec.CommandContext(ctx, "sh", "-c", command)
			cmd.Stdin = bytes.NewReader(stdin)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			err := cmd.Run()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return out.Bytes(), errb.Bytes(), exit.ExitCode(), nil
			}
			return out.Bytes(), errb.Bytes(), 0, err
		}
	}
	dir := filepath.Join(t.TempDir(), "fleet dir", "inv")
	o, err := OpenStorage("ssh://ops@Jump.example:2222"+dir, StorageOptions{SSH: open, Option: func(name string) (string, error) {
		if name == "ssh_args" {
			return `-i "my key" -o ControlMaster=auto`, nil
		}
		return "", nil
	}})
	if err != nil || o.String() != "ssh://ops@jump.example:2222"+dir {
		t.Fatalf("open: %v %v", o, err)
	}
	if strings.Join(argv, "|") != "ops@jump.example|2222|-i|my key|-o|ControlMaster=auto" {
		t.Errorf("ssh: %q", argv)
	}
	exerciseObjects(t, o)
	if fi, err := os.Stat(filepath.Join(dir, "db-01.yaml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file: %v %v", fi, err)
	}
	// a failure names the reason
	failing := &sshObjects{dir: dir, run: func(ctx context.Context, command string, stdin []byte) ([]byte, []byte, int, error) {
		return nil, []byte("Warning: something\nops@jump: Permission denied (publickey).\n"), 255, nil
	}}
	if _, err := failing.All(context.Background()); err == nil || err.Error() != "reading: ssh: ops@jump: Permission denied (publickey)." {
		t.Errorf("failure: %v", err)
	}
}

func TestS3Objects(t *testing.T) {
	srv := fakeS3(t)
	defer srv.Close()
	options := StorageOptions{Client: srv.Client(),
		Option: func(name string) (string, error) {
			return map[string]string{"endpoint": srv.URL}[name], nil
		},
		Credential: func(name string) (string, error) {
			return map[string]string{"access_key": "AK", "secret_key": "SK"}[name], nil
		},
	}
	o, err := OpenStorage("s3://bucket/fleet/inventory/", options)
	if err != nil || o.String() != "s3://bucket/fleet/inventory" {
		t.Fatalf("open: %v %v", o, err)
	}
	exerciseObjects(t, o)
	// a deeper object is none of the prefix's
	o.Put(context.Background(), "x.yml", nil)
	deeper, _ := OpenStorage("s3://bucket/fleet", options)
	if all, err := deeper.All(context.Background()); err != nil || len(all) != 0 {
		t.Errorf("deeper: %v %v", all, err)
	}
}

func TestSessionToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "x-amz-security-token") {
			t.Errorf("token not signed: %s", r.Header.Get("Authorization"))
		}
		got = r.Header.Get("X-Amz-Security-Token")
		w.WriteHeader(404)
	}))
	defer srv.Close()
	o, err := OpenStorage("s3://bucket/inv", StorageOptions{Client: srv.Client(),
		Option: func(name string) (string, error) { return map[string]string{"endpoint": srv.URL}[name], nil },
		Credential: func(name string) (string, error) {
			return map[string]string{"access_key": "AK", "secret_key": "SK", "session_token": "ST"}[name], nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := o.Get(context.Background(), "web-01.yml"); ok || err != nil || got != "ST" {
		t.Errorf("get: %v %v %q", ok, err, got)
	}
}

func TestConsulObjects(t *testing.T) {
	var mu sync.Mutex
	kv := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Consul-Token") != "ctok" {
			w.WriteHeader(403)
			fmt.Fprint(w, "ACL not found")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		q := r.URL.Query()
		switch r.Method {
		case http.MethodGet:
			if q.Has("recurse") {
				var out []map[string]any
				for k, v := range kv {
					if strings.HasPrefix(k, key) {
						var value any
						if len(v) > 0 {
							value = base64.StdEncoding.EncodeToString(v)
						}
						out = append(out, map[string]any{"Key": k, "Value": value})
					}
				}
				if out == nil {
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(out)
				return
			}
			v, ok := kv[key]
			if !ok || !q.Has("raw") {
				w.WriteHeader(404)
				return
			}
			w.Write(v)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			kv[key] = body
			fmt.Fprint(w, "true")
		case http.MethodDelete:
			delete(kv, key)
			fmt.Fprint(w, "true")
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	creds := func(token string) StorageOptions {
		return StorageOptions{Client: srv.Client(), Credential: func(name string) (string, error) {
			return map[string]string{"consul_token": token}[name], nil
		}}
	}
	o, err := OpenStorage("http+consul://"+addr+"/fleet/inventory", creds("ctok"))
	if err != nil || o.String() != "http+consul://"+addr+"/fleet/inventory" {
		t.Fatalf("open: %v %v", o, err)
	}
	exerciseObjects(t, o)
	if string(kv["fleet/inventory/db-01.yaml"]) != "plugin: ofzhur.ansible.host\n" {
		t.Errorf("stored: %q", kv)
	}
	kv["fleet/inventory/sub/x.yml"] = []byte("x")
	if all, err := o.All(context.Background()); err != nil || len(all) != 2 {
		t.Errorf("deeper: %v %v", all, err)
	}
	denied, _ := OpenStorage("http+consul://"+addr+"/fleet/inventory", creds("wrong"))
	if _, err := denied.All(context.Background()); err == nil || err.Error() != "reading: ACL not found (HTTP 403)" {
		t.Errorf("denied: %v", err)
	}
}

func TestVaultObjects(t *testing.T) {
	var mu sync.Mutex
	secrets := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "vtok" || r.Header.Get("X-Vault-Namespace") != "admin" {
			w.WriteHeader(403)
			w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list") == "true":
			prefix := strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")
			var keys []string
			for k := range secrets {
				if rest, ok := strings.CutPrefix(k, prefix); ok {
					if i := strings.Index(rest, "/"); i >= 0 {
						rest = rest[:i+1]
					}
					if !slices.Contains(keys, rest) {
						keys = append(keys, rest)
					}
				}
			}
			if keys == nil {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		case r.Method == http.MethodDelete:
			delete(secrets, strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/"))
			w.WriteHeader(204)
		case r.Method == http.MethodGet:
			s, ok := secrets[strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")]
			if !ok {
				w.WriteHeader(404)
				w.Write([]byte(`{"errors":[]}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": s, "metadata": map[string]any{"version": 1}}})
		case r.Method == http.MethodPost:
			var req struct {
				Data map[string]any `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			secrets[strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")] = req.Data
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": 1}})
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	options := StorageOptions{Client: srv.Client(),
		Option: func(name string) (string, error) { return map[string]string{"vault_namespace": "admin"}[name], nil },
		Credential: func(name string) (string, error) {
			return map[string]string{"vault_token": "vtok"}[name], nil
		},
	}
	o, err := OpenStorage("http+vault://"+addr+"/secret/fleet/inventory", options)
	if err != nil || o.String() != "http+vault://"+addr+"/secret/fleet/inventory" {
		t.Fatalf("open: %v %v", o, err)
	}
	exerciseObjects(t, o)
	// the collection's format: the object as the field json
	if s := secrets["fleet/inventory/db-01.yaml"]; len(s) != 1 || s["json"] != "plugin: ofzhur.ansible.host\n" {
		t.Errorf("stored: %v", s)
	}
	// a secret without it is an object that is none of the collection's
	secrets["fleet/inventory/hand.yml"] = map[string]any{"user": "x"}
	if data, ok, err := o.Get(context.Background(), "hand.yml"); err != nil || !ok || string(data) != `{"user":"x"}` {
		t.Errorf("foreign secret: %q %v %v", data, ok, err)
	}
}

func TestOpenStorage(t *testing.T) {
	creds := StorageOptions{
		Credential: func(name string) (string, error) {
			return map[string]string{"access_key": "a", "secret_key": "b", "vault_token": "t"}[name], nil
		},
		SSH: func(target, port string, args []string) SSHRunner { return nil },
	}
	for _, c := range []struct{ url, want string }{
		{"file:///srv/fleet/inventory/", "file:///srv/fleet/inventory"},
		{"file://localhost/srv/inv", "file://localhost/srv/inv"},
		{"ssh://ops@Jump:22/srv/inv", "ssh://ops@jump:22/srv/inv"},
		{"s3://Fleet/inventory", "s3://fleet/inventory"},
		{"r2://0123456789abcdef0123456789abcdef/fleet", "r2://0123456789abcdef0123456789abcdef/fleet"},
		{"consul://consul.example:8500/fleet/inventory", "consul://consul.example:8500/fleet/inventory"},
		{"http+vault://[2001:db8::1]:8200/secret/fleet/inventory", "http+vault://[2001:db8::1]:8200/secret/fleet/inventory"},
	} {
		o, err := OpenStorage(c.url, creds)
		if err != nil || o.String() != c.want {
			t.Errorf("%s: %v %v", c.url, o, err)
		}
	}
	missing := StorageOptions{Missing: func(names ...string) string { return "set " + strings.Join(names, ", ") }}
	for _, c := range []struct{ url, want string }{
		{"s3://bucket/inv?access_key=a", "a query string in the URL is not read; credentials go into [ofzhur_storage] of ansible.cfg"},
		{"s3://bucket/inv#x", "a query string in the URL is not read; credentials go into [ofzhur_storage] of ansible.cfg"},
		{"s3://a:b@bucket/inv", "a password in the URL is not read"},
		{"ssh://ops:pw@jump/srv", "a password in the URL is not read"},
		{"consul://token@consul:8500/inv", "a user in the URL is not read (consul://)"},
		{"s3://bucket:9000/inv", "a port in the URL is not read (s3://)"},
		{"file:///", "file:///path"},
		{"ssh://jump", "ssh://[user@]host[:port]/path"},
		{"ssh://-oProxyCommand=x/srv", "ssh://[user@]host[:port]/path"},
		{"s3:///inv", "s3://bucket/prefix"},
		{"r2://account", "r2://account/bucket/prefix, where account is the Cloudflare account ID"},
		{"consul://consul:8500", "consul://host:8500/prefix"},
		{"vault://vault:8200/secret", "vault://host:8200/mount/prefix"},
		{"s3://bucket/inv", "the access key and the secret key are missing: set access_key, secret_key"},
		{"r2://account/bucket", "the access key and the secret key are missing: set access_key, secret_key"},
		{"vault://vault:8200/secret/inv", "the token is missing: set vault_token"},
		{"https://storage.example/inv", "unknown scheme https (file, ssh, s3, r2, consul, http+consul, vault, http+vault)"},
		{"inventory", "not the URL of a storage (file, ssh, s3, r2, consul, http+consul, vault, http+vault)"},
	} {
		if _, err := OpenStorage(c.url, missing); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", c.url, err)
		}
	}
	endpoint := creds
	endpoint.Option = func(name string) (string, error) {
		return map[string]string{"endpoint": "ftp://minio.example"}[name], nil
	}
	if _, err := OpenStorage("s3://bucket/inv", endpoint); err == nil || err.Error() != `endpoint "ftp://minio.example": not an http(s) address of a host` {
		t.Errorf("endpoint: %v", err)
	}
	ca := creds
	ca.Option = func(name string) (string, error) {
		return map[string]string{"ca_file": filepath.Join(t.TempDir(), "ca.pem")}[name], nil
	}
	if _, err := OpenStorage("consul://consul:8500/inv", ca); err == nil || !strings.HasPrefix(err.Error(), "ca_file: no such file or directory: ") {
		t.Errorf("ca_file: %v", err)
	}
	if got := StorageLabel("s3://user:pass@bucket:x/inv?token=1"); got != "s3://bucket:x/inv" {
		t.Errorf("label of a bad URL: %q", got)
	}
}

func TestSplitShell(t *testing.T) {
	for in, want := range map[string]string{
		``:                                 ``,
		`  -i key   -o A=b `:               `-i|key|-o|A=b`,
		`-o "ProxyCommand=ssh -W %h:%p j"`: `-o|ProxyCommand=ssh -W %h:%p j`,
		`'my key' a\ b "x\"y\\z\n" ''`:     `my key|a b|x"y\z\n|`,
		`a'b'"c"d`:                         `abcd`,
		"one\ttwo\nthree":                  `one|two|three`,
	} {
		got, err := SplitShell(in)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for in, want := range map[string]string{`-i "key`: "No closing quotation", `-i 'key`: "No closing quotation", `key\`: "No escaped character"} {
		if _, err := SplitShell(in); err == nil || err.Error() != want {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// TestMinIO runs the contract against MinIO in a container, where Docker
// is there; without it, or with -short, it is skipped.
func TestMinIO(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no Docker")
	}
	if _, err := docker("info"); err != nil {
		t.Skip("Docker does not answer")
	}
	image := "quay.io/minio/minio"
	if _, err := docker("image", "inspect", image); err != nil {
		if _, err := docker("pull", "-q", image); err != nil {
			t.Skip(image + " is not available")
		}
	}
	name := fmt.Sprintf("ofzhur-test-ump-minio-%d", os.Getpid())
	if out, err := docker("run", "-d", "--name", name, "-p", "127.0.0.1::9000", "-e", "MINIO_ROOT_USER=ofzhur-test", "-e", "MINIO_ROOT_PASSWORD=ofzhur-test-secret", image, "server", "/data"); err != nil {
		t.Fatalf("docker run: %s", out)
	}
	t.Cleanup(func() { docker("rm", "-f", name) })
	address, err := docker("port", name, "9000/tcp")
	if err != nil {
		t.Fatalf("docker port: %s", address)
	}
	address = strings.Fields(address)[0]
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := docker("exec", name, "sh", "-c", "mc alias set t http://127.0.0.1:9000 ofzhur-test ofzhur-test-secret >/dev/null && mc mb -p t/fleet >/dev/null"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MinIO does not answer")
		}
		time.Sleep(time.Second)
	}
	options := func(secret string) StorageOptions {
		return StorageOptions{
			Option: func(name string) (string, error) {
				return map[string]string{"endpoint": "http://" + address}[name], nil
			},
			Credential: func(name string) (string, error) {
				return map[string]string{"access_key": "ofzhur-test", "secret_key": secret}[name], nil
			},
		}
	}
	o, err := OpenStorage("s3://fleet/inventory", options("ofzhur-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	exerciseObjects(t, o)
	wrong, _ := OpenStorage("s3://fleet/inventory", options("Wr0ngS3cret"))
	if _, err := wrong.All(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "listing: SignatureDoesNotMatch: ") {
		t.Errorf("wrong secret: %v", err)
	}
}
