// Package hetznercloud integrates Hetzner Cloud servers through the Cloud
// API (https://docs.hetzner.cloud/).
//
// The client is hand-written over the REST API rather than using the
// official SDK, which pulls in Prometheus and protobuf for a listing that
// needs one paginated call.
package hetznercloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "hetzner-cloud"

// DefaultBaseURL is the Hetzner Cloud API endpoint.
const DefaultBaseURL = "https://api.hetzner.cloud/v1"

const perPage = 50

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:        TypeName,
	Kind:        provider.KindCloud,
	Description: "Hetzner Cloud servers",
	Fields: []provider.Field{
		{
			Key:      "token",
			Label:    "API token",
			Help:     "a project API token; read permission is enough for listing (Cloud Console: project → Security → API tokens)",
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

// Provider is a configured Hetzner Cloud project.
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
func (p *Provider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename}
}

// Rename implements provider.Renamer. Hetzner Cloud requires names to be
// unique within the project and valid hostnames.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	id, err := strconv.ParseInt(m.ID, 10, 64)
	if err != nil {
		return provider.Machine{}, fmt.Errorf("%s: %q is not a server ID", TypeName, m.ID)
	}
	s, err := p.client.UpdateServer(ctx, id, name)
	if err != nil {
		return provider.Machine{}, err
	}
	return p.machine(*s), nil
}

// Describe implements provider.Describer from GET /servers/{id}.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	id, err := strconv.ParseInt(m.ID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a server ID", TypeName, m.ID)
	}
	s, err := p.client.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &provider.Details{Machine: p.machine(*s)}

	server := provider.Section{Name: "Server"}
	var backup, placement any
	if s.BackupWindow != nil && *s.BackupWindow != "" {
		backup = *s.BackupWindow
	}
	if s.PlacementGroup != nil {
		placement = s.PlacementGroup.Name
	}
	server.Add("created", s.Created).Add("locked", s.Locked).Add("rescue_enabled", s.RescueEnabled).
		Add("backup_window", backup).Add("placement_group", placement).Add("volumes", ids(s.Volumes)).
		Add("primary_disk", provider.Bytes(s.PrimaryDiskSize)<<30)

	typ := provider.Section{Name: "Type"}
	typ.Add("name", s.ServerType.Name).Add("description", s.ServerType.Description).Add("cores", s.ServerType.Cores).
		Add("memory", provider.Bytes(s.ServerType.Memory*float64(1<<30))).Add("disk", provider.Bytes(s.ServerType.Disk)<<30).
		Add("cpu_type", s.ServerType.CPUType).Add("architecture", s.ServerType.Architecture)

	image := provider.Section{Name: "Image"}
	if s.Image != nil {
		name := s.Image.Description
		if name == "" {
			name = s.Image.Name
		}
		image.Add("name", name).Add("os_flavor", s.Image.OSFlavor).Add("os_version", s.Image.OSVersion).Add("type", s.Image.Type)
	} else {
		image.Add("name", nil)
	}
	var iso any
	if s.ISO != nil {
		iso = s.ISO.Name
	}
	image.Add("iso", iso)

	location := provider.Section{Name: "Location"}
	location.Add("datacenter", s.Datacenter.Name).Add("description", s.Datacenter.Description).
		Add("city", s.Datacenter.Location.City).Add("country", s.Datacenter.Location.Country).
		Add("network_zone", s.Datacenter.Location.NetworkZone)

	network := provider.Section{Name: "Network"}
	if v4 := s.PublicNet.IPv4; v4 != nil {
		network.Add("ipv4", v4.IP).AddLabeled("ipv4_dns_ptr", "IPv4 DNS PTR", v4.ptrV4()).AddLabeled("ipv4_blocked", "IPv4 blocked", v4.Blocked)
	} else {
		network.Add("ipv4", nil)
	}
	if v6 := s.PublicNet.IPv6; v6 != nil {
		network.Add("ipv6", v6.IP).AddLabeled("ipv6_dns_ptr", "IPv6 DNS PTR", v6.ptrV6()).AddLabeled("ipv6_blocked", "IPv6 blocked", v6.Blocked)
	} else {
		network.Add("ipv6", nil)
	}
	private, firewalls := []string{}, []string{}
	for _, n := range s.PrivateNet {
		private = append(private, fmt.Sprintf("%s on network %d (%s)", n.IP, n.Network, n.MACAddress))
	}
	for _, f := range s.PublicNet.Firewalls {
		firewalls = append(firewalls, fmt.Sprintf("%d (%s)", f.ID, f.Status))
	}
	network.AddLabeled("floating_ips", "Floating IPs", ids(s.PublicNet.FloatingIPs)).Add("private_networks", private).Add("firewalls", firewalls)

	traffic := provider.Section{Name: "Traffic"}
	traffic.Add("included", bytesOrNil(s.IncludedTraffic)).Add("outgoing", bytesOrNil(s.OutgoingTraffic)).Add("ingoing", bytesOrNil(s.IngoingTraffic))

	protection := provider.Section{Name: "Protection"}
	protection.Add("delete", s.Protection.Delete).Add("rebuild", s.Protection.Rebuild)

	d.Sections = append(d.Sections, server, typ, image, location, network, traffic, protection)
	return d, nil
}

func ids(list []int64) []string {
	out := make([]string, 0, len(list))
	for _, id := range list {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

func bytesOrNil(v *int64) any {
	if v == nil {
		return nil
	}
	return provider.Bytes(*v)
}

// List implements provider.Inventory.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	servers, err := p.client.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Machine, 0, len(servers))
	for _, s := range servers {
		out = append(out, p.machine(s))
	}
	return out, nil
}

func (p *Provider) machine(s Server) provider.Machine {
	m := provider.Machine{
		Provider: TypeName,
		Account:  p.account,
		ID:       strconv.FormatInt(s.ID, 10),
		Name:     s.Name,
		Kind:     provider.KindCloud,
		Location: strings.ToLower(s.Datacenter.Name),
		Product:  s.ServerType.Name,
		Status:   strings.ToLower(s.Status),
	}
	if m.Location == "" {
		m.Location = strings.ToLower(s.Datacenter.Location.Name)
	}
	if s.PublicNet.IPv4 != nil {
		m.IPv4 = s.PublicNet.IPv4.IP
	}
	if s.PublicNet.IPv6 != nil {
		m.IPv6 = s.PublicNet.IPv6.IP
	}
	if len(s.Labels) > 0 {
		m.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			m.Labels[k] = v
		}
	}
	return m
}

// Server is a cloud server as returned by GET /servers and GET /servers/{id}.
type Server struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Created   string `json:"created"`
	PublicNet struct {
		IPv4        *IPInfo `json:"ipv4"`
		IPv6        *IPInfo `json:"ipv6"`
		FloatingIPs []int64 `json:"floating_ips"`
		Firewalls   []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"firewalls"`
	} `json:"public_net"`
	PrivateNet []struct {
		Network    int64  `json:"network"`
		IP         string `json:"ip"`
		MACAddress string `json:"mac_address"`
	} `json:"private_net"`
	ServerType struct {
		Name         string  `json:"name"`
		Description  string  `json:"description"`
		Cores        int     `json:"cores"`
		Memory       float64 `json:"memory"`
		Disk         int     `json:"disk"`
		CPUType      string  `json:"cpu_type"`
		Architecture string  `json:"architecture"`
	} `json:"server_type"`
	Datacenter struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Location    struct {
			Name        string `json:"name"`
			City        string `json:"city"`
			Country     string `json:"country"`
			NetworkZone string `json:"network_zone"`
		} `json:"location"`
	} `json:"datacenter"`
	Image *struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		OSFlavor    string `json:"os_flavor"`
		OSVersion   string `json:"os_version"`
		Type        string `json:"type"`
	} `json:"image"`
	ISO *struct {
		Name string `json:"name"`
	} `json:"iso"`
	Volumes    []int64 `json:"volumes"`
	Protection struct {
		Delete  bool `json:"delete"`
		Rebuild bool `json:"rebuild"`
	} `json:"protection"`
	Labels         map[string]string `json:"labels"`
	RescueEnabled  bool              `json:"rescue_enabled"`
	Locked         bool              `json:"locked"`
	BackupWindow   *string           `json:"backup_window"`
	PlacementGroup *struct {
		Name string `json:"name"`
	} `json:"placement_group"`
	PrimaryDiskSize int    `json:"primary_disk_size"`
	IncludedTraffic *int64 `json:"included_traffic"`
	OutgoingTraffic *int64 `json:"outgoing_traffic"`
	IngoingTraffic  *int64 `json:"ingoing_traffic"`
}

// IPInfo is a public address assignment. DNSPtr is a string for IPv4
// and a list of {ip, dns_ptr} for IPv6.
type IPInfo struct {
	IP      string          `json:"ip"`
	Blocked bool            `json:"blocked"`
	DNSPtr  json.RawMessage `json:"dns_ptr"`
}

func (i *IPInfo) ptrV4() string {
	var s string
	_ = json.Unmarshal(i.DNSPtr, &s)
	return s
}

func (i *IPInfo) ptrV6() []string {
	var entries []struct {
		IP     string `json:"ip"`
		DNSPtr string `json:"dns_ptr"`
	}
	_ = json.Unmarshal(i.DNSPtr, &entries)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.IP+" "+e.DNSPtr)
	}
	return out
}

// Client is a minimal Hetzner Cloud API client.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// ListServers returns all servers of the project, following pagination.
func (c *Client) ListServers(ctx context.Context) ([]Server, error) {
	var out []Server
	for page := 1; page > 0; {
		var resp struct {
			Servers []Server `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		path := "/servers?page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(perPage)
		if err := c.get(ctx, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Servers...)
		if resp.Meta.Pagination.NextPage == nil || *resp.Meta.Pagination.NextPage <= page {
			break
		}
		page = *resp.Meta.Pagination.NextPage
	}
	return out, nil
}

// GetServer returns one server.
func (c *Client) GetServer(ctx context.Context, id int64) (*Server, error) {
	var raw struct {
		Server Server `json:"server"`
	}
	if err := c.get(ctx, "/servers/"+strconv.FormatInt(id, 10), &raw); err != nil {
		return nil, err
	}
	return &raw.Server, nil
}

// UpdateServer sets the server name (PUT /servers/{id}).
func (c *Client) UpdateServer(ctx context.Context, id int64, name string) (*Server, error) {
	var raw struct {
		Server Server `json:"server"`
	}
	body := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := c.do(ctx, http.MethodPut, "/servers/"+strconv.FormatInt(id, 10), body, &raw); err != nil {
		return nil, err
	}
	return &raw.Server, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
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
	if err := json.Unmarshal(data, out); err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "unexpected response: " + err.Error()}
	}
	return nil
}

func parseError(status int, body []byte) error {
	apiErr := &provider.APIError{Provider: TypeName, Status: status}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		apiErr.Code = e.Error.Code
		apiErr.Message = e.Error.Message
	}
	switch {
	case status == http.StatusUnauthorized:
		apiErr.Message = "authentication failed (check the API token)"
	case apiErr.Code == "uniqueness_error":
		apiErr.Message = "the name is already used in this project"
	case apiErr.Code == "invalid_input" && e.Error.Message != "":
		apiErr.Message = e.Error.Message
	case status == http.StatusTooManyRequests:
		apiErr.Message = "rate limit exceeded"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
