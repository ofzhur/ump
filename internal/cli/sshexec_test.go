package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRunSSHInteractiveReturns: an interactive session is a child
// process, so ump gets its exit status back and goes on (releases the
// lock, pushes the home) instead of being replaced by ssh.
func TestRunSSHInteractiveReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stands in for ssh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	code, err := runSSH([]string{"ssh", "root@203.0.113.10"}, true)
	if err != nil || code != 3 {
		t.Errorf("runSSH = %d, %v; want 3", code, err)
	}
}
