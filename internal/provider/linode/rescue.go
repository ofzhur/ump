package linode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ofzhur/ump/internal/provider"
)

// Linode's rescue mode boots Finnix from RAM with the instance's disks
// attached, for one boot; the next reboot starts the instance's own
// configuration again. Finnix takes no SSH key, but its console has a
// root shell, and Lish, Linode's console gateway, lets in over SSH with
// the keys of the profile. So EnableRescue puts UMP's key into the
// profile's Lish keys, boots the instance into rescue mode, and types
// root's authorized_keys and the start of sshd into the console; from
// then on the instance is a rescue system like any other, and nothing
// on its disks was touched.

var (
	// rescueBoot is how long the instance may take to run again after the
	// rescue call; rescueConsole how long Finnix may take to give a shell
	// on the console.
	rescueBoot    = 5 * time.Minute
	rescueConsole = 5 * time.Minute
	rescuePoll    = 3 * time.Second
)

// readyMark is what the console prints once the setup ran; the command
// computes it, so that the echo of the typed command never matches.
const readyMark = "UMP-RESCUE-READY-42"

// console opens the instance's console through Lish; tests replace it.
type consoleDialer func(ctx context.Context, region, user, label string, signer ssh.Signer) (io.ReadWriteCloser, error)

// RescueEnabled implements provider.Rescue. Rescue mode lasts one boot
// and is never left armed, so there is nothing to disable afterwards.
func (p *Provider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	return false, nil
}

// DisableRescue implements provider.Rescue: nothing to disarm, the next
// reboot boots the instance's configuration.
func (p *Provider) DisableRescue(ctx context.Context, m provider.Machine) error {
	return nil
}

// EnableRescue implements provider.Rescue: Finnix with the instance's
// disks, root reachable over SSH with opts.Keys. It reboots the instance
// and returns once sshd runs there.
func (p *Provider) EnableRescue(ctx context.Context, m provider.Machine, opts provider.RescueOptions) (*provider.RescueInfo, error) {
	id, err := instanceID(m)
	if err != nil {
		return nil, err
	}
	if opts.NoReboot {
		return nil, &provider.APIError{Provider: TypeName, Message: "Linode's rescue mode starts with a reboot and cannot be armed for a later one"}
	}
	if opts.Identity == "" {
		return nil, &provider.APIError{Provider: TypeName, Message: "the rescue system is set up through the Lish console, which needs UMP's private key"}
	}
	signer, err := loadSigner(opts.Identity)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, k := range opts.Keys {
		line := strings.TrimSpace(k.Data)
		if strings.ContainsAny(line, "'\n\r") {
			return nil, &provider.APIError{Provider: TypeName, Message: fmt.Sprintf("the key %s cannot be typed into the console", k.Name)}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, &provider.APIError{Provider: TypeName, Message: "the rescue system needs a key for root"}
	}

	var in Instance
	if err := p.client.do(ctx, http.MethodGet, path(id), nil, &in); err != nil {
		return nil, err
	}
	user, err := p.ensureLishKey(ctx, signer.PublicKey())
	if err != nil {
		return nil, err
	}
	devices, err := p.rescueDevices(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := p.client.do(ctx, http.MethodPost, path(id)+"/rescue", map[string]any{"devices": devices}, nil); err != nil {
		return nil, err
	}
	if err := p.waitRunning(ctx, id); err != nil {
		return nil, err
	}
	dial := p.console
	if dial == nil {
		dial = dialLish
	}
	if err := setupConsole(ctx, dial, in.Region, user, in.Label, signer, lines); err != nil {
		return nil, err
	}
	return &provider.RescueInfo{
		Address: m.IPv4,
		User:    "root",
		Reboots: true,
		Ready:   true,
		Note:    "Finnix, Linode's rescue mode, made reachable over SSH through the Lish console; UMP's key stays in your Linode profile's Lish keys",
	}, nil
}

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("UMP's private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("UMP's private key %s: %w", path, err)
	}
	return signer, nil
}

// Profile is the part of GET /profile that Lish needs.
type Profile struct {
	Username       string   `json:"username"`
	LishAuthMethod string   `json:"lish_auth_method"`
	AuthorizedKeys []string `json:"authorized_keys"`
}

// ensureLishKey adds key to the profile's Lish keys unless it is there,
// keeping every other key, and returns the profile's user name, which is
// the Lish login.
func (p *Provider) ensureLishKey(ctx context.Context, key ssh.PublicKey) (string, error) {
	var prof Profile
	if err := p.client.do(ctx, http.MethodGet, "/profile", nil, &prof); err != nil {
		return "", err
	}
	if prof.LishAuthMethod == "disabled" {
		return "", &provider.APIError{Provider: TypeName, Message: "Lish is disabled in your Linode profile (Cloud Manager: profile → LISH Console Settings); ump sets up the rescue system through it"}
	}
	blob := keyData(string(ssh.MarshalAuthorizedKey(key)))
	for _, k := range prof.AuthorizedKeys {
		if keyData(k) == blob {
			return prof.Username, nil
		}
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " ump"
	keys := append(append([]string{}, prof.AuthorizedKeys...), line)
	if err := p.client.do(ctx, http.MethodPut, "/profile", map[string]any{"authorized_keys": keys}, nil); err != nil {
		return "", fmt.Errorf("adding UMP's key to the profile's Lish keys: %w", err)
	}
	return prof.Username, nil
}

// Config is an instance's boot configuration.
type Config struct {
	ID      int64                    `json:"id"`
	Devices map[string]*ConfigDevice `json:"devices"`
}

// ConfigDevice is a disk or volume of a configuration.
type ConfigDevice struct {
	DiskID   *int64 `json:"disk_id,omitempty"`
	VolumeID *int64 `json:"volume_id,omitempty"`
}

// rescueDevices attaches what the instance boots with: the devices of
// its first configuration, else its disks in order.
func (p *Provider) rescueDevices(ctx context.Context, id int64) (map[string]ConfigDevice, error) {
	configs, err := list[Config](ctx, p.client, path(id)+"/configs")
	if err != nil {
		return nil, err
	}
	out := map[string]ConfigDevice{}
	if len(configs) > 0 {
		for name, d := range configs[0].Devices {
			if d != nil && (d.DiskID != nil || d.VolumeID != nil) {
				out[name] = *d
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	disks, err := list[struct {
		ID int64 `json:"id"`
	}](ctx, p.client, path(id)+"/disks")
	if err != nil {
		return nil, err
	}
	for i, d := range disks {
		if i >= 7 {
			break
		}
		id := d.ID
		out["sd"+string(rune('a'+i))] = ConfigDevice{DiskID: &id}
	}
	return out, nil
}

// waitRunning waits until the instance runs again after the rescue call
// (it reports rebooting, then running).
func (p *Provider) waitRunning(ctx context.Context, id int64) error {
	start := time.Now()
	deadline := start.Add(rescueBoot)
	rebooting := false
	for {
		var in Instance
		if err := p.client.do(ctx, http.MethodGet, path(id), nil, &in); err != nil {
			return err
		}
		// running again after rebooting; or running all along once the
		// reboot had time to show (a fast one can fall between two polls;
		// Linode may also queue the job for a while)
		if in.Status != "running" {
			rebooting = true
		} else if rebooting || time.Since(start) > time.Minute {
			return nil
		}
		if time.Now().After(deadline) {
			return &provider.APIError{Provider: TypeName, Message: fmt.Sprintf("the instance did not boot into rescue mode within %s (status %s)", rescueBoot, in.Status)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rescuePoll):
		}
	}
}

// dialLish opens the console of the instance with the given label
// through the Lish gateway of its region, as the profile's user with
// UMP's key. The gateway's host key is not checked: only public keys and
// a shell command go through it, and the rescue system reached
// afterwards is as new as its host key.
func dialLish(ctx context.Context, region, user, label string, signer ssh.Signer) (io.ReadWriteCloser, error) {
	addr := net.JoinHostPort("lish-"+region+".linode.com", "22")
	d := net.Dialer{Timeout: 20 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("Lish gateway %s: %w", addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see above
		Timeout:         20 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("Lish gateway %s: %w", addr, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("Lish: %w", err)
	}
	if err := sess.RequestPty("vt100", 24, 200, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		client.Close()
		return nil, fmt.Errorf("Lish: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		client.Close()
		return nil, err
	}
	if err := sess.Start(label); err != nil {
		client.Close()
		return nil, fmt.Errorf("Lish console of %s: %w", label, err)
	}
	return &lishConsole{Reader: stdout, Writer: stdin, close: func() error { sess.Close(); return client.Close() }}, nil
}

type lishConsole struct {
	io.Reader
	io.Writer
	close func() error
}

func (c *lishConsole) Close() error { return c.close() }

// setupConsole types root's authorized_keys and the start of sshd into
// Finnix's console once its shell shows a prompt, and again at a fresh
// prompt until it says it is done (the command does no harm twice).
// Finnix's login may ask the terminal for its size (a cursor position
// report); the console answers as a 24x200 terminal would, since an
// unanswered question makes the login read, and swallow, what is typed
// next.
func setupConsole(ctx context.Context, dial consoleDialer, region, user, label string, signer ssh.Signer, keys []string) error {
	var cmd strings.Builder
	cmd.WriteString("mkdir -p /root/.ssh && printf '%s\\n'")
	for _, k := range keys {
		cmd.WriteString(" '" + k + "'")
	}
	cmd.WriteString(" > /root/.ssh/authorized_keys && chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys && systemctl start ssh && echo UMP-RESCUE-READY-$((6*7))\r")

	deadline := time.Now().Add(rescueConsole)
	var last string
	for {
		// Lish drops the console when the instance reboots under it; a new
		// one picks up where the boot is
		done, out, err := consoleSession(ctx, dial, region, user, label, signer, cmd.String(), deadline)
		last += out
		switch {
		case done:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case time.Now().After(deadline):
			if err != nil {
				return fmt.Errorf("Lish console of %s: %w", label, err)
			}
			return consoleTimeout(label, last)
		}
		if err := sleepCtx(ctx, 3*rescuePoll); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// consoleSession is one connection to the console: it reports whether
// the setup ran, what the console printed, and why it ended otherwise
// (the console closed, or the deadline passed).
func consoleSession(ctx context.Context, dial consoleDialer, region, user, label string, signer ssh.Signer, cmd string, deadline time.Time) (bool, string, error) {
	con, err := dial(ctx, region, user, label, signer)
	if err != nil {
		return false, "", err
	}
	defer con.Close()

	var mu sync.Mutex
	var out bytes.Buffer
	var readErr error
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := con.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			if err != nil {
				readErr = err
			}
			mu.Unlock()
			if n > 0 && bytes.Contains(buf[:n], []byte("\x1b[6n")) {
				_, _ = io.WriteString(con, "\x1b[24;200R")
			}
			if err != nil {
				return
			}
		}
	}()
	seen := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.String(), readErr
	}

	// Lish replays what the console printed so far, and Finnix logs root
	// in by itself: the prompt shows up without a key pressed. Typing
	// before it, or Ctrl-C into the login's terminal probing, leaves the
	// console in a state that takes no more commands.
	atPrompt := func(from int) (bool, error) {
		s, rerr := seen()
		if len(s) < from {
			return false, rerr
		}
		return strings.HasSuffix(strings.TrimRight(s[from:], " "), "#"), rerr
	}
	from := 0
	for time.Now().Before(deadline) {
		ok, rerr := atPrompt(from)
		if rerr != nil {
			s, _ := seen()
			return false, s, rerr
		}
		if !ok {
			if err := sleepCtx(ctx, rescuePoll/3); err != nil {
				return false, "", err
			}
			continue
		}
		if _, err := io.WriteString(con, cmd); err != nil {
			s, _ := seen()
			return false, s, err
		}
		for wait := time.Now().Add(10 * rescuePoll); time.Now().Before(wait); {
			s, rerr := seen()
			if strings.Contains(s, readyMark) {
				return true, s, nil
			}
			if rerr != nil {
				return false, s, rerr
			}
			if err := sleepCtx(ctx, rescuePoll/3); err != nil {
				return false, s, err
			}
		}
		// no answer: a fresh line, and the command again at its prompt
		s, _ := seen()
		from = len(s)
		if _, err := io.WriteString(con, "\r"); err != nil {
			return false, s, err
		}
	}
	s, _ := seen()
	return false, s, nil
}

func consoleTimeout(label, out string) error {
	if len(out) > 300 {
		out = out[len(out)-300:]
	}
	return &provider.APIError{Provider: TypeName, Message: fmt.Sprintf("Finnix on %s gave no shell on the console within %s (last output: %q)", label, rescueConsole, out)}
}
