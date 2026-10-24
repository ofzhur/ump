package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// newServerCmd groups everything that happens to a server.
func (a *app) newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"s", "srv"},
		Short:   "List, inspect, install and control servers",
		Long: `Everything that happens to a server: listing and showing them, installing
an operating system the way a profile says and provisioning it with
Ansible from the infrastructure repository (ump provision afterwards),
the rescue system, power, names and reverse DNS. Servers are named by
name, provider ID, IP address or provider/id, and found across every
configured account.`,
		Example: `  ump server list
  ump server show web-01
  ump server reinstall web-01 --profile db --group db
  ump server multiinstall web-01 web-02 web-03 --group web
  ump server set web-01 --group monitoring
  ump server rescue web-01
  ump server reboot web-01 --wait
  ump server rdns set web-01 web-01.example.net
  ump server set web-01 --profile db`,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				return cmd.Help()
			case args[0] == "provision":
				return usageErrorf(cmd, "ump server provision is ump provision now: it runs the playbooks on the fleet, or with --hosts HOSTNAME on one server; ump server set NAME --group GROUP adds a group")
			}
			return usageErrorf(cmd, "unknown command %q for ump server", args[0])
		},
	}
	cmd.AddCommand(
		a.newServerListCmd(),
		a.newServerShowCmd(),
		a.newServerReinstallCmd(),
		a.newServerMultiinstallCmd(),
		a.newServerRescueCmd(),
		a.newServerUnrescueCmd(),
		a.newServerRebootCmd(),
		a.newServerStartCmd(),
		a.newServerStopCmd(),
		a.newServerRenameCmd(),
		a.newServerRDNSCmd(),
		a.newServerSetCmd(),
		a.newServerAddCmd(),
		a.newServerRemoveCmd(),
	)
	return cmd
}

// newServerSetCmd stores what the user chooses for one server.
func (a *app) newServerSetCmd() *cobra.Command {
	var (
		profile    string
		clear      bool
		price      string
		currency   string
		period     string
		clearPrice bool
		attrs      []string
		unset      []string
		groups     groupFlags
		ungroups   groupFlags
	)
	cmd := &cobra.Command{
		Use:   "set NAME|ID|IP|PROVIDER/ID [--profile NAME | --clear] [--price AMOUNT [--currency CUR] [--period month|hour] | --clear-price] [--attr KEY=VALUE]... [--unset-attr KEY]... [--group GROUP]... [--ungroup GROUP]...",
		Short: "Set what ump remembers about a server: its profile, its price, your attributes, its groups",
		Long: `Remember, in the state, what ump cannot learn from the provider: the
profile the server is reinstalled with (ump server reinstall NAME then
takes it without --profile; --clear forgets it), the price it costs
where the provider's API does not say (--price, with --currency and
--period; it wins over the API's price and shows in ump server list -o
wide and ump server show; --clear-price forgets it), attributes of your
own for the inventory (--attr owner=alice --attr role=db; --unset-attr
forgets one), and the groups of a server ump installed (--group adds,
--ungroup removes; repeatable, or comma-separated). The profile is
checked to exist, locally or on the storage, and a group to be no
host's name: not the server's hostname, nor, run from the
infrastructure repository, another host of its inventory. Nothing runs:
the next ump provision puts the server into its groups and runs their
plays on it; what the plays of a group it left did stays until a
reinstall.`,
		Example: `  ump server set web-01 --profile db
  ump server set web-01 --clear
  ump server set rack-07 --price 89 --currency EUR --period month
  ump server set web-01 --attr owner=alice --attr role=web
  ump server set web-01 --unset-attr role --clear-price
  ump server set web-01 --group monitoring --ungroup lab`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if profile != "" && clear {
				return usageErrorf(cmd, "--profile and --clear exclude each other")
			}
			if price != "" && clearPrice {
				return usageErrorf(cmd, "--price and --clear-price exclude each other")
			}
			if (currency != "" || period != "") && price == "" {
				return usageErrorf(cmd, "--currency and --period go with --price")
			}
			if profile == "" && !clear && price == "" && !clearPrice && len(attrs) == 0 && len(unset) == 0 && len(groups) == 0 && len(ungroups) == 0 {
				return usageErrorf(cmd, "give at least one of --profile, --clear, --price, --clear-price, --attr, --unset-attr, --group or --ungroup")
			}
			add, err := groups.list(cmd)
			if err != nil {
				return err
			}
			for _, g := range listFlag(ungroups).values() {
				if g == commonGroup {
					return usageErrorf(cmd, "--ungroup common: every server ump installed is in common")
				}
			}
			drop, err := ungroups.list(cmd)
			if err != nil {
				return err
			}
			for _, g := range drop {
				if slices.Contains(add, g) {
					return usageErrorf(cmd, "--group and --ungroup both name %s", g)
				}
			}
			var newPrice *provider.Price
			if price != "" {
				amount, err := strconv.ParseFloat(strings.Replace(price, ",", ".", 1), 64)
				if err != nil || amount < 0 {
					return usageErrorf(cmd, "--price %q is not an amount", price)
				}
				cur := strings.ToUpper(currency)
				if cur == "" {
					cur = "EUR"
				}
				if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(cur) {
					return usageErrorf(cmd, "--currency %q is not a three-letter code", currency)
				}
				switch period {
				case "":
					period = "month"
				case "month", "hour":
				default:
					return usageErrorf(cmd, "--period %q is not month or hour", period)
				}
				newPrice = &provider.Price{Amount: amount, Currency: cur, Period: period}
			}
			newAttrs := map[string]string{}
			for _, kv := range attrs {
				k, v, ok := strings.Cut(kv, "=")
				k = strings.TrimSpace(k)
				if !ok || k == "" || !attrKey.MatchString(k) {
					return usageErrorf(cmd, "--attr %q: expected KEY=VALUE with a key of letters, digits, '-', '_' and '.'", kv)
				}
				newAttrs[k] = strings.TrimSpace(v)
			}
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			m, _, err := a.resolveOne(cmd, args[0])
			if err != nil {
				return err
			}
			if profile != "" {
				if _, _, err := a.loadProfile(cmd.Context(), profile, cfg.StorageURL()); err != nil {
					return err
				}
			}
			// a group is no host's name: not the server's hostname, nor
			// another host of the inventory where the repository is at hand
			var inv *inventory
			if len(add) > 0 {
				repo, _, err := a.inventoryRepo()
				if err != nil {
					return err
				}
				if repo != nil {
					if inv, err = a.readInventory(cmd.Context(), repo, ""); err != nil {
						return err
					}
				}
			}
			var said []string
			err = a.updateState(cmd.Context(), func(d *state.Document) error {
				st := d.Servers[m.Ref()]
				said = nil
				switch {
				case clear:
					st.Profile = ""
					said = append(said, fmt.Sprintf("no profile set; reinstall uses the default profile (%s)", cfg.DefaultProfileName()))
				case profile != "":
					st.Profile = profile
					said = append(said, fmt.Sprintf("profile %s; ump server reinstall %s uses it", profile, m.Name))
				}
				switch {
				case clearPrice:
					st.Price = nil
					said = append(said, "no price set")
				case newPrice != nil:
					st.Price = newPrice
					said = append(said, "price "+newPrice.String())
				}
				if len(newAttrs) > 0 || len(unset) > 0 {
					if st.Attributes == nil {
						st.Attributes = map[string]string{}
					}
					for k, v := range newAttrs {
						st.Attributes[k] = v
					}
					for _, k := range unset {
						delete(st.Attributes, k)
					}
					if len(st.Attributes) == 0 {
						st.Attributes = nil
						said = append(said, "no attributes")
					} else {
						said = append(said, "attributes "+joinMap(st.Attributes))
					}
				}
				if len(add) > 0 || len(drop) > 0 {
					if st.Installed == nil {
						return withExit(exitUsage, fmt.Errorf("ump did not install %s: its groups come with the installation (ump server reinstall %s --group GROUP)", m.Name, args[0]))
					}
					for _, g := range drop {
						if !slices.Contains(st.Groups, g) {
							return withExit(exitUsage, fmt.Errorf("%s is not in the group %s (its groups: %s)", m.Name, g, strings.Join(append([]string{commonGroup}, st.Groups...), ", ")))
						}
					}
					for _, g := range add {
						if g == inventoryHostname(m, *st.Installed) {
							return withExit(exitUsage, fmt.Errorf("--group %s is the hostname of %s: %s", g, m.Name, sameName))
						}
						if inv == nil {
							continue
						}
						if r, ok := inv.hosts[g]; ok && r != m.Ref() {
							return withExit(exitUsage, fmt.Errorf("--group %s is a host of the inventory: %s; rename the host, or use another group", g, sameName))
						}
					}
					st.Groups = slices.DeleteFunc(st.Groups, func(g string) bool { return slices.Contains(drop, g) })
					for _, g := range add {
						if !slices.Contains(st.Groups, g) {
							st.Groups = append(st.Groups, g)
						}
					}
					said = append(said, "groups "+strings.Join(append([]string{commonGroup}, st.Groups...), ", ")+" (ump provision applies them)")
				}
				d.Servers[m.Ref()] = st
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s.\n", describeRef(m), strings.Join(said, "; "))
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "the profile (a name or a file)")
	cmd.Flags().BoolVar(&clear, "clear", false, "forget the profile set for the server")
	cmd.Flags().StringVar(&price, "price", "", "what the server costs per period, e.g. 39.90")
	cmd.Flags().StringVar(&currency, "currency", "", "the currency of --price, a three-letter code (default EUR)")
	cmd.Flags().StringVar(&period, "period", "", "the period of --price: month (default) or hour")
	cmd.Flags().BoolVar(&clearPrice, "clear-price", false, "forget the price set for the server")
	cmd.Flags().StringArrayVar(&attrs, "attr", nil, "an attribute of your own, KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&unset, "unset-attr", nil, "forget an attribute (repeatable)")
	groups.add(cmd, "group", "a group to put a server ump installed into")
	ungroups.add(cmd, "ungroup", "a group to take a server ump installed out of")
	return cmd
}

var attrKey = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
