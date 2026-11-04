package pxe

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// The features of a rescue image a server needs: a boot config in front
// of it for every boot, the units of an unattended installation for
// those.
const (
	FeatureBootConfig = "boot-config"
	FeaturePXEInstall = "pxe-install"
)

// Manifest is a rescue image's manifest.json, <image>/<arch>/
// (images/rescue/build.sh): its version, what it can do beyond kexec,
// the memory it needs, and the SHA-256 and size of each file.
type Manifest struct {
	Version  string                  `json:"version"`
	Arch     string                  `json:"arch"`
	Kernel   string                  `json:"kernel"`
	Cmdline  string                  `json:"cmdline"`
	Features []string                `json:"features"`
	MemoryMB int                     `json:"memory_mb"`
	Files    map[string]ManifestFile `json:"files"`
}

// ManifestFile is a file of a manifest.
type ManifestFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Has says whether the image has the feature.
func (m *Manifest) Has(feature string) bool { return slices.Contains(m.Features, feature) }

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseManifest reads a manifest of the architecture: a version that
// can name a directory, and vmlinuz and initrd.zst with their sums.
func ParseManifest(data []byte, arch string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	switch {
	case !safeVersion.MatchString(m.Version) || strings.Contains(m.Version, ".."):
		return nil, fmt.Errorf("manifest.json: version %q", m.Version)
	case m.Arch != "" && m.Arch != arch:
		return nil, fmt.Errorf("manifest.json: the image is %s, not %s", m.Arch, arch)
	}
	for _, f := range []string{"vmlinuz", "initrd.zst"} {
		mf, ok := m.Files[f]
		if !ok || mf.Size <= 0 || len(mf.SHA256) != 64 {
			return nil, fmt.Errorf("manifest.json: no size and sha256 for %s", f)
		}
	}
	return &m, nil
}

// Cache keeps the rescue image a server boots, per architecture, under
// Dir/rescue/<id>/<arch>/, where id is the version, or the version and
// the start of the initrd's SHA-256 for files the storage published
// again under a version the cache holds other files of (a rebuild of
// the day): a build comes in whole, verified against its manifest (size
// and SHA-256) before anything of it is served, and stays until Prune
// removes it, so that a boot keeps the build it started with. An arm64
// kernel, a gzip-compressed Image, is kept unpacked next to it (Image),
// which iPXE boots as an EFI stub.
type Cache struct {
	Dir string
	// Client fetches the image; nil: a client without a time limit (the
	// context bounds it).
	Client *http.Client

	mu      sync.Mutex
	checked map[string]bool
}

// Rescue is a rescue image of one architecture in the cache: its ID
// there (Cache), its manifest, and the files a boot serves.
type Rescue struct {
	ID       string
	Manifest Manifest
	// Kernel is what a boot gets as vmlinuz, Initrd as the image's
	// initrd (after the boot config).
	Kernel string
	Initrd string
}

func (c *Cache) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{}
}

func (c *Cache) dir(id, arch string) string {
	return filepath.Join(c.Dir, "rescue", id, arch)
}

// rebuildID is the ID in the cache of a build published under a version
// the cache holds other files of.
func rebuildID(m *Manifest) string {
	return m.Version + "-" + m.Files["initrd.zst"].SHA256[:8]
}

// sameBuild says whether two manifests describe the same files and
// image.
func sameBuild(a, b *Manifest) bool {
	return a.Version == b.Version && a.Kernel == b.Kernel && a.Cmdline == b.Cmdline && a.MemoryMB == b.MemoryMB &&
		slices.Equal(a.Features, b.Features) && a.Files["vmlinuz"] == b.Files["vmlinuz"] && a.Files["initrd.zst"] == b.Files["initrd.zst"]
}

// Manifest fetches the manifest of the image (its base URL) for the
// architecture.
func (c *Cache) Manifest(ctx context.Context, image, arch string) (*Manifest, error) {
	url := strings.TrimSuffix(image, "/") + "/" + arch + "/manifest.json"
	data, err := c.get(ctx, url, 1<<20)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data, arch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return m, nil
}

// Rescue is the image's current build for the architecture: from the
// cache when it holds that build, else fetched and verified first.
func (c *Cache) Rescue(ctx context.Context, image, arch string) (*Rescue, error) {
	m, err := c.Manifest(ctx, image, arch)
	if err != nil {
		return nil, err
	}
	id := m.Version
	if r, err := c.load(id, arch); err == nil {
		if sameBuild(&r.Manifest, m) {
			return r, nil
		}
		id = rebuildID(m) // the version again, with other files
		if r, err := c.load(id, arch); err == nil && sameBuild(&r.Manifest, m) {
			return r, nil
		}
	}
	return c.fetch(ctx, image, arch, id, m)
}

// Cached is the newest version of the architecture the cache holds,
// verified, for a server that cannot reach the image; an error when it
// holds none.
func (c *Cache) Cached(arch string) (*Rescue, error) {
	versions, err := c.Versions(arch)
	if err != nil {
		return nil, err
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if r, err := c.load(versions[i], arch); err == nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no rescue image for %s in %s", arch, c.Dir)
}

// Versions are the IDs of the architecture's builds in the cache, oldest
// first (versions are dates, and a rebuild's ID follows its version's).
func (c *Cache) Versions(arch string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(c.Dir, "rescue"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(c.dir(e.Name(), arch), "manifest.json")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Prune removes the builds of the architecture but those to keep (their
// IDs).
func (c *Cache) Prune(arch string, keep ...string) error {
	versions, err := c.Versions(arch)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range versions {
		if slices.Contains(keep, v) {
			continue
		}
		delete(c.checked, c.dir(v, arch))
		if err := os.RemoveAll(c.dir(v, arch)); err != nil {
			return err
		}
		os.Remove(filepath.Join(c.Dir, "rescue", v)) // once no architecture is left
	}
	return nil
}

// load is a build in the cache, its files checked against its manifest
// once per process.
func (c *Cache) load(id, arch string) (*Rescue, error) {
	dir := c.dir(id, arch)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data, arch)
	if err != nil {
		return nil, err
	}
	if id != m.Version && id != rebuildID(m) {
		return nil, fmt.Errorf("%s holds version %s", dir, m.Version)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked[dir] {
		for _, f := range []string{"vmlinuz", "initrd.zst"} {
			if err := verifyFile(filepath.Join(dir, f), m.Files[f]); err != nil {
				return nil, err
			}
		}
		if err := unpackKernel(dir); err != nil {
			return nil, err
		}
		if c.checked == nil {
			c.checked = map[string]bool{}
		}
		c.checked[dir] = true
	}
	return c.rescue(id, dir, m), nil
}

func (c *Cache) rescue(id, dir string, m *Manifest) *Rescue {
	r := &Rescue{ID: id, Manifest: *m, Kernel: filepath.Join(dir, "vmlinuz"), Initrd: filepath.Join(dir, "initrd.zst")}
	if _, err := os.Stat(filepath.Join(dir, "Image")); err == nil {
		r.Kernel = filepath.Join(dir, "Image")
	}
	return r
}

// fetch downloads a build into a directory of its own, verifies it and
// only then puts it in place, as id.
func (c *Cache) fetch(ctx context.Context, image, arch, id string, m *Manifest) (*Rescue, error) {
	dir := c.dir(id, arch)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+arch+"-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	base := strings.TrimSuffix(image, "/") + "/" + arch + "/"
	for _, f := range []string{"vmlinuz", "initrd.zst"} {
		if err := c.download(ctx, base+f, filepath.Join(tmp, f), m.Files[f]); err != nil {
			return nil, err
		}
	}
	if err := unpackKernel(tmp); err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked[dir] {
		// fetched meanwhile, for another boot: what is there
		data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			return nil, err
		}
		have, err := ParseManifest(data, arch)
		if err != nil {
			return nil, err
		}
		return c.rescue(id, dir, have), nil
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	if c.checked == nil {
		c.checked = map[string]bool{}
	}
	c.checked[dir] = true
	return c.rescue(id, dir, m), nil
}

// get fetches a small file.
func (c *Cache) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// download fetches a file of the manifest into path, checking its size
// and SHA-256 on the way.
func (c *Cache) download(ctx context.Context, url, path string, want ManifestFile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, want.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", url, err)
	case n != want.Size:
		return fmt.Errorf("%s: %d bytes, the manifest says %d", url, n, want.Size)
	case hex.EncodeToString(h.Sum(nil)) != want.SHA256:
		return fmt.Errorf("%s: the SHA-256 is not the manifest's", url)
	}
	return nil
}

// verifyFile checks a file of the cache against its manifest.
func verifyFile(path string, want ManifestFile) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	switch {
	case err != nil:
		return err
	case n != want.Size || hex.EncodeToString(h.Sum(nil)) != want.SHA256:
		return fmt.Errorf("%s is not the file of its manifest", path)
	}
	return nil
}

// maxKernel bounds an unpacked kernel.
const maxKernel = 512 << 20

// unpackKernel puts the Image of a gzip-compressed kernel (arm64's
// vmlinuz) next to it, once.
func unpackKernel(dir string) error {
	src := filepath.Join(dir, "vmlinuz")
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		return nil // not gzip: a bzImage or an Image as it is
	}
	dst := filepath.Join(dir, "Image")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(zr, maxKernel+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxKernel {
		err = fmt.Errorf("%s unpacks to more than %d MB", src, maxKernel>>20)
	}
	if err != nil {
		os.Remove(dst + ".tmp")
		return fmt.Errorf("%s: %w", src, err)
	}
	return os.Rename(dst+".tmp", dst)
}
