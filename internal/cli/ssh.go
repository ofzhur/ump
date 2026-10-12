package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func (a *app) newSSHCmd() *cobra.Command {
	var (
		user    string
		staging bool
	)
	sub := &cobra.Command{
		Use:   "server NAME|ID|IP|PROVIDER/ID [-- COMMAND...]",
		Short: "Open an SSH session on a server, or run a command there",
		Long: `Resolve the server and start the local ssh client against its primary
address: an interactive session, or the command given after "--", whose
exit status becomes ump's. Your own SSH configuration and keys apply.

--staging connects the way UMP does to rescue and staging systems: with
UMP's own key (see ump config key) and without host key checks, because
those systems have a fresh host key every boot.`,
		Example: `  ump ssh server web-01
  ump ssh server web-01 -- uptime
  ump ssh server web-01 --staging -- lsblk`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			refs, command := splitDash(cmd.ArgsLenAtDash(), args)
			if len(refs) != 1 {
				return usageErrorf(cmd, "expected one server reference, got %d", len(refs))
			}
			return a.sshServer(cmd, refs[0], user, staging, command)
		},
	}
	sub.Flags().StringVar(&user, "user", "root", "the login name")
	sub.Flags().BoolVar(&staging, "staging", false, "use UMP's own key and no host key checks (rescue and staging systems)")
	return a.serverVerbCmd("ssh", "Open an SSH session on a server", "Open an SSH session on a server, or run a command there.",
		"  ump ssh server web-01\n  ump ssh server web-01 -- uptime", sub)
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
	keyPath := ""
	if staging {
		keyPath = cfg.StagingKeyPath()
	}
	argv := sshArgs(user, addr, keyPath, staging, command)
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
