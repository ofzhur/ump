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

func usersPath() string { return filepath.Join(config.ProfilesDir(), "users.yaml") }

var userSet = &namedSet{
	file: "users.yaml", key: "users", noun: "user",
	use:     "user",
	aliases: []string{"u", "users"},
	short:   "The named users the profiles refer to (interactive wizard)",
	long: `The users live in users.yaml next to the profiles
(` + config.AbbreviateHome(usersPath()) + `), one named entry each; a profile
lists names from it in its users field (users: [ump, ops]), or carries
users of its own, so the same user can be in many profiles. The first
use puts the published users there: ump, the administrator UMP
provides (sudo without a password, bash, UMP's key and yours). A user
has sudo, groups, a shell and key references (ump, local, agent, a key
name from ump config key, a file, or a key line); docs/profiles.md
describes the fields.

Without a subcommand and on a terminal, ump config user is a small
wizard over the subcommands below.`,
	example: `  ump config user list
  ump config user get ump
  ump config user create ops --from ump   # then your editor
  ump config user edit
  ump config user delete ops`,
	defaultFrom: "ump",
	headers:     []string{"NAME", "SUDO", "SHELL", "KEYS", "STATE", "DESCRIPTION"},
	rows: func(data []byte, names []string) ([][]string, error) {
		users, err := profile.ParseUsers(data)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, n := range names {
			u := users[n]
			state := "ok"
			if err := u.Validate(); err != nil {
				state = "invalid"
			}
			sudo := "-"
			if u.Sudo {
				sudo = "yes"
			}
			shell := u.Shell
			if shell == "" {
				shell = "/bin/bash"
			}
			rows = append(rows, []string{n, sudo, shell, strings.Join(keySummary(u.Keys), ", "), state, u.Description})
		}
		return rows, nil
	},
	check: func(data []byte) error {
		users, err := profile.ParseUsers(data)
		if err != nil {
			return err
		}
		for _, n := range sortedKeys(users) {
			if err := users[n].Validate(); err != nil {
				return err
			}
		}
		return nil
	},
}

// keySummary shortens key lines to their type and comment for a listing.
func keySummary(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if profile.IsKeyLine(r) {
			f := strings.Fields(r)
			s := f[0]
			if len(f) > 2 {
				s += " " + f[2]
			}
			out = append(out, "("+s+")")
			continue
		}
		out = append(out, r)
	}
	return out
}

func (a *app) newConfigUserCmd() *cobra.Command { return userSet.newCmd(a) }

// loadUsers reads users.yaml: the user's own in the profiles directory,
// else the published one.
func (a *app) loadUsers(ctx context.Context, storage string) (map[string]*profile.User, error) {
	data, from, err := a.loadNamed(ctx, storage, "users.yaml")
	if err != nil {
		return nil, err
	}
	users, err := profile.ParseUsers(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return users, nil
}

// localUsers reads users.yaml from the profiles directory alone; an
// absent or broken file gives no users.
func localUsers() map[string]*profile.User {
	data, err := readFile(usersPath())
	if err != nil {
		return nil
	}
	users, _ := profile.ParseUsers(data)
	return users
}
