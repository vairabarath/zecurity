---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 3
execution: P
title: Provider Console (read-only)
status: planned
depends_on: [2, "M2-Phase1"]   # needs R endpoints and M2-H6 login contract
schema_change: false
tags:
  - react
  - typescript
  - provider-console
  - provider-dashboard
---

# Phase 3 (P) — Provider Console (read-only)

> **Decision Record:** D-18 (separate React app, own build and domain, network-locked, may share components with `admin/`), D-04 (REST), D-23 (existing telemetry only).
> **Needs:** M1-R endpoints and M2-H6 (callback redirect to `PROVIDER_CONSOLE_ORIGIN`, CORS for `/provider/*`).

## Problem (verified)

- There is no provider UI. `admin/` is the tenant app: Apollo GraphQL, the tenant token flow (`#token=` from `/auth/callback`), and a Vite proxy for `/graphql`, `/auth/refresh`, `/api`, `/ca.crt` (`admin/vite.config.ts`).
- Provider APIs are REST under `/provider/*` with a bearer provider JWT (15 min, no refresh).

## Goal

A separate, **read-only** web console where provider operators log in with Google and see:
- relays;
- tenants;
- provider audit;
- certificate health.

## Stack and reuse (implementation choice; reviewable)

- **New top-level directory `provider-console/`:** its own `package.json`, same toolchain versions as `admin/` (Vite 8, React 19, TypeScript 6, Tailwind 4, Radix, `lucide-react`, `react-router-dom` 7, `zustand`, Vitest + Testing Library).
- **No Apollo and no GraphQL codegen:** a small typed `fetch` client for `/provider/*`.
- **Reuse:** copy the needed UI primitives (button, card, table, badge, dialog, select, toast) from `admin/src/components/ui/`, plus the Tailwind theme tokens.
  - **Don't modify `admin/`**, and don't restructure the repo into npm workspaces in this sprint. A shared package can come later.
- **Dev server:** port **5174**. Proxy `/provider` → `http://localhost:8080` (no CORS in dev).
  - Set `PROVIDER_CONSOLE_ORIGIN=http://localhost:5174` on the controller so the login redirect lands on the console.
  - Google's redirect still goes to the controller: `PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/provider/auth/callback`.

## Files

| Path | Purpose |
|------|---------|
| `provider-console/package.json`, `vite.config.ts`, `tsconfig*.json`, `eslint.config.js`, `index.html` | Scaffold |
| `src/api/client.ts` | `fetch` wrapper: bearer header; 401 → clear token and redirect to login |
| `src/api/types.ts` | DTO types mirroring the M1-R JSON |
| `src/auth/store.ts` | zustand token store (memory + `sessionStorage`), expiry timestamp |
| `src/pages/Login.tsx`, `AuthCallback.tsx` | Start login (`GET /provider/auth/initiate` → `auth_url`); read `#token=&expires_in=` |
| `src/pages/Relays.tsx`, `RelayDetail.tsx` | List + detail |
| `src/pages/Tenants.tsx`, `TenantDetail.tsx` | List + detail |
| `src/pages/Audit.tsx` | Filterable, paginated provider audit |
| `src/pages/Certificates.tsx` | Expiry buckets + table, filter by kind and tenant |
| `src/components/ui/*` | Copied primitives |
| `src/components/Layout.tsx` | Nav (only sections the role can read), user email, logout |
| `README.md` | Dev, build, env, deployment notes |

## Steps

### P1 — Scaffold

Create the app and copy the primitives and theme. Check that `npm run dev`, `lint`, `test` and `build` all work.

### P2 — Auth

1. **Login:** `GET /provider/auth/initiate` → `window.location = auth_url`.
2. **Return:** Google → controller `/provider/auth/callback` → **302** to `<console>/auth/callback#token=…&expires_in=…` (M2-H6).
   - `AuthCallback` stores the token and expiry, **clears the fragment** (`history.replaceState`), then loads `/provider/me`.
3. **Every request:** send `Authorization: Bearer <token>`. On **401** (expired, revoked by logout, or generation bump), clear the token and go to Login with a notice. On **403**, show "not permitted", not a crash.
4. **Logout:** `POST /provider/auth/logout`, then clear the token. Other tabs' tokens are invalidated server-side.
5. **Expiry:** show the remaining session time; at expiry, send the user to Login. There is no refresh token (15 min TTL).
6. **Role-aware nav:** from `/provider/me.role`:
   - relay-ops sees **Relays** only;
   - super-admin sees everything.

   The server still enforces access. The nav only hides sections the user can't open.

### P3 — Pages (read-only)

- **Relays:**
  - Columns: status badge, name, version, hostname, public address, capacity label and `connection_count/max_connections`, last heartbeat as relative time, cert expiry (colour by bucket), attached connectors.
  - Filter by status.
  - **Detail:** allowlists, cert history table, attachments.
- **Tenants:**
  - Columns: status, slug, name, trust domain, CA expiry, counts (connectors by status, shields, remote networks, users, devices).
  - **Detail:** remote networks, connectors, shields. Per OQ-2, no user identities.
- **Audit:** newest first; filters (action, target type/id, provider email, time range); cursor pagination; `details` JSON shown collapsed.
- **Certificates:**
  - Summary tiles: expired, `<24h`, `<7d`, `<30d`, ok.
  - A table filterable by kind and tenant, linking to the relay or tenant detail.
- **Rule:** **no mutating controls anywhere.** No create, revoke, delete, suspend or edit buttons, and no form that POSTs, except logout.

### P4 — Tests and deployment notes

- **Vitest + Testing Library:**
  - the callback parses and clears the fragment;
  - a 401 clears the token and redirects;
  - nav hides tenant, audit and certificate sections for relay-ops;
  - each page renders fixture data;
  - the certificate bucket colouring;
  - a static test that the API client exposes **no** method other than GET, plus `logout`.
- **`README.md`:**
  - dev setup (`PROVIDER_CONSOLE_ORIGIN`, the redirect URI, port 5174);
  - production build (`npm run build` → `dist/`, a static host on its **own domain**);
  - the requirement that the console origin is **network-locked** (VPN or IP allowlist at the host or proxy, per D-18);
  - the CORS origin must equal the deployed console origin.

## Invariants

1. **The console is read-only:** the only non-GET call is `POST /provider/auth/logout`.
2. The token is never written to `localStorage`, logged, or left in the URL after the callback.
3. The console never talks to tenant endpoints (`/graphql`, `/auth/*` tenant routes).
4. `admin/` is unchanged and still builds.
5. The UI shows only what the API returns: no computed "health" beyond the D-23 fields and expiry buckets.

## Acceptance Criteria

- [ ] AT-P.1 … AT-P.7 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd provider-console && npm ci && npm run lint && npm test && npm run build
cd admin && npm run build     # unchanged; must still build
```

## Implementation Checklist

- [ ] P1 scaffold + primitives copied
- [ ] P2 auth (login, callback, 401/403, logout, expiry, role-aware nav)
- [ ] P3 pages: Relays (+detail), Tenants (+detail), Audit, Certificates
- [ ] P4 tests; README with deployment and network-lock notes; build gate

## Post-Phase Fixes

_None yet._
