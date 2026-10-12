package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// CapDescribe is implemented by providers that report details of one
// machine (see Describer).
const CapDescribe Capability = "describe"

// Describer is implemented by providers that can report everything they
// know about one machine, for `ump describe server`.
type Describer interface {
	Provider
	Describe(ctx context.Context, m Machine) (*Details, error)
}

// Details is what `ump describe server` shows: the machine, plus
// provider-specific facts grouped in named sections in display order.
//
// In JSON and YAML the machine fields come first and the sections follow
// under "details", each as {"name": ..., "fields": {...}}.
type Details struct {
	Machine  `yaml:",inline"`
	Sections []Section `json:"details" yaml:"details"`
}

// Section is a named group of facts.
type Section struct {
	Name  string
	Items []Item
}

// Item is one fact. Key is a stable snake_case identifier used in JSON
// and YAML; Label is the display name, derived from Key when empty.
// Value is a string, bool, number, Bytes, []string or nil.
type Item struct {
	Key   string
	Label string
	Value any
}

// Add appends a fact and returns the section, for chaining.
func (s *Section) Add(key string, value any) *Section {
	s.Items = append(s.Items, Item{Key: key, Value: value})
	return s
}

// AddLabeled appends a fact with an explicit display label.
func (s *Section) AddLabeled(key, label string, value any) *Section {
	s.Items = append(s.Items, Item{Key: key, Label: label, Value: value})
	return s
}

var acronyms = map[string]string{
	"id": "ID", "ip": "IP", "ips": "IPs", "ipv4": "IPv4", "ipv6": "IPv6", "dns": "DNS", "ptr": "PTR",
	"os": "OS", "cpu": "CPU", "ram": "RAM", "mac": "MAC", "ssh": "SSH", "vnc": "VNC", "wol": "WoL",
	"iso": "ISO", "url": "URL", "api": "API", "raid": "RAID", "uefi": "UEFI", "gb": "GB", "mb": "MB",
	"tb": "TB", "mbps": "Mbps", "vcpus": "vCPUs", "wan": "WAN", "lan": "LAN", "cpanel": "cPanel",
}

// DisplayLabel returns the label, or one derived from the key:
// "paid_until" becomes "Paid until", "ipv6_network" becomes "IPv6 network".
func (i Item) DisplayLabel() string {
	if i.Label != "" {
		return i.Label
	}
	words := strings.Split(i.Key, "_")
	for n, w := range words {
		if a, ok := acronyms[w]; ok {
			words[n] = a
		} else if n == 0 && w != "" {
			words[n] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// MarshalJSON renders the section as {"name": ..., "fields": {...}} with
// the fields in display order.
func (s Section) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	name, err := json.Marshal(s.Name)
	if err != nil {
		return nil, err
	}
	b.Write(name)
	b.WriteString(`,"fields":{`)
	for n, it := range s.Items {
		if n > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(it.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(it.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteString("}}")
	return b.Bytes(), nil
}

// MarshalYAML renders the section like MarshalJSON, keeping the order.
func (s Section) MarshalYAML() (any, error) {
	fields := &yaml.Node{Kind: yaml.MappingNode}
	for _, it := range s.Items {
		var v yaml.Node
		if err := v.Encode(it.Value); err != nil {
			return nil, err
		}
		fields.Content = append(fields.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: it.Key}, &v)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name"},
		{Kind: yaml.ScalarNode, Value: s.Name},
		{Kind: yaml.ScalarNode, Value: "fields"},
		fields,
	}}, nil
}

// ErrorSection is a section that reports why a group of facts could not
// be fetched, so a partial description is still honest.
func ErrorSection(name string, err error) Section {
	return Section{Name: name, Items: []Item{{Key: "error", Value: err.Error()}}}
}

// Bytes is a size in bytes. It prints human-readable in text output
// (1024-based units, as the providers quote them) and as a plain number
// in JSON and YAML.
type Bytes int64

// String implements fmt.Stringer.
func (b Bytes) String() string {
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(int64(b), 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := int64(b) / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return fmt.Sprintf("%s %cB", s, "KMGTP"[exp])
}
