---
type: decision
status: proposed
date: 2026-06-13
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[Decisions/ADR-008-Drop-InstallCommand-From-Mutation]]"
tags:
  - adr
  - controller
  - graphql
  - dos
  - rate-limiting
  - transport
  - security
  - hardening
---

# ADR-009 — GraphQL DoS Hardening (Complexity, Depth, Transport, Rate Limiting)

## Context

The Stage 3 connector-flow audit surfaced that gqlgen's `handler.NewDefaultServer`
does not configure any complexity limit, depth limit, or rate limit. Closes audit
findings **STAGE3-F2** (no complexity/depth limit) and creates a path for future
rate-limit work.

### The vulnerability today

`/graphql` accepts arbitrarily expensive queries. A single request can drive
Postgres CPU to saturation:

```graphql
query Bomb {
  workspace {
    connectors {
      resources {
        connector {
          resources {
            connector {
              resources {
                connector { resources { connector { name } } }
              }
            }
          }
        }
      }
    }
  }
}
```

The schema's cyclic relationships (`Connector.remoteNetwork → RemoteNetwork.connectors`,
`Resource.shield → Shield.remoteNetwork`, etc.) mean a single query can chain
unbounded JOINs / N+1 fetches.

### Why this is a real finding (not theoretical)

- Any authenticated workspace member can submit one
- Listed in OWASP API Security Top 10 (API4:2023 — Unrestricted Resource Consumption)
- Standard external-pentest finding — appears in every GraphQL audit report
- Combines with STAGE3-F1 (introspection — now fixed) and STAGE3-F5 (raw error
  leak) to make schema discovery + bomb construction trivial

### Attack vectors that matter

| Attack | What it does | Defense |
|--------|--------------|---------|
| **Single bomb query** | One 1000-complexity query stalls DB | Complexity limit |
| **Deep nesting cycle** | Recursive type traversal | Depth limit |
| **Field-count flood** | Flat query with 500 selected fields | Field count limit |
| **Request flood** | 100 normal queries per second | Per-IP rate limit |
| **Distributed flood** | 1000 IPs sending normal queries | Edge-level rate limit (CDN/WAF) |
| **Targeted abuse** | Authenticated user spamming costly mutations | Per-tenant rate limit |

A single defense is insufficient. Layered approach required.

## Decision

Implement DoS hardening in **three tiers**, deployed incrementally:

### Tier 1 — GraphQL-level limits + transport hardening (this audit pass)

This tier rolls THREE related Stage 3 findings into one `gqlSrv` restructure
because they touch the same setup code:

| Finding | What it adds |
|---------|--------------|
| STAGE3-F1 (introspection — already fixed) | conditional `introspectionDisabler{}` |
| STAGE3-F2 (no complexity/depth limit) | `FixedComplexityLimit(300)`, `FixedDepthLimit(10)` |
| STAGE3-F3 (GET transport enabled) | drop `transport.GET`, keep only `POST` + `MultipartForm` |

#### Code shape (Tier 1)

Replace `handler.NewDefaultServer(...)` with explicit `handler.New(...)` plus
chosen transports, caches, and extensions. `NewDefaultServer` is documented
as "not suitable for production use" by gqlgen itself.

```go
import (
    "github.com/99designs/gqlgen/graphql/handler"
    "github.com/99designs/gqlgen/graphql/handler/extension"
    "github.com/99designs/gqlgen/graphql/handler/lru"
    "github.com/99designs/gqlgen/graphql/handler/transport"
    "github.com/vektah/gqlparser/v2/ast"
)

gqlSrv := handler.New(graph.NewExecutableSchema(graph.Config{
    Resolvers:  &resolvers.Resolver{...},
    Directives: graph.DirectiveRoot{HasRole: resolvers.HasRole},
}))

// Transports: POST + MultipartForm only.
// GET deliberately omitted — STAGE3-F3 (CWE-598).
// Websocket only needed for subscriptions (none today).
gqlSrv.AddTransport(transport.POST{})
gqlSrv.AddTransport(transport.MultipartForm{})

gqlSrv.SetQueryCache(lru.New[*ast.QueryDocument](1000))

// Introspection: dev only — STAGE3-F1 (already fixed via introspectionDisabler).
// After this refactor, the cleaner pattern is to skip the install entirely
// in non-dev rather than install+disable. introspectionDisabler can be removed.
if os.Getenv("ENV") == "development" {
    gqlSrv.Use(extension.Introspection{})
}

// DoS limits — STAGE3-F2.
gqlSrv.Use(extension.FixedComplexityLimit(300))
gqlSrv.Use(extension.FixedDepthLimit(10))

// Apollo APQ (kept from NewDefaultServer; harmless and useful).
gqlSrv.Use(extension.AutomaticPersistedQuery{
    Cache: lru.New[string](100),
})
```

**Closes STAGE3-F1, STAGE3-F2, STAGE3-F3.** Bounds the cost of any single
query before execution. gqlgen rejects above-limit queries at parse time,
before any DB call.

**Parameters to discuss with the team:**
- Complexity limit: `300` starting value. Typical admin UI queries score 5-30;
  cyclic-bomb queries score 1000+. Adjustable via env var if needed.
- Depth limit: `10` starting value. The deepest legitimate UI query today
  is ~4-5 levels.
- Transports kept: POST (mandatory), MultipartForm (currently unused; safe to
  keep for future file-upload mutations). Drop both if neither is used.

#### Why drop GET transport (STAGE3-F3 detail)

gqlgen's `NewDefaultServer` installs `transport.GET` by default, which allows
queries via `GET /graphql?query=...&variables=...`. **The Apollo frontend
never uses GET** — Apollo's `HttpLink` always POSTs — so removing it is a
no-op for legitimate traffic. The exposure is:

| Where the URL leaks | Risk |
|---------------------|------|
| Reverse-proxy access logs (nginx, ALB, Cloudflare) | Query content + arguments in standard log lines |
| Browser history | Query stored across sessions |
| `Referer` header on navigation | Query sent to external sites |
| Browser/CDN cache | GET responses can be cached by default |
| Bookmarks / copy-paste / screen shares | URL visible everywhere |

For a security product where queries can carry sensitive selections (e.g.
`{me{email}}`, `{workspace{caCertPem}}`), GET-via-URL pushes data into
log/history surfaces that POST does not touch. Industry convention for
GraphQL APIs handling sensitive data is **POST only** — GitHub, Shopify,
Linear, Apollo's own production guidance.

CWE-598: Information Exposure Through Query Strings in GET Request.

### Tier 2 — Per-IP rate limit middleware (next hardening pass)

HTTP middleware tracking request rate per client IP using Go's
`golang.org/x/time/rate` token bucket.

**Two implementation paths:**

- **In-process** (single controller instance): one map[ip]*rate.Limiter, ~30 LOC
- **Redis-backed** (multi-instance): leverages the existing Valkey infrastructure
  with `github.com/ulule/limiter` + Redis store, ~50 LOC

Starting parameters:
- 10 req/s, burst 30 per IP
- 429 Too Many Requests on excess

Trusted-proxy header support (`X-Forwarded-For`) needs to be added explicitly
since the controller may run behind a reverse proxy.

### Tier 3 — Per-tenant + per-operation rate limits (pre-GA)

Different limits for different operation classes:

| Operation class | Limit |
|-----------------|-------|
| `InitiateAuth` (login start) | 10/min per IP — prevent password spray |
| `RegenerateConnectorToken` (REST too) | 5/min per connector |
| Heavy queries (`workspace.connectors`) | 30/min per tenant |
| Mutations (general) | 60/min per user |
| `lookupWorkspacesByEmail` (public) | 30/min per IP — prevent enumeration |

Implementation: parse the GraphQL `operationName` from the request body in
middleware, look up the right bucket, allow / 429.

### Tier 4 — Edge / proxy-level rate limiting (deployment-time, not code)

Cloudflare / AWS WAF / Nginx `limit_req_zone` configured at the
deployment / infra layer. Handles bulk traffic before reaching the
controller. Zero application code. Recommended for production GA.

## Alternatives Considered

### Alt A — `extension.ComplexityLimit` with custom per-field weights (rejected for v1)

gqlgen supports per-field complexity directives:
```graphql
type RemoteNetwork {
  connectors: [Connector!]! @complexity(value: 10)
}
```

**Rejected for v1**: requires schema annotation across many types. Tier 1's
flat `FixedComplexityLimit(300)` catches the most common bomb pattern with
zero schema change. Custom weights can be added later if the flat limit
proves insufficient.

### Alt B — Reject all queries with cyclic relationships (rejected)

Could disallow `Connector.remoteNetwork` etc. in the schema.

**Rejected**: cyclic relationships are a legitimate GraphQL pattern; the admin
UI legitimately needs them (e.g. resolving a connector's network from a
connector list). The fix is to limit cost, not to remove the relationship.

### Alt C — Rely on edge / WAF only, skip application-level limits (rejected)

**Rejected**: requires production infrastructure that may not always be in
place (especially in beta). Application-level limit is defense-in-depth and
works regardless of deployment topology. Edge layer SHOULD also exist in
production, but application layer must not depend on it.

### Alt D — Per-resolver execution timeout instead of complexity limit (rejected)

Set a hard timeout (e.g. 5s) on every resolver. If a query takes longer,
abort.

**Rejected**:
- Doesn't prevent the resource consumption that already happened during the 5s
- Doesn't protect downstream resources (DB connections, memory)
- Complexity check at parse-time is cheaper and rejects BEFORE any resolution

Worth adding as a SUPPLEMENT (defense in depth), but not as the primary fix.

## Consequences

### Wins from Tier 1
- Closes STAGE3-F2 immediately
- 5 lines of code, low review risk
- Catches single-bomb attacks at parse stage (cheap rejection)
- Documented and configurable

### Costs from Tier 1
- May reject legitimate queries that legitimately have high complexity
  (need to monitor and tune the `300` threshold)
- New env-var surface area if we make the limit configurable

### Wins from Tier 2-4
- Tier 2: protects against request floods
- Tier 3: per-tenant / per-operation precision
- Tier 4: protects against true DDoS

### Risks
- Tier 2 in-process limiter has memory growth concern (one entry per IP). Need
  an LRU cache or periodic cleanup of inactive IPs.
- Tier 2 needs `X-Forwarded-For` handling if controller is behind a proxy —
  must trust the right hop, not the original (spoofable) header.
- Tier 3 requires identifying the operation in middleware, which means parsing
  the GraphQL request body — adds latency and parsing cost. Mitigated by only
  parsing the operationName field, not the full query.

## Plan

### Phase 1 — Tier 1 (single restructure: F1 cleanup + F2 + F3)

1. In `controller/cmd/server/main.go`, replace `handler.NewDefaultServer(...)`
   with `handler.New(...)` + explicit transport / extension setup (see code
   shape above)
2. Add imports for `transport`, `extension`, `lru`, `ast`
3. Add explicit transports: POST + MultipartForm only (drop GET — STAGE3-F3)
4. Conditional `extension.Introspection{}` install — dev only — STAGE3-F1
   (lets us delete the `introspectionDisabler{}` shim added earlier)
5. Add `extension.FixedComplexityLimit(300)` — STAGE3-F2
6. Add `extension.FixedDepthLimit(10)` — STAGE3-F2
7. Keep `extension.AutomaticPersistedQuery` + `SetQueryCache(lru...)`
   for parity with the deprecated default
8. Verify with `go vet` + smoke test (Apollo UI still works; introspection
   blocked in non-dev; bomb query rejected with `complexity 1500 > 300`)
9. Update `07-Connector-Audit.md`: mark F1 as ✅ (already), F2 as ✅, F3 as ✅

### Phase 2 — Tier 2 (separate change)

1. Add per-IP rate limit middleware to `controller/internal/middleware/`
2. Wire it into the `/graphql` route AFTER `AuthMiddleware`
   (so we know the tenant ID for logging)
3. Use in-process token bucket for v1
4. Add `X-Forwarded-For` trusted-proxy parsing
5. Document Tier 2 completion in the audit doc

### Phase 3 — Tier 3 (pre-GA)

1. Define operation-class table (see above) in config
2. Add operation-name extraction in middleware
3. Implement per-class buckets
4. Per-tenant accounting via Valkey/Redis (not in-process — scales with
   multiple controllers)
5. Document Tier 3 completion

### Phase 4 — Tier 4 (deployment)

Not code. Document in the ops/deployment guide:
- Cloudflare rate-limit recipe
- Nginx `limit_req_zone` recipe
- AWS WAF rate-based rule recipe

## Configuration

Future env vars (none required for Tier 1's hardcoded defaults):

| Var | Purpose | Tier 1 default |
|-----|---------|----------------|
| `GRAPHQL_COMPLEXITY_LIMIT` | Per-query complexity cap | 300 |
| `GRAPHQL_DEPTH_LIMIT` | Per-query depth cap | 10 |
| `RATE_LIMIT_PER_IP_RPS` | Tier 2 — requests per second per IP | 10 (when Tier 2 ships) |
| `RATE_LIMIT_PER_IP_BURST` | Tier 2 — burst allowance | 30 (when Tier 2 ships) |

## Notes

- Closes audit findings **STAGE3-F2** AND **STAGE3-F3** in
  [[CodeStudy/07-Connector-Audit]] in a single `gqlSrv` restructure
- Folds in the cleanup of **STAGE3-F1** (introspection). The temporary
  `introspectionDisabler{}` shim added in the F1 fix becomes unnecessary
  after this restructure — extension.Introspection{} is simply not
  installed outside dev.
- After both: schema discovery blocked (F1) AND cost-bomb blocked (F2) AND
  query-in-URL leak surface eliminated (F3).
- Does NOT address **STAGE3-F5** (raw pgx errors leak via default
  ErrorPresenter) — separate concern; warrants its own follow-up.
- Does NOT address **STAGE1-F5** (`X-Public-Operation` header trust
  pattern) — also separate; the public handler ALSO needs complexity
  limits since attacker could route bombs through the public path.
  Consider whether Tier 1 limits should apply to both `protected` and
  `public` gqlSrv invocations. **TODO during implementation**: confirm
  `gqlSrv.Use(...)` applies globally to the single `gqlSrv` instance
  (which it does, since it's the same Server object referenced by both
  routes). ✅

## Open questions for team discussion

1. **Complexity limit threshold**: is 300 too restrictive? Too permissive?
   Need to instrument current admin UI traffic and see realistic scores
   before locking the number.

2. **Depth limit**: should it be 5 or 10? Realistic legitimate depth in the
   admin UI today is 4-5. 10 provides slack.

3. **Tier 2 timing**: do we want per-IP rate limiting in v1 (this PR) or
   defer to a separate hardening pass? Adds ~30 LOC + new dependency.

4. **Redis-backed vs in-process for Tier 2**: when do we expect multi-instance
   deployment? If soon, skip in-process and go directly to Valkey-backed.

5. **Per-operation limits scope (Tier 3)**: which operations are worth
   protecting specifically? `lookupWorkspacesByEmail` and `InitiateAuth` are
   the obvious candidates; others?

6. **Edge layer plan**: is there an existing reverse proxy / CDN strategy
   for production? If yes, can defer some application-level work.
