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

// FreeBSD is installed in two steps. The installer in the UMP rescue
// system plans, wipes the disks and puts the UMP FreeBSD rescue system
// on them, with its boot config (it ends with stagedMarker); the
// firmware starts that system from the disk, and the installer for
// FreeBSD does the rest there. ump follows what the installer did: a
// run that staged has a second step (installStaged).

// stagedMarker starts the last line of an installer run that staged an
// installation in two steps, where a complete one says "==> installed:".
const stagedMarker = "==> staged:"

// releasingDisks is the line both installers print right before their
// first write to a disk.
const releasingDisks = "==> releasing the disks"

// noWayBack is what the question of a reinstall adds for FreeBSD on a
// server whose provider has no rescue system: the direct way into the
// UMP rescue system is a kexec, and FreeBSD has none into a Linux.
const noWayBack = "FreeBSD cannot be left again from here: the next reinstall of this server is the provider's."

// freebsdFetchFailed is the exit status of the command that runs the
// installer for FreeBSD when the installer could not be fetched: one the
// installer does not have, where fetch's own is 1, a failed installation.
const freebsdFetchFailed = 69

// installerStaged says that the installer's output ends an installation
// in two steps after its first: a line that starts with stagedMarker.
func installerStaged(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, "\r"), stagedMarker) {
			return true
		}
	}
	return false
}

// installStaged runs the second step of an installation in two steps,
// FreeBSD's, on a server whose disks the installer in the UMP rescue
// system (e) wiped and staged: the provider is told to start the server
// from its disks, before this first reboot already; the rescue system is
// rebooted; ump waits until the UMP FreeBSD rescue system answers, and
// runs the installer for FreeBSD there with the same specification.
// After it e is the server in the UMP FreeBSD rescue system with FreeBSD
// on its disks, as startInstalled takes it. An error says where the
// installation stopped and what the disks hold.
func (a *app) installStaged(ctx context.Context, p *installPlan, e *installEntry) error {
	m := p.m
	lead := m.Name + " has the UMP FreeBSD rescue system on its disks"
	// until the reboot the server is in the UMP rescue system still
	stays := fmt.Sprintf("Until a reboot %s stays in the UMP rescue system (%s); the same reinstall again starts over from there, and wipes and stages the disks once more", m.Name, e.staging)
	diskBoot, err := bootFromDisks(ctx, m, p.prov, lead)
	if err != nil {
		return fmt.Errorf("%w. %s", err, stays)
	}
	e.prepared, e.diskBoot = true, diskBoot
	fmt.Fprintf(a.stderr, "Rebooting %s into the UMP FreeBSD rescue system ...\n", m.Name)
	if err := a.rebootEntry(ctx, p, e, "systemctl reboot", diskBoot, lead); err != nil {
		return fmt.Errorf("%w. %s", err, stays)
	}
	// a rescue system's host keys are new at every boot: those pinned for
	// the UMP rescue system are not the FreeBSD one's
	a.unpin(e.addr)
	version, err := a.waitForFreeBSDRescue(ctx, p, e)
	if err != nil {
		return err
	}
	e.freebsd, e.user, e.version = true, "root", version

	code, out, err := a.runFreeBSDInstaller(ctx, p, e)
	if err != nil {
		return err
	}
	// the installer's exit statuses are those of the first step's: 2 a
	// refusal before anything is written, 1 a step that failed after the
	// first disk was cleared
	gone := "its disks hold that rescue system alone, and the old system is gone"
	again := "the same reinstall again starts over" + a.throughProvider(p)
	switch {
	case code == 0:
		return nil
	case code == 255:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation; the server is in the UMP FreeBSD rescue system, or was: look there (%s), and at its console. %s is not installed until the installer said so; %s", e.addr, e.staging, p.spec.OS, again))
	case code == freebsdFetchFailed || !installerRan(out):
		return fmt.Errorf("%s could not fetch the installer for FreeBSD from %s (see above); the server must reach the storage. %s stays in the UMP FreeBSD rescue system (%s): %s; %s", e.addr, freebsdInstaller(p.storage), m.Name, e.staging, gone, again)
	case code == 2:
		return fmt.Errorf("the installer for FreeBSD refused the specification or the machine (exit status 2, see above) and wrote nothing; %s stays in the UMP FreeBSD rescue system (%s): %s; %s", m.Name, e.staging, gone, again)
	}
	return fmt.Errorf("the installation on %s failed in the UMP FreeBSD rescue system (exit status %d, see above); the server stays there (%s), and a reboot finds no system on its disks; %s", m.Name, code, e.staging, again)
}

// throughProvider says, after "the same reinstall again starts over",
// by which way: no kexec leads out of the UMP FreeBSD rescue system, so
// the next attempt is the provider's.
func (a *app) throughProvider(p *installPlan) string {
	if _, ok := p.prov.(provider.Rescue); ok {
		return ", through the provider's rescue system"
	}
	if _, ok := p.prov.(provider.StockInstaller); ok {
		return ": the provider reinstalls the server with a stock Linux first, since it has no rescue system"
	}
	return fmt.Sprintf(" once the server runs a Linux that lets UMP in again, which is the provider's to bring (its console, its own rescue system, its reinstall): provider type %s has no rescue system", p.m.Provider)
}

// freebsdInstaller is where the storage has the installer for FreeBSD.
func freebsdInstaller(storage string) string {
	return storage + "/install/freebsd/install.sh"
}

// runFreeBSDInstaller fetches the installer for FreeBSD from the storage
// and runs it in the UMP FreeBSD rescue system (e), with the
// specification on its stdin, as runInstaller does in the UMP rescue
// system: the keys go into root's first, and what the installer prints
// goes to stderr as it comes, and comes back with its exit status.
func (a *app) runFreeBSDInstaller(ctx context.Context, p *installPlan, e *installEntry) (int, string, error) {
	remote := ensureKeysCommand(p.keys) + "cat > " + installerSpec + " && " + fetchAndRunSh(freebsdInstaller(p.storage), "/tmp/ump-install.sh", []string{"--spec", installerSpec})
	argv := withSSHOptions(a.sshTo(e.user, e.addr, []string{p.keyPath}, true, true, []string{remote}), batchOptions(10*time.Second)...)
	var buf bytes.Buffer
	w := io.MultiWriter(a.stderr, &buf)
	code, err := a.sshExec(ctx, argv, bytes.NewReader(p.specJSON), w, w)
	return code, buf.String(), err
}

// fetchAndRunSh is fetchAndRun for FreeBSD: a shell command that fetches
// a script from the storage with fetch, which the base system has, to
// path on the machine and runs it with sh and args. A failed fetch is
// tried again for half a minute, as there; when every try failed the
// command ends with freebsdFetchFailed.
func fetchAndRunSh(url, path string, args []string) string {
	return fmt.Sprintf(`f=%s; i=0; until fetch -q -T 10 -o "$f" %s; do i=$((i+1)); [ $i -lt 6 ] || exit %d; sleep 5; done; sh "$f" %s`, path, shellJoin([]string{url}), freebsdFetchFailed, shellJoin(args))
}

// freebsdRescueCommand asks a machine whether it is the UMP FreeBSD
// rescue system: that one prints its version and "FreeBSD" with its
// release. The UMP rescue system has the version file too and says
// Linux; the old system and a provider's rescue system have no such
// file.
const freebsdRescueCommand = "cat /etc/ump-rescue-version && uname -sr"

// freebsdRescueVersion asks the machine at addr, as root with UMP's key,
// for the version of the UMP FreeBSD rescue system it runs and its
// release ("FreeBSD 15.1-RELEASE"); anything else answering, or nothing,
// is false.
func (a *app) freebsdRescueVersion(ctx context.Context, addr, keyPath string) (version, release string, ok bool) {
	argv := withSSHOptions(a.sshTo("root", addr, []string{keyPath}, true, true, []string{freebsdRescueCommand}), batchOptions(5*time.Second)...)
	var out bytes.Buffer
	code, err := a.sshExec(ctx, argv, nil, &out, io.Discard)
	if err != nil || code != 0 {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		return "", "", false
	}
	version, release = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if version == "" || !strings.HasPrefix(release, "FreeBSD ") {
		return "", "", false
	}
	return version, release, true
}

// waitForFreeBSDRescue waits until the UMP FreeBSD rescue system answers
// where the UMP rescue system did (e.addr: it has that system's address,
// from its boot config) and returns its version. The firmware starts it
// from the disk, which takes minutes on a dedicated server; the UMP
// rescue system may keep answering for a moment, so the wait is for the
// version file and FreeBSD, not for the port. When it ends without an
// answer the error says what the disks hold and how to go on (exit
// status 5).
func (a *app) waitForFreeBSDRescue(ctx context.Context, p *installPlan, e *installEntry) (string, error) {
	limit := a.waitLimit(imageWaitTimeout)
	fmt.Fprintf(a.stderr, "Waiting for the UMP FreeBSD rescue system on %s (up to %s) ...", e.addr, fmtWait(limit))
	start := time.Now()
	deadline := start.Add(limit)
	tick := a.waitTicker(start)
	for {
		if v, release, ok := a.freebsdRescueVersion(ctx, e.addr, p.keyPath); ok {
			fmt.Fprintf(a.stderr, " up after %s (ump-rescue %s, %s).\n", time.Since(start).Round(time.Second), v, release)
			return v, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(a.stderr)
			return "", withExit(exitUnreachable, a.noFreeBSDRescue(p, e, limit))
		}
		if err := sleepCtx(ctx, waitPoll); err != nil {
			fmt.Fprintln(a.stderr)
			return "", err
		}
		tick()
	}
}

// noFreeBSDRescue is the error of a UMP FreeBSD rescue system that did
// not answer in time: what the disks hold, the way back, which is the
// provider's, and where to look for the reason.
func (a *app) noFreeBSDRescue(p *installPlan, e *installEntry, limit time.Duration) error {
	m := p.m
	back := fmt.Sprintf("ump server rescue %s brings the server into the UMP rescue system through its provider; the same reinstall again, or one with another profile, goes that way by itself and starts over", m.Name)
	if _, ok := p.prov.(provider.Rescue); !ok {
		back = fmt.Sprintf("Provider type %s has no rescue system: the way back is the provider's (its console, its own rescue system, its reinstall with a Linux), and the same reinstall again, or one with another profile, starts over from a system that lets UMP in", m.Provider)
		if _, ok := p.prov.(provider.StockInstaller); ok {
			back = fmt.Sprintf("Provider type %s has no rescue system: the same reinstall again, or one with another profile, starts over once the provider reinstalled the server with a stock Linux, which ump asks it for", m.Provider)
		}
	}
	return fmt.Errorf("the UMP FreeBSD rescue system on %s did not answer within %s. The disks of %s hold that rescue system and nothing else: the old system is gone, and %s is not installed. %s. The installer of the next attempt shows the boot log the FreeBSD rescue system left on a disk, when it came as far as its network (with --dry-run it shows it and writes nothing); the server's console shows what its firmware and the kernel say. A machine whose firmware is slow may need a longer wait (wait: 30m in config.yaml, or UMP_WAIT=30m)", e.addr, fmtWait(limit), m.Name, p.spec.OS, back)
}
