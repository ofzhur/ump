package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/sshkey"
)

type rescueOptions struct {
	keyFiles  []string
	noReboot  bool
	noWait    bool
	noConnect bool
	yes       bool
	output    string
}

func (a *app) newRescueCmd() *cobra.Command {
	var o rescueOptions
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID [-- COMMAND...]",
		Short: "Boot a server into its provider's rescue system and connect",
		Long: `Enable the provider's rescue system for the server with UMP's own SSH
key (generated on first use, see ump config key) and any --ssh-key files,
registering keys the provider does not know yet, reset the server into
it, wait until SSH answers and hand the terminal over to ssh. With a
command after "--" the command runs in the rescue system instead and its
exit status becomes ump's; --no-connect only prints how to connect.

--no-reboot only arms the rescue system for the next boot; --no-wait
returns right after the reset. Rescue systems have a fresh host key every
boot, so the connection skips host key checks; providers that report the
host key fingerprints have them printed. Asks for confirmation on a
terminal; without one, --yes is required.`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			refs, command := splitDash(cmd.ArgsLenAtDash(), args)
			if len(refs) != 1 {
				return usageErrorf(cmd, "expected one server reference, got %d", len(refs))
			}
			return a.rescueServer(cmd, refs[0], command, o)
		},
	}
	sub.Flags().StringArrayVar(&o.keyFiles, "ssh-key", nil, "additional public key file to put into the rescue system (repeatable)")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "only arm the rescue system for the next boot")
	sub.Flags().BoolVar(&o.noWait, "no-wait", false, "do not wait until SSH answers (and do not connect)")
	sub.Flags().BoolVar(&o.noConnect, "no-connect", false, "print how to connect instead of connecting")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the connection details as json or yaml instead of connecting")
	return a.serverVerbCmd("rescue", "Boot a server into its rescue system and connect", "Boot a server into its provider's rescue system and connect to it.",
		"  ump rescue server web-01\n  ump rescue server web-01 --yes -- lsblk\n  ump rescue server web-01 --no-reboot", sub)
}

func (a *app) newUnrescueCmd() *cobra.Command {
	var (
		reboot bool
		wait   bool
		yes    bool
	)
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID",
		Short: "Leave the rescue system: the next boot is from disk",
		Long: `Disable the provider's rescue system so that the next boot is from disk.
With --reboot the server is reset right away (--wait waits for SSH).
Some providers (OVHcloud Public Cloud) reboot the instance themselves.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.unrescueServer(cmd, args[0], reboot, wait, yes)
		},
	}
	sub.Flags().BoolVar(&reboot, "reboot", false, "reset the server now")
	sub.Flags().BoolVar(&wait, "wait", false, "with --reboot: wait until SSH answers again")
	sub.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation of the reboot")
	return a.serverVerbCmd("unrescue", "Leave the rescue system", "Disable the rescue system so that the next boot is from disk.",
		"  ump unrescue server web-01 --reboot --wait", sub)
}

// stagingKey returns UMP's own public key, generating the pair on first
// use.
func (a *app) stagingKey(cfg *config.Config) (*sshkey.Key, string, error) {
	path := cfg.StagingKeyPath()
	k, created, err := sshkey.Ensure(path, sshkey.DefaultComment())
	if err != nil {
		return nil, path, err
	}
	if created {
		fmt.Fprintf(a.stderr, "Generated UMP's SSH key at %s (public key %s).\n", config.AbbreviateHome(path), k.FingerprintSHA256())
	}
	return k, path, nil
}

// rescueKeys collects the public keys for the rescue system: UMP's own
// key first, then the given files.
func (a *app) rescueKeys(cfg *config.Config, files []string) (keys []provider.PublicKey, paths []string, keyPath string, err error) {
	k, keyPath, err := a.stagingKey(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	keys = append(keys, provider.PublicKey{Name: keyName(k, keyPath+".pub"), Data: k.Line(), Fingerprint: k.FingerprintMD5()})
	paths = append(paths, keyPath+".pub")
	for _, p := range files {
		k, err := sshkey.ReadFile(expandHome(p))
		if err != nil {
			return nil, nil, "", err
		}
		keys = append(keys, provider.PublicKey{Name: keyName(k, p), Data: k.Line(), Fingerprint: k.FingerprintMD5()})
		paths = append(paths, p)
	}
	return keys, paths, keyPath, nil
}

// keyName is the name a key is registered under at a provider.
func keyName(k *sshkey.Key, path string) string {
	if k.Comment != "" {
		return k.Comment
	}
	return "ump-" + strings.TrimSuffix(filepath.Base(path), ".pub")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

type rescueResult struct {
	provider.Machine `yaml:",inline"`
	Rescue           provider.RescueInfo `json:"rescue" yaml:"rescue"`
	Keys             []string            `json:"keys,omitempty" yaml:"keys,omitempty"`
	SSH              string              `json:"ssh,omitempty" yaml:"ssh,omitempty"`
}

func (a *app) rescueServer(cmd *cobra.Command, ref string, command []string, o rescueOptions) error {
	format := outputFormat("")
	if o.output != "" {
		f, err := parseFormat(o.output)
		if err != nil || (f != formatJSON && f != formatYAML) {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", o.output)
		}
		format = f
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	keys, keyPaths, keyPath, err := a.rescueKeys(cfg, o.keyFiles)
	if err != nil {
		return err
	}
	m, prov, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	rs, ok := prov.(provider.Rescue)
	if !ok {
		return fmt.Errorf("account %s: provider type %s has no rescue system", m.Account, m.Provider)
	}
	pw, canReboot := prov.(provider.Power)
	if !o.noReboot && !canReboot {
		return fmt.Errorf("account %s: provider type %s cannot reboot; use --no-reboot and reboot the server yourself", m.Account, m.Provider)
	}
	action := "Boot"
	if o.noReboot {
		action = "Arm the rescue system for the next boot of"
	}
	if err := a.confirm(cmd, o.yes, fmt.Sprintf("%s %s into the rescue system?", action, describeRef(m))); err != nil {
		return err
	}
	ctx := cmd.Context()
	wasUp := !o.noReboot && !o.noWait && a.probe(ctx, m.IPv4)
	info, err := rs.EnableRescue(ctx, m, provider.RescueOptions{Keys: keys})
	if err != nil {
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	rebooted := info.Reboots
	if !o.noReboot && !info.Reboots {
		if err := pw.Reboot(ctx, m, provider.RebootHard); err != nil {
			return fmt.Errorf("account %s: rescue system enabled, but the reboot failed: %w", m.Account, err)
		}
		rebooted = true
	}
	user := info.User
	if user == "" {
		user = "root"
	}
	argv := sshArgs(user, info.Address, keyPath, true, command)
	if format != "" {
		return printEncoded(a.stdout, rescueResult{Machine: m, Rescue: *info, Keys: keyPaths, SSH: shellJoin(argv)}, format)
	}
	switch {
	case rebooted:
		fmt.Fprintf(a.stdout, "Rescue system enabled for %s; rebooting into it.\n", describeRef(m))
	default:
		fmt.Fprintf(a.stdout, "Rescue system enabled for %s; it boots at the next reboot.\n", describeRef(m))
	}
	fmt.Fprintf(a.stdout, "SSH keys: %s\n", strings.Join(keyPaths, ", "))
	if info.Password != "" {
		fmt.Fprintf(a.stdout, "Password: %s\n", info.Password)
	}
	if len(info.HostKeys) > 0 {
		fmt.Fprintf(a.stdout, "Host key fingerprints: %s\n", strings.Join(info.HostKeys, ", "))
	}
	if info.Note != "" {
		fmt.Fprintf(a.stdout, "Note: %s\n", info.Note)
	}
	if info.Address == "" {
		return nil
	}
	if !rebooted || o.noWait {
		fmt.Fprintf(a.stdout, "Connect once it is up: %s\n", shellJoin(argv))
		return nil
	}
	if err := a.waitForSSH(ctx, info.Address, wasUp); err != nil {
		return err
	}
	interactive := len(command) == 0
	if o.noConnect || (interactive && (a.isTTY == nil || !a.isTTY())) {
		fmt.Fprintf(a.stdout, "Connect: %s\n", shellJoin(argv))
		return nil
	}
	if interactive {
		fmt.Fprintf(a.stderr, "Connecting to the rescue system of %s as %s; exit the shell to return.\n", m.Name, user)
	}
	code, err := a.ssh(argv, interactive)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

func (a *app) unrescueServer(cmd *cobra.Command, ref string, reboot, wait, yes bool) error {
	m, prov, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	rs, ok := prov.(provider.Rescue)
	if !ok {
		return fmt.Errorf("account %s: provider type %s has no rescue system", m.Account, m.Provider)
	}
	pw, canReboot := prov.(provider.Power)
	if reboot && !canReboot {
		return fmt.Errorf("account %s: provider type %s cannot reboot", m.Account, m.Provider)
	}
	if reboot {
		if err := a.confirm(cmd, yes, fmt.Sprintf("Disable the rescue system of %s and reboot it?", describeRef(m))); err != nil {
			return err
		}
	}
	ctx := cmd.Context()
	wasUp := reboot && wait && sshAnswers(ctx, m.IPv4)
	if err := rs.DisableRescue(ctx, m); err != nil {
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	if !reboot {
		fmt.Fprintf(a.stdout, "Rescue system disabled for %s; the next boot is from disk.\n", describeRef(m))
		return nil
	}
	if err := pw.Reboot(ctx, m, provider.RebootHard); err != nil {
		return fmt.Errorf("account %s: rescue system disabled, but the reboot failed: %w", m.Account, err)
	}
	fmt.Fprintf(a.stdout, "Rescue system disabled for %s; rebooting from disk.\n", describeRef(m))
	if wait {
		return a.waitForSSH(ctx, m.IPv4, wasUp)
	}
	return nil
}
