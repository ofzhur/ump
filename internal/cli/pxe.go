package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
	pxeprovider "github.com/ofzhur/ump/internal/provider/pxe"
	"github.com/ofzhur/ump/internal/pxe"
	"github.com/ofzhur/ump/internal/state"
)

// The machines ump boots over the network: pxe.yaml in the home, next
// to config.yaml (internal/pxe). ump config pxe edits it and renders
// what the PXE servers of each site serve, site.json under .pxe/<site>/
// in the backend; a hand edit reaches no server until ump config pxe
// apply.

func pxePath() string { return filepath.Join(config.UserConfigDir(), pxe.FileName) }

// pxeSkeleton is a new pxe.yaml.
const pxeSkeleton = `# The machines ump boots over the network: ump config pxe edits this file and
# renders what the PXE servers of each site serve (ump config pxe apply).
#
# unknown: ignore           # a MAC without an entry: ignore or local
# defaults:                 # under every machine; a site's, then a machine's values replace these
#   mode: local             # local (the disk) or rescue (UMP's rescue system at every boot)
#   rescue-keys: [oleg]     # root's keys in a standing rescue, besides UMP's and staging.keys
#   network:                # the installed system's uplink
#     ipv4: dhcp            # dhcp, none or {address: 192.0.2.17/24, gateway: 192.0.2.1}
#     dns: dhcp             # dhcp (the lease's) or [192.0.2.53, ...]
# sites:                    # optional; without it there is one site, default
#   fra1:
#     description: Frankfurt, rack 1
#     jump: ump@pxe1        # SSH to its machines goes through this host
#     network:
#       ipv4: {gateway: 192.0.2.1}
machines:
`

// pxeApplyWait is how long apply waits for the running servers of a
// site to serve what it rendered, polling every pxeApplyPoll; a server
// whose status is older than pxeLive does not run.
var (
	pxeApplyWait = 30 * time.Second
	pxeApplyPoll = 2 * time.Second
)

const pxeLive = 3 * time.Minute

// pxeRescuePoll is how often a wait for a network boot's rescue system
// reads the status documents, and pxeReadyWait how long arming it
// waits for a PXE server to take the arm (provider/pxe.Home).
var (
	pxeRescuePoll = 3 * time.Second
	pxeReadyWait  = 30 * time.Second
)

func (a *app) newConfigPXECmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pxe",
		Aliases: []string{"px"},
		Short:   "The machines ump boots over the network (pxe.yaml), and their sites",
		Long: `The machines ump boots over the network live in pxe.yaml in the home
(` + config.AbbreviateHome(pxePath()) + `), one named entry each: its MACs,
its site, how it boots when nothing is armed (mode: local, its disk, or
rescue, UMP's rescue system at every boot), its installed system's
network, and reinstall's values for it (profile, layout, group, ...).
defaults come under every machine, and a site's values under its
machines'. A site is a private network whose DHCP server points the
machines at iPXE and iPXE at the site's PXE server (ump pxe serve); its
jump is the host SSH to its machines goes through.

Every change through these commands is checked and applied: site.json,
what the PXE servers of a site serve, is rendered into the backend
(.pxe/<site>/). A hand edit of pxe.yaml reaches no server until
ump config pxe apply.`,
		Example: `  ump config pxe create web-07 --mac 0c:c4:7a:aa:bb:01 --site fra1 --set network.ipv4.address=192.0.2.17/24
  ump config pxe list                 # the machines, their status and when a server saw them
  ump config pxe get web-07 --merged  # with the defaults' and its site's values
  ump config pxe edit web-07          # that machine alone, in your editor
  ump config pxe edit                 # the whole file
  ump config pxe apply                # render site.json after a hand edit
  ump config pxe delete web-07`,
	}
	cmd.AddCommand(a.newPXEListCmd(), a.newPXEGetCmd(), a.newPXECreateCmd(), a.newPXEEditCmd(), a.newPXEDeleteCmd(), a.newPXEApplyCmd())
	return cmd
}

// loadPXE reads pxe.yaml, checked: no file is no machines, one that is
// not valid a usage error (exit status 2).
func loadPXE() (*pxe.Config, error) {
	data, err := os.ReadFile(pxePath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	c, err := pxe.Parse(data)
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		return nil, withExit(exitUsage, fmt.Errorf("%s: %w", config.AbbreviateHome(pxePath()), err))
	}
	return c, nil
}

// pxeStore is where the documents of the sites are: the home's backend,
// or the local home.
func (a *app) pxeStore() pxe.Store {
	if a.home != nil && a.home.Backend != nil {
		return pxe.Store{Files: a.home.Backend}
	}
	return pxe.Store{Files: &backend.Local{Dir: config.UserConfigDir()}}
}

// pxeHome is what the pxe provider type lists from: pxe.yaml as
// loadState read it, the documents of its sites, and the machines ump
// installed (the state as read last).
func (a *app) pxeHome() *pxeprovider.Home {
	h := &pxeprovider.Home{Config: a.pxeConf, Err: a.pxeErr, Store: a.pxeStore(), By: armedBy(), Now: a.now,
		Poll: pxeRescuePoll, ReadyWait: pxeReadyWait, Installed: map[string]bool{}}
	if a.doc != nil {
		for ref, s := range a.doc.Servers {
			if name, ok := strings.CutPrefix(ref, pxe.TypeName+"/"); ok && s.Installed != nil {
				h.Installed[name] = true
			}
		}
	}
	return h
}

// armedBy names who arms a network boot: user@host.
func armedBy() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	if user == "" {
		user = "ump"
	}
	host, _ := os.Hostname()
	return user + "@" + host
}

// pxeSet is pxe.yaml as a file of named entries, for edit: every save
// is checked (pxeCheck).
func (a *app) pxeSet(ctx context.Context) *namedSet {
	return &namedSet{
		file: pxe.FileName, key: "machines", noun: "machine", use: "pxe",
		dir:   config.UserConfigDir,
		local: true,
		check: func(data []byte) error { return a.pxeCheck(ctx, data) },
	}
}

// pxeCheck checks pxe.yaml as it would be saved: valid, and every
// machine's rescue keys resolve (exit status 2 when not), and no
// machine with a pending arm moved but those deleted with theirs
// (pxeArmCheck).
func (a *app) pxeCheck(ctx context.Context, data []byte, deleted ...string) error {
	c, err := pxe.Parse(data)
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		return withExit(exitUsage, err)
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	keys := a.pxeKeys(cfg)
	for _, m := range c.Merged() {
		if _, err := keys(m); err != nil {
			return withExit(exitUsage, fmt.Errorf("machines.%s: rescue-keys: %w", m.Name, err))
		}
	}
	return a.pxeArmCheck(ctx, c, deleted...)
}

// pxeArmCheck refuses pxe.yaml (exit status 3) while it takes a machine
// with a pending arm away from what its site serves: to another site,
// to other MACs, or out of the file, but for the machines deleted with
// their arms. It compares with the site.json in the backend, which the
// servers serve, not with pxe.yaml as it was, so that neither a hand
// edit nor apply gets past it.
func (a *app) pxeArmCheck(ctx context.Context, c *pxe.Config, deleted ...string) error {
	store := a.pxeStore()
	pending, err := store.Pending(ctx, a.now())
	if err != nil {
		return err
	}
	for _, site := range sortedKeys(pending) {
		// what the site serves; a site.json that does not parse (changed
		// by hand) gives no MACs to keep, and apply renders it again
		data, version, err := store.Files.Read(ctx, pxe.SitePath(site))
		if err != nil {
			return err
		}
		var served *pxe.SiteDoc
		if version != "" {
			served, _ = pxe.ParseSite(data)
		}
		for _, name := range sortedKeys(pending[site]) {
			if slices.Contains(deleted, name) {
				continue
			}
			arm := pending[site][name]
			m := c.Machine(name)
			if m != nil && m.Site == site {
				var sm *pxe.SiteMachine
				if served != nil {
					sm = served.Machine(name)
				}
				if sm == nil || slices.Equal(m.MACs, sm.MACs) {
					continue
				}
			}
			what := "its site or MACs change"
			if m == nil {
				what = "it goes"
			}
			return withExit(exitAborted, fmt.Errorf("%s has an armed %s at site %s (%s, by %s); disarm it before %s (ump pxe disarm %s)", name, arm.Mode, site, pxe.ShortID(arm.ID), arm.By, what, name))
		}
	}
	return nil
}

// pxeDoc reads pxe.yaml as a document of named entries, a new one when
// there is none; a file without machines gets the mapping.
func (a *app) pxeDoc(set *namedSet) (*namedDoc, error) {
	data, err := os.ReadFile(pxePath())
	if errors.Is(err, fs.ErrNotExist) || err == nil && strings.TrimSpace(string(data)) == "" {
		data, err = []byte(pxeSkeleton), nil
	}
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", config.AbbreviateHome(pxePath()), err)
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 && root.Content[0].Kind == yaml.MappingNode {
		top := root.Content[0]
		found := false
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "machines" {
				found = true
				if v := top.Content[i+1]; v.Kind != yaml.MappingNode && v.ShortTag() == "!!null" {
					top.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				}
			}
		}
		if !found {
			top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "machines"}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		}
		out, err := yaml.Marshal(&root)
		if err != nil {
			return nil, err
		}
		data = out
	}
	d, err := set.parse(data)
	if err != nil {
		return nil, err
	}
	d.table.Style = 0 // entries in block style, also in a file with machines: {}
	return d, nil
}

// pxeSave checks the document as it would be saved, saves it and
// applies it.
func (a *app) pxeSave(cmd *cobra.Command, set *namedSet, d *namedDoc, said string) error {
	data, err := d.bytes()
	if err != nil {
		return err
	}
	if err := set.check(data); err != nil {
		return fmt.Errorf("nothing was saved: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(pxePath()), 0o700); err != nil {
		return err
	}
	if err := d.save(); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, said)
	return a.pxeApplyNow(cmd)
}

// pxeApplyNow applies pxe.yaml as it is now.
func (a *app) pxeApplyNow(cmd *cobra.Command) error {
	c, err := loadPXE()
	if err != nil {
		return err
	}
	return a.pxeApply(cmd, c)
}

func (a *app) newPXEApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Check pxe.yaml and render what the PXE servers of each site serve",
		Long: `Check pxe.yaml and render site.json for each of its sites into the
backend (.pxe/<site>/site.json), which the site's PXE servers serve: the
machines, their standing mode and root's keys in their rescue system
(UMP's, staging.keys and rescue-keys). The site.json of a site no longer
in pxe.yaml is removed, so that its servers serve nothing. Then it waits
up to 30 seconds for the running servers of a changed site to serve the
new one, and names those that do not (exit status 5). Every other ump
config pxe command ends with this; after a hand edit of pxe.yaml it is
how the change reaches the servers.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.pxeApplyNow(cmd)
		},
	}
}

// pxeApply renders site.json for every site of pxe.yaml, removes that
// of the sites gone, and waits for the running servers of the changed
// sites; a machine with a pending arm keeps what its site serves
// (pxeArmCheck).
func (a *app) pxeApply(cmd *cobra.Command, c *pxe.Config) error {
	ctx := cmd.Context()
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	if err := a.pxeArmCheck(ctx, c); err != nil {
		return err
	}
	a.warnPXEGone(ctx, cfg, c)
	store := a.pxeStore()
	keys := a.pxeKeys(cfg)
	var changed []*pxe.SiteDoc
	sites := c.SiteNames()
	for _, site := range sites {
		d, err := pxe.Render(c, site, cfg.StagingImage(), keys)
		if err != nil {
			return err
		}
		ok, err := store.PutSite(ctx, d)
		if err != nil {
			return fmt.Errorf("site %s: %w", site, err)
		}
		if ok {
			changed = append(changed, d)
			fmt.Fprintf(a.stdout, "Applied site %s: %s (site.json %s).\n", site, plural(len(d.Machines), "machine"), d.Hash)
		} else {
			fmt.Fprintf(a.stdout, "Site %s is up to date (site.json %s).\n", site, d.Hash)
		}
	}
	rendered, err := store.Sites(ctx)
	if err != nil {
		return err
	}
	for _, site := range rendered {
		if slices.Contains(sites, site) {
			continue
		}
		if err := store.DeleteSite(ctx, site); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed site %s, gone from pxe.yaml: its servers serve nothing now.\n", site)
	}
	return a.pxeWait(ctx, store, changed)
}

// warnPXEGone says which machines the state holds that pxe.yaml lacks,
// after a hand edit: they stay in the fleet until ump config pxe
// delete.
func (a *app) warnPXEGone(ctx context.Context, cfg *config.Config, c *pxe.Config) {
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return
	}
	for _, ref := range sortedKeys(doc.Servers) {
		if name, ok := strings.CutPrefix(ref, pxe.TypeName+"/"); ok && c.Machine(name) == nil {
			fmt.Fprintf(a.stderr, "ump: %s is in the state, but pxe.yaml has no machine %s: ump config pxe delete %s forgets it\n", ref, name, name)
		}
	}
}

// pxeLeaves says what a machine takes with it when it leaves the fleet:
// its installation record and its private addresses, as the state
// holds them; "" when nothing.
func pxeLeaves(subject string, s state.Server) string {
	switch {
	case s.Installed != nil && len(s.Private) > 0:
		return fmt.Sprintf(" %s leaves the fleet with its installation record and its %s.", subject, privateAddresses(s.Private))
	case s.Installed != nil:
		return fmt.Sprintf(" %s leaves the fleet with its installation record.", subject)
	case len(s.Private) > 0:
		return fmt.Sprintf(" %s gives back its %s.", subject, privateAddresses(s.Private))
	}
	return ""
}

// pxeForget forgets machines that left pxe.yaml: pxe/NAME in the state,
// and what any site holds of them (arm, outcome, log).
func (a *app) pxeForget(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	var forgot []string
	err := a.updateState(ctx, func(d *state.Document) error {
		forgot = nil
		for _, name := range names {
			if _, ok := d.Servers[pxe.Ref(name)]; ok {
				delete(d.Servers, pxe.Ref(name))
				forgot = append(forgot, pxe.Ref(name))
			}
		}
		if len(forgot) == 0 {
			return errNothingGone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothingGone) {
		return err
	}
	for _, ref := range forgot {
		fmt.Fprintf(a.stdout, "Forgot %s in the state.\n", ref)
	}
	store := a.pxeStore()
	for _, name := range names {
		sites, err := store.Holding(ctx, name)
		if err != nil {
			return err
		}
		for _, site := range sites {
			if err := store.Forget(ctx, site, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// pxeKeys resolves a machine's keys in its rescue system: UMP's own,
// staging.keys and its rescue-keys, as key lines.
func (a *app) pxeKeys(cfg *config.Config) func(m *pxe.Machine) ([]string, error) {
	return func(m *pxe.Machine) ([]string, error) {
		pks, _, _, err := a.rescueKeys(cfg, m.RescueKeys)
		if err != nil {
			return nil, err
		}
		lines := make([]string, len(pks))
		for i, k := range pks {
			lines[i] = k.Data
		}
		return lines, nil
	}
}

// pxeWait waits until the running servers of the sites serve their new
// site.json, and says which do and which sites have none running.
func (a *app) pxeWait(ctx context.Context, store pxe.Store, docs []*pxe.SiteDoc) error {
	if len(docs) == 0 {
		return nil
	}
	var names []string
	for _, d := range docs {
		names = append(names, d.Site)
	}
	start := time.Now()
	for {
		sn, err := store.Load(ctx, names...)
		if err != nil {
			return err
		}
		var lagging, said []string
		for _, d := range docs {
			var serving []string
			for _, st := range sn.Statuses[d.Site] {
				if a.now().Sub(st.Updated) > pxeLive {
					continue
				}
				if st.SiteHash != d.Hash {
					lagging = append(lagging, d.Site+"/"+st.Daemon)
					continue
				}
				serving = append(serving, st.Daemon)
			}
			switch {
			case len(serving) > 0:
				said = append(said, fmt.Sprintf("Site %s: served by %s.", d.Site, strings.Join(serving, ", ")))
			case !slices.ContainsFunc(lagging, func(l string) bool { return strings.HasPrefix(l, d.Site+"/") }):
				said = append(said, fmt.Sprintf("Site %s: no PXE server of it runs (ump pxe serve --site %s).", d.Site, d.Site))
			}
		}
		if len(lagging) == 0 || time.Since(start) >= pxeApplyWait {
			for _, s := range said {
				fmt.Fprintln(a.stdout, s)
			}
			if len(lagging) > 0 {
				return withExit(exitUnreachable, fmt.Errorf("these PXE servers do not serve the new site.json after %s: %s; they read it at their next poll, or see their journal", pxeApplyWait, strings.Join(lagging, ", ")))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pxeApplyPoll):
		}
	}
}

// pxeRow is a machine as ump config pxe list shows it.
type pxeRow struct {
	Name string   `json:"name" yaml:"name"`
	Site string   `json:"site" yaml:"site"`
	MACs []string `json:"macs" yaml:"macs"`
	// Mode is the standing mode, Status what it is now: its arm, its
	// outcome, or the mode.
	Mode    string     `json:"mode" yaml:"mode"`
	Status  string     `json:"status" yaml:"status"`
	Profile string     `json:"profile,omitempty" yaml:"profile,omitempty"`
	Address string     `json:"address,omitempty" yaml:"address,omitempty"`
	Jump    string     `json:"jump,omitempty" yaml:"jump,omitempty"`
	Note    string     `json:"note,omitempty" yaml:"note,omitempty"`
	Seen    *time.Time `json:"seen,omitempty" yaml:"seen,omitempty"`
	SeenBy  string     `json:"seen_by,omitempty" yaml:"seen_by,omitempty"`
}

func (a *app) newPXEListCmd() *cobra.Command {
	var output string
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the machines: site, MAC, status, profile, address and when a server saw them",
		Long: `List the machines of pxe.yaml, one per row: the site, the MAC (the
first; -o wide all of them), the status (the standing mode, or an arm and
what came of it: armed rescue, installing, installed (unverified), ...),
the profile, the address ump reaches the installed system at (a static
one, or address), and when a PXE server of the site saw it last. It says
when pxe.yaml changed since it was applied. -o json and -o yaml carry
everything.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil || format == formatName {
				return usageErrorf(cmd, "unknown output format %q (use one of table, wide, json, yaml)", output)
			}
			c, err := loadPXE()
			if err != nil {
				return err
			}
			rows, _, err := a.pxeRows(cmd, c)
			if err != nil {
				return err
			}
			switch format {
			case formatJSON, formatYAML:
				return printEncoded(a.stdout, rows, format)
			}
			if len(rows) == 0 {
				fmt.Fprintf(a.stderr, "No machines in %s; ump config pxe create NAME --mac MAC adds one.\n", config.AbbreviateHome(pxePath()))
				return nil
			}
			headers := []string{"NAME", "SITE", "MAC", "STATUS", "PROFILE", "ADDRESS", "SEEN"}
			if format == formatWide {
				headers = append(headers, "JUMP", "NOTE")
			}
			var table [][]string
			for _, r := range rows {
				mac := r.MACs[0]
				if format == formatWide {
					mac = strings.Join(r.MACs, ",")
				} else if len(r.MACs) > 1 {
					mac += fmt.Sprintf(" +%d", len(r.MACs)-1)
				}
				seen := "-"
				if r.Seen != nil {
					seen = a.pxeAgo(*r.Seen)
				}
				row := []string{r.Name, r.Site, mac, r.Status, orDash(r.Profile), orDash(r.Address), seen}
				if format == formatWide {
					row = append(row, orDash(r.Jump), r.Note)
				}
				table = append(table, row)
			}
			return writeTable(a.stdout, headers, table, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide (every MAC, the jump host and the note), json or yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

// pxeRows are the machines with what the backend and the state know of
// them, and the documents of their sites; a site whose site.json is not
// what pxe.yaml renders is said.
func (a *app) pxeRows(cmd *cobra.Command, c *pxe.Config) ([]pxeRow, *pxe.Snapshot, error) {
	ctx := cmd.Context()
	cfg, _, err := a.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	sn, err := a.pxeStore().Load(ctx, c.SiteNames()...)
	if err != nil {
		return nil, nil, err
	}
	keys := a.pxeKeys(cfg)
	var stale []string
	for _, site := range c.SiteNames() {
		d, err := pxe.Render(c, site, cfg.StagingImage(), keys)
		if err != nil {
			fmt.Fprintf(a.stderr, "ump: site %s cannot be applied: %v\n", site, err)
			continue
		}
		if cur := sn.Sites[site]; cur == nil || cur.Hash != d.Hash {
			stale = append(stale, site)
		}
	}
	if len(stale) > 0 && len(c.Names()) > 0 {
		fmt.Fprintf(a.stderr, "pxe.yaml changed since it was applied (site %s): ump config pxe apply\n", strings.Join(stale, ", "))
	}
	rows := []pxeRow{}
	for _, m := range c.Merged() {
		r := pxeRow{Name: m.Name, Site: m.Site, MACs: m.MACs, Mode: m.Mode, Jump: m.Jump, Note: m.Note, Address: pxeAddress(m)}
		if m.Plan.Profile != nil {
			r.Profile = *m.Plan.Profile
		}
		installed := doc.Servers[pxe.Ref(m.Name)].Installed != nil
		r.Status = pxe.StatusWords(m.Mode, sn.Arm(m.Site, m.Name), sn.Outcome(m.Site, m.Name), installed, a.now())
		for _, st := range sn.Statuses[m.Site] {
			for _, mac := range m.MACs {
				if s := st.Machines[mac]; s != nil && (r.Seen == nil || s.At.After(*r.Seen)) {
					at := s.At
					r.Seen, r.SeenBy = &at, st.Daemon
				}
			}
		}
		rows = append(rows, r)
	}
	return rows, sn, nil
}

// pxeAddress is where ump reaches a machine's installed system: its
// static IPv4 address, else address, else its static IPv6 address.
func pxeAddress(m *pxe.Machine) string {
	if m.Network.IPv4.Mode == profile.UplinkStatic {
		return strings.Split(m.Network.IPv4.Address, "/")[0]
	}
	if m.Address != "" {
		return m.Address
	}
	if m.Network.IPv6.Mode == profile.UplinkStatic {
		return strings.Split(m.Network.IPv6.Address, "/")[0]
	}
	return ""
}

func (a *app) newPXEGetCmd() *cobra.Command {
	var output string
	var merged bool
	cmd := &cobra.Command{
		Use:     "get NAME [--merged]",
		Aliases: verbAliases("get"),
		Short:   "Print a machine as pxe.yaml has it, or with the defaults' and its site's values",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format := formatYAML
			if output != "" {
				f, err := parseFormat(output)
				if err != nil || f != formatJSON && f != formatYAML {
					return usageErrorf(cmd, "unknown output format %q (use one of json, yaml)", output)
				}
				format = f
			}
			c, err := loadPXE()
			if err != nil {
				return err
			}
			name := args[0]
			m := c.Machine(name)
			if m == nil {
				return a.pxeSet(cmd.Context()).missing(name)
			}
			var node *yaml.Node
			if merged {
				node = mergedNode(m)
			} else {
				d, err := a.pxeDoc(a.pxeSet(cmd.Context()))
				if err != nil {
					return err
				}
				node = d.node(name)
			}
			if format == formatJSON {
				var v any
				if err := node.Decode(&v); err != nil {
					return err
				}
				return printEncoded(a.stdout, map[string]any{name: v}, formatJSON)
			}
			data, err := config.MarshalYAML(map[string]*yaml.Node{name: node})
			if err != nil {
				return err
			}
			_, err = a.stdout.Write(data)
			return err
		},
	}
	cmd.Flags().BoolVar(&merged, "merged", false, "with the values of defaults and of its site, as the machine gets them")
	cmd.Flags().StringVarP(&output, "output", "o", "", "print as json or yaml (the default)")
	return cmd
}

// mergedNode is a merged machine as an entry of pxe.yaml would give it.
func mergedNode(m *pxe.Machine) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) {
		var val yaml.Node
		if err := val.Encode(v); err != nil {
			return
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &val)
	}
	opt := func(key, v string) {
		if v != "" {
			add(key, v)
		}
	}
	add("site", m.Site)
	add("mac", m.MACs)
	add("mode", m.Mode)
	add("hostname", m.Hostname)
	opt("arch", m.Arch)
	opt("firmware", m.Firmware)
	opt("console", m.Console)
	if len(m.RescueKeys) > 0 {
		add("rescue-keys", m.RescueKeys)
	}
	opt("jump", m.Jump)
	opt("address", m.Address)
	opt("note", m.Note)
	family := func(f profile.UplinkAddress) any {
		if f.Mode != profile.UplinkStatic {
			return f.Mode
		}
		v := map[string]string{"address": f.Address}
		if f.Gateway != "" {
			v["gateway"] = f.Gateway
		}
		return v
	}
	network := map[string]any{"uplink": m.Network.MAC, "ipv4": family(m.Network.IPv4), "ipv6": family(m.Network.IPv6)}
	switch {
	case m.Network.DNSFromDHCP:
		network["dns"] = "dhcp"
	case len(m.Network.DNS) > 0:
		network["dns"] = m.Network.DNS
	}
	add("network", network)
	// reinstall's values, those given
	var plan yaml.Node
	if err := plan.Encode(m.Plan); err == nil {
		for i := 0; i+1 < len(plan.Content); i += 2 {
			if v := plan.Content[i+1]; v.ShortTag() != "!!null" {
				n.Content = append(n.Content, plan.Content[i], v)
			}
		}
	}
	return n
}

func (a *app) newPXECreateCmd() *cobra.Command {
	var macs, sets []string
	var site, mode string
	cmd := &cobra.Command{
		Use:     "create NAME --mac MAC [--site SITE] [--mode local|rescue] [--set KEY=VALUE]...",
		Aliases: verbAliases("create"),
		Short:   "Add a machine to pxe.yaml (in your editor on a terminal without --mac)",
		Long: `Add a machine to pxe.yaml: NAME is the server's name (pxe/NAME) and,
unless hostname says otherwise, its hostname; --mac the MACs it boots
from (repeatable, the first is its uplink), --site its site (required
when pxe.yaml has sites), --mode local or rescue. --set gives any other
key as KEY=VALUE, the value in YAML and the key with dots for the
network: --set network.ipv4.address=192.0.2.17/24 --set profile=web
--set group='[web, db]'. On a terminal without --mac, the new entry
opens in your editor. The file is checked and applied.`,
		Example: `  ump config pxe create web-07 --mac 0c:c4:7a:aa:bb:01 --site fra1 --set network.ipv4.address=192.0.2.17/24
  ump config pxe create db-01 --mac 0c:c4:7a:aa:bb:10 --mac 0c:c4:7a:aa:bb:11 --set address=192.0.2.30
  ump config pxe create lab-1          # in your editor`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validateServerName(name); err != nil {
				return usageErrorf(cmd, "%v", err)
			}
			before, err := loadPXE()
			if err != nil {
				return err
			}
			if before.Machines[name] != nil {
				return fmt.Errorf("machine %s exists already (ump config pxe edit %s)", name, name)
			}
			set := a.pxeSet(cmd.Context())
			d, err := a.pxeDoc(set)
			if err != nil {
				return err
			}
			entry := &yaml.Node{Kind: yaml.MappingNode}
			if len(macs) == 0 {
				if a.isTTY == nil || !a.isTTY() {
					return usageErrorf(cmd, "--mac is needed without a terminal: ump config pxe create %s --mac MAC", name)
				}
				if len(sets) > 0 || site != "" || mode != "" {
					return usageErrorf(cmd, "--site, --mode and --set need --mac; without any of them the new machine opens in your editor")
				}
				// a new entry in the editor: saved once it is valid, not
				// at all when it comes back unchanged
				stub := map[string]string{"mac": ""}
				if names := before.SiteNames(); len(before.Sites) > 0 {
					stub["site"] = names[0]
				}
				entry.Encode(stub)
				saved, err := a.pxeEditEntry(cmd, set, name, entry, true)
				if err != nil || !saved {
					return err
				}
				return a.pxeApplyNow(cmd)
			}
			kv := func(k string, v any) error {
				var n yaml.Node
				if err := n.Encode(v); err != nil {
					return err
				}
				return setNodePath(entry, []string{k}, &n)
			}
			if len(macs) == 1 {
				kv("mac", macs[0])
			} else {
				kv("mac", macs)
			}
			if site != "" {
				kv("site", site)
			}
			if mode != "" {
				kv("mode", mode)
			}
			for _, s := range sets {
				k, v, ok := strings.Cut(s, "=")
				k = strings.TrimSpace(k)
				if !ok || k == "" {
					return usageErrorf(cmd, "--set needs KEY=VALUE, got %q", s)
				}
				var val yaml.Node
				if err := yaml.Unmarshal([]byte(v), &val); err != nil {
					return usageErrorf(cmd, "--set %s: %v", k, err)
				}
				value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
				if len(val.Content) > 0 {
					value = val.Content[0]
				}
				if err := setNodePath(entry, strings.Split(k, "."), value); err != nil {
					return usageErrorf(cmd, "--set %s: %v", k, err)
				}
			}
			d.add(name, entry)
			return a.pxeSave(cmd, set, d, fmt.Sprintf("Created the machine %s in %s.", name, config.AbbreviateHome(pxePath())))
		},
	}
	cmd.Flags().StringArrayVar(&macs, "mac", nil, "a MAC the machine boots from (repeatable; the first is its uplink)")
	cmd.Flags().StringVar(&site, "site", "", "its site (required when pxe.yaml has sites)")
	cmd.Flags().StringVar(&mode, "mode", "", "local (its disk, the default) or rescue (UMP's rescue system at every boot)")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "another key, KEY=VALUE in YAML (network.ipv4.address=192.0.2.17/24, profile=web; repeatable)")
	return cmd
}

// setNodePath sets the key path (network, ipv4, address) of a mapping
// to value, making the mappings on the way.
func setNodePath(m *yaml.Node, path []string, value *yaml.Node) error {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != path[0] {
			continue
		}
		if len(path) == 1 {
			m.Content[i+1] = value
			return nil
		}
		next := m.Content[i+1]
		if next.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", path[0])
		}
		return setNodePath(next, path[1:], value)
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: path[0]}
	if len(path) == 1 {
		m.Content = append(m.Content, key, value)
		return nil
	}
	next := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, key, next)
	return setNodePath(next, path[1:], value)
}

func (a *app) newPXEEditCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "edit [NAME] [--yes]",
		Aliases: verbAliases("edit"),
		Short:   "Edit one machine in your editor, or all of pxe.yaml without a name",
		Long: `With a name, open that machine alone in your editor; when the editor
returns, pxe.yaml is checked with it and saved, the rest of the file
untouched. Without a name, open the whole file (a new one, commented,
when there is none) and save it once it is valid; a machine taken out
of it that ump installed, or that holds private addresses, leaves the
fleet as with delete, which is asked first (--yes without a terminal).
Either way the change is applied: site.json of every site is rendered
again. A machine whose boot is armed keeps its site and MACs until it
is disarmed.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := a.pxeSet(cmd.Context())
			if len(args) == 0 {
				saved, err := a.pxeEditFile(cmd, set, yes)
				if err != nil || !saved {
					return err
				}
				return a.pxeApplyNow(cmd)
			}
			d, err := a.pxeDoc(set)
			if err != nil {
				return err
			}
			node := d.node(args[0])
			if node == nil {
				return set.missing(args[0])
			}
			if _, err := a.pxeEditEntry(cmd, set, args[0], node, false); err != nil {
				return err
			}
			return a.pxeApplyNow(cmd)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "let machines taken out of the file leave the fleet without asking")
	return cmd
}

// pxeEditFile opens a copy of pxe.yaml in the editor (a new one when
// there is none) and saves it once it is valid; one that is not is
// offered again on a terminal and otherwise kept in its file, with
// nothing saved. A machine it takes out that the state holds leaves the
// fleet as delete has it leave, once confirmed. It says whether it
// saved.
func (a *app) pxeEditFile(cmd *cobra.Command, set *namedSet, yes bool) (bool, error) {
	ctx := cmd.Context()
	orig, err := os.ReadFile(pxePath())
	if errors.Is(err, fs.ErrNotExist) {
		orig, err = []byte(pxeSkeleton), nil
	}
	if err != nil {
		return false, err
	}
	// what leaves: the machines of the file as it is, a valid one
	var before []string
	if c, err := pxe.Parse(orig); err == nil {
		before = c.Names()
	}
	f, err := os.CreateTemp("", "ump-pxe-*.yaml")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	_, err = f.Write(orig)
	f.Close()
	if err != nil {
		return false, err
	}
	for {
		if err := a.editor(tmp); err != nil {
			return false, err
		}
		data, err := os.ReadFile(tmp)
		if err != nil {
			return false, err
		}
		gone, err := a.pxeEditCheck(ctx, set, before, data)
		leaves := ""
		for _, name := range sortedKeys(gone) {
			leaves += pxeLeaves(name, gone[name])
		}
		if err == nil && leaves != "" {
			if err := a.confirm(cmd, yes, "Save "+config.AbbreviateHome(pxePath())+"?"+leaves); err != nil {
				return false, withExit(exitCodeFor(err), fmt.Errorf("%w; nothing was saved, your edit is in %s", err, tmp))
			}
		}
		if err == nil {
			if err := os.MkdirAll(filepath.Dir(pxePath()), 0o700); err != nil {
				return false, err
			}
			if err := os.WriteFile(pxePath(), data, 0o600); err != nil {
				return false, err
			}
			os.Remove(tmp)
			c, _ := pxe.Parse(data)
			fmt.Fprintf(a.stdout, "%s, all valid.\n", plural(len(c.Names()), "machine"))
			return true, a.pxeForget(ctx, sortedKeys(gone)...)
		}
		fmt.Fprintf(a.stderr, "ump: %s is not valid: %v\n", config.AbbreviateHome(pxePath()), err)
		code := exitCodeFor(err)
		if code == exitGeneric {
			return false, fmt.Errorf("nothing was saved; your edit is in %s: %w", tmp, err)
		}
		ui := a.resolverUI(cmd)
		if ui == nil {
			return false, withExit(code, fmt.Errorf("nothing was saved; your edit is in %s", tmp))
		}
		if again, cerr := ui.Confirm("Open it again?", true); cerr != nil || !again {
			return false, withExit(code, fmt.Errorf("nothing was saved; your edit is in %s", tmp))
		}
	}
}

// pxeEditCheck checks pxe.yaml as an edit of the whole file would save
// it, and returns the machines of before that it takes out and that
// the state holds, with what the state holds of them.
func (a *app) pxeEditCheck(ctx context.Context, set *namedSet, before []string, data []byte) (map[string]state.Server, error) {
	if err := set.check(data); err != nil {
		return nil, err
	}
	c, err := pxe.Parse(data)
	if err != nil {
		return nil, err
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	doc, err := a.loadState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	gone := map[string]state.Server{}
	for _, name := range before {
		if s, ok := doc.Servers[pxe.Ref(name)]; ok && c.Machine(name) == nil {
			gone[name] = s
		}
	}
	return gone, nil
}

// pxeEditEntry opens a machine's entry (node; a new one with isNew) in
// the editor, as "NAME:" with its keys in a file of its own, and puts it
// into pxe.yaml when the file is valid with it; the rest of the file
// stays as it is. An entry that comes back unchanged changes nothing;
// one that is not valid is offered again on a terminal and otherwise
// kept in its file, with nothing saved. It says whether it saved.
func (a *app) pxeEditEntry(cmd *cobra.Command, set *namedSet, name string, node *yaml.Node, isNew bool) (bool, error) {
	f, err := os.CreateTemp("", "ump-machine-"+name+"-*.yaml")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	what := "The machine " + name + " of " + config.AbbreviateHome(pxePath())
	if isNew {
		what = "The new machine " + name + " for " + config.AbbreviateHome(pxePath())
	}
	body, err := config.MarshalYAML(map[string]*yaml.Node{name: node})
	if err != nil {
		f.Close()
		return false, err
	}
	orig := "# " + what + ". Save and close to apply it;\n# the rest of the file stays as it is. The name cannot change here.\n" + string(body)
	_, err = f.WriteString(orig)
	f.Close()
	if err != nil {
		return false, err
	}
	for {
		if err := a.editor(tmp); err != nil {
			return false, err
		}
		data, err := os.ReadFile(tmp)
		if err != nil {
			return false, err
		}
		if string(data) == orig {
			os.Remove(tmp)
			if isNew {
				fmt.Fprintf(a.stdout, "No machine %s created.\n", name)
			} else {
				fmt.Fprintf(a.stdout, "No changes to the machine %s.\n", name)
			}
			return false, nil
		}
		entry, err := set.entryNode(data, name)
		var d *namedDoc
		if err == nil {
			d, err = a.pxeDoc(set)
		} else {
			err = withExit(exitUsage, err)
		}
		if err == nil {
			switch {
			case isNew && d.node(name) != nil:
				err = withExit(exitUsage, fmt.Errorf("machine %s exists already", name))
			case isNew:
				d.add(name, entry)
			default:
				d.replace(name, entry)
			}
		}
		var whole []byte
		if err == nil {
			if whole, err = d.bytes(); err == nil {
				err = set.check(whole)
			}
		}
		if err == nil {
			if err := os.MkdirAll(filepath.Dir(pxePath()), 0o700); err != nil {
				return false, err
			}
			if err := d.save(); err != nil {
				return false, err
			}
			os.Remove(tmp)
			fmt.Fprintf(a.stdout, "Saved the machine %s in %s.\n", name, config.AbbreviateHome(pxePath()))
			return true, nil
		}
		fmt.Fprintf(a.stderr, "ump: the machine %s is not valid: %v\n", name, err)
		code := exitCodeFor(err)
		if code == exitGeneric {
			return false, fmt.Errorf("nothing was saved; your edit is in %s: %w", tmp, err)
		}
		ui := a.resolverUI(cmd)
		if ui == nil {
			return false, withExit(code, fmt.Errorf("nothing was saved; your edit is in %s", tmp))
		}
		if again, cerr := ui.Confirm("Open it again?", true); cerr != nil || !again {
			return false, withExit(code, fmt.Errorf("nothing was saved; your edit is in %s", tmp))
		}
		orig = ""
	}
}

func (a *app) newPXEDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME [--yes]",
		Aliases: verbAliases("delete"),
		Short:   "Remove a machine: from pxe.yaml, the state and its site",
		Long: `Remove a machine from pxe.yaml, forget pxe/NAME in the state (its
installation record and its private addresses, which are free again)
and remove what its site holds of it (its arm, outcome and log); then
apply. A machine a hand edit took out of pxe.yaml is forgotten the same
way. The machine itself is not touched: with no entry, the PXE servers
no longer boot it. Asks first; without a terminal, --yes is required. A
machine whose installation is being written is refused.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := strings.TrimPrefix(args[0], pxe.TypeName+"/")
			c, err := loadPXE()
			if err != nil {
				return err
			}
			set := a.pxeSet(ctx)
			cfg, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			doc, err := a.loadState(ctx, cfg)
			if err != nil {
				return err
			}
			store := a.pxeStore()
			// its site, and every site that holds something of it
			sites, err := store.Holding(ctx, name)
			if err != nil {
				return err
			}
			m := c.Machine(name)
			if m != nil && !slices.Contains(sites, m.Site) {
				sites = append(sites, m.Site)
			}
			ref := pxe.Ref(name)
			s, inState := doc.Servers[ref]
			if m == nil && !inState && len(sites) == 0 {
				return set.missing(name)
			}
			var armed []string
			for _, site := range sites {
				arm, _, err := store.Arm(ctx, site, name)
				if err != nil {
					return err
				}
				o, _, err := store.Outcome(ctx, site, name)
				if err != nil {
					return err
				}
				if o != nil && o.Phase == pxe.PhaseWriting && a.now().Before(o.Deadline) {
					return withExit(exitAborted, fmt.Errorf("%s is being installed (since %s, by %s/%s); it ends with its report or at %s", name, a.fmtTime(o.At), site, o.Daemon, a.fmtTime(o.Deadline)))
				}
				if arm != nil && arm.Pending(o, a.now()) {
					armed = append(armed, fmt.Sprintf(" Its armed %s (%s) goes with it.", arm.Mode, pxe.ShortID(arm.ID)))
				}
			}
			q := fmt.Sprintf("Delete the machine %s from %s?", name, config.AbbreviateHome(pxePath()))
			if m == nil {
				q = fmt.Sprintf("Forget %s, which %s has no more?", ref, config.AbbreviateHome(pxePath()))
			}
			if inState {
				q += pxeLeaves("It", s)
			}
			q += strings.Join(armed, "")
			if err := a.confirm(cmd, yes, q); err != nil {
				return err
			}
			if m != nil {
				d, err := a.pxeDoc(set)
				if err != nil {
					return err
				}
				d.remove(name)
				data, err := d.bytes()
				if err != nil {
					return err
				}
				if err := a.pxeCheck(ctx, data, name); err != nil {
					return fmt.Errorf("nothing was deleted: %w", err)
				}
				if err := d.save(); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Deleted the machine %s from %s.\n", name, config.AbbreviateHome(pxePath()))
			}
			if err := a.pxeForget(ctx, name); err != nil {
				return err
			}
			return a.pxeApplyNow(cmd)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}
