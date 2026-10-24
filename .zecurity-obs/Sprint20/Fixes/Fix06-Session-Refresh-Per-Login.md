---
type: fix
sprint: 20
fix: 6
title: Admin and client sessions die after a re-login (one refresh slot per user)
status: fixed and verified live (2026-10-22). Committed locally, not pushed.
source: Phase B2 live run 2026-10-21, finding 4 ("admin and client sessions became invalid after controller restarts")
severity: medium
component: controller (internal/auth, internal/client)
branch: feat/sprint20-m1-phase2
date: 2026-10-22
tags:
  - go
  - auth
  - valkey
  - session
---

# Fix 06: one refresh session per login, not per user

## Symptom

During the B2 live run (2026-10-21), the admin UI and the client daemon had to sign in again after controller restarts, three times in one day.

## Root cause

- The refresh session was stored under **one Valkey key per user**: `refresh:<userID>` (`internal/auth/valkey.go`, ADR-006 "under the same `refresh:<userID>` key").
- Every login wrote that key: web callback `callback.go` and client `TokenExchange` (`internal/client/service.go`).
- Every refresh rotated the token stored in that key (`refresh.go`).
- The admin UI (cookie, `/auth/refresh`) and the client daemon (`ClientService.RefreshSession`) share `auth.Service.RefreshSession`.

So two sessions of the **same user** overwrite each other. Whichever writes second wins; the other's next refresh fails with `refresh token mismatch` and that session is dead. Two admin tabs with separate logins do the same.

**Why it showed up "after restarts":** in the lab the admin and the client are the same user (local test login). After a controller restart someone signs in again in one of them, and that new login replaces the other's refresh token. The restart itself invalidates nothing: `JWT_SECRET` comes from `controller/.env`, and Valkey keeps its data (container not recreated, RDB save on).

## Follow-up observation (2026-10-22, not a Fix 06 defect)

- After two unclean power losses of the lab PC (12:24 and 13:43, no shutdown record), the client's session was dead: `session expired; re-login required`.
- **Cause:** Valkey runs with RDB snapshots only (`save 3600 1 300 100 60 10000`, no AOF). It reloaded a snapshot older than the client's 13:13 rotation, so the client presented a newer refresh token than the stored one.
- The old one-slot design fails the same way. Production durability of the refresh store (AOF / `appendfsync`) is a separate deployment concern.

## Live reproduction (2026-10-22, baseline controller `bf3abed`)

- **Setup:**
  - controller built from `bf3abed` into `~/.cache/zecurity-s3/controller-base`, started at 12:29:17;
  - workspace `xiyo`, local test identity provider, admin user `sathiyaseelank326@gmail.com`;
  - admin-UI logins through that provider, refreshes via `POST /auth/refresh`;
  - the real client daemon on this PC.

| Time | Step | Result |
|---|---|---|
| 12:29:26 | admin login (old session A) | 200 |
| 12:29:45 | client daemon refreshes (its own, older session) | `refresh session dead — user must sign in again` |
| ~12:30 | admin login A → refresh A → admin login B → refresh A | A: **401 `refresh token mismatch`** |
| | refresh B, refresh A | B 200, A **401 `refresh token mismatch`** |
| 12:31:10 | `zecurity-client login` (fresh client session) | logged in, ACL synced |
| ~12:31:20 | refresh admin B | **401 `refresh token mismatch`**: the client login killed the admin session |
| 12:31:33 | admin login C | 200; the client's refresh token is now stale |
| 12:45:10 | client daemon's scheduled refresh (first since its 12:31:10 login) | **`refresh session dead — user must sign in again`**: the admin login killed the client session |

Both directions reproduced live: admin login kills the client session, client login kills the admin session, and admin logins kill each other.

## Fix

**Model:** one refresh session per login. A new login never displaces the user's other sessions. Logout ends only the session it belongs to. Revoking the user (status or `identity_generation`) still ends all of them.

- **Refresh token** = `<sid>.<secret>`:
  - `sid` = 16 random bytes, base64url (22 chars);
  - `secret` = 32 random bytes;
  - stored at `refresh:{<userID>}:<sid>`.
  - The `{userID}` hash tag keeps a user's keys in one slot. valkey-go refuses a `MULTI` across slots (same class as the Phase A `ClearHeartbeatThrottle` panic).
- **Index set** `refresh_sids:{<userID>}`: `SADD` + `EXPIRE` in the same `MULTI` as the session write. Its TTL is pushed out on every write, so it outlives the sessions it lists.
- **Access JWT** carries a new `sid` claim (`omitempty`). Logout (`EndSession`, used by `/auth/logout` and `ClientService.Logout`) only has the access token, so it needs the sid there.
- **Refresh** (`refresh.go`):
  - takes the sid from the refresh token and validates it (`validSessionID`: exactly 22 base64url characters, so client input can't inject key characters);
  - refuses an access token whose `sid` names a different session (`refresh token mismatch`);
  - rotates the secret and keeps the sid;
  - re-stamps the new access token with the sid.
- **Revocation:**
  - lifecycle or generation failure at refresh, and `InvalidateUserSessions` (identity.Revoker), call `DeleteAllRefreshSessions`: every indexed session plus the legacy key;
  - the max-lifetime cap and logout delete only their own session.
- **Legacy sessions:** a refresh token without a sid, or an access token without a `sid` claim, maps to the old `refresh:<userID>` key. On its next refresh it gets a new sid, is stored under the new key, and the legacy key is deleted. Nobody is forced to sign in again by the deploy.
- **API:** `auth.Service` replaces `IssueAccessToken` + `IssueRefreshToken` with `IssueSession(ctx, userID, tenantID, role, email, gen) (access, expiresIn, refresh, err)`. It is used by `callback.go` and the client's `TokenExchange`, so the refresh session exists before the access token that names it.
- **Clients:** no change. The Rust client and the admin UI treat both tokens as opaque. They only read `exp` / known claims from the JWT, and serde ignores the unknown `sid`.

**Files:**
- `controller/internal/auth/valkey.go`, `session.go`, `refresh.go`, `logout.go`, `callback.go`, `service.go`;
- `controller/internal/client/service.go`;
- tests: new `internal/auth/multi_session_test.go`; adjusted `valkey_test.go`, `session_test.go`, `session_rpc_test.go`, `refresh_test.go`, `integration_test.go`, `internal/client/posture_integration_test.go`.

**ADR-006** gets a one-line amendment (storage key and per-login model).

## Tests

- **New** (`multi_session_test.go`):
  - `TestRefreshSessions_IndependentPerLogin`: two logins of one user, interleaved rotations. Fails on the old model.
  - `TestRefreshSession_AccessTokenOfAnotherSessionRefused`
  - `TestRefreshSession_RotationKeepsSessionID`
  - `TestEndSession_EndsOnlyItsOwnSession`
  - `TestInvalidateUserSessions_EndsEverySession` (incl. the legacy key; another user untouched)
  - `TestRefreshSession_LegacySessionMigrates`
  - `TestSplitRefreshToken` (malformed or injected sids)
- **Gate:** `go build ./...`, `go vet ./...` OK. With every `*_TEST_DATABASE_URL` and `AUTH_TEST_VALKEY_URL=redis://localhost:6379/5`, `go test -count=1 -v ./internal/auth/ ./internal/client/ ./internal/identity/` gives **161 PASS, 0 FAIL, 0 SKIP**. That includes the DB + real-Valkey `TestAuthIntegration_LoginBootstrapAndJWTIssue`.

## Live verification (fixed controller)

Fixed binary `~/.cache/zecurity-s3/controller-fix` (this branch plus the fix), same lab, same user. The baseline process was stopped after checking that its `/proc/<pid>/exe` sha matched `controller-base`. **All PASS.**

| Time | Step | Result |
|---|---|---|
| 12:45:30 | fixed controller up; refresh admin C (a **legacy** token from the baseline run), twice | 200, 200. The token now carries a sid; `refresh:<userID>` is gone and `refresh:{<userID>}:quArLs…` plus the index exist (**legacy migration**) |
| 12:45:39 | fresh `zecurity-client login`, then admin logins A and B **after** it | all 200 |
| | refresh A, B, C interleaved, twice | **all 200**. Valkey holds 4 per-login keys (client, A, B, C); index has 4 members |
| 12:45:47 | **restart the fixed controller**; refresh A, B, C | **all 200** |
| | `POST /auth/logout` with A's access token, then refresh A, B, C | logout 204; A **401 `refresh token expired`**; B and C 200; 3 keys left (**logout ends only its own session**) |
| 12:59:36 | client daemon's scheduled refresh: the gRPC `ClientService.RefreshSession` path, with the Rust client storing and re-presenting a `<sid>.<secret>` token, made after admin logins A and B | **`session refreshed proactively`**; 12:59:45 `ACL snapshot synced`; 0 `Unauthenticated` / `dead` / `re-login` lines since 12:45:40 |
| 12:59:50 | refresh admin B and C after the client's rotation | **200, 200**. `zecurity-client status`: running, logged in |

Compared with the baseline, the same sequence no longer kills any session: client then admin logins, controller restart, and a client rotation.

**Lab state after the run:**
- the fixed controller is still running from `~/.cache/zecurity-s3/controller-fix` (log `controller-fix.log`) on :8080/:9090;
- the client daemon is logged in under a per-login session;
- the scripted admin sessions B and C stay in Valkey until their idle TTL.
