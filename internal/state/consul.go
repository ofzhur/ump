package state

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

// Consul is a key in Consul's KV store (consul://host:8500/path/to/state).
// The version tag is the key's ModifyIndex and a write is a
// check-and-set on it; the per-server locks are keys below
// <path>.locks/, created with cas=0.
type Consul struct {
	Address string // http://host:8500
	Path    string // kv path without a leading slash
	Token   string
	Client  *http.Client
}

func (c *Consul) String() string {
	return "consul://" + strings.TrimPrefix(strings.TrimPrefix(c.Address, "http://"), "https://") + "/" + c.Path
}

func (c *Consul) do(ctx context.Context, method, path, query string, body []byte) (*http.Response, []byte, error) {
	u := strings.TrimRight(c.Address, "/") + "/v1/kv/" + path
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
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, out, err
}

type consulEntry struct {
	Key         string `json:"Key"`
	Value       string `json:"Value"`
	ModifyIndex uint64 `json:"ModifyIndex"`
}

func (c *Consul) get(ctx context.Context, path string) (*consulEntry, error) {
	resp, body, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var entries []consulEntry
		if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
			return nil, fmt.Errorf("unexpected answer for %s", path)
		}
		return &entries[0], nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
}

func (c *Consul) Read(ctx context.Context) ([]byte, string, error) {
	e, err := c.get(ctx, c.Path)
	if err != nil || e == nil {
		return nil, "", err
	}
	data, err := base64.StdEncoding.DecodeString(e.Value)
	if err != nil {
		return nil, "", err
	}
	return data, strconv.FormatUint(e.ModifyIndex, 10), nil
}

func (c *Consul) cas(ctx context.Context, path string, data []byte, index string) (bool, error) {
	if index == "" {
		index = "0"
	}
	resp, body, err := c.do(ctx, http.MethodPut, path, "cas="+index, data)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("PUT %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)) == "true", nil
}

func (c *Consul) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	ok, err := c.cas(ctx, c.Path, data, ifVersion)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrConflict
	}
	e, err := c.get(ctx, c.Path)
	if err != nil || e == nil {
		return "", err
	}
	return strconv.FormatUint(e.ModifyIndex, 10), nil
}

func (c *Consul) lockPath(key string) string { return c.Path + ".locks/" + lockKey(key) }

func (c *Consul) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	ok, err := c.cas(ctx, c.lockPath(key), payload, "0")
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	held := LockInfo{Key: key}
	if e, err := c.get(ctx, c.lockPath(key)); err == nil && e != nil {
		if b, err := base64.StdEncoding.DecodeString(e.Value); err == nil {
			json.Unmarshal(b, &held)
		}
	}
	return &ErrLocked{Key: key, Info: held}
}

func (c *Consul) Unlock(ctx context.Context, key string) error {
	resp, body, err := c.do(ctx, http.MethodDelete, c.lockPath(key), "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Consul) Locks(ctx context.Context) ([]LockInfo, error) {
	resp, body, err := c.do(ctx, http.MethodGet, c.Path+".locks/", "recurse", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET locks: HTTP %d", resp.StatusCode)
	}
	var entries []consulEntry
	if err := json.Unmarshal(body, &entries); err != nil {
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
