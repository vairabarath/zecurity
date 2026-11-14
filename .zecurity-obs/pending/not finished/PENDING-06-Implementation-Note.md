---
type: implementation-note
status: in-progress
id: PENDING-06
domain: identity
created: 2026-11-14
related:
  - PENDING-06-MFA-Step-Up-Auth
  - PENDING-09-Continuous-Authorization
tags: [pending, identity, mfa, implementation-note]
---

# PENDING-06 — implementation note (state on 2026-11-14)

Work was stopped on request part-way through. This note says what is built, what is not, and what has
not been verified. Branch: local `feat/pending06-mfa-step-up` (3 commits, not pushed), based on
`docs/pending-audit`, not on `main`, because the reorganised pending docs exist only there.

**Short version:** the controller and the Rust CLI are built and their automated tests pass. The admin
console UI is not started, nothing has been run live, and the PENDING-06 doc itself is not updated.
The feature is not usable end to end by an admin yet: there is no screen to turn it on.

## Decisions taken (approved in the plan, not yet written into the PENDING-06 doc)

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

## Built

### Controller (Go) — built, tests pass
- `migrations/061_mfa_step_up.sql` `[schema: reset DB]`: `user_totp`; `resource_policies.auth_frequency_minutes`,
  `require_mfa`; `client_devices.session_auth_at`, `session_mfa_at`, `risk_score`, `risk_reasons`,
  `reauth_required_at`; `workspaces.console_require_mfa`.
- `internal/totp` (new): the RFC 6238 code moved out of `internal/provider`; `provider/totp.go` now wraps it.
- `internal/mfa` (new): TOTP service, Postgres store, Valkey limiter, console policy, REST handlers
  `GET /auth/mfa/status`, `POST /auth/mfa/enroll`, `POST /auth/mfa/verify`.
- `internal/auth`: sessions record `auth_at`; the MFA mark lives in its own Valkey key
  (`refresh_mfa:{user}:sid`) so stamping never races refresh rotation; `mfa_at` claim on access tokens;
  `IssueSessionWithAuth`, `StampSessionMFA`, `ClearUserSessionMFA`; both sign-in paths record IdP `amr` proof.
- `internal/middleware`: `TenantContext` carries `SessionID` and `MFAAt`; `RequireConsoleMFA` on `/graphql`,
  invitation create, connector and shield token routes.
- `internal/policy`: `DeviceAuthenticated` / `applyAuthentication` in the compiler; `GetDeviceAuthState`.
- `internal/posture`: `SetResourcePolicyAuthentication`, `ResourceAuthForWorkspace`; country-move detection;
  posture regression hook.
- `internal/risk` (new): the score, one audit row and one recompile when the threshold is first reached.
- `internal/client`: `StartReauth` RPC, step-up on the existing `/api/clients/callback`, controller-hosted
  TOTP page (`POST /api/clients/reauth/mfa`), `auth_required_count` on `GetACLSnapshot`.
- GraphQL: `ResourcePolicy.authFrequencyMinutes/requireMfa`, `setResourcePolicyAuthentication`,
  `consoleSecurity`, `setConsoleRequireMfa`, `resetUserMfa`, `User.mfaEnrolled`, device
  `lastAuthenticatedAt/lastMfaAt/riskScore/riskReasons/reauthRequired`.
- `RequireBreakGlassMFA` is real and is called from `enableScimBreakGlass` and `acceptScimConflict`.
- Proto (additive): `StartReauth`, `GetACLSnapshotResponse.auth_required_count`.

### Rust client — built, tests pass
- `zecurity-client auth` (`client/src/cmd/auth.rs`); `status` and `resources` print how many resources
  are waiting for an authentication.

## Verified

- `go build ./...`, `go vet ./...`: clean.
- `go test ./... -count=1` with Postgres and Valkey up: all packages pass (run after the last change).
  DB-backed tests ran, not skipped.
- Rust client: `cargo check --all-targets` clean; `cargo test`: 384 passed, 0 failed (in the dev-disk
  build container).
- New tests cover: TOTP enrol/verify/replay/lockout/fail-closed; store races (8 concurrent requests, one
  winner); compiler gate and `ValidUntil`; session mark across rotation, reset and session end; console
  middleware; break-glass 15-minute rule; risk threshold and concurrency; step-up page (enrol, wrong
  code, skip, single-use ticket, identity mismatch, IdP MFA, mandatory MFA on a risk demand).

## Not done (the balance)

1. **Admin console UI — not started.** Nothing in `admin/` was touched, and `npm run codegen` was not run.
   - `/mfa` page (enrol with QR, verify) and global handling of `MFA_REQUIRED`.
   - Authentication section in `pages/ResourcePolicies.tsx`.
   - Console-MFA toggle in settings; "Reset MFA" on the user row.
   - Device view: last authentication, MFA, risk score and reasons.
   - Break-glass dialogs: prompt for a code on `MFA_REQUIRED` and retry.
   - **Effect today:** these settings can only be changed through raw GraphQL. Break-glass in the
     existing UI will now fail with `MFA_REQUIRED` and the UI has no way to satisfy it.
2. **Nothing was run live.** No controller was started with this code, the local DB was not recreated
   with `061_`, and the browser step-up was never exercised against the mock IdP. The planned live cases
   (policy with MFA → `zecurity-client auth` → enrol → resource reachable; IdP `amr` skip; expiry cuts a
   tunnel; console toggle; break-glass) are all open.
3. **Tests not written:**
   - `mfa` REST handlers (`Status`, `Enroll`, `Verify`).
   - Resolvers `setConsoleRequireMfa` (lock-out rule), `resetUserMfa`, `setResourcePolicyAuthentication`.
   - `StartReauth` success path (needs an IdP fake) and `auth_required_count` on `GetACLSnapshot`.
   - The country-move and posture-regression risk hooks end to end.
4. **Docs not updated:** PENDING-06 status and decisions M-1 … M-10, `pending/README.md`, the PENDING-09
   C-5 note, `docs/database-development.md`, Session Log. The doc stays in `not finished/`.
5. **Mobile and Windows GUI:** no prompt UI. The shared Rust core has the RPC; only the CLI has the command.

## Differences from the approved plan

- **Policy preview** does not count authentication (the plan said it would). The preview takes proposed
  device profiles; authentication is something the user does on demand, so the count would mislead.
- **Step-up reuses `/api/clients/callback`** instead of a new callback URL, so no identity connection
  needs a second redirect URI registered at the IdP.
- **"Skip for now"** on the TOTP page when MFA is asked only because a policy requires it: the user
  gets the non-MFA resources and the MFA ones stay closed. It is not offered when the risk score
  demanded the authentication and the user has an authenticator.
- **New Go dependency:** `github.com/skip2/go-qrcode` (QR code on the enrolment page). It was added
  without a dependency review; the hook asked for one and it was skipped because sub-agents were
  turned off. Review it before merge, or remove it and show only the text key.

## Known limits of what is built

- **Expiry is noticed lazily.** A snapshot past its `ValidUntil` is recompiled on the next read
  (connector heartbeat or client poll), the same as posture staleness. There is no timer.
- **MFA reset has a tail of up to 15 minutes** on the console: an access token already minted keeps its
  `mfa_at` until it expires. Devices lose the mark at once.
- **The step-up does not force the IdP to re-prompt** (`prompt=login` / `max_age` are not sent). If the
  user's IdP session is alive, the IdP round trip is silent, as in Twingate. It proves the IdP session
  is still valid, not that the user typed a password again.
- **Console MFA lasts for the whole session** once passed; only break-glass asks for a recent one.
- **Step-up state is process-local** (auth sessions and TOTP tickets), consistent with the single
  controller instance rule (D-02).
- **`User.mfaEnrolled`** is one query per user in a list.

## Security review

- The automated review of commit 1 did not run (API returned 429). It has not been reviewed.
- The review of commit 2 found two fail-open paths; both are fixed in commit 3 with a regression test:
  the break-glass MFA check on `acceptScimConflict` is now unconditional, and a step-up that answers a
  risk demand without a second factor clears the older MFA mark.
- Commit 3 has not been reviewed.

## To pick this up

```bash
git checkout feat/pending06-mfa-step-up
cd controller && docker compose down -v && docker compose up -d      # 061_ needs a fresh DB (shared local stack: check first)
cd controller && GOTOOLCHAIN=go1.26.2 go tool gqlgen generate --config graph/gqlgen.yml   # host Go 1.27 breaks gqlgen
cd admin && npm run codegen
```

Suggested order: admin UI (item 1) → live run (item 2) → missing tests (item 3) → docs (item 4).
