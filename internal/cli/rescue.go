package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
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
	force     bool
	output    string
	image     string
	via       string
}

func (a *app) newServerRescueCmd() *cobra.Command {
	var o rescueOptions
	sub := &cobra.Command{
		Use:   "rescue NAME|ID|IP|PROVIDER/ID [-- COMMAND...]",
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
without one, --yes is required.

Where the provider has no rescue system for ump (DigitalOcean, Linode,
Vultr, servers added by hand), a failed jump leaves only the provider's
console to bring the server back, so rescue refuses the jump without
--force; a server that already runs the UMP rescue system is connected
to either way. rescue never rebuilds or erases a server.`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			refs, command := splitDash(cmd.ArgsLenAtDash(), args)
			if len(refs) != 1 {
				return usageErrorf(cmd, "expected one server reference, got %d", len(refs))
			}
			return a.rescueServer(cmd, refs[0], command, o)
		},
	}
	sub.Flags().StringArrayVar(&o.keyFiles, "ssh-key", nil, "another key for the rescue system, next to UMP's and staging.keys: a named key, local, agent, a file or a key line (repeatable)")
	sub.Flags().StringVar(&o.image, "image", "", "base URL of the UMP rescue image (default: staging.image in the configuration)")
	sub.Flags().StringVar(&o.via, "via", "auto", "how to get root first: direct (SSH into the running system), provider (its rescue system), or auto")
	sub.Flags().BoolVar(&o.noImage, "no-image", false, "stop in the provider's rescue system instead of loading UMP's rescue image")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "only arm the provider's rescue system for the next boot")
	sub.Flags().BoolVar(&o.noWait, "no-wait", false, "do not wait until the provider's rescue system answers (and do not connect)")
	sub.Flags().BoolVar(&o.noConnect, "no-connect", false, "print how to connect instead of connecting")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().BoolVar(&o.force, "force", false, "jump into the rescue system even where no provider rescue system stands behind the server")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the connection details as json or yaml instead of connecting")
	return sub
}

func (a *app) newServerUnrescueCmd() *cobra.Command {
	var (
		reboot bool
		wait   bool
		yes    bool
	)
	sub := &cobra.Command{
		Use:   "unrescue NAME|ID|IP|PROVIDER/ID",
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
	return sub
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
// key first, which ump itself gets in with and which is always there,
// then the keys chosen for rescue systems (staging.keys) and the
// --ssh-key references of this run, each once. labels name them for
// people: "ump (path)" and the references.
func (a *app) rescueKeys(cfg *config.Config, refs []string) (keys []provider.PublicKey, labels []string, keyPath string, err error) {
	k, keyPath, err := a.stagingKey(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	keys = append(keys, provider.PublicKey{Name: keyName(k, keyPath+".pub"), Data: k.Line(), Fingerprint: k.FingerprintMD5()})
	labels = append(labels, "ump ("+config.AbbreviateHome(keyPath+".pub")+")")
	seen := map[string]bool{k.FingerprintSHA256(): true}
	resolve := a.keyResolver(cfg)
	for _, ref := range append(append([]string{}, cfg.Staging.Keys...), refs...) {
		lines, err := resolve(ref)
		if err != nil {
			return nil, nil, "", fmt.Errorf("rescue key %q: %w", ref, err)
		}
		added := 0
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			pk, err := sshkey.Parse(line)
			if err != nil {
				return nil, nil, "", fmt.Errorf("rescue key %q: %w", ref, err)
			}
			if seen[pk.FingerprintSHA256()] {
				continue
			}
			seen[pk.FingerprintSHA256()] = true
			name := keyName(pk, ref)
			if isNamedKeyRef(ref) {
				// registered at the providers under its ump name
				name = ref
				if added > 0 {
					name = fmt.Sprintf("%s-%d", ref, added+1)
				}
			}
			keys = append(keys, provider.PublicKey{Name: name, Data: pk.Line(), Fingerprint: pk.FingerprintMD5()})
			added++
		}
		if added > 0 {
			labels = append(labels, ref)
		}
	}
	return keys, labels, keyPath, nil
}

// isNamedKeyRef says whether a key reference names a key kept with ump
// config key add (not ump, local, agent, a file or a key line).
func isNamedKeyRef(ref string) bool {
	switch ref {
	case "ump", "local", "agent":
		return false
	}
	return !profile.IsKeyLine(ref) && !strings.ContainsAny(ref, "/~ ") && !strings.HasSuffix(ref, ".pub")
}

// warnUMPKeyOnly says, so that nobody misses it, that only UMP's own key
// opens the rescue system: nobody gets in but through ump, and losing
// UMP's key loses the way in.
func (a *app) warnUMPKeyOnly(name string) {
	lines := []string{
		"WARNING: only UMP's own key opens the rescue system of " + name + ".",
		"",
		"None of your keys goes in. You reach it only through ump (ump ssh",
		name + " --staging), and if UMP's key is lost, nobody gets in.",
		"",
		"Choose your keys once:  ump config key rescue NAME...",
		"  (a named key from ump config key list, local, agent, or a file)",
		"or for this run:        --ssh-key NAME",
	}
	width := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > width {
			width = n
		}
	}
	bar := strings.Repeat("━", width+2)
	var b strings.Builder
	b.WriteString("┏" + bar + "┓\n")
	for _, l := range lines {
		b.WriteString("┃ " + l + strings.Repeat(" ", width-utf8.RuneCountInString(l)) + " ┃\n")
	}
	b.WriteString("┗" + bar + "┛\n")
	fmt.Fprint(a.stderr, b.String())
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
	if len(keys) == 1 {
		a.warnUMPKeyOnly(ref)
	}
	m, prov, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	defer a.invalidateListing(m.Account)
	a.warnNoRescue(m, prov)
	a.warnPreflight(cmd.Context(), m, prov)
	doc, err := a.loadState(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	unlock, err := a.lockServer(cmd.Context(), m, "rescue")
	if err != nil {
		return err
	}
	defer unlock()
	if providerOnly {
		return a.providerRescueOnly(cmd, m, prov, keys, keyPath, keyPaths, command, o, format)
	}
	imageURL := strings.TrimRight(o.image, "/")
	if imageURL == "" {
		imageURL = cfg.StagingImage()
	}
	entry, err := a.bringToImage(cmd, m, prov, keys, keyPath, keyPaths, imageURL, entryOptions{via: o.via, yes: o.yes, quiet: format != "", unguarded: o.force, users: a.loginUsers(cfg, m, doc)})
	if err != nil {
		return err
	}
	return a.connectRescue(cmd, m, entry.user, entry.addr, keyPath, keyPaths, command, o, format, "UMP rescue system",
		&imageInfo{URL: imageURL, Version: entry.version, Via: entry.via}, entry.info)
}

// providerRescueOnly is the rescue command with the provider's rescue
// system as the destination (--no-image, --no-reboot, --no-wait).
func (a *app) providerRescueOnly(cmd *cobra.Command, m provider.Machine, prov provider.Provider, keys []provider.PublicKey, keyPath string, keyPaths, command []string, o rescueOptions, format outputFormat) error {
	rs, ok := prov.(provider.Rescue)
	if !ok {
		return fmt.Errorf("account %s: provider type %s has no rescue system", m.Account, m.Provider)
	}
	pw, canReboot := prov.(provider.Power)
	if !o.noReboot && !canReboot {
		return fmt.Errorf("account %s: provider type %s cannot reboot; use --no-reboot and reboot the server yourself", m.Account, m.Provider)
	}
	question := fmt.Sprintf("Boot %s into the provider's rescue system?", describeRef(m))
	if o.noReboot {
		question = fmt.Sprintf("Arm the rescue system for the next boot of %s?", describeRef(m))
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
	ctx := cmd.Context()
	// with an encoded result to come, the progress must not get into it;
	// the encoded result itself comes right away, there is nothing to wait for
	out := a.stdout
	if format != "" {
		out = io.Discard
	}
	info, user, rebooted, wasUp, err := a.enableProviderRescue(ctx, m, rs, pw, keys, keyPath, keyPaths, out, o.noReboot)
	if err != nil {
		return err
	}
	argv := sshArgs(user, info.Address, []string{keyPath}, true, true, command)
	if format != "" {
		return printEncoded(a.stdout, rescueResult{Machine: m, Rescue: info, Keys: keyPaths, SSH: shellJoin(argv)}, format)
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
	return a.connectRescue(cmd, m, user, info.Address, keyPath, keyPaths, command, o, format, "rescue system", nil, info)
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
	defer a.invalidateListing(m.Account)
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
