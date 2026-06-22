---
type: decision
status: accepted
date: 2026-06-13
related:
  - "[[CodeStudy/04-Connector-Enrollment-Flow]]"
  - "[[Decisions/ADR-005-Email-Normalization]]"
  - "[[Decisions/ADR-006-Refresh-Token-Rotation]]"
  - "[[Decisions/ADR-007-JWT-Secret-Minimum-Length]]"
tags:
  - adr
  - controller
  - frontend
  - graphql
  - apollo
  - connector
  - shield
  - security
  - data-hygiene
---

# ADR-008 — Drop `installCommand` From `Generate{Connector,Shield}Token` Mutation Response

## Context

During the Stage 1 audit of the connector-enrollment flow
(see [[CodeStudy/04-Connector-Enrollment-Flow]]), an independent reviewer
surfaced that the `GenerateConnectorToken` GraphQL mutation returns
`installCommand` in its response. The `installCommand` string is the full
one-liner including the enrollment JWT:

```
curl -fsSL ... | sudo CONTROLLER_ADDR=... ENROLLMENT_TOKEN=<24h-JWT> bash
```

Apollo Client's `InMemoryCache` automatically caches every mutation result. So
after the admin closes the "Add Connector" modal, the entire `installCommand`
sits in the Apollo cache for the duration of the session — accessible via
`apolloClient.cache.extract()`.

### Why this is a real (not theoretical) finding

It's a **credential-in-unexpected-cache violation**:

1. The enrollment JWT grants **one-time connector enrollment**.
2. The JWT is valid for **24 hours**.
3. Any XSS bug anywhere else in the SPA → attacker's JS runs in same origin →
   `apolloClient.cache.extract()` returns `{ generateConnectorToken: { installCommand: "..." } }`
   → token extracted → attacker enrolls an attacker-controlled connector
   into the workspace.

The fix is independent of whether an XSS bug currently exists — sensitive
credentials should not linger in long-lived client caches, regardless.

### Why the field is unused by the frontend

[`admin/src/components/InstallCommandModal.tsx:99-112`](admin/src/components/InstallCommandModal.tsx#L99-L112):

```tsx
const result = await generateConnectorToken({...})
const connectorId = result.data?.generateConnectorToken.connectorId   // ← only field read
handleClose()
if (connectorId) navigate(`/connectors/${connectorId}`)
```

`result.data?.generateConnectorToken.installCommand` is never read anywhere
in the frontend. It is dead data on the wire.

### How the install command is actually displayed today

The `ConnectorDetail` page fetches the install command via a separate REST
endpoint on mount when the connector is in `pending` state:

[`admin/src/pages/ConnectorDetail.tsx:152-173`](admin/src/pages/ConnectorDetail.tsx#L152-L173):

```tsx
const response = await fetch(`/api/connectors/${connectorId}/token`, {
  method: 'POST',
  ...
})
setInstallCommand(result.install_command)  // ← THIS is what displays
```

This REST endpoint, handled by [`RegenerateTokenHandler`](controller/internal/connector/token_handler.go),
**mints a fresh enrollment token on every call**. So:

- Admin creates connector → mutation runs → DB row created with status `pending`
- Detail page mounts → REST call → fresh token #1
- Admin logs out / browser closed
- Admin logs back in later → detail page mounts → REST call → fresh token #2
- Admin runs install command (token #2) → connector enrolls

The state that needs to persist across sessions (the `connectors` DB row, the
`enrollment_token_jti` linkage, the workspace, etc.) all lives **server-side**.
The frontend never needs to remember the token — it asks the server for a
fresh one whenever the detail page is viewed.

The mutation field was therefore **superfluous from day one** — the REST
endpoint always was the source of truth, and the design supports
log-out / log-in cycles inherently.

### Shield variant has the same issue

[`mutations.graphql:38-43`](admin/src/graphql/mutations.graphql#L38-L43) has
the equivalent dead `installCommand` field on `GenerateShieldToken`.
[`admin/src/pages/ShieldDetail.tsx`](admin/src/pages/ShieldDetail.tsx) ignores
it and fetches via REST `/api/shields/{id}/token` for the same reasons.

## Decision

**Remove `installCommand` from both mutations' response shapes**:

```diff
mutation GenerateConnectorToken($remoteNetworkId: ID!, $connectorName: String!) {
  generateConnectorToken(remoteNetworkId: $remoteNetworkId, connectorName: $connectorName) {
    connectorId
-   installCommand
  }
}

mutation GenerateShieldToken(...) {
  generateShieldToken(...) {
    shieldId
-   installCommand
  }
}
```

After the fix:
- The token is delivered ONLY via the REST endpoint (`POST /api/{connectors,shields}/{id}/token`).
- The mutation returns only the identifier the frontend uses for navigation.
- Apollo cache never holds the token.
- React state in `ConnectorDetail` / `ShieldDetail` holds the token only while
  the page is mounted; component unmount drops it.

The server-side resolver should be updated to no longer populate the field
(or the field can be removed from the schema entirely after the frontend
codegen settles).

## Alternatives Considered

### Alt A — Evict the cache entry after display (rejected)

After the modal closes, run:
```ts
apolloClient.cache.evict({ id: 'ROOT_MUTATION', fieldName: 'generateConnectorToken' })
apolloClient.cache.gc()
```

**Rejected because:**
- Brittle: depends on Apollo's internal cache key format, breaks if Apollo
  internals change.
- Easy to forget on future call sites.
- Doesn't fix the underlying issue (sending the token over GraphQL when it
  doesn't need to be there).

### Alt B — `fetchPolicy: 'no-cache'` on the mutation (rejected)

Pass per-call:
```ts
useMutation(GenerateConnectorTokenDocument, { fetchPolicy: 'no-cache' })
```

**Rejected because:**
- Same fragility as Alt A — every call site must remember.
- The dead field still goes over the wire, just not cached.
- Doesn't reduce the GraphQL response payload.

### Alt C — Keep the field, document the risk (rejected)

**Rejected because:**
- Knowingly shipping a 24h-credential in a long-lived client cache when the
  fix is trivial fails any reasonable code-review bar.

## Consequences

### Wins
- Apollo cache no longer holds the enrollment JWT.
- Mutation response payload is smaller (no curl one-liner with ~600-byte JWT).
- The data model becomes coherent: mutations return identifiers, REST handles
  on-demand credential issuance.
- Defense-in-depth: even if XSS exists elsewhere, the token isn't reachable
  through Apollo's cache surface.

### Costs
- 2-line schema change.
- Run `npm run codegen` to regenerate TypeScript types.
- Trivial backend resolver tweak (or leave the field populated and have
  frontend just not query it — equivalent at the wire level for legacy callers).
- No UX change. The "Add Connector" → detail page → copy command flow is
  unchanged because the detail page was always the display point.

### Risks
- Any other consumer of these mutations that DID rely on `installCommand`
  would break. Grep confirms there are no other consumers — the field is dead.

## Plan

### Phase 1 — Frontend
1. Remove `installCommand` from the mutation selection sets in
   [`admin/src/graphql/mutations.graphql`](admin/src/graphql/mutations.graphql)
   (both connector + shield).
2. `npm run codegen` to regenerate types.
3. Verify `tsc` is green — no callers referenced the field.

### Phase 2 — Backend (optional, defense-in-depth)
1. Remove `installCommand` from `ConnectorToken` and `ShieldToken` GraphQL
   types in `controller/graph/*.graphqls`.
2. Remove population in `connector.resolvers.go` and `shield.resolvers.go`.
3. `make gqlgen` to regenerate.

Phase 2 is optional. Phase 1 alone closes the Apollo-cache exposure since the
frontend stops requesting the field. Phase 2 prevents anyone from
re-introducing the request via another client.

### Phase 3 — Verification
- `tsc` green
- `go build` / `go vet` green
- Manual: create connector, check Apollo devtools cache extract — confirms
  `installCommand` is no longer present.

## Notes

- This ADR closes Stage 1 audit finding **STAGE1-F4** from the connector flow
  audit.
- The reviewer also flagged **STAGE1-F5** (`X-Public-Operation` header trust
  pattern as a class concern) — that is a separate cross-cutting issue and
  warrants its own ADR (proposed: ADR-009).
- **Implemented 2026-06-19** on branch `fix/lazy-enrollment-token-mint` — see the Implementation Note below.

### Related observation (out of scope for this ADR)

`RegenerateTokenHandler` ([`token_handler.go:84-99`](controller/internal/connector/token_handler.go#L84-L99))
generates a NEW token and UPDATEs `connectors.enrollment_token_jti` to the
new JTI — but the OLD JTI is still in Redis under its old key with its
original TTL. Multiple valid enrollment tokens can therefore be in flight
per pending connector during regeneration windows. That's a Stage 8/9
concern and not in scope for ADR-008.

## Implementation Note (2026-06-19)

> Added during implementation by a follow-up reviewer. **Confirm with the ADR
> author (Yogesh) that this scope extension is agreed.**

The decision as originally written (drop the field from the response) closes the
**Apollo-cache exposure (P1)** but does **not** stop the wasteful mint: the create
resolver builds `installCommand` in its body, so it still signs a JWT and writes
Redis (`enrollment:jti:*`) on every create regardless of what the client selects.
Removing the field from the selection/struct/schema only stops *returning* the
token, not *creating* it.

So implementation extended the scope to also make minting **lazy** (call this
**S2**), which is what actually closes the double-mint / orphaned-token (P2):

- `GenerateConnectorToken` / `GenerateShieldToken` now only validate + reserve the
  `pending` row and return the ID. The slug lookup, CA-fingerprint,
  `GenerateEnrollmentToken`, `StoreEnrollmentJTI`, and `UPDATE … enrollment_token_jti`
  were removed from the create path (for shields, the `ShieldSvc.GenerateShieldToken`
  call was removed; the placeholder-connector INSERT stays to satisfy the FK).
- `installCommand` removed from both mutations + the `ConnectorToken` / `ShieldToken`
  GraphQL types (the original S1 decision).
- **All** issuance now flows through the existing REST endpoints
  (`POST /api/{connectors,shields}/{id}/token`) on detail-page load — unchanged.

Safe because `enrollment_token_jti` is nullable and enrollment burns the jti carried
in the *presented JWT* via Redis (`enrollment.go`), not from the row column.

The "Related observation" above (re-mint orphans the prior jti → multiple valid
tokens) remains **out of scope** and is tracked as a separate follow-up (S3).
