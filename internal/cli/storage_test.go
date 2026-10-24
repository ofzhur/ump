package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestStorageOptions: the options of the storage resolve as Ansible
// resolves those of the collection: the pointer's, then the environment
// (also empty), then [ofzhur_storage] of ansible.cfg; a file wins over a
// value; paths relative to ansible.cfg, or to the repository.
func TestStorageOptions(t *testing.T) {
	for _, k := range []string{"ACCESS_KEY", "ACCESS_KEY_FILE", "SECRET_KEY", "SECRET_KEY_FILE", "REGION", "ENDPOINT", "VAULT_TOKEN", "SSH_ARGS"} {
		t.Setenv(storageEnv+k, "")
		os.Unsetenv(storageEnv + k)
	}
	repo := t.TempDir()
	write := func(name, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755)
		os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600)
	}
	write("secrets/ak", "AKFILE\n")
	write("secrets/empty", "\n")
	write("other/ak", " AKOTHER \n")
	t.Setenv("SECRETS", filepath.Join(repo, "other"))
	write("ansible.cfg", `[defaults]
inventory = inventory

[DEFAULT]
vault_token = from-default

[ofzhur_storage]
# the keys
Access_Key = AKVALUE ; a comment
access_key_file = secrets/ak
secret_key = S;K
session_token_file = secrets/empty
session_token = ignored
consul_token: CT
ssh_args = -i key
  -o A=b
vault_namespace = "admin"
`)
	c, err := newStorageConfig(repo, &storagePointer{options: map[string]string{"region": "pointer-1", "ca_file": "ca.pem"}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string, credential bool) string {
		t.Helper()
		f := c.option
		if credential {
			f = c.credential
		}
		v, err := f(name)
		if err != nil {
			return "error: " + err.Error()
		}
		return v
	}
	for _, x := range []struct {
		name       string
		credential bool
		want       string
	}{
		{"access_key", true, "AKFILE"},
		{"access_key", false, "AKVALUE"},
		{"access_key_file", false, filepath.Join(repo, "secrets/ak")},
		{"secret_key", true, "S;K"},
		{"session_token", true, ""},
		{"consul_token", true, "CT"},
		{"vault_token", true, "from-default"},
		{"ssh_args", false, "-i key\n-o A=b"},
		{"region", false, "pointer-1"},
		{"ca_file", false, filepath.Join(repo, "ca.pem")},
		{"endpoint", false, ""},
		{"vault_namespace", false, "error: vault_namespace in [ofzhur_storage] of ansible.cfg is in quotes, which ansible-core 2.19 and later take off and earlier versions keep: leave them out"},
	} {
		if got := get(x.name, x.credential); got != x.want {
			t.Errorf("%s: %q, want %q", x.name, got, x.want)
		}
	}
	// the environment wins, also when empty; its paths are relative to
	// the repository, with $VAR and ~ expanded
	t.Setenv(storageEnv+"ACCESS_KEY_FILE", "$SECRETS/ak")
	t.Setenv(storageEnv+"SECRET_KEY", "")
	t.Setenv(storageEnv+"REGION", "env-1")
	t.Setenv(storageEnv+"SSH_ARGS", "")
	t.Setenv("HOME", filepath.Join(repo, "home"))
	t.Setenv(storageEnv+"VAULT_TOKEN_FILE", "~/vt")
	write("home/vt", "VT")
	for name, want := range map[string]string{"access_key": "AKOTHER", "secret_key": "", "vault_token": "VT"} {
		if got := get(name, true); got != want {
			t.Errorf("env %s: %q, want %q", name, got, want)
		}
	}
	if got := get("region", false); got != "pointer-1" {
		t.Errorf("the pointer's region: %q", got)
	}
	t.Setenv(storageEnv+"ACCESS_KEY_FILE", "secrets/nope")
	if got := get("access_key", true); got != "error: access_key_file: no such file or directory: "+filepath.Join(repo, "secrets/nope") {
		t.Errorf("missing file: %q", got)
	}
	if got := get("ssh_args", false); got != "" {
		t.Errorf("empty env: %q", got)
	}
	// without the section, the defaults
	write("ansible.cfg", "[DEFAULT]\naccess_key = x\n")
	if c, err = newStorageConfig(repo, &storagePointer{}); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv(storageEnv + "REGION")
	os.Unsetenv(storageEnv + "SSH_ARGS")
	if get("region", false) != "us-east-1" || get("ssh_args", false) != "" || get("consul_token", true) != "" {
		t.Errorf("defaults: %q %q", get("region", false), get("ssh_args", false))
	}
	for cfg, want := range map[string]string{
		"access_key = x\n":                          "ansible.cfg: line 1: no section header",
		"[ofzhur_storage]\nkey\n":                   `ansible.cfg: line 2: "key" is no option`,
		"[ofzhur_storage]\na = 1\nA = 2\n":          "ansible.cfg: line 3: option a again in section ofzhur_storage",
		"[ofzhur_storage]\n[x]\n[ofzhur_storage]\n": "ansible.cfg: line 3: section ofzhur_storage again",
	} {
		write("ansible.cfg", cfg)
		if _, err := newStorageConfig(repo, &storagePointer{}); err == nil || err.Error() != want {
			t.Errorf("%q: %v", cfg, err)
		}
	}
}

// TestHostFilesInAStorage: where a file of inventory/ points to host
// files in a storage, reinstall and provision write them there, with the
// content and the checks of inventory/hosts/, and nothing into it.
func TestHostFilesInAStorage(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	store := filepath.Join(t.TempDir(), "fleet", "inventory")
	url := "file://" + store
	pointer := filepath.Join(e.repo, "inventory", "hosts.yml")
	os.WriteFile(pointer, []byte("# the host files\nplugin: ofzhur.ansible.host\nurl: "+url+"\n"), 0o644)
	object := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(store, name))
		return string(data)
	}
	hosts := filepath.Join(e.repo, "inventory", "hosts")

	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--group", "web", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Wrote "+url+"/web-01.yml: web-01 in common, web.\nProvisioning: 00-base on the fleet (1 host) ...\n") {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	if got := object("web-01.yml"); !strings.HasPrefix(got, "# web-01: written by ump (ump server reinstall, ump provision).\n") || !strings.Contains(got, "plugin: ofzhur.ansible.host\ngroups: [common, web]\nvars:\n  ansible_host: 203.0.113.10\n  ump_admin: ump\n  ump_name: web-01\n  ump_ref: fake/1\n") {
		t.Errorf("host file:\n%s", got)
	}
	// of Ansible's variables, the address alone: who logs in, with which
	// key and how is for whoever runs Ansible, not for the host file
	ansibleVars := func(name string) []string {
		var f struct {
			Vars map[string]any `yaml:"vars"`
		}
		if err := yaml.Unmarshal([]byte(object(name)), &f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		var names []string
		for k := range f.Vars {
			if strings.HasPrefix(k, "ansible_") {
				names = append(names, k)
			}
		}
		return names
	}
	if got := ansibleVars("web-01.yml"); !slices.Equal(got, []string{"ansible_host"}) {
		t.Errorf("Ansible's variables: %v", got)
	}
	if fi, err := os.Stat(filepath.Join(store, "web-01.yml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v", err)
	}
	if _, err := os.Stat(hosts); err == nil {
		t.Errorf("inventory/hosts/ written")
	}
	// the same again writes nothing
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || strings.Contains(errOut, "Wrote") {
		t.Errorf("unchanged: code %d, %q", code, errOut)
	}
	// a new hostname: the old object goes
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--hostname", "web-02", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Removed "+url+"/web-01.yml: fake/1 has the hostname web-02 now.\nWrote "+url+"/web-02.yml: web-02 in common.\n") || object("web-01.yml") != "" {
		t.Errorf("new hostname: code %d, %q", code, errOut)
	}
	// --no-provision writes it too
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--hostname", "web-02", "--group", "db", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Wrote "+url+"/web-02.yml: web-02 in common, db.\n") || !strings.Contains(object("web-02.yml"), "groups: [common, db]\n") || !slices.Equal(ansibleVars("web-02.yml"), []string{"ansible_host"}) {
		t.Errorf("--no-provision: code %d, %q", code, errOut)
	}

	// an object of the hostname that is not the server's: refused before
	// anything happens
	for _, f := range []struct{ name, content, hostname, want string }{
		{"web-02.yaml", "web-02:\n", "web-02", url + "/web-02.yaml has no ump_ref: written by hand or by another tool; remove it, ump writes " + url + "/web-02.yml"},
		{"db-01.yml", "plugin: ofzhur.ansible.host\nvars:\n  ump_ref: fake/2\n", "db-01", url + "/db-01.yml is the host file of fake/2 (its ump_ref); give another --hostname, or remove the file if that server is gone"},
	} {
		os.WriteFile(filepath.Join(store, f.name), []byte(f.content), 0o600)
		for _, noProvision := range []bool{false, true} {
			args := []string{"server", "reinstall", "web-01", "--yes", "--hostname", f.hostname, "--account", "one"}
			if noProvision {
				args = append(args, "--no-provision")
			}
			e.remoteCalls, e.ansibleCalls = nil, nil
			resetFakePower()
			if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, "ump: "+f.want+"\n") || len(e.remoteCalls) != 0 || len(fakePower.actions) != 0 {
				t.Errorf("%s, %v: code %d, %q", f.name, args, code, errOut)
			}
		}
		os.Remove(filepath.Join(store, f.name))
	}
	os.WriteFile(filepath.Join(store, "web-02.yaml"), []byte("web-02:\n"), 0o600)
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "has no ump_ref: written by hand or by another tool; remove it, ump writes "+url+"/web-02.yml\n") || len(e.ansibleRuns()) != 0 {
		t.Errorf("provision over a hand-written object: code %d, %q", code, errOut)
	}
	os.Remove(filepath.Join(store, "web-02.yaml"))

	// a storage that cannot be read stops the run before anything changes
	if os.Geteuid() != 0 {
		os.Chmod(store, 0o300)
		e.ansibleCalls = nil
		if _, errOut, code := e.run("", false, "provision", "--yes"); code != 1 || !strings.HasSuffix(errOut, "ump: inventory/hosts.yml: "+url+": listing: permission denied\n") || len(e.ansibleRuns()) != 0 {
			t.Errorf("unreadable: code %d, %q", code, errOut)
		}
		os.Chmod(store, 0o700)
	}

	// beside host files in inventory/hosts/, or beside ump as the
	// inventory, or another pointer: refused
	ump := filepath.Join(e.repo, "inventory", "ump.yml")
	second := filepath.Join(e.repo, "inventory", "more.yaml")
	for _, c := range []struct {
		setup, undo func()
		want        string
	}{
		{func() {
			os.MkdirAll(hosts, 0o755)
			os.WriteFile(filepath.Join(hosts, "lb-01.yml"), []byte("plugin: ofzhur.ansible.host\n"), 0o644)
		}, func() { os.RemoveAll(hosts) },
			"inventory/hosts.yml points to host files in a storage, and inventory/hosts/ has host files too (lb-01.yml): ump writes them to the storage alone; move them there, or remove them"},
		{func() { os.WriteFile(ump, []byte("plugin: ofzhur.ansible.ump\n"), 0o644) }, func() { os.Remove(ump) },
			"inventory/hosts.yml points to host files in a storage, and inventory/ump.yml makes ump the inventory; keep one of the two"},
		{func() { os.WriteFile(second, []byte("plugin: ofzhur.ansible.host\nurl: file:///srv/x\n"), 0o644) }, func() { os.Remove(second) },
			"inventory/hosts.yml, inventory/more.yaml all point to host files in a storage; keep one"},
	} {
		c.setup()
		for _, args := range [][]string{{"provision", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--account", "one"}, {"server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"}} {
			e.remoteCalls, e.ansibleCalls = nil, nil
			resetFakePower()
			if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, "ump: "+c.want+"\n") || len(e.remoteCalls) != 0 || len(e.ansibleCalls) != 0 {
				t.Errorf("%v: code %d, %q", args, code, errOut)
			}
		}
		c.undo()
	}

	// the pointer: the plugin's keys only, a URL
	for content, want := range map[string]string{
		"plugin: ofzhur.ansible.host\nurl: " + url + "\naccess_key: AK\n": "inventory/hosts.yml: unknown key(s) access_key; a pointer to host files in a storage has plugin, url, endpoint, region, vault_namespace, ca_file, ssh_args; credentials go into [ofzhur_storage] of ansible.cfg",
		"plugin: ofzhur.ansible.host\nurl: inventory/hosts\n":             "inventory/hosts.yml: url must be the URL of a storage (s3://, r2://, consul://, vault://, ssh://, file://)",
		"plugin: ofzhur.ansible.host\nurl: " + url + "\nregion: 1\n":      "inventory/hosts.yml: region must be a string",
		"plugin: ofzhur.ansible.host\nurl: https://storage.example/inv\n": "inventory/hosts.yml: https://storage.example/inv: unknown scheme https (file, ssh, s3, r2, consul, http+consul, vault, http+vault)",
		"plugin: ofzhur.ansible.host\nurl: s3://u:secret@fleet/inv\n":     "inventory/hosts.yml: s3://fleet/inv: a password in the URL is not read",
	} {
		os.WriteFile(pointer, []byte(content), 0o644)
		e.ansibleCalls = nil
		if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, "ump: "+want+"\n") || len(e.ansibleCalls) != 0 {
			t.Errorf("%q: code %d, %q", content, code, errOut)
		}
	}

	// the credentials are the collection's, never those of ump's state;
	// without them, nothing runs, not even Ansible's checks
	os.WriteFile(pointer, []byte("plugin: ofzhur.ansible.host\nurl: r2://0123456789abcdef0123456789abcdef/fleet/inventory\n"), 0o644)
	t.Setenv("AWS_ACCESS_KEY_ID", "state-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "state-secret")
	t.Setenv("VAULT_TOKEN", "state-token")
	want := "ump: inventory/hosts.yml: r2://0123456789abcdef0123456789abcdef/fleet/inventory: the access key and the secret key are missing: access_key_file and secret_key_file (or access_key and secret_key) in [ofzhur_storage] of ansible.cfg, or ANSIBLE_OFZHUR_STORAGE_ACCESS_KEY_FILE and ANSIBLE_OFZHUR_STORAGE_SECRET_KEY_FILE; ump takes them from there, as Ansible does, never those of its state\n"
	for _, args := range [][]string{{"provision", "--yes"}, {"server", "reinstall", "web-01", "--yes", "--account", "one"}, {"server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"}} {
		e.remoteCalls, e.ansibleCalls = nil, nil
		resetFakePower()
		if _, errOut, code := e.run("", false, args...); code != 2 || !strings.HasSuffix(errOut, want) || len(e.remoteCalls) != 0 || len(e.ansibleCalls) != 0 {
			t.Errorf("%v without credentials: code %d, %q", args, code, errOut)
		}
	}
	os.WriteFile(pointer, []byte("plugin: ofzhur.ansible.host\nurl: http+vault://127.0.0.1:8200/secret/fleet/inventory\n"), 0o644)
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 2 || !strings.HasSuffix(errOut, ": the token is missing: vault_token_file (or vault_token) in [ofzhur_storage] of ansible.cfg, or ANSIBLE_OFZHUR_STORAGE_VAULT_TOKEN_FILE; ump takes them from there, as Ansible does, never those of its state\n") {
		t.Errorf("vault without a token: code %d, %q", code, errOut)
	}
}

// TestHostFilesInS3: the host files go to S3 with the credentials of
// [ofzhur_storage], from their files relative to ansible.cfg, or from
// ANSIBLE_OFZHUR_STORAGE_*.
func TestHostFilesInS3(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]string{}
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth := r.Header.Get("Authorization")
		key, _, _ := strings.Cut(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), "/")
		keys = append(keys, key+" "+r.Header.Get("X-Amz-Security-Token"))
		name := strings.TrimPrefix(r.URL.Path, "/fleet/")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			io.WriteString(w, "<ListBucketResult>")
			for k := range objects {
				io.WriteString(w, "<Contents><Key>"+k+"</Key></Contents>")
			}
			io.WriteString(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
		case r.Method == http.MethodGet:
			if v, ok := objects[name]; ok {
				io.WriteString(w, v)
				return
			}
			w.WriteHeader(404)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[name] = string(body)
			if r.Header.Get("Content-Type") != "application/yaml" {
				t.Errorf("content type %q", r.Header.Get("Content-Type"))
			}
		case r.Method == http.MethodDelete:
			delete(objects, name)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	os.WriteFile(filepath.Join(e.repo, "inventory", "hosts.yml"), []byte("plugin: ofzhur.ansible.host\nurl: s3://fleet/inventory\nendpoint: "+srv.URL+"\n"), 0o644)
	os.MkdirAll(filepath.Join(e.repo, "secrets"), 0o700)
	os.WriteFile(filepath.Join(e.repo, "secrets", "ak"), []byte("AKFILE\n"), 0o600)
	os.WriteFile(filepath.Join(e.repo, "secrets", "sk"), []byte("SKFILE\n"), 0o600)
	os.WriteFile(filepath.Join(e.repo, "ansible.cfg"), []byte("[defaults]\ninventory = inventory\n\n[ofzhur_storage]\nendpoint = http://192.0.2.1:9000\naccess_key = AKVALUE\naccess_key_file = secrets/ak\nsecret_key_file = secrets/sk\n"), 0o644)
	t.Setenv("AWS_ACCESS_KEY_ID", "state-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "state-secret")
	t.Setenv("AWS_SESSION_TOKEN", "state-token")

	resetFakePower()
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Wrote s3://fleet/inventory/web-01.yml: web-01 in common.\n") || !strings.Contains(objects["inventory/web-01.yml"], "  ump_ref: fake/1\n") {
		t.Fatalf("reinstall: code %d, %q\n%v", code, errOut, objects)
	}
	for _, k := range keys {
		if k != "AKFILE " {
			t.Errorf("signed with %q", k)
		}
	}
	// the environment's file wins over ansible.cfg's
	os.WriteFile(filepath.Join(e.repo, "other-ak"), []byte("AKENV"), 0o600)
	t.Setenv(storageEnv+"ACCESS_KEY_FILE", "other-ak")
	t.Setenv(storageEnv+"SESSION_TOKEN", "ST")
	keys = nil
	if _, errOut, code := e.run("", false, "provision", "--yes"); code != 0 || len(keys) == 0 {
		t.Errorf("provision: code %d, %q", code, errOut)
	}
	for _, k := range keys {
		if k != "AKENV ST" {
			t.Errorf("signed with %q", k)
		}
	}
}
