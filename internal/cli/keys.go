package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/sshkey"
)

// Named public keys: keys.yaml in the configuration directory maps a
// name to a key line, and a user's keys list names them next to ump
// (UMP's own key), local, agent, a file or a key line.

func keysPath() string { return filepath.Join(config.UserConfigDir(), "keys.yaml") }

type keysFile struct {
	Keys map[string]string `yaml:"keys"`
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// namedKeys reads keys.yaml; no file is no keys.
func namedKeys() (map[string]string, error) {
	data, err := os.ReadFile(keysPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var f keysFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", keysPath(), err)
	}
	if f.Keys == nil {
		f.Keys = map[string]string{}
	}
	return f.Keys, nil
}

func saveNamedKeys(keys map[string]string) error {
	var b strings.Builder
	b.WriteString("# Named public keys for the users' keys fields (ump config key add NAME ...).\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(keysFile{Keys: keys}); err != nil {
		return err
	}
	enc.Close()
	if err := os.MkdirAll(filepath.Dir(keysPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(keysPath(), []byte(b.String()), 0o600)
}

func (a *app) newConfigKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "key",
		Aliases: []string{"k"},
		Short:   "UMP's own SSH key (generated on first use) and the named keys of the users",
		Long: `UMP has its own SSH key pair, which rescue systems and installed
systems are reached with. It is generated on first use at the path in
the configuration (staging.key), by default ~/.config/ump/staging_ed25519,
as an ed25519 key that only you can read; it is never part of a
release. Put the public key wherever UMP should be able to log in; the
key reference "ump" in a user's keys stands for it.

More public keys are kept under names in ` + config.AbbreviateHome(keysPath()) + `:
a user's keys may name them (keys: [ump, oleg, laptop]) next to local,
agent, a file or a key line.

Every rescue system ump boots gets UMP's key, which ump needs to get in,
and the keys chosen with ump config key rescue (staging.keys), so that
you get in without ump too; they are registered at the providers that
keep keys (Hetzner Robot and Cloud, Vultr, Linode) under their names.
Without any, ump warns before each rescue and reinstall.

On a terminal, ump config key add without arguments asks for a name
and where the key comes from.`,
		Example: `  ump config key list                   # UMP's own key first, then the named ones
  ump config key show                   # UMP's own public key and its fingerprints
  ump config key add                    # asks for a name and a file, a line or the agent
  ump config key add oleg --file ~/.ssh/id_ed25519.pub
  ump config key add laptop --line 'ssh-ed25519 AAAA... oleg@laptop'
  ump config key add work --agent        # the keys ssh-add -L lists, one name each (work, work-2, ...)
  ump config key rescue oleg work        # these go into every rescue system, next to UMP's
  ump config key rescue                 # which ones do
  ump config key edit laptop             # that key alone, in your editor
  ump config key remove laptop`,
	}
	cmd.AddCommand(a.newKeyShowCmd(), a.newKeyListCmd(), a.newKeyAddCmd(), a.newKeyEditCmd(), a.newKeyRemoveCmd(), a.newKeyRescueCmd())
	return cmd
}

// showUMPKey prints UMP's own public key, generating the pair on first use.
func (a *app) showUMPKey(cmd *cobra.Command, output string) error {
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	k, path, err := a.stagingKey(cfg)
	if err != nil {
		return err
	}
	if output != "" {
		f, err := parseFormat(output)
		if err != nil || (f != formatJSON && f != formatYAML) {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
		}
		return printEncoded(a.stdout, struct {
			Path      string `json:"path" yaml:"path"`
			PublicKey string `json:"public_key" yaml:"public_key"`
			SHA256    string `json:"fingerprint_sha256" yaml:"fingerprint_sha256"`
			MD5       string `json:"fingerprint_md5" yaml:"fingerprint_md5"`
		}{Path: path, PublicKey: k.Line(), SHA256: k.FingerprintSHA256(), MD5: k.FingerprintMD5()}, f)
	}
	fmt.Fprintf(a.stdout, "Path:         %s\n", config.AbbreviateHome(path))
	fmt.Fprintf(a.stdout, "Public key:   %s\n", k.Line())
	fmt.Fprintf(a.stdout, "Fingerprint:  %s (MD5 %s)\n", k.FingerprintSHA256(), k.FingerprintMD5())
	return nil
}

func (a *app) newKeyShowCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "show",
		Aliases: verbAliases("show"),
		Short:   "Show UMP's own public key and its fingerprints",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.showUMPKey(cmd, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "print as json or yaml")
	return cmd
}

// addKeys keeps the key lines under name (name-2, name-3, ... for the
// ones after the first) and reports each.
func (a *app) addKeys(name string, lines []string) error {
	var keyLines []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if !profile.IsKeyLine(l) {
			return fmt.Errorf("%q is not a public key line", truncateKey(l))
		}
		keyLines = append(keyLines, l)
	}
	if len(keyLines) == 0 {
		return errors.New("no public key found")
	}
	keys, err := namedKeys()
	if err != nil {
		return err
	}
	for i, l := range keyLines {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s-%d", name, i+1)
		}
		if _, exists := keys[n]; exists {
			return fmt.Errorf("key %s exists already (ump config key remove %s first)", n, n)
		}
		keys[n] = l
		fmt.Fprintf(a.stdout, "Added the key %s (%s %s).\n", n, keyType(l), keyComment(l))
	}
	return saveNamedKeys(keys)
}

func checkKeyName(name string) error {
	if err := config.ValidateAccountName(name); err != nil || name == "ump" || name == "local" || name == "agent" {
		return fmt.Errorf("key name %q: lower-case letters, digits, '-', '_' and '.', and not ump, local or agent", name)
	}
	return nil
}

func (a *app) newKeyListCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the named keys (and ump, UMP's own)",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			keys, err := namedKeys()
			if err != nil {
				return err
			}
			rows := [][]string{}
			if k, path, err := a.stagingKey(cfg); err == nil {
				rows = append(rows, []string{"ump", keyType(k.Line()), keyComment(k.Line()), k.FingerprintSHA256(), "UMP's own key (" + config.AbbreviateHome(path) + "), in every rescue system"})
			}
			rescue := map[string]bool{}
			for _, r := range cfg.Staging.Keys {
				rescue[r] = true
			}
			names := make([]string, 0, len(keys))
			for n := range keys {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fp := "-"
				if k, err := sshkey.Parse(keys[n]); err == nil {
					fp = k.FingerprintSHA256()
				}
				note := ""
				if rescue[n] {
					note = "in every rescue system"
				}
				rows = append(rows, []string{n, keyType(keys[n]), keyComment(keys[n]), fp, note})
			}
			return writeTable(a.stdout, []string{"NAME", "TYPE", "COMMENT", "FINGERPRINT", "NOTE"}, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newKeyAddCmd() *cobra.Command {
	var file, line string
	var agent bool
	cmd := &cobra.Command{
		Use:     "add [NAME] (--file FILE | --line 'ssh-... AAAA...' | --agent)",
		Aliases: verbAliases("add"),
		Short:   "Keep a public key under a name (asks on a terminal)",
		Args:    usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			var lines []string
			if len(args) > 0 {
				name = args[0]
			} else {
				ui := a.resolverUI(cmd)
				if ui == nil {
					return usageErrorf(cmd, "a key name is needed without a terminal: ump config key add NAME --file FILE")
				}
				var err error
				if name, err = ui.Line("Name for the key", ""); err != nil {
					return err
				}
				if file == "" && line == "" && !agent {
					how, err := ui.Choose("Where from?", []string{"a public key file", "a public key line", "the keys of the ssh-agent"}, 0)
					if err != nil {
						return err
					}
					switch how {
					case 0:
						if file, err = ui.Line("Public key file", "~/.ssh/id_ed25519.pub"); err != nil {
							return err
						}
					case 1:
						if line, err = ui.Line("Public key line (ssh-ed25519 AAAA... comment)", ""); err != nil {
							return err
						}
					case 2:
						agent = true
					}
				}
			}
			if err := checkKeyName(name); err != nil {
				return usageErrorf(cmd, "%v", err)
			}
			switch {
			case file != "" && line == "" && !agent:
				data, err := os.ReadFile(expandHome(file))
				if err != nil {
					return err
				}
				lines = strings.Split(string(data), "\n")
			case line != "" && file == "" && !agent:
				lines = []string{line}
			case agent && file == "" && line == "":
				var err error
				if lines, err = a.agentKeys(); err != nil {
					return err
				}
			default:
				return usageErrorf(cmd, "give exactly one of --file, --line or --agent")
			}
			return a.addKeys(name, lines)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "a public key file (one key per line)")
	cmd.Flags().StringVar(&line, "line", "", "the public key line itself")
	cmd.Flags().BoolVar(&agent, "agent", false, "every key the ssh-agent holds (ssh-add -L)")
	return cmd
}

// keySet is keys.yaml as a file of named entries, for edit: the same
// as for users and firewalls, with nothing published to download.
var keySet = &namedSet{
	file: "keys.yaml", key: "keys", noun: "key",
	use:   "key",
	dir:   config.UserConfigDir,
	local: true,
	check: checkKeysFile,
}

// checkKeysFile validates keys.yaml: names and public key lines.
func checkKeysFile(data []byte) error {
	var f keysFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return err
	}
	for _, n := range sortedKeys(f.Keys) {
		if err := checkKeyName(n); err != nil {
			return err
		}
		if !profile.IsKeyLine(strings.TrimSpace(f.Keys[n])) {
			return fmt.Errorf("key %s: %q is not a public key line", n, truncateKey(f.Keys[n]))
		}
	}
	return nil
}

func (a *app) newKeyEditCmd() *cobra.Command {
	cmd := keySet.newEditCmd(a)
	inner := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(keysPath()); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no named keys yet (ump config key add NAME --file FILE)")
		}
		return inner(cmd, args)
	}
	return cmd
}

func (a *app) newKeyRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Aliases: verbAliases("remove"),
		Short:   "Forget a named key",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := namedKeys()
			if err != nil {
				return err
			}
			if _, ok := keys[args[0]]; !ok {
				return fmt.Errorf("no key %q (ump config key list)", args[0])
			}
			delete(keys, args[0])
			if err := saveNamedKeys(keys); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Removed the key %s.\n", args[0])
			return nil
		},
	}
}

func keyType(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return "-"
	}
	return f[0]
}

func keyComment(line string) string {
	f := strings.Fields(line)
	if len(f) < 3 {
		return "-"
	}
	return strings.Join(f[2:], " ")
}

func truncateKey(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func (a *app) newKeyRescueCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "rescue [REF...]",
		Short: "Choose the keys that go into every rescue system next to UMP's",
		Long: `Without arguments, show the keys that go into every rescue system ump
boots, next to UMP's own key, which is always there. With arguments, set
them: key references as in a user's keys (a named key from ump config
key list, local, agent, a file, or a key line), saved as staging.keys in
the configuration. They are registered at the providers that keep keys
(Hetzner Robot and Cloud, Vultr, Linode) under their names. --clear
leaves UMP's key alone again, and ump warns before each rescue and
reinstall.`,
		Example: `  ump config key rescue oleg work
  ump config key rescue local
  ump config key rescue --clear`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if clear && len(args) > 0 {
				return usageErrorf(cmd, "--clear takes no key references")
			}
			if !clear && len(args) == 0 {
				if len(cfg.Staging.Keys) == 0 {
					fmt.Fprintln(a.stdout, "Only UMP's own key goes into rescue systems; ump config key rescue NAME... adds yours.")
					return nil
				}
				_, labels, _, err := a.rescueKeys(cfg, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Rescue systems get: %s\n", strings.Join(labels, ", "))
				return nil
			}
			refs := dedupe(append([]string{}, args...))
			for _, r := range refs {
				if r == "ump" {
					return usageErrorf(cmd, "ump is always in rescue systems; name your own keys")
				}
			}
			old := cfg.Staging.Keys
			cfg.Staging.Keys = refs
			if clear {
				cfg.Staging.Keys = nil
			}
			// every reference must resolve now, not at the next rescue
			if _, _, _, err := a.rescueKeys(cfg, nil); err != nil {
				cfg.Staging.Keys = old
				return err
			}
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			if clear {
				fmt.Fprintln(a.stdout, "Only UMP's own key goes into rescue systems now.")
				return nil
			}
			fmt.Fprintf(a.stdout, "Rescue systems get UMP's key and %s (saved as staging.keys in %s).\n", strings.Join(refs, ", "), config.AbbreviateHome(path))
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "no keys but UMP's")
	return cmd
}
