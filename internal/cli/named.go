package cli

import (
	"context"

	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
)

// namedSet is a file of named entries next to the profiles (layouts.yaml,
// firewalls.yaml) that profiles refer to by name, with the command that
// lists, prints, creates, edits and deletes them.
type namedSet struct {
	// file is the file name in the profiles directory, key the mapping
	// in it, noun what one entry is called (layout, firewall).
	file, key, noun string
	// use, aliases, short, long and example are the command's.
	use     string
	aliases []string
	short   string
	long    string
	example string
	// defaultFrom is the entry a new one is copied from.
	defaultFrom string
	// headers and rows render the list: rows gets the file and the names
	// in order, and returns one row per name.
	headers []string
	rows    func(data []byte, names []string) ([][]string, error)
	// check parses the file and validates every entry.
	check func(data []byte) error
}

func (set *namedSet) path() string { return filepath.Join(config.ProfilesDir(), set.file) }

func (set *namedSet) listHint() string { return "ump config " + set.use + " list" }

func (set *namedSet) newCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     set.use,
		Aliases: set.aliases,
		Short:   set.short,
		Long:    set.long,
		Example: set.example,
	}
	cmd.AddCommand(set.newListCmd(a), set.newGetCmd(a), set.newCreateCmd(a), set.newEditCmd(a), set.newDeleteCmd(a))
	return cmd
}

// namedDoc is the file as a YAML document, comments included, with the
// mapping node of its entries.
type namedDoc struct {
	set   *namedSet
	root  yaml.Node
	table *yaml.Node
}

func (set *namedSet) load() (*namedDoc, error) {
	data, err := os.ReadFile(set.path())
	if err != nil {
		return nil, err
	}
	return set.parse(data)
}

// parse reads the file's content as a document with the entries.
func (set *namedSet) parse(data []byte) (*namedDoc, error) {
	d := &namedDoc{set: set}
	if err := yaml.Unmarshal(data, &d.root); err != nil {
		return nil, fmt.Errorf("%s: %w", set.path(), err)
	}
	if d.root.Kind != yaml.DocumentNode || len(d.root.Content) == 0 || d.root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a mapping with a %s key", set.path(), set.key)
	}
	top := d.root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == set.key && top.Content[i+1].Kind == yaml.MappingNode {
			d.table = top.Content[i+1]
		}
	}
	if d.table == nil {
		return nil, fmt.Errorf("%s: no %s mapping", set.path(), set.key)
	}
	return d, nil
}

func (d *namedDoc) names() []string {
	var names []string
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		names = append(names, d.table.Content[i].Value)
	}
	sort.Strings(names)
	return names
}

func (d *namedDoc) node(name string) *yaml.Node {
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		if d.table.Content[i].Value == name {
			return d.table.Content[i+1]
		}
	}
	return nil
}

func (d *namedDoc) add(name string, from *yaml.Node) {
	copied := *from
	d.table.Content = append(d.table.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, &copied)
}

func (d *namedDoc) remove(name string) {
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		if d.table.Content[i].Value == name {
			d.table.Content = append(d.table.Content[:i], d.table.Content[i+2:]...)
			return
		}
	}
}

func (d *namedDoc) save() error {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&d.root); err != nil {
		return err
	}
	enc.Close()
	return os.WriteFile(d.set.path(), []byte(b.String()), 0o600)
}

func (d *namedDoc) render(name string) string {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(map[string]*yaml.Node{name: d.node(name)})
	enc.Close()
	return b.String()
}

func (set *namedSet) prepare(a *app, cmd *cobra.Command) (*namedDoc, error) {
	if _, err := a.prepareProfiles(cmd); err != nil {
		return nil, err
	}
	return set.load()
}

func (set *namedSet) missing(name string) error {
	return fmt.Errorf("no %s %q in %s (%s)", set.noun, name, config.AbbreviateHome(set.path()), set.listHint())
}

func (set *namedSet) newListCmd(a *app) *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the " + set.noun + "s",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			data, _ := os.ReadFile(set.path())
			rows, err := set.rows(data, d.names())
			if err != nil {
				return err
			}
			return writeTable(a.stdout, set.headers, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (set *namedSet) newGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "get NAME",
		Aliases: verbAliases("get"),
		Short:   "Print a " + set.noun,
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			if d.node(args[0]) == nil {
				return set.missing(args[0])
			}
			_, err = fmt.Fprint(a.stdout, d.render(args[0]))
			return err
		},
	}
}

func (set *namedSet) newCreateCmd(a *app) *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:     "create [NAME] [--from NAME]",
		Aliases: verbAliases("create"),
		Short:   "Create a " + set.noun + " from another one and open the file in your editor (asks on a terminal)",
		Args:    usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			var name string
			if len(args) > 0 {
				name = args[0]
			} else {
				ui := a.resolverUI(cmd)
				if ui == nil {
					return usageErrorf(cmd, "a %s name is needed without a terminal: ump config %s create NAME --from NAME", set.noun, set.use)
				}
				names := d.names()
				ui.Printf("%ss in %s: %s\n", strings.ToUpper(set.noun[:1])+set.noun[1:], config.AbbreviateHome(set.path()), strings.Join(names, ", "))
				if name, err = ui.Line("Name of the new "+set.noun, ""); err != nil {
					return err
				}
				if !cmd.Flags().Changed("from") {
					def := 0
					for i, n := range names {
						if n == from {
							def = i
						}
					}
					i, err := ui.Choose("Start from", names, def)
					if err != nil {
						return err
					}
					from = names[i]
				}
			}
			if err := config.ValidateAccountName(name); err != nil {
				return usageErrorf(cmd, "%s name: %v", set.noun, err)
			}
			if d.node(name) != nil {
				return fmt.Errorf("%s %s exists already (ump config %s edit)", set.noun, name, set.use)
			}
			src := d.node(from)
			if src == nil {
				return fmt.Errorf("no %s %q to start from", set.noun, from)
			}
			d.add(name, src)
			if err := d.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Created the %s %s from %s in %s.\n", set.noun, name, from, config.AbbreviateHome(set.path()))
			if a.isTTY != nil && a.isTTY() {
				return set.edit(a, cmd.Context())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", set.defaultFrom, "the "+set.noun+" to copy")
	return cmd
}

func (set *namedSet) newEditCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "edit [NAME]",
		Aliases: verbAliases("edit"),
		Short:   "Open " + set.file + " in your editor and check every " + set.noun + " afterwards",
		Args:    usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			if len(args) == 1 && d.node(args[0]) == nil {
				return set.missing(args[0])
			}
			return set.edit(a, cmd.Context())
		},
	}
}

func (set *namedSet) newDeleteCmd(a *app) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: verbAliases("delete"),
		Short:   "Delete a " + set.noun,
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			name := args[0]
			if d.node(name) == nil {
				return set.missing(name)
			}
			if err := a.confirm(cmd, yes, fmt.Sprintf("Delete the %s %s from %s?", set.noun, name, config.AbbreviateHome(set.path()))); err != nil {
				return err
			}
			d.remove(name)
			if err := d.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Deleted the %s %s.\n", set.noun, name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// edit runs the editor on the file and checks every entry.
func (set *namedSet) edit(a *app, ctx context.Context) error {
	if err := a.editor(set.path()); err != nil {
		return err
	}
	data, err := os.ReadFile(set.path())
	if err != nil {
		return err
	}
	if err := set.check(data); err != nil {
		return fmt.Errorf("the file is kept, but it is not valid: %w", err)
	}
	d, err := set.load()
	if err != nil {
		return fmt.Errorf("the file is kept, but it is not valid: %w", err)
	}
	names := d.names()
	fmt.Fprintf(a.stdout, "%d %ss, all valid: %s.\n", len(names), set.noun, strings.Join(names, ", "))
	return nil
}

// merge replaces the file with the published one, keeping the entries
// of the local file that the published one does not have (the user's
// own); it returns their names. A local file that does not parse is
// replaced as it is.
func (set *namedSet) merge(published []byte) (kept []string, err error) {
	pub, err := set.parse(published)
	if err != nil {
		return nil, fmt.Errorf("the published %s: %w", set.file, err)
	}
	if local, err := set.load(); err == nil {
		for _, n := range local.names() {
			if pub.node(n) == nil {
				pub.add(n, local.node(n))
				kept = append(kept, n)
			}
		}
	}
	return kept, pub.save()
}

// loadNamed reads one of the files: the user's own in the profiles
// directory, else the published one on the storage.
func (a *app) loadNamed(ctx context.Context, storage, file string) ([]byte, string, error) {
	local := filepath.Join(config.ProfilesDir(), file)
	data, err := os.ReadFile(local)
	if err == nil {
		return data, local, nil
	}
	url := storage + "/profiles/" + file
	if data, err = a.fetch(ctx, url); err != nil {
		return nil, "", fmt.Errorf("%s: not in %s, and %w", file, config.AbbreviateHome(config.ProfilesDir()), err)
	}
	return data, url, nil
}

// sortedKeys are the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
