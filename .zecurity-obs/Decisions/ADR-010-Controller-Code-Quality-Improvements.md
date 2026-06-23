---
type: decision
status: proposed
date: 2026-06-13
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[Decisions/ADR-009-GraphQL-DoS-Hardening]]"
tags:
  - adr
  - controller
  - code-quality
  - hygiene
  - middleware
  - observability
  - refactor
---

# ADR-010 — Controller Code Quality Improvements (Stage 3 Audit Follow-ups)

## Context

The Stage 3 connector-flow audit surfaced **seven small improvements** to
the controller's HTTP server / middleware / GraphQL setup. None are
security-critical (those live in ADR-009). All are quality-of-life
improvements: cleaner code, better observability, less duplication.

Grouping them in one ADR so the team can decide them as a single set
("polish PR") rather than per-finding micro-decisions.

The audit considered each in isolation. This ADR captures the package.

## Items

### 1. F6 — Workspace status cache (performance)

[middleware/workspace.go:34-38](controller/internal/middleware/workspace.go#L34-L38) — every
authenticated `/graphql` request runs `SELECT status FROM workspaces WHERE id = $1`.
Admin UI polls every 30s; at 50 concurrent admins that's ~1.6 DB queries/sec
just for status lookups, none of which return changing data.

**Fix**: small LRU + 30s TTL cache. Invalidate on workspace status change
(if/when "suspend workspace" feature ships).

**Defer rationale**: not a real problem until traffic scales. Add reactively.

### 2. F7 — Distinguish DB errors from "not found" (observability)

[middleware/workspace.go:39-42](controller/internal/middleware/workspace.go#L39-L42):
```go
if err != nil {
    writeJSON403(w, "workspace not found")
    return
}
```

Treats ANY pgx error (connection drop, pool exhausted, timeout) as 403
"workspace not found." Misleads ops debugging — a DB outage looks like
"authorization issues" instead of triggering 5xx alerts.

**Fix** (5 LOC):
```go
if errors.Is(err, pgx.ErrNoRows) {
    writeJSON403(w, "workspace not found")
    return
}
if err != nil {
    log.Printf("workspace_guard: DB error for workspace %s: %v", tc.TenantID, err)
    http.Error(w, "internal server error", http.StatusInternalServerError)
    return
}
```

**Win**: monitoring/alerting catches DB problems via 5xx spikes rather than
misclassified 403s.

### 3. F13 — Panic-recovery middleware (observability)

Go's `http.Server` already recovers from handler panics by default (the
server doesn't crash). What's missing:
- Clean JSON 500 response to the client (default: connection drops)
- Structured logs with full stack trace
- Hook point for future metrics

**Fix** (~15 LOC):
```go
import "runtime/debug"

func RecoverMW(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                log.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
                w.Header().Set("Content-Type", "application/json")
                w.WriteHeader(http.StatusInternalServerError)
                _, _ = w.Write([]byte(`{"error":"internal server error"}`))
            }
        }()
        next.ServeHTTP(w, r)
    })
}
```

Wrap the mux at the top: `log.Fatal(http.ListenAndServe(addr, RecoverMW(mux)))`.

**Win**: when panics happen (rare), ops gets a clean log + the client gets
a proper 500 instead of a mysterious connection drop.

### 4. CQ-1 — Extract `jwtSecret` from `mustEnv("JWT_SECRET")` calls (readability)

[main.go](controller/cmd/server/main.go) reads `JWT_SECRET` **eight times**
(lines 76, 94, 103, 171, 180, 189, 197, 207). Each call repeats `mustEnv(...)`.

**Fix**:
```go
jwtSecret := mustEnv("JWT_SECRET")
// ... use jwtSecret in every downstream call ...
```

**Win**: single source of truth. Easier to add validation (length check
from ADR-007) or rotation later.

### 5. CQ-2 — Shared JSON error helper (consistency)

Three middleware files define three different "write JSON error" helpers:

| File | Format |
|------|--------|
| `auth.go::writeJSON401` | GraphQL-style `{"errors":[{"message":...,"extensions":{"code":"UNAUTHORIZED"}}]}` |
| `workspace.go::writeJSON403` | Same GraphQL-style with `"FORBIDDEN"` |
| `role.go` (inline) | Different: `{"error":"unauthenticated"}` |

The third doesn't match the others. Frontend sees inconsistent error
shapes depending on which middleware rejected the request.

**Fix** (~30 LOC delta):

```go
// In controller/internal/middleware/errors.go:
package middleware

import (
    "fmt"
    "net/http"
)

func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    fmt.Fprintf(w,
        `{"errors":[{"message":%q,"extensions":{"code":%q}}]}`, message, code)
}
```

All three middleware files use it. Frontend gets uniform error shape with
codes.

### 6. CQ-3 — Extract middleware chains (auditability)

[main.go:171-213](controller/cmd/server/main.go#L171-L213) hand-builds the
same `AuthMiddleware → (RequireRole?) → WorkspaceGuard → handler` chain
five times.

**Fix**: extract chain constructors:
```go
jwtSecret := mustEnv("JWT_SECRET")
authMW := middleware.AuthMiddleware(jwtSecret)
wsGuardMW := middleware.WorkspaceGuard(db.Pool)
adminMW := middleware.RequireRole("admin")

authedAPI := func(h http.Handler) http.Handler { return authMW(wsGuardMW(h)) }
adminAPI  := func(h http.Handler) http.Handler { return authMW(adminMW(wsGuardMW(h))) }

mux.Handle("POST /api/invitations",                adminAPI(http.HandlerFunc(inviteHandler.Create)))
mux.Handle("POST /api/invitations/{token}/accept", authedAPI(http.HandlerFunc(inviteHandler.Accept)))
mux.Handle("/api/connectors/",                     adminAPI(connector.RegenerateTokenHandler(...)))
mux.Handle("/api/shields/",                        adminAPI(shieldSvc.TokenHandler()))
```

**Win**: every route's auth chain is visible on its own line. Adding a
new admin endpoint is `adminAPI(handler)`. Auditors (and the linter from
F8 someday) can mechanically check that every privileged route uses
`adminAPI`.

### 7. CQ-4 — Modernize mux patterns (Go 1.22 method+path)

Mixed styles in [main.go](controller/cmd/server/main.go):
- Old-style prefix: `mux.Handle("/api/connectors/", ...)` (matches any HTTP method)
- Go 1.22: `mux.Handle("POST /api/invitations", ...)` (method+path)

**Fix**: migrate prefix routes to method+path:
```go
mux.Handle("POST /api/connectors/{id}/token", adminAPI(connector.RegenerateTokenHandler(...)))
mux.Handle("POST /api/shields/{id}/token",    adminAPI(shieldSvc.TokenHandler()))
```

Then drop the manual `strings.Split(r.URL.Path, ...)` parsing inside the
handlers — use `r.PathValue("id")` instead.

**Win**: method constraint enforced by mux (no more "I'm a POST handler
but I'm letting GET reach me"). Cleaner routes. Less manual parsing.

## Decision

Adopt all 7 improvements. Treat as a single "polish PR" the team reviews
as one set. Order recommended:

1. **CQ-1** (jwtSecret extract) — touches main.go, sets up CQ-3
2. **F7** (DB error classification) — 5 LOC, immediate observability win
3. **CQ-2** (shared error helper) — consolidation across 3 files
4. **CQ-3** (middleware chains) — main.go cleanup, builds on CQ-1
5. **CQ-4** (mux patterns) — main.go modernization
6. **F13** (panic recovery) — wraps the mux
7. **F6** (workspace cache) — DEFER unless perf issue surfaces

The first 6 are uncontroversial and small. F6 is the only one likely to
slip.

## Alternatives Considered

### Alt A — Skip all, focus on security only
**Rejected**: most of these are small (5-30 LOC each), and they make
future security work easier (clearer code → fewer surprises in audit).

### Alt B — Make these a series of micro-PRs instead of one ADR
**Rejected for v1**: too much process for items this small. Single
"hygiene PR" is the right granularity. Future micro-improvements can
be PRs without an ADR.

### Alt C — Defer everything to a "before GA" milestone
**Rejected for the first 6**: code quality compounds. Each PR after this
that touches main.go is harder if the file is still wired manually. Better
to clean now while context is fresh.

## Consequences

### Wins
- Single source for `JWT_SECRET` (CQ-1)
- Uniform error response shape (CQ-2)
- Routes visibly authz-classified (CQ-3)
- Method-safe routing (CQ-4)
- Better ops signal on DB issues (F7)
- Better panic visibility (F13)
- Less surface area for future security audits to re-cover

### Costs
- ~100 LOC of changes across 4 files
- Single review burden
- Risk of mis-refactor (mitigated by tests + the changes being mechanical)

### Risks
- **CQ-3 risk**: if `adminAPI` is mis-applied (e.g. wrapping the wrong
  handler), a route could lose its admin requirement. Mitigation: review
  each route in the diff with explicit "is this admin-only?" labeling.
- **CQ-4 risk**: changing prefix to exact path could miss edge cases
  (trailing slashes, etc.). Mitigation: integration tests for each route.

## Plan

### Phase 1 — Mechanical refactors (low risk)
- CQ-1 (jwtSecret extract)
- F7 (DB error classification)
- CQ-2 (shared error helper)

### Phase 2 — Structural refactors (medium risk)
- CQ-3 (middleware chains)
- CQ-4 (mux patterns)
- F13 (panic recovery)

### Phase 3 — Performance (defer)
- F6 (workspace cache) — only if real perf issue surfaces

## Verification

- `go vet ./...` clean
- `go test ./...` (if test suite exists) passes
- Manual smoke test:
  - Sign in → admin UI loads
  - Create a connector → invite flow works
  - Hit each REST endpoint with wrong role → confirms 403
  - Stop Postgres briefly → confirm 500 instead of 403 (F7)
  - Trigger a deliberate panic in a test handler → confirm clean 500 response + stack trace in log (F13)

## Notes

- These are explicitly **NOT security fixes** — those live in ADR-008
  (token caching), ADR-009 (DoS hardening). This ADR is hygiene only.
- F6 deferred — most likely never needed at expected scale.
- Each item is independently mergeable. The "single PR" recommendation
  is a process suggestion, not a requirement.
- Closes audit follow-ups: STAGE3-F6, F7, F13, CQ-1, CQ-2, CQ-3, CQ-4
  in [[CodeStudy/07-Connector-Audit]].
