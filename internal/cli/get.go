package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

var getResources = []string{"servers"}

func (a *app) newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get RESOURCE [NAME...]",
		Aliases: []string{"list"},
		Short:   "List resources",
		Long: `List resources from the configured provider accounts.

Resources: servers (aliases: server, srv, machines, machine)`,
		Example: `  # every server of every configured account
  ump get servers

  # one account, with product and IPv6 columns
  ump get servers --account hetzner-main -o wide

  # look a server up by name, ID, address or provider/ID
  ump get servers web-01 hetzner-cloud/7654321

  # for scripts and agents
  ump get servers -o json | jq '.[] | select(.status == "ready")'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageErrorf(cmd, "unknown resource type %q (known: %s)", args[0], strings.Join(getResources, ", "))
		},
	}
	cmd.AddCommand(a.newGetServersCmd())
	return cmd
}

func (a *app) newGetServersCmd() *cobra.Command {
	var (
		output    string
		noHeaders bool
	)
	cmd := &cobra.Command{
		Use:     "servers [NAME|ID|IP|PROVIDER/ID ...]",
		Aliases: []string{"server", "srv", "machines", "machine"},
		Short:   "List servers of all configured accounts",
		Long: `List servers (dedicated machines and cloud instances) from every
configured provider account, or from the accounts given with --account.

Positional arguments select single servers by name, provider ID, IP
address or "provider/id". Accounts are queried in parallel; an account
whose API fails is reported on stderr and the others are still listed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.getServers(cmd, args, output, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide, json, yaml or name (default: the configured default, else table)")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line of table output")
	return cmd
}

func (a *app) getServers(cmd *cobra.Command, refs []string, output string, noHeaders bool) error {
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	if output == "" {
		output = cfg.Defaults.Output
	}
	format, err := parseFormat(output)
	if err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured in %s; run `ump config` to add one", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return err
	}

	results := a.listAccounts(cmd.Context(), cfg, names)
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	var machines []provider.Machine
	var failed []listResult
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, r)
			continue
		}
		machines = append(machines, r.machines...)
	}
	provider.SortMachines(machines)

	var notFound []string
	if len(refs) > 0 {
		machines, notFound = selectMachines(machines, refs)
	}

	if len(machines) == 0 && (format == formatTable || format == formatWide) {
		if len(refs) == 0 && len(failed) == 0 {
			fmt.Fprintln(a.stderr, "No servers found.")
		}
	} else if err := printMachines(a.stdout, machines, format, noHeaders); err != nil {
		return err
	}

	for _, r := range failed {
		fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
	}
	for _, ref := range notFound {
		fmt.Fprintf(a.stderr, "ump: server %q not found\n", ref)
	}
	switch {
	case len(failed) > 0:
		return withExit(exitProvider, fmt.Errorf("%d of %d accounts could not be listed", len(failed), len(results)))
	case len(notFound) > 0:
		return withExit(exitGeneric, fmt.Errorf("%d of %d servers not found", len(notFound), len(refs)))
	}
	return nil
}

// selectMachines keeps the machines matching any of refs, in listing
// order, and returns the refs that matched nothing.
func selectMachines(ms []provider.Machine, refs []string) ([]provider.Machine, []string) {
	var out []provider.Machine
	matched := map[string]bool{}
	for _, m := range ms {
		hit := false
		for _, ref := range refs {
			if m.Matches(ref) {
				matched[ref] = true
				hit = true
			}
		}
		if hit {
			out = append(out, m)
		}
	}
	var missing []string
	for _, ref := range refs {
		if !matched[ref] {
			missing = append(missing, ref)
		}
	}
	return out, missing
}

type listResult struct {
	account  string
	typ      string
	prov     provider.Provider
	machines []provider.Machine
	err      error
}

// listAccounts queries the named accounts in parallel.
func (a *app) listAccounts(ctx context.Context, cfg *config.Config, names []string) []listResult {
	results := make([]listResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = a.listAccount(ctx, name, cfg.Accounts[name])
		}(i, name)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool { return results[i].account < results[j].account })
	return results
}

func (a *app) listAccount(ctx context.Context, name string, acct config.Account) listResult {
	r := listResult{account: name, typ: acct.Type}
	p, err := a.buildProvider(name, acct)
	if err != nil {
		r.err = err
		return r
	}
	r.prov = p
	inv, ok := p.(provider.Inventory)
	if !ok {
		r.err = fmt.Errorf("account %s: provider type %s cannot list servers", name, acct.Type)
		return r
	}
	ms, err := inv.List(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.err = err
		} else {
			r.err = fmt.Errorf("account %s: %w", name, err)
		}
		return r
	}
	r.machines = ms
	return r
}
