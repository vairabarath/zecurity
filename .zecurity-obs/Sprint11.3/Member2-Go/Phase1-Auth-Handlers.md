---
type: phase
member: M2
sprint: 11.3
phase: 1
title: Controller — /auth/refresh and /auth/logout Handlers
status: completed
commit: 5cddc9b
depends_on: []
---

# Phase 1 — Controller: /auth/refresh and /auth/logout

## Goal

Add two HTTP endpoints the client daemon calls for session management:
- `/auth/refresh` — exchange an expired access token + valid refresh token for a fresh pair
- `/auth/logout` — invalidate the caller's refresh session so leaked tokens cannot be replayed

## Files

| File | Change |
|---|---|
| `controller/internal/auth/refresh.go` | `/auth/refresh` handler |
| `controller/internal/auth/logout.go` | `/auth/logout` handler |
| `controller/internal/auth/service.go` | Register both routes |
| `controller/cmd/server/main.go` | Wire auth service into startup |

## /auth/refresh contract

```
POST /auth/refresh
Authorization: Bearer <expired-or-valid-access-token>
X-Refresh-Token: <refresh-token>

→ 200 { "access_token": "...", "refresh_token": "..." }
→ 401  (refresh token invalid/expired/revoked — session dead)
→ 5xx  (transient error)
```

- Access token accepted even if expired — used only for identity extraction
- Refresh token rotated on every call (rolling window); DB must be updated before returning
- New refresh token returned in JSON body (not cookie — CLI path)

## /auth/logout contract

```
POST /auth/logout
Authorization: Bearer <access-token>

→ 204  (session invalidated, or was already invalid)
```

Idempotent. Revokes the refresh session in DB. Returns 204 whether or not
the session existed — client proceeds to clear local state regardless.

## Implementation Checklist

- [x] **M2-A1** `refresh.go` — validate refresh token, mint fresh pair, rotate in DB, return JSON
- [x] **M2-A2** `logout.go` — revoke refresh session; idempotent 204
- [x] **M2-A3** `service.go` — register `/auth/refresh` and `/auth/logout`
- [x] **M2-A4** `main.go` — wire auth service
- [x] **Build gate:** `cd controller && go build ./...` passes
