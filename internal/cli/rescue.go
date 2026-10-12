package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/sshkey"
)

type rescueOptions struct {
	keyFiles []string
	noKeys   bool
	noReboot bool
	noWait   bool
	yes      bool
	output   string
}

func (a *app) newRescueCmd() *cobra.Command {
	var o rescueOptions
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID",
		Short: "Boot a server into its provider's rescue system",
		Long: `Enable the provider's rescue system for the server, with your SSH public
keys, and reboot into it. The keys come from --ssh-key files, or from the
first of ~/.ssh/id_ed25519.pub, id_ecdsa.pub and id_rsa.pub that exists;
keys the provider does not know yet are registered there. Without any
key (--no-keys), providers that issue a root password do so.

By default the command reboots the server (hard reset) and waits until
SSH answers; --no-reboot only arms the rescue system for the next boot,
--no-wait does not wait. The rescue system has its own host key, so
expect a known_hosts warning. Asks for confirmation on a terminal;
without one, --yes is required.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.rescueServer(cmd, args[0], o)
		},
	}
	sub.Flags().StringArrayVar(&o.keyFiles, "ssh-key", nil, "public key file to put into the rescue system (repeatable)")
	sub.Flags().BoolVar(&o.noKeys, "no-keys", false, "do not put any SSH key into the rescue system")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "only arm the rescue system for the next boot")
	sub.Flags().BoolVar(&o.noWait, "no-wait", false, "do not wait until SSH answers")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the connection details as json or yaml")
	return a.serverVerbCmd("rescue", "Boot a server into its rescue system", "Boot a server into its provider's rescue system.",
		"  ump rescue server web-01\n  ump rescue server web-01 --ssh-key ~/.ssh/ops.pub --yes\n  ump rescue server web-01 --no-reboot", sub)
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

// rescueKeys collects the public keys for the rescue system.
func rescueKeys(files []string) ([]provider.PublicKey, []string, error) {
	var paths []string
	if len(files) > 0 {
		paths = files
	} else {
		for _, p := range sshkey.DefaultPaths() {
			if _, err := os.Stat(p); err == nil {
				paths = []string{p}
				break
			}
		}
	}
	var keys []provider.PublicKey
	for _, p := range paths {
		k, err := sshkey.ReadFile(expandHome(p))
		if err != nil {
			return nil, nil, err
		}
		name := k.Comment
		if name == "" {
			name = "ump-" + strings.TrimSuffix(filepath.Base(p), ".pub")
		}
		keys = append(keys, provider.PublicKey{Name: name, Data: k.Line(), Fingerprint: k.FingerprintMD5()})
	}
	return keys, paths, nil
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
}

func (a *app) rescueServer(cmd *cobra.Command, ref string, o rescueOptions) error {
	format := outputFormat("")
	if o.output != "" {
		f, err := parseFormat(o.output)
		if err != nil || (f != formatJSON && f != formatYAML) {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", o.output)
		}
		format = f
	}
	if o.noKeys && len(o.keyFiles) > 0 {
		return usageErrorf(cmd, "--no-keys and --ssh-key exclude each other")
	}
	var keys []provider.PublicKey
	var keyPaths []string
	if !o.noKeys {
		var err error
		keys, keyPaths, err = rescueKeys(o.keyFiles)
		if err != nil {
			return err
		}
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
	wasUp := !o.noReboot && !o.noWait && sshAnswers(ctx, m.IPv4)
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
	if format != "" {
		return printEncoded(a.stdout, rescueResult{Machine: m, Rescue: *info, Keys: keyPaths}, format)
	}
	switch {
	case rebooted:
		fmt.Fprintf(a.stdout, "Rescue system enabled for %s; rebooting into it.\n", describeRef(m))
	default:
		fmt.Fprintf(a.stdout, "Rescue system enabled for %s; it boots at the next reboot.\n", describeRef(m))
	}
	if len(keyPaths) > 0 {
		fmt.Fprintf(a.stdout, "SSH keys: %s\n", strings.Join(keyPaths, ", "))
	}
	if info.Password != "" {
		fmt.Fprintf(a.stdout, "Password: %s\n", info.Password)
	}
	if info.Note != "" {
		fmt.Fprintf(a.stdout, "Note: %s\n", info.Note)
	}
	if rebooted && !o.noWait {
		if err := a.waitForSSH(ctx, info.Address, wasUp); err != nil {
			return err
		}
	}
	user := info.User
	if user == "" {
		user = "root"
	}
	if info.Address != "" {
		fmt.Fprintf(a.stdout, "Connect: ssh %s@%s\n", user, info.Address)
		fmt.Fprintln(a.stderr, "The rescue system has its own host key; expect a known_hosts warning.")
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
