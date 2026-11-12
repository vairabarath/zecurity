package provider

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/yourorg/ztna/controller/internal/health"
)

// Control-plane health for the provider console (quick-win F25):
// GET /provider/health, health.read (super-admin only), not audited.
//
// It reuses the controller's health registry (PENDING-10) and reshapes its
// report for an operator. The registry's own report is an internal
// diagnostic: a database or Valkey check failure carries the driver's error
// text (hosts, users, addresses) and components carry details such as pool
// statistics. None of that leaves here. A component reports only:
//   - name, critical flag, latency;
//   - status: healthy (up), degraded (down, not critical) or unhealthy
//     (down, critical);
//   - message: one of the fixed check messages below, or "check failed".
//
// A failing check is a component status, never an HTTP error: the response
// is 200 with every component the registry evaluated.

// HealthSource is what the health read needs from health.Registry.
type HealthSource interface {
	Evaluate(ctx context.Context) *health.DiagnosticReport
	Critical() map[string]bool
}

// Health statuses.
const (
	HealthHealthy   = "healthy"
	HealthDegraded  = "degraded"
	HealthUnhealthy = "unhealthy"
)

// healthMessages are the fixed messages the built-in checks set themselves.
// They name a state, not infrastructure, so they may be shown. Any other
// text (a driver error) is replaced by healthCheckFailed.
var healthMessages = map[string]bool{
	"database pool not initialized":    true,
	"valkey client not initialized":    true,
	"pki provider not initialized":     true,
	"no active controller certificate": true,
	"certificate not yet valid":        true,
	"certificate expired":              true,
	"outbox processor stopped":         true,
}

const healthCheckFailed = "check failed"

// HealthComponent is one component in GET /provider/health.
type HealthComponent struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	Critical  bool    `json:"critical"`
	LatencyMS float64 `json:"latency_ms"`
	Message   *string `json:"message"`
}

// HealthResult is GET /provider/health.
type HealthResult struct {
	Status     string            `json:"status"`
	CheckedAt  time.Time         `json:"checked_at"`
	Components []HealthComponent `json:"components"`
}

// SanitizeHealth reshapes a registry report for the provider console.
// critical labels each check; a component missing from it is treated as
// critical (the conservative reading).
func SanitizeHealth(report *health.DiagnosticReport, critical map[string]bool) HealthResult {
	res := HealthResult{Status: HealthHealthy, Components: []HealthComponent{}}
	if report == nil {
		res.Status = HealthUnhealthy
		return res
	}
	res.CheckedAt = report.Timestamp.UTC()
	for name, c := range report.Components {
		isCritical, known := critical[name]
		if !known {
			isCritical = true
		}
		comp := HealthComponent{Name: name, Critical: isCritical, LatencyMS: c.LatencyMS, Status: HealthHealthy}
		if c.Status != "up" {
			msg := healthCheckFailed
			if healthMessages[c.Error] {
				msg = c.Error
			}
			comp.Message = &msg
			if isCritical {
				comp.Status = HealthUnhealthy
			} else {
				comp.Status = HealthDegraded
			}
		}
		switch {
		case comp.Status == HealthUnhealthy:
			res.Status = HealthUnhealthy
		case comp.Status == HealthDegraded && res.Status == HealthHealthy:
			res.Status = HealthDegraded
		}
		res.Components = append(res.Components, comp)
	}
	sort.Slice(res.Components, func(i, j int) bool { return res.Components[i].Name < res.Components[j].Name })
	return res
}

// SetHealth attaches the controller's health registry (built after the read
// handlers in main). Without one, GET /provider/health answers 503.
func (h *ReadHandlers) SetHealth(src HealthSource) { h.health = src }

// Health: GET /provider/health (not audited).
func (h *ReadHandlers) Health(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadHealth(actor)) {
		return
	}
	if h.health == nil {
		writeReadError(w, http.StatusServiceUnavailable, "health_unavailable")
		return
	}
	writeReadJSON(w, SanitizeHealth(h.health.Evaluate(r.Context()), h.health.Critical()))
}
