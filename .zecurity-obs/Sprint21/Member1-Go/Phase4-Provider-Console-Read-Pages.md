---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 4
execution: P
title: Provider Console — Read Pages (relays, tenants, audit, certificates)
status: planned
depends_on: [2, 3]   # console foundation (C) + read APIs (R)
schema_change: false
tags:
  - react
  - typescript
  - provider-console
  - provider-dashboard
---

# Phase 4 (P) — Provider Console: Read Pages

> **Decision Record:** D-18 (separate console app), D-04 (REST), D-23 (existing telemetry only).
> **Builds on:** Phase C, the dedicated `provider-console/` app with login, roles and nav ([[Sprint21/Member1-Go/Phase2-Provider-Console-Foundation]]), and Phase R's read endpoints ([[Sprint21/Member1-Go/Phase3-Provider-Read-APIs]]).
> This phase adds **read-only** pages to the existing console. It doesn't change the scaffold, the auth flow, or the operator-management page.

## Goal

Provider operators see true fleet and tenant state in the console:
- relays (both roles);
- tenants, provider audit and certificate health (super-admin).

## Files

| Path | Purpose |
|------|---------|
| `provider-console/src/api/types.ts` | DTOs mirroring the Phase R JSON |
| `src/api/reads.ts` | Typed GET wrappers for `/provider/relays*`, `/provider/tenants*`, `/provider/audit`, `/provider/certificates` |
| `src/auth/roles.ts` | Add the new sections to the role matrix (from Phase C) |
| `src/pages/Relays.tsx`, `RelayDetail.tsx` | List + detail |
| `src/pages/Tenants.tsx`, `TenantDetail.tsx` | List + detail |
| `src/pages/Audit.tsx` | Filterable, paginated provider audit |
| `src/pages/Certificates.tsx` | Expiry buckets + table |

## Steps

### P1 — Role matrix entries

In `roles.ts`, add: Relays → `super-admin`, `relay-ops`; Tenants, Audit, Certificates → `super-admin`. The server still enforces access (`decide()`).

### P2 — Pages (read-only)

- **Relays:**
  - Columns: status badge, name, version, hostname, public address, capacity label and `connection_count/max_connections`, last heartbeat as relative time, cert expiry (colour by bucket), attached connectors.
  - Filter by status.
  - **Detail:** allowlists, cert history table (current serial highlighted, revoked rows marked), attachments.
- **Tenants:**
  - Columns: status, slug, name, trust domain, CA expiry, counts (connectors by status, shields, remote networks, users, devices).
  - **Detail:** remote networks, connectors, shields. Per OQ-2, no user identities.
- **Audit:**
  - Newest first. Filters: action, target type/id, provider email, time range. Cursor pagination.
  - `details` JSON shown collapsed.
  - Includes the Phase H/U actions (`provider_user.*`, `provider_session.logout`).
- **Certificates:**
  - Summary tiles: expired, `<24h`, `<7d`, `<30d`, ok.
  - A table filterable by kind and tenant, linking to the relay or tenant detail.
- **Rule:** these pages have **no mutating controls**. No create, revoke, delete, suspend or edit.

### P3 — Tests

- Each page renders fixture data and handles an empty state and an API error.
- Relay and tenant detail links work.
- Audit filters and cursor paging send the right query parameters.
- Certificate bucket colouring matches `summary`.
- The nav hides Tenants, Audit and Certificates for relay-ops.
- The API-surface test (from Phase C) still passes: no new non-GET calls.

## Invariants

1. The read pages are read-only. The console's non-GET calls stay exactly logout plus operator management (Phase C).
2. The UI shows only what the API returns: no computed "health" beyond the D-23 fields and expiry buckets.
3. `admin/` is unchanged and still builds.

## Acceptance Criteria

- [ ] AT-P.1 … AT-P.4 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd provider-console && npm ci && npm run lint && npm test && npm run build
cd admin && npm run build
```

## Implementation Checklist

- [ ] P1 role-matrix entries
- [ ] P2 pages: Relays (+detail), Tenants (+detail), Audit, Certificates
- [ ] P3 tests; build gate

## Post-Phase Fixes

_None yet._
