---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 2
execution: C
title: Provider Console Foundation (dedicated app, local login, roles, operator management UI)
status: planned
depends_on: ["M2-Phase1"]   # C1–C4 need M2-H (local auth endpoints); C5 also needs M2-U (operator API)
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

> **Decision Record:** D-18 (separate React app, own build and domain, network-locked, may share components with `admin/`); amendment **2026-09-26**:
> - **D-24:** local email + password login;
> - **D-25:** provider JWT, `session_generation`;
> - **D-26:** forced first password change;
> - **D-27:** operator lifecycle.
>
> **Needs:** M2-H (`/provider/auth/login`, `/provider/auth/password`, `/provider/auth/logout`) for C1–C4; M2-U (operator API) for C5.
> **Why this comes before the read pages:** the provider console is its **own dedicated Vite + React project**, and its first job is **login with roles**: who can get in, and what each role sees. The fleet pages (relays, tenants, audit, certificates) are added to this same app in Phase P, once the read APIs (Phase R) exist.

## Problem (verified)

- **There is no provider UI anywhere.** `admin/` is the tenant app:
  - every route in `admin/src/App.tsx` is a tenant page;
  - its roles are tenant roles (`ADMIN` / `MEMBER` / `VIEWER`, `controller/graph/schema.graphqls:92`; `App.tsx:54`, `Sidebar.tsx:175`).
  - No branch has provider code in `admin/`, so there is nothing to split out of it. The provider console is a new project.
- **What's combined today is the backend.** Provider login uses Google through the controller's shared auth service, with the tenant `JWT_SECRET` and issuer. Phase H replaces this with local accounts and a provider-only token.
- **Operators can't be managed.** Provider roles exist only in the backend, and until Phase U there's no way to manage operators.

## Goal

A dedicated `provider-console/` app where provider operators:
- sign in with **email and password**;
- change their temporary password when required;
- see only what their role allows;
- log out.

Super-admins also manage operators: add, change role, disable, re-enable, reset password.

## Stack and reuse (implementation choice; reviewable)

- **New top-level directory `provider-console/`:** its own `package.json`, same toolchain versions as `admin/` (Vite 8, React 19, TypeScript 6, Tailwind 4, Radix, `lucide-react`, `react-router-dom` 7, `zustand`, Vitest + Testing Library).
- **No Apollo and no GraphQL codegen:** a small typed `fetch` client for REST `/provider/*` (D-04).
- **Reuse:** copy the needed UI primitives (button, card, table, badge, dialog, select, input, toast) and Tailwind theme tokens from `admin/src/components/ui/`.
  - **`admin/` is not modified**, and the repo isn't restructured into npm workspaces in this sprint.
- **Dev:** port **5174**; Vite proxy `/provider` → `http://localhost:8080` (same-origin in dev, so no CORS).
  - In production the console is on its own origin, and the controller sets `PROVIDER_CONSOLE_ORIGIN` for CORS (Phase H8).
- **No OAuth:** there is no redirect or callback route. Login is a JSON POST (D-24).

## Files

| Path | Purpose |
|------|---------|
| `provider-console/package.json`, `vite.config.ts`, `tsconfig*.json`, `eslint.config.js`, `index.html` | Scaffold |
| `src/api/client.ts` | `fetch` wrapper: bearer header; 401 → clear token and go to Login; 403 `password_change_required` → Change password; other 403 → Forbidden |
| `src/api/types.ts` | DTO types (`LoginResponse`, `Me`, `ProviderUser`, error codes) |
| `src/auth/store.ts` | zustand token store (memory + `sessionStorage`), expiry, `passwordChangeRequired`, current user (`/provider/me`) |
| `src/auth/roles.ts` | The single role matrix: section → allowed roles |
| `src/auth/RequireRole.tsx` | Route guard: not signed in → Login; password change pending → Change password; wrong role → Forbidden |
| `src/pages/Login.tsx` | Email + password form |
| `src/pages/ChangePassword.tsx` | Forced first change **and** voluntary change from the account menu |
| `src/pages/Forbidden.tsx`, `Home.tsx` | Forbidden page; landing page with the signed-in identity and role |
| `src/pages/ProviderUsers.tsx` | **C5:** operator management (super-admin only) |
| `src/components/Layout.tsx` | Shell: role-aware nav, user email + role badge, session countdown, account menu (change password, logout) |
| `src/components/ui/*` | Copied primitives |
| `README.md` | Dev, build, env, deployment and network-lock notes |

## Steps

### C1 — Scaffold

Create the app, copy the primitives and theme, and add a router with public routes (`/login`, `/change-password`) and a guarded layout. Check that `npm run dev`, `lint`, `test` and `build` all work.

### C2 — Login and session

1. **Login:** the form posts `POST /provider/auth/login {email, password}`.
   - **200 with `password_change_required: true`** → store the password-change-only token and go to **Change password**. No other route is reachable.
   - **200 otherwise** → store the token and expiry, load `GET /provider/me`, go to Home.
   - **401 `invalid_credentials`** → a generic "email or password is incorrect". Never say which one.
   - **429 `too_many_attempts`** → "too many attempts, try again in N minutes" (from `Retry-After`).
   - **503** → "login unavailable".
2. **Change password:** `POST /provider/auth/password {current_password, new_password}`.
   - Check the policy on the client (12–128 characters, not the email), but the **server decides**.
   - On success, store the **new** token returned, then load `/provider/me` and go to Home. The old token is dead (generation bump).
3. **Every request** sends `Authorization: Bearer <token>`.
   - **401** (expired, logged out, disabled, role changed or password reset: Phase H/U generation bumps) → clear the token and show Login with a notice.
   - **403 `password_change_required`** → Change password.
   - Any other **403** → Forbidden.
4. **Logout:** `POST /provider/auth/logout`, then clear the token. This ends the user's sessions in every tab.
5. **Expiry:** show a session countdown; at expiry go to Login. There is no refresh token (15 min TTL, D-28).
6. **Passwords** exist only in form state. They're cleared right after submit and never stored, logged or kept in the URL.

### C3 — Roles in the UI

- The role comes from `/provider/me.role`:
  - `super-admin` sees every section (**Provider users** now; Relays, Tenants, Audit, Certificates arrive in Phase P);
  - `relay-ops` sees **Relays** only (Phase P).
- Each route is wrapped in `RequireRole([...])`. The nav shows only permitted sections. **The server still enforces access** (`decide()`); the UI only hides what would be refused.
- All entries live in `src/auth/roles.ts`, so Phase P adds pages by adding entries there.

### C4 — Home

A landing page showing the signed-in email, role, last login time, and when the session expires. Until Phase P lands, this is the default page for `relay-ops`.

### C5 — Provider users page (needs M2-U merged)

This page is super-admin only.
- **List:** email, role badge, status (active/disabled), last login, "must change password" badge.
- **Add operator:** a dialog with an email and a role select → `POST /provider/users`.
  - The response's `temporary_password` is shown **once**, in a dialog with a copy button and the warning "shown once — share it securely; they must change it at first login".
  - Closing the dialog discards it from memory. It's never stored or re-fetchable.
- **Change role:** a select → `PATCH /provider/users/{id}`, after a confirmation dialog that names the email and both roles.
- **Disable / enable:** buttons → `POST …/disable` / `…/enable`, after a confirmation dialog.
- **Reset password:** a button → `POST …/reset-password`, after a confirmation dialog. The temporary password is shown once, the same way as on add.
- **Guard UX:** mirror the server guards, but always rely on the server's answer.
  - Your own row has no disable, demote or reset controls (use Change password instead).
  - Show the API's `409` code as a readable message (`last_super_admin`, `cannot_modify_self`, `already_exists`, `exists_disabled`).
- **If another admin changes your role, disables you or resets your password,** your next request returns 401 and you land on Login.

### C6 — Tests and README

- **Vitest + Testing Library:**
  - login success; `password_change_required` routes to Change password and blocks other routes;
  - the generic 401 message; the 429 message uses `Retry-After`;
  - a successful change password stores the new token;
  - a 401 clears the token and redirects; a 403 shows Forbidden;
  - `RequireRole` blocks relay-ops from `/users`; the nav matches the role matrix;
  - the Provider users page lists fixtures and submits add, change-role, disable, enable and reset with the right method and path;
  - the temporary password is shown once and gone after the dialog closes;
  - 409 codes show as messages; your own row has no destructive controls.
- **API-surface test:** the only non-GET calls in the client are the three auth calls (`login`, `password`, `logout`) and the five operator mutations.
- **No-persistence test:** passwords and tokens never reach `localStorage`. Tokens are in `sessionStorage` only.
- **`README.md`:** dev setup (bootstrap env from `docs/database-development.md`); production build (`dist/` on a static host, **own domain**); the console origin must be **network-locked** (VPN or IP allowlist, D-18); and `PROVIDER_CONSOLE_ORIGIN` must equal the deployed origin.

## Invariants

1. **A dedicated app:** `provider-console/` builds and deploys independently, and `admin/` is unchanged.
2. **Allowed writes:** the console's only non-GET calls are the auth calls (login, change password, logout) and operator management (super-admin). Every other view is read-only.
3. Passwords are never persisted, logged or put in the URL. Tokens are never written to `localStorage`. A temporary password is displayed once and then discarded.
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
- [ ] C2 login form, forced change password, 401/403/429 handling, logout, expiry
- [ ] C3 role matrix (`roles.ts`), `RequireRole`, role-aware nav
- [ ] C4 Home
- [ ] C5 Provider users page (after M2-U merged), incl. show-once temporary passwords
- [ ] C6 tests; README with deployment and network-lock notes; build gate

## Post-Phase Fixes

_None yet._
