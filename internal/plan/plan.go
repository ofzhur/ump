// Package plan has the values of reinstall's flags for one server as a
// file gives them, under the flags' names: a multiinstall plan for its
// defaults and for each of its servers, pxe.yaml for each machine. A
// value not given is nil, so that values layer: a server's over the
// defaults (Over).
package plan

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Values are the values of reinstall's flags for one server, under the
// flags' names (Keys); nil is not given.
type Values struct {
	Profile     *string `yaml:"profile"`
	Layout      *string `yaml:"layout"`
	Firewall    *string `yaml:"firewall"`
	OS          *string `yaml:"os"`
	Hostname    *string `yaml:"hostname"`
	PrivateNet  List    `yaml:"private-network"`
	PrivateIP   List    `yaml:"private-ip"`
	Timezone    *string `yaml:"timezone"`
	Disks       List    `yaml:"disks"`
	SSHKey      List    `yaml:"ssh-key"`
	Set         List    `yaml:"set"`
	User        List    `yaml:"user"`
	Userdata    List    `yaml:"userdata"`
	Group       List    `yaml:"group"`
	IPv6        *bool   `yaml:"ipv6"`
	Mitigations *bool   `yaml:"mitigations"`
	Via         *string `yaml:"via"`
	Storage     *string `yaml:"storage"`
}

// List is the value of a repeatable flag: a list, or one value.
type List []string

func (l *List) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = List{n.Value}
		return nil
	case yaml.SequenceNode:
		out := List{}
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a list of values, one per line or in [ ]", c.Line)
			}
			out = append(out, c.Value)
		}
		*l = out
		return nil
	}
	return fmt.Errorf("line %d: a value, or a list of values", n.Line)
}

// Over is the values of a server over v, the defaults: each it gives
// replaces theirs.
func (v Values) Over(server Values) Values {
	out := v
	dst, src := reflect.ValueOf(&out).Elem(), reflect.ValueOf(server)
	for i := range src.NumField() {
		if !src.Field(i).IsNil() {
			dst.Field(i).Set(src.Field(i))
		}
	}
	return out
}

// Keys are the names of the values, in their order.
func Keys() []string {
	t := reflect.TypeFor[Values]()
	keys := make([]string, t.NumField())
	for i := range t.NumField() {
		keys[i], _, _ = strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
	}
	return keys
}
