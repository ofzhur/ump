package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/sshkey"
)

func (a *app) newSSHCmd() *cobra.Command {
	var (
		user    string
		staging bool
	)
	sub := &cobra.Command{
		Use:   "ssh NAME|ID|IP|PROVIDER/ID [-- COMMAND...]",
		Short: "Open an SSH session on a server, or run a command there",
		Long: `Resolve the server and start the local ssh client against its primary
address: an interactive session, or the command given after "--", whose
exit status becomes ump's. UMP's own key (see ump config key) is offered
together with your own keys (the agent's and the usual ~/.ssh/id_* files),
so a machine that knows only UMP's key is reachable too; your SSH
configuration applies.

The login name is --user, else the administrator of the profile set for
the server (ump server set --profile), else of the default profile: the
sudo user of a UMP-installed system, where root cannot log in over SSH
(ump by default). Rescue and staging systems take root.

--staging connects the way UMP does to rescue and staging systems: with
UMP's key alone and without host key checks, because those systems have
a fresh host key every boot.`,
		Example: `  ump ssh web-01
  ump ssh web-01 -- uptime
  ump ssh web-01 --staging -- lsblk`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			refs, command := splitDash(cmd.ArgsLenAtDash(), args)
			if len(refs) != 1 {
				return usageErrorf(cmd, "expected one server reference, got %d", len(refs))
			}
			return a.sshServer(cmd, refs[0], user, staging, command)
		},
	}
	sub.Flags().StringVar(&user, "user", "", "the login name (default: the profile's administrator, ump; root with --staging)")
	sub.Flags().BoolVar(&staging, "staging", false, "use UMP's key alone and no host key checks (rescue and staging systems)")
	return sub
}

func (a *app) sshServer(cmd *cobra.Command, ref, user string, staging bool, command []string) error {
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	m, _, err := a.resolveOne(cmd, ref)
	if err != nil {
		return err
	}
	addr := m.IPv4
	if addr == "" {
		addr = m.IPv6
	}
	if addr == "" {
		return fmt.Errorf("%s has no public address", m.Ref())
	}
	_, umpKey, err := a.stagingKey(cfg)
	if err != nil {
		return err
	}
	identities := []string{umpKey}
	if !staging {
		identities = append(identities, sshkey.DefaultIdentityFiles()...)
	}
	if user == "" {
		user = "root"
		if !staging {
			user = a.profileAdmin(cfg, cfg.Servers[m.Ref()].Profile)
		}
	}
	argv := sshArgs(user, addr, identities, staging, staging, command)
	interactive := len(command) == 0
	if interactive && (a.isTTY == nil || !a.isTTY()) {
		return withExit(exitAborted, errors.New("an interactive session needs a terminal; give a command after --"))
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
