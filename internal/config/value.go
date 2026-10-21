package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Value is a configuration setting: either a plain string or a reference
// to a secret kept elsewhere. In YAML a reference is a one-key mapping:
//
//	password: { env: HROBOT_PASS }
//	password: { file: ~/.config/ump/secrets/main.password }
//	password: { command: "pass show robot/password" }
type Value struct {
	Plain   string
	Env     string
	File    string
	Command string
}

// Plain returns a plain value.
func PlainValue(s string) Value { return Value{Plain: s} }

// IsRef reports whether the value is a secret reference.
func (v Value) IsRef() bool {
	return v.Env != "" || v.File != "" || v.Command != ""
}

// IsZero reports whether the value is unset.
func (v Value) IsZero() bool {
	return v == Value{}
}

// String renders the value for display: references in the --set syntax
// (env:NAME, file:PATH, command:CMD), plain values as they are.
func (v Value) String() string {
	switch {
	case v.Env != "":
		return "env:" + v.Env
	case v.File != "":
		return "file:" + v.File
	case v.Command != "":
		return "command:" + v.Command
	}
	return v.Plain
}

// ParseValue parses the --set syntax: "env:NAME", "file:PATH",
// "command:CMD" (or "cmd:CMD") are references; anything else is plain.
func ParseValue(s string) Value {
	for _, p := range []string{"env:", "file:", "command:", "cmd:"} {
		if strings.HasPrefix(s, p) {
			rest := strings.TrimSpace(strings.TrimPrefix(s, p))
			switch p {
			case "env:":
				return Value{Env: rest}
			case "file:":
				return Value{File: rest}
			default:
				return Value{Command: rest}
			}
		}
	}
	return Value{Plain: s}
}

// Resolve returns the actual value: the environment variable, the file's
// content without trailing newlines, the command's standard output without
// trailing newlines, or the plain value.
func (v Value) Resolve() (string, error) {
	switch {
	case v.Env != "":
		s, ok := os.LookupEnv(v.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", v.Env)
		}
		return s, nil
	case v.File != "":
		b, err := readSecretFile(v.File)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case v.Command != "":
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", v.Command)
		} else {
			cmd = exec.Command("sh", "-c", v.Command)
		}
		cmd.Stdin = nil
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				return "", fmt.Errorf("command %q failed: %s", v.Command, msg)
			}
			return "", fmt.Errorf("command %q failed: %w", v.Command, err)
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	return v.Plain, nil
}

// MarshalYAML renders references as flow mappings ({ env: NAME }) and
// plain values as scalars.
func (v Value) MarshalYAML() (any, error) {
	if !v.IsRef() {
		return v.Plain, nil
	}
	key, val := "env", v.Env
	switch {
	case v.File != "":
		key, val = "file", v.File
	case v.Command != "":
		key, val = "command", v.Command
	}
	return &yaml.Node{
		Kind:  yaml.MappingNode,
		Style: yaml.FlowStyle,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: key},
			{Kind: yaml.ScalarNode, Value: val},
		},
	}, nil
}

// UnmarshalYAML accepts a scalar (plain value) or a one-key mapping with
// env, file or command.
func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*v = Value{}
			return nil
		}
		*v = Value{Plain: n.Value}
		return nil
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return errors.New("a secret reference needs exactly one of env, file or command")
		}
		key, val := n.Content[0].Value, n.Content[1].Value
		if n.Content[1].Kind != yaml.ScalarNode || val == "" {
			return fmt.Errorf("secret reference %s needs a value", key)
		}
		switch key {
		case "env":
			*v = Value{Env: val}
		case "file":
			*v = Value{File: val}
		case "command":
			*v = Value{Command: val}
		default:
			return fmt.Errorf("unknown secret reference %q (use env, file or command)", key)
		}
		return nil
	}
	return errors.New("a setting must be a string or a secret reference")
}

// readSecretFile reads the file of a file reference (see FilePath).
func readSecretFile(ref string) ([]byte, error) {
	return os.ReadFile(FilePath(ref))
}

// FilePath is the file a file reference reads. A relative path is in
// the home (secrets/NAME, as the wizard writes them), so it travels with
// the state to every machine. A path into a home as one machine sees it
// (~/.config/ump/secrets/NAME before a migration, or a backend's mirror
// under ~/.cache/ump/homes/) that is not there is the same file in the
// home of this run, where the state brought it.
func FilePath(ref string) string {
	p := ExpandHome(ref)
	if !filepath.IsAbs(p) {
		return filepath.Join(UserConfigDir(), p)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		return p
	}
	if rel, ok := inSomeHome(p); ok {
		if q := filepath.Join(UserConfigDir(), rel); fileExists(q) {
			return q
		}
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// inSomeHome gives p relative to the home it is in: the local
// directory, or a backend's mirror.
func inSomeHome(p string) (string, bool) {
	if rel, err := filepath.Rel(LocalDir(), p); err == nil && filepath.IsLocal(rel) {
		return rel, true
	}
	if rel, err := filepath.Rel(filepath.Join(CacheDir(), "homes"), p); err == nil && filepath.IsLocal(rel) {
		if _, rest, ok := strings.Cut(filepath.ToSlash(rel), "/"); ok && rest != "" {
			return filepath.FromSlash(rest), true
		}
	}
	return "", false
}
