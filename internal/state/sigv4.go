package state

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signV4 signs an S3 request with AWS Signature Version 4 (the payload
// hash is sent as a header, so the body is read once).
func signV4(req *http.Request, body []byte, accessKey, secretKey, region string, now time.Time) {
	payload := sha256.Sum256(body)
	payloadHex := hex.EncodeToString(payload[:])
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHex)
	req.Header.Set("host", req.URL.Host)

	var names []string
	for k := range req.Header {
		k = strings.ToLower(k)
		if k == "host" || k == "content-type" || strings.HasPrefix(k, "x-amz-") || k == "if-match" || k == "if-none-match" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		v := req.Header.Get(k)
		if k == "host" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
	}
	signed := strings.Join(names, ";")

	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonQuery []string
	for _, k := range keys {
		for _, v := range query[k] {
			canonQuery = append(canonQuery, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), strings.Join(canonQuery, "&"), canonHeaders.String(), signed, payloadHex}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	canonHash := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(canonHash[:])}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+secretKey), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
