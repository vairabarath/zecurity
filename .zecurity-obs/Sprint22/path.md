---
type: planning
status: planned
sprint: 22
tags:
  - sprint22
  - dependencies
  - execution-path
  - team-coordination
  - provider-dashboard
  - provider-identity
  - totp
  - relay-management
  - relay-drain
---

# Sprint 22 — Provider Dashboard Phase 3: Super-Admin TOTP and Relay Management

> **Read this before writing a single line of code.**
> - **Scope source of truth:** `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"** (D-01 … D-23), the **2026-09-26 amendment** (D-24 … D-29) and the **2026-10-26 amendment** (D-30 … D-34).
> - **Previous sprint:** [[Sprint21/path]] (read-only provider console; redesign merged as PR #136).
>
> This sprint **introduces no architectural decisions** beyond D-30 … D-34, which are already recorded. Where this plan picks an *implementation* approach, it is marked as such and may be changed in code review. Anything that looks like a new decision is listed under **Open questions**. Stop and ask; don't decide it in code.

## Sprint Goal

Make the provider console a **safe place to manage the relay fleet**:
1. **Super-admins must use TOTP** (D-28) before any relay mutation reaches the console (D-33).
2. **Harden the existing relay admin API** for console use: JSON errors, D-17 reason and typed confirmation, and the D-31 role split.
3. **Enrol relays from the console** (one-time token and a ready-to-paste install command).
4. **Revoke and delete relays from the console** (super-admin only, D-17 safeguards).
5. **Drain and resume relays** (D-32), reusing the connectors' existing migration path.

**Out of scope:** tenant suspension (separate epic, D-34), relay pools/regions (D-03), new telemetry (D-23), recovery codes and refresh tokens (D-28), external IdPs (D-29).

**What exists today (verified 2026-10-26):**
- **Relay admin API** (`controller/internal/relay/admin_handler.go`): `POST /provider/relays` (pending relay + one-time 24 h provisioning token, audited `relay.create`), `POST /provider/relays/{id}/revoke`, `DELETE /provider/relays/{id}` (both revoke serials onto the CRL, audit, and call `OnRelayRevoked` → `broadcastRelayList`, `cmd/server/main.go:471`).
  - Revoke reads its reason from an **optional query parameter**; Delete uses a fixed reason (`"relay deleted"`). D-17 is not met.
  - Errors are **plain text** (`http.Error`) and some append `err.Error()`; the console expects `{"error":"<code>"}`.
  - `Create` is authorised by `CanIssueProvisioningToken` (`relay.issue_token`); revoke/delete by `CanRevokeRelay` / `CanDeleteRelay`. Under today's prefix rule (`authz.go` `decide()`), **relay-ops can revoke and delete**.
  - The audit IP is `r.RemoteAddr` (with port); provider auth uses `clientIP(r)`.
  - `RevokeRelay` (`relay/store.go:346`) is idempotent and silent on an already revoked relay, and never downgrades `deleted`.
- **Relay enrolment needs** `RELAY_ID`, `CONTROLLER_ADDR`, `CONTROLLER_HTTP_ADDR`, `RELAY_CA_FINGERPRINT`, `RELAY_PROVISIONING_TOKEN` (or `_FILE`), optional `RELAY_DNS_SANS` / `RELAY_IP_SANS` / `RELAY_BIND` / `RELAY_STATE_DIR` (`relay/src/config.rs`). The fingerprint is SHA-256 of the intermediate CA DER, the same value the shield token uses (`shield/token.go` `loadCAFingerprint`) and `/ca.crt` serves.
- **Published relay list** (`relay/store.go` `BuildLabelledRelayList`): `status='active'`, capacity label high/medium, routable address. Its `version` is a **content hash** (`relayListVersion`), so any membership change triggers connectors to re-probe.
- **Connector migration** (`connector/src/relay_selector.rs:336`): when the active relay leaves the list, the connector migrates immediately, keeps the old session until its streams reach zero or `relay_drain_timeout_secs` (default **120**, `connector/src/config.rs:48`) passes, then closes it. Drain needs **no connector change**.
- **Provider identity** (`controller/internal/provider/`): a single token path `IdentityService.IssueSession(u, amr, pwcOnly)`; claims `gen`, `pwc`, `amr` (`["pwd"]`); full TTL 15 min, `pwc` TTL 10 min; `RequireProvider` loads the user by ID and checks `gen`; the Valkey login limiter (5 per email / 20 per IP / 15 min). `provider_users` has `password_hash`, `must_change_password`, `session_generation`, `last_login_at` (037). **No MFA columns or code.**
- **At-rest secret encryption** already exists: `pki.EncryptSecret` / `DecryptSecret` (AES-256-GCM + HKDF under `PKI_MASTER_SECRET`, caller-unique context; used for OIDC client secrets).
- **Break-glass** (`zecurity-controller provider recover-admin`) is documented (ADR-029, D-26 note) but **not built**; the DR targets it "at the latest with the super-admin TOTP follow-up".

## Development rule — schema changes reset the local database

Unchanged from Sprint 21 (`docs/database-development.md`): a schema change is a new numbered SQL file in `controller/migrations/`; nothing upgrades an existing database; every schema PR is labelled **`[schema: reset DB]`** and everyone runs `cd controller && docker compose down -v && docker compose up -d`. **Never edit an existing schema file.**

This sprint uses exactly two new files: **`042_provider_totp.sql`** (Phase T) and **`043_relay_drain.sql`** (Phase D). The next free number after this sprint is `044_`.

## Constraints from the Decision Record (binding)

| Ref | Constraint for this sprint |
|-----|----------------------------|
| **D-02** | Single controller instance. Broadcasts and the live-stream registry are process-local; no cross-replica work. |
| **D-04** | REST under `/provider/*`. Mutations are explicit verbs (`/revoke`, `/drain`, `/resume`). |
| **D-05 → D-31** | Roles stay `super-admin` / `relay-ops`. **Exception:** `relay.revoke` and `relay.delete` are super-admin only, enforced in `decide()`. relay-ops keeps relay read, create (`relay.issue_token` / `relay.create`), drain and resume. |
| **D-14** | Every relay mutation, TOTP enrolment, TOTP reset and recovery is audited in `provider_audit_logs`, transactionally with the change. |
| **D-17** | Relay revoke and delete need a **mandatory reason** (stored in the audit) and a **typed confirmation** (the relay name). No dual approval or step-up beyond TOTP. |
| **D-20** | A relay with an expired token or lost key is **replaced** (delete, then create). No token re-issue. |
| **D-25** | Any credential change (TOTP enrol, TOTP reset, recovery) bumps `session_generation`. |
| **D-28 → D-33** | Super-admin TOTP is **mandatory** and ships **before** any console relay mutation. Recovery codes, rotation policies and refresh tokens stay deferred. |
| **D-30** | Console writes allowed: auth calls, operator management, relay create / revoke / delete / drain / resume. **Nothing else.** |
| **D-32** | Drain = exclusion from the published relay list. Keep the last-eligible-relay guard, audit, connector migration behaviour and the 120 s connector drain timeout. **No connector or proto change.** **Draining reduces relay availability:** it removes capacity from the fleet, and streams on the drained relay can be cut when the connector's 120 s drain timeout passes. relay-ops can drain (D-31), so the console states this before confirming. |
| **D-34** | Tenant suspension is **not** in this sprint. Don't change the `workspaces.status='active'` predicates. |
| **ADR-029** | Provider Identity stays in `controller/internal/provider`. |
| Sprint 20 invariants | `revoked` (and `deleted`) relays are terminal: no write path may leave them. Relay/connectivity changes notify the transport plane, never the ACL (ADR-017). |

## Implementation choices made in this plan (not architectural — reviewable)

| Area | Choice | Why |
|------|--------|-----|
| TOTP algorithm | RFC 6238: SHA-1, 6 digits, 30 s step, accept ±1 step. 20-byte secret from `crypto/rand`, Base32 in an `otpauth://totp/Zecurity%20Provider:<email>?secret=…&issuer=Zecurity%20Provider` URI. | Works with every authenticator app. |
| TOTP storage | `042_provider_totp.sql`: `totp_secret_ciphertext TEXT`, `totp_secret_nonce TEXT`, `totp_enabled_at TIMESTAMPTZ`, `totp_last_step BIGINT`. Secret encrypted with `pki.EncryptSecret`, context `"provider-totp:"+user_id`. | Reuses the audited primitive; domain-separated key. |
| Replay protection | Reject a code whose step is ≤ `totp_last_step`; update it in the same statement that accepts the code. | A code can't be used twice, even within its window. |
| Login flow | Password OK, then: (1) `must_change_password` → existing `pwc` token, unchanged; (2) operator **requires TOTP** and is enrolled → **`mfa:"verify"` token** (5 min) accepted only by `POST /provider/auth/totp/verify`; (3) super-admin **not enrolled** → `mfa:"enroll"` token accepted only by `POST /provider/auth/totp/enroll` and `/enroll/confirm`. A password change ends the same way (an operator who requires TOTP gets an `mfa` token, not a full one). A full token is minted only by `IssueSession` with `amr:["pwd","otp"]`. | Same confined-token pattern as `pwc`; one minting path (D-29); no parallel mechanism. |
| Enforcement | An operator **requires TOTP** if `role = super-admin` **or** TOTP is enabled on the account. Checked twice: `IssueSession` refuses to mint a full session without `otp` for such an operator, and `RequireProvider` rejects such an operator's full token without `otp` with 403 `mfa_required` on every request (DB row, not token claims). | No token issuer and no pre-deploy token can bypass TOTP (OQ-3). |
| TOTP rate limit | Valkey, per user: 5 failed codes per 15 min → 429 `too_many_attempts`; a trip writes `provider_auth.totp_rate_limit`. | Matches the login limiter; a 6-digit code must not be brute-forced. |
| Enrolment secret | Shown **once** in the `/enroll` response (`Cache-Control: no-store`), never logged or audited; console renders the URI as a QR code and the Base32 text, holds it only in component state. | Same discipline as temporary passwords. |
| QR rendering | Add one small client-side QR dependency to `provider-console` (no network use). Reviewed in Phase T. | No server-side image generation. |
| TOTP reset | `POST /provider/users/{id}/reset-totp` `{reason}` (1–500 chars): super-admin only; no self-reset; disabled target → 409 `account_disabled`; not enrolled → 409 `totp_not_enrolled`; clears secret, enabled time and last step in one transaction with the audit row and a generation bump. | OQ-1. |
| Break-glass | `zecurity-controller provider recover-admin --email <e>`: on the controller host, resets the target super-admin's password (new temporary, printed once) **and** clears its TOTP, bumps the generation, writes `provider_user.recover_admin` audit (actor `system:cli`). | DR target "with the TOTP follow-up"; the last super-admin losing a phone must not need SQL. |
| Relay errors | `{"error":"<code>"}` via the provider JSON helpers, exported for the relay package (`provider.WriteError`, `provider.ClientIP`; today unexported `writeAuthError` / `clientIP` in `auth_handlers.go`). Codes: `invalid_request`, `invalid_name`, `invalid_dns_san`, `invalid_ip_san`, `reason_required`, `reason_too_long`, `confirmation_mismatch`, `not_found`, `forbidden`, `already_revoked`, `already_deleted`, `invalid_state`, `last_eligible_relay`, `server_error`. No internal error text in responses. | Consistent with operator routes; nothing leaks. |
| D-17 server check | Revoke/delete body `{"reason": "...", "confirm_name": "<relay name>"}`. Reason trimmed, 1–500 chars; `confirm_name` must equal the relay's name exactly. | The typed confirmation can't be skipped by a script. |
| Repeat actions | Revoke on `revoked` → 409 `already_revoked`; any action on `deleted` → 409 `already_deleted`; delete on `revoked` is allowed (revoke-then-remove). | Clear answers instead of silent no-ops. |
| Install command | `POST /provider/relays` response adds `ca_fingerprint`, and `controller_grpc_addr` / `controller_http_addr` from new **optional** settings `PROVIDER_RELAY_CONTROLLER_ADDR` / `PROVIDER_RELAY_CONTROLLER_HTTP_ADDR` (empty → the console shows placeholders). Response is `Cache-Control: no-store`. | One paste on the relay host; fingerprint is public. |
| Drain storage | `043_relay_drain.sql`: `relays.drain_started_at TIMESTAMPTZ NULL`. A column, not a status. | Heartbeats already move `status` between active/inactive; drain is orthogonal. |
| Drain list rule | `BuildLabelledRelayList` adds `AND drain_started_at IS NULL`. | The content-hash version changes on its own; connectors migrate. |
| Last-eligible guard | In one transaction: lock the currently eligible relay rows (`FOR UPDATE`, same predicate as the list); if the target is eligible and is the only one, 409 `last_eligible_relay`. A non-eligible target (inactive, low tier) may be drained. | Two concurrent drains can't empty the list. |
| Drain states | Drain allowed on `active` / `inactive`; `pending` → 409 `invalid_state`; `revoked` / `deleted` → 409 `already_revoked` / `already_deleted`. Resume on a non-draining relay → 200 no-op, not audited. | Mirrors the operator-route no-op rule. |

## Open questions (answer before the phase that needs them)

| # | Question | Needed by | Recommendation |
|---|----------|-----------|----------------|
| OQ-1 | **Lost authenticator.** May a super-admin reset another operator's TOTP? | T | **Decided 2026-10-26: YES.** Server-side authorization (`provider_user.manage`, super-admin); the acting super-admin must hold a TOTP-backed session (enforced by `RequireProvider` for every super-admin request); a **reason is required** and audited with actor, target and timestamp; the target's enrolment is cleared (secret, enabled time, last step) and its sessions end (generation bump), so it must enrol afresh; no self-reset; no unauthenticated self-service recovery. `recover-admin` stays the path for a locked-out last super-admin. |
| OQ-2 | **relay-ops and TOTP.** | T | **Decided 2026-10-26: NO mandatory TOTP for relay-ops in Sprint 22.** relay-ops may create, drain and resume relays; only super-admins revoke or delete (D-31). Every endpoint enforces roles server-side; console visibility is never the control. Voluntary relay-ops enrolment is **not built** in Sprint 22 (deferred). An operator who already has TOTP enabled (e.g. a demoted super-admin) must still verify it. |
| OQ-3 | **Rollout.** | T | **Decided 2026-10-26: NO grace period.** From the Phase T deploy, a super-admin session without `otp` is rejected on its next request, regardless of its remaining lifetime; the 15-minute TTL is **not** the enforcement mechanism. There are no refresh tokens (D-28), and every token-issuing path (login, password change, TOTP verify, TOTP enrol confirm) goes through `IssueSession`, which refuses a full session without `otp` for an operator who requires TOTP. Enrolment (forced at sign-in) and recovery (`reset-totp`, `recover-admin`) ship in the same release as enforcement. |

## Team Assignments

| Member | Person | Role | Area |
|--------|--------|------|------|
| **M1** | Sathiya | Testing | Acceptance runs per phase (AT-T, AT-A, AT-E, AT-X, AT-D), live checks on the lab fleet, F49 follow-up (separate). |
| **M2** | Barath | Go + Rust + React | All Sprint 22 development: phases T, A, E, X, D. |

**Phase files:**
- [[Sprint22/Member2-Go-Rust/Phase1-Super-Admin-TOTP]] (T)
- [[Sprint22/Member2-Go-Rust/Phase2-Relay-Admin-API-Hardening]] (A)
- [[Sprint22/Member2-Go-Rust/Phase3-Relay-Enrollment]] (E)
- [[Sprint22/Member2-Go-Rust/Phase4-Relay-Revoke-Delete-Console]] (X)
- [[Sprint22/Member2-Go-Rust/Phase5-Relay-Drain]] (D)
- [[Sprint22/Acceptance-Test-Plan]]

## Critical Rule: Conflict Zones

| File | Phase | Rule |
|------|-------|------|
| `controller/migrations/` | T (`042_`), D (`043_`) | Only these two files. Never edit existing ones. Each PR labelled `[schema: reset DB]`. |
| `controller/internal/provider/identity.go`, `session.go`, `auth_handlers.go`, `store.go` | T | TOTP only. `IssueSession` stays the single minting path. |
| `controller/internal/middleware/provider.go` | T | Adds the `mfa` token confinement and the super-admin `otp` check. |
| `controller/internal/provider/authz.go` | A | The D-31 exception only; new actions `relay.drain` / `relay.resume` added in D. |
| `controller/internal/relay/admin_handler.go`, `store.go` | A, E, D | Surgical edits; A before E before D. |
| `controller/cmd/server/main.go` | T (routes, CLI), E (settings), D (routes) | Order **T → A → E → D**. |
| `provider-console/src/api/*`, `api-surface.test.ts`, `test/static-guards.test.ts` | T, E, X, D | Every new write is added to the API-surface pin; clipboard guard extended once (E) for the token dialog. |
| `connector/**`, `relay/**`, `proto/**` | — | **No changes this sprint.** |
| `admin/` | — | **Not modified.** |

## Dependency Graph

```text
T  Super-admin TOTP + recover-admin  ──────────────┐   (D-33: nothing below ships first)
                                                   ▼
A  Relay admin API hardening (errors, D-17, D-31) ─┬──────────────┐
                                                   ▼              ▼
E  Relay enrolment (API fields + console)     X  Revoke/delete console
                                                   │
                                                   ▼
D  Drain + resume (043, API, list rule, console)  (needs A's error/audit helpers; X's danger-zone card)
                                                   ▼
V  Acceptance (M1) for each phase as it merges; final sprint run
```

E and X are independent once A is merged and may be built in either order. D comes last because it reuses A's helpers and X's danger-zone card.

## Execution Path (implementation checklist)

Every item is **test-first**: write the failing test, then the code, then tick the box. A phase is done only when its **test gate** passes with DB-backed tests running (not skipped).

### Phase T — M2: Super-Admin TOTP (D-28, D-33) + break-glass

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase1-Super-Admin-TOTP]]. **Blocks everything else.**

- [x] **T0** OQ-1, OQ-2, OQ-3 answered (2026-10-26) and recorded above.
- [x] **T1** *(2026-10-26: written first; confirmed failing — compile errors naming the missing pieces, then assertion failures against signature-only stubs.)* Tests first: TOTP generation/verification vectors (RFC 6238 appendix B), ±1 step, replay rejection, rate limit, encrypt/decrypt round trip with the domain context.
- [x] **T2** `042_provider_totp.sql` `[schema: reset DB]`; store methods (enrol begin/confirm, verify with step update, reset).
- [x] **T3** Login flow (and the password-change path, which now ends in the TOTP step for an operator who requires it): `mfa:"verify"` and `mfa:"enroll"` confined tokens; `POST /provider/auth/totp/verify`, `/totp/enroll`, `/totp/enroll/confirm`; full token `amr:["pwd","otp"]` via `IssueSession` only.
- [x] **T4** `IssueSession` refuses a full session without `otp` for an operator who requires TOTP; `RequireProvider`: super-admin without `otp` → 403 `mfa_required`; `mfa` tokens confined to their routes.
- [x] **T5** Operator TOTP reset (OQ-1): `POST /provider/users/{id}/reset-totp` with a required `reason`; super-admin only; no self-reset; disabled target refused; clears the enrolment; generation bump; audit `provider_user.totp_reset` with the reason.
- [x] **T6** `zecurity-controller provider recover-admin --email`: resets password + TOTP, prints the temporary password once, audit `provider_user.recover_admin`.
- [x] **T7** Console (`/mfa` page; QR via `qrcode.react@4.2.0`, pinned): second login step (code entry), enrolment screen (QR + text, confirm code), the same steps after a password change, `mfa_required` handling, Operators "Reset MFA" with a reason; API-surface pin updated.
- [x] **T8** Docs: `CLAUDE.md` switched to Sprint 22 as the active sprint, with the console write rule from D-30 (2026-10-26, after PR #138 merged as `7621e260`); `agent.md` doesn't repeat the rule, so it's unchanged.
- [x] **T-gate** see Test gates. **PASS 2026-10-28 (Sathiya) on `fixed-pendings` `84a06023`, CI-style env** (every `*_TEST_DATABASE_URL` → empty `ci_empty_admin`, `AUTH_TEST_VALKEY_URL` → throwaway Valkey `:6390/15`; dev DB and dev Valkey untouched):
  - Go: `go build ./...` + `go vet ./...` clean; gate packages (`provider`, `middleware`, `relay`, `providerquery`) 338 pass, **0 skip**; whole controller 1457 pass / 0 fail / 2 skip (both are unconditional `t.Skip` stubs, not DB/Valkey gates).
  - Console: `npm test` 395 pass, `npm run lint` clean, `npm run build` ok.
  - Schema: clean init on a scratch Postgres volume (45 SQL files, 0 errors); controller started on it (:18080 / :19090); `provider_users.totp_*` columns present; bootstrap operator created.
  - Live: AT-T PASS 2026-10-27 (V1).
  - *Earlier note:* *(2026-10-26, local: Go — build ok, `go test ./...` 862 pass / 0 fail / 32 skip, all skips outside Phase T (posture/resource suites needing `RESOURCE_TEST_SHIELD_ID`, plus 2 intentional); provider, middleware and cmd/server ran their DB- and Valkey-backed tests with no skips. Console — 395 tests pass, lint clean, build ok. Schema — `042_` applied on every throwaway test database; the dev database was **not** reset (team rule), so `[schema: reset DB]` applies at merge. Still open: live AT-T run with a real authenticator (M1), and T8.)*

### Phase A — M2: Relay admin API hardening (D-17, D-31)

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase2-Relay-Admin-API-Hardening]]. **Depends on T merged.**

- [x] **A1** *(2026-10-26: written first; all 10 failed for the expected reasons — relay-ops allowed to revoke/delete, plain-text errors, silent repeats, leaked DB error on audit failure, `RemoteAddr` with port.)* Tests first: role matrix (relay-ops 403 on revoke/delete, 2xx on create), every error code, reason/confirmation validation, repeat-action 409s, audit row contents (reason, actor, client IP), rollback on audit failure.
- [x] **A2** `decide()`: the D-31 exception for `relay.revoke` / `relay.delete`.
- [x] **A3** Export `provider.WriteError` / `provider.ClientIP` (thin wrappers over the existing helpers); JSON errors on all three relay admin routes; no internal error text.
- [x] **A4** Revoke/delete JSON body `{reason, confirm_name}`; reason into audit details and `revocation_reason`; `clientIP(r)` for the audit IP.
- [x] **A5** `already_revoked` / `already_deleted` handling (name and status checked under the row lock via the new `Store.RevokeRelayGuarded`; `RevokeRelay` unchanged for existing callers); stale `Delete` doc comment corrected.
- [x] **A-gate** *(2026-10-26, local: `go test ./...` 872 pass / 0 fail / 32 skip, all skips outside Phase A; relay, provider, middleware and cmd/server ran their DB- and Valkey-backed tests with 0 skips. Console untouched: 395 pass. Live AT-A on the lab still pending (M1).)*

### Phase E — M2: Relay enrolment (D-30, D-31, D-20)

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase3-Relay-Enrollment]]. **Depends on A merged.**

- [x] **E1** Tests first: create response fields, `no-store`, fingerprint equals `/ca.crt` SHA-256, optional settings empty/set; console dialog, token never in URL/storage, cleared on close.
- [x] **E2** Controller: `ca_fingerprint` (shared helper with the shield token), `controller_grpc_addr` / `controller_http_addr` from the optional settings; `.env.example` entries.
- [x] **E3** Console: "Add relay" on Relays (super-admin and relay-ops); form with DNS/IP inputs (plain comma/space-separated fields, not chips) validated like the server; one-time token dialog with Copy and the install block; pending-relay hint (D-20).
- [x] **E4** Clipboard static guard extended to the token dialog only; API-surface pin.
- [x] **E-gate** *(2026-10-27, local: `go test ./...` 1420 pass / 0 fail / 2 skip, both unconditional stubs; every DB- and Valkey-backed test ran. Console 436 pass, lint and build clean. Live AT-E1…E3 PASS on an isolated stack; AT-E4/E5 on the lab pending (M1).)*

### Phase X — M2: Revoke and delete in the console (D-17, D-31)

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase4-Relay-Revoke-Delete-Console]]. **Depends on A merged.**

- [ ] **X1** Tests first: danger-zone visibility (super-admin only; hidden on revoked/deleted), required reason, typed name enables the button, error mapping, refresh after success.
- [ ] **X2** `ConfirmDangerDialog` (reason + typed confirmation), reused by D.
- [ ] **X3** Relay detail "Danger zone": Revoke and Delete; API-surface pin.
- [ ] **X-gate**

### Phase D — M2: Drain and resume (D-32)

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase5-Relay-Drain]]. **Depends on A and X merged.**

- [ ] **D1** Tests first: list exclusion and version change, resume restores, last-eligible guard incl. two concurrent drains, state rules, audit, authz (relay-ops allowed), broadcast called once post-commit.
- [ ] **D2** `043_relay_drain.sql` `[schema: reset DB]`.
- [ ] **D3** `POST /provider/relays/{id}/drain` and `/resume`; `relay.drain` / `relay.resume` actions; audit; `broadcastRelayList` post-commit.
- [ ] **D4** `BuildLabelledRelayList` rule; `RelaySummary.draining_since` (additive).
- [ ] **D5** Console: Draining badge, Drain/Resume on Relay detail, attached-connector progress and "Drained" state, Overview attention item; API-surface pin.
- [ ] **D6** Regression: connector `relay_selector` tests run unchanged (no connector code touched).
- [ ] **D-gate**

### Phase V — M1: Acceptance

- [x] **V1** Run AT-T on the lab after T merges. *(2026-10-27, Sathiya: **PASS**, 40/40 API checks plus the console QR/verify pages. Run on an isolated local stack (throwaway DB, own Valkey, controller :18080) rather than the lab fleet: AT-T needs only the controller and the console. See Acceptance-Test-Plan → Live run — Phase T.)*
- [ ] **V2** Run AT-A, AT-E, AT-X after their merges. *(AT-A: live PASS 2026-10-26 on an isolated local stack, 38/38 checks; see the Acceptance Test Plan. AT-E: live E1…E3 PASS 2026-10-27 on an isolated local stack; E4/E5 on the lab pending. AT-X pending.)*
- [ ] **V3** Run AT-D on the lab fleet (≥ 2 relays) after D merges.
- [ ] **V4** Final sprint run of [[Sprint22/Acceptance-Test-Plan]]; failures filed as Post-Phase Fixes; Session Log entry.

## Test gates (a phase can't be ticked without these)

Run with the Sprint 21 DB/Valkey test environment (throwaway databases, isolated Valkey `ci_valkey_6390`; **never** the dev database or dev Valkey). DB-backed suites must **run**, not skip.

| Gate | Commands | Must show |
|------|----------|-----------|
| **Go** (T, A, E, D) | `cd controller && go build ./... && go vet ./...`; `go test ./internal/provider/... ./internal/middleware/... ./internal/relay/... ./internal/providerquery/...` with `*_TEST_DATABASE_URL` and `AUTH_TEST_VALKEY_URL` set | all pass; no `SKIP` for DB/Valkey suites |
| **Console** (T, E, X, D) | `cd provider-console && npm test && npm run lint && npm run build` | all pass; API-surface and static-guard tests updated in the same PR |
| **Schema** (T, D) | `cd controller && docker compose down -v && docker compose up -d` on a scratch volume, controller starts, new columns present | clean init with `042_` / `043_` |
| **Connector regression** (D) | `cd connector && cargo test relay_selector` | unchanged and passing |
| **Live** (each phase) | the phase's AT cases on the lab, by M1 | recorded in the Acceptance Test Plan |

## Sprint 22 Done Criteria

- [ ] Super-admins **cannot act** without TOTP; enrolment, verification, replay rejection, rate limit and recovery work and are audited.
- [ ] Relay revoke/delete are **super-admin only** server-side; relay-ops can create, drain and resume.
- [ ] Revoke/delete require a **reason and typed name**, server-checked; errors are JSON codes; repeat actions get clear 409s.
- [ ] A relay can be **enrolled from the console** with a one-time token and a complete install command.
- [ ] A relay can be **drained and resumed**; connectors migrate with no connector change; the last eligible relay can't be drained.
- [ ] The console's writes are exactly: auth calls, operator management, relay create/revoke/delete/drain/resume (API-surface test).
- [ ] No connector, relay, proto or `admin/` changes; `workspaces.status='active'` predicates untouched.
- [ ] All scenarios in [[Sprint22/Acceptance-Test-Plan]] pass.

## Follow-ups (tracked)

| # | Item | Source | Status |
|---|------|--------|--------|
| FU-1 | **Create audit atomicity (D-14).** `POST /provider/relays` writes its `relay.create` audit best-effort, outside any transaction, after the relay row, the Valkey JTI and the JTI attach. A failed audit write is only logged, and a server error part-way through can leave a pending relay without a usable token. Revoke and delete are transactionally audited (Phase A). Fix: relay row + audit in one transaction, then token, Valkey JTI and attach, with clean-up on failure. | Phase A decision, 2026-10-26 | Open — separate change; Create must not be described as transactionally audited until then |
| FU-2 | **Workspace consistency in the schema (B-1 defense in depth).** `access_rules` has single-column foreign keys, so the database accepts a rule whose workspace, resource and group belong to different workspaces; `group_members` has no workspace column at all. Add `UNIQUE (id, tenant_id)` on `resources` and `remote_networks`, `UNIQUE (id, workspace_id)` on `groups`, composite foreign keys from `access_rules` and `resources (remote_network_id, tenant_id)` (the resource-network fix showed the application can miss such a reference); and put the reporting connector into `connector_scan_results`' key (or record which connector each scan request was sent to), so a report can never suppress or stand in for another connector's result (FU-3 residual); decide how to constrain `group_members` (column + composite FK, or trigger). Needs a migration number (043 is reserved for Phase D). | B-1 fix, 2026-10-27 | Open — the application and compiler now enforce it |
| FU-3 | **`triggerScan` / `getScanResults` are not workspace-scoped (security, likely High).** `triggerScan(connectorId, …)` pushes a scan command to any connected connector by ID (`ConnectorRegistry.PushScanCommand`), the connector scans any non-loopback, non-multicast target (up to 512 targets × 16 ports), and `getScanResults(requestId)` reads results by request ID alone, which `triggerScan` returns. A workspace admin who knows another workspace's connector ID (sent to that workspace's devices in ACL snapshots) could likely scan that workspace's internal network through its own connector. Found by code reading during the resource-network review; not reproduced (needs a connected connector). | Resource-network review, 2026-10-27 | **Fixed** (branch `security/fu3-scan-authorization`, see Post-Sprint Fixes → FU-3); result squatting moved to FU-2 |
| FU-4 | **`promoteDiscoveredService` can't create a resource:** it never sets `remote_network_id` (the shield's network would be the natural value). Feature repair, not security. | Resource-network review, 2026-10-27 | **Fixed** 2026-10-28 (branch `fix/m1-tgate-fu4`, see Post-Sprint Fixes → FU-4) |

## Post-Sprint Fixes

### Fix: B-1 cross-tenant access rules, groups and memberships (security, High)
**Issue:** an admin of one workspace could read and change another workspace's policy data by sending its IDs (reproduced on a throwaway database with two fabricated workspaces):
- `group(id)` returned any workspace's group, its members' emails and its resources' hosts/ports;
- `assignResourceToGroup` accepted a foreign resource or group, wrote a row mixing workspaces, and its response listed the foreign resource and the foreign workspace's groups (names and IDs); its `ON CONFLICT` touched the foreign workspace's own row;
- `unassignResourceFromGroup` deleted the foreign workspace's rule. That workspace's users lost access once its snapshot was next rebuilt, and only the *caller's* workspace was notified;
- `addGroupMember` accepted a foreign user and returned their email;
- a mixed row pulled the foreign resource and the foreign remote network's connectors (LAN addresses) into the caller's compiled snapshot.

No cross-tenant network access: the foreign connector enforces its own workspace's snapshot (exact SPIFFE match). Attacker needs: any workspace admin role (self-serve sign-up creates one) and one foreign resource ID (every end-user device of that workspace receives resource IDs in its ACL snapshot); the assign response then disclosed the group IDs needed for unassign.

**Root Cause:** the resolvers checked the role (`@hasRole(ADMIN)`) but not ownership of the IDs; the store mutations and the read helpers (`GetGroup`, `ListGroupMembers`, `ListResourcesForGroup`, `ListGroupsForResource`, `loadResourceByID`, `loadUser`; `loadResourceWithGroups` ignored its tenant argument) looked rows up by ID alone; the compiler joined resources, shields and connectors without a workspace check; the schema can't express workspace consistency (FU-2).

**Fix Applied:**
- `internal/policy/store.go`:
  - `AssignResourceToGroup` inserts only when both the resource and the group belong to the workspace; `ON CONFLICT … DO UPDATE … WHERE access_rules.workspace_id = EXCLUDED.workspace_id`;
  - `UnassignResourceFromGroup` adds `AND workspace_id = $3`;
  - `AddGroupMember` inserts only when the group and the user belong to the workspace (an existing member still succeeds);
  - the list helpers take the workspace and join resources/groups/users on it;
  - `GetGroup` and the mutations answer a malformed ID with `ErrNotFound` instead of a cast error;
  - compiler: `ListEnabledRulesWithResources` requires the resource, group, remote network and shield to belong to the rule's workspace (a foreign shield reads as missing, so the entry fails closed); `GetConnectorsForRemoteNetworks` takes the workspace and filters on it.
- `graph/resolvers/policy.resolvers.go`, `policy_helpers.go`: workspace passed everywhere; a foreign ID answers exactly like a missing one, with a user-safe message (`group not found`, `resource or group not found`, `rule not found`, `group or user not found`); other errors stay masked by the fail-closed `ErrorPresenter`.
- Mutations notify only the caller's workspace, which is now the only one they can change.

**Tests:** `graph/resolvers/policy_tenant_isolation_test.go` (7 tests: foreign group query, foreign/missing/malformed IDs on assign with no row written and no version bump, `ON CONFLICT` guard (mutation-checked), unassign, foreign user, planted foreign rows hidden from reads, same-workspace behaviour) and `internal/policy/compiler_tenant_isolation_test.go` (planted rows and a foreign shield never cross tenants; connector lookup scoped). All failed before the fix. Full controller suite: 1420 pass / 0 fail / 2 skip (unconditional stubs), every DB-backed test ran.

**Not done here:** schema constraints (FU-2); lab database check for existing mixed rows (queries in the Session Log entry, for M1).

### Fix: resources could reference another workspace's remote network (security, found in the B-1 review)
**Issue:** `createResource` / `updateResource` stored any `remote_network_id` they were given. A workspace-A admin could put a resource on workspace B's network (B's network IDs reach every B device in its ACL snapshot). Reproduced on a throwaway database: both calls accepted B's network and the response showed **B's network name**. Before the B-1 compiler filter, A's snapshot also routed that resource through **B's network and B's connector** (its LAN address reached A's devices; B's connector would still deny A's devices, since it enforces B's snapshot). A missing network gave a generic INTERNAL error instead of a not-found.

**Root Cause:** `resource.Create` / `resource.Update` (`internal/resource/store.go`) wrote the network ID without checking its tenant (`AutoMatchShield` is tenant-scoped but only finds no shield); the read joins (`resourceJoins`, `loadResourceByID`) joined the network by ID alone; the schema has a single-column foreign key (FU-2).

**Fix Applied:**
- `Create`: `INSERT … SELECT … FROM remote_networks WHERE id = $2 AND tenant_id = $1`, so a foreign or missing network writes nothing; `Update`: a new network adds `AND EXISTS (SELECT 1 FROM remote_networks WHERE id = … AND tenant_id = $2)` to the same `UPDATE`, so no field changes. Both answer `resource.ErrRemoteNetworkNotFound` (user-safe "remote network not found", the same for foreign, missing and malformed IDs).
- Reads: the network join requires `rn.tenant_id = r.tenant_id` (LEFT JOIN, name `''`), so a legacy row still lists and can be moved back, but never shows the foreign name.
- Compiler: already covered by the B-1 filter (`rn.tenant_id = ar.workspace_id`); pinned by a test.

**Tests:** `graph/resolvers/resource_network_ownership_test.go` (create and update with foreign/missing/malformed network rejected with no row change and no version bump; same-workspace create/move/description-only update; legacy row hides the foreign name and can be repaired; direct store calls under a concurrent burst) and `internal/policy/compiler_resource_network_test.go` (legacy resource on a foreign network never brings that network or its connectors into the snapshot; fails on the pre-B-1 base). Full controller suite: 1426 pass / 0 fail / 2 skip (unconditional stubs).

**After merging #146 (Fix 18):** `resource.Update` became one transaction (row lock, shield re-binding); the network-ownership check now runs inside that transaction before the write (`SELECT EXISTS … remote_networks WHERE id = … AND tenant_id = …`; a network never changes tenant). #146's new `ListGroupsByResource` got the B-1 joins (resource and group must belong to the rule's workspace).

**Depends on:** the B-1 fix (this branch is stacked on its uncommitted changes; the compiler guarantee comes from B-1).

**Review follow-up (same branch):** the independent review found that the new not-found answer created a probe in `promoteDiscoveredService`, and the same unscoped lookup let `getDiscoveredServices` return **any** workspace's discovered services (bound IPs, ports, service names), both reproduced before the fix. `discovery.GetDiscoveredServices` now takes the workspace and checks the shield's ownership in the same query (`shields LEFT JOIN shield_discovered_services … WHERE s.id = $1 AND s.tenant_id = $2`); a foreign, missing or malformed shield is `discovery.ErrShieldNotFound` ("shield not found"), the same for all three. Tests: `graph/resolvers/discovery_tenant_isolation_test.go` (3). Also strengthened: the legacy-row read test now fails on a read error and asserts the exact result; the compiler test has a positive control (same group and device, own network); the direct-store test no longer claims concurrency coverage (the check is in the write statement itself). Full controller suite: 1429 pass / 0 fail / 2 skip.

*Fixed later (FU-4, 2026-10-28):* ~~`promoteDiscoveredService` never sets a remote network, so it can't create a resource even on the caller's own shield.~~ It now uses the shield's network.

### Fix: FU-3 network scans and scan results were not workspace-scoped (security)
**Issue:** an admin of one workspace could use another workspace's connector to scan, and read another workspace's scan results:
- `triggerScan(connectorId, …)` pushed the scan command to any connected connector by ID (`ConnectorRegistry.PushScanCommand(connectorID, msg)` ignored the stream's tenant). The connector scans any target except loopback, multicast and unspecified addresses (up to 512 targets × 16 ports), so this reached the other workspace's internal network through its own connector. Connector IDs reach every device of that workspace in its ACL snapshot.
- `getScanResults(requestId)` read results by request ID alone, so the results of any request whose ID the caller had (or that a foreign connector reported under the same ID) were returned.

Reproduced on a throwaway database and with fake connector streams: B's connector received A's command; A read B's results, and both workspaces' rows under a shared request ID. Not run against a real network.

**Root Cause:** the registry already records each live stream's tenant (from the authenticated connector record, `Control`), but `PushScanCommand` didn't compare it; scan results carry only the reporting connector, and `GetScanResults` never joined it to a workspace.

**Fix Applied:**
- `internal/connector/control_stream.go`: `PushScanCommand(workspaceID, connectorID, msg)` sends only if the live stream's tenant equals `workspaceID`; otherwise `ErrConnectorNotAvailable` ("connector not found or not connected", user-safe; the same for foreign, missing, malformed and not-connected) and nothing is sent. The old signature is gone, so no unchecked path remains. A refusal is logged server-side (`security: scan dispatch refused: workspace=… connector="…" reason=not_connected|foreign_workspace`, the connector ID quoted and truncated, no scan targets); the client message is unchanged (review follow-up).
- `internal/discovery/store.go`: `GetScanResults(tenantID, requestID)` joins `connectors` on `c.tenant_id = $2`. The row's connector is the authenticated stream that reported it and a connector's workspace never changes, so the join is the ownership check; foreign and unknown request IDs both return an empty list. Writes are unchanged: `ON CONFLICT … DO NOTHING` never reassigns a row.
- `graph/resolvers/discovery.resolvers.go`: both resolvers pass the caller's workspace.
- No migration.

**Tests:** `internal/connector/scan_dispatch_test.go` (registry boundary with fake streams: own connector receives the command; foreign connected, missing and malformed get the same error and nothing is sent; B still reaches its own) and `graph/resolvers/scan_authorization_test.go` (3, DB-backed: same client answer for every unavailable connector; results scoped per workspace with positive controls, including a request ID reported by both; a late foreign report can't take over or read a result). All failed before the fix. Full controller suite: 1416 pass / 0 fail / 2 skip (unconditional stubs).

**Remaining (not fixed here):**
- A result's primary key is `(request_id, ip, port, protocol)` without the connector, so a connector that knew another workspace's request ID could insert a row first and hide that one result from its owner (the read filter hides the foreign row, `DO NOTHING` drops the owner's). Request IDs are random and only sent to the owning connector. Closing it needs the connector in the key (a migration; see FU-2 style schema work).
- Within one workspace, results aren't bound to the connector the command went to: any of the workspace's connectors can add rows under its request IDs.
- Scan targets are limited only connector-side (no loopback/multicast); an admin can scan anything their own connector can reach. Product decision, not a tenant boundary.

### Fix: FU-4 `promoteDiscoveredService` could never create a resource
**Issue:** promotion called `resource.Create` without a remote network, so even on the caller's own shield it failed ("remote network not found").

**Fix:**
- `discovery.ShieldRemoteNetwork` is a workspace-scoped lookup; a foreign, missing or malformed shield gives `ErrShieldNotFound`.
- The resolver creates the resource in the discovering shield's network, then calls `NotifyPolicyChange`, like `createResource`.
- Files: `controller/internal/discovery/store.go` and `controller/graph/resolvers/discovery.resolvers.go`.

**Tests:**
- `TestDiscoveryIsolation_PromoteOwnShieldCreatesResource` replaces the test that pinned the failure. It checks host, ports, network and workspace, a resource count of +1, A notified and B not, and that an undiscovered port creates nothing.
- Mutants killed: network not set; no notify.

### Fix: SCIM resolver tests leaked one database per test
**Issue:** `newScimEnableHarness` (`graph/resolvers/idp_update_scim_config_enable_test.go`) dropped its throwaway database over a connection *to that database*. Postgres refuses this, and the error was discarded, so every run leaked 6 `resolvers_updsc_*` databases. 315 had piled up in the dev Postgres.

**Fix:**
- The drop runs through the admin DSN with `WITH (FORCE)`, from a `t.Cleanup` registered right after `CREATE DATABASE`, so a failed setup doesn't leak either.
- `teardown()` now only closes the pool and the fixture server.
- The 321 leaked databases were dropped. After a full suite run only `postgres` and `ztna_platform` remain.

