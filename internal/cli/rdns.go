package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

var rdnsAliases = []string{"ptr", "reverse-dns"}

// ptrRow is one line of `ump server rdns get`.
type ptrRow struct {
	Provider string `json:"provider" yaml:"provider"`
	Account  string `json:"account" yaml:"account"`
	ID       string `json:"id" yaml:"id"`
	Server   string `json:"server" yaml:"server"`
	IP       string `json:"ip" yaml:"ip"`
	PTR      string `json:"ptr,omitempty" yaml:"ptr,omitempty"`
}

func (a *app) newGetRDNSCmd() *cobra.Command {
	var (
		output    string
		noHeaders bool
	)
	cmd := &cobra.Command{
		Use:     "get [NAME|ID|IP|PROVIDER/ID ...]",
		Aliases: []string{"list", "ls"},
		Short:   "List reverse DNS entries of servers",
		Long: `List the reverse DNS (PTR) entries of the public addresses of servers:
the primary IPv4 always, plus every other address of the server that has
an entry. Without references, every server of the selected accounts is
listed. Providers that cannot manage reverse DNS are noted on stderr.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.getRDNS(cmd, args, output, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line of table output")
	return cmd
}

func (a *app) newServerRDNSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rdns",
		Aliases: rdnsAliases,
		Short:   "Reverse DNS entries of servers: get, set, delete",
		Long: `The reverse DNS (PTR) entries of servers' public addresses: list them,
set one, or delete one. Providers that cannot manage reverse DNS say so.`,
		Example: `  ump server rdns get
  ump server rdns set web-01 web-01.example.net
  ump server rdns delete web-01 --ip 2001:db8::1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageErrorf(cmd, "unknown command %q for ump server rdns (get, set, delete)", args[0])
		},
	}
	cmd.AddCommand(a.newGetRDNSCmd(), a.newSetRDNSCmd(), a.newDeleteRDNSCmd())
	return cmd
}

func (a *app) newSetRDNSCmd() *cobra.Command {
	var (
		ip     string
		output string
	)
	cmd := &cobra.Command{
		Use:   "set NAME|ID|IP|PROVIDER/ID [PTR-NAME]",
		Short: "Set the reverse DNS entry of a server address",
		Long: `Set the reverse DNS (PTR) entry of one of a server's public addresses:
the primary IPv4 unless --ip names another address of the server (an
IPv6 address inside the server's prefix, an additional IP). Without a
PTR name, the server's name is used, which is the convention: the
machine name is the hostname is the reverse DNS.

Some providers check that the name resolves to the address (OVHcloud
does); create the forward record first there.`,
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			return a.setRDNS(cmd, args[0], name, ip, output, false)
		},
	}
	cmd.Flags().StringVar(&ip, "ip", "", "the address to set the entry for (default: the server's primary IPv4)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "print the resulting entry as json or yaml instead of a sentence")
	return cmd
}

func (a *app) newDeleteRDNSCmd() *cobra.Command {
	var ip string
	cmd := &cobra.Command{
		Use:     "delete NAME|ID|IP|PROVIDER/ID",
		Aliases: []string{"rm", "unset"},
		Short:   "Remove the reverse DNS entry of a server address",
		Long: `Remove the reverse DNS (PTR) entry of one of a server's public
addresses: the primary IPv4 unless --ip names another address. Providers
with a default entry (Hetzner Cloud) show that again afterwards.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.setRDNS(cmd, args[0], "", ip, "", true)
		},
	}
	cmd.Flags().StringVar(&ip, "ip", "", "the address to remove the entry of (default: the server's primary IPv4)")
	return cmd
}

// listedMachines lists the selected accounts and returns the machines,
// the results by account and how many accounts failed.
func (a *app) listedMachines(cmd *cobra.Command) ([]provider.Machine, map[string]listResult, int, int, error) {
	cfg, path, err := a.loadConfig()
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// the state's manual servers bring their account along
	if _, err := a.loadState(cmd.Context(), cfg); err != nil {
		return nil, nil, 0, 0, err
	}
	if len(cfg.Accounts) == 0 {
		return nil, nil, 0, 0, fmt.Errorf("no accounts configured in %s; run `ump config isp` to add one, or ump server add for a server by hand", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	ctx := cmd.Context()
	results := a.listAccounts(ctx, cfg, names, cache.Open(path), a.cachedListing(cfg))
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, 0, err
	}
	var machines []provider.Machine
	byAccount := map[string]listResult{}
	failed := 0
	for _, r := range results {
		byAccount[r.account] = r
		if r.err != nil {
			failed++
			fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
			continue
		}
		machines = append(machines, r.machines...)
	}
	provider.SortMachines(machines)
	return machines, byAccount, failed, len(results), nil
}

func (a *app) getRDNS(cmd *cobra.Command, refs []string, output string, noHeaders bool) error {
	format, err := parseFormat(output)
	if err != nil || format == formatName || format == formatWide {
		return usageErrorf(cmd, "unknown output format %q (use one of table, json, yaml)", output)
	}
	machines, byAccount, failed, total, err := a.listedMachines(cmd)
	if err != nil {
		return err
	}
	var notFound []string
	if len(refs) > 0 {
		machines, notFound = selectMachines(machines, refs)
	}

	type result struct {
		rows []ptrRow
		err  error
	}
	results := make([]result, len(machines))
	unsupported := map[string]bool{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, m := range machines {
		rd, ok := byAccount[m.Account].prov.(provider.ReverseDNS)
		if !ok {
			unsupported[m.Account] = true
			continue
		}
		wg.Add(1)
		go func(i int, m provider.Machine, rd provider.ReverseDNS) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ptrs, err := rd.PTRs(cmd.Context(), m)
			if err != nil {
				results[i].err = fmt.Errorf("account %s: %s: %w", m.Account, m.Ref(), err)
				return
			}
			for _, p := range ptrs {
				results[i].rows = append(results[i].rows, ptrRow{Provider: m.Provider, Account: m.Account, ID: m.ID, Server: m.Name, IP: p.IP, PTR: p.Name})
			}
		}(i, m, rd)
	}
	wg.Wait()
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	rows := []ptrRow{}
	errs := 0
	for _, r := range results {
		if r.err != nil {
			errs++
			fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
			continue
		}
		rows = append(rows, r.rows...)
	}
	if len(rows) > 0 || format != formatTable {
		if err := printPTRRows(a.stdout, rows, format, noHeaders); err != nil {
			return err
		}
	} else if len(refs) == 0 && failed == 0 && errs == 0 && len(unsupported) == 0 {
		fmt.Fprintln(a.stderr, "No servers found.")
	}
	for acct := range unsupported {
		fmt.Fprintf(a.stderr, "account %s: provider type %s cannot manage reverse DNS\n", acct, byAccount[acct].typ)
	}
	for _, ref := range notFound {
		fmt.Fprintf(a.stderr, "ump: server %q not found\n", ref)
	}
	switch {
	case failed > 0 || errs > 0:
		return withExit(exitProvider, fmt.Errorf("%d of %d accounts or servers could not be read", failed+errs, total+len(machines)))
	case len(notFound) > 0:
		return withExit(exitGeneric, fmt.Errorf("%d of %d servers not found", len(notFound), len(refs)))
	}
	return nil
}

func printPTRRows(w interface{ Write([]byte) (int, error) }, rows []ptrRow, f outputFormat, noHeaders bool) error {
	if f == formatJSON || f == formatYAML {
		return printEncoded(w, rows, f)
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		ptr := r.PTR
		if ptr == "" {
			ptr = "<none>"
		}
		table = append(table, []string{r.Account, r.Server, r.IP, ptr})
	}
	return writeTable(w, []string{"ACCOUNT", "SERVER", "IP", "PTR"}, table, noHeaders)
}

func (a *app) setRDNS(cmd *cobra.Command, ref, name, ipFlag, output string, remove bool) error {
	format := outputFormat("")
	if output != "" {
		f, err := parseFormat(output)
		if err != nil || (f != formatJSON && f != formatYAML) {
			return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
		}
		format = f
	}
	if !remove && name != "" {
		name = strings.TrimSuffix(name, ".")
		if err := validateServerName(name); err != nil {
			return usageErrorf(cmd, "%v", err)
		}
	}
	var ip netip.Addr
	if ipFlag != "" {
		addr, err := netip.ParseAddr(ipFlag)
		if err != nil {
			return usageErrorf(cmd, "invalid address %q", ipFlag)
		}
		ip = addr
	}

	machines, byAccount, failed, total, err := a.listedMachines(cmd)
	if err != nil {
		return err
	}
	m, err := a.resolveServer(a.resolverUI(cmd), machines, ref)
	switch {
	case errors.Is(err, errQuit):
		return withExit(exitAborted, errors.New("aborted"))
	case err != nil:
		var nf *notFoundError
		if errors.As(err, &nf) && failed > 0 {
			return fmt.Errorf("%w (%d of %d accounts could not be listed)", err, failed, total)
		}
		return err
	}
	rd, ok := byAccount[m.Account].prov.(provider.ReverseDNS)
	if !ok {
		return fmt.Errorf("account %s: provider type %s cannot manage reverse DNS", m.Account, m.Provider)
	}
	if !ip.IsValid() {
		if m.IPv4 == "" {
			return fmt.Errorf("%s has no primary IPv4 address; use --ip", m.Ref())
		}
		ip, err = netip.ParseAddr(m.IPv4)
		if err != nil {
			return fmt.Errorf("%s: unexpected primary address %q", m.Ref(), m.IPv4)
		}
	}
	if !remove && name == "" {
		name = m.Name
		if err := validateServerName(name); err != nil {
			return fmt.Errorf("the server name %q cannot be used as reverse DNS entry; give a name explicitly: %v", m.Name, err)
		}
	}
	defer a.invalidateListing(m.Account)
	if err := rd.SetPTR(cmd.Context(), m, ip, name); err != nil {
		return fmt.Errorf("account %s: %w", m.Account, err)
	}
	if remove {
		fmt.Fprintf(a.stdout, "Removed reverse DNS of %s (%s, account %s)\n", ip, m.Ref(), m.Account)
		return nil
	}
	if format != "" {
		return printEncoded(a.stdout, ptrRow{Provider: m.Provider, Account: m.Account, ID: m.ID, Server: m.Name, IP: ip.String(), PTR: name}, format)
	}
	fmt.Fprintf(a.stdout, "Set reverse DNS of %s (%s, account %s) to %s\n", ip, m.Ref(), m.Account, name)
	return nil
}
