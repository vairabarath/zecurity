---
type: planning
status: completed
sprint: 11.3
tags:
  - sprint11.3
  - client
  - controller
  - auth
  - refresh-token
  - session-management
---

# Sprint 11.3 — Client Access Token Refresh & Session Management

> **Status: COMPLETED**
> All items shipped in commit `5cddc9b` on 2026-07-02.

---

## Sprint Goal

Implement proactive access token refresh in the client daemon. The access JWT
expires every 15 minutes; previously the client had no mechanism to rotate it,
causing ACL fetches to fail silently after expiry. This sprint adds:

1. `client/src/auth.rs` — refresh and logout helpers.
2. A proactive `run_refresh_scheduler` daemon task that sleeps until the
   token nears expiry and rotates it ahead of time.
3. `fetch_acl_snapshot_with_refresh` — transparent single-retry on 401:
   call `/auth/refresh`, persist rotated tokens, retry the ACL fetch.
4. Server-side `/auth/logout` handler and best-effort client-side logout on
   `zecurity logout`.

---

## Dependency Graph

```text
Phase A — Controller: /auth/refresh + /auth/logout handlers (M2)
  ↓
Phase B — Client: auth.rs helpers + daemon refresh scheduler + transparent retry (M1)
```

---

## Execution Path

### Phase A — M2: Controller Auth Handlers

> See [[Sprint11.3/Member2-Go/Phase1-Auth-Handlers]].

- [x] **M2-A1** `controller/internal/auth/refresh.go` — `/auth/refresh` handler: accept expired access token in `Authorization: Bearer`, read new `X-Refresh-Token` header, validate refresh token, mint fresh pair, rotate refresh token in DB, return `{ access_token, refresh_token }` JSON
- [x] **M2-A2** `controller/internal/auth/logout.go` — `/auth/logout` handler: invalidate refresh session in DB; idempotent (returns 204 whether or not session existed)
- [x] **M2-A3** `controller/internal/auth/service.go` — register `/auth/refresh` and `/auth/logout` routes
- [x] **M2-A4** `controller/cmd/server/main.go` — wire auth service into server startup
- [x] **Build gate:** `cd controller && go build ./...`

### Phase B — M1: Client Auth Module & Daemon Integration

> Depends on Phase A. See [[Sprint11.3/Member1-Client/Phase1-TokenRefresh]].

- [x] **M1-B1** `client/src/auth.rs` (new) — `refresh_access_token()`: POST `/auth/refresh`, `Bearer` expired token + `X-Refresh-Token` header, decode response, extract `exp` from JWT payload without signature verification
- [x] **M1-B2** `client/src/auth.rs` — `RefreshError` enum: `SessionDead` (401 → user must re-login) vs `Transient` (network/server → retry later)
- [x] **M1-B3** `client/src/auth.rs` — `logout()`: best-effort POST `/auth/logout`; returns `Ok` on non-2xx (server is idempotent)
- [x] **M1-B4** `client/src/auth.rs` — `extract_exp()`: base64url-decode JWT payload, deserialize `exp` claim
- [x] **M1-B5** `client/src/daemon.rs` — `run_refresh_scheduler`: background task sleeping until `expires_at - 60s`, then calling `refresh_access_token`; updates in-memory state and persists rotated tokens atomically
- [x] **M1-B6** `client/src/daemon.rs` — `fetch_acl_snapshot_with_refresh`: wraps `fetch_acl_snapshot`; on `Unauthenticated` gRPC status, refreshes tokens and retries once; dead session clears local state
- [x] **M1-B7** `client/src/state_store.rs` — `save_rotated_tokens()`: atomically persist new `access_token`, `refresh_token`, `expires_at` to encrypted state store
- [x] **M1-B8** `client/src/cmd/logout.rs` — call `auth::logout()` before clearing local state; log warning on transport failure but proceed regardless
- [x] **M1-B9** `client/src/main.rs` — wire `auth` module
- [x] **Build gate:** `cd client && cargo build`

---

## Final Build Gates

- [x] `cd controller && go build ./...`
- [x] `cd client && cargo build`

---

## Acceptance Criteria

- [x] Client daemon proactively refreshes access token 60s before expiry — no user action needed.
- [x] ACL fetch on 401 triggers one transparent refresh + retry; session dead → prompts re-login.
- [x] Rotated refresh token persisted atomically before retry — no replay on crash.
- [x] `zecurity logout` invalidates server-side session via `/auth/logout`; clears local state regardless of server response.
- [x] `RefreshError::SessionDead` (HTTP 401) distinguished from `Transient` (network/5xx) — dead session does not retry.
- [x] `extract_exp` reads `exp` from JWT payload without signature verification (signature enforced server-side on each API call).

---

## Commits

| Commit | Date | Author | Description |
|---|---|---|---|
| `5cddc9b` | 2026-07-02 | Yogesh | client refresh token |
