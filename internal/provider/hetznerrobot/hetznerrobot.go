// Package hetznerrobot integrates Hetzner's dedicated servers through the
// Robot web service (https://robot.hetzner.com/doc/webservice/en.html).
//
// The client is hand-written over the REST API: UMP needs a handful of
// calls, and the API is simple (HTTP basic auth, JSON). Keep in mind that
// Robot allows only 200 requests per hour per account.
package hetznerrobot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
const TypeName = "hetzner-robot"

// DefaultBaseURL is the Robot web service endpoint.
const DefaultBaseURL = "https://robot-ws.your-server.de"

// Type is the provider type description, registered by provider/all.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindBareMetal,
	Description:  "Hetzner dedicated servers (Robot web service)",
	Capabilities: capabilities,
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

var capabilities = []provider.Capability{provider.CapInventory, provider.CapDescribe, provider.CapRename, provider.CapReverseDNS, provider.CapPower, provider.CapRescue}

// RescueOS is the rescue system Robot boots.
const RescueOS = "linux"

// powerPoll is how often the power state is checked while waiting for a
// power cycle.
var powerPoll = 3 * time.Second

// Provider is a configured Hetzner Robot account.
type Provider struct {
	account string
	client  *Client

	mu      sync.Mutex
	servers map[int]Server // from the last listing, so PTRs needs no second lookup
	rdns    []RDNS         // the account's entries, fetched once per process
	rdnsOK  bool
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
func (p *Provider) Capabilities() []provider.Capability { return capabilities }

// server returns the listing data of a server, from the last listing or
// by looking it up.
func (p *Provider) server(ctx context.Context, m provider.Machine) (Server, error) {
	number, err := strconv.Atoi(m.ID)
	if err != nil {
		return Server{}, fmt.Errorf("%s: %q is not a server number", TypeName, m.ID)
	}
	p.mu.Lock()
	s, ok := p.servers[number]
	p.mu.Unlock()
	if ok {
		return s, nil
	}
	d, err := p.client.GetServer(ctx, number)
	if err != nil {
		return Server{}, err
	}
	return d.Server, nil
}

// addresses returns the single addresses and the prefixes of a server.
func addresses(s Server) (singles, prefixes []string) {
	singles = append(singles, s.ServerIP)
	singles = append(singles, s.IP...)
	for _, sn := range s.Subnet {
		prefixes = append(prefixes, sn.IP+"/"+sn.Mask)
	}
	if s.ServerIPv6Net != "" {
		prefixes = append(prefixes, s.ServerIPv6Net+"/64")
	}
	return singles, prefixes
}

func (p *Provider) number(m provider.Machine) (int, error) {
	n, err := strconv.Atoi(m.ID)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a server number", TypeName, m.ID)
	}
	return n, nil
}

// PowerState implements provider.Power from GET /reset.
func (p *Provider) PowerState(ctx context.Context, m provider.Machine) (provider.PowerState, error) {
	n, err := p.number(m)
	if err != nil {
		return provider.PowerUnknown, err
	}
	r, err := p.client.GetReset(ctx, n)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return powerState(r.OperatingStatus), nil
}

func powerState(operatingStatus string) provider.PowerState {
	switch operatingStatus {
	case "running":
		return provider.PowerOn
	case "shut off":
		return provider.PowerOff
	}
	return provider.PowerUnknown
}

// Reboot implements provider.Power. A hardware or software reset does
// nothing on a server that is switched off, so one that is off is
// switched on instead. A power cycle is a long press of the power button,
// a wait until the server is off, and a short press.
func (p *Provider) Reboot(ctx context.Context, m provider.Machine, mode provider.RebootMode) error {
	n, err := p.number(m)
	if err != nil {
		return err
	}
	r, err := p.client.GetReset(ctx, n)
	if err != nil {
		return err
	}
	if powerState(r.OperatingStatus) == provider.PowerOff {
		return p.client.Reset(ctx, n, "power")
	}
	switch mode {
	case provider.RebootSoft:
		return p.client.Reset(ctx, n, "sw")
	case provider.RebootPowerCycle:
		if err := p.client.Reset(ctx, n, "power_long"); err != nil {
			return err
		}
		if err := p.waitPower(ctx, n, provider.PowerOff); err != nil {
			return err
		}
		return p.client.Reset(ctx, n, "power")
	}
	return p.client.Reset(ctx, n, "hw")
}

// PowerOn implements provider.Power with a short press of the power button.
func (p *Provider) PowerOn(ctx context.Context, m provider.Machine) error {
	n, err := p.number(m)
	if err != nil {
		return err
	}
	r, err := p.client.GetReset(ctx, n)
	if err != nil {
		return err
	}
	if powerState(r.OperatingStatus) == provider.PowerOn {
		return nil
	}
	return p.client.Reset(ctx, n, "power")
}

// PowerOff implements provider.Power: a short press of the power button
// asks the operating system to shut down, a long press cuts the power.
func (p *Provider) PowerOff(ctx context.Context, m provider.Machine, hard bool) error {
	n, err := p.number(m)
	if err != nil {
		return err
	}
	r, err := p.client.GetReset(ctx, n)
	if err != nil {
		return err
	}
	if powerState(r.OperatingStatus) == provider.PowerOff {
		return nil
	}
	if hard {
		return p.client.Reset(ctx, n, "power_long")
	}
	return p.client.Reset(ctx, n, "power")
}

func (p *Provider) waitPower(ctx context.Context, n int, want provider.PowerState) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		r, err := p.client.GetReset(ctx, n)
		if err != nil {
			return err
		}
		if powerState(r.OperatingStatus) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return &provider.APIError{Provider: TypeName, Message: fmt.Sprintf("server %d did not reach power state %s in time", n, want)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(powerPoll):
		}
	}
}

// RescueEnabled implements provider.Rescue.
func (p *Provider) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	n, err := p.number(m)
	if err != nil {
		return false, err
	}
	r, err := p.client.GetRescue(ctx, n)
	if err != nil {
		return false, err
	}
	return r.Active, nil
}

// EnableRescue implements provider.Rescue: keys unknown to the account
// are registered first, because Robot only takes fingerprints of
// registered keys; then the rescue system is armed for the next boot.
func (p *Provider) EnableRescue(ctx context.Context, m provider.Machine, opts provider.RescueOptions) (*provider.RescueInfo, error) {
	n, err := p.number(m)
	if err != nil {
		return nil, err
	}
	var fingerprints []string
	for _, k := range opts.Keys {
		fp, err := p.client.EnsureKey(ctx, k)
		if err != nil {
			return nil, err
		}
		fingerprints = append(fingerprints, fp)
	}
	r, err := p.client.EnableRescue(ctx, n, fingerprints)
	if err != nil {
		return nil, err
	}
	info := &provider.RescueInfo{Address: m.IPv4, User: "root", Password: r.Password}
	if info.Address == "" {
		info.Address = r.ServerIP
	}
	for _, hk := range r.HostKey {
		info.HostKeys = append(info.HostKeys, strings.TrimSpace(hk.Key.Type+" "+hk.Key.Fingerprint))
	}
	return info, nil
}

// DisableRescue implements provider.Rescue.
func (p *Provider) DisableRescue(ctx context.Context, m provider.Machine) error {
	n, err := p.number(m)
	if err != nil {
		return err
	}
	r, err := p.client.GetRescue(ctx, n)
	if err != nil || !r.Active {
		return err
	}
	return p.client.DisableRescue(ctx, n)
}

// PTRs implements provider.ReverseDNS from the account-wide entry list.
func (p *Provider) PTRs(ctx context.Context, m provider.Machine) ([]provider.PTR, error) {
	s, err := p.server(ctx, m)
	if err != nil {
		return nil, err
	}
	entries, err := p.allRDNS(ctx)
	if err != nil {
		return nil, err
	}
	singles, prefixes := addresses(s)
	byIP := map[string]string{}
	for _, e := range entries {
		if a, err := netip.ParseAddr(e.IP); err == nil && provider.Contains(a, singles, prefixes) {
			byIP[a.String()] = e.PTR
		}
	}
	out := []provider.PTR{{IP: s.ServerIP, Name: byIP[s.ServerIP]}}
	delete(byIP, s.ServerIP)
	others := make([]string, 0, len(byIP))
	for ip := range byIP {
		others = append(others, ip)
	}
	sort.Strings(others)
	for _, ip := range others {
		out = append(out, provider.PTR{IP: ip, Name: byIP[ip]})
	}
	return out, nil
}

// SetPTR implements provider.ReverseDNS.
func (p *Provider) SetPTR(ctx context.Context, m provider.Machine, ip netip.Addr, name string) error {
	s, err := p.server(ctx, m)
	if err != nil {
		return err
	}
	if singles, prefixes := addresses(s); !provider.Contains(ip, singles, prefixes) {
		return fmt.Errorf("%s: address %s does not belong to server %s", TypeName, ip, m.ID)
	}
	p.mu.Lock()
	p.rdnsOK = false
	p.mu.Unlock()
	if name == "" {
		return p.client.DeleteRDNS(ctx, ip.String())
	}
	return p.client.SetRDNS(ctx, ip.String(), name)
}

func (p *Provider) allRDNS(ctx context.Context) ([]RDNS, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.rdnsOK {
		entries, err := p.client.ListRDNS(ctx)
		if err != nil {
			return nil, err
		}
		p.rdns, p.rdnsOK = entries, true
	}
	return p.rdns, nil
}

// Rename implements provider.Renamer by setting the Robot server name.
func (p *Provider) Rename(ctx context.Context, m provider.Machine, name string) (provider.Machine, error) {
	number, err := strconv.Atoi(m.ID)
	if err != nil {
		return provider.Machine{}, fmt.Errorf("%s: %q is not a server number", TypeName, m.ID)
	}
	s, err := p.client.RenameServer(ctx, number, name)
	if err != nil {
		return provider.Machine{}, err
	}
	return p.machine(s.Server), nil
}

// Describe implements provider.Describer: the server's contract, its
// addresses and features, and when the server supports them its power
// state (GET /reset) and rescue system state (GET /boot/rescue).
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	number, err := strconv.Atoi(m.ID)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a server number", TypeName, m.ID)
	}
	s, err := p.client.GetServer(ctx, number)
	if err != nil {
		return nil, err
	}
	d := &provider.Details{Machine: p.machine(s.Server)}

	contract := provider.Section{Name: "Contract"}
	contract.Add("product", s.Product).Add("datacenter", s.DC).Add("traffic", s.Traffic).
		Add("paid_until", s.PaidUntil).Add("cancelled", s.Cancelled).Add("status", s.Status)
	var storagebox any
	if s.LinkedStoragebox != nil {
		storagebox = *s.LinkedStoragebox
	}
	contract.Add("linked_storagebox", storagebox)

	extra, subnets := []string{}, []string{}
	for _, ip := range s.IP {
		if ip != s.ServerIP {
			extra = append(extra, ip)
		}
	}
	for _, sn := range s.Subnet {
		subnets = append(subnets, sn.IP+"/"+sn.Mask)
	}
	addresses := provider.Section{Name: "Addresses"}
	addresses.AddLabeled("primary_ip", "Primary IP", s.ServerIP).AddLabeled("additional_ips", "Additional IPs", extra).Add("subnets", subnets)

	features := provider.Section{Name: "Features"}
	features.Add("reset", s.Reset).Add("rescue", s.Rescue).Add("vnc", s.VNC).Add("windows", s.Windows).
		Add("plesk", s.Plesk).Add("cpanel", s.CPanel).Add("wol", s.WOL).Add("hot_swap", s.HotSwap)
	d.Sections = append(d.Sections, contract, addresses, features)

	if r, err := p.client.GetReset(ctx, number); err == nil {
		power := provider.Section{Name: "Power"}
		power.Add("state", r.OperatingStatus).Add("reset_types", nonNil(r.Type))
		d.Sections = append(d.Sections, power)
	} else if !isNotFound(err) {
		d.Sections = append(d.Sections, provider.ErrorSection("Power", err))
	}
	if r, err := p.client.GetRescue(ctx, number); err == nil {
		keys := []string{}
		for _, k := range r.AuthorizedKey {
			keys = append(keys, k.Key.Name+" ("+k.Key.Fingerprint+")")
		}
		rescue := provider.Section{Name: "Rescue"}
		rescue.Add("active", r.Active).Add("systems", r.Systems()).Add("architectures", r.Architectures()).Add("authorized_keys", keys)
		d.Sections = append(d.Sections, rescue)
	} else if !isNotFound(err) {
		d.Sections = append(d.Sections, provider.ErrorSection("Rescue", err))
	}
	return d, nil
}

func isNotFound(err error) bool {
	var apiErr *provider.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// List implements provider.Inventory.
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	servers, err := p.client.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Machine, 0, len(servers))
	p.mu.Lock()
	p.servers = make(map[int]Server, len(servers))
	for _, s := range servers {
		p.servers[s.ServerNumber] = s
	}
	p.mu.Unlock()
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

// ServerDetail is GET /server/{server-number}: the listing fields plus
// which features the server supports.
type ServerDetail struct {
	Server
	Reset            bool `json:"reset"`
	Rescue           bool `json:"rescue"`
	VNC              bool `json:"vnc"`
	Windows          bool `json:"windows"`
	Plesk            bool `json:"plesk"`
	CPanel           bool `json:"cpanel"`
	WOL              bool `json:"wol"`
	HotSwap          bool `json:"hot_swap"`
	LinkedStoragebox *int `json:"linked_storagebox"`
}

// ResetInfo is GET /reset/{server-number}: the reset types the server
// supports and its power state ("running", "shut off", "not supported").
type ResetInfo struct {
	Type            []string `json:"type"`
	OperatingStatus string   `json:"operating_status"`
}

// RescueInfo is GET /boot/{server-number}/rescue, and the answer to
// enabling it (then with the generated password). OS and Arch are the
// lists of options while the rescue system is inactive and the chosen
// values once it is active; Systems and Architectures read both.
type RescueInfo struct {
	ServerIP      string `json:"server_ip"`
	OS            any    `json:"os"`
	Arch          any    `json:"arch"`
	Active        bool   `json:"active"`
	Password      string `json:"password"`
	AuthorizedKey []struct {
		Key struct {
			Name        string `json:"name"`
			Fingerprint string `json:"fingerprint"`
			Type        string `json:"type"`
			Size        int    `json:"size"`
		} `json:"key"`
	} `json:"authorized_key"`
	HostKey []struct {
		Key struct {
			Fingerprint string `json:"fingerprint"`
			Type        string `json:"type"`
			Size        int    `json:"size"`
		} `json:"key"`
	} `json:"host_key"`
}

// Key is an SSH public key registered at the account (GET /key).
type Key struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
	Size        int    `json:"size"`
	Data        string `json:"data"`
}

// RDNS is one reverse DNS entry of the account (GET /rdns).
type RDNS struct {
	IP  string `json:"ip"`
	PTR string `json:"ptr"`
}

// Systems returns the rescue operating systems (available, or the active one).
func (r *RescueInfo) Systems() []string { return anyStrings(r.OS) }

// Architectures returns the rescue architectures (available, or the active one).
func (r *RescueInfo) Architectures() []string { return anyStrings(r.Arch) }

func anyStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return []string{}
	case string:
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, anyString(e))
		}
		return out
	}
	return []string{anyString(v)}
}

func anyString(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
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

// GetServer returns one server with its feature flags.
func (c *Client) GetServer(ctx context.Context, number int) (*ServerDetail, error) {
	var raw struct {
		Server ServerDetail `json:"server"`
	}
	if err := c.get(ctx, "/server/"+strconv.Itoa(number), &raw); err != nil {
		return nil, err
	}
	return &raw.Server, nil
}

// GetReset returns the reset options and power state of a server.
func (c *Client) GetReset(ctx context.Context, number int) (*ResetInfo, error) {
	var raw struct {
		Reset ResetInfo `json:"reset"`
	}
	if err := c.get(ctx, "/reset/"+strconv.Itoa(number), &raw); err != nil {
		return nil, err
	}
	return &raw.Reset, nil
}

// GetRescue returns the rescue system state of a server.
func (c *Client) GetRescue(ctx context.Context, number int) (*RescueInfo, error) {
	var raw struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.get(ctx, "/boot/"+strconv.Itoa(number)+"/rescue", &raw); err != nil {
		return nil, err
	}
	return &raw.Rescue, nil
}

// RenameServer sets the server name (POST /server/{server-number}).
func (c *Client) RenameServer(ctx context.Context, number int, name string) (*ServerDetail, error) {
	var raw struct {
		Server ServerDetail `json:"server"`
	}
	if err := c.postForm(ctx, "/server/"+strconv.Itoa(number), url.Values{"server_name": {name}}, &raw); err != nil {
		return nil, err
	}
	return &raw.Server, nil
}

// Reset triggers a reset of the given type: sw, hw, power, power_long.
// "man" (a technician) is deliberately not offered.
func (c *Client) Reset(ctx context.Context, number int, typ string) error {
	return c.postForm(ctx, "/reset/"+strconv.Itoa(number), url.Values{"type": {typ}}, nil)
}

// ListKeys returns the SSH keys registered at the account.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var raw []struct {
		Key Key `json:"key"`
	}
	if err := c.get(ctx, "/key", &raw); err != nil {
		if isCode(err, http.StatusNotFound, "NOT_FOUND") {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Key, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Key)
	}
	return out, nil
}

// EnsureKey returns the fingerprint of a registered key equal to k,
// registering it when the account does not have it yet.
func (c *Client) EnsureKey(ctx context.Context, k provider.PublicKey) (string, error) {
	keys, err := c.ListKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, have := range keys {
		if strings.EqualFold(have.Fingerprint, k.Fingerprint) {
			return have.Fingerprint, nil
		}
	}
	var raw struct {
		Key Key `json:"key"`
	}
	if err := c.postForm(ctx, "/key", url.Values{"name": {k.Name}, "data": {k.Data}}, &raw); err != nil {
		return "", err
	}
	return raw.Key.Fingerprint, nil
}

// EnableRescue arms the rescue system (POST /boot/{n}/rescue) with the
// fingerprints of registered keys; without keys Robot generates a
// password, which the answer carries.
func (c *Client) EnableRescue(ctx context.Context, number int, fingerprints []string) (*RescueInfo, error) {
	form := url.Values{"os": {RescueOS}}
	for _, fp := range fingerprints {
		form.Add("authorized_key[]", fp)
	}
	var raw struct {
		Rescue RescueInfo `json:"rescue"`
	}
	if err := c.postForm(ctx, "/boot/"+strconv.Itoa(number)+"/rescue", form, &raw); err != nil {
		return nil, err
	}
	return &raw.Rescue, nil
}

// DisableRescue disarms the rescue system (DELETE /boot/{n}/rescue).
func (c *Client) DisableRescue(ctx context.Context, number int) error {
	return c.do(ctx, http.MethodDelete, "/boot/"+strconv.Itoa(number)+"/rescue", "", nil, nil)
}

// ListRDNS returns every reverse DNS entry of the account.
func (c *Client) ListRDNS(ctx context.Context) ([]RDNS, error) {
	var raw []struct {
		RDNS RDNS `json:"rdns"`
	}
	if err := c.get(ctx, "/rdns", &raw); err != nil {
		if isCode(err, http.StatusNotFound, "RDNS_NOT_FOUND") {
			return nil, nil
		}
		return nil, err
	}
	out := make([]RDNS, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.RDNS)
	}
	return out, nil
}

// SetRDNS creates or updates the reverse DNS entry of an address.
func (c *Client) SetRDNS(ctx context.Context, ip, ptr string) error {
	form := url.Values{"ptr": {ptr}}
	err := c.postForm(ctx, "/rdns/"+url.PathEscape(ip), form, nil)
	if isCode(err, http.StatusNotFound, "RDNS_NOT_FOUND") {
		// no entry yet: the Robot wants PUT for creation
		return c.do(ctx, http.MethodPut, "/rdns/"+url.PathEscape(ip), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	}
	return err
}

// DeleteRDNS removes the reverse DNS entry of an address; a missing entry
// is not an error.
func (c *Client) DeleteRDNS(ctx context.Context, ip string) error {
	err := c.do(ctx, http.MethodDelete, "/rdns/"+url.PathEscape(ip), "", nil, nil)
	if isCode(err, http.StatusNotFound, "RDNS_NOT_FOUND") {
		return nil
	}
	return err
}

func isCode(err error, status int, code string) bool {
	var apiErr *provider.APIError
	return errors.As(err, &apiErr) && apiErr.Status == status && apiErr.Code == code
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

// postForm sends form-encoded data, which is what the Robot API expects
// for writes.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if err != nil {
		return &provider.APIError{Provider: TypeName, Message: err.Error()}
	}
	req.SetBasicAuth(c.Login, c.Password)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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
	case apiErr.Code == "INVALID_INPUT":
		apiErr.Message = "the Robot API rejected the input"
		if e.Error.Message != "" {
			apiErr.Message += ": " + e.Error.Message
		}
	case apiErr.Code == "IP_NOT_FOUND":
		apiErr.Message = "the address is not assigned to this account"
	case apiErr.Code == "RESET_MANUAL_ACTIVE":
		apiErr.Message = "a manual reset is already in progress"
	case apiErr.Code == "RESET_FAILED":
		apiErr.Message = "the reset failed at Hetzner; try again or contact support"
	case apiErr.Code == "BOOT_ALREADY_ENABLED":
		apiErr.Message = "another boot option is already active; disable it first"
	case apiErr.Code == "RATE_LIMIT_EXCEEDED":
		apiErr.Message = "rate limit exceeded (the Robot API allows 200 requests per hour)"
	case apiErr.Message == "":
		apiErr.Message = http.StatusText(status)
	}
	return apiErr
}
