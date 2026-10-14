package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/cache"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/provider"
)

var getResources = []string{"servers", "rdns"}

type getServersOptions struct {
	output    string
	noHeaders bool
	diff      bool
	since     string
	cached    bool
}

func (a *app) newServerListCmd() *cobra.Command {
	var opts getServersOptions
	cmd := &cobra.Command{
		Use:     "list [NAME|ID|IP|PROVIDER/ID ...]",
		Aliases: []string{"ls", "l", "get", "g"},
		Short:   "List servers of all configured accounts",
		Long: `List servers (dedicated machines and cloud instances) from every
configured provider account, or from the accounts given with --account.

Positional arguments select single servers by name, provider ID, IP
address or "provider/id". Accounts of different provider types are
queried in parallel, accounts of one type one after the other; an account
whose API fails is reported on stderr and the others are still listed.

Every listing is recorded in the cache under ~/.cache/ump, as a new
snapshot only when something changed. --diff compares the current
listing with the previous one and prints what was added, removed or
changed; --since compares with the state as of an earlier time; --cached
prints the last listing without calling the providers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.getServers(cmd, args, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "output format: table, wide, json, yaml or name (default: the configured default, else table)")
	cmd.Flags().BoolVar(&opts.noHeaders, "no-headers", false, "omit the header line of table output")
	cmd.Flags().BoolVar(&opts.diff, "diff", false, "show what changed since the previous listing: added, removed and changed servers")
	cmd.Flags().StringVar(&opts.since, "since", "", "compare with the listing as of this time (7d, 12h, 2026-10-01, RFC 3339); implies --diff")
	cmd.Flags().BoolVar(&opts.cached, "cached", false, "print the last listing from the cache without calling the providers")
	return cmd
}

func (a *app) getServers(cmd *cobra.Command, refs []string, opts getServersOptions) error {
	if opts.since != "" {
		opts.diff = true
	}
	switch {
	case opts.cached && opts.diff:
		return usageErrorf(cmd, "--cached cannot be combined with --diff or --since")
	case opts.diff && len(refs) > 0:
		return usageErrorf(cmd, "--diff compares whole listings; it cannot be combined with server references")
	}
	cfg, path, err := a.loadConfig()
	if err != nil {
		return err
	}
	if opts.output == "" {
		opts.output = cfg.Defaults.Output
	}
	format, err := parseFormat(opts.output)
	if err != nil {
		return usageErrorf(cmd, "%v", err)
	}
	if _, err := a.loadState(cmd.Context(), cfg, path); err != nil {
		return err
	}
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("no accounts configured in %s; run `ump config isp` to add one, or ump server add for a server by hand", config.AbbreviateHome(path))
	}
	names, err := a.selectAccounts(cmd, cfg)
	if err != nil {
		return err
	}
	store := cache.Open(path)
	ctx := cmd.Context()
	if opts.diff {
		return a.diffServers(cmd, store, cfg, names, opts.since, format, opts.noHeaders)
	}

	var results []listResult
	failCode := exitProvider
	if opts.cached {
		var notes []string
		results, notes = a.cachedListings(store, cfg, names)
		if len(notes) > 0 {
			fmt.Fprintf(a.stderr, "From cache: %s\n", strings.Join(notes, ", "))
		}
		failCode = exitGeneric
	} else {
		results = a.listAccounts(ctx, cfg, names, store)
		if err := ctx.Err(); err != nil {
			return err
		}
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

	var notFound, duplicates []string
	if len(refs) > 0 {
		for _, ref := range refs {
			if ms := matching(machines, ref); len(ms) > 1 {
				duplicates = append(duplicates, fmt.Sprintf("%q matches %d servers: %s", ref, len(ms), candidates(ms)))
			}
		}
		machines, notFound = selectMachines(machines, refs)
	}

	if len(machines) == 0 && (format == formatTable || format == formatWide) {
		if len(refs) == 0 && len(failed) == 0 {
			fmt.Fprintln(a.stderr, "No servers found.")
		}
	} else if err := printMachines(a.stdout, machines, format, opts.noHeaders); err != nil {
		return err
	}

	for _, d := range duplicates {
		fmt.Fprintf(a.stderr, "Note: %s\n", d)
	}
	for _, r := range failed {
		fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
	}
	for _, ref := range notFound {
		fmt.Fprintf(a.stderr, "ump: server %q not found\n", ref)
	}
	switch {
	case len(failed) > 0:
		return withExit(failCode, fmt.Errorf("%d of %d accounts could not be listed", len(failed), len(results)))
	case len(notFound) > 0:
		return withExit(exitGeneric, fmt.Errorf("%d of %d servers not found", len(notFound), len(refs)))
	}
	return nil
}

// diffReport is the result of `ump server list --diff`.
type diffReport struct {
	Accounts  []accountDiff  `json:"accounts" yaml:"accounts"`
	Changes   []cache.Change `json:"changes" yaml:"changes"`
	Unchanged int            `json:"unchanged" yaml:"unchanged"`
}

type accountDiff struct {
	Account  string `json:"account" yaml:"account"`
	Compared bool   `json:"compared" yaml:"compared"`
	// Since is the time the account's listing is compared with.
	Since *time.Time `json:"since,omitempty" yaml:"since,omitempty"`
	// Reason says why the account was not compared.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

func (a *app) diffServers(cmd *cobra.Command, store *cache.Store, cfg *config.Config, names []string, sinceFlag string, format outputFormat, noHeaders bool) error {
	ctx := cmd.Context()
	now := a.now()
	var since time.Time
	if sinceFlag != "" {
		t, err := parseSince(sinceFlag, now, a.tz)
		if err != nil {
			return usageErrorf(cmd, "%v", err)
		}
		since = t
	}
	baselines := make(map[string]*cache.Snapshot, len(names))
	for _, n := range names {
		var snap *cache.Snapshot
		var err error
		if sinceFlag != "" {
			snap, err = store.AsOf(n, since)
		} else {
			snap, err = store.Latest(n)
		}
		if err != nil {
			return fmt.Errorf("cache: %w", err)
		}
		baselines[n] = snap
	}
	results := a.listAccounts(ctx, cfg, names, store)
	if err := ctx.Err(); err != nil {
		return err
	}

	report := diffReport{Changes: []cache.Change{}}
	failed := 0
	var notes []string
	for _, r := range results {
		ad := accountDiff{Account: r.account}
		switch {
		case r.err != nil:
			failed++
			fmt.Fprintf(a.stderr, "ump: %v\n", r.err)
			ad.Reason = "listing failed"
		case baselines[r.account] == nil:
			switch oldest, ok := store.Oldest(r.account); {
			case sinceFlag != "" && ok:
				ad.Reason = fmt.Sprintf("no listing from before %s (the cache starts %s)", a.fmtTime(since), a.fmtTime(oldest))
			default:
				ad.Reason = fmt.Sprintf("first listing, nothing to compare yet (%s recorded)", plural(len(r.machines), "server"))
			}
			notes = append(notes, "account "+r.account+": "+ad.Reason)
		default:
			b := baselines[r.account]
			changes, unchanged := cache.Diff(b.Machines, r.machines)
			t := b.SeenAt
			if sinceFlag != "" {
				t = since
			}
			ad.Compared, ad.Since = true, &t
			report.Changes = append(report.Changes, changes...)
			report.Unchanged += unchanged
		}
		report.Accounts = append(report.Accounts, ad)
	}
	cache.SortChanges(report.Changes)

	switch format {
	case formatJSON, formatYAML:
		if err := printEncoded(a.stdout, report, format); err != nil {
			return err
		}
	default:
		if len(report.Changes) > 0 {
			if err := printChanges(a.stdout, report.Changes, format, noHeaders); err != nil {
				return err
			}
		}
	}
	for _, n := range notes {
		fmt.Fprintln(a.stderr, n)
	}
	if summary := a.diffSummary(report); summary != "" {
		fmt.Fprintln(a.stderr, summary)
	}
	if failed > 0 {
		return withExit(exitProvider, fmt.Errorf("%d of %d accounts could not be listed", failed, len(results)))
	}
	return nil
}

// diffSummary is the one-line result for people: counts and the time the
// listing was compared with. Empty when no account could be compared.
func (a *app) diffSummary(r diffReport) string {
	var times []string
	seen := map[string]bool{}
	differ := false
	for _, ad := range r.Accounts {
		if !ad.Compared {
			continue
		}
		s := a.fmtTime(*ad.Since)
		if len(seen) > 0 && !seen[s] {
			differ = true
		}
		if !seen[s] {
			seen[s] = true
			times = append(times, s)
		}
	}
	if len(times) == 0 {
		return ""
	}
	since := times[0]
	if differ {
		var parts []string
		for _, ad := range r.Accounts {
			if ad.Compared {
				parts = append(parts, a.fmtTime(*ad.Since)+" ("+ad.Account+")")
			}
		}
		since = strings.Join(parts, ", ")
	}
	counts := map[string]int{}
	for _, c := range r.Changes {
		counts[c.Change]++
	}
	if len(r.Changes) == 0 {
		return fmt.Sprintf("No changes since %s (%s).", since, plural(r.Unchanged, "server"))
	}
	return fmt.Sprintf("%d added, %d removed, %d changed, %d unchanged since %s.",
		counts[cache.Added], counts[cache.Removed], counts[cache.Changed], r.Unchanged, since)
}

var relativeSince = regexp.MustCompile(`^(\d+)\s*([smhdw])$`)

// parseSince understands durations back from now (30m, 12h, 7d, 2w, or
// anything time.ParseDuration accepts) and dates or timestamps.
func parseSince(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := relativeSince.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		units := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
		return now.Add(-time.Duration(n) * units[m[2]]), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: use a duration like 7d, 12h or 30m, or a date like 2026-10-01", s)
}

// cachedListings serves the last recorded listing of each account.
func (a *app) cachedListings(store *cache.Store, cfg *config.Config, names []string) ([]listResult, []string) {
	doc, _ := a.loadState(context.Background(), cfg, "")
	results := make([]listResult, 0, len(names))
	var notes []string
	for _, n := range names {
		r := listResult{account: n, typ: cfg.Accounts[n].Type}
		snap, err := store.Latest(n)
		switch {
		case err != nil:
			r.err = fmt.Errorf("account %s: cache: %w", n, err)
		case snap == nil:
			r.err = fmt.Errorf("account %s: not in the cache yet (run `ump server list` without --cached first)", n)
		default:
			r.machines = snap.Machines
			applyState(doc, r.machines)
			notes = append(notes, n+" ("+a.fmtTime(snap.SeenAt)+")")
		}
		results = append(results, r)
	}
	return results, notes
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
	cacheErr error
}

// listAccounts queries the named accounts in parallel and records each
// successful listing in the store (when one is given).
// listAccounts queries the accounts: different provider types in parallel,
// accounts of the same type one after the other, so that one provider's
// API never sees several of our accounts at once (rate limits are per
// API, and some count every account against the same client).
func (a *app) listAccounts(ctx context.Context, cfg *config.Config, names []string, store *cache.Store) []listResult {
	doc, err := a.loadState(ctx, cfg, "")
	if err != nil {
		fmt.Fprintf(a.stderr, "ump: %v\n", err)
	}
	results := make([]listResult, len(names))
	byType := map[string][]int{}
	var types []string
	for i, name := range names {
		t := cfg.Accounts[name].Type
		if _, seen := byType[t]; !seen {
			types = append(types, t)
		}
		byType[t] = append(byType[t], i)
	}
	var wg sync.WaitGroup
	for _, t := range types {
		wg.Add(1)
		go func(indexes []int) {
			defer wg.Done()
			for _, i := range indexes {
				results[i] = a.listAccount(ctx, names[i], cfg.Accounts[names[i]], store)
				applyState(doc, results[i].machines)
			}
		}(byType[t])
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool { return results[i].account < results[j].account })
	for _, r := range results {
		if r.cacheErr != nil {
			fmt.Fprintf(a.stderr, "ump: %v\n", r.cacheErr)
		}
	}
	return results
}

func (a *app) listAccount(ctx context.Context, name string, acct config.Account, store *cache.Store) listResult {
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
	if store != nil {
		if _, _, err := store.Record(name, acct.Type, ms, a.now()); err != nil {
			r.cacheErr = fmt.Errorf("account %s: cache: %w", name, err)
		}
	}
	return r
}
