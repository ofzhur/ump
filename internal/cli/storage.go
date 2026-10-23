package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/ofzhur/ump/internal/backend"
)

// Host files in a storage: a file of the repository's inventory/ that
// says plugin: ofzhur.ansible.host and url: URL (inventory/hosts.yml)
// points the collection's inventory plugin to host files in a storage,
// one object per host under the URL's prefix, beside the facts
// (ofzhur.ansible.facts). ump writes the host files there instead of
// into inventory/hosts/. The storage's options and credentials are the
// collection's: those of the pointer that are no credentials, else
// ANSIBLE_OFZHUR_STORAGE_*, else [ofzhur_storage] of the repository's
// ansible.cfg, resolved as Ansible resolves them, so that ump and
// Ansible always reach the same storage with the same credentials;
// never those of ump's state.

const (
	// hostPlugin is the collection's inventory plugin of the host files.
	hostPlugin = collection + ".host"
	// storageSection is the section of ansible.cfg with the storage's
	// options, and storageEnv the prefix of their variables.
	storageSection = "ofzhur_storage"
	storageEnv     = "ANSIBLE_OFZHUR_STORAGE_"
)

// pointerKeys are the keys of a pointer: its URL and the options of the
// storage that are no credentials.
var pointerKeys = []string{"plugin", "url", "endpoint", "region", "vault_namespace", "ca_file", "ssh_args"}

// storagePathOptions are the options that are paths; the others are
// strings.
var storagePathOptions = []string{"access_key_file", "secret_key_file", "session_token_file", "consul_token_file", "vault_token_file", "ca_file"}

// storageDefaults are the defaults of the options that have one.
var storageDefaults = map[string]string{"region": "us-east-1", "ssh_args": ""}

// storagePointer is a file of inventory/ that points to host files in a
// storage.
type storagePointer struct {
	// file is the pointer, relative to the repository.
	file string
	url  string
	// options are its own, that win over the environment and ansible.cfg.
	options map[string]string
}

// inventoryFiles are what the files directly in inventory/ make of the
// inventory: the files that make ump the inventory, and the pointers to
// host files in a storage. Ansible skips dot names, and so does ump.
func inventoryFiles(dir string) (plugins []string, pointers []string) {
	entries, err := os.ReadDir(filepath.Join(dir, "inventory"))
	if err != nil {
		return nil, nil
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !isYAMLFile(n) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "inventory", n))
		if err != nil {
			continue
		}
		var f map[string]any
		if yaml.Unmarshal(data, &f) != nil {
			continue
		}
		switch p, _ := f["plugin"].(string); {
		case p == umpPlugin:
			plugins = append(plugins, "inventory/"+n)
		case p == hostPlugin && hasKey(f, "url"):
			pointers = append(pointers, "inventory/"+n)
		}
	}
	return plugins, pointers
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// readPointer reads a pointer as the collection's plugin does: its keys,
// its URL; a credential in it is refused, it goes into ansible.cfg. A
// relative ca_file is relative to the pointer.
func readPointer(dir, file string) (*storagePointer, error) {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return nil, err
	}
	var f map[string]any
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", file, err)
	}
	var unknown []string
	for k := range f {
		if !slices.Contains(pointerKeys, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, fmt.Errorf("%s: unknown key(s) %s; a pointer to host files in a storage has %s; credentials go into [%s] of ansible.cfg", file, strings.Join(unknown, ", "), strings.Join(pointerKeys, ", "), storageSection)
	}
	p := &storagePointer{file: file, options: map[string]string{}}
	if p.url, _ = f["url"].(string); !strings.Contains(p.url, "://") {
		return nil, fmt.Errorf("%s: url must be the URL of a storage (s3://, r2://, consul://, vault://, ssh://, file://)", file)
	}
	for _, k := range pointerKeys[2:] {
		switch v := f[k].(type) {
		case nil:
		case string:
			if k == "ca_file" {
				if v = expandUser(v); !filepath.IsAbs(v) {
					v = filepath.Join(dir, filepath.Dir(file), v)
				}
			}
			p.options[k] = v
		default:
			return nil, fmt.Errorf("%s: %s must be a string", file, k)
		}
	}
	return p, nil
}

// storageConfig resolves the options of the storage of the host files
// as Ansible resolves those of the collection's plugins (in the order of
// the doc fragment ofzhur.ansible.storage): the pointer's, else the
// environment (ANSIBLE_OFZHUR_STORAGE_NAME, also when empty), else
// [ofzhur_storage] of ansible.cfg, else the default. A path is
// expanded ($VAR, ~) and taken relative to ansible.cfg when it comes
// from there, else relative to where Ansible runs, the repository.
type storageConfig struct {
	cfg     string
	workdir string
	pointer map[string]string
	ini     *iniFile
}

func newStorageConfig(repo string, p *storagePointer) (*storageConfig, error) {
	c := &storageConfig{cfg: filepath.Join(repo, "ansible.cfg"), workdir: repo, pointer: p.options}
	data, err := os.ReadFile(c.cfg)
	if err != nil {
		return nil, err
	}
	if c.ini, err = parseINI(data); err != nil {
		return nil, fmt.Errorf("ansible.cfg: %v", err)
	}
	return c, nil
}

// option is the value of an option; empty when it is not set.
func (c *storageConfig) option(name string) (string, error) {
	isPath := slices.Contains(storagePathOptions, name)
	if v, ok := c.pointer[name]; ok {
		if isPath {
			return resolvePath(v, c.workdir, c.workdir), nil
		}
		return v, nil
	}
	if v, ok := os.LookupEnv(storageEnv + strings.ToUpper(name)); ok {
		if isPath {
			return resolvePath(v, c.workdir, c.workdir), nil
		}
		return v, nil
	}
	if v, ok := c.ini.get(storageSection, name); ok {
		if isPath {
			return resolvePath(v, filepath.Dir(c.cfg), c.workdir), nil
		}
		// ansible-core 2.19 and later take the quotes off, earlier
		// versions keep them
		if isQuoted(v) {
			return "", fmt.Errorf("%s in [%s] of ansible.cfg is in quotes, which ansible-core 2.19 and later take off and earlier versions keep: leave them out", name, storageSection)
		}
		return v, nil
	}
	return storageDefaults[name], nil
}

// credential is a credential: the content of its file where NAME_file
// is set (an empty file is none), else its value.
func (c *storageConfig) credential(name string) (string, error) {
	f, err := c.option(name + "_file")
	if err != nil {
		return "", err
	}
	if f == "" {
		return c.option(name)
	}
	data, err := os.ReadFile(expandUser(f))
	if err != nil {
		return "", fmt.Errorf("%s_file: %s: %s", name, pathReason(err), f)
	}
	return strings.TrimSpace(string(data)), nil
}

// storageMissing says where the credentials of names are set.
func storageMissing(names ...string) string {
	var files, values, env []string
	for _, n := range names {
		files = append(files, n+"_file")
		values = append(values, n)
		env = append(env, storageEnv+strings.ToUpper(n)+"_FILE")
	}
	return fmt.Sprintf("%s (or %s) in [%s] of ansible.cfg, or %s; ump takes them from there, as Ansible does, never those of its state", strings.Join(files, " and "), strings.Join(values, " and "), storageSection, strings.Join(env, " and "))
}

// pathReason is why a file could not be read, without its path.
func pathReason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// openStorage opens the storage of a pointer, for the host files; an
// error is the configuration's, exit status 2.
func (a *app) openStorage(repo string, p *storagePointer) (backend.Objects, error) {
	var o backend.Objects
	c, err := newStorageConfig(repo, p)
	if err == nil {
		o, err = backend.OpenStorage(p.url, backend.StorageOptions{Option: c.option, Credential: c.credential, Missing: storageMissing, SSH: a.storageSSH})
	}
	if err != nil {
		return nil, withExit(exitUsage, fmt.Errorf("%s: %s: %v", p.file, backend.StorageLabel(p.url), err))
	}
	return o, nil
}

// storageSSH runs a command on the machine of an ssh:// storage with the
// user's ssh client, as the collection does: in batch mode, with the
// arguments of ssh_args before the target.
func (a *app) storageSSH(target, port string, args []string) backend.SSHRunner {
	return func(ctx context.Context, command string, stdin []byte) ([]byte, []byte, int, error) {
		argv := append([]string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}, args...)
		if port != "" {
			argv = append(argv, "-p", port)
		}
		argv = append(argv, "--", target, command)
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		var out, errb bytes.Buffer
		code, err := a.sshExec(ctx, argv, bytes.NewReader(stdin), &out, &errb)
		if ctx.Err() != nil && err == nil {
			err = errors.New("no answer within 60 seconds")
		}
		return out.Bytes(), errb.Bytes(), code, err
	}
}

// resolvePath is a path as Ansible resolves one of type path: {{CWD}}
// where Ansible runs (cwd), $VAR and ${VAR} expanded (an unknown one
// stays), then ~, and relative to basedir.
func resolvePath(p, basedir, cwd string) string {
	p = strings.ReplaceAll(p, "{{CWD}}", cwd)
	p = expandUser(expandVars(p))
	if !filepath.IsAbs(p) {
		p = filepath.Join(basedir, p)
	}
	return filepath.Clean(p)
}

var varRef = regexp.MustCompile(`\$([A-Za-z0-9_]+|\{[^}]*\})`)

// expandVars expands $VAR and ${VAR} as Python's os.path.expandvars: an
// unknown variable stays as it is.
func expandVars(p string) string {
	return varRef.ReplaceAllStringFunc(p, func(ref string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(ref, "$"), "{"), "}")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return ref
	})
}

// expandUser expands ~ and ~USER at the start of a path as Python's
// os.path.expanduser: ~ is HOME; an unknown user stays as it is.
func expandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	i := strings.IndexByte(p, '/')
	if i < 0 {
		i = len(p)
	}
	var home string
	if i == 1 {
		var ok bool
		if home, ok = os.LookupEnv("HOME"); !ok {
			u, err := user.Current()
			if err != nil {
				return p
			}
			home = u.HomeDir
		}
	} else {
		u, err := user.Lookup(p[1:i])
		if err != nil {
			return p
		}
		home = u.HomeDir
	}
	if out := strings.TrimRight(home, "/") + p[i:]; out != "" {
		return out
	}
	return "/"
}

// isQuoted is Ansible's is_quoted: the same quote first and last.
func isQuoted(s string) bool {
	return len(s) > 1 && s[0] == s[len(s)-1] && (s[0] == '"' || s[0] == '\'') && s[len(s)-2] != '\\'
}

// iniFile is an ini file as Python's configparser reads ansible.cfg for
// Ansible: keys in lower case, ";" after a blank starts a comment, an
// indented line continues a value, and [DEFAULT] is under every section.
type iniFile struct {
	sections map[string]map[string][]string
	defaults map[string][]string
}

var (
	iniSection = regexp.MustCompile(`^\[(.+)\]`)
	iniOption  = regexp.MustCompile(`^(.*?)\s*[=:]\s*(.*)$`)
)

func parseINI(data []byte) (*iniFile, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("not UTF-8")
	}
	f := &iniFile{sections: map[string]map[string][]string{}, defaults: map[string][]string{}}
	var cur map[string][]string
	var section, option string
	seen := map[string]bool{}
	indent := 0
	for n, line := range strings.Split(string(data), "\n") {
		// comments: a line that starts with # or ;, and ; after a blank
		start := -1
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			start = 0
		} else {
			for i := 0; i < len(line); i++ {
				if line[i] == ';' && (i == 0 || isSpace(line[i-1])) {
					start = i
					break
				}
			}
		}
		value := line
		if start >= 0 {
			value = line[:start]
		}
		if value = strings.TrimSpace(value); value == "" {
			// an empty line within a value is part of it
			if start < 0 && cur != nil && option != "" {
				cur[option] = append(cur[option], "")
			}
			continue
		}
		level := len(line) - len(strings.TrimLeftFunc(line, unicode.IsSpace))
		if cur != nil && option != "" && level > indent {
			cur[option] = append(cur[option], value)
			continue
		}
		indent = level
		if m := iniSection.FindStringSubmatch(value); m != nil {
			section, option = m[1], ""
			switch {
			case section == "DEFAULT":
				cur = f.defaults
			case f.sections[section] != nil:
				return nil, fmt.Errorf("line %d: section %s again", n+1, section)
			default:
				cur = map[string][]string{}
				f.sections[section] = cur
			}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: no section header", n+1)
		}
		m := iniOption.FindStringSubmatch(value)
		if m == nil || m[1] == "" {
			return nil, fmt.Errorf("line %d: %q is no option", n+1, strings.TrimSpace(line))
		}
		option = strings.ToLower(strings.TrimRightFunc(m[1], unicode.IsSpace))
		if seen[section+"\x00"+option] {
			return nil, fmt.Errorf("line %d: option %s again in section %s", n+1, option, section)
		}
		seen[section+"\x00"+option] = true
		cur[option] = []string{m[2]}
	}
	return f, nil
}

// get returns the value of a key of a section, or of [DEFAULT] when the
// section exists without it.
func (f *iniFile) get(section, key string) (string, bool) {
	s, ok := f.sections[section]
	if !ok {
		return "", false
	}
	v, ok := s[strings.ToLower(key)]
	if !ok {
		if v, ok = f.defaults[strings.ToLower(key)]; !ok {
			return "", false
		}
	}
	return strings.TrimRightFunc(strings.Join(v, "\n"), unicode.IsSpace), true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v'
}
