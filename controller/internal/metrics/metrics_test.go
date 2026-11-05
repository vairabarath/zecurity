package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func TestMetrics_CardinalityInvariantRule(t *testing.T) {
	// CARDINALITY RULE: labels must never contain unbounded high-cardinality entities.
	forbiddenLabels := map[string]bool{
		"tenant_id":    true,
		"workspace_id": true,
		"resource_id":  true,
		"user_id":      true,
		"client_id":    true,
		"device_id":    true,
		"ip":           true,
		"ip_address":   true,
		"serial":       true,
		"cert_serial":  true,
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics from private registry: %v", err)
	}

	for _, mf := range metricFamilies {
		name := mf.GetName()
		for _, m := range mf.GetMetric() {
			for _, label := range m.GetLabel() {
				lname := strings.ToLower(label.GetName())
				if forbiddenLabels[lname] {
					t.Fatalf("HIGH-CARDINALITY VIOLATION: metric %q carries forbidden label %q", name, lname)
				}
			}
		}
	}
}

func TestACLCompilerMetrics(t *testing.T) {
	RecordACLCompile(15*time.Millisecond, nil)
	RecordACLCompile(25*time.Millisecond, errors.New("compiler error"))
	RecordPolicyNotification("policy")
	RecordPolicyNotification("transport")

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	foundCompile := false
	foundNotification := false
	for _, mf := range mfs {
		if mf.GetName() == "acl_compiles_total" {
			foundCompile = true
			if len(mf.GetMetric()) < 2 {
				t.Fatalf("expected at least 2 label series for acl_compiles_total, got %d", len(mf.GetMetric()))
			}
		}
		if mf.GetName() == "policy_notifications_total" {
			foundNotification = true
		}
	}
	if !foundCompile || !foundNotification {
		t.Fatalf("missing ACL compile or policy notification metrics in gather")
	}
}

func TestRelayFleetMetrics(t *testing.T) {
	SetRelayFleetGauges(5, 750, 1000)
	RecordRelayHeartbeat("accepted")
	RecordRelayCapacityTransition("normal_to_warning")

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var utilizationValue float64
	for _, mf := range mfs {
		if mf.GetName() == "relay_capacity_utilization_ratio" {
			utilizationValue = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}

	if utilizationValue != 0.75 {
		t.Fatalf("expected utilization 0.75, got %v", utilizationValue)
	}
}

func TestConnectorFleetMetrics(t *testing.T) {
	SetConnectorActiveStreams(12)
	SetConnectorActiveCount(15)
	RecordConnectorHeartbeat("ok")
	RecordConnectorACLLag(10, 8) // lag = 2

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var streamCount float64
	for _, mf := range mfs {
		if mf.GetName() == "connector_active_streams" {
			streamCount = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}

	if streamCount != 12 {
		t.Fatalf("expected 12 active streams, got %v", streamCount)
	}
}

func TestPKIMetrics(t *testing.T) {
	now := time.Now().UTC()
	// Set valid cert with 7 days runway
	SetCertRunway("controller_grpc", now.Add(-1*time.Hour), now.Add(7*24*time.Hour), true)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var foundValid, foundRunway bool
	for _, mf := range mfs {
		if mf.GetName() == "cert_valid" {
			for _, m := range mf.GetMetric() {
				if hasLabel(m, "entity", "controller_grpc") {
					foundValid = true
					if m.GetGauge().GetValue() != 1.0 {
						t.Fatalf("expected cert_valid = 1.0, got %v", m.GetGauge().GetValue())
					}
				}
			}
		}
		if mf.GetName() == "cert_runway_seconds" {
			for _, m := range mf.GetMetric() {
				if hasLabel(m, "entity", "controller_grpc") {
					foundRunway = true
					if m.GetGauge().GetValue() <= 0 {
						t.Fatalf("expected positive runway, got %v", m.GetGauge().GetValue())
					}
				}
			}
		}
	}

	if !foundValid || !foundRunway {
		t.Fatalf("controller_grpc metrics missing from certValid / certRunway")
	}
}

func TestOutboxMetrics(t *testing.T) {
	RecordOutboxEventPublished("device.trust.revoke.requested")
	RecordOutboxEventProcessed("success")
	RecordOutboxDeadLetter()
	SetOutboxQueueGauges(42, 5*time.Minute)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var pending float64
	for _, mf := range mfs {
		if mf.GetName() == "outbox_events_pending" {
			pending = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}

	if pending != 42 {
		t.Fatalf("expected 42 pending events, got %v", pending)
	}
}

func TestDBPoolMetrics(t *testing.T) {
	SetDBPoolStats(5, 10, 15, 20)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var foundAcquired bool
	for _, mf := range mfs {
		if mf.GetName() == "db_pool_connections" {
			for _, m := range mf.GetMetric() {
				if hasLabel(m, "state", "acquired") {
					foundAcquired = true
					if m.GetGauge().GetValue() != 5 {
						t.Fatalf("expected 5 acquired conns, got %v", m.GetGauge().GetValue())
					}
				}
			}
		}
	}
	if !foundAcquired {
		t.Fatalf("state=acquired missing from db_pool_connections")
	}
}

func TestHandler_ExpositionFormat(t *testing.T) {
	handler := Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "acl_compiles_total") {
		t.Fatalf("expected acl_compiles_total in exposition output")
	}
	if !strings.Contains(body, "relay_capacity_max") {
		t.Fatalf("expected relay_capacity_max in exposition output")
	}
}

func hasLabel(m *dto.Metric, name, value string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}
