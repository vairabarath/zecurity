package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type mockChecker struct {
	name        string
	critical    bool
	status      ComponentStatus
	err         error
	invocations atomic.Int64
}

func (m *mockChecker) Name() string     { return m.name }
func (m *mockChecker) IsCritical() bool { return m.critical }
func (m *mockChecker) Check(ctx context.Context) (ComponentStatus, error) {
	m.invocations.Add(1)
	return m.status, m.err
}

type mockCertProvider struct {
	serial    string
	notBefore time.Time
	notAfter  time.Time
	ok        bool
}

func (m *mockCertProvider) CurrentCertInfo() (string, time.Time, time.Time, bool) {
	return m.serial, m.notBefore, m.notAfter, m.ok
}

type mockOutboxProvider struct {
	running bool
}

func (m *mockOutboxProvider) IsRunning() bool {
	return m.running
}

func TestLivenessHandler_IndependentAndMinimal(t *testing.T) {
	handler := LivenessHandler()

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if body != `{"status":"ok"}` {
		t.Fatalf("expected minimal status 'ok', got %q", body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
}

func TestPublicReadiness_MinimalAndProtected(t *testing.T) {
	reg := NewRegistry(time.Second, time.Second)

	dbMock := &mockChecker{
		name:     "database",
		critical: true,
		status:   ComponentStatus{Status: "up", LatencyMS: 1.5},
	}
	reg.Register(dbMock)

	// Test ready state
	handler := reg.PublicReadinessHandler()
	req := httptest.NewRequest(http.MethodGet, "/readyz?verbose=true", nil) // query param must be ignored
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rr.Code)
	}
	if rr.Body.String() != `{"status":"ready"}` {
		t.Fatalf("expected minimal 'ready' without details, got %q", rr.Body.String())
	}

	// Make database fail
	dbMock.status = ComponentStatus{Status: "down", Error: "connection refused"}
	dbMock.err = errors.New("connection refused")
	reg.lastReport = nil // clear cache

	rrFail := httptest.NewRecorder()
	handler.ServeHTTP(rrFail, req)

	if rrFail.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", rrFail.Code)
	}
	if rrFail.Body.String() != `{"status":"not_ready"}` {
		t.Fatalf("expected minimal 'not_ready' without details, got %q", rrFail.Body.String())
	}
}

func TestPublicHealthzAndHealth_Compatibility(t *testing.T) {
	reg := NewRegistry(time.Second, time.Second)
	dbMock := &mockChecker{
		name:     "database",
		critical: true,
		status:   ComponentStatus{Status: "up"},
	}
	reg.Register(dbMock)

	// Healthz
	hzHandler := reg.PublicHealthzHandler()
	rrHz := httptest.NewRecorder()
	hzHandler.ServeHTTP(rrHz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rrHz.Code != http.StatusOK || rrHz.Body.String() != `{"status":"ok"}` {
		t.Fatalf("healthz: expected 200 'ok', got %d %q", rrHz.Code, rrHz.Body.String())
	}

	// Legacy Health
	hHandler := reg.PublicHealthHandler()
	rrH := httptest.NewRecorder()
	hHandler.ServeHTTP(rrH, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rrH.Code != http.StatusOK || rrH.Body.String() != `{"status":"ok"}` {
		t.Fatalf("health: expected 200 'ok', got %d %q", rrH.Code, rrH.Body.String())
	}
}

func TestInternalDiagnosticsHandler_DetailedDiagnostics(t *testing.T) {
	reg := NewRegistry(time.Second, time.Second)

	dbMock := &mockChecker{
		name:     "database",
		critical: true,
		status: ComponentStatus{
			Status:    "up",
			LatencyMS: 0.8,
			Details:   map[string]any{"idle_conns": 5},
		},
	}
	reg.Register(dbMock)

	handler := reg.InternalDiagnosticsHandler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rr.Code)
	}

	var report DiagnosticReport
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("failed to decode diagnostics JSON: %v", err)
	}

	if report.Status != "ready" {
		t.Fatalf("expected report status ready, got %s", report.Status)
	}
	dbComp, exists := report.Components["database"]
	if !exists || dbComp.Status != "up" || dbComp.LatencyMS != 0.8 {
		t.Fatalf("expected database component details, got %+v", dbComp)
	}
}

func TestRegistry_CachingPreventsProbeStorms(t *testing.T) {
	reg := NewRegistry(time.Second, 100*time.Millisecond)

	mock := &mockChecker{
		name:     "cached_comp",
		critical: true,
		status:   ComponentStatus{Status: "up"},
	}
	reg.Register(mock)

	// First call executes check
	_ = reg.Evaluate(context.Background())
	if mock.invocations.Load() != 1 {
		t.Fatalf("expected 1 invocation, got %d", mock.invocations.Load())
	}

	// Second rapid call should be cached
	_ = reg.Evaluate(context.Background())
	if mock.invocations.Load() != 1 {
		t.Fatalf("expected cached call not to invoke checker, got %d", mock.invocations.Load())
	}

	// Sleep past TTL
	time.Sleep(120 * time.Millisecond)
	_ = reg.Evaluate(context.Background())
	if mock.invocations.Load() != 2 {
		t.Fatalf("expected second invocation after TTL, got %d", mock.invocations.Load())
	}
}

func TestPKIChecker(t *testing.T) {
	now := time.Now().UTC()

	// 1. Valid certificate
	provider := &mockCertProvider{
		serial:    "1234abcd",
		notBefore: now.Add(-1 * time.Hour),
		notAfter:  now.Add(24 * time.Hour),
		ok:        true,
	}
	chk := NewPKIChecker(provider)
	st, err := chk.Check(context.Background())
	if err != nil || st.Status != "up" {
		t.Fatalf("expected up, got status=%s err=%v", st.Status, err)
	}
	if val, ok := st.Details["expires_in_seconds"].(int64); !ok || val <= 0 {
		t.Fatalf("expected positive expires_in_seconds, got %v", val)
	}

	// 2. Expired certificate
	provider.notAfter = now.Add(-1 * time.Minute)
	stExpired, errExpired := chk.Check(context.Background())
	if errExpired == nil || stExpired.Status != "down" {
		t.Fatalf("expected down for expired cert, got status=%s err=%v", stExpired.Status, errExpired)
	}

	// 3. Not yet valid
	provider.notBefore = now.Add(1 * time.Hour)
	provider.notAfter = now.Add(25 * time.Hour)
	stFuture, errFuture := chk.Check(context.Background())
	if errFuture == nil || stFuture.Status != "down" {
		t.Fatalf("expected down for future cert, got status=%s err=%v", stFuture.Status, errFuture)
	}

	// 4. Missing/not configured
	provider.ok = false
	stMissing, errMissing := chk.Check(context.Background())
	if errMissing == nil || stMissing.Status != "down" {
		t.Fatalf("expected down for missing cert, got status=%s err=%v", stMissing.Status, errMissing)
	}
}

func TestOutboxChecker(t *testing.T) {
	provider := &mockOutboxProvider{running: true}
	chk := NewOutboxChecker(provider)

	st, err := chk.Check(context.Background())
	if err != nil || st.Status != "up" {
		t.Fatalf("expected up, got %s %v", st.Status, err)
	}

	// Stopped outbox
	provider.running = false
	stStop, errStop := chk.Check(context.Background())
	if errStop == nil || stStop.Status != "down" {
		t.Fatalf("expected down when stopped, got %s %v", stStop.Status, errStop)
	}
}

// /health keeps its pre-PENDING-10 contract (a database ping): a Valkey outage
// makes /readyz and /healthz 503 but must not take /health down.
func TestPublicHealth_DatabaseOnly(t *testing.T) {
	reg := NewRegistry(time.Second, time.Nanosecond)
	db := &mockChecker{name: "database", critical: true, status: ComponentStatus{Status: "up"}}
	vk := &mockChecker{name: "valkey", critical: true, status: ComponentStatus{Status: "down"}}
	reg.Register(db)
	reg.Register(vk)

	get := func(h http.Handler, path string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr.Code
	}
	if c := get(reg.PublicHealthHandler(), "/health"); c != http.StatusOK {
		t.Fatalf("/health with valkey down = %d, want 200", c)
	}
	if c := get(reg.PublicReadinessHandler(), "/readyz"); c != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with valkey down = %d, want 503", c)
	}
	db.status = ComponentStatus{Status: "down"}
	if c := get(reg.PublicHealthHandler(), "/health"); c != http.StatusServiceUnavailable {
		t.Fatalf("/health with database down = %d, want 503", c)
	}
}

type ctxChecker struct{}

func (ctxChecker) Name() string     { return "database" }
func (ctxChecker) IsCritical() bool { return true }
func (ctxChecker) Check(ctx context.Context) (ComponentStatus, error) {
	if err := ctx.Err(); err != nil {
		return ComponentStatus{}, err
	}
	return ComponentStatus{Status: "up"}, nil
}

// A caller that aborts its probe must not cache "not_ready" for everyone else.
func TestEvaluate_CallerCancellationNotCached(t *testing.T) {
	reg := NewRegistry(time.Second, time.Minute)
	reg.Register(ctxChecker{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := reg.Evaluate(ctx).Status; got != "ready" {
		t.Fatalf("checks inherited the caller's cancellation: %s", got)
	}
	if got := reg.Evaluate(context.Background()).Status; got != "ready" {
		t.Fatalf("cached status = %s, want ready", got)
	}
}

// Critical labels each registered check by name (provider health read).
func TestRegistry_Critical(t *testing.T) {
	r := NewRegistry(time.Second, time.Second)
	r.Register(NewDatabaseChecker(nil))
	r.Register(NewOutboxChecker(nil))
	got := r.Critical()
	if len(got) != 2 || !got["database"] || got["outbox"] {
		t.Fatalf("Critical() = %v, want database critical, outbox not", got)
	}
}
