---
type: phase
member: M1
sprint: 17
phase: 12
title: OIDC ↔ SCIM Canonical-Key Equivalence (Finding C, second half)
status: done
depends_on: [10, 11]
tags: [go, identity, scim, oidc, mapping-gate, adr-025, equivalence]
---

# Phase 12 — OIDC ↔ SCIM Canonical-Key Equivalence

## Relationship to prior phases (honesty boundary)

- **Phase 10** proved the **SCIM-side** mapping round-trip:
  `scimIdentifier → Provision → storage → Get → same canonical value returned`.
  It deliberately did **not** prove the OIDC side (its own doc says so).

- **Phase 11** made the **OIDC-side** `subjectClaim` operational in the real
  login path: `conn.SubjectClaim → ProviderFor → OIDC provider →
  ExtractSubjectClaim → AuthenticationContext.Subject → resolver.Resolve`.
  Before Phase 11, `ExtractSubjectClaim` was dead code in production.

- **Phase 12** (this phase) proves the **cross-equivalence**: for the same
  logical person, the OIDC-derived canonical key and the SCIM-derived canonical
  key resolve to the **same Canonical Identity Key** under the **same
  connection identity namespace**.

Phase 12 is a **follow-up to Phase 10 Finding C**, enabled by Phase 11. It is
**not** a correction of Phase 10 (Phase 10 met its original SCIM-side scope)
and **not** a re-do of Phase 11.

## What this phase does NOT do (explicit)

Phase 12 does **NOT** perform a live OIDC login, JWT validation, IdP
authentication/authorization, or session creation. It does not contact an
external IdP, exchange an authorization code, or bypass OIDC security
validation (which remains authoritative in `internal/auth/providers/oidc.go`
for real logins only).

The proof is:

    auth/mapping.ExtractSubjectClaim(probeClaims, conn.SubjectClaim)
        ==
    SCIM-derived canonical key (from the real Provision → Get path)

for the **same** synthetic probe person, computed with the **same production
extractor** the login path uses.

## Source-verified architecture

Both identity derivations resolve the **same** `identity_connections` row and
write/read `external_identities` under the same `(tenant_id, connection_id)`:

- OIDC: `callback.go:64` `conn = idpStore.GetByID(pkce.ConnectionID)` →
  `:79` `providerForFn(conn, …)` → `idp_adapter.go:64`
  `p.SetSubjectClaim(conn.SubjectClaim)` → `oidc.go` Authenticate derives
  `Subject = auth/mapping.ExtractSubjectClaim(rawClaims, conn.SubjectClaim)` →
  `callback.go:97` `identitySvc.Authenticate(ctx, authCtx, conn.ID, …)` →
  `service.go:62` `resolver.Resolve(ctx, conn.ID, authCtx.Subject, "")`.
- SCIM: `directory_service.go:97-128` `resolveScope(connID)` → `GetByID(connID)`
  → `scope{connectionID: connID}`; `:161` `resolver.Resolve(ctx, sc.connectionID,
  key, …)`; `provisioner.go:47` writes
  `external_identities(connection_id=connID, subject=key)`.

The `Resolver` keys 1:1 on `(connection_id, subject)` (`resolver.go:28-49`),
so **equal canonical keys under the same connection ⇒ the same
`external_identities` row ⇒ the same logical user** — exactly ADR-025 §3.1's
"converge on the same logical user / row". The equivalence assertion is
therefore honest and sufficient: no larger architectural change is required.

## Implementation (approved scope)

- `controller/internal/scim/mapping_probe.go`
  - import `internal/auth/mapping` (leaf package; no import cycle).
  - Build synthetic OIDC claims for the **same** probe person:
    `probeClaims` carries `sub`, `email`, `oid`, and — when the connection
    configures a non-default `subjectClaim` — that configured claim set to the
    same `probeExternalID`.
  - After the SCIM `Provision → Get` round-trip succeeds, compute
    `oidcKey := authmapping.ExtractSubjectClaim(probeClaims, conn.SubjectClaim)`.
  - Fail closed if `oidcKey == ""` or `oidcKey != probeExternalID`; otherwise
    report `Verified: true` with a Reason stating the two extractors resolve to
    the same Canonical Identity Key for the probe user (no live login
    performed).
  - Cleanup `defer Deprovision` unchanged; cleanup failure stays best-effort.

- `controller/internal/scim/mapping_probe_test.go`
  - Invert the `containsSubjectClaimProvenClaim` guard so it forbids claiming a
    **live OIDC login** was performed, while allowing the legitimate
    canonical-key equivalence claim.
  - Add T1 (matching subjectClaim="email"/scimIdentifier="externalId" → proven),
    T2 (configured claim absent → unproven), T3 (configured claim empty →
    unproven), T4 (subjectClaim="" defaults to "sub" → proven).
  - Existing tests (SCIM round-trip failure, cleanup, token safety, C1
    enablement, break-glass, OIDC subjectClaim behavior) remain intact.

- `controller/internal/scim/validation.go`
  - `WithRoundTrip` Reason wording updated (was factually outdated: "subjectClaim
    is an OIDC login-side contract not exercised by this probe"). `MappingGate`
    / `MappingState` shape unchanged.

- Phase 10 limitation note: a one-line clarification appended stating Phase 12
  subsequently resolves that limitation. Phase 10 is **not** rewritten as
  incomplete.

## Safety invariants preserved

- No `Store.Mint` / `Store.Revoke`; no `scim_tokens` create/modify/rotate.
- No SCIM auto-enable; `TestIdpConnection`'s C1 `SetSCIMEnabled(..., false)`
  (`idp.resolvers.go:270`) untouched.
- `UpdateScimConfig`, break-glass, `revocation.go`, `directory_service.go`
  Deprovision, outbox/PENDING-13, frontend, migrations: untouched.
- No second `ExtractSubjectClaim` implementation — reuses `auth/mapping`.

## Verification gates

    cd controller
    go build ./...
    go vet ./...
    go test ./internal/scim/...
    go test ./internal/auth/...
    go test ./internal/identity/...
    go test ./graph/...

`git diff --name-only` must contain only the approved Phase 12 files plus the
Phase 10 clarification. No commit.

## Remaining limitation

After Phase 12, ADR-025 §3.1's cross-equivalence is **proven at the mapping/
canonical-key layer** using the real production extractors. This is a
configuration proof (the admin configured the two extractors to converge), not
a live end-to-end OIDC↔SCIM login integration against a real IdP — which
ADR-025 §3.1 itself lists only as a fallback "safe read-only validation" and
explicitly says the probe avoids. The invariant "for the same person both
resolve to the same value" is now actively verified by `testIdpConnection`.
