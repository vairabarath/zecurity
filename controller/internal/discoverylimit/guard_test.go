package discoverylimit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/yourorg/ztna/controller/internal/metrics"
)

// A resolver reached without the discovery middleware (a wiring mistake) is
// refused: the guard fails closed instead of running unlimited.
func TestCharge_WithoutRequestStateFailsClosed(t *testing.T) {
	err := Charge(context.Background(), OpSlugLookup, "")
	var ge *gqlerror.Error
	if !errors.As(err, &ge) || ge.Extensions["code"] != CodeUnavailable {
		t.Fatalf("err = %v, want a %s GraphQL error", err, CodeUnavailable)
	}
}

func TestProtectHTTP(t *testing.T) {
	l, mr := newTestLimiter(t, Config{IPLimit: 2})
	var served int
	h := ProtectHTTP(l, NewIPResolver(nil), OpRESTAuth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusNotFound) // the handler's own answers pass through untouched
	}))
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/workspaces/acme/auth", nil)
		req.RemoteAddr = "203.0.113.7:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := do(); w.Code != http.StatusNotFound {
			t.Fatalf("request %d below the limit: %d", i, w.Code)
		}
	}
	w := do()
	if w.Code != http.StatusTooManyRequests || served != 2 {
		t.Fatalf("over the limit: status %d, handler ran %d times", w.Code, served)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "rate_limited" {
		t.Fatalf("body = %s", w.Body.String())
	}

	mr.Close()
	w = do()
	if w.Code != http.StatusServiceUnavailable || served != 2 {
		t.Fatalf("Valkey down: status %d, handler ran %d times", w.Code, served)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "service_unavailable" || len(body) != 1 {
		t.Fatalf("Valkey down body = %s", w.Body.String())
	}
}

// The /graphql wrapper turns a resolver's refusal into the HTTP status, and
// leaves every other response exactly as the handler wrote it.
func TestMiddleware_StatusMapping(t *testing.T) {
	l, mr := newTestLimiter(t, Config{IPLimit: 1})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("discover") == "1" {
			_ = Charge(r.Context(), OpSlugLookup, "")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	h := Middleware(l, NewIPResolver(nil))(inner)
	do := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/graphql?"+q, nil)
		req.RemoteAddr = "203.0.113.7:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := do("discover=0"); w.Code != 200 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("non-discovery request changed: %d", w.Code)
	}
	if w := do("discover=1"); w.Code != 200 {
		t.Fatalf("first discovery request: %d", w.Code)
	}
	w := do("discover=1")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := do("discover=0"); w.Code != 200 {
		t.Fatalf("a request without discovery fields is never limited: %d", w.Code)
	}
	mr.Close()
	if w := do("discover=1"); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "" {
		t.Fatalf("Valkey down: %d", w.Code)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

// The websocket transport hijacks the connection; the wrapper must allow it.
func TestMiddleware_PassesHijack(t *testing.T) {
	l, _ := newTestLimiter(t, Config{})
	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := Middleware(l, NewIPResolver(nil))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("wrapped writer is not an http.Hijacker")
		}
		c, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}))
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/graphql", nil))
	if !rec.hijacked {
		t.Fatal("hijack did not reach the underlying writer")
	}
}

var discoverySeries = regexp.MustCompile(`discovery_requests_total\{operation="([^"]*)",result="([^"]*)"\}`)

// Metric labels are bounded: only the known operations and results ever
// appear, never an IP or an email.
func TestMetrics_BoundedLabels(t *testing.T) {
	l, _ := newTestLimiter(t, Config{IPLimit: 1, EmailLimit: 1})
	ctx := context.Background()
	_, _ = l.Allow(ctx, OpEmailLookup, "4:203.0.113.7", "victim@example.com")
	_, _ = l.Allow(ctx, OpEmailLookup, "4:203.0.113.7", "victim@example.com")
	_, _ = l.Allow(ctx, OpEmailLookup, "4:203.0.113.8", "victim@example.com")

	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	text := w.Body.String()
	if strings.Contains(text, "203.0.113") || strings.Contains(text, "victim") {
		t.Fatal("an IP or email reached the metrics")
	}
	ops := map[string]bool{}
	for _, o := range Operations {
		ops[string(o)] = true
	}
	results := map[string]bool{ResultAllowed: true, ResultIPLimited: true, ResultEmailLimited: true, ResultLimiterError: true}
	seen := 0
	for _, m := range discoverySeries.FindAllStringSubmatch(text, -1) {
		seen++
		if !ops[m[1]] || !results[m[2]] {
			t.Fatalf("unexpected label set operation=%q result=%q", m[1], m[2])
		}
	}
	if seen != len(Operations)*4 {
		t.Fatalf("%d series exposed, want every operation x result pre-created (%d)", seen, len(Operations)*4)
	}
}

func TestDeniedResponse(t *testing.T) {
	r := deniedResponse(&Denial{Status: 429, Code: CodeRateLimited, RetryAfter: 1500 * time.Millisecond})
	if string(r.Data) != "null" || len(r.Errors) != 1 || r.Errors[0].Extensions["code"] != CodeRateLimited ||
		r.Errors[0].Extensions["retryAfterSeconds"] != 2 {
		t.Fatalf("denied response = %+v", r)
	}
	r = deniedResponse(&Denial{Status: 503, Code: CodeUnavailable})
	if _, ok := r.Errors[0].Extensions["retryAfterSeconds"]; ok {
		t.Fatal("no retry hint when the limiter is down")
	}
}
