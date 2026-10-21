package backend

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

// SSH is a directory on another machine, reached over SSH
// (ssh://user@host/path): a shared directory on a jump host or a NAS,
// with nothing to run there but a shell. Versions are content hashes
// checked on the remote side; the advisory locks are directories under
// .locks/, made atomically.
type SSH struct {
	Host string
	Dir  string
	Run  Runner
}

func (s *SSH) String() string { return "ssh://" + s.Host + s.Dir }

func (s *SSH) run(ctx context.Context, script string, stdin []byte) ([]byte, int, error) {
	if s.Run == nil {
		return nil, 0, errors.New("no SSH runner")
	}
	return s.Run(ctx, s.Host, "sh -c "+shellQuote(script), stdin)
}

func (s *SSH) file(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.Dir, "/") + "/" + p, nil
}

func (s *SSH) Read(ctx context.Context, p string) ([]byte, string, error) {
	f, err := s.file(p)
	if err != nil {
		return nil, "", err
	}
	out, code, err := s.run(ctx, fmt.Sprintf("if [ -f %s ]; then cat %s; else exit 3; fi", shellQuote(f), shellQuote(f)), nil)
	if err != nil {
		return nil, "", err
	}
	switch code {
	case 0:
		return out, HashVersion(out), nil
	case 3:
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("reading %s: exit status %d", f, code)
}

func (s *SSH) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	f, err := s.file(p)
	if err != nil {
		return "", err
	}
	q := shellQuote(f)
	script := fmt.Sprintf(`set -e; p=%s; mkdir -p "$(dirname "$p")"; cur=""; if [ -f "$p" ]; then cur="$(sha256sum "$p" | cut -c1-16)"; fi; [ "$cur" = %s ] || exit 4; cat > "$p.tmp" && chmod 600 "$p.tmp" && mv "$p.tmp" "$p"`, q, shellQuote(ifVersion))
	_, code, err := s.run(ctx, script, data)
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
		return HashVersion(data), nil
	case 4:
		return "", ErrConflict
	}
	return "", fmt.Errorf("writing %s: exit status %d", f, code)
}

func (s *SSH) Delete(ctx context.Context, p string) error {
	f, err := s.file(p)
	if err != nil {
		return err
	}
	_, code, err := s.run(ctx, fmt.Sprintf("rm -f %s", shellQuote(f)), nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("deleting %s: exit status %d", f, code)
	}
	return nil
}

func (s *SSH) List(ctx context.Context) ([]Entry, error) {
	d := shellQuote(strings.TrimRight(s.Dir, "/"))
	out, code, err := s.run(ctx, fmt.Sprintf(`d=%s; [ -d "$d" ] || exit 0; cd "$d" && find . -type f ! -path './.locks/*' ! -name '*.tmp' ! -name '*.lock' ! -name '.mirror.json' -exec sha256sum {} +`, d), nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("listing %s: exit status %d", s.Dir, code)
	}
	var entries []Entry
	for _, line := range strings.Split(string(out), "\n") {
		sum, path, ok := strings.Cut(strings.TrimSpace(line), "  ")
		path = strings.TrimPrefix(path, "./")
		if !ok || len(sum) < 16 || !ValidPath(path) {
			continue
		}
		entries = append(entries, Entry{Path: path, Version: sum[:16]})
	}
	return entries, nil
}

func (s *SSH) Lock(ctx context.Context, key string, info LockInfo) error {
	dir := strings.TrimRight(s.Dir, "/") + "/.locks/" + lockKey(key)
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
	_, code, err := s.run(ctx, fmt.Sprintf("rm -rf %s", shellQuote(strings.TrimRight(s.Dir, "/")+"/.locks/"+lockKey(key))), nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("unlocking %s: exit status %d", key, code)
	}
	return nil
}

func (s *SSH) Locks(ctx context.Context) ([]LockInfo, error) {
	out, code, err := s.run(ctx, fmt.Sprintf(`d=%s; [ -d "$d" ] || exit 0; for f in "$d"/*/info.json; do [ -f "$f" ] && cat "$f" && echo; done; exit 0`, shellQuote(strings.TrimRight(s.Dir, "/")+"/.locks")), nil)
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
