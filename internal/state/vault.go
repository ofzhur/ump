package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Vault is a secret in a KV version 2 engine of HashiCorp Vault
// (vault://host:8200/mount/path/to/state). The document is the field
// "state" of the secret; the version tag is the secret's version and a
// write is a check-and-set on it. The per-server locks are secrets
// below <path>.locks/, created with cas=0 and deleted with their
// metadata.
type Vault struct {
	Address string // https://host:8200
	Mount   string
	Path    string
	Token   string
	Client  *http.Client
}

func (v *Vault) String() string {
	return "vault://" + strings.TrimPrefix(strings.TrimPrefix(v.Address, "http://"), "https://") + "/" + v.Mount + "/" + v.Path
}

func (v *Vault) do(ctx context.Context, method, path string, body any) (*http.Response, []byte, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.Address, "/")+"/v1/"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X-Vault-Token", v.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := v.Client
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

func vaultError(resp *http.Response, body []byte) error {
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 {
		return fmt.Errorf("%s (HTTP %d)", strings.Join(e.Errors, "; "), resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

type vaultSecret struct {
	Data struct {
		Data     map[string]string `json:"data"`
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

func (v *Vault) read(ctx context.Context, path string) (*vaultSecret, error) {
	resp, body, err := v.do(ctx, http.MethodGet, v.Mount+"/data/"+path, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var s vaultSecret
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, err
		}
		return &s, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, vaultError(resp, body)
}

func (v *Vault) Read(ctx context.Context) ([]byte, string, error) {
	s, err := v.read(ctx, v.Path)
	if err != nil || s == nil {
		return nil, "", err
	}
	return []byte(s.Data.Data["state"]), strconv.Itoa(s.Data.Metadata.Version), nil
}

func (v *Vault) write(ctx context.Context, path string, data map[string]string, cas string) (string, error) {
	n := 0
	if cas != "" {
		var err error
		if n, err = strconv.Atoi(cas); err != nil {
			return "", ErrConflict // a version that never was
		}
	}
	resp, body, err := v.do(ctx, http.MethodPost, v.Mount+"/data/"+path, map[string]any{"data": data, "options": map[string]any{"cas": n}})
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var s vaultSecret
		if json.Unmarshal(body, &s) == nil && s.Data.Metadata.Version > 0 {
			return strconv.Itoa(s.Data.Metadata.Version), nil
		}
		var raw struct {
			Data struct {
				Version int `json:"version"`
			} `json:"data"`
		}
		json.Unmarshal(body, &raw)
		return strconv.Itoa(raw.Data.Version), nil
	case http.StatusBadRequest:
		if bytes.Contains(body, []byte("check-and-set")) {
			return "", ErrConflict
		}
	}
	return "", vaultError(resp, body)
}

func (v *Vault) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	return v.write(ctx, v.Path, map[string]string{"state": string(data)}, ifVersion)
}

func (v *Vault) lockPath(key string) string { return v.Path + ".locks/" + lockKey(key) }

func (v *Vault) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	_, err := v.write(ctx, v.lockPath(key), map[string]string{"lock": string(payload)}, "0")
	if err == nil {
		return nil
	}
	if err != ErrConflict {
		return err
	}
	held := LockInfo{Key: key}
	if s, err := v.read(ctx, v.lockPath(key)); err == nil && s != nil {
		json.Unmarshal([]byte(s.Data.Data["lock"]), &held)
	}
	return &ErrLocked{Key: key, Info: held}
}

func (v *Vault) Unlock(ctx context.Context, key string) error {
	resp, body, err := v.do(ctx, http.MethodDelete, v.Mount+"/metadata/"+v.lockPath(key), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return vaultError(resp, body)
}

func (v *Vault) Locks(ctx context.Context) ([]LockInfo, error) {
	resp, body, err := v.do(ctx, "LIST", v.Mount+"/metadata/"+v.Path+".locks/", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, vaultError(resp, body)
	}
	var list struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var locks []LockInfo
	for _, k := range list.Data.Keys {
		s, err := v.read(ctx, v.Path+".locks/"+k)
		if err != nil || s == nil {
			continue
		}
		var info LockInfo
		if json.Unmarshal([]byte(s.Data.Data["lock"]), &info) == nil {
			locks = append(locks, info)
		}
	}
	return locks, nil
}
