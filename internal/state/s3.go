package state

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// S3 is an object in an S3 bucket, or in any S3-compatible store
// (s3://bucket/key with an endpoint; r2://account/bucket/key for
// Cloudflare R2). The version tag is the object's ETag and a write is
// conditional on it (If-Match, If-None-Match: * for the first), so the
// store must support conditional writes (AWS S3, R2 and MinIO do). The
// per-server locks are small objects next to it, created with
// If-None-Match: *.
type S3 struct {
	Endpoint  string // https://s3.eu-central-1.amazonaws.com, https://<account>.r2.cloudflarestorage.com, ...
	Bucket    string
	Key       string
	Region    string
	AccessKey string
	SecretKey string
	Client    *http.Client
	Now       func() time.Time
	label     string
}

func (s *S3) String() string {
	if s.label != "" {
		return s.label
	}
	return "s3://" + s.Bucket + "/" + s.Key
}

func (s *S3) do(ctx context.Context, method, key string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	u := strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if method == http.MethodPut && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/yaml")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	region := s.Region
	if region == "" {
		region = "auto"
	}
	signV4(req, body, s.AccessKey, s.SecretKey, region, now())
	client := s.Client
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

func s3Error(resp *http.Response, body []byte) error {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Errorf("%s: %s (HTTP %d)", e.Code, e.Message, resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

func (s *S3) Read(ctx context.Context) ([]byte, string, error) {
	resp, body, err := s.do(ctx, http.MethodGet, s.Key, nil, nil)
	if err != nil {
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusNotFound:
		return nil, "", nil
	}
	return nil, "", s3Error(resp, body)
}

func (s *S3) Write(ctx context.Context, data []byte, ifVersion string) (string, error) {
	headers := map[string]string{}
	if ifVersion == "" {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = `"` + ifVersion + `"`
	}
	resp, body, err := s.do(ctx, http.MethodPut, s.Key, data, headers)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", ErrConflict
	case http.StatusNotImplemented:
		return "", fmt.Errorf("the store does not support conditional writes (If-Match), which the state needs")
	}
	return "", s3Error(resp, body)
}

func (s *S3) lockKey(key string) string {
	return s.Key + ".locks/" + lockKey(key) + ".json"
}

func (s *S3) Lock(ctx context.Context, key string, info LockInfo) error {
	payload, _ := json.Marshal(info)
	resp, body, err := s.do(ctx, http.MethodPut, s.lockKey(key), payload, map[string]string{"If-None-Match": "*", "Content-Type": "application/json"})
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		held := LockInfo{Key: key}
		if r, b, err := s.do(ctx, http.MethodGet, s.lockKey(key), nil, nil); err == nil && r.StatusCode == http.StatusOK {
			json.Unmarshal(b, &held)
		}
		return &ErrLocked{Key: key, Info: held}
	}
	return s3Error(resp, body)
}

func (s *S3) Unlock(ctx context.Context, key string) error {
	resp, body, err := s.do(ctx, http.MethodDelete, s.lockKey(key), nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return s3Error(resp, body)
}

func (s *S3) Locks(ctx context.Context) ([]LockInfo, error) {
	prefix := s.Key + ".locks/"
	resp, body, err := s.do(ctx, http.MethodGet, "?list-type=2&prefix="+url.QueryEscape(prefix), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s3Error(resp, body)
	}
	var list struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var locks []LockInfo
	for _, c := range list.Contents {
		r, b, err := s.do(ctx, http.MethodGet, c.Key, nil, nil)
		if err != nil || r.StatusCode != http.StatusOK {
			continue
		}
		var info LockInfo
		if json.Unmarshal(b, &info) == nil {
			locks = append(locks, info)
		}
	}
	return locks, nil
}
