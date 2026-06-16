---
type: code-study
flow: auth-audit-end-to-end
created: 2026-06-10
status: complete
related:
  - "[[Decisions/ADR-005-Email-Normalization]]"
  - "[[Decisions/ADR-006-Refresh-Token-Rotation]]"
  - "[[Decisions/ADR-007-JWT-Secret-Minimum-Length]]"
  - "[[CodeStudy/01-Auth-Flow]]"
  - "[[CodeStudy/02-Invite-Flow]]"
---

# Code Study 06 — Auth Flow Security Audit

> Adversarial security audit of the controller's authentication flow:
> bootstrap, OAuth callback, ID-token verification, token exchange, JWT
> issuance, refresh handler, storage, and middleware verifier.
>
> Goal: pieces clean enough that an external tester would find nothing
> actionable. Audit covered 10 pieces; landed 3 ADRs and ~12 code edits.

---

## Scope

| Piece | File | Purpose |
|-------|------|---------|
| 1 | `bootstrap.go::Bootstrap` lookup | Returning-user lookup + `last_login_at` |
| 2 | `bootstrap.go` pending-invite + `runInvitedUserTransaction` | Invited-user binding |
| 3 | `bootstrap.go::runBootstrapTransaction` | New-workspace creation |
| 4 | `bootstrap.go::slugify` | Slug derivation |
| 5 | `auth/callback.go::CallbackHandler` | OAuth callback orchestration |
| 6 | `auth/idtoken.go::VerifyGoogleIDToken` | Google ID token signature + claims |
| 7 | `auth/exchange.go::exchangeCodeForTokens` | Google token exchange |
| 8 | `auth/session.go::issueAccessToken/issueRefreshToken` | JWT + refresh issuance |
| 9 | `auth/refresh.go::RefreshHandler` + `auth/valkey.go` | Refresh flow + storage |
| 10 | `middleware/auth.go::AuthMiddleware` | Production JWT verifier |

## Methodology

For each piece:
1. Read the code with strict "current exploitable bug" filter (latent/defense-in-depth notes
   recorded but not counted as actionable findings).
2. Cross-check claims in comments against implementation.
3. Trace inputs from trust boundaries.
4. Then a final adversarial re-pass looking for things missed.

---

# Piece 1 — Returning-user lookup

[`bootstrap.go:39-66`](controller/internal/bootstrap/bootstrap.go#L39-L66)

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P1-F1 | 🟡 latent | Deferred | `LIMIT 1` no `ORDER BY` — non-deterministic if multi-row scenario ever becomes reachable; bootstrap currently caps each Google account to one workspace so latent only |
| P1-F2 | 🟡 hygiene | ✅ Fixed | `fmt.Printf` → `log.Printf("bootstrap: ...")` |
| P1-F3 | 🟠 def-in-depth | Deferred (with code comment) | No `users.status`/`workspaces.status` filter on login. Latent because no "Suspend User/Workspace" feature exists today. Code comment added at the lookup site referencing this for future implementers |
| P1-F4 | 🟡 perf | ✅ Fixed | `last_login_at` UPDATE moved to goroutine with `context.Background()` |
| P1-F5 | 🟡 def-in-depth | ✅ Fixed | Added `provider != "google"` allowlist check at function entry |

### What's verified clean

- Index `idx_users_provider_sub` covers the lookup; no seq scan
- Error path doesn't leak DB schema details to user

---

# Piece 2 — Pending-invite lookup + `runInvitedUserTransaction`

[`bootstrap.go:73-117`](controller/internal/bootstrap/bootstrap.go#L73-L117) + [`:205-251`](controller/internal/bootstrap/bootstrap.go#L205-L251)

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P2-F1 | 🟠 → closed | Closed (consumer error) | Invite-token bypass: bootstrap matches by email only. Exploit requires admin to typo an invite email + attacker to control that email at Google. Resource access additionally gated by ACL group membership, so non-exploitable today |
| P2-F2 | 🔴 → ✅ | Fixed via [[ADR-005]] | Case-sensitive email match in pending-invite lookup. Mixed-case admin invite ≠ lowercase Google email → user creates NEW workspace as admin |
| P2-F3 | 🟡 ops | Deferred (not security) | `workspace_members.status` stays `'invited'` after binding. Users have working JWT but invisible in admin "active members" view |
| P2-F4 | 🟠 def-in-depth | Deferred | `role` flows from DB to `users` INSERT without re-validation. Latent because invite-creation hardcodes `'member'` |
| P2-F5 | 🟡 latent | Deferred | `LIMIT 1` no `ORDER BY` for multi-workspace pending invites |
| P2-F6 | 🟡 latent | Deferred (with P1-F3) | No workspace status check |
| P2-F7 | 🟠 ops | Deferred | No invite expiry check in bootstrap. `invitations.expires_at` enforced only by AcceptInvitation; bootstrap queries workspace_members which has no expiry. Stale invites = dormant access |
| P2-F8 | 🟠 race | Deferred | TOCTOU race between pending-invite lookup and `runInvitedUserTransaction` |
| P2-F9 | 🟡 race | Deferred | UPDATE workspace_members missing `user_id IS NULL` guard |
| P2-F10 | 🟡 hygiene | Deferred | UPDATE result (`RowsAffected`) not checked |
| P2-F11 | 🟡 hygiene | Deferred | INSERT users doesn't `RETURNING role` — function returns param role |

### Decisive insight

Resource access in this codebase is gated by **ACL group membership**, not just JWT
presence. A typo'd/orphaned invite that auto-binds via the bootstrap path produces a
user with a JWT but **no group membership** → ACL contains no entries permitting their
SPIFFE ID → connector rejects every connection. So most "invite path looseness" findings
are downgraded; only the actual case-sensitivity bug (P2-F2) was a real-today data bug.

---

# Piece 3 — New-workspace creation transaction

[`bootstrap.go:96-200`](controller/internal/bootstrap/bootstrap.go#L96-L200)

### Findings (under strict "current security only" criteria)

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P3-F1 | 🟡 ops | Deferred | TOCTOU race: same Google account double-tapping OAuth could race past the lookup at `:39` and create TWO workspaces. Race window sub-second; result is duplicate workspaces for one user, not unauthorized access |

### Things verified clean

- Slug controllability: `slugify` restricts to alphanum + hyphens
- Workspace name stored as-is: frontend renders via React (auto-escapes)
- CA generation outside `tx`: PKI is pure crypto, doesn't write DB; bootstrap INSERTs result inside `tx` (atomic)
- Anyone with Google account can create workspace → by design (open signup)
- First user is admin → by design (workspace creator owns workspace)

**Zero actionable security findings in Piece 3.**

---

# Piece 4 — `slugify`

[`bootstrap.go:253-277`](controller/internal/bootstrap/bootstrap.go#L253-L277)

| Concern | Verdict |
|---------|---------|
| Injection via `name` | Safe — alphanum + `-` only |
| Path traversal / URL safety | Safe — no `/`, `..`, control chars possible |
| SQL injection | n/a — caller parameterizes |
| XSS via slug | Safe — alphanum + hyphens are HTML-safe |
| Empty string `"workspace"` fallback | UX/availability concern only |
| Non-Latin letters | Accepted; URL would percent-encode. UX issue, not security |
| Trust domain derivation | Safe — `ws-<slug>.zecurity.in` is well-formed |

**Zero security findings.**

---

# Piece 5 — `callback.go::CallbackHandler`

[`auth/callback.go`](controller/internal/auth/callback.go)

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P5-F1 | 🟡 ops | Deferred | `time.ParseDuration(s.cfg.JWTRefreshTTL)` error ignored. If config is malformed, refresh cookie becomes session-only silently |

### Things verified clean

- State HMAC verification with `hmac.Equal` (constant-time)
- GETDEL atomic on Redis PKCE state (replay protection)
- PKCE `code_verifier` proves server-side origin
- ID token verified before claim use (signature, aud, iss, exp, email_verified, sub)
- `client_secret` never reaches browser (server-to-server exchange)
- httpOnly + Secure + SameSite=Strict + Path-scoped cookie attributes
- JWT delivered via URL fragment (not sent to server logs)
- `fail()` redirect uses config-controlled `AllowedOrigin`, not user input — no open redirect
- Generic error codes in `fail()` (no internal details leaked)

---

# Piece 6 — `idtoken.go::VerifyGoogleIDToken`

[`auth/idtoken.go`](controller/internal/auth/idtoken.go)

### All 6 documented checks verified

| Check | Implementation |
|-------|----------------|
| Signature valid | `jwt.ParseWithClaims` with key from JWKS |
| Signing alg = RS256 only | keyFunc rejects non-RSA (blocks `alg=none` and `HS256` confusion) |
| `aud` matches clientID | `GetAudience()` + `containsString` |
| `iss` is Google | Both `accounts.google.com` and `https://accounts.google.com` accepted |
| Expiry required | `jwt.WithExpirationRequired()` + `token.Valid` |
| `email_verified` true | Explicit check |
| `sub` non-empty | Explicit check |

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P6-F1 | 🟠 availability | ✅ Fixed | No HTTP timeout on JWKS fetch — `http.DefaultClient` would hang on slow JWKS endpoint. Replaced with dedicated `jwksHTTPClient` with `Timeout: 10s` |
| P6-F2 | 🟡 def-in-depth | Deferred | No body size limit on JWKS response decode |
| P6-F3 | 🟡 hygiene | Deferred | Response status not explicitly checked before JSON decode (caught implicitly by empty-keys error) |

---

# Piece 7 — `exchange.go::exchangeCodeForTokens`

[`auth/exchange.go`](controller/internal/auth/exchange.go)

### Things verified clean

- Explicit 10s timeout on HTTP client
- `NewRequestWithContext` for ctx propagation
- HTTPS endpoint
- Status code check before JSON decode
- PKCE `code_verifier` sent server-side
- `id_token` presence checked after decode
- TLS verification via default `http.Client`

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P7-F1 | 🟡 maintenance | Deferred | `ExchangeCode` and `exchangeCodeForTokens` are 95% duplicated — security fix has to land in two places |
| P7-F2 | 🟡 def-in-depth | Deferred | No body size limit on JSON decode |
| P7-F3 | 🟡 hygiene | Deferred | Error-body decode error swallowed; non-JSON errors logged as `body=map[]` |

---

# Piece 8 — `session.go::issueAccessToken/issueRefreshToken`

[`auth/session.go`](controller/internal/auth/session.go)

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P8-F1 | 🟡 design | Acknowledged | Refresh storage keyed by `user_id` → single-session-per-user model; login on device B overwrites device A |
| P8-F2 | 🟠 risk | Acknowledged | Two JWT verifiers (`session.go::verifyAccessToken` + `middleware/auth.go::AuthMiddleware`) must stay in sync. Production uses middleware — audited in Piece 10 |
| P8-F3 | 🟡 ops | Deferred | `time.ParseDuration` errors silently fall back to defaults |
| P8-F4 | 🟡 def-in-depth | Deferred | Same `JWTSecret` used for state HMAC and JWT signing — key separation would be more defensive |
| P8-F5 | 🟡 design | Deferred | No `jti` revocation list — suspended users keep working JWTs until expiry (mitigated by 15-min TTL) |
| P8-F6 | 🟡 design | Deferred | No JWT signing-key rotation infrastructure |

---

# Piece 9 — `refresh.go::RefreshHandler` + storage

[`auth/refresh.go`](controller/internal/auth/refresh.go) + [`auth/valkey.go`](controller/internal/auth/valkey.go)

### Findings (the highest-impact piece)

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P9-F1 | 🟠 def-in-depth | ✅ Fixed | Refresh keyFunc didn't explicitly enforce `*jwt.SigningMethodHMAC`. Added the check mirroring `middleware/auth.go` |
| **P9-F2** | 🟠 **real gap** | ✅ **Fixed via [[ADR-006]]** | **Refresh tokens not rotated on use** — stolen cookie kept working until idle TTL expired |
| **P9-F3** | 🟠 **real gap** | ✅ **Fixed via [[ADR-006]]** | **TTL slid forever on each refresh** — no absolute lifetime cap. Combined with F2, a stolen cookie was effectively a permanent session |
| P9-F4 | 🟡 hygiene | Deferred | Email DB-lookup fallback (parameterized; safe but `s.cfg.Pool != nil` smell) |
| P9-F5 | 🟡 ops | Deferred | No rate limiting on refresh endpoint |
| P9-F6 | 🟡 latent | Deferred | User suspension not re-checked on refresh (pairs with P1-F3) |
| P9-F7 | 🟡 def-in-depth | Deferred | Refresh tokens stored as plaintext (not hashed) in Redis |

### ADR-006 implementation summary

| File | Change |
|------|--------|
| `valkey.go` | New `RefreshSession` struct + JSON storage; `SetRefreshSession` / `GetRefreshSession` methods |
| `config.go` | New `JWTRefreshMaxLifetime` config field (default `720h` = 30 days) |
| `session.go` | `issueRefreshToken` stores `original_iat` + `max_lifetime_at` |
| `refresh.go` | Rotates token on every use, enforces absolute cap, sets new cookie, P9-F1 alg check |
| 3 test files | Updated to use new session-based methods |

---

# Piece 10 — `middleware/auth.go::AuthMiddleware`

[`controller/internal/middleware/auth.go`](controller/internal/middleware/auth.go)

### Strongest piece in the chain

| Check | Present? |
|-------|----------|
| HS256 enforced (blocks `alg=none`/RS256 confusion) | ✅ |
| Issuer validation | ✅ |
| Expiry required | ✅ |
| Subject (user_id) required | ✅ |
| TenantID required | ✅ |
| Role required | ✅ |
| Generic 401 on any failure | ✅ |

### Findings

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| P10-F1 | 🟡 def-in-depth | Deferred | `email` not required claim |
| P10-F2 | 🟡 compat | Deferred | Bearer scheme case-sensitive (RFC 6750 says case-insensitive) |
| P10-F3 | 🟡 def-in-depth | Deferred | No allowlist on `role` value |
| P10-F4 | 🟡 design | Deferred | No `jti` revocation (same as P8-F5) |

---

# Adversarial Re-Pass — Final Sweep

After completing Pieces 1-10, a deliberate "what would a tester find that I missed"
sweep surfaced three additional findings.

| ID | Severity | Status | Summary |
|----|----------|--------|---------|
| NEW-F1 | 🟠 → 🟡 | Closed (consumer threat model) | `lookupWorkspacesByEmail` is unauthenticated → email enumeration. Closed because resource access is gated independently and the threat model accepts that workspace membership is not a secret |
| **NEW-F2** | 🟠 | ✅ **Fixed via [[ADR-007]]** | **No minimum length on `JWTSecret`** — empty-only check let `JWT_SECRET=x` pass startup, enabling trivial offline brute-force of forged JWTs |
| NEW-F3 | 🟡 functional | Open | No logout endpoint — `DeleteRefreshToken` method exists but no HTTP handler invokes it; users can clear local JWT but refresh cookie persists |

---

# Cross-Cutting Themes

1. **Latent-feature findings**: many findings (`status='suspended'` checks, role allowlists,
   jti revocation lists) are gated on features that don't exist today. The audit recorded
   them as breadcrumbs for whoever ships those features.

2. **Threat-model alignment**: several findings (P2-F1, NEW-F1) were downgraded because
   resource access in this codebase is gated by ACL group membership, not just JWT presence.
   An auth-only audit overweights these.

3. **Production verifier is the strongest**: `middleware/auth.go` is the strictest JWT
   verifier in the codebase. Other verifiers (test paths, refresh path) had gaps; the
   refresh-path gap (P9-F1) was fixed in this audit.

4. **ADR-driven decisions**: 3 ADRs landed (ADR-005 email normalization, ADR-006 refresh
   rotation, ADR-007 JWT secret length). Each documents what was decided AND what was
   considered-but-rejected, so future maintainers can revisit with context.

---

# Final Scorecard

| Severity | Open | Fixed | Closed (consumer) | Deferred (latent / non-security) |
|----------|------|-------|-------------------|----------------------------------|
| 🔴 Critical | 0 | 0 | — | — |
| 🟠 Significant | 0 | 4 (P2-F2, P6-F1, P9-F1, P9-F2+F3, NEW-F2) | 2 (P2-F1, NEW-F1) | 4 (P1-F3, P2-F4, P2-F7, P2-F8) |
| 🟡 Minor | 1 (NEW-F3) | 4 (P1-F2, P1-F4, P1-F5, +helpers) | — | ~20 |

### Closed audit findings (action taken)
- **ADR-005** — Email normalization at write time
  - `invitation/store.go::CreateInvitation` + `::AcceptInvitation`: lowercase + trim at entry
  - `bootstrap.go::Bootstrap`: same at entry
  - `client/store.go::upsertUser`: same at entry
  - `graph/resolvers/schema.resolvers.go::LookupWorkspacesByEmail`: same on query param
  - `migrations/016_email_lowercase.sql`: backfill historical mixed-case rows

- **ADR-006** — Refresh rotation + absolute lifetime cap (30 days)
  - `valkey.go`: new `RefreshSession` JSON storage
  - `session.go::issueRefreshToken`: tracks `original_iat` + `max_lifetime_at`
  - `refresh.go::RefreshHandler`: rotates on every use, enforces absolute cap, sets new cookie
  - Test fixtures updated across 3 files

- **ADR-007** — JWT secret minimum length (32 bytes) at startup
  - `config.go::NewService`: length validation with actionable error
  - Test secret updated in `integration_test.go`

- **Piece 1 bundle**
  - `bootstrap.go`: `fmt.Printf` → `log.Printf`; `last_login_at` UPDATE in goroutine;
    provider allowlist check; P1-F3 noted in code comment for future Suspend feature

- **P6-F1** — JWKS HTTP timeout via dedicated `jwksHTTPClient`

- **P9-F1** — Explicit HS256 alg check in refresh keyFunc

### Open
- **NEW-F3** — Logout endpoint not implemented. Functional gap with security implications;
  not actively exploitable. Decision deferred.

### Closed (consumer threat model)
- P2-F1 (invite-token bypass) — admin typo + attacker email-control
- NEW-F1 (email enumeration via `lookupWorkspacesByEmail`) — pre-auth public API; closed
  because resource access is independently ACL-gated

### Deferred (latent / not security)
- ~12 findings spanning Pieces 2-10. Documented inline in the relevant code or in the
  per-piece tables above. Mostly defense-in-depth, hygiene, or feature-gated until specific
  admin features ship (Suspend User/Workspace, JWT revocation list, etc.)

---

# Decision References

- [[Decisions/ADR-005-Email-Normalization]] — written + implemented
- [[Decisions/ADR-006-Refresh-Token-Rotation]] — written + implemented (includes
  vendor-comparison rationale for 30-day max lifetime)
- [[Decisions/ADR-007-JWT-Secret-Minimum-Length]] — written + implemented

---

# Files Touched (this audit pass)

```
Modified
  controller/internal/auth/config.go
  controller/internal/auth/idtoken.go
  controller/internal/auth/refresh.go
  controller/internal/auth/session.go
  controller/internal/auth/valkey.go
  controller/internal/auth/integration_test.go
  controller/internal/auth/session_test.go
  controller/internal/auth/valkey_test.go
  controller/internal/bootstrap/bootstrap.go
  controller/internal/client/store.go
  controller/internal/invitation/store.go
  controller/graph/resolvers/schema.resolvers.go

Added
  controller/migrations/016_email_lowercase.sql
  .zecurity-obs/Decisions/ADR-005-Email-Normalization.md
  .zecurity-obs/Decisions/ADR-006-Refresh-Token-Rotation.md
  .zecurity-obs/Decisions/ADR-007-JWT-Secret-Minimum-Length.md
  .zecurity-obs/CodeStudy/06-Auth-Audit.md   (this doc)
```

# Verification

- `go vet ./internal/auth/`, `./internal/bootstrap/`, `./internal/client/`,
  `./graph/resolvers/` → all clean
- `go test ./internal/auth/` — all refresh / session / valkey tests pass.
  One pre-existing failure (`TestNewValkeyClient_Success`) is unrelated to this audit;
  it's about miniredis not supporting `CLIENT TRACKING` and predates these changes
