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
	"github.com/ofzhur/ump/internal/provider/manual"
)

func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Aliases: []string{"c", "cfg"},
		Short:   "Provider accounts (isp), OS profiles (os), disk layouts (disk-layout), firewalls (firewall), users (user), userdata (userdata), keys (key) and the home (state)",
		Long: `The configuration: provider accounts under isp, the profiles that say
what a server becomes under os, the named disk layouts, firewalls,
users and userdata they refer to, UMP's own SSH key and the named keys,
and where it all lives (state).

Every entity has the same verbs with the same short forms: list (ls, l),
get (g, show), view (v, show), create (c, new, add), add (a, new,
create), edit (e), delete (d, rm, remove), remove (rm, d, delete),
update (up), default (def), test (t). The entities: isp (i, provider,
account), os (o, profile), disk-layout (dl, layout), firewall (fw),
user (u), userdata (ud), key (k), state (st); config itself is c or cfg.`,
		Example: `  ump config isp add              # the account wizard
  ump config isp view             # the accounts, secrets redacted
  ump config os create            # the profile wizard
  ump config os edit db           # a profile in your editor
  ump config disk-layout list     # the named disk layouts
  ump config firewall list        # the named firewalls
  ump config user list            # the named users
  ump config userdata list        # what runs in a freshly installed system
  ump config key list             # UMP's own key and the named keys
  ump config state view           # where the home is
  ump c o ls                      # the short form of ump config os list`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(a.newConfigISPCmd(), a.newConfigOSCmd(), a.newConfigLayoutCmd(), a.newConfigFirewallCmd(), a.newConfigUserCmd(), a.newConfigUserdataCmd(), a.newConfigStateCmd(), a.newConfigKeyCmd())
	return cmd
}

func (a *app) newConfigISPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "isp",
		Aliases: []string{"i", "provider", "account"},
		Short:   "Set up provider accounts",
		Long: `Set up provider accounts. On a terminal, ump config
isp add without arguments is a wizard that asks for a provider type, a
name and the credentials, tests them and writes them to the
configuration file. Secrets are stored as references (a file readable
only by you, an environment variable or a command), never as plaintext.`,
		Example: `  ump config isp add                  # the wizard, on a terminal
  ump config isp test                 # check every account's credentials
  ump config isp add hetzner-robot main --set login=#ws+AbCdEfGh --set password=env:HROBOT_PASSWORD
  ump config isp add hetzner-cloud hcloud --set token=file:~/.config/ump/secrets/hcloud.token
  ump config isp add ovh ovh --set endpoint=ovh-eu --set application_key=... \
      --set application_secret=command:"pass show ovh/app-secret" --set consumer_key=env:OVH_CK
  ump config isp remove main`,
	}
	cmd.AddCommand(
		a.newConfigViewCmd(),
		a.newConfigTypesCmd(),
		a.newConfigAddCmd(),
		a.newConfigRemoveCmd(),
		a.newConfigTestCmd(),
	)
	return cmd
}

func (a *app) newConfigViewCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "view",
		Aliases: verbAliases("view"),
		Short:   "Show the configuration with secrets redacted",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(a.stdout, "# %s (does not exist yet; run `ump config isp add` to create it)\n", config.AbbreviateHome(path))
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

func (a *app) newConfigTypesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "types",
		Short: "List supported provider types and their settings",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := [][]string{}
			for _, t := range provider.Types() {
				caps := make([]string, len(t.Capabilities))
				for i, c := range t.Capabilities {
					caps[i] = string(c)
				}
				capsText := strings.Join(caps, ", ")
				if capsText == "" {
					capsText = "<none>"
				}
				rows = append(rows, []string{t.Name, string(t.Kind), capsText, t.Description})
			}
			if err := writeTable(a.stdout, []string{"TYPE", "KIND", "CAPABILITIES", "DESCRIPTION"}, rows, false); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Settings, for `ump config isp add TYPE NAME --set KEY=VALUE` (VALUE may be plain, env:NAME, file:PATH or command:CMD):")
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
		Use:     "add [TYPE NAME --set KEY=VALUE ...]",
		Aliases: verbAliases("add"),
		Short:   "Add or replace an account: a wizard on a terminal, or from flags",
		Long: `Add an account. Without arguments and on a terminal, a wizard asks for
the provider type, a name and the credentials, tests them and saves
them. With TYPE and NAME, the settings are given with --set; a VALUE may
be plain text or a secret reference:

  env:NAME        an environment variable
  file:PATH       the content of a file (trailing newline stripped)
  command:CMD     the output of a command (pass, op, sops, vault, ...)

` + "`ump config types`" + ` lists the settings of every provider type. The
credentials are tested before the account is saved unless --no-test is
given. An existing account of the same name is replaced.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return usageErrorf(cmd, "either no arguments (the wizard) or TYPE NAME, got %d arguments", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if len(sets) > 0 || noTest {
					return usageErrorf(cmd, "the flags need TYPE and NAME")
				}
				return a.runWizard(cmd)
			}
			return a.configAdd(cmd, args[0], args[1], sets, noTest)
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "a setting as KEY=VALUE (repeatable)")
	cmd.Flags().BoolVar(&noTest, "no-test", false, "save without testing the credentials")
	return cmd
}

func (a *app) configAdd(cmd *cobra.Command, typeName, name string, sets []string, noTest bool) error {
	if typeName == manual.TypeName {
		return usageErrorf(cmd, "manual servers are not an account to add: ump server add NAME --ipv4 ADDRESS puts one into the inventory")
	}
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
		Aliases: verbAliases("remove"),
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
		Use:     "test [NAME...]",
		Aliases: verbAliases("test"),
		Short:   "Check that the accounts' credentials work",
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

// verbAliases are the short forms of a subcommand verb, the same under
// every entity of ump config (docs/cli.md lists them).
func verbAliases(verb string) []string {
	switch verb {
	case "list":
		return []string{"ls", "l"}
	case "get":
		return []string{"g", "show"}
	case "view":
		return []string{"v", "show"}
	case "show":
		return []string{"v", "view"}
	case "create":
		return []string{"c", "new", "add"}
	case "add":
		return []string{"a", "new", "create"}
	case "edit":
		return []string{"e"}
	case "delete":
		return []string{"d", "rm", "remove"}
	case "remove":
		return []string{"rm", "d", "delete"}
	case "update":
		return []string{"up"}
	case "default":
		return []string{"def"}
	case "test":
		return []string{"t"}
	}
	return nil
}
