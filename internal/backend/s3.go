package backend

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

// S3 is a prefix in an S3 bucket, or in any S3-compatible store
// (s3://bucket/prefix with an endpoint; r2://account/bucket/prefix for
// Cloudflare R2). A file's version tag is the object's ETag and a write
// is conditional on it (If-Match, If-None-Match: * for the first), so
// the store must support conditional writes (AWS S3, R2 and MinIO do).
// The advisory locks are small objects under .locks/, created with
// If-None-Match: *.
type S3 struct {
	Endpoint  string // https://s3.eu-central-1.amazonaws.com, https://<account>.r2.cloudflarestorage.com, ...
	Bucket    string
	Prefix    string
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
	return "s3://" + s.Bucket + "/" + s.Prefix
}

func (s *S3) object(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if s.Prefix == "" {
		return p, nil
	}
	return strings.TrimRight(s.Prefix, "/") + "/" + p, nil
}

// do sends a request for an object key (or, with a key that starts with
// "?", for the bucket with that query). The key is sent and signed in
// SigV4's encoding, so that a key with characters such as "%" or a space
// (the lock of vultr/ID is the object vultr%2FID.json) reaches the same
// object the signature names.
func (s *S3) do(ctx context.Context, method, key string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	key, rawQuery, _ := strings.Cut(key, "?")
	u, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil {
		return nil, nil, err
	}
	objectPath := "/" + s.Bucket + "/" + key
	u.RawPath = u.EscapedPath() + awsEscape(objectPath, false)
	u.Path += objectPath
	if rawQuery != "" {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return nil, nil, err
		}
		u.RawQuery = awsQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if method == http.MethodPut && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
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

func (s *S3) Read(ctx context.Context, p string) ([]byte, string, error) {
	key, err := s.object(p)
	if err != nil {
		return nil, "", err
	}
	resp, body, err := s.do(ctx, http.MethodGet, key, nil, nil)
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

func (s *S3) Write(ctx context.Context, p string, data []byte, ifVersion string) (string, error) {
	key, err := s.object(p)
	if err != nil {
		return "", err
	}
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if ifVersion == "" {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = `"` + ifVersion + `"`
	}
	resp, body, err := s.do(ctx, http.MethodPut, key, data, headers)
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

func (s *S3) Delete(ctx context.Context, p string) error {
	key, err := s.object(p)
	if err != nil {
		return err
	}
	resp, body, err := s.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return s3Error(resp, body)
}

// list returns the objects under a prefix with their ETags.
func (s *S3) list(ctx context.Context, prefix string) ([]Entry, error) {
	var out []Entry
	token := ""
	for {
		q := "?list-type=2&prefix=" + url.QueryEscape(prefix)
		if token != "" {
			q += "&continuation-token=" + url.QueryEscape(token)
		}
		resp, body, err := s.do(ctx, http.MethodGet, q, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, s3Error(resp, body)
		}
		var list struct {
			Contents []struct {
				Key  string `xml:"Key"`
				ETag string `xml:"ETag"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		for _, c := range list.Contents {
			out = append(out, Entry{Path: c.Key, Version: strings.Trim(c.ETag, `"`)})
		}
		if !list.IsTruncated || list.NextContinuationToken == "" {
			return out, nil
		}
		token = list.NextContinuationToken
	}
}

func (s *S3) List(ctx context.Context) ([]Entry, error) {
	prefix := ""
	if s.Prefix != "" {
		prefix = strings.TrimRight(s.Prefix, "/") + "/"
	}
	objects, err := s.list(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, o := range objects {
		rel := strings.TrimPrefix(o.Path, prefix)
		if rel == "" || Excluded(rel) {
			continue
		}
		out = append(out, Entry{Path: rel, Version: o.Version})
	}
	return out, nil
}

func (s *S3) locksPrefix() string {
	if s.Prefix == "" {
		return ".locks/"
	}
	return strings.TrimRight(s.Prefix, "/") + "/.locks/"
}

func (s *S3) lockKey(key string) string { return s.locksPrefix() + lockKey(key) + ".json" }

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
	objects, err := s.list(ctx, s.locksPrefix())
	if err != nil {
		return nil, err
	}
	var locks []LockInfo
	for _, c := range objects {
		r, b, err := s.do(ctx, http.MethodGet, c.Path, nil, nil)
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
