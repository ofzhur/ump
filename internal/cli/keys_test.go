package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRollFiles runs the swap of a key roll with a plain sh over three
// homes: the old key becomes the new one wherever it is, options and
// comments kept, and files without it are not touched.
func TestRollFiles(t *testing.T) {
	oldLine := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOLD+/old= ump@a"
	newLine := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINEW+/new= ump@b"
	root := t.TempDir()
	files := map[string]string{
		"root/.ssh/authorized_keys":   "ssh-rsa AAAAother me\n" + oldLine + "\n",
		"alice/.ssh/authorized_keys2": `from="10.0.0.0/8" ` + oldLine + "\n",
		"bob/.ssh/authorized_keys":    "ssh-rsa AAAAother me\n",
	}
	var homes []string
	for f, body := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		homes = append(homes, shellQuote(filepath.Join(root, strings.SplitN(f, "/", 2)[0])))
	}
	out, err := exec.Command("sh", "-c", rollFiles(oldLine, newLine, strings.Join(homes, " "))).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := map[string]string{
		"root/.ssh/authorized_keys":   "ssh-rsa AAAAother me\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINEW+/new= ump@a\n",
		"alice/.ssh/authorized_keys2": `from="10.0.0.0/8" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINEW+/new= ump@a` + "\n",
		"bob/.ssh/authorized_keys":    "ssh-rsa AAAAother me\n",
	}
	for f, w := range want {
		got, _ := os.ReadFile(filepath.Join(root, f))
		if string(got) != w {
			t.Errorf("%s:\n%s\nwant\n%s", f, got, w)
		}
	}
	for _, f := range []string{"root/.ssh/authorized_keys", "alice/.ssh/authorized_keys2"} {
		if !strings.Contains(string(out), "rolled "+filepath.Join(root, f)) {
			t.Errorf("%s not named in %q", f, out)
		}
	}
	if strings.Contains(string(out), "bob") {
		t.Errorf("an untouched file named: %q", out)
	}
	if !strings.HasPrefix(rollScript(oldLine, newLine), "# ump key roll\n") {
		t.Error("rollScript lost its marker")
	}
}
