package sshkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A throwaway key generated with ssh-keygen -t ed25519; its fingerprints
// were taken from ssh-keygen -l -E md5 and ssh-keygen -l.
const (
	testKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 test@example"
	testMD5    = "d7:0b:dd:9a:1b:64:cf:90:82:28:e2:50:f2:dc:bf:81"
	testSHA256 = "SHA256:Lb8lR035J2JwU9QfgPWJW7mQw4fu3EOJ24fakgRVZ0s"
)

func TestParseAndFingerprints(t *testing.T) {
	k, err := Parse(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Type != "ssh-ed25519" || k.Comment != "test@example" || len(k.Blob) != 51 {
		t.Errorf("parsed: %+v", k)
	}
	if k.Line() != testKey {
		t.Errorf("Line() = %q", k.Line())
	}
	if got := k.FingerprintMD5(); got != testMD5 {
		t.Errorf("MD5 = %s, want %s", got, testMD5)
	}
	if got := k.FingerprintSHA256(); got != testSHA256 {
		t.Errorf("SHA256 = %s, want %s", got, testSHA256)
	}
	for _, bad := range []string{"", "ssh-ed25519", "# comment", "ssh-ed25519 not-base64!!", "ssh-ed25519 AAA="} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "id_ed25519.pub")
	os.WriteFile(p, []byte("# my key\n\n"+testKey+"\n"), 0o600)
	k, err := ReadFile(p)
	if err != nil || k.FingerprintMD5() != testMD5 {
		t.Errorf("ReadFile: %+v %v", k, err)
	}
	os.WriteFile(p, []byte("garbage\n"), 0o600)
	if _, err := ReadFile(p); err == nil || !strings.Contains(err.Error(), "not an SSH public key") {
		t.Errorf("garbage: %v", err)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.pub")); err == nil {
		t.Error("missing file accepted")
	}
	if paths := DefaultPaths(); len(paths) != 3 || !strings.HasSuffix(paths[0], "id_ed25519.pub") {
		t.Errorf("DefaultPaths: %v", paths)
	}
}

func TestGenerateAndEnsure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "staging_ed25519")
	k, created, err := Ensure(path, "ump@test")
	if err != nil || !created {
		t.Fatalf("Ensure: %v, created %v", err, created)
	}
	if k.Type != "ssh-ed25519" || k.Comment != "ump@test" || len(k.Blob) != 51 {
		t.Errorf("generated key: %+v", k)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode: %v %v", fi, err)
	}
	if dfi, _ := os.Stat(filepath.Dir(path)); dfi.Mode().Perm() != 0o700 {
		t.Errorf("key directory mode %v", dfi.Mode().Perm())
	}
	// the private key is valid OpenSSH and matches the public key
	data, _ := os.ReadFile(path)
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	if string(signer.PublicKey().Marshal()) != string(k.Blob) {
		t.Error("private key does not match the public key file")
	}
	// a second Ensure reuses it, never regenerates
	k2, created, err := Ensure(path, "other")
	if err != nil || created || k2.FingerprintSHA256() != k.FingerprintSHA256() {
		t.Errorf("second Ensure: %+v %v %v", k2, created, err)
	}
	if _, err := Generate(path, "x"); err == nil {
		t.Error("Generate overwrote an existing key")
	}
	if c := DefaultComment(); !strings.HasPrefix(c, "ump") {
		t.Errorf("DefaultComment = %q", c)
	}
}

func TestDefaultIdentityFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := DefaultIdentityFiles(); len(got) != 0 {
		t.Errorf("no keys: %v", got)
	}
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_rsa"), []byte("k"), 0o600)
	os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("k"), 0o600)
	got := DefaultIdentityFiles()
	if len(got) != 2 || !strings.HasSuffix(got[0], "id_ed25519") || !strings.HasSuffix(got[1], "id_rsa") {
		t.Errorf("DefaultIdentityFiles = %v", got)
	}
}
