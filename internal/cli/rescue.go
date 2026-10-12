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
	noImage   bool
	yes       bool
	output    string
	image     string
	via       string
}

func (a *app) newRescueCmd() *cobra.Command {
	var o rescueOptions
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID [-- COMMAND...]",
		Short: "Boot a server into the UMP rescue system and connect",
		Long: `Bring the server into UMP's own rescue system, a Debian that runs from
RAM with the disks untouched, and hand the terminal over to ssh there.
With a command after "--" the command runs in the rescue system instead
and its exit status becomes ump's; --no-connect only prints how to
connect.

How the server gets there depends on what answers. A server that already
runs the UMP rescue system is connected to as it is. A running system
that lets UMP in as root over SSH (with UMP's own key, see ump config
key, or your keys) loads the image and kexecs into it right away, with
no reboot through the provider. Otherwise the provider's rescue system
is enabled with UMP's key and any --ssh-key files, the server is reset
into it, and the image is loaded from there. --via direct or provider
forces one way; --no-image stops in the provider's rescue system, for
the provider's own tools. The image comes from the base URL in the
configuration (staging.image) or --image.

--no-reboot only arms the provider's rescue system for the next boot;
--no-wait returns right after the reset; both stop at the provider's
rescue system. Rescue systems have a fresh host key every boot, so the
connections skip host key checks; providers that report the host key
fingerprints have them printed. Asks for confirmation on a terminal;
without one, --yes is required.`,
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
	sub.Flags().StringVar(&o.image, "image", "", "base URL of the UMP rescue image (default: staging.image in the configuration)")
	sub.Flags().StringVar(&o.via, "via", "auto", "how to get root first: direct (SSH into the running system), provider (its rescue system), or auto")
	sub.Flags().BoolVar(&o.noImage, "no-image", false, "stop in the provider's rescue system instead of loading UMP's rescue image")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "only arm the provider's rescue system for the next boot")
	sub.Flags().BoolVar(&o.noWait, "no-wait", false, "do not wait until the provider's rescue system answers (and do not connect)")
	sub.Flags().BoolVar(&o.noConnect, "no-connect", false, "print how to connect instead of connecting")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the connection details as json or yaml instead of connecting")
	return a.serverVerbCmd("rescue", "Boot a server into the UMP rescue system and connect", "Boot a server into UMP's rescue system, directly over SSH or through the provider's rescue system, and connect to it.",
		"  ump rescue server web-01\n  ump rescue server web-01 --yes -- lsblk\n  ump rescue server web-01 --no-image\n  ump rescue server web-01 --no-reboot", sub)
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

// imageInfo describes the UMP rescue system a server was brought into.
type imageInfo struct {
	URL     string `json:"url" yaml:"url"`
	Version string `json:"version" yaml:"version"`
	// Via is how the server got there: direct (a kexec from the running
	// system), provider (through its rescue system) or running (it was
	// there already).
	Via string `json:"via" yaml:"via"`
}

type rescueResult struct {
	provider.Machine `yaml:",inline"`
	Rescue           *provider.RescueInfo `json:"rescue,omitempty" yaml:"rescue,omitempty"`
	Image            *imageInfo           `json:"image,omitempty" yaml:"image,omitempty"`
	Keys             []string             `json:"keys,omitempty" yaml:"keys,omitempty"`
	SSH              string               `json:"ssh,omitempty" yaml:"ssh,omitempty"`
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
	switch o.via {
	case "", "auto", "direct", "provider":
	default:
		return usageErrorf(cmd, "unknown --via %q (use one of auto, direct, provider)", o.via)
	}
	// --no-image, --no-reboot and --no-wait are about the provider's
	// rescue system: with them it is the destination, not the first hop
	providerOnly := o.noImage || o.noReboot || o.noWait
	if o.via == "direct" && providerOnly {
		return usageErrorf(cmd, "--via direct cannot be combined with --no-image, --no-reboot or --no-wait")
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
	imageURL := strings.TrimRight(o.image, "/")
	if imageURL == "" {
		imageURL = cfg.StagingImage()
	}
	addr := m.IPv4
	if addr == "" {
		addr = m.IPv6
	}
	ctx := cmd.Context()
	rs, hasRescue := prov.(provider.Rescue)

	// The direct way: the server may already run the image, or let UMP in
	// as root, in which case the provider is not needed at all.
	if !providerOnly && o.via != "provider" && addr != "" {
		fmt.Fprintf(a.stderr, "Checking root access to %s over SSH ...", addr)
		identities := append([]string{keyPath}, sshkey.DefaultIdentityFiles()...)
		p, ok := a.probeHost(ctx, "root", addr, identities, false)
		switch {
		case ok && p.version != "":
			fmt.Fprintf(a.stderr, " %s already runs the UMP rescue system %s.\n", m.Name, p.version)
			return a.connectRescue(cmd, m, "root", addr, keyPath, keyPaths, command, o, format, "UMP rescue system",
				&imageInfo{URL: imageURL, Version: p.version, Via: "running"}, nil)
		case ok && p.root:
			fmt.Fprintf(a.stderr, " yes (%s).\n", nonEmpty(p.os, "unknown system"))
			if err := a.confirm(cmd, o.yes, fmt.Sprintf("Load the UMP rescue image on %s over SSH and kexec into it? The running system ends without a shutdown.", describeRef(m))); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "Loading the UMP rescue image from %s on %s, without the provider's rescue system.\n", imageURL, m.Name)
			if err := a.enterImage(ctx, "root", addr, identities, false, imageURL, rescueHostname(m.Name), keys); err != nil {
				return err
			}
			version, err := a.waitForImage(ctx, addr, keyPath)
			if err != nil {
				return err
			}
			return a.connectRescue(cmd, m, "root", addr, keyPath, keyPaths, command, o, format, "UMP rescue system",
				&imageInfo{URL: imageURL, Version: version, Via: "direct"}, nil)
		case ok:
			fmt.Fprintln(a.stderr, " no (not root).")
		default:
			fmt.Fprintln(a.stderr, " no.")
		}
		if o.via == "direct" {
			return withExit(exitUnreachable, fmt.Errorf("no root access to %s over SSH; --via provider goes through the provider's rescue system", addr))
		}
		if !hasRescue {
			return fmt.Errorf("no root access to %s over SSH, and provider type %s has no rescue system", addr, m.Provider)
		}
		fmt.Fprintln(a.stderr, "Going through the provider's rescue system.")
	}

	if !hasRescue {
		return fmt.Errorf("account %s: provider type %s has no rescue system", m.Account, m.Provider)
	}
	pw, canReboot := prov.(provider.Power)
	if !o.noReboot && !canReboot {
		return fmt.Errorf("account %s: provider type %s cannot reboot; use --no-reboot and reboot the server yourself", m.Account, m.Provider)
	}
	var question string
	switch {
	case o.noReboot:
		question = fmt.Sprintf("Arm the rescue system for the next boot of %s?", describeRef(m))
	case providerOnly:
		question = fmt.Sprintf("Boot %s into the provider's rescue system?", describeRef(m))
	default:
		question = fmt.Sprintf("Boot %s into the UMP rescue system through the provider's rescue system?", describeRef(m))
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
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
	argv := sshArgs(user, info.Address, []string{keyPath}, true, true, command)
	if providerOnly && format != "" {
		return printEncoded(a.stdout, rescueResult{Machine: m, Rescue: info, Keys: keyPaths, SSH: shellJoin(argv)}, format)
	}
	// with an encoded result to come, the progress must not get into it
	out := a.stdout
	if format != "" {
		out = a.stderr
	}
	switch {
	case rebooted:
		fmt.Fprintf(out, "Rescue system enabled for %s; rebooting into it.\n", describeRef(m))
	default:
		fmt.Fprintf(out, "Rescue system enabled for %s; it boots at the next reboot.\n", describeRef(m))
	}
	fmt.Fprintf(out, "SSH keys: %s\n", strings.Join(keyPaths, ", "))
	if info.Password != "" {
		fmt.Fprintf(out, "Password: %s\n", info.Password)
	}
	if len(info.HostKeys) > 0 {
		fmt.Fprintf(out, "Host key fingerprints: %s\n", strings.Join(info.HostKeys, ", "))
	}
	if info.Note != "" {
		fmt.Fprintf(out, "Note: %s\n", info.Note)
	}
	if info.Address == "" {
		return nil
	}
	if !rebooted || o.noWait {
		fmt.Fprintf(out, "Connect once it is up: %s\n", shellJoin(argv))
		return nil
	}
	if err := a.waitForSSH(ctx, info.Address, wasUp); err != nil {
		return err
	}
	if providerOnly {
		return a.connectRescue(cmd, m, user, info.Address, keyPath, keyPaths, command, o, format, "rescue system", nil, info)
	}

	// The provider's rescue system is only the first hop.
	p, ok := a.probeHost(ctx, user, info.Address, []string{keyPath}, true)
	if !ok {
		manual := shellJoin(sshArgs(user, info.Address, []string{keyPath}, true, true, nil))
		return withExit(exitUnreachable, fmt.Errorf("the rescue system of %s did not accept UMP's key; connect by hand (%s) and run: curl -fsSL %s/kexec-into-rescue.sh -o /tmp/kexec-into-rescue.sh && bash /tmp/kexec-into-rescue.sh -u %s", m.Name, manual, imageURL, imageURL))
	}
	if p.version != "" {
		fmt.Fprintf(a.stderr, "%s already runs the UMP rescue system %s.\n", m.Name, p.version)
		return a.connectRescue(cmd, m, user, info.Address, keyPath, keyPaths, command, o, format, "UMP rescue system",
			&imageInfo{URL: imageURL, Version: p.version, Via: "running"}, info)
	}
	fmt.Fprintf(a.stderr, "Provider rescue system reached (%s); loading the UMP rescue image from %s.\n", nonEmpty(p.os, "unknown system"), imageURL)
	if err := a.enterImage(ctx, user, info.Address, []string{keyPath}, true, imageURL, rescueHostname(m.Name), keys); err != nil {
		return err
	}
	version, err := a.waitForImage(ctx, info.Address, keyPath)
	if err != nil {
		return err
	}
	return a.connectRescue(cmd, m, "root", info.Address, keyPath, keyPaths, command, o, format, "UMP rescue system",
		&imageInfo{URL: imageURL, Version: version, Via: "provider"}, info)
}

// connectRescue is the last step: the terminal is handed over to ssh, or
// the command runs there, or the way to connect is printed (--no-connect,
// no terminal, or an encoded result).
func (a *app) connectRescue(cmd *cobra.Command, m provider.Machine, user, addr, keyPath string, keyPaths, command []string, o rescueOptions, format outputFormat, system string, img *imageInfo, info *provider.RescueInfo) error {
	argv := sshArgs(user, addr, []string{keyPath}, true, true, command)
	if format != "" {
		return printEncoded(a.stdout, rescueResult{Machine: m, Rescue: info, Image: img, Keys: keyPaths, SSH: shellJoin(argv)}, format)
	}
	interactive := len(command) == 0
	if o.noConnect || (interactive && (a.isTTY == nil || !a.isTTY())) {
		fmt.Fprintf(a.stdout, "Connect: %s\n", shellJoin(argv))
		return nil
	}
	if interactive {
		fmt.Fprintf(a.stderr, "Connecting to the %s of %s as %s; exit the shell to return.\n", system, m.Name, user)
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
