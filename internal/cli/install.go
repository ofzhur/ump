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
	"sync"
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
	hosts       string
	playbooks   listFlag

	// privateNetworks is --private-network (names, or none), privateIPs
	// --private-ip (NETWORK=ADDRESS, or an address alone)
	privateNetworks []string
	privateIPs      []string
}

// serverFlags are the flags of reinstall that concern the installation
// on one server, by name: multiinstall takes them for every server, and
// its plan file per server, under these names.
var serverFlags = []string{"profile", "layout", "firewall", "os", "hostname", "private-network", "private-ip", "timezone", "disks", "ssh-key", "set", "user", "userdata", "group", "ipv6", "mitigations", "via", "storage"}

// addServerFlags adds the flags of serverFlags.
func addServerFlags(sub *cobra.Command, o *installOptions) {
	sub.Flags().StringVar(&o.profile, "profile", "", "the profile: a name (profiles/NAME.yaml in the configuration directory, else published) or a file (default: the one set for the server with ump server set, else default)")
	sub.Flags().StringVar(&o.layout, "layout", "", "the disk layout, by name from layouts.yaml (default: the profile's)")
	sub.Flags().StringVar(&o.firewall, "firewall", "", "the firewall, by name from firewalls.yaml (default: the profile's)")
	sub.Flags().StringVar(&o.os, "os", "", "the operating system image (default: the profile's)")
	sub.Flags().StringVar(&o.hostname, "hostname", "", "hostname of the new system (default: the server's name)")
	sub.Flags().StringArrayVar(&o.privateNetworks, "private-network", nil, "the private networks, by name from networks.yaml, in order, or none (repeatable, or comma-separated; default: the profile's)")
	sub.Flags().StringArrayVar(&o.privateIPs, "private-ip", nil, "the server's address in a private network, NETWORK=ADDRESS, or ADDRESS alone in its only one (repeatable, or comma-separated; default: the one it has there, else the lowest free one)")
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
	o.groups.add(sub, "group", "a group of the new system besides common")
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

The server gets an address in each of the profile's private networks
(the published profiles name default, 10.0.0.0/24, from networks.yaml;
a profile without network.private gives none), or in those of
--private-network NAME[,NAME] (none: no address): the one --private-ip
NETWORK=ADDRESS gives (ADDRESS alone in the server's only network), else
the one it has there, else the lowest free one; never another server's.
They are checked with everything else, recorded in the state before the
installation starts (an installation that fails before the installer
wrote the disks gives them back), go to the installer, and into the
inventory as ump_private_networks, with ump_private_address,
ump_private_network and ump_private_cidr of the first network; nothing
configures an interface yet (docs/networks.md).

After the first boot ump provisions from the infrastructure repository
it runs in: ansible.cfg, inventory/ and playbooks/ in the current
directory and ansible-playbook in PATH, checked before anything is
touched. The server is in the group common and the groups given with
--group. ansible-playbook --syntax-check runs on the playbooks first,
and ansible-inventory checks that no host and group share a name and
that --hosts matches a host with the server in it. Once the new system
is up and recorded, ump provision runs: the host files of every server
ump installed (into inventory/hosts/, or the storage a file of
inventory/ points to with url:, such as inventory/hosts.yml; none where
one says plugin: ofzhur.ansible.ump, and ump is the inventory), then
every playbooks/*.yml on the whole fleet, each host logged in to as its
administrator with UMP's key only; the plays pick their hosts by their
groups. --hosts PATTERN and --playbook NAME narrow it, as for ump
provision; the confirmation covers it. Exit status 6 names the hosts
that failed: the server is installed and recorded, and provisioned
unless it is one of them; fix that and run ump provision. --no-provision
runs nothing and needs no layout; where the layout is there, the host
file is still written, under the fleet's lock, once ansible-inventory
has checked the names, and the next ump provision includes the server.
An installation that cannot be recorded is printed, to add by hand, and
not provisioned (exit status 6). docs/provisioning.md describes the
repository; ump server multiinstall installs several servers at once.

--dry-run brings the server into the rescue system and prints the
installer's plan and the playbooks without touching a disk or the
inventory; --no-reboot leaves the server in the rescue system after the
installation, recorded but not provisioned (ump provision --hosts
HOSTNAME after the reboot). Asks for confirmation on a terminal;
without one, --yes is required.`,
		Example: `  ump server reinstall web-01 --dry-run
  ump server reinstall web-01 --group web
  ump server reinstall web-01 --profile web --group web,monitoring --yes
  ump server reinstall web-01 --group web --hosts web-01
  ump server reinstall web-01 --user oleg:local --user deploy --userdata ubuntu,web
  ump server reinstall db-01 --private-ip 10.0.0.10
  ump server reinstall db-01 --private-network default,db --private-ip db=10.0.1.10
  ump server reinstall lab-01 --no-provision --private-network none`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.installServer(cmd, args[0], o)
		},
	}
	addServerFlags(sub, &o)
	sub.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the installer's plan instead of installing")
	sub.Flags().BoolVar(&o.noReboot, "no-reboot", false, "leave the server in the rescue system after the installation")
	sub.Flags().BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
	sub.Flags().StringVarP(&o.output, "output", "o", "", "print the result as json or yaml")
	sub.Flags().BoolVar(&o.noProvision, "no-provision", false, "do not provision: no Ansible layout needed, no playbook runs")
	hostsFlag(sub, &o.hosts, "an Ansible host pattern: provision the hosts it matches alone (default: the whole fleet)")
	sub.Flags().StringArrayVar((*[]string)(&o.playbooks), "playbook", nil, "a playbook of playbooks/, without .yml: provision with those alone (repeatable, or comma-separated; default: every one)")
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
	// Playbooks are those the provisioning runs, when ump provisions;
	// Provisioned says that it completed, Provision what it reported.
	Playbooks   []string         `json:"playbooks,omitempty" yaml:"playbooks,omitempty"`
	Provisioned bool             `json:"provisioned" yaml:"provisioned"`
	Provision   *provisionResult `json:"provision,omitempty" yaml:"provision,omitempty"`
	// Admin is the user ump (and you) log in as.
	Admin string `json:"admin,omitempty" yaml:"admin,omitempty"`
	// System is what answered after the reboot.
	System *installedSystem `json:"system,omitempty" yaml:"system,omitempty"`
	// written says that the installer wrote the disks, or may have (the
	// connection ended while it ran): the private addresses are the new
	// system's then
	written bool
}

type installedSystem struct {
	Name    string `json:"name" yaml:"name"`
	Root    string `json:"root" yaml:"root"`
	Details string `json:"details,omitempty" yaml:"details,omitempty"`
}

// installFlags are the flags of one installation as checked: the groups
// of --group (common aside), the users of --user, the userdata of
// --userdata, the private networks and addresses.
type installFlags struct {
	groups   []string
	users    []userFlag
	userdata []string
	private  privateFlags
}

// checkServerFlags checks what the flags of one installation say by
// themselves (exit status 2).
func checkServerFlags(cmd *cobra.Command, o installOptions) (installFlags, error) {
	var f installFlags
	switch o.via {
	case "", "auto", "direct", "provider":
	default:
		return f, usageErrorf(cmd, "unknown --via %q (use one of auto, direct, provider)", o.via)
	}
	var err error
	if f.groups, err = o.groups.list(cmd); err != nil {
		return f, err
	}
	if o.os != "" && strings.ContainsAny(o.os, "/ ") {
		return f, usageErrorf(cmd, "invalid --os %q", o.os)
	}
	if o.hostname != "" {
		if err := validateServerName(o.hostname); err != nil {
			return f, usageErrorf(cmd, "%v", err)
		}
	}
	if f.private, err = checkPrivateFlags(cmd, o.privateNetworks, o.privateIPs); err != nil {
		return f, err
	}
	if f.users, err = parseUserFlags(cmd, o.users); err != nil {
		return f, err
	}
	if f.userdata, err = parseUserdataFlags(cmd, o.userdata); err != nil {
		return f, err
	}
	return f, nil
}

// installRepo is the infrastructure repository that the provisioning
// after an installation needs, checked before anything else: with the
// run, whose playbooks pass the syntax check, and the inventory.
// --no-provision needs none, and puts the servers into the inventory of
// the one there is when it can check the names there; unchecked says why
// it cannot.
func (a *app) installRepo(ctx context.Context, noProvision bool, named []string, pattern string) (repo *infraRepo, run fleetRun, inv *inventory, unchecked string, err error) {
	if noProvision {
		if repo, unchecked, err = a.inventoryRepo(); err != nil {
			return nil, run, nil, "", err
		}
	} else {
		var all []string
		if repo, all, err = a.requireRepo(", or give --no-provision"); err != nil {
			return nil, run, nil, "", err
		}
		if run.playbooks, err = choosePlaybooks(all, named); err != nil {
			return nil, run, nil, "", err
		}
		run.narrowed, run.pattern = len(named) > 0, pattern
		if err := a.syntaxCheck(ctx, repo, run.playbooks); err != nil {
			return nil, run, nil, "", err
		}
	}
	if repo != nil {
		if inv, err = a.readInventory(ctx, repo, ""); err != nil {
			return nil, run, nil, "", err
		}
	}
	return repo, run, inv, unchecked, nil
}

// installPlan is an installation on one server, resolved and checked:
// what runInstall runs.
type installPlan struct {
	// ref is the server as the user named it, m and prov the server
	ref  string
	m    provider.Machine
	prov provider.Provider
	// hostname is the new system's, groups are its groups of --group
	// (common aside)
	hostname string
	groups   []string
	via      string
	// keys go into the rescue system on the way, keyPaths name them,
	// keyPath is UMP's key
	keys     []provider.PublicKey
	keyPaths []string
	keyPath  string
	// storage serves the installer and the images, rescueURL the rescue
	// image
	storage   string
	rescueURL string
	// profile is the profile's name and from where it came, spec what
	// it resolved to for the installer
	profile  string
	from     string
	spec     *profile.Spec
	specJSON []byte
	// privateNets are the private networks (none: none) and privateIPs
	// the addresses given by hand, by network; private are the addresses
	// the server gets (assignPlans), had those it had then; reserved says
	// that reservePrivate recorded them, over before
	privateNets []*profile.PrivateNetwork
	privateIPs  map[string]string
	private     []state.Private
	had         []state.Private
	reserved    bool
	before      []state.Private
	// logins are the users tried on the running system
	logins []string
}

// newInstallPlan starts the plan of an installation on m: the server
// needs an address, and the new system's hostname is --hostname, else
// the server's name.
func newInstallPlan(ref string, m provider.Machine, prov provider.Provider, o installOptions, f installFlags) (*installPlan, error) {
	if m.IPv4 == "" && m.IPv6 == "" {
		return nil, fmt.Errorf("%s has no public address", m.Ref())
	}
	hostname := o.hostname
	if hostname == "" {
		hostname = rescueHostname(m.Name)
		if hostname == "" {
			return nil, fmt.Errorf("the name %q of %s is not a valid hostname; give one with --hostname", m.Name, m.Ref())
		}
	}
	return &installPlan{ref: ref, m: m, prov: prov, hostname: hostname, groups: f.groups, via: o.via}, nil
}

// inGroups are the groups of the new system in the inventory, common
// first.
func (p *installPlan) inGroups() []string {
	return append([]string{commonGroup}, p.groups...)
}

// result is the result of the installation before it runs: the server
// with the os, profile, layout and groups of this installation, its
// groups as in the inventory; installed_at comes with the record.
func (p *installPlan) result(dryRun bool) installResult {
	r := installResult{Machine: p.m, Hostname: p.hostname, DryRun: dryRun}
	r.OS, r.Profile, r.Layout = p.spec.OS, p.profile, p.spec.Layout.Name
	r.Groups, r.InstalledAt = p.inGroups(), nil
	putPrivate(&r.Machine, p.private)
	return r
}

// what is the installation in words.
func (p *installPlan) what() string {
	private := ""
	if len(p.private) > 0 {
		private = ", " + privateAddresses(p.private)
	}
	return fmt.Sprintf("%s (profile %s from %s, layout %s%s)", p.spec.OS, p.profile, p.from, p.spec.Layout.Name, private)
}

// assignPlans gives the planned installations their private addresses
// (assignPrivate, in doc) and renders their specifications; the index
// says which plan an error is about.
func assignPlans(doc *state.Document, plans []*installPlan) (int, error) {
	reqs := make([]privateRequest, len(plans))
	for i, p := range plans {
		reqs[i] = privateRequest{ref: p.m.Ref(), networks: p.privateNets, given: p.privateIPs}
	}
	private, i, err := assignPrivate(doc, reqs)
	if err != nil {
		return i, err
	}
	for i, p := range plans {
		p.private, p.had = private[i], doc.Servers[p.m.Ref()].Private
		p.spec.Network.Private, p.spec.Network.PrivateNetworks = nil, nil
		for _, q := range p.private {
			p.spec.Network.PrivateNetworks = append(p.spec.Network.PrivateNetworks, profile.PrivateSpec{Network: q.Network, CIDR: q.CIDR, Address: q.Address})
		}
		if len(p.private) > 0 {
			first := p.spec.Network.PrivateNetworks[0]
			p.spec.Network.Private = &first
		}
		if p.specJSON, err = p.spec.JSON(); err != nil {
			return i, err
		}
	}
	return -1, nil
}

// resolveInstall resolves the profile of a planned installation into
// the specification the installer takes: --profile, else the one set for
// the server in the state (doc), else default, with the other flags
// over it and --set last; its private networks; and the users ump tries
// on the running system. assignPlans gives the addresses and renders the
// specification.
func (a *app) resolveInstall(ctx context.Context, cmd *cobra.Command, cfg *config.Config, doc *state.Document, p *installPlan, o installOptions, f installFlags) error {
	storage := strings.TrimRight(o.storage, "/")
	p.rescueURL = cfg.StagingImage()
	if storage == "" {
		storage = cfg.StorageURL()
	} else if cfg.Staging.Image == "" {
		// --storage moves the rescue image along, unless it is pinned
		p.rescueURL = storage + "/rescue"
	}
	p.storage = storage
	profileName := o.profile
	if profileName == "" {
		profileName = doc.Servers[p.m.Ref()].Profile
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
	if f.private.given {
		prof.Network.Private = profile.PrivateNetworks{profile.NoPrivateNetwork}
		if len(f.private.networks) > 0 {
			prof.Network.Private = f.private.networks
		}
	}
	// the named users, when the profile or --user refers to any (or
	// --ssh-key must find the administrator)
	var users map[string]*profile.User
	if (len(f.users) == 0 && profile.HasUserRefs(prof)) || namesUsers(f.users) {
		if users, err = a.loadUsers(ctx, storage); err != nil {
			return err
		}
	}
	if len(f.users) > 0 {
		if err := replaceUsers(prof, f.users, users); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
	}
	var userdata map[string]*profile.Userdata
	if len(f.userdata) > 0 {
		if userdata, err = a.loadUserdata(ctx, storage); err != nil {
			return err
		}
		prof.Userdata = nil
		for _, n := range f.userdata {
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
	// the private networks: --private-network, else the profile's
	var networks map[string]*profile.PrivateNetwork
	names := prof.PrivateNetworkNames()
	if len(names) > 0 {
		if networks, err = a.loadNetworks(ctx, storage); err != nil {
			return err
		}
		for _, name := range names {
			if slices.Contains(f.private.networks, name) && networks[name] == nil {
				return usageErrorf(cmd, "--private-network %s: not in networks.yaml (known: %s; ump config network create %s makes it)", name, strings.Join(sortedKeys(networks), ", "), name)
			}
		}
	}
	if p.privateNets, err = prof.ResolvePrivate(networks); err != nil {
		return err
	}
	if len(f.private.ips) > 0 && len(names) == 0 {
		return usageErrorf(cmd, "--private-ip %s: profile %s gives the server no private network; --private-network NAME gives it one", f.private.ips[0].text, prof.Name)
	}
	if p.privateIPs, err = f.private.addresses(names); err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	spec.Hostname = p.hostname
	spec.Storage = storage
	if o.disks != "" {
		spec.Disks = strings.Split(o.disks, ",")
	}
	p.profile, p.from, p.spec = prof.Name, from, spec
	logins := a.loginUsers(cfg, p.m, doc)
	if u := prof.Admin(users); u != "" && u != "root" {
		logins = append([]string{"root", u}, logins[1:]...)
	}
	p.logins = dedupe(logins)
	return nil
}

func (a *app) installServer(cmd *cobra.Command, ref string, o installOptions) error {
	format, err := resultFormat(cmd, o.output)
	if err != nil {
		return err
	}
	a.dryRun = o.dryRun
	f, err := checkServerFlags(cmd, o)
	if err != nil {
		return err
	}
	named, err := playbookNames(cmd, o.playbooks)
	if err != nil {
		return err
	}
	pattern, err := hostsPattern(cmd, o.hosts)
	if err != nil {
		return err
	}
	if o.noProvision && (pattern != "" || len(named) > 0) {
		return usageErrorf(cmd, "--hosts and --playbook narrow the provisioning, which --no-provision leaves out")
	}
	ctx := cmd.Context()
	// provisioning needs the infrastructure repository, checked before
	// anything else; --no-provision needs none, and puts the server into
	// the inventory of the one there is when it can check the names there
	repo, run, inv, unchecked, err := a.installRepo(ctx, o.noProvision, named, pattern)
	if err != nil {
		return err
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	// the rescue system on the way gets UMP's key, the keys chosen for
	// rescue systems and the --ssh-key references; the administrator of
	// the new system gets the --ssh-key ones too (resolveInstall)
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
	p, err := newInstallPlan(ref, m, prov, o, f)
	if err != nil {
		return err
	}
	p.keys, p.keyPaths, p.keyPath = keys, keyPaths, keyPath
	hostname, inGroups := p.hostname, p.inGroups()
	// the state after the listing, which forgot the servers gone from
	// their providers
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return err
	}
	// the host file goes where no other's is, under a name that is no
	// group, and --hosts matches a host of the inventory with the server
	// in it; the hosts of servers gone are none
	if repo != nil {
		files, err := repo.checkHost(ctx, a.goneServer(ctx, cfg, doc), hostname, m.Ref(), inGroups, "give another --hostname", "")
		if err != nil {
			return err
		}
		inv = inv.without(files.goneHosts())
		if err := inv.check(hostname, m.Ref(), inGroups, "give another --hostname", ""); err != nil {
			return err
		}
	}
	if pattern != "" {
		in := strings.Join(inGroups, ", ")
		future := inv.with(map[string]inventoryHost{hostname: {m: m, groups: inGroups}})
		if err := a.matchHosts(ctx, repo, future, pattern, fmt.Sprintf("--hosts %s matches no host of the inventory with %s in %s; nothing was changed", pattern, hostname, in)); err != nil {
			return err
		}
	}
	a.warnNoRescue(m, prov)
	a.warnPreflight(ctx, m, prov)
	// the hostname is no group of another server in the state
	if err := checkStateGroups(doc, hostname, m.Ref(), "give another --hostname"); err != nil {
		return err
	}
	if err := a.resolveInstall(ctx, cmd, cfg, doc, p, o, f); err != nil {
		return err
	}
	if _, err := assignPlans(doc, []*installPlan{p}); err != nil {
		return err
	}
	question := fmt.Sprintf("Install %s on %s as %s? Every disk of the server is erased.", p.what(), describeRef(m), hostname)
	if noRescueWarning(m, prov) != "" {
		question += " There is no rescue system to fall back on: a failed installation is yours to recover."
	}
	if !o.noProvision {
		question += " Then ump provision runs " + run.String() + "."
	}
	if o.dryRun {
		question = fmt.Sprintf("Bring %s into the rescue system and show the installation plan for %s? (dry run: nothing is erased)", describeRef(m), p.what())
	}
	// an OS without an image for the machine's architecture stops here,
	// before the reboot, the rescue system or a provider's reinstall
	if err := a.checkArch(ctx, cmd, m, p.spec.OS, p.storage, p.logins, keyPath, o.yes); err != nil {
		return err
	}
	if err := a.confirm(cmd, o.yes, question); err != nil {
		return err
	}
	// the server is locked for the whole installation and its
	// provisioning: another ump must not reinstall or rescue it meanwhile;
	// other servers are free
	unlock, err := a.lockServer(ctx, m, "reinstall")
	if err != nil {
		return err
	}
	defer unlock()
	// the private addresses are the server's from now on: no other
	// installation gets them meanwhile; one whose installer did not write
	// the disks gives them back, under the lock
	if !o.dryRun {
		if err := a.reservePrivate(ctx, []*installPlan{p}); err != nil {
			return err
		}
	}
	r := installRun{dryRun: o.dryRun, noReboot: o.noReboot}
	if !o.noProvision {
		r.after = run.String()
	}
	result, record, err := a.runInstall(cmd, p, r)
	if err != nil {
		if !result.written {
			a.releasePrivate(ctx, []*installPlan{p})
		}
		return err
	}
	result.Playbooks = run.playbooks
	if o.dryRun {
		return a.installReport(result, format)
	}
	recorded := a.recordInstallation(ctx, m, *record, p.groups)
	if o.noReboot {
		if !recorded {
			fmt.Fprintf(a.stderr, "Not provisioned: after the reboot, record it and run ump provision --hosts %s.\n", hostname)
		} else {
			fmt.Fprintf(a.stderr, "Not provisioned: after the reboot, ump provision --hosts %s runs the playbooks on it. It is in the inventory from its record on: in ump's, and in the host files from the next ump provision, which finds it unreachable until then.\n", hostname)
		}
		return a.installReport(result, format)
	}
	// the inventory comes from the record: without it, the old one
	switch {
	case !recorded && !o.noProvision:
		if err := a.installReport(result, format); err != nil {
			return err
		}
		return withExit(exitProvision, fmt.Errorf("provisioning failed: %s is installed but not recorded, and the inventory comes from the record; record it and run ump provision --hosts %s", hostname, hostname))
	case !recorded:
		fmt.Fprintln(a.stderr, "Not provisioned (--no-provision), nor in the inventory as installed: the inventory comes from the record; record it.")
	case !o.noProvision:
		// the server is provisioned when its own line of the recap says so,
		// whatever the run did on other hosts
		res, err := a.provision(ctx, cfg, repo, run, "provision after reinstalling "+m.Name, []provider.Machine{m})
		result.Provision, result.Provisioned = res, ownRecap(res, hostname)
		var failed *runFailure
		if err != nil {
			// the result still says what was installed
			if rerr := a.installReport(result, format); rerr != nil {
				return rerr
			}
			if !result.Provisioned || ctx.Err() != nil || !errors.As(err, &failed) {
				return notProvisioned(ctx, err, hostname)
			}
			return withExit(exitProvision, fmt.Errorf("%s is provisioned; the run failed%s", hostname, failed.detail()))
		}
		if !result.Provisioned {
			fmt.Fprintf(a.stderr, "%s is not provisioned: no play of the run matched it. %s\n", hostname, res.summary())
			break
		}
		fmt.Fprintf(a.stderr, "%s is provisioned. %s\n", hostname, res.summary())
	case repo != nil:
		err := a.addToInventory(ctx, cfg, repo, "reinstalling "+m.Name, []hostEntry{{hostname, inGroups, a.inventoryVars(cfg, m, *record, p.private)}})
		if err != nil {
			if rerr := a.installReport(result, format); rerr != nil {
				return rerr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return withExit(exitCodeFor(err), fmt.Errorf("%s is installed and recorded, but not written into the inventory: %w", hostname, err))
		}
		fmt.Fprintf(a.stderr, "Not provisioned (--no-provision); the next ump provision includes it (ump provision --hosts %s for it alone).\n", hostname)
	default:
		why := "no Ansible layout here"
		if unchecked != "" {
			why = unchecked
		}
		fmt.Fprintf(a.stderr, "Not provisioned (--no-provision), and no host file for %s (%s): ump provision, run from the infrastructure repository, writes it and includes the server; where ump is the inventory, it is in it already.\n", hostname, why)
	}
	return a.installReport(result, format)
}

// ownRecap says that the host is provisioned by its own line of a run's
// recap: there is one, with nothing failed or unreachable.
func ownRecap(res *provisionResult, host string) bool {
	if res == nil {
		return false
	}
	own, ok := res.Hosts[host]
	return ok && own.Failed == 0 && own.Unreachable == 0
}

// installRun is how runInstall runs a plan: as a dry run (the
// installer's plan alone), without the reboot (the new system stays on
// the disks), and with the provisioning to announce for after the first
// boot (empty: none).
type installRun struct {
	dryRun   bool
	noReboot bool
	after    string
}

// runInstall runs a planned installation, without a question, on a
// server whose lock the caller holds: it brings the server into the UMP
// rescue system, runs the installer there with the specification,
// reboots into the new system and checks that it answers, all said on
// stderr. It records nothing: the record of a completed installation
// comes back with the result, and an error says where the installation
// stopped (after a failed installer the server stays in the rescue
// system); the result says whether the disks were written, also with an
// error.
func (a *app) runInstall(cmd *cobra.Command, p *installPlan, r installRun) (installResult, *state.Install, error) {
	ctx := cmd.Context()
	m, prov, ref, spec := p.m, p.prov, p.ref, p.spec
	result := p.result(r.dryRun)
	// everything on the way is progress (stderr); stdout is for the result
	entry, err := a.bringToImage(cmd, m, prov, p.keys, p.keyPath, p.keyPaths, p.rescueURL, entryOptions{via: p.via, confirmed: true, quiet: true, stock: !r.dryRun, unguarded: true, users: p.logins})
	if err != nil {
		return result, nil, err
	}
	result.Via = entry.via

	// the installer, from the storage, in the rescue system, with the
	// specification on its stdin
	args := []string{"--spec", "/tmp/ump-install.json"}
	if r.dryRun {
		args = append(args, "--dry-run")
		fmt.Fprintf(a.stderr, "Installation plan for %s on %s:\n", p.what(), m.Name)
	} else {
		fmt.Fprintf(a.stderr, "Installing %s on %s as %s ...\n", p.what(), m.Name, p.hostname)
	}
	if r.after != "" {
		fmt.Fprintf(a.stderr, "Provisioning after the first boot: %s.\n", r.after)
	}
	installer := p.storage + "/install/install.sh"
	// the keys go into the rescue system too, so that it stays reachable
	// with them while the installation runs
	remote := ensureKeysCommand(p.keys) + "cat > /tmp/ump-install.json && " + fetchAndRun(installer, "/tmp/ump-install.sh", args)
	argv := withSSHOptions(sshArgs(entry.user, entry.addr, []string{p.keyPath}, true, true, []string{remote}), batchOptions(10*time.Second)...)
	var buf bytes.Buffer
	w := io.MultiWriter(a.stderr, &buf)
	code, err := a.sshExec(ctx, argv, bytes.NewReader(p.specJSON), w, w)
	if err != nil {
		return result, nil, err
	}
	// the installer ends with 1 (a failed step) or 2 (a bad specification
	// or environment) and says so; anything else without a line of it is
	// the fetch that failed
	installerRan := strings.Contains(buf.String(), "==> ") || strings.Contains(buf.String(), "install.sh:")
	result.written = !r.dryRun && (code == 0 || code == 255 && installerRan)
	switch {
	case code == 255:
		return result, nil, withExit(exitUnreachable, fmt.Errorf("the connection to %s ended during the installation; the server is in the UMP rescue system, look there (ump ssh %s --staging)", entry.addr, ref))
	case code != 0 && !installerRan && code != 1 && code != 2:
		return result, nil, fmt.Errorf("%s could not fetch the installer from %s (exit status %d); the server must reach the storage", entry.addr, installer, code)
	case code == 2:
		return result, nil, fmt.Errorf("the installer refused the specification or the machine (exit status 2, see above); nothing was changed, %s stays in the UMP rescue system", m.Name)
	case code != 0:
		return result, nil, fmt.Errorf("the installation on %s failed (exit status %d, see above); the server stays in the UMP rescue system (ump ssh %s --staging)", m.Name, code, ref)
	}
	if r.dryRun {
		fmt.Fprintf(a.stderr, "Dry run done; %s stays in the UMP rescue system (ump ssh %s --staging).\n", m.Name, ref)
		return result, nil, nil
	}

	// providers that keep the rescue system as the boot option until told
	// otherwise would boot it again
	if rs, ok := prov.(provider.Rescue); ok {
		if on, err := rs.RescueEnabled(ctx, m); err == nil && on {
			if err := rs.DisableRescue(ctx, m); err != nil {
				return result, nil, fmt.Errorf("account %s: %s is installed, but the provider's rescue system could not be disabled, and would boot again: %w", m.Account, spec.OS, err)
			}
		}
	}
	// providers that boot their own kernel or boot loader by default are
	// told to boot the installed system's
	db, diskBoot := prov.(provider.DiskBooter)
	if diskBoot {
		if err := db.BootFromDisk(ctx, m); err != nil {
			return result, nil, fmt.Errorf("account %s: %s is installed, but the provider could not be switched to boot it from its disks: %w", m.Account, spec.OS, err)
		}
	}
	if r.noReboot {
		// the disks hold the new system: its record, and the old host key
		// goes, so that ump provision works after the reboot
		admin := spec.Admin()
		record := installRecord(spec.OS, p.profile, spec.Layout.Name, p.hostname, admin, "", a.now())
		result.InstalledAt, result.Admin = &record.At, admin
		a.forgetReplacedHostKey(entry.addr)
		how := "a reboot"
		if diskBoot {
			how = "a reboot through the provider (ump server reboot " + ref + ")"
		}
		fmt.Fprintf(a.stderr, "Installed; %s stays in the UMP rescue system, %s starts %s.\n", m.Name, how, spec.OS)
		return result, &record, nil
	}

	fmt.Fprintf(a.stderr, "Rebooting %s into %s ...\n", m.Name, spec.OS)
	reboot := withSSHOptions(sshArgs(entry.user, entry.addr, []string{p.keyPath}, true, true, []string{"systemctl reboot"}), batchOptions(10*time.Second)...)
	// a changed boot configuration takes effect with the provider's reboot
	code, rerr := 0, error(nil)
	if !diskBoot {
		code, rerr = a.sshExec(ctx, reboot, nil, io.Discard, io.Discard)
	}
	if diskBoot || rerr != nil || (code != 0 && code != 255) {
		pw, ok := prov.(provider.Power)
		if !ok {
			return result, nil, fmt.Errorf("%s is installed, but could not be rebooted over SSH, and provider type %s cannot reboot it; reboot it yourself", m.Name, m.Provider)
		}
		if err := pw.Reboot(ctx, m, provider.RebootHard); err != nil {
			return result, nil, fmt.Errorf("account %s: %s is installed, but the reboot failed: %w", m.Account, m.Name, err)
		}
	}
	result.Rebooted = true
	if err := a.waitForSSH(ctx, entry.addr, true); err != nil {
		return result, nil, err
	}
	admin := spec.Admin()
	sys, ok := a.installedSystem(ctx, entry.addr, p.keyPath, admin)
	if !ok {
		return result, nil, withExit(exitUnreachable, fmt.Errorf("SSH answers on %s, but not as %s with UMP's key; the new system may not have booted, look at the server's console", entry.addr, admin))
	}
	result.System, result.Admin = sys, admin
	a.forgetReplacedHostKey(entry.addr)
	// what was installed goes into the state, for the inventory
	record := installRecord(spec.OS, p.profile, spec.Layout.Name, p.hostname, admin, sys.Details, a.now())
	result.InstalledAt = &record.At
	fmt.Fprintf(a.stderr, "%s is up: %s, hostname %s, root on %s; log in as %s (ump ssh %s).\n", m.Name, sys.Name, p.hostname, sys.Root, admin, m.Name)
	return result, &record, nil
}

// serverRecord is an installation to record: the server, the record and
// the groups it was installed with, and its private addresses, for the
// inventory (reservePrivate recorded them before the installation).
type serverRecord struct {
	m       provider.Machine
	record  state.Install
	groups  []string
	private []state.Private
}

// recordInstallation puts the installation into the state, with the
// groups: a new system has the groups it is installed with, none
// without --group, and the facts of the old one, which the next
// provisioning clears. A failure is told, with the record to add by
// hand, and reported: the server is installed either way, but the state
// has the old record.
func (a *app) recordInstallation(ctx context.Context, m provider.Machine, record state.Install, groups []string) bool {
	return a.recordInstallations(ctx, []serverRecord{{m: m, record: record, groups: groups}})
}

// recordInstallations records installations as recordInstallation does
// one, all in one change of the state.
func (a *app) recordInstallations(ctx context.Context, records []serverRecord) bool {
	err := a.updateState(ctx, func(d *state.Document) error {
		for _, r := range records {
			s := d.Servers[r.m.Ref()]
			s.Installed = &r.record
			s.Groups = r.groups
			s.StaleFacts = true
			d.Servers[r.m.Ref()] = s
		}
		return nil
	})
	if err == nil {
		return true
	}
	entries := map[string]state.Server{}
	for _, r := range records {
		entries[r.m.Ref()] = state.Server{Installed: &r.record, Groups: r.groups, StaleFacts: true}
	}
	var entry bytes.Buffer
	enc := yaml.NewEncoder(&entry)
	enc.SetIndent(2)
	enc.Encode(map[string]map[string]state.Server{"servers": entries})
	if len(records) == 1 {
		fmt.Fprintf(a.stderr, "ump: the installation is not recorded in the state: %v\n", err)
		fmt.Fprintf(a.stderr, "Record it by hand: in state.yaml (ump config state view says where), the server's installed, groups and stale_facts are these:\n%s", entry.String())
	} else {
		fmt.Fprintf(a.stderr, "ump: the installations are not recorded in the state: %v\n", err)
		fmt.Fprintf(a.stderr, "Record them by hand: in state.yaml (ump config state view says where), the servers' installed, groups and stale_facts are these:\n%s", entry.String())
	}
	return false
}

// knownHosts keeps the changes of ~/.ssh/known_hosts apart: ssh-keygen
// -R rewrites the file, and installations side by side would lose each
// other's.
var knownHosts sync.Mutex

// forgetReplacedHostKey removes the old system's host key of the
// address, which would make ssh cry man-in-the-middle at the next login:
// the disks hold the system ump just installed.
func (a *app) forgetReplacedHostKey(addr string) {
	knownHosts.Lock()
	forgot, err := a.forgetOldHostKey(addr)
	knownHosts.Unlock()
	if err == nil && forgot {
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
	bad := a.osArchs(ctx, os, storage)
	if len(bad) == 0 {
		return nil
	}
	arch, how := m.Arch, "as the provider says"
	if arch == "" && m.IPv4 != "" {
		if _, p, ok, _ := a.probeAdmin(ctx, logins, m.IPv4, []string{keyPath}, false); ok {
			arch, how = p.arch, "as its running system says"
		}
	}
	if arch != "" {
		return archRefusal(m, os, arch, how, bad)
	}
	only := archsWith(bad)
	fmt.Fprintf(a.stderr, "Warning: %s exists for %s only, and ump cannot tell the architecture of %s (its provider does not say, and its running system does not let ump in). On another architecture the installation stops in the rescue system, after the server was rebooted into it or, without a rescue system, reinstalled by its provider: a wiped server and no new system.\n", os, strings.Join(only, " and "), m.Name)
	return a.confirm(cmd, yes, fmt.Sprintf("Is %s an %s machine?", m.Name, strings.Join(only, " or ")))
}

// osArchs are the architectures the storage has no image of the OS for,
// each with why; none when it has one for every architecture, or for
// none, or cannot be read.
func (a *app) osArchs(ctx context.Context, os, storage string) map[string]string {
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
	if good == 0 {
		return nil
	}
	return bad
}

// archRefusal refuses the OS on a machine of an architecture it has no
// image for (bad, from osArchs); how says who told the architecture.
func archRefusal(m provider.Machine, os, arch, how string, bad map[string]string) error {
	if reason, no := bad[arch]; no {
		return withExit(exitUsage, fmt.Errorf("%s cannot be installed on %s, which is %s (%s): %s. Nothing was changed", os, m.Name, arch, how, reason))
	}
	return nil
}

// archsWith are the architectures with an image, of those bad leaves.
func archsWith(bad map[string]string) []string {
	var only []string
	for _, x := range []string{"amd64", "arm64"} {
		if _, no := bad[x]; !no {
			only = append(only, x)
		}
	}
	return only
}
