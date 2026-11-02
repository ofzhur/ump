package pxe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// SiteFormat is the format of site.json this ump writes; a server
// refuses a newer one, which an ump newer than itself rendered.
const SiteFormat = 1

// SiteDoc is what the PXE servers of a site serve, rendered by the CLI
// from pxe.yaml (Render) into .pxe/<site>/site.json: the machines with
// how they boot when nothing is armed, root's keys in their rescue
// system, the rescue image and the policy for MACs without an entry.
// Hash is the rendering's, which a server reports back in its status.
type SiteDoc struct {
	Format   int           `json:"format"`
	Site     string        `json:"site"`
	Hash     string        `json:"hash"`
	Image    string        `json:"image"`
	Unknown  string        `json:"unknown"`
	Machines []SiteMachine `json:"machines"`
}

// SiteMachine is a machine as a PXE server knows it.
type SiteMachine struct {
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	MACs     []string `json:"macs"`
	// Mode is the standing mode: local, rescue, or install (which a
	// server answers as local: the arm carries an installation).
	Mode     string `json:"mode"`
	Arch     string `json:"arch,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Console  string `json:"console,omitempty"`
	// Keys are root's keys in its rescue system, as key lines: UMP's,
	// staging.keys and its rescue-keys.
	Keys []string `json:"keys"`
}

// Render renders site.json for a site of a valid pxe.yaml: image is the
// rescue image's base URL (staging.image), and keys resolves a machine's
// rescue keys to key lines (UMP's own first).
func Render(c *Config, site, image string, keys func(m *Machine) ([]string, error)) (*SiteDoc, error) {
	d := &SiteDoc{Format: SiteFormat, Site: site, Image: image, Unknown: UnknownIgnore, Machines: []SiteMachine{}}
	if c.Unknown != "" {
		d.Unknown = c.Unknown
	}
	if s := c.Sites[site]; s != nil && s.Unknown != "" {
		d.Unknown = s.Unknown
	}
	for _, m := range c.Merged() {
		if m.Site != site {
			continue
		}
		k, err := keys(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		d.Machines = append(d.Machines, SiteMachine{Name: m.Name, Hostname: m.Hostname, MACs: m.MACs, Mode: m.Mode,
			Arch: m.Arch, Firmware: m.Firmware, Console: m.Console, Keys: k})
	}
	slices.SortFunc(d.Machines, func(a, b SiteMachine) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	d.Hash = d.hash()
	return d, nil
}

// hash is the rendering's hash: of its JSON without the hash.
func (d *SiteDoc) hash() string {
	c := *d
	c.Hash = ""
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// JSON is site.json.
func (d *SiteDoc) JSON() []byte {
	data, _ := json.MarshalIndent(d, "", "  ")
	return append(data, '\n')
}

// ParseSite reads site.json, refusing a format newer than this ump's
// and a rendering whose hash does not match.
func ParseSite(data []byte) (*SiteDoc, error) {
	var d SiteDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("site.json: %w", err)
	}
	switch {
	case d.Format > SiteFormat:
		return nil, fmt.Errorf("site.json of site %s has format %d, newer than this ump's (%d): update ump", d.Site, d.Format, SiteFormat)
	case d.Format < 1:
		return nil, fmt.Errorf("site.json of site %s has no format", d.Site)
	case d.Hash != d.hash():
		return nil, fmt.Errorf("site.json of site %s was changed by hand (its hash does not match); ump config pxe apply renders it again", d.Site)
	}
	for _, m := range d.Machines {
		if !validHostname(m.Name) {
			return nil, fmt.Errorf("site.json of site %s has a machine named %q, which is no name of pxe.yaml", d.Site, m.Name)
		}
	}
	return &d, nil
}

// Lookup is the machine with the MAC (normalized), nil when none.
func (d *SiteDoc) Lookup(mac string) *SiteMachine {
	for i := range d.Machines {
		if slices.Contains(d.Machines[i].MACs, mac) {
			return &d.Machines[i]
		}
	}
	return nil
}

// Machine is the machine of that name, nil when none.
func (d *SiteDoc) Machine(name string) *SiteMachine {
	for i := range d.Machines {
		if d.Machines[i].Name == name {
			return &d.Machines[i]
		}
	}
	return nil
}
