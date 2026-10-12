package sshkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
