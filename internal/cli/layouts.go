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

func layoutsPath() string { return filepath.Join(config.ProfilesDir(), "layouts.yaml") }

var layoutSet = &namedSet{
	file: "layouts.yaml", key: "layouts", noun: "layout",
	use:     "disk-layout",
	aliases: []string{"dl", "layout", "layouts"},
	short:   "The named disk layouts the profiles refer to (interactive wizard)",
	long: `The disk layouts live in layouts.yaml next to the profiles
(` + config.AbbreviateHome(layoutsPath()) + `), one named entry each; a profile
names one in its disks field, or carries its own. The first use puts the
published layouts there. A layout is a list of parts (slices on matching
disks, md arrays, logical volumes); profiles/README.md in the repository
describes the fields.

Without a subcommand and on a terminal, ump config disk-layout is a
small wizard over the subcommands below.`,
	example: `  ump config dl list
  ump config dl get lvm-raid1
  ump config dl create nvme-raid1 --from lvm-raid1   # then your editor
  ump config dl edit
  ump config dl delete nvme-raid1`,
	defaultFrom: "lvm-linear",
	headers:     []string{"NAME", "PARTS", "STATE", "DESCRIPTION"},
	rows: func(data []byte, names []string) ([][]string, error) {
		layouts, err := profile.ParseLayouts(data)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, n := range names {
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
		return rows, nil
	},
	check: func(data []byte) error {
		layouts, err := profile.ParseLayouts(data)
		if err != nil {
			return err
		}
		for _, n := range sortedKeys(layouts) {
			if err := layouts[n].Validate(); err != nil {
				return err
			}
		}
		return nil
	},
}

func (a *app) newConfigLayoutCmd() *cobra.Command { return layoutSet.newCmd(a) }

// loadLayouts reads layouts.yaml: the user's own in the profiles
// directory, else the published one.
func (a *app) loadLayouts(ctx context.Context, storage string) (map[string]*profile.Layout, error) {
	data, from, err := a.loadNamed(ctx, storage, "layouts.yaml")
	if err != nil {
		return nil, err
	}
	layouts, err := profile.ParseLayouts(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return layouts, nil
}
