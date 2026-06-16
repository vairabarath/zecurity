---
type: decision
status: accepted
date: 2026-06-10
implemented: 2026-06-10
related:
  - "[[Decisions/ADR-006-Refresh-Token-Rotation]]"
tags:
  - adr
  - controller
  - auth
  - jwt
  - configuration
  - security
  - hardening
---

# ADR-007 — JWT Secret Must Be At Least 32 Bytes At Startup

## Context

The audit's adversarial re-pass surfaced that
[`controller/internal/auth/config.go::NewService`](controller/internal/auth/config.go)
validated `JWTSecret` for **non-empty only**:

```go
if cfg.JWTSecret == "" {
    return nil, fmt.Errorf("auth: JWTSecret is required")
}
```

No length check, no entropy check, no warning. An operator could deploy with
`JWT_SECRET=x` (1 byte) and the controller would happily start. All access
JWTs, refresh-flow JWT verifications, OAuth state HMACs, and middleware
verifications use this secret.

### Why this is dangerous

`JWT_SECRET` is the single most catastrophic key in session security:

| Use site | What it protects |
|----------|------------------|
| `session.go::issueAccessToken` | Signs every user's access JWT |
| `middleware/auth.go::AuthMiddleware` | Verifies every authenticated request |
| `refresh.go::RefreshHandler` | Verifies the (expired) JWT presented at refresh |
| `oidc.go::generateSignedState` / `verifySignedState` | OAuth state CSRF protection |

A weak `JWT_SECRET` lets an attacker who guesses or brute-forces it:
1. Forge an access JWT for any user (any role, any tenant)
2. Pass the middleware verification check
3. Bypass OAuth state CSRF protection

This is a **total auth bypass** scenario.

### Brute-force cost for HMAC-SHA256

For HMAC-SHA256, the attacker has plenty of input — every JWT they observe is
a `(message, HMAC)` pair they can verify guesses against offline:

| Key | Time to brute-force |
|-----|---------------------|
| 1 char (~6 bits) | Milliseconds |
| 6 chars (~36 bits) | Seconds on a GPU |
| 8 chars (~48 bits) | Minutes on a GPU |
| 16 chars (~96 bits) | Practically infeasible |
| **32 bytes (256 bits)** | Heat death of the universe |

### Why we'd ever ship without this check

Pre-this-ADR, the only gate was the empty-string check. Plausible misconfig
paths that produced a working-but-broken controller:
- A tutorial copy-paste with `JWT_SECRET=changeme`
- A CI/CD bug producing a short literal
- A demo environment with `JWT_SECRET=demo`
- Re-using a different secret (e.g. cookie key) that happens to be short

Any of these reaches production = total auth compromise.

## Decision

**Reject startup if `JWT_SECRET` is shorter than 32 bytes.**

Fail-fast with an actionable error message that tells the operator how to
generate a correct secret.

The constant `minJWTSecretBytes = 32` is defined alongside `NewService` in
`config.go` so the policy is colocated with the check.

## Alternatives Considered

### Alt A — Entropy check (rejected)

Compute Shannon entropy or shannon-like measure on the secret and reject low
entropy. E.g. reject `"aaaaaa...aaaa"` (32 chars but no entropy).

**Why not**: false-positive risk — a base64-encoded random secret looks
patterned to naive entropy measures. The operational floor (32 bytes) catches
the vast majority of real misconfigurations; entropy checks add complexity
and noisy errors for cosmetically-correct secrets.

### Alt B — Warning instead of hard rejection (rejected)

Log a warning at startup, allow operation.

**Why not**: warnings are routinely ignored. The whole point of validation is
to prevent deployment of a weak secret. A blocking error forces correction
before any user can sign in.

### Alt C — Require a specific format (e.g. base64-encoded) (rejected)

Insist the secret be base64-encoded N bytes of random output.

**Why not**: imposes a format the operator must encode/decode. A 32-byte
secret in any encoding (hex, base64, raw bytes from `openssl rand -hex 32`)
provides the same security. Forcing a format adds friction without security
benefit.

### Alt D — 64 bytes minimum (rejected as overkill today)

Some auth providers recommend 64 bytes (the SHA-256 block size, ideal HMAC
key length).

**Why not now**: 32 bytes (256 bits) is already past the security-budget
horizon for SHA-256. Going to 64 bytes is defense-in-depth but yields no
practical advantage given current crypto. If/when a future migration off
HS256 happens (e.g. to RS256 with longer keys), we can raise this.

## Consequences

### Wins
- Operator-level misconfigurations (short / guessable secret) caught at startup.
- Error message tells operator exactly how to fix it (`openssl rand -base64 48`).
- Matches "fail closed, not silent" pattern used elsewhere in the codebase.
- No runtime cost — single check at service init.

### Costs
- One additional startup validation. Negligible.
- Test fixtures that used short test secrets must be updated.
  - Updated: `integration_test.go` (`phase-7-auth-jwt-secret` → `phase-7-auth-jwt-secret-32-bytes!!`).
  - Already passing: `helpers_test.go` already uses a 32-byte secret.

### Risks
- An operator running the controller with `JWT_SECRET=x` will now see
  startup fail with a clear error. This is the **intended** behavior — the
  alternative is silent acceptance and a forgeable token system. Not actually
  a risk in the negative sense.

## Plan

### Phase 1 — Add the check (done in this PR)
1. Define `minJWTSecretBytes = 32` in `config.go`.
2. After the existing non-empty check, validate length.
3. Return descriptive error with remediation hint on failure.

### Phase 2 — Fix test fixtures (done in this PR)
- `integration_test.go::JWTSecret` extended to 34 bytes (was 23).
- Other test files already use 32-byte values.

### Phase 3 — Documentation (this ADR)
- Documented decision + rationale + alternatives.

### Phase 4 (future) — Ops documentation
- Add a note in the deploy guide instructing operators to use
  `openssl rand -base64 48` (yielding ~64 random bytes) for `JWT_SECRET`.
- Out of scope for this ADR.

## Notes

- Closes audit finding **NEW-F2** from the adversarial re-pass.
- Does NOT address JWT_SECRET rotation, KMS/HSM-backed storage, or
  per-environment key isolation — those are separate concerns.
- The check applies only to the controller's session JWT secret. Other keys
  (workspace CA private key, PKI intermediate, refresh token entropy from
  `crypto/rand`) have their own length/entropy guarantees.
