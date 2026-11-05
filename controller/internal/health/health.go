// Package health provides liveness, readiness, and internal diagnostic health checks
// for the Zecurity controller.
//
// Security & Architecture Invariants:
//  1. Liveness (/livez) is independent of database or external dependencies (returns 200 OK
//     as long as the process is running and its HTTP listener is responsive).
//  2. Public readiness endpoints (/readyz, /healthz, /health on :8080) return MINIMAL status
//     only ({"status":"ready"} or {"status":"not_ready"}) with appropriate HTTP 200/503 codes.
//     Detailed component statuses, latencies, and certificate metrics are strictly restricted
//     to internal management interfaces (:9102/healthz) and never exposed via unauthenticated query parameters.
//  3. Readiness checks use bounded context timeouts and short-lived caching to prevent
//     probe storms from saturating downstream dependencies.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"
)

// Default check parameters.
const (
	DefaultCheckTimeout = 2 * time.Second
	DefaultCacheTTL     = 1 * time.Second
)

// ComponentStatus describes the health of a single sub-system.
type ComponentStatus struct {
	Status    string         `json:"status"`               // "up" or "down"
	LatencyMS float64        `json:"latency_ms,omitempty"` // Probe latency in milliseconds
	Error     string         `json:"error,omitempty"`      // Error message if status is down
	Details   map[string]any `json:"details,omitempty"`    // Diagnostic details (internal only)
}

// DiagnosticReport is the detailed health payload returned to internal management endpoints.
type DiagnosticReport struct {
	Status     string                     `json:"status"` // "ready" or "not_ready"
	Timestamp  time.Time                  `json:"timestamp"`
	DurationMS float64                    `json:"duration_ms"`
	Components map[string]ComponentStatus `json:"components"`
}

// Checker is a named health check function.
type Checker interface {
	Name() string
	Check(ctx context.Context) (ComponentStatus, error)
	IsCritical() bool
}

// Registry coordinates health checkers, caching, and HTTP handlers.
type Registry struct {
	mu           sync.RWMutex
	checkers     []Checker
	checkTimeout time.Duration
	cacheTTL     time.Duration
	lastReport   *DiagnosticReport
	lastChecked  time.Time
}

// NewRegistry creates a health registry with the given check timeout and cache TTL.
func NewRegistry(timeout, cacheTTL time.Duration) *Registry {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	if cacheTTL <= 0 {
		cacheTTL = DefaultCacheTTL
	}
	return &Registry{
		checkTimeout: timeout,
		cacheTTL:     cacheTTL,
	}
}

// Register adds a checker to the registry.
func (r *Registry) Register(c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers = append(r.checkers, c)
	// Invalidate cache when new checkers are added
	r.lastReport = nil
}

// Evaluate runs all checkers or returns a fresh cached report.
func (r *Registry) Evaluate(ctx context.Context) *DiagnosticReport {
	r.mu.RLock()
	if r.lastReport != nil && time.Since(r.lastChecked) < r.cacheTTL {
		cached := r.lastReport
		r.mu.RUnlock()
		return cached
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double check under write lock
	if r.lastReport != nil && time.Since(r.lastChecked) < r.cacheTTL {
		return r.lastReport
	}

	start := time.Now()
	report := &DiagnosticReport{
		Status:     "ready",
		Timestamp:  start.UTC(),
		Components: make(map[string]ComponentStatus, len(r.checkers)),
	}

	checkCtx, cancel := context.WithTimeout(ctx, r.checkTimeout)
	defer cancel()

	type checkResult struct {
		name     string
		status   ComponentStatus
		critical bool
	}

	results := make(chan checkResult, len(r.checkers))
	for _, c := range r.checkers {
		go func(chk Checker) {
			status, err := chk.Check(checkCtx)
			if err != nil && status.Status == "" {
				status.Status = "down"
				status.Error = err.Error()
			}
			results <- checkResult{
				name:     chk.Name(),
				status:   status,
				critical: chk.IsCritical(),
			}
		}(c)
	}

	for i := 0; i < len(r.checkers); i++ {
		res := <-results
		report.Components[res.name] = res.status
		if res.critical && res.status.Status != "up" {
			report.Status = "not_ready"
		}
	}

	report.DurationMS = float64(time.Since(start).Microseconds()) / 1000.0
	r.lastReport = report
	r.lastChecked = time.Now()

	return report
}

// ── Built-in Checkers ────────────────────────────────────────────────────────

// DatabaseChecker verifies connectivity to PostgreSQL via pgxpool.
type DatabaseChecker struct {
	pool *pgxpool.Pool
}

func NewDatabaseChecker(pool *pgxpool.Pool) *DatabaseChecker {
	return &DatabaseChecker{pool: pool}
}

func (c *DatabaseChecker) Name() string     { return "database" }
func (c *DatabaseChecker) IsCritical() bool { return true }
func (c *DatabaseChecker) Check(ctx context.Context) (ComponentStatus, error) {
	if c.pool == nil {
		return ComponentStatus{Status: "down", Error: "database pool not initialized"}, errors.New("nil database pool")
	}

	start := time.Now()
	err := c.pool.Ping(ctx)
	latency := float64(time.Since(start).Microseconds()) / 1000.0

	stat := c.pool.Stat()
	details := map[string]any{
		"acquired_conns": stat.AcquiredConns(),
		"idle_conns":     stat.IdleConns(),
		"total_conns":    stat.TotalConns(),
		"max_conns":      stat.MaxConns(),
	}

	if err != nil {
		return ComponentStatus{
			Status:    "down",
			LatencyMS: latency,
			Error:     err.Error(),
			Details:   details,
		}, err
	}

	return ComponentStatus{
		Status:    "up",
		LatencyMS: latency,
		Details:   details,
	}, nil
}

// ValkeyChecker verifies connectivity to the Valkey cache.
type ValkeyChecker struct {
	client valkey.Client
}

func NewValkeyChecker(client valkey.Client) *ValkeyChecker {
	return &ValkeyChecker{client: client}
}

func (c *ValkeyChecker) Name() string     { return "valkey" }
func (c *ValkeyChecker) IsCritical() bool { return true }
func (c *ValkeyChecker) Check(ctx context.Context) (ComponentStatus, error) {
	if c.client == nil {
		return ComponentStatus{Status: "down", Error: "valkey client not initialized"}, errors.New("nil valkey client")
	}

	start := time.Now()
	err := c.client.Do(ctx, c.client.B().Ping().Build()).Error()
	latency := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		return ComponentStatus{
			Status:    "down",
			LatencyMS: latency,
			Error:     err.Error(),
		}, err
	}

	return ComponentStatus{
		Status:    "up",
		LatencyMS: latency,
	}, nil
}

// CertInfoProvider provides current certificate metadata.
type CertInfoProvider interface {
	CurrentCertInfo() (serial string, notBefore, notAfter time.Time, ok bool)
}

// PKIChecker verifies that the controller gRPC server has an active, unexpired certificate.
type PKIChecker struct {
	provider CertInfoProvider
}

func NewPKIChecker(provider CertInfoProvider) *PKIChecker {
	return &PKIChecker{provider: provider}
}

func (c *PKIChecker) Name() string     { return "pki" }
func (c *PKIChecker) IsCritical() bool { return true }
func (c *PKIChecker) Check(ctx context.Context) (ComponentStatus, error) {
	if c.provider == nil {
		return ComponentStatus{Status: "down", Error: "pki provider not initialized"}, errors.New("nil pki provider")
	}

	_, notBefore, notAfter, ok := c.provider.CurrentCertInfo()
	if !ok {
		return ComponentStatus{Status: "down", Error: "no active controller certificate"}, errors.New("no active certificate")
	}

	now := time.Now().UTC()
	if now.Before(notBefore) {
		return ComponentStatus{Status: "down", Error: "certificate not yet valid"}, errors.New("certificate not yet valid")
	}
	if now.After(notAfter) {
		return ComponentStatus{Status: "down", Error: "certificate expired"}, errors.New("certificate expired")
	}

	runwaySeconds := int64(notAfter.Sub(now).Seconds())
	return ComponentStatus{
		Status: "up",
		Details: map[string]any{
			"cert_valid":         true,
			"expires_in_seconds": runwaySeconds,
			"not_after":          notAfter.Format(time.RFC3339),
		},
	}, nil
}

// OutboxStatusProvider provides outbox execution state.
type OutboxStatusProvider interface {
	IsRunning() bool
}

// OutboxChecker verifies that the asynchronous outbox processor is running.
type OutboxChecker struct {
	provider OutboxStatusProvider
}

func NewOutboxChecker(provider OutboxStatusProvider) *OutboxChecker {
	return &OutboxChecker{provider: provider}
}

func (c *OutboxChecker) Name() string     { return "outbox" }
func (c *OutboxChecker) IsCritical() bool { return false } // Non-blocking for readiness if recovering
func (c *OutboxChecker) Check(ctx context.Context) (ComponentStatus, error) {
	if c.provider == nil {
		return ComponentStatus{Status: "up", Details: map[string]any{"configured": false}}, nil
	}

	running := c.provider.IsRunning()
	if !running {
		return ComponentStatus{
			Status:  "down",
			Error:   "outbox processor stopped",
			Details: map[string]any{"running": false},
		}, errors.New("outbox processor not running")
	}

	return ComponentStatus{
		Status:  "up",
		Details: map[string]any{"running": true},
	}, nil
}

// ── HTTP Handlers ────────────────────────────────────────────────────────────

// LivenessHandler provides the /livez endpoint.
// Invariant: independent of database or external dependencies.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// PublicReadinessHandler serves /readyz on the public listener (:8080).
// Invariant: returns minimal status {"status":"ready"} or {"status":"not_ready"}.
func (r *Registry) PublicReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report := r.Evaluate(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if report.Status == "ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready"}`))
		}
	})
}

// PublicHealthzHandler serves /healthz on the public listener (:8080).
// Invariant: returns minimal status {"status":"ok"} or {"status":"unhealthy"}.
func (r *Registry) PublicHealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report := r.Evaluate(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if report.Status == "ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
		}
	})
}

// PublicHealthHandler preserves the legacy /health contract: it reflects the
// database only (it used to be a bare db.Pool.Ping), so a Valkey, PKI or outbox
// problem doesn't take a controller out of an external load balancer that
// still probes /health. Use /readyz for full readiness.
func (r *Registry) PublicHealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report := r.Evaluate(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if report.Components["database"].Status == "up" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy"}`))
		}
	})
}

// InternalDiagnosticsHandler serves full component diagnostics on the internal
// management listener (:9102/healthz).
func (r *Registry) InternalDiagnosticsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report := r.Evaluate(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if report.Status == "ready" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
	})
}
