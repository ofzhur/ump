package cli

import (
	"context"
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
// publishedProfiles are the profiles the storage publishes; the named
// files next to them hold the published entries and the user's own.
var publishedProfiles = []string{"ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04", "debian-13", "debian-12", "nixos-25.11", "archlinux"}

var namedFiles = []*namedSet{layoutSet, firewallSet, userSet, userdataSet}

// profileFiles are the files the first run downloads.
var profileFiles = func() []string {
	var files []string
	for _, p := range publishedProfiles {
		files = append(files, p+".yaml")
	}
	for _, set := range namedFiles {
		files = append(files, set.file)
	}
	return files
}()

func isPublishedProfile(name string) bool {
	for _, p := range publishedProfiles {
		if p == name {
			return true
		}
	}
	return false
}

// ensureProfiles makes sure the profiles directory exists with the
// published profiles, layouts, firewalls and users in it: the first run
// downloads them from the storage, so that there is always something to
// show and start from. An existing directory is left as it is.
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
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stderr, "Downloaded the published profiles, layouts, firewalls and users into %s (ump config os); make your own from them.\n", config.AbbreviateHome(dir))
	return nil
}

func (a *app) newOSUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "update",
		Aliases: verbAliases("update"),
		Short:   "Refresh the published profiles, layouts, firewalls and users from the storage",
		Long: `Fetch the published files again. The published profiles (` + strings.Join(publishedProfiles, ", ") + `)
are replaced, which is why they are not for editing: make your own
from them (ump config os create NAME --from ` + publishedProfiles[0] + `) and those
are never touched. In layouts.yaml, firewalls.yaml and users.yaml the
published entries are replaced and the entries you added are kept
(an entry of yours with a published name is replaced too, so give
yours their own names).`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.prepareProfiles(cmd)
			if err != nil {
				return err
			}
			dir := config.ProfilesDir()
			for _, name := range publishedProfiles {
				data, err := a.fetch(cmd.Context(), cfg.StorageURL()+"/profiles/"+name+".yaml")
				if err != nil {
					return fmt.Errorf("the published %s.yaml could not be fetched: %w", name, err)
				}
				local, _ := os.ReadFile(filepath.Join(dir, name+".yaml"))
				if string(local) == string(data) {
					fmt.Fprintf(a.stdout, "%s.yaml: up to date\n", name)
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name+".yaml"), data, 0o600); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "%s.yaml: updated\n", name)
			}
			for _, set := range namedFiles {
				data, err := a.fetch(cmd.Context(), cfg.StorageURL()+"/profiles/"+set.file)
				if err != nil {
					return fmt.Errorf("the published %s could not be fetched: %w", set.file, err)
				}
				kept, err := set.merge(data)
				if err != nil {
					return err
				}
				if len(kept) > 0 {
					fmt.Fprintf(a.stdout, "%s: updated; your %s kept: %s\n", set.file, set.plurals(), strings.Join(kept, ", "))
				} else {
					fmt.Fprintf(a.stdout, "%s: updated\n", set.file)
				}
			}
			return nil
		},
	}
}

func (a *app) newOSDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "default [NAME]",
		Aliases: verbAliases("default"),
		Short:   "Show or set the default profile (what ump server reinstall takes without --profile)",
		Args:    usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.prepareProfiles(cmd)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				fmt.Fprintln(a.stdout, cfg.DefaultProfileName())
				return nil
			}
			return a.setDefaultProfile(cfg, args[0])
		},
	}
}

func (a *app) setDefaultProfile(cfg *config.Config, name string) error {
	if _, err := os.Stat(profilePath(name)); err != nil {
		return fmt.Errorf("no profile %q in %s (ump config os list)", name, config.AbbreviateHome(config.ProfilesDir()))
	}
	cfg.DefaultProfile = name
	if name == config.DefaultProfile {
		cfg.DefaultProfile = ""
	}
	path := config.ResolvePath(a.configFlag)
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "The default profile is %s now (ump server reinstall without --profile, and servers without a profile of their own).\n", name)
	return nil
}

func (a *app) newConfigOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "os",
		Aliases: []string{"o", "profile"},
		Short:   "The profiles that say what a server becomes",
		Long: `Profiles are YAML files in the profiles directory next to the
configuration (` + config.AbbreviateHome(config.ProfilesDir()) + `), one per name,
plus layouts.yaml with the named disk layouts. The first run puts the
published default profile and layouts there. A profile names the image,
the disk layout, the users with their keys, the userdata that runs in
the new system, the firewall and the provision step; docs/profiles.md in the repository describes
every field. ump server reinstall NAME --profile NAME installs with one,
ump server set NAME --profile NAME remembers it for a server.

On a terminal, ump config os create is a wizard that asks what a new
profile holds.`,
		Example: `  ump config os list
  ump config os get ubuntu-26.04
  ump config os create              # the wizard: a name and a few questions
  ump config os create db --from ubuntu-26.04   # a copy, then your editor
  ump config os edit db
  ump config os default db
  ump config os delete db`,
	}
	cmd.AddCommand(a.newOSListCmd(), a.newOSGetCmd(), a.newOSCreateCmd(), a.newOSEditCmd(), a.newOSDeleteCmd(), a.newOSUpdateCmd(), a.newOSDefaultCmd())
	return cmd
}

// profilePath is the file of a named profile in the profiles directory.
func profilePath(name string) string {
	return filepath.Join(config.ProfilesDir(), name+".yaml")
}

// localProfiles lists the profiles in the directory, by name.
// isNamedFile reports whether a file of the profiles directory is one of
// the named sets (layouts.yaml, firewalls.yaml, users.yaml, userdata.yaml,
// ...) rather than a profile.
func isNamedFile(file string) bool {
	for _, set := range namedFiles {
		if set.file == file {
			return true
		}
	}
	return false
}

// checkProfileName refuses a profile name that is not valid, or that
// would be the file of a named set.
func checkProfileName(name string) error {
	if err := config.ValidateAccountName(name); err != nil {
		return err
	}
	if isNamedFile(name + ".yaml") {
		return fmt.Errorf("%s.yaml holds the named %s entries; give the profile another name", name, name)
	}
	return nil
}

func localProfiles() ([]string, error) {
	entries, err := os.ReadDir(config.ProfilesDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".yaml") || isNamedFile(n) {
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
		Aliases: verbAliases("list"),
		Short:   "List the profiles",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.prepareProfiles(cmd)
			if err != nil {
				return err
			}
			names, err := localProfiles()
			if err != nil {
				return err
			}
			def := cfg.DefaultProfileName()
			var rows [][]string
			for _, n := range names {
				mark := ""
				if n == def {
					mark = "*"
				}
				p, err := profile.Load(profilePath(n))
				if err != nil {
					rows = append(rows, []string{mark, n, "-", "-", "-", "-", "invalid: " + err.Error()})
					continue
				}
				disks := p.Disks.Name
				if p.Disks.Layout != nil {
					disks = "(inline)"
				}
				var users []string
				for _, r := range p.Users {
					if r.User != nil {
						users = append(users, r.User.Name)
					} else {
						users = append(users, r.Name)
					}
				}
				fw := p.Firewall.Name
				switch {
				case p.Firewall.Rules != nil:
					fw = "(inline)"
				case fw == "":
					fw = "none"
				}
				kind := "published"
				if !isPublishedProfile(n) {
					kind = "yours"
				}
				rows = append(rows, []string{mark, n, p.OS, disks, strings.Join(users, ","), fw, kind, p.Description})
			}
			if err := writeTable(a.stdout, []string{"", "NAME", "OS", "DISKS", "USERS", "FIREWALL", "FROM", "DESCRIPTION"}, rows, noHeaders); err != nil {
				return err
			}
			if !noHeaders {
				fmt.Fprintf(a.stdout, "* the default (ump config os default NAME changes it); published profiles are replaced by ump config os update, make your own with ump config os create NAME --from %s\n", def)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header line")
	return cmd
}

func (a *app) newOSGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get NAME",
		Aliases: verbAliases("get"),
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
		Use:     "create [NAME] [--from NAME]",
		Aliases: verbAliases("create"),
		Short:   "Create a profile: a wizard on a terminal, or a copy of another one",
		Long: `On a terminal, ump config os create is a wizard: it asks for the name
when none is given, then what the profile should hold (the image from
the storage, the disk layout, the firewall, the administrator with shell
and keys, the kernel switches, the time zone, the userdata, the
interface names, the provision step) and writes it. --from NAME copies another
profile instead and opens it in your editor; without a terminal that is
the only way.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.prepareProfiles(cmd); err != nil {
				return err
			}
			ui := a.resolverUI(cmd)
			var name string
			if len(args) > 0 {
				name = args[0]
			} else {
				if ui == nil {
					return usageErrorf(cmd, "a profile name is needed without a terminal: ump config os create NAME --from NAME")
				}
				names, err := localProfiles()
				if err != nil {
					return err
				}
				ui.Printf("Profiles in %s: %s\n", config.AbbreviateHome(config.ProfilesDir()), strings.Join(names, ", "))
				if name, err = ui.Line("Name of the new profile", ""); err != nil {
					return err
				}
			}
			if err := checkProfileName(name); err != nil {
				return usageErrorf(cmd, "profile name: %v", err)
			}
			if _, err := os.Stat(profilePath(name)); err == nil {
				return fmt.Errorf("profile %s exists already (ump config os edit %s)", name, name)
			}
			if !cmd.Flags().Changed("from") && ui != nil {
				return a.profileWizard(cmd, ui, name)
			}
			if from == "" {
				cfg, _, err := a.loadConfig()
				if err != nil {
					return err
				}
				from = cfg.DefaultProfileName()
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
	cmd.Flags().StringVar(&from, "from", "", "the profile to copy (default: the default profile)")
	return cmd
}

func (a *app) newOSEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "edit NAME",
		Aliases: verbAliases("edit"),
		Short:   "Open a profile in your editor and check it afterwards",
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
			if isPublishedProfile(args[0]) {
				return fmt.Errorf("%s is a published profile, which ump config os update replaces; make your own from it: ump config os create NAME --from %s", args[0], args[0])
			}
			return a.editProfile(cmd, args[0])
		},
	}
}

func (a *app) newOSDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: verbAliases("delete"),
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
	var users map[string]*profile.User
	if profile.HasUserRefs(p) {
		if users, err = a.loadUsers(ctx, cfg.StorageURL()); err != nil {
			return err
		}
	}
	var userdata map[string]*profile.Userdata
	if profile.HasUserdataRefs(p) {
		if userdata, err = a.loadUserdata(ctx, cfg.StorageURL()); err != nil {
			return err
		}
	}
	if _, err := p.Resolve(layouts, firewalls, users, userdata, checkKeys); err != nil {
		return fmt.Errorf("the profile is kept, but it is not valid: %w", err)
	}
	fmt.Fprintf(a.stdout, "Profile %s is valid (%s, layout %s).\n", name, p.OS, layoutName(p))
	return nil
}

// checkKeys stands in for the key resolver when a profile is only
// checked: every reference yields a key.
func checkKeys(ref string) ([]string, error) {
	return []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTeN check"}, nil
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
