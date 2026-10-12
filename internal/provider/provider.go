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
}

func (o Options) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Client returns the HTTP client to use, with request logging attached when
// verbose output is on. Authorization headers are never logged.
func (o Options) Client() *http.Client {
	c := o.httpClient()
	if o.Logf == nil {
		return c
	}
	cc := *c
	rt := cc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	cc.Transport = &loggingTransport{next: rt, logf: o.Logf}
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
