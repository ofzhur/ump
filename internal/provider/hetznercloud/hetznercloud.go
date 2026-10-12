// Package hetznercloud integrates Hetzner Cloud servers through the Cloud
// API (https://docs.hetzner.cloud/).
//
// The client is hand-written over the REST API rather than using the
// official SDK, which pulls in Prometheus and protobuf for a listing that
// needs one paginated call.
package hetznercloud

import (
	"context"
	"encoding/json"
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
	return []provider.Capability{provider.CapInventory}
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

// Server is a cloud server as returned by GET /servers.
type Server struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	PublicNet struct {
		IPv4 *IPInfo `json:"ipv4"`
		IPv6 *IPInfo `json:"ipv6"`
	} `json:"public_net"`
	ServerType struct {
		Name         string `json:"name"`
		Architecture string `json:"architecture"`
	} `json:"server_type"`
	Datacenter struct {
		Name     string `json:"name"`
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
	Labels        map[string]string `json:"labels"`
	RescueEnabled bool              `json:"rescue_enabled"`
}

// IPInfo is a public address assignment.
type IPInfo struct {
	IP string `json:"ip"`
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

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &provider.APIError{Provider: TypeName, Status: resp.StatusCode, Message: "reading response: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return parseError(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
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
	case status == http.StatusTooManyRequests:
		apiErr.Message = "rate limit exceeded"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
