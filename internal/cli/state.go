package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/manual"
	"github.com/ofzhur/ump/internal/state"
)

// The state: what ump knows about the fleet beyond the providers' word
// (per-server settings, manual servers, what was installed where),
// behind a backend chosen by URL. Where it lives: UMP_STATE in the
// environment, else state.url in the configuration, else the local
// state file (a fresh system).

// stateURL is where the state lives, and whether the environment said so.
func stateURL(cfg *config.Config) (url string, fromEnv bool) {
	if u := strings.TrimSpace(os.Getenv("UMP_STATE")); u != "" {
		return u, true
	}
	return strings.TrimSpace(cfg.State.URL), false
}

// stateCredentials gathers what the backend needs: the environment
// wins over the configuration, so that CI needs no file.
func (a *app) stateCredentials(cfg *config.Config) (state.Credentials, error) {
	c := state.Credentials{Client: a.httpClient, Runner: a.sshRunner()}
	resolve := func(v config.Value, env ...string) (string, error) {
		for _, e := range env {
			if x := os.Getenv(e); x != "" {
				return x, nil
			}
		}
		if v.IsZero() {
			return "", nil
		}
		return v.Resolve()
	}
	var err error
	if c.Endpoint = os.Getenv("UMP_STATE_ENDPOINT"); c.Endpoint == "" {
		c.Endpoint = cfg.State.Endpoint
	}
	if c.Region = os.Getenv("AWS_REGION"); c.Region == "" {
		if c.Region = os.Getenv("AWS_DEFAULT_REGION"); c.Region == "" {
			c.Region = cfg.State.Region
		}
	}
	if c.AccessKey, err = resolve(cfg.State.AccessKey, "AWS_ACCESS_KEY_ID"); err != nil {
		return c, fmt.Errorf("state.access_key: %w", err)
	}
	if c.SecretKey, err = resolve(cfg.State.SecretKey, "AWS_SECRET_ACCESS_KEY"); err != nil {
		return c, fmt.Errorf("state.secret_key: %w", err)
	}
	if c.Token, err = resolve(cfg.State.Token, "UMP_STATE_TOKEN", "VAULT_TOKEN", "CONSUL_HTTP_TOKEN"); err != nil {
		return c, fmt.Errorf("state.token: %w", err)
	}
	if c.User, err = resolve(cfg.State.User, "UMP_STATE_USER"); err != nil {
		return c, fmt.Errorf("state.user: %w", err)
	}
	if c.Password, err = resolve(cfg.State.Password, "UMP_STATE_PASSWORD"); err != nil {
		return c, fmt.Errorf("state.password: %w", err)
	}
	return c, nil
}

// sshRunner runs a command on a host with the user's ssh client (their
// keys, agent and configuration), for the ssh:// backend.
func (a *app) sshRunner() state.Runner {
	return func(ctx context.Context, host, command string, stdin []byte) ([]byte, int, error) {
		argv := withSSHOptions([]string{"ssh", host, "--", command}, batchOptions(15*time.Second)...)
		var out, errb bytes.Buffer
		code, err := a.sshExec(ctx, argv, bytes.NewReader(stdin), &out, &errb)
		if err != nil {
			return nil, 0, err
		}
		if code == 255 {
			return nil, 0, fmt.Errorf("ssh %s: %s", host, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), code, nil
	}
}

// stateStore opens the state's backend for the configuration.
func (a *app) stateStore(cfg *config.Config) (*state.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	url, _ := stateURL(cfg)
	creds, err := a.stateCredentials(cfg)
	if err != nil {
		return nil, err
	}
	b, err := state.Open(url, creds)
	if err != nil {
		return nil, err
	}
	a.store = &state.Store{Backend: b}
	return a.store, nil
}

// loadState reads the state for the configuration, moving what an older
// configuration file kept itself (servers, manual) into it once, and
// makes the manual servers known to the manual provider type.
func (a *app) loadState(ctx context.Context, cfg *config.Config, cfgPath string) (*state.Document, error) {
	if a.doc != nil {
		a.manualAccount(cfg, a.doc)
		return a.doc, nil
	}
	st, err := a.stateStore(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.Servers) > 0 || len(cfg.Manual) > 0 {
		if err := st.Update(ctx, func(d *state.Document) error {
			for ref, s := range cfg.Servers {
				if _, exists := d.Servers[ref]; !exists {
					d.Servers[ref] = state.Server{Profile: s.Profile, Price: s.Price, Attributes: s.Attributes}
				}
			}
			for name, m := range cfg.Manual {
				if _, exists := d.Manual[name]; !exists {
					d.Manual[name] = m
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		cfg.Servers, cfg.Manual = nil, nil
		if cfgPath != "" {
			if err := config.Save(cfgPath, cfg); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(a.stderr, "Moved the per-server settings and the manual servers from the configuration file into the state (%s).\n", st.Backend)
	}
	d, _, err := st.Read(ctx)
	if err != nil {
		return nil, err
	}
	a.doc = d
	a.manualAccount(cfg, d)
	return d, nil
}

// manualAccount makes the manual servers of the state known to the
// manual provider type, with the account that lists them (in memory;
// nothing is written to the configuration file).
func (a *app) manualAccount(cfg *config.Config, d *state.Document) {
	a.manual = d.Manual
	if len(d.Manual) == 0 {
		return
	}
	if _, ok := cfg.Accounts[manual.AccountName]; !ok {
		if cfg.Accounts == nil {
			cfg.Accounts = map[string]config.Account{}
		}
		cfg.Accounts[manual.AccountName] = config.Account{Type: manual.TypeName, Settings: map[string]config.Value{}}
	}
}

// updateState changes the state; the next loadState reads it again.
func (a *app) updateState(ctx context.Context, cfg *config.Config, fn func(d *state.Document) error) error {
	st, err := a.stateStore(cfg)
	if err != nil {
		return err
	}
	a.doc = nil
	return st.Update(ctx, fn)
}

// lockServer takes the server's lock for an operation and returns the
// release; the message names who holds it when it is taken.
func (a *app) lockServer(ctx context.Context, cfg *config.Config, m provider.Machine, what string) (func(), error) {
	st, err := a.stateStore(cfg)
	if err != nil {
		return nil, err
	}
	unlock, err := st.Lock(ctx, m.Ref(), what)
	if err != nil {
		var locked *state.ErrLocked
		if errors.As(err, &locked) {
			return nil, withExit(exitAborted, err)
		}
		return nil, err
	}
	return func() {
		if err := unlock(); err != nil {
			fmt.Fprintf(a.stderr, "ump: releasing the lock of %s: %v\n", m.Ref(), err)
		}
	}, nil
}

// applyState puts what the state knows onto the listed machines: a
// price set by hand wins over the provider's, the attributes are the
// user's alone.
func applyState(d *state.Document, ms []provider.Machine) {
	if d == nil {
		return
	}
	for i := range ms {
		s, ok := d.Servers[ms[i].Ref()]
		if !ok {
			continue
		}
		if s.Price != nil {
			p := *s.Price
			p.Source = "manual"
			ms[i].Price = &p
		}
		if len(s.Attributes) > 0 {
			ms[i].Attributes = s.Attributes
		}
	}
}

func (a *app) newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Where the state lives, and what it holds",
		Long: `The state is what ump knows about the fleet beyond the providers' word:
the profile, price and attributes set per server, the servers added by
hand, and what ump installed where. It is one YAML document behind a
backend, shared by whoever uses the same one, and locked per server for
the operations that must not run twice at once (ump server reinstall,
rescue).

Where it lives: UMP_STATE in the environment, else state.url in the
configuration file, else the local state file (` + state.DefaultPath() + `).
Backends: local; file:///path (a file, for example in the fleet's git
repository); ssh://[user@]host/path (a file on another machine);
s3://bucket/key with an endpoint, or r2://account/bucket/key; http(s)://
(GET, PUT with If-Match, LOCK and UNLOCK ?key=...); consul://host:8500/path;
vault://host:8200/mount/path (KV v2). Credentials: state.* in the
configuration (secret references), or AWS_ACCESS_KEY_ID,
AWS_SECRET_ACCESS_KEY, AWS_REGION, UMP_STATE_ENDPOINT, UMP_STATE_TOKEN
(also VAULT_TOKEN, CONSUL_HTTP_TOKEN), UMP_STATE_USER, UMP_STATE_PASSWORD.

Without a subcommand: where the state is, its version and what it holds.
Edit it by hand where it is a file, or with edit, pull and push here;
every command rewrites it in its own layout.`,
		Example: `  ump state
  ump state show
  ump state edit
  ump state migrate --to s3://fleet/ump/state.yaml
  UMP_STATE=file:///srv/fleet/ump/state.yaml ump server list
  ump state locks
  ump state unlock hetzner-cloud/123456`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			d, err := a.loadState(cmd.Context(), cfg, path)
			if err != nil {
				return err
			}
			st, _ := a.stateStore(cfg)
			url, fromEnv := stateURL(cfg)
			how := "the default, the local state file"
			switch {
			case fromEnv:
				how = "UMP_STATE in the environment"
			case url != "":
				how = "state.url in the configuration"
			}
			_, version, _ := st.Read(cmd.Context())
			fmt.Fprintf(a.stdout, "State:     %s (%s)\n", st.Backend, how)
			if version == "" {
				fmt.Fprintln(a.stdout, "Version:   none yet (a fresh system; the first ump server set, add or reinstall writes it)")
			} else {
				fmt.Fprintf(a.stdout, "Version:   %s\n", version)
			}
			if !d.Updated.IsZero() {
				fmt.Fprintf(a.stdout, "Updated:   %s\n", a.fmtTime(d.Updated))
			}
			installed := 0
			for _, s := range d.Servers {
				if s.Installed != nil {
					installed++
				}
			}
			fmt.Fprintf(a.stdout, "Servers:   %d with settings, %d with an installation recorded, %d added by hand\n", len(d.Servers), installed, len(d.Manual))
			if locks, err := st.Locks(cmd.Context()); err == nil && len(locks) > 0 {
				fmt.Fprintf(a.stdout, "Locks:     %d (ump state locks)\n", len(locks))
			}
			return nil
		},
	}
	cmd.AddCommand(a.newStateShowCmd(), a.newStateEditCmd(), a.newStatePullCmd(), a.newStatePushCmd(), a.newStateMigrateCmd(), a.newStateLocksCmd(), a.newStateUnlockCmd())
	return cmd
}

func (a *app) newStateShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the state document",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			d, err := a.loadState(cmd.Context(), cfg, path)
			if err != nil {
				return err
			}
			data, err := d.Bytes()
			if err != nil {
				return err
			}
			_, err = a.stdout.Write(data)
			return err
		},
	}
}

func (a *app) newStateEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the state in your editor and write it back, checked, when nothing changed meanwhile",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, err := a.loadState(cmd.Context(), cfg, path); err != nil {
				return err
			}
			st, _ := a.stateStore(cfg)
			d, version, err := st.Read(cmd.Context())
			if err != nil {
				return err
			}
			data, err := d.Bytes()
			if err != nil {
				return err
			}
			tmp, err := os.CreateTemp("", "ump-state-*.yaml")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			tmp.Write(data)
			tmp.Close()
			if err := a.editor(tmp.Name()); err != nil {
				return err
			}
			edited, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			if bytes.Equal(edited, data) {
				fmt.Fprintln(a.stdout, "No change.")
				return nil
			}
			return a.pushState(cmd.Context(), st, edited, version)
		},
	}
}

// pushState writes an edited document after checking it.
func (a *app) pushState(ctx context.Context, st *state.Store, data []byte, ifVersion string) error {
	d, err := state.Parse(data)
	if err != nil {
		return fmt.Errorf("the state is not valid, nothing was written: %w", err)
	}
	d.Updated = time.Now().UTC().Truncate(time.Second)
	out, err := d.Bytes()
	if err != nil {
		return err
	}
	if _, err := st.Backend.Write(ctx, out, ifVersion); err != nil {
		if errors.Is(err, state.ErrConflict) {
			return errors.New("the state changed since it was read; read it again (ump state pull) and redo the change, or push --force")
		}
		return fmt.Errorf("state %s: %w", st.Backend, err)
	}
	fmt.Fprintf(a.stdout, "Wrote the state to %s.\n", st.Backend)
	return nil
}

func (a *app) newStatePullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull FILE",
		Short: "Write the state document to a file (- for stdout)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, err := a.loadState(cmd.Context(), cfg, path); err != nil {
				return err
			}
			st, _ := a.stateStore(cfg)
			d, version, err := st.Read(cmd.Context())
			if err != nil {
				return err
			}
			data, err := d.Bytes()
			if err != nil {
				return err
			}
			// the version travels with the file, so that push can refuse a
			// file pulled before someone else wrote
			data = append([]byte(fmt.Sprintf("# pulled from %s, version %s\n", st.Backend, nonEmpty(version, "none"))), data...)
			if args[0] == "-" {
				_, err = a.stdout.Write(data)
				return err
			}
			if err := os.WriteFile(expandHome(args[0]), data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Wrote the state to %s.\n", args[0])
			return nil
		},
	}
}

func (a *app) newStatePushCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "push FILE [--force]",
		Short: "Write a file as the state, checked; refused when the state changed since it was pulled unless --force",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, err := a.loadState(cmd.Context(), cfg, path); err != nil {
				return err
			}
			st, _ := a.stateStore(cfg)
			var data []byte
			if args[0] == "-" {
				data, err = io.ReadAll(a.stdin)
			} else {
				data, err = os.ReadFile(expandHome(args[0]))
			}
			if err != nil {
				return err
			}
			_, version, err := st.Read(cmd.Context())
			if err != nil {
				return err
			}
			// a pulled file carries the version it was pulled from and is
			// written back over that one; a file without it, or --force,
			// goes over whatever is there now
			if m := pulledVersion.FindSubmatch(data); m != nil && !force {
				pulled := string(m[1])
				if pulled == "none" {
					pulled = ""
				}
				if pulled != version {
					return errors.New("the state was written after this file was pulled; pull again and redo the change, or push --force")
				}
			}
			return a.pushState(cmd.Context(), st, data, version)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "write even when the state changed since the file was pulled")
	return cmd
}

var pulledVersion = regexp.MustCompile(`(?m)^# pulled from .*, version (\S+)$`)

func (a *app) newStateMigrateCmd() *cobra.Command {
	var to string
	var force bool
	cmd := &cobra.Command{
		Use:   "migrate --to URL [--force]",
		Short: "Copy the state to another backend and switch the configuration to it",
		Long: `Read the state where it is, write it to the backend URL names (which
must be empty unless --force) and set state.url in the configuration
file to it. The credentials of the new backend come from the
environment or from state.* in the configuration, which you set before
(ump state --help lists them). With UMP_STATE in the environment the
configuration is not changed: the environment keeps saying where the
state is. The old copy is left where it was.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageErrorf(cmd, "--to URL is required")
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			d, err := a.loadState(cmd.Context(), cfg, path)
			if err != nil {
				return err
			}
			creds, err := a.stateCredentials(cfg)
			if err != nil {
				return err
			}
			dest, err := state.Open(to, creds)
			if err != nil {
				return err
			}
			if dest.String() == a.store.Backend.String() {
				return fmt.Errorf("the state is at %s already", dest)
			}
			existing, version, err := dest.Read(cmd.Context())
			if err != nil {
				return fmt.Errorf("state %s: %w", dest, err)
			}
			if len(existing) > 0 && !force {
				return fmt.Errorf("%s holds a state already; --force overwrites it", dest)
			}
			d.Updated = time.Now().UTC().Truncate(time.Second)
			data, err := d.Bytes()
			if err != nil {
				return err
			}
			if _, err := dest.Write(cmd.Context(), data, version); err != nil {
				return fmt.Errorf("state %s: %w", dest, err)
			}
			if _, fromEnv := stateURL(cfg); fromEnv {
				fmt.Fprintf(a.stdout, "Copied the state to %s. UMP_STATE still points at %s; change it to %s.\n", dest, a.store.Backend, to)
				return nil
			}
			cfg.State.URL = to
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Copied the state to %s and set state.url in %s; the old copy at %s stays.\n", dest, config.AbbreviateHome(path), a.store.Backend)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the backend to move to (a URL, see ump state --help)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a state the destination holds")
	return cmd
}

func (a *app) newStateLocksCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:   "locks",
		Short: "List the per-server locks held",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			st, err := a.stateStore(cfg)
			if err != nil {
				return err
			}
			locks, err := st.Locks(cmd.Context())
			if err != nil {
				return err
			}
			rows := make([][]string, 0, len(locks))
			for _, l := range locks {
				rows = append(rows, []string{l.Key, l.What, l.Who, a.fmtTime(l.Since)})
			}
			return writeTable(a.stdout, []string{"SERVER", "OPERATION", "HELD BY", "SINCE"}, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newStateUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock SERVER",
		Short: "Release a server's lock left behind by an ump that died",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			st, err := a.stateStore(cfg)
			if err != nil {
				return err
			}
			if err := st.Backend.Unlock(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("state %s: %w", st.Backend, err)
			}
			fmt.Fprintf(a.stdout, "Unlocked %s.\n", args[0])
			return nil
		},
	}
}
