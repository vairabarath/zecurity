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

**Live check (2026-11-16):** the mock IdP sends `amr: ["mfa"]` but no `auth_time`, so its users now get the TOTP page. That is the intended behaviour.

### Fix: step-up not bound to the client that started it (security review, MEDIUM)
**Issue:** The IdP callback, and the TOTP or "Skip for now" form, stamped the device directly. Nothing tied the browser to the client that ran `zecurity-client auth`. Someone holding a stolen device and its session could start a step-up and send the link to the real user. If that user signed in and typed a real code, the stolen device was stamped.

**Fix Applied:** the step-up is now bound to the client the same way sign-in is:
- **Proto (additive):** `StartReauthRequest.code_challenge = 3` and `local_redirect_uri = 4`, both required, and the redirect must be an `http` loopback address with a port. `StartReauthResponse.session_id = 2`. New RPC `CompleteReauth(access_token, device_id, session_id, ctrl_code, code_verifier)` returns `{mfa, auth_required_count}`.
- **Controller (`reauth.go`):** the browser step no longer stamps the device. `grantStepUp` parks the result under a one-time code (60 s, process-local, D-02) and sends the browser with a 303 to the client's loopback. `CompleteReauth` consumes the grant on any attempt. It checks, in constant time, the code and SHA256(verifier) against the challenge. It also checks that the access token's workspace, user and session and the device id match `StartReauth`. Only then does it call `completeStepUp`. Every refusal is audited as `auth.step_up_failed`. The step-up pages' CSP `form-action` allows `'self'` plus only that client's loopback origin, so the TOTP form's redirect is allowed.
- **Client:** `login.rs` now shares `new_pkce()` and `start_loopback()` with `cmd/auth.rs`. `auth` waits for the loopback code, calls `CompleteReauth`, then syncs; the polling loop is gone.
- **Compatibility:** clients built before this change cannot step up (`InvalidArgument`). Nothing is released, so this is acceptable.

**Tests:** `TestCompleteReauth_BoundToTheClient` covers:
- the browser step stamps nothing;
- wrong code, wrong verifier, another session or another device is refused and audited;
- the grant is single use;
- the right client is stamped exactly once.

The handler tests now redeem through `CompleteReauth`. `TestStartReauth_Preconditions` rejects a missing, non-loopback or portless redirect. Results: controller `internal/client` 100 PASS, 0 skipped; client `cargo test` 384 PASS.

**Live check (2026-11-16, Orca browser):**
- `login` through the refactored loopback works.
- `auth` with authenticator enrolment redirects to the loopback, and the resources come back (`mfa.enrolled`, `auth.step_up {mfa: totp}`).
- **The attack, simulated:** the client was killed after `StartReauth`, then the link was completed with a valid code. The browser's redirect failed, the resources stayed withheld, and no `auth.step_up` row was written.
- "Skip for now" returns only the resource without MFA.

### Fix: a stranger's token could burn a step-up grant (security review, MEDIUM)
**Issue:** `CompleteReauth` consumed the grant before it checked the caller. Anyone with a valid token who saw the step-up `state` (it is in the auth URL) could destroy the owner's grant and write `auth.step_up_failed` rows into the owner's workspace.

**Fix Applied:** `takeStepUpGrant` takes an ownership check: same workspace, user, session and device. A caller who doesn't own the grant gets `NotFound`, the grant stays in place and no audit row is written. For the owner, any attempt consumes the grant. A wrong code or verifier is still audited. `TestCompleteReauth_BoundToTheClient` covers strangers by session, device, user and workspace, then redeems the same grant as the owner.

### Fixed (e86690bf, in #206): sign-in also trusted an old IdP `amr`
Client and console sign-in now stamp MFA at the IdP's `auth_time` (capped at now); a missing `auth_time` means no MFA (`auth.IdPMFAAt`). The original note follows.

Fix 1 covers only the step-up. Two sign-in paths still record MFA at "now" from the IdP's `amr` with no `auth_time` check:
- client login (`TokenExchange`, `service.go` → `sess.IdpMFA`);
- admin console login (`auth/callback.go` → `SessionAuthFromAMR`).

So `zecurity-client logout && zecurity-client login` on a live IdP session can meet a recent-MFA policy with an old MFA. The same applies to console MFA (M-6). The fix is the same `auth_time` gate, with the start time recorded at `InitiateAuth` or the console's `initiateAuth`. Google is not affected: it never sets `amr`, and its `AuthTime` is only `iat`.


### Fixes from the code review of merged #205–#208 (2026-11-17, branch `fix/mfa-review-findings`)
- **Wrong-code limit was check-then-act** (`internal/mfa/limiter.go`, `mfa.go` Verify). Parallel codes all passed `Blocked()` before any failure was counted. `Limiter` is now `Attempt` (INCR first, refuse above `FailMax`) + `Reset`; the FailMax-th wrong code trips the lockout. Every attempt counts, including one that fails as not-enrolled or unavailable. Test: `limiter_test.go` (50 parallel attempts → exactly 5 allowed). Commit 3f0a807c.
- **`/auth/mfa/verify` renewed a suspended user's token** (`internal/auth/session.go` StampSessionMFA). Now runs the same lifecycle + generation check as `RefreshSession`. Test: `TestStampSessionMFA_RefusesSuspendedOrRevoked` (DB). Commit 3f0a807c.
- **A profile edit counted as a device regression (M-9)** (`internal/posture/evaluate.go`). The hook now also requires `previous.ProfileRevision == profile.Revision`. Test: `regression_integration_test.go` (DB; fails without the fix). Commit 10f0cde5.
- **MFA reset left step-ups in flight** (`graph/resolvers/mfa_helpers.go` → `mfa.Service.Reset`). `mfa.Service.OnReset` hooks; the client service drops the user's tickets and grants (`cancelUserStepUps`). `completeStepUp` also stamps a TOTP step-up's MFA only while a confirmed `user_totp` row exists, so a redemption racing the reset gets no MFA. Tests: `TestCancelUserStepUps`, `TestResetRunsHooks`, `TestCompleteReauth_TOTPAfterResetStampsNoMFA`. Commits e1d610ad, 46eef997.
- **Console-policy cache could keep the old value for 15 s** (`internal/mfa/console.go`). Per-workspace version counter; `Set` writes the new value; a read caches only if no `Set` ran meanwhile. No test (race). Commit 498008e4.
- **`zecurity-client auth` redeemed with a stale token** (`client/src/cmd/auth.rs`). Syncs and takes the current token after the browser step. Commit 19a22880.
- **Deleted workspace got 503 from the MFA check** (`cmd/server/main.go`). `WorkspaceGuard` now runs before `requireConsoleMFA` on `/graphql` and the three REST routes. Commit 21affa60.
- Client audit action constants reuse `mfa.ActionEnrolled/ActionLocked` (4f4e85e2).

Second review (PR #209), fixed before merge:
- A TOTP step-up racing an MFA reset is now refused as a whole (`completeStepUp` WHERE clause), so it neither clears the risk demand nor reports MFA it did not stamp.
- `StampSessionMFA`: a DB error returns an error (503 at the handler), not ErrSessionNotFound (401).
- `Verify` counts the attempt after loading and decrypting the record, still before the code check, so not-enrolled or outage results don't use up attempts.
- `cancelUserStepUps` drops only TOTP grants; IdP and no-MFA step-ups stay valid.
- `ConsolePolicy.Set` drops the cache entry rather than writing it, so two Sets that finish out of order can't leave the old value cached.
Left as is: a suspended user's first enrolment code is accepted before the 401 (the authenticator is not usable while suspended); a real regression at the first evaluation after a profile edit is not signalled (accepted trade-off); DB tests skip without `*_TEST_DATABASE_URL` (repo convention; CI sets them).

Not done (performance, follow-up): `GetACLSnapshot` runs `authRequiredCount` on every poll; `User.mfaEnrolled` is N+1 (batch with `EnrolledUsers`).
Not in this diff, same pattern: the provider operator TOTP limiter (`internal/provider/ratelimit.go`, `totp_handlers.go`) is also check-then-count.

### Live run after #205–#209 (2026-11-17, main at e8641069)
Isolated stack: the controller built from `e8641069` on a copy of the shared database and its own Valkey DB, reachable from the lab at this machine's LAN address. Connector on luffy (nika), test resource on omarchy (manoj, a web server on port 8765, never 22), client on this machine. Sign-in through the local test identity provider.

| Check | Result |
|---|---|
| Console sign-in, IdP sends no `auth_time` | PASS: no `mfa_at`, audit `mfa: none` (e86690bf) |
| Console MFA turned on | PASS: the next MFA-less request gets `MFA_REQUIRED` immediately |
| 20 wrong codes in parallel | PASS: exactly 5 checked (401), 15 refused (429), one `mfa.locked`, 900 s window |
| Suspended user's correct code at `/auth/mfa/verify` | PASS: 401, no token |
| Inactive workspace | PASS: 403 `workspace inactive` on `/graphql` and `/api/connectors/…`, not 503 |
| Connector reinstall with an old identity present (#205) | PASS: moved to `/var/lib/zecurity-connector.bak-<timestamp>`; the earlier `.bak-p16live` untouched; connector active |
| Client step-up, policy requires MFA | PASS: TOTP page → `auth.step_up {"mfa":"totp"}` → resource released |
| Skip on the TOTP page | as designed: sign-in refreshed, earlier MFA mark kept |
| Tightening a required profile (M-9 fix) | PASS: device fails revision 4, `risk_score` stays 0, no reauth demand |
| Data path client → luffy connector → omarchy | PASS: connector `access allowed` + `tunnel_opened ok`, page served |

Not run live: MFA reset during a step-up (the copied database has one admin; covered by DB tests); IdP MFA with an old `auth_time` (would need the shared test IdP's config changed).

Observations (not bugs in the fixes):
- A stopped system client left its `fwmark 0x5a lookup main` rule (priority 49) behind; the test client's `ip rule add` then failed. Worth checking that `systemctl stop` cleans it up (it may be left from an earlier crash).
- A dev client can't run its tunnel without root: iproute2's `ip` drops file capabilities for non-root users, so `setcap` on a copy of `ip` does not help. The test daemon ran as root for the data path.
