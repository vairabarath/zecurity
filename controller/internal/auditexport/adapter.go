package auditexport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Batch is one delivery: consecutive events of one workspace stream.
type Batch struct {
	DeliveryID  string // uuid, X-Zecurity-Delivery
	WorkspaceID string
	Stream      string
	Events      []Event
}

// Destination is a sink's decrypted delivery settings. It never leaves the
// exporter (no logging, no API).
type Destination struct {
	SinkID          string
	URL             string
	SigningKey      []byte // HMAC-SHA256 key for X-Zecurity-Signature
	AuthHeaderName  string // optional static header, e.g. "Authorization"
	AuthHeaderValue string
	Config          json.RawMessage // adapter settings (non-secret)
}

// Adapter formats and delivers batches for one kind of SIEM (P-6). Shared
// code (Client, post) handles HTTP; adapters only shape the request.
type Adapter interface {
	Kind() string
	// ValidateConfig checks the non-secret adapter settings.
	ValidateConfig(cfg json.RawMessage) error
	// Deliver sends the batch; nil means the destination acknowledged it.
	Deliver(ctx context.Context, c *http.Client, d Destination, b Batch) error
}

// Registry maps kind → adapter.
type Registry struct{ adapters map[string]Adapter }

// NewRegistry registers the given adapters.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range as {
		r.adapters[a.Kind()] = a
	}
	return r
}

// DefaultRegistry holds the v1 adapters (S-8): webhook and Splunk HEC.
func DefaultRegistry() *Registry { return NewRegistry(Webhook{}, SplunkHEC{}) }

// Get returns the adapter for kind.
func (r *Registry) Get(kind string) (Adapter, bool) {
	a, ok := r.adapters[kind]
	return a, ok
}

// Kinds lists the registered kinds.
func (r *Registry) Kinds() []string {
	ks := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Signature returns the X-Zecurity-Signature value:
// t=<unix>,v1=<hex HMAC-SHA256(key, "<unix>." + body)>.
func Signature(key []byte, t time.Time, body []byte) string {
	unix := strconv.FormatInt(t.Unix(), 10)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(unix))
	m.Write([]byte("."))
	m.Write(body)
	return "t=" + unix + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// DeliveryError is a failed delivery, worded for the console: it never
// carries the URL (which may hold a token), headers or the response body.
type DeliveryError struct{ Msg string }

func (e *DeliveryError) Error() string { return e.Msg }

// maxResponseRead bounds how much of a response is read (and discarded).
const maxResponseRead = 4 << 10

// post sends a signed request. Any 2xx is an acknowledgement.
func post(ctx context.Context, c *http.Client, d Destination, b Batch, contentType string, body []byte, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytesReader(body))
	if err != nil {
		return &DeliveryError{Msg: "invalid destination URL"}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "zecurity-audit-export/1")
	req.Header.Set("X-Zecurity-Delivery", b.DeliveryID)
	req.Header.Set("X-Zecurity-Signature", Signature(d.SigningKey, now, body))
	if d.AuthHeaderName != "" {
		req.Header.Set(d.AuthHeaderName, d.AuthHeaderValue)
	}
	resp, err := c.Do(req)
	if err != nil {
		return &DeliveryError{Msg: describeTransportError(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseRead))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &DeliveryError{Msg: fmt.Sprintf("destination answered HTTP %d", resp.StatusCode)}
	}
	return nil
}

// ErrorMessage returns a console-safe description of a delivery failure.
func ErrorMessage(err error) string {
	var de *DeliveryError
	if errors.As(err, &de) {
		return de.Msg
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "delivery timed out"
	}
	return "delivery failed"
}
