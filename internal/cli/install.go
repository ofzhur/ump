package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/sshkey"
)

type installOptions struct {
	os          string
	hostname    string
	storage     string
	timezone    string
	disks       string
	keyFiles    []string
	ipv6        bool
	mitigations bool
	yes         bool
	noReboot    bool
	dryRun      bool
	via         string
	output      string
}

func (a *app) newInstallCmd() *cobra.Command {
	var o installOptions
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID",
		Short: "Install an operating system on a server",
		Long: `Put an operating system on the server's disks from a UMP base image and
boot it. The server is brought into the UMP rescue system first (the way
ump rescue does it: as it is when it runs it already, by a kexec from the
running system when that lets UMP in as root, else through the provider's
rescue system), the installer is fetched from the storage and run there,
the server is rebooted into the new system, and ump waits until it
answers over SSH with UMP's key and reports what came up.

Every disk of the server is erased. The default layout, lvm-linear, puts
the boot partitions on the first disk and makes every disk a physical
volume of one volume group, with root as one linear volume over all of
it. The new system gets the server's name as hostname (or --hostname),
the address, gateway and resolvers 1.1.1.1 and 8.8.8.8, root with UMP's
key and the keys the rescue system had, and a kernel command line with
mitigations=off and ipv6.disable=1 unless --mitigations or --ipv6 keep
them on (see docs/cli.md for turning them on later).

--dry-run brings the server into the rescue system and prints the
installer's plan without touching a disk; --no-reboot leaves the server
in the rescue system after the installation. Asks for confirmation on a
terminal; without one, --yes is required.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installServer(cmd, args[0], o)
		},
	}
	sub.Flags().StringVar(&o.os, "os", "ubuntu-26.04", "the operating system image")
	sub.Flags().StringVar(&o.hostname, "hostname", "", "hostname of the new system (default: the server's name)")
	sub.Flags().StringVar(&o.storage, "storage", "", "base URL of the storage with the installer and the images (default: storage in the configuration)")
	sub.Flags().StringVar(&o.timezone, "timezone", "UTC", "time zone of the new system")
	sub.Flags().StringVar(&o.disks, "disks", "", "the disks to use, comma-separated (default: every disk)")
	sub.Flags().StringArrayVar(&o.keyFiles, "ssh-key", nil, "additional public key file for root of the new system (repeatable)")
	sub.Flags().BoolVar(&o.ipv6, "ipv6", false, "keep IPv6 on (default: off, ipv6.disable=1)")
	sub.Flags().BoolVar(&o.mitigations, "mitigations", false, "keep the CPU vulnerability mitigations on (default: off, mitigations=off)")
	sub.Flags().StringVar(&o.via, "via", "auto", "how to reach the rescue system first: direct, provider, or auto")
	sub.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the installer's plan instead of installing")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "leave the server in the rescue system after the installation")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	return a.serverVerbCmd("install", "Install an operating system on a server", "Install an operating system on a server from a UMP base image, through the UMP rescue system.",
		"  ump install server web-01\n  ump install server web-01 --yes --hostname web-01.example.net\n  ump install server web-01 --dry-run\n  ump install server web-01 --ipv6 --mitigations", sub)
}

// installResult is what ump install reports.
type installResult struct {
	provider.Machine `yaml:",inline"`
	OS               string `json:"os" yaml:"os"`
	Hostname         string `json:"hostname" yaml:"hostname"`
	Via              string `json:"via" yaml:"via"`
	DryRun           bool   `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	Rebooted         bool   `json:"rebooted" yaml:"rebooted"`
	// System is what answered after the reboot.
	System *installedSystem `json:"system,omitempty" yaml:"system,omitempty"`
}

type installedSystem struct {
	Name    string `json:"name" yaml:"name"`
	Root    string `json:"root" yaml:"root"`
	Details string `json:"details,omitempty" yaml:"details,omitempty"`
}

func (a *app) installServer(cmd *cobra.Command, ref string, o installOptions) error {
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
	if o.os == "" || strings.ContainsAny(o.os, "/ ") {
		return usageErrorf(cmd, "invalid --os %q", o.os)
	}
	if o.hostname != "" {
		if err := validateServerName(o.hostname); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	// root of the new system gets UMP's key, the user's own public keys
	// (next to the usual ~/.ssh/id_* files) and the --ssh-key files
	keyFiles := append([]string{}, o.keyFiles...)
	for _, id := range sshkey.DefaultIdentityFiles() {
		if _, err := os.Stat(id + ".pub"); err == nil {
			keyFiles = append(keyFiles, id+".pub")
		}
	}
	keys, keyPaths, keyPath, err := a.rescueKeys(cfg, keyFiles)
	if err != nil {
		return err
	}
	m, prov, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	if m.IPv4 == "" && m.IPv6 == "" {
		return fmt.Errorf("%s has no public address", m.Ref())
	}
	storage := strings.TrimRight(o.storage, "/")
	rescueURL := cfg.StagingImage()
	if storage == "" {
		storage = cfg.StorageURL()
	} else if cfg.Staging.Image == "" {
		// --storage moves the rescue image along, unless it is pinned
		rescueURL = storage + "/rescue"
	}
	hostname := o.hostname
	if hostname == "" {
		hostname = rescueHostname(m.Name)
		if hostname == "" {
			return fmt.Errorf("the name %q of %s is not a valid hostname; give one with --hostname", m.Name, m.Ref())
		}
	}
	question := fmt.Sprintf("Install %s on %s as %s? Every disk of the server is erased.", o.os, describeRef(m), hostname)
	if o.dryRun {
		question = fmt.Sprintf("Bring %s into the rescue system and show the installation plan for %s? (dry run: nothing is erased)", describeRef(m), o.os)
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
	ctx := cmd.Context()
	// everything on the way is progress (stderr); stdout is for the result
	entry, err := a.bringToImage(cmd, m, prov, keys, keyPath, keyPaths, rescueURL, entryOptions{via: o.via, confirmed: true, quiet: true})
	if err != nil {
		return err
	}

	// the installer, from the storage, in the rescue system
	args := []string{"--storage", storage, "--os", o.os, "--hostname", hostname}
	if o.ipv6 {
		args = append(args, "--ipv6")
	}
	if o.mitigations {
		args = append(args, "--mitigations")
	}
	if o.disks != "" {
		args = append(args, "--disks", o.disks)
	}
	if o.timezone != "" && o.timezone != "UTC" {
		args = append(args, "--timezone", o.timezone)
	}
	if o.dryRun {
		args = append(args, "--dry-run")
	}
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Installation plan for %s on %s:\n", o.os, m.Name)
	} else {
		fmt.Fprintf(a.stderr, "Installing %s on %s as %s ...\n", o.os, m.Name, hostname)
	}
	installer := storage + "/install/install.sh"
	argv := withSSHOptions(sshArgs(entry.user, entry.addr, []string{keyPath}, true, true, []string{fetchAndRun(installer, "/tmp/ump-install.sh", args)}), batchOptions(10*time.Second)...)
	var buf bytes.Buffer
	w := io.MultiWriter(a.stderr, &buf)
	code, err := a.sshExec(ctx, argv, nil, w, w)
	if err != nil {
		return err
	}
	switch {
	case code == 255:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation; the server is in the UMP rescue system, look there (ump ssh server %s --staging)", entry.addr, ref))
	case code != 0 && !strings.Contains(buf.String(), "==> "):
		return fmt.Errorf("%s could not fetch the installer from %s (exit status %d); the server must reach the storage", entry.addr, installer, code)
	case code != 0:
		return fmt.Errorf("the installation on %s failed (exit status %d); the server stays in the UMP rescue system (ump ssh server %s --staging)", m.Name, code, ref)
	}
	result := installResult{Machine: m, OS: o.os, Hostname: hostname, Via: entry.via, DryRun: o.dryRun}
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Dry run done; %s stays in the UMP rescue system (ump ssh server %s --staging).\n", m.Name, ref)
		return a.installReport(result, format)
	}

	// providers that keep the rescue system as the boot option until told
	// otherwise would boot it again
	if rs, ok := prov.(provider.Rescue); ok {
		if on, err := rs.RescueEnabled(ctx, m); err == nil && on {
			if err := rs.DisableRescue(ctx, m); err != nil {
				return fmt.Errorf("account %s: %s is installed, but the provider's rescue system could not be disabled, and would boot again: %w", m.Account, o.os, err)
			}
		}
	}
	if o.noReboot {
		fmt.Fprintf(a.stderr, "Installed; %s stays in the UMP rescue system, a reboot starts %s.\n", m.Name, o.os)
		return a.installReport(result, format)
	}

	fmt.Fprintf(a.stderr, "Rebooting %s into %s ...\n", m.Name, o.os)
	reboot := withSSHOptions(sshArgs(entry.user, entry.addr, []string{keyPath}, true, true, []string{"systemctl reboot"}), batchOptions(10*time.Second)...)
	if code, err := a.sshExec(ctx, reboot, nil, io.Discard, io.Discard); err != nil || (code != 0 && code != 255) {
		pw, ok := prov.(provider.Power)
		if !ok {
			return fmt.Errorf("%s is installed, but could not be rebooted over SSH, and provider type %s cannot reboot it; reboot it yourself", m.Name, m.Provider)
		}
		if err := pw.Reboot(ctx, m, provider.RebootHard); err != nil {
			return fmt.Errorf("account %s: %s is installed, but the reboot failed: %w", m.Account, m.Name, err)
		}
	}
	result.Rebooted = true
	if err := a.waitForSSH(ctx, entry.addr, true); err != nil {
		return err
	}
	sys, ok := a.installedSystem(ctx, entry.addr, keyPath)
	if !ok {
		return withExit(exitUnreachable, fmt.Errorf("SSH answers on %s, but not with UMP's key; the new system may not have booted, look at the server's console", entry.addr))
	}
	result.System = sys
	fmt.Fprintf(a.stderr, "%s is up: %s, hostname %s, root on %s.\n", m.Name, sys.Name, hostname, sys.Root)
	return a.installReport(result, format)
}

func (a *app) installReport(r installResult, format outputFormat) error {
	if format == "" {
		return nil
	}
	return printEncoded(a.stdout, r, format)
}

// installedSystem asks the machine, with UMP's key, what it runs now.
func (a *app) installedSystem(ctx context.Context, addr, keyPath string) (*installedSystem, bool) {
	const query = `sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'; findmnt -no SOURCE / ; cat /etc/ump/install 2>/dev/null | tr '\n' ' '`
	argv := withSSHOptions(sshArgs("root", addr, []string{keyPath}, true, true, []string{query}), batchOptions(10*time.Second)...)
	var out bytes.Buffer
	code, err := a.sshExec(ctx, argv, nil, &out, io.Discard)
	if err != nil || code != 0 {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	sys := &installedSystem{}
	if len(lines) > 0 {
		sys.Name = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		sys.Root = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		sys.Details = strings.TrimSpace(lines[2])
	}
	return sys, sys.Name != ""
}
