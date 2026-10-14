package cli

import (
	"fmt"
	"net/netip"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/manual"
)

// Manual servers: machines no configured provider knows, added by hand
// so that the inventory is complete and ump can work on them over SSH.

func (a *app) newServerAddCmd() *cobra.Command {
	var s provider.ManualServer
	var kind string
	cmd := &cobra.Command{
		Use:   "add NAME --ipv4 ADDRESS [--ipv6 ADDRESS] [--location NAME] [--product NAME] [--kind bare-metal|cloud] [--note TEXT]",
		Short: "Add a server by hand, without a provider API",
		Long: `Add a server no configured provider lists: a machine in your own rack, at
a provider ump has no support for, or anything with an address. It goes
into the configuration file (manual:) and is listed by the account
"manual" of provider type manual, next to the others, with the status
"manual"; ump server set gives it a price and attributes like any other.

What ump can do with it is what SSH allows: ump server rescue and ump
server reinstall work when the running system lets UMP in (UMP's key,
or yours, as root or a sudo user; ump config key shows the key), by
loading the rescue image over SSH and kexec. There is no provider
rescue system behind it: when an installation fails or the new system
does not boot, the machine is yours to recover (a console, the
provider's own rescue system, a reinstall by their means). ump says so
before it erases anything. ump server remove forgets the server again.`,
		Example: `  ump server add rack-07 --ipv4 192.0.2.7 --location basement --product "Dell R640" --note "KVM at 192.0.2.250"
  ump server add vps-x --ipv4 203.0.113.5 --kind cloud
  ump server set rack-07 --price 0 --attr owner=ops`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validateServerName(name); err != nil {
				return usageErrorf(cmd, "%v", err)
			}
			if s.IPv4 == "" && s.IPv6 == "" {
				return usageErrorf(cmd, "give the server's address: --ipv4 and/or --ipv6")
			}
			for _, addr := range []struct{ flag, v string }{{"--ipv4", s.IPv4}, {"--ipv6", s.IPv6}} {
				if addr.v == "" {
					continue
				}
				if _, err := netip.ParseAddr(addr.v); err != nil {
					if _, err := netip.ParsePrefix(addr.v); err != nil || addr.flag == "--ipv4" {
						return usageErrorf(cmd, "%s %q is not an address", addr.flag, addr.v)
					}
				}
			}
			switch kind {
			case "", "bare-metal":
				s.Kind = provider.KindBareMetal
			case "cloud":
				s.Kind = provider.KindCloud
			default:
				return usageErrorf(cmd, "--kind %q is not bare-metal or cloud", kind)
			}
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			if _, exists := cfg.Manual[name]; exists {
				return fmt.Errorf("manual server %s exists already (ump server remove %s first)", name, name)
			}
			if acct, ok := cfg.Accounts[manual.AccountName]; ok && acct.Type != manual.TypeName {
				return fmt.Errorf("the account name %q is taken by a %s account; manual servers need it", manual.AccountName, acct.Type)
			}
			if cfg.Accounts == nil {
				cfg.Accounts = map[string]config.Account{}
			}
			if _, ok := cfg.Accounts[manual.AccountName]; !ok {
				cfg.Accounts[manual.AccountName] = config.Account{Type: manual.TypeName, Settings: map[string]config.Value{}}
			}
			if cfg.Manual == nil {
				cfg.Manual = map[string]provider.ManualServer{}
			}
			cfg.Manual[name] = s
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			m := manual.Machine(manual.AccountName, name, s)
			fmt.Fprintf(a.stdout, "Added %s. No provider API stands behind it: ump reaches it over SSH alone, and a failed installation is yours to recover (see ump server add --help).\n", describeRef(m))
			return nil
		},
	}
	cmd.Flags().StringVar(&s.IPv4, "ipv4", "", "the public IPv4 address")
	cmd.Flags().StringVar(&s.IPv6, "ipv6", "", "the public IPv6 address or prefix")
	cmd.Flags().StringVar(&s.Location, "location", "", "where it is (free text)")
	cmd.Flags().StringVar(&s.Product, "product", "", "the model or instance type (free text)")
	cmd.Flags().StringVar(&kind, "kind", "", "bare-metal (default) or cloud")
	cmd.Flags().StringVar(&s.Note, "note", "", "free text: how to reach a console, who rents it, how to recover it")
	return cmd
}

func (a *app) newServerRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove NAME [--yes]",
		Short: "Forget a server added by hand",
		Long: `Remove a manual server from the configuration (and what ump server set
remembered about it). The machine itself is not touched. Servers a
provider lists cannot be removed here: they are the provider's.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			s, ok := cfg.Manual[name]
			if !ok {
				if _, isAccount := cfg.Accounts[name]; isAccount {
					return fmt.Errorf("%s is an account, not a manual server (ump config isp remove %s)", name, name)
				}
				return fmt.Errorf("no manual server %q (ump server list --account manual); servers a provider lists cannot be removed here", name)
			}
			m := manual.Machine(manual.AccountName, name, s)
			if err := a.confirm(cmd, yes, fmt.Sprintf("Forget %s? The machine itself is not touched.", describeRef(m))); err != nil {
				return err
			}
			delete(cfg.Manual, name)
			delete(cfg.Servers, m.Ref())
			if len(cfg.Manual) == 0 {
				cfg.Manual = nil
				if acct, ok := cfg.Accounts[manual.AccountName]; ok && acct.Type == manual.TypeName {
					delete(cfg.Accounts, manual.AccountName)
				}
			}
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Removed %s from the configuration.\n", describeRef(m))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// noRescueWarning is said before anything disruptive happens to a server
// whose provider has no rescue system: a manual one.
func noRescueWarning(m provider.Machine, prov provider.Provider) string {
	if _, ok := prov.(provider.Rescue); ok {
		return ""
	}
	return fmt.Sprintf("Warning: %s has no provider rescue system behind it (provider type %s). ump reaches it over SSH alone; if the installation fails or the new system does not boot, the machine is yours to recover (a console, the provider's own rescue system).", describeRef(m), m.Provider)
}

func (a *app) warnNoRescue(m provider.Machine, prov provider.Provider) {
	if w := noRescueWarning(m, prov); w != "" {
		fmt.Fprintln(a.stderr, w)
	}
}
