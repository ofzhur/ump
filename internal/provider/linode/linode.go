// Package linode integrates Linode instances (Akamai Cloud Computing)
// through the Linode API v4 (https://techdocs.akamai.com/linode-api/).
//
// It lists, describes, renames, powers and sets reverse DNS. Its rescue
// system is Linode's rescue mode, Finnix, set up for SSH with UMP's key
// through the Lish console (rescue.go), so an instance is reached
// without touching its disks; the rebuild with a stock image and UMP's
// key (StockInstaller) remains for a caller that asks for it.
package linode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "linode"

// DefaultBaseURL is the Linode API endpoint.
const DefaultBaseURL = "https://api.linode.com/v4"

const pageSize = 500

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindCloud,
	Description:  "Linode (Akamai Cloud Computing) instances",
	Capabilities: capabilities,
	Fields: []provider.Field{
		{
			Key:      "token",
			Label:    "Personal access token",
			Help:     "Cloud Manager: your profile → API Tokens → Create a Personal Access Token, with Linodes and IPs read/write and Images read only (the rescue system also adds UMP's key to the profile's Lish keys)",
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

var capabilities = []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapReverseDNS, provider.CapPower, provider.CapRescue}

// stockWait is how long a rebuilt instance may take to let UMP in.
var stockWait = 15 * time.Minute

// Provider is a configured Linode account.
type Provider struct {
	account string
	client  *Client

	mu      sync.Mutex
	regions map[string]Region
	// console opens an instance's console; nil means through Lish.
	console consoleDialer
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

func instanceID(m provider.Machine) (int64, error) {
	id, err := strconv.ParseInt(m.ID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a Linode ID", TypeName, m.ID)
	}
	return id, nil
}

func path(id int64) string { return "/linode/instances/" + strconv.FormatInt(id, 10) }

// List implements provider.Inventory: the instances, with the monthly
// price of their types in their regions.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	instances, err := list[Instance](ctx, p.client, "/linode/instances")
	if err != nil {
		return nil, err
	}
	var types map[string]LinodeType
	if len(instances) > 0 {
		all, err := list[LinodeType](ctx, p.client, "/linode/types")
		if err != nil {
			return nil, err
		}
		types = map[string]LinodeType{}
		for _, t := range all {
			types[t.ID] = t
		}
	}
	out := make([]provider.Machine, 0, len(instances))
	for _, in := range instances {
		m := p.machine(in)
		m.Price = price(types, in.Type, in.Region)
		out = append(out, m)
	}
	return out, nil
}

func (p *Provider) machine(in Instance) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       strconv.FormatInt(in.ID, 10),
		Name:     in.Label,
		Kind:     provider.KindCloud,
		Arch:     "amd64", // Linode has x86 instances only
		IPv6:     strings.TrimSuffix(in.IPv6, "/128"),
		Location: strings.ToLower(in.Region),
		Product:  in.Type,
		Status:   strings.ToLower(in.Status),
	}
	// the first public IPv4; Linode lists private ones (192.168.128.0/17)
	// in the same field
	for _, a := range in.IPv4 {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() && !ip.IsPrivate() {
			m.IPv4 = a
			break
		}
	}
	if len(in.Tags) > 0 {
		m.Labels = map[string]string{}
		for _, t := range in.Tags {
			m.Labels[t] = ""
		}
	}
	return m
}

// price is the type's monthly price in the region (some regions cost
// more), else its base price.
func price(types map[string]LinodeType, typ, region string) *provider.Price {
	t, ok := types[typ]
	if !ok {
		return nil
	}
	amount := 0.0
	if t.Price != nil {
		amount = t.Price.Monthly
	}
	for _, rp := range t.RegionPrices {
		if strings.EqualFold(rp.ID, region) {
			amount = rp.Monthly
		}
	}
	if amount <= 0 {
		return nil
	}
	return &provider.Price{Amount: amount, Currency: "USD", Period: "month", Source: "provider", Note: "list price"}
}

// PowerState implements provider.Power from the instance status.
func (p *Provider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	id, err := instanceID(m)
	if err != nil {
		return provider.PowerUnknown, err
	}
	var in Instance
	if err := p.client.do(ctx, http.MethodGet, path(id), nil, &in); err != nil {
		return provider.PowerUnknown, err
	}
	switch in.Status {
	case "running":
		return provider.PowerOn, nil
	case "offline", "stopped":
		return provider.PowerOff, nil
	}
	return provider.PowerUnknown, nil
}

// Reboot implements provider.Power. Linode has one reboot (it shuts the
// instance down and boots it); a power cycle is a shutdown and a boot.
func (p *Provider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	id, err := instanceID(m)
	if err != nil {
		return err
	}
	if mode == provider.RebootPowerCycle {
		if err := p.client.do(ctx, http.MethodPost, path(id)+"/shutdown", struct{}{}, nil); err != nil {
			return err
		}
		return p.client.do(ctx, http.MethodPost, path(id)+"/boot", struct{}{}, nil)
	}
	return p.client.do(ctx, http.MethodPost, path(id)+"/reboot", struct{}{}, nil)
}

// PowerOn implements provider.Power.
func (p *Provider) PowerOn(ctx context.Context, m provider.Machine) error {
	id, err := instanceID(m)
	if err != nil {
		return err
	}
	return p.client.do(ctx, http.MethodPost, path(id)+"/boot", struct{}{}, nil)
}

// PowerOff implements provider.Power. Linode shuts an instance down with
// an ACPI request and powers it off when it does not stop in time; there
// is no separate hard power off, so both modes shut down.
func (p *Provider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	id, err := instanceID(m)
	if err != nil {
		return err
	}
	return p.client.do(ctx, http.MethodPost, path(id)+"/shutdown", struct{}{}, nil)
}

// Rename implements provider.Renamer: the instance's label, which is the
// name ump shows (3 to 64 letters, digits, '-', '_' and '.', starting
// with a letter). The hostname inside the system is not touched.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	id, err := instanceID(m)
	if err != nil {
		return provider.Machine{}, err
	}
	var in Instance
	if err := p.client.do(ctx, http.MethodPut, path(id), map[string]string{"label": name}, &in); err != nil {
		return provider.Machine{}, err
	}
	out := p.machine(in)
	out.Price = m.Price
	return out, nil
}

// PTRs implements provider.ReverseDNS: the public IPv4 addresses and the
// SLAAC IPv6 address with their reverse names.
func (p *Provider) PTRs(ctx context.Context, m provider.Machine) ([]provider.PTR, error) {
	id, err := instanceID(m)
	if err != nil {
		return nil, err
	}
	ips, err := p.client.IPs(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []provider.PTR
	if ips.IPv4 != nil {
		for _, a := range ips.IPv4.Public {
			out = append(out, provider.PTR{IP: a.Address, Name: a.RDNS})
		}
	}
	if ips.IPv6 != nil && ips.IPv6.SLAAC != nil {
		out = append(out, provider.PTR{IP: ips.IPv6.SLAAC.Address, Name: ips.IPv6.SLAAC.RDNS})
	}
	return out, nil
}

// SetPTR implements provider.ReverseDNS with PUT /networking/ips/{ip}.
// An empty name resets the entry to Linode's default.
func (p *Provider) SetPTR(ctx context.Context, m provider.Machine, ip netip.Addr, name string) error {
	id, err := instanceID(m)
	if err != nil {
		return err
	}
	ips, err := p.client.IPs(ctx, id)
	if err != nil {
		return err
	}
	var singles, prefixes []string
	if ips.IPv4 != nil {
		for _, a := range ips.IPv4.Public {
			singles = append(singles, a.Address)
		}
	}
	if ips.IPv6 != nil {
		if ips.IPv6.SLAAC != nil {
			singles = append(singles, ips.IPv6.SLAAC.Address)
		}
		for _, r := range ips.IPv6.Global {
			prefixes = append(prefixes, r.Range+"/"+strconv.Itoa(r.Prefix))
		}
	}
	if !provider.Contains(ip, singles, prefixes) {
		return fmt.Errorf("%s: address %s does not belong to instance %s", TypeName, ip, m.ID)
	}
	var rdns *string
	if name != "" {
		rdns = &name
	}
	body := struct {
		RDNS *string `json:"rdns"`
	}{RDNS: rdns}
	return p.client.do(ctx, http.MethodPut, "/networking/ips/"+url.PathEscape(ip.String()), body, nil)
}

// stockImage picks the image an instance is rebuilt with: the newest
// Debian, else the newest Ubuntu, among the public images that are not
// deprecated.
func (p *Provider) stockImage(ctx context.Context) (Image, error) {
	images, err := list[Image](ctx, p.client, "/images")
	if err != nil {
		return Image{}, err
	}
	var best Image
	bestRank := -1.0
	for _, img := range images {
		if !img.IsPublic || img.Deprecated || (img.Status != "" && img.Status != "available") {
			continue
		}
		f := strings.Fields(img.Label)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		var rank float64
		switch f[0] {
		case "Debian":
			rank = 1000 + v
		case "Ubuntu":
			rank = v
		default:
			continue
		}
		if rank > bestRank {
			best, bestRank = img, rank
		}
	}
	if best.ID == "" {
		return Image{}, &provider.APIError{Provider: TypeName, Message: "no public Debian or Ubuntu image to rebuild with"}
	}
	return best, nil
}

// InstallStock implements provider.StockInstaller: UMP's keys are added
// to the profile's SSH keys (for the next deploys; best effort, a token
// without that permission skips it), and Linode rebuilds the instance
// with its newest Debian (else Ubuntu) image, the keys for root and a
// random root password nobody needs, and boots it.
func (p *Provider) InstallStock(ctx context.Context, m provider.Machine, keys []provider.PublicKey) (*provider.StockInstall, error) {
	id, err := instanceID(m)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, &provider.APIError{Provider: TypeName, Message: "a stock installation needs a key for root"}
	}
	for _, k := range keys {
		_ = p.client.EnsureSSHKey(ctx, k)
	}
	img, err := p.stockImage(ctx)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, k := range keys {
		lines = append(lines, strings.TrimSpace(k.Data))
	}
	pass, err := rootPassword()
	if err != nil {
		return nil, err
	}
	body := map[string]any{"image": img.ID, "root_pass": pass, "authorized_keys": lines, "booted": true}
	if err := p.client.do(ctx, http.MethodPost, path(id)+"/rebuild", body, nil); err != nil {
		return nil, err
	}
	return &provider.StockInstall{Image: img.Label, Address: m.IPv4, Wait: stockWait}, nil
}

// rootPassword is a random password that meets Linode's strength rules:
// 32 characters of letters and digits, with the classes forced in.
func rootPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	s = strings.NewReplacer("-", "x", "_", "y").Replace(s)
	return "Ump7" + s, nil
}

// region returns a region by its ID, from GET /regions read once.
func (p *Provider) region(ctx context.Context, id string) (Region, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.regions == nil {
		all, err := list[Region](ctx, p.client, "/regions")
		if err != nil {
			return Region{}, err
		}
		p.regions = map[string]Region{}
		for _, r := range all {
			p.regions[r.ID] = r
		}
	}
	return p.regions[id], nil
}

// Describe implements provider.Describer from GET /linode/instances/{id}
// and its addresses.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	id, err := instanceID(m)
	if err != nil {
		return nil, err
	}
	var in Instance
	if err := p.client.do(ctx, http.MethodGet, path(id), nil, &in); err != nil {
		return nil, err
	}
	d := &provider.Details{Machine: p.machine(in)}
	d.Machine.Price = m.Price

	server := provider.Section{Name: "Server"}
	tags := append([]string{}, in.Tags...)
	sort.Strings(tags)
	server.Add("created", in.Created).Add("updated", in.Updated).Add("status", in.Status).Add("image", orNil(in.Image)).
		Add("hypervisor", orNil(in.Hypervisor)).Add("group", orNil(in.Group)).Add("tags", tags).
		Add("watchdog", in.WatchdogEnabled).Add("backups", in.Backups != nil && in.Backups.Enabled)

	typ := provider.Section{Name: "Type"}
	typ.Add("type", in.Type)
	if s := in.Specs; s != nil {
		typ.AddLabeled("vcpus", "vCPUs", s.VCPUs).Add("memory", provider.Bytes(s.Memory)<<20).Add("disk", provider.Bytes(s.Disk)<<20).
			Add("transfer", provider.Bytes(s.Transfer)<<30)
	}
	if m.Price != nil {
		typ.Add("price", m.Price.String())
	}

	location := provider.Section{Name: "Location"}
	location.Add("region", in.Region)
	if r, err := p.region(ctx, in.Region); err == nil && r.Label != "" {
		location.Add("label", r.Label).Add("country", strings.ToUpper(r.Country))
	}

	network := provider.Section{Name: "Network"}
	if ips, err := p.client.IPs(ctx, id); err == nil {
		var public, private, ranges []string
		if ips.IPv4 != nil {
			for _, a := range ips.IPv4.Public {
				public = append(public, a.Address+" "+a.RDNS)
			}
			for _, a := range ips.IPv4.Private {
				private = append(private, a.Address)
			}
		}
		var slaac any
		if ips.IPv6 != nil {
			if ips.IPv6.SLAAC != nil {
				slaac = ips.IPv6.SLAAC.Address + " " + ips.IPv6.SLAAC.RDNS
			}
			for _, r := range ips.IPv6.Global {
				ranges = append(ranges, r.Range+"/"+strconv.Itoa(r.Prefix))
			}
		}
		network.AddLabeled("ipv4_public", "IPv4 public", nonNil(public)).AddLabeled("ipv4_private", "IPv4 private", nonNil(private)).
			AddLabeled("ipv6_slaac", "IPv6 SLAAC", slaac).AddLabeled("ipv6_ranges", "IPv6 ranges", nonNil(ranges))
	} else {
		d.Sections = append(d.Sections, server, typ, location, provider.ErrorSection("Network", err))
		return d, nil
	}
	d.Sections = append(d.Sections, server, typ, location, network)
	return d, nil
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

// Instance is a Linode as returned by GET /linode/instances.
type Instance struct {
	ID         int64    `json:"id"`
	Label      string   `json:"label"`
	Region     string   `json:"region"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Image      string   `json:"image"`
	Group      string   `json:"group"`
	IPv4       []string `json:"ipv4"`
	IPv6       string   `json:"ipv6"`
	Created    string   `json:"created"`
	Updated    string   `json:"updated"`
	Hypervisor string   `json:"hypervisor"`
	Tags       []string `json:"tags"`
	Specs      *struct {
		Disk     int64 `json:"disk"`
		Memory   int64 `json:"memory"`
		VCPUs    int   `json:"vcpus"`
		Transfer int64 `json:"transfer"`
	} `json:"specs"`
	WatchdogEnabled bool `json:"watchdog_enabled"`
	Backups         *struct {
		Enabled bool `json:"enabled"`
	} `json:"backups"`
}

// LinodeType is an instance type with its prices.
type LinodeType struct {
	ID    string `json:"id"`
	Price *struct {
		Monthly float64 `json:"monthly"`
	} `json:"price"`
	RegionPrices []struct {
		ID      string  `json:"id"`
		Monthly float64 `json:"monthly"`
	} `json:"region_prices"`
}

// Region is a Linode region such as de-fra-2.
type Region struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Country string `json:"country"`
}

// Image is an image an instance can be rebuilt with.
type Image struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Vendor     string `json:"vendor"`
	IsPublic   bool   `json:"is_public"`
	Deprecated bool   `json:"deprecated"`
	Status     string `json:"status"`
}

// IP is an address of an instance.
type IP struct {
	Address string `json:"address"`
	RDNS    string `json:"rdns"`
}

// IPs are an instance's addresses (GET /linode/instances/{id}/ips).
type IPs struct {
	IPv4 *struct {
		Public  []IP `json:"public"`
		Private []IP `json:"private"`
	} `json:"ipv4"`
	IPv6 *struct {
		SLAAC  *IP `json:"slaac"`
		Global []struct {
			Range  string `json:"range"`
			Prefix int    `json:"prefix"`
		} `json:"global"`
	} `json:"ipv6"`
}

// SSHKey is a key of the profile.
type SSHKey struct {
	ID     int64  `json:"id"`
	Label  string `json:"label"`
	SSHKey string `json:"ssh_key"`
}

// Client is a minimal Linode API v4 client.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// list follows the page pagination of a list endpoint.
func list[T any](ctx context.Context, c *Client, p string) ([]T, error) {
	var out []T
	for page := 1; ; page++ {
		var resp struct {
			Data  []T `json:"data"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
		if err := c.do(ctx, http.MethodGet, p+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if page >= resp.Pages {
			return out, nil
		}
	}
}

// IPs returns an instance's addresses.
func (c *Client) IPs(ctx context.Context, id int64) (*IPs, error) {
	var ips IPs
	if err := c.do(ctx, http.MethodGet, path(id)+"/ips", nil, &ips); err != nil {
		return nil, err
	}
	return &ips, nil
}

// EnsureSSHKey adds the key to the profile's SSH keys unless a key with
// the same type and data is there already.
func (c *Client) EnsureSSHKey(ctx context.Context, k provider.PublicKey) error {
	have, err := list[SSHKey](ctx, c, "/profile/sshkeys")
	if err != nil {
		return err
	}
	for _, h := range have {
		if keyData(h.SSHKey) == keyData(k.Data) {
			return nil
		}
	}
	label := k.Name
	if label == "" {
		label = "ump"
	}
	if len(label) > 64 {
		label = label[:64]
	}
	return c.do(ctx, http.MethodPost, "/profile/sshkeys", map[string]string{"label": label, "ssh_key": strings.TrimSpace(k.Data)}, nil)
}

func keyData(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return strings.TrimSpace(line)
	}
	return f[0] + " " + f[1]
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

// parseError turns Linode's {"errors": [{"reason", "field"}]} into an
// APIError that keeps the reasons.
func parseError(status int, body []byte) error {
	apiErr := &provider.APIError{Provider: TypeName, Status: status}
	var e struct {
		Errors []struct {
			Reason string `json:"reason"`
			Field  string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil {
		var msgs []string
		for _, r := range e.Errors {
			if r.Field != "" {
				msgs = append(msgs, r.Field+": "+r.Reason)
			} else if r.Reason != "" {
				msgs = append(msgs, r.Reason)
			}
		}
		apiErr.Message = strings.Join(msgs, "; ")
	}
	switch {
	case status == http.StatusUnauthorized:
		apiErr.Message = "authentication failed: " + nonEmpty(apiErr.Message, "invalid token") + " (check the personal access token)"
	case status == http.StatusForbidden:
		apiErr.Message = nonEmpty(apiErr.Message, "forbidden") + " (the token's scopes may not allow this: Linodes and IPs read/write, Images read only)"
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
