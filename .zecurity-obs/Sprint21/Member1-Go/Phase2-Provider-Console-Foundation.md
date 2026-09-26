---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 2
execution: C
title: Provider Console Foundation (dedicated app, login, roles, operator management UI)
status: planned
depends_on: ["M2-Phase1"]   # C1–C4 need M2-H (login contract); C5 also needs M2-U (operator API)
schema_change: false
tags:
  - react
  - typescript
  - vite
  - provider-console
  - rbac
  - provider-dashboard
---

# Phase 2 (C) — Provider Console Foundation

> **Decision Record:** D-18 (separate React app, own build and domain, network-locked, may share components with `admin/`), D-05 (roles `super-admin` / `relay-ops`), D-16 (Google login, logout).
> **Needs:** M2-H6 (callback redirect to `PROVIDER_CONSOLE_ORIGIN`, CORS for `/provider/*`) for C1–C4; M2-U (operator API) for C5.
> **Why this comes before the read pages:** the provider console is its **own dedicated Vite + React project**, and its first job is **login with roles**: who can get in, and what each role sees. The fleet pages (relays, tenants, audit, certificates) are added to this same app in Phase P, once the read APIs (Phase R) exist.

## Problem (verified)

- **There is no provider UI anywhere.** `admin/` is the tenant app:
  - every route in `admin/src/App.tsx` is a tenant page;
  - its roles are tenant roles (`ADMIN` / `MEMBER` / `VIEWER`, `controller/graph/schema.graphqls:92`; `App.tsx:54`, `Sidebar.tsx:175`).
  - No branch has provider code in `admin/`, so there is nothing to split out of it. The provider console is a new project.
- **What's combined today is the backend.** Provider and tenant logins share the controller's Google OAuth service, `JWT_SECRET` and issuer. Phase H splits that.
- **Operators can't be managed.** Provider roles exist only in the backend, and until Phase U there's no way to manage operators at all.

## Goal

A dedicated `provider-console/` app where provider operators:
- sign in with Google;
- see only what their role allows;
- log out.

Super-admins can also manage operators (add, change role, disable, re-enable).

## Stack and reuse (implementation choice; reviewable)

- **New top-level directory `provider-console/`:** its own `package.json`, same toolchain versions as `admin/` (Vite 8, React 19, TypeScript 6, Tailwind 4, Radix, `lucide-react`, `react-router-dom` 7, `zustand`, Vitest + Testing Library).
- **No Apollo and no GraphQL codegen:** a small typed `fetch` client for REST `/provider/*` (D-04).
- **Reuse:** copy the needed UI primitives (button, card, table, badge, dialog, select, input, toast) and Tailwind theme tokens from `admin/src/components/ui/`.
  - **`admin/` is not modified**, and the repo isn't restructured into npm workspaces in this sprint.
- **Dev:** port **5174**; Vite proxy `/provider` → `http://localhost:8080` (no CORS in dev).
  - Controller env: `PROVIDER_CONSOLE_ORIGIN=http://localhost:5174` and `PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/provider/auth/callback`.

## Files

| Path | Purpose |
|------|---------|
| `provider-console/package.json`, `vite.config.ts`, `tsconfig*.json`, `eslint.config.js`, `index.html` | Scaffold |
| `src/api/client.ts` | `fetch` wrapper: bearer header; 401 → clear token and go to Login; 403 → "not permitted" |
| `src/api/types.ts` | DTO types (`Me`, `ProviderUser`, error codes) |
| `src/auth/store.ts` | zustand token store (memory + `sessionStorage`), expiry, current user (`/provider/me`) |
| `src/auth/RequireRole.tsx` | Route guard: not signed in → Login; wrong role → Forbidden page |
| `src/pages/Login.tsx`, `AuthCallback.tsx`, `Forbidden.tsx`, `Home.tsx` | Login flow; landing page with the signed-in identity and role |
| `src/pages/ProviderUsers.tsx` | **C5:** operator management (super-admin only) |
| `src/components/Layout.tsx` | Shell: role-aware nav, user email + role badge, session countdown, logout |
| `src/components/ui/*` | Copied primitives |
| `README.md` | Dev, build, env, deployment and network-lock notes |

## Steps

### C1 — Scaffold

Create the app, copy the primitives and theme, and add a router with public routes (`/login`, `/auth/callback`) and a guarded layout. Check that `npm run dev`, `lint`, `test` and `build` all work.

### C2 — Login and session

1. **Login:** `GET /provider/auth/initiate` → `window.location = auth_url`.
2. **Return:** Google → controller `/provider/auth/callback` → **302** `<console>/auth/callback#token=…&expires_in=…` (M2-H6).
   - `AuthCallback` stores the token and expiry, **clears the fragment** (`history.replaceState`), then loads `GET /provider/me`.
3. **Every request** sends `Authorization: Bearer <token>`.
   - **401** (expired, logged out, role changed or disabled; Phase H/U generation bump) → clear the token and show Login with a notice.
   - **403** → the Forbidden page.
4. **Logout:** `POST /provider/auth/logout`, then clear the token.
5. **Expiry:** show a session countdown; at expiry go to Login. There is no refresh (15 min TTL, D-16).

### C3 — Roles in the UI

- The role comes from `/provider/me.role`:
  - `super-admin` sees every section (**Provider users** now; Relays, Tenants, Audit, Certificates arrive in Phase P).
  - `relay-ops` sees **Relays** only (Phase P).
- Each route is wrapped in `RequireRole([...])`. The nav shows only permitted sections. **The server still enforces access** (`decide()`); the UI only hides what would be refused.
- A role matrix lives in **one file** (`src/auth/roles.ts`), so Phase P adds its pages by adding entries there.

### C4 — Home

A landing page showing the signed-in email, role, `hd`, and when the session expires. Until Phase P lands, this is the default page for `relay-ops`.

### C5 — Provider users page (needs M2-U merged)

This page is super-admin only.
- **List:** email, role badge, status (active/disabled), "bound" (has signed in), `pinned` badge ("managed by config").
- **Add operator:** a dialog with an email and a role select → `POST /provider/users`. Afterwards, show "they can now sign in with Google at this console".
- **Change role:** a select → `PATCH /provider/users/{id}`, after a confirmation dialog that names the email and both roles.
- **Disable / enable:** buttons → `POST …/disable` / `…/enable`, after a confirmation dialog.
- **Guard UX:** mirror the server guards, but always rely on the server's answer.
  - Hide or disable the controls on your own row and on pinned rows.
  - Show the API's `409` code as a readable message (`last_super_admin`, `managed_by_bootstrap`, `cannot_modify_self`, `already_exists`, `exists_disabled`).
- **After changing your own session state indirectly** (e.g. another admin demotes you), the next request returns 401 and you land on Login (the generation bump).

### C6 — Tests and README

- **Vitest + Testing Library:**
  - the callback parses and clears the fragment;
  - a 401 clears the token and redirects;
  - a 403 shows Forbidden;
  - `RequireRole` blocks relay-ops from `/users`;
  - the nav matches the role matrix;
  - the Provider users page lists fixtures, submits add, change-role, disable and enable with the right method and path, and shows each 409 code as a message;
  - own and pinned rows have no destructive controls.
- **API-surface test:** the only non-GET calls in the client are `POST /provider/auth/logout` and the four operator mutations.
- **`README.md`:** dev setup; production build (`dist/` on a static host, **own domain**); the console origin must be **network-locked** (VPN or IP allowlist, D-18); and `PROVIDER_CONSOLE_ORIGIN` must equal the deployed origin.

## Invariants

1. **A dedicated app:** `provider-console/` builds and deploys independently, and `admin/` is unchanged.
2. **Allowed writes:** the console's only non-GET calls are logout and operator management (super-admin). Every other view is read-only.
3. The token is never written to `localStorage`, logged, or left in the URL after the callback.
4. The console never calls tenant endpoints (`/graphql`, tenant `/auth/*`).
5. The UI never grants access the server wouldn't. It hides controls, and the server decides.

## Acceptance Criteria

- [ ] AT-C.1 … AT-C.7 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd provider-console && npm ci && npm run lint && npm test && npm run build
cd admin && npm run build     # unchanged; must still build
```

## Implementation Checklist

- [ ] C1 scaffold + primitives copied
- [ ] C2 login, callback, 401/403 handling, logout, expiry
- [ ] C3 role matrix (`roles.ts`), `RequireRole`, role-aware nav
- [ ] C4 Home
- [ ] C5 Provider users page (after M2-U merged)
- [ ] C6 tests; README with deployment and network-lock notes; build gate

## Post-Phase Fixes

_None yet._
