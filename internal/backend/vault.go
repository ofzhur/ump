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

// Vault is a prefix in a KV version 2 engine of HashiCorp Vault
// (vault://host:8200/mount/prefix). Every file is a secret with the
// field "data" (base64); a file's version tag is the secret's version
// and a write is a check-and-set on it. The advisory locks are secrets
// under .locks/, created with cas=0 and deleted with their metadata.
type Vault struct {
	Address string // https://host:8200
	Mount   string
	Prefix  string
	Token   string
	Client  *http.Client
}

func (v *Vault) String() string {
	return "vault://" + strings.TrimPrefix(strings.TrimPrefix(v.Address, "http://"), "https://") + "/" + v.Mount + "/" + v.Prefix
}

func (v *Vault) secretPath(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(v.Prefix, "/") + "/" + p, nil
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
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
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

func (v *Vault) Read(ctx context.Context, p string) ([]byte, string, error) {
	path, err := v.secretPath(p)
	if err != nil {
		return nil, "", err
	}
	s, err := v.read(ctx, path)
	if err != nil || s == nil {
		return nil, "", err
	}
	data, err := base64.StdEncoding.DecodeString(s.Data.Data["data"])
	if err != nil {
		return nil, "", fmt.Errorf("%s: not base64", path)
	}
	return data, strconv.Itoa(s.Data.Metadata.Version), nil
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

func (v *Vault) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	path, err := v.secretPath(p)
	if err != nil {
		return "", err
	}
	return v.write(ctx, path, map[string]string{"data": base64.StdEncoding.EncodeToString(data)}, ifVersion)
}

func (v *Vault) deleteMetadata(ctx context.Context, path string) error {
	resp, body, err := v.do(ctx, http.MethodDelete, v.Mount+"/metadata/"+path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return vaultError(resp, body)
}

func (v *Vault) Delete(ctx context.Context, p string) error {
	path, err := v.secretPath(p)
	if err != nil {
		return err
	}
	return v.deleteMetadata(ctx, path)
}

// walk lists the secrets under a prefix, recursively (a key ending in
// "/" is a folder).
func (v *Vault) walk(ctx context.Context, prefix string, out *[]string) error {
	resp, body, err := v.do(ctx, "LIST", v.Mount+"/metadata/"+prefix, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return vaultError(resp, body)
	}
	var list struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return err
	}
	for _, k := range list.Data.Keys {
		if strings.HasSuffix(k, "/") {
			if err := v.walk(ctx, prefix+k, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, prefix+k)
	}
	return nil
}

func (v *Vault) List(ctx context.Context) ([]Entry, error) {
	prefix := strings.TrimRight(v.Prefix, "/") + "/"
	var paths []string
	if err := v.walk(ctx, prefix, &paths); err != nil {
		return nil, err
	}
	var out []Entry
	for _, p := range paths {
		rel := strings.TrimPrefix(p, prefix)
		if Excluded(rel) {
			continue
		}
		// the version is in the metadata; one call per file
		resp, body, err := v.do(ctx, http.MethodGet, v.Mount+"/metadata/"+p, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var meta struct {
			Data struct {
				CurrentVersion int `json:"current_version"`
			} `json:"data"`
		}
		json.Unmarshal(body, &meta)
		out = append(out, Entry{Path: rel, Version: strconv.Itoa(meta.Data.CurrentVersion)})
	}
	return out, nil
}

func (v *Vault) lockPath(key string) string {
	return strings.TrimRight(v.Prefix, "/") + "/.locks/" + lockKey(key)
}

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
	return v.deleteMetadata(ctx, v.lockPath(key))
}

func (v *Vault) Locks(ctx context.Context) ([]LockInfo, error) {
	var paths []string
	if err := v.walk(ctx, strings.TrimRight(v.Prefix, "/")+"/.locks/", &paths); err != nil {
		return nil, err
	}
	var locks []LockInfo
	for _, p := range paths {
		s, err := v.read(ctx, p)
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
