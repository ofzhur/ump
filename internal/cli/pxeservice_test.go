package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ofzhur/ump/packaging"
)

func TestPXEUnit(t *testing.T) {
	e := newTestEnv(t)
	if out, errOut, code := e.run("", false, "pxe", "unit"); code != 0 || out != packaging.Service {
		t.Errorf("unit: code %d, %q\n%s", code, errOut, out)
	}
	out, _, code := e.run("", false, "pxe", "unit", "--check", "--exec", "/usr/local/bin/ump")
	if want := strings.Replace(packaging.Check, "ExecStart=/usr/bin/ump ", "ExecStart=/usr/local/bin/ump ", 1); code != 0 || out != want {
		t.Errorf("unit --check --exec: code %d\n%s", code, out)
	}
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"--exec", "bin/ump"}, `--exec "bin/ump": an absolute path, without spaces, quotes, backslashes, % or $`},
		{[]string{"--exec", "/opt/my ump"}, `--exec "/opt/my ump"`},
		{[]string{"--exec", "/opt/ump/"}, `--exec "/opt/ump/"`},
		{[]string{"--exec", "/opt/%i/ump"}, `--exec "/opt/%i/ump"`},
		{[]string{"site"}, `unknown command "site"`},
	} {
		if out, errOut, code := e.run("", false, append([]string{"pxe", "unit"}, c.args...)...); code != 2 || out != "" || !strings.Contains(errOut, c.err) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
}

// credentialEnv clears the backend's secrets for the test, and gives
// them back after it.
func credentialEnv(t *testing.T) {
	for _, name := range pxeCredentialVars {
		t.Setenv(name, "")
	}
}

func TestPXECredentials(t *testing.T) {
	credentialEnv(t)
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("AWS_ACCESS_KEY_ID", "from-env")
	t.Setenv("UMP_STATE_USER", "from-env")
	os.WriteFile(filepath.Join(dir, "AWS_ACCESS_KEY_ID"), []byte("AKIAEXAMPLE\n"), 0o400)
	os.WriteFile(filepath.Join(dir, "AWS_SECRET_ACCESS_KEY"), []byte("s3cret"), 0o400)
	os.WriteFile(filepath.Join(dir, "UMP_STATE_USER"), nil, 0o400) // empty: the variable stays
	os.WriteFile(filepath.Join(dir, "AWS_REGION"), []byte("eu-central-1"), 0o400)
	if err := pxeCredentials(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE", "AWS_SECRET_ACCESS_KEY": "s3cret", "UMP_STATE_USER": "from-env", "UMP_STATE_TOKEN": "", "AWS_REGION": ""} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	os.Mkdir(filepath.Join(dir, "VAULT_TOKEN"), 0o700)
	if err := pxeCredentials(); err == nil || !strings.Contains(err.Error(), "the credential VAULT_TOKEN: ") {
		t.Errorf("a credential that cannot be read: %v", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if err := pxeCredentials(); err != nil {
		t.Errorf("without credentials: %v", err)
	}
}

// TestPXECredentialsBackend: the PXE server reaches the backend with a
// credential's secret, ahead of the env file's; other commands take no
// credentials.
func TestPXECredentialsBackend(t *testing.T) {
	e := newTestEnv(t)
	credentialEnv(t)
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()
	creds := t.TempDir()
	os.WriteFile(filepath.Join(creds, "UMP_STATE_TOKEN"), []byte("from-credential\n"), 0o400)
	t.Setenv("UMP_STATE", srv.URL+"/fleet")
	t.Setenv("UMP_STATE_TOKEN", "from-env")
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Compact(slices.Sorted(slices.Values(auth)))
		auth = nil
		return out
	}

	_, errOut, code := e.run("", false, "pxe", "serve", "--check", "--http", "127.0.0.1:0", "--data", t.TempDir(), "--cache", t.TempDir())
	if got := seen(); code != 1 || !strings.Contains(errOut, "no site.json for site default") || !slices.Equal(got, []string{"Bearer from-credential"}) {
		t.Errorf("serve: code %d, %q, the backend saw %q", code, errOut, got)
	}
	t.Setenv("UMP_STATE_TOKEN", "from-env")
	e.run("", false, "pxe", "status")
	if got := seen(); !slices.Equal(got, []string{"Bearer from-env"}) {
		t.Errorf("status: the backend saw %q", got)
	}
	os.Mkdir(filepath.Join(creds, "AWS_SECRET_ACCESS_KEY"), 0o700)
	if _, errOut, code := e.run("", false, "pxe", "serve", "--check"); code != 1 || !strings.Contains(errOut, "ump: the credential AWS_SECRET_ACCESS_KEY: ") || len(seen()) != 0 {
		t.Errorf("a credential that cannot be read: code %d, %q", code, errOut)
	}
}

func TestUpdateCommand(t *testing.T) {
	was := Packaging
	defer func() { Packaging = was }()
	storage := "https://ump.example.org/dl"
	rpm := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	for packaging, want := range map[string]string{
		"":    "curl -fsSL https://ump.example.org/dl/get.sh | sh",
		"deb": "curl -fsSLO https://ump.example.org/dl/pkg/latest/ump-" + runtime.GOARCH + ".deb && sudo apt install ./ump-" + runtime.GOARCH + ".deb",
		"rpm": "sudo dnf install https://ump.example.org/dl/pkg/latest/ump-" + rpm + ".rpm",
	} {
		Packaging = packaging
		if got := updateCommand(storage); got != want {
			t.Errorf("%q: %q, want %q", packaging, got, want)
		}
	}
}

// TestUpdateHintPackaged: a packaged ump takes the latest release from
// the packages' VERSION, not from the binaries', which upload.sh
// publishes first and a storage may hold alone.
func TestUpdateHintPackaged(t *testing.T) {
	e := newTestEnv(t)
	pkg := "v0.2.0\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bin/latest/VERSION":
			io.WriteString(w, "v0.3.0\n")
		case r.URL.Path == "/pkg/latest/VERSION" && pkg != "":
			io.WriteString(w, pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e.write(testConfig + "storage: " + srv.URL + "\n")
	was, wasPackaging := Version, Packaging
	defer func() { Version, Packaging = was, wasPackaging }()
	Version, Packaging = "v0.1.0", "deb"

	want := "ump v0.2.0 is available (this is v0.1.0): " + updateCommand(srv.URL) + "\n"
	if _, errOut, code := e.run("", false, "config", "isp", "list"); code != 0 || !strings.Contains(errOut, want) {
		t.Errorf("deb: code %d, %q", code, errOut)
	}
	// the latest package installed, newer binaries: nothing to install
	e.now = e.now.Add(25 * time.Hour)
	Version = "v0.2.0"
	if _, errOut, _ := e.run("", false, "config", "isp", "list"); strings.Contains(errOut, "available") {
		t.Errorf("binaries ahead: %q", errOut)
	}
	// a storage without packages: no hint
	e.now = e.now.Add(25 * time.Hour)
	Version, Packaging, pkg = "v0.1.0", "rpm", ""
	if _, errOut, _ := e.run("", false, "config", "isp", "list"); strings.Contains(errOut, "available") {
		t.Errorf("no packages: %q", errOut)
	}
}
