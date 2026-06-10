---
type: code-study
flow: protect-resource-end-to-end
created: 2026-06-01
status: in-progress
---

# Code Study 02 — Protect Resource Flow End-to-End

> Trace the complete path of an admin protecting a resource: from clicking **"Protect"** in the Admin UI, through the Controller (Go) updating the DB and pushing an instruction, the Connector (Rust) relaying it, the Shield (Rust) writing an nftables firewall rule, and the **ResourceAck** travelling all the way back to flip the DB to `protected`.
>
> We review this in **8 ordered pieces**. Each piece has its own section below with a checkbox, the files involved, what to review, and a notes area for findings/improvements.

---

## High-Level Flow

```
[Admin UI] ──protectResource(id)──► [Controller resolver]
                                         │
                          ┌──────────────┴───────────────┐
                          ▼                               ▼
                  MarkProtecting (DB)            PushInstruction → Connector
                                                          │
                                            ┌─────────────┴─────────────┐
                                  online: control-stream push    offline: buffer in DB
                                                          ▼
                                              [Connector] receives batch
                                                          │
                                       ShieldRegistry.push_instructions
                                            ┌─────────────┴─────────────┐
                                  shield online: mpsc channel    offline: buffer in map
                                                          ▼
                                          [Shield] control-stream recv
                                                          │
                                   validate_host → apply_nftables (flush+rebuild)
                                                          ▼
                                            ResourceAck (protected/failed)
                                                          │
                                  Shield ──► Connector ──► Controller (ResourceAckBatch)
                                                          ▼
                                              [DB] resource → 'protected'
```

---

## Progress Tracker

- [x] **Piece 1** — Admin UI trigger ✅
- [x] **Piece 2** — Controller resolver ✅
- [x] **Piece 3** — Controller DB write (state machine) ✅ (findings recorded; fixes deferred)
- [ ] **Piece 4** — Controller → Connector delivery
- [ ] **Piece 5** — Connector receives + queues
- [ ] **Piece 6** — Connector → Shield forwarding (:9091)
- [ ] **Piece 7** — Shield applies firewall rule
- [ ] **Piece 8** — Ack back: Shield → Connector → Controller

---

# Piece 1 — Admin UI Trigger

**Status:** ✅ reviewed (2026-06-01)

Admin clicks "Protect this resource" → fires the `protectResource(id)` GraphQL mutation.

**Files**
- `admin/src/pages/ResourceDetail.tsx:108` — `useMutation(ProtectResourceDocument)`
- `admin/src/pages/ResourceDetail.tsx:132-135` — `handleProtect` → `protectResource({ variables: { id: resourceId } })`
- `admin/src/pages/ResourceDetail.tsx:219,308,512` — three buttons, all call `handleProtect`
- `admin/src/pages/ResourceDetail.tsx:485-515` — shield selector panel
- `admin/src/graphql/mutations.graphql:81` — `mutation ProtectResource($id: ID!)`
- `controller/graph/resource.graphqls:45` — `protectResource(id: ID!): Resource!`

**To review**
- [x] Reconcile git-diff mismatch — **RESOLVED**: modified files (`Resources.tsx`, `CreateResourceModal.tsx`) are an unrelated refactor of the *create-resource prefill* flow (Prettier + lazy `useState` initializers + modal `key` remount). No second protect entry point; protect lives only in `ResourceDetail.tsx` (unchanged), rendered as 3 buttons → 1 handler.
- [x] Optimistic UI / loading state — `protecting` flag disables button in-flight; real progress signal is the `protecting` transitional banner (`:346-354`); polling tightens to 3s while transitional (`:102-106`). Correct.
- [x] UI/backend gate parity (Finding 2) — protect now gated on `canProtect = shield.status === Active`, mirroring the `MarkProtecting` `status='active'` precondition exactly.
- [x] Error handling — all three mutations wire `onError` → toast (`:113`). OK.

**Notes**

🔴 **Finding 1 (important) — shield picker is a dead control. → ✅ FIXED (2026-06-01).**

**Fix applied** in `admin/src/pages/ResourceDetail.tsx` (tsc clean):
- Deleted `selectedShieldId` state + its effect, and the entire `GetShields` query / `shields` / `candidateShields` (relied on `resource.shield` which already carries `{id,name,status,lanIp}` from `GetAllResources`).
- Removed unused imports: `GetShieldsDocument`, `ShieldStatus`, `CheckCircle2`.
- Protection panel: picker → read-only bound-shield card (name + lanIp + "Ready") + single **"Protect this resource"** button.
- All three gates (header `:disabled`, hero text branch, hero button) now key off the bound `shield` instead of `candidateShields.length` — also tightens Finding 2's UI looseness (no longer "any network shield").
- "No shield installed" fallback now triggers on null `resource.shield` (shield revoked/deleted), which is more correct.

---

_Original analysis below._


`ResourceDetail.tsx:485-515` renders a shield selector (`selectedShieldId`) and a button labelled *"Protect with {shieldName}"*, but `handleProtect` (`:132`) sends only `{ id: resourceId }`, and the mutation `ProtectResource($id: ID!)` has no `shieldId` param. `selectedShieldId` is used only for highlight (`:492`), checkmark (`:506`), and label (`:514`) — never sent.

**Confirmed root cause:** the shield binding is automatic and immutable, not an admin choice:
- `store.go:85-105` `AutoMatchShield` — at create, shield is matched by `shields.lan_ip == resource.host` within the remote network (`LIMIT 1`).
- `store.go:107-128` `Create` — that shieldID is written to `resources.shield_id`.
- `store.go:205-212` `UpdateInput` — has **no `shield_id` / no `host`** field → binding cannot be changed via edit. Fixed 1:1, host → shield-on-that-host.

The picker is also *misleading*: its `candidateShields` list = all non-revoked shields in the **remote network** (`:87-94`), but only the host-matched shield (already in `resource.shield_id`) can enforce this resource.

**Fix:** delete the selector + `selectedShieldId` state (`:78`, `:96-100`, `:486-510`); replace with a read-only "will be enforced by `{resource.shield.name}` (`{lanIp}`)" line (mirror the protected panel `:466-479`); button label → "Protect this resource". Keep the "No shield installed" fallback (`:517-525`) for null `resource.shield`.

🟠 **Finding 2 (minor) — UI gate looser than backend precondition. → ✅ FIXED (2026-06-01).**
`candidateShields` filtered only `!== Revoked`; button disabled only when `candidateShields.length === 0`. But `MarkProtecting` (Piece 3) requires the assigned shield be `active`. A `disconnected`/`pending` shield still enabled the button → click → controller rejects → error toast.

**Mapping verified:** controller maps DB status → GraphQL enum verbatim via `strings.ToUpper(status)` (`helpers.go:113,179`) — **no** heartbeat-staleness derivation. So DB `'active'` ⇔ `ShieldStatus.Active` exactly. The backend gate (`MarkProtecting` `status='active'`) mirrors 1:1 to the UI.

**Fix applied** in `ResourceDetail.tsx` (tsc clean):
- Added derived `const canProtect = shield?.status === ShieldStatus.Active` (re-added `ShieldStatus` import).
- All three protect gates now use `canProtect` (header `disabled`, hero button render, panel button render) instead of mere shield existence.
- Added status messaging for the bound-but-not-active case: hero text + panel show *"The shield … is {status}. It must reconnect before this resource can be protected."*, and the panel pill shows the live status (amber) instead of "Ready".
- Net effect: you can only click Protect when the bound shield is genuinely `active` — exactly when the controller will accept it. No more click→error round-trip.

✅ **Correct:** DB-first ordering's UI counterpart (success → refetch + 3s polling to observe the ack from Piece 8); clean separation of in-flight flag vs. transitional banner; error paths wired.

**Carry-forward to verify in later pieces:**
- Where/when does `resources.shield_id` actually get assigned (create? edit? host-match)? Needed to judge Finding 1's fix. → revisit in Piece 3.

---

# Piece 2 — Controller Resolver

**Status:** ✅ reviewed (2026-06-01)

`ProtectResource` resolver: `tenant.MustGet` → `MarkProtecting` (DB-first) → `PushInstruction` → return transitional resource.

**Files**
- `controller/graph/resolvers/resource.resolvers.go:56-67` — resolver body
- `controller/graph/resolvers/resolver.go:15-28` — `Resolver` struct (`ResourceCfg`, `ConnectorRegistry`)
- `controller/internal/tenant/context.go:16-54` — `TenantContext`, `MustGet` (panics if middleware bypassed)
- `controller/internal/connector/control_stream.go:72-112` — `PushInstruction` / `PushResourceInstruction`
- `admin/src/graphql/mutations.graphql:81-86` — mutation selects only `{ id, status }`

**To review**
- [x] Tenant scoping — `MustGet` reads JWT-derived identity; `TenantID` flows into `MarkProtecting` WHERE clause → IDOR-safe across tenants.
- [x] Ordering: DB write before push — correct, DB is source of truth; enables offline-safe replay.
- [x] `PushInstruction` return ignored — offline case fine; online-send-failure is a reliability edge (Finding 4).
- [x] Authorization — **MISSING** (Finding 3).

**Notes**

✅ **Correct:**
- Thin orchestrator; all logic in store/registry.
- Tenant isolation real: `MustGet` panics if middleware bypassed (gqlgen recover → error, no crash); `tenant_id` in `MarkProtecting` WHERE prevents cross-tenant protect (returns "not found").
- DB-first ordering is the central invariant enabling offline-safe delivery.
- Response is minimal `{id,status}`; `status='protecting'` is the transitional signal Piece 1 polls on. Partial `Shield` from `toResourceGQL` (no `lanIp`) is never observed here.

🔴 **Finding 3 (important) — no authorization/role check. → ✅ FIXED (2026-06-01, admin-only).**
`ProtectResource` (and sibling `Create`/`Update`/`Unprotect`/`Delete` resource mutations) checked authentication (`MustGet`) but never authorization. No schema directives exist; auth is done **inline per-resolver** elsewhere — e.g. `client.resolvers.go:27`, `log.resolvers.go:23`: `if tc.Role != "admin" { return forbidden }`. Resource mutations omitted this.

**Reachable today, not latent:** the invitation flow already mints non-admin users — `invitation/store.go:70-73` hardcodes `role='member'`, and invited users log in with a `member` JWT (`bootstrap.go:205` `runInvitedUserTransaction`). First/workspace-creator user is `admin` (`bootstrap.go:183,198`). So any invited member could protect/unprotect/delete resources (live firewall changes). Stale TODO at `invitation/handler.go:49` referenced a planned `RequireRole` middleware that never landed.

**Fix — superseded by centralized RBAC (2026-06-01).** Initially added inline `tc.Role != "admin"` to the 5 resource mutations; then converted the whole API to a **`@hasRole` GraphQL directive** (decision: admin-only now; reads gated too; revokeDevice stays admin).

**What landed:**
- `directive @hasRole(roles: [Role!]!) on FIELD_DEFINITION` declared in `schema.graphqls`; impl `resolvers.HasRole` (`graph/resolvers/directives.go`) reads `TenantContext`, case-insensitive role match, `tenant.Get` (clean error, no panic); wired via `graph.Config{ Directives: ... }` in `cmd/server/main.go`.
- Annotated **all admin mutations** (resource ×5, connector ×5, shield ×3, discovery ×2, policy ×7, client createInvitation/revokeDevice) **and infra read queries** (remoteNetworks/remoteNetwork/connectors/connector, shields/shield, resources/allResources, groups/group, getDiscoveredServices/getScanResults, users, clientDevices, connectorLogs) with `@hasRole(roles: [ADMIN])`.
- **Left open (any authed):** `me`, `myDevices`, `workspace`. **Public (unannotated):** `initiateAuth`, `invitation(token)`, `lookupWorkspace`, `lookupWorkspacesByEmail`.
- Removed all 9 inline `tc.Role` checks (resource ×5, client ×2, clientDevices, connectorLogs).
- **REST loose ends closed:** wrapped `/api/connectors/` + `/api/shields/` token routes with existing `RequireRole("admin")` middleware (mirrors invite route) — these were also ungated.
- Regenerated `controller/graph/generated.go` (`make gqlgen`) + frontend (`npm run codegen`, no TS change). `go build`/`go vet`/`tsc` all clean.

**Why directive not HTTP middleware:** `RequireRole` is route-level; `/graphql` is one route multiplexing all ops, so it can't gate per-operation. The directive runs in the GraphQL execution layer where it sees each field. **Future roles (devops/auditor):** just widen the list, e.g. `@hasRole(roles: [ADMIN, AUDITOR])` on read queries — no signature change, no resolver edits.

**Reachability that justified urgency:** the invitation flow already mints `member`-role users (`invitation/store.go:70-73` hardcodes `'member'`; `bootstrap.go:205` `runInvitedUserTransaction`), and the admin SPA's route guard (`App.tsx:47`) is client-side only — a member with a JWT could call any ungated mutation directly. Now closed.

🟠 **Finding 4 (minor / reliability edge) — push outcome silently discarded.**
`PushInstruction` returns nothing; internally `_ = r.PushResourceInstruction(...)` (`control_stream.go:84`) swallows the error. Offline connector → fine (reconnect replay, Piece 4). But **online-but-`c.send`-failed** (`:107`) is only logged; row stays `protecting`, and with no reconnect there's no auto-resend → resource may sit in `protecting` while Piece 1 polls forever. Also: `PushInstruction` runs **synchronously** in the resolver — if `c.send` can block on a full channel it blocks the GraphQL request. → **Verify in Piece 4:** reconciliation/heartbeat resend? `c.send` blocking semantics?

**Dismissed:** partial `Shield` in `toResourceGQL` — harmless here (mutation doesn't select shield fields); only a latent risk if a future mutation returning this partial object selects `shield { lanIp }`.

---

# Piece 3 — Controller DB Write (State Machine)

**Status:** ✅ reviewed (2026-06-02) — findings recorded, fixes deferred (no code change this pass)

The resource lifecycle is `status` + `pending_action` on the `resources` row. States: `pending → protecting(apply) → protected/failed`, and `protected → protecting(remove) → unprotected`. `protecting` is the only transitional state and is **left only by an incoming `RecordAck` from the shield** (or reconnect replay) — the controller writes intent, the shield writes outcome.

**Files**
- `controller/internal/resource/store.go:275-294` — `MarkProtecting` (guards: tenant + source-state `{pending,failed,unprotected}` + shield `active`, all atomic)
- `controller/internal/resource/store.go:297-315` — `MarkUnprotecting` (source must be `protected`; **no** shield-active check)
- `controller/internal/resource/store.go:337-356` — `RecordAck` (shield ack → terminal state; stale-ack guards; periodic re-verify path)
- `controller/internal/resource/store.go:318-334` — `SoftDelete` (hard `DELETE`; guard only `status != 'protecting'`)
- `controller/internal/resource/store.go:176-201` — `GetPendingForShield` (reconnect replay of `protecting` rows)
- `migrations/007,008,009` — status CHECK enum + `deleted_at` + partial index history

**To review**
- [x] Allowed source states `{pending,failed,unprotected}` + shield-active precondition atomic in one UPDATE (no TOCTOU). Correct.
- [x] `error_message=NULL` reset on retry. Correct.
- [x] `RecordAck` stale-ack guards (the two `AND NOT (...)` clauses) prevent a late opposite-direction ack from clobbering in-flight intent. Correct.
- [x] Soft-delete handling — **vestigial** (Finding 8).

**Notes**

🔴 **Finding 5 (important) — deleting a `protected` resource orphans the shield firewall rule + guard contradicts its error. → PLANNED: see [[Decisions/ADR-004-Resource-Reconciliation]].**
`SoftDelete` (`:318-334`) guards only `status != 'protecting'`, so it **allows deleting a `protected` row** — but the error says *"must be unprotected before deleting"*. It's a hard `DELETE`, so no `remove` is sent to the shield; the shield rebuilds its chain from its own active set and keeps enforcing the orphaned nftables rule. Later shield re-verify acks hit `RecordAck`'s `WHERE … deleted_at IS NULL` → 0 rows, silent no-op. No console path to clean it up. Also: `failed` (port-not-listening) **still holds a rule** (`shield/src/resources.rs handle_apply`), so blocking only `protected` is insufficient.

**Decision (ADR-004):** do NOT immediate-delete. Move to **desired-state reconciliation** with a `deleting` **tombstone** + shield **state report** + controller **reconciler**, governed by the invariant *"never destroy the record of intent until the effect is observably confirmed."* Rejected the "delete now, reconcile later" idea (destroys the reconciliation anchor → permanent invisible drift; transient disconnect retains the rule). 4-phase manual plan in the ADR — Phase 1 (tombstone delete) is independently shippable and fixes this finding; Phases 2–3 add snapshot-on-reconnect + closed-loop reconciliation.

**✅ Phase 1 IMPLEMENTED + VERIFIED (2026-06-03)** on the live stack — online delete reaps in <2s; offline delete holds at `deleting` then auto-reaps on shield reconnect (no orphan); pending delete is immediate. Migration 015 (+ Finding 9 index fix) live. Not yet committed. See [[Decisions/ADR-004-Resource-Reconciliation]].

🔴 **Finding 6b (NEW) — fail-open after shield reboot.**
Shield reboot wipes in-kernel nftables (`network::setup()` deletes the whole `zecurity` table on startup) and the in-memory `active` list (`SharedResourceState::new()` starts empty). The controller only re-pushes `status='protecting'` on reconnect (`GetPendingForShield`), **never `protected`** — so previously-protected resources silently become **unprotected** and are never restored. Security fail-open. **Same root cause as Finding 5/6; fixed by ADR-004 Phase 2 (desired-state snapshot on reconnect).**

🟠 **Finding 6 — no controller-side backstop out of `protecting` (= Piece 2 Finding 4, confirmed at DB layer).**
A row leaves `protecting` ONLY via `RecordAck` or reconnect replay (`GetPendingForShield`). No timeout sweep (`protecting` too long → `failed`), no periodic re-push. With Piece 2 Finding 4 (discarded online send error) + no reconnect → **`protecting` forever**, UI polls indefinitely. **Subsumed by ADR-004 Phase 3 (closed-loop reconciler re-issues applies/removes).**

🟠 **Finding 7 — protect/unprotect shield-active asymmetry → unprotect can stick forever.**
`MarkProtecting` requires shield `active`; `MarkUnprotecting` (`:297-315`) requires only `status='protected'` — no shield check. Unprotecting against an offline/disconnected/**revoked** shield → `protecting/remove` waiting for an ack that never comes if the shield is gone. Consider resolving unprotect-against-dead-shield straight to `unprotected`. **Decision needed; deferred.**

🟡 **Finding 8 (minor) — vestigial soft-delete scaffolding. ✅ FIXED (2026-06-10, ADR-004 Phase 4.2, Option A).**
Schema has `deleted_at` (mig 007) + `'deleted'` status (008/009) + ~5 `deleted_at IS NULL` filters, but `SoftDelete` hard-deletes (`DELETE FROM`). So `deleted_at` is never set → all those filters are no-ops, `'deleted'` status is unreachable, function name lies, UI `'deleted'` tone is dead. Cleanup: rename to `Delete` + drop dead filters/status, OR restore real soft-delete (the latter also helps Finding 5).
> **Resolution:** dropped the scaffolding (migration `017_resources_drop_soft_delete.sql`: `DROP COLUMN deleted_at`, rebuilt both partial indexes without the predicate, removed `'deleted'` from `resources_status_check`); stripped all seven `deleted_at IS NULL` filters in `store.go`; aligned `resourceTone` in both `Resources.tsx` and `ResourceDetail.tsx` to the real enum (also fixed a missing `deleting` tone in `Resources.tsx`). Only the `resources` table was touched — the `'deleted'` soft-delete on workspaces/users/connectors/shields/remote_networks is real and untouched.

🟡 **Finding 9 (minor, perf) — stale partial index after state rename.**
Mig 007 `idx_resources_managing ON resources(shield_id,status) WHERE status IN ('managing','removing')`; mig 009 renamed those states to `protecting` but never recreated the index → it now matches **zero rows**, and `GetPendingForShield`'s `WHERE shield_id=$1 AND status='protecting'` has no supporting partial index. Replace predicate with `WHERE status='protecting'`.

**Dismissed:** `RecordAck` writing shield-supplied status verbatim — `resources_status_check` CHECK (mig 009) rejects out-of-enum values; shield is mTLS-trusted. Not an issue.

---

# Piece 4 — Controller → Connector Delivery

**Status:** ⬜ not reviewed

`PushInstruction` builds the proto, `PushResourceInstruction` sends over the live control stream. `pushPendingInstructions` replays DB-pending instructions on reconnect.

**Files**
- `controller/internal/connector/control_stream.go:69-112` — push path
- `controller/internal/connector/control_stream.go:229-274` — reconnect replay

**To review**
- [ ] Offline-safe semantics (returns nil when connector absent)
- [ ] Per-shield batching in `ResourceInstructionBatch`
- [ ] Reconnect query scope (`status NOT IN ('revoked','deleted')`)
- [ ] **Doc drift:** CLAUDE.md says "heartbeat piggyback only — no new RPCs", but this is a persistent bidirectional control stream pushing in near-real-time. Confirm intent / update docs.

**Notes**
> _(findings go here)_

---

# Piece 5 — Connector Receives + Queues

**Status:** ⬜ not reviewed

`handle_controller_msg` dispatches the batch → `ShieldRegistry::push_instructions`: send via mpsc if shield online, else buffer in `resource_instructions` map.

**Files**
- `connector/src/control_stream.rs:247-251` — receive + dispatch
- `connector/src/agent_server.rs:82-105` — `push_instructions`
- `connector/src/agent_server.rs:28-34` — `ShieldMaps` buffer struct

**To review**
- [ ] Race: shield connects between `get(tx)` check and buffer insert
- [ ] Buffer overwrite semantics (`insert` replaces prior buffered vec — lost instructions?)
- [ ] `tokio::spawn` per-push ordering guarantees
- [ ] Channel-closed handling (`warn` + break)

**Notes**
> _(findings go here)_

---

# Piece 6 — Connector → Shield Forwarding (:9091)

**Status:** ⬜ not reviewed

The `control()` gRPC handler on `:9091`. On shield connect: drain buffered instructions. Live: forward via `instr_rx`. Piggybacked on `ShieldControlMessage`.

**Files**
- `connector/src/agent_server.rs:342-472` — `control()` handler
- `connector/src/agent_server.rs:370-387` — drain buffered on connect
- `connector/src/agent_server.rs:450-458` — forward live instructions

**To review**
- [ ] Drain-then-subscribe ordering (no gap between buffered drain and live channel)
- [ ] mTLS / SPIFFE verification on the shield connection
- [ ] Channel capacity (32) backpressure behavior

**Notes**
> _(findings go here)_

---

# Piece 7 — Shield Applies Firewall Rule

**Status:** ⬜ not reviewed

`handle_instruction` → `handle_apply` → `validate_host` (`resource.host == detect_lan_ip()`) → `apply_nftables` (flush + atomic rebuild of `resource_protect` chain) → `check_port` → builds `ResourceAck`.

**Files**
- `shield/src/resources.rs:211-227` — `handle_instruction` dispatch
- `shield/src/resources.rs:229-317` — `handle_apply`
- `shield/src/resources.rs:56-64` — `validate_host`
- `shield/src/resources.rs:93-164` — `apply_nftables` (flush + atomic rebuild)
- `shield/src/util.rs:34-56` — `detect_lan_ip`

**To review**
- [ ] Host validation correctness (`127.0.0.1` shortcut + RFC-1918 match)
- [ ] Atomic flush+rebuild of `resource_protect` chain (never appended)
- [ ] State rollback on nftables failure (`retain` removes failed resource)
- [ ] `check_port` → `status` mapping (`protected` vs `failed` on "port not listening")
- [ ] Three rules per resource: `iif lo accept`, `127.0.0.0/8 accept`, `port drop`

**Notes**
> _(findings go here)_

---

# Piece 8 — Ack Back: Shield → Connector → Controller

**Status:** ⬜ not reviewed

Shield emits `ResourceAck` (immediately on apply + drained on next heartbeat). Connector forwards via `ack_tx` → `ResourceAckBatch`. Controller updates DB → `protected`/`failed`.

**Files**
- `shield/src/control_stream.rs:137-144` — drain acks on heartbeat tick
- `shield/src/control_stream.rs:173-187` — immediate ack on instruction
- `connector/src/agent_server.rs:412-414` — receive `ResourceAck` from shield
- `connector/src/control_stream.rs:190-207` — forward as `ResourceAckBatch` to controller
- _(Controller-side ack handler — locate during review)_

**To review**
- [ ] Double-send: ack sent immediately AND drained on heartbeat — dedup needed?
- [ ] Controller-side handler that flips `protecting` → `protected`/`failed` (find it)
- [ ] `verified_at` / `port_reachable` persistence
- [ ] Idempotency of ack application

**Notes**
> _(findings go here)_

---

## Cross-Cutting Findings / Improvements

> Running list of issues spanning multiple pieces.

1. **Doc drift** — CLAUDE.md "heartbeat piggyback only — no new RPCs" vs. actual persistent bidirectional control stream with near-real-time push. (Pieces 4, 6)
2. ~~**Git-diff mismatch**~~ — RESOLVED in Piece 1: modified UI files are an unrelated create-modal prefill refactor, not part of the protect path.
3. ~~**Shield picker is a dead control**~~ — ✅ FIXED (Piece 1, Finding 1). Removed the picker; panel now shows the auto-bound shield read-only. Shield is matched by host IP at create (`AutoMatchShield`) and is immutable. Also tightened the UI gate (Finding 2 UI side).
4. ~~**No authorization on resource mutations**~~ — ✅ FIXED via centralized **`@hasRole` directive** (Piece 2, Finding 3). All admin mutations + infra read queries gated `[ADMIN]`; `me`/`myDevices`/`workspace` open; public ops unannotated. 9 inline checks removed. REST token routes (`/api/connectors/`, `/api/shields/`) wrapped with `RequireRole("admin")`. Future roles widen the directive's role list. **This was the systemic fix — covers connector/shield/discovery/policy, not just resources.**
5. **Push error discarded; possible stuck `protecting`** (Piece 2 Finding 4 / Piece 3 Finding 6) — online-but-send-failed isn't retried unless connector reconnects; state machine has NO timeout/reconciliation out of `protecting`. **Open — Piece 4.**
6. **Delete of `protected` orphans shield firewall rule** (Piece 3, Finding 5) + **fail-open after reboot** (Finding 6b) + **stuck `protecting`** (Finding 6) + **protect/unprotect asymmetry** (Finding 7) — **all one root cause: incremental delivery, no reconciliation.** → **Planned fix: [[Decisions/ADR-004-Resource-Reconciliation]]** (tombstone delete + desired-state snapshot + closed-loop reconciler; 4-phase manual plan). Invariant: *never destroy intent until effect is observably confirmed.*
7. **Vestigial soft-delete + stale index** (Piece 3, Findings 8/9) — `deleted_at`/`'deleted'` scaffolding is dead (hard-delete); `idx_resources_managing` matches zero rows post-rename. **Folded into ADR-004 (Phase 1 index fix, Phase 4 cleanup).**

---

## Proto Reference

- `proto/shield/v1/shield.proto:44-59` — `ResourceInstruction`, `ResourceAck`
- `proto/shield/v1/shield.proto:82-101` — `ShieldControlMessage` oneof
- `proto/connector/v1/connector.proto:97-100` — `ResourceInstructionBatch`
- `proto/connector/v1/connector.proto:70-90` — `ConnectorControlMessage` oneof
