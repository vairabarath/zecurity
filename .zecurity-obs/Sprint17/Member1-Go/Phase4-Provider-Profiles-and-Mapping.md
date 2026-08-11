---
type: phase
member: M1
sprint: 17
phase: 4
title: Provider Profiles + Identity Mapping Validation
status: planned
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
- [ ] Provider profiles carry defaults (subject claim, identifier attr, scopes), capabilities, quirks, mapping overrides — **no per-provider handler types**. Generic SCIM 2.0 always available.
- [ ] `testIdpConnection`: probe-user lifecycle `POST → GET → verify identifier → DELETE`; read-only fallback where create/delete unsupported.
- [ ] Fail-closed: unproven mapping keeps SCIM **disabled**; only `identity.mapping.break_glass` (Phase 3) may override, requiring reason + `scim.mapping.break_glass_override` audit.

## Rules
- Never hardcode `sub == externalId`. Both extractors resolve to `external_identities.subject`.

## Build gate
`go build ./...` + tests: mapping proven→enabled, unproven→disabled, ADMIN cannot override (only the permission).
