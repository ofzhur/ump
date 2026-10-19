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
// installed, by roles (web, db, monitoring, ...) that a strategy of the
// profile (provision.type: ansible-bootstrap, ansible-pull) applies.
// The roles are recorded with the server in the state; running a
// strategy is not implemented yet. docs/provisioning.md describes the
// model.

// roleFlags are --role and --roles, which mean the same: both repeat and
// take comma-separated lists.
type roleFlags struct {
	role, roles []string
}

func (f *roleFlags) add(cmd *cobra.Command, what string) {
	cmd.Flags().StringArrayVar(&f.role, "role", nil, what+" (repeatable, or comma-separated)")
	cmd.Flags().StringArrayVar(&f.roles, "roles", nil, "the same as --role")
}

var roleName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// list returns the roles in the order given, without duplicates; a name
// is lower-case letters, digits, '-', '_' and '.'.
func (f *roleFlags) list(cmd *cobra.Command) ([]string, error) {
	var out []string
	for _, v := range append(append([]string{}, f.role...), f.roles...) {
		for _, r := range strings.Split(v, ",") {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			if !roleName.MatchString(r) {
				return nil, usageErrorf(cmd, "role %q: a role name is lower-case letters, digits, '-', '_' and '.', starting with a letter or digit", r)
			}
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// provisionNotYet is said wherever roles are recorded but not run.
func provisionNotYet(roles []string) string {
	return fmt.Sprintf("Provisioning the role(s) %s is not implemented yet; they are recorded for the server (ump server show, ump server list -o wide). See docs/provisioning.md.", strings.Join(roles, ", "))
}

func (a *app) newServerProvisionCmd() *cobra.Command {
	var rf roleFlags
	cmd := &cobra.Command{
		Use:   "provision NAME|ID|IP|PROVIDER/ID --role ROLE...",
		Short: "Provision an installed server with roles",
		Long: `Provision a server that ump installed with one or more roles, by the
strategy its profile names (provision.type: ansible-bootstrap or
ansible-pull), without reinstalling it. The roles are added to those the
server has; a role is removed only by a reinstall.

The way that is recommended is to give the roles at the installation
(ump server reinstall NAME --role ROLE), so that every role starts on a
fresh system: roles are best treated as immutable, and a server that
should change roles is wiped and reinstalled with the new ones. Adding a
role to a running server suits roles built for it, such as a basic one
every server gets (users, monitoring, hardening).

Running the strategy is not implemented yet: the roles are checked and
recorded with the server (ump server show, ump server list -o wide), and
ump says so. docs/provisioning.md describes the strategies.`,
		Example: `  ump server provision web-01 --role base
  ump server provision web-01 --roles base,monitoring`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			roles, err := rf.list(cmd)
			if err != nil {
				return err
			}
			if len(roles) == 0 {
				return usageErrorf(cmd, "give the role(s) with --role or --roles")
			}
			m, _, err := a.resolveOne(cmd, args[0])
			if err != nil {
				return err
			}
			var now []string
			err = a.updateState(cmd.Context(), func(d *state.Document) error {
				st := d.Servers[m.Ref()]
				for _, r := range roles {
					if !slices.Contains(st.Roles, r) {
						st.Roles = append(st.Roles, r)
					}
				}
				now = st.Roles
				d.Servers[m.Ref()] = st
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: roles %s.\n", describeRef(m), strings.Join(now, ", "))
			fmt.Fprintln(a.stderr, provisionNotYet(roles))
			return nil
		},
	}
	rf.add(cmd, "a role to provision the server with")
	return cmd
}
