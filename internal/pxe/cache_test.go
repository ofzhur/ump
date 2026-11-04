package pxe

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// storage serves rescue images as the storage does: <arch>/manifest.json
// and the files it names; files it was asked for are counted.
type storage struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

func (s *storage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/rescue/")
	s.hits[p]++
	data, ok := s.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

// publish puts a version of the image for an architecture there, with a
// manifest that says what its files are (lie: what the manifest says of
// the initrd instead).
func (s *storage) publish(arch, version string, kernel, initrd []byte, lie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	m := Manifest{Version: version, Arch: arch, Cmdline: "console=ttyS0,115200n8 console=tty0", Features: []string{FeatureBootConfig, FeaturePXEInstall}, MemoryMB: 412,
		Files: map[string]ManifestFile{"vmlinuz": {SHA256: sum(kernel), Size: int64(len(kernel))}, "initrd.zst": {SHA256: sum(initrd), Size: int64(len(initrd))}}}
	if lie != "" {
		m.Files["initrd.zst"] = ManifestFile{SHA256: sum([]byte(lie)), Size: int64(len(initrd))}
	}
	data, _ := json.Marshal(m)
	s.files[arch+"/manifest.json"] = data
	s.files[arch+"/vmlinuz"] = kernel
	s.files[arch+"/initrd.zst"] = initrd
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func TestCache(t *testing.T) {
	ctx := context.Background()
	st := &storage{files: map[string][]byte{}, hits: map[string]int{}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	image := srv.URL + "/rescue"
	c := &Cache{Dir: t.TempDir()}

	st.publish(ArchAMD64, "2026.11.01", []byte("bzImage"), []byte("initrd one"), "")
	r, err := c.Rescue(ctx, image, ArchAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if r.Manifest.Version != "2026.11.01" || !r.Manifest.Has(FeatureBootConfig) || r.Manifest.MemoryMB != 412 || filepath.Base(r.Kernel) != "vmlinuz" {
		t.Errorf("rescue: %+v", r)
	}
	if data, _ := os.ReadFile(r.Initrd); string(data) != "initrd one" {
		t.Errorf("initrd: %q", data)
	}
	// the version in the cache is served from there
	if _, err := c.Rescue(ctx, image, ArchAMD64); err != nil || st.hits["amd64/vmlinuz"] != 1 || st.hits["amd64/manifest.json"] != 2 {
		t.Errorf("again: %v, hits %v", err, st.hits)
	}
	// a new process checks the files once more
	fresh := &Cache{Dir: c.Dir}
	if r, err := fresh.Cached(ArchAMD64); err != nil || r.Manifest.Version != "2026.11.01" {
		t.Errorf("cached: %+v %v", r, err)
	}

	// a version whose initrd is not the manifest's never comes in
	st.publish(ArchAMD64, "2026.11.02", []byte("bzImage 2"), []byte("initrd two"), "something else")
	if _, err := c.Rescue(ctx, image, ArchAMD64); err == nil || !strings.Contains(err.Error(), "the SHA-256 is not the manifest's") {
		t.Errorf("a bad initrd: %v", err)
	}
	if v, _ := c.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.01" {
		t.Errorf("versions after a bad download: %v", v)
	}
	if r, err := c.Cached(ArchAMD64); err != nil || r.Manifest.Version != "2026.11.01" {
		t.Errorf("the old one stays: %+v %v", r, err)
	}
	st.publish(ArchAMD64, "2026.11.03", []byte("bzImage 3"), []byte("initrd three"), "")
	if r, err := c.Rescue(ctx, image, ArchAMD64); err != nil || r.Manifest.Version != "2026.11.03" {
		t.Errorf("the next one: %+v %v", r, err)
	}
	if v, _ := c.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.01,2026.11.03" {
		t.Errorf("versions: %v", v)
	}
	if err := c.Prune(ArchAMD64, "2026.11.03"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.03" {
		t.Errorf("versions after pruning: %v", v)
	}

	// the version published again with other files (a rebuild of the
	// day) comes in beside the first build, which boots keep until it is
	// pruned; the first build again is the one in the cache
	first, _ := c.Rescue(ctx, image, ArchAMD64)
	st.publish(ArchAMD64, "2026.11.03", []byte("bzImage 3b"), []byte("initrd three b"), "")
	st.mu.Lock()
	var m Manifest
	json.Unmarshal(st.files["amd64/manifest.json"], &m)
	m.Features = []string{FeatureBootConfig}
	st.files["amd64/manifest.json"], _ = json.Marshal(m)
	st.mu.Unlock()
	rebuilt, err := (&Cache{Dir: c.Dir}).Rescue(ctx, image, ArchAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(rebuilt.Initrd); string(data) != "initrd three b" || rebuilt.Manifest.Version != "2026.11.03" || rebuilt.Manifest.Has(FeaturePXEInstall) || rebuilt.ID != rebuildID(&m) || rebuilt.ID == first.ID {
		t.Errorf("the rebuild: %+v, initrd %q", rebuilt, data)
	}
	if data, _ := os.ReadFile(first.Initrd); string(data) != "initrd three" {
		t.Errorf("the first build's initrd: %q", data)
	}
	if v, _ := c.Versions(ArchAMD64); strings.Join(v, ",") != "2026.11.03,"+rebuilt.ID {
		t.Errorf("builds: %v", v)
	}
	if r, err := c.Cached(ArchAMD64); err != nil || r.ID != rebuilt.ID {
		t.Errorf("the newest cached: %+v %v", r, err)
	}
	if r, err := c.Rescue(ctx, image, ArchAMD64); err != nil || r.ID != rebuilt.ID {
		t.Errorf("the rebuild again: %+v %v", r, err)
	}
	st.publish(ArchAMD64, "2026.11.03", []byte("bzImage 3"), []byte("initrd three"), "")
	if r, err := c.Rescue(ctx, image, ArchAMD64); err != nil || r.ID != "2026.11.03" || !r.Manifest.Has(FeaturePXEInstall) {
		t.Errorf("the first build again: %+v %v", r, err)
	}
	if err := c.Prune(ArchAMD64, rebuilt.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Versions(ArchAMD64); strings.Join(v, ",") != rebuilt.ID {
		t.Errorf("builds after pruning: %v", v)
	}

	// arm64's kernel is gzip-compressed; the Image is served
	st.publish(ArchARM64, "2026.11.03", gzipped([]byte("arm64 Image")), []byte("initrd arm64"), "")
	r, err = c.Rescue(ctx, image, ArchARM64)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(r.Kernel); filepath.Base(r.Kernel) != "Image" || string(data) != "arm64 Image" {
		t.Errorf("arm64 kernel %s: %q", r.Kernel, data)
	}

	// a file changed in the cache is found by the next process
	os.WriteFile(r.Initrd, []byte("tampered"), 0o600)
	if _, err := (&Cache{Dir: c.Dir}).Cached(ArchARM64); err == nil {
		t.Error("a changed file was served")
	}
	if _, err := c.Rescue(ctx, srv.URL+"/nothing", ArchAMD64); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("no image there: %v", err)
	}
	if _, err := (&Cache{Dir: t.TempDir()}).Cached(ArchAMD64); err == nil {
		t.Error("an empty cache")
	}
}

func TestParseManifest(t *testing.T) {
	ok := `{"version": "2026.11.01", "arch": "amd64", "files": {"vmlinuz": {"sha256": "` + strings.Repeat("a", 64) + `", "size": 1}, "initrd.zst": {"sha256": "` + strings.Repeat("b", 64) + `", "size": 2}}}`
	if _, err := ParseManifest([]byte(ok), ArchAMD64); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ text, arch, err string }{
		{ok, ArchARM64, "the image is amd64, not arm64"},
		{strings.Replace(ok, "2026.11.01", "../x", 1), ArchAMD64, `version "../x"`},
		{strings.Replace(ok, "2026.11.01", "", 1), ArchAMD64, `version ""`},
		{strings.Replace(ok, `"size": 2`, `"size": 0`, 1), ArchAMD64, "no size and sha256 for initrd.zst"},
		{`{"version": "1", "files": {}}`, ArchAMD64, "no size and sha256 for vmlinuz"},
		{`[`, ArchAMD64, "manifest.json: unexpected end of JSON input"},
	} {
		if _, err := ParseManifest([]byte(c.text), c.arch); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %s", c.text, err, c.err)
		}
	}
}
