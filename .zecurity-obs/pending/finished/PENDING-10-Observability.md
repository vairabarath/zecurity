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
> 3. **Phase 3 — OpenTelemetry Distributed Tracing (`ce737446`):** Opt-in OTLP exporter and W3C TraceContext propagation across HTTP and gRPC. Outbox dispatch spans start a new trace that links to the publishing request's span (`050_outbox_trace_context.sql`, see Post-live-test fixes).
> 4. **Phase 4 — Alerts & Dashboards:** Prometheus alert definitions in `deploy/monitoring/alerts.yaml` and production Grafana dashboard in `deploy/monitoring/dashboards/zecurity_overview.json`.
>
> **Live test 2026-11-05 (Sathiya):** the first live run found alerts firing on a healthy controller, alerts and dashboard queries pointing at metrics that don't exist, HTTP/gRPC metrics never recorded and a tracing service name lost. All fixed and re-verified live the same day; see **Post-live-test fixes** at the end. Commit hashes above are from before the branch was rebased onto PR #177.

---

## Architectural Decision Note

During the review of PENDING-10, the following core architectural decisions were approved and implemented:

### Decision 1: Health Check Detail Level — Minimal Public Responses
- **Public Port (`:8080`):**
  - `/livez`: Minimal liveness check returning `200 {"status":"ok"}`. Strictly independent of the database and external dependencies.
  - `/readyz`: Minimal readiness check returning `200 {"status":"ready"}` or `503 {"status":"not_ready"}` with bounded timeout contexts.
  - `/healthz`: Minimal overall health check.
  - `/health`: Preserved for backward compatibility: it reflects the database only, as the old `db.Pool.Ping` handler did. Valkey, PKI or outbox problems show in `/readyz` and `/healthz`, not `/health`.
- **Internal Port (`:9102` / `METRICS_ADDR`):**
  - Component diagnostics (Postgres connection pool, Valkey, PKI validity and runway, Outbox processor) are restricted exclusively to the private management listener.
  - No detailed diagnostics are exposed through unauthenticated public query parameters.

### Decision 2: OpenTelemetry Exporter Default — Opt-in Tracing
- **Tracing Provider:** Tracing is strictly opt-in. The OTLP trace exporter connects only when `OTEL_EXPORTER_OTLP_ENDPOINT` is configured.
- **Zero Overhead:** When tracing is disabled, a no-op TracerProvider is utilized, avoiding exporter initialization, background workers, and network connections.
- **W3C TraceContext:** Full propagation of `traceparent` across HTTP middleware and gRPC unary/streaming server interceptors.
- **Outbox dispatch:** each dispatch runs in its own span (`outbox.dispatch_<event_type>`), which starts a **new trace** with a **span link** to the span that published the event. `Enqueue` stores that span's W3C `traceparent` in `outbox_events.traceparent` (`050_outbox_trace_context.sql`); it stores NULL when tracing is disabled or the publishing context has no span. A link, not a parent: an event can be retried with backoff for hours after the request's trace ended. Only `traceparent` is stored; `tracestate` and baggage are client-supplied request data and never reach the database.

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
  - `relay_active_count` (relays with status active in the DB), `relay_capacity_max`, `relay_connections_total`, `relay_capacity_utilization_ratio`
  - `relay_heartbeats_total{status}`, `relay_capacity_transitions_total{transition}`
  - `connector_active_streams` (open control streams), `connector_active_count` (connectors with a heartbeat within the disconnect threshold), `connector_heartbeats_total{status}`, `connector_acl_lag_versions` (histogram, in ACL versions, not seconds)
- **Engine Metrics:**
  - `acl_compile_duration_seconds` (histogram), `acl_compiles_total`
  - `policy_notifications_total{plane="policy|transport"}`
- **PKI & Certificate Runway:**
  - `cert_runway_seconds`, `cert_valid`, `cert_lifetime_seconds`, each `{entity="root_ca|intermediate_ca|controller_grpc"}`, computed at scrape time
  - `pki_rotations_total{entity="controller_grpc", status="ok|error"}`
- **Outbox Pipeline:**
  - `outbox_events_published_total{event_type}`, `outbox_events_processed_total{status}`
  - `outbox_dead_letter_total`, `outbox_events_pending`, `outbox_oldest_pending_age_seconds`
- **HTTP & gRPC servers:**
  - `http_requests_total{method, handler, status_code}`, `http_request_duration_seconds{handler}`; `handler` is the ServeMux route pattern (`unmatched` when none)
  - `grpc_server_handled_total{grpc_service, grpc_method, grpc_code}`, `grpc_server_handling_seconds{grpc_service, grpc_method}`
- **Database Connection Pool:**
  - `db_pool_connections{state="acquired|idle|total|max"}`

### 3. OpenTelemetry Distributed Tracing (`controller/internal/telemetry/`)
- `Init(ctx, serviceName)`: Initializes OTLP gRPC trace exporter if `OTEL_EXPORTER_OTLP_ENDPOINT` is present; returns graceful shutdown hook.
- `HTTPMiddleware(next)`: Extracts incoming W3C `traceparent` / `baggage` and records HTTP request spans.
- `UnaryServerInterceptor()`, `StreamServerInterceptor()`: Extracts W3C headers from gRPC metadata and traces RPCs.
- `outbox/processor.go`: Dispatches outbox events within an OpenTelemetry span (`outbox.dispatch_<event_type>`), a new root trace linked to the publishing span (`telemetry.CaptureTraceparent` / `LinkFromTraceparent` in `internal/telemetry/outbox.go`).
- `OTEL_EXPORTER_OTLP_ENDPOINT` takes `host:port` or a URL (`http://…` is plaintext, `https://…` TLS); export errors are logged.

### 4. Alert Rules & Dashboard Deliverables
- `deploy/monitoring/alerts.yaml`:
  - `CertExpiryImminentWarning` (under 1/4 of the cert's lifetime left) and `CertExpiryImminentCritical` (< 48 hours)
  - `CertInvalid` (cryptographic or chain failure)
  - `RelayFleetNearCapacity` (> 85% capacity for > 5m)
  - `ConnectorACLLagHigh` (p95 lag > 5 ACL versions)
  - `ACLCompileLatencyHigh` (p95 compile duration > 250ms)
  - `OutboxBacklogAccumulating` (> 500 events or oldest age > 300s)
  - `OutboxDeadLetterDetected` (> 0 dead letters)
  - `DatabasePoolExhausted` (all connections acquired for > 1m)
  - `ControllerReadinessFailing` (readiness probe down > 1m; needs a blackbox-exporter job probing `/readyz`, not shipped)
- `deploy/monitoring/alerts_test.yaml`: promtool unit tests for the rules.
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
  - `TestTraceContext_PropagationAcrossAsyncBoundaries` (removed in the post-live-test fixes with the unused inject/extract helpers)
  - `TestHTTPMiddleware_TracesRequest` (PASS)
  - `TestUnaryServerInterceptor` (PASS)
- **Full Controller Test Suite:**
  - Ran `cd controller && go test ./...`
  - Result: **100% Green, 0 Failures** across all controller packages. **Correction:** this run set no `*_TEST_DATABASE_URL`, so the DB-backed tests skipped. The 2026-11-05 gate with every DB variable set is in Post-live-test fixes.

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

> **Correction (2026-11-05):** the `0` runway values for `intermediate_ca` and `root_ca` above were the bug the live test found, not healthy output: the CA gauges were pre-set to 0 and never updated, which fired `CertInvalid` and `CertExpiryImminentCritical`.

---

## Post-live-test fixes (2026-11-05)

Live test on the real lab (controller with luffy's connector and the system client, Prometheus v2.53.2 loading `alerts.yaml`, Jaeger all-in-one 1.62.0). Branch rebased onto PR #177 (`8df26ced`); fixes in `f1f4be86` and `ce6ae2ca`.

### Fix: CA cert gauges stuck at 0 (alerts fired on a healthy controller)
**Issue:** `cert_runway_seconds` and `cert_valid` for `root_ca` and `intermediate_ca` were 0 forever, so `CertInvalid` and `CertExpiryImminentCritical` fired within two minutes of start.
**Root cause:** `metrics.init()` pre-populated all three entities at 0; only `controller_grpc` was ever set (by the rotator). Runway was also computed once at set time, so it never fell between rotations.
**Fix applied:** `internal/metrics/metrics.go`: the cert gauges are a custom collector that computes runway and validity at scrape time from recorded `notBefore`/`notAfter`; unrecorded entities are absent, not 0. New `cert_lifetime_seconds`. `internal/pki/service.go` `Init` records `root_ca` and `intermediate_ca` after the chain audit. New `pki_rotations_total{entity,status}` from the rotator.

### Fix: `CertExpiryImminentWarning` pending forever for the controller cert
**Issue:** the controller gRPC cert lives 7 days (configurable), always under the 14-day threshold.
**Fix applied:** `alerts.yaml`: warning is `cert_runway_seconds / cert_lifetime_seconds < 0.25`. The rotator renews at 2/3 of the lifetime, so a healthy cert stays at about 1/3 or more. The 48 h critical stays as a backstop.

### Fix: alerts that could never fire
- `DatabasePoolExhausted`: `acquired >= max` matched nothing (the sides differ in the `state` label). Now `>= ignoring(state)`; promtool test with 25 of 25 acquired.
- `ConnectorACLLagHigh`: read `connector_acl_lag_seconds_bucket`; the metric is `connector_acl_lag_versions` (versions, not seconds).
- `OutboxDeadLetterDetected`: read `outbox_events_dead_letter_total`; the metric is `outbox_dead_letter_total`.
- Annotations read `$labels.cert_type` (rendered `Certificate ''`); the label is `entity`.
- `ControllerReadinessFailing` still needs a blackbox-exporter job (`probe_success`); documented in the rule, not shipped.

### Fix: HTTP and gRPC metrics never recorded
**Issue:** `http_requests_total`, `http_request_duration_seconds`, `grpc_server_handled_total` and `grpc_server_handling_seconds` were registered, but `RecordHTTPRequest`/`RecordGRPCCall` had no callers.
**Fix applied:** `metrics.HTTPMiddleware` wraps the public mux directly (`main.go`) and labels by `Request.Pattern` (`unmatched` otherwise), never the raw path; methods fold to a fixed set. The telemetry gRPC interceptors record service, method and code.

### Fix: `connector_active_count` counted stale rows
**Issue:** reported 4 with 1 connector connected (3 rows with status `active` that never heartbeated).
**Fix applied:** the fleet-metrics query also requires `last_heartbeat_at` within `CONNECTOR_DISCONNECT_THRESHOLD` in an active workspace (the disconnect watcher's predicate). `relay_active_count`'s help now says it is the DB status (relay liveness lives in Valkey).

### Fix: tracing service name and endpoint
- `service.name` was `unknown_service:controller`: merging `resource.Default()` with a semconv-v1.26 schema resource failed on the schema URL and the code fell back to the default. Service attributes are now schemaless; a merge error is logged.
- `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` exported nothing, silently: the value went to `WithEndpoint`, which wants `host:port`. URLs are parsed now (`http` = plaintext, `https` = TLS); export errors are logged through `otel.SetErrorHandler`; startup logs the endpoint.
- HTTP span names used the raw path (IDs in span names); they are now the route pattern, with `http.route` set.
- The outbox trace-continuity claim was false: the dispatch span is a new root trace. The unused `InjectTraceContext`/`ExtractTraceContext` helpers and their test were removed, and the text above corrected. Real linking needs a trace-context column on `outbox_events`: added afterwards, see *Outbox trace link* below.

### Fix: `/health` failed when Valkey was down
**Issue:** `/health` used to be a database ping; it had become full readiness, so an external load balancer still probing it would drop the controller during a Valkey outage.
**Fix applied:** `PublicHealthHandler` follows the `database` component only.

### Fix: dashboard queries
12 of 22 queries returned nothing: six wrong metric names (`relay_capacity_total_*`, `connector_acl_lag_seconds` ×3, `pki_rotations_total` didn't exist, `outbox_events_dead_letter_total`), `{{cert_type}}` legends, the processed panel grouped by `event_type` (label is `status`), and the scrape-job regex. All fixed; the ACL lag panel is in versions. Cosmetic: log line "metrics listenin on" fixed.

### Fix: security review after push (2026-11-05)
- `internal/health/health.go` `Evaluate`: the checks ran in the triggering request's context, and the result is cached for every caller. A client aborting its `/readyz` request cancelled the checks and cached `not_ready` for 1 s, so repeated aborted probes could make a load balancer drop the controller. Checks now run in `context.WithoutCancel(ctx)` with the 2 s timeout. Test `TestEvaluate_CallerCancellationNotCached`.
- `internal/telemetry/http.go`: spans recorded `http.target` (raw path, which carries invitation tokens on `/api/invitations/{token}`) and `http.host` (client-supplied Host). Both are gone; `http.route` (the pattern) remains. Asserted in `TestHTTPMiddleware_SpanNamedByRoute`.

### Outbox trace link (2026-11-05) `[schema: reset DB]`
- `controller/migrations/050_outbox_trace_context.sql`: nullable `outbox_events.traceparent TEXT`, with a CHECK for the W3C format.
- `internal/telemetry/outbox.go`: `CaptureTraceparent(ctx)` injects only `traceparent`, through `propagation.TraceContext{}` and not the global propagator (no baggage), and with tracestate cleared. It returns nil when tracing is off, so a client-supplied `traceparent` isn't persisted on the disabled path. `LinkFromTraceparent` parses the stored value into a `trace.Link`.
- `internal/outbox/store.go`: `Enqueue` writes it; `ClaimEvents` returns it. `processor.go`: the dispatch span gets `trace.WithLinks(...)`.
- Publishers: both callers, `graph/resolvers/idp_helpers.go` (GraphQL) and `internal/scim/directory_service.go` (SCIM), enqueue with the HTTP request context, which goes through `telemetry.HTTPMiddleware`, so both are linked.
- Tests: `TestOutboxTraceLinkIntegration` (stored traceparent = publishing span; dispatch span is a new root with exactly one link to it; no span means NULL and no link; the schema rejects a malformed value), `TestCaptureTraceparent` (tracestate not stored; round trip), `TestCaptureTraceparent_DisabledStoresNothing`, `TestLinkFromTraceparent_Invalid`.
- Gate: full suite with every DB variable set, **1712 pass, 0 fail**, the same 2 skips.
- Not live-checked in Jaeger yet: it needs a DB reset and a branch-built controller with a collector.

### Regression guards
- `internal/metrics/monitoring_refs_test.go`: every metric and label used by `alerts.yaml` and the dashboards must be exported by the controller (`up` and `probe_success` allowed). It fails on the pre-fix files.
- `deploy/monitoring/alerts_test.yaml`: promtool tests (healthy controller fires nothing; stuck rotation warns then goes invalid; pool exhaustion; dead letter; ACL lag).
- Unit tests for the scrape-time cert collector, route-pattern HTTP labels and span names, endpoint forms, the service name, and `/health` with Valkey down.

### Gate and live re-run (2026-11-05)
- `go test -count=1 ./...` with every `*_TEST_DATABASE_URL` and `AUTH_TEST_VALKEY_URL` set: **1705 pass (after the security-review fix), 0 fail**, the 2 usual skips. `go vet` clean. promtool `check rules` and `test rules` pass.
- Live, controller built from the branch tip (binary sha checked):
  - cert gauges real for all three entities (CA runway in years, valid 1); `pki_rotations_total` present.
  - `connector_active_count 1` = `connector_active_streams 1`.
  - `http_requests_total` labels: `/graphql`, `/readyz`, `GET /provider/relays/{id}`, `unmatched` (no IDs); `grpc_server_handled_total` for `client.v1.ClientService` calls.
  - Valkey paused: `/readyz` 503 (bounded 2 s), `/healthz` 503, `/health` 200. Postgres paused: `/health` 503. All 200 after unpause.
  - Jaeger: service `zecurity-controller` with both `localhost:4317` and `http://localhost:4317`; a request's span is the child of its `traceparent`; span names are routes.
  - Outbox: one IdP connection create + delete published and processed one `policy.invalidation.requested` event; its dispatch span is a new root trace, as documented.
  - Dashboard: 21 of 22 queries return data; the remaining one is `probe_success` (blackbox exporter, not shipped).
  - Prometheus with the shipped rules, 7½ minutes after start: **0 alerts pending or firing**; runway/lifetime ratio 0.99 for all three certs.
  - Collector unreachable (`http://localhost:4399`): the export error is logged (`telemetry: traces export: … connection refused`).
- Not tested: relay metrics (no relay in this lab); `connector_active_streams` dropping when the connector stops.

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
