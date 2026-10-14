package state

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
	// TLS, when false, makes consul:// and vault:// use http (they
	// default to https; a URL may say http+consul:// or http+vault://).
	Client *http.Client
	Runner Runner
}

// DefaultPath is the local state file.
func DefaultPath() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "ump", "state.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "state", "ump", "state.yaml")
	}
	return filepath.Join(home, ".local", "state", "ump", "state.yaml")
}

// Open returns the backend a URL names:
//
//	local                           the default state file (DefaultPath)
//	file:///path/to/state.yaml      a file on this machine (a plain path does too)
//	ssh://[user@]host/path          a file on another machine, over SSH
//	s3://bucket/key                 an S3 bucket (Endpoint, Region, keys)
//	r2://account/bucket/key         a Cloudflare R2 bucket (keys)
//	http(s)://host/path             an HTTP service (Token, or User and Password)
//	consul://host:8500/path         Consul KV over https (http+consul:// for http; Token)
//	vault://host:8200/mount/path    Vault KV v2 over https (http+vault:// for http; Token)
func Open(raw string, c Credentials) (Backend, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "local" {
		return &Local{Path: DefaultPath()}, nil
	}
	if !strings.Contains(raw, "://") {
		return &Local{Path: expandHome(raw)}, nil
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
			return nil, fmt.Errorf("state %q: no path", raw)
		}
		return &Local{Path: expandHome(p)}, nil
	case "ssh":
		if u.Host == "" || u.Path == "" {
			return nil, fmt.Errorf("state %q: ssh://[user@]host/path", raw)
		}
		host := u.Host
		if u.User != nil {
			host = u.User.Username() + "@" + host
		}
		return &SSH{Host: host, Path: u.Path, Run: c.Runner}, nil
	case "s3":
		key := strings.TrimPrefix(u.Path, "/")
		if u.Host == "" || key == "" {
			return nil, fmt.Errorf("state %q: s3://bucket/key", raw)
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
			return nil, fmt.Errorf("state %s: the access key and the secret key are missing (state.access_key and state.secret_key in the configuration, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)", raw)
		}
		return &S3{Endpoint: endpoint, Bucket: u.Host, Key: key, Region: region, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: c.Client}, nil
	case "r2":
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
		if u.Host == "" || len(parts) != 2 || parts[1] == "" {
			return nil, fmt.Errorf("state %q: r2://account/bucket/key", raw)
		}
		if c.AccessKey == "" || c.SecretKey == "" {
			return nil, fmt.Errorf("state %s: the access key and the secret key are missing (state.access_key and state.secret_key in the configuration, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)", raw)
		}
		return &S3{Endpoint: "https://" + u.Host + ".r2.cloudflarestorage.com", Bucket: parts[0], Key: parts[1], Region: "auto", AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: c.Client, label: raw}, nil
	case "http", "https":
		return &HTTP{URL: raw, Token: c.Token, User: c.User, Password: c.Password, Client: c.Client}, nil
	case "consul", "http+consul":
		scheme := "https"
		if u.Scheme == "http+consul" {
			scheme = "http"
		}
		path := strings.TrimPrefix(u.Path, "/")
		if u.Host == "" || path == "" {
			return nil, fmt.Errorf("state %q: consul://host:8500/path", raw)
		}
		return &Consul{Address: scheme + "://" + u.Host, Path: path, Token: c.Token, Client: c.Client}, nil
	case "vault", "http+vault":
		scheme := "https"
		if u.Scheme == "http+vault" {
			scheme = "http"
		}
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
		if u.Host == "" || len(parts) != 2 || parts[1] == "" {
			return nil, fmt.Errorf("state %q: vault://host:8200/mount/path", raw)
		}
		if c.Token == "" {
			return nil, fmt.Errorf("state %s: the token is missing (state.token in the configuration, or VAULT_TOKEN)", raw)
		}
		return &Vault{Address: scheme + "://" + u.Host, Mount: parts[0], Path: parts[1], Token: c.Token, Client: c.Client}, nil
	}
	return nil, fmt.Errorf("state %q: unknown scheme %s (local, file, ssh, s3, r2, http, https, consul, vault)", raw, u.Scheme)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
