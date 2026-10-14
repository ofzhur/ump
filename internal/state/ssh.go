package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Runner runs a command on a host over SSH with stdin, and returns its
// stdout and exit status. The CLI supplies the ssh client.
type Runner func(ctx context.Context, host string, command string, stdin []byte) (stdout []byte, exit int, err error)

// SSH is a file on another machine, reached over SSH (ssh://user@host/path):
// a shared file on a jump host or a NAS without any service. The version
// tag is the content's hash, checked on the remote side before a write;
// the per-server locks are directories made atomically next to it.
type SSH struct {
	Host string
	Path string
	Run  Runner
}

func (s *SSH) String() string { return "ssh://" + s.Host + s.Path }

func (s *SSH) run(ctx context.Context, script string, stdin []byte) ([]byte, int, error) {
	if s.Run == nil {
		return nil, 0, errors.New("no SSH runner")
	}
	return s.Run(ctx, s.Host, "sh -c "+shellQuote(script), stdin)
}

func (s *SSH) Read(ctx context.Context) ([]byte, string, error) {
	out, code, err := s.run(ctx, fmt.Sprintf("if [ -f %s ]; then cat %s; else exit 3; fi", shellQuote(s.Path), shellQuote(s.Path)), nil)
	if err != nil {
		return nil, "", err
	}
	switch code {
	case 0:
		return out, hashVersion(out), nil
	case 3:
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("reading %s: exit status %d", s.Path, code)
}

func (s *SSH) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	// the check and the write are one remote command, so that no other
	// writer slips in between; the hash is the same one Read uses
	q := shellQuote(s.Path)
	script := fmt.Sprintf(`set -e; p=%s; mkdir -p "$(dirname "$p")"; cur=""; if [ -f "$p" ]; then cur="$(sha256sum "$p" | cut -c1-16)"; fi; [ "$cur" = %s ] || exit 4; cat > "$p.tmp" && chmod 600 "$p.tmp" && mv "$p.tmp" "$p"`, q, shellQuote(ifVersion))
	_, code, err := s.run(ctx, script, data)
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
		return hashVersion(data), nil
	case 4:
		return "", ErrConflict
	}
	return "", fmt.Errorf("writing %s: exit status %d", s.Path, code)
}

func (s *SSH) Lock(ctx context.Context, key string, info LockInfo) error {
	dir := s.Path + ".locks/" + lockKey(key)
	payload, _ := json.Marshal(info)
	script := fmt.Sprintf(`d=%s; mkdir -p "$(dirname "$d")"; if mkdir "$d" 2>/dev/null; then cat > "$d/info.json"; else cat "$d/info.json" 2>/dev/null; exit 5; fi`, shellQuote(dir))
	out, code, err := s.run(ctx, script, payload)
	if err != nil {
		return err
	}
	switch code {
	case 0:
		return nil
	case 5:
		held := LockInfo{Key: key}
		json.Unmarshal(out, &held)
		return &ErrLocked{Key: key, Info: held}
	}
	return fmt.Errorf("locking %s: exit status %d", key, code)
}

func (s *SSH) Unlock(ctx context.Context, key string) error {
	_, code, err := s.run(ctx, fmt.Sprintf("rm -rf %s", shellQuote(s.Path+".locks/"+lockKey(key))), nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("unlocking %s: exit status %d", key, code)
	}
	return nil
}

func (s *SSH) Locks(ctx context.Context) ([]LockInfo, error) {
	out, code, err := s.run(ctx, fmt.Sprintf(`d=%s; [ -d "$d" ] || exit 0; for f in "$d"/*/info.json; do [ -f "$f" ] && cat "$f" && echo; done; exit 0`, shellQuote(s.Path+".locks")), nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("listing locks: exit status %d", code)
	}
	var locks []LockInfo
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var info LockInfo
		if json.Unmarshal(line, &info) == nil {
			locks = append(locks, info)
		}
	}
	return locks, nil
}

// SSHVersion is what the remote side hashes: the first 16 hex characters
// of sha256, the same as hashVersion.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
