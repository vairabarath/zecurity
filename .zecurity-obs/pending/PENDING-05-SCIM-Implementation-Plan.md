---
type: plan
status: locked
id: PENDING-05-PLAN
domain: identity
created: 2026-08-10
authoritative_contract: ADR-025-SCIM-Directory-Synchronization
related:
  - ADR-025-SCIM-Directory-Synchronization
  - ADR-024-Identity-Linking-and-Provider-Migration
  - PENDING-05-Directory-Sync-SCIM
  - PENDING-15-Durable-Outbox-Infrastructure
  - PENDING-13-Client-Device-Lifecycle
tags: [plan, identity, scim, pending-05, locked]
---

# PENDING-05 — SCIM Implementation Plan (LOCKED)

> **Authoritative contract:** [[ADR-025-SCIM-Directory-Synchronization]] (ACCEPTED 2026-08-10). This
> plan **implements** that ADR; it does **not** change its architecture. Where this plan and the ADR
> ever disagree, the ADR wins.

## Locked decisions (from review, 2026-08-10)

- **Q1 — Device side via boundary, not a fake path.** SCIM identity lifecycle ships **independently of
  PENDING-15**. Device-trust events are emitted through the `identity.SideEffectSink` boundary. The
  **production guarantee comes only from PENDING-15.** We do **NOT** implement a direct/best-effort
  device-revocation path — nothing may create an ambiguous "maybe-enforced" security state.
- **Q2 — Real permission, not ADMIN.** `identity.mapping.break_glass` is a **dedicated fine-grained
  permission**. Introduce the smallest permission primitive to represent and check it. ADMIN does **not**
  implicitly grant it. Break-glass requires: the explicit permission · a mandatory reason · an audit
  event · MFA **when the auth infrastructure can support it** (PENDING-06; hook now, enforce later).
- **Q3 — Full stack.** Backend + GraphQL **and** the React admin UI are in scope.
- **Q4 — Conformance now, live interop pending.** Build the SCIM conformance test suite now; mark
  **live Okta/Entra interoperability as pending tenant access** — never mark P-tests "done" without it.
- **Q5 — No reserved migration number.** Coordinate after migrations `030` (FQDN) and `033` (outbox)
  land on `fixed-pendings`, then take the next number from the actual tree.

## The PENDING-15 boundary (the one real dependency)

Only **durable delivery of device-trust events** depends on PENDING-15. Everything else is independent.
The deprovision transaction splits:

| Deprovision effect | Depends on |
| --- | --- |
| suspend/delete · `identity_generation` bump · **session kill** (`identity.Revoker`) · audit · ACL invalidate (`policy.Notifier`) | **INDEPENDENT** (local, in-tx, reuses PENDING-04) |
| enqueue `device.trust.revoke.requested` durably | **PENDING-15** |
| execute cert/device revocation | **PENDING-13** |

**The seam — `identity.SideEffectSink`:**

```text
interface SideEffectSink { Enqueue(ctx, tx, DeviceTrustEvent) error }

impls:
  DurableOutboxSink   → PENDING-15 outbox.Enqueue(tx, evt)   [production guarantee]
  UnwiredSink (interim) → writes an audit_logs record "device-trust <type> REQUESTED,
                          NOT durably delivered (PENDING-15 unwired)"; returns nil so the
                          identity tx commits; NO revocation, NO fake delivery.
```

**Interim posture (until PENDING-15 lands) — stated, not hidden:**
- SCIM deprovision **fully cuts Zecurity access** (sessions dead, policy re-evaluated). ✅
- Device cert revocation is **NOT guaranteed** — the event is audit-recorded but not delivered. A
  startup log + an Identity-Health / admin surface must show **"device-trust delivery: not configured."**
- Residual risk during the gap: a deprovisioned user's existing device cert remains valid until its TTL;
  they cannot authenticate to obtain a new one (sessions + login are cut). Bounded, and surfaced.
- **On PENDING-15 landing:** swap the sink (one wiring line) **and** run a one-time **reconciliation**
  that replays device-trust intents recorded in `audit_logs` during the gap.

## Dependency legend
`[I]` independent · `[15]` needs PENDING-15 · `[13]` needs PENDING-13 · `[PERM]` uses the break-glass
permission primitive (P3) · `[FE]` frontend · `[TEN]` needs live Okta/Entra tenants.

---

## Backend phases

### P1 — Schema `[I]`
Migration (number per Q5). ADR-025 §12 exactly: `identity_connections` (+`subject_claim`,
`scim_identifier`, `scim_enabled`, `last_sync_at`, `status`+`'deleted'`); `users` (+`provisioned_by`
immutable, +`provisioning_owner` mutable, +`sync_instance_id`); `groups` (+`origin`, +`external_id`);
`external_identities` (+`sync_instance_id`); new `scim_tokens`, `scim_identity_conflicts` (+ unique
pending index), `scim_sync_instances`. **Not** `outbox_events` (that is PENDING-15's migration).
**Gate:** applies clean on a fresh DB; existing PENDING-04 integration tests still green.

### P2 — SCIM token authentication `[I]`
`internal/scim` token store: **HMAC-SHA256** keyed by new env `SCIM_TOKEN_HASH_KEY` (separate from
`PKI_MASTER_SECRET`); dual-token rotation (≤2 active per `(workspace,connection)`, 24h grace via
`SCIM_TOKEN_ROTATION_GRACE_HOURS`, never extend an earlier expiry, row-lock on rotate); lookup-by-hash →
bind `(workspace_id, connection_id)`; `last_used_at`; explicit revoke; auto-expire event. Bearer
middleware for `/scim/v2/*`. **Gate:** rotation/grace/scope unit-tested; a token for (A,X) cannot touch
(B,·) or (A,Y).

### P3 — Break-glass permission primitive `[I]` `[PERM]`
Smallest fine-grained permission store (e.g. `workspace_permissions(workspace_id, user_id, permission,
granted_by, granted_at)`) + `HasPermission(ctx, workspace, user, perm)` + grant/revoke (audited).
`identity.mapping.break_glass` is the first permission. **ADMIN does not implicitly hold it.** MFA hook
present, enforced only where the auth infra supports it (PENDING-06). **Gate:** possession is explicit;
an ADMIN without the grant is denied; grant/revoke audited.

### P4 — Provider profiles + identity mapping + validation `[I]` `[PERM]`
Built-in **provider profiles** (Okta / Entra / JumpCloud / Keycloak / Generic SCIM 2.0) + per-connection
overrides — **one engine, no per-provider handlers**. Canonical Identity Key from `scimIdentifier`;
`testIdpConnection` does the **active probe-user round-trip** (`POST → GET → verify → DELETE`), read-only
fallback where unsupported. Fail-closed unless `identity.mapping.break_glass` (P3) → override needs
reason + audit (`scim.mapping.break_glass_override`) + MFA-when-available. **Gate:** mapping equivalence
enforced; unproven mapping keeps SCIM disabled; only the permission (not ADMIN) can override.

### P5 — SCIM Users: provision + update `[I]`
`internal/scim` engine + `identity.DirectoryService` binding `(workspace_id, connection_id)` from the
token into **every** query (§10). Provision → `identity.Resolver` on `(connection, CanonicalIdentityKey)`;
miss → `identity.Linker` JIT-create (`provisioned_by=scim`, `provisioning_owner=scim`, `sync_instance_id`).
Update → directory-owned attrs only, **Zecurity-owned rejected at the mutation layer**. RFC 7644
envelopes/errors, `meta.version` (seam for `If-Match`), filter `eq` only, tombstones hidden from
collections. **Gate:** provision/update DB-integration green; scope isolation proven.

### P6 — SCIM Users: deprovision + reactivate  (identity `[I]` · device `[15]`/`[13]`)
Atomic, one transaction: suspend (`active=false`) / soft-delete (`DELETE`) + `identity_generation` bump +
`identity.Revoker` session kill + `audit` + `policy.Notifier` — **all `[I]`**. In the *same* tx, call
`SideEffectSink.Enqueue(device.trust.revoke.requested | re_enrollment_required)` — boundary `[I]`, the
**durable guarantee `[15]`**, execution `[13]`. A downstream failure must never roll back the committed
identity mutation. Repeated DELETE idempotent. **Gate:** identity effects fully tested independent of
PENDING-15; sink called in-tx; interim sink records-not-delivers.

### P7 — SCIM Groups `[I]`
`scim`-origin groups keyed on connection-scoped `external_id`; sync `group_members`; `NotifyPolicyChange`;
out-of-order member → **`404`** (no dangling membership); origin-aware identifiers everywhere. **Gate:**
group sync + ACL invalidation tested; manual/system groups untouched.

### P8 — Identity Conflict workflow `[I]` (`[FE]` for the queue UI)
`scim_identity_conflicts`: `409 identity_conflict` on JIT/manual collision; one **pending** record per
`(workspace, connection, canonical_identity_key)`; consistent across POST/PUT/PATCH/DELETE; no
auto-takeover; **Accept-Link** (admin, atomic: confirm `external_identities` link + `provisioning_owner
→ scim`, preserve local roles/policies/devices, audit) / **Reject** / **Reopen**. **Gate:** conflict FSM
+ uniqueness + "rejected never auto-approves" tested.

### P9 — Connection lifecycle + Identity Health + Sync instances `[I]` (`[FE]` for surfaces)
`status: active → disabled → deleted`: **DISABLE** suspends users (reversible) + kills sessions +
`provisioning_owner scim → unmanaged`; **DELETE** guarded (0 linked users or explicit destructive
confirmation), never deletes users. `last_sync_at` → **Identity Health** (Healthy/Delayed/Disconnected)
surface (+ "device-trust delivery: not configured" while interim, per Q1). `scim_sync_instances` opened
per connect; stamp provisioned objects; reconcile on reconnect. **Gate:** lifecycle transitions +
no-lockout + health tested.

### P10 — SideEffectSink production wiring `[15]`
Implement `DurableOutboxSink` over PENDING-15 `outbox.Enqueue(tx, evt)`; swap it in at `main.go`
(one line). Verify the enqueue commits in the deprovision tx and that downstream failure never rolls it
back. Run the **gap reconciliation** (replay device-trust intents recorded during interim). **Gate:**
end-to-end deprovision → outbox → (PENDING-13 consumer) once 15 + 13 exist.

---

## Frontend phases (Q3 — in scope)  `[FE]`

### P11 — Connection SCIM config + token UI `[FE]`
Provider-preset picker (pre-fills issuer template / scopes / `subject_claim` / `scim_identifier`),
enable-SCIM, run mapping validation, mint/rotate SCIM token (**plaintext shown once**), show SCIM base
URL to paste into the IdP.

### P12 — Identity Health + directory-owned fields `[FE]`
Identity Health indicator (Healthy/Delayed/Disconnected + "device-trust delivery: not configured" during
interim). Directory-owned user attributes rendered **read-only** with "Managed by Google Workspace /
Microsoft Entra"; Zecurity-owned fields editable.

### P13-UI — Provisioning Conflicts + group origins `[FE]`
Provisioning-Conflicts queue (Accept-Link / Reject / Reopen, with reason + audit); groups always shown
origin-labelled (`Engineering · SCIM` / `· Local` / `· System`) with `external_id`/connection where
relevant — **never display-name alone**.

---

## P-TEST — Tests + conformance `[I]` + `[TEN]`
- Unit: token rotation/grace, mapping validation, break-glass authorization, ownership enforcement,
  conflict FSM, connection-lifecycle guards, sink boundary.
- DB-integration: provision/update/deprovision (identity effects), groups, tenant/connection scope
  isolation, atomic-tx, generation-bump session kill.
- **SCIM conformance suite** (mock SCIM client + fixtures): RFC 7644 PATCH semantics, `eq` filter,
  out-of-order-404 retry, idempotency, error envelopes.
- **`[TEN]` Live interoperability (Okta + Entra): implement the harness; mark PENDING until real tenants
  are available.** Do not report live interop "done" without tenant runs.

---

## Sequencing

```
Backbone (sequential):  P1 → P2 → P3 → P4 → P5
After P5 (parallel):    P6(identity) · P7 · P8 · P9
Gated on PENDING-15:    P6(device enqueue guarantee) · P10
Frontend (after its backend API):  P11 (needs P2/P4) · P12 (needs P5/P9) · P13-UI (needs P8)
Continuous from P5:     P-TEST
```

**Ships without PENDING-15 ever arriving:** P1–P9 backend + P11–P13-UI frontend + P-TEST (minus live
interop) = directory-driven provisioning, updates, groups, conflicts, connection lifecycle, and
**access-cutting deprovision (sessions + policy)**. Only durable **cert revocation** waits on P10 (15+13).

Go-only in `controller/` for backend (SCIM is inbound HTTP — no Rust/proto); React in `admin/` for
frontend. Per-phase gate: `go build ./... && go vet ./... && go test …` + `gqlgen generate` (backend
schema changes) + `npm run codegen` (frontend).

## Cross-team boundaries
- **PENDING-05 (this):** decide + enqueue. Owns the SCIM engine, identity lifecycle, conflict workflow,
  connection lifecycle, and the `SideEffectSink` boundary.
- **PENDING-15 (Sathiya):** durable delivery/retry (`outbox_events`, `Enqueue`, claim, retry, recovery).
- **PENDING-13 (device):** consume `device.trust.*` → cert/device-trust execution (`RevokeUserDevices`).

## Open follow-ups (not blockers)
- MFA on break-glass activates when PENDING-06 (step-up) lands.
- Provider-preset catalog (issuer template + scopes + mapping defaults) shared with ADR-024 §7.
- Migration number assigned at integration time (Q5).
