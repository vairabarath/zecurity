---
type: phase
member: M1
sprint: 17
phase: 4
title: Provider Profiles + Identity Mapping Validation
status: done
depends_on: [2, 3]
tags: [go, identity, scim, provider-profiles, mapping, pending-05]
---

# Phase 4 — Provider Profiles + Identity Mapping Validation

> Depends on Phase 2, 3. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §3, §3.1, §3.2 · [[PENDING-05-SCIM-Implementation-Plan]] P4.

## Goal
One provider-agnostic engine driven by **provider profiles** + per-connection overrides, and an
**active** mapping-validation that proves `subjectClaim(OIDC) == scimIdentifier(SCIM)`.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/profiles.go` | **new** — built-in profiles (Okta/Entra/JumpCloud/Keycloak/Generic) + capability metadata |
| `controller/internal/scim/mapping.go` | **new** — Canonical Identity Key extraction |
| `controller/internal/auth/idp` / `testIdpConnection` | active probe-user round-trip validation |

## Steps
- [x] Provider profiles carry defaults (subject claim, identifier attr, scopes), capabilities, quirks, mapping overrides — **no per-provider handler types**. Generic SCIM 2.0 always available.
- [x] `testIdpConnection` active checks within the Phase 4 boundary: OIDC discovery probe + mapping-config validation + fail-closed `MappingGate`. The literal probe-user lifecycle `POST → GET → verify identifier → DELETE` is **deferred to Phase 5** (the `/Users` endpoint does not exist yet — the controller is the SCIM *server*, and round-tripping requires it). Phase 5 plugs into the same gate via `MappingGateResult.WithRoundTrip` without changing this contract.
- [x] Fail-closed: unproven mapping keeps SCIM **disabled**; only `identity.mapping.break_glass` (Phase 3) may override, requiring reason + `scim.mapping.break_glass_override` audit. ADMIN role alone is denied.

## Rules
- Never hardcode `sub == externalId`. Both extractors resolve to `external_identities.subject`.
- Phase 4 does NOT claim the mapping is `proven` without the real round-trip; the gate stays `unproven` and SCIM disabled until Phase 5 proves it or a break-glass override applies.

## Build gate
`go build ./...` + tests: mapping proven→enabled (Phase 5 seam), unproven→disabled, ADMIN cannot override (only the permission + reason).
