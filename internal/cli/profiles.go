package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
)

// profileFiles are what a profiles directory starts with.
var profileFiles = []string{"default.yaml", "layouts.yaml", "firewalls.yaml"}

// ensureProfiles makes sure the profiles directory exists with the
// published default profile and layouts in it: the first run downloads
// them from the storage, so that there is always something to show and
// edit. An existing directory is left as it is.
func (a *app) ensureProfiles(ctx context.Context, storage string) error {
	dir := config.ProfilesDir()
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range profileFiles {
		data, err := a.fetch(ctx, storage+"/profiles/"+name)
		if err != nil {
			os.Remove(dir)
			return fmt.Errorf("the profiles directory %s is new and the published %s could not be fetched: %w", config.AbbreviateHome(dir), name, err)
		}
		if err := writePublished(dir, name, data); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stderr, "Downloaded the default profile, the layouts and the firewalls into %s; they are yours to edit (ump config os).\n", config.AbbreviateHome(dir))
	return nil
}

// publishedDir keeps a copy of each file as it was published, so that
// an update can tell a file the user edited from one left as it was.
const publishedDir = ".published"

func writePublished(dir, name string, data []byte) error {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, publishedDir), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, publishedDir, name), data, 0o600)
}

func (a *app) newOSUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Refresh the published profile, layouts and firewalls from the storage",
		Long: `Fetch default.yaml, layouts.yaml and firewalls.yaml from the storage
again. A local file that is still as it was published is replaced; one
you edited is kept, and the new published version is put next to it in
` + publishedDir + `/ for you to compare. Your own profiles (other names) are
never touched.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.prepareProfiles(cmd)
			if err != nil {
				return err
			}
			dir := config.ProfilesDir()
			for _, name := range profileFiles {
				data, err := a.fetch(cmd.Context(), cfg.StorageURL()+"/profiles/"+name)
				if err != nil {
					return fmt.Errorf("the published %s could not be fetched: %w", name, err)
				}
				local, _ := os.ReadFile(filepath.Join(dir, name))
				published, _ := os.ReadFile(filepath.Join(dir, publishedDir, name))
				switch {
				case string(local) == string(data):
					fmt.Fprintf(a.stdout, "%s: up to date\n", name)
					os.MkdirAll(filepath.Join(dir, publishedDir), 0o700)
					os.WriteFile(filepath.Join(dir, publishedDir, name), data, 0o600)
				case local == nil || string(local) == string(published):
					if err := writePublished(dir, name, data); err != nil {
						return err
					}
					fmt.Fprintf(a.stdout, "%s: updated\n", name)
				default:
					os.MkdirAll(filepath.Join(dir, publishedDir), 0o700)
					if err := os.WriteFile(filepath.Join(dir, publishedDir, name), data, 0o600); err != nil {
						return err
					}
					fmt.Fprintf(a.stdout, "%s: you changed it, so it is kept; the new published version is %s\n", name, config.AbbreviateHome(filepath.Join(dir, publishedDir, name)))
				}
			}
			return nil
		},
	}
}

func (a *app) newConfigOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "os",
		Aliases: []string{"profile", "profiles"},
		Short:   "The profiles that say what a server becomes (interactive wizard)",
		Long: `Profiles are YAML files in the profiles directory next to the
configuration (` + config.AbbreviateHome(config.ProfilesDir()) + `), one per name,
plus layouts.yaml with the named disk layouts. The first run puts the
published default profile and layouts there. A profile names the image,
the disk layout, the users with their keys, extra packages, the firewall
and the provision step; docs/profiles.md in the repository describes
every field. ump server reinstall NAME --profile NAME installs with one,
ump server set NAME --profile NAME remembers it for a server.

Without a subcommand and on a terminal, ump config os is a small wizard
over the subcommands below.`,
		Example: `  ump config os list
  ump config os get default
  ump config os create db           # from default, then your editor
  ump config os edit db
  ump config os delete db`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.profilesWizard(cmd)
		},
	}
	cmd.AddCommand(a.newOSListCmd(), a.newOSGetCmd(), a.newOSCreateCmd(), a.newOSEditCmd(), a.newOSDeleteCmd(), a.newOSUpdateCmd())
	return cmd
}

// profilePath is the file of a named profile in the profiles directory.
func profilePath(name string) string {
	return filepath.Join(config.ProfilesDir(), name+".yaml")
}

// localProfiles lists the profiles in the directory, by name.
func localProfiles() ([]string, error) {
	entries, err := os.ReadDir(config.ProfilesDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".yaml") || n == "layouts.yaml" || n == "firewalls.yaml" {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".yaml"))
	}
	sort.Strings(names)
	return names, nil
}

func (a *app) prepareProfiles(cmd *cobra.Command) (*config.Config, error) {
	cfg, _, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := a.ensureProfiles(cmd.Context(), cfg.StorageURL()); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (a *app) newOSListCmd() *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the profiles",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			names, err := localProfiles()
			if err != nil {
				return err
			}
			var rows [][]string
			for _, n := range names {
				p, err := profile.Load(profilePath(n))
				if err != nil {
					rows = append(rows, []string{n, "-", "-", "-", "-", "invalid: " + err.Error()})
					continue
				}
				disks := p.Disks.Name
				if p.Disks.Layout != nil {
					disks = "(inline)"
				}
				var users []string
				for _, u := range p.Users {
					users = append(users, u.Name)
				}
				fw := p.Firewall.Name
				switch {
				case p.Firewall.Rules != nil:
					fw = "(inline)"
				case fw == "":
					fw = "none"
				}
				rows = append(rows, []string{n, p.OS, disks, strings.Join(users, ","), fw, p.Description})
			}
			return writeTable(a.stdout, []string{"NAME", "OS", "DISKS", "USERS", "FIREWALL", "DESCRIPTION"}, rows, noHeaders)
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newOSGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get NAME",
		Aliases: []string{"show", "cat"},
		Short:   "Print a profile",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			data, err := os.ReadFile(profilePath(args[0]))
			if err != nil {
				return fmt.Errorf("no profile %q in %s (ump config os list)", args[0], config.AbbreviateHome(config.ProfilesDir()))
			}
			_, err = a.stdout.Write(data)
			return err
		},
	}
}

func (a *app) newOSCreateCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "create NAME [--from NAME]",
		Short: "Create a profile from another one and open it in your editor",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			name := args[0]
			if err := config.ValidateAccountName(name); err != nil {
				return usageErrorf(cmd, "profile name: %v", err)
			}
			if _, err := os.Stat(profilePath(name)); err == nil {
				return fmt.Errorf("profile %s exists already (ump config os edit %s)", name, name)
			}
			data, err := os.ReadFile(profilePath(from))
			if err != nil {
				return fmt.Errorf("no profile %q to start from", from)
			}
			text := string(data)
			text = strings.Replace(text, "\nname: "+from+"\n", "\nname: "+name+"\n", 1)
			if err := os.WriteFile(profilePath(name), []byte(text), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Created %s from %s.\n", config.AbbreviateHome(profilePath(name)), from)
			if a.isTTY != nil && a.isTTY() {
				return a.editProfile(cmd, name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "default", "the profile to copy")
	return cmd
}

func (a *app) newOSEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit NAME",
		Short: "Open a profile in your editor and check it afterwards",
		Long: `Open the profile in $VISUAL or $EDITOR (vi otherwise). The file is checked
when the editor returns: an invalid profile is kept, with the problem
named, so nothing is lost.`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			if _, err := os.Stat(profilePath(args[0])); err != nil {
				return fmt.Errorf("no profile %q in %s (ump config os list)", args[0], config.AbbreviateHome(config.ProfilesDir()))
			}
			return a.editProfile(cmd, args[0])
		},
	}
}

func (a *app) newOSDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a profile",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			path := profilePath(args[0])
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no profile %q in %s (ump config os list)", args[0], config.AbbreviateHome(config.ProfilesDir()))
			}
			if err := a.confirm(cmd, yes, fmt.Sprintf("Delete the profile %s (%s)?", args[0], config.AbbreviateHome(path))); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Deleted %s.\n", config.AbbreviateHome(path))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

// editProfile runs the editor on a profile and checks the result.
func (a *app) editProfile(cmd *cobra.Command, name string) error {
	path := profilePath(name)
	if err := a.editor(path); err != nil {
		return err
	}
	return a.checkProfile(cmd.Context(), name)
}

// checkProfile parses and validates a profile and resolves its layout;
// the keys are not resolved, so a missing key file is found at install.
func (a *app) checkProfile(ctx context.Context, name string) error {
	p, err := profile.Load(profilePath(name))
	if err != nil {
		return fmt.Errorf("the profile is kept, but it is not valid: %w", err)
	}
	if p.Name == "" {
		p.Name = name
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	var layouts map[string]*profile.Layout
	if p.Disks.Layout == nil {
		if layouts, err = a.loadLayouts(ctx, cfg.StorageURL()); err != nil {
			return err
		}
	}
	var firewalls map[string]*profile.Firewall
	if p.Firewall.Name != "" {
		if firewalls, err = a.loadFirewalls(ctx, cfg.StorageURL()); err != nil {
			return err
		}
	}
	if _, err := p.Resolve(layouts, firewalls, func(ref string) ([]string, error) {
		return []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTeN check"}, nil
	}); err != nil {
		return fmt.Errorf("the profile is kept, but it is not valid: %w", err)
	}
	fmt.Fprintf(a.stdout, "Profile %s is valid (%s, layout %s).\n", name, p.OS, layoutName(p))
	return nil
}

func layoutName(p *profile.Profile) string {
	if p.Disks.Layout != nil {
		return "inline"
	}
	return p.Disks.Name
}

// runEditor opens a file in the user's editor, with the terminal.
func runEditor(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command("sh", "-c", editor+` "$0"`, path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor %s: %w", editor, err)
	}
	return nil
}

// profilesWizard is ump config os on a terminal: what is there, and what
// to do with it.
func (a *app) profilesWizard(cmd *cobra.Command) error {
	ui := a.resolverUI(cmd)
	if ui == nil {
		return withExit(exitAborted, errors.New("ump config os is interactive and needs a terminal; use its subcommands (list, get, create, edit, delete) instead"))
	}
	if _, err := a.prepareProfiles(cmd); err != nil {
		return err
	}
	for {
		names, err := localProfiles()
		if err != nil {
			return err
		}
		ui.Printf("Profiles in %s: %s\n", config.AbbreviateHome(config.ProfilesDir()), strings.Join(names, ", "))
		choice, err := ui.Choose("What now?", []string{"edit a profile", "create a profile", "delete a profile", "show a profile"}, -1)
		if err != nil {
			return err
		}
		var name string
		switch choice {
		case 1: // create
			name, err = ui.Line("Name of the new profile", "")
			if err != nil {
				return err
			}
			if err := config.ValidateAccountName(name); err != nil {
				ui.Printf("%v\n", err)
				continue
			}
			from, err := ui.Choose("Start from", names, 0)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(profilePath(names[from]))
			if err != nil {
				return err
			}
			text := strings.Replace(string(data), "\nname: "+names[from]+"\n", "\nname: "+name+"\n", 1)
			if err := os.WriteFile(profilePath(name), []byte(text), 0o600); err != nil {
				return err
			}
			if err := a.editProfile(cmd, name); err != nil {
				ui.Printf("%v\n", err)
			}
		case 0, 2, 3:
			i, err := ui.Choose("Which profile?", names, -1)
			if err != nil {
				return err
			}
			name = names[i]
			switch choice {
			case 0:
				if err := a.editProfile(cmd, name); err != nil {
					ui.Printf("%v\n", err)
				}
			case 2:
				ok, err := ui.Confirm(fmt.Sprintf("Delete %s?", name), false)
				if err != nil {
					return err
				}
				if ok {
					if err := os.Remove(profilePath(name)); err != nil {
						return err
					}
					ui.Printf("Deleted %s.\n", name)
				}
			case 3:
				data, err := os.ReadFile(profilePath(name))
				if err != nil {
					return err
				}
				ui.Printf("%s\n", data)
			}
		}
		again, err := ui.Confirm("Anything else?", false)
		if err != nil || !again {
			return err
		}
	}
}
