package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/sshkey"
	"github.com/ofzhur/ump/internal/state"
)

type installOptions struct {
	profile     string
	layout      string
	firewall    string
	os          string
	hostname    string
	storage     string
	timezone    string
	disks       string
	keyFiles    []string
	sets        []string
	ipv6        bool
	mitigations bool
	yes         bool
	noReboot    bool
	dryRun      bool
	via         string
	output      string
	roles       roleFlags
}

func (a *app) newServerReinstallCmd() *cobra.Command {
	var o installOptions
	sub := &cobra.Command{
		Use:     "reinstall NAME|ID|IP|PROVIDER/ID",
		Aliases: []string{"install"},
		Short:   "Install an operating system on a server",
		Long: `Put an operating system on the server's disks from a UMP base image and
boot it, the way a profile says. The server is brought into the UMP
rescue system first (the way ump rescue does it: as it is when it runs
it already, by a kexec from the running system when that lets UMP in as
root, else through the provider's rescue system), the installer is
fetched from the storage and run there with the profile, the server is
rebooted into the new system, and ump waits until it answers over SSH
with UMP's key and reports what came up.

Every disk of the server is erased. A profile (--profile, default:
default; a file of that name in the profiles directory of the
configuration, else the published one) says which image, which disk
layout (by name from layouts.yaml, or its own), which users with which
keys, which userdata runs in it, what the firewall lets in, and the provision
strategy for roles. The default profile puts Ubuntu 26.04 on
every disk as one linear volume group, gives the sudo user ump UMP's key
and your ~/.ssh/id_*.pub keys (root has no SSH login), lets ICMP and SSH
in and nothing else, and a kernel command line with mitigations=off and
ipv6.disable=1; --mitigations and --ipv6 keep them on (install/README.md
says how to turn them on later). The other flags override the profile for
this one installation.

Without --role, the server ends up as the operating system ready to use
with the profile's userdata, and nothing more. --role (or --roles) names
the roles to provision it with after the first boot, by the profile's
provision strategy; running that is not implemented yet, the roles are
recorded with the server (ump server provision adds roles later;
docs/provisioning.md).

--dry-run brings the server into the rescue system and prints the
installer's plan without touching a disk; --no-reboot leaves the server
in the rescue system after the installation. Asks for confirmation on a
terminal; without one, --yes is required.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installServer(cmd, args[0], o)
		},
	}
	sub.Flags().StringVar(&o.profile, "profile", "", "the profile: a name (profiles/NAME.yaml in the configuration directory, else published) or a file (default: the one set for the server with ump server set, else default)")
	sub.Flags().StringVar(&o.layout, "layout", "", "the disk layout, by name from layouts.yaml (default: the profile's)")
	sub.Flags().StringVar(&o.firewall, "firewall", "", "the firewall, by name from firewalls.yaml (default: the profile's)")
	sub.Flags().StringVar(&o.os, "os", "", "the operating system image (default: the profile's)")
	sub.Flags().StringVar(&o.hostname, "hostname", "", "hostname of the new system (default: the server's name)")
	sub.Flags().StringVar(&o.storage, "storage", "", "base URL of the storage with the installer and the images (default: storage in the configuration)")
	sub.Flags().StringVar(&o.timezone, "timezone", "", "time zone of the new system (default: the profile's)")
	sub.Flags().StringVar(&o.disks, "disks", "", "the disks to use, comma-separated (default: every disk)")
	sub.Flags().StringArrayVar(&o.keyFiles, "ssh-key", nil, "another key for the administrator of the new system and the rescue system on the way: a named key, local, agent, a file or a key line (repeatable)")
	sub.Flags().StringArrayVar(&o.sets, "set", nil, "override any field of the profile, FIELD=VALUE in YAML (kernel.extra=quiet, users[0].shell=/bin/bash, userdata='[ubuntu, web]'; repeatable)")
	sub.Flags().BoolVar(&o.ipv6, "ipv6", false, "keep IPv6 on, whatever the profile says")
	sub.Flags().BoolVar(&o.mitigations, "mitigations", false, "keep the CPU vulnerability mitigations on, whatever the profile says")
	sub.Flags().StringVar(&o.via, "via", "auto", "how to reach the rescue system first: direct, provider, or auto")
	sub.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the installer's plan instead of installing")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "leave the server in the rescue system after the installation")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	o.roles.add(sub, "a role to provision the new system with after the first boot")
	return sub
}

// installResult is what ump install reports.
type installResult struct {
	provider.Machine `yaml:",inline"`
	Profile          string `json:"profile" yaml:"profile"`
	Layout           string `json:"layout" yaml:"layout"`
	OS               string `json:"os" yaml:"os"`
	Hostname         string `json:"hostname" yaml:"hostname"`
	Via              string `json:"via" yaml:"via"`
	DryRun           bool   `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	Rebooted         bool   `json:"rebooted" yaml:"rebooted"`
	// Roles are the roles the server is to be provisioned with.
	Roles []string `json:"roles,omitempty" yaml:"roles,omitempty"`
	// Admin is the user ump (and you) log in as.
	Admin string `json:"admin,omitempty" yaml:"admin,omitempty"`
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
	roles, err := o.roles.list(cmd)
	if err != nil {
		return err
	}
	if o.os != "" && strings.ContainsAny(o.os, "/ ") {
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
	// the rescue system on the way gets UMP's key, the keys chosen for
	// rescue systems and the --ssh-key references; the administrator of
	// the new system gets the --ssh-key ones too (below)
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
	if m.IPv4 == "" && m.IPv6 == "" {
		return fmt.Errorf("%s has no public address", m.Ref())
	}
	a.warnNoRescue(m, prov)
	a.warnPreflight(cmd.Context(), m, prov)
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
	ctx := cmd.Context()
	// the profile, resolved to the specification the installer takes:
	// --profile, else the one set for the server, else default
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return err
	}
	profileName := o.profile
	if profileName == "" {
		profileName = doc.Servers[m.Ref()].Profile
	}
	if profileName == "" {
		profileName = cfg.DefaultProfileName()
	}
	prof, from, err := a.loadProfile(ctx, profileName, storage)
	if err != nil {
		return err
	}
	if o.layout != "" {
		prof.Disks = profile.Disks{Name: o.layout}
	}
	if o.firewall != "" {
		prof.Firewall = profile.FirewallRef{Name: o.firewall}
	}
	if o.os != "" {
		prof.OS = o.os
	}
	if o.timezone != "" {
		prof.Timezone = o.timezone
	}
	if o.ipv6 {
		t := true
		prof.Kernel.IPv6 = &t
	}
	if o.mitigations {
		t := true
		prof.Kernel.Mitigations = &t
	}
	// the named users, when the profile refers to any (or --ssh-key must
	// find the administrator)
	var users map[string]*profile.User
	if profile.HasUserRefs(prof) {
		if users, err = a.loadUsers(ctx, storage); err != nil {
			return err
		}
	}
	if len(o.keyFiles) > 0 {
		addAdminKeys(prof, users, o.keyFiles)
	}
	if len(o.sets) > 0 {
		if prof, err = prof.Set(o.sets); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
	}
	var layouts map[string]*profile.Layout
	if prof.Disks.Layout == nil {
		if layouts, err = a.loadLayouts(ctx, storage); err != nil {
			return err
		}
	}
	var firewalls map[string]*profile.Firewall
	if prof.Firewall.Name != "" {
		if firewalls, err = a.loadFirewalls(ctx, storage); err != nil {
			return err
		}
	}
	var userdata map[string]*profile.Userdata
	if profile.HasUserdataRefs(prof) {
		if userdata, err = a.loadUserdata(ctx, storage); err != nil {
			return err
		}
	}
	spec, err := prof.Resolve(layouts, firewalls, users, userdata, a.keyResolver(cfg))
	if err != nil {
		return err
	}
	spec.Hostname = hostname
	spec.Storage = storage
	if o.disks != "" {
		spec.Disks = strings.Split(o.disks, ",")
	}
	specJSON, err := spec.JSON()
	if err != nil {
		return err
	}
	what := fmt.Sprintf("%s (profile %s from %s, layout %s)", spec.OS, prof.Name, from, spec.Layout.Name)
	question := fmt.Sprintf("Install %s on %s as %s? Every disk of the server is erased.", what, describeRef(m), hostname)
	if noRescueWarning(m, prov) != "" {
		question += " There is no rescue system to fall back on: a failed installation is yours to recover."
	}
	if o.dryRun {
		question = fmt.Sprintf("Bring %s into the rescue system and show the installation plan for %s? (dry run: nothing is erased)", describeRef(m), what)
	}
	logins := a.loginUsers(cfg, m, doc)
	if u := prof.Admin(users); u != "" && u != "root" {
		logins = append([]string{"root", u}, logins[1:]...)
	}
	logins = dedupe(logins)
	// an OS without an image for the machine's architecture stops here,
	// before the reboot, the rescue system or a provider's reinstall
	if err := a.checkArch(ctx, cmd, m, spec.OS, storage, logins, keyPath, o.yes); err != nil {
		return err
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
	// the server is locked for the whole installation: another ump must
	// not reinstall or rescue it meanwhile; other servers are free
	unlock, err := a.lockServer(ctx, m, "reinstall")
	if err != nil {
		return err
	}
	defer unlock()
	// everything on the way is progress (stderr); stdout is for the result
	entry, err := a.bringToImage(cmd, m, prov, keys, keyPath, keyPaths, rescueURL, entryOptions{via: o.via, confirmed: true, quiet: true, stock: !o.dryRun, unguarded: true, users: logins})
	if err != nil {
		return err
	}

	// the installer, from the storage, in the rescue system, with the
	// specification on its stdin
	args := []string{"--spec", "/tmp/ump-install.json"}
	if o.dryRun {
		args = append(args, "--dry-run")
	}
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Installation plan for %s on %s:\n", what, m.Name)
	} else {
		fmt.Fprintf(a.stderr, "Installing %s on %s as %s ...\n", what, m.Name, hostname)
	}
	if len(roles) > 0 {
		fmt.Fprintf(a.stderr, "Roles: %s, by the provision strategy %s after the first boot (not run yet).\n", strings.Join(roles, ", "), spec.Provision.Type)
	}
	installer := storage + "/install/install.sh"
	// the keys go into the rescue system too, so that it stays reachable
	// with them while the installation runs
	remote := ensureKeysCommand(keys) + "cat > /tmp/ump-install.json && " + fetchAndRun(installer, "/tmp/ump-install.sh", args)
	argv := withSSHOptions(sshArgs(entry.user, entry.addr, []string{keyPath}, true, true, []string{remote}), batchOptions(10*time.Second)...)
	var buf bytes.Buffer
	w := io.MultiWriter(a.stderr, &buf)
	code, err := a.sshExec(ctx, argv, bytes.NewReader(specJSON), w, w)
	if err != nil {
		return err
	}
	// the installer ends with 1 (a failed step) or 2 (a bad specification
	// or environment) and says so; anything else without a line of it is
	// the fetch that failed
	installerRan := strings.Contains(buf.String(), "==> ") || strings.Contains(buf.String(), "install.sh:")
	switch {
	case code == 255:
		return withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation; the server is in the UMP rescue system, look there (ump ssh %s --staging)", entry.addr, ref))
	case code != 0 && !installerRan && code != 1 && code != 2:
		return fmt.Errorf("%s could not fetch the installer from %s (exit status %d); the server must reach the storage", entry.addr, installer, code)
	case code == 2:
		return fmt.Errorf("the installer refused the specification or the machine (exit status 2, see above); nothing was changed, %s stays in the UMP rescue system", m.Name)
	case code != 0:
		return fmt.Errorf("the installation on %s failed (exit status %d, see above); the server stays in the UMP rescue system (ump ssh %s --staging)", m.Name, code, ref)
	}
	result := installResult{Machine: m, Profile: prof.Name, Layout: spec.Layout.Name, OS: spec.OS, Hostname: hostname, Via: entry.via, DryRun: o.dryRun, Roles: roles}
	if o.dryRun {
		fmt.Fprintf(a.stderr, "Dry run done; %s stays in the UMP rescue system (ump ssh %s --staging).\n", m.Name, ref)
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
	// providers that boot their own kernel or boot loader by default are
	// told to boot the installed system's
	db, diskBoot := prov.(provider.DiskBooter)
	if diskBoot {
		if err := db.BootFromDisk(ctx, m); err != nil {
			return fmt.Errorf("account %s: %s is installed, but the provider could not be switched to boot it from its disks: %w", m.Account, o.os, err)
		}
	}
	if o.noReboot {
		how := "a reboot"
		if diskBoot {
			how = "a reboot through the provider (ump server reboot " + ref + ")"
		}
		fmt.Fprintf(a.stderr, "Installed; %s stays in the UMP rescue system, %s starts %s.\n", m.Name, how, spec.OS)
		return a.installReport(result, format)
	}

	fmt.Fprintf(a.stderr, "Rebooting %s into %s ...\n", m.Name, spec.OS)
	reboot := withSSHOptions(sshArgs(entry.user, entry.addr, []string{keyPath}, true, true, []string{"systemctl reboot"}), batchOptions(10*time.Second)...)
	// a changed boot configuration takes effect with the provider's reboot
	code, rerr := 0, error(nil)
	if !diskBoot {
		code, rerr = a.sshExec(ctx, reboot, nil, io.Discard, io.Discard)
	}
	if diskBoot || rerr != nil || (code != 0 && code != 255) {
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
	admin := spec.Admin()
	sys, ok := a.installedSystem(ctx, entry.addr, keyPath, admin)
	if !ok {
		return withExit(exitUnreachable, fmt.Errorf("SSH answers on %s, but not as %s with UMP's key; the new system may not have booted, look at the server's console", entry.addr, admin))
	}
	result.System = sys
	result.Admin = admin
	// the old system's host key would make ssh cry man-in-the-middle at
	// the next login; this is the new system ump just installed
	if forgot, err := a.forgetOldHostKey(entry.addr); err == nil && forgot {
		fmt.Fprintf(a.stderr, "Removed the old host key of %s from ~/.ssh/known_hosts: the system is new, and so is its host key.\n", entry.addr)
	}
	// what was installed goes into the state, for the inventory
	record := installRecord(spec.OS, prof.Name, spec.Layout.Name, hostname, admin, sys.Details, a.now())
	if err := a.updateState(ctx, func(d *state.Document) error {
		s := d.Servers[m.Ref()]
		s.Installed = &record
		// a new system has the roles it is installed with, none without
		s.Roles = roles
		d.Servers[m.Ref()] = s
		return nil
	}); err != nil {
		fmt.Fprintf(a.stderr, "ump: the installation is not recorded in the state: %v\n", err)
	}
	fmt.Fprintf(a.stderr, "%s is up: %s, hostname %s, root on %s; log in as %s (ump ssh %s).\n", m.Name, sys.Name, hostname, sys.Root, admin, m.Name)
	if len(roles) > 0 {
		fmt.Fprintln(a.stderr, provisionNotYet(roles))
	} else if spec.Provision.Type != "none" {
		fmt.Fprintf(a.stderr, "The provision strategy %s is recorded in /etc/ump/install on the server; it runs only with roles (--role), and running it is not implemented yet.\n", spec.Provision.Type)
	}
	return a.installReport(result, format)
}

// installRecord is what the state keeps of an installation: the facts
// of /etc/ump/install that the verification read (key=value pairs).
func installRecord(os, profile, layout, hostname, admin, details string, now time.Time) state.Install {
	r := state.Install{OS: os, Profile: profile, Layout: layout, Hostname: hostname, Admin: admin, At: now.UTC().Truncate(time.Second)}
	for _, f := range strings.Fields(details) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "image_version":
			r.ImageVersion = v
		case "image_kernel":
			r.Kernel = v
		case "interfaces":
			r.Interfaces = map[string]string{}
			for _, pair := range strings.Split(v, ",") {
				if n, mac, ok := strings.Cut(pair, "="); ok {
					r.Interfaces[n] = mac
				}
			}
		}
	}
	return r
}

// loadProfile finds a profile: a file when the name is a path, else
// <config>/profiles/NAME.yaml, else <storage>/profiles/NAME.yaml.
func (a *app) loadProfile(ctx context.Context, name, storage string) (*profile.Profile, string, error) {
	if strings.ContainsAny(name, "/\\") || strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
		p, err := profile.Load(expandHome(name))
		if err != nil {
			return nil, "", err
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(name), ".yaml"), ".yml")
		}
		return p, name, nil
	}
	if isNamedFile(name + ".yaml") {
		return nil, "", fmt.Errorf("%s is not a profile: %s.yaml holds the named %s entries (ump config os list shows the profiles)", name, name, name)
	}
	if err := a.ensureProfiles(ctx, storage); err != nil {
		return nil, "", err
	}
	local := filepath.Join(config.ProfilesDir(), name+".yaml")
	if _, err := os.Stat(local); err == nil {
		p, err := profile.Load(local)
		if err != nil {
			return nil, "", err
		}
		if p.Name == "" {
			p.Name = name
		}
		return p, config.AbbreviateHome(local), nil
	}
	url := storage + "/profiles/" + name + ".yaml"
	data, err := a.fetch(ctx, url)
	if err != nil {
		return nil, "", fmt.Errorf("profile %q: not in %s, and %w", name, config.AbbreviateHome(config.ProfilesDir()), err)
	}
	p, err := profile.Parse(data)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", url, err)
	}
	if p.Name == "" {
		p.Name = name
	}
	return p, "the storage", nil
}

// fetch gets a small text file from the storage.
func (a *app) fetch(ctx context.Context, url string) ([]byte, error) {
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// keyResolver resolves the key references of a profile: ump (UMP's own
// key), local (the public keys next to the usual ~/.ssh/id_* files),
// agent (what ssh-add -L lists), a file, or a key line.
func (a *app) keyResolver(cfg *config.Config) profile.KeyResolver {
	return func(ref string) ([]string, error) {
		switch ref {
		case "ump":
			k, _, err := a.stagingKey(cfg)
			if err != nil {
				return nil, err
			}
			return []string{k.Line()}, nil
		case "local":
			var keys []string
			for _, id := range sshkey.DefaultIdentityFiles() {
				if data, err := os.ReadFile(id + ".pub"); err == nil {
					keys = append(keys, strings.Split(string(data), "\n")...)
				}
			}
			return keys, nil
		case "agent":
			keys, err := a.agentKeys()
			if err != nil {
				return nil, err
			}
			return keys, nil
		}
		if profile.IsKeyLine(ref) {
			return []string{ref}, nil
		}
		if !strings.ContainsAny(ref, "/~ ") && !strings.HasSuffix(ref, ".pub") {
			keys, err := namedKeys()
			if err != nil {
				return nil, err
			}
			if k, ok := keys[ref]; ok {
				return []string{k}, nil
			}
			return nil, fmt.Errorf("not a key reference (ump, local, agent, a named key from ump config key list, a file, or a public key line)")
		}
		if strings.HasPrefix(ref, "~") || strings.Contains(ref, "/") || strings.HasSuffix(ref, ".pub") {
			data, err := os.ReadFile(expandHome(ref))
			if err != nil {
				return nil, err
			}
			return strings.Split(string(data), "\n"), nil
		}
		return nil, errors.New("not a key reference (ump, local, agent, a named key, a file, or a public key line)")
	}
}

// addAdminKeys adds --ssh-key files to the administrator's keys (the
// first sudo user, else root), or a root user with them when the profile
// has neither. A named administrator becomes an inline copy with the
// keys added, so that users.yaml stays as it is.
func addAdminKeys(p *profile.Profile, users map[string]*profile.User, files []string) {
	admin := p.Admin(users)
	for i := range p.Users {
		r := &p.Users[i]
		switch {
		case r.User != nil && r.User.Name == admin:
			r.User.Keys = append(r.User.Keys, files...)
			return
		case r.User == nil && r.Name == admin && users[admin] != nil:
			u := *users[admin]
			u.Keys = append(append([]string{}, u.Keys...), files...)
			*r = profile.UserRef{User: &u}
			return
		}
	}
	p.Users = append(p.Users, profile.UserRef{User: &profile.User{Name: "root", Keys: append([]string{"ump"}, files...)}})
}

// dedupe keeps the first of every repeated string.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// sshAgentKeys lists the public keys of the running ssh-agent.
func sshAgentKeys() ([]string, error) {
	out, err := exec.Command("ssh-add", "-L").Output()
	if err != nil {
		return nil, errors.New("ssh-add -L lists no keys (is an agent running, with keys loaded?)")
	}
	return strings.Split(string(out), "\n"), nil
}

func (a *app) installReport(r installResult, format outputFormat) error {
	if format == "" {
		return nil
	}
	return printEncoded(a.stdout, r, format)
}

// installedSystem asks the machine, as user with UMP's key, what it runs
// now.
func (a *app) installedSystem(ctx context.Context, addr, keyPath, user string) (*installedSystem, bool) {
	const query = `sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'; findmnt -no SOURCE / ; cat /etc/ump/install 2>/dev/null | tr '\n' ' '`
	argv := withSSHOptions(sshArgs(user, addr, []string{keyPath}, true, true, []string{query}), batchOptions(10*time.Second)...)
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

// ensureKeysCommand is a shell command prefix that adds the keys to
// root's authorized_keys on the machine, those that are not there yet.
func ensureKeysCommand(keys []provider.PublicKey) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, shellJoin([]string{k.Data}))
	}
	return `mkdir -p -m 700 /root/.ssh && touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && for k in ` + strings.Join(parts, " ") + `; do grep -qxF "$k" /root/.ssh/authorized_keys || echo "$k" >> /root/.ssh/authorized_keys; done; `
}

// forgetOldHostKey removes the address's entries from ~/.ssh/known_hosts
// (ssh-keygen keeps the previous file as known_hosts.old) and says
// whether there were any.
func (a *app) forgetOldHostKey(addr string) (bool, error) {
	if a.forgetHostKey != nil {
		return a.forgetHostKey(addr)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(path); err != nil {
		return false, nil
	}
	// -F finds hashed entries too
	if err := exec.Command("ssh-keygen", "-F", addr, "-f", path).Run(); err != nil {
		return false, nil
	}
	if out, err := exec.Command("ssh-keygen", "-R", addr, "-f", path).CombinedOutput(); err != nil {
		return false, fmt.Errorf("ssh-keygen -R %s: %v: %s", addr, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// checkArch keeps an installation from starting that cannot end well: the
// storage has no image of the OS for the machine's architecture (Arch
// Linux is x86_64 only), and the installer would find that out in the
// rescue system, after the machine was rebooted into it or, without a
// rescue system, reinstalled by its provider. The architecture comes
// from the provider, else from the running system over SSH; when neither
// tells, ump warns and asks. An OS with an image for every architecture,
// or a storage that cannot be read, passes without a word (the installer
// still checks before it touches a disk).
func (a *app) checkArch(ctx context.Context, cmd *cobra.Command, m provider.Machine, os, storage string, logins []string, keyPath string, yes bool) error {
	bad := map[string]string{}
	good := 0
	for _, arch := range []string{"amd64", "arm64"} {
		data, err := a.fetch(ctx, storage+"/os/"+os+"/"+arch+"/manifest.json")
		switch {
		case err != nil && strings.HasSuffix(err.Error(), "HTTP 404"):
			bad[arch] = "the storage has no image of " + os + " for " + arch
		case err != nil:
			return nil
		default:
			var mf struct {
				Unsupported string `json:"unsupported"`
			}
			if json.Unmarshal(data, &mf) == nil && mf.Unsupported != "" {
				bad[arch] = mf.Unsupported
			} else {
				good++
			}
		}
	}
	if len(bad) == 0 || good == 0 {
		return nil
	}
	arch, how := m.Arch, "as the provider says"
	if arch == "" && m.IPv4 != "" {
		if _, p, ok, _ := a.probeAdmin(ctx, logins, m.IPv4, []string{keyPath}, false); ok {
			arch, how = p.arch, "as its running system says"
		}
	}
	if arch != "" {
		if reason, no := bad[arch]; no {
			return withExit(exitUsage, fmt.Errorf("%s cannot be installed on %s, which is %s (%s): %s. Nothing was changed", os, m.Name, arch, how, reason))
		}
		return nil
	}
	var only []string
	for _, x := range []string{"amd64", "arm64"} {
		if _, no := bad[x]; !no {
			only = append(only, x)
		}
	}
	fmt.Fprintf(a.stderr, "Warning: %s exists for %s only, and ump cannot tell the architecture of %s (its provider does not say, and its running system does not let ump in). On another architecture the installation stops in the rescue system, after the server was rebooted into it or, without a rescue system, reinstalled by its provider: a wiped server and no new system.\n", os, strings.Join(only, " and "), m.Name)
	return a.confirm(cmd, yes, fmt.Sprintf("Is %s an %s machine?", m.Name, strings.Join(only, " or ")))
}
