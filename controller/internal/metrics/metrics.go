// Package metrics owns the controller's Prometheus collectors and the HTTP
// handler that exposes them.
//
// It uses a PRIVATE registry (not prometheus.DefaultRegisterer) so the controller
// publishes exactly what it intends, and exposes typed increment/set helpers so
// call sites never import prometheus directly. Per ADR-004 Phase 4.3 the metrics
// here observe the closed-loop reconciler (internal/connector/reconcile.go).
//
// CARDINALITY RULE: labels must stay low-cardinality. Never label by shield_id,
// tenant_id, or resource_id — those are unbounded and would explode the series
// count. Per-entity detail belongs in the logs, not in metric labels.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var registry = prometheus.NewRegistry()

var (
	reconcileReports = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "reconcile_reports_total",
		Help: "Total shield actual-state reports processed by the reconciler.",
	})

	reconcileDriftDetected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reconcile_drift_detected_total",
		Help: "Resources observed in drift. kind=orphan (a TRUE zombie: enforced, never desired, and not a 'deleting' tombstone mid-removal); kind=missing (desired but not enforced).",
	}, []string{"kind"})

	reconcileResyncs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "reconcile_resyncs_total",
		Help: "Corrective desired-state snapshot re-pushes triggered after drift persisted past the hysteresis threshold.",
	})

	reconcileTombstonesReaped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "reconcile_tombstones_reaped_total",
		Help: "Resource tombstones reaped after the shield's reports confirmed the rule is gone.",
	})

	reconcileShieldsDrifting = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "reconcile_shields_drifting",
		Help: "Shields currently observed in drift (non-zero hysteresis counter).",
	})

	reconcileTombstonesPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "reconcile_tombstones_pending",
		Help: "Tombstones the reconciler is actively tracking toward reap, i.e. seen in a live report from a REPORTING shield. NOTE: a fully-disconnected shield sends no reports, so its stuck tombstone is NOT counted here — detect that break-glass (forceDeleteResource) case via a row stuck in 'deleting' plus shield status 'disconnected', not this gauge.",
	})
)

func init() {
	registry.MustRegister(
		reconcileReports,
		reconcileDriftDetected,
		reconcileResyncs,
		reconcileTombstonesReaped,
		reconcileShieldsDrifting,
		reconcileTombstonesPending,
		// Baseline process/runtime visibility alongside the domain metrics.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Pre-create the known drift label series at 0 so they are exposed from
	// startup (a CounterVec emits nothing until a label set is observed) —
	// dashboards and rate() queries then have no gaps before the first drift.
	reconcileDriftDetected.WithLabelValues("orphan")
	reconcileDriftDetected.WithLabelValues("missing")
}

// ── Reconciler instrumentation (called from internal/connector/reconcile.go) ──

// ReconcileReport records that one shield actual-state report was processed.
func ReconcileReport() { reconcileReports.Inc() }

// DriftDetected records one drifting resource. kind must be "orphan" or "missing".
func DriftDetected(kind string) { reconcileDriftDetected.WithLabelValues(kind).Inc() }

// Resync records one corrective desired-state snapshot re-push.
func Resync() { reconcileResyncs.Inc() }

// TombstoneReaped records one confirmed-absent tombstone being reaped.
func TombstoneReaped() { reconcileTombstonesReaped.Inc() }

// SetReconcileGauges sets the current-state gauges: how many shields are drifting
// right now, and how many tombstones are awaiting reap confirmation.
func SetReconcileGauges(driftingShields, pendingTombstones int) {
	reconcileShieldsDrifting.Set(float64(driftingShields))
	reconcileTombstonesPending.Set(float64(pendingTombstones))
}

// ── Exposition ───────────────────────────────────────────────────────────────

// Handler serves the controller's metrics in the Prometheus text exposition
// format. Wire it onto an INTERNAL listener — the metrics leak operational data
// and must not sit on the public mux.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
