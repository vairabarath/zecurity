package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape returns the body of a /metrics request against the package handler.
func scrape(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestHandlerExposesReconcileFamilies(t *testing.T) {
	body := scrape(t)
	for _, name := range []string{
		"reconcile_reports_total",
		"reconcile_drift_detected_total",
		"reconcile_resyncs_total",
		"reconcile_tombstones_reaped_total",
		"reconcile_shields_drifting",
		"reconcile_tombstones_pending",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics output missing family %q", name)
		}
	}
}

func TestCountersAndGaugesUpdate(t *testing.T) {
	ReconcileReport()
	DriftDetected("orphan")
	DriftDetected("missing")
	Resync()
	TombstoneReaped()
	SetReconcileGauges(2, 5)

	body := scrape(t)

	// Counters carry their label set; gauges reflect the last Set.
	wantSubstrings := []string{
		`reconcile_drift_detected_total{kind="orphan"} 1`,
		`reconcile_drift_detected_total{kind="missing"} 1`,
		"reconcile_shields_drifting 2",
		"reconcile_tombstones_pending 5",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}
