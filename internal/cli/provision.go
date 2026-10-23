package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/state"
)

// Provisioning: what a server becomes after its operating system is
// installed. ump puts every server it installs into the inventory of the
// infrastructure repository, in the group common and the groups given
// with --group, and runs the playbooks of those groups for it (see
// ansible.go); docs/provisioning.md describes the model.

// groupName is what Ansible takes as a group name; ump keeps to lower
// case.
var groupName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ansibleOwnName says that a name is Ansible's own: its groups all and
// ungrouped, localhost, and _meta, the host variables in the JSON of an
// inventory script.
func ansibleOwnName(name string) bool {
	switch name {
	case "all", "ungrouped", "localhost", "_meta":
		return true
	}
	return false
}

// groupFlags is --group, repeatable, each a comma-separated list.
type groupFlags []string

func (f *groupFlags) add(cmd *cobra.Command, what string) {
	cmd.Flags().StringArrayVar((*[]string)(f), "group", nil, what+" (repeatable, or comma-separated)")
}

// list returns the groups in the order given, without duplicates and
// without common, which every server is in anyway.
func (f groupFlags) list(cmd *cobra.Command) ([]string, error) {
	var out []string
	for _, v := range f {
		for _, g := range strings.Split(v, ",") {
			g = strings.TrimSpace(g)
			switch {
			case g == "" || g == commonGroup || slices.Contains(out, g):
				continue
			case ansibleOwnName(g):
				return nil, usageErrorf(cmd, "group %q: all, ungrouped, localhost and _meta are Ansible's own names", g)
			case !groupName.MatchString(g):
				return nil, usageErrorf(cmd, "group %q: a group name is lower-case letters, digits and '_', not starting with a digit", g)
			}
			out = append(out, g)
		}
	}
	return out, nil
}

func (a *app) newServerProvisionCmd() *cobra.Command {
	var gf groupFlags
	cmd := &cobra.Command{
		Use:   "provision NAME|ID|IP|PROVIDER/ID [--group GROUP]...",
		Short: "Put a server ump installed into the inventory and run its run list",
		Long: `Put a server that ump installed into the inventory of the infrastructure
repository and run its run list, without reinstalling it. ump runs from
the root of that repository: it needs ansible.cfg and inventory/ there
and ansible-playbook in PATH.

The server is in the group common and in the groups it was installed
with (ump server reinstall --group); --group adds groups, in order. Its
run list is those groups, common first; each is a playbook,
playbooks/GROUP.yml of the repository when it has one, else the
collection's ofzhur.ansible.GROUP. ump checks the run list with
ansible-playbook --syntax-check and the inventory with ansible-inventory
(no host and group of the same name), writes
inventory/hosts/HOSTNAME.yml, and runs ansible-playbook with the run
list for that host alone, logging in as the server's administrator with
UMP's key only. Without --group, it runs the run list the server has: a
converge. Where a file of inventory/ says plugin: ofzhur.ansible.ump,
ump is the inventory: no host file is written, and one in
inventory/hosts/ for the server stops it.

Groups are best given at the installation, so that each starts on a
fresh system; a group is removed only by a reinstall. Adding a group in
place suits groups built for it, such as monitoring.

Exit status 6 means the run list did not complete (Ansible's output says
where): fix that and run the command again. docs/provisioning.md
describes the layout of the repository.`,
		Example: `  ump server provision web-01
  ump server provision web-01 --group monitoring
  ump server provision web-01 --group web,monitoring`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.provisionServer(cmd, args[0], gf)
		},
	}
	gf.add(cmd, "a group to add")
	return cmd
}

func (a *app) provisionServer(cmd *cobra.Command, ref string, gf groupFlags) error {
	add, err := gf.list(cmd)
	if err != nil {
		return err
	}
	repo, err := a.requireRepo("")
	if err != nil {
		return err
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	m, _, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	// the lock first: another ump must not reinstall or rescue the server
	// meanwhile, nor change the record read next
	unlock, err := a.lockServer(ctx, m, "provision")
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return err
	}
	st := doc.Servers[m.Ref()]
	if st.Installed == nil {
		return withExit(exitUsage, fmt.Errorf("ump did not install %s: nothing to put into the inventory; ump server reinstall %s", m.Name, ref))
	}
	if primaryAddress(m) == "" {
		return fmt.Errorf("%s has no public address", m.Ref())
	}
	rec := *st.Installed
	hostname := inventoryHostname(m, rec)
	if hostname == "" {
		return fmt.Errorf("the installation record of %s has no hostname, and its name %q is not a valid one; ump server reinstall %s --hostname NAME", m.Ref(), m.Name, ref)
	}
	groups := slices.Clone(st.Groups)
	for _, g := range add {
		if !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	rl := repo.runList(groups)
	retry := "reinstall the server with another --hostname"
	if err := repo.checkHost(hostname, m.Ref(), rl, retry); err != nil {
		return err
	}
	if err := a.syntaxCheck(ctx, repo, rl); err != nil {
		return err
	}
	inv, err := a.readInventory(ctx, repo)
	if err != nil {
		return err
	}
	if err := inv.check(hostname, m.Ref(), rl, retry); err != nil {
		return err
	}
	_, keyPath, err := a.stagingKey(cfg)
	if err != nil {
		return err
	}
	if len(groups) > len(st.Groups) {
		if err := a.updateState(ctx, func(d *state.Document) error {
			s := d.Servers[m.Ref()]
			for _, g := range add {
				if !slices.Contains(s.Groups, g) {
					s.Groups = append(s.Groups, g)
				}
			}
			d.Servers[m.Ref()] = s
			return nil
		}); err != nil {
			return err
		}
	}
	if err := a.provisionHost(ctx, repo, rl, hostname, a.inventoryVars(cfg, m, rec), keyPath); err != nil {
		return a.provisionFailed(ctx, err, fmt.Sprintf("the run list of %s did not complete", hostname), "run ump server provision "+ref+" again")
	}
	fmt.Fprintf(a.stdout, "%s: provisioned, run list %s.\n", describeRef(m), strings.Join(rl.groups, ", "))
	return nil
}
