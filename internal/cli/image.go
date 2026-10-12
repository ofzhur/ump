package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

// imageWaitTimeout is how long the UMP rescue system has to answer after
// the kexec. A kexec skips the firmware, so a minute is usual; the limit
// is generous for slow network cards and large machines.
var imageWaitTimeout = 5 * time.Minute

// bootMarker is the line the entry script prints right before it jumps.
const bootMarker = "==> booting into the UMP rescue system"

// probeCommand asks a machine who it is: the uid we got, the UMP rescue
// version when it runs the image, and the name of the operating system.
const probeCommand = `id -u; cat /etc/ump-rescue-version 2>/dev/null || echo -; sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"'`

// hostProbe is a machine's answer to probeCommand.
type hostProbe struct {
	root    bool
	version string
	os      string
}

// batchOptions make a non-interactive ssh run fail instead of asking.
func batchOptions(connectTimeout time.Duration) []string {
	return []string{"BatchMode=yes", fmt.Sprintf("ConnectTimeout=%d", int(connectTimeout.Seconds()))}
}

// probeHost logs in as user and asks who answers. ok is false when SSH did
// not let us in: no route, no SSH, or no key the machine accepts.
func (a *app) probeHost(ctx context.Context, user, addr string, identities []string, identitiesOnly bool) (p hostProbe, ok bool) {
	argv := withSSHOptions(sshArgs(user, addr, identities, identitiesOnly, true, []string{probeCommand}), batchOptions(10*time.Second)...)
	var out, errb bytes.Buffer
	code, err := a.sshExec(ctx, argv, nil, &out, &errb)
	if err != nil || code != 0 {
		if a.verbose {
			fmt.Fprintf(a.stderr, "\nssh %s@%s: %s", user, addr, strings.TrimSpace(errb.String()))
		}
		return hostProbe{}, false
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) > 0 {
		p.root = lines[0] == "0"
	}
	if len(lines) > 1 && lines[1] != "-" {
		p.version = lines[1]
	}
	if len(lines) > 2 {
		p.os = lines[2]
	}
	return p, true
}

// entryCommand is what runs on the machine to load the image: it fetches
// the entry script from the base URL (with curl, or wget where there is
// no curl) and runs it with the base URL, the hostname and the keys to
// carry over. Nothing is bundled with ump: the storage behind the base
// URL is the one source, see images/rescue/README.md for what it serves.
func entryCommand(url, hostname string, keys []provider.PublicKey) string {
	args := []string{"-u", url}
	if hostname != "" {
		args = append(args, "-H", hostname)
	}
	for _, k := range keys {
		args = append(args, "-K", k.Data)
	}
	script := shellJoin([]string{url + "/kexec-into-rescue.sh"})
	return fmt.Sprintf(`f=/tmp/kexec-into-rescue.sh; if command -v curl >/dev/null; then curl -fsSL %s -o "$f"; else wget -q -O "$f" %s; fi && bash "$f" %s`, script, script, shellJoin(args))
}

// enterImage loads the image on the machine as user: the entry script
// downloads it from url, carries the network configuration, the hostname
// and the keys over and kexecs. It returns once the script reported the
// jump; the connection then drops with the old kernel. The script's
// output goes to stderr as it comes.
func (a *app) enterImage(ctx context.Context, user, addr string, identities []string, identitiesOnly bool, url, hostname string, keys []provider.PublicKey) error {
	argv := withSSHOptions(sshArgs(user, addr, identities, identitiesOnly, true, []string{entryCommand(url, hostname, keys)}), batchOptions(10*time.Second)...)
	var buf bytes.Buffer
	w := io.MultiWriter(a.stderr, &buf)
	code, err := a.sshExec(ctx, argv, nil, w, w)
	if err != nil {
		return err
	}
	out := buf.String()
	switch {
	case strings.Contains(out, bootMarker):
		return nil
	case code == 255:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended before the rescue image was loaded", addr))
	case !strings.Contains(out, "==> "):
		return fmt.Errorf("%s could not fetch the entry script from %s/kexec-into-rescue.sh (exit status %d); the server must reach the image's base URL", addr, url, code)
	case code != 0:
		return fmt.Errorf("loading the rescue image on %s failed (exit status %d)", addr, code)
	}
	return fmt.Errorf("the entry script on %s ended without booting the rescue image", addr)
}

// imageVersion asks the machine for the version of the UMP rescue system
// it runs, with UMP's key; anything else answering, or nothing, is false.
func (a *app) imageVersion(ctx context.Context, addr, keyPath string) (string, bool) {
	argv := withSSHOptions(sshArgs("root", addr, []string{keyPath}, true, true, []string{"cat /etc/ump-rescue-version"}), batchOptions(5*time.Second)...)
	var out bytes.Buffer
	code, err := a.sshExec(ctx, argv, nil, &out, io.Discard)
	v := strings.TrimSpace(out.String())
	if err != nil || code != 0 || v == "" {
		return "", false
	}
	return v, true
}

// waitForImage waits until the UMP rescue system answers at addr and
// returns its version. The old system may keep answering for a moment,
// and the provider's rescue system accepts UMP's key too, so the wait is
// for the version file, not for the port.
func (a *app) waitForImage(ctx context.Context, addr, keyPath string) (string, error) {
	fmt.Fprintf(a.stderr, "Waiting for the UMP rescue system on %s ...", addr)
	start := time.Now()
	deadline := start.Add(imageWaitTimeout)
	lastDot := start
	for {
		if v, ok := a.imageVersion(ctx, addr, keyPath); ok {
			fmt.Fprintf(a.stderr, " up after %s (ump-rescue %s).\n", time.Since(start).Round(time.Second), v)
			return v, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(a.stderr)
			return "", withExit(exitUnreachable, fmt.Errorf("the UMP rescue system on %s did not answer within %s; the machine may need a reset (ump reboot server ... --hard) and its console", addr, imageWaitTimeout))
		}
		if err := sleepCtx(ctx, waitPoll); err != nil {
			fmt.Fprintln(a.stderr)
			return "", err
		}
		if time.Since(lastDot) >= 10*time.Second {
			fmt.Fprint(a.stderr, ".")
			lastDot = time.Now()
		}
	}
}

// rescueHostname is the hostname the rescue system gets: the server's
// name when it is a valid one, otherwise nothing, which leaves the name
// of the system the jump started from.
func rescueHostname(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if validateServerName(n) != nil {
		return ""
	}
	return n
}

// nonEmpty returns s, or def when s is empty.
func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
