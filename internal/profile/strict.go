package profile

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeStrict decodes a node like Node.Decode, but refuses keys that
// name no field, as the decoder with KnownFields does for a whole file:
// Node.Decode drops that setting, so a typo inside an inline firewall,
// user, layout or userdata would otherwise vanish without a word.
func decodeStrict(n *yaml.Node, v any) error {
	if err := knownFields(n, reflect.TypeOf(v), true); err != nil {
		return err
	}
	return n.Decode(v)
}

var unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// knownFields walks the node along the type and names the first key
// that no field takes. A type with its own UnmarshalYAML checks itself
// (top is the type being decoded by one).
func knownFields(n *yaml.Node, t reflect.Type, top bool) error {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if !top && reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return nil
		}
		fields, open := structFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			ft, ok := fields[k.Value]
			if !ok {
				if open || k.Value == "<<" {
					continue
				}
				return fmt.Errorf("line %d: field %s not found in type %s", k.Line, k.Value, t)
			}
			if err := knownFields(n.Content[i+1], ft, false); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			return nil
		}
		for _, c := range n.Content {
			if err := knownFields(c, t.Elem(), false); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 1; i < len(n.Content); i += 2 {
			if err := knownFields(n.Content[i], t.Elem(), false); err != nil {
				return err
			}
		}
	}
	return nil
}

// structFields maps the YAML keys of a struct to their types, inline
// structs included; open says an inline map takes any other key.
func structFields(t reflect.Type) (map[string]reflect.Type, bool) {
	out := map[string]reflect.Type{}
	open := false
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if strings.Contains(","+opts+",", ",inline,") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Map {
				open = true
				continue
			}
			inner, o := structFields(ft)
			for k, v := range inner {
				out[k] = v
			}
			open = open || o
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out, open
}
