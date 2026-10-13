package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
)

// newServerCmd groups everything that happens to a server.
func (a *app) newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"s", "servers", "srv", "machine", "machines"},
		Short:   "List, inspect, install and control servers",
		Long: `Everything that happens to a server: listing and showing them, installing
an operating system the way a profile says, the rescue system, power,
names and reverse DNS. Servers are named by name, provider ID, IP address
or provider/id, and found across every configured account.`,
		Example: `  ump server list
  ump server show web-01
  ump server reinstall web-01 --profile db
  ump server rescue web-01
  ump server reboot web-01 --wait
  ump server rdns set web-01 web-01.example.net
  ump server set web-01 --profile db`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageErrorf(cmd, "unknown command %q for ump server", args[0])
		},
	}
	cmd.AddCommand(
		a.newServerListCmd(),
		a.newServerShowCmd(),
		a.newServerReinstallCmd(),
		a.newServerRescueCmd(),
		a.newServerUnrescueCmd(),
		a.newServerRebootCmd(),
		a.newServerStartCmd(),
		a.newServerStopCmd(),
		a.newServerRenameCmd(),
		a.newServerRDNSCmd(),
		a.newServerSetCmd(),
	)
	return cmd
}

// newServerSetCmd stores what the user chooses for one server.
func (a *app) newServerSetCmd() *cobra.Command {
	var (
		profile string
		clear   bool
	)
	cmd := &cobra.Command{
		Use:   "set NAME|ID|IP|PROVIDER/ID --profile NAME",
		Short: "Set the profile a server is reinstalled with",
		Long: `Remember, in the configuration file, which profile a server is reinstalled
with: ump server reinstall NAME then takes it without --profile. --clear
forgets it. The profile is checked to exist, locally or on the storage.`,
		Example: `  ump server set web-01 --profile db
  ump server set web-01 --clear`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (profile == "") == !clear {
				return usageErrorf(cmd, "give --profile NAME or --clear")
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			m, _, err := a.resolveOne(cmd, args[0])
			if err != nil {
				return err
			}
			if clear {
				delete(cfg.Servers, m.Ref())
				if err := config.Save(path, cfg); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "%s: no profile set; reinstall uses the default profile (%s).\n", describeRef(m), cfg.DefaultProfileName())
				return nil
			}
			if _, _, err := a.loadProfile(cmd.Context(), profile, cfg.StorageURL()); err != nil {
				return err
			}
			if cfg.Servers == nil {
				cfg.Servers = map[string]config.ServerSettings{}
			}
			cfg.Servers[m.Ref()] = config.ServerSettings{Profile: profile}
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: profile %s; ump server reinstall %s uses it.\n", describeRef(m), profile, m.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "the profile (a name or a file)")
	cmd.Flags().BoolVar(&clear, "clear", false, "forget the profile set for the server")
	return cmd
}
