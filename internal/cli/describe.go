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
	"github.com/ofzhur/ump/internal/state"
)

var describeResources = []string{"server"}

func (a *app) newServerShowCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "show NAME|ID|IP|PROVIDER/ID ...",
		Aliases: []string{"v", "view", "describe", "info"},
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
	if _, err := a.loadState(cmd.Context(), cfg); err != nil {
		return err
	}
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured in %s; run `ump config isp` to add one, or ump server add for a server by hand", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	results := a.listAccounts(ctx, cfg, names, cache.Open(path), a.cachedListing(cfg))
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
		if err == nil {
			doc, _ := a.loadState(ctx, cfg)
			d.Sections = append(d.Sections, umpSection(doc, m)...)
		}
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
// umpSection is what ump itself knows about a server: the profile set
// for it, its price and the user's attributes.
func umpSection(doc *state.Document, m provider.Machine) []provider.Section {
	var s state.Server
	if doc != nil {
		s = doc.Servers[m.Ref()]
	}
	var items []provider.Item
	if s.Profile != "" {
		items = append(items, provider.Item{Key: "profile", Value: s.Profile})
	}
	if s.Note != "" {
		items = append(items, provider.Item{Key: "note", Value: s.Note})
	}
	if m.Price != nil {
		v := m.Price.String() + " (" + m.Price.Source
		if m.Price.Note != "" {
			v += ", " + m.Price.Note
		}
		items = append(items, provider.Item{Key: "price", Value: v + ")"})
	}
	if len(m.Attributes) > 0 {
		items = append(items, provider.Item{Key: "attributes", Value: m.Attributes})
	}
	if len(s.Roles) > 0 {
		items = append(items, provider.Item{Key: "roles", Value: strings.Join(s.Roles, ", ")})
	}
	if in := s.Installed; in != nil {
		v := in.OS
		if in.ImageVersion != "" {
			v += " " + in.ImageVersion
		}
		v += " (profile " + in.Profile
		if in.Layout != "" {
			v += ", layout " + in.Layout
		}
		if in.Admin != "" {
			v += ", administrator " + in.Admin
		}
		items = append(items, provider.Item{Key: "installed", Value: v + ") on " + in.At.Format("2006-01-02 15:04 MST")})
		if len(in.Interfaces) > 0 {
			items = append(items, provider.Item{Key: "interfaces", Value: in.Interfaces})
		}
	}
	if len(items) == 0 {
		return nil
	}
	return []provider.Section{{Name: "UMP", Items: items}}
}

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
