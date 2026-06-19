---
type: code-study
flow: connector-flow-audit
created: 2026-06-13
status: in-progress
related:
  - "[[CodeStudy/04-Connector-Enrollment-Flow]]"
  - "[[Decisions/ADR-008-Drop-InstallCommand-From-Mutation]]"
---

# Code Study 07 — Connector Flow Security Audit

> Findings from the connector flow audit. New connector-flow findings get
> appended here. Different flows get their own doc.

---

# Stage 1 — Admin opens /connectors, clicks Add Connector

## Findings

### STAGE1-F4 — Enrollment JWT cached in Apollo `InMemoryCache` (🟠 High)

The `GenerateConnectorToken` GraphQL mutation returns `installCommand` in its
response. Apollo's `InMemoryCache` caches the result. The `installCommand`
contains the full enrollment JWT (24-hour validity). Frontend never reads
this field — the actual display fetches the command via the REST endpoint
`POST /api/connectors/{id}/token` (`RegenerateTokenHandler` mints fresh
tokens on demand).

**Net effect**: dead field carries a 24-hour-valid credential into a
session-long cache. Any XSS bug elsewhere in the SPA → `apolloClient.cache.extract()`
→ token leaked → attacker enrolls a connector.

**Fix decision**: documented in [[Decisions/ADR-008-Drop-InstallCommand-From-Mutation]].
Deferred — apply later.

---

# Stage 3 — Middleware → gqlgen → Resolver

## Findings

### STAGE3-F1 — GraphQL Introspection enabled in production (🔴 Critical)

[main.go:126](controller/cmd/server/main.go#L126):
```go
gqlSrv := handler.NewDefaultServer(...)
```

gqlgen's `NewDefaultServer` enables the `extension.Introspection` extension
by default. Any caller reaching `/graphql` can submit `{ __schema { ... } }`
and receive the entire schema: every mutation, query, type, argument, hidden
field — the complete API surface in machine-readable form.

**CWE-200 — Information Exposure**
**OWASP API Security Top 10 — API10:2023**

**Why it's Critical in this codebase** (not just Medium in isolation):

- **Combined with STAGE1-F5** (`X-Public-Operation` header bypass): an
  unauthenticated caller can introspect by setting
  `X-Public-Operation: InitiateAuth` and putting the introspection query
  in the body. The router sees the header, routes to the public handler
  (no auth middleware), and gqlgen processes the introspection request.
- **Combined with STAGE3-F2** (no complexity limit): an attacker can use
  introspection to find expensive resolver chains, then submit them for DoS.
- **Combined with STAGE3-F5** (raw pgx errors leaked): introspection
  reveals argument types; crafted invalid values trigger DB errors that leak
  constraint/table names — mapping the entire database schema.

**Why this matters for a ZTNA product**: ZTNA's value proposition is
attack-surface reduction. Exposing the control-plane API contradicts that.
Standard pentest finding ($250-$1500 bug bounty range at peer companies);
fails security questionnaires (SOC 2, ISO 27001 vendor reviews).

**Fix applied (2026-06-13)**: added `introspectionDisabler` extension in
[main.go:158-160](controller/cmd/server/main.go#L158-L160). It re-sets
`OperationContext.DisableIntrospection = true` after gqlgen's
`extension.Introspection{}` runs, restoring the off-by-default behavior.
Skipped when `ENV=development` so local tooling (Apollo Studio, GraphiQL)
still works. Type definition at
[main.go:347-360](controller/cmd/server/main.go#L347-L360).

**Status**: ✅ Fixed.

> Note: when ADR-009 Tier 1 lands, this shim will be replaced by a cleaner
> conditional install (skip `extension.Introspection{}` entirely outside dev,
> rather than install+disable). Same security outcome, less code.

---

### STAGE3-F2 — No GraphQL query complexity/depth limit (🟠 High)

[main.go:128](controller/cmd/server/main.go#L128) — `handler.NewDefaultServer`
does not configure any complexity or depth limit. The schema's cyclic
relationships (`Connector.remoteNetwork ↔ RemoteNetwork.connectors`,
`Resource.shield ↔ Shield.remoteNetwork`) allow an attacker to construct a
single query whose cost grows exponentially with nesting. With no limit,
the GraphQL executor accepts these and stalls Postgres on JOINs / N+1
fetches.

**CWE-770 — Allocation of Resources Without Limits or Throttling**
**OWASP API Security Top 10 — API4:2023**

Combined with **STAGE3-F1** (introspection — now fixed) and **STAGE3-F5**
(raw pgx error leaks), an attacker could enumerate the schema, find the
deepest cyclic chain, and submit a bomb that hangs the DB.

**Fix decision**: documented in [[Decisions/ADR-009-GraphQL-DoS-Hardening]].
4-tier layered approach (complexity → rate-limit → per-tenant → edge).
Tier 1 (complexity + depth + transport hardening) is a single `gqlSrv`
restructure — ~15 LOC — pending team discussion.

**Status**: 📝 Documented in ADR-009, awaiting team discussion before fix.

---

### STAGE3-F3 — GraphQL GET transport enabled (🟠 High)

[main.go:128](controller/cmd/server/main.go#L128) — `NewDefaultServer`
installs `transport.GET` by default, allowing GraphQL queries via
`GET /graphql?query=...&variables=...`. The Apollo frontend never uses GET,
but the endpoint accepts manual GET requests. Query content (including
sensitive arguments like emails or IDs) then ends up in:

- Reverse-proxy access logs (nginx / ALB / Cloudflare standard log line)
- Browser history
- `Referer` header on outbound navigation
- Browser / CDN cache
- Bookmarks / screen shares / copy-as-cURL exports

**CWE-598 — Information Exposure Through Query Strings in GET Request**

Industry convention for security-sensitive GraphQL APIs is **POST only**
(GitHub, Shopify, Linear, Apollo's own production guidance).

**Fix decision**: bundled into [[Decisions/ADR-009-GraphQL-DoS-Hardening]]
Tier 1 — same `gqlSrv` restructure that adds complexity limits also drops
the GET transport. Folding the two fixes together keeps the diff small
and review burden low.

**Status**: 📝 Documented in ADR-009 (Tier 1), awaiting team discussion.
