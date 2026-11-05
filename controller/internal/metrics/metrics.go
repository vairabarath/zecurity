// Package metrics owns the controller's Prometheus collectors and the HTTP
// handler that exposes them.
//
// It uses a PRIVATE registry (not prometheus.DefaultRegisterer) so the controller
// publishes exactly what it intends, and exposes typed increment/set helpers so
// call sites never import prometheus directly. Per ADR-004 Phase 4.3 the metrics
// here observe the closed-loop reconciler (internal/connector/reconcile.go),
// relay fleet, connector health, ACL compiler, PKI, and outbox pipeline.
//
// CARDINALITY RULE: labels must stay low-cardinality.
// NEVER introduce:
//   - tenant_id / workspace_id
//   - resource_id
//   - user_id / client_id / device_id
//   - IP addresses / hostnames
//   - Certificate serial numbers
//   - Arbitrary / unbounded error messages
//
// Only bounded categorical labels are permitted. Per-entity detail belongs in logs.
package metrics

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var registry = prometheus.NewRegistry()

var (
	// ── Reconciler Metrics ──────────────────────────────────────────────────
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
		Help: "Tombstones the reconciler is actively tracking toward reap, i.e. seen in a live report from a REPORTING shield.",
	})

	discoveryRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "discovery_requests_total",
		Help: "Public workspace-discovery requests (FU-11) by operation and outcome. Never labelled by IP or email.",
	}, []string{"operation", "result"})

	geoipDatabaseBuild = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "geoip_database_build_timestamp_seconds",
		Help: "Build time (Unix seconds) of the loaded GeoIP country database (PENDING-20); 0 when none is loaded.",
	})

	// ── ACL Compiler & Policy Metrics (PENDING-10 Phase 2) ──────────────────
	aclCompileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "acl_compile_duration_seconds",
		Help:    "Latency of ACL snapshot compilations.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5},
	}, []string{"status"}) // status: "ok", "error"

	aclCompiles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "acl_compiles_total",
		Help: "Total ACL snapshot compilation attempts by status.",
	}, []string{"status"}) // status: "ok", "error"

	policyNotifications = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "policy_notifications_total",
		Help: "Total policy and transport plane change notifications emitted.",
	}, []string{"plane"}) // plane: "policy", "transport"

	// ── Relay Fleet Metrics (PENDING-10 Phase 2) ────────────────────────────
	relayHeartbeats = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_heartbeats_total",
		Help: "Total relay heartbeats received by outcome.",
	}, []string{"status"}) // status: "accepted", "rejected", "revoked"

	relayActiveCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relay_active_count",
		Help: "Relays with status active in the database (heartbeat freshness is tracked in Valkey, not here).",
	})

	relayConnectionsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relay_connections_total",
		Help: "Total active client tunnel connections across all registered relays.",
	})

	relayCapacityMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relay_capacity_max",
		Help: "Total maximum connection capacity across all registered relays.",
	})

	relayCapacityUtilization = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relay_capacity_utilization_ratio",
		Help: "Aggregate relay fleet utilization ratio (connections / max capacity).",
	})

	relayCapacityTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_capacity_transitions_total",
		Help: "Total capacity label transitions observed on relays.",
	}, []string{"transition"})

	// ── Connector Fleet Metrics (PENDING-10 Phase 2) ────────────────────────
	connectorHeartbeats = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "connector_heartbeats_total",
		Help: "Total connector health reports received by outcome.",
	}, []string{"status"}) // status: "ok", "rejected", "revoked"

	connectorActiveStreams = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "connector_active_streams",
		Help: "Number of actively open connector gRPC control streams.",
	})

	connectorActiveCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "connector_active_count",
		Help: "Connectors in active workspaces with status active, not revoked, and a heartbeat within the disconnect threshold.",
	})

	connectorACLLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "connector_acl_lag_versions",
		Help:    "Difference between controller expected ACL version and connector reported ACL version.",
		Buckets: []float64{0, 1, 2, 5, 10, 25, 50},
	})

	// ── PKI & Certificate Runway (PENDING-10 Phase 2) ───────────────────────
	// cert_runway_seconds, cert_valid and cert_lifetime_seconds are computed at
	// scrape time by certs (below) so the runway keeps falling between rotations.
	pkiRotations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pki_rotations_total",
		Help: "Controller certificate rotation attempts by entity and outcome.",
	}, []string{"entity", "status"}) // entity: "controller_grpc"; status: "ok", "error"

	// ── Outbox Pipeline Metrics (PENDING-10 Phase 2) ────────────────────────
	outboxEventsPublished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_events_published_total",
		Help: "Total events written to the durable outbox.",
	}, []string{"event_type"})

	outboxEventsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_events_processed_total",
		Help: "Total outbox events dispatched by outcome.",
	}, []string{"status"}) // status: "success", "retryable_failure", "dead_letter"

	outboxEventsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_events_pending",
		Help: "Current count of pending outbox events awaiting processing.",
	})

	outboxDeadLetterTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_dead_letter_total",
		Help: "Total outbox events that exceeded maximum retries and were abandoned or failed terminally.",
	})

	outboxOldestPendingAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_oldest_pending_age_seconds",
		Help: "Age in seconds of the oldest pending uncompleted outbox event.",
	})

	// ── Database Connection Pool (PENDING-10 Phase 2) ───────────────────────
	dbPoolConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_pool_connections",
		Help: "PostgreSQL pgxpool connection counts by state.",
	}, []string{"state"}) // state: "acquired", "idle", "total", "max"

	// ── HTTP & gRPC Traffic Metrics (PENDING-10 Phase 2) ────────────────────
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests handled by method, handler, and status code.",
	}, []string{"method", "handler", "status_code"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by handler.",
		Buckets: prometheus.DefBuckets,
	}, []string{"handler"})

	grpcCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_handled_total",
		Help: "Total gRPC server calls handled by service, method, and status code.",
	}, []string{"grpc_service", "grpc_method", "grpc_code"})

	grpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "grpc_server_handling_seconds",
		Help:    "gRPC server call latency by service and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"grpc_service", "grpc_method"})
)

// appCollectors are the controller's own collectors (runtime collectors are
// registered separately). Tests describe these to check that every metric the
// alert rules and dashboard reference actually exists.
var appCollectors = []prometheus.Collector{
	// Reconciler
	reconcileReports,
	reconcileDriftDetected,
	reconcileResyncs,
	reconcileTombstonesReaped,
	reconcileShieldsDrifting,
	reconcileTombstonesPending,
	discoveryRequests,
	geoipDatabaseBuild,

	// ACL Compiler
	aclCompileDuration,
	aclCompiles,
	policyNotifications,

	// Relay Fleet
	relayHeartbeats,
	relayActiveCount,
	relayConnectionsTotal,
	relayCapacityMax,
	relayCapacityUtilization,
	relayCapacityTransitions,

	// Connector Fleet
	connectorHeartbeats,
	connectorActiveStreams,
	connectorActiveCount,
	connectorACLLag,

	// PKI
	certs,
	pkiRotations,

	// Outbox
	outboxEventsPublished,
	outboxEventsProcessed,
	outboxEventsPending,
	outboxDeadLetterTotal,
	outboxOldestPendingAge,

	// DB Pool
	dbPoolConnections,

	// HTTP & gRPC
	httpRequests,
	httpRequestDuration,
	grpcCalls,
	grpcDuration,
}

func init() {
	registry.MustRegister(appCollectors...)
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Pre-populate known label series to ensure zero-value metrics are emitted from startup
	reconcileDriftDetected.WithLabelValues("orphan")
	reconcileDriftDetected.WithLabelValues("missing")

	aclCompileDuration.WithLabelValues("ok")
	aclCompileDuration.WithLabelValues("error")
	aclCompiles.WithLabelValues("ok")
	aclCompiles.WithLabelValues("error")

	policyNotifications.WithLabelValues("policy")
	policyNotifications.WithLabelValues("transport")

	relayHeartbeats.WithLabelValues("accepted")
	relayHeartbeats.WithLabelValues("rejected")
	relayHeartbeats.WithLabelValues("revoked")

	connectorHeartbeats.WithLabelValues("ok")
	connectorHeartbeats.WithLabelValues("rejected")
	connectorHeartbeats.WithLabelValues("revoked")

	outboxEventsProcessed.WithLabelValues("success")
	outboxEventsProcessed.WithLabelValues("retryable_failure")
	outboxEventsProcessed.WithLabelValues("dead_letter")

	// Cert series are NOT pre-populated: an entity nobody has reported yet is
	// absent, never a 0 that reads as "expired and invalid".
	pkiRotations.WithLabelValues("controller_grpc", "ok")
	pkiRotations.WithLabelValues("controller_grpc", "error")

	for _, state := range []string{"acquired", "idle", "total", "max"} {
		dbPoolConnections.WithLabelValues(state)
	}
}

// ── Reconciler instrumentation ──────────────────────────────────────────────

func ReconcileReport()          { reconcileReports.Inc() }
func DriftDetected(kind string) { reconcileDriftDetected.WithLabelValues(kind).Inc() }
func Resync()                   { reconcileResyncs.Inc() }
func TombstoneReaped()          { reconcileTombstonesReaped.Inc() }
func SetReconcileGauges(drifting, pending int) {
	reconcileShieldsDrifting.Set(float64(drifting))
	reconcileTombstonesPending.Set(float64(pending))
}

// ── Workspace-discovery rate limiting ───────────────────────────────────────

func DiscoveryRequest(operation, result string) {
	discoveryRequests.WithLabelValues(operation, result).Inc()
}

func DiscoveryInitSeries(operation string, results ...string) {
	for _, r := range results {
		discoveryRequests.WithLabelValues(operation, r)
	}
}

// ── ACL Compiler & Policy Engine ────────────────────────────────────────────

// RecordACLCompile records compilation duration and status outcome.
func RecordACLCompile(duration time.Duration, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	aclCompileDuration.WithLabelValues(status).Observe(duration.Seconds())
	aclCompiles.WithLabelValues(status).Inc()
}

// RecordPolicyNotification increments notifications emitted on the policy or transport plane.
func RecordPolicyNotification(plane string) {
	policyNotifications.WithLabelValues(plane).Inc()
}

// ── Relay Fleet Metrics ─────────────────────────────────────────────────────

// RecordRelayHeartbeat counts relay heartbeats by bounded status outcome.
func RecordRelayHeartbeat(status string) {
	relayHeartbeats.WithLabelValues(status).Inc()
}

// SetRelayFleetGauges updates active relay counts, total connections, and capacity utilization.
func SetRelayFleetGauges(activeCount int, totalConns, maxCapacity uint32) {
	relayActiveCount.Set(float64(activeCount))
	relayConnectionsTotal.Set(float64(totalConns))
	relayCapacityMax.Set(float64(maxCapacity))

	if maxCapacity > 0 {
		ratio := float64(totalConns) / float64(maxCapacity)
		relayCapacityUtilization.Set(math.Round(ratio*1000) / 1000)
	} else {
		relayCapacityUtilization.Set(0)
	}
}

// RecordRelayCapacityTransition records transitions between capacity states.
func RecordRelayCapacityTransition(transition string) {
	relayCapacityTransitions.WithLabelValues(transition).Inc()
}

// ── Connector Fleet Metrics ─────────────────────────────────────────────────

// RecordConnectorHeartbeat counts connector health reports by status.
func RecordConnectorHeartbeat(status string) {
	connectorHeartbeats.WithLabelValues(status).Inc()
}

// SetConnectorActiveStreams updates the count of currently open connector gRPC control streams.
func SetConnectorActiveStreams(count int) {
	connectorActiveStreams.Set(float64(count))
}

// SetConnectorActiveCount updates the count of live connectors (see the metric help).
func SetConnectorActiveCount(count int) {
	connectorActiveCount.Set(float64(count))
}

// RecordConnectorACLLag observes the version gap between controller ACL and connector reported ACL.
func RecordConnectorACLLag(expectedVersion, reportedVersion uint64) {
	if expectedVersion >= reportedVersion {
		lag := float64(expectedVersion - reportedVersion)
		connectorACLLag.Observe(lag)
	}
}

// ── PKI & Certificate Runway ────────────────────────────────────────────────

// SetCertRunway records the validity window of a certificate entity
// ("controller_grpc", "intermediate_ca", "root_ca"). Runway and validity are
// computed from it at every scrape, so they stay current between calls.
// valid=false forces cert_valid to 0 (e.g. a failed chain check).
func SetCertRunway(entity string, notBefore, notAfter time.Time, valid bool) {
	certs.set(entity, certWindow{notBefore: notBefore, notAfter: notAfter, valid: valid})
}

// RecordPKIRotation counts a certificate rotation attempt.
func RecordPKIRotation(entity string, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	pkiRotations.WithLabelValues(entity, status).Inc()
}

type certWindow struct {
	notBefore, notAfter time.Time
	valid               bool
}

// certCollector emits the cert gauges from the recorded windows at scrape time.
type certCollector struct {
	mu      sync.RWMutex
	windows map[string]certWindow
	now     func() time.Time

	runway, valid, lifetime *prometheus.Desc
}

var certs = &certCollector{
	windows:  map[string]certWindow{},
	now:      time.Now,
	runway:   prometheus.NewDesc("cert_runway_seconds", "Time remaining until certificate expiration in seconds (0 once expired).", []string{"entity"}, nil),
	valid:    prometheus.NewDesc("cert_valid", "Certificate validity indicator (1 = valid and within its validity window, 0 = expired, not yet valid, or failed validation).", []string{"entity"}, nil),
	lifetime: prometheus.NewDesc("cert_lifetime_seconds", "Total validity window (notAfter - notBefore) of the current certificate in seconds.", []string{"entity"}, nil),
}

func (c *certCollector) set(entity string, w certWindow) {
	c.mu.Lock()
	c.windows[entity] = w
	c.mu.Unlock()
}

func (c *certCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.runway
	ch <- c.valid
	ch <- c.lifetime
}

func (c *certCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for entity, w := range c.windows {
		runway := w.notAfter.Sub(now).Seconds()
		if runway < 0 {
			runway = 0
		}
		valid := 0.0
		if w.valid && now.After(w.notBefore) && now.Before(w.notAfter) {
			valid = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.runway, prometheus.GaugeValue, runway, entity)
		ch <- prometheus.MustNewConstMetric(c.valid, prometheus.GaugeValue, valid, entity)
		ch <- prometheus.MustNewConstMetric(c.lifetime, prometheus.GaugeValue, w.notAfter.Sub(w.notBefore).Seconds(), entity)
	}
}

// ── Outbox Pipeline Metrics ─────────────────────────────────────────────────

// RecordOutboxEventPublished increments published events by type.
func RecordOutboxEventPublished(eventType string) {
	outboxEventsPublished.WithLabelValues(eventType).Inc()
}

// RecordOutboxEventProcessed increments processed events by status.
func RecordOutboxEventProcessed(status string) {
	outboxEventsProcessed.WithLabelValues(status).Inc()
}

// RecordOutboxDeadLetter increments the dead-letter terminal failure counter.
func RecordOutboxDeadLetter() {
	outboxDeadLetterTotal.Inc()
}

// SetOutboxQueueGauges updates pending outbox queue depth and oldest event age.
func SetOutboxQueueGauges(pendingCount int, oldestAge time.Duration) {
	outboxEventsPending.Set(float64(pendingCount))
	if oldestAge > 0 {
		outboxOldestPendingAge.Set(oldestAge.Seconds())
	} else {
		outboxOldestPendingAge.Set(0)
	}
}

// ── Database Connection Pool Metrics ────────────────────────────────────────

// SetDBPoolStats updates PostgreSQL pool connection metrics.
func SetDBPoolStats(acquired, idle, total, max int32) {
	dbPoolConnections.WithLabelValues("acquired").Set(float64(acquired))
	dbPoolConnections.WithLabelValues("idle").Set(float64(idle))
	dbPoolConnections.WithLabelValues("total").Set(float64(total))
	dbPoolConnections.WithLabelValues("max").Set(float64(max))
}

// ── HTTP & gRPC Server Metrics ──────────────────────────────────────────────

// RecordHTTPRequest records an incoming HTTP request duration and status.
// handler must be a route pattern, never a raw path (cardinality rule).
func RecordHTTPRequest(method, handler, statusCode string, duration time.Duration) {
	httpRequests.WithLabelValues(method, handler, statusCode).Inc()
	httpRequestDuration.WithLabelValues(handler).Observe(duration.Seconds())
}

// RecordGRPCCall records a gRPC server call duration and code.
func RecordGRPCCall(service, method, code string, duration time.Duration) {
	grpcCalls.WithLabelValues(service, method, code).Inc()
	grpcDuration.WithLabelValues(service, method).Observe(duration.Seconds())
}

// HTTPMiddleware records http_requests_total and http_request_duration_seconds.
// It must wrap the http.ServeMux directly: the mux sets Request.Pattern on the
// request it receives, and the handler label is that route pattern ("unmatched"
// when no route matched), never the raw path, which carries IDs. The method
// label is folded to a fixed set for the same reason.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		handler := r.Pattern
		if handler == "" {
			handler = "unmatched"
		}
		RecordHTTPRequest(boundedMethod(r.Method), handler, strconv.Itoa(sw.status), time.Since(start))
	})
}

func boundedMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "other"
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush/Hijack on the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ── Exposition ──────────────────────────────────────────────────────────────

func SetGeoIPBuild(build time.Time) {
	if build.IsZero() {
		geoipDatabaseBuild.Set(0)
		return
	}
	geoipDatabaseBuild.Set(float64(build.Unix()))
}

// Handler serves the controller's metrics in Prometheus format.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
