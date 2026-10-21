package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
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
  ump config key get ump                # UMP's own public key and its fingerprints (also: show)
  ump config key get oleg -o json       # a named key, as data
  ump config key roll                   # a new key pair for UMP, on every server ump installed
  ump config key add                    # asks for a name and a file, a line or the agent
  ump config key add oleg --file ~/.ssh/id_ed25519.pub
  ump config key add laptop --line 'ssh-ed25519 AAAA... oleg@laptop'
  ump config key add work --agent        # the keys ssh-add -L lists, one name each (work, work-2, ...)
  ump config key rescue oleg work        # these go into every rescue system, next to UMP's
  ump config key rescue                 # which ones do
  ump config key edit laptop             # that key alone, in your editor
  ump config key remove laptop`,
	}
	cmd.AddCommand(a.newKeyListCmd(), a.newKeyGetCmd(), a.newKeyAddCmd(), a.newKeyEditCmd(), a.newKeyRemoveCmd(), a.newKeyRollCmd(), a.newKeyRescueCmd())
	return cmd
}

// keyInfo is one key as get and list print it: UMP's own (with its
// path) or a named one.
type keyInfo struct {
	Name      string `json:"name" yaml:"name"`
	Path      string `json:"path,omitempty" yaml:"path,omitempty"`
	Type      string `json:"type" yaml:"type"`
	Comment   string `json:"comment,omitempty" yaml:"comment,omitempty"`
	PublicKey string `json:"public_key" yaml:"public_key"`
	SHA256    string `json:"fingerprint_sha256" yaml:"fingerprint_sha256"`
	MD5       string `json:"fingerprint_md5" yaml:"fingerprint_md5"`
	// Rescue says that the key goes into every rescue system (UMP's own
	// always does, the named ones chosen with ump config key rescue).
	Rescue bool `json:"rescue" yaml:"rescue"`
}

// umpKeyInfo is UMP's own key, generated on first use.
func (a *app) umpKeyInfo(cfg *config.Config) (keyInfo, error) {
	k, path, err := a.stagingKey(cfg)
	if err != nil {
		return keyInfo{}, err
	}
	return keyInfo{Name: "ump", Path: path, Type: keyType(k.Line()), Comment: commentOf(k.Line()), PublicKey: k.Line(), SHA256: k.FingerprintSHA256(), MD5: k.FingerprintMD5(), Rescue: true}, nil
}

// namedKeyInfo is a key of keys.yaml; a line that does not parse keeps
// its text with "-" for the fingerprints.
func namedKeyInfo(cfg *config.Config, name, line string) keyInfo {
	info := keyInfo{Name: name, Type: keyType(line), Comment: commentOf(line), PublicKey: line, SHA256: "-", MD5: "-"}
	if k, err := sshkey.Parse(line); err == nil {
		info.SHA256, info.MD5 = k.FingerprintSHA256(), k.FingerprintMD5()
	}
	for _, r := range cfg.Staging.Keys {
		if r == name {
			info.Rescue = true
		}
	}
	return info
}

func (a *app) printKey(cmd *cobra.Command, info keyInfo, output string) error {
	if output != "" {
		f, err := parseFormat(output)
		if err != nil || (f != formatJSON && f != formatYAML) {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
		}
		return printEncoded(a.stdout, info, f)
	}
	fmt.Fprintf(a.stdout, "Name:         %s\n", info.Name)
	if info.Path != "" {
		fmt.Fprintf(a.stdout, "Path:         %s\n", config.AbbreviateHome(info.Path))
	}
	fmt.Fprintf(a.stdout, "Public key:   %s\n", info.PublicKey)
	fmt.Fprintf(a.stdout, "Fingerprint:  %s (MD5 %s)\n", info.SHA256, info.MD5)
	return nil
}

func (a *app) newKeyGetCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "get [NAME]",
		Aliases: verbAliases("get"),
		Short:   "Print a key: UMP's own (ump, the default) or a named one",
		Long: `Print one public key with its fingerprints: ump, UMP's own key pair
(generated on first use; with the path of the private key), or a key
kept under a name with ump config key add. The public key line is what
goes into an authorized_keys file, a provider's key list or a git
host's deploy keys; -o json or -o yaml give it as data
(ump config key get ump -o json | jq -r .public_key).`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			name := "ump"
			if len(args) == 1 {
				name = args[0]
			}
			if name == "ump" {
				info, err := a.umpKeyInfo(cfg)
				if err != nil {
					return err
				}
				return a.printKey(cmd, info, output)
			}
			keys, err := namedKeys()
			if err != nil {
				return err
			}
			line, ok := keys[name]
			if !ok {
				return fmt.Errorf("no key %q in %s (ump config key list)", name, config.AbbreviateHome(keysPath()))
			}
			return a.printKey(cmd, namedKeyInfo(cfg, name, line), output)
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
	var output string
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the keys: ump (UMP's own) and the named ones",
		Long: `List UMP's own key (ump) and the keys kept under names, with type,
comment and fingerprint, and which of them go into every rescue system.
-o wide adds the public key lines and the path of UMP's key; -o json
and -o yaml carry everything.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			format, err := parseFormat(output)
			if err != nil || format == formatName {
				return usageErrorf(cmd, "unknown output format %q (use one of table, wide, json, yaml)", output)
			}
			keys, err := namedKeys()
			if err != nil {
				return err
			}
			var infos []keyInfo
			if info, err := a.umpKeyInfo(cfg); err == nil {
				infos = append(infos, info)
			}
			for _, n := range sortedKeys(keys) {
				infos = append(infos, namedKeyInfo(cfg, n, keys[n]))
			}
			switch format {
			case formatJSON, formatYAML:
				return printEncoded(a.stdout, infos, format)
			}
			headers := []string{"NAME", "TYPE", "COMMENT", "FINGERPRINT", "NOTE"}
			if format == formatWide {
				headers = append(headers, "PUBLIC KEY")
			}
			rows := make([][]string, 0, len(infos))
			for _, k := range infos {
				note := ""
				switch {
				case k.Path != "":
					note = "UMP's own key (" + config.AbbreviateHome(k.Path) + "), in every rescue system"
				case k.Rescue:
					note = "in every rescue system"
				}
				row := []string{k.Name, k.Type, orDash(k.Comment), k.SHA256, note}
				if format == formatWide {
					row = append(row, k.PublicKey)
				}
				rows = append(rows, row)
			}
			return writeTable(a.stdout, headers, rows, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide (with the public key lines), json or yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

// commentOf is the comment of a key line, empty without one.
func commentOf(line string) string {
	if c := keyComment(line); c != "-" {
		return c
	}
	return ""
}

func orDash(s string) string {
	if s == "" || s == "-" {
		return "-"
	}
	return s
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
		if len(args) == 1 && args[0] == "ump" {
			return fmt.Errorf("ump is UMP's own key pair, kept as files and not an entry of %s, so it cannot be edited: ump config key get ump shows it, ump config key roll replaces it with a new one", config.AbbreviateHome(keysPath()))
		}
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

// newKeyRollCmd replaces UMP's own key pair with a new one and puts the
// new public key onto every server ump installed, taking the old one
// away there.
func (a *app) newKeyRollCmd() *cobra.Command {
	var yes, noServers, dropOld bool
	cmd := &cobra.Command{
		Use:   "roll [--no-servers] [--drop-old] [--yes]",
		Short: "Replace UMP's own key pair with a new one, on every server ump installed",
		Long: `Generate a new key pair for UMP and retire the current one, for when
it may have leaked or someone who could read it has left. The new key
goes into every server ump installed, over SSH with the old key as the
administrator: through sudo, the old key becomes the new one in every
account's authorized_keys that holds it; a server that cannot be
reached keeps the old key and is named, so that you add the new key by
hand (ump config key get ump). The old
pair is kept next to the new one as staging_ed25519.old, for exactly
that, and deleted once every server has the new key. While it is still
there, the next roll refuses, since it would lose the only key those
servers take; --drop-old deletes it when you are done with it.

--no-servers rolls the key alone and touches no server. The providers
that keep keys (Hetzner Robot and Cloud, Vultr, Linode) still have the
old public key registered under its name: a leaked key is removed there
by hand, the new one is registered at the next rescue. The keys you
keep under names are not affected. Asks for confirmation on a terminal;
without one, --yes is required.`,
		Example: `  ump config key roll
  ump config key roll --no-servers
  ump config key roll --drop-old`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			old, err := a.umpKeyInfo(cfg)
			if err != nil {
				return err
			}
			path := old.Path
			oldPath := path + ".old"
			if _, err := os.Stat(oldPath); err == nil && !dropOld {
				return fmt.Errorf("the key of a previous roll is still at %s, for servers that did not get the new key; a new roll would delete it. Put the current key on those servers (ump config key get ump), then roll with --drop-old", config.AbbreviateHome(oldPath))
			}
			ctx := cmd.Context()
			failed := 0
			// the servers first, so that the question can name them
			type target struct {
				m     provider.Machine
				admin string
			}
			var targets []target
			if !noServers {
				doc, err := a.loadState(ctx, cfg)
				if err != nil {
					return err
				}
				var machines []provider.Machine
				var total int
				machines, _, failed, total, err = a.listedMachines(cmd)
				if err != nil {
					return err
				}
				if failed > 0 {
					fmt.Fprintf(a.stderr, "ump: %d of %d accounts could not be listed; their servers keep the old key\n", failed, total)
				}
				for _, m := range machines {
					s := doc.Servers[m.Ref()]
					if s.Installed == nil || s.Installed.Admin == "" {
						continue
					}
					if m.IPv4 == "" && m.IPv6 == "" {
						failed++ // may hold the old key, so it stays
						continue
					}
					targets = append(targets, target{m, s.Installed.Admin})
				}
			}
			q := fmt.Sprintf("Replace UMP's key %s with a new one? %s get the new key over SSH and lose the old one; the old pair is kept as %s.old.", old.SHA256, plural(len(targets), "installed server"), config.AbbreviateHome(old.Path))
			if err := a.confirm(cmd, yes, q); err != nil {
				return err
			}
			// the new pair, then the swap: the old one stays as .old
			newKey, err := sshkey.Generate(path+".new", sshkey.DefaultComment())
			if err != nil {
				return err
			}
			for _, f := range []string{oldPath, oldPath + ".pub"} {
				os.Remove(f)
			}
			if err := os.Rename(path, oldPath); err != nil {
				return err
			}
			if err := os.Rename(path+".pub", oldPath+".pub"); err != nil {
				return err
			}
			if err := os.Rename(path+".new", path); err != nil {
				return err
			}
			if err := os.Rename(path+".new.pub", path+".pub"); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Rolled UMP's key: %s is the new one (was %s, kept at %s).\n", newKey.FingerprintSHA256(), old.SHA256, config.AbbreviateHome(oldPath))
			// the old pair goes once nothing can still need it
			dropKept := func() {
				if noServers {
					return
				}
				if failed > 0 {
					fmt.Fprintf(a.stdout, "The old pair stays at %s: %s could not be checked and may still take only the old key.\n", config.AbbreviateHome(oldPath), plural(failed, "account or server"))
					return
				}
				for _, f := range []string{oldPath, oldPath + ".pub"} {
					os.Remove(f)
				}
				fmt.Fprintf(a.stdout, "Every installed server has the new key; the old pair is deleted.\n")
			}
			if len(targets) == 0 {
				if !noServers {
					fmt.Fprintln(a.stdout, "No installed server to update.")
				}
				dropKept()
				return nil
			}
			// every installed server: the old key becomes the new one in
			// every authorized_keys that holds it, then a check that the
			// new key gets in
			swap := rollScript(old.PublicKey, newKey.Line())
			done, kept := 0, 0
			for _, t := range targets {
				addr := t.m.IPv4
				if addr == "" {
					addr = t.m.IPv6
				}
				var outb, errb bytes.Buffer
				argv := withSSHOptions(sshArgs(t.admin, addr, []string{oldPath}, true, true, []string{swap}), batchOptions(10*time.Second)...)
				code, err := a.sshExec(ctx, argv, nil, &outb, &errb)
				if err != nil || code != 0 {
					kept++
					fmt.Fprintf(a.stdout, "  %s (%s): not reached as %s with the old key, keeps it; add the new key by hand: %s\n", t.m.Name, addr, t.admin, strings.TrimSpace(errb.String()))
					continue
				}
				var files []string
				noSudo := false
				for _, l := range strings.Split(outb.String(), "\n") {
					if f, ok := strings.CutPrefix(l, "rolled "); ok {
						files = append(files, f)
					} else if l == "nosudo" {
						noSudo = true
					}
				}
				argv = withSSHOptions(sshArgs(t.admin, addr, []string{path}, true, true, []string{"id -u; echo rolled"}), batchOptions(10*time.Second)...)
				if code, err := a.sshExec(ctx, argv, nil, io.Discard, io.Discard); err != nil || code != 0 {
					kept++
					fmt.Fprintf(a.stdout, "  %s (%s): the new key was added, but does not open %s@%s; the old key may still be there\n", t.m.Name, addr, t.admin, addr)
					continue
				}
				where := "the old one is gone"
				if len(files) > 0 {
					where = "the old one is replaced in " + strings.Join(files, ", ")
				}
				if noSudo {
					kept++
					fmt.Fprintf(a.stdout, "  %s (%s): the new key opens %s, %s; %s has no sudo without a password, so other accounts there may still hold the old key\n", t.m.Name, addr, t.admin, where, t.admin)
					continue
				}
				done++
				fmt.Fprintf(a.stdout, "  %s (%s): the new key opens %s, %s\n", t.m.Name, addr, t.admin, where)
			}
			fmt.Fprintf(a.stdout, "%s with the new key", plural(done, "server"))
			if kept > 0 {
				fmt.Fprintf(a.stdout, ", %d still with the old one (%s)", kept, config.AbbreviateHome(oldPath))
			}
			fmt.Fprintln(a.stdout, ".")
			if kept == 0 {
				dropKept()
			}
			if kept > 0 {
				return withExit(exitUnreachable, fmt.Errorf("%d of %d servers keep the old key", kept, len(targets)))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noServers, "no-servers", false, "roll the key alone; no server is touched")
	cmd.Flags().BoolVar(&dropOld, "drop-old", false, "delete the key a previous roll kept")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// rollScript is what a key roll runs on a server as its administrator:
// as root (directly or with sudo) the old key becomes the new one in
// every account's authorized_keys that holds it, options and comments
// kept, and each such file is named ("rolled FILE"); without sudo
// ("nosudo") only the administrator's own. The administrator's file
// gets the new line in any case, for the check that follows.
func rollScript(oldLine, newLine string) string {
	inner := rollFiles(oldLine, newLine, `$( (getent passwd 2>/dev/null || cat /etc/passwd) | cut -d: -f6 | sort -u)`)
	own := rollFiles(oldLine, newLine, `"$HOME"`)
	return "# ump key roll\numask 077; " +
		`if [ "$(id -u)" = 0 ]; then sh -c ` + shellQuote(inner) +
		`; elif sudo -n true 2>/dev/null; then sudo -n sh -c ` + shellQuote(inner) +
		`; else echo nosudo; sh -c ` + shellQuote(own) + `; fi; ` +
		`mkdir -p ~/.ssh; f=~/.ssh/authorized_keys; touch "$f"; grep -qF ` + shellQuote(keyBlob(newLine)) + ` "$f" || echo ` + shellQuote(newLine) + ` >> "$f"`
}

// rollFiles replaces the old key's blob with the new one's in the
// authorized_keys files under homes (a shell word list).
func rollFiles(oldLine, newLine, homes string) string {
	return "old=" + shellQuote(keyBlob(oldLine)) + "; new=" + shellQuote(keyType(newLine)+" "+keyBlob(newLine)) + "; oldt=" + shellQuote(keyType(oldLine)+" "+keyBlob(oldLine)) +
		`; for h in ` + homes + `; do for f in "$h/.ssh/authorized_keys" "$h/.ssh/authorized_keys2"; do [ -f "$f" ] && grep -qF "$old" "$f" || continue; sed "s|$oldt|$new|g" "$f" > "$f.ump" && cat "$f.ump" > "$f"; rm -f "$f.ump"; echo "rolled $f"; done; done`
}

// keyBlob is the second field of a public key line, the key itself.
func keyBlob(line string) string {
	if f := strings.Fields(line); len(f) > 1 {
		return f[1]
	}
	return ""
}
