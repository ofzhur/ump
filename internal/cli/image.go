package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/sshkey"
)

// imageWaitTimeout is how long the UMP rescue system has to answer after
// the kexec. A kexec skips the firmware, so a minute is usual; the limit
// is generous for slow network cards and large machines.
var imageWaitTimeout = 5 * time.Minute

// bootMarker is the line the entry script prints right before it jumps.
const bootMarker = "==> booting into the UMP rescue system"

// probeCommand asks a machine who it is: the uid we got, the UMP rescue
// version when it runs the image, and the name of the operating system.
const probeCommand = `id -u; cat /etc/ump-rescue-version 2>/dev/null || echo -; { sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"'; echo; } | head -n 1; sudo -n true 2>/dev/null && echo sudo; true`

// hostProbe is a machine's answer to probeCommand.
type hostProbe struct {
	root    bool
	sudo    bool
	version string
	os      string
}

// admin reports whether the login can do what root can: it is root, or
// sudo asks it no password.
func (p hostProbe) admin() bool { return p.root || p.sudo }

// defaultAdmin is the administrator of the default profile, and the
// user ump tries after root on a system it does not know.
const defaultAdmin = "ump"

// loginUsers are the users ump tries, in order, to reach the running
// system of m: root, the administrator of the profile set for the server
// (a sudo user, where root cannot log in), and the default one.
func (a *app) loginUsers(cfg *config.Config, m provider.Machine) []string {
	users := []string{"root"}
	if u := a.profileAdmin(cfg, cfg.Servers[m.Ref()].Profile); u != "" && u != "root" {
		users = append(users, u)
	}
	if users[len(users)-1] != defaultAdmin {
		users = append(users, defaultAdmin)
	}
	return users
}

// profileAdmin is the administrator of the named profile when it is in
// the profiles directory (no storage is asked), else of the default one.
func (a *app) profileAdmin(cfg *config.Config, name string) string {
	if name == "" {
		name = "default"
	}
	if strings.ContainsAny(name, "/\\") {
		name = expandHome(name)
	} else {
		name = filepath.Join(config.ProfilesDir(), name+".yaml")
	}
	p, err := profile.Load(name)
	if err != nil {
		return defaultAdmin
	}
	return p.Admin()
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
	if len(lines) > 3 {
		p.sudo = lines[3] == "sudo"
	}
	return p, true
}

// probeAdmin tries the users in turn and returns the first that gets in,
// with its answer; ok is false when none did. notAdmin is the last user
// that got in without being root or sudo, for the message.
func (a *app) probeAdmin(ctx context.Context, users []string, addr string, identities []string, identitiesOnly bool) (user string, p hostProbe, ok bool, notAdmin string) {
	for _, u := range users {
		p, ok := a.probeHost(ctx, u, addr, identities, identitiesOnly)
		if !ok {
			continue
		}
		if p.admin() || p.version != "" {
			return u, p, true, notAdmin
		}
		notAdmin = u
	}
	return "", hostProbe{}, false, notAdmin
}

// entryCommand is what runs on the machine to load the image: the entry
// script, fetched from the base URL, with the base URL, the hostname and
// the keys to carry over.
func entryCommand(url, hostname string, keys []provider.PublicKey, sudo bool) string {
	args := []string{"-u", url}
	if hostname != "" {
		args = append(args, "-H", hostname)
	}
	for _, k := range keys {
		args = append(args, "-K", k.Data)
	}
	cmd := fetchAndRun(url+"/kexec-into-rescue.sh", "/tmp/kexec-into-rescue.sh", args)
	if sudo {
		return "sudo -n sh -c " + shellJoin([]string{cmd})
	}
	return cmd
}

// enterImage loads the image on the machine as user (through sudo when
// it is not root): the entry script downloads it from url, carries the
// network configuration, the hostname and the keys over and kexecs. It
// returns once the script reported the jump; the connection then drops
// with the old kernel. The script's output goes to stderr as it comes.
func (a *app) enterImage(ctx context.Context, user, addr string, identities []string, identitiesOnly bool, url, hostname string, keys []provider.PublicKey) error {
	argv := withSSHOptions(sshArgs(user, addr, identities, identitiesOnly, true, []string{entryCommand(url, hostname, keys, user != "root")}), batchOptions(10*time.Second)...)
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

// stockThenImage is the way in for a reinstall of a machine whose rescue
// system takes no key: the provider puts its stock Linux on the machine
// with UMP's key for root (erasing it, which the reinstall does anyway),
// then the direct way kexecs from there into the UMP rescue system.
func (a *app) stockThenImage(ctx context.Context, m provider.Machine, si provider.StockInstaller, keys []provider.PublicKey, keyPath, imageURL, hostname string) (*imageEntry, error) {
	fmt.Fprintf(a.stderr, "The provider's rescue system of %s takes no SSH key; the provider reinstalls it with a stock Linux and UMP's key first (this takes a few minutes) ...\n", m.Name)
	stock, err := si.InstallStock(ctx, m, keys)
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", m.Account, err)
	}
	addr := stock.Address
	if addr == "" {
		addr = m.IPv4
	}
	fmt.Fprintf(a.stderr, "%s runs the provider's %s now; waiting for root access over SSH ...", m.Name, stock.Image)
	start := time.Now()
	deadline := start.Add(imageWaitTimeout)
	identities := []string{keyPath}
	for {
		if p, ok := a.probeHost(ctx, "root", addr, identities, true); ok && p.root {
			fmt.Fprintf(a.stderr, " yes after %s (%s).\n", time.Since(start).Round(time.Second), nonEmpty(p.os, "unknown system"))
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(a.stderr)
			return nil, withExit(exitUnreachable, fmt.Errorf("%s did not let UMP in as root within %s after the provider's reinstall; look at the server's console", m.Name, imageWaitTimeout))
		}
		if err := sleepCtx(ctx, waitPoll); err != nil {
			fmt.Fprintln(a.stderr)
			return nil, err
		}
	}
	fmt.Fprintf(a.stderr, "Loading the UMP rescue image from %s on %s.\n", imageURL, m.Name)
	if err := a.enterImage(ctx, "root", addr, identities, true, imageURL, hostname, keys); err != nil {
		return nil, err
	}
	version, err := a.waitForImage(ctx, addr, keyPath)
	if err != nil {
		return nil, err
	}
	return &imageEntry{addr: addr, user: "root", version: version, via: "stock"}, nil
}

// fetchAndRun is a shell command that fetches a script from the storage
// (with curl, or wget where there is no curl) to path on the machine and
// runs it with args. Nothing is bundled with ump: the storage is the one
// source, see images/rescue/README.md for what it serves.
func fetchAndRun(url, path string, args []string) string {
	src := shellJoin([]string{url})
	return fmt.Sprintf(`f=%s; if command -v curl >/dev/null; then curl -fsSL %s -o "$f"; else wget -q -O "$f" %s; fi && bash "$f" %s`, path, src, src, shellJoin(args))
}

// entryOptions say how a server is brought into the UMP rescue system.
type entryOptions struct {
	// via is auto (the default), direct or provider.
	via string
	// yes skips the questions (--yes); confirmed says the caller asked
	// its own question already, so none is asked here either.
	yes, confirmed bool
	// quiet sends the provider's rescue details to stderr, because an
	// encoded result follows on stdout.
	quiet bool
	// stock allows a provider's stock installation (which erases the
	// machine) as the way in when its rescue system takes no key; only a
	// reinstall, which erases the machine anyway, sets it.
	stock bool
	// users are tried in turn for the direct way: root, and the sudo
	// users where root cannot log in (loginUsers); root and ump when
	// empty.
	users []string
}

// imageEntry is how a server was brought into the UMP rescue system and
// how to reach it there.
type imageEntry struct {
	addr    string
	user    string
	version string
	// via is running (it was there already), direct (a kexec from the
	// running system) or provider (through its rescue system).
	via string
	// info is the provider's rescue system, when it was the way in.
	info *provider.RescueInfo
}

// enableProviderRescue arms the provider's rescue system with the keys,
// resets the server into it unless noReboot, and prints what the provider
// gave (password, host keys, notes) to out.
func (a *app) enableProviderRescue(ctx context.Context, m provider.Machine, rs provider.Rescue, pw provider.Power, keys []provider.PublicKey, keyPaths []string, out io.Writer, noReboot bool) (info *provider.RescueInfo, user string, rebooted, wasUp bool, err error) {
	wasUp = !noReboot && a.probe(ctx, m.IPv4)
	info, err = rs.EnableRescue(ctx, m, provider.RescueOptions{Keys: keys})
	if err != nil {
		return nil, "", false, false, fmt.Errorf("account %s: %w", m.Account, err)
	}
	rebooted = info.Reboots
	if !noReboot && !info.Reboots {
		if err := pw.Reboot(ctx, m, provider.RebootHard); err != nil {
			return nil, "", false, false, fmt.Errorf("account %s: rescue system enabled, but the reboot failed: %w", m.Account, err)
		}
		rebooted = true
	}
	user = info.User
	if user == "" {
		user = "root"
	}
	if rebooted {
		fmt.Fprintf(out, "Rescue system enabled for %s; rebooting into it.\n", describeRef(m))
	} else {
		fmt.Fprintf(out, "Rescue system enabled for %s; it boots at the next reboot.\n", describeRef(m))
	}
	fmt.Fprintf(out, "SSH keys: %s\n", strings.Join(keyPaths, ", "))
	if info.Password != "" {
		fmt.Fprintf(out, "Password: %s\n", info.Password)
	}
	if len(info.HostKeys) > 0 {
		fmt.Fprintf(out, "Host key fingerprints: %s\n", strings.Join(info.HostKeys, ", "))
	}
	if info.Note != "" {
		fmt.Fprintf(out, "Note: %s\n", info.Note)
	}
	return info, user, rebooted, wasUp, nil
}

// bringToImage brings the server into the UMP rescue system and reports
// how to reach it: a server that runs it already is left as it is, a
// running system that lets UMP in as root kexecs into it, otherwise the
// provider's rescue system is the first hop. It says what it does on
// stderr.
func (a *app) bringToImage(cmd *cobra.Command, m provider.Machine, prov provider.Provider, keys []provider.PublicKey, keyPath string, keyPaths []string, imageURL string, o entryOptions) (*imageEntry, error) {
	ctx := cmd.Context()
	addr := m.IPv4
	if addr == "" {
		addr = m.IPv6
	}
	rs, hasRescue := prov.(provider.Rescue)
	ask := func(question string) error {
		if o.confirmed {
			return nil
		}
		return a.confirm(cmd, o.yes, question)
	}
	hostname := rescueHostname(m.Name)

	if o.via != "provider" && addr != "" {
		fmt.Fprintf(a.stderr, "Checking root access to %s over SSH ...", addr)
		identities := append([]string{keyPath}, sshkey.DefaultIdentityFiles()...)
		users := o.users
		if len(users) == 0 {
			users = []string{"root", defaultAdmin}
		}
		user, p, ok, notAdmin := a.probeAdmin(ctx, users, addr, identities, false)
		switch {
		case ok && p.version != "":
			fmt.Fprintf(a.stderr, " %s already runs the UMP rescue system %s.\n", m.Name, p.version)
			return &imageEntry{addr: addr, user: user, version: p.version, via: "running"}, nil
		case ok:
			how := ""
			if !p.root {
				how = fmt.Sprintf(", as %s with sudo", user)
			}
			fmt.Fprintf(a.stderr, " yes (%s%s).\n", nonEmpty(p.os, "unknown system"), how)
			if err := ask(fmt.Sprintf("Load the UMP rescue image on %s over SSH and kexec into it? The running system ends without a shutdown.", describeRef(m))); err != nil {
				return nil, err
			}
			fmt.Fprintf(a.stderr, "Loading the UMP rescue image from %s on %s, without the provider's rescue system.\n", imageURL, m.Name)
			if err := a.enterImage(ctx, user, addr, identities, false, imageURL, hostname, keys); err != nil {
				return nil, err
			}
			version, err := a.waitForImage(ctx, addr, keyPath)
			if err != nil {
				return nil, err
			}
			return &imageEntry{addr: addr, user: "root", version: version, via: "direct"}, nil
		case notAdmin != "":
			fmt.Fprintf(a.stderr, " no (%s gets in, but is not root and has no sudo).\n", notAdmin)
		default:
			fmt.Fprintf(a.stderr, " no (tried %s).\n", strings.Join(users, ", "))
		}
		if o.via == "direct" {
			return nil, withExit(exitUnreachable, fmt.Errorf("no root access to %s over SSH (as root, or a sudo user with UMP's key); --via provider goes through the provider's rescue system", addr))
		}
		if !hasRescue {
			return nil, fmt.Errorf("no root access to %s over SSH, and provider type %s has no rescue system", addr, m.Provider)
		}
		fmt.Fprintln(a.stderr, "Going through the provider's rescue system.")
	}

	if !hasRescue {
		return nil, fmt.Errorf("account %s: provider type %s has no rescue system", m.Account, m.Provider)
	}
	pw, canReboot := prov.(provider.Power)
	if !canReboot {
		return nil, fmt.Errorf("account %s: provider type %s cannot reboot; use ump server rescue --no-reboot and reboot the server yourself", m.Account, m.Provider)
	}
	if kr, ok := prov.(provider.KeylessRescue); ok && kr.RescueTakesNoKey(m) {
		if si, ok := prov.(provider.StockInstaller); ok && o.stock && addr != "" {
			return a.stockThenImage(ctx, m, si, keys, keyPath, imageURL, hostname)
		}
		return nil, withExit(exitUnreachable, fmt.Errorf("the provider's rescue system of %s takes no SSH key (the password goes by mail), so UMP could not get in by itself; give UMP root access to the running system (UMP's key in root's authorized_keys, see ump config key) for the direct way, or use ump server rescue --no-image with the mailed password", m.Name))
	}
	if err := ask(fmt.Sprintf("Boot %s into the UMP rescue system through the provider's rescue system?", describeRef(m))); err != nil {
		return nil, err
	}
	out := a.stdout
	if o.quiet {
		out = a.stderr
	}
	info, user, _, wasUp, err := a.enableProviderRescue(ctx, m, rs, pw, keys, keyPaths, out, false)
	if err != nil {
		return nil, err
	}
	if info.Address == "" {
		return nil, errors.New("the provider named no address for its rescue system")
	}
	if err := a.waitForSSH(ctx, info.Address, wasUp); err != nil {
		return nil, err
	}
	// the provider's rescue system is only the first hop
	p, ok := a.probeHost(ctx, user, info.Address, []string{keyPath}, true)
	if !ok {
		manual := shellJoin(sshArgs(user, info.Address, []string{keyPath}, true, true, nil))
		return nil, withExit(exitUnreachable, fmt.Errorf("the rescue system of %s did not accept UMP's key; connect by hand (%s) and run: curl -fsSL %s/kexec-into-rescue.sh -o /tmp/kexec-into-rescue.sh && bash /tmp/kexec-into-rescue.sh -u %s", m.Name, manual, imageURL, imageURL))
	}
	if p.version != "" {
		fmt.Fprintf(a.stderr, "%s already runs the UMP rescue system %s.\n", m.Name, p.version)
		return &imageEntry{addr: info.Address, user: user, version: p.version, via: "running", info: info}, nil
	}
	fmt.Fprintf(a.stderr, "Provider rescue system reached (%s); loading the UMP rescue image from %s.\n", nonEmpty(p.os, "unknown system"), imageURL)
	if err := a.enterImage(ctx, user, info.Address, []string{keyPath}, true, imageURL, hostname, keys); err != nil {
		return nil, err
	}
	version, err := a.waitForImage(ctx, info.Address, keyPath)
	if err != nil {
		return nil, err
	}
	return &imageEntry{addr: info.Address, user: "root", version: version, via: "provider", info: info}, nil
}
