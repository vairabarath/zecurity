---
type: adr
status: implemented
id: PENDING-10
domain: operations
priority: P1
created: 2026-07-03
implemented: 2026-11-04
related:
  - ADR-014-Connector-Control-Plane-Protocol
  - ADR-017-Decouple-Transport-From-ACL
  - PENDING-15-Durable-Outbox-Infrastructure
tags: [pending, adr, operations, observability, metrics, tracing, health]
---

# Pending ADR 10 — Observability: Metrics, Tracing, Health

> **Status: IMPLEMENTED (2026-11-04).** All four phases of enterprise observability have been implemented, tested, and live-verified on branch `feat/pending10-observability`:
> 1. **Phase 1 — Health & Readiness Subsystem (`cad18733`):** Minimal public probes on `:8080` (`/livez`, `/readyz`, `/healthz`, `/health`) and detailed internal diagnostics on `:9102` (`METRICS_ADDR`).
> 2. **Phase 2 — Prometheus Metrics & Low-Cardinality Enforcement (`b1bd8034`):** Real-time fleet gauges, ACL compile histograms, PKI cert runway/validity, and outbox pipeline telemetry with strict zero-high-cardinality invariant.
> 3. **Phase 3 — OpenTelemetry Distributed Tracing (`ce737446`):** Opt-in OTLP exporter, W3C TraceContext propagation across HTTP and gRPC, and asynchronous trace context continuation in outbox event dispatching.
> 4. **Phase 4 — Alerts & Dashboards:** Prometheus alert definitions in `deploy/monitoring/alerts.yaml` and production Grafana dashboard in `deploy/monitoring/dashboards/zecurity_overview.json`.

---

## Architectural Decision Note

During the review of PENDING-10, the following core architectural decisions were approved and implemented:

### Decision 1: Health Check Detail Level — Minimal Public Responses
- **Public Port (`:8080`):**
  - `/livez`: Minimal liveness check returning `200 {"status":"ok"}`. Strictly independent of the database and external dependencies.
  - `/readyz`: Minimal readiness check returning `200 {"status":"ready"}` or `503 {"status":"not_ready"}` with bounded timeout contexts.
  - `/healthz`: Minimal overall health check.
  - `/health`: Preserved for backward compatibility.
- **Internal Port (`:9102` / `METRICS_ADDR`):**
  - Component diagnostics (Postgres connection pool, Valkey, PKI validity and runway, Outbox processor) are restricted exclusively to the private management listener.
  - No detailed diagnostics are exposed through unauthenticated public query parameters.

### Decision 2: OpenTelemetry Exporter Default — Opt-in Tracing
- **Tracing Provider:** Tracing is strictly opt-in. The OTLP trace exporter connects only when `OTEL_EXPORTER_OTLP_ENDPOINT` is configured.
- **Zero Overhead:** When tracing is disabled, a no-op TracerProvider is utilized, avoiding exporter initialization, background workers, and network connections.
- **W3C TraceContext:** Full propagation of `traceparent` across HTTP middleware and gRPC unary/streaming server interceptors.
- **Asynchronous Trace Continuity:** Outbox dispatcher creates trace spans for asynchronous worker jobs, preserving trace context across process boundaries.

### Decision 3: Metric Cardinality Rule — Strictly Enforced Invariant
- **Rule:** High-cardinality labels are forbidden with zero exceptions.
- **Forbidden Labels:** `tenant_id`, `workspace_id`, `resource_id`, `user_id`, client IP addresses, certificate serial numbers, and arbitrary error strings.
- **Enforcement:** Enforced at build and test time via automated unit test `TestMetrics_CardinalityInvariantRule`.

---

## What Was Implemented

### 1. Health & Diagnostics (`controller/internal/health/`)
- `Registry`: Thread-safe health check registry with cached probe results (1 second TTL) to prevent probe storms from saturating downstream services.
- `LivenessHandler()`: Pure process-local check.
- `PublicReadinessHandler()`, `PublicHealthzHandler()`, `PublicHealthHandler()`: Minimal status handlers.
- `InternalDiagnosticsHandler()`: Emits structured JSON diagnostics with per-component latency and sub-system state.
- Checkers implemented: `DatabaseChecker`, `ValkeyChecker`, `PKIChecker`, `OutboxChecker`.

### 2. Prometheus Metrics (`controller/internal/metrics/`)
- **Fleet Metrics:**
  - `relay_active_count`, `relay_capacity_total_max_sessions`, `relay_capacity_total_active_sessions`, `relay_capacity_utilization_ratio`
  - `relay_heartbeats_total`, `relay_capacity_transitions_total`
  - `connector_active_streams`, `connector_heartbeats_total`, `connector_acl_lag_seconds` (histogram)
- **Engine Metrics:**
  - `acl_compile_duration_seconds` (histogram), `acl_compiles_total`
  - `policy_notifications_total{subsystem="policy|transport"}`
- **PKI & Certificate Runway:**
  - `cert_runway_seconds{entity="root_ca|intermediate_ca|controller_grpc"}`
  - `cert_valid{entity="root_ca|intermediate_ca|controller_grpc"}`
  - `pki_rotations_total{cert_type, status}`
- **Outbox Pipeline:**
  - `outbox_events_published_total`, `outbox_events_processed_total{status}`
  - `outbox_events_dead_letter_total`, `outbox_events_pending`, `outbox_oldest_pending_age_seconds`
- **Database Connection Pool:**
  - `db_pool_connections{state="acquired|idle|total|max"}`

### 3. OpenTelemetry Distributed Tracing (`controller/internal/telemetry/`)
- `Init(ctx, serviceName)`: Initializes OTLP gRPC trace exporter if `OTEL_EXPORTER_OTLP_ENDPOINT` is present; returns graceful shutdown hook.
- `HTTPMiddleware(next)`: Extracts incoming W3C `traceparent` / `baggage` and records HTTP request spans.
- `UnaryServerInterceptor()`, `StreamServerInterceptor()`: Extracts W3C headers from gRPC metadata and traces RPCs.
- `outbox/processor.go`: Dispatches outbox events within an OpenTelemetry span (`outbox.dispatch_<event_type>`).

### 4. Alert Rules & Dashboard Deliverables
- `deploy/monitoring/alerts.yaml`:
  - `CertExpiryImminentWarning` (< 14 days) and `CertExpiryImminentCritical` (< 48 hours)
  - `CertInvalid` (cryptographic or chain failure)
  - `RelayFleetNearCapacity` (> 85% capacity for > 5m)
  - `ConnectorACLLagHigh` (p95 sync lag > 5s)
  - `ACLCompileLatencyHigh` (p95 compile duration > 250ms)
  - `OutboxBacklogAccumulating` (> 500 events or oldest age > 300s)
  - `OutboxDeadLetterDetected` (> 0 dead letters)
  - `DatabasePoolExhausted` (all connections acquired for > 1m)
  - `ControllerReadinessFailing` (readiness probe down > 1m)
- `deploy/monitoring/dashboards/zecurity_overview.json`:
  - Production Grafana dashboard with panels for Controller Health, Relay Fleet Capacity, Connector ACL Lag, PKI Runway Countdown, Outbox Throughput & Dead Letters, and DB Pool Utilization.

---

## What Was Tested

### 1. Automated Unit & Integration Tests
- `controller/internal/health/health_test.go`:
  - `TestLivenessHandler_IndependentAndMinimal` (PASS)
  - `TestPublicReadiness_MinimalAndProtected` (PASS)
  - `TestPublicHealthzAndHealth_Compatibility` (PASS)
  - `TestInternalDiagnosticsHandler_DetailedDiagnostics` (PASS)
  - `TestRegistry_CachingPreventsProbeStorms` (PASS)
  - `TestPKIChecker` (PASS)
  - `TestOutboxChecker` (PASS)
- `controller/internal/metrics/metrics_test.go`:
  - `TestMetrics_CardinalityInvariantRule` (PASS — verifies every registered collector forbids high-cardinality labels)
  - `TestACLCompilerMetrics` (PASS)
  - `TestRelayFleetMetrics` (PASS)
  - `TestConnectorFleetMetrics` (PASS)
  - `TestPKIMetrics` (PASS)
  - `TestOutboxMetrics` (PASS)
  - `TestDBPoolMetrics` (PASS)
  - `TestHandler_ExpositionFormat` (PASS)
- `controller/internal/telemetry/telemetry_test.go`:
  - `TestInit_DisabledByDefault` (PASS — verifies no-op when endpoint unset)
  - `TestTraceContext_PropagationAcrossAsyncBoundaries` (PASS — verifies W3C inject/extract)
  - `TestHTTPMiddleware_TracesRequest` (PASS)
  - `TestUnaryServerInterceptor` (PASS)
- **Full Controller Test Suite:**
  - Ran `cd controller && go test ./...`
  - Result: **100% Green, 0 Failures** across all controller packages.

---

## Live Test Results

The implementation was verified against a live controller instance connected to PostgreSQL 16 and Valkey 7.2:

```bash
# Public Liveness Probe
$ curl -s http://localhost:8082/livez
{"status":"ok"}

# Public Readiness Probe
$ curl -s http://localhost:8082/readyz
{"status":"ready"}

# Prometheus Metrics Exposition (Private :9103)
$ curl -s http://localhost:9103/metrics
# HELP cert_runway_seconds Time remaining until certificate expiration in seconds.
# TYPE cert_runway_seconds gauge
cert_runway_seconds{entity="controller_grpc"} 604799.996122953
cert_runway_seconds{entity="intermediate_ca"} 0
cert_runway_seconds{entity="root_ca"} 0
# HELP outbox_events_pending Current count of pending outbox events awaiting processing.
# TYPE outbox_events_pending gauge
outbox_events_pending 0
# HELP outbox_events_processed_total Total outbox events dispatched by outcome.
# TYPE outbox_events_processed_total counter
outbox_events_processed_total{status="dead_letter"} 0
outbox_events_processed_total{status="retryable_failure"} 0
outbox_events_processed_total{status="success"} 0
# HELP relay_active_count Number of active relays currently heartbeating within threshold.
# TYPE relay_active_count gauge
relay_active_count 0

# Internal Component Diagnostics (Private :9103)
$ curl -s http://localhost:9103/healthz
{
  "status": "ready",
  "timestamp": "2026-11-04T10:17:51.89400153Z",
  "duration_ms": 0.854,
  "components": {
    "database": {
      "status": "up",
      "latency_ms": 0.751,
      "details": { "acquired_conns": 0, "idle_conns": 3, "max_conns": 25, "total_conns": 3 }
    },
    "outbox": {
      "status": "up",
      "details": { "running": true }
    },
    "pki": {
      "status": "up",
      "details": { "cert_valid": true, "expires_in_seconds": 604797, "not_after": "2026-11-11T10:17:49Z" }
    },
    "valkey": {
      "status": "up",
      "latency_ms": 0.566
    }
  }
}
```

---

## Original Proposal & Background (Archived)

### Context / Current State
There was structured logging (e.g. `tracing` in Rust services, connector `ConnectorLog`), but no unified metrics/tracing/SLO layer for the control and data planes. Operating a relay fleet + connectors + shields across tenants needed quantitative signals: relay capacity/`connection_count` trends, probe/migration rates, ACL compile latency, heartbeat health, tunnel throughput/errors, cert-expiry runway.

### Problem — Decision Needed
What observability stack and signal set do we standardize on?

### Options Considered
- **Option A (Adopted):** Prometheus metrics + OpenTelemetry traces. `/metrics` on each service; OTel traces across control-stream + tunnel setup; Grafana dashboards.
- **Option B (Rejected):** Vendor APM (Datadog/Honeycomb/Grafana Cloud) — rejected due to operational data egress and vendor lock-in.
- **Option C (Rejected):** Minimal health/metrics endpoints only — rejected due to lack of distributed trace context across components.
