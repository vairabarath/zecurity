---
type: phase
member: M1
person: Barath   # reassigned 2026-09-28 (Sathiya on testing, Phase V)
sprint: 21
phase: 4
execution: P
title: Provider Console — Read Pages (relays, tenants, audit, certificates)
status: done   # 2026-10-06, branch sprint21/p-provider-read-pages (P1–P7)
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

- [x] AT-P.1 … AT-P.4 in [[Sprint21/Acceptance-Test-Plan]] pass. *(Verified in development: unit tests plus the live check; see "Closeout" below. Formal acceptance is M1's.)*

## Build Check

```bash
cd provider-console && npm ci && npm run lint && npm test && npm run build
cd admin && npm run build
```

## Implementation Checklist

- [x] P1 role-matrix entries
- [x] P2 pages: Relays (+detail), Tenants (+detail), Audit, Certificates
- [x] P3 tests; build gate

## Closeout (2026-10-06)

### Commits (branch `sprint21/p-provider-read-pages`)
1. `ff75b86` Typed read wrappers (6 GETs, allowlisted query strings) and transport types; the API-surface test pins reads and writes.
2. `73ea6e4` Shared pieces: `bucketFor()`, `usePagedQuery` (URL filters, cursor stack), `StatusBadge`, `ExpiryBadge`, `DataState`, `Pager`.
3. `510fb03` Relays list and detail; `useDetailQuery`.
4. `a3d54fe` Tenants list and the audited tenant detail.
5. `0de39b1` Audit page; the shared hook now also keys on the history entry.
6. `362bed6` Certificates page.
7. `4086f54` Narrow-screen fix for the Certificates tenant filter (found in the live check).
8. This closeout (docs and README).

### Decisions (locked in review)
- **Landing page:** Home stays the landing page for both roles; post-login routing is unchanged.
- **Tenant detail:**
  - The button is labelled exactly "Refresh (records an audit entry)".
  - One GET on navigation (StrictMode included); Refresh and Retry are one GET each.
  - No polling, no refetch on focus/visibility, no automatic retry, no prefetch.
  - The UUID is validated before any request.
  - It uses `useDetailQuery`, never the paged hook.
- **Platform CA note on Certificates:** "Platform CAs (root, intermediate) are counted in the summary; their expiry is beyond the listing window." The API was not changed.
- **`bucketFor()`** mirrors the server's boundaries exactly (expired ≤ now < lt_24h ≤ +24h < lt_7d ≤ +7d < lt_30d ≤ +30d < ok). It's the only client-derived "health" value.
- **URLs carry only allowlisted filter fields.** Any filter change or navigation clears the cursor stack; a cursor is never reused under other filters.
- **Relay attachments** show tenant ids as plain text. On Certificates, tenant ids link to tenant detail on an explicit click only.
- **A hand-edited `within`** such as `48h` is accepted and sent as written (the API allows any duration ≤ 8760h). The dropdown shows no label for it; a "Custom" label can come later.
- **Certificate row badges** use the server's per-row `bucket`, which is authoritative and avoids clock drift. The shared helper is tested against the same boundaries.

### Notes for later readers
- **The certificates API has no issue date, `is_current` or revocation fields** (revoked certificates are excluded server-side). The Certificates page therefore shows none of those, and tests prove it never infers them. Current and revoked state is shown on Relay detail, from `is_current` and `revoked_at` only.
- **The shared paging hook is keyed on the history entry (`location.key`) as well as the filters** (commit 5). Before that, a round trip invalid range → Reset back to the same filters reused the old result without a request. Now every navigation is one fresh request from page 1; StrictMode still sends one.
- **Mutation-harness caveat:** the per-mutation numbers in some earlier reports could be failing-file counts rather than failing-test counts. The caught/not-caught results were correct throughout.

### Live check (2026-10-06)
**Setup:** real controller on a throwaway Postgres DB and a throwaway Valkey container, behind the console dev server (Vite, React StrictMode) in Orca's browser. Operators: a super-admin and a relay-ops created through the operator API.

- **AT-P.1, nav per role:**
  - super-admin sees Home | Relays | Tenants | Audit | Certificates | Provider users;
  - relay-ops sees Home | Relays; `/tenants`, a tenant detail, `/audit` and `/certificates` all show Forbidden; `/relays` works;
  - relay-ops added no `tenant.read` rows.
- **AT-P.2, relays:**
  - Every list column matches `GET /provider/relays` for the live relay. Its status read "inactive", correctly mirroring the API after the controller's eviction loop (the test heartbeat had expired).
  - Relay detail certificate history:
    - `c2` is Current (from `is_current`);
    - `b2` is Revoked with time and reason;
    - `a1` is expired but not marked revoked.
  - Allowlists are shown, and attachments have no tenant links.
- **AT-P.3, tenants:**
  - The list excludes the deleted tenant by default.
  - **Audit deltas in the StrictMode dev build:**

    | Action | New `tenant.read` rows |
    |---|---|
    | Open the list | 0 |
    | Hover a tenant link | 0 |
    | Open the detail | 1 |
    | Focus or visibility change | 0 |
    | Refresh | 1 |
    | Away and back | 1 |

    Every row had the right target, actor and `{"view":"detail"}`.
  - The detail lists the deleted network with its status and the revoked connector, and resolves names. No identity canaries (tenant admin email, `provider_sub`, device owner, hostname, public IP, CA secrets) appear, and the only link is "Back to tenants".
- **AT-P.3, audit:**
  - Phase H/U actions are shown (login, password change, operator create, bootstrap).
  - The `action=tenant.read` filter is kept in the URL and returns the 3 rows; details show `{"view":"detail"}`.
  - Page order equals API order, and viewing the audit added no audit rows.
- **AT-P.3, certificates:**
  - Tiles equal the API summary, and the summary is identical for the default and 8760h windows.
  - Row order and buckets equal the API; 8760h adds the workspace CA row.
  - The platform CA note shows; client devices show "—"; no secrets appear.
- **AT-P.4, read-only:** the only controls are filters, paging, Refresh/Retry and the audit details toggle; every request was a GET.
- **Layout:** Relays, relay detail, Tenants and Audit are clean at 375/640/768/1024px. Certificates scrolled sideways at 360px because of the tenant-filter width; fixed in `4086f54` and re-measured clean. Tenant detail was deliberately not loaded in measurement frames, since each load is audited.
- **Cleanup:** DB dropped, Valkey container removed, servers stopped, tabs closed, no dev-Valkey keys.

### Final gates
- `provider-console`: `npm ci` (0 vulnerabilities), `npm run lint` (0 problems), `npm test` (26 files, 374 tests, no React warnings), `npm run build` all pass.
- `admin/` and `controller/` are untouched on the branch.
- **Mutation checks, all caught:**
  - P1: 6;
  - P2: 12;
  - P3: 9;
  - P4: 11;
  - P5: 17 required plus 1;
  - P6: 21.

### Acceptance (verified in development)

| Case | Evidence |
|---|---|
| AT-P.1 Role-aware nav | Unit tests and live (both roles) |
| AT-P.2 Relays | List and detail match the API for a live relay (status, heartbeat, capacity, cert expiry, history) |
| AT-P.3 Tenants / Audit / Certificates | Pages match API responses; audit filters, paging and H/U actions; certificate buckets and colours match the summary and rows |
| AT-P.4 Read-only | No mutating controls; API-surface test passes; build and lint pass |

## Post-Phase Fixes

_None yet._
