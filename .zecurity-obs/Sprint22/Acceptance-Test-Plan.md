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

### Live run — Phase T (2026-10-27): PASS

**Setup:**
- Isolated local stack, not the shared lab: a throwaway database (`zecurity_live_phaset_*`, all 45 migrations), its own Valkey (`127.0.0.1:6390`, container `zecurity-phaset-valkey`), and a controller on ports 18080/19090 with fresh secrets and a create-only bootstrap super-admin.
- The controller binary was built from the `sprint20/fix18-resource-host-edit` branch. Its provider, TOTP, middleware and console code is identical to `fixed-pendings` `fcb7e4eb` (empty diff).
- The provider console dev server (`provider-console`, a temporary untracked Vite config proxying to :18080) was driven in Orca's browser.
- TOTP codes were computed from the secret each enrolment returned, standing in for an authenticator app.
- Tokens "minted before Phase T" (T10, T12) were signed with the stack's own provider key, with `amr: ["pwd"]` and no MFA stage, exactly as a pre-Phase-T controller issued them.
- Run from Sathiya's session; the throwaway database, Valkey and processes were removed afterwards.

**Results: 40/40 API checks passed, plus the console checks for T1.**

| Case | Result | Evidence |
|---|---|---|
| AT-T1 | PASS (API and console) | bootstrap super-admin with no TOTP → `mfa:"enroll"`, `amr:["pwd"]`; the enroll token gets 403 on `/provider/me`, `/relays`, `/users`, `/audit` and on the verify route (`mfa_required`). Console: forced password change → **"Set up two-factor authentication" with the QR code and the setup key as text**; confirming a code → dashboard |
| AT-T2 | PASS | enrol returns a secret and an `otpauth://totp/` URI; confirm → full token `amr:["pwd","otp"]`; `totp_enabled_at` set; `session_generation` 2 → 3; the pre-enrol token → 401; one `provider_user.totp_enroll` row; **none of the 8 secrets / otpauth URIs of the run appear in any audit row or in the controller log** |
| AT-T3 | PASS (API and console) | next sign-in → `mfa:"verify"`; wrong code → 401 `invalid_code`; correct → full token. Console: verify page, a wrong code shows an alert, the correct one opens the dashboard |
| AT-T4 | PASS | the same code on two verify tokens → first 200, second 401 `invalid_code` |
| AT-T5 | PASS | wrong codes 1–5 → 401 `invalid_code`; **the 6th attempt** → 429 `too_many_attempts`; one `provider_auth.totp_rate_limit` row (the lockout starts once five codes have failed) |
| AT-T6 | PASS | super-admin with TOTP, password reset by another super-admin → login gives a `pwc` token (no `mfa`), which gets 403 on the TOTP route; the password change → `mfa:"verify"` → verify 200 |
| AT-T7 | PASS | reset with a reason → 204; the target's token → 401; its next login → `mfa:"enroll"`; one `provider_user.totp_reset` row with actor, target id, time and reason; own row → 409 `cannot_modify_self`; blank reason → 400 `reason_required`; relay-ops → 403 `forbidden`; no audit row for any refusal |
| AT-T8 | PASS | `zecurity-controller provider recover-admin --email <super-admin>` → exit 0, one stdout line (the temporary password), TOTP cleared, the old token → 401, the next login needs a password change; one `provider_user.recover_admin` row; a relay-ops email and an unknown email → exit 1 with no stdout |
| AT-T9 | PASS | relay-ops without TOTP: first change and later logins → full token `amr:["pwd"]`, no `mfa` stage; `GET /provider/relays` 200 |
| AT-T10 / AT-CORE-1 | PASS | a super-admin token with `amr:["pwd"]` (as minted before Phase T) → 403 `mfa_required` on `GET /provider/me`, `/relays`, `/users` and `POST /provider/users` |
| AT-T11 | PASS | forced change (no TOTP yet) → `mfa:"enroll"` token; forced change with TOTP → `mfa:"verify"`; voluntary change by a signed-in super-admin → `mfa:"verify"`; never a full token, `amr` never has `otp` |
| AT-T12 | PASS | a super-admin demoted to relay-ops with TOTP still on → `mfa:"verify"` at sign-in; a password-only token for them → 403 `mfa_required` |

**Setup note:** the console's `node_modules` predated #138 and lacked `qrcode.react`; `npm ci` (lockfile unchanged) fixed it.

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

### Live run — Phase A (2026-10-26): PASS

**Setup:**
- Isolated local stack, not the lab: a controller built from `da0a1613` on its own throwaway database (`zecurity_live_phasea_*`, all 45 migrations), the isolated test Valkey `:6390` and ports 18080/19090. The dev database (`ztna_platform`) was not touched.
- A real relay (`relay/target/debug/zecurity-relay`) was provisioned against it, giving real serial `16f73897…`.
- Operators signed in for real: bootstrap password → forced change → TOTP enrolment (code computed from the returned secret, standing in for an authenticator app). relay-ops was created through `POST /provider/users`.
- Run from Barath's session; the throwaway database was dropped afterwards.

**Results: 38 checks passed, 0 failed.** One driver check was itself wrong (it read the IPv6 loopback `::1` as "IP with port"); it was corrected and re-run, and passed.

| Case | Result | Evidence |
|---|---|---|
| AT-CORE-2 | PASS | relay-ops `POST …/revoke` and `DELETE` → 403 `forbidden`; relay still `active`; no audit row |
| AT-A1 | PASS | no body → 400 `invalid_request`; blank reason → 400 `reason_required`; 501-char reason → 400 `reason_too_long` |
| AT-A2 | PASS | wrong-case or missing `confirm_name` → 400 `confirmation_mismatch`; malformed UUID → 404 `not_found` |
| AT-A3 | PASS (API and CRL) | revoke → 204; relay `revoked`; certificate revoked with reason `live phase-a revoke`; **published `/relay.crl` lists the serial**; audit row has the actor, reason, `serials_revoked: 1` and IP `::1`, with spoofed `X-Forwarded-For` / `X-Real-IP` ignored. *Connector migration not observed: no connector or tenant on this stack (covered later by AT-X4).* |
| AT-A4 | PASS | revoke again → 409 `already_revoked`; delete a revoked relay → 204; revoke after delete → 409 `already_deleted`; delete again → 409 `already_deleted`; exactly one revoke and one delete audit row |
| AT-A5 | PASS | audit write forced to fail (temporary `NOT VALID` constraint) → 500 with body exactly `{"error":"server_error"}`; relay status unchanged; the server log kept the cause; after the constraint was removed, the same revoke → 204 |
| AT-A6 | PASS | invalid name, DNS SAN, IP SAN, unknown field and oversized body → 400 with the documented codes and **no relay row created**; relay-ops create → 201 + `Cache-Control: no-store`; create audit rows hold no provisioning token; IP without port |
| AT-A7 | PASS | two revokes released together (thread barrier) → exactly one 204 and one 409 `already_revoked` |

No provisioning token, TOTP secret or `otpauth` URI appeared in the controller log.

## Phase E — Relay enrolment

| ID | Scenario | Expected |
|----|----------|----------|
| AT-E1 | Create a relay via the API | 201 with `ca_fingerprint` (= SHA-256 of `/ca.crt` DER) and the controller addresses (or empty); `Cache-Control: no-store` |
| AT-E2 | Console: Add relay with invalid DNS/IP entries | Field errors matching the server's rules |
| AT-E3 | Console: Add relay, close the token dialog | Token shown once; absent from storage, URL and DOM after close; pending relay listed |
| AT-E4 | **Live:** paste the install block on a lab host | Relay goes pending → active; token used up (Valkey JTI gone) |
| AT-E5 | Pending relay whose token expired | Hint explains D-20; delete + add new works |

### Live run — Phase E (2026-10-27): AT-E1 … AT-E3 PASS

**Setup:** isolated local stack, not the lab. A controller built from the Phase E working tree on a throwaway database (`zecurity_live_phasee_*`, 45 migrations), the test Valkey `:6390`, ports 18080/19090, `PROVIDER_RELAY_CONTROLLER_ADDR=localhost:19090`, `PROVIDER_RELAY_CONTROLLER_HTTP_ADDR=localhost:18080`. The console ran under Vite against it and was driven in a real browser (Orca). The super-admin signed in for real (bootstrap password → change → TOTP enrolment, code computed from the setup key). The dev database was not touched; the throwaway database was dropped afterwards.

| Case | Result | Evidence |
|---|---|---|
| AT-E1 | PASS | `POST /provider/relays` → 201, `Cache-Control: no-store`, body has `ca_fingerprint`, `controller_grpc_addr=localhost:19090`, `controller_http_addr=localhost:18080`; the fingerprint equals `curl /ca.crt \| openssl x509 -outform DER \| sha256sum`. A malformed `PROVIDER_RELAY_CONTROLLER_HTTP_ADDR=http://…` stops startup with a clear message. |
| AT-E2 | PASS | `127.0.0.01` in IP addresses → "Enter an IP address in canonical form…", Create disabled; fixed → enabled. |
| AT-E3 | PASS | Token dialog "Install live-e-relay1": installer + direct command + token, 24 h expiry. After **I’ve saved it, close**: token absent from the DOM, `sessionStorage`, `localStorage` and the URL; the list reloaded. A second relay left pending shows "Waiting for enrolment". Token not in the controller log; the `relay.create` audit row holds name/SANs/TTL and IP `::1`, no token. |
| AT-E4 (local stand-in) | PASS locally; **lab run pending** | The direct command copied from the dialog was run unmodified (only `RELAY_BIND` and `RELAY_STATE_DIR` added through the outer environment) with `relay/target/debug/zecurity-relay`: "verified controller Intermediate CA fingerprint", certificate stored, relay `active` within a second. *Valkey JTI consumption not checked in this run.* |
| AT-E5 | Not run live | Needs an expired token (24 h) and Phase X delete. The hint is shown in the token dialog and on the pending relay's detail page (tests). |

Found and fixed during the run: **Add relay** reopened with the previous relay's values (see the phase file → Post-Phase Fixes). Tooling note: in Orca, clicking **Confirm and continue** on `/mfa` and the token dialog's close button didn't fire; `requestSubmit()` / a DOM click did. The login and password forms clicked normally. Worth a glance in AT-T with a real browser and authenticator.

## Phase X — Revoke and delete in the console

| ID | Scenario | Expected |
|----|----------|----------|
| AT-X1 | relay-ops opens a relay | No danger-zone card |
| AT-X2 | Super-admin opens a revoked or deleted relay | No danger-zone card |
| AT-X3 | Revoke dialog without reason or with the wrong name | Button disabled |
| AT-X4 | **Live:** revoke a relay carrying a connector | Connector logs "Active relay absent from new list; migrating immediately"; traffic continues; CRL lists the serial; audit shows the reason |
| AT-X5 | **Live:** delete a relay | Relay leaves the default list; certificates revoked; audit shows the reason |

### Live run — Phase X (2026-10-28)

**Setup:** isolated local stack, not the lab: controller built from the Phase X working tree on a throwaway database, the test Valkey, ports 18080/19090; the console under Vite on its **own origin** (`:5174`, `VITE_PROVIDER_API_BASE` pointing at the controller, `PROVIDER_CONSOLE_ORIGIN` set), so every write went through real CORS as in production. Super-admin signed in for real (bootstrap password → change → TOTP enrolment, code computed from the setup key). One relay provisioned with the real relay binary (certificate serial `a4b780e8…`), one left pending. The database was dropped afterwards.

| Case | Result | Evidence |
|---|---|---|
| CORS | PASS | Preflight `OPTIONS` for `DELETE` from the console origin → 204 with `GET, POST, PATCH, DELETE, OPTIONS`; from a foreign origin → 403. |
| AT-X1 | Covered by tests, not run live | relay-ops gets no danger zone (`RelayDetail.test.tsx`); unknown roles can't open the page. |
| AT-X2 | PASS | After the revoke only **Delete relay** remained; after each delete the danger zone was gone. |
| AT-X3 | PASS | Confirm disabled with no input and with `Live-X-Relay1` (wrong case); enabled with the exact name. |
| AT-X4 | PASS (second run, below) | First run: revoke itself PASS (status, CRL, audit). Connector migration verified in the second run. |
| AT-X5 | PASS | The revoked relay and the pending relay (D-20 path) deleted from the browser (cross-origin `DELETE`): both `deleted`, absent from the default list, one `relay.delete` audit row each with its reason. |

**Second run, AT-X4 with a real connector (2026-10-28):** same kind of isolated stack, with throwaway PKI, JWT and provider secrets. A workspace and its admin were provisioned by the real `bootstrap.Service` (a scratch seed program; never committed); the remote network and connector came from the real GraphQL mutations, the enrollment token from `POST /api/connectors/{id}/token`. Two real relays (rebuilt from current source) reached the controller over the LAN IP, so they were advertised at private addresses (`192.168.1.223:19094` / `:19095`, `RELAY_DEV_ALLOW_PRIVATE_ADDR`). A real connector enrolled and attached to `x4-relay-b`. The super-admin revoked `x4-relay-b` from the console (Orca, cross-origin):
- 111 ms later the connector logged `received LabelledRelayList push … relay_count=1` and **`Active relay absent from new list; migrating immediately relay_id=<x4-relay-b>`**; 31 ms after that `Connector registered with Relay` (now `x4-relay-a`), then `old relay drain completed after active streams reached zero`. The placement row moved to `x4-relay-a`.
- `x4-relay-b` `revoked`; its certificate revoked with the typed reason; `/relay.crl` lists the serial; one `relay.revoke` audit row with the actor, reason and `serials_revoked: 1`.
- Not exercised: "traffic continues" — no client tunnel was open (zero active streams at migration).

## Phase D — Drain and resume

| ID | Scenario | Expected |
|----|----------|----------|
| AT-D1 | Drain an eligible relay (2+ eligible) | 204; relay absent from the published list; list version changed; audit `relay.drain` with actor, reason, client IP |
| AT-D2 | Drain the only eligible relay | 409 `last_eligible_relay`; nothing changed |
| AT-D3 | Two operators drain the last two eligible relays at once | Exactly one 204, the other 409 `last_eligible_relay`; the **published list** still holds one relay (automated: goroutines released together on a real DB) |
| AT-D4 | Resume | 204; relay back in the list; version equals the pre-drain value; audit `relay.resume` |
| AT-D5 | relay-ops drains and resumes | Allowed; revoke/delete still 403 |
| AT-D6 | **Live:** drain a relay with a long download and open a new connection | New connection lands on another relay; download finishes, or is cut at ~120 s; console goes `migrating` → `streams_open` → "Drained (as of the last heartbeat)" |
| AT-D7 | Drain pending / revoked / deleted relays; repeat drain; resume a non-draining relay | 409 `invalid_state` / `already_revoked` / `already_deleted`; 204 no-op, no audit; 204 no-op, no audit |
| AT-D8 | `drain_state` timing (automated, explicit timestamps, no sleeps) | Zero count measured before `drain_started_at + 120 s` → not `drained`; zero count older than 90 s → `unverified`; fresh zero after the window with 0 attached → `drained`; non-zero with 0 attached → `streams_open`; a stale zero can't hide newer streams; attached > 0 → `migrating` |
| AT-D9 | Valkey bookkeeping fails (automated) | Flag set fails after drain, flag delete fails after resume, controller restart while draining, flag expired, flag read error: never an early `drained` (only `unverified` or a later `drained`); the expiry sweep restores the flag |
| AT-D10 | Eligibility fields (automated) | `in_relay_list` and `eligible_relay_count` match `BuildLabelledRelayList` for empty/NULL address, public scope without observed IP, low tier, inactive and draining relays; draining a relay updates both |
| AT-D11 | Last-relay warning in the console | Revoke/delete of the only relay in the list shows the warning; confirm stays enabled and the action succeeds; no warning when another relay is eligible |
| AT-D12 | Broadcast failure (automated) | Logged with the relay id; the drain stays committed; a reconnecting connector receives the current list without the drained relay |

---

## 6. Development workflow checks

- `042_` and `043_` apply on a fresh volume; the controller starts; `provider_users` has the TOTP columns and `relays.drain_started_at` exists.
- No existing schema file changed (`git diff --stat controller/migrations` shows only additions).
