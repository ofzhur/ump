package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Set up provider accounts (interactive wizard)",
		Long: `Set up provider accounts. Without a subcommand, ump config starts an
interactive wizard that asks for a provider type, a name and the
credentials, tests them and writes them to the configuration file.
Secrets are stored as references (a file readable only by you, an
environment variable or a command), never as plaintext.

Without a terminal, use ` + "`ump config add`" + ` instead.`,
		Example: `  ump config                      # the wizard
  ump config view                 # the configuration, secrets redacted
  ump config test                 # check every account's credentials
  ump config add hetzner-robot main --set login=#ws+AbCdEfGh --set password=env:HROBOT_PASSWORD
  ump config add hetzner-cloud hcloud --set token=file:~/.config/ump/secrets/hcloud.token
  ump config add ovh ovh --set endpoint=ovh-eu --set application_key=... \
      --set application_secret=command:"pass show ovh/app-secret" --set consumer_key=env:OVH_CK
  ump config remove main`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runWizard(cmd)
		},
	}
	cmd.AddCommand(
		a.newConfigViewCmd(),
		a.newConfigPathCmd(),
		a.newConfigTypesCmd(),
		a.newConfigAddCmd(),
		a.newConfigRemoveCmd(),
		a.newConfigTestCmd(),
	)
	return cmd
}

func (a *app) newConfigViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "view",
		Short: "Show the configuration with secrets redacted",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(a.stdout, "# %s (does not exist yet; run `ump config` to create it)\n", config.AbbreviateHome(path))
			} else {
				fmt.Fprintf(a.stdout, "# %s\n", config.AbbreviateHome(path))
			}
			data, err := config.MarshalYAML(redactConfig(cfg))
			if err != nil {
				return err
			}
			_, err = a.stdout.Write(data)
			return err
		},
	}
}

// redactConfig returns a copy of cfg in which plaintext secrets are
// replaced by "<redacted>". Secret references are kept: they are not
// secret themselves.
func redactConfig(cfg *config.Config) *config.Config {
	out := &config.Config{Defaults: cfg.Defaults, Accounts: map[string]config.Account{}, Rest: cfg.Rest}
	for name, acct := range cfg.Accounts {
		t, known := provider.Lookup(acct.Type)
		copied := config.Account{Type: acct.Type, Settings: map[string]config.Value{}}
		for k, v := range acct.Settings {
			if !v.IsRef() {
				f, isField := t.Field(k)
				if !known || !isField || f.Secret {
					v = config.PlainValue("<redacted>")
				}
			}
			copied.Settings[k] = v
		}
		out.Accounts[name] = copied
	}
	return out
}

func (a *app) newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(a.stdout, config.ResolvePath(a.configFlag))
			return nil
		},
	}
}

func (a *app) newConfigTypesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "types",
		Short: "List supported provider types and their settings",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			tw := tabwriter.NewWriter(a.stdout, 0, 8, 3, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tKIND\tDESCRIPTION")
			for _, t := range provider.Types() {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, t.Kind, t.Description)
			}
			tw.Flush()
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Settings, for `ump config add TYPE NAME --set KEY=VALUE` (VALUE may be plain, env:NAME, file:PATH or command:CMD):")
			for _, t := range provider.Types() {
				fmt.Fprintf(a.stdout, "\n  %s\n", t.Name)
				tw := tabwriter.NewWriter(a.stdout, 0, 8, 3, ' ', 0)
				for _, f := range t.Fields {
					var attrs []string
					if f.Required {
						attrs = append(attrs, "required")
					} else {
						attrs = append(attrs, "optional")
					}
					if f.Secret {
						attrs = append(attrs, "secret")
					}
					help := f.Help
					if len(f.Choices) > 0 {
						help = strings.TrimSpace(help + " [" + strings.Join(f.Choices, "|") + "]")
					}
					if f.Default != "" {
						help = strings.TrimSpace(help + " (default " + f.Default + ")")
					}
					if help == "" {
						fmt.Fprintf(tw, "    %s\t%s\n", f.Key, strings.Join(attrs, ", "))
						continue
					}
					fmt.Fprintf(tw, "    %s\t%s\t%s\n", f.Key, strings.Join(attrs, ", "), help)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (a *app) newConfigAddCmd() *cobra.Command {
	var (
		sets   []string
		noTest bool
	)
	cmd := &cobra.Command{
		Use:   "add TYPE NAME --set KEY=VALUE ...",
		Short: "Add or replace an account without the wizard",
		Long: `Add an account non-interactively. Settings are given with --set; a
VALUE may be plain text or a secret reference:

  env:NAME        an environment variable
  file:PATH       the content of a file (trailing newline stripped)
  command:CMD     the output of a command (pass, op, sops, vault, ...)

` + "`ump config types`" + ` lists the settings of every provider type. The
credentials are tested before the account is saved unless --no-test is
given. An existing account of the same name is replaced.`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.configAdd(cmd, args[0], args[1], sets, noTest)
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "a setting as KEY=VALUE (repeatable)")
	cmd.Flags().BoolVar(&noTest, "no-test", false, "save without testing the credentials")
	return cmd
}

func (a *app) configAdd(cmd *cobra.Command, typeName, name string, sets []string, noTest bool) error {
	t, ok := provider.Lookup(typeName)
	if !ok {
		return usageErrorf(cmd, "unknown provider type %q (known: %s)", typeName, strings.Join(provider.TypeNames(), ", "))
	}
	if err := config.ValidateAccountName(name); err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	acct := config.Account{Type: t.Name, Settings: map[string]config.Value{}}
	for _, s := range sets {
		k, v, found := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		if !found || k == "" {
			return usageErrorf(cmd, "--set needs KEY=VALUE, got %q", s)
		}
		if _, isField := t.Field(k); !isField {
			return usageErrorf(cmd, "%s has no setting %q (settings: %s)", t.Name, k, strings.Join(fieldKeys(t), ", "))
		}
		acct.Settings[k] = config.ParseValue(v)
	}
	values, err := acct.Resolved()
	if err != nil {
		return err
	}
	if errs := t.Validate(values); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return usageErrorf(cmd, "%s", strings.Join(msgs, "; "))
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	var tested string
	if !noTest {
		n, err := a.testSettings(cmd, t, name, values)
		if err != nil {
			return fmt.Errorf("not saved: %w (use --no-test to save anyway)", err)
		}
		tested = " (" + plural(n, "server") + ")"
	}
	_, replaced := cfg.Accounts[name]
	cfg.Accounts[name] = acct
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	verb := "Added"
	if replaced {
		verb = "Replaced"
	}
	fmt.Fprintf(a.stdout, "%s account %s (%s) in %s%s\n", verb, name, t.Name, config.AbbreviateHome(path), tested)
	return nil
}

func fieldKeys(t provider.Type) []string {
	keys := make([]string, 0, len(t.Fields))
	for _, f := range t.Fields {
		keys = append(keys, f.Key)
	}
	return keys
}

// testSettings builds a provider from resolved settings and lists its
// machines once. It returns how many were found.
func (a *app) testSettings(cmd *cobra.Command, t provider.Type, name string, values map[string]string) (int, error) {
	p, err := provider.Build(t.Name, name, values, a.providerOptions())
	if err != nil {
		return 0, err
	}
	inv, ok := p.(provider.Inventory)
	if !ok {
		return 0, fmt.Errorf("provider type %s cannot list servers", t.Name)
	}
	ms, err := inv.List(cmd.Context())
	if err != nil {
		return 0, err
	}
	return len(ms), nil
}

func (a *app) newConfigRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove an account and the secret files the wizard created for it",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			acct, ok := cfg.Accounts[name]
			if !ok {
				return fmt.Errorf("account %q is not configured (configured: %s)", name, joinOrNone(cfg.AccountNames()))
			}
			delete(cfg.Accounts, name)
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Removed account %s (%s) from %s\n", name, acct.Type, config.AbbreviateHome(path))
			secretsDir := config.SecretsDir()
			for _, v := range acct.Settings {
				if v.File == "" {
					continue
				}
				p := config.ExpandHome(v.File)
				if !strings.HasPrefix(p, secretsDir+string(os.PathSeparator)) {
					continue
				}
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(a.stderr, "ump: could not remove %s: %v\n", config.AbbreviateHome(p), err)
					continue
				}
				fmt.Fprintf(a.stdout, "Removed secret file %s\n", config.AbbreviateHome(p))
			}
			return nil
		},
	}
}

func (a *app) newConfigTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test [NAME...]",
		Short: "Check that the accounts' credentials work",
		Long: `Resolve each account's secrets, call its provider API once and report
the result. All accounts are tested when no name is given.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if len(cfg.Accounts) == 0 {
				return fmt.Errorf("no accounts configured in %s; run `ump config` to add one", config.AbbreviateHome(path))
			}
			names := args
			if len(names) == 0 {
				names, err = a.selectAccounts(cmd, cfg)
				if err != nil {
					return err
				}
			}
			for _, n := range names {
				if _, ok := cfg.Accounts[n]; !ok {
					return usageErrorf(cmd, "account %q is not configured (configured: %s)", n, joinOrNone(cfg.AccountNames()))
				}
			}
			sort.Strings(names)
			results := a.listAccounts(cmd.Context(), cfg, names, cache.Open(path))
			tw := tabwriter.NewWriter(a.stdout, 0, 8, 3, ' ', 0)
			fmt.Fprintln(tw, "ACCOUNT\tTYPE\tRESULT")
			failed := 0
			for _, r := range results {
				if r.err != nil {
					failed++
					msg := strings.TrimPrefix(r.err.Error(), "account "+r.account+": ")
					fmt.Fprintf(tw, "%s\t%s\terror: %s\n", r.account, r.typ, msg)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\tok, %s\n", r.account, r.typ, plural(len(r.machines), "server"))
			}
			tw.Flush()
			if failed > 0 {
				return withExit(exitProvider, fmt.Errorf("%d of %d accounts failed", failed, len(results)))
			}
			return nil
		},
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// secretFilePath is where the wizard stores one secret of an account.
func secretFilePath(account, key string) string {
	return filepath.Join(config.SecretsDir(), account+"."+key)
}
