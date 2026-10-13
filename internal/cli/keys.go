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
	var output string
	cmd := &cobra.Command{
		Use:   "key",
		Short: "UMP's own SSH key (generated on first use) and the named keys of the users (interactive wizard)",
		Long: `UMP has its own SSH key pair, which rescue systems and installed
systems are reached with. It is generated on first use at the path in
the configuration (staging.key), by default ~/.config/ump/staging_ed25519,
as an ed25519 key that only you can read; it is never part of a
release. Put the public key wherever UMP should be able to log in; the
key reference "ump" in a user's keys stands for it.

More public keys are kept under names in ` + config.AbbreviateHome(keysPath()) + `:
a user's keys may name them (keys: [ump, oleg, laptop]) next to local,
agent, a file or a key line.

Without a subcommand and on a terminal, ump config key is a small
wizard over the subcommands below; without a terminal, or with -o, it
prints UMP's own public key (ump config key show does that always).`,
		Example: `  ump config key                        # the wizard
  ump config key show                   # UMP's own public key and its fingerprints
  ump config key add oleg --file ~/.ssh/id_ed25519.pub
  ump config key add laptop --line 'ssh-ed25519 AAAA... oleg@laptop'
  ump config key add work --agent        # the keys ssh-add -L lists, one name each (work, work-2, ...)
  ump config key list
  ump config key remove laptop`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				if ui := a.resolverUI(cmd); ui != nil {
					return a.keysWizard(cmd, ui)
				}
			}
			return a.showUMPKey(cmd, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "print UMP's own key as json or yaml")
	cmd.AddCommand(a.newKeyShowCmd(), a.newKeyListCmd(), a.newKeyAddCmd(), a.newKeyRemoveCmd())
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
		Use:   "show",
		Short: "Show UMP's own public key and its fingerprints",
		Args:  usageArgs(cobra.NoArgs),
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

// keysWizard is ump config key on a terminal.
func (a *app) keysWizard(cmd *cobra.Command, ui *prompter) error {
	for {
		if err := a.showUMPKey(cmd, ""); err != nil {
			return err
		}
		keys, err := namedKeys()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(keys))
		for n := range keys {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			ui.Printf("No named keys yet in %s; users refer to keys as ump, local, agent, a name from here, a file or a key line.\n", config.AbbreviateHome(keysPath()))
		} else {
			ui.Printf("Named keys in %s:\n", config.AbbreviateHome(keysPath()))
			for _, n := range names {
				ui.Printf("  %-12s %s %s\n", n, keyType(keys[n]), keyComment(keys[n]))
			}
		}
		choice, err := ui.Choose("What now?", []string{"add a key from a file", "add a key as a line", "add the keys of the ssh-agent", "remove a key"}, -1)
		if err != nil {
			return err
		}
		switch choice {
		case 0, 1, 2:
			name, err := ui.Line("Name for the key", "")
			if err != nil {
				return err
			}
			if err := checkKeyName(name); err != nil {
				ui.Printf("%v\n", err)
				break
			}
			var lines []string
			switch choice {
			case 0:
				file, err := ui.Line("Public key file", "~/.ssh/id_ed25519.pub")
				if err != nil {
					return err
				}
				data, err := os.ReadFile(expandHome(file))
				if err != nil {
					ui.Printf("%v\n", err)
					break
				}
				lines = strings.Split(string(data), "\n")
			case 1:
				line, err := ui.Line("Public key line (ssh-ed25519 AAAA... comment)", "")
				if err != nil {
					return err
				}
				lines = []string{line}
			case 2:
				if lines, err = a.agentKeys(); err != nil {
					ui.Printf("%v\n", err)
					break
				}
			}
			if len(lines) > 0 {
				if err := a.addKeys(name, lines); err != nil {
					ui.Printf("%v\n", err)
				}
			}
		case 3:
			if len(names) == 0 {
				ui.Printf("nothing to remove\n")
				break
			}
			i, err := ui.Choose("Which key?", names, -1)
			if err != nil {
				return err
			}
			delete(keys, names[i])
			if err := saveNamedKeys(keys); err != nil {
				return err
			}
			ui.Printf("Removed the key %s.\n", names[i])
		}
		again, err := ui.Confirm("Anything else?", false)
		if err != nil || !again {
			return err
		}
	}
}

func (a *app) newKeyListCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
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
				rows = append(rows, []string{"ump", keyType(k.Line()), keyComment(k.Line()), k.FingerprintSHA256(), "UMP's own key (" + config.AbbreviateHome(path) + ")"})
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
				rows = append(rows, []string{n, keyType(keys[n]), keyComment(keys[n]), fp, ""})
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
		Use:   "add NAME (--file FILE | --line 'ssh-... AAAA...' | --agent)",
		Short: "Keep a public key under a name",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := checkKeyName(name); err != nil {
				return usageErrorf(cmd, "%v", err)
			}
			var lines []string
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

func (a *app) newKeyRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm", "delete"},
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
