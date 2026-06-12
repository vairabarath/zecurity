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
- [x] **Piece 4** — Controller → Connector delivery ✅ (ADR-004 Phase 2/3 landed; F10 fixed)
- [x] **Piece 5** — Connector receives + queues ✅ (F16/F17/F18 fixed; F19 accepted/recorded)
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

**Status:** ✅ reviewed (2026-06-11)

`PushInstruction` builds the proto, `PushResourceInstruction` sends over the live control stream. `pushPendingInstructions` replays DB-pending instructions on reconnect.

> ⚠️ **The code has moved two ADR-004 phases past this doc's original snapshot.** The naive incremental-push path described here now sits inside a desired-state delivery system: **Phase 2** (snapshot-on-reconnect) lives in `control_stream.go`, and **Phase 3** (closed-loop reconciler) is an entirely new file `reconcile.go` the doc never listed. This is the "we already did something related in earlier pieces" feeling — the ADR-004 plan referenced all over Piece 3 (Findings 5, 6, 6b) has **landed**, and it resolves the carry-forwards that Pieces 2/3 parked here.

**Files**
- `controller/internal/connector/control_stream.go:126-169` — push path (`PushInstruction` / `PushResourceInstruction`)
- `controller/internal/connector/control_stream.go:71-124` — `buildSnapshotMsg` / `PushSnapshotForShield` (ADR-004 Phase 2)
- `controller/internal/connector/control_stream.go:288-345` — `pushPendingInstructions` (reconnect: snapshot-per-shield + pending replay)
- `controller/internal/connector/reconcile.go` — `handleResourceState` / `reconcileShield` (ADR-004 Phase 3 closed-loop reconciler) — **NEW, not in original doc**
- `controller/internal/resource/store.go:176-240` — `GetPendingForShield` / `GetDesiredForShield`
- `controller/internal/resource/store.go:474-506` — `GetDeletingForShield` / `ReapTombstone`

**To review**
- [x] Offline-safe semantics — ✅ all three push entry points guard: `PushInstruction` returns on empty `ConnectorID` (`:130`); `PushResourceInstruction` returns nil when connector absent (`:151-154`, "already written to DB by caller"); `PushSnapshotForShield` returns on nil connector (`:112-115`).
- [x] Per-shield batching in `ResourceInstructionBatch` — ✅ structure correct (`map[shield]→[]instr`) but **the online hot path never batches** (Finding F13): `PushInstruction` sends one resource for one shield per resolver call. Only `pushPendingInstructions` (reconnect) fills the batch.
- [x] Reconnect query scope (`status NOT IN ('revoked','deleted')`) — ✅ correct. This is the **shields** table (`control_stream.go:291`), where the `'deleted'` soft-delete is still real — unlike `resources`, where Finding 8 dropped it.
- [x] **Doc drift** — ✅ CONFIRMED real (Finding F1-doc). It is a persistent bidirectional `Control` gRPC stream with near-real-time push, plus new `ResourceState` reports and `ResourceSnapshotBatch`. CLAUDE.md "heartbeat piggyback only — no new RPCs" is now definitively contradicted.

**Carry-forwards from earlier pieces — RESOLVED by ADR-004 Phase 2/3**

| Carried into Piece 4 | Status |
|---|---|
| **Finding 4 / X-cut #5** — online `c.send` fails → stuck `protecting` forever, no resend | ✅ **Resolved.** Reconciler detects `missing` drift (desired-but-not-reported) and re-pushes the snapshot after 2 reports (`reconcile.go` drift path). Self-heals. *(Latency-coupling half remains — see F14.)* |
| **Finding 6b** — fail-open after shield reboot (protected silently lost) | ✅ **Resolved.** `GetDesiredForShield` includes `protected`+`failed` (`store.go:217-222`); snapshot pushed for **every** non-revoked/deleted shield on reconnect (`control_stream.go:308`), even with nothing pending. Reboot re-protects from the connector's cache. |
| **Finding 5** — delete of `protected` orphans the shield rule | ✅ **Resolved.** `deleting` tombstone + reap loop (`reconcile.go` tombstone pass + `ReapTombstone` `store.go:496`); snapshot replace-semantics also drops zombies. |

**Notes**

🔴 **Finding F10 (important) — reconciler held one global mutex across DB queries + network send. → ✅ FIXED (2026-06-11).**
`reconcileShield` took `h.Recon.mu` with `defer Unlock()` on entry and held it across `GetDesiredForShield`, `GetDeletingForShield`, `buildSnapshotMsg` (another DB query), `client.send`, and `ReapTombstone`. The mutex only needs to guard the in-memory `drift`/`absent` hysteresis maps — but holding it across I/O serialized reconciliation for **every connector in the controller** behind one slow query or a stalled stream send.

**Fix applied** (`reconcile.go:56-160`, build/vet/`-race` tests clean):
- Read-only DB queries + drift classification now run **lock-free** (no shared state).
- **One short locked section** updates the counter maps, captures the decisions (`shouldResync`, `resyncDriftRuns`, `toReap`) and snapshots the gauge values, then unlocks.
- **All I/O after the unlock** — snapshot re-push (`buildSnapshotMsg` + `client.send`) and `ReapTombstone` deletes.
- Behavior preserved exactly: same thresholds, counter resets, reap semantics, gauge values, and log lines.
- **Why still race-free:** a shield is owned by exactly one connector whose stream is processed by a single goroutine → per-key counter access is single-writer; the lock only guards the Go map structure against *cross-connector* concurrent access (different keys). Documented in-code so the lock isn't re-tightened later.

🟠 **Finding F11 (minor) — snapshot `Generation` was wall-clock millis, and its "Phase 3 replaces this" comment was false. → ✅ FIXED (2026-06-11).**
`buildSnapshotMsg` stamped `Generation: uint64(time.Now().UnixMilli())`. Two problems: (a) **non-monotonic** — an NTP step backwards makes a newer snapshot carry a *lower* generation, so the shield's `generation <= last` gate (`shield/src/resources.rs:405`) drops it as stale → silent drift that only self-heals on the next reconcile cycle; two snapshots in the same ms tie. (b) The comment claimed *"Phase 3 replaces this with reconciliation"* — but Phase 3 (`reconcile.go`) re-used the same wall-clock generation; it was never replaced.

**Fix applied (Option F — generation behind the Go desired-state computation, no SQL semantics):**
- **Migration `018_shield_snapshot_generation.sql`** — adds two **opaque** columns `snapshot_generation BIGINT` + `snapshot_fingerprint TEXT` to `shields`. No trigger, no predicate; SQL carries zero desired-state knowledge.
- **Single source of truth** — extracted `resource.desiredForShield(querier, shieldID)` as the *one* definition of a shield's desired set; both the reconciler (`GetDesiredForShield`) and snapshot delivery route through it, so the predicate can't drift (this was the key design constraint — see the rejected trigger approaches).
- **`resource.BuildShieldSnapshot`** — in one tx with `SELECT … FOR UPDATE` on the shield row: read stored (gen, fp) → read desired set → hash the exact rows (`fingerprintDesired`, sorted by ID) → bump generation **only when the fingerprint changes**, else reuse. So generation tracks real content changes, is MVCC-consistent with the rows it stamps (later content ⇒ higher gen ⇒ shield resolves out-of-order deliveries), survives controller restarts, and **metadata/audit writes never churn it**.
- **`buildSnapshotMsg`** now reads `snap.Generation`; the misleading comment is corrected.
- **Verified** against real Postgres (`snapshot_integration_test.go`, guarded by `RESOURCE_TEST_DATABASE_URL`): lifecycle `first=1 → dedup=1 → metadata=1 → payload=2 → left=3`. `go build`/`go vet`/`go test -race` clean.

> **Design note — why not a trigger:** the first two attempts (unconditional `AFTER INSERT/UPDATE/DELETE` trigger, then a column-gated trigger) were rejected: the unconditional one churned generation on every audit/metadata write (defeating the shield's dedup), and *both* re-encoded the desired-state predicate in PL/pgSQL — a second source of truth that drifts from Go's `desiredForShield` the moment the rule changes. Option F keeps the rule in Go alone and lets SQL store generation as semantics-free bytes.

🟡 **Finding F12 (minor) — reconnect double-delivers APPLIES. → ✅ FIXED (2026-06-11).**
`pushPendingInstructions` sent the desired-state **snapshot** *and then* the full **pending instructions** for the same shield. Investigating the lifecycle showed it's not symmetric redundancy:
- **Applies were genuinely redundant.** `handle_snapshot` (`shield/src/resources.rs:398-484`) enforces the full desired set AND acks each resource (protected/failed), so a `protecting/apply` row completes via the snapshot alone. Re-sending it as an explicit `apply` made the shield rebuild the chain a *second* time and ack again.
- **Removes are NOT redundant.** The snapshot drops removed resources by **omission, with no ack** (`resources.rs:396-397`). The explicit `remove` instruction is what makes the shield emit the `unprotected` ack that `RecordAck` (`store.go:518-530`) uses to reap a `deleting` tombstone **immediately**; the Phase 3 state-report reconciler is only the slower backstop. So dropping removes would slow tombstone reaping on reconnect.

**Fix:** keep the snapshot (authoritative apply path) and filter the reconnect pending-send to `pending_action == 'remove'` only (`control_stream.go`). This matches the shield's documented contract — *snapshot = apply mechanism, explicit removes = remove mechanism* — and removes the redundant second rebuild + duplicate ack for every applied resource. `go build`/`go vet`/`go test -race` clean. (Delivery path has no unit coverage; logic-verified against the shield/RecordAck contract, not a live-stack run.)

🟡 **Finding F13 — the "batch" never batches on the hot path. → ✅ ANALYZED, not a defect (2026-06-11).**
`ResourceInstructionBatch.ShieldResources` is `map[shield]→[]instr`, but `PushInstruction` sends exactly one resource for one shield per resolver mutation. This is **inherent**, not a missed optimization: the GraphQL API is per-resource (`protectResource(id)` etc. — no bulk protect), so there's never more than one resource to batch online. The batch wire format is correctly shared with `pushPendingInstructions`, which *does* batch on reconnect. No change.

**Investigating it surfaced — and then dismissed — a hot-path "dual-delivery" (would-be F15).** Each mutation calls both `PushInstruction(row)` and `PushSnapshotForShield(...)` (`resource.resolvers.go:67-68,81-82,105-106`), and the connector live-forwards the snapshot to a connected shield (`agent_server.rs:122-134`), so a fresh protect rebuilds the shield's chain twice (apply instruction + snapshot). Looked like F12's hot-path mirror — but **dropping the incremental would be a bug**, because the two deliveries carry *different* semantics:
- **Snapshot** = "this is the desired set" — idempotent, content-deduped (F11), refreshes the connector cache for reboot-safety.
- **Incremental instruction** = "(re-)evaluate this resource **now**" — acts even when the desired *content* is unchanged.

Decisive case: **re-protecting a `failed` resource.** `MarkProtecting` allows `failed → protecting/apply` (`store.go:423`), but both states are in the desired set with identical payload, so the F11 fingerprint is unchanged → the snapshot's generation does NOT bump → the shield ignores it (`generation <= last`). Only the incremental `apply` makes the shield re-check the port and emit a fresh ack; without it, the retry-after-fixing-the-service flow hangs in `protecting`. So the incremental is load-bearing for force-evaluate intent, and the double rebuild on a fresh protect is the accepted cost of supporting both semantics. **No code change.**

🟡 **Finding F14 (minor / reliability edge) — residual of Finding 4: `c.send` was synchronous in the resolver. → ✅ FIXED (2026-06-11).**
`send()` held `sendMu` and called `stream.Send()` directly. Per gRPC-Go, `Send` blocks until the message reaches the transport; under HTTP/2 flow control a connector that stops reading fills the window and makes `Send` block until the stream context is cancelled. Because `PushInstruction`/`PushSnapshotForShield` run inline in the GraphQL resolver, a wedged connector could hang the admin's mutation — and `sendMu` being held meant every other message bound for that connector (heartbeat acks, ACL snapshots, reconcile resyncs) queued behind it.

**Fix — outbound mailbox + single writer goroutine (`control_stream.go`):**
- Each `connectorStreamClient` gets a buffered `outbound` channel (cap `connectorSendQueueSize = 128`); `send()` does a **non-blocking** enqueue and **fails fast** (returns error) when the mailbox is full, so a wedged connector can never stall a resolver or the reconciler.
- A single `runWriter(ctx)` goroutine is the *sole* caller of `stream.Send` (satisfying gRPC's one-concurrent-sender rule), started in `Control()` before any send. A blocking `Send` now blocks only that goroutine; it exits when the stream context is cancelled (handler returns) or `Send` fails (the recv loop also tears down).
- `sendMu` deleted — only the writer touches `Send`. All send sites (resolver pushes, pending replay, ACL snapshot, scan command, reconcile resync, initial ping) benefit transparently.
- Dropped-on-full messages are recovered by reconnect replay or the Phase 3 reconciler's drift resync (same backstop as the offline-safe path).

Verified: `go build`/`go vet`/`go test -race ./internal/connector` clean; new unit test `control_stream_test.go` asserts the fail-fast-when-full contract.

**Note (not a finding) — desired/pending/deleting query helpers trust their caller for tenant isolation.**
`GetDesiredForShield` / `GetPendingForShield` / `GetDeletingForShield` (`store.go`) scope by `shield_id` only, no `tenant_id`. Safe today: `handleResourceState` validates `shield ∈ (connector, tenant)` before reconciling (`reconcile.go:43-51`), `pushPendingInstructions` selects shields by the authenticated `connector_id`, and `ReapTombstone` *does* scope by tenant. A one-line contract comment would stop a future caller reusing them unscoped.

✅ **Correct:** offline-safe push at every entry point; snapshot-on-reconnect is the right fail-closed primitive (replace-semantics drops zombies + restores protected in one shot); hysteresis (2 drift reports before resync, 3 absent before reap) avoids acting on in-flight state; reconnect query correctly uses the shields-table `'deleted'` which is still live.

---

# Piece 5 — Connector Receives + Queues

**Status:** ✅ reviewed (2026-06-12) — F16/F17/F18 fixed; F19 accepted as deliberate best-effort (decision recorded)

`handle_controller_msg` (`control_stream.rs:244-265`) processes controller messages **sequentially**, fanning each batch out per-shield to `push_instructions` / `push_snapshot`. All per-shield state lives in one `ShieldMaps` behind a single `parking_lot::Mutex`.

**Files**
- `connector/src/control_stream.rs:244-265` — `handle_controller_msg` receive + per-shield dispatch (called from the controller-stream loop at `:183`)
- `connector/src/agent_server.rs:30-43` — `ShieldMaps` state struct
- `connector/src/agent_server.rs:95-132` — `push_instructions` (**F16/F17**)
- `connector/src/agent_server.rs:137-152` — `push_snapshot` (race-free counterpart — the model the fix mirrors)
- `connector/src/agent_server.rs:390-566` — shield-facing `control()`: connect-time snapshot replay + buffer drain (`:419-461`), live-forward select arms (`:530-547`), disconnect cleanup (`:556-560`)

**To review**
- [x] Race: shield connects between `get(tx)` check and buffer insert — **F16, was real → ✅ fixed.**
- [x] Buffer overwrite semantics (`insert` replaces prior buffered vec) — **F17, was real → ✅ fixed (append).**
- [x] `tokio::spawn` per-push ordering guarantees — **F18, real → ✅ fixed (in-order `try_send`; single-producer invariant documented).**
- [x] Channel-closed handling (`warn` + break) — **F19, drops-not-rebuffers → ✅ accepted as deliberate best-effort (decision recorded in code + doc).**

**Notes**

The contrast that frames the whole piece: **`push_snapshot` is race-free; `push_instructions` was not.** `push_snapshot` (`:137-152`) updates the cache AND reads `snapshot_txs` under *one* lock, and the connect handler *replays from cache* (never removes it), so a concurrent connect can't strand anything (worst case is a double-send the shield's generation gate dedups). `push_instructions` lacked that discipline.

🔴 **Finding F16 — connect-vs-buffer TOCTOU strands instructions. → ✅ FIXED (2026-06-12).**
`push_instructions` read `instruction_txs` under one lock, **released it**, then (offline branch) re-took the lock to `insert` the buffer — two separate critical sections. Interleaving: push reads txs→None (shield offline) and releases; the shield connects (inserts its `tx` at `:405`, then its spawned drain removes the *still-empty* buffer at `:447` and goes live on `instr_rx`); push then writes the buffer. The instruction sits in `resource_instructions` **unsent until the next reconnect**. The connect handler's own invariant (insert tx *before* draining) is correct; the bug was purely that push let a write land *after* the drain.
**Fix:** do the tx-check and the buffer write in **one** lock scope (`agent_server.rs:107-121`). If push observes no tx it buffers under that same lock, so it either sees the tx (→ live send) or the connect handler's drain (ordered after its tx-insert) is guaranteed to pick the buffer up.

🔴 **Finding F17 — buffer overwrite drops instructions. → ✅ FIXED (2026-06-12).**
The offline branch used `resource_instructions.insert(shield_id, instructions)` — a **replace**. Two pushes while a shield was offline → the second clobbered the first. **Fix:** `entry(shield_id).or_default().extend(instructions)` — append. Regression-guarded by `agent_server.rs` unit test `offline_pushes_append_not_overwrite` (constructs a network-free registry via `connect_lazy`, asserts two offline pushes accumulate in order). `cargo build` + `cargo test` clean.

🔴 **Finding F18 — `tokio::spawn`-per-push lost cross-push ordering. → ✅ FIXED (2026-06-12).**
Each online push spawned a detached task to drain into the channel; two pushes for the same shield raced, so `apply X` then `remove X` could deliver reversed → shield ends up enforcing X. Order was preserved *within* a push (the `for` loop) but not *across* pushes.
**Fix:** drop the spawn; enqueue in arrival order with non-blocking `tx.try_send` (`agent_server.rs:push_instructions`). The per-shield ordered forwarder already exists — `instr_rx` is FIFO and the `control()` `select!` loop is its single drainer — so everything *from the channel onward* was already ordered; the bug was only in *feeding* it. A single sequential producer + FIFO channel + single drainer now preserves end-to-end order.
**The load-bearing invariant (made explicit, not assumed):** ordering rests on **exactly one producer per shield queue**, NOT on `try_send` itself. `mpsc` is multi-producer and will silently interleave concurrent producers; `try_send` only avoids reordering *a single* producer's enqueues. Today that single producer is `handle_controller_msg` (a sequential loop). This is now documented as an INVARIANT on the `instruction_txs` field, with the rule that any future sharded/parallel controller processing **must partition by `shield_id`** (ordering key == concurrency key) or ordering breaks silently. Deeper point recorded too: deltas (apply/remove) require a serialization point; the versioned snapshot is the order-independent durable authority, so the instruction channel is explicitly the best-effort fast path.
**Backpressure choice:** `try_send` over blocking `send` — awaiting a full channel would stall the dispatcher and head-of-line-block every other shield + acks on the shared controller stream. On Full we stop and let the snapshot/reconciler repair; channel cap bumped `32 → 256` (`SHIELD_INSTRUCTION_QUEUE_CAP`) so Full is unreachable at realistic rates. Verified: `cargo build` + `cargo test` clean; new unit test `online_pushes_preserve_arrival_order` (flaky under the old spawn, deterministic now).

🟢 **Finding F19 — send-failure drops, doesn't re-buffer. → ✅ ACCEPTED as deliberate best-effort (decision recorded 2026-06-12; no behavior change).**
On the online path, if `try_send` fails — `Closed` (shield disconnected mid-push) or `Full` (channel backed up) — we drop the rest of the batch rather than re-buffering. This is now an explicit design decision, documented in code at `push_instructions`.

**Why not a bug:** F19 is a latency edge, not a correctness gap. `Full` is effectively unreachable (cap 256 vs one-instruction-per-mutation rates). A `Closed` drop is recovered by the snapshot — `apply` via the cached snapshot replayed on reconnect (`:419-442`), `remove` via snapshot-omission + the Phase 3 reaper. So a dropped delta self-heals; the only cost is a rare, slightly-slower reap when a `remove` is dropped during a mid-push disconnect.

**Options considered:**
- **A — accept best-effort (chosen).** Zero new code; correctness already guaranteed by the snapshot, and recovery is already wired (unconditional snapshot replay on reconnect).
- **B — re-buffer on `Closed` only.** Marginal latency win, but reopens the **F16 buffer TOCTOU race** we just hardened, and needs split `Closed`≠`Full` handling — bad risk/value for something the snapshot already recovers.
- **C — delete the instruction buffer entirely, rely 100% on the snapshot.** Intellectually cleanest (F16/F17 existed *because* the buffer exists), but loses fast offline removes and is a bigger change deserving its own review. Parked as a candidate.
- **D — per-shield sequence numbers + consumer reorder.** Solves a problem the snapshot's versioning already solves; overkill.

**Decision:** A. Instruction delivery is intentionally the best-effort fast path; the versioned snapshot is the durable authority. **Revisit trigger:** if telemetry shows reconnect-storm reap/apply latency that operators notice, do B (Closed-only re-buffer with F16 discipline), or scope C as a deliberate simplification.

**Architectural note:** post-ADR-004 the instruction buffer is largely redundant with the snapshot cache for **applies** (the cache re-protects the full desired set on reconnect, latest-wins). Its remaining value is **fast removes** (explicit remove → `unprotected` ack → immediate reap) — the same logic as F12 on the controller side — so it's worth keeping and fixing, not deleting. Disconnect cleanup (`:556-560`) correctly preserves the snapshot cache as the recovery anchor while dropping the live tx entries.

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
5. ~~**Push error discarded; possible stuck `protecting`**~~ (Piece 2 Finding 4 / Piece 3 Finding 6) — ✅ **Resolved (Piece 4):** the ADR-004 Phase 3 closed-loop reconciler (`reconcile.go`) detects `missing` drift and re-pushes the snapshot, so an online-but-send-failed apply self-heals without waiting for a reconnect. The latency-coupling residual (synchronous `c.send` in the resolver) is also now ✅ fixed — Finding F14 moved sends to a per-connector writer goroutine with a fail-fast mailbox.
6. ~~**Delete of `protected` orphans shield firewall rule** + **fail-open after reboot** (6b) + **stuck `protecting`** (6)~~ — ✅ **Resolved (Piece 4)** by ADR-004 Phase 2/3, now landed: tombstone delete + reap, desired-state snapshot on reconnect (restores `protected`/`failed` after reboot), closed-loop reconciler. **Still open:** protect/unprotect shield-active **asymmetry** (Piece 3 Finding 7 — unprotect against a dead shield can stick); decision deferred. Invariant held: *never destroy intent until effect is observably confirmed.*
8. **Reconciler held a global mutex across DB + network I/O** (Piece 4 Finding F10) — ✅ **FIXED (2026-06-11):** lock narrowed to the in-memory hysteresis maps only; all I/O moved outside it.
9. ~~**Snapshot `Generation` is non-monotonic wall-clock**~~ (Piece 4 Finding F11) — ✅ **FIXED (2026-06-11):** replaced wall-clock millis with a per-shield monotonic counter bumped only on desired-content change (fingerprint over `desiredForShield`'s rows), stored as opaque columns (migration 018). Desired-state rule stays single-sourced in Go — no trigger, no SQL predicate. Verified end-to-end against real Postgres.
7. **Vestigial soft-delete + stale index** (Piece 3, Findings 8/9) — `deleted_at`/`'deleted'` scaffolding is dead (hard-delete); `idx_resources_managing` matches zero rows post-rename. **Folded into ADR-004 (Phase 1 index fix, Phase 4 cleanup).**

---

## Proto Reference

- `proto/shield/v1/shield.proto:44-59` — `ResourceInstruction`, `ResourceAck`
- `proto/shield/v1/shield.proto:82-101` — `ShieldControlMessage` oneof
- `proto/connector/v1/connector.proto:97-100` — `ResourceInstructionBatch`
- `proto/connector/v1/connector.proto:70-90` — `ConnectorControlMessage` oneof
