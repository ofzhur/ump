// Package cli implements the ump command line interface.
//
// Commands follow the kubectl shape: `ump <verb> <resource>` for resources
// (`ump get servers`), `ump config` for the configuration, and `-o` for
// output formats. Every error maps to one of the documented exit codes.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/home"
	"github.com/ofzhur/ump/internal/provider"
	_ "github.com/ofzhur/ump/internal/provider/all"
	"github.com/ofzhur/ump/internal/state"
)

// Version is the release version, set at build time with
// -ldflags "-X github.com/ofzhur/ump/internal/cli.Version=v0.1.0".
var Version = "dev"

// Exit codes, see docs/cli.md.
const (
	exitOK          = 0
	exitGeneric     = 1
	exitUsage       = 2
	exitAborted     = 3
	exitProvider    = 4
	exitUnreachable = 5
)

// app holds the process-level dependencies so that tests can run commands
// against buffers and fake providers.
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// isTTY reports whether stdin is an interactive terminal.
	isTTY func() bool
	// readSecret reads a line without echo; nil means "read a plain line".
	readSecret func() (string, error)
	// saveTerm captures the terminal state and returns a function that
	// restores it, for prompts abandoned while the echo is off.
	saveTerm func() func()
	// httpClient overrides the HTTP client handed to providers (tests).
	httpClient *http.Client
	// manual holds the servers added by hand, from the state loaded
	// last, for the manual provider type.
	manual map[string]provider.ManualServer
	// home is where this run's home is; store is the state document's
	// store, opened once per run, and doc the document read last (nil
	// after a change); pullFailed says the backend did not answer and
	// the mirror is used as it is.
	home       *home.Resolved
	pullFailed bool
	store      *state.Store
	doc        *state.Document
	// now is the clock, for cache timestamps; nil means time.Now.
	now func() time.Time
	// tz is the time zone for times shown to people; nil means local.
	tz *time.Location
	// ssh runs the local ssh client; nil means the real one.
	ssh sshRunner
	// probe reports whether SSH answers at an address; nil means a TCP
	// connect to the SSH port.
	probe func(ctx context.Context, addr string) bool
	// sshExec runs the local ssh client without a terminal, for what ump
	// does on machines by itself; nil means the real one.
	sshExec sshExecer
	// agentKeys lists the ssh-agent's public keys; nil means ssh-add -L.
	agentKeys func() ([]string, error)
	// editor opens a file in the user's editor; nil means $VISUAL/$EDITOR.
	editor func(path string) error
	// forgetHostKey removes an address's entries from the user's
	// known_hosts and says whether there were any; nil means ssh-keygen -R
	// on ~/.ssh/known_hosts.
	forgetHostKey func(addr string) (bool, error)
	// waitWarned says that a bad wait setting was reported already.
	waitWarned bool
	// ttlWarned says that a bad cache TTL setting was reported already.
	ttlWarned bool
	// liveTerm reports the size of the terminal stdout writes to, for
	// output that redraws itself; nil or ok false means no terminal.
	liveTerm func() (width, height int, ok bool)

	// global flags
	configFlag string
	accounts   []string
	verbose    bool
}

// Main runs the CLI with the given arguments and returns the exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// The first Ctrl-C cancels the context and lets commands unwind;
		// a second one terminates the process the usual way.
		<-ctx.Done()
		stop()
	}()
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	fd := int(os.Stdin.Fd())
	a.isTTY = func() bool { return term.IsTerminal(fd) }
	a.readSecret = func() (string, error) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	}
	a.liveTerm = func() (int, int, bool) {
		out := int(os.Stdout.Fd())
		if !term.IsTerminal(out) || os.Getenv("TERM") == "dumb" {
			return 0, 0, false
		}
		w, h, err := term.GetSize(out)
		return w, h, err == nil && w > 0 && h > 0
	}
	a.saveTerm = func() func() {
		state, err := term.GetState(fd)
		if err != nil {
			return func() {}
		}
		return func() { _ = term.Restore(fd, state) }
	}
	return a.run(ctx, args)
}

// offlineCommand says whether the arguments ask for something that
// must work at once and without a network: shell completion (which a
// bashrc runs, and which runs at every Tab), help and the version. Those
// neither pull the home from its backend nor check for updates.
func offlineCommand(args []string) bool {
	first := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--":
			return first == ""
		case "-h", "--help", "--version":
			return true
		case "--config", "--account":
			i++ // the global flags' values are not the command
		default:
			if first == "" && !strings.HasPrefix(arg, "-") {
				first = arg
			}
		}
	}
	switch first {
	case "", "completion", "help", "version", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true // "": the help
	}
	return false
}

func (a *app) run(ctx context.Context, args []string) int {
	if a.now == nil {
		a.now = time.Now
	}
	if a.tz == nil {
		a.tz = time.Local
	}
	if a.ssh == nil {
		a.ssh = runSSH
	}
	if a.probe == nil {
		a.probe = sshAnswers
	}
	if a.sshExec == nil {
		a.sshExec = execSSH
	}
	if a.agentKeys == nil {
		a.agentKeys = sshAgentKeys
	}
	if a.editor == nil {
		a.editor = runEditor
	}
	root := a.newRootCmd()
	root.SetArgs(args)
	if err := a.resolveHome(ctx, offlineCommand(args)); err != nil {
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
		return exitGeneric
	}
	err := root.ExecuteContext(ctx)
	a.pushHome(context.Background())
	if err == nil {
		return exitOK
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(a.stderr, "ump: interrupted")
		return exitAborted
	}
	var silent exitCode
	if errors.As(err, &silent) {
		return int(silent)
	}
	fmt.Fprintf(a.stderr, "ump: %v\n", err)
	code := exitCodeFor(err)
	if code == exitUsage {
		var ue *exitError
		if errors.As(err, &ue) && ue.cmd != nil {
			fmt.Fprintf(a.stderr, "Run '%s --help' for usage.\n", ue.cmd.CommandPath())
		}
	}
	return code
}

func (a *app) newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ump",
		Short: "Unus Mundus Provisioner: one operating system, in the same shape, on any machine",
		Long: `ump puts the same operating system, in the same shape, on any machine:
dedicated servers, cloud VMs and boxes in your own rack.

Everything happens to servers: ` + "`ump server list`" + ` lists the machines of every
configured provider account (the inventory of the fleet: -o wide|json,
--diff for what changed, prices, your attributes, servers added by hand
with ` + "`ump server add`" + `), ` + "`ump server show NAME`" + ` shows one in detail,
` + "`ump server reinstall NAME`" + ` installs it the way a profile says, ` + "`ump ssh NAME`" + `
logs in. ` + "`ump config isp`" + ` sets accounts up, ` + "`ump config os`" + ` the profiles.

UMP is made by Oleg Zhurko <oleg@zhurko.de>, https://ump.zhurko.de, and
licensed under the Apache License 2.0.`,
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			a.updateHint(cmd)
		},
	}
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetVersionTemplate("ump {{.Version}}\n")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &exitError{code: exitUsage, err: err, cmd: cmd}
	})
	pf := root.PersistentFlags()
	pf.StringVar(&a.configFlag, "config", "", "configuration file or directory (default $UMP_CONFIG, then ~/.config/ump/config.yaml)")
	pf.StringSliceVar(&a.accounts, "account", nil, "only use these accounts (repeatable, or comma-separated)")
	pf.BoolVarP(&a.verbose, "verbose", "v", false, "log API requests to stderr")

	root.AddCommand(a.newServerCmd())
	root.AddCommand(a.newSSHCmd())
	root.AddCommand(a.newConfigCmd())
	root.AddCommand(a.newVersionCmd())
	rejectUnknownSubcommands(root)
	return root
}

func (a *app) newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ump version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(a.stdout, "ump %s\n", versionString())
			return nil
		},
	}
}

func versionString() string {
	v := Version
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if modified == "true" {
				rev += "-dirty"
			}
			v += " (" + rev + ")"
		}
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	return v
}

// exitError carries an exit code with an error.
type exitError struct {
	code int
	err  error
	cmd  *cobra.Command
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

func usageErrorf(cmd *cobra.Command, format string, args ...any) error {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, args...), cmd: cmd}
}

// rejectUnknownSubcommands makes every command group without a run of
// its own show its help when bare and refuse an unknown word as a usage
// error; cobra itself only does that at the top level, and shows the
// help with exit status 0 below it (ump config isp view, after view was
// gone).
func rejectUnknownSubcommands(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		rejectUnknownSubcommands(sub)
	}
	if cmd.Parent() == nil || !cmd.HasSubCommands() || cmd.Run != nil || cmd.RunE != nil {
		return
	}
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		var names []string
		for _, s := range c.Commands() {
			if s.IsAvailableCommand() {
				names = append(names, s.Name())
			}
		}
		return usageErrorf(c, "unknown command %q for %s (%s)", args[0], c.CommandPath(), strings.Join(names, ", "))
	}
}

// usageArgs turns a cobra argument validator's error into a usage error.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return &exitError{code: exitUsage, err: err, cmd: cmd}
		}
		return nil
	}
}

func exitCodeFor(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		return exitProvider
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand flag") || strings.HasPrefix(msg, "invalid argument") {
		return exitUsage
	}
	return exitGeneric
}

// providerOptions builds the shared dependencies for provider instances.
func (a *app) providerOptions() provider.Options {
	opts := provider.Options{
		HTTPClient: a.httpClient,
		UserAgent:  "ump/" + Version,
		Manual:     a.manual,
	}
	if a.verbose {
		opts.Logf = func(format string, args ...any) {
			fmt.Fprintf(a.stderr, format+"\n", args...)
		}
	}
	return opts
}

// loadConfig loads the configuration file selected by the global flags.
func (a *app) loadConfig() (*config.Config, string, error) {
	path := config.ResolvePath(a.configFlag)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}

// buildProvider resolves an account's secrets and constructs its provider.
func (a *app) buildProvider(name string, acct config.Account) (provider.Provider, error) {
	settings, err := acct.Resolved()
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", name, err)
	}
	p, err := provider.Build(acct.Type, name, settings, a.providerOptions())
	if err != nil {
		return nil, fmt.Errorf("account %s: %w", name, err)
	}
	return p, nil
}

// selectAccounts returns the account names to use: those given with
// --account, or all configured ones.
func (a *app) selectAccounts(cmd *cobra.Command, cfg *config.Config) ([]string, error) {
	// the state's manual servers bring their account along
	if _, err := a.loadState(cmd.Context(), cfg); err != nil {
		return nil, err
	}
	if len(a.accounts) == 0 {
		return cfg.AccountNames(), nil
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range a.accounts {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if _, ok := cfg.Accounts[n]; !ok {
			return nil, usageErrorf(cmd, "account %q is not configured (configured: %s)", n, joinOrNone(cfg.AccountNames()))
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, nil
}

// fmtTime renders a time for people, in the configured zone.
func (a *app) fmtTime(t time.Time) string {
	return t.In(a.tz).Format("2006-01-02 15:04 MST")
}

func joinOrNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
