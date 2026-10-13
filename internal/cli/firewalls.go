package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
)

func firewallsPath() string { return filepath.Join(config.ProfilesDir(), "firewalls.yaml") }

var firewallSet = &namedSet{
	file: "firewalls.yaml", key: "firewalls", noun: "firewall",
	use:     "firewall",
	aliases: []string{"fw", "firewalls"},
	short:   "The named firewalls the profiles refer to (interactive wizard)",
	long: `The firewalls live in firewalls.yaml next to the profiles
(` + config.AbbreviateHome(firewallsPath()) + `), one named entry each; a profile
names one in its firewall field, or carries its own. The first use puts
the published firewalls there. A firewall is the input policy (drop or
accept) and what it lets in (icmp, ssh, tcp/80, udp/53, or {proto, port,
from}); the same list holds for connections forwarded in from outside,
so a port a container publishes is reachable only when it is allowed.
profiles/README.md in the repository describes the fields.

Without a subcommand and on a terminal, ump config firewall is a small
wizard over the subcommands below.`,
	example: `  ump config fw list
  ump config fw get web
  ump config fw create mail --from default   # then your editor
  ump config fw edit
  ump config fw delete mail`,
	defaultFrom: "default",
	headers:     []string{"NAME", "POLICY", "ALLOW", "STATE", "DESCRIPTION"},
	rows: func(data []byte, names []string) ([][]string, error) {
		firewalls, err := profile.ParseFirewalls(data)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, n := range names {
			f := firewalls[n]
			state := "ok"
			if err := f.Validate(); err != nil {
				state = "invalid"
			}
			rows = append(rows, []string{n, f.Policy, allowSummary(f), state, f.Description})
		}
		return rows, nil
	},
	check: func(data []byte) error {
		firewalls, err := profile.ParseFirewalls(data)
		if err != nil {
			return err
		}
		for _, n := range sortedKeys(firewalls) {
			if err := firewalls[n].Validate(); err != nil {
				return err
			}
		}
		return nil
	},
}

// allowSummary renders the allowed entries as they are written: icmp,
// ssh, tcp/80, udp/53 from 10.0.0.0/8.
func allowSummary(f *profile.Firewall) string {
	if f.Policy == "accept" {
		return "everything"
	}
	var parts []string
	for _, a := range f.Allow {
		s := a.Proto
		switch {
		case a.Proto == "tcp" && a.Port == "22" && a.From == "":
			s = "ssh"
		case a.Proto != "icmp":
			s += "/" + a.Port
		}
		if a.From != "" {
			s += " from " + a.From
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func (a *app) newConfigFirewallCmd() *cobra.Command { return firewallSet.newCmd(a) }

// loadFirewalls reads firewalls.yaml: the user's own in the profiles
// directory, else the published one.
func (a *app) loadFirewalls(ctx context.Context, storage string) (map[string]*profile.Firewall, error) {
	data, from, err := a.loadNamed(ctx, storage, "firewalls.yaml")
	if err != nil {
		return nil, err
	}
	firewalls, err := profile.ParseFirewalls(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return firewalls, nil
}
