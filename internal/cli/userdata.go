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

func userdataPath() string { return filepath.Join(config.ProfilesDir(), "userdata.yaml") }

var userdataSet = &namedSet{
	file: "userdata.yaml", key: "userdata", noun: "userdata", plural: "userdata",
	use:     "userdata",
	aliases: []string{"ud"},
	short:   "The named userdata the profiles refer to: what runs in a freshly installed system",
	long: `The userdata lives in userdata.yaml next to the profiles
(` + config.AbbreviateHome(userdataPath()) + `), one named entry each; a profile
lists names from it in its userdata field (userdata: [ubuntu, web]), or
carries entries of its own. An entry is a script (type bash) that runs
as root in the freshly installed system, before its first boot, with
the network of the rescue system: install packages, drop files, enable
services. The first use puts the published entry there: ubuntu, which
does nothing yet. Operating systems that take a declarative
configuration (cloud-init, Talos, NixOS) will take it here too, as
other types. docs/userdata.md describes the fields.

On a terminal, create without a name asks for one and for the
userdata to start from.`,
	example: `  ump config userdata list
  ump config userdata get ubuntu
  ump config userdata create web --from ubuntu   # then your editor
  ump config userdata edit
  ump config userdata delete web`,
	defaultFrom: "ubuntu",
	headers:     []string{"NAME", "TYPE", "LINES", "STATE", "DESCRIPTION"},
	rows: func(data []byte, names []string) ([][]string, error) {
		entries, err := profile.ParseUserdata(data)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, n := range names {
			u := entries[n]
			state := "ok"
			if err := u.Validate(); err != nil {
				state = "invalid"
			}
			lines := len(strings.Split(strings.TrimRight(u.Script, "\n"), "\n"))
			rows = append(rows, []string{n, u.Type, fmt.Sprint(lines), state, u.Description})
		}
		return rows, nil
	},
	check: func(data []byte) error {
		entries, err := profile.ParseUserdata(data)
		if err != nil {
			return err
		}
		for _, n := range sortedKeys(entries) {
			if err := entries[n].Validate(); err != nil {
				return err
			}
		}
		return nil
	},
}

func (a *app) newConfigUserdataCmd() *cobra.Command { return userdataSet.newCmd(a) }

// loadUserdata reads userdata.yaml: the user's own in the profiles
// directory, else the published one.
func (a *app) loadUserdata(ctx context.Context, storage string) (map[string]*profile.Userdata, error) {
	data, from, err := a.loadNamed(ctx, storage, "userdata.yaml")
	if err != nil {
		return nil, err
	}
	entries, err := profile.ParseUserdata(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return entries, nil
}
