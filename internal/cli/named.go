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
	// in it, noun what one entry is called (layout, firewall) and plural
	// what several are (noun + "s" when empty).
	file, key, noun, plural string
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
	// dir is the directory of the file (default: the profiles directory),
	// and local means that nothing is downloaded before a command (the
	// file is the user's alone, like keys.yaml).
	dir   func() string
	local bool
	// saved, when set, runs after an edit is saved: with the entry's
	// name, or "" after an edit of the whole file.
	saved func(name string)
}

// plurals is what several entries are called.
func (set *namedSet) plurals() string {
	if set.plural != "" {
		return set.plural
	}
	return set.noun + "s"
}

func (set *namedSet) path() string {
	if set.dir != nil {
		return filepath.Join(set.dir(), set.file)
	}
	return filepath.Join(config.ProfilesDir(), set.file)
}

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

func (d *namedDoc) replace(name string, node *yaml.Node) {
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		if d.table.Content[i].Value == name {
			d.table.Content[i+1] = node
			return
		}
	}
}

func (d *namedDoc) bytes() ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&d.root); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte(b.String()), nil
}

func (d *namedDoc) save() error {
	data, err := d.bytes()
	if err != nil {
		return err
	}
	return os.WriteFile(d.set.path(), data, 0o600)
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
	if !set.local {
		if _, err := a.prepareProfiles(cmd); err != nil {
			return nil, err
		}
	}
	return set.load()
}

func (set *namedSet) missing(name string) error {
	return fmt.Errorf("no %s %q in %s (%s)", set.noun, name, config.AbbreviateHome(set.path()), set.listHint())
}

func (set *namedSet) newListCmd(a *app) *cobra.Command {
	var output string
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: verbAliases("list"),
		Short:   "List the " + set.plurals(),
		Long: "List the " + set.plurals() + " of " + set.file + ", one per row, with whether each is\n" +
			"valid. -o wide is the same table (nothing is held back); -o json and -o yaml\n" +
			"print the entries themselves, by name.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			format, err := parseFormat(output)
			if err != nil || format == formatName {
				return usageErrorf(cmd, "unknown output format %q (use one of table, wide, json, yaml)", output)
			}
			if format == formatJSON || format == formatYAML {
				return printEncoded(a.stdout, d.entries(), format)
			}
			data, _ := os.ReadFile(set.path())
			rows, err := set.rows(data, d.names())
			if err != nil {
				return err
			}
			return writeTable(a.stdout, set.headers, rows, noHeaders)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, wide, json or yaml")
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

// entries decodes every entry, by name, for -o json and -o yaml.
func (d *namedDoc) entries() map[string]any {
	out := map[string]any{}
	for _, n := range d.names() {
		var v any
		if err := d.node(n).Decode(&v); err != nil {
			v = map[string]string{"invalid": err.Error()}
		}
		out[n] = v
	}
	return out
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
				ui.Printf("%s in %s: %s\n", strings.ToUpper(set.plurals()[:1])+set.plurals()[1:], config.AbbreviateHome(set.path()), strings.Join(names, ", "))
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
				return set.editEntry(a, cmd, d, name)
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
		Short:   "Edit one " + set.noun + " in your editor, or all of " + set.file + " without a name",
		Long: "With a name, open that " + set.noun + " alone in your editor; when the editor\n" +
			"returns, the " + set.noun + " is checked and put back into " + set.file + ", the\n" +
			"other " + set.plurals() + " untouched. Without a name, open the whole file and check\n" +
			"every " + set.noun + " afterwards.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := set.prepare(a, cmd)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return set.edit(a, cmd.Context())
			}
			if d.node(args[0]) == nil {
				return set.missing(args[0])
			}
			return set.editEntry(a, cmd, d, args[0])
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
	if set.saved != nil {
		set.saved("")
	}
	names := d.names()
	fmt.Fprintf(a.stdout, "%d %s, all valid: %s.\n", len(names), set.plurals(), strings.Join(names, ", "))
	return nil
}

// editEntry opens one entry in the editor, as "NAME:" with its fields
// in a file of its own, and puts it back into the document when it is
// valid together with the others; the other entries and the comments
// stay as they are. An invalid edit is offered again on a terminal and
// otherwise kept in its file, with nothing saved.
func (set *namedSet) editEntry(a *app, cmd *cobra.Command, d *namedDoc, name string) error {
	f, err := os.CreateTemp("", "ump-"+set.noun+"-"+name+"-*.yaml")
	if err != nil {
		return err
	}
	tmp := f.Name()
	header := fmt.Sprintf("# The %s %s from %s. Save and close to apply it; the\n# other %s stay as they are. The name cannot change here.\n", set.noun, name, config.AbbreviateHome(set.path()), set.plurals())
	orig := header + d.render(name)
	_, err = f.WriteString(orig)
	f.Close()
	if err != nil {
		return err
	}
	for {
		if err := a.editor(tmp); err != nil {
			return err
		}
		data, err := os.ReadFile(tmp)
		if err != nil {
			return err
		}
		if string(data) == orig {
			os.Remove(tmp)
			fmt.Fprintf(a.stdout, "No changes to the %s %s.\n", set.noun, name)
			return nil
		}
		node, err := set.entryNode(data, name)
		if err == nil {
			d.replace(name, node)
			var whole []byte
			if whole, err = d.bytes(); err == nil {
				err = set.check(whole)
			}
		}
		if err == nil {
			if err := d.save(); err != nil {
				return err
			}
			os.Remove(tmp)
			if set.saved != nil {
				set.saved(name)
			}
			fmt.Fprintf(a.stdout, "Saved the %s %s in %s.\n", set.noun, name, config.AbbreviateHome(set.path()))
			return nil
		}
		fmt.Fprintf(a.stderr, "ump: the %s %s is not valid: %v\n", set.noun, name, err)
		ui := a.resolverUI(cmd)
		if ui == nil {
			return fmt.Errorf("nothing was saved; your edit is in %s", tmp)
		}
		if again, cerr := ui.Confirm("Open it again?", true); cerr != nil || !again {
			return fmt.Errorf("nothing was saved; your edit is in %s", tmp)
		}
		// the next round starts from the edit, and the document from the file
		orig = ""
		if d, err = set.load(); err != nil {
			return err
		}
	}
}

// entryNode reads an edited entry: one mapping key, the entry's name.
func (set *namedSet) entryNode(data []byte, name string) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode || len(root.Content[0].Content) != 2 {
		return nil, fmt.Errorf("expected one %s, %q with its fields", set.noun, name+":")
	}
	if k := root.Content[0].Content[0].Value; k != name {
		return nil, fmt.Errorf("the name changed to %q; edit keeps the name (create %s --from %s, then delete %s, renames)", k, k, name, name)
	}
	return root.Content[0].Content[1], nil
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
