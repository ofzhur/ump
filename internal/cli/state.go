package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/home"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/manual"
	"github.com/ofzhur/ump/internal/state"
)

// The home: everything ump keeps (the configuration, the secrets, the
// profiles, the keys, the state, the listing cache) in one directory,
// which is ~/.config/ump unless UMP_STATE or ~/.config/ump/backend.yaml
// names a backend; then ump works on a mirror of the backend, pulled
// before a command and pushed after it. ump config state manages the
// pointer; the state document itself (state.yaml) is read and written
// through the backend directly, with the locks of the servers and the
// fleet.

// pullFresh is how long a pull of the mirror holds: a command within it
// of the last one works with the mirror as it is. Changes still go back
// after the command, each over the version it was pulled at, so nothing
// is lost to the shortcut.
var pullFresh = 30 * time.Second

// pullRetry is how long after a failed pull the next command tries
// again: offline, ump does not wait for the backend at every command.
// What the commands change meanwhile stays in the mirror and goes back
// with the first push that reaches the backend.
var pullRetry = 2 * time.Minute

// pullTimeout bounds a pull: a backend that does not answer costs a
// command this much at most.
const pullTimeout = 10 * time.Second

// resolveHome finds the home for this run and, behind a backend, brings
// the mirror up to date. It never fails the command for a backend that
// does not answer: the mirror is what there is, and the user is told.
// offline (shell completion, help, the version) never touches the
// backend, not even its credentials: those must not wait on anything.
//
// An ump that ump runs (the inventory plugin ofzhur.ansible.ump, under
// the Ansible of ump server reinstall and provision) has UMP_PARENT,
// which says where its parent reads the state: it reads it there too,
// the backend or, when the parent's pull failed, the mirror as it is, so
// that it sees what the parent recorded. It leaves the mirror to the
// parent, which pulled it and pushes it after the command: no pull, no
// push, so it takes no lock of the mirror and writes nothing there but
// the listing cache, which the parent's push carries along.
func (a *app) resolveHome(ctx context.Context, offline bool) error {
	a.doc, a.store, a.manual = nil, nil, nil
	parent := os.Getenv(parentEnv)
	a.nested = parent != ""
	resolve := func() (*home.Resolved, error) { return home.Resolve(a.sshRunner(), a.httpClient) }
	if offline {
		resolve = home.ResolveLocal
	}
	h, err := resolve()
	if err != nil {
		return err
	}
	a.home = h
	config.SetHome(h.Dir)
	if h.Backend == nil {
		return nil
	}
	if a.nested {
		a.pullFailed = parent == "mirror"
		return nil
	}
	m := &home.Mirror{Backend: h.Backend, Dir: h.Dir, Now: a.now}
	pulled, failed := m.LastPull()
	now := a.now()
	switch {
	case pulled.After(failed) && now.Sub(pulled) < pullFresh:
		return nil
	case failed.After(pulled) && now.Sub(failed) < pullRetry:
		// told a moment ago; the mirror is what there is
		a.pullFailed = true
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	if _, _, err := m.Pull(pctx); err != nil {
		fmt.Fprintf(a.stderr, "ump: the state could not be pulled from %s (%v); working with the mirror as it is\n", h.Backend, err)
		a.pullFailed = true
		m.PullFailed()
	}
	return nil
}

// pushHome writes what a command changed in the mirror back to the
// backend; conflicts are named and left for ump config state push.
func (a *app) pushHome(ctx context.Context) {
	if a.home == nil || a.home.Backend == nil || a.pullFailed || a.nested {
		return
	}
	m := &home.Mirror{Backend: a.home.Backend, Dir: a.home.Dir, Now: a.now}
	changes, err := m.Push(ctx)
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: the home could not be pushed to %s: %v (ump config state push retries)\n", a.home.Backend, err)
	}
	for _, c := range changes {
		if c.Kind == "conflict" {
			fmt.Fprintf(a.stderr, "ump: %s changed in %s meanwhile; your copy is kept in the mirror: ump config state pull --force takes theirs, push --force takes yours\n", c.Path, a.home.Backend)
		}
	}
}

// sshRunner runs a command on a host with the user's ssh client (their
// keys, agent and configuration), for the ssh:// backend.
func (a *app) sshRunner() backend.Runner {
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

// stateStore is the state document's store: the backend itself when
// there is one (changes go straight there, with locks), else the local
// home.
func (a *app) stateStore() *state.Store {
	if a.store != nil {
		return a.store
	}
	var b backend.Backend
	if a.home != nil && a.home.Backend != nil && !a.pullFailed {
		b = a.home.Backend
	} else {
		b = &backend.Local{Dir: config.UserConfigDir()}
	}
	a.store = &state.Store{Backend: b}
	return a.store
}

// loadState reads the state document and makes the manual servers
// known to the manual provider type, with the account that lists them.
func (a *app) loadState(ctx context.Context, cfg *config.Config) (*state.Document, error) {
	if a.doc == nil {
		d, _, err := a.stateStore().Read(ctx)
		if err != nil {
			return nil, err
		}
		a.doc = d
	}
	a.manualAccount(cfg, a.doc)
	return a.doc, nil
}

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

// updateState changes the state document; the next loadState reads it
// again. Behind a backend the mirror's copy is refreshed too, so that
// the push after the command has nothing stale to send; an ump that ump
// runs leaves that to its parent.
func (a *app) updateState(ctx context.Context, fn func(d *state.Document) error) error {
	a.doc = nil
	st := a.stateStore()
	if err := st.Update(ctx, fn); err != nil {
		return err
	}
	if a.home != nil && a.home.Backend != nil && st.Backend == a.home.Backend && !a.nested {
		m := &home.Mirror{Backend: a.home.Backend, Dir: a.home.Dir, Now: a.now}
		if _, _, err := m.Pull(ctx); err != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
		}
	}
	return nil
}

// lockServer takes the server's lock for an operation and returns the
// release; the message names who holds it when it is taken.
func (a *app) lockServer(ctx context.Context, m provider.Machine, what string) (func(), error) {
	unlock, err := a.stateStore().Lock(ctx, m.Ref(), what)
	if err != nil {
		var locked *backend.ErrLocked
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
// user's alone, and so are the private addresses. The OS, profile and
// layout are those of the last installation; the groups are those of
// the inventory: common and the recorded ones (the state keeps them
// without common) for a server ump installed, none for the others.
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
		ms[i].Groups, ms[i].Profile, ms[i].Layout = nil, "", ""
		putPrivate(&ms[i], s.Private)
		if s.Installed != nil {
			at := s.Installed.At
			ms[i].OS, ms[i].InstalledAt = s.Installed.OS, &at
			ms[i].Profile, ms[i].Layout = s.Installed.Profile, s.Installed.Layout
			ms[i].Groups = append([]string{commonGroup}, s.Groups...)
		}
	}
}

// putPrivate puts the private addresses onto a machine: all of them, and
// the network and address of the first alone.
func putPrivate(m *provider.Machine, ps []state.Private) {
	m.PrivateNetworks, m.PrivateNetwork, m.PrivateAddress = ps, "", ""
	if len(ps) > 0 {
		m.PrivateNetwork, m.PrivateAddress = ps[0].Network, ps[0].Address
	}
}

// --- ump config state ---------------------------------------------------

const stateLong = `The state is everything ump keeps: config.yaml with the accounts, the
secret files, the profiles, the named keys, UMP's own SSH key,
state.yaml (what ump knows about servers beyond the providers' word)
and the listing cache. It lives in a state backend: without one
configured, the local directory ` + "`~/.config/ump`" + `.

With a shared state backend, everyone who uses it acts as one UMP, and
ump works on a mirror under its cache directory: pulled before every
command, pushed after it, file by file with version tags, so the files
stay editable by hand and a concurrent writer is noticed, never
overwritten. state.yaml, the locks of a server (ump server reinstall and
rescue) and the fleet's lock (ump provision) go through the backend
directly.

Which backend: UMP_STATE in the environment names one (with
AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION, UMP_STATE_ENDPOINT,
UMP_STATE_TOKEN (also VAULT_TOKEN, CONSUL_HTTP_TOKEN), UMP_STATE_USER and
UMP_STATE_PASSWORD as needed); else ` + "`~/.config/ump/backend.yaml`" + `, which
ump config state setup writes; else the local directory. Backends:

  file:///path/to/dir              a directory (one in the fleet's git repository)
  ssh://[user@]host/path/to/dir    a directory on another machine, through your ssh client
  s3://bucket/prefix               S3 or any store with conditional writes (endpoint, region, keys)
  r2://account/bucket/prefix       Cloudflare R2 (keys); account is the Cloudflare account ID,
                                   on the dashboard's R2 overview and in the bucket's S3 API
                                   address https://ACCOUNT.r2.cloudflarestorage.com; the keys
                                   from R2 > Manage API tokens (Object Read & Write)
  http(s)://host/path              a service: GET/PUT with ETag, DELETE, GET ?list, LOCK/UNLOCK
  consul://host:8500/prefix        Consul KV (token); http+consul:// for plain http
  vault://host:8200/mount/prefix   Vault KV v2 (token); http+vault:// for plain http

On a terminal, ump config state setup is a wizard: it asks for the
backend and, when this machine holds state already, offers to migrate
it there.`

func (a *app) newConfigStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "state",
		Aliases: []string{"st"},
		Short:   "The state backend: where ump keeps everything, the local directory or one shared by a team",
		Long:    stateLong,
		Example: `  ump config state view            # which backend, and what the state holds
  ump config state setup           # the wizard: pick a backend, migrate the state there
  ump config state migrate --to file:///srv/fleet/ump
  ump config state set --url s3://fleet/ump --region eu-central-1 --access-key env:FLEET_KEY --secret-key file:~/.fleet.secret
  ump config state pull
  ump config state locks
  UMP_STATE=r2://account/bucket/ump ump server list`,
	}
	cmd.AddCommand(a.newStateViewCmd(), a.newStateSetupCmd(), a.newStateSetCmd(), a.newStateClearCmd(), a.newStateMigrateCmd(), a.newStatePullCmd(), a.newStatePushCmd(), a.newStateTestCmd(), a.newStateLocksCmd(), a.newStateUnlockCmd())
	return cmd
}

func (a *app) newStateViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "view",
		Aliases: verbAliases("view"),
		Short:   "Which backend the state is in, and what it holds",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			h := a.home
			switch {
			case h.Backend == nil:
				fmt.Fprintf(a.stdout, "Backend:   none, the state is in %s\n", config.AbbreviateHome(h.Dir))
			default:
				how := "backend.yaml"
				if h.Source == "env" {
					how = "UMP_STATE in the environment"
				}
				fmt.Fprintf(a.stdout, "Backend:   %s (%s)\n", h.Backend, how)
				fmt.Fprintf(a.stdout, "Mirror:    %s\n", config.AbbreviateHome(h.Dir))
				if a.pullFailed {
					fmt.Fprintln(a.stdout, "Pulled:    no, the backend did not answer; the mirror is what there is")
				}
			}
			cfg, _, err := a.loadConfig()
			if err == nil {
				fmt.Fprintf(a.stdout, "Accounts:  %d\n", len(cfg.Accounts))
			}
			if d, err := a.loadState(cmd.Context(), cfgOrEmpty(cfg)); err == nil {
				installed := 0
				for _, s := range d.Servers {
					if s.Installed != nil {
						installed++
					}
				}
				fmt.Fprintf(a.stdout, "Servers:   %d with settings, %d with an installation recorded, %d added by hand\n", len(d.Servers), installed, len(d.Manual))
			}
			if locks, err := a.stateStore().Locks(cmd.Context()); err == nil && len(locks) > 0 {
				fmt.Fprintf(a.stdout, "Locks:     %d (ump config state locks)\n", len(locks))
			}
			return nil
		},
	}
}

func cfgOrEmpty(cfg *config.Config) *config.Config {
	if cfg == nil {
		return config.New()
	}
	return cfg
}

// pointerFlags are the backend's settings as flags.
type pointerFlags struct {
	url, endpoint, region, accessKey, secretKey, token, user, password string
}

func (f *pointerFlags) add(cmd *cobra.Command, urlFlag string) {
	if urlFlag != "" {
		cmd.Flags().StringVar(&f.url, urlFlag, "", "the backend URL (see ump config state --help)")
	}
	cmd.Flags().StringVar(&f.endpoint, "endpoint", "", "the S3-compatible endpoint, for s3://")
	cmd.Flags().StringVar(&f.region, "region", "", "the region, for s3://")
	cmd.Flags().StringVar(&f.accessKey, "access-key", "", "the access key (env:NAME, file:PATH, command:CMD, or the value, which is kept in a file of its own)")
	cmd.Flags().StringVar(&f.secretKey, "secret-key", "", "the secret key (reference or value)")
	cmd.Flags().StringVar(&f.token, "token", "", "the token for http(s)://, consul:// and vault:// (reference or value)")
	cmd.Flags().StringVar(&f.user, "user", "", "the user for http(s):// basic auth (reference or value)")
	cmd.Flags().StringVar(&f.password, "password", "", "the password for http(s):// basic auth (reference or value)")
}

// pointer builds the pointer from the flags; a plain value for a secret
// is written to a file under the local directory and referenced.
func (f *pointerFlags) pointer(p *home.Pointer) (*home.Pointer, error) {
	if p == nil {
		p = &home.Pointer{}
	}
	if f.url != "" {
		p.URL = f.url
	}
	if f.endpoint != "" {
		p.Endpoint = f.endpoint
	}
	if f.region != "" {
		p.Region = f.region
	}
	for _, s := range []struct {
		flag, name string
		dst        *config.Value
	}{{f.accessKey, "access_key", &p.AccessKey}, {f.secretKey, "secret_key", &p.SecretKey}, {f.token, "token", &p.Token}, {f.user, "user", &p.User}, {f.password, "password", &p.Password}} {
		if s.flag == "" {
			continue
		}
		v := config.ParseValue(s.flag)
		if !v.IsRef() {
			path := pointerSecretPath(s.name)
			if err := os.MkdirAll(config.LocalDir(), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(s.flag+"\n"), 0o600); err != nil {
				return nil, err
			}
			v = config.Value{File: config.AbbreviateHome(path)}
		}
		*s.dst = v
	}
	return p, nil
}

func pointerSecretPath(name string) string {
	return config.LocalDir() + "/backend." + name
}

// checkBackend opens the pointer's backend and lists it.
func (a *app) checkBackend(ctx context.Context, p *home.Pointer) (backend.Backend, int, error) {
	b, err := home.OpenPointer(p, a.sshRunner(), a.httpClient)
	if err != nil {
		return nil, 0, err
	}
	entries, err := b.List(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("%s does not answer: %w", b, err)
	}
	return b, len(entries), nil
}

func (a *app) newStateSetCmd() *cobra.Command {
	var f pointerFlags
	var noTest bool
	cmd := &cobra.Command{
		Use:   "set --url URL [--endpoint URL] [--region R] [--access-key REF] [--secret-key REF] [--token REF] [--user REF] [--password REF]",
		Short: "Point ump at a state backend as it is, from flags (setup asks, migrate copies)",
		Long: `Write backend.yaml: the backend ump works on from now on. The state
there is used as it is; to carry the current state over, use migrate,
and on a terminal, setup asks for everything. The backend is listed
once to check that it answers, unless --no-test.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.url == "" {
				return usageErrorf(cmd, "--url is required (ump config state setup asks for it on a terminal)")
			}
			p, err := f.pointer(nil)
			if err != nil {
				return err
			}
			if !noTest {
				b, n, err := a.checkBackend(cmd.Context(), p)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "%s answers (%d files there).\n", b, n)
			}
			if err := home.SavePointer(p); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "The state backend is %s now (%s); the next command works on its mirror.\n", p.URL, config.AbbreviateHome(home.PointerPath()))
			if os.Getenv("UMP_STATE") != "" {
				fmt.Fprintln(a.stdout, "UMP_STATE is set in this environment and wins over the file.")
			}
			return nil
		},
	}
	f.add(cmd, "url")
	cmd.Flags().BoolVar(&noTest, "no-test", false, "do not check that the backend answers")
	return cmd
}

func (a *app) newStateClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Forget the backend: the state is the local directory's again (the mirror and the backend stay)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := home.SavePointer(nil); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "No backend configured; the state is in %s. The mirror and the backend's files stay as they are.\n", config.AbbreviateHome(config.LocalDir()))
			if os.Getenv("UMP_STATE") != "" {
				fmt.Fprintln(a.stdout, "UMP_STATE is set in this environment and still names a backend.")
			}
			return nil
		},
	}
}

func (a *app) newStateMigrateCmd() *cobra.Command {
	var f pointerFlags
	var force bool
	cmd := &cobra.Command{
		Use:   "migrate --to URL [backend flags] [--force]",
		Short: "Copy the state to a backend and point ump at it",
		Long: `Write every file of the current state (the local directory, or the
mirror of the current backend) to the backend URL names, which must be
empty unless --force, then write backend.yaml so that the next command
works there. The copy left behind stays. With UMP_STATE in the
environment the file is written too, but the environment keeps winning
until it says the new URL.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.url == "" {
				return usageErrorf(cmd, "--to URL is required")
			}
			p, err := f.pointer(nil)
			if err != nil {
				return err
			}
			b, _, err := a.checkBackend(cmd.Context(), p)
			if err != nil {
				return err
			}
			if a.home.Backend != nil && a.home.Backend.String() == b.String() {
				return fmt.Errorf("the state is at %s already", b)
			}
			n, err := home.Copy(cmd.Context(), a.home.Dir, b, force)
			if err != nil {
				return err
			}
			if err := home.SavePointer(p); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Copied %d files to %s and wrote %s; the next command works there. The copy at %s stays.\n", n, b, config.AbbreviateHome(home.PointerPath()), config.AbbreviateHome(a.home.Dir))
			if os.Getenv("UMP_STATE") != "" {
				fmt.Fprintf(a.stdout, "UMP_STATE is set in this environment and wins over the file; point it at %s.\n", p.URL)
			}
			return nil
		},
	}
	f.add(cmd, "to")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files the backend holds")
	return cmd
}

func (a *app) newStatePullCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "pull [--force]",
		Short: "Bring the mirror up to date with the backend",
		Long: `Download every file whose version changed in the backend since the
last pull. A file that changed in the mirror too is left as it is, for
push to name as a conflict, unless --force, which takes the backend's
copy over the mirror's.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.home.Backend == nil {
				return errors.New("no backend configured; the state is in the local directory")
			}
			m := &home.Mirror{Backend: a.home.Backend, Dir: a.home.Dir, Now: a.now, Theirs: force}
			pulled, removed, err := m.Pull(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Pulled %d files from %s, removed %d; the mirror is %s.\n", pulled, a.home.Backend, removed, config.AbbreviateHome(a.home.Dir))
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "take the backend's copy of a file that changed on both sides")
	return cmd
}

func (a *app) newStatePushCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "push [--force]",
		Short: "Write what changed in the mirror to the backend",
		Long: `Push every file that changed in the mirror since it was pulled, each
over the version it was pulled at. A file that changed on both sides is
a conflict: it is named and left alone, unless --force, which writes the
mirror's copy over the backend's.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.home.Backend == nil {
				return errors.New("no backend configured; the state is in the local directory")
			}
			m := &home.Mirror{Backend: a.home.Backend, Dir: a.home.Dir, Now: a.now}
			changes, err := m.Push(cmd.Context())
			if err != nil {
				return err
			}
			conflicts := 0
			for _, c := range changes {
				if c.Kind == "conflict" {
					conflicts++
					if force {
						if err := m.Force(cmd.Context(), c.Path); err != nil {
							return err
						}
						fmt.Fprintf(a.stdout, "%s: forced\n", c.Path)
						continue
					}
					fmt.Fprintf(a.stdout, "%s: conflict, changed in %s too (pull --force takes theirs; push --force takes yours)\n", c.Path, a.home.Backend)
					continue
				}
				fmt.Fprintf(a.stdout, "%s: %s\n", c.Path, c.Kind)
			}
			if len(changes) == 0 {
				fmt.Fprintf(a.stdout, "Nothing to push; %s is up to date.\n", a.home.Backend)
			}
			if conflicts > 0 && !force {
				return withExit(exitGeneric, fmt.Errorf("%d files in conflict", conflicts))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "write the mirror's copy over a file that changed on both sides")
	return cmd
}

func (a *app) newStateTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "test",
		Aliases: verbAliases("test"),
		Short:   "Check that the backend answers",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.home.Backend == nil {
				fmt.Fprintf(a.stdout, "No backend configured; the home is %s.\n", config.AbbreviateHome(a.home.Dir))
				return nil
			}
			entries, err := a.home.Backend.List(cmd.Context())
			if err != nil {
				return fmt.Errorf("%s does not answer: %w", a.home.Backend, err)
			}
			fmt.Fprintf(a.stdout, "%s answers (%d files there).\n", a.home.Backend, len(entries))
			return nil
		},
	}
}

func (a *app) newStateLocksCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:   "locks",
		Short: "List the locks held: the servers', and the fleet's (ump provision)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			locks, err := a.stateStore().Locks(cmd.Context())
			if err != nil {
				return err
			}
			rows := make([][]string, 0, len(locks))
			for _, l := range locks {
				rows = append(rows, []string{l.Key, l.What, l.Who, a.fmtTime(l.Since)})
			}
			return writeTable(a.stdout, []string{"LOCK", "OPERATION", "HELD BY", "SINCE"}, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newStateUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock SERVER|fleet",
		Short: "Release a lock left behind by an ump that died: a server's (provider/id), or the fleet's",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.Trim(args[0], ". ") == "" {
				return usageErrorf(cmd, "%q is not the name of a lock", args[0])
			}
			st := a.stateStore()
			if err := st.Backend.Unlock(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("%s: %w", st.Backend, err)
			}
			fmt.Fprintf(a.stdout, "Unlocked %s.\n", args[0])
			return nil
		},
	}
}

// newStateSetupCmd is the wizard: ump config state setup.
func (a *app) newStateSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "setup",
		Aliases: []string{"configure", "edit", "wizard"},
		Short:   "Choose the state backend and migrate the state there (a wizard, on a terminal)",
		Long: `Ask which backend the state should live in, its address and credentials,
check that it answers, and then: a backend that holds state already is
used as it is unless you choose to overwrite it with this machine's;
an empty one gets this machine's state copied over when there is any
(a migration), and the next command works there. "none" goes back to
the local directory. Without a terminal, use set or migrate.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ui := a.resolverUI(cmd)
			if ui == nil {
				return withExit(exitAborted, errors.New("ump config state setup is interactive and needs a terminal; use set --url URL or migrate --to URL instead"))
			}
			return a.stateWizard(cmd, ui)
		},
	}
}

// stateSummary says what the state in dir holds, for the wizard:
// "2 accounts, 5 profile files, 3 servers with settings"; "" when nothing.
func (a *app) stateSummary(ctx context.Context, dir string) string {
	var parts []string
	if cfg, _, err := a.loadConfig(); err == nil && len(cfg.Accounts) > 0 {
		parts = append(parts, fmt.Sprintf("%d accounts", len(cfg.Accounts)))
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "profiles")); err == nil && len(entries) > 0 {
		parts = append(parts, fmt.Sprintf("%d profile files", len(entries)))
	}
	if d, err := a.loadState(ctx, config.New()); err == nil && len(d.Servers)+len(d.Manual) > 0 {
		parts = append(parts, fmt.Sprintf("%d servers with settings", len(d.Servers)+len(d.Manual)))
	}
	if _, err := os.Stat(filepath.Join(dir, "keys.yaml")); err == nil {
		parts = append(parts, "named keys")
	}
	return strings.Join(parts, ", ")
}

// stateWizard is ump config state setup, over the prompter of the
// caller (the account wizard runs it on a fresh system).
func (a *app) stateWizard(cmd *cobra.Command, ui *prompter) error {
	ctx := cmd.Context()
	held := a.stateSummary(ctx, a.home.Dir)
	switch {
	case a.home.Backend != nil && held != "":
		ui.Printf("The state backend is %s (mirror %s); the state holds %s.\n", a.home.Backend, config.AbbreviateHome(a.home.Dir), held)
	case a.home.Backend != nil:
		ui.Printf("The state backend is %s (mirror %s); the state is empty.\n", a.home.Backend, config.AbbreviateHome(a.home.Dir))
	case held != "":
		ui.Printf("No state backend is configured; the state is in %s on this machine and holds %s.\n", config.AbbreviateHome(a.home.Dir), held)
	default:
		ui.Printf("No state backend is configured; the state is in %s on this machine, and it is empty: a clean start.\n", config.AbbreviateHome(a.home.Dir))
	}
	kinds := []string{"file  a directory on this machine (one in the fleet's git repository)", "ssh  a directory on another machine", "s3  an S3 bucket, or any store with conditional writes", "r2  a Cloudflare R2 bucket", "http  an HTTP service", "consul  Consul KV", "vault  Vault KV v2", "none  the local directory, no backend"}
	i, err := ui.Choose("Where should the state live?", kinds, -1)
	if err != nil {
		return err
	}
	kind := strings.Fields(kinds[i])[0]
	if kind == "none" {
		if err := home.SavePointer(nil); err != nil {
			return err
		}
		ui.Printf("No backend; the state stays in %s.\n", config.AbbreviateHome(config.LocalDir()))
		return nil
	}
	var f pointerFlags
	ask := func(label, def string) (string, error) { return ui.Line(label, def) }
	switch kind {
	case "file":
		f.url, err = ask("Directory (file:///path)", "file:///srv/fleet/ump")
	case "ssh":
		f.url, err = ask("ssh://[user@]host/path", "ssh://ops@jump/srv/ump")
	case "s3":
		if f.url, err = ask("s3://bucket/prefix", "s3://fleet/ump"); err == nil {
			if f.endpoint, err = ask("Endpoint (empty for AWS)", ""); err == nil {
				f.region, err = ask("Region", "eu-central-1")
			}
		}
	case "r2":
		ui.Printf("The account is the Cloudflare account ID (32 hex characters): \"Account ID\" on the dashboard's R2 overview, and the first part of the bucket's S3 API address https://ACCOUNT.r2.cloudflarestorage.com. The keys come next: R2 > Manage API tokens > Create API token, permission Object Read & Write on this bucket; Access Key ID and Secret Access Key.\n")
		f.url, err = ask("r2://account/bucket/prefix", "")
	case "http":
		f.url, err = ask("URL", "https://")
	case "consul":
		f.url, err = ask("consul://host:8500/prefix", "consul://consul.example.net:8500/ump")
	case "vault":
		f.url, err = ask("vault://host:8200/mount/prefix", "vault://vault.example.net:8200/kv/ump")
	}
	if err != nil {
		return err
	}
	secret := func(label string) (string, error) {
		ui.Printf("%s: a reference (env:NAME, file:PATH, command:CMD) or the value itself, which is kept in a file only you can read.\n", label)
		return ui.Secret(label)
	}
	switch kind {
	case "s3", "r2":
		if f.accessKey, err = secret("Access key"); err != nil {
			return err
		}
		if f.secretKey, err = secret("Secret key"); err != nil {
			return err
		}
	case "consul", "vault":
		if f.token, err = secret("Token"); err != nil {
			return err
		}
	case "http":
		if f.token, err = secret("Bearer token (empty for basic auth or none)"); err != nil {
			return err
		}
		if f.token == "" {
			if f.user, err = ask("User (empty for none)", ""); err != nil {
				return err
			}
			if f.user != "" {
				if f.password, err = secret("Password"); err != nil {
					return err
				}
			}
		}
	}
	p, err := f.pointer(nil)
	if err != nil {
		return err
	}
	b, n, err := a.checkBackend(ctx, p)
	if err != nil {
		return err
	}
	if a.home.Backend != nil && a.home.Backend.String() == b.String() {
		ui.Printf("%s answers; it is the state backend already.\n", b)
		return nil
	}
	ui.Printf("%s answers (%d files there).\n", b, n)
	// a backend that holds state is used as it is unless overwritten; an
	// empty one gets this machine's state when there is any: a migration
	move := false
	switch {
	case n > 0 && held != "":
		choice, err := ui.Choose("It holds state already, and so does this machine ("+held+"). Which one?", []string{"use the state in the backend; this machine's stays where it is, unused", "overwrite the backend with this machine's state (a migration)"}, 0)
		if err != nil {
			return err
		}
		move = choice == 1
	case n > 0:
		ui.Printf("The state there is used as it is.\n")
	case held != "":
		if move, err = ui.Confirm("Migrate this machine's state ("+held+") there?", true); err != nil {
			return err
		}
	default:
		ui.Printf("Nothing to migrate: the state starts empty there.\n")
	}
	if move {
		copied, err := home.Copy(ctx, a.home.Dir, b, n > 0)
		if err != nil {
			return err
		}
		ui.Printf("Copied %d files to %s; the copy in %s stays.\n", copied, b, config.AbbreviateHome(a.home.Dir))
	}
	if err := home.SavePointer(p); err != nil {
		return err
	}
	ui.Printf("The state backend is %s now (%s); the next command works on its mirror.\n", b, config.AbbreviateHome(home.PointerPath()))
	if os.Getenv("UMP_STATE") != "" {
		ui.Printf("UMP_STATE is set in this environment and wins over the file; point it at %s.\n", p.URL)
	}
	return nil
}
