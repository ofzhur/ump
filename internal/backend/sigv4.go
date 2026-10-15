package backend

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

	// the canonical URI is the decoded path encoded again the SigV4 way,
	// which is what the server computes from what it received
	canonical := strings.Join([]string{req.Method, awsEscape(req.URL.Path, false), awsQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHex}, "\n")
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

// awsEscape encodes s the way SigV4 does: every byte but the unreserved
// characters A-Z a-z 0-9 - _ . ~ as %XX in upper-case hex, and "/" kept
// unless encodeSlash.
func awsEscape(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// awsQuery is a query string in SigV4's canonical form: keys and values
// encoded with awsEscape, sorted by key and value. The request sends the
// same string, so the two cannot differ.
func awsQuery(q url.Values) string {
	var pairs []string
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, awsEscape(k, true)+"="+awsEscape(v, true))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
