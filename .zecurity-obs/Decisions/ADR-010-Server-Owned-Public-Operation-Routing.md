---
type: decision
status: accepted
date: 2026-06-19
related:
  - "[[CodeStudy/04-Connector-Enrollment-Flow]]"
  - "[[Decisions/ADR-008-Drop-InstallCommand-From-Mutation]]"
  - "[[Decisions/ADR-009-GraphQL-DoS-Hardening]]"
tags:
  - adr
  - controller
  - frontend
  - graphql
  - auth
  - security
---

# ADR-010 — Server-Owned GraphQL Public-Operation Routing

> Resolves connector-flow audit finding **STAGE1-F5** (the `X-Public-Operation`
> header trust pattern). Coordinate with the STAGE1-F5 author (Yogesh) — this is
> his finding; this ADR records the implemented resolution.

## Context

`/graphql` decided whether a request could bypass the auth middleware based on a
**client-supplied header**, `X-Public-Operation`, cross-checked against a server
`publicOperations` map. The frontend maintained a parallel `PUBLIC_OPERATIONS`
list (`admin/src/apollo/links/auth.ts`). Two problems:

- **Drift** — two hand-maintained allowlists kept in sync only by a comment.
- **Header/body mismatch class** — the routing header is a separate channel from
  the operation actually executed; the header value need not match the body.

The routing decision was effectively *client-controlled*.

## Decision

The **server** owns the public/protected decision, derived solely from the parsed
request body. Remove `X-Public-Operation` and the frontend `PUBLIC_OPERATIONS`
list entirely.

`routeGraphQL` parses the GraphQL document and routes a request to the public
(no-auth) handler **only if** it is a single query/mutation whose **every root
selection is a plain field in the server allowlist `publicRootFields`**:

```
publicRootFields = { initiateAuth, lookupWorkspace, lookupWorkspacesByEmail }
```

Everything else is **fail-closed to protected**.

### Why root fields, not operation names

Two server-parsed designs were compared:

- **(A) operation-name allowlist** — match the parsed operation's *name*.
- **(B) root-field allowlist** — match the *fields the operation selects*. **Chosen.**

The security delta is a wash: in both designs `@hasRole` (with the deny-by-default
`schema_authz_test.go` guard) is the primary control that actually prevents an
unauthenticated protected-field execution. B does **not** close an exploit A
leaves open.

The deciding factor is **ownership and stability**, not security: operation names
are client-chosen labels that change during frontend refactors/codegen; **schema
field names are the server-executed contract**. Anchoring routing to the schema
removes the residual coupling that an operation-name allowlist would keep alive
(a frontend rename silently 401-ing a public page). The extra logic is minimal
and fail-closed, so robustness wins for near-zero cost.

### Constraints honored

- No soft-auth middleware; no anonymous tenant contexts.
- Protected operations still hit the auth middleware before resolvers.
- `@hasRole` authorization semantics unchanged — routing only gates the auth
  *middleware*, not authorization.

## Fail-closed edge cases (all route protected)

Non-POST (GET, OPTIONS, websocket upgrade), non-JSON (multipart), batch arrays
(gqlgen rejects them anyway), APQ hash-only requests (empty `query` at routing
time — APQ is enabled by `NewDefaultServer` but unused by our client), multiple
operations in one document, subscriptions, fragment spreads / inline fragments at
the root, unknown/protected root fields, and parse errors.

Matching is on the field `.Name`, never the alias, so an aliased protected field
cannot masquerade as public. Multiple root fields require **all** to be public.

### Introspection (explicit)

**Unauthenticated introspection is blocked by this routing.** An introspection
query's root field is `__schema` or `__type`, which are not in `publicRootFields`,
so the request routes to the protected handler and requires a valid JWT. This is
unchanged from the prior header-based routing (introspection was never a public
operation) — it is now simply derived by parsing instead of by header-absence.

This is independent of, and complementary to, the `introspectionDisabler`
extension: outside `ENV=development` introspection is disabled for *everyone*
(authenticated or not); in development it is enabled but still **auth-required**
by this routing (e.g. the dev-only `/playground` needs a token).

**No tooling depends on unauthenticated introspection.** Frontend
`graphql-codegen` (`admin/codegen.yml`) and Go `gqlgen` both read the `.graphqls`
schema files directly, not HTTP introspection, so codegen needs no running server
or token.

## Security review

- No client-controlled routing channel remains; the decision comes from the same
  parsed document gqlgen executes.
- Default-deny everywhere → public surface is **≤** the prior behavior and cannot
  grow accidentally.
- `@hasRole` + the Stage 1 guard test remain the primary authz control; this
  change makes routing self-sufficient (it no longer depends on `@hasRole`
  coverage to be safe) and shrinks the unauthenticated surface that reaches
  gqlgen.
- **Out of scope (non-bypass):** APQ stays enabled but unused (disabling it is
  ADR-009 territory); the router now parses each POST query in addition to gqlgen
  (double parse) — a body-size cap / token-limited parse is DoS hardening for
  ADR-009, not an auth concern.

## Consequences

- Single source of truth (`publicRootFields`), keyed on the schema contract.
- No client header, no duplicated allowlist, no header/body mismatch class.
- Frontend `authLink` simplifies to "attach Bearer if present."

## Implementation

- `controller/cmd/server/main.go` — `publicRootFields` + `routeGraphQL` /
  `isPublicGraphQLRequest` / `requestSelectsOnlyPublicFields` (pure predicate).
- `controller/cmd/server/route_test.go` — table-driven tests incl. name-spoof,
  smuggle, alias, multi-op, subscription, APQ, batch, introspection.
- `admin/src/apollo/links/auth.ts` — removed `PUBLIC_OPERATIONS` + the header.

Implemented 2026-06-19 on branch `fix/lazy-enrollment-token-mint` (Stage-2 batch).
