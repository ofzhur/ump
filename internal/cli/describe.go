package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

var describeResources = []string{"server"}

func (a *app) newDescribeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe RESOURCE NAME...",
		Short: "Show details of a resource",
		Long: `Show everything the provider knows about a resource.

Resources: server (aliases: servers, srv, machine, machines)`,
		Example: `  ump describe server web-01
  ump describe server hetzner-robot/123456 -o json
  ump describe server 203.0.113.10 --account hetzner-main`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return usageErrorf(cmd, "unknown resource type %q (known: %s)", args[0], strings.Join(describeResources, ", "))
		},
	}
	cmd.AddCommand(a.newDescribeServerCmd())
	return cmd
}

func (a *app) newDescribeServerCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "server NAME|ID|IP|PROVIDER/ID ...",
		Aliases: []string{"servers", "srv", "machine", "machines"},
		Short:   "Show details of one or more servers",
		Long: `Show the common fields of a server and everything else its provider
reports about it: contract and rescue state at Hetzner Robot, type, image,
network and protection at Hetzner Cloud, service, boot and hardware at
OVHcloud, and so on.

Servers are found by name, provider ID, IP address or "provider/id" across
the configured accounts (--account restricts the search). Names are not
unique: when a name matches several servers you are asked which one is
meant; without a terminal the command fails and lists them, so use
"provider/id" or --account there.`,
		Args: usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.describeServers(cmd, args, output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text, json or yaml (json and yaml print an array)")
	return cmd
}

func (a *app) describeServers(cmd *cobra.Command, refs []string, output string) error {
	format, err := parseDescribeFormat(output)
	if err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured in %s; run `ump config` to add one", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	results := a.listAccounts(ctx, cfg, names, cache.Open(path))
	if err := ctx.Err(); err != nil {
		return err
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

	var details []provider.Details
	notFound, providerErrors := 0, 0
	ui := a.resolverUI(cmd)
	for _, ref := range refs {
		m, err := a.resolveServer(ui, machines, ref)
		var nf *notFoundError
		var amb *ambiguousError
		switch {
		case errors.As(err, &nf):
			notFound++
			if failed > 0 {
				fmt.Fprintf(a.stderr, "ump: %v (%d of %d accounts could not be listed)\n", err, failed, len(results))
			} else {
				fmt.Fprintf(a.stderr, "ump: %v\n", err)
			}
			continue
		case errors.As(err, &amb):
			notFound++
			fmt.Fprintf(a.stderr, "ump: %v\n", err)
			continue
		case errors.Is(err, errQuit):
			return withExit(exitAborted, errors.New("aborted"))
		case err != nil:
			return err
		}
		d, err := describeMachine(ctx, byAccount[m.Account].prov, m)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			providerErrors++
			fmt.Fprintf(a.stderr, "ump: account %s: %v\n", m.Account, err)
			continue
		}
		details = append(details, *d)
	}

	if len(details) > 0 || format != formatText {
		if err := printDetails(a.stdout, details, format); err != nil {
			return err
		}
	}
	switch {
	case providerErrors > 0 || failed > 0:
		return withExit(exitProvider, fmt.Errorf("%d of %d servers described", len(details), len(refs)))
	case notFound > 0:
		return withExit(exitGeneric, fmt.Errorf("%d of %d servers described", len(details), len(refs)))
	}
	return nil
}

// describeMachine asks the provider for details, or falls back to the
// listing's view when the provider cannot describe machines.
func describeMachine(ctx context.Context, p provider.Provider, m provider.Machine) (*provider.Details, error) {
	if d, ok := p.(provider.Describer); ok {
		return d.Describe(ctx, m)
	}
	return &provider.Details{Machine: m}, nil
}

const formatText outputFormat = "text"

func parseDescribeFormat(s string) (outputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return formatText, nil
	case "json":
		return formatJSON, nil
	case "yaml":
		return formatYAML, nil
	}
	return "", fmt.Errorf("unknown output format %q (use one of text, json, yaml)", s)
}

func printDetails(w io.Writer, ds []provider.Details, f outputFormat) error {
	if ds == nil {
		ds = []provider.Details{}
	}
	if f == formatJSON || f == formatYAML {
		return printEncoded(w, ds, f)
	}
	for i, d := range ds {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeDescription(w, d)
	}
	return nil
}

// writeDescription prints one machine kubectl-describe style: the common
// fields, then each section indented under its name.
func writeDescription(w io.Writer, d provider.Details) {
	m := d.Machine
	top := []provider.Item{
		{Key: "name", Label: "Name", Value: m.Name},
		{Key: "ref", Label: "Ref", Value: m.Ref()},
		{Key: "account", Label: "Account", Value: m.Account},
		{Key: "provider", Label: "Provider", Value: m.Provider},
		{Key: "kind", Label: "Kind", Value: string(m.Kind)},
		{Key: "status", Label: "Status", Value: m.Status},
		{Key: "location", Label: "Location", Value: m.Location},
		{Key: "product", Label: "Product", Value: m.Product},
		{Key: "ipv4", Label: "IPv4", Value: m.IPv4},
		{Key: "ipv6", Label: "IPv6", Value: m.IPv6},
		{Key: "labels", Label: "Labels", Value: m.Labels},
	}
	writeItems(w, "", top)
	for _, s := range d.Sections {
		fmt.Fprintf(w, "\n%s:\n", s.Name)
		writeItems(w, "  ", s.Items)
	}
}

func writeItems(w io.Writer, indent string, items []provider.Item) {
	width := 0
	for _, it := range items {
		if n := len(it.DisplayLabel()) + 1; n > width {
			width = n
		}
	}
	for _, it := range items {
		lines := valueLines(it.Value)
		fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, it.DisplayLabel()+":", lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, "", l)
		}
	}
}

// valueLines renders a value for text output; lists and label maps take
// one line per entry, empty values show as <none>.
func valueLines(v any) []string {
	switch x := v.(type) {
	case nil:
		return []string{"<none>"}
	case string:
		if x == "" {
			return []string{"<none>"}
		}
		return []string{x}
	case bool:
		if x {
			return []string{"yes"}
		}
		return []string{"no"}
	case fmt.Stringer:
		return []string{x.String()}
	case []string:
		if len(x) == 0 {
			return []string{"<none>"}
		}
		return x
	case []any:
		if len(x) == 0 {
			return []string{"<none>"}
		}
		lines := make([]string, len(x))
		for i, e := range x {
			lines[i] = fmt.Sprint(e)
		}
		return lines
	case map[string]string:
		if len(x) == 0 {
			return []string{"<none>"}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines := make([]string, len(keys))
		for i, k := range keys {
			lines[i] = k + "=" + x[k]
		}
		return lines
	}
	return []string{fmt.Sprint(v)}
}
