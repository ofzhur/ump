// Package digitalocean integrates DigitalOcean droplets through the
// DigitalOcean API v2 (https://docs.digitalocean.com/reference/api/).
//
// DigitalOcean has no rescue system in its API and sets reverse DNS from
// the droplet's name, so this provider lists, describes, renames and
// powers droplets; ump reaches a droplet for a reinstall over SSH into
// its running system, and when that does not let it in, DigitalOcean
// rebuilds the droplet with a stock image and the SSH keys it was
// created with (StockInstaller), which are usually the user's own.
package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "digitalocean"

// DefaultBaseURL is the DigitalOcean API endpoint.
const DefaultBaseURL = "https://api.digitalocean.com/v2"

const perPage = 200

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindCloud,
	Description:  "DigitalOcean droplets",
	Capabilities: capabilities,
	Fields: []provider.Field{
		{
			Key:      "token",
			Label:    "Personal access token",
			Help:     "Control Panel: API → Tokens → Generate New Token; read is enough for listing, write (or the custom scopes droplet read and update, actions read) for renaming and power",
			Secret:   true,
			Required: true,
		},
		{
			Key:     "url",
			Label:   "API base URL",
			Default: DefaultBaseURL,
			Hidden:  true,
		},
	},
	New: newProvider,
}

var capabilities = []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapPower}

// actionPoll is how often a running action is checked, actionWait how
// long it may run (a rebuild, rebuildWait).
var actionPoll, actionWait, rebuildWait = time.Second, 5 * time.Minute, 15 * time.Minute

// Provider is a configured DigitalOcean account (a team).
type Provider struct {
	account string
	client  *Client
}

func newProvider(account string, settings map[string]string, opts provider.Options) (provider.Provider, error) {
	base := settings["url"]
	if base == "" {
		base = DefaultBaseURL
	}
	return &Provider{
		account: account,
		client: &Client{
			BaseURL:   base,
			Token:     settings["token"],
			HTTP:      opts.Client(),
			UserAgent: opts.UserAgent,
		},
	}, nil
}

// Type implements provider.Provider.
func (p *Provider) Type() string { return TypeName }

// Account implements provider.Provider.
func (p *Provider) Account() string { return p.account }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() []provider.Capability { return capabilities }

func dropletID(m provider.Machine) (int64, error) {
	id, err := strconv.ParseInt(m.ID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a droplet ID", TypeName, m.ID)
	}
	return id, nil
}

func path(id int64) string { return "/droplets/" + strconv.FormatInt(id, 10) }

// List implements provider.Inventory: the droplets with the monthly price
// of their sizes, which the droplet records carry.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	droplets, err := p.client.ListDroplets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Machine, 0, len(droplets))
	for _, d := range droplets {
		out = append(out, p.machine(d))
	}
	return out, nil
}

func (p *Provider) machine(d Droplet) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       strconv.FormatInt(d.ID, 10),
		Name:     d.Name,
		Kind:     provider.KindCloud,
		Product:  d.SizeSlug,
		Status:   strings.ToLower(d.Status),
	}
	if d.Region != nil {
		m.Location = strings.ToLower(d.Region.Slug)
	}
	if d.Networks != nil {
		for _, n := range d.Networks.V4 {
			if n.Type == "public" && m.IPv4 == "" {
				m.IPv4 = n.IPAddress
			}
		}
		for _, n := range d.Networks.V6 {
			if n.Type == "public" && m.IPv6 == "" {
				m.IPv6 = n.IPAddress
			}
		}
	}
	if len(d.Tags) > 0 {
		m.Labels = map[string]string{}
		for _, t := range d.Tags {
			m.Labels[t] = ""
		}
	}
	if d.Size != nil && d.Size.PriceMonthly > 0 {
		m.Price = &provider.Price{Amount: d.Size.PriceMonthly, Currency: "USD", Period: "month", Source: "provider", Note: "list price"}
	}
	return m
}

// PowerState implements provider.Power from the droplet status.
func (p *Provider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	id, err := dropletID(m)
	if err != nil {
		return provider.PowerUnknown, err
	}
	d, err := p.client.GetDroplet(ctx, id)
	if err != nil {
		return provider.PowerUnknown, err
	}
	switch d.Status {
	case "active":
		return provider.PowerOn, nil
	case "off":
		return provider.PowerOff, nil
	}
	return provider.PowerUnknown, nil
}

// Reboot implements provider.Power: reboot (ACPI), or power_cycle for a
// hard reset and a power cycle alike; it returns when the action is done.
func (p *Provider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	if mode == provider.RebootSoft {
		return p.action(ctx, m, map[string]string{"type": "reboot"})
	}
	return p.action(ctx, m, map[string]string{"type": "power_cycle"})
}

// PowerOn implements provider.Power.
func (p *Provider) PowerOn(ctx context.Context, m provider.Machine) error {
	return p.action(ctx, m, map[string]string{"type": "power_on"})
}

// PowerOff implements provider.Power: shutdown (ACPI) or power_off.
func (p *Provider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	if hard {
		return p.action(ctx, m, map[string]string{"type": "power_off"})
	}
	return p.action(ctx, m, map[string]string{"type": "shutdown"})
}

// Rename implements provider.Renamer with the rename action. DigitalOcean
// sets the droplet's reverse DNS from its name when that is a fully
// qualified domain name, so renaming is also how the PTR changes.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	if err := p.action(ctx, m, map[string]string{"type": "rename", "name": name}); err != nil {
		return provider.Machine{}, err
	}
	id, _ := dropletID(m)
	d, err := p.client.GetDroplet(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	return p.machine(*d), nil
}

// action starts a droplet action and waits until it completed.
func (p *Provider) action(ctx context.Context, m provider.Machine, body map[string]string) error {
	id, err := dropletID(m)
	if err != nil {
		return err
	}
	var raw struct {
		Action Action `json:"action"`
	}
	if err := p.client.do(ctx, http.MethodPost, path(id)+"/actions", body, &raw); err != nil {
		return err
	}
	return p.client.WaitAction(ctx, raw.Action)
}

// stockImage picks the image a droplet is rebuilt with: the newest
// Debian, else the newest Ubuntu, among the distribution images.
func (p *Provider) stockImage(ctx context.Context) (Image, error) {
	images, err := p.client.ListImages(ctx)
	if err != nil {
		return Image{}, err
	}
	var best Image
	bestRank := -1.0
	for _, img := range images {
		if img.Status != "" && img.Status != "available" {
			continue
		}
		// debian-13-x64, ubuntu-24-04-x64
		parts := strings.Split(img.Slug, "-")
		if len(parts) < 3 || parts[len(parts)-1] != "x64" {
			continue
		}
		v, err := strconv.ParseFloat(strings.Join(parts[1:len(parts)-1], "."), 64)
		if err != nil {
			continue
		}
		var rank float64
		switch parts[0] {
		case "debian":
			rank = 1000 + v
		case "ubuntu":
			rank = v
		default:
			continue
		}
		if rank > bestRank {
			best, bestRank = img, rank
		}
	}
	if best.Slug == "" {
		return Image{}, &provider.APIError{Provider: TypeName, Message: "no Debian or Ubuntu distribution image to rebuild with"}
	}
	return best, nil
}

// InstallStock implements provider.StockInstaller: DigitalOcean rebuilds
// the droplet with its newest Debian (else Ubuntu) image. Its rebuild
// takes no SSH key: root gets the keys the droplet was created with, so
// ump gets in with whichever of them it holds (yours, or UMP's when the
// droplet was created with it). The call returns when the rebuild is done.
func (p *Provider) InstallStock(ctx context.Context, m provider.Machine, keys []provider.PublicKey) (*provider.StockInstall, error) {
	id, err := dropletID(m)
	if err != nil {
		return nil, err
	}
	img, err := p.stockImage(ctx)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Action Action `json:"action"`
	}
	if err := p.client.do(ctx, http.MethodPost, path(id)+"/actions", map[string]string{"type": "rebuild", "image": img.Slug}, &raw); err != nil {
		return nil, err
	}
	if err := p.client.waitAction(ctx, raw.Action, rebuildWait); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(img.Distribution + " " + img.Name)
	return &provider.StockInstall{Image: name, Address: m.IPv4, Wait: 10 * time.Minute, Keys: "the SSH keys the droplet was created with"}, nil
}

// Describe implements provider.Describer from GET /droplets/{id}.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	id, err := dropletID(m)
	if err != nil {
		return nil, err
	}
	d, err := p.client.GetDroplet(ctx, id)
	if err != nil {
		return nil, err
	}
	det := &provider.Details{Machine: p.machine(*d)}

	server := provider.Section{Name: "Droplet"}
	var image any
	if d.Image != nil {
		image = strings.TrimSpace(d.Image.Distribution + " " + d.Image.Name)
	}
	tags := append([]string{}, d.Tags...)
	sort.Strings(tags)
	features := append([]string{}, d.Features...)
	sort.Strings(features)
	server.Add("created", d.Created).Add("status", d.Status).Add("locked", d.Locked).Add("image", image).
		Add("tags", tags).Add("features", features).Add("vpc", orNil(d.VPCUUID)).Add("volumes", nonNil(d.VolumeIDs))

	size := provider.Section{Name: "Size"}
	size.Add("size", d.SizeSlug).AddLabeled("vcpus", "vCPUs", d.Vcpus).Add("memory", provider.Bytes(d.Memory)<<20).Add("disk", provider.Bytes(d.Disk)<<30)
	if d.Size != nil {
		size.Add("transfer", fmt.Sprintf("%g TB", d.Size.Transfer))
	}
	if det.Machine.Price != nil {
		size.Add("price", det.Machine.Price.String())
	}

	location := provider.Section{Name: "Location"}
	if d.Region != nil {
		location.Add("region", d.Region.Slug).Add("name", d.Region.Name)
	}

	network := provider.Section{Name: "Network"}
	var public, private, v6 []string
	if d.Networks != nil {
		for _, n := range d.Networks.V4 {
			s := n.IPAddress + " (netmask " + n.Netmask + ", gateway " + n.Gateway + ")"
			if n.Type == "public" {
				public = append(public, s)
			} else {
				private = append(private, s)
			}
		}
		for _, n := range d.Networks.V6 {
			v6 = append(v6, fmt.Sprintf("%s/%d (gateway %s)", n.IPAddress, n.Netmask, n.Gateway))
		}
	}
	network.AddLabeled("ipv4_public", "IPv4 public", nonNil(public)).AddLabeled("ipv4_private", "IPv4 private", nonNil(private)).
		Add("ipv6", nonNil(v6)).AddLabeled("reverse_dns", "Reverse DNS", "set by DigitalOcean from the droplet name when it is a fully qualified domain name")

	det.Sections = append(det.Sections, server, size, location, network)
	return det, nil
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// Droplet is a droplet as returned by GET /droplets.
type Droplet struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Memory   int64  `json:"memory"`
	Vcpus    int    `json:"vcpus"`
	Disk     int64  `json:"disk"`
	Locked   bool   `json:"locked"`
	Status   string `json:"status"`
	Created  string `json:"created_at"`
	SizeSlug string `json:"size_slug"`
	Region   *struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"region"`
	Image *struct {
		Name         string `json:"name"`
		Distribution string `json:"distribution"`
		Slug         string `json:"slug"`
	} `json:"image"`
	Size *struct {
		Slug         string  `json:"slug"`
		PriceMonthly float64 `json:"price_monthly"`
		Transfer     float64 `json:"transfer"`
	} `json:"size"`
	Networks *struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Netmask   string `json:"netmask"`
			Gateway   string `json:"gateway"`
			Type      string `json:"type"`
		} `json:"v4"`
		V6 []struct {
			IPAddress string `json:"ip_address"`
			Netmask   int    `json:"netmask"`
			Gateway   string `json:"gateway"`
			Type      string `json:"type"`
		} `json:"v6"`
	} `json:"networks"`
	Tags      []string `json:"tags"`
	Features  []string `json:"features"`
	VolumeIDs []string `json:"volume_ids"`
	VPCUUID   string   `json:"vpc_uuid"`
}

// Image is a distribution image a droplet can be rebuilt with.
type Image struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Distribution string `json:"distribution"`
	Status       string `json:"status"`
}

// Action is an asynchronous DigitalOcean operation.
type Action struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// Client is a minimal DigitalOcean API v2 client.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// ListDroplets returns every droplet, following the pages.
func (c *Client) ListDroplets(ctx context.Context) ([]Droplet, error) {
	var out []Droplet
	for page := 1; ; page++ {
		var resp struct {
			Droplets []Droplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}
		if err := c.do(ctx, http.MethodGet, "/droplets?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Droplets...)
		if resp.Links.Pages.Next == "" || len(resp.Droplets) == 0 {
			return out, nil
		}
	}
}

// GetDroplet returns one droplet.
func (c *Client) GetDroplet(ctx context.Context, id int64) (*Droplet, error) {
	var raw struct {
		Droplet Droplet `json:"droplet"`
	}
	if err := c.do(ctx, http.MethodGet, path(id), nil, &raw); err != nil {
		return nil, err
	}
	return &raw.Droplet, nil
}

// ListImages returns the distribution images.
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out []Image
	for page := 1; ; page++ {
		var resp struct {
			Images []Image `json:"images"`
			Links  struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		q := url.Values{"type": {"distribution"}, "page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}
		if err := c.do(ctx, http.MethodGet, "/images?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Images...)
		if resp.Links.Pages.Next == "" || len(resp.Images) == 0 {
			return out, nil
		}
	}
}

// WaitAction polls an action until it completed or errored.
func (c *Client) WaitAction(ctx context.Context, a Action) error {
	return c.waitAction(ctx, a, actionWait)
}

func (c *Client) waitAction(ctx context.Context, a Action, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for a.Status == "in-progress" {
		if time.Now().After(deadline) {
			return &provider.APIError{Provider: TypeName, Message: fmt.Sprintf("action %s still in progress after %s", a.Type, limit)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(actionPoll):
		}
		var raw struct {
			Action Action `json:"action"`
		}
		if err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(a.ID, 10), nil, &raw); err != nil {
			return err
		}
		a = raw.Action
	}
	if a.Status == "errored" {
		return &provider.APIError{Provider: TypeName, Message: "action " + a.Type + " failed"}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return &provider.APIError{Provider: TypeName, Message: "encoding request: " + err.Error()}
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+p, body)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "reading response: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return parseError(resp.StatusCode, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "unexpected response: " + err.Error()}
	}
	return nil
}

// parseError turns DigitalOcean's {"id", "message"} into an APIError.
func parseError(status int, body []byte) error {
	apiErr := &provider.APIError{Provider: TypeName, Status: status}
	var e struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		apiErr.Code = e.ID
		apiErr.Message = e.Message
	}
	switch {
	case status == http.StatusUnauthorized:
		apiErr.Message = "authentication failed: " + nonEmpty(apiErr.Message, "unable to authenticate") + " (check the personal access token)"
	case status == http.StatusForbidden:
		apiErr.Message = nonEmpty(apiErr.Message, "forbidden") + " (a read-only token cannot rename or power droplets)"
	case status == http.StatusTooManyRequests:
		apiErr.Message = "rate limit exceeded"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
