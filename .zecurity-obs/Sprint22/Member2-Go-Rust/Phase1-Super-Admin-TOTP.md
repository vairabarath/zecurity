---
type: phase
member: M2
person: Barath
sprint: 22
phase: 1
execution: T
title: Super-Admin TOTP and Break-Glass Recovery
status: implemented   # 2026-10-26 on docs/sprint22-relay-management-plan, not committed; T8 and live AT-T open
depends_on: []   # first phase; blocks A, E, X, D (D-33)
schema_change: true   # 042_provider_totp.sql
tags:
  - go
  - react
  - provider
  - totp
  - mfa
  - audit
  - provider-dashboard
---

# Phase 1 (T) — Super-Admin TOTP and Break-Glass Recovery

> **Decision Record:**
> - **D-28** (TOTP mandatory for super-admin before dangerous mutations; recovery codes, rotation, refresh tokens deferred);
> - **D-33** (TOTP ships before any console relay mutation);
> - **D-24 / D-29** (Provider Identity Service: one session-issuing path, `amr` claim);
> - **D-25** (credential changes bump `session_generation`);
> - **D-26 note / ADR-029** (break-glass is the `recover-admin` CLI, not an env var);
> - **D-27** (operator lifecycle guards), **D-14** (audit).
>
> **Why first:** Sprint 22 puts revoke/delete in the console. A stolen super-admin password must not be enough to use them.

## Problem (verified 2026-10-26)

- Provider login is password-only. Tokens carry `amr:["pwd"]` (`internal/provider/identity.go` `AMRPassword`, `session.go` `ProviderClaims.AMR`).
- `provider_users` has no MFA columns (`migrations/037_provider_local_auth.sql`).
- `RequireProvider` checks the generation, email and active status, but not how the user authenticated.
- `recover-admin` is documented (ADR-029) but not built, so a sole super-admin who loses access needs SQL.

## Goal

A super-admin can only reach any provider route with a token that proves a TOTP code (`amr` contains `otp`). Enrolment is forced at first login, codes can't be replayed or brute-forced, a lost authenticator can be recovered (by another super-admin, or by `recover-admin` on the controller host), and every step is audited.

## Implementation choices

See [[Sprint22/path]] → *Implementation choices* (TOTP algorithm, storage, replay protection, login flow, enforcement, rate limit, enrolment secret, QR rendering, TOTP reset, break-glass).

**Decisions (2026-10-26, recorded in [[Sprint22/path]] → Open questions):** OQ-1 **yes** (super-admin may reset another operator's TOTP, with a reason); OQ-2 **no** mandatory TOTP for relay-ops (voluntary enrolment not built); OQ-3 **no grace period** (pre-deploy super-admin sessions rejected on their next request).

**The rule:** an operator *requires TOTP* if `role = super-admin` or TOTP is enabled on the account. `IssueSession` refuses a full session without `otp` for such an operator, and `RequireProvider` rejects such an operator's full token without `otp`. Both read the DB row.

## Schema — `042_provider_totp.sql` `[schema: reset DB]`

```sql
ALTER TABLE provider_users
    ADD COLUMN totp_secret_ciphertext TEXT,
    ADD COLUMN totp_secret_nonce      TEXT,
    ADD COLUMN totp_enabled_at        TIMESTAMPTZ,
    ADD COLUMN totp_last_step         BIGINT;
```

A pending enrolment (secret issued, not yet confirmed) has a ciphertext but `totp_enabled_at IS NULL`; a new `/enroll` call replaces it.

## Routes

| Route | Token accepted | Body | Result |
|-------|----------------|------|--------|
| `POST /provider/auth/login` (changed) | none | `{email, password}` | As today, plus a `mfa` field: requires TOTP and enrolled → `mfa:"verify"` token (5 min); super-admin not enrolled → `mfa:"enroll"` token. `pwc` still takes precedence. Otherwise a full token (relay-ops without TOTP). |
| `POST /provider/auth/password` (changed) | full or `pwc` | `{current_password, new_password}` | As today, but an operator who requires TOTP gets an `mfa` token (verify or enroll), never a full one. |
| `POST /provider/auth/totp/verify` | `mfa:"verify"` | `{code}` | **200** full token (`amr:["pwd","otp"]`); audit `provider_session.mfa_verify`. Wrong or replayed code → 401 `invalid_code`; limit → 429 `too_many_attempts`. |
| `POST /provider/auth/totp/enroll` | `mfa:"enroll"` | — | **200** `{secret, otpauth_uri}`, `Cache-Control: no-store`. Replaces any pending secret; already enabled → 409 `totp_already_enabled`. |
| `POST /provider/auth/totp/enroll/confirm` | `mfa:"enroll"` | `{code}` | **200** full token; sets `totp_enabled_at` and `totp_last_step`, bumps the generation; audit `provider_user.totp_enroll`. Wrong code → 401 `invalid_code`; rate-limited like verify. |
| `POST /provider/users/{id}/reset-totp` | full super-admin token (so TOTP-backed) | `{reason}` | **204**; clears the enrolment, bumps the generation; audit `provider_user.totp_reset` with `{email, reason}`. 400 `reason_required` / `reason_too_long`; 404 `not_found`; 409 `cannot_modify_self` / `account_disabled` / `totp_not_enrolled`; relay-ops → 403. |

`RequireProvider`: an operator who requires TOTP and presents a full token without `otp` → **403 `mfa_required`**; an `mfa` token outside its own routes → 403 `mfa_required`.

## CLI — `zecurity-controller provider recover-admin --email <email>`

Runs on the controller host with the controller's DB config. For an existing **super-admin** only: new temporary password (printed once to stdout, never logged), `must_change_password=true`, TOTP cleared, generation bumped, audit `provider_user.recover_admin` with actor `system:cli`. Refuses non-super-admins and unknown emails with a non-zero exit code.

## Console

- Login: after the password, a **code step** (6-digit input, numeric keyboard on phones) for `mfa:"verify"`; an **enrolment screen** for `mfa:"enroll"` (QR + Base32 text, "enter the current code to confirm").
- `403 mfa_required` anywhere → back to the code step (not a generic error).
- After a password change, the same code or enrolment step when the response carries `mfa`.
- Operators page: **Reset MFA** row action (OQ-1) with a required reason, never on your own row; `totp_enabled` shown per operator.
- The enrolment secret lives only in component state and is cleared on unmount; never in `sessionStorage`, the URL or logs (static guard extended).
- API-surface pin: add the three `/auth/totp/*` writes and `reset-totp`.

## Tests (write first)

**Go (DB- and Valkey-backed):**
- RFC 6238 appendix B vectors (SHA-1); ±1 step accepted, ±2 rejected.
- Replay: the same code twice → second rejected; an older step after a newer one → rejected.
- Rate limit: 5 bad codes → 429; a trip writes `provider_auth.totp_rate_limit`; a good code resets nothing about the trip window.
- Encryption: round trip with context `"provider-totp:"+id`; a different user's context fails to decrypt.
- Login matrix: relay-ops without TOTP → full token `amr:["pwd"]`; super-admin not enrolled → `mfa:"enroll"`; enrolled → `mfa:"verify"`; `must_change_password` → `pwc` first; a demoted super-admin with TOTP enabled → `mfa:"verify"`.
- **Issuance guard:** `IssueSession` refuses a full session without `otp` for a super-admin and for any operator with TOTP enabled.
- **Old sessions (OQ-3):** a pre-Phase-T super-admin token (`amr:["pwd"]`, valid generation, unexpired) → 403 `mfa_required` on its next request.
- **Password change can't bypass:** a super-admin (forced or voluntary change) receives an `mfa` token, not a full one.
- Middleware: super-admin token without `otp` → 403 `mfa_required` on every `/provider/*` route except the TOTP routes; relay-ops without TOTP → allowed; relay-ops with TOTP enabled and no `otp` → 403.
- `mfa` token confinement: rejected on all other routes; expires after 5 min.
- Reset TOTP: relay-ops → 403; self → 409; disabled target → 409; not enrolled → 409; missing/blank/over-long reason → 400; success clears all TOTP columns, bumps the generation (target's tokens 401), audits actor, target and reason; target's next login → `mfa:"enroll"`; no audit row on any refusal.
- Recovery boundaries: no route lets an operator reset their own TOTP; an `mfa:"enroll"` token can't be obtained by an enrolled operator.
- `recover-admin`: resets password + TOTP, audit row, refuses non-super-admin, prints the password once.
- No secret or code in any audit row or log line (grep test over captured logs).

**Console (Vitest):**
- Code step and enrolment flows; `mfa_required` routing; secret never in storage or URL; Reset MFA row action; API-surface pin.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-T1 … AT-T9**.

## Test gate

Go gate (incl. `internal/middleware`), console gate, schema gate (`042_`), all from [[Sprint22/path]] → *Test gates*. Plus a manual check with a real authenticator app (Google Authenticator or Aegis) on the lab.

## Implementation record (2026-10-26)

**Files:**
- Controller: `migrations/042_provider_totp.sql`; `internal/provider/totp.go` (RFC 6238), `store_totp.go`, `totp_handlers.go` (verify, enroll, confirm, operator reset, `RecoverAdmin`), `identity.go` (`IssueSession` guard, `IssueMFAToken`), `session.go` (`mfa` claim), `auth_handlers.go` (`sessionAfterPassword` for login and password change), `ratelimit.go` (`NewValkeyTOTPLimiter`), `store.go` (`totp_enabled_at`), `operator_handlers.go` (`totp_enabled`, `reset-totp`), `internal/middleware/provider.go` (`AllowMFAToken`, per-request TOTP rule), `cmd/server/main.go` (routes, CLI dispatch), `cmd/server/recover_admin.go`.
- Console: `src/pages/Mfa.tsx`, `src/auth/{session,flows,guards}.ts(x)`, `src/api/{auth,operators,client,types}.ts`, `src/pages/{ChangePassword,Login,ProviderUsers}.tsx`, `src/components/OperatorDialogs.tsx` (`ResetMfaDialog`), `src/App.tsx`, `src/lib/operator-errors.ts`; dependency `qrcode.react@4.2.0` (exact pin, no runtime deps).

**Test-first evidence:** the new Go tests first failed to compile (missing `AMROTP`, `IssueMFAToken`, `TOTPEnabledAt`, …), then failed on assertions against signature-only stubs. The 12 DB-backed tests all stopped at the same check: a super-admin's password change returned a full token, the bypass this phase removes. The console tests failed for missing headings, menu items and calls before the implementation.

**Existing tests changed** (all because super-admin password-only sessions are now refused, by design):
- e2e harnesses complete the TOTP step through the real routes (`completeMFA`);
- token fixtures give super-admins `["pwd","otp"]`;
- `TestLogin_Success_Audited` uses relay-ops, where a password-only full session is still correct;
- `TestChangeRole_EndsSessions` now asserts a promoted operator must enrol first;
- `TestRequireProvider_ValidTokenInjectsActorFromDB` adds a check that the TOTP rule also reads the DB row.

## Post-Phase Fixes

*(None yet.)*
