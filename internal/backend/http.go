package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTP is a service that stores files under a base URL, Terraform-style:
// GET <base>/<path> returns a file with an ETag, PUT stores it with
// If-Match (If-None-Match: * for the first write; 412 or 409 is a
// conflict), DELETE removes it, GET <base>?list returns every file as
// JSON [{"path": ..., "version": ...}], LOCK and UNLOCK <base>?key=NAME
// take and release an advisory lock (423 when held, with the holder's
// LockInfo as JSON in the body), and GET <base>?locks lists them.
// Authentication: a bearer token, or basic auth.
type HTTP struct {
	URL      string
	Token    string
	User     string
	Password string
	Client   *http.Client
}

func (h *HTTP) String() string { return h.URL }

func (h *HTTP) do(ctx context.Context, method, p, query string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	u := strings.TrimRight(h.URL, "/")
	if p != "" {
		u += "/" + p
	}
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	switch {
	case h.Token != "":
		req.Header.Set("Authorization", "Bearer "+h.Token)
	case h.User != "":
		req.SetBasicAuth(h.User, h.Password)
	}
	client := h.Client
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

func (h *HTTP) Read(ctx context.Context, p string) ([]byte, string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return nil, "", err
	}
	resp, body, err := h.do(ctx, http.MethodGet, p, "", nil, nil)
	if err != nil {
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusNotFound, http.StatusNoContent:
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("GET %s: HTTP %d", p, resp.StatusCode)
}

func (h *HTTP) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if ifVersion == "" {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = `"` + ifVersion + `"`
	}
	resp, _, err := h.do(ctx, http.MethodPut, p, "", data, headers)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", ErrConflict
	}
	return "", fmt.Errorf("PUT %s: HTTP %d", p, resp.StatusCode)
}

func (h *HTTP) Delete(ctx context.Context, p string) error {
	p, err := cleanPath(p)
	if err != nil {
		return err
	}
	resp, _, err := h.do(ctx, http.MethodDelete, p, "", nil, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("DELETE %s: HTTP %d", p, resp.StatusCode)
}

func (h *HTTP) List(ctx context.Context) ([]Entry, error) {
	resp, body, err := h.do(ctx, http.MethodGet, "", "list", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET ?list: HTTP %d", resp.StatusCode)
	}
	var raw []struct {
		Path    string `json:"path"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("GET ?list: %w", err)
	}
	var out []Entry
	for _, e := range raw {
		if !Excluded(e.Path) {
			out = append(out, Entry{Path: e.Path, Version: e.Version})
		}
	}
	return out, nil
}

func (h *HTTP) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	resp, body, err := h.do(ctx, "LOCK", "", "key="+lockKey(key), payload, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusLocked, http.StatusConflict:
		held := LockInfo{Key: key}
		json.Unmarshal(body, &held)
		return &ErrLocked{Key: key, Info: held}
	}
	return fmt.Errorf("LOCK: HTTP %d", resp.StatusCode)
}

func (h *HTTP) Unlock(ctx context.Context, key string) error {
	resp, _, err := h.do(ctx, "UNLOCK", "", "key="+lockKey(key), nil, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("UNLOCK: HTTP %d", resp.StatusCode)
}

func (h *HTTP) Locks(ctx context.Context) ([]LockInfo, error) {
	resp, body, err := h.do(ctx, http.MethodGet, "", "locks", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET ?locks: HTTP %d", resp.StatusCode)
	}
	var locks []LockInfo
	if err := json.Unmarshal(body, &locks); err != nil {
		return nil, fmt.Errorf("GET ?locks: %w", err)
	}
	return locks, nil
}
