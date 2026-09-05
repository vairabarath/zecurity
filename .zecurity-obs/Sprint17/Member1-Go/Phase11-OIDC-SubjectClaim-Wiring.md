---
type: phase
member: M1
sprint: 17
phase: 11
title: OIDC subjectClaim Wiring (login-path half of ADR-025 §3.1)
status: done
depends_on: [10]
tags: [go, identity, auth, oidc, mapping-gate, hardening]
---

# Phase 11 — OIDC `subjectClaim` Wiring

> Depends on Phase 10 (mapping round-trip probe). This phase closes the
> **login-path** half of ADR-025 §3.1: make the configured IdP
> `subjectClaim` actually determine `AuthenticationContext.Subject` during
> OIDC login, instead of the hardcoded raw OIDC `sub`. Phase 10 already proved
> the SCIM-side (`scimIdentifier` round-trip). This phase makes the two sides
> *symmetric in code*, but does **not** by itself prove the two resolve to the
> same logical identity for the same person — see "Remaining limitation".

## Why this was needed

Phase 10 (and the pre-Phase-10 review) found `scim.ExtractSubjectClaim`
(`internal/scim/mapping.go`) had **no production caller**: the OIDC login path
hardcoded `AuthenticationContext.Subject = claims.Subject` in
`internal/auth/providers/oidc.go`, and `internal/auth/idp_adapter.go` built the
OIDC provider **without** passing `conn.SubjectClaim`. So
`idp.Connection.SubjectClaim` was stored/configured but inert — the
ADR-025 §3.1 mapping equivalence could never be verified end to end.

## Design (read before changing)

**No import cycle.** `internal/auth/providers` is a declared leaf package (it
must not import `internal/auth` or anything above it). `internal/scim` is
heavy (imports identity/idp/policy/permission/audit/outbox) and does not import
`providers`/`auth`. `providers → scim` would create a cycle
(`scim → identity → providers → scim`), so `providers` cannot call
`scim.ExtractSubjectClaim` directly.

Resolution: the canonical extractor moved into a **new leaf package**
`internal/auth/mapping` (`package mapping`), which imports **nothing** from
`auth`, `scim`, `identity`, `idp`, or `providers`. Both `providers` and `scim`
import it:

- `providers/oidc.go` → `mapping.ExtractSubjectClaim(rawClaims, subjectClaim)`
  to derive `AuthenticationContext.Subject`.
- `scim/mapping.go` → `ExtractSubjectClaim` is now a **1-line delegate** to
  `mapping.ExtractSubjectClaim`. The `scim` symbol, its tests, and all other
  `scim` mapping helpers (`ExtractScimIdentifier`, `ValidateMappingConfig`,
  `ResolveMapping`, `DefaultScimIdentifier`) are unchanged. This is the
  minimum SCIM touch required to keep a single canonical implementation
  (no second competing extractor).

The `subjectClaim` is passed from the connection to the provider via a new
post-construction setter `OIDCProvider.SetSubjectClaim(claim)`, set in
`idp_adapter.go:ProviderFor`. The `NewOIDCProvider` constructor signature is
**unchanged**, so the discovery-only caller (`TestIdpConnection`'s
`OIDCProvider.Probe` at `idp.resolvers.go:221`) is untouched and keeps
`subjectClaim == ""` (legacy `sub`).

## Security (authoritative validation preserved)

The existing typed `oidcClaims` parse in `verify()` remains the **only**
security gate: signature/JWKS, issuer, audience, expiry, not-before, nonce,
and `email_verified` are all enforced there exactly as before. After the token
passes that gate, `verify()` also produces a **raw claims map** via
`jwt.ParseUnverified` — an *additional representation of the same already
validated token*, used only so a configured non-default claim (e.g. `email`,
`oid`) can be read. The raw map is **never** consulted for issuer/aud/exp/nbf/
nonce/email_verified; a parse failure there is non-fatal (custom claims simply
unavailable, and subject derivation fails closed if a non-default claim was
needed). A custom subject claim therefore **cannot** bypass any existing
validation.

## Fail-closed semantics (preserved exactly)

`mapping.ExtractSubjectClaim`:
- empty `claimName` → default `"sub"` (legacy behavior; raw `sub` unchanged).
- explicit `claimName` present in claims → that value.
- explicit `claimName` **missing/empty** in claims → `""` → `Authenticate`
  returns an error and **does not** fall back to `sub`.

`oidc.go` additionally keeps the prior baseline guard (`claims.Subject == ""`
rejected) so a malformed token with no `sub` is still refused.

## Canonical identity trace

`idp_adapter.ProviderFor(conn)` sets the claim →
`providers.OIDCProvider.Authenticate` derives `Subject` from the verified
claims via the configured claim → `AuthenticationContext.Subject` →
`auth/callback.go:97` `identitySvc.Authenticate(ctx, authCtx, ...)` →
`internal/identity/service.go:62` `s.resolver.Resolve(ctx, connectionID,
authCtx.Subject, "")` — the subject is the identity anchor threaded into
resolution/provisioning. Confirmed by the new
`TestAuthenticate_SubjectIsCanonicalAnchor` (identity package), which asserts
the resolver receives exactly the derived subject, never raw `sub`.

## Files

| File | Change |
| --- | --- |
| `controller/internal/auth/mapping/mapping.go` | **New leaf package.** `asString` (exported as `AsString`), `DefaultSubjectClaim`, `ExtractSubjectClaim` — the canonical, single implementation. |
| `controller/internal/auth/mapping/mapping_test.go` | **New.** Canonical extraction behavior + fail-closed guarantee. |
| `controller/internal/auth/providers/oidc.go` | `subjectClaim` field + `SetSubjectClaim`; `verify()` returns raw claims map (validated token only); `Authenticate` derives `Subject` via `mapping.ExtractSubjectClaim` and fails closed on `""`. Security validation untouched. |
| `controller/internal/auth/providers/oidc_test.go` | `newTestOIDCWithClaim` helper + tests: default=`sub`, configured claim, missing-claim fails closed, empty-claim fails closed, no silent `sub` fallback, custom (`oid`) claim. |
| `controller/internal/auth/idp_adapter.go` | `ProviderFor` calls `p.SetSubjectClaim(conn.SubjectClaim)`. |
| `controller/internal/scim/mapping.go` | `ExtractSubjectClaim` becomes a thin delegate to `mapping.ExtractSubjectClaim` (import `internal/auth/mapping`). No other SCIM behavior changed. |
| `controller/internal/identity/service_test.go` | `TestAuthenticate_SubjectIsCanonicalAnchor`: traces the derived subject through the existing identity resolution path. |

## Not changed (by design)

`mapping_probe.go`, `TestIdpConnection`, `idp.resolvers.go`, `MappingGate`/
`WithRoundTrip`, SCIM enablement semantics, `Store.Mint`/token rotation,
`DirectoryService.Deprovision`, `revocation.go`, `directory_service.go`,
outbox/PENDING-13, frontend, migrations. SCIM is **not** auto-enabled by this
task.

## Build gate

`go build ./...`, `go vet ./...` clean.
`go test ./internal/auth/... ./internal/scim/... ./internal/identity/...
./graph/...` green. (SCIM integration tests skip cleanly without
`PKI_TEST_DATABASE_URL`; the new `auth/mapping` tests are pure unit tests.)

## Remaining limitation (explicit — NOT silently closed)

This phase makes the OIDC side **operational** and proves:

- OIDC: configured `subjectClaim` is honored and determines
  `AuthenticationContext.Subject`.

Phase 10 proved:

- SCIM: configured `scimIdentifier` round-trips through `Provision → Get`.

What is **still not automatically proven** at runtime: that for the *same
person* `subjectClaim(OIDC)` and `scimIdentifier(SCIM)` resolve to the *same*
logical identity. The two extractors are now symmetric and share the same
canonical logic, but nothing yet asserts, at login or at provision time, that
the two resolved values match. Proving bidirectional equivalence would require
a separate runtime comparison (e.g. the Phase 10 probe extended to also
evaluate the OIDC-derived subject, or a reconciliation check). That assertion
is intentionally **out of scope** for this phase and must be tracked
separately — do not mark ADR-025 §3.1 "fully proven" on the strength of this
phase alone.
