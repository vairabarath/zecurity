---
type: implementation-note
status: finished
id: PENDING-06
domain: identity
created: 2026-11-14
updated: 2026-11-14
related:
  - PENDING-06-MFA-Step-Up-Auth
  - PENDING-09-Continuous-Authorization
tags: [pending, finished, identity, mfa, implementation-note]
---

# PENDING-06 — Implementation Note (Completed 2026-11-14)

Branch: `feat/pending06-mfa-step-up`.
All components — Controller, Rust Client, and Admin Console UI — are built, tested, and verified end-to-end.

## Decisions Taken (Approved & Implemented)

| # | Decision |
|---|---|
| M-1 | Native TOTP for workspace users (Twingate model). Sealed secret, replay-safe, rate-limited, fail closed. No WebAuthn, no recovery codes. |
| M-2 | Resource policy → Authentication: frequency (1 h … 31 days, or none = device-only) and `requireMfa` (needs a frequency). Default is device-only. |
| M-3 | A verified IdP `amr` (`mfa`, `otp`, `hwk`, …) counts as MFA. No per-connection attestation. |
| M-4 | Enforced in the ACL compiler next to the posture gate; expiry through the snapshot `ValidUntil`. |
| M-5 | Step-up does not re-enrol the device: `zecurity-client auth` → IdP → TOTP page if needed. |
| M-6 | Admin Console Security: workspace `consoleRequireMfa`; console requests without MFA get `MFA_REQUIRED`. |
| M-7 | Break-glass needs MFA within the last 15 minutes. |
| M-8 | An admin resets another user's TOTP. |
| M-9 | Risk score per device: country move +50, 5 wrong TOTP codes +50, required profile pass → fail +30; ≥ 50 demands re-authentication. |
| M-10 | Not built: Sign In Policy MFA, WebAuthn, recovery codes, group policy overrides, mobile UI. |

## Built Components

### Controller (Go)
- Schema: `migrations/061_mfa_step_up.sql` `[schema: reset DB]`: `user_totp`; `resource_policies.auth_frequency_minutes`,
  `require_mfa`; `client_devices.session_auth_at`, `session_mfa_at`, `risk_score`, `risk_reasons`,
  `reauth_required_at`; `workspaces.console_require_mfa`.
- `internal/totp`: RFC 6238 TOTP engine.
- `internal/mfa`: TOTP service, Postgres store, Valkey limiter, console policy, REST handlers
  `GET /auth/mfa/status`, `POST /auth/mfa/enroll`, `POST /auth/mfa/verify`.
- `internal/auth`: Session auth tracking; Valkey `refresh_mfa` key; `mfa_at` token claim; IdP `amr` parsing.
- `internal/middleware`: `TenantContext` carries `SessionID` and `MFAAt`; `RequireConsoleMFA` on `/graphql`,
  invitation create, connector and shield token routes.
- `internal/policy`: `DeviceAuthenticated` compiler gate, `GetDeviceAuthState`.
- `internal/posture`: `SetResourcePolicyAuthentication`, `ResourceAuthForWorkspace`, country-move detection, posture regression hook.
- `internal/risk`: Device risk scoring engine and notifications.
- `internal/client`: `StartReauth` RPC, `/api/clients/reauth/mfa` TOTP page, `auth_required_count` on `GetACLSnapshot`.
- Resolvers: `setResourcePolicyAuthentication`, `setConsoleRequireMfa`, `resetUserMfa`, `RequireBreakGlassMFA`.
- Proto stubs updated: `StartReauth`, `GetACLSnapshotResponse.auth_required_count`.

### Rust Client
- `zecurity-client auth` (`client/src/cmd/auth.rs`): triggers re-auth URL, opens browser, polls daemon status.
- `status` and `resources` CLI hints for pending authentication.

### Admin Console UI
- `components/auth/MfaPrompt.tsx`: modal prompt for enrollment (QR + secret key) and code verification.
- Apollo error link (`apollo/links/error.ts`): intercepts `MFA_REQUIRED` 403 / GraphQL error, queues operations, prompts user, retries on success.
- `components/settings/ConsoleSecurityCard.tsx`: Settings → Security card with console MFA toggle.
- `components/ResourcePolicyModal.tsx`: Authentication frequency and Require MFA configuration.
- `pages/TeamUsers.tsx` & `components/users/ResetMfaDialog.tsx`: MFA enrolled column and Reset MFA action (disallowed on own row).
- `pages/DeviceManagement.tsx`: Last sign-in, MFA timestamp, risk score and reasons.

## Verification & Test Results

### 1. Automated Tests (All Passed)
- `go test ./internal/mfa/...`: PASS
- `go test ./internal/client/...`: PASS (including `TestStartReauth_SuccessPath`, `TestGetACLSnapshot_AuthRequiredCount`, `TestCountryMove_RiskHook_EndToEnd`, `TestPostureRegression_RiskHook_EndToEnd`)
- `go test ./graph/resolvers/...`: PASS (including `TestResolvers_SetConsoleRequireMfa_Lockout`, `TestResolvers_ResetUserMfa`, `TestResolvers_SetResourcePolicyAuthentication`)
- Admin unit tests: 324 passed, 0 failed (`npm test`). Build and lint clean.
- Rust client unit tests: 384 passed, 0 failed.

### 2. Live Verification (All Passed on Local Lab `ztna_p06`)
- **Console MFA:** Setup QR, invalid code rejection, replay protection, right code verification. Refusal of console sessions without MFA when toggle enabled.
- **Client Step-Up:** `zecurity-client auth` opens browser, authenticates via IdP, displays code page, resources immediately become reachable in tunnel.
- **Session Expiry:** Aging `session_auth_at` past policy duration withholds resources and reports pending count in `zc status`.
- **Skip for Now:** Non-MFA resources restored, MFA-required resources stay withheld.
- **Reset MFA from UI:** Admin resets another user's MFA; audited; user prompt requires re-enrollment.
- **Break-Glass in UI:** SCIM break-glass with unproven mapping triggers `MFA_REQUIRED`; prompt opens over dialog; code submission replays and completes override.
- **Member at `/install`:** With console MFA enabled, navigating to `/install` prompts member for MFA.
- **Sign-Out from Prompt:** Clicking "Sign out" in prompt clears state and redirects to `/login`.
- **IdP `amr` Skip:** Mock IdP emitting `amr: ["mfa"]` skips TOTP code page automatically; audit records `{"mfa": "idp"}`.

## Security & Dependency Review

- **Unreviewed Commits Reviewed:**
  - `5af0175d`: `zecurity-client auth` command, break-glass MFA check made unconditional (fail-closed), risk demand clears previous MFA mark. Approved.
  - `46dc4333`: Admin console MFA UI, Apollo error link interceptor, Reset MFA dialog (self-reset prevented). Approved.
  - `e2822e8a`: Dev server proxy configuration for `/auth/mfa`. Approved.
- **Dependencies Reviewed:**
  - `github.com/skip2/go-qrcode` (v0.0.0-20200617195104-da1b6568686e): pure Go, MIT license, zero runtime dependencies outside Go stdlib. No CVEs.
  - `qrcode.react` (4.2.0 exact): ISC license, zero runtime dependencies, peer-depends on React only. 0 vulnerabilities in `npm audit`. Matches provider-console.

## Post-Phase Fixes

### Fix: `TestHandlers_Status` panics after 2026-11-14 (time-bomb)
**Issue:** `controller/internal/mfa/handlers_test.go` panicked with a nil-pointer dereference in `pgxpool.(*Pool).Acquire` from 2026-11-15 on. The 2026-11-14 report that the suite passed was true only on that day.

**Root Cause:** `newTestHandlers` pinned the service clock to 2026-11-14 12:00 and seeded the `ConsolePolicy` cache "valid until now + 1 h", but left `ConsolePolicy.now = time.Now`. Once real time passed 13:00 that day the entry counted as expired, and `Required` fell through to the nil pool.

**Fix Applied:** `cp.now = func() time.Time { return now }` in `newTestHandlers`.

Verified 2026-11-16 against a throwaway DB built from migrations 001–061 (all `*_TEST_DATABASE_URL` set, so the DB-backed tests ran instead of skipping): `./internal/mfa/... ./internal/client/... ./graph/resolvers/...` gave 283 PASS, 0 FAIL. One skip remains, `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, and it is intentional and older than this work.

### Fix: Users table Name and Email columns collapse (admin UI)
**Issue:** On the Users page (`admin/src/pages/TeamUsers.tsx`) the Name and Email cells were not visible and the headers were out of line with the cells. Found in the live run on 2026-11-16 in the Orca browser.

**Root Cause:** The MFA column added 100 px and the actions column grew from 130 px to 250 px. The fixed columns plus gaps (about 992 px) now took more than the table's `min-w-[1000px]` minus padding, so the two `2fr` columns got no width.

**Fix Applied:** Name and Email are now `minmax(160px,2fr)`, and the header and rows use `min-w-[1360px]`. The table scrolls horizontally inside `.table-scroll` instead of collapsing.

### Fix: IdP MFA from an old sign-in counted as a fresh second factor (security review, HIGH)
**Issue:** A step-up does not force the IdP to prompt again. On a live IdP session the round trip is silent, and the ID token can still carry `amr: ["mfa"]` from a sign-in done days earlier. `finishReauthIdP` counted that `amr` as MFA "now", stamped `session_mfa_at = NOW()` and skipped the TOTP page. A "MFA every 1 hour" policy could therefore be met by an old IdP MFA.

**Fix Applied (`controller/internal/client/reauth.go`):** `reauthInfo.StartedAt` records when `StartReauth` ran. The IdP's MFA now counts only through `idpMFAFresh`, which requires the ID token's `auth_time` to be no older than `StartedAt` minus 1 minute of clock skew. A missing `auth_time` never counts. Otherwise the user gets the controller's TOTP page, the same as with no `amr`. M-3 is tightened: a verified IdP `amr` counts as MFA only if the IdP authenticated the user during this step-up.

**Not done:** `max_age=0` / `prompt=login` is still not sent, so the IdP sign-in itself can stay silent. The `auth_time` check is what stops the stale MFA. Some IdPs, such as the local mock, send no `auth_time`, so their users always get the TOTP page.

**Tests:** `TestIdPMFAFresh` (fresh, within skew, stale, missing, no amr) and a stale-`amr` case in `TestFinishReauthIdP_IdentityMustMatch`.

**Open (security review, MEDIUM):** the step-up is not bound to the client that started it. Someone holding a stolen device can send the step-up link to its real user. The fix needs PKCE plus a loopback code and a redeem RPC (additive proto and client work). It is tracked as a follow-up.
