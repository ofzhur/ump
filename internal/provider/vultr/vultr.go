// Package vultr integrates Vultr cloud instances and bare metal servers
// through the Vultr API v2 (https://www.vultr.com/api/).
//
// Vultr has no rescue system in its API, so this provider lists,
// describes, renames, powers and sets reverse DNS; ump reaches a Vultr
// machine for a reinstall over SSH into its running system.
package vultr

import (
	"bytes"
	"context"
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

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "vultr"

// DefaultBaseURL is the Vultr API endpoint.
const DefaultBaseURL = "https://api.vultr.com/v2"

const perPage = 500

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindMixed,
	Description:  "Vultr cloud instances and bare metal servers",
	Capabilities: capabilities,
	Fields: []provider.Field{
		{
			Key:      "api_key",
			Label:    "API key",
			Help:     "the personal API key (customer portal: Account → API → Enable API); its access control must allow this machine's address, or all IPv4 and IPv6",
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

var capabilities = []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapReverseDNS, provider.CapPower}

// Provider is a configured Vultr account.
type Provider struct {
	account string
	client  *Client

	mu      sync.Mutex
	regions map[string]Region
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
			Key:       settings["api_key"],
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

// List implements provider.Inventory: the instances and the bare metal
// servers, with the list price of their plans.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	instances, err := p.client.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	metals, err := p.client.ListBareMetals(ctx)
	if err != nil {
		return nil, err
	}
	prices := map[string]float64{}
	if len(instances) > 0 {
		plans, err := p.client.ListPlans(ctx)
		if err != nil {
			return nil, err
		}
		for _, pl := range plans {
			prices[pl.ID] = pl.MonthlyCost
		}
	}
	if len(metals) > 0 {
		plans, err := p.client.ListMetalPlans(ctx)
		if err != nil {
			return nil, err
		}
		for _, pl := range plans {
			prices[pl.ID] = pl.MonthlyCost
		}
	}
	out := make([]provider.Machine, 0, len(instances)+len(metals))
	for _, in := range instances {
		out = append(out, p.instanceMachine(in, prices))
	}
	for _, bm := range metals {
		out = append(out, p.metalMachine(bm, prices))
	}
	return out, nil
}

func (p *Provider) instanceMachine(in Instance, prices map[string]float64) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       in.ID,
		Name:     name(in.Label, in.Hostname, in.ID),
		Kind:     provider.KindCloud,
		IPv4:     addr(in.MainIP),
		IPv6:     ipv6(in.V6Network, in.V6NetworkSize, in.V6MainIP),
		Location: strings.ToLower(in.Region),
		Product:  in.Plan,
		Status:   instanceStatus(in),
		Labels:   tags(in.Tags),
	}
	m.Price = price(prices, in.Plan)
	return m
}

func (p *Provider) metalMachine(bm BareMetal, prices map[string]float64) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       bm.ID,
		Name:     name(bm.Label, "", bm.ID),
		Kind:     provider.KindBareMetal,
		IPv4:     addr(bm.MainIP),
		IPv6:     ipv6(bm.V6Network, bm.V6NetworkSize, bm.V6MainIP),
		Location: strings.ToLower(bm.Region),
		Product:  bm.Plan,
		Status:   strings.ToLower(bm.Status),
		Labels:   tags(bm.Tags),
	}
	m.Price = price(prices, bm.Plan)
	return m
}

// instanceStatus is the power state of an active instance (running,
// stopped), else what Vultr says (pending, suspended, resizing).
func instanceStatus(in Instance) string {
	if strings.EqualFold(in.Status, "active") && in.PowerStatus != "" {
		return strings.ToLower(in.PowerStatus)
	}
	return strings.ToLower(in.Status)
}

func name(label, hostname, id string) string {
	switch {
	case label != "":
		return label
	case hostname != "":
		return hostname
	}
	return id
}

// addr drops the placeholders Vultr reports for missing addresses.
func addr(s string) string {
	if s == "" || s == "0.0.0.0" || s == "::" {
		return ""
	}
	return s
}

func ipv6(network string, size int, main string) string {
	if addr(network) != "" && size > 0 {
		return network + "/" + strconv.Itoa(size)
	}
	return addr(main)
}

func tags(list []string) map[string]string {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]string, len(list))
	for _, t := range list {
		out[t] = ""
	}
	return out
}

func price(prices map[string]float64, plan string) *provider.Price {
	v, ok := prices[plan]
	if !ok || v <= 0 {
		return nil
	}
	return &provider.Price{Amount: v, Currency: "USD", Period: "month", Source: "provider", Note: "list price"}
}

// the paths of a machine: /instances/{id} or /bare-metals/{id}
func base(m provider.Machine) string {
	if m.Kind == provider.KindBareMetal {
		return "/bare-metals/" + url.PathEscape(m.ID)
	}
	return "/instances/" + url.PathEscape(m.ID)
}

// PowerState implements provider.Power: an instance's power status; a
// bare metal server's state is not reported by the API.
func (p *Provider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	if m.Kind == provider.KindBareMetal {
		return provider.PowerUnknown, nil
	}
	in, err := p.client.GetInstance(ctx, m.ID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	switch strings.ToLower(in.PowerStatus) {
	case "running":
		return provider.PowerOn, nil
	case "stopped":
		return provider.PowerOff, nil
	}
	return provider.PowerUnknown, nil
}

// Reboot implements provider.Power. Vultr has one reboot; a power cycle
// is a halt followed by a start.
func (p *Provider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	if mode == provider.RebootPowerCycle {
		if err := p.client.do(ctx, http.MethodPost, base(m)+"/halt", nil, nil); err != nil {
			return err
		}
		return p.client.do(ctx, http.MethodPost, base(m)+"/start", nil, nil)
	}
	return p.client.do(ctx, http.MethodPost, base(m)+"/reboot", nil, nil)
}

// PowerOn implements provider.Power.
func (p *Provider) PowerOn(ctx context.Context, m provider.Machine) error {
	return p.client.do(ctx, http.MethodPost, base(m)+"/start", nil, nil)
}

// PowerOff implements provider.Power. Vultr only halts (a hard power
// off); there is no ACPI shutdown in the API, so both modes halt.
func (p *Provider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	return p.client.do(ctx, http.MethodPost, base(m)+"/halt", nil, nil)
}

// Rename implements provider.Renamer: the label, which is the name ump
// shows. The hostname inside the system is not touched.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, newName string) (provider.Machine, error) {
	body := map[string]string{"label": newName}
	if err := p.client.do(ctx, http.MethodPatch, base(m), body, nil); err != nil {
		return provider.Machine{}, err
	}
	if m.Kind == provider.KindBareMetal {
		bm, err := p.client.GetBareMetal(ctx, m.ID)
		if err != nil {
			return provider.Machine{}, err
		}
		out := p.metalMachine(*bm, nil)
		out.Price = m.Price
		return out, nil
	}
	in, err := p.client.GetInstance(ctx, m.ID)
	if err != nil {
		return provider.Machine{}, err
	}
	out := p.instanceMachine(*in, nil)
	out.Price = m.Price
	return out, nil
}

// PTRs implements provider.ReverseDNS for instances: every IPv4 with its
// reverse name, and the IPv6 entries that were set.
func (p *Provider) PTRs(ctx context.Context, m provider.Machine) ([]provider.PTR, error) {
	if m.Kind == provider.KindBareMetal {
		return nil, errBareMetalRDNS
	}
	v4, err := p.client.IPv4s(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	var out []provider.PTR
	for _, a := range v4 {
		out = append(out, provider.PTR{IP: a.IP, Name: a.Reverse})
	}
	v6, err := p.client.ReverseIPv6s(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range v6 {
		out = append(out, provider.PTR{IP: r.IP, Name: r.Reverse})
	}
	return out, nil
}

var errBareMetalRDNS = fmt.Errorf("%s: the API sets reverse DNS for instances only; set it for a bare metal server in the customer portal", TypeName)

// SetPTR implements provider.ReverseDNS. An empty name resets an IPv4
// entry to Vultr's default and removes an IPv6 one.
func (p *Provider) SetPTR(ctx context.Context, m provider.Machine, ip netip.Addr, ptr string) error {
	if m.Kind == provider.KindBareMetal {
		return errBareMetalRDNS
	}
	v4, err := p.client.IPv4s(ctx, m.ID)
	if err != nil {
		return err
	}
	var singles, prefixes []string
	for _, a := range v4 {
		singles = append(singles, a.IP)
	}
	if v6, err := p.client.IPv6s(ctx, m.ID); err == nil {
		for _, n := range v6 {
			if n.Network != "" && n.NetworkSize > 0 {
				prefixes = append(prefixes, n.Network+"/"+strconv.Itoa(n.NetworkSize))
			}
		}
	} else {
		return err
	}
	if m.IPv6 != "" {
		prefixes = append(prefixes, m.IPv6)
	}
	if !provider.Contains(ip, singles, prefixes) {
		return fmt.Errorf("%s: address %s does not belong to instance %s", TypeName, ip, m.ID)
	}
	path := base(m)
	switch {
	case ip.Is4() && ptr == "":
		return p.client.do(ctx, http.MethodPost, path+"/ipv4/reverse/default", map[string]string{"ip": ip.String()}, nil)
	case ip.Is4():
		return p.client.do(ctx, http.MethodPost, path+"/ipv4/reverse", map[string]string{"ip": ip.String(), "reverse": ptr}, nil)
	case ptr == "":
		return p.client.do(ctx, http.MethodDelete, path+"/ipv6/reverse/"+url.PathEscape(ip.String()), nil, nil)
	}
	return p.client.do(ctx, http.MethodPost, path+"/ipv6/reverse", map[string]string{"ip": ip.String(), "reverse": ptr}, nil)
}

// region returns a region by its ID, from GET /regions read once.
func (p *Provider) region(ctx context.Context, id string) (Region, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.regions == nil {
		list, err := p.client.ListRegions(ctx)
		if err != nil {
			return Region{}, err
		}
		p.regions = map[string]Region{}
		for _, r := range list {
			p.regions[r.ID] = r
		}
	}
	return p.regions[id], nil
}

// Describe implements provider.Describer from GET /instances/{id} or
// GET /bare-metals/{id}.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	d := &provider.Details{}
	var regionID string
	server := provider.Section{Name: "Server"}
	plan := provider.Section{Name: "Plan"}
	network := provider.Section{Name: "Network"}
	if m.Kind == provider.KindBareMetal {
		bm, err := p.client.GetBareMetal(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		d.Machine = p.metalMachine(*bm, nil)
		regionID = bm.Region
		server.Add("created", bm.DateCreated).Add("status", bm.Status).Add("os", bm.OS).
			Add("tags", nonNil(bm.Tags)).Add("features", nonNil(bm.Features))
		plan.Add("plan", bm.Plan).Add("cpu_count", bm.CPUCount).Add("ram", bm.RAM).Add("disk", bm.Disk)
		network.Add("ipv4", addrOrNil(bm.MainIP)).Add("netmask", addrOrNil(bm.NetmaskV4)).Add("gateway", addrOrNil(bm.GatewayV4)).
			Add("ipv6", addrOrNil(bm.V6MainIP)).AddLabeled("ipv6_network", "IPv6 network", orNil(ipv6(bm.V6Network, bm.V6NetworkSize, ""))).
			Add("mac", bm.MACAddress)
	} else {
		in, err := p.client.GetInstance(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		d.Machine = p.instanceMachine(*in, nil)
		regionID = in.Region
		server.Add("created", in.DateCreated).Add("status", in.Status).Add("power_status", in.PowerStatus).
			Add("server_status", in.ServerStatus).Add("hostname", in.Hostname).Add("os", in.OS).
			Add("tags", nonNil(in.Tags)).Add("features", nonNil(in.Features))
		plan.Add("plan", in.Plan).AddLabeled("vcpus", "vCPUs", in.VCPUCount).Add("ram", provider.Bytes(in.RAM)<<20).
			Add("disk", provider.Bytes(in.Disk)<<30).AddLabeled("allowed_bandwidth", "Allowed bandwidth", fmt.Sprintf("%d GB", in.AllowedBandwidth))
		network.Add("ipv4", addrOrNil(in.MainIP)).Add("netmask", addrOrNil(in.NetmaskV4)).Add("gateway", addrOrNil(in.GatewayV4)).
			Add("ipv6", addrOrNil(in.V6MainIP)).AddLabeled("ipv6_network", "IPv6 network", orNil(ipv6(in.V6Network, in.V6NetworkSize, ""))).
			Add("internal_ip", orNil(in.InternalIP)).Add("firewall_group", orNil(in.FirewallGroupID))
	}
	d.Machine.Price = m.Price
	if d.Machine.Price != nil {
		plan.Add("price", d.Machine.Price.String())
	}
	location := provider.Section{Name: "Location"}
	if r, err := p.region(ctx, regionID); err == nil {
		location.Add("region", regionID).Add("city", orNil(r.City)).Add("country", orNil(r.Country)).Add("continent", orNil(r.Continent))
	} else {
		location.Add("region", regionID)
	}
	d.Sections = append(d.Sections, server, plan, location, network)
	return d, nil
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func addrOrNil(s string) any { return orNil(addr(s)) }

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	out := append([]string{}, list...)
	sort.Strings(out)
	return out
}

// Instance is a cloud instance as returned by GET /instances.
type Instance struct {
	ID               string   `json:"id"`
	OS               string   `json:"os"`
	RAM              int64    `json:"ram"`
	Disk             int64    `json:"disk"`
	MainIP           string   `json:"main_ip"`
	VCPUCount        int      `json:"vcpu_count"`
	Region           string   `json:"region"`
	Plan             string   `json:"plan"`
	DateCreated      string   `json:"date_created"`
	Status           string   `json:"status"`
	AllowedBandwidth int64    `json:"allowed_bandwidth"`
	NetmaskV4        string   `json:"netmask_v4"`
	GatewayV4        string   `json:"gateway_v4"`
	PowerStatus      string   `json:"power_status"`
	ServerStatus     string   `json:"server_status"`
	V6Network        string   `json:"v6_network"`
	V6MainIP         string   `json:"v6_main_ip"`
	V6NetworkSize    int      `json:"v6_network_size"`
	Label            string   `json:"label"`
	InternalIP       string   `json:"internal_ip"`
	Hostname         string   `json:"hostname"`
	Tags             []string `json:"tags"`
	Features         []string `json:"features"`
	FirewallGroupID  string   `json:"firewall_group_id"`
}

// BareMetal is a bare metal server as returned by GET /bare-metals.
type BareMetal struct {
	ID            string   `json:"id"`
	OS            string   `json:"os"`
	RAM           string   `json:"ram"`
	Disk          string   `json:"disk"`
	MainIP        string   `json:"main_ip"`
	CPUCount      int      `json:"cpu_count"`
	Region        string   `json:"region"`
	DateCreated   string   `json:"date_created"`
	Status        string   `json:"status"`
	NetmaskV4     string   `json:"netmask_v4"`
	GatewayV4     string   `json:"gateway_v4"`
	Plan          string   `json:"plan"`
	V6Network     string   `json:"v6_network"`
	V6MainIP      string   `json:"v6_main_ip"`
	V6NetworkSize int      `json:"v6_network_size"`
	MACAddress    any      `json:"mac_address"`
	Label         string   `json:"label"`
	Tags          []string `json:"tags"`
	Features      []string `json:"features"`
}

// Plan is a plan with its list price.
type Plan struct {
	ID          string  `json:"id"`
	MonthlyCost float64 `json:"monthly_cost"`
}

// Region is a Vultr region such as fra.
type Region struct {
	ID        string `json:"id"`
	City      string `json:"city"`
	Country   string `json:"country"`
	Continent string `json:"continent"`
}

// IPv4 is an address of an instance with its reverse name.
type IPv4 struct {
	IP      string `json:"ip"`
	Reverse string `json:"reverse"`
}

// IPv6 is a network of an instance.
type IPv6 struct {
	IP          string `json:"ip"`
	Network     string `json:"network"`
	NetworkSize int    `json:"network_size"`
}

// ReverseIPv6 is an IPv6 reverse entry.
type ReverseIPv6 struct {
	IP      string `json:"ip"`
	Reverse string `json:"reverse"`
}

// Client is a minimal Vultr API v2 client.
type Client struct {
	BaseURL   string
	Key       string
	UserAgent string
	HTTP      *http.Client
}

type meta struct {
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}

// list follows the cursor pagination of a list endpoint; field is the
// key of the list in the response.
func list[T any](ctx context.Context, c *Client, path, field string) ([]T, error) {
	var out []T
	cursor := ""
	for {
		q := url.Values{"per_page": {strconv.Itoa(perPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var raw map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+sep+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		var items []T
		if data, ok := raw[field]; ok {
			if err := json.Unmarshal(data, &items); err != nil {
				return nil, &provider.APIError{Provider: TypeName, Message: "unexpected response: " + err.Error()}
			}
		}
		out = append(out, items...)
		var m meta
		if data, ok := raw["meta"]; ok {
			_ = json.Unmarshal(data, &m)
		}
		if m.Links.Next == "" || m.Links.Next == cursor || len(items) == 0 {
			return out, nil
		}
		cursor = m.Links.Next
	}
}

// ListInstances returns every instance of the account.
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	return list[Instance](ctx, c, "/instances", "instances")
}

// ListBareMetals returns every bare metal server of the account.
func (c *Client) ListBareMetals(ctx context.Context) ([]BareMetal, error) {
	return list[BareMetal](ctx, c, "/bare-metals", "bare_metals")
}

// ListPlans returns the cloud plans with their prices.
func (c *Client) ListPlans(ctx context.Context) ([]Plan, error) {
	return list[Plan](ctx, c, "/plans?type=all", "plans")
}

// ListMetalPlans returns the bare metal plans with their prices.
func (c *Client) ListMetalPlans(ctx context.Context) ([]Plan, error) {
	return list[Plan](ctx, c, "/plans-metal", "plans_metal")
}

// ListRegions returns the regions.
func (c *Client) ListRegions(ctx context.Context) ([]Region, error) {
	return list[Region](ctx, c, "/regions", "regions")
}

// GetInstance returns one instance.
func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	var raw struct {
		Instance Instance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	return &raw.Instance, nil
}

// GetBareMetal returns one bare metal server.
func (c *Client) GetBareMetal(ctx context.Context, id string) (*BareMetal, error) {
	var raw struct {
		BareMetal BareMetal `json:"bare_metal"`
	}
	if err := c.do(ctx, http.MethodGet, "/bare-metals/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	return &raw.BareMetal, nil
}

// IPv4s returns an instance's IPv4 addresses with their reverse names.
func (c *Client) IPv4s(ctx context.Context, id string) ([]IPv4, error) {
	return list[IPv4](ctx, c, "/instances/"+url.PathEscape(id)+"/ipv4", "ipv4s")
}

// IPv6s returns an instance's IPv6 networks.
func (c *Client) IPv6s(ctx context.Context, id string) ([]IPv6, error) {
	return list[IPv6](ctx, c, "/instances/"+url.PathEscape(id)+"/ipv6", "ipv6s")
}

// ReverseIPv6s returns an instance's IPv6 reverse entries.
func (c *Client) ReverseIPv6s(ctx context.Context, id string) ([]ReverseIPv6, error) {
	var raw struct {
		Reverse []ReverseIPv6 `json:"reverse_ipv6s"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(id)+"/ipv6/reverse", nil, &raw); err != nil {
		return nil, err
	}
	return raw.Reverse, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return &provider.APIError{Provider: TypeName, Message: "encoding request: " + err.Error()}
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
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

func parseError(status int, body []byte) error {
	apiErr := &provider.APIError{Provider: TypeName, Status: status}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		apiErr.Message = e.Error
	}
	switch {
	case status == http.StatusUnauthorized && strings.HasPrefix(apiErr.Message, "Unauthorized IP address"):
		// Vultr names the address: the key is right, its access control
		// does not list this machine
		apiErr.Message = apiErr.Message + " (the API key's access control does not allow it: customer portal → Account → API → Access Control, add the address)"
	case status == http.StatusUnauthorized && apiErr.Message != "":
		apiErr.Message = "authentication failed: " + apiErr.Message + " (check the API key, and that its access control allows this machine's address)"
	case status == http.StatusUnauthorized:
		apiErr.Message = "authentication failed (check the API key, and that its access control allows this machine's address)"
	case status == http.StatusForbidden && apiErr.Message == "":
		apiErr.Message = "forbidden (the API key's access control may not allow this machine's address)"
	case status == http.StatusTooManyRequests:
		apiErr.Message = "rate limit exceeded"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
