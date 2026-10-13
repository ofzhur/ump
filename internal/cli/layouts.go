package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
)

func layoutsPath() string { return filepath.Join(config.ProfilesDir(), "layouts.yaml") }

func (a *app) newConfigLayoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "disk-layout",
		Aliases: []string{"dl", "layout", "layouts"},
		Short:   "The named disk layouts the profiles refer to (interactive wizard)",
		Long: `The disk layouts live in layouts.yaml next to the profiles
(` + config.AbbreviateHome(layoutsPath()) + `), one named entry each; a profile
names one in its disks field, or carries its own. The first use puts the
published layouts there. A layout is a list of parts (slices on matching
disks, md arrays, logical volumes); profiles/README.md in the repository
describes the fields.

Without a subcommand and on a terminal, ump config disk-layout is a
small wizard over the subcommands below.`,
		Example: `  ump config dl list
  ump config dl get lvm-raid1
  ump config dl create nvme-raid1 --from lvm-raid1   # then your editor
  ump config dl edit
  ump config dl delete nvme-raid1`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.layoutsWizard(cmd)
		},
	}
	cmd.AddCommand(a.newLayoutListCmd(), a.newLayoutGetCmd(), a.newLayoutCreateCmd(), a.newLayoutEditCmd(), a.newLayoutDeleteCmd())
	return cmd
}

// layoutsDoc is layouts.yaml as a YAML document, comments included, with
// the mapping node of its layouts.
type layoutsDoc struct {
	root  yaml.Node
	table *yaml.Node
}

func loadLayoutsDoc() (*layoutsDoc, error) {
	data, err := os.ReadFile(layoutsPath())
	if err != nil {
		return nil, err
	}
	d := &layoutsDoc{}
	if err := yaml.Unmarshal(data, &d.root); err != nil {
		return nil, fmt.Errorf("%s: %w", layoutsPath(), err)
	}
	if d.root.Kind != yaml.DocumentNode || len(d.root.Content) == 0 || d.root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a mapping with a layouts key", layoutsPath())
	}
	top := d.root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "layouts" && top.Content[i+1].Kind == yaml.MappingNode {
			d.table = top.Content[i+1]
		}
	}
	if d.table == nil {
		return nil, fmt.Errorf("%s: no layouts mapping", layoutsPath())
	}
	return d, nil
}

func (d *layoutsDoc) names() []string {
	var names []string
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		names = append(names, d.table.Content[i].Value)
	}
	sort.Strings(names)
	return names
}

func (d *layoutsDoc) node(name string) *yaml.Node {
	for i := 0; i+1 < len(d.table.Content); i += 2 {
		if d.table.Content[i].Value == name {
			return d.table.Content[i+1]
		}
	}
	return nil
}

func (d *layoutsDoc) save() error {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&d.root); err != nil {
		return err
	}
	enc.Close()
	return os.WriteFile(layoutsPath(), []byte(b.String()), 0o600)
}

func (a *app) prepareLayouts(cmd *cobra.Command) (*layoutsDoc, error) {
	if _, err := a.prepareProfiles(cmd); err != nil {
		return nil, err
	}
	return loadLayoutsDoc()
}

func (a *app) newLayoutListCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the layouts",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepareLayouts(cmd)
			if err != nil {
				return err
			}
			data, _ := os.ReadFile(layoutsPath())
			layouts, err := profile.ParseLayouts(data)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, n := range d.names() {
				l := layouts[n]
				state := "ok"
				if err := l.Validate(); err != nil {
					state = "invalid"
				}
				var kinds []string
				for _, p := range l.Parts {
					kinds = append(kinds, p.Type)
				}
				rows = append(rows, []string{n, fmt.Sprintf("%d (%s)", len(l.Parts), strings.Join(kinds, ",")), state, l.Description})
			}
			return writeTable(a.stdout, []string{"NAME", "PARTS", "STATE", "DESCRIPTION"}, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newLayoutGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get NAME",
		Aliases: []string{"show", "cat"},
		Short:   "Print a layout",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepareLayouts(cmd)
			if err != nil {
				return err
			}
			n := d.node(args[0])
			if n == nil {
				return fmt.Errorf("no layout %q in %s (ump config disk-layout list)", args[0], config.AbbreviateHome(layoutsPath()))
			}
			var b strings.Builder
			enc := yaml.NewEncoder(&b)
			enc.SetIndent(2)
			if err := enc.Encode(map[string]*yaml.Node{args[0]: n}); err != nil {
				return err
			}
			enc.Close()
			_, err = fmt.Fprint(a.stdout, b.String())
			return err
		},
	}
}

func (a *app) newLayoutCreateCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "create NAME [--from NAME]",
		Short: "Create a layout from another one and open the file in your editor",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepareLayouts(cmd)
			if err != nil {
				return err
			}
			name := args[0]
			if err := config.ValidateAccountName(name); err != nil {
				return usageErrorf(cmd, "layout name: %v", err)
			}
			if d.node(name) != nil {
				return fmt.Errorf("layout %s exists already (ump config disk-layout edit)", name)
			}
			src := d.node(from)
			if src == nil {
				return fmt.Errorf("no layout %q to start from", from)
			}
			copied := *src
			d.table.Content = append(d.table.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, &copied)
			if err := d.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Created the layout %s from %s in %s.\n", name, from, config.AbbreviateHome(layoutsPath()))
			if a.isTTY != nil && a.isTTY() {
				return a.editLayouts(cmd.Context())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "lvm-linear", "the layout to copy")
	return cmd
}

func (a *app) newLayoutEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit [NAME]",
		Short: "Open layouts.yaml in your editor and check every layout afterwards",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepareLayouts(cmd)
			if err != nil {
				return err
			}
			if len(args) == 1 && d.node(args[0]) == nil {
				return fmt.Errorf("no layout %q in %s (ump config disk-layout list)", args[0], config.AbbreviateHome(layoutsPath()))
			}
			return a.editLayouts(cmd.Context())
		},
	}
}

func (a *app) newLayoutDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a layout",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.prepareLayouts(cmd)
			if err != nil {
				return err
			}
			name := args[0]
			if d.node(name) == nil {
				return fmt.Errorf("no layout %q in %s (ump config disk-layout list)", name, config.AbbreviateHome(layoutsPath()))
			}
			if err := a.confirm(cmd, yes, fmt.Sprintf("Delete the layout %s from %s?", name, config.AbbreviateHome(layoutsPath()))); err != nil {
				return err
			}
			for i := 0; i+1 < len(d.table.Content); i += 2 {
				if d.table.Content[i].Value == name {
					d.table.Content = append(d.table.Content[:i], d.table.Content[i+2:]...)
					break
				}
			}
			if err := d.save(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Deleted the layout %s.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// editLayouts runs the editor on layouts.yaml and checks every layout.
func (a *app) editLayouts(ctx context.Context) error {
	if err := a.editor(layoutsPath()); err != nil {
		return err
	}
	data, err := os.ReadFile(layoutsPath())
	if err != nil {
		return err
	}
	layouts, err := profile.ParseLayouts(data)
	if err != nil {
		return fmt.Errorf("the file is kept, but it is not valid: %w", err)
	}
	names := make([]string, 0, len(layouts))
	for n := range layouts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := layouts[n].Validate(); err != nil {
			return fmt.Errorf("the file is kept, but it is not valid: %w", err)
		}
	}
	fmt.Fprintf(a.stdout, "%d layouts, all valid: %s.\n", len(names), strings.Join(names, ", "))
	return nil
}

// layoutsWizard is ump config disk-layout on a terminal.
func (a *app) layoutsWizard(cmd *cobra.Command) error {
	ui := a.resolverUI(cmd)
	if ui == nil {
		return withExit(exitAborted, errors.New("ump config disk-layout is interactive and needs a terminal; use its subcommands (list, get, create, edit, delete) instead"))
	}
	for {
		d, err := a.prepareLayouts(cmd)
		if err != nil {
			return err
		}
		names := d.names()
		ui.Printf("Layouts in %s: %s\n", config.AbbreviateHome(layoutsPath()), strings.Join(names, ", "))
		choice, err := ui.Choose("What now?", []string{"edit the layouts", "create a layout", "delete a layout", "show a layout"}, -1)
		if err != nil {
			return err
		}
		switch choice {
		case 0:
			if err := a.editLayouts(cmd.Context()); err != nil {
				ui.Printf("%v\n", err)
			}
		case 1:
			name, err := ui.Line("Name of the new layout", "")
			if err != nil {
				return err
			}
			if err := config.ValidateAccountName(name); err != nil || d.node(name) != nil {
				ui.Printf("%s is not a free, valid name\n", name)
				continue
			}
			from, err := ui.Choose("Start from", names, 0)
			if err != nil {
				return err
			}
			copied := *d.node(names[from])
			d.table.Content = append(d.table.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, &copied)
			if err := d.save(); err != nil {
				return err
			}
			if err := a.editLayouts(cmd.Context()); err != nil {
				ui.Printf("%v\n", err)
			}
		case 2, 3:
			i, err := ui.Choose("Which layout?", names, -1)
			if err != nil {
				return err
			}
			if choice == 3 {
				var b strings.Builder
				enc := yaml.NewEncoder(&b)
				enc.SetIndent(2)
				enc.Encode(map[string]*yaml.Node{names[i]: d.node(names[i])})
				enc.Close()
				ui.Printf("%s\n", b.String())
				break
			}
			ok, err := ui.Confirm(fmt.Sprintf("Delete %s?", names[i]), false)
			if err != nil {
				return err
			}
			if ok {
				d.table.Content = append(d.table.Content[:2*i], d.table.Content[2*i+2:]...)
				if err := d.save(); err != nil {
					return err
				}
				ui.Printf("Deleted %s.\n", names[i])
			}
		}
		again, err := ui.Confirm("Anything else?", false)
		if err != nil || !again {
			return err
		}
	}
}
