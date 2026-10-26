---
type: test-plan
sprint: 22
owner: M1 (Sathiya) + M2 (Barath)
status: planned
tags: [sprint22, tests, acceptance, provider-dashboard, totp, relay-management, relay-drain]
---

# Sprint 22 — Acceptance Test Plan

> **Purpose.** Prove that super-admins can't act without TOTP, and that relay create, revoke, delete, drain and resume work from the console with the D-17 safeguards, the D-31 role split and the D-32 drain guarantees.
> - The sprint is **not done** until §1 passes and every phase table is green.
> - Go tests are table-driven `*_test.go`. DB-backed tests use throwaway databases and the isolated Valkey (`AUTH_TEST_VALKEY_URL`); **a skipped DB test is a failed acceptance case**. Never run against the dev database or dev Valkey.
> - Recreate the local DB after pulling a `[schema: reset DB]` PR (`042_`, `043_`).
> - "Live" cases run on the lab (controller, ≥ 2 relays, connector, shield, client) by M1.

---

## 1. Core invariants

### AT-CORE-1 — A super-admin can't act without a TOTP-backed token
- A super-admin token whose `amr` lacks `otp` (including any token minted before Phase T) → **403 `mfa_required`** on every `/provider/*` route except the TOTP routes.
- An `mfa` (verify/enroll) token → 403 on every route except its own.

### AT-CORE-2 — relay-ops can't revoke or delete, whatever the client does
- relay-ops token (direct `curl`, not the console) → **403** on `POST /provider/relays/{id}/revoke` and `DELETE /provider/relays/{id}`; relay unchanged; no audit row.

### AT-CORE-3 — The console writes only what D-30 allows
- API-surface test: the console's non-GET calls are exactly the auth calls (incl. `/auth/totp/*`), operator management (incl. `reset-totp` if OQ-1 yes), and relay create / revoke / delete / drain / resume.

### AT-CORE-4 — Nothing outside scope changed
- No diff under `connector/`, `relay/`, `proto/`, `admin/`; `workspaces.status='active'` predicates unchanged (grep check in review).

---

## Phase T — Super-admin TOTP

| ID | Scenario | Expected |
|----|----------|----------|
| AT-T1 | Super-admin with no TOTP signs in (password OK) | `mfa:"enroll"` token; console shows QR + text; any other route → 403 |
| AT-T2 | Confirm enrolment with the current code from an authenticator app | Full token, `amr:["pwd","otp"]`; `totp_enabled_at` set; generation bumped (older tokens 401); audit `provider_user.totp_enroll`; secret absent from audit and logs |
| AT-T3 | Next sign-in | `mfa:"verify"`; correct code → full token; wrong code → 401 `invalid_code` |
| AT-T4 | Same code submitted twice (within its window) | Second → 401 `invalid_code` |
| AT-T5 | Five wrong codes within 15 min | 429 `too_many_attempts`; `provider_auth.totp_rate_limit` audited |
| AT-T6 | `must_change_password` super-admin with TOTP | Password change first (`pwc`), then the TOTP step |
| AT-T7 | (OQ-1) Super-admin resets another operator's TOTP with a reason; tries own row; tries without a reason; relay-ops tries | Target's sessions end; next login → enrol; audit `provider_user.totp_reset` with actor, target, time and reason; own row → 409 `cannot_modify_self`; no reason → 400 `reason_required`; relay-ops → 403; no audit on refusals |
| AT-T8 | `zecurity-controller provider recover-admin --email <super-admin>` on the controller host | Temporary password printed once; TOTP cleared; sessions end; audit `provider_user.recover_admin`; refuses a relay-ops email and an unknown email (non-zero exit) |
| AT-T9 | relay-ops without TOTP signs in (OQ-2) | Full token `amr:["pwd"]`; relay pages work; no enrolment prompt |
| AT-T10 | (OQ-3) A super-admin token minted before the Phase T deploy, still unexpired | 403 `mfa_required` on its next request |
| AT-T11 | Super-admin changes password (forced and voluntary) | Response carries an `mfa` token, never a full one |
| AT-T12 | Demoted super-admin (now relay-ops) with TOTP still enabled signs in | `mfa:"verify"`; a password-only token for them → 403 `mfa_required` |

## Phase A — Relay admin API hardening

| ID | Scenario | Expected |
|----|----------|----------|
| AT-A1 | Revoke with no body / blank reason / 501-char reason | 400 `reason_required` / `reason_required` / `reason_too_long`; nothing changed |
| AT-A2 | Revoke with `confirm_name` wrong or different case | 400 `confirmation_mismatch` |
| AT-A3 | Valid revoke by super-admin | 204; serials on the CRL with the reason; audit row has the reason and the client IP without port; connectors migrate |
| AT-A4 | Revoke again; revoke a deleted relay; delete a revoked relay | 409 `already_revoked`; 409 `already_deleted`; 204 |
| AT-A5 | Any 5xx path | Body exactly `{"error":"server_error"}` |
| AT-A6 | relay-ops creates a relay | 201 (create still allowed), `Cache-Control: no-store`; an invalid name/SAN → 400 with no relay row left behind |
| AT-A7 | Two super-admins revoke the same relay at once | Exactly one 204; the other 409 `already_revoked` |

## Phase E — Relay enrolment

| ID | Scenario | Expected |
|----|----------|----------|
| AT-E1 | Create a relay via the API | 201 with `ca_fingerprint` (= SHA-256 of `/ca.crt` DER) and the controller addresses (or empty); `Cache-Control: no-store` |
| AT-E2 | Console: Add relay with invalid DNS/IP entries | Field errors matching the server's rules |
| AT-E3 | Console: Add relay, close the token dialog | Token shown once; absent from storage, URL and DOM after close; pending relay listed |
| AT-E4 | **Live:** paste the install block on a lab host | Relay goes pending → active; token used up (Valkey JTI gone) |
| AT-E5 | Pending relay whose token expired | Hint explains D-20; delete + add new works |

## Phase X — Revoke and delete in the console

| ID | Scenario | Expected |
|----|----------|----------|
| AT-X1 | relay-ops opens a relay | No danger-zone card |
| AT-X2 | Super-admin opens a revoked or deleted relay | No danger-zone card |
| AT-X3 | Revoke dialog without reason or with the wrong name | Button disabled |
| AT-X4 | **Live:** revoke a relay carrying a connector | Connector logs "Active relay absent from new list; migrating immediately"; traffic continues; CRL lists the serial; audit shows the reason |
| AT-X5 | **Live:** delete a relay | Relay leaves the default list; certificates revoked; audit shows the reason |

## Phase D — Drain and resume

| ID | Scenario | Expected |
|----|----------|----------|
| AT-D1 | Drain an eligible relay (2+ eligible) | 204; relay absent from the published list; list version changed; audit `relay.drain` |
| AT-D2 | Drain the only eligible relay | 409 `last_eligible_relay`; nothing changed |
| AT-D3 | Two operators drain the last two eligible relays at once | Exactly one succeeds |
| AT-D4 | Resume | 204; relay back in the list; version equals the pre-drain value; audit `relay.resume` |
| AT-D5 | relay-ops drains and resumes | Allowed; revoke/delete still 403 |
| AT-D6 | **Live:** drain a relay with a long download and open a new connection | New connection lands on another relay; download finishes, or is cut at ~120 s; attached count reaches 0; console shows "Drained" |
| AT-D7 | Drain pending / revoked / deleted relays; repeat drain; resume a non-draining relay | 409 `invalid_state` / `already_revoked` / `already_deleted`; 204 no-op, no audit; 204 no-op, no audit |

---

## 6. Development workflow checks

- `042_` and `043_` apply on a fresh volume; the controller starts; `provider_users` has the TOTP columns and `relays.drain_started_at` exists.
- No existing schema file changed (`git diff --stat controller/migrations` shows only additions).
