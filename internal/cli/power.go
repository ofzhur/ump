package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/provider"
)

// sshPort is probed when a command waits for a machine to come back.
var sshPort = 22

// waitTimeout is how long a --wait waits for SSH to answer, unless the
// configuration's wait says otherwise.
var (
	waitTimeout  = 15 * time.Minute
	waitDownTime = 90 * time.Second
	waitPoll     = 2 * time.Second
	dialTimeout  = 3 * time.Second
)

type powerOptions struct {
	hard       bool
	powerCycle bool
	wait       bool
	yes        bool
}

func (a *app) newServerRebootCmd() *cobra.Command {
	var o powerOptions
	sub := &cobra.Command{
		Use:   "reboot NAME|ID|IP|PROVIDER/ID",
		Short: "Reboot a server",
		Long: `Reboot a server through its provider. By default the operating system is
asked to reboot (ACPI); --hard resets the machine without asking it;
--power-cycle cuts the power and switches it on again. A server that is
switched off is switched on instead.

--wait waits until SSH stops answering and answers again. Rebooting
asks for confirmation on a terminal; without one, --yes is required.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.powerCommand(cmd, args[0], "reboot", o)
		},
	}
	sub.Flags().BoolVar(&o.hard, "hard", false, "reset the machine without asking the operating system")
	sub.Flags().BoolVar(&o.powerCycle, "power-cycle", false, "cut the power and switch it on again")
	sub.Flags().BoolVar(&o.wait, "wait", false, "wait until SSH answers again")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	return sub
}

func (a *app) newServerStartCmd() *cobra.Command {
	var o powerOptions
	sub := &cobra.Command{
		Use:   "start NAME|ID|IP|PROVIDER/ID",
		Short: "Switch a server on",
		Long:  "Switch a server on through its provider; --wait waits until SSH answers.",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.powerCommand(cmd, args[0], "start", o)
		},
	}
	sub.Flags().BoolVar(&o.wait, "wait", false, "wait until SSH answers")
	return sub
}

func (a *app) newServerStopCmd() *cobra.Command {
	var o powerOptions
	sub := &cobra.Command{
		Use:   "stop NAME|ID|IP|PROVIDER/ID",
		Short: "Switch a server off",
		Long: `Switch a server off through its provider: the operating system is asked
to shut down unless --hard cuts the power. Asks for confirmation on a
terminal; without one, --yes is required.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.powerCommand(cmd, args[0], "stop", o)
		},
	}
	sub.Flags().BoolVar(&o.hard, "hard", false, "cut the power instead of asking the operating system")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	return sub
}

// resolveOne lists the accounts and resolves one server reference, with
// the duplicate prompt where there is a terminal.
func (a *app) resolveOne(cmd *cobra.Command, ref string) (provider.Machine, provider.Provider, error) {
	machines, byAccount, failed, total, err := a.listedMachines(cmd)
	if err != nil {
		return provider.Machine{}, nil, err
	}
	m, err := a.resolveServer(a.resolverUI(cmd), machines, ref)
	switch {
	case errors.Is(err, errQuit):
		return provider.Machine{}, nil, withExit(exitAborted, errors.New("aborted"))
	case err != nil:
		var nf *notFoundError
		if errors.As(err, &nf) && failed > 0 {
			return provider.Machine{}, nil, fmt.Errorf("%w (%d of %d accounts could not be listed)", err, failed, total)
		}
		return provider.Machine{}, nil, err
	}
	return m, byAccount[m.Account].prov, nil
}

// confirm asks before a disruptive action. --yes skips the question;
// without a terminal the question cannot be asked and --yes is required
// (exit code 3), as the safety model says.
func (a *app) confirm(cmd *cobra.Command, yes bool, question string) error {
	if yes {
		return nil
	}
	ui := a.resolverUI(cmd)
	if ui == nil {
		return withExit(exitAborted, fmt.Errorf("%s needs confirmation: run it in a terminal or pass --yes", cmd.CommandPath()))
	}
	ok, err := ui.Confirm(question, false)
	if errors.Is(err, errQuit) || (err == nil && !ok) {
		return withExit(exitAborted, errors.New("aborted"))
	}
	if err != nil {
		return err
	}
	return nil
}

func describeRef(m provider.Machine) string {
	s := m.Ref() + " (" + m.Name
	if m.IPv4 != "" {
		s += ", " + m.IPv4
	}
	return s + ")"
}

func (a *app) powerCommand(cmd *cobra.Command, ref, verb string, o powerOptions) error {
	if o.hard && o.powerCycle {
		return usageErrorf(cmd, "--hard and --power-cycle exclude each other")
	}
	m, prov, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	pw, ok := prov.(provider.Power)
	if !ok {
		return fmt.Errorf("account %s: provider type %s cannot control power", m.Account, m.Provider)
	}
	ctx := cmd.Context()
	switch verb {
	case "reboot":
		mode := provider.RebootSoft
		switch {
		case o.hard:
			mode = provider.RebootHard
		case o.powerCycle:
			mode = provider.RebootPowerCycle
		}
		if err := a.confirm(cmd, o.yes, fmt.Sprintf("Reboot %s (%s)?", describeRef(m), mode)); err != nil {
			return err
		}
		wasUp := o.wait && a.probe(ctx, m.IPv4)
		if err := pw.Reboot(ctx, m, mode); err != nil {
			return fmt.Errorf("account %s: %w", m.Account, err)
		}
		fmt.Fprintf(a.stdout, "Rebooting %s (%s)\n", describeRef(m), mode)
		if o.wait {
			return a.waitForSSH(ctx, m.IPv4, wasUp)
		}
	case "start":
		if err := pw.PowerOn(ctx, m); err != nil {
			return fmt.Errorf("account %s: %w", m.Account, err)
		}
		fmt.Fprintf(a.stdout, "Switching on %s\n", describeRef(m))
		if o.wait {
			return a.waitForSSH(ctx, m.IPv4, false)
		}
	case "stop":
		how := "shutdown"
		if o.hard {
			how = "power off"
		}
		if err := a.confirm(cmd, o.yes, fmt.Sprintf("Switch off %s (%s)?", describeRef(m), how)); err != nil {
			return err
		}
		if err := pw.PowerOff(ctx, m, o.hard); err != nil {
			return fmt.Errorf("account %s: %w", m.Account, err)
		}
		fmt.Fprintf(a.stdout, "Switching off %s (%s)\n", describeRef(m), how)
	}
	return nil
}

// sshAnswers reports whether something accepts TCP connections on the
// SSH port of addr.
func sshAnswers(ctx context.Context, addr string) bool {
	if addr == "" {
		return false
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(sshPort)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForSSH waits until the SSH port of addr answers. When it answered
// before the action (wasUp), it first waits for it to stop answering, so
// that a reboot is not mistaken for the old system still running.
func (a *app) waitForSSH(ctx context.Context, addr string, wasUp bool) error {
	if addr == "" {
		return errors.New("the server has no IPv4 address to wait for")
	}
	start := time.Now()
	if wasUp {
		fmt.Fprintf(a.stderr, "Waiting for SSH on %s to go down ...", addr)
		deadline := start.Add(waitDownTime)
		for a.probe(ctx, addr) {
			if time.Now().After(deadline) {
				fmt.Fprintln(a.stderr, " still up; assuming it rebooted quickly.")
				break
			}
			if err := sleepCtx(ctx, waitPoll); err != nil {
				fmt.Fprintln(a.stderr)
				return err
			}
		}
		if !a.probe(ctx, addr) {
			fmt.Fprintf(a.stderr, " down after %s.\n", time.Since(start).Round(time.Second))
		}
	}
	limit := a.waitLimit(waitTimeout)
	fmt.Fprintf(a.stderr, "Waiting for SSH on %s (up to %s) ...", addr, fmtWait(limit))
	deadline := time.Now().Add(limit)
	tick := a.waitTicker(start)
	for !a.probe(ctx, addr) {
		if time.Now().After(deadline) {
			fmt.Fprintln(a.stderr)
			return withExit(exitUnreachable, fmt.Errorf("SSH on %s did not answer within %s; a slow provider may need a longer wait (wait: 30m in config.yaml, or UMP_WAIT=30m)", addr, fmtWait(limit)))
		}
		if err := sleepCtx(ctx, waitPoll); err != nil {
			fmt.Fprintln(a.stderr)
			return err
		}
		tick()
	}
	fmt.Fprintf(a.stderr, " up after %s.\n", time.Since(start).Round(time.Second))
	return nil
}

// waitLimit is how long a wait may take: UMP_WAIT, else the
// configuration's wait, else def. A value that is not a duration is
// said once and ignored.
func (a *app) waitLimit(def time.Duration) time.Duration {
	v, from := os.Getenv("UMP_WAIT"), "UMP_WAIT"
	if v == "" {
		if cfg, _, err := a.loadConfig(); err == nil {
			v, from = cfg.Wait, "wait in the configuration"
		}
	}
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		if !a.waitWarned {
			fmt.Fprintf(a.stderr, "ump: %s is %q, not a duration such as 20m; waiting up to %s\n", from, v, fmtWait(def))
			a.waitWarned = true
		}
		return def
	}
	return d
}

// waitTicker returns what a wait loop calls after each poll: a dot every
// ten seconds, and the minutes waited instead of the dot at each full
// minute (.....1m.....2m), so that the wait shows how long it has run.
func (a *app) waitTicker(start time.Time) func() {
	last := start
	return func() {
		if time.Since(last) < 10*time.Second {
			return
		}
		last = time.Now()
		elapsed := time.Since(start)
		if m := int(elapsed / time.Minute); m > 0 && elapsed%time.Minute < 10*time.Second {
			fmt.Fprintf(a.stderr, "%dm", m)
			return
		}
		fmt.Fprint(a.stderr, ".")
	}
}

// fmtWait renders a limit for people: 15m, 1h30m, 45s.
func fmtWait(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var out string
	if h > 0 {
		out += fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dm", m)
	}
	if sec > 0 || out == "" {
		out += fmt.Sprintf("%ds", sec)
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// joinNotes renders optional notes for one-line outputs.
func joinNotes(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
