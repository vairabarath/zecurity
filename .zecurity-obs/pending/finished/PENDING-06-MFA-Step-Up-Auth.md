---
type: adr
status: implemented
id: PENDING-06
domain: identity
priority: P2
created: 2026-07-03
updated: 2026-11-14
related:
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
  - PENDING-09-Continuous-Authorization
tags: [pending, finished, adr, identity, mfa]
---

# Pending ADR 06 — MFA & Step-Up Authentication

> **Status: IMPLEMENTED (2026-11-14, branch `feat/pending06-mfa-step-up`).**
> - **Workspace users:** native TOTP service, resource policy authentication requirements (frequency + optional MFA),
>   IdP `amr` claim recognition, risk scoring (country moves, failed codes, posture regressions), client step-up
>   (`zecurity-client auth`), console MFA policy, and break-glass MFA gating (`RequireBreakGlassMFA`).
> - **Provider operators:** existing mandatory TOTP (ADR-028, PR #138).

## Decisions Taken (2026-11-14)

| # | Decision |
|---|---|
| M-1 | **Native TOTP for workspace users** (Twingate model). Sealed secret, replay-safe (Valkey epoch sliding window), rate-limited (5 fails = 15m lockout), fail closed. No WebAuthn, no recovery codes. |
| M-2 | **Resource policy → Authentication:** frequency (1 h … 31 days, or none = device-only) and `requireMfa` (requires a frequency). Default is device-only. |
| M-3 | **A verified IdP `amr` (`mfa`, `otp`, `hwk`, …) counts as MFA.** Code entry is skipped when the IdP attests a second factor. *Amended 2026-11-16 (security review):* on a step-up, the `amr` counts only if the ID token's `auth_time` is from this step-up (1 min skew). Otherwise the TOTP page is shown. Sign-in (client login and admin console) does not check `auth_time` yet; see the implementation note. |
| M-4 | **Enforced in the ACL compiler** next to posture gate; expiry through snapshot `ValidUntil`. Lazy recompile on client poll or connector heartbeat. |
| M-5 | **Step-up does not re-enroll device:** `zecurity-client auth` → IdP → TOTP page if needed → tunnel stays up, ACL refreshed. |
| M-6 | **Admin Console Security:** workspace `consoleRequireMfa`; console requests without MFA receive `MFA_REQUIRED` (403 or GraphQL error) and prompt for TOTP. |
| M-7 | **Break-glass needs fresh MFA:** `RequireBreakGlassMFA` requires MFA within the last 15 minutes before executing break-glass mutations (`enableScimBreakGlass`, `acceptScimConflict`). |
| M-8 | **Admin resets another user's TOTP:** `resetUserMfa` resolver; audited, self-reset disallowed, clears user's active session MFA marks. |
| M-9 | **Risk score per device:** country move (+50), 5 wrong TOTP codes (+50), required profile pass → fail (+30); score ≥ 50 demands immediate re-authentication. |
| M-10 | **Scope limits (not built):** Sign In Policy MFA, WebAuthn, recovery codes, group policy overrides, mobile GUI prompt. |

## Implementation Summary

- **Database:** `migrations/061_mfa_step_up.sql` (`user_totp`, resource policy auth fields, client device session auth timestamps and risk score fields, workspace console MFA toggle).
- **Controller Modules:**
  - `internal/totp`: pure RFC 6238 TOTP engine.
  - `internal/mfa`: TOTP store, rate limiter, REST handlers (`/auth/mfa/status`, `/auth/mfa/enroll`, `/auth/mfa/verify`).
  - `internal/auth`: session auth timestamps, Valkey `refresh_mfa` stamp, token claims.
  - `internal/policy`: `DeviceAuthenticated` compiler gate, `GetDeviceAuthState`.
  - `internal/posture`: risk hooks (country move, posture regression).
  - `internal/risk`: device risk service and scoring.
  - `internal/client`: `StartReauth` gRPC endpoint, `/api/clients/reauth/mfa` HTML template, `auth_required_count`.
  - `graph/resolvers`: `setResourcePolicyAuthentication`, `setConsoleRequireMfa`, `resetUserMfa`.
- **Rust Client:** `zecurity-client auth` command (`client/src/cmd/auth.rs`), CLI hint lines in `status` and `resources`.
- **Admin UI:** `MfaPrompt` modal, Apollo error link interception with automatic retry, Settings → Security card, resource policy editor auth settings, Users page Reset MFA action, Devices page risk and sign-in columns.
