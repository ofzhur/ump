// Package manual is the provider type for servers added by hand: no API,
// no rescue system, no power control. ump knows what the user told it
// (ump server add) and reaches the machine over SSH alone; what it can
// do there, it does the same as everywhere.
package manual

import (
	"context"
	"sort"

	"github.com/ofzhur/ump/internal/provider"
)

// TypeName is the provider type name.
const TypeName = "manual"

// AccountName is the account every manual server belongs to.
const AccountName = "manual"

// Type registers the manual provider type.
var Type = provider.Type{
	Name:         TypeName,
	Kind:         provider.KindMixed,
	Description:  "servers added by hand (ump server add): no API, no rescue system; reached over SSH alone",
	Capabilities: []provider.Capability{provider.CapInventory, provider.CapDescribe},
	New: func(account string, settings map[string]string, opts provider.Options) (provider.Provider, error) {
		return &Provider{account: account, servers: opts.Manual}, nil
	},
}

// Provider lists the manual servers of the configuration.
type Provider struct {
	account string
	servers map[string]provider.ManualServer
}

func (p *Provider) Type() string    { return TypeName }
func (p *Provider) Account() string { return p.account }
func (p *Provider) Capabilities() []provider.Capability {
	return []provider.Capability{provider.CapInventory, provider.CapDescribe}
}

// List returns the servers, as machines with the status "manual".
func (p *Provider) List(ctx context.Context) ([]provider.Machine, error) {
	names := make([]string, 0, len(p.servers))
	for n := range p.servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]provider.Machine, 0, len(names))
	for _, n := range names {
		out = append(out, Machine(p.account, n, p.servers[n]))
	}
	return out, nil
}

// Machine is the machine a manual server entry describes.
func Machine(account, name string, s provider.ManualServer) provider.Machine {
	kind := s.Kind
	if kind == "" {
		kind = provider.KindBareMetal
	}
	m := provider.Machine{
		Provider: TypeName,
		Account:  account,
		ID:       name,
		Name:     name,
		Kind:     kind,
		IPv4:     s.IPv4,
		IPv6:     s.IPv6,
		Location: s.Location,
		Product:  s.Product,
		Status:   "manual",
	}
	if s.Note != "" {
		m.Labels = map[string]string{"note": s.Note}
	}
	return m
}

// Describe says what ump knows, and what it cannot do.
func (p *Provider) Describe(ctx context.Context, m provider.Machine) (*provider.Details, error) {
	s, ok := p.servers[m.ID]
	if !ok {
		return &provider.Details{Machine: m}, nil
	}
	return &provider.Details{
		Machine: m,
		Sections: []provider.Section{{
			Name: "Manual",
			Items: []provider.Item{
				{Key: "note", Value: s.Note},
				{Key: "api", Value: "none: added with ump server add; ump server remove forgets it"},
				{Key: "rescue", Value: "none: ump server rescue and reinstall work only when the running system lets UMP in over SSH, and a failed installation is yours to recover"},
			},
		}},
	}, nil
}
