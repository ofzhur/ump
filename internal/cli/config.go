package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/manual"
)

func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Aliases: []string{"c", "cfg"},
		Short:   "Provider accounts (isp), OS profiles (os), disk layouts (disk-layout), firewalls (firewall), users (user), userdata (userdata), keys (key) and the state backend (state)",
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
  ump config isp list             # the accounts: type, servers, settings, where the secrets come from
  ump config os create            # the profile wizard
  ump config os edit db           # a profile in your editor
  ump config disk-layout list     # the named disk layouts
  ump config firewall list        # the named firewalls
  ump config user list            # the named users
  ump config userdata list        # what runs in a freshly installed system
  ump config key list             # UMP's own key and the named keys
  ump config state view           # which state backend, and what the state holds
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
  ump config isp get main             # one account, its settings and where its secrets come from
  ump config isp add hetzner-robot main --set login=#ws+AbCdEfGh --set password=env:HROBOT_PASSWORD
  ump config isp add hetzner-cloud hcloud --set token=file:~/.config/ump/secrets/hcloud.token
  ump config isp add ovh ovh --set endpoint=ovh-eu --set application_key=... \
      --set application_secret=command:"pass show ovh/app-secret" --set consumer_key=env:OVH_CK
  ump config isp edit main             # that account alone, in your editor
  ump config isp remove main`,
	}
	cmd.AddCommand(
		a.newConfigISPListCmd(),
		a.newConfigISPGetCmd(),
		a.newConfigTypesCmd(),
		a.newConfigAddCmd(),
		a.newConfigEditCmd(),
		a.newConfigRemoveCmd(),
		a.newConfigTestCmd(),
	)
	return cmd
}

func (a *app) newConfigISPListCmd() *cobra.Command {
	var output string
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the accounts: type, servers, settings and where their secrets come from",
		Long: `List the configured provider accounts, one per row: the type, how
many servers the last listing found (from the cache, without asking the
provider; - when never listed), the settings that are not secret, and
for each secret where it comes from (file, env, command). -o wide spells
the secret references out; -o json and -o yaml carry everything. A
secret written as plain text in the configuration is never shown; it
reads "plain!", a hint to move it into a file or a secret store.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			format, err := parseFormat(output)
			if err != nil || format == formatName {
				return usageErrorf(cmd, "unknown output format %q (use one of table, wide, json, yaml)", output)
			}
			rows := ispRows(cfg, cache.Open(path))
			switch format {
			case formatJSON, formatYAML:
				return printEncoded(a.stdout, rows, format)
			}
			if len(rows) == 0 {
				fmt.Fprintf(a.stderr, "No accounts in %s; ump config isp add sets one up.\n", config.AbbreviateHome(path))
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, r := range rows {
				servers := "-"
				if r.Servers != nil {
					servers = strconv.Itoa(*r.Servers)
				}
				var secrets []string
				for _, k := range sortedKeys(r.Secrets) {
					ref := r.Secrets[k]
					if format != formatWide {
						ref, _, _ = strings.Cut(ref, ":")
					}
					secrets = append(secrets, k+"="+ref)
				}
				var settings []string
				for _, k := range sortedKeys(r.Settings) {
					settings = append(settings, k+"="+r.Settings[k])
				}
				table = append(table, []string{r.Name, r.Type, servers, orNone(strings.Join(settings, " ")), orNone(strings.Join(secrets, " "))})
			}
			return writeTable(a.stdout, []string{"NAME", "TYPE", "SERVERS", "SETTINGS", "SECRETS"}, table, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide (the secret references in full), json or yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line of table output")
	return cmd
}

// ispRow is an account as ump config isp list shows it.
type ispRow struct {
	Name string `json:"name" yaml:"name"`
	Type string `json:"type" yaml:"type"`
	// Servers is what the last listing found; nil when never listed.
	Servers *int `json:"servers,omitempty" yaml:"servers,omitempty"`
	// Settings are the values that are not secret.
	Settings map[string]string `json:"settings" yaml:"settings"`
	// Secrets say where each secret comes from: file:PATH, env:NAME,
	// command:CMD, or plain! for a secret written into the configuration.
	Secrets map[string]string `json:"secrets" yaml:"secrets"`
}

// newConfigISPGetCmd prints one account as it stands in the
// configuration: its type, its settings, and for each secret where it
// comes from, never a secret itself.
func (a *app) newConfigISPGetCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "get NAME",
		Aliases: verbAliases("get"),
		Short:   "Print an account: type, settings, where its secrets come from",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Accounts[args[0]]; !ok {
				return fmt.Errorf("no account %q in %s (ump config isp list)", args[0], config.AbbreviateHome(path))
			}
			format, err := parseFormat(output)
			if err != nil || format == formatName || format == formatWide {
				return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
			}
			var row ispRow
			for _, r := range ispRows(cfg, cache.Open(path)) {
				if r.Name == args[0] {
					row = r
				}
			}
			if format == formatJSON {
				return printEncoded(a.stdout, row, format)
			}
			// as the entry reads in the file: the type, then the settings
			// with the secrets as their references
			entry := map[string]string{"type": row.Type}
			for k, v := range row.Settings {
				entry[k] = v
			}
			for k, v := range row.Secrets {
				entry[k] = v
			}
			return printEncoded(a.stdout, map[string]map[string]string{row.Name: entry}, formatYAML)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "print as json (the listing's row) or yaml (the entry, the default)")
	return cmd
}

func ispRows(cfg *config.Config, store *cache.Store) []ispRow {
	rows := []ispRow{}
	for _, name := range cfg.AccountNames() {
		acct := cfg.Accounts[name]
		t, known := provider.Lookup(acct.Type)
		r := ispRow{Name: name, Type: acct.Type, Settings: map[string]string{}, Secrets: map[string]string{}}
		for k, v := range acct.Settings {
			f, isField := t.Field(k)
			secret := !known || !isField || f.Secret
			switch {
			case v.File != "":
				r.Secrets[k] = "file:" + v.File
			case v.Env != "":
				r.Secrets[k] = "env:" + v.Env
			case v.Command != "":
				r.Secrets[k] = "command:" + v.Command
			case secret:
				r.Secrets[k] = "plain!"
			default:
				r.Settings[k] = v.Plain
			}
		}
		if snap, err := store.Latest(name); err == nil && snap != nil {
			n := len(snap.Machines)
			r.Servers = &n
		}
		rows = append(rows, r)
	}
	return rows
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
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
	a.invalidateListingFor(name, 0)
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
			a.invalidateListingFor(name, 0)
			fmt.Fprintf(a.stdout, "Removed account %s (%s) from %s\n", name, acct.Type, config.AbbreviateHome(path))
			secretsDir := config.SecretsDir()
			for _, v := range acct.Settings {
				if v.File == "" {
					continue
				}
				p := config.FilePath(v.File)
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
			results := a.listAccounts(cmd.Context(), cfg, names, cache.Open(path), listOptions{})
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

// secretFileRef is how the configuration names that file: relative to
// the home (config.SecretsDir is its secrets directory).
func secretFileRef(account, key string) string {
	return "secrets/" + account + "." + key
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

// accountSet is the accounts of the configuration file as named
// entries, for edit: the same as for users and firewalls. The file is
// the one in use (--config, UMP_CONFIG), read when the command runs.
func (a *app) accountSet() *namedSet {
	path := config.ResolvePath(a.configFlag)
	return &namedSet{
		file: filepath.Base(path), key: "accounts", noun: "account",
		use:   "isp",
		dir:   func() string { return filepath.Dir(path) },
		local: true,
		check: checkAccountsFile,
		saved: func(name string) {
			names := []string{name}
			if name == "" {
				if cfg, err := config.Load(path); err == nil {
					names = cfg.AccountNames()
				}
			}
			for _, n := range names {
				a.invalidateListingFor(n, 0)
			}
		},
	}
}

// checkAccountsFile validates the accounts of a configuration file
// without resolving their secrets: names, types, known and required
// settings. ump config isp test checks the credentials themselves.
func checkAccountsFile(data []byte) error {
	cfg := config.New()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return err
	}
	for _, name := range sortedKeys(cfg.Accounts) {
		acct := cfg.Accounts[name]
		if err := config.ValidateAccountName(name); err != nil {
			return err
		}
		t, ok := provider.Lookup(acct.Type)
		if !ok || acct.Type == manual.TypeName {
			return fmt.Errorf("account %s: unknown provider type %q (known: %s)", name, acct.Type, strings.Join(provider.TypeNames(), ", "))
		}
		for _, k := range sortedKeys(acct.Settings) {
			if _, isField := t.Field(k); !isField {
				return fmt.Errorf("account %s: %s has no setting %q (settings: %s)", name, t.Name, k, strings.Join(fieldKeys(t), ", "))
			}
		}
		for _, f := range t.Fields {
			if f.Required && acct.Settings[f.Key].IsZero() {
				return fmt.Errorf("account %s: the setting %s is required for %s", name, f.Key, t.Name)
			}
		}
	}
	return nil
}

func (a *app) newConfigEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "edit [NAME]",
		Aliases: verbAliases("edit"),
		Short:   "Edit one account in your editor, or all of them without a name",
		Long: `With a name, open that account alone in your editor (its type and
settings, values plain or as secret references); when the editor
returns, the account is checked and put back into the configuration
file, the other accounts and the rest of the file untouched. Without a
name, open the whole configuration file and check every account
afterwards. The check covers the type and the settings, not the
credentials: ump config isp test NAME tries them.`,
		Example: `  ump config isp edit hcloud
  ump config isp edit`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := a.accountSet()
			if _, err := set.load(); err != nil {
				if cfg, lerr := config.Load(set.path()); lerr == nil && len(cfg.Accounts) == 0 {
					return fmt.Errorf("no accounts yet (ump config isp add)")
				}
				return err
			}
			if err := set.newEditCmd(a).RunE(cmd, args); err != nil {
				return err
			}
			if len(args) == 1 {
				fmt.Fprintf(a.stdout, "ump config isp test %s tries its credentials.\n", args[0])
			}
			return nil
		},
	}
}
