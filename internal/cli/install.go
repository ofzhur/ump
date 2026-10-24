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
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

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
	users       []string
	userdata    []string
	sets        []string
	ipv6        bool
	mitigations bool
	yes         bool
	noReboot    bool
	dryRun      bool
	via         string
	output      string
	groups      groupFlags
	noProvision bool
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
keys, which userdata runs in it and what the firewall lets in. The
default profile puts Ubuntu 26.04 on every disk as one linear volume
group, gives the sudo user ump UMP's key and your ~/.ssh/id_*.pub keys
(root has no SSH login), lets ICMP and SSH in and nothing else, and a
kernel command line with mitigations=off and ipv6.disable=1;
--mitigations and --ipv6 keep them on (install/README.md says how to
turn them on later). The other flags override the profile for this one
installation: --user replaces its users, each with sudo (NAME:KEY,...
with those key references, NAME alone as in users.yaml; the first, root
aside, is the administrator, the user ump logs in as, and gets UMP's
key; root takes its keys alone), --userdata its userdata, by name from
userdata.yaml, and --set FIELD=VALUE any field, after the other flags.

After the first boot ump provisions the server from the infrastructure
repository it runs in: ansible.cfg and inventory/ in the current
directory and ansible-playbook in PATH, checked before anything is
touched. The server is in the group common and the groups given with
--group, in order: its run list. Each group is a playbook,
playbooks/GROUP.yml of the repository when it has one, else the
collection's ofzhur.ansible.GROUP; ansible-playbook --syntax-check runs
on them first, and ansible-inventory checks that no host and group share
a name. ump writes inventory/hosts/HOSTNAME.yml and runs the run list
for the server alone, as its administrator with UMP's key only; where a
file of inventory/ says plugin: ofzhur.ansible.ump, ump is the inventory
and writes no host file (one in inventory/hosts/ for the server stops
it). Exit status 6 means the run list did not complete: the server is
installed and recorded; fix that and run ump server provision NAME.
--no-provision runs nothing and needs no layout; where the layout is
there, the host file is still written once ansible-inventory has
checked the names. Where ump is the inventory, the server is in it from
its record on; a record that fails is printed, to add by hand, and
nothing runs (exit status 6). docs/provisioning.md describes the
repository.

--dry-run brings the server into the rescue system and prints the
installer's plan and the run list without touching a disk or the
inventory; --no-reboot leaves the server in the rescue system after the
installation, recorded but not provisioned (ump server provision NAME
after the reboot). Asks for confirmation on a terminal; without one,
--yes is required.`,
		Example: `  ump server reinstall web-01 --dry-run
  ump server reinstall web-01 --group web
  ump server reinstall web-01 --profile web --group web,monitoring --yes
  ump server reinstall web-01 --user oleg:local --user deploy --userdata ubuntu,web
  ump server reinstall lab-01 --no-provision`,
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
	sub.Flags().StringArrayVar(&o.users, "user", nil, "a user of the new system, with sudo, instead of the profile's: NAME:KEY[,KEY...] with key references (ump, local, agent, a named key, a file, a key line), or NAME from users.yaml; the first, root aside, is the administrator and gets UMP's key (repeatable)")
	sub.Flags().StringArrayVar(&o.userdata, "userdata", nil, "the userdata instead of the profile's, by name from userdata.yaml, in order (repeatable, or comma-separated)")
	sub.Flags().StringArrayVar(&o.sets, "set", nil, "override any field of the profile, after the other flags, FIELD=VALUE in YAML (kernel.extra=quiet, users[0].shell=/bin/bash, userdata='[ubuntu, web]'; repeatable)")
	sub.Flags().BoolVar(&o.ipv6, "ipv6", false, "keep IPv6 on, whatever the profile says")
	sub.Flags().BoolVar(&o.mitigations, "mitigations", false, "keep the CPU vulnerability mitigations on, whatever the profile says")
	sub.Flags().StringVar(&o.via, "via", "auto", "how to reach the rescue system first: direct, provider, or auto")
	sub.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the installer's plan instead of installing")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "leave the server in the rescue system after the installation")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	sub.Flags().BoolVar(&o.noProvision, "no-provision", false, "do not provision the new system: no Ansible layout needed, no run list")
	o.groups.add(sub, "a group of the new system besides common")
	return sub
}

// installResult is what ump install reports: the machine with the os,
// profile, layout and groups of this installation.
type installResult struct {
	provider.Machine `yaml:",inline"`
	Hostname         string `json:"hostname" yaml:"hostname"`
	Via              string `json:"via" yaml:"via"`
	DryRun           bool   `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	Rebooted         bool   `json:"rebooted" yaml:"rebooted"`
	// RunList is the server's groups, common first; Playbooks what they
	// resolve to, when ump provisions; Provisioned says that the run
	// list completed.
	RunList     []string `json:"run_list" yaml:"run_list"`
	Playbooks   []string `json:"playbooks,omitempty" yaml:"playbooks,omitempty"`
	Provisioned bool     `json:"provisioned" yaml:"provisioned"`
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
	groups, err := o.groups.list(cmd)
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
	givenUsers, err := parseUserFlags(cmd, o.users)
	if err != nil {
		return err
	}
	givenUserdata, err := parseUserdataFlags(cmd, o.userdata)
	if err != nil {
		return err
	}
	// provisioning needs the infrastructure repository, checked before
	// anything else; --no-provision needs none, and puts the server into
	// the inventory of the one there is when it can check the names there
	var repo *infraRepo
	var rl runList
	var inv *inventory
	var unchecked string
	if o.noProvision {
		repo, unchecked = a.inventoryRepo()
	} else if repo, err = a.requireRepo(", or give --no-provision"); err != nil {
		return err
	}
	if repo != nil {
		rl = repo.runList(groups)
		if !o.noProvision {
			if err := a.syntaxCheck(cmd.Context(), repo, rl); err != nil {
				return err
			}
		}
		if inv, err = a.readInventory(cmd.Context(), repo); err != nil {
			return err
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
	hostname := o.hostname
	if hostname == "" {
		hostname = rescueHostname(m.Name)
		if hostname == "" {
			return fmt.Errorf("the name %q of %s is not a valid hostname; give one with --hostname", m.Name, m.Ref())
		}
	}
	// the host file goes where no other's is, under a name that is no group
	if repo != nil {
		if err := repo.checkHost(hostname, m.Ref(), rl, "give another --hostname"); err != nil {
			return err
		}
		if err := inv.check(hostname, m.Ref(), rl, "give another --hostname"); err != nil {
			return err
		}
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
	// the named users, when the profile or --user refers to any (or
	// --ssh-key must find the administrator)
	var users map[string]*profile.User
	if (len(givenUsers) == 0 && profile.HasUserRefs(prof)) || namesUsers(givenUsers) {
		if users, err = a.loadUsers(ctx, storage); err != nil {
			return err
		}
	}
	if len(givenUsers) > 0 {
		if err := replaceUsers(prof, givenUsers, users); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
	}
	var userdata map[string]*profile.Userdata
	if len(givenUserdata) > 0 {
		if userdata, err = a.loadUserdata(ctx, storage); err != nil {
			return err
		}
		prof.Userdata = nil
		for _, n := range givenUserdata {
			if userdata[n] == nil {
				return usageErrorf(cmd, "--userdata %s: not in userdata.yaml (known: %s; ump config userdata create %s makes it)", n, strings.Join(sortedKeys(userdata), ", "), n)
			}
			prof.Userdata = append(prof.Userdata, profile.UserdataRef{Name: n})
		}
	}
	if len(o.keyFiles) > 0 {
		addAdminKeys(prof, users, o.keyFiles)
	}
	// --set comes last, over what the other flags made
	if len(o.sets) > 0 {
		if prof, err = prof.Set(o.sets); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
		if users == nil && profile.HasUserRefs(prof) {
			if users, err = a.loadUsers(ctx, storage); err != nil {
				return err
			}
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
	if userdata == nil && profile.HasUserdataRefs(prof) {
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
	if !o.noProvision {
		fmt.Fprintf(a.stderr, "Run list after the first boot: %s.\n", rl)
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
	result := installResult{Machine: m, Hostname: hostname, Via: entry.via, DryRun: o.dryRun, RunList: append([]string{commonGroup}, groups...)}
	// the server's os, profile, layout and groups are those of this
	// installation, its groups as in the inventory; installed_at comes
	// with the record
	result.OS, result.Profile, result.Layout = spec.OS, prof.Name, spec.Layout.Name
	result.Groups, result.InstalledAt = slices.Clone(result.RunList), nil
	if !o.noProvision {
		result.Playbooks = rl.playbooks
	}
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
		// the disks hold the new system: it is recorded, and the old host
		// key goes, so that ump server provision works after the reboot
		admin := spec.Admin()
		record := installRecord(spec.OS, prof.Name, spec.Layout.Name, hostname, admin, "", a.now())
		result.InstalledAt, result.Admin = &record.At, admin
		recorded := a.recordInstallation(ctx, m, record, groups)
		a.forgetReplacedHostKey(entry.addr)
		how := "a reboot"
		if diskBoot {
			how = "a reboot through the provider (ump server reboot " + ref + ")"
		}
		fmt.Fprintf(a.stderr, "Installed; %s stays in the UMP rescue system, %s starts %s.\n", m.Name, how, spec.OS)
		switch {
		case !recorded:
			fmt.Fprintf(a.stderr, "Not provisioned: after the reboot, record it and run ump server provision %s from the infrastructure repository.\n", ref)
		case repo != nil && repo.plugin != "":
			fmt.Fprintf(a.stderr, "Not provisioned: %s makes ump the inventory, which has %s from its record on; after the reboot, ump server provision %s runs its run list.\n", repo.plugin, hostname, ref)
		default:
			fmt.Fprintf(a.stderr, "Not provisioned: after the reboot, ump server provision %s, run from the infrastructure repository, puts %s into the inventory and runs its run list (where ump is the inventory, %s is in it from its record on).\n", ref, hostname, hostname)
		}
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
	a.forgetReplacedHostKey(entry.addr)
	// what was installed goes into the state, for the inventory
	record := installRecord(spec.OS, prof.Name, spec.Layout.Name, hostname, admin, sys.Details, a.now())
	result.InstalledAt = &record.At
	recorded := a.recordInstallation(ctx, m, record, groups)
	fmt.Fprintf(a.stderr, "%s is up: %s, hostname %s, root on %s; log in as %s (ump ssh %s).\n", m.Name, sys.Name, hostname, sys.Root, admin, m.Name)
	// the new system goes into the inventory and gets its run list; ump
	// as the inventory lists the record, and without it the old one
	vars := a.inventoryVars(cfg, m, record)
	switch {
	case !recorded && repo != nil && repo.plugin != "" && !o.noProvision:
		if err := a.installReport(result, format); err != nil {
			return err
		}
		return withExit(exitProvision, fmt.Errorf("provisioning failed: %s is installed but not recorded, and %s makes ump the inventory, which lists the old record; record it and run ump server provision %s", hostname, repo.plugin, ref))
	case !recorded && repo != nil && repo.plugin != "":
		fmt.Fprintf(a.stderr, "Not provisioned (--no-provision), and not in the inventory as installed: %s makes ump the inventory, which lists the old record; record it.\n", repo.plugin)
	case !o.noProvision:
		if err := a.provisionHost(ctx, repo, rl, hostname, vars, keyPath); err != nil {
			// the result still says what was installed
			if rerr := a.installReport(result, format); rerr != nil {
				return rerr
			}
			return a.provisionFailed(ctx, err, "the server is installed and recorded, its run list did not complete", "run ump server provision "+ref)
		}
		result.Provisioned = true
		fmt.Fprintf(a.stderr, "%s is provisioned: %s.\n", hostname, strings.Join(rl.groups, ", "))
	case repo != nil:
		if _, err := a.putInInventory(repo, hostname, rl.groups, vars); err != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
			break
		}
		fmt.Fprintf(a.stderr, "Not provisioned (--no-provision); ump server provision %s runs its run list.\n", ref)
	default:
		why := "no Ansible layout here"
		if unchecked != "" {
			why = unchecked
		}
		fmt.Fprintf(a.stderr, "No host file for %s (%s): ump server provision %s, run from the infrastructure repository, writes it", hostname, why, ref)
		if recorded {
			fmt.Fprintf(a.stderr, "; where ump is the inventory, %s is in it from its record on", hostname)
		}
		fmt.Fprintln(a.stderr, ".")
	}
	return a.installReport(result, format)
}

// recordInstallation puts the installation into the state, with the
// groups: a new system has the groups it is installed with, none
// without --group. A failure is told, with the record to add by hand,
// and reported: the server is installed either way, but the state has
// the old record.
func (a *app) recordInstallation(ctx context.Context, m provider.Machine, record state.Install, groups []string) bool {
	err := a.updateState(ctx, func(d *state.Document) error {
		s := d.Servers[m.Ref()]
		s.Installed = &record
		s.Groups = groups
		d.Servers[m.Ref()] = s
		return nil
	})
	if err == nil {
		return true
	}
	fmt.Fprintf(a.stderr, "ump: the installation is not recorded in the state: %v\n", err)
	var entry bytes.Buffer
	enc := yaml.NewEncoder(&entry)
	enc.SetIndent(2)
	enc.Encode(map[string]map[string]state.Server{"servers": {m.Ref(): {Installed: &record, Groups: groups}}})
	fmt.Fprintf(a.stderr, "Record it by hand: in state.yaml (ump config state view says where), the server's installed and groups are these:\n%s", entry.String())
	return false
}

// forgetReplacedHostKey removes the old system's host key of the
// address, which would make ssh cry man-in-the-middle at the next login:
// the disks hold the system ump just installed.
func (a *app) forgetReplacedHostKey(addr string) {
	if forgot, err := a.forgetOldHostKey(addr); err == nil && forgot {
		fmt.Fprintf(a.stderr, "Removed the old host key of %s from ~/.ssh/known_hosts: the system is new, and so is its host key.\n", addr)
	}
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

// userFlag is one --user: the name, and the key references after the
// first ':'; none means the user of that name from users.yaml.
type userFlag struct {
	name string
	keys []string
}

// parseUserFlags reads --user NAME[:KEY[,KEY...]], in order: the name is
// a user name, given once, and every key reference is there.
func parseUserFlags(cmd *cobra.Command, values []string) ([]userFlag, error) {
	var out []userFlag
	for _, v := range values {
		name, refs, withKeys := strings.Cut(v, ":")
		u := userFlag{name: strings.TrimSpace(name)}
		if err := (&profile.User{Name: u.name, Keys: []string{"ump"}}).Validate(); err != nil {
			return nil, usageErrorf(cmd, "--user %q: %v", v, err)
		}
		for _, given := range out {
			if given.name == u.name {
				return nil, usageErrorf(cmd, "--user %s is given twice", u.name)
			}
		}
		if withKeys {
			for _, k := range strings.Split(refs, ",") {
				if k = strings.TrimSpace(k); k == "" {
					return nil, usageErrorf(cmd, "--user %q: an empty key reference (NAME:KEY[,KEY...], or NAME alone for the user of users.yaml)", v)
				}
				u.keys = append(u.keys, k)
			}
		}
		out = append(out, u)
	}
	return out, nil
}

// namesUsers reports whether a --user is to be taken from users.yaml.
func namesUsers(given []userFlag) bool {
	for _, u := range given {
		if len(u.keys) == 0 {
			return true
		}
	}
	return false
}

// replaceUsers makes the --user users the profile's, inline and in
// order: each with sudo and its key references, or as its entry of
// users.yaml with sudo; root, as in a profile, with its keys alone. The
// administrator, the first that is not root (else root), gets UMP's key
// when it lacks it, so that ump reaches the new system.
func replaceUsers(p *profile.Profile, given []userFlag, users map[string]*profile.User) error {
	p.Users = make([]profile.UserRef, 0, len(given))
	for _, g := range given {
		u := profile.User{Name: g.name, Keys: slices.Clone(g.keys)}
		if len(g.keys) == 0 {
			def, ok := users[g.name]
			if !ok {
				return fmt.Errorf("--user %s: not in users.yaml (known: %s); give its keys, --user %s:KEY[,KEY...]", g.name, strings.Join(sortedKeys(users), ", "), g.name)
			}
			u = *def
			u.Groups, u.Keys = slices.Clone(def.Groups), slices.Clone(def.Keys)
		}
		if u.Name == "root" {
			u = profile.User{Name: "root", Keys: u.Keys}
		} else {
			u.Sudo = true
		}
		p.Users = append(p.Users, profile.UserRef{User: &u})
	}
	admin := p.Admin(nil)
	for _, r := range p.Users {
		if r.User.Name == admin && !slices.Contains(r.User.Keys, "ump") {
			r.User.Keys = append([]string{"ump"}, r.User.Keys...)
		}
	}
	return nil
}

// parseUserdataFlags reads --userdata NAME[,NAME...], in order: names of
// entries of userdata.yaml, each once.
func parseUserdataFlags(cmd *cobra.Command, values []string) ([]string, error) {
	var out []string
	for _, v := range values {
		for _, n := range strings.Split(v, ",") {
			n = strings.TrimSpace(n)
			switch {
			case n == "":
				continue
			case !profile.IsUserdataName(n):
				return nil, usageErrorf(cmd, "--userdata %q: %q is not a userdata name", v, n)
			case slices.Contains(out, n):
				return nil, usageErrorf(cmd, "--userdata %s is given twice", n)
			}
			out = append(out, n)
		}
	}
	if len(values) > 0 && len(out) == 0 {
		return nil, usageErrorf(cmd, "--userdata names no entry (--set userdata=[] runs none)")
	}
	return out, nil
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
