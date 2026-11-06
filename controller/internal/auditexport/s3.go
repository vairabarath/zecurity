package auditexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ── Amazon S3 (PENDING-11 §9, W-3 … W-6) ───────────────────────────────────

// S3 writes each batch as one gzip NDJSON object in the customer's bucket,
// Twingate-style, for the SIEM to pull (Wazuh: aws-s3 "Custom Logs Buckets").
//
//	<prefix>/zecurity/v1/<workspace_id>/<stream>/YYYY/MM/DD/HH/<first>-<last>.jsonl.gz
//
// The hour is the first event's recorded_at (UTC). Delivery stays
// at-least-once: a retry that picks up more events writes a different key,
// so consumers deduplicate on (workspace_id, stream, seq).
//
// The secret access key is stored in the sealed auth-header slot under
// S3SecretSlot; it is never sent as a header (SigV4 signs with it). The
// object also carries x-amz-meta-zecurity-signature, the sink's HMAC over
// the gzip body.
type S3 struct{}

const (
	// S3SecretSlot is the auth-header name the secret access key is stored
	// under.
	S3SecretSlot = "AWS-Secret-Access-Key"
	// S3BatchWindow is how long the oldest pending event waits before an
	// object is written (W-5), unless MaxBatchEvents are pending.
	S3BatchWindow = 5 * time.Minute
)

// S3Config is the non-secret S3 settings (stored in the sink's config).
type S3Config struct {
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
	Prefix      string `json:"prefix,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"` // S3-compatible store, path-style
	AccessKeyID string `json:"access_key_id"`
}

var (
	s3Bucket      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3Region      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$`)
	s3Prefix      = regexp.MustCompile(`^[A-Za-z0-9/_.-]{0,256}$`)
	s3AccessKeyID = regexp.MustCompile(`^[A-Za-z0-9]{3,128}$`)
)

func (S3) Kind() string { return "s3" }

func (S3) BatchWindow() time.Duration { return S3BatchWindow }

func (S3) parse(cfg json.RawMessage) (S3Config, error) {
	var c S3Config
	if err := decodeStrict(cfg, &c); err != nil {
		return c, err
	}
	if !s3Bucket.MatchString(c.Bucket) || strings.Contains(c.Bucket, "..") {
		return c, errors.New("S3 bucket must be a valid bucket name (3-63 characters: a-z 0-9 . -)")
	}
	if !s3Region.MatchString(c.Region) {
		return c, errors.New("S3 region must look like eu-west-1")
	}
	if !s3Prefix.MatchString(c.Prefix) || strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, "//") ||
		strings.HasSuffix(c.Prefix, "/") {
		return c, errors.New("S3 prefix must be up to 256 characters of A-Z a-z 0-9 / _ . - with no leading, trailing or double '/'")
	}
	for _, seg := range strings.Split(c.Prefix, "/") {
		if seg == "." || seg == ".." {
			return c, errors.New("S3 prefix must not contain '.' or '..' segments")
		}
	}
	if !s3AccessKeyID.MatchString(c.AccessKeyID) {
		return c, errors.New("S3 access key ID must be 3-128 letters or digits")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("S3 endpoint must be https://host[:port] with no path")
		}
	}
	return c, nil
}

func (a S3) ValidateConfig(cfg json.RawMessage) error {
	_, err := a.parse(cfg)
	return err
}

// DeriveURL is the sink's destination URL, built from its config: the bucket
// address, without key. The store keeps it in the url column, so the dial-time
// SSRF guard and the "credential doesn't follow a new destination" rule apply
// to S3 sinks as to the others.
func (a S3) DeriveURL(cfg json.RawMessage) (string, error) {
	c, err := a.parse(cfg)
	if err != nil {
		return "", err
	}
	return s3BucketURL(c), nil
}

func s3BucketURL(c S3Config) string {
	switch {
	case c.Endpoint != "":
		return strings.TrimSuffix(c.Endpoint, "/") + "/" + c.Bucket
	case strings.Contains(c.Bucket, "."):
		// Dotted names break the *.s3 wildcard certificate: path-style.
		return "https://s3." + c.Region + ".amazonaws.com/" + c.Bucket
	default:
		return "https://" + c.Bucket + ".s3." + c.Region + ".amazonaws.com"
	}
}

// S3ObjectKey is the object key for a batch (W-4).
func S3ObjectKey(prefix string, b Batch, now time.Time) string {
	first, last := b.Events[0], b.Events[len(b.Events)-1]
	at := now.UTC()
	if t, err := time.Parse(time.RFC3339Nano, first.RecordedAt); err == nil {
		at = t.UTC()
	}
	name := fmt.Sprintf("%d-%d", first.Seq, last.Seq)
	if first.Seq == 0 || last.Seq == 0 {
		// export.test and export.gap carry no seq of their own.
		name = first.Action + "-" + b.DeliveryID
	}
	key := fmt.Sprintf("zecurity/v1/%s/%s/%s/%s.jsonl.gz", b.WorkspaceID, b.Stream, at.Format("2006/01/02/15"), name)
	if prefix != "" {
		key = prefix + "/" + key
	}
	return key
}

// s3Body is the gzip NDJSON object: one zecurity.audit.v1 event per line.
func s3Body(events []Event) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw) // Encode appends '\n'
	enc.SetEscapeHTML(false)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a S3) Deliver(ctx context.Context, c *http.Client, d Destination, b Batch) error {
	cfg, err := a.parse(d.Config)
	if err != nil {
		return &DeliveryError{Msg: "invalid S3 config"}
	}
	if d.AuthHeaderName != S3SecretSlot || d.AuthHeaderValue == "" {
		return &DeliveryError{Msg: "S3 secret access key is not set"}
	}
	if len(b.Events) == 0 {
		return nil
	}
	body, err := s3Body(b.Events)
	if err != nil {
		return fmt.Errorf("encode batch: %w", err)
	}
	now := time.Now()
	u, err := url.Parse(d.URL)
	if err != nil {
		return &DeliveryError{Msg: "invalid destination URL"}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + S3ObjectKey(cfg.Prefix, b, now)
	u.RawPath = uriEncode(u.Path, false) // the bytes on the wire are the bytes signed
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{Msg: "invalid destination URL"}
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Amz-Meta-Zecurity-Delivery", b.DeliveryID)
	req.Header.Set("X-Amz-Meta-Zecurity-Signature", Signature(d.SigningKey, now, body))
	signV4(req, sigV4Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: d.AuthHeaderValue},
		cfg.Region, "s3", sha256Hex(body), now)
	req.Header.Set("User-Agent", "zecurity-audit-export/1")

	resp, err := c.Do(req)
	if err != nil {
		return &DeliveryError{Msg: describeTransportError(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseRead))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		return &DeliveryError{Msg: "S3 refused the request (HTTP 403): check the access key, its PutObject permission and the region"}
	case resp.StatusCode == http.StatusNotFound:
		return &DeliveryError{Msg: "S3 answered HTTP 404: check the bucket name and region"}
	case resp.StatusCode == http.StatusMovedPermanently:
		return &DeliveryError{Msg: "S3 answered HTTP 301: check the region"}
	case resp.StatusCode == http.StatusBadRequest:
		// Many causes (wrong region, a bucket policy or Object Lock default
		// retention that needs Content-MD5, …); the body isn't shown.
		return &DeliveryError{Msg: "S3 refused the request (HTTP 400): check the region and the bucket's policy and Object Lock settings"}
	default:
		return &DeliveryError{Msg: fmt.Sprintf("destination answered HTTP %d", resp.StatusCode)}
	}
}
