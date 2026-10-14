package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Consul is a prefix in Consul's KV store (consul://host:8500/prefix).
// A file's version tag is its key's ModifyIndex and a write is a
// check-and-set on it; the advisory locks are keys under .locks/,
// created with cas=0.
type Consul struct {
	Address string // https://host:8500
	Prefix  string // kv prefix without a leading slash
	Token   string
	Client  *http.Client
}

func (c *Consul) String() string {
	return "consul://" + strings.TrimPrefix(strings.TrimPrefix(c.Address, "http://"), "https://") + "/" + c.Prefix
}

func (c *Consul) key(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(c.Prefix, "/") + "/" + p, nil
}

func (c *Consul) do(ctx context.Context, method, key, query string, body []byte) (*http.Response, []byte, error) {
	u := strings.TrimRight(c.Address, "/") + "/v1/kv/" + key
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if c.Token != "" {
		req.Header.Set("X-Consul-Token", c.Token)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp, out, err
}

type consulEntry struct {
	Key         string `json:"Key"`
	Value       string `json:"Value"`
	ModifyIndex uint64 `json:"ModifyIndex"`
}

func (c *Consul) get(ctx context.Context, key string) (*consulEntry, error) {
	resp, body, err := c.do(ctx, http.MethodGet, key, "", nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var entries []consulEntry
		if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
			return nil, fmt.Errorf("unexpected answer for %s", key)
		}
		return &entries[0], nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, fmt.Errorf("GET %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
}

func (c *Consul) Read(ctx context.Context, p string) ([]byte, string, error) {
	key, err := c.key(p)
	if err != nil {
		return nil, "", err
	}
	e, err := c.get(ctx, key)
	if err != nil || e == nil {
		return nil, "", err
	}
	data, err := base64.StdEncoding.DecodeString(e.Value)
	if err != nil {
		return nil, "", err
	}
	return data, strconv.FormatUint(e.ModifyIndex, 10), nil
}

func (c *Consul) cas(ctx context.Context, key string, data []byte, index string) (bool, error) {
	if index == "" {
		index = "0"
	}
	if _, err := strconv.ParseUint(index, 10, 64); err != nil {
		return false, nil // a version that never was
	}
	resp, body, err := c.do(ctx, http.MethodPut, key, "cas="+index, data)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("PUT %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)) == "true", nil
}

func (c *Consul) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	key, err := c.key(p)
	if err != nil {
		return "", err
	}
	ok, err := c.cas(ctx, key, data, ifVersion)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrConflict
	}
	e, err := c.get(ctx, key)
	if err != nil || e == nil {
		return "", err
	}
	return strconv.FormatUint(e.ModifyIndex, 10), nil
}

func (c *Consul) Delete(ctx context.Context, p string) error {
	key, err := c.key(p)
	if err != nil {
		return err
	}
	resp, body, err := c.do(ctx, http.MethodDelete, key, "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Consul) recurse(ctx context.Context, prefix string) ([]consulEntry, error) {
	resp, body, err := c.do(ctx, http.MethodGet, prefix, "recurse", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", prefix, resp.StatusCode)
	}
	var entries []consulEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (c *Consul) List(ctx context.Context) ([]Entry, error) {
	prefix := strings.TrimRight(c.Prefix, "/") + "/"
	entries, err := c.recurse(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range entries {
		rel := strings.TrimPrefix(e.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") || Excluded(rel) {
			continue
		}
		out = append(out, Entry{Path: rel, Version: strconv.FormatUint(e.ModifyIndex, 10)})
	}
	return out, nil
}

func (c *Consul) lockKey(key string) string {
	return strings.TrimRight(c.Prefix, "/") + "/.locks/" + lockKey(key)
}

func (c *Consul) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	ok, err := c.cas(ctx, c.lockKey(key), payload, "0")
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	held := LockInfo{Key: key}
	if e, err := c.get(ctx, c.lockKey(key)); err == nil && e != nil {
		if b, err := base64.StdEncoding.DecodeString(e.Value); err == nil {
			json.Unmarshal(b, &held)
		}
	}
	return &ErrLocked{Key: key, Info: held}
}

func (c *Consul) Unlock(ctx context.Context, key string) error {
	resp, body, err := c.do(ctx, http.MethodDelete, c.lockKey(key), "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Consul) Locks(ctx context.Context) ([]LockInfo, error) {
	entries, err := c.recurse(ctx, strings.TrimRight(c.Prefix, "/")+"/.locks/")
	if err != nil {
		return nil, err
	}
	var locks []LockInfo
	for _, e := range entries {
		b, err := base64.StdEncoding.DecodeString(e.Value)
		if err != nil {
			continue
		}
		var info LockInfo
		if json.Unmarshal(b, &info) == nil {
			locks = append(locks, info)
		}
	}
	return locks, nil
}
