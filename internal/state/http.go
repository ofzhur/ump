package state

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

// HTTP is a document behind an HTTP service, Terraform-style: GET
// returns it with an ETag, PUT stores it with If-Match (If-None-Match: *
// for the first write; 412 or 409 is a conflict), LOCK and UNLOCK with
// ?key=<server> take and release a per-server lock (423 when held, with
// the holder's LockInfo as JSON in the body), and GET ?locks lists them.
// Authentication: a bearer token, or basic auth.
type HTTP struct {
	URL      string
	Token    string
	User     string
	Password string
	Client   *http.Client
}

func (h *HTTP) String() string { return h.URL }

func (h *HTTP) do(ctx context.Context, method, query string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	u := h.URL
	if query != "" {
		if strings.Contains(u, "?") {
			u += "&" + query
		} else {
			u += "?" + query
		}
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
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, out, err
}

func (h *HTTP) Read(ctx context.Context) ([]byte, string, error) {
	resp, body, err := h.do(ctx, http.MethodGet, "", nil, nil)
	if err != nil {
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusNotFound, http.StatusNoContent:
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("GET: HTTP %d", resp.StatusCode)
}

func (h *HTTP) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	headers := map[string]string{"Content-Type": "application/yaml"}
	if ifVersion == "" {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = `"` + ifVersion + `"`
	}
	resp, _, err := h.do(ctx, http.MethodPut, "", data, headers)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", ErrConflict
	}
	return "", fmt.Errorf("PUT: HTTP %d", resp.StatusCode)
}

func (h *HTTP) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	resp, body, err := h.do(ctx, "LOCK", "key="+lockKey(key), payload, map[string]string{"Content-Type": "application/json"})
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
	resp, _, err := h.do(ctx, "UNLOCK", "key="+lockKey(key), nil, nil)
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
	resp, body, err := h.do(ctx, http.MethodGet, "locks", nil, nil)
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
