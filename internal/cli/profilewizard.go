package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/config"
	"github.com/ofzhur/ump/internal/profile"
)

// osImage is an operating system image the storage offers.
type osImage struct {
	Name    string
	Version string
	Kernel  string
}

// knownImages are tried when the storage publishes no os/index.json.
var knownImages = []string{"ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04", "debian-13", "debian-12", "nixos-25.11", "archlinux", "cachyos"}

// osImages lists the images of the storage: the names from
// os/index.json (or the known ones that answer), with the version and
// kernel of each from its amd64 manifest.
func (a *app) osImages(ctx context.Context, storage string) []osImage {
	var names []string
	if data, err := a.fetch(ctx, storage+"/os/index.json"); err == nil {
		var index struct {
			Images []string `json:"images"`
		}
		if json.Unmarshal(data, &index) == nil {
			names = index.Images
		}
	}
	probe := names == nil
	if probe {
		names = knownImages
	}
	var images []osImage
	for _, n := range names {
		img := osImage{Name: n}
		data, err := a.fetch(ctx, storage+"/os/"+n+"/amd64/manifest.json")
		if err != nil && probe {
			continue
		}
		if err == nil {
			var m struct {
				Version string `json:"version"`
				Kernel  string `json:"kernel"`
			}
			if json.Unmarshal(data, &m) == nil {
				img.Version, img.Kernel = m.Version, m.Kernel
			}
		}
		images = append(images, img)
	}
	return images
}

func (i osImage) String() string {
	s := i.Name
	var details []string
	if i.Kernel != "" {
		details = append(details, "kernel "+strings.TrimSuffix(i.Kernel, "-generic"))
	}
	if i.Version != "" {
		details = append(details, "built "+i.Version)
	}
	if len(details) > 0 {
		s += "  (" + strings.Join(details, ", ") + ")"
	}
	return s
}

// profileWizard asks what a new profile should hold and writes it:
// the image, the layout, the firewall, the administrator and the keys,
// the kernel switches, the time zone, the userdata and the interface
// names. The defaults are those of the default profile.
func (a *app) profileWizard(cmd *cobra.Command, ui *prompter, name string) error {
	ctx := cmd.Context()
	cfg, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	storage := cfg.StorageURL()
	if name == "" {
		for {
			if name, err = ui.Line("Name of the new profile", ""); err != nil {
				return err
			}
			if err := checkProfileName(name); err != nil {
				ui.Printf("%v\n", err)
				continue
			}
			if _, err := os.Stat(profilePath(name)); err == nil {
				ui.Printf("profile %s exists already\n", name)
				continue
			}
			break
		}
	}
	p := &profile.Profile{Name: name}

	// the image
	images := a.osImages(ctx, storage)
	items := make([]string, 0, len(images)+1)
	def := 0
	for i, img := range images {
		items = append(items, img.String())
		if img.Name == "ubuntu-26.04" {
			def = i
		}
	}
	items = append(items, "another name")
	i, err := ui.Choose("Operating system image (from "+storage+"/os/)", items, def)
	if err != nil {
		return err
	}
	if i < len(images) {
		p.OS = images[i].Name
	} else if p.OS, err = ui.Line("Image name (os/<name>/ on the storage)", "ubuntu-26.04"); err != nil {
		return err
	}

	// the layout and the firewall, by name
	layouts, err := a.loadLayouts(ctx, storage)
	if err != nil {
		return err
	}
	p.Disks.Name, err = chooseNamed(ui, "Disk layout (ump config disk-layout)", layouts, "lvm-linear", func(l *profile.Layout) string { return l.Description })
	if err != nil {
		return err
	}
	firewalls, err := a.loadFirewalls(ctx, storage)
	if err != nil {
		return err
	}
	fw, err := chooseNamed(ui, "Firewall (ump config firewall)", firewalls, "default", func(f *profile.Firewall) string { return f.Description })
	if err != nil {
		return err
	}
	p.Firewall = profile.FirewallRef{Name: fw}

	// the users, from users.yaml; a new name is defined now and kept there
	users, err := a.loadUsers(ctx, storage)
	if err != nil {
		return err
	}
	ui.Printf("Users come from users.yaml (ump config user): %s. The first with sudo is the administrator, the one ump logs in as; root gets no SSH login unless listed.\n", strings.Join(sortedKeys(users), ", "))
	for {
		answer, err := ui.Line("Users (comma-separated; a new name is defined next)", "ump")
		if err != nil {
			return err
		}
		p.Users = nil
		ok := true
		for _, n := range splitList(answer) {
			if _, known := users[n]; !known {
				u, err := a.defineUser(ui, n)
				if err != nil {
					return err
				}
				if u == nil {
					ok = false
					break
				}
				users[n] = u
			}
			p.Users = append(p.Users, profile.UserRef{Name: n})
		}
		if ok && len(p.Users) > 0 {
			break
		}
	}

	// the kernel, the time zone, the userdata, the interfaces
	mit, err := ui.Confirm("CPU vulnerability mitigations on? (they cost performance; mitigations=off otherwise)", false)
	if err != nil {
		return err
	}
	v6, err := ui.Confirm("IPv6 on? (ipv6.disable=1 otherwise)", false)
	if err != nil {
		return err
	}
	p.Kernel = profile.Kernel{Mitigations: &mit, IPv6: &v6}
	if p.Timezone, err = ui.Line("Time zone", "UTC"); err != nil {
		return err
	}
	userdata, err := a.loadUserdata(ctx, storage)
	if err != nil {
		return err
	}
	ui.Printf("Userdata comes from userdata.yaml (ump config userdata): %s. It runs as root in the new system before its first boot.\n", strings.Join(sortedKeys(userdata), ", "))
	for {
		// the published entry of the system's name (ubuntu, debian, nixos)
		def := ""
		if name, _, _ := strings.Cut(p.OS, "-"); userdata[name] != nil {
			def = name
		}
		answer, err := ui.Line("Userdata (comma-separated, in order; none for nothing; "+profile.UserdataType(profile.Family(p.OS))+" for "+p.OS+")", def)
		if err != nil {
			return err
		}
		p.Userdata = nil
		ok := true
		for _, n := range splitList(answer) {
			if n == "none" {
				continue
			}
			if _, known := userdata[n]; !known {
				ui.Printf("%s is not in userdata.yaml; ump config userdata create %s makes it.\n", n, n)
				ok = false
				break
			}
			p.Userdata = append(p.Userdata, profile.UserdataRef{Name: n})
		}
		if ok {
			break
		}
	}
	rename, err := ui.Confirm("Uniform interface names (ext01, int01, ... instead of the kernel's)?", true)
	if err != nil {
		return err
	}
	p.Network.Rename = &rename
	// provisioning is by push from the infrastructure repository; the
	// section is kept for ansible-pull
	p.Provision = profile.Provision{Type: "none"}
	if p.Description, err = ui.Line("Description (one line)", ""); err != nil {
		return err
	}

	// check it the way an installation would, without resolving keys
	if _, err := p.Resolve(layouts, firewalls, users, userdata, checkKeys); err != nil {
		return err
	}
	text, err := renderProfile(p, a.now().UTC().Format("2006-01-02"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(profilePath(name), []byte(text), 0o600); err != nil {
		return err
	}
	ui.Printf("\n%s", text)
	ui.Printf("Saved %s. Use it with ump server reinstall NAME --profile %s, or pin it with ump server set NAME --profile %s; ump config os edit %s refines it.\n", config.AbbreviateHome(profilePath(name)), name, name, name)
	open, err := ui.Confirm("Open it in your editor now?", false)
	if err != nil || !open {
		return err
	}
	return a.editProfile(cmd, name)
}

// chooseNamed offers the entries of a named set, with their descriptions.
func chooseNamed[V any](ui *prompter, label string, set map[string]V, def string, describe func(V) string) (string, error) {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]string, len(names))
	d := 0
	for i, n := range names {
		items[i] = n
		if desc := describe(set[n]); desc != "" {
			items[i] += "  (" + desc + ")"
		}
		if n == def {
			d = i
		}
	}
	i, err := ui.Choose(label, items, d)
	if err != nil {
		return "", err
	}
	return names[i], nil
}

// defineUser asks what a new user is and adds it to users.yaml; nil
// when the name is not valid (the caller asks again).
func (a *app) defineUser(ui *prompter, name string) (*profile.User, error) {
	u := &profile.User{Name: name}
	if err := (&profile.User{Name: name, Keys: []string{"ump"}}).Validate(); err != nil {
		ui.Printf("%v\n", err)
		return nil, nil
	}
	ui.Printf("%s is not in users.yaml yet; defining it now.\n", name)
	var err error
	if u.Sudo, err = ui.Confirm(name+": sudo without a password (an administrator)?", true); err != nil {
		return nil, err
	}
	if u.Shell, err = ui.Line("Shell of "+name+" (the image has /bin/bash, with colors and completion; another shell needs its package)", "/bin/bash"); err != nil {
		return nil, err
	}
	keys, err := ui.Line("Keys of "+name+" (ump: UMP's own; local: your ~/.ssh/id_*.pub; agent: ssh-add -L; a name from ump config key; a file; a key line; comma-separated)", "ump, local")
	if err != nil {
		return nil, err
	}
	u.Keys = splitList(keys)
	if u.Description, err = ui.Line("Description of "+name+" (one line)", ""); err != nil {
		return nil, err
	}
	if err := addUserToFile(u); err != nil {
		return nil, err
	}
	ui.Printf("Added %s to %s.\n", name, config.AbbreviateHome(usersPath()))
	return u, nil
}

// addUserToFile appends a user to users.yaml (which the first run put
// there), keeping the file's comments.
func addUserToFile(u *profile.User) error {
	d, err := userSet.load()
	if err != nil {
		return err
	}
	entry := *u
	entry.Name = ""
	var n yaml.Node
	if err := n.Encode(entry); err != nil {
		return err
	}
	d.add(u.Name, &n)
	return d.save()
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// renderProfile writes a profile as YAML with a short header.
func renderProfile(p *profile.Profile, date string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Profile %s, made by ump config os create on %s; profiles/README.md\n# describes the fields, ump config os edit %s opens this file.\n", p.Name, date, p.Name)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return "", err
	}
	enc.Close()
	return b.String(), nil
}
