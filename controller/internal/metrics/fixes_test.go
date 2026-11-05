package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 1024)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

func gaugeFor(t *testing.T, reg *prometheus.Registry, name, entity string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if hasLabel(m, "entity", entity) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// The runway must be computed at scrape time: it used to be a value set once
// at startup (CA entities: never set, so a constant 0 that fired CertInvalid and
// CertExpiryImminentCritical on a healthy controller).
func TestCertCollectorRunwayIsLiveAndUnsetIsAbsent(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c := &certCollector{windows: map[string]certWindow{}, now: func() time.Time { return now },
		runway: certs.runway, valid: certs.valid, lifetime: certs.lifetime}
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	if _, ok := gaugeFor(t, reg, "cert_valid", "root_ca"); ok {
		t.Fatal("root_ca reported before anyone recorded it")
	}

	c.set("root_ca", certWindow{notBefore: now.Add(-time.Hour), notAfter: now.Add(10 * 24 * time.Hour), valid: true})
	r1, _ := gaugeFor(t, reg, "cert_runway_seconds", "root_ca")
	if r1 != (10 * 24 * time.Hour).Seconds() {
		t.Fatalf("runway = %v", r1)
	}
	if v, _ := gaugeFor(t, reg, "cert_valid", "root_ca"); v != 1 {
		t.Fatalf("cert_valid = %v, want 1", v)
	}
	if l, _ := gaugeFor(t, reg, "cert_lifetime_seconds", "root_ca"); l != (10*24*time.Hour + time.Hour).Seconds() {
		t.Fatalf("lifetime = %v", l)
	}

	now = now.Add(24 * time.Hour)
	if r2, _ := gaugeFor(t, reg, "cert_runway_seconds", "root_ca"); r2 != r1-86400 {
		t.Fatalf("runway after a day = %v, want %v", r2, r1-86400)
	}

	now = now.Add(20 * 24 * time.Hour)
	r3, _ := gaugeFor(t, reg, "cert_runway_seconds", "root_ca")
	v3, _ := gaugeFor(t, reg, "cert_valid", "root_ca")
	if r3 != 0 || v3 != 0 {
		t.Fatalf("expired cert: runway %v valid %v, want 0 0", r3, v3)
	}

	c.set("intermediate_ca", certWindow{notBefore: now.Add(-time.Hour), notAfter: now.Add(time.Hour), valid: false})
	if v, _ := gaugeFor(t, reg, "cert_valid", "intermediate_ca"); v != 0 {
		t.Fatalf("valid=false must report 0, got %v", v)
	}
}

func TestPKIRotationCounter(t *testing.T) {
	ok0 := counterValue(t, pkiRotations.WithLabelValues("controller_grpc", "ok"))
	er0 := counterValue(t, pkiRotations.WithLabelValues("controller_grpc", "error"))
	RecordPKIRotation("controller_grpc", nil)
	RecordPKIRotation("controller_grpc", errors.New("boom"))
	if counterValue(t, pkiRotations.WithLabelValues("controller_grpc", "ok")) != ok0+1 ||
		counterValue(t, pkiRotations.WithLabelValues("controller_grpc", "error")) != er0+1 {
		t.Fatal("rotation counter didn't move")
	}
}

// http_requests_total is labelled by the ServeMux route pattern, never the raw
// path (which carries IDs), and unmatched paths collapse to one series.
func TestHTTPMiddlewareLabelsByRoutePattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /provider/relays/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	h := HTTPMiddleware(mux)

	before := seriesCount(httpRequests)
	for _, target := range []string{"/provider/relays/7f9c2a", "/provider/relays/0b11d3", "/graphql", "/nope/a1", "/nope/b2"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/graphql", nil))

	if got := counterValue(t, httpRequests.WithLabelValues("GET", "GET /provider/relays/{id}", "418")); got < 2 {
		t.Fatalf("pattern series = %v, want >= 2", got)
	}
	if got := counterValue(t, httpRequests.WithLabelValues("GET", "/graphql", "200")); got < 1 {
		t.Fatalf("/graphql series = %v", got)
	}
	if got := counterValue(t, httpRequests.WithLabelValues("GET", "unmatched", "404")); got < 2 {
		t.Fatalf("unmatched series = %v, want >= 2", got)
	}
	if got := counterValue(t, httpRequests.WithLabelValues("other", "/graphql", "200")); got < 1 {
		t.Fatalf("unknown method not folded to other: %v", got)
	}
	// 5 distinct label sets at most: relays pattern, /graphql GET, unmatched, other.
	if added := seriesCount(httpRequests) - before; added > 4 {
		t.Fatalf("%d new series from 6 requests: raw paths leaked into labels", added)
	}
}
