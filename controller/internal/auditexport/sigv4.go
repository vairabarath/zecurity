package auditexport

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

// AWS Signature Version 4 for the one request the S3 sink makes, PutObject
// (PENDING-11 §9 W-6). Hand-written so the shared SSRF-safe client stays in
// charge of every connection; checked against AWS's published examples and
// botocore in sigv4_test.go.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
)

// sigV4Credentials is an access key pair.
type sigV4Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// sha256Hex returns hex(SHA-256(b)).
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncode encodes s per SigV4: everything except A-Z a-z 0-9 - _ . ~ is
// percent-encoded (upper-case hex); '/' is kept when encodeSlash is false.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
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

// canonicalQuery sorts and encodes the query parameters.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// signV4 adds x-amz-date, x-amz-content-sha256 (unless set) and
// Authorization to req. payloadHash is hex(SHA-256(body)). Every header
// already on req, plus Host, is signed. req.URL.Path must be the decoded
// object path; the canonical URI encodes it once (S3 rules: no double
// encoding, no path normalisation).
func signV4(req *http.Request, creds sigV4Credentials, region, service, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateFormat)
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	if req.Header.Get("X-Amz-Content-Sha256") == "" && service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, vs := range req.Header {
		l := strings.ToLower(name)
		if l == "authorization" || l == "user-agent" {
			continue
		}
		vals := make([]string, len(vs))
		for i, v := range vs {
			vals[i] = strings.Join(strings.Fields(v), " ") // trim, collapse spaces
		}
		headers[l] = strings.Join(vals, ",")
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n)
		ch.WriteByte(':')
		ch.WriteString(headers[n])
		ch.WriteByte('\n')
	}
	signed := strings.Join(names, ";")

	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	canonicalURI := uriEncode(path, false)
	if service != "s3" {
		// Non-S3 services encode the (already encoded) path a second time.
		canonicalURI = uriEncode(canonicalURI, false)
	}
	canonical := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery(req.URL.Query()),
		ch.String(),
		signed,
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{sigV4Algorithm, amzDate, scope, sha256Hex([]byte(canonical))}, "\n")

	k := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	req.Header.Set("Authorization", sigV4Algorithm+" Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}
