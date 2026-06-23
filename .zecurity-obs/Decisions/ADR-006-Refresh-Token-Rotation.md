---
type: decision
status: proposed
date: 2026-06-10
related:
  - "[[CodeStudy/Auth-Audit]]"
tags:
  - adr
  - controller
  - auth
  - refresh
  - session
  - oauth
  - security
---

# ADR-006 — Refresh Token Rotation + Absolute Lifetime Cap

## Context

The auth audit of `refresh.go` + `valkey.go` surfaced two coupled behaviors that together
let a stolen refresh-token cookie act as a **permanent session**:

### Behavior 1 — Refresh tokens are not rotated on use

[`refresh.go:28`](controller/internal/auth/refresh.go#L28) comment:
> "The refresh token itself is NOT rotated on every refresh."

After successful validation, the same token stays in Redis. The cookie is unchanged.
A future refresh presents the same value again.

### Behavior 2 — TTL slides forward indefinitely on each use

[`refresh.go:116-120`](controller/internal/auth/refresh.go#L116-L120):
```go
ttl, perr := time.ParseDuration(s.cfg.JWTRefreshTTL)
if perr != nil {
    ttl = 7 * 24 * time.Hour
}
s.redisClient.SetRefreshToken(ctx, userID, cookieToken, ttl)
```

Each successful refresh extends the Redis TTL by another full window (default 7 days)
**from now**. There is no record of the original issue time, so no absolute lifetime
cap can be enforced.

### Resulting threat

If an attacker steals the refresh cookie (e.g. via a one-shot XSS bug elsewhere on the
SPA, a browser exploit, or a compromised proxy) the cookie continues to work for as long
as the attacker keeps using it within the 7-day rolling window. There is no detection
signal, no rotation, no absolute cap, no theft mitigation.

This is the most significant current security gap surfaced in the auth audit.

### What OAuth 2.0 best practice says

RFC 8252 (OAuth 2.0 for Native Apps) and RFC 6749 §10.4 both recommend:
1. **Rotate refresh tokens on every use** — issue a new token, invalidate the old.
2. **Detect reuse of a rotated token** — if the previous value is presented again, it
   indicates either a buggy client OR theft; the safe response is to revoke the entire
   token family for that user.
3. **Cap absolute lifetime** — even with rotation, an unbounded lifetime is risky. Force
   re-authentication after a fixed window (e.g. 30 days) regardless of activity.

## Decision

Implement **refresh token rotation with an absolute lifetime cap**, in two pieces:

### Piece A — Rotate on every use (replace-on-refresh)

On each successful refresh:
1. Generate a NEW random 256-bit token.
2. Replace the stored token in Redis with the new value (under the same `refresh:<userID>` key).
3. Set the new cookie with the new value.
4. The OLD token is now invalid — any future request presenting it fails the
   constant-time compare.

The same `subtle.ConstantTimeCompare` against the stored value naturally enforces this —
once rotated, the old value no longer matches.

### Piece B — Absolute lifetime cap (30 days from initial issue)

Augment the Redis payload from `<token_string>` to `<JSON: {token, original_iat, max_lifetime_at}>`:
- `original_iat`: Unix timestamp of the initial login.
- `max_lifetime_at`: `original_iat + 30 days`.

On refresh, before issuing a new token:
- If `now() > max_lifetime_at` → reject, require full login.
- Otherwise, keep `original_iat` unchanged; carry it forward into the new payload.

The rolling 7-day TTL on the Redis key still applies (idle expiry), but the absolute cap
limits the worst-case session lifetime to 30 days from initial OAuth signin.

### Piece C — Defer reuse detection (RFC's "family revocation")

Real reuse detection requires tracking which old tokens were rotated AWAY from. That
needs either:
- A history in Redis of rotated-out tokens per user, OR
- A token-family identifier (e.g. `jti`) and a separate "revoked family" set.

Both add complexity. For this ADR, **defer** to a future ADR-007 if/when an incident
suggests it's worth the cost. Piece A + Piece B together address the most common
exploitation paths.

## Alternatives Considered

### Alt A — Hash refresh token before Redis storage (rejected as primary fix)

Store SHA-256(token) instead of the token itself. Compare hashed cookie value on refresh.

**Why not now**: orthogonal concern (defense against Redis compromise). Doesn't address
the rotation/lifetime problem. Worth doing later, but not what this ADR is about.

### Alt B — Switch to opaque tokens with database-backed storage (rejected)

Use a database table with full token lifecycle: issued_at, last_used_at, revoked_at, etc.

**Why not**: Redis is already the storage; migrating to DB adds latency to every refresh
and overlap with Redis isn't worth it for this fix.

### Alt C — Drop refresh tokens entirely; require login every 15 minutes (rejected)

**Why not**: terrible UX. The 15-minute access TTL with refresh is the standard
short-access + long-refresh model. Don't break the model; fix it.

## Consequences

### Wins
- Stolen refresh cookie is invalidated the next time the real user refreshes.
- Absolute lifetime cap (30 days) bounds the worst-case session theft window.
- Aligns with OAuth 2.0 BCP / RFC 8252.
- No new dependencies; reuses existing Redis storage.

### Costs
- Cookie value changes on every refresh — frontend must accept this (HttpOnly cookie
  is set by `Set-Cookie` header on every refresh response; browser handles transparently).
- Redis stored payload changes from string to JSON — small migration concern; need
  backward-compat read path.
- Users are forced to re-authenticate every 30 days regardless of activity. Acceptable
  per industry norms (most SaaS does 30-90 days).

### Risks
- **Token rotation race**: if two clients (browser tabs) refresh simultaneously,
  one rotation can overwrite the other's not-yet-set cookie. Mitigation: use Redis
  atomic `SET ... XX` (only if exists) — but the simpler model just accepts that
  one tab gets logged out on next refresh. This already happens in the current
  single-session model.
- **Backend redeploy during transition**: tokens issued under the old format must
  still be accepted briefly. Backward-compat parser handles this.

## Plan

### Phase 1 — Data model
1. Modify `valkey.go::SetRefreshToken` to accept and store the JSON payload.
2. Modify `valkey.go::GetRefreshToken` to return `{token, original_iat, max_lifetime_at}`.
3. Backward-compat: if stored value is a bare string (old format), parse it as the
   token with `original_iat=0` (treated as "old; will rotate to new format on next use").

### Phase 2 — Refresh handler rotation
1. In `refresh.go::RefreshHandler`:
   - After constant-time compare succeeds, check `now() <= max_lifetime_at`.
   - Generate new 256-bit token.
   - SET in Redis with `{new_token, original_iat (preserved), max_lifetime_at (preserved)}`.
   - Set new cookie via `http.SetCookie(w, ...)` on the response.
   - Return new access JWT as before.

### Phase 3 — Initial issuance
1. In `session.go::issueRefreshToken`:
   - Compute `original_iat = now()` and `max_lifetime_at = now + 30 days`.
   - SET the JSON payload in Redis.

### Phase 4 — Test coverage
- Test that rotated cookie value differs from previous.
- Test that stale (previous) cookie value fails after one rotation.
- Test that after 30 days from `original_iat`, refresh is rejected.
- Test backward-compat read of legacy string-format Redis value.

## Configuration

Two new (or repurposed) config values:
- `JWT_REFRESH_TTL` (existing — 7 days) — idle TTL on the Redis key, controls auto-expiry.
- `JWT_REFRESH_MAX_LIFETIME` (new — default 30 days) — absolute cap from initial issuance.

## Why 30 days for max lifetime?

30 days is the **industry middle-ground default**, not derived from any one
vendor. The choice balances three tensions:

1. **User friction** — too short = constant re-login complaints.
2. **Stolen-cookie blast radius** — too long = stolen refresh works longer.
3. **Audit defensibility** — picking a wildly outside-norm value invites review questions.

### Reference points from mainstream auth providers

| Provider | Refresh / session policy |
|----------|--------------------------|
| AWS Cognito | **30 days** default for refresh tokens |
| Auth0 | **30 days** default for rotating refresh tokens |
| Azure AD / Entra ID | **90 days** for typical user sessions (with conditional access overrides) |
| Okta | Configurable; typically **90 days** default |
| GitHub OAuth Apps | **6 months** refresh |
| Google Identity (consumer) | Effectively no expiry; **6 months of inactivity** invalidates |

### Rough ZTNA-vendor positioning (verify against current docs)

These are not cited numbers — verify before quoting in any external doc.

| Vendor | What I recall |
|--------|---------------|
| **Twingate** | Device session tied to device cert validity (typically 30–90 days); admin-configurable "Authentication Frequency" per resource (6h to 30d) |
| **Cloudflare Access** | Default app session **24h**, max configurable to **1 month** |
| **Firezone** | Shorter — refresh window in hours; favors frequent SSO re-auth |
| **Tailscale** | Device key default **180 days**, admin-configurable |

### Mapping vendor concepts to our settings

What competitors call "session length" rarely maps 1:1 to our model. Three layers:

| Layer | What it is | Our value |
|-------|------------|-----------|
| Access token TTL | Per-request auth credential | 15 min |
| Refresh idle TTL | Logged-out-after-X-days-of-inactivity | 7 days |
| Refresh absolute max | Hard cap regardless of activity | **30 days** |
| Device cert validity | mTLS client cert lifetime | Separate (PKI service) |

A competitor's published "session" value typically maps to either our **idle TTL**
(if they describe an inactivity cutoff) or our **absolute max** (if they describe a
forced re-auth cadence).

### Why we landed at 30 days

- Matches AWS Cognito + Auth0 defaults exactly — "normal" for an auth audit.
- Slightly stricter than Azure/Okta (90d) — reasonable for a ZTNA product where
  security posture is the differentiator.
- Looser than Firezone-style hours — avoids user-friction complaints.
- Equal to or shorter than Cloudflare Access max — keeps us defensible in
  ZTNA-vendor comparisons.

### When to revisit

- **Regulatory requirements** (SOC 2 Type II, HIPAA, FedRAMP) may mandate
  shorter values; revisit if/when those audits scope us.
- **Customer pushback** during real deployment: if users complain about monthly
  re-auth, consider 60d.
- **Per-workspace configurability** could land in a future ADR if different
  customer tiers want different policies.

## Notes

- This ADR closes audit findings **P9-F2** and **P9-F3** (the audit's most significant
  open finding).
- This ADR does NOT address **P9-F7** (refresh tokens stored as plaintext in Redis) —
  that's defense-in-depth, separate concern, future ADR.
- This ADR does NOT address **P9-F1** (no explicit `alg` check in refresh's JWT-parse
  keyFunc) — that's a 3-line defensive fix bundled with this PR but conceptually separate.
