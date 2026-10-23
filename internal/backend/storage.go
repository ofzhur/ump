package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A storage holds the objects of the Ansible collection ofzhur.ansible
// (its module_utils/storage.py): the host files of an inventory, named
// objects directly under a prefix, which ump writes and the collection's
// inventory plugin ofzhur.ansible.host reads. Its URLs are those of the
// backends but http(s)://, its options and credentials the collection's
// own (StorageOptions), and an object is kept as the collection keeps
// it: as it is, and in Vault as the field json of a secret.

// Objects is a storage: objects by name under its prefix. A name never
// contains "/".
type Objects interface {
	// Get returns an object; ok is false when there is none.
	Get(ctx context.Context, name string) (data []byte, ok bool, err error)
	// Put creates or replaces an object as a whole.
	Put(ctx context.Context, name string, data []byte) error
	// Remove deletes an object; a missing one is no error.
	Remove(ctx context.Context, name string) error
	// All returns the host files by name: the objects directly under the
	// prefix whose names end in .yml or .yaml, but for dot names.
	All(ctx context.Context) (map[string][]byte, error)
	// String is the storage's URL without what may be secret.
	String() string
}

// hostFileName says whether All takes an object of that name.
func hostFileName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.Contains(name, "/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml"))
}

// StorageOptions are the options of a storage, by the names of the
// collection's doc fragment ofzhur.ansible.storage.
type StorageOptions struct {
	// Option returns an option that is no credential: endpoint, region,
	// vault_namespace, ca_file (a path), ssh_args; empty when not set.
	Option func(name string) (string, error)
	// Credential returns a credential: access_key, secret_key,
	// session_token, consul_token, vault_token; the content of its file
	// where NAME_file is set, else its value.
	Credential func(name string) (string, error)
	// Missing says where the credentials of names are set, for the
	// error that they are not.
	Missing func(names ...string) string
	// SSH runs a command with the ssh client for ssh://, with the port
	// (empty: ssh's) and the arguments of ssh_args before the target.
	SSH func(target, port string, args []string) SSHRunner
	// Client is the HTTP client, in place of the storage's own (tests).
	Client *http.Client
}

// SSHRunner runs a shell command on the machine of an ssh:// storage
// with stdin; exit status 255 is the ssh client's own failure.
type SSHRunner func(ctx context.Context, command string, stdin []byte) (stdout, stderr []byte, exit int, err error)

// StorageSchemes are the schemes of a storage.
const StorageSchemes = "file, ssh, s3, r2, consul, http+consul, vault, http+vault"

// storageHTTPTimeout is how long a request to a storage may take.
const storageHTTPTimeout = 30 * time.Second

// OpenStorage returns the storage a URL names, with the checks of the
// collection's open_store, in its order:
//
//	file:///PATH                       a directory on this machine
//	ssh://[USER@]HOST[:PORT]/PATH      a directory on another machine
//	s3://BUCKET/PREFIX                 S3, or an S3-compatible store at endpoint
//	r2://ACCOUNT/BUCKET/PREFIX         Cloudflare R2
//	consul://HOST:8500/PREFIX          Consul's KV store (http+consul:// for http)
//	vault://HOST:8200/MOUNT/PREFIX     a KV version 2 engine of Vault (http+vault://)
//
// Credentials never come from the URL: a query string, a password, or a
// user (but ssh's) is refused. The path is taken as written, not
// decoded, as the collection takes it. Nothing is read from the storage
// yet.
func OpenStorage(raw string, o StorageOptions) (Objects, error) {
	if !strings.Contains(raw, "://") {
		return nil, fmt.Errorf("not the URL of a storage (%s)", StorageSchemes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("not a URL: %v", err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("a query string in the URL is not read; credentials go into [ofzhur_storage] of ansible.cfg")
	}
	if _, set := u.User.Password(); set {
		return nil, errors.New("a password in the URL is not read")
	}
	if u.User != nil && u.Scheme != "ssh" {
		return nil, fmt.Errorf("a user in the URL is not read (%s://)", u.Scheme)
	}
	port := u.Port()
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n > 65535 {
			return nil, fmt.Errorf("not a URL: port %s", port)
		} else if n == 0 {
			port = ""
		}
	}
	if port != "" && (u.Scheme == "s3" || u.Scheme == "r2") {
		return nil, fmt.Errorf("a port in the URL is not read (%s://)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	rawPath := urlPath(raw)
	p := strings.Trim(rawPath, "/")
	label := StorageLabel(raw)
	option := func(name string) (string, error) {
		if o.Option == nil {
			return "", nil
		}
		return o.Option(name)
	}
	credential := func(name string) (string, error) {
		if o.Credential == nil {
			return "", nil
		}
		return o.Credential(name)
	}
	missing := func(names ...string) string {
		if o.Missing == nil {
			return strings.Join(names, " and ")
		}
		return o.Missing(names...)
	}
	caFile, err := option("ca_file")
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "file":
		dir := rawPath
		if u.Host != "" && u.Host != "localhost" {
			dir = "/" + u.Host + rawPath
		}
		if strings.Trim(dir, "/") == "" {
			return nil, errors.New("file:///path")
		}
		return &Directory{Dir: dir, label: label}, nil

	case "ssh":
		if host == "" || p == "" || strings.HasPrefix(host, "-") {
			return nil, errors.New("ssh://[user@]host[:port]/path")
		}
		target := host
		if u.User != nil && u.User.Username() != "" {
			target = u.User.Username() + "@" + host
		}
		sshArgs, err := option("ssh_args")
		if err != nil {
			return nil, err
		}
		args, err := SplitShell(sshArgs)
		if err != nil {
			return nil, fmt.Errorf("ssh_args: %v", err)
		}
		if o.SSH == nil {
			return nil, errors.New("no ssh client")
		}
		return &sshObjects{run: o.SSH(target, port, args), dir: "/" + p, label: label}, nil

	case "s3", "r2":
		var bucket, prefix, endpoint, region string
		if u.Scheme == "s3" {
			if host == "" {
				return nil, errors.New("s3://bucket/prefix")
			}
			bucket, prefix = u.Host, p
			if endpoint, err = option("endpoint"); err != nil {
				return nil, err
			}
			if region, err = option("region"); err != nil {
				return nil, err
			}
			if region == "" {
				region = "us-east-1"
			}
		} else {
			bucket, prefix, _ = strings.Cut(p, "/")
			if host == "" || bucket == "" {
				return nil, errors.New("r2://account/bucket/prefix, where account is the Cloudflare account ID")
			}
			endpoint, region = "https://"+u.Host+".r2.cloudflarestorage.com", "auto"
		}
		accessKey, err := credential("access_key")
		if err != nil {
			return nil, err
		}
		secretKey, err := credential("secret_key")
		if err != nil {
			return nil, err
		}
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("the access key and the secret key are missing: %s", missing("access_key", "secret_key"))
		}
		token, err := credential("session_token")
		if err != nil {
			return nil, err
		}
		if endpoint == "" {
			// AWS
			endpoint = "https://s3." + region + ".amazonaws.com"
		} else if e, err := url.Parse(endpoint); err != nil || (e.Scheme != "http" && e.Scheme != "https") || e.Hostname() == "" || e.RawQuery != "" || e.User != nil {
			return nil, fmt.Errorf("endpoint %q: not an http(s) address of a host", endpoint)
		}
		client, err := storageClient(o.Client, caFile)
		if err != nil {
			return nil, err
		}
		s := &S3{Endpoint: endpoint, Bucket: bucket, Prefix: strings.Trim(prefix, "/"), Region: region, AccessKey: accessKey, SecretKey: secretKey, SessionToken: token, Client: client}
		return &s3Objects{s: s, label: label}, nil

	case "consul", "http+consul":
		if host == "" || p == "" {
			return nil, errors.New("consul://host:8500/prefix")
		}
		token, err := credential("consul_token")
		if err != nil {
			return nil, err
		}
		client, err := storageClient(o.Client, caFile)
		if err != nil {
			return nil, err
		}
		scheme := "https"
		if u.Scheme == "http+consul" {
			scheme = "http"
		}
		return &consulObjects{c: &Consul{Address: scheme + "://" + u.Host, Prefix: p, Token: token, Client: client}, label: label}, nil

	case "vault", "http+vault":
		mount, prefix, _ := strings.Cut(p, "/")
		if host == "" || prefix == "" {
			return nil, errors.New("vault://host:8200/mount/prefix")
		}
		token, err := credential("vault_token")
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("the token is missing: %s", missing("vault_token"))
		}
		namespace, err := option("vault_namespace")
		if err != nil {
			return nil, err
		}
		client, err := storageClient(o.Client, caFile)
		if err != nil {
			return nil, err
		}
		scheme := "https"
		if u.Scheme == "http+vault" {
			scheme = "http"
		}
		return &vaultObjects{v: &Vault{Address: scheme + "://" + u.Host, Mount: mount, Prefix: prefix, Token: token, Namespace: namespace, Client: client}, label: label}, nil
	}
	return nil, fmt.Errorf("unknown scheme %s (%s)", u.Scheme, StorageSchemes)
}

// StorageLabel is a storage's URL without what may be secret: no
// password, no user but ssh's, no query; without a trailing "/".
func StorageLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		raw, _, _ = strings.Cut(raw, "?")
		raw, _, _ = strings.Cut(raw, "#")
		return strings.TrimRight(userInfo.ReplaceAllString(raw, "//"), "/")
	}
	netloc := strings.ToLower(u.Hostname())
	if strings.Contains(netloc, ":") {
		netloc = "[" + netloc + "]"
	}
	if u.Port() != "" {
		netloc += ":" + u.Port()
	}
	if u.Scheme == "ssh" && u.User != nil && u.User.Username() != "" {
		netloc = u.User.Username() + "@" + netloc
	}
	return strings.TrimRight(u.Scheme+"://"+netloc+urlPath(raw), "/")
}

// urlPath is the path of a URL as written, not decoded, as Python's
// urlsplit has it.
func urlPath(raw string) string {
	_, rest, _ := strings.Cut(raw, "://")
	i := strings.IndexAny(rest, "/?#")
	if i < 0 {
		return ""
	}
	rest = rest[i:]
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

var userInfo = regexp.MustCompile(`//[^/]*@`)

// storageClient is the HTTP client of a storage: the system's CA
// certificates, or those of caFile alone, and no redirect followed (the
// credentials stay with the storage).
func storageClient(given *http.Client, caFile string) (*http.Client, error) {
	if given != nil {
		return given, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %s: %s", pathError(err), caFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file: no PEM certificate in %s", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       storageHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// pathError is the reason of a file's error, without the path.
func pathError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// httpMessage is a storage's answer in a line: its body, short, and the
// status.
func httpMessage(status int, body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("%s (HTTP %d)", text, status)
}

// getAll reads the objects of names, eight at a time; one that is gone
// meanwhile is left out.
func getAll(ctx context.Context, names []string, get func(context.Context, string) ([]byte, bool, error)) (map[string][]byte, error) {
	type result struct {
		data []byte
		ok   bool
		err  error
	}
	results := make([]result, len(names))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(8, len(names)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				d, ok, err := get(ctx, names[i])
				results[i] = result{d, ok, err}
			}
		}()
	}
	for i := range names {
		work <- i
	}
	close(work)
	wg.Wait()
	out := map[string][]byte{}
	for i, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		if r.ok {
			out[names[i]] = r.data
		}
	}
	return out, nil
}

// --- a directory on this machine -------------------------------------

// Directory is a directory on this machine (file:///PATH), or the host
// files of a repository (inventory/hosts/). An object is written under a
// dot name, which nothing reads, and renamed into place.
type Directory struct {
	Dir string
	// DirMode and FileMode are those of a directory and an object Put
	// makes, less the umask: 0700 and 0600 when zero, as the collection
	// makes them.
	DirMode, FileMode fs.FileMode
	label             string
}

func (d *Directory) String() string {
	if d.label != "" {
		return d.label
	}
	return d.Dir
}

func (d *Directory) Get(ctx context.Context, name string) ([]byte, bool, error) {
	data, err := os.ReadFile(filepath.Join(d.Dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %s", name, pathError(err))
	}
	return data, true, nil
}

func (d *Directory) Put(ctx context.Context, name string, data []byte) error {
	dirMode, fileMode := d.DirMode, d.FileMode
	if dirMode == 0 {
		dirMode = 0o700
	}
	if fileMode == 0 {
		fileMode = 0o600
	}
	err := func() error {
		if err := os.MkdirAll(d.Dir, dirMode); err != nil {
			return err
		}
		// a name of its own, made as os.WriteFile makes a file: less the umask
		var f *os.File
		var tmp string
		var err error
		for i := 0; i < 100; i++ {
			tmp = filepath.Join(d.Dir, fmt.Sprintf(".%s.%d.%d", name, os.Getpid(), time.Now().UnixNano()+int64(i)))
			if f, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode); !errors.Is(err, fs.ErrExist) {
				break
			}
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, filepath.Join(d.Dir, name))
		}
		if err != nil {
			os.Remove(tmp)
		}
		return err
	}()
	if err != nil {
		return fmt.Errorf("writing %s: %s", name, pathError(err))
	}
	return nil
}

func (d *Directory) Remove(ctx context.Context, name string) error {
	if err := os.Remove(filepath.Join(d.Dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %s: %s", name, pathError(err))
	}
	return nil
}

func (d *Directory) All(ctx context.Context) (map[string][]byte, error) {
	entries, err := os.ReadDir(d.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing: %s", pathError(err))
	}
	out := map[string][]byte{}
	for _, e := range entries {
		n := e.Name()
		if !hostFileName(n) {
			continue
		}
		// a file, or a link to one
		if fi, err := os.Stat(filepath.Join(d.Dir, n)); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, ok, err := d.Get(ctx, n)
		if err != nil {
			return nil, err
		}
		if ok {
			out[n] = data
		}
	}
	return out, nil
}

// --- a directory on another machine, over SSH ---------------------------

// sshObjects is a directory on another machine (ssh://), with the
// collection's scripts: they need sh, cat, mkdir, mktemp, mv and rm.
type sshObjects struct {
	run   SSHRunner
	dir   string
	label string
}

func (s *sshObjects) String() string { return s.label }

func (s *sshObjects) path(name string) string { return shellQuote(path.Join(s.dir, name)) }

func (s *sshObjects) do(ctx context.Context, what, script string, stdin []byte, codes ...int) ([]byte, int, error) {
	out, errOut, code, err := s.run(ctx, "sh -c "+shellQuote(script), stdin)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", what, err)
	}
	if code == 0 || slices.Contains(codes, code) {
		return out, code, nil
	}
	lines := strings.Split(strings.TrimSpace(string(errOut)), "\n")
	why := strings.TrimSpace(lines[len(lines)-1])
	if why == "" {
		why = fmt.Sprintf("exit status %d", code)
	}
	// 255: ssh itself, whose own messages start with "ssh: " already
	if code == 255 && !strings.HasPrefix(why, "ssh: ") {
		why = "ssh: " + why
	}
	return nil, code, fmt.Errorf("%s: %s", what, why)
}

func (s *sshObjects) Get(ctx context.Context, name string) ([]byte, bool, error) {
	out, code, err := s.do(ctx, "reading "+name, `f=`+s.path(name)+`; [ -f "$f" ] || exit 3; exec cat -- "$f"`, nil, 3)
	if err != nil || code == 3 {
		return nil, false, err
	}
	return out, true, nil
}

func (s *sshObjects) Put(ctx context.Context, name string, data []byte) error {
	d := shellQuote(s.dir)
	_, _, err := s.do(ctx, "writing "+name, `set -e; umask 077; mkdir -p -- `+d+`; t=$(mktemp `+shellQuote(path.Join(s.dir, "."+name+"."))+`XXXXXX);`+
		` trap 'rm -f -- "$t"' EXIT; cat > "$t"; mv -f -- "$t" `+s.path(name), data)
	return err
}

func (s *sshObjects) Remove(ctx context.Context, name string) error {
	_, _, err := s.do(ctx, "deleting "+name, "rm -f -- "+s.path(name), nil)
	return err
}

// All reads every host file in one connection: each as "SIZE NAME\n"
// and its content.
func (s *sshObjects) All(ctx context.Context) (map[string][]byte, error) {
	d := shellQuote(s.dir)
	script := `export LC_ALL=C; [ -d ` + d + ` ] || exit 0; cd -- ` + d + ` || exit 1;` +
		` for f in ''*'.yml' ''*'.yaml'; do [ -f "$f" ] || continue;` +
		` c=$(cat -- "$f" && echo .) || continue; c=${c%.}; printf "%s %s\n%s" "${#c}" "$f" "$c"; done; exit 0`
	out, _, err := s.do(ctx, "reading", script, nil)
	if err != nil {
		return nil, err
	}
	objects := map[string][]byte{}
	for i := 0; i < len(out); {
		j := strings.IndexByte(string(out[i:]), '\n')
		if j < 0 {
			return nil, errors.New("reading: not the output of the script")
		}
		size, name, ok := strings.Cut(string(out[i:i+j]), " ")
		n, err := strconv.Atoi(size)
		if !ok || err != nil || i+j+1+n > len(out) {
			return nil, errors.New("reading: not the output of the script")
		}
		if hostFileName(name) {
			objects[name] = out[i+j+1 : i+j+1+n]
		}
		i += j + 1 + n
	}
	return objects, nil
}

// --- S3 and R2 -------------------------------------------------------------

type s3Objects struct {
	s     *S3
	label string
}

func (o *s3Objects) String() string { return o.label }

func (o *s3Objects) key(name string) string {
	if o.s.Prefix == "" {
		return name
	}
	return o.s.Prefix + "/" + name
}

func (o *s3Objects) Get(ctx context.Context, name string) ([]byte, bool, error) {
	resp, body, err := o.s.do(ctx, http.MethodGet, o.key(name), nil, nil)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", name, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("reading %s: %w", name, s3Error(resp, body))
}

func (o *s3Objects) Put(ctx context.Context, name string, data []byte) error {
	contentType := "application/json"
	if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
		contentType = "application/yaml"
	}
	resp, body, err := o.s.do(ctx, http.MethodPut, o.key(name), data, map[string]string{"Content-Type": contentType})
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("writing %s: %w", name, s3Error(resp, body))
	}
	return nil
}

func (o *s3Objects) Remove(ctx context.Context, name string) error {
	resp, body, err := o.s.do(ctx, http.MethodDelete, o.key(name), nil, nil)
	if err != nil {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("deleting %s: %w", name, s3Error(resp, body))
}

// names lists the objects directly under the prefix.
func (o *s3Objects) names(ctx context.Context) ([]string, error) {
	prefix := ""
	if o.s.Prefix != "" {
		prefix = o.s.Prefix + "/"
	}
	var names []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "delimiter": {"/"}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, body, err := o.s.do(ctx, http.MethodGet, "?"+q.Encode(), nil, nil)
		if err != nil {
			return nil, fmt.Errorf("listing: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing: %w", s3Error(resp, body))
		}
		var list struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(body, &list); err != nil {
			return nil, errors.New("listing: not an S3 answer")
		}
		for _, c := range list.Contents {
			if n, ok := strings.CutPrefix(c.Key, prefix); ok && hostFileName(n) {
				names = append(names, n)
			}
		}
		if !list.IsTruncated || list.NextContinuationToken == "" {
			return names, nil
		}
		token = list.NextContinuationToken
	}
}

func (o *s3Objects) All(ctx context.Context) (map[string][]byte, error) {
	names, err := o.names(ctx)
	if err != nil {
		return nil, err
	}
	return getAll(ctx, names, o.Get)
}

// --- Consul ----------------------------------------------------------------

type consulObjects struct {
	c     *Consul
	label string
}

func (o *consulObjects) String() string { return o.label }

// key is the key of an object, escaped for the request.
func (o *consulObjects) key(name string) string {
	return awsEscape(o.c.Prefix+"/"+name, false)
}

func (o *consulObjects) call(ctx context.Context, what, method, key, query string, body []byte) (int, []byte, error) {
	resp, out, err := o.c.do(ctx, method, key, query, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return 0, nil, fmt.Errorf("%s: %s", what, httpMessage(resp.StatusCode, out))
	}
	return resp.StatusCode, out, nil
}

func (o *consulObjects) Get(ctx context.Context, name string) ([]byte, bool, error) {
	status, out, err := o.call(ctx, "reading "+name, http.MethodGet, o.key(name), "raw", nil)
	if err != nil || status != http.StatusOK {
		return nil, false, err
	}
	return out, true, nil
}

func (o *consulObjects) Put(ctx context.Context, name string, data []byte) error {
	status, out, err := o.call(ctx, "writing "+name, http.MethodPut, o.key(name), "", data)
	if err != nil {
		return err
	}
	if status != http.StatusOK || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("writing %s: %s", name, httpMessage(status, out))
	}
	return nil
}

func (o *consulObjects) Remove(ctx context.Context, name string) error {
	_, _, err := o.call(ctx, "deleting "+name, http.MethodDelete, o.key(name), "", nil)
	return err
}

// All reads every key under the prefix in one request.
func (o *consulObjects) All(ctx context.Context) (map[string][]byte, error) {
	prefix := o.c.Prefix + "/"
	status, out, err := o.call(ctx, "reading", http.MethodGet, awsEscape(prefix, false), "recurse", nil)
	if err != nil {
		return nil, err
	}
	objects := map[string][]byte{}
	if status == http.StatusNotFound {
		return objects, nil
	}
	var entries []struct {
		Key   string  `json:"Key"`
		Value *string `json:"Value"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, errors.New("reading: not a Consul answer")
	}
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Key, prefix)
		if !ok || !hostFileName(n) {
			continue
		}
		var data []byte
		if e.Value != nil {
			if data, err = base64.StdEncoding.DecodeString(*e.Value); err != nil {
				return nil, errors.New("reading: not a Consul answer")
			}
		}
		objects[n] = data
	}
	return objects, nil
}

// --- Vault -----------------------------------------------------------------

// vaultObjects is a prefix of a KV version 2 engine; each object is a
// secret with the field json, which holds it as text.
type vaultObjects struct {
	v     *Vault
	label string
}

func (o *vaultObjects) String() string { return o.label }

// path is the API path of an object of kind data or metadata, escaped
// for the request.
func (o *vaultObjects) path(kind, name string) string {
	return awsEscape(o.v.Mount+"/"+kind+"/"+o.v.Prefix+"/"+name, false)
}

func (o *vaultObjects) call(ctx context.Context, what, method, p string, body any) (int, []byte, error) {
	resp, out, err := o.v.do(ctx, method, p, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", what, err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return resp.StatusCode, out, nil
	}
	return 0, nil, fmt.Errorf("%s: %w", what, vaultError(resp, out))
}

func (o *vaultObjects) Get(ctx context.Context, name string) ([]byte, bool, error) {
	status, out, err := o.call(ctx, "reading "+name, http.MethodGet, o.path("data", name), nil)
	if err != nil || status == http.StatusNotFound {
		return nil, false, err
	}
	var s struct {
		Data *struct {
			Data any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &s); err != nil || s.Data == nil {
		return nil, false, fmt.Errorf("reading %s: not a Vault answer", name)
	}
	if fields, ok := s.Data.Data.(map[string]any); ok {
		if text, ok := fields["json"].(string); ok {
			return []byte(text), true, nil
		}
	}
	// a secret without the field json: its fields, an object that is
	// none of the collection's
	data, _ := json.Marshal(s.Data.Data)
	return data, true, nil
}

func (o *vaultObjects) Put(ctx context.Context, name string, data []byte) error {
	status, _, err := o.call(ctx, "writing "+name, http.MethodPost, o.path("data", name), map[string]any{"data": map[string]string{"json": string(data)}})
	if err == nil && status == http.StatusNotFound {
		return fmt.Errorf("writing %s: HTTP 404 (is %s a KV version 2 engine?)", name, o.v.Mount)
	}
	return err
}

func (o *vaultObjects) Remove(ctx context.Context, name string) error {
	_, _, err := o.call(ctx, "deleting "+name, http.MethodDelete, o.path("metadata", name), nil)
	return err
}

func (o *vaultObjects) All(ctx context.Context) (map[string][]byte, error) {
	status, out, err := o.call(ctx, "listing", http.MethodGet, o.path("metadata", "")+"?list=true", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return map[string][]byte{}, nil
	}
	var list struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, errors.New("listing: not a Vault answer")
	}
	var names []string
	for _, k := range list.Data.Keys {
		if hostFileName(k) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return getAll(ctx, names, o.Get)
}

// --- shell words -------------------------------------------------------------

// SplitShell splits a command line into words as Python's shlex.split
// does, which the collection reads ssh_args with: blanks separate
// words, '...' is literal, "..." takes a backslash before " and \ only,
// and a backslash outside quotes escapes any character.
func SplitShell(s string) ([]string, error) {
	var words []string
	var w strings.Builder
	in := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case ' ', '\t', '\r', '\n':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		case '\\':
			if i++; i == len(rs) {
				return nil, errors.New("No escaped character")
			}
			w.WriteRune(rs[i])
			in = true
		case '\'', '"':
			in = true
			for i++; ; i++ {
				if i == len(rs) {
					return nil, errors.New("No closing quotation")
				}
				if rs[i] == c {
					break
				}
				if c == '"' && rs[i] == '\\' {
					if i++; i == len(rs) {
						return nil, errors.New("No escaped character")
					}
					if rs[i] != '"' && rs[i] != '\\' {
						w.WriteRune('\\')
					}
				}
				w.WriteRune(rs[i])
			}
		default:
			w.WriteRune(c)
			in = true
		}
	}
	if in {
		words = append(words, w.String())
	}
	return words, nil
}
