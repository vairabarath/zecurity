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
| Drain storage | `043_relay_drain.sql`: `relays.drain_started_at TIMESTAMPTZ NULL` (a column, not a status) and `relays.connection_count_at TIMESTAMPTZ NULL` (when the stored `connection_count` was measured). | Heartbeats already move `status` between active/inactive; drain is orthogonal. The stored count can be ~5 min old (DB-write throttle), so "Drained" needs its age (design review 2026-10-29, P4). |
| Drain list rule | **One shared eligibility predicate** (`status='active'`, label high/medium, `drain_started_at IS NULL`, `NULLIF(public_addr,'')` set or public scope with `observed_ip`) used by `BuildLabelledRelayList`, the guard and the read API. | The content-hash version changes on its own; connectors migrate. The guard can't count a relay that is never published (P1). |
| Last-eligible guard | In one transaction: lock the target and the currently eligible relay rows in one `ORDER BY id FOR UPDATE` statement; if the target is eligible and is the only one, 409 `last_eligible_relay`. A non-eligible target (inactive, low tier) may be drained. Revoke/delete are **not** guarded (security actions); the console only warns. | Two concurrent drains can't empty the list; fixed lock order (P2, P3). |
| Drain states | Drain allowed on `active` / `inactive`; `pending` → 409 `invalid_state`; `revoked` / `deleted` → 409 `already_revoked` / `already_deleted`. Repeat drain and resume on a non-draining relay → 204 no-op, not audited, no broadcast. | Mirrors the operator-route no-op rule. |
| Drain progress | Server-computed `drain_state`: `migrating` (connectors attached) → `unverified` (count missing or > 90 s old) → `streams_open` (count > 0) → `unverified` (count measured < 120 s after the drain started) → `drained`. Console says "Drained (as of the last heartbeat)". | `attached_connectors` drops when a connector switches, before its old session closes; only a fresh relay stream count after the 120 s window shows the relay is idle (P4). |
| Fresh counts while draining | Valkey flag `relay:heartbeat:draining:<id>` (TTL 10 min) makes every heartbeat of a draining relay a DB write; set after drain, cleared after resume, re-set every 60 s from Postgres (`RunDrainFlagLoop`). Postgres stays authoritative. | Any flag failure only makes counts older (`unverified`), never an early `drained`. Controller only. |
| Last-relay warning | `RelayDetail.in_relay_list` and `eligible_relay_count` (shared predicate); revoke/delete dialogs warn when the target is the only relay in the list. Advisory, never blocks. | The console can't compute eligibility itself (the read API hides `observed_ip`). |

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

- [x] **X1** Tests first: danger-zone visibility (super-admin only; hidden on revoked/deleted), required reason, typed name enables the button, error mapping, refresh after success.
- [x] **X2** `ConfirmDangerDialog` (reason + typed confirmation), reused by D.
- [x] **X3** Relay detail "Danger zone": Revoke and Delete; API-surface pin. *(Delete also stays available on a revoked relay; the provider CORS policy now allows `DELETE`, see the phase file.)*
- [x] **X-gate** *(2026-10-28: console 469 pass, lint and build clean; middleware tests pass. Live AT-X2, AT-X3, AT-X4 and AT-X5 PASS on isolated stacks with the console on its own origin; AT-X4 with a real connector migrating to the other relay (traffic continuity not exercised: no client tunnel).)*

### Phase D — M2: Drain and resume (D-32)

> **Phase file:** [[Sprint22/Member2-Go-Rust/Phase5-Relay-Drain]]. **Depends on A and X merged.**

- [x] **D0** Design review (2026-10-29): P1–P10 decided (shared eligibility predicate, row-lock guard, unguarded revoke with advisory warning, `drain_state` with measured count age, Valkey write flag, no new rebroadcast). See the phase file → Design decisions.
- [x] **D1** Tests first: shared predicate, list exclusion and version change, resume restores, last-eligible guard incl. two concurrent drains, state rules, audit, authz (relay-ops allowed), broadcast and flag hooks once post-commit, heartbeat writes while draining, `drain_state` timing, eligibility fields.
- [x] **D2** `043_relay_drain.sql` `[schema: reset DB]` (`drain_started_at`, `connection_count_at`).
- [x] **D3** `POST /provider/relays/{id}/drain` and `/resume`; `relay.drain` / `relay.resume` actions; audit; post-commit flag and `broadcastRelayList`; heartbeat write flag and expiry-sweep reconcile.
- [x] **D4** Shared eligibility predicate; `RelaySummary.draining_since` / `drain_state`, `RelayDetail.in_relay_list` / `eligible_relay_count` (additive).
- [x] **D5** Console: Draining badge, Drain/Resume on Relay detail, `drain_state` progress, last-relay warning on revoke/delete, Overview attention item; dialog props; API-surface pin.
- [x] **D6** Regression: connector `relay_selector` tests run unchanged (no connector code touched).
- [x] **D-gate** *(Status 2026-10-30: **merged** in PR #156 (merge commit `11d3d3e1`), CI green; live control-plane cases PASS; **AT-D6 client data plane PASS on the lab 2026-10-30** (finding FU-14). Review residuals FU-6 and FU-7 fixed afterwards (see Post-Sprint Fixes). Code gates PASS after the independent review fixes: Go 1520 pass / 0 fail / 2 expected skips, `go vet` clean; console 507 pass, lint and build clean; `cargo test relay_selector` 27 pass. Live AT-D on the lab fleet pending (V3).)*

### Phase V — M1: Acceptance

- [x] **V1** Run AT-T on the lab after T merges. *(2026-10-27, Sathiya: **PASS**, 40/40 API checks plus the console QR/verify pages. Run on an isolated local stack (throwaway DB, own Valkey, controller :18080) rather than the lab fleet: AT-T needs only the controller and the console. See Acceptance-Test-Plan → Live run — Phase T.)*
- [ ] **V2** Run AT-A, AT-E, AT-X after their merges. *(AT-A: live PASS 2026-10-26 on an isolated local stack, 38/38 checks; see the Acceptance Test Plan. AT-E: live E1…E3 PASS 2026-10-27 on an isolated local stack; E4 **PASS on the lab 2026-10-30** (manojinkyank); E5: delete + add again PASS live, expired-token hint still needs a 24 h token. AT-X: PASS 2026-10-28, see the Acceptance Test Plan.)*
- [x] **V3** Run AT-D on the lab fleet (≥ 2 relays) after D merges. *(2026-10-30: AT-D6 client data plane **PASS** with a real client, connector on manojinkyank and two relays; connector migrated in 17 ms, new connections on the other relay after the client's 60 s sync, the running download completed; FU-14 filed. Earlier:)* *(2026-10-29, M2: live control-plane run on an isolated local stack (throwaway DB, isolated Valkey, two real relays, a real connector, console in the embedded browser; torn down after): AT-D1–D5, D7, D9, D11 PASS; **AT-D6 PARTIAL**: drain progression to "drained" verified; client data plane not performed. Remaining on the lab: (1) a running download completes or is cut at ~120 s, (2) a new client connection uses the remaining relay. See Acceptance-Test-Plan → Live run — Phase D.)*
- [ ] **V4** Final sprint run of [[Sprint22/Acceptance-Test-Plan]]; failures filed as Post-Phase Fixes; Session Log entry. *(2026-10-30: targeted final run **PASS**, cases 1–7 plus the AT-E5 hint; see Acceptance Test Plan → V4. Two observations, no failures. Only the AT-E5 expired-token refusal remains, on 2026-10-31.)*

### Remaining Sprint 22 checks (updated 2026-10-30)
- [x] **Drain console text vs FU-14.** Decided 2026-10-30: correct it now. The card, the drain dialog and the `streams_open` line no longer promise a 120 s cut; they say open connections "keep running until they end". The `unverified` line's 120 s measurement window is the server's and stays. Branch `sprint22/drain-text-fu14`.
- [x] **V4 final run: 2026-10-30.** PASS: cases 1–7 and the AT-E5 hint (Acceptance Test Plan → V4).
- [ ] **AT-E5 expired-token refusal: 2026-10-31, after 16:34:25 IST.** The hint already PASSES (V4 case 8). Remaining check: start the kept V4 controller (DB `zecurity_live_v4_*`; env, token and TOTP helper in `~/.cache/zecurity-v4-at-e5`) with the isolated Valkey. Run `zecurity-relay` with `v4-target`'s token: enrolment must be refused. Then delete `v4-target`, drop the V4 DB and remove the folder. Then tick V2, V4 and "All scenarios pass".

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

- [x] Super-admins **cannot act** without TOTP; enrolment, verification, replay rejection, rate limit and recovery work and are audited. *(AT-T PASS 2026-10-27.)*
- [x] Relay revoke/delete are **super-admin only** server-side; relay-ops can create, drain and resume. *(AT-A PASS 2026-10-26; drain/resume by relay-ops in `TestDrainDB_RelayOpsMayDrainAndResume` and live AT-D.)*
- [x] Revoke/delete require a **reason and typed name**, server-checked; errors are JSON codes; repeat actions get clear 409s. *(AT-A PASS 2026-10-26.)*
- [x] A relay can be **enrolled from the console** with a one-time token and a complete install command. *(AT-E1…E3 PASS; AT-E4 PASS on the lab 2026-10-30.)*
- [x] A relay can be **drained and resumed**; connectors migrate with no connector change; the last eligible relay can't be drained. *(AT-D PASS live incl. AT-D6 client data plane 2026-10-30; FU-14: running streams aren't cut at 120 s.)*
- [x] The console's writes are exactly: auth calls, operator management, relay create/revoke/delete/drain/resume (API-surface test). *(`api-surface.test.ts` pins them; console 507 pass at the D merge.)*
- [x] No connector, relay, proto or `admin/` changes; `workspaces.status='active'` predicates untouched. *(Checked 2026-10-30: no Sprint 22 phase commit touches `connector/`, `relay/`, `proto/` or `admin/`, and none removes a workspace status predicate. Other merged work in the same window (Sprint 20 fixes, device approval) did touch them; it is not Sprint 22 scope.)*
- [ ] All scenarios in [[Sprint22/Acceptance-Test-Plan]] pass.

## Follow-ups (tracked)

| # | Item | Source | Status |
|---|------|--------|--------|
| FU-1 | **Create audit atomicity (D-14).** `POST /provider/relays` writes its `relay.create` audit best-effort, outside any transaction, after the relay row, the Valkey JTI and the JTI attach. A failed audit write is only logged, and a server error part-way through can leave a pending relay without a usable token. Revoke and delete are transactionally audited (Phase A). Fix: relay row + audit in one transaction, then token, Valkey JTI and attach, with clean-up on failure. | Phase A decision, 2026-10-26 | **Fixed** 2026-10-28 (branch `fix/m1-tgate-fu4`, see Post-Sprint Fixes → FU-1) |
| FU-2 | **Workspace consistency in the schema (B-1 defense in depth).** `access_rules` has single-column foreign keys, so the database accepts a rule whose workspace, resource and group belong to different workspaces; `group_members` has no workspace column at all. Add `UNIQUE (id, tenant_id)` on `resources` and `remote_networks`, `UNIQUE (id, workspace_id)` on `groups`, composite foreign keys from `access_rules` and `resources (remote_network_id, tenant_id)` (the resource-network fix showed the application can miss such a reference); and put the reporting connector into `connector_scan_results`' key (or record which connector each scan request was sent to), so a report can never suppress or stand in for another connector's result (FU-3 residual); decide how to constrain `group_members` (column + composite FK, or trigger). Needs a migration number (043 is reserved for Phase D). | B-1 fix, 2026-10-27 | **Fixed** 2026-10-28 (branch `fix/fu2-schema-workspace-constraints`, migration `045_`, see Post-Sprint Fixes → FU-2) |
| FU-3 | **`triggerScan` / `getScanResults` are not workspace-scoped (security, likely High).** `triggerScan(connectorId, …)` pushes a scan command to any connected connector by ID (`ConnectorRegistry.PushScanCommand`), the connector scans any non-loopback, non-multicast target (up to 512 targets × 16 ports), and `getScanResults(requestId)` reads results by request ID alone, which `triggerScan` returns. A workspace admin who knows another workspace's connector ID (sent to that workspace's devices in ACL snapshots) could likely scan that workspace's internal network through its own connector. Found by code reading during the resource-network review; not reproduced (needs a connected connector). | Resource-network review, 2026-10-27 | **Fixed** (branch `security/fu3-scan-authorization`, see Post-Sprint Fixes → FU-3); result squatting moved to FU-2 |
| FU-4 | **`promoteDiscoveredService` can't create a resource:** it never sets `remote_network_id` (the shield's network would be the natural value). Feature repair, not security. | Resource-network review, 2026-10-27 | **Fixed** 2026-10-28 (branch `fix/m1-tgate-fu4`, see Post-Sprint Fixes → FU-4) |
| FU-5 | **The rest of FU-2's class.** A catalogue audit found 28 more single-column foreign keys between two workspace-scoped tables: devices, posture reports and evaluations, profiles and bindings, shields, connectors, `resources.shield_id`, SCIM sync instances, user references. Also: scan requests weren't bound to a connector, so another connector of the same workspace could add rows; and scan results had no key to their connector. | FU-2 follow-up, 2026-10-28 | **Fixed** 2026-10-28 (branch `fix/fu2-schema-workspace-constraints`, migration `046_`, see Post-Sprint Fixes → FU-5); the 5 keys to `identity_connections` are excluded on purpose |
| FU-6 | **Relay list reconciliation.** A failed post-commit `broadcastRelayList` (drain, revoke, expiry) leaves connected connectors on the old list until the next broadcast or reconnect. Phase D only logs it (P8). Consider a periodic re-broadcast or a list-version check in the connector heartbeat response. | Phase D design review, 2026-10-29 | **Fixed** 2026-10-30 (branch `sprint22/fu6-fu7-relay-drain-residuals`, see Post-Sprint Fixes → FU-6) |
| FU-7 | **Phase D review residuals (low).** Drain vs `EvictRelays` deadlock (retryable 500); `RecordHeartbeat` newer-count guard; one clock for the 120 s window; `GetActiveRelay` ignoring drain; NULL-safe `eligible.Where`; concurrency tests for drain vs revoke/eviction/resume. See the Phase D file → Post-Phase Fixes. | Phase D review, 2026-10-29 | **Fixed** 2026-10-30 (same branch, see Post-Sprint Fixes → FU-7) |
| FU-8 | **Clients learn regained access only on their 60 s poll (O-3, live E2E run 2026-10-29).** Losing access is pushed (connectors tear sessions down at once), but a client only re-adds a resource on its background ACL sync, so a just-approved device or re-added member sees up to a minute of refusals (`zecurity-client sync` is immediate). Needs a design decision (push a hint to clients, or a shorter poll); left as is by the user's decision. | Live E2E run, 2026-10-29 | Open |
| FU-9 | **A steadily slow reader loses its tunnel (F-2, live E2E run 2026-10-29).** A download read at 25 KB/s through the tunnel is ended by the connector after 2–10 s (`device stalled with its tunnel queue full`, 3/3 runs); 50 KB/s and faster, and a 5 s pause at 500 KB/s, survive. Cause: Sprint 20 Fix 14 `DEVICE_STALL_LIMIT = 2 s` (accepted limit, F48) — all tunnels of a shield share one Control stream. Fix is the recorded per-tunnel flow control (proto change). | Live E2E run, 2026-10-29 | Open |
| FU-10 | **The tunnel isn't restored after a client daemon restart or reboot (O-5).** The daemon loads its state but only `login` / `up` bring the tunnel up, so a user has no access after a reboot until `zecurity-client up`. `status` now says `Tunnel: down` (fixed); restoring the previous up state at start needs a decision. | Live E2E run, 2026-10-29 | Open |
| FU-11 | **Anyone can look up which workspaces an email belongs to (F-6, live E2E run 2026-10-30).** Public `lookupWorkspacesByEmail` (login page) returns the workspaces of any email without sign-in and without a limit (85 lookups in a burst, all answered). ADR-009 (proposed) plans 30/min per IP for it "to prevent enumeration" (Tier 3, pre-GA). Options: that limit, and/or email the list instead of returning it. | Live E2E run, 2026-10-30 | **Mitigated** 2026-10-31 (branch `fu11/discovery-rate-limit`): shared per-IP budget on all four discovery entry points + per-email budget; the oracle remains (email-verified discovery to decide before GA). See Post-Sprint Fixes → FU-11 |
| FU-12 | **A sustained download through a relay is torn down (F-7, live E2E run 2026-10-30).** A 20 MB download at 400 KB/s through a relay was cut ~35–45 s in (19.9 of 20 MB) in 3/3 runs, drain or not, with every session on that client↔relay path ended at once (connector `session unregistered` ×4, relay `bridge ended with error … pipe Connector to Client`); the same download over the direct path completes. Cause not yet identified: the relay logs only the outer error context. Next: log the full error chain (`{:#}`) in `relay/src/session.rs` and rerun. Separately, on both paths new connections to the same shield time out (5 s) while a download runs — the shared shield stream (FU-9). | Live E2E run, 2026-10-30 | **Fixed** 2026-10-30 (branch `fix/e2e-j7-findings`): the Connector now shuts a relayed shield-tunnel stream down on a clean end (TLS close_notify); see Post-Sprint Fixes → F-7. The probe timeouts are FU-9 |
| FU-13 | **The outbox processor is started twice** (`cmd/server/main.go`, workers `outbox-processor-1` and `-2`, same instance; the second came with Sprint 17 `2d14f333`). Not the shutdown hang (O-8); check whether concurrent `Run` on one processor is intended. | Live E2E run, 2026-10-30 | **Fixed** 2026-10-30 (branch `fix/e2e-j7-findings`): accidental duplicate from Sprint 17's merge fix; second start removed |
| FU-14 | **The connector's relay drain timeout doesn't cut streams.** *(Numbered FU-11 in PR #158; renumbered when #159's FU-11…FU-13 merged.)* After `relay_drain_timeout_secs` (120 s) `relay_selector` only aborts the old session task; each stream is a detached `tokio::spawn` (`connector/src/relay_handler.rs:167`), so running streams keep the old relay's QUIC connection open until they end. Found live in AT-D6: a 1 GiB download kept using the drained relay for 442 s. Fix: close the old QUIC connection (or abort tracked stream tasks) when the timeout fires, with a test; then the console's "up to 120 seconds" text is true. Connector change, so not in Sprint 22 (D-32). | AT-D6 lab run, 2026-10-30 | **Fixed** 2026-10-31 (branch `fix/connector-drain-timeout-cut`): the drain timeout closes the old relay connection. See Post-Sprint Fixes → FU-14 |

## Post-Sprint Fixes

### Fix: FU-14 the connector's drain timeout cuts the streams still open
**Issue:** live AT-D6 (2026-10-30): after a drain, a 1 GiB download kept using the drained relay for 442 s. The connector logged "old relay drain timed out with active streams still open" at 120 s and cut nothing.

**Root Cause:** each relay stream runs as a detached `tokio::spawn` task (`relay_handler.rs`). On timeout `relay_selector` only called `old.handle.abort()`, which ends the session's accept loop. The stream tasks kept the old QUIC connection alive.

**Fix Applied (`connector/src/relay_handler.rs`, `relay_selector.rs`):**
- **Tracker:** `RelayDrainTracker` keeps the relay connection its streams run on. `RelayHandler::run` attaches it.
- **Cut:** `cut()` closes that connection with application code `DRNT` (0x44524E54), "relay drain timeout".
- **Timeout path:** `migrate` now calls the new `drain_old_relay`. It waits up to `drain_timeout` for the streams to end, then cuts.
- **Unchanged:** a drain that completes in time closes nothing early.

```rust
// AFTER (relay_selector.rs):
async fn drain_old_relay(tracker: &RelayDrainTracker, drain_timeout: Duration, relay_id: &str) -> bool {
    if tokio::time::timeout(drain_timeout, tracker.wait_for_idle()).await.is_ok() { return true; }
    emit_event(SelectorEvent::DrainTimeoutFired);
    tracker.cut(); // closes the old relay connection: every stream on it ends now
    false
}
```
- **Effect:** with the relay fix (FU-15 relay path) the client gets a reset for streams cut this way; without it, the relay ends them with its own abort code.

**Tests:**
- `cut_closes_the_attached_connection_and_its_streams`;
- `cut_without_a_connection_is_a_no_op`;
- `drain_timeout_cuts_the_streams_still_open`;
- `a_completed_drain_does_not_cut`;
- `the_drain_tracker_can_cut_the_relay_connection_the_handler_serves` (real `RelayHandler`).

Connector 277 pass; 3 mutants killed (no cut at timeout, handler doesn't attach, cut doesn't close). Not live-tested.

**Console text (follow-up decision):** with FU-14 deployed, open connections on a draining relay end at most `relay_drain_timeout_secs` (120 s) after their connector moves. The console text changed in #160 ("keep running until they end") can name the limit again once connectors run this fix. Older connectors still don't cut.

### Fix: FU-11 public workspace discovery is rate limited (mitigation, not elimination)
**Issue:** `lookupWorkspacesByEmail` (and the slug lookups) answered any number of unauthenticated calls: 85 in a burst during the live E2E run (F-6). That makes it an enumeration oracle.

**Root Cause:** the per-operation limits planned in ADR-009 (Tier 3) were never built, and one request can repeat a field under aliases.

**Fix Applied:** new package `controller/internal/discoverylimit`, plus one `Charge` call per resolver.
- **Shared budget:** one Valkey fixed-window budget per client IP (IPv6 /64), 30/60 s, shared by `lookupWorkspacesByEmail`, `lookupWorkspace`, `lookupIdpConnections` and `GET /workspaces/{slug}/auth`.
- **Email budget:** 5/hour per canonical email, keyed by HMAC (HKDF from `PKI_MASTER_SECRET`).
- **Charged per resolver run,** so aliases, fragments, the authenticated route and every transport count.
- **Whole-response refusal:** a gqlgen response interceptor blanks the whole operation when any field is refused.
- **Status codes:** 429 + `Retry-After` / 503 fail closed.
- **Optional trusted proxies:** `DISCOVERY_TRUSTED_PROXY_CIDRS`.
- **Metric:** `discovery_requests_total{operation,result}`.

```go
// AFTER (each discovery resolver, before any read):
if err := discoverylimit.Charge(ctx, discoverylimit.OpEmailLookup, email); err != nil {
	return nil, err
}
```
Details: ADR-009 → Workspace-discovery enumeration (FU-11); `docs/workspace-discovery-rate-limit.md`. Tests: `internal/discoverylimit/*_test.go`, `graph/resolvers/discovery_ratelimit_test.go`; 11 mutants killed.

### Fix: FU-6 a missed relay list reaches the connector within a minute
**Issue:** a failed post-commit `broadcastRelayList` (drain, revoke, eviction), a full connector send queue, or two broadcasts delivered out of order left a connected connector on an old relay list until the next pool change or a reconnect. After a drain, that connector kept using the draining relay.

**Root Cause:** broadcasts were fire-and-forget; nothing recorded which list each connector had been sent.

**Fix Applied (`internal/connector/control_stream.go`, `cmd/server/main.go`):** each Control stream records the version of the last relay list enqueued on it (`sendRelayList`, under a per-stream lock so the recorded version is the one the connector receives last). `ConnectorRegistry.RunRelayListSync` builds the list every 60 s and sends it only to streams whose recorded version differs (`SyncRelayList`). A blind periodic re-broadcast was rejected: the connector re-probes all relays on every push (`relay_selector::handle_list_change`), so streams already current get nothing. No connector, relay or proto change.

**Tests:** `TestSyncRelayList_SendsOnlyToStreamsBehind`, `…_CorrectsOutOfOrderBroadcast`, `…_FailedSendRetriedNextSync`, `TestRunRelayListSync_DeliversMissedList`. Mutation-checked: always sending, or not recording the version, fails them.

### Fix: FU-7 Phase D review residuals
Details in the Phase D file → Post-Phase Fixes. In short:
- `EvictRelays` locks its rows in id order (as `DrainRelay` does): the drain/eviction deadlock (reproduced by the new race test against the old statement) is gone.
- `RecordHeartbeat` keeps a newer `connection_count` when an older heartbeat is written later.
- `drain_started_at` is the controller's clock, read after the row locks are held, like `connection_count_at`.
- `policy.GetActiveRelay` skips draining relays.
- `eligible.Where` is `COALESCE(…, false)`: never NULL.
- Race tests: drain vs eviction, revoke and resume.


### Fix: Phase D review — post-commit hooks detached from the request (Medium)
Drain/resume and revoke/delete fan-out hooks ran on the request context; a disconnecting client could cancel the relay-list broadcast of a committed change. Now `fanoutContext(r)` (`context.WithoutCancel` + 10 s). Also: broadcast failures logged with the relay id (`relay.DrainFanout`), and an inactive draining relay gets its own console text. Details and accepted follow-ups (FU-7): Phase D file → Post-Phase Fixes.

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

**Not done here:** schema constraints (FU-2); lab database check for existing mixed rows (queries in the Session Log entry, for M1). *Both done 2026-10-28, see Post-Sprint Fixes → FU-2.*

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

### Fix: FU-1 relay create is transactionally audited (D-14)
**Issue:** `POST /provider/relays` created the relay row, issued the token, stored the Valkey JTI and attached it, then wrote `relay.create` best-effort outside any transaction. A failed audit was only logged, and a failure part-way could leave a pending relay without a usable token.

**Fix (`internal/relay/store.go`, `internal/relay/admin_handler.go`):**
- `Store.CreateRelayAudited` does the following in one transaction:
  1. inserts the relay row;
  2. calls the handler's `issue`, which mints the token and writes the Valkey JTI;
  3. attaches the JTI;
  4. runs the audit `InsertAuditTx`;
  5. commits.
- Any failure rolls back the row, the JTI and the audit together. The handler then deletes the Valkey JTI it wrote, and the response is a plain 500 `server_error`.
- A committed relay therefore always has a live token.
- The one remaining edge: a crash between the Valkey write and the commit leaves a JTI for a relay that doesn't exist. Provisioning refuses it, and it expires with the token (24 h).
- `AttachJTI` was removed, because nothing calls it any more.

**Tests (DB + isolated Valkey):**
- `TestAdminDB_CreateAuditFailureRollsBack`: no relay row and no leftover JTI.
- `TestAdminDB_CreateValkeyFailureLeavesNoRelay`: no row and no audit.
- `TestAdminDB_CreateByRelayOps` now also checks that the committed relay's JTI is live in Valkey and names it.
- Mutants killed (4/4): audit error ignored; audit after commit; no JTI clean-up; Valkey error ignored.

### Fix: warn when `CONNECTOR_CERT_TTL` is not longer than `CONNECTOR_RENEWAL_WINDOW`
**Seen in the Sprint 20 G4 live run.**
- With such settings every new connector certificate starts inside the renewal window.
- The controller re-asks on each new Control stream (the connector ignores it as a duplicate), and connectors renew on every 10-min ReEnroll resend instead of near expiry.

**Fix:**
- The controller logs a startup warning: `connectorRenewalWarning` in `cmd/server/renewal_config.go`, with a unit test.
- Behaviour is unchanged. Clamping the window, as the shield does with `EffectiveRenewalWindow`, would be a design change and was not made.

### Fix: FU-2 the schema refuses cross-workspace rows (B-1 defense in depth) `[schema: reset DB]`
**Issue:** the B-1 and resource-network fixes enforce workspace consistency in the application and the ACL compiler only. The schema used single-column foreign keys, so it accepted:
- a resource on another workspace's remote network;
- an access rule whose resource or group belongs to another workspace;
- a group member from another workspace (`group_members` had no workspace column).

Also, a scan result's key `(request_id, ip, port, protocol)` didn't include the connector. A foreign connector that knew a request ID could report first and hide the owner's row (FU-3 residual).

**Fix (`controller/migrations/045_workspace_consistency.sql`):**
- Adds `UNIQUE (id, tenant_id)` on `remote_networks`, `resources` and `users`, and `UNIQUE (id, workspace_id)` on `groups`, as targets for the composite keys.
- `resources (remote_network_id, tenant_id)` → `remote_networks`.
- `access_rules (resource_id, workspace_id)` → `resources`, and `(group_id, workspace_id)` → `groups`.
- `group_members` gets `workspace_id NOT NULL`, with keys to `groups (id, workspace_id)` and `users (id, tenant_id)`.
  - A `BEFORE INSERT OR UPDATE` trigger always copies the workspace from the group, whatever the writer sent, so the key to `users` is the real check.
  - The Go writers (`policy/store.go`, the two SCIM inserts) are unchanged. This was M1's choice of the options FU-2 left open; the alternatives were writers passing the column, or a check-only trigger.
- Every replaced key keeps `ON DELETE CASCADE`.
- `connector_scan_results` primary key is now `(request_id, connector_id, ip, port, protocol)`. `discovery.UpsertScanResults` `ON CONFLICT` follows.
- The SCIM member lookups are already scoped to the workspace (`u.tenant_id`), so no current path starts failing.

**Tests:**
- New `internal/policy/workspace_consistency_test.go`:
  - consistent rows are accepted, and the trigger fills the member's workspace;
  - 7 cross-workspace writes fail with SQLSTATE 23503 on the named constraint: insert and update of a resource's network; a rule's foreign resource; a rule's foreign group; a foreign member; a writer that sends the user's workspace; moving a member into a foreign group;
  - deletes still cascade.
- The planted-row tests keep testing the application and compiler defence. Each drops the new constraints on its own throwaway database first (`allowCrossWorkspaceRows`):
  - `TestCompileACLSnapshot_TenantIsolation` and `TestCompileACLSnapshot_ResourceOnForeignNetwork`;
  - `TestPolicyIsolation_ConflictCannotTouchForeignRow` and `TestPolicyIsolation_ReadsIgnorePlantedForeignRows`;
  - `TestResourceNetwork_ReadsHideForeignNetworkName`.
- `TestScanIsolation_LateReportCannotTakeOverResult` became `TestScanIsolation_ForeignReportNeitherHidesNorTakesOverResult`. With the foreign report either before or after the owner's, there is one row per connector and each workspace reads only its own connector's row.
- **Mutants killed (7/7):** each composite key reverted to single-column (4); the trigger trusting the writer's value; the trigger on INSERT only; the scan key without the connector.
- **Gate:**
  - full controller suite in a CI-style environment (empty admin DB, isolated Valkey): 1485 pass / 0 fail / 2 skip (unconditional stubs);
  - fresh-volume init of all 47 SQL files: 0 errors;
  - the controller starts on it.

**Lab check (M1, open item from B-1):** the dev database `ztna_platform` had no mixed rows before the reset: 51 resources, 51 rules and 1 membership checked across all four relationships.

**Left for FU-5 (fixed in `046_`, next entry):** the same single-column pattern on `connectors` / `shields` and elsewhere, and the per-request connector binding within a workspace.

### Fix: FU-5 every workspace-scoped foreign key carries the workspace; scan requests bound to their connector `[schema: reset DB]`
**Issue:**
- **The audit:** after FU-2, a catalogue query listed every single-column foreign key from one `tenant_id`/`workspace_id` table to another. 28 remained.
- **Scan requests:** they weren't bound to a connector, so another connector of the same workspace could add rows to a request.
- **Scan results:** they had no key to their connector, so they outlived it until the 24 h purge.

**Evidence first:** the dev DB had 0 mismatches on all 32 keys checked (FU-2's plus these 28). The only cross-workspace link was 1 `external_identities` row pointing at a platform connection.

**Fix (`controller/migrations/046_workspace_consistency_all.sql`):**
- **23 keys made composite** with the workspace:
  - devices → users (owner and approver);
  - posture reports and evaluations → devices, profiles and reports;
  - profile bindings → resources and profiles;
  - `resources.shield_id` → shields;
  - shields → connectors and networks;
  - connectors → networks;
  - users, groups and external identities → SCIM sync instances;
  - invitations, outbox events, workspace members and permissions → users.
- **Generated from the catalogue**, and each key keeps its ON DELETE action. SET NULL keys use `ON DELETE SET NULL (col)`, so a delete clears only the reference, never the row's workspace.
- **Targets:** `UNIQUE (id, workspace)` on `client_devices`, `connectors`, `device_posture_reports`, `device_profiles`, `scim_sync_instances` and `shields`.
- **Excluded on purpose:** the 5 keys to `identity_connections`.
  - A connection with `tenant_id NULL` is a shared platform connection (ADR-024) that every workspace's external identities use. A composite key can't express "NULL or the same workspace".
  - A trigger could, but that would be a new ADR-024 rule, so it was not added.
- **`scan_requests`** (request, connector, tenant, created_at) has a key to `connectors (id, tenant_id)` ON DELETE CASCADE. `connector_scan_results` gets a key `(request_id, connector_id)` → `scan_requests` ON DELETE CASCADE.

**Code:**
- **`TriggerScan`** records the request with `discovery.RecordScanRequest` before sending it. The insert is scoped to the workspace; a malformed ID is simply not found.
  - A connector outside the workspace gets the same `connector not found or not connected` answer as before, and the refusal is logged.
  - If the send fails, the request is deleted.
- **`UpsertScanResults`** stores a row only if its request was sent to that connector (`INSERT … SELECT … WHERE EXISTS`). Anything else is dropped; the report handler only logs errors, so the stream isn't affected.
- **`PurgeScanResults`** also deletes old requests.
- **Side effect:** each request now has one connector, so the `ScanModal` duplicate-key case can't happen.
- **#2 `resources.shield_id` was not a live bug:** no path takes a shield ID from the caller. `Create` uses the workspace-scoped `AutoMatchShield`, and `MarkProtecting` joins `s.tenant_id = r.tenant_id`. The key is defence in depth.

**Tests:**
- **`TestWorkspaceConsistency_EveryCrossTableKeyCarriesTheWorkspace`:** a catalogue check that also fails if a future migration adds such a key.
- **`TestWorkspaceConsistency_DevicesShieldsAndProfiles`:**
  - 12 refusals, each by its named constraint;
  - SET NULL keeps the row's workspace (approver, report, shield);
  - a deleted profile takes its bindings and evaluations with it.
- **Scan:**
  - `TestScanIsolation_UnsolicitedReportIsDropped`: another workspace's connector and a second connector of the same workspace, before and after the owner;
  - `TestScanIsolation_RecordAndPurgeRequests`: own, foreign, missing and malformed connectors; purge; a connector delete cascades;
  - the triggerScan refusal test also checks that no request is left behind.
- **Planted-row tests:** two more get the drop-first treatment. `TestCompileACLSnapshot_TenantIsolation` (a resource on a foreign shield) goes through the extended `allowCrossWorkspaceRows`, and `TestReportDevicePostureIntegration` (a device in B owned by A's user) drops its one constraint inline.
- **Mutants killed (10/10):**
  - key reverted to single-column;
  - SET NULL without the column list (×3);
  - upsert without the request check;
  - record ignoring the tenant;
  - unsent request not deleted;
  - purge keeping requests;
  - results → requests key dropped (killed by the connector-delete test added for it).
- **Gate:**
  - controller 1500 pass / 0 fail / 2 skip (unconditional stubs);
  - fresh-volume init of all 48 SQL files: 0 errors, 28 `*_same_workspace_fkey`;
  - the controller starts.

### Fix: live E2E run findings — firewall posture on iptables-legacy (F-1), token no-store (O-1), admin audit (O-2), login exit code (O-4)
Found by the live end-to-end run of `fixed-pendings` `35cb31c5` (2026-10-29, M1; record:
`Sprint22/Live-E2E-Run-2026-10-29.md`). Branch `fix/e2e-live-findings`.

- **F-1** `client/src/posture.rs`: the firewall check failed with ufw active on an iptables-legacy host. Fixed with
  an iptables-legacy-only ufw config fallback (user decision). Details in
  `Sprint15/Member2-Rust-Client/Phase1-Proto-and-Linux-Collectors.md` → Post-Phase Fixes.
- **O-1** `internal/connector/token_handler.go`, `internal/shield/token_handler.go`: the token responses carry a
  single-use enrollment JWT but had no `Cache-Control`. Now `no-store` (as the relay provisioning response).
- **O-2** workspace admin changes wrote no audit row (only device, IdP, SCIM and force-delete did). New
  `graph/resolvers/audit_helpers.go` `auditAdmin` (best-effort `audit.Record`, after the change commits and before
  its notifications) in every unaudited admin mutation: `remote_network.create/delete`,
  `connector.create/revoke/delete`, `shield.create/revoke/delete`, `resource.create/update/protect/unprotect/delete`,
  `group.create/update/delete`, `group.member.add/remove`, `resource.group.assign/unassign`,
  `device_profile.create/mode/manual_trust/delete/requirement.add/requirement.remove/bind/unbind`,
  `scan.trigger`, `invitation.create`, `device.revoke`; plus `connector.token.issue` / `shield.token.issue` from the
  REST token endpoints. No row carries a token. `TestEveryMutationIsAudited` (go/ast) fails for any new mutation that
  neither audits nor is listed with a reason (`initiateAuth`, `testIdpConnection`, device approval and SCIM conflict
  actions are audited elsewhere or change nothing).
- **O-4** `client/src/cmd/login.rs`: after a successful login the daemon's `PostLoginState` has already brought the
  tunnel up; `login` then sent a second `Up`, got `already up` and exited non-zero after a good login. The redundant
  `Up` is gone.

**Tests:** client 293 pass (F-1: 8 new tests, 4/4 mutants); controller 1504 / 0 / 2 (skips unchanged), incl.
`TestAdminAudit_ChangesRecordedWithActor` (6 changes audited with actor and target, refused ones not; 2/2 mutants:
dropped call, audit before the change), `TestEveryMutationIsAudited` (catches a removed call),
`TestShieldTokenEndpoint_NoStoreAndAudited`, `TestEnrollBinding_TokenIssueAuditedWithoutToken`, and every connector
token reissue now asserts `no-store`.

**Live (2026-10-29):** F-1 PASS/FAIL both ways (see the phase file); `POST /api/connectors/{id}/token` →
`Cache-Control: no-store`; `connector.create` / `connector.token.issue` / `connector.delete` /
`device_profile.requirement.add` rows with actor and details, no token; logout + login → `Zecurity is up.`, exit 0.

### Fix: live E2E run — connector restart outage (F-3) and `status` tunnel line (O-5)
Found in journey 3 of the live E2E run (2026-10-29, record `Sprint22/Live-E2E-Run-2026-10-29.md`). Branch
`fix/e2e-live-findings`.

**Issue (F-3):** restarting the connector a shield is attached to cut that shield's resources for 32 s, though a
second connector was healthy and the restarted one served again within 2 s.

**Root Cause:** `client/src/net_stack.rs` counted `SHIELD_NOT_ATTACHED` as a denial ("re-polling won't help"), so once
the early resync had applied a map taken while the connector was marked disconnected, the client failed closed with
no further resync until the 60 s tick; `run_transport_recovery` also stopped at the first changed map.

**Fix Applied:**
```rust
// net_stack.rs, Some("SHIELD_NOT_ATTACHED") arm — AFTER:
saw_transport_failure = true; // a shield re-homing is routing, not a denial
// daemon.rs run_transport_recovery — BEFORE: Ok(true) => { apply; break }
// AFTER: recovery_step(changed, applied): Apply (and check again after the backoff) | Retry | Stop once unchanged after a change
```
**O-5:** `status` now prints `Tunnel: up` / `Tunnel: down — … run zecurity-client up` (`IpcResponse.tunnel_up`).

**Tests:** see the run record; client 296 pass. **Live:** restart outage 32 s → 3 s; status line correct after a
daemon restart. F-2 and restoring the tunnel at start → FU-9, FU-10.

### Fix: live E2E run — signing in again after a device revoke (F-4)
Found in journey 4 (2026-10-29). After `revokeDevice` the daemon exits (Sprint 19 D-D, kept); `login` then failed and
every attempt left an active orphan device, because the revoked state on disk made each daemon start exit again and
`login` enrolled before reaching the daemon. `client/src/cmd/login.rs` now clears a revoked device's state
(`state_store::clear_revoked_state`) and checks the daemon before the browser flow. Live: one `login` restores access;
an unreachable daemon stops `login` with a clear message and no device. Details in the Sprint 19 Track 2 file.

### Fix: live E2E run — promoted resource names, repeated promote, unscannable targets (F-5, O-7)
Found in journey 5 (2026-10-29). `promoteDiscoveredService` named an unnamed service `" on <shield id>"` and answered a
repeated promote with the raw unique-violation error; `triggerScan` accepted loopback/multicast/unspecified targets the
connector silently drops. Now `discovery.PromotedResourceName` (`<service or proto/port> on <shield name>`), a user
error "already a resource", and `discovery.CheckScanTarget` before the request is recorded. Tests and live re-check in
the run record (`Sprint22/Live-E2E-Run-2026-10-29.md`).

### Fix: live E2E run journey 7 — relay revoke recovery (F-8) and controller shutdown deadlock (O-8)
Found in journey 7 (2026-10-30, record `Sprint22/Live-E2E-Run-2026-10-29.md`). Branch `fix/e2e-j7-findings`.
- **F-8** after a relay revoke, relay-path clients failed for 35 s and the revoked relay kept accepting them. Client:
  resync cooldown 30 → 5 s, max backoff 16 → 8 s, no sleep after the last attempt (PENDING-03 file). Relay: a revoked
  relay closes every connection (`relay revoked`) and stops (Fix 17 file). Live: 3.5 s outage.
- **O-8** controller stops hung until SIGKILL: the connector Control handler could wait forever after its stream
  ended (random `select`), holding `GracefulStop` and deadlocking `Stop`. Fixed with a `ctx.Done()` case
  (connector-revocation Phase A file); shutdown waits for named workers at most 15 s. Live: 6/6 stops in 10 s.
- **F-7** investigated, not fixed → FU-12; duplicate outbox processor → FU-13.

### Fix: live E2E run — relayed responses lost their end (F-7 / FU-12) and the duplicate outbox processor (FU-13)
- **F-7** through a relay every shield-tunnel flow ended without the inner TLS `close_notify`; the Client reset the
  app connection and the last buffered bytes were lost. `connector/src/agent_tunnel.rs` `relay_stream` now shuts the
  device stream down on a clean Shield close (up to 120 s, cancellable); errors still end abruptly. Live: 20 MB through
  a relay 2/2 complete, 0 close_notify errors in 55 flows. Details in `Sprint20/Fixes/Fix14-Shield-Stream-Reconnect-Sessions.md`.
- **FU-13** `cmd/server/main.go` started `outboxProcessor.Run` twice (Sprint 17's fix for the HTTP server goroutine
  re-added it); the duplicate is removed (claiming was already SKIP LOCKED, so only polling and reaper logs halve).

### Live check FU-2 + FU-5 (2026-10-28, M1): PASS
**Setup:**
- Dev DB recreated from all 48 files (28 `*_same_workspace_fkey`, `scan_requests`).
- Controller `2d7530a6` (sha `d7ea9ffc`) on :8080/:9090.
- Workspace `xiyo` provisioned with `bootstrap.Service.Provision` from a throwaway program (deleted, never committed).
- Connector `fu5-conn` on inkyank .75; client on .164, rebuilt with `client/scripts/dev-build.sh`, because the reset made a new platform root and the client refused the old pin (expected).

**1. Scan:**
- `triggerScan` recorded `scan_requests` bound to `fu5-conn`.
- The connector's report was stored: 192.168.1.75:8811 open, `reachableFrom` = `fu5-conn`; 8812 (closed) absent.
- A foreign connector ID got `connector not found or not connected`, was logged with `connector="…" reason=not_in_workspace`, and left no request behind.

**2. Policy and access:**
- `createGroup`, `addGroupMember` (the trigger filled `workspace_id`), `createResource` and `assignResourceToGroup` all went through the new keys.
- The client listed `fu5-web`, and `curl 192.168.1.75:8811` returned 200 through the tunnel. The connector logged `access allowed` and `tunnel_opened ok` for device `550b6d45`.
- The connector received ACL v5 and v6 within 1 s.

**3. Device and posture:**
- Device enrolment wrote `client_devices` (key to the user).
- An audit profile (`linux.firewall.active`, `zecurity.device.approved`) bound to the resource was evaluated from the device's report: the report is linked and the workspace is correct.
- `approveDevice` stored an approver from the same workspace, audited `device.approve`, and re-evaluated at once.
- Result: not satisfied, `linux.firewall.active: FAIL: nftables present but no default-deny input chain` (this PC; matches #149). Access stayed 200 because the profile is in audit mode.

**Cleanup:** connector revoked and stopped; the web server, token and state removed on inkyank; the token server and controller stopped. The dev DB keeps workspace `xiyo`.

### Observed: `TestProcessorRunReapsAbandonedIntegration` (outbox) flaky under full-suite load
- 2026-10-28: it failed once in 3 full controller runs ("event was not reaped before timeout", 4 s deadline).
- It passes alone (3/3, about 1.6 s) and in the other 2 full runs. It's unrelated to the changes above.
- **Investigated 2026-10-28, test-only fix:**
  - Not reproduced in 55 runs: 30 alone, 25 under concurrent DB-heavy package runs.
  - **A transient window is confirmed, though.** The reaper marks the event `failed`, retry 1, with the next attempt about 2 s later. The test's processor then re-claims it and, with no handler registered, makes it terminal (`failed`, retry 100).
  - With a 5 s pause before the first poll, the old test fails deterministically with `retry_count = 100, want 1`.
  - **Fix:** the test's outbox uses a 1 h retry base, so the reaped state stays put, and the deadline is 15 s (the loop ends as soon as the reaper has run). With the same 5 s pause the fixed test passes.
  - The original "not reaped before timeout" (reaper later than 4 s under load) is covered by the longer deadline. That cause is unconfirmed.

