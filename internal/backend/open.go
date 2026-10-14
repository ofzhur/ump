package backend

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Credentials are what a backend may need, from the configuration or
// the environment; which ones depends on the URL's scheme.
type Credentials struct {
	// Endpoint is the S3-compatible endpoint for s3:// (AWS when empty,
	// from the region).
	Endpoint string
	// AccessKey and SecretKey are for s3:// and r2://.
	AccessKey string
	SecretKey string
	// Region is for s3:// (auto for r2://).
	Region string
	// Token is for http(s)://, consul:// and vault://.
	Token string
	// User and Password are for http(s)://.
	User     string
	Password string
	// Client is the HTTP client (tests); Runner runs ssh for ssh://.
	Client *http.Client
	Runner Runner
}

// Open returns the backend a URL names:
//
//	file:///path/to/dir             a directory on this machine (a plain path does too)
//	ssh://[user@]host/path/to/dir   a directory on another machine, over SSH
//	s3://bucket/prefix              an S3 bucket (Endpoint, Region, keys)
//	r2://account/bucket/prefix      a Cloudflare R2 bucket (keys)
//	http(s)://host/path             an HTTP service (Token, or User and Password)
//	consul://host:8500/prefix       Consul KV over https (http+consul:// for http; Token)
//	vault://host:8200/mount/prefix  Vault KV v2 over https (http+vault:// for http; Token)
func Open(raw string, c Credentials) (Backend, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("no backend URL")
	}
	if !strings.Contains(raw, "://") {
		return &Local{Dir: expandHome(raw)}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("state %q: %w", raw, err)
	}
	switch u.Scheme {
	case "file":
		p := u.Path
		if u.Host != "" && u.Host != "localhost" {
			p = "/" + u.Host + u.Path
		}
		if p == "" {
			return nil, fmt.Errorf("backend %q: no path", raw)
		}
		return &Local{Dir: expandHome(p)}, nil
	case "ssh":
		if u.Host == "" || u.Path == "" {
			return nil, fmt.Errorf("backend %q: ssh://[user@]host/path", raw)
		}
		host := u.Host
		if u.User != nil {
			host = u.User.Username() + "@" + host
		}
		return &SSH{Host: host, Dir: u.Path, Run: c.Runner}, nil
	case "s3":
		key := strings.Trim(u.Path, "/")
		if u.Host == "" {
			return nil, fmt.Errorf("backend %q: s3://bucket/prefix", raw)
		}
		endpoint := c.Endpoint
		region := c.Region
		if endpoint == "" {
			if region == "" {
				region = "us-east-1"
			}
			endpoint = "https://s3." + region + ".amazonaws.com"
		}
		if c.AccessKey == "" || c.SecretKey == "" {
			return nil, fmt.Errorf("backend %s: the access key and the secret key are missing (access_key and secret_key in backend.yaml, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)", raw)
		}
		return &S3{Endpoint: endpoint, Bucket: u.Host, Prefix: key, Region: region, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: c.Client}, nil
	case "r2":
		parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
		if u.Host == "" || parts[0] == "" {
			return nil, fmt.Errorf("backend %q: r2://account/bucket/prefix", raw)
		}
		prefix := ""
		if len(parts) == 2 {
			prefix = parts[1]
		}
		if c.AccessKey == "" || c.SecretKey == "" {
			return nil, fmt.Errorf("backend %s: the access key and the secret key are missing (access_key and secret_key in backend.yaml, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)", raw)
		}
		return &S3{Endpoint: "https://" + u.Host + ".r2.cloudflarestorage.com", Bucket: parts[0], Prefix: prefix, Region: "auto", AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: c.Client, label: raw}, nil
	case "http", "https":
		return &HTTP{URL: raw, Token: c.Token, User: c.User, Password: c.Password, Client: c.Client}, nil
	case "consul", "http+consul":
		scheme := "https"
		if u.Scheme == "http+consul" {
			scheme = "http"
		}
		path := strings.Trim(u.Path, "/")
		if u.Host == "" || path == "" {
			return nil, fmt.Errorf("backend %q: consul://host:8500/prefix", raw)
		}
		return &Consul{Address: scheme + "://" + u.Host, Prefix: path, Token: c.Token, Client: c.Client}, nil
	case "vault", "http+vault":
		scheme := "https"
		if u.Scheme == "http+vault" {
			scheme = "http"
		}
		parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
		if u.Host == "" || len(parts) != 2 || parts[1] == "" {
			return nil, fmt.Errorf("backend %q: vault://host:8200/mount/prefix", raw)
		}
		if c.Token == "" {
			return nil, fmt.Errorf("backend %s: the token is missing (token in backend.yaml, or VAULT_TOKEN)", raw)
		}
		return &Vault{Address: scheme + "://" + u.Host, Mount: parts[0], Prefix: parts[1], Token: c.Token, Client: c.Client}, nil
	}
	return nil, fmt.Errorf("backend %q: unknown scheme %s (file, ssh, s3, r2, http, https, consul, vault)", raw, u.Scheme)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
