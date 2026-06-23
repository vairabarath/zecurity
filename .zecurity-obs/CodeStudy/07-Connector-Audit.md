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

### STAGE3-F4 — WorkspaceGuard error message leaks workspace status (🟡 Low)

[middleware/workspace.go:44-46](controller/internal/middleware/workspace.go#L44-L46)
returned `fmt.Sprintf("workspace not active: %s", status)` in the 403 response
body. This leaked the workspace's lifecycle state
(`'provisioning'` / `'suspended'` / `'deleted'`) to any caller, enabling
mild workspace enumeration / state recon.

**CWE-209 — Information Exposure Through Error Messages**

**Fix applied (2026-06-13)**: in
[workspace.go:45-52](controller/internal/middleware/workspace.go#L45-L52),
the specific status is now `log.Printf`'d server-side for ops debugging, and
the response body returns a generic `"workspace inactive"`. Standard
"log specific, return generic" pattern. Added `log` import.

**Status**: ✅ Fixed.

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

---

### STAGE3-F5 — Raw pgx errors leak via default GraphQL ErrorPresenter (🟡 Medium)

`handler.NewDefaultServer` uses gqlgen's default `ErrorPresenter`, which
returns `err.Error()` verbatim in the GraphQL response body. The codebase's
standard `fmt.Errorf("...: %w", err)` wrapping pattern then flows raw pgx
errors all the way to the client.

Example response when an admin creates a connector with a name that
collides with an existing one ([connector.resolvers.go:88-92](controller/graph/resolvers/connector.resolvers.go#L88-L92)):

```json
{
  "errors": [{
    "message": "generate connector token: insert connector: ERROR: duplicate key value violates unique constraint \"connectors_name_key\" (SQLSTATE 23505)"
  }]
}
```

**Reveals**: table name (`connectors`), constraint name
(`connectors_name_key`), column grouping, SQLSTATE codes, internal call-path
breadcrumbs (`"insert connector"`, `"load intermediate CA"`, `"store jti"`).

**CWE-209 — Information Exposure Through Error Messages**

**Scope is global**: every resolver in the codebase uses
`fmt.Errorf("%w")` for its error returns. After STAGE3-F1 (introspection)
was fixed, this is the **next-easiest schema-discovery vector** for a
pentester — craft invalid inputs, trigger DB errors, map the schema
field-by-field from the leaked constraint/table names.

Mirrors the HTTP-middleware pattern already adopted at
[workspace.go:45-52](controller/internal/middleware/workspace.go#L45-L52)
for STAGE3-F4.

**Fix decision**: bundled into [[Decisions/ADR-009-GraphQL-DoS-Hardening]]
Tier 1 — the same `gqlSrv` restructure also installs a global
`SetErrorPresenter` that sanitizes wrapped errors. Once installed, ALL
current and future resolvers benefit — no per-resolver discipline required.
Incremental follow-up: refactor select resolver errors to structured
`*gqlerror.Error` so user-meaningful messages (NOT_FOUND etc.) still pass
through.

**Status**: 📝 Documented in ADR-009 (Tier 1), awaiting team discussion.
