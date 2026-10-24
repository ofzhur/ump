// Package provider defines UMP's view of the APIs that know about machines:
// the Machine model, the capability interfaces a provider can implement, and
// the registry of provider types that the CLI, the configuration and the
// setup wizard are driven by.
//
// Providers are deliberately small. They translate between a provider API
// and the Machine model; everything that happens on the machine itself is
// the same for every provider and lives elsewhere.
package provider

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind says what sort of machine a provider or a machine is.
type Kind string

const (
	// KindBareMetal is a dedicated physical server.
	KindBareMetal Kind = "bare-metal"
	// KindCloud is a virtual machine in a cloud.
	KindCloud Kind = "cloud"
	// KindMixed is used for provider types that list both.
	KindMixed Kind = "mixed"
)

// Machine is UMP's provider-independent description of a server.
//
// Field order and names are part of the JSON output contract of
// `ump get servers -o json`.
type Machine struct {
	// Provider is the provider type, e.g. "hetzner-robot".
	Provider string `json:"provider" yaml:"provider"`
	// Account is the name of the configured account the machine was listed from.
	Account string `json:"account" yaml:"account"`
	// ID is the provider-native identifier. Provider + "/" + ID is the
	// unambiguous machine reference.
	ID string `json:"id" yaml:"id"`
	// Name is the name as known at the provider.
	Name string `json:"name" yaml:"name"`
	// Kind says whether this is bare metal or a cloud VM.
	Kind Kind `json:"kind" yaml:"kind"`
	// Arch is the CPU architecture (amd64, arm64) where the provider
	// says; empty when it does not.
	Arch string `json:"arch,omitempty" yaml:"arch,omitempty"`
	// IPv4 is the primary public IPv4 address, if any.
	IPv4 string `json:"ipv4,omitempty" yaml:"ipv4,omitempty"`
	// IPv6 is the primary public IPv6 address or the assigned prefix
	// (providers hand out a /64 more often than a single address).
	IPv6 string `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
	// Location is the datacenter, region or zone, lower-cased.
	Location string `json:"location,omitempty" yaml:"location,omitempty"`
	// Product is the server model or instance type.
	Product string `json:"product,omitempty" yaml:"product,omitempty"`
	// Status is the provider's status for the machine, lower-cased.
	Status string `json:"status" yaml:"status"`
	// Labels are provider-side labels or tags, when the provider has them.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// Price is what the machine costs: from the provider's API where it
	// says (Hetzner Cloud), else what the user set with ump server set.
	Price *Price `json:"price,omitempty" yaml:"price,omitempty"`
	// Attributes are what the user set for the machine with ump server
	// set --attr: an owner, a role, a cost center, anything the inventory
	// should carry.
	Attributes map[string]string `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	// OS is the operating system ump installed last (ubuntu-26.04), and
	// InstalledAt when; from the state, empty for a machine ump never
	// installed.
	OS          string     `json:"os,omitempty" yaml:"os,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty" yaml:"installed_at,omitempty"`
	// Groups are the Ansible groups ump put the machine in, as in the
	// inventory: common first, then those of ump server reinstall
	// --group and ump server set --group; from the state, for a machine
	// ump installed.
	Groups []string `json:"groups,omitempty" yaml:"groups,omitempty"`
	// Profile and Layout are the profile and the disk layout of the
	// installation ump made last (inline: the profile carried its own
	// layout); from the state, like OS.
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`
	Layout  string `json:"layout,omitempty" yaml:"layout,omitempty"`
	// PrivateNetworks are the machine's addresses in the private networks
	// ump gave it, in their order, and PrivateNetwork and PrivateAddress
	// those of the first; from the state.
	PrivateNetwork  string    `json:"private_network,omitempty" yaml:"private_network,omitempty"`
	PrivateAddress  string    `json:"private_address,omitempty" yaml:"private_address,omitempty"`
	PrivateNetworks []Private `json:"private_networks,omitempty" yaml:"private_networks,omitempty"`
}

// Private is a machine's address in a private network of ump's, with
// the network's CIDR as it was when the address was given.
type Private struct {
	Network string `json:"network" yaml:"network"`
	CIDR    string `json:"cidr" yaml:"cidr"`
	Address string `json:"address" yaml:"address"`
}

// Price is what a machine costs per period.
type Price struct {
	Amount   float64 `json:"amount" yaml:"amount"`
	Currency string  `json:"currency" yaml:"currency"`
	// Period is month or hour.
	Period string `json:"period" yaml:"period"`
	// Source is provider (from the API) or manual (ump server set);
	// omitted in the configuration file.
	Source string `json:"source,omitempty" yaml:"-"`
	// Note says what the amount is, e.g. "net" or "gross", when known.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
}

// String renders a price for a table: 4.76 EUR/mo.
func (p *Price) String() string {
	if p == nil {
		return ""
	}
	unit := map[string]string{"month": "mo", "hour": "h"}[p.Period]
	if unit == "" {
		unit = p.Period
	}
	return strconv.FormatFloat(p.Amount, 'f', 2, 64) + " " + p.Currency + "/" + unit
}

// ManualServer is a server the user added by hand (ump server add): no
// API knows it, so this is all ump knows.
type ManualServer struct {
	IPv4     string `json:"ipv4,omitempty" yaml:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
	Location string `json:"location,omitempty" yaml:"location,omitempty"`
	Product  string `json:"product,omitempty" yaml:"product,omitempty"`
	// Kind is bare-metal (the default) or cloud.
	Kind Kind `json:"kind,omitempty" yaml:"kind,omitempty"`
	// Note is free text: where the machine is, who rents it, how to
	// recover it.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
}

// Ref returns the unambiguous machine reference, "provider/id".
func (m Machine) Ref() string {
	return m.Provider + "/" + m.ID
}

// Matches reports whether ref refers to this machine. A reference is
// "provider/id", a bare ID, the name, or an IP address. IP addresses also
// match when the machine's IPv6 field is a prefix that contains them.
func (m Machine) Matches(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if ref == m.Ref() || ref == m.ID || strings.EqualFold(ref, m.Name) {
		return true
	}
	addr, err := netip.ParseAddr(ref)
	if err != nil {
		return false
	}
	if m.IPv4 != "" {
		if a, err := netip.ParseAddr(m.IPv4); err == nil && a == addr {
			return true
		}
	}
	if m.IPv6 != "" {
		if p, err := netip.ParsePrefix(m.IPv6); err == nil && p.Contains(addr) {
			return true
		}
		if a, err := netip.ParseAddr(m.IPv6); err == nil && a == addr {
			return true
		}
	}
	return false
}

// SortMachines orders machines by provider, account and name, so listings
// are stable between runs.
func SortMachines(ms []Machine) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

// Capability names what a provider can do. The CLI checks them with type
// assertions on the interfaces below; the names are for display.
type Capability string

const (
	CapInventory Capability = "inventory"
)

// Provider is implemented by every provider instance (a configured account).
type Provider interface {
	// Type is the provider type name, e.g. "hetzner-robot".
	Type() string
	// Account is the name of the configured account.
	Account() string
	// Capabilities lists what the provider can do.
	Capabilities() []Capability
}

// Inventory is implemented by providers that can list machines.
type Inventory interface {
	Provider
	List(ctx context.Context) ([]Machine, error)
}

// Field describes one configuration setting of a provider type. The wizard
// prompts for fields in order; the config validator checks required ones.
type Field struct {
	// Key is the setting name in config.yaml.
	Key string
	// Label is the prompt shown by the wizard.
	Label string
	// Help is a one-line explanation shown with the prompt.
	Help string
	// Secret marks credentials: hidden input, stored as a secret reference.
	Secret bool
	// Required fields must be present and non-empty.
	Required bool
	// Default is used when the user enters nothing.
	Default string
	// Choices restricts the value to a fixed set (the wizard shows a menu).
	// The value may still be something else when entered non-interactively,
	// so types that accept free-form values must validate themselves.
	Choices []string
	// Hidden fields are accepted in config.yaml but not prompted for
	// (e.g. an API base URL override for tests and proxies).
	Hidden bool
	// Deferrable marks a required field the wizard may leave empty because
	// Type.Complete fills it in (e.g. a key the provider issues on request).
	Deferrable bool
}

// Options are shared dependencies handed to a provider constructor.
type Options struct {
	// HTTPClient is used for every API call. nil means a default client
	// with a timeout.
	HTTPClient *http.Client
	// UserAgent is sent with every request.
	UserAgent string
	// Logf receives one line per API call when verbose output is on.
	Logf func(format string, args ...any)
	// Manual holds the servers of a manual account, by name (the manual
	// provider type lists them; other types ignore it).
	Manual map[string]ManualServer
}

func (o Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: defaultTransport}
}

// defaultTransport is http.DefaultTransport with a connection that must
// be up in 10 seconds (not 30): without a network, a command gives up on
// an API soon and says so, while a slow answer still has the client's
// whole minute.
var defaultTransport = func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	return t
}()

// Client returns the HTTP client to use: a request whose connection fails
// in the TLS handshake is sent again (handshakeRetry), and with verbose
// output on every request is logged (each try). Authorization headers are
// never logged.
func (o Options) Client() *http.Client {
	cc := *o.httpClient()
	rt := cc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if o.Logf != nil {
		rt = &loggingTransport{next: rt, logf: o.Logf}
	}
	cc.Transport = &handshakeRetry{next: rt, waits: handshakeWaits}
	return &cc
}

type loggingTransport struct {
	next http.RoundTripper
	logf func(format string, args ...any)
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		t.logf("%s %s: %v (%s)", req.Method, req.URL.Redacted(), err, time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	t.logf("%s %s: %s (%s)", req.Method, req.URL.Redacted(), resp.Status, time.Since(start).Round(time.Millisecond))
	return resp, nil
}

// Prompter is the small interactive surface a provider type may use during
// the setup wizard (see Type.Complete). The CLI implements it on a TTY.
type Prompter interface {
	Printf(format string, args ...any)
	// Line asks for a line of input; def is returned for an empty answer.
	Line(label, def string) (string, error)
	// Secret asks for a line of input without echo.
	Secret(label string) (string, error)
	// Confirm asks a yes/no question.
	Confirm(label string, def bool) (bool, error)
}

// Type describes a provider type: how it is configured and how an instance
// is built. Provider packages register their Type.
type Type struct {
	// Name is the type name used in config.yaml, e.g. "hetzner-robot".
	Name string
	// Kind says what the type lists.
	Kind Kind
	// Description is one line for menus and `ump config view`.
	Description string
	// Capabilities documents what instances of this type can do; the
	// instances report the same through Provider.Capabilities.
	Capabilities []Capability
	// Fields are the settings, in prompting order.
	Fields []Field
	// New builds a provider for an account. settings holds every field
	// with its secret references already resolved; required fields are
	// guaranteed to be present.
	New func(account string, settings map[string]string, opts Options) (Provider, error)
	// Complete, when set, runs in the wizard after all fields were asked
	// and may fill in settings interactively (for example by requesting
	// an API credential and handing the user a validation URL).
	Complete func(ctx context.Context, ui Prompter, settings map[string]string, opts Options) error
}

// Field returns the field with the given key.
func (t Type) Field(key string) (Field, bool) {
	for _, f := range t.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Validate checks settings against the type's fields: required fields are
// present, choices are respected, and no unknown keys are used. It returns
// one error per problem.
func (t Type) Validate(settings map[string]string) []error {
	var errs []error
	for _, f := range t.Fields {
		v, ok := settings[f.Key]
		if f.Required && (!ok || strings.TrimSpace(v) == "") {
			errs = append(errs, &FieldError{Type: t.Name, Key: f.Key, Msg: "is required"})
			continue
		}
		if len(f.Choices) > 0 && ok && v != "" && !contains(f.Choices, v) && !isURL(v) {
			errs = append(errs, &FieldError{Type: t.Name, Key: f.Key, Msg: "must be one of " + strings.Join(f.Choices, ", ")})
		}
	}
	for k := range settings {
		if _, ok := t.Field(k); !ok {
			errs = append(errs, &FieldError{Type: t.Name, Key: k, Msg: "is not a setting of this provider type"})
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

// FieldError reports a problem with one setting.
type FieldError struct {
	Type string
	Key  string
	Msg  string
}

func (e *FieldError) Error() string {
	return e.Type + ": " + e.Key + " " + e.Msg
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

var (
	mu       sync.RWMutex
	registry = map[string]Type{}
)

// Register adds a provider type. It panics on a duplicate name, which is a
// programming error.
func Register(t Type) {
	mu.Lock()
	defer mu.Unlock()
	if t.Name == "" || t.New == nil {
		panic("provider: Register: type needs a Name and a New function")
	}
	if _, dup := registry[t.Name]; dup {
		panic("provider: Register: duplicate type " + t.Name)
	}
	registry[t.Name] = t
}

// Lookup returns the provider type with the given name.
func Lookup(name string) (Type, bool) {
	mu.RLock()
	defer mu.RUnlock()
	t, ok := registry[name]
	return t, ok
}

// Types returns all registered provider types, sorted by name.
func Types() []Type {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Type, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TypeNames returns the registered type names, sorted.
func TypeNames() []string {
	ts := Types()
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return names
}

// Build validates settings for the named type and constructs the provider.
func Build(typeName, account string, settings map[string]string, opts Options) (Provider, error) {
	t, ok := Lookup(typeName)
	if !ok {
		return nil, &UnknownTypeError{Name: typeName}
	}
	if errs := t.Validate(settings); len(errs) > 0 {
		return nil, errs[0]
	}
	return t.New(account, settings, opts)
}

// UnknownTypeError is returned for a provider type that is not registered.
type UnknownTypeError struct{ Name string }

func (e *UnknownTypeError) Error() string {
	return "unknown provider type " + e.Name + " (known: " + strings.Join(TypeNames(), ", ") + ")"
}

// APIError is returned by providers when the provider's API refused or
// failed a request. The CLI maps it to exit code 4.
type APIError struct {
	Provider string
	Status   int    // HTTP status, 0 if the request never completed
	Code     string // provider-specific error code, if any
	Message  string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.Provider)
	b.WriteString(": ")
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString("API request failed")
	}
	if e.Code != "" || e.Status != 0 {
		b.WriteString(" (")
		if e.Code != "" {
			b.WriteString(e.Code)
			if e.Status != 0 {
				b.WriteString(", ")
			}
		}
		if e.Status != 0 {
			b.WriteString("HTTP ")
			b.WriteString(strconv.Itoa(e.Status))
		}
		b.WriteString(")")
	}
	return b.String()
}
