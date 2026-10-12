// Package hetznerrobot integrates Hetzner's dedicated servers through the
// Robot web service (https://robot.hetzner.com/doc/webservice/en.html).
//
// The client is hand-written over the REST API: UMP needs a handful of
// calls, and the API is simple (HTTP basic auth, JSON). Keep in mind that
// Robot allows only 200 requests per hour per account.
package hetznerrobot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name used in config.yaml.
const TypeName = "hetzner-robot"

// DefaultBaseURL is the Robot web service endpoint.
const DefaultBaseURL = "https://robot-ws.your-server.de"

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:        TypeName,
	Kind:        provider.KindBareMetal,
	Description: "Hetzner dedicated servers (Robot web service)",
	Fields: []provider.Field{
		{
			Key:      "login",
			Label:    "Web service login",
			Help:     "the Robot web service user, e.g. #ws+AbCdEfGh (Robot: Settings → Web service and app settings)",
			Required: true,
		},
		{
			Key:      "password",
			Label:    "Web service password",
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

// Provider is a configured Hetzner Robot account.
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
			Login:     settings["login"],
			Password:  settings["password"],
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
		ID:       strconv.Itoa(s.ServerNumber),
		Name:     s.ServerName,
		Kind:     provider.KindBareMetal,
		Location: strings.ToLower(s.DC),
		Product:  s.Product,
		Status:   strings.ToLower(s.Status),
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if strings.Contains(s.ServerIP, ":") {
		m.IPv6 = s.ServerIP
	} else {
		m.IPv4 = s.ServerIP
	}
	for _, sn := range s.Subnet {
		if strings.Contains(sn.IP, ":") {
			m.IPv6 = sn.IP + "/" + sn.Mask
			break
		}
	}
	if m.IPv6 == "" && s.ServerIPv6Net != "" {
		m.IPv6 = s.ServerIPv6Net + "/64"
	}
	if s.Cancelled {
		m.Status = "cancelled"
	}
	return m
}

// Server is a dedicated server as returned by GET /server.
type Server struct {
	ServerIP      string   `json:"server_ip"`
	ServerIPv6Net string   `json:"server_ipv6_net"`
	ServerNumber  int      `json:"server_number"`
	ServerName    string   `json:"server_name"`
	Product       string   `json:"product"`
	DC            string   `json:"dc"`
	Traffic       string   `json:"traffic"`
	Status        string   `json:"status"`
	Cancelled     bool     `json:"cancelled"`
	PaidUntil     string   `json:"paid_until"`
	IP            []string `json:"ip"`
	Subnet        []Subnet `json:"subnet"`
}

// Subnet is an address block assigned to a server.
type Subnet struct {
	IP   string `json:"ip"`
	Mask string `json:"mask"`
}

// Client is a minimal Robot web service client.
type Client struct {
	BaseURL   string
	Login     string
	Password  string
	UserAgent string
	HTTP      *http.Client
}

// ListServers returns all servers of the account.
func (c *Client) ListServers(ctx context.Context) ([]Server, error) {
	var raw []struct {
		Server Server `json:"server"`
	}
	if err := c.get(ctx, "/server", &raw); err != nil {
		var apiErr *provider.APIError
		// Robot answers 404 SERVER_NOT_FOUND for an account without servers.
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "SERVER_NOT_FOUND" {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Server, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Server)
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.SetBasicAuth(c.Login, c.Password)
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
			Status  int    `json:"status"`
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
		apiErr.Message = "authentication failed (check the web service login and password)"
	case apiErr.Code == "RATE_LIMIT_EXCEEDED":
		apiErr.Message = "rate limit exceeded (the Robot API allows 200 requests per hour)"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
