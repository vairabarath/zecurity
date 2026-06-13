---
type: planning
status: active
tags:
  - session-log
  - history
---

# Session Log

---

## 2026-05-14 — Codex (GPT-5) — Connector Lint Diagnostics

**What was done:**
- Added `controller/internal/connector/doc.go` with a package comment to satisfy ST1000 for the connector package.
- Removed unused `pingClient` helper from `control_stream.go`.
- Replaced remaining `interface{}` signatures in the connector package with `any`.

**Key decisions:**
- Removed the unreferenced keepalive helper instead of suppressing `unusedfunc`, since no production code or tests call it.

**What's next:**
- Re-run connector package tests in an environment with Docker access; local sandbox verification cannot start the Valkey test container.

---

## 2026-04-30 — Codex (GPT-5) — Invitation Bug Fixes

**What was done:**
- Fixed invitation acceptance email validation: access JWTs now include `email`, auth middleware copies it into `TenantContext`, and `AcceptInvitation` requires matching token + workspace + email before consuming an invite or activating `workspace_members`.
- Updated client token exchange to pass the session email into issued access tokens.
- Rebuilt `admin/src/pages/ClientInstall.tsx` into the member install page: workspace name, signed-in email, Linux release links, copyable install/setup commands, and enrolled devices.
- Documented fixes in Sprint 7 phase docs/path and marked `.zecurity-obs/Sprint8.5/Invitation-Bugs-Remaining.md` done.

**Key decisions:**
- Refresh preserves email from the old JWT and falls back to querying `users.email` only when an older token lacks the claim and the auth service has a DB pool.
- `/client-install` remains under the normal authenticated shell because the route gate is auth-only, not admin-only.

**What's next:**
- Run a browser-level invite acceptance smoke test with a real member account once OAuth and SMTP/dev invite links are available.

---

## 2026-04-30 — M4 (vairabarath), Sprint 8 + Sprint 8.5 — Connector ACL + Client Daemon Foundation

**What was done:**

- **Sprint 8 Phase C (Connector ACL Snapshot Handling):**
  - `connector/src/policy/mod.rs` (new): `PolicyCache` with `RwLock<Option<AclSnapshot>>`, `is_allowed()`, `resolve_resource()`, 8 unit tests covering all default-deny cases.
  - `connector/src/control_stream.rs`: added `CBody::AclSnapshot` heartbeat arm to store snapshot in `PolicyCache`.
  - `connector/src/main.rs`: wired `Arc<PolicyCache>` into `run_control_stream`.
  - M4-C1/C2/C3 checkboxes marked done in Sprint 8 path.md.

- **Sprint 8.5 Phase A (Daemon Scaffold + IPC):**
  - `client/src/daemon.rs` (new): daemon process, Unix socket accept loop, 7 IPC handlers (`Status`, `Shutdown`, `LoadState`, `GetToken`, `PostLoginState`, `Up`, `Down`), `sd_notify_ready` for systemd `Type=notify`.
  - `client/src/ipc.rs` (new): `IpcRequest`/`IpcResponse` types, `SO_PEERCRED` same-user enforcement, `send_ipc`, `ensure_daemon_and_send`.
  - `client/src/config.rs`: added `Clone` to `ClientConf` derive.
  - `client/src/main.rs`: added hidden `daemon` subcommand.
  - `client/zecurity-client.service`: `daemon` subcommand, `Type=notify`, `CAP_NET_ADMIN`, `RuntimeDirectory=zecurity-client`.
  - `client/Cargo.toml`: added `tracing` + `tracing-subscriber`.

- **Sprint 8.5 Phase B (Command Refactor):**
  - `client/src/cmd/login.rs`: sends `PostLoginState` IPC after OAuth flow; CLI no longer calls `save_workspace_state` directly.
  - `client/src/cmd/status.rs`: queries daemon `Status` IPC; prints email, cert expiry, device_id, SPIFFE ID, ACL version.
  - `client/src/cmd/logout.rs`: best-effort `Shutdown` IPC before `clear_workspace_state`.
  - `client/src/cmd/invite.rs`: tries `GetToken` IPC first; only falls back to full OAuth if no active session.
  - Fixed: `PostLoginState` handler now reconstructs `LoginResult` and calls `StoredWorkspaceState::from_login` to correctly decode JWT claims (workspace_id, user_id, role).

- **Sprint 8.5 Phase C (ACL Runtime Fetch):**
  - `client/src/runtime.rs`: added `acl_snapshot: Option<AclSnapshot>` to `RuntimeState`.
  - `client/src/daemon.rs`: `fetch_and_store_acl` spawned after `PostLoginState` and on daemon startup; never reverts snapshot to `None` on transient failure; Status response exposes `acl_snapshot_version`.

**Key decisions:**
- `ensure_daemon_and_send`: tries IPC → `systemctl start` → 500ms sleep → retry → fail closed.
- Snapshot replacement rule: keep existing snapshot on fetch failure (never revert to `None`).
- `from_login` JWT decoding preserved by reconstructing `LoginResult` in daemon rather than exposing `decode_claims`.

**Merged as:** `member_04/sprint_08` → `main` (PR #25)

**What's next:**
- Sprint 9 RDE data plane: `device_tunnel.rs` reads `acl_snapshot` from daemon memory for per-connection enforcement.

---

## 2026-04-30 — M1 (Yogesh), Sprint 8 — Groups Policy UI + Bug Fixes

**What was done:**

- Fixed refresh token TTL not sliding: `controller/internal/auth/refresh.go` — added `SetRefreshToken` call after issuing new access token so Redis TTL resets on every refresh (active users were being logged out after 7 days).
- Fixed Apollo Client v4 HTTP 401 not caught: `admin/src/apollo/links/error.ts` — extended `isUnauthorizedError` to handle network-level 401 in addition to GraphQL-level `UNAUTHORIZED` errors.
- Fixed `Makefile` GQLGEN_VERSION mismatch: `v0.17.89` → `v0.17.90` to match `controller/go.mod`.
- Fixed `admin/codegen.yml` missing `policy.graphqls` schema source — group types were not being generated.
- Completed all M1-D1 through M1-D5 Sprint 8 tasks: Groups page, GroupDetail (Members + Resources tabs), Resources page groups column, all GraphQL queries/mutations.
- Added `users: [User!]!` GraphQL query (M2's zone) + resolver in `schema.resolvers.go` — needed for the Add Member user picker in GroupDetail; M2's schema didn't include it.
- Fixed M3's broken controller build: added `groupRowToGQL`, `loadGroup`, `loadResourceWithGroups` to `controller/graph/resolvers/policy_helpers.go` — M3's `policy.resolvers.go` referenced these but they were never implemented.

**Key decisions:**
- `loadGroup` and `loadResourceWithGroups` defined on `*Resolver` (not `*queryResolver`/`*mutationResolver`) because both resolver types call `loadGroup` after group mutations.
- Branched to `member_01/sprint_08` from main and cherry-picked auth fixes to keep work clean from stale `member_03/sprint_7`.

**What's next:**
- M4-C: Connector ACL snapshot receive/store (M4's work, Sprint 8 Phase C).
- Final verification checklist in `path.md` once M4-C lands.

Most recent first. Every agent appends an entry after their session.

---

## 2026-04-28 — Claude Code — TL Review + UDP Discovery Planning

**What was done:**
- Fixed `connector/src/discovery/scan.rs` — Phase 2 probe loop used `while let Some(Ok((ip, port, true, svc)))` which exits on first closed-port result (Rust literal pattern). Changed to `while let Some(result)` + inner `if let Ok`. Root cause of scan always returning 0 results.
- Fixed `shield/src/discovery.rs` — IPv6 wildcard `::` was reported as `bound_ip` causing PromoteDiscoveredService to fail. Added `normalize_bound_ip()` with LAN IP detection.
- Fixed `shield/src/discovery.rs` — `parse_proc_ipv6` used `word.to_be().to_le_bytes()` double-conversion garbling `::1` (loopback) to `::100:0`. Changed to `word.to_le_bytes()`.
- Released both connector and shield at `v1.0.4` — deleted and re-pushed `shield-v1.0.4` after the IPv6 parse fix landed.
- Fixed `admin/src/components/ScanModal.tsx` — collapsed 6-column results table to 3 (Host:Port, Service, + icon). Removed VIA (UUID) and Protocol columns. Fixed scroll: table wrapper missing `flex-1 min-h-0`, last row was clipped.
- Conducted full Sprint 6 flow review for TL: data flow diagrams, file references, correctness points, E2E checklist.
- Planned Phase E2 — UDP local discovery for Shield. Created `Sprint6/Member4-Rust-Shield/Phase3-UDP-Discovery.md`. Updated `Sprint6/path.md` with Phase E2 checkboxes.

**Key decisions:**
- UDP discovery added to Shield local scan (`/proc/net/udp`) because resource UI already supports UDP resources and neither TCP nor UDP has data-plane tunneling yet — they are on equal footing until Sprint 10.
- Connector network scanner stays TCP-only — UDP network scanning (no handshake, ICMP-dependent) is a fundamentally different problem deferred to Sprint 10.
- PID/process name correlation (inode → `/proc/<pid>/fd/`) rejected for main daemon — requires `CAP_DAC_READ_SEARCH` + `CAP_SYS_PTRACE` on a non-root Shield user. Option 1 (accept limitation, port lookup covers 90%) recommended.

**What's next:**
- Implement Phase E2 (M4): extend `discovery.rs` with UDP proc parsing — 4 steps, one file only.
- Run Sprint 6 E2E verification checklist (all items unchecked in path.md).
- Sprint 7 planning: Client enrollment (end-user device cert + SPIFFE identity).

---

## 2026-04-27 — Claude Code — M1 Phase 2 (Connector Network Scan UI)

**What was done:**
- Added `TriggerScan` mutation and `GetScanResults` query to admin GraphQL ops; ran `npm run codegen`
- New `admin/src/components/ScanModal.tsx` — two-step (form → polled results); `parseTargets`/`parsePorts` helpers; ≤16 ports + 1–65535 validation; `startPolling(3000)` with 60s hard stop via `setTimeout` + `stopPolling()`
- Modified `admin/src/pages/RemoteNetworks.tsx` — per-network connector selector + "Scan Network" button mounting `ScanModal` with `{connectorId, networkId, connectorName}`
- Modified `admin/src/components/CreateResourceModal.tsx` — accepts a new `defaults` prop and prefills name/host/protocol/port from scan results
- Modified `admin/src/pages/Resources.tsx` — accepts route-state defaults so navigating from `ScanModal` opens `CreateResourceModal` prefilled
- `npm run codegen && npm run build` clean (2411 modules, 0 errors)

**Key decisions:**
- Polling stop uses both `pollingExpired` state + `stopPolling()` so React effect cleanup runs even if user closes the modal mid-scan
- 512-target cap is enforced server-side only (CIDRs aren't expanded in the browser); ScanModal validates port count and presence, surfaces server errors inline
- Prefill goes through Resources page route state rather than ScanModal owning a CreateResourceModal — keeps ScanModal focused on scan UX and reuses the existing Create flow

**What's next:**
- End-to-end smoke test now possible (M3 backend landed in PR #7)
- Sprint 6 M1 frontend complete (Phase 1 shipped, Phase 2 ready to merge)

---

## 2026-04-27 — Claude Code — M1 Phase 1 (Shield Discovery Tab)

**What was done:**
- Added `discovery.graphqls` to `admin/codegen.yml` schema list
- Added `GetDiscoveredServices` query and `PromoteDiscoveredService` mutation to admin GraphQL ops
- Ran `npm run codegen` — typed Apollo docs generated
- New `admin/src/components/DiscoveredServicesPanel.tsx` — polled table (30s), empty/loading states, Promote button per row
- New `admin/src/components/PromoteServiceModal.tsx` — confirmation modal calling `PromoteDiscoveredService`, success toast, error inline
- Modified `admin/src/pages/Shields.tsx` — per-row chevron toggle to expand/collapse `DiscoveredServicesPanel`; added 36px column for the toggle
- `npm run build` passes clean

**Key decisions:**
- Used `cache-and-network` fetch policy on the query so the panel paints from cache while Apollo refetches
- Toggle state held as a `Set<string>` keyed by shield id rather than per-row state, to keep Shields.tsx flat
- Modal calls `refetchQueries: GetAllResourcesDocument` on success so the Resources page reflects the new pending resource immediately

**What's next:**
- M1 Phase 2 (Scan UI on RemoteNetworks page) — `TriggerScan` + `GetScanResults` ops, `ScanModal` component
- Backend wiring: M3 still needs to implement the `getDiscoveredServices` / `promoteDiscoveredService` resolvers (currently panic stubs in `controller/graph/resolvers/discovery.resolvers.go`); UI cannot be exercised end-to-end until that lands

---

## 2026-04-23 — Codex (GPT-5) — Admin Design Handoff Implementation

**What was done:**
- Re-themed the admin app to the dark mint handoff design system by replacing the shared CSS tokens, shell layout, navigation, and header treatment in `admin/src/index.css` and the layout components
- Rebuilt the auth and signup flow screens around reusable handoff-style auth primitives so `/login`, `/signup`, `/signup/workspace`, and `/signup/auth` now match the exported design language
- Restyled the main admin surfaces (`Dashboard`, `RemoteNetworks`, `Connectors`, `Shields`, `AllConnectors`, `AllShields`, `Resources`) while preserving existing GraphQL data flows, install actions, and resource management behavior
- Added `admin/src/lib/console.tsx` and `admin/src/components/auth/AuthLayout.tsx` to centralize status pills, relative-time formatting, empty states, and auth layout primitives
- Ran `cd admin && npm run build` successfully after fixing TypeScript issues introduced during the redesign

**Key decisions:**
- Kept the implementation inside the existing route/data architecture instead of porting the prototype files literally, so the result remains compatible with the current GraphQL hooks and route structure
- Avoided editing already-dirty user-touched files (`InstallCommandModal.tsx`, `ConnectorDetail.tsx`, `ShieldDetail.tsx`) to prevent overwriting unrelated in-progress work
- Applied the handoff primarily to the shared shell plus the main dashboard/list/auth surfaces first, since those are the screens directly represented by the exported prototype bundle

**What's next:**
- Extend the same design language into `RemoteNetworkDetail.tsx`, `ConnectorDetail.tsx`, and `ShieldDetail.tsx` once the existing in-progress local edits on those pages are settled
- Consider route-level code splitting in the admin app if the current Vite chunk-size warning needs to be addressed

---

## 2026-04-22 — Claude Code (Sonnet 4.6) — M1 Sprint 5 (Hard Delete Fix)

**Member:** M1 (Frontend)
**Branch:** `sprint5-member1`

**What was done:**
- Changed `SoftDelete` in `controller/internal/resource/store.go` from `UPDATE ... SET deleted_at` to `DELETE FROM resources` — hard delete so the `(shield_id, name)` unique constraint is immediately freed and the same name can be reused right after deletion

**Key decisions:**
- Hard delete is correct here — soft delete was causing duplicate key errors when recreating a resource with the same name; since nftables state is managed by Shield heartbeat acks (not by the DB row), hard delete is safe

**What's next:**
- Full integration test: create → protect → unprotect → delete → recreate with same name

---

## 2026-04-17 — Claude Code (Opus 4) — M1 Phase 4

**Member:** M1 (Frontend)
**Phase:** Phase 4 — RemoteNetworks NetworkHealth + Shield Count — **DONE**
**Branch:** `sprint-4-m1`

**What was done:**
- Extended `GetRemoteNetworks` query with `networkHealth` and `shields { id, status }` fields; re-ran `npm run codegen`
- `RemoteNetworks.tsx`: added `healthConfig` map (Online/Degraded/Offline dot + label classes); rendered pulsing health dot + label inside each card's name column
- Updated count line to spec format: `"X / Y connectors active · Z shields active"` (active counts derived by filtering on `ConnectorStatus.Active` / `ShieldStatus.Active`)
- Tightened delete-button guard: now shown only when **both** `connectorCount === 0` AND `shieldCount === 0` (previously only connectors), preventing 4xx when shields exist
- Sidebar: confirmed Phase 1 link in place (`Shield` icon → `/remote-networks`). Per-route contextual target deferred (see decisions)
- `cd admin && npm run build` → 0 errors

**Key decisions:**
- **Sidebar stays global-static** — making it route-aware (useLocation + param parse) is unnecessary complexity for Sprint 4; per-network drill-through already happens through RemoteNetworks card click. Same call as Phase 1.
- **Delete guard now also checks shields** — matches the server-side constraint that a network can't be deleted while resources exist. Previously only blocked on connectors; Phase 4 adds shield coverage to prevent a UX trap.
- Used Tailwind `animate-pulse` for the health dot. Degrades gracefully to solid if class unavailable.

**What's next:**
- Commit + push `sprint-4-m1`; open PR to main
- Sprint 4 frontend now complete (per phase-4 `unlocks` field)
---

## 2026-04-22 — Claude Code (Sonnet 4.6) — M1 Sprint 5 (Edit Resource + Store Fix)

**Member:** M1 (Frontend)
**Branch:** `sprint5-member1`

**What was done:**
- Pulled latest `origin/main` twice — picked up M4 Shield nftables work and Cargo.toml bumps
- Fixed `AutoMatchShield` in `controller/internal/resource/store.go` — removed invalid `AND deleted_at IS NULL` on the `shields` table (shields have no such column; filtered by `status NOT IN ('revoked','deleted')` instead)
- Added three-dot (`MoreHorizontal`) Actions dropdown to Resources table — replaced inline buttons with a `DropdownMenu` per row; options: Edit, Protect, Unprotect, Delete (with separator + red style); spinner shown for in-progress states
- Renamed last column header from empty string to `"Actions"`
- Created `admin/src/components/EditResourceModal.tsx` — pre-populated form with Remote Network, Name, Description, Protocol, Port From/To; calls `updateResource` mutation
- Added `UpdateResourceInput` + `updateResource` mutation to `controller/graph/resource.graphqls`
- Added `UpdateResource` mutation to `admin/src/graphql/mutations.graphql`
- Re-ran `make gqlgen` → gqlgen generated stub resolver; re-ran `npm run codegen` → TS types updated
- Implemented `resource.Update()` in `store.go` — dynamic SET clause (only non-nil fields written), returns updated row
- Implemented `UpdateResource` resolver in `resource.resolvers.go` — wired to `resource.Update()` with tenant context
- Updated `Phase1-Resources-Page.md` with full record of what was built and M3 action note
- `cd admin && npm run build` and `cd controller && go build ./...` both pass

**Key decisions:**
- Edit allowed on all non-deleted resources (not just `pending`) — more flexible for name/description changes
- `Update()` uses dynamic SET via `strings.Builder` — only non-nil fields written, safe for partial updates
- Host IP intentionally excluded from editable fields — it's tied to shield auto-match and cannot change safely

**What's next:**
- Integration test Edit modal end-to-end once controller is running
- M4 nftables `failed to apply resource_protect chain` error needs fix — table `inet zecurity` may not exist on shield restart; M4 should add `add table inet zecurity` guard before chain flush in `resources.rs`

---

## 2026-04-22 — Claude Code (Sonnet 4.6) — M4 Sprint 5 Phase D (Resources + Heartbeat Ack)

**Member:** M4 (Rust — Shield)
**Phases:** Phase D (M4-D1 → M4-D4) — **DONE**

**What was done:**
- Created `shield/src/resources.rs` — `ActiveResource`, `SharedResourceState`, `validate_host`, `check_port`, `apply_nftables` (flush + atomic rebuild of `chain resource_protect` via nftables crate), `run_health_check_loop` (30s ticker, replaces acks per resource_id, no duplicates)
- Modified `shield/src/config.rs` — added `resource_check_interval_secs: u64` (default 30) with figment serde default
- Modified `shield/src/main.rs` — registered `mod resources`, created `Arc<SharedResourceState>`, spawned health check loop, wired `resource_state` into `heartbeat::run`
- Modified `shield/src/heartbeat.rs` — added `Arc<SharedResourceState>` param to `run`; drains pending acks into `HeartbeatRequest.resource_acks` each tick; processes `HeartbeatResponse.resources` via `handle_apply` / `handle_remove`; `handle_apply` validates host, upserts active list, rebuilds nftables, pushes `protecting` ack; `handle_remove` drops from active list, rebuilds nftables, pushes `removed` ack; `push_ack` replaces existing ack for same resource_id
- `cargo build --manifest-path shield/Cargo.toml` passes (warnings only, all pre-existing)
- Checked M4-D1 → M4-D4 in `Sprint5/path.md`; set Phase 1 and Phase 2 status to `done`

**Key decisions:**
- Used `nftables` crate (already a dependency from `network.rs`) instead of shelling out — consistent with existing code, typed rule construction, no string injection risk
- `resource_protect` chain at priority 10 (after `input` at priority 0) — lo and connector traffic already accepted by input chain before resource_protect fires; LAN traffic falls through to the drop rules
- Port range expressed as `Expression::Range` for multi-port, `Expression::Number` for single-port
- "apply" action upserts (replace-if-present) so re-delivered instructions are idempotent
- Phase 1 build gate achieved by adding `resource_acks: vec![]` placeholder to heartbeat.rs, replaced by real drain logic in Phase 2

**What's next:**
- M4 Sprint 5 work is complete — integration testing once M1 frontend and M3 connector relay are also merged
- Run full integration checklist from `Sprint5/path.md` once all phases land

---

## 2026-04-17 — Kiro — Member 4 (Sprint 4 Phases 1–3)

**What was done:**
- Pulled latest `origin/main` — picked up M2 (shield proto + service), M3 (DB schema + GraphQL resolvers + Goodbye RPC), M1 (Shields page + GraphQL ops + codegen)
- Merged `origin/main` into `member_4` branch (resolved session log conflict)
- **Phase 1 (Crate Scaffold)** — created `shield/Cargo.toml`, `shield/build.rs`, `shield/Cross.toml`, `shield/Dockerfile`, `shield/src/main.rs` stub; `cargo build` passes
- **Phase 2 (Core Modules)** — created `shield/src/appmeta.rs`, `config.rs`, `crypto.rs`, `tls.rs`, `util.rs`, `types.rs`; full `main.rs` startup flow with SIGTERM handler; `cargo build` passes
- **Phase 3 (Enrollment)** — created `shield/src/enrollment.rs` (12-step flow: JWT parse → CA fetch → fingerprint verify → keygen → CSR → gRPC Enroll → save certs + state.json → config cleanup); wired into `main.rs`; `cargo build` passes
- Marked M4-G1–G4, M4-H1–H6, M4-I1 ✅ in `path.md`; set Phase 1/2/3 status to `done`
- Added `shield/target/` to `.gitignore`; removed build cache from tracking

**Key decisions:**
- `ShieldState` moved to `types.rs` (not `main.rs`) to avoid circular imports between `main.rs` and `enrollment.rs`
- `time` crate added to `Cargo.toml` with `formatting + macros` features for RFC 3339 timestamps
- `tonic_prost_build::configure()` used in `build.rs` — matches the tonic-prost split in this project
- Enrollment uses plain HTTP for CA fetch + fingerprint verification for MITM detection (same pattern as connector)
- `network::setup()` stubbed with a warning — Phase K will implement it

**What's next:**
- Phase J: `heartbeat.rs` + `renewal.rs` (mTLS heartbeat loop to connector :9091)
- Phase K: `network.rs` (zecurity0 TUN interface + nftables)
- Phase L: `updater.rs` + systemd units + install script
- Phase M: CI workflow + `connector/src/main.rs` wiring

---

## 2026-04-17 — Claude Code (Sonnet 4.6) — M3 Phases 2–4

**What was done:**
- **Phase 2 — GraphQL Resolvers:**
  - Added `shield.graphqls` to `gqlgen.yml` and ran codegen — generated `Shield`, `ShieldToken`, `NetworkHealth` types
  - Added `Service` interface to `internal/shield/config.go`
  - Added `ShieldSvc shield.Service` to `Resolver` struct
  - Implemented `GenerateShieldToken`, `RevokeShield`, `DeleteShield` mutations + `Shields`, `Shield` queries in `shield.resolvers.go`
  - Added `scanShield`, `loadShields`, `computeNetworkHealth` helpers to `helpers.go`
  - `RemoteNetworks` and `RemoteNetwork` now populate `NetworkHealth` and `Shields` inline
  - Fixed `connector/src/heartbeat.rs`: added `shields: vec![]` to `HeartbeatRequest`
- **Phase 3 — Connector Goodbye RPC:** Created `controller/internal/connector/goodbye.go`
- **Phase 4 — Connector Heartbeat Shield Processing:** Modified `heartbeat.go` to process `req.Shields`

**Key decisions:**
- `NetworkHealth` and `Shields` are direct struct fields populated inline during queries
- Merge conflict in `shield.resolvers.go` resolved by keeping full implementation over M2's codegen panic stubs

**What's next:**
- Phase 5 (`connector/src/agent_server.rs`) — waiting on M4 to confirm `ShieldServer::new()` API signature

---

## 2026-04-17 — Kiro — Member 4

---

## 2026-04-17 — Codex (GPT-5) — M3 Phase 1

**What was done:**
- Created `controller/migrations/003_shield_schema.sql` with the `shields` table and Sprint 4 indexes
- Added `controller/graph/shield.graphqls` with `Shield`, `ShieldStatus`, `ShieldToken`, and Shield query/mutation schema
- Updated `controller/graph/connector.graphqls` with `NetworkHealth` plus `networkHealth` and `shields` on `RemoteNetwork`
- Marked M3 Day 1 items complete in `Sprint4/path.md` and set the Phase 1 task note status to `done`

**Key decisions:**
- Kept `interface_addr` unique per tenant via a partial unique index so unassigned shields can coexist until enrollment
- Left Shield data modeled in GraphQL at the schema layer first; resolver implementation stays in M3 Phase 2

**What's next:**
- Wait for M2 `token.go` support so `GenerateShieldToken` can call into the Shield service
- Then implement M3 Phase 2 resolvers in `controller/graph/resolvers/shield.resolvers.go` and `connector.resolvers.go`

---

## 2026-04-17 — Claude Code (Opus 4) — M1 Sprint 4 Phase 1

**Member:** M1 (Frontend)
**Phase:** Phase 1 — Layout & Routing Scaffold — **DONE**
**Branch / commit:** `sprint-4-m1` @ `deb908d` (pushed to origin)

**What was done:**
- Created new branch `sprint-4-m1` off `main`
- Scaffolded `admin/src/pages/Shields.tsx` — breadcrumb, header, "Add Shield" placeholder button, 4-row skeleton loading state, empty state with CTA, full row/table layout ready for data, status config matching spec colors (PENDING gray / ACTIVE emerald / DISCONNECTED amber / REVOKED red)
- Added route `/remote-networks/:id/shields` in `admin/src/App.tsx`
- Added "Shields" nav entry in `admin/src/components/layout/Sidebar.tsx` under Infrastructure → Connectors (points to `/remote-networks` — sidebar has no per-network context; deep-link comes in Phase 4)
- Build check: `cd admin && npm run build` — 0 new errors from Phase 1 changes (4 pre-existing `ConnectorDetail.tsx` errors for missing `publicIp`/`certNotAfter`/`createdAt` fields on `GetConnector` query are unrelated to M1 Phase 1 — flag to M3 as a separate task)

**Decisions:**
- Sidebar "Shields" target is `/remote-networks` (not `/shields`) because there's no AllShields global page in Sprint 4 scope. Matches the existing sidebar ergonomics (user picks network → deep-links).
- Kept `showInstall` state as a placeholder (`const [, setShowInstall] = useState(false)`) so the "Add Shield" button click still does *something* — full `InstallCommandModal` wiring is Phase 3 scope.

**What's next:**
- M1 Phase 2 blocked on Day 1 deliverables from M2 + M3 (shield.proto + connector.proto changes + graph schemas) followed by `buf generate`, `go generate ./graph/...`, and `cd admin && npm run codegen`.
- M1 Phase 4 (RemoteNetworks NetworkHealth + sidebar/per-network Shields link) can proceed in parallel with Phase 3 once codegen has run.
- Open a PR `sprint-4-m1 → main` when ready for review.

**Unresolved follow-up:**
- Pre-existing `ConnectorDetail.tsx` type errors — owner likely M3 (GraphQL schema) or previous M1 work. Separate issue.

---
## 2026-04-16 — Claude Code (Sonnet 4.6) — Sprint 4 Planning

**What was done:**
- Deep-read `sprint4-shield-plan.md` (full 1700-line spec)
- Created `.zecurity-obs/Sprint4/` folder with complete execution documentation:
  - `path.md` — master dependency map, conflict zones, ordered checklist with checkboxes, dependency graph
  - `Member1-Frontend/` — 4 phase files (Layout, GraphQL ops, Shields page, RemoteNetworks+Sidebar)
  - `Member2-Go-Proto-Shield/` — 4 phase files (Proto+appmeta Day1, Shield package, PKI, Main wiring)
  - `Member3-Go-DB-GraphQL/` — 5 phase files (DB+Schema Day1, Resolvers, Goodbye RPC, Heartbeat, AgentServer Rust)
  - `Member4-Rust-Shield-CI/` — 7 phase files (Crate scaffold, Core modules, Enrollment, Heartbeat+Renewal, Network, Updater+Systemd, CI+Connector main)
- Created `Services/Shield.md` — full service documentation (module map, startup flow, SPIFFE identity, state files, config)
- Updated `Planning/Roadmap.md` — added Sprint 4 section with team split + decisions, future sprints table
- Updated `Home.md` — added Shield service, Sprint 4 navigation links, updated system diagrams
- Updated `agent.md` — Shield added to project overview, Sprint 4 quick rules, new build commands, updated proto section

**Key decisions:**
- `path.md` is the team's single source of truth for execution order — agents must check it before touching any file
- Phase files use frontmatter `depends_on`/`unlocks` for machine-readable dependency tracking
- Conflict zone table in `path.md` prevents simultaneous edits to shared files
- Day 1 items (M2 proto + M3 DB/GraphQL schema) must land before anyone fans out — documented as critical
- Agreed with spec: Shield heartbeats to Connector :9091 only, never directly to Controller

**What's next:**
- M2 should start Phase 1 (proto/shield/v1/shield.proto + connector.proto modifications + appmeta) — this is the Day 1 unblocking commit
- M3 should start Phase 1 in parallel (003_shield_schema.sql + graph/shield.graphqls + connector.graphqls)
- Once both Day 1 commits land: run `buf generate` + `go generate ./graph/...` + `npm run codegen`
- M1 can start Phase 1 (layout/routing scaffold) immediately — no backend dependency
- M4 can start Phase 1 (crate scaffold) immediately after proto lands
- Phase 6 end-to-end renewal test should be run before Sprint 4 merges

---

## 2026-04-16 — Claude Code (Sonnet 4.6)

**What was done:**
- Reviewed cert renewal implementation (Phases 1–5) done by external model
- Found 4 bugs: duplicate gRPC registration, CSR-vs-PKIX mismatch, PEM-passed-as-DER, empty CA chain in renewal response
- Fixed all 4 bugs:
  - Moved `RenewCert` handler to `EnrollmentHandler` (single gRPC registration)
  - Changed Go PKI to parse CSR from connector instead of PKIX public key (adds proof-of-possession)
  - Fixed `parse_cert_not_after` to decode PEM → base64 → DER before parsing
  - Fixed `RenewConnectorCert` to return full CA chain; `RenewCert` handler now calls `loadCACerts()`
  - Added mTLS channel rebuild after renewal in `heartbeat.rs`
- Built and released `connector-v0.2.0` via GitHub Actions workflow
- Set up Obsidian vault (`.zecurity-obs/`) with full maintenance structure

**Key decisions:**
- Used CSR (not raw PKIX public key) for renewal — self-signed CSR proves key possession, simpler Rust side, one less dependency
- `RenewCert` handler stays on `EnrollmentHandler` (not a separate struct) — one gRPC registration is a hard requirement
- Vault mirrors p2p-network structure: Services/ (not Modules/), same Planning/ + Architecture/ layout

**What's next:**
- Phase 6 end-to-end renewal test with `CONNECTOR_CERT_TTL=3m CONNECTOR_RENEWAL_WINDOW=2m`
- After test passes: reset TTLs to production values, tag `connector-v0.3.0` (or patch release)
- Sprint 4: traffic proxying (WireGuard / tun)

---

## 2026-04-16 — Kiro (Lead Session)

**What was done:**
- Diagnosed CI failure: `cross` was running from `connector/` subdirectory, so the Docker container couldn't access `../proto/` outside that directory
- Migrated proto to repo root: `proto/connector/v1/connector.proto` (single source of truth)
- Moved `buf.yaml` + `buf.gen.yaml` to repo root; updated `buf.yaml` with `roots: [proto]`
- Updated `connector/build.rs` to reference `../proto/connector/v1/connector.proto`
- Fixed CI workflow: removed `working-directory: connector`, added `--manifest-path connector/Cargo.toml` so cross mounts full repo
- Reverted `Cross.toml` GHCR custom image references (images never existed) back to `pre-build` apt-get
- Fixed `Makefile` `generate-proto` target: `cd controller && buf generate` → `buf generate` (from repo root)
- Updated `agent.md` proto conventions to reflect new repo-root proto location
- Released `connector-v0.3.0` (re-tagged twice to pick up fixes)

**Key decisions:**
- Repo-root `proto/` is the correct structure for multi-language monorepos — no service "owns" the contract
- `--manifest-path` over `working-directory` for cross: ensures full repo is mounted in the Docker container
- `pre-build` apt-get in `Cross.toml` is sufficient; custom GHCR images are unnecessary overhead unless apt-get proves consistently unreliable

**What's next:**
- Verify `connector-v0.3.0` CI build passes end-to-end
- Phase 6 end-to-end renewal test
- Sprint 4: traffic proxying (WireGuard / tun)

---

## 2026-04-16 — OpenCode (External Model)

**What was done:**
- Migrated proto from manual `protoc` to Buf CLI workflow
- Created versioned proto: `controller/proto/connector/v1/connector.proto` with `package connector.v1`
- Created Buf configs: `buf.yaml` (breaking: PACKAGE, lint: DEFAULT) + `buf.gen.yaml` (remote plugins)
- Updated Go imports in 4 files: `enrollment.go`, `heartbeat.go`, `spiffe.go`, `main.go`
- Updated Rust `build.rs` to reference `../controller/proto/connector/v1/connector.proto`
- Fixed Rust `main.rs` to use `include_proto!("connector.v1")` (package match)
- Added `generate-proto` target to Makefile
- Deleted duplicate directories: `proto/connector/*.pb.go`, `github.com/`
- Cleaned up duplicate generated files in `gen/go/proto/connector/`

**Key decisions:**
- Used Option A: keep module as `github.com/yourorg/ztna/controller` (not repo-level) — current architecture stability
- Used `paths=source_relative` — generates to `gen/go/proto/connector/v1/` (mirrors source structure)
- go_package = `github.com/yourorg/ztna/controller/gen/go/proto/connector/v1;connectorv1`
- Import path: `github.com/yourorg/ztna/controller/gen/go/proto/connector/v1`

**What's next:**
- Verify builds pass manually
- Test Phase 6 renewal flow
- Update agent.md proto conventions if needed

---

## Template for Future Sessions

```markdown
## YYYY-MM-DD — [Agent Name]

**What was done:**
- bullet points of changes made

**Key decisions:**
- architectural choices and why

**What's next:**
- what the next session should pick up
```

## 2026-04-17 — Codex (M3 Phase 5)

**What was done:**
- Completed Sprint 4 M2 Phase 1 Day 1 unblockers
- Created `proto/shield/v1/shield.proto` with `ShieldService` and all enrollment, heartbeat, renewal, and goodbye messages
- Updated `proto/connector/v1/connector.proto` with `Goodbye`, `GoodbyeRequest`, `GoodbyeResponse`, `ShieldHealth`, and `HeartbeatRequest.shields = 5`
- Added Shield SPIFFE and networking constants plus `ShieldSPIFFEID()` in `controller/internal/appmeta/identity.go`
- Ran `buf generate` from repo root and verified `controller/gen/go/proto/shield/v1/` plus updated connector stubs
- Ran `cd controller && go build ./...` successfully
- Marked M2 Day 1 items and the team `buf generate` step done in `Sprint4/path.md`

**Key decisions:**
- Kept existing connector proto field numbers unchanged and assigned the new repeated `shields` field to `HeartbeatRequest = 5`
- Used the repo's actual Go module path (`github.com/yourorg/ztna/controller/...`) for the new shield proto `go_package` so generated imports stay consistent with the existing controller module

**What's next:**
- Coordinate with M3 on `controller/migrations/003_shield_schema.sql`
- Start M2 shield service implementation under `controller/internal/shield/`
- Watch the dependency mismatch between `path.md` and `Phase2-Shield-Package.md` about whether the DB migration is required before all of Phase 2 or only the DB-backed parts

---

## 2026-04-17 — Codex

**What was done:**
- Completed Sprint 4 M1 Phase 2 GraphQL operations and codegen verification
- Confirmed Shield mutations were added in `admin/src/graphql/mutations.graphql`
- Confirmed the `GetShields` query was added in `admin/src/graphql/queries.graphql`
- Confirmed generated GraphQL artifacts exist under `admin/src/generated/`, including `ShieldStatus` and the Shield document nodes
- Marked `M1-N6` done in `Sprint4/path.md`
- Marked `Phase2-GraphQL-Operations.md` status as `done`

**Key decisions:**
- Treated Phase 2 as complete based on the repo's actual GraphQL codegen output, which generates typed document nodes rather than Apollo `use*` hooks
- Kept sprint tracking aligned with the verified build-check outcome instead of the older wording in the phase checklist

**What's next:**
- Continue with M1 Phase 3 to wire `Shields.tsx` to generated Shield queries and mutations
- Use the generated `GetShieldsDocument` and related mutation documents in the page implementation

## 2026-04-17 — Codex

**What was done:**
- Pulled latest `main`, including M3 Day 1 schema and GraphQL updates
- Implemented `controller/internal/shield/` Phase 2 package: `config.go`, `token.go`, `enrollment.go`, `heartbeat.go`, and `spiffe.go`
- Added `shield.NewService(...)` with Redis, DB, and PKI dependencies plus ShieldService interface compliance
- Implemented shield enrollment JWT generation and verification, Redis JTI burn, connector selection, interface address assignment from `100.64.0.0/10`, and the Enroll gRPC handler
- Implemented shield disconnect watcher logic for stale ACTIVE shields
- Added PKI `SignShieldCert` and `RenewShieldCert` support in `controller/internal/pki/`
- Ran `cd controller && go build ./...` successfully
- Marked M2 Phase 2 and Phase 3 done in Sprint 4 tracking docs

**Key decisions:**
- Derived `connector_addr` from the selected connector's `public_ip` with port `9091`, since Sprint 4 expects Shield post-enrollment traffic to target Connector `:9091` and the schema does not have a dedicated connector address column
- Kept shield cert issuance parallel to connector cert issuance instead of mutating existing connector PKI methods
- Reused the controller's intermediate CA fingerprint flow already used by connector enrollment

**What's next:**
- Wire Shield config and service registration into `controller/cmd/server/main.go`
- Add Shield env vars to `controller/.env` and `.env.example`
- Coordinate with M3/M1 on the remaining team `go generate ./graph/...` step when GraphQL codegen is needed

---

## 2026-04-17 — Codex (M1 Phase 3)

**What was done:**
- Completed Sprint 4 M1 Phase 3 Shields page implementation
- Replaced the `Shields.tsx` stub with live GraphQL-backed Shield data, 30-second polling, empty/loading states, and revoke/delete actions
- Extended `InstallCommandModal` to support a Shield variant for the Add Shield flow
- Fixed the unrelated frontend query/type mismatch by expanding `GetRemoteNetworks` connector fields and regenerated frontend GraphQL artifacts
- Verified `cd admin && npm run codegen` and `cd admin && npm run build` pass
- Marked `M1-N1` done in `Sprint4/path.md`
- Marked `Phase3-Shields-Page.md` status as `done`

**Key decisions:**
- Used the repo's actual Apollo pattern with generated `*Document` nodes plus `useQuery` and `useMutation`, instead of the phase note's outdated generated-hook wording
- Reused and extended the shared install modal instead of creating a second Shield-specific modal component
- Kept `Via Connector` rendering as truncated `connectorId`, since the current Shield query does not expose connector name

**What's next:**
- Continue with M1 Phase 4 to add `networkHealth` and shield counts on `RemoteNetworks.tsx`
- Decide whether to mark any additional M1-N items complete after reviewing exact scope against Phase 4

---

## 2026-04-17 — Codex (M2 Phase 4)

**What was done:**
- Implemented M2 Phase 4 controller wiring for Shield support
- Added Shield config loading in `controller/cmd/server/main.go`
- Instantiated `shield.NewService(...)` with DB, PKI, and Redis dependencies
- Registered `ShieldServiceServer` on the controller gRPC server alongside `ConnectorService`
- Started the Shield disconnect watcher goroutine from `main.go`
- Added Shield env vars to `controller/.env` and `controller/.env.example`
- Ran `cd controller && go build ./...` successfully
- Marked M2 Phase 4 done in Sprint 4 tracking docs

**Key decisions:**
- Reused the existing shared Redis client for Shield enrollment JTI storage instead of introducing a second client path
- Registered Shield on the same controller gRPC listener and TLS stack as Connector, matching the Sprint 4 service model

**What's next:**
- Push the Phase 4 changes on the active branch
- Coordinate final integration steps with M4 once Shield enrollment is exercised against a running controller

## 2026-04-17 — Codex (M3 Phase 5 Started)

**What was done:**
- Started Sprint 4 M3 Phase 5 after M4 confirmed the `ShieldServer` API matches the phase spec
- Added `connector/src/agent_server.rs` with the agreed `ShieldServer::new(...)` and `get_alive_shields()` API
- Added ShieldService RPC handlers for `Heartbeat`, `RenewCert`, `Goodbye`, and `Enroll` returning UNIMPLEMENTED
- Updated connector proto generation to include `proto/shield/v1/shield.proto`
- Exposed `agent_server` and `shield_proto` modules from `connector/src/main.rs` without starting the server

**Key decisions:**
- Kept Connector `:9091` startup wiring out of scope because M4 owns `connector/src/main.rs` startup in Phase 7
- Left the M3 Phase 5 checklist incomplete because real mTLS peer-certificate SPIFFE verification and cert-expiry renewal signaling still need to be completed

**What's next:**
- `cd connector && cargo build` now passes with warnings only
- Complete real peer-certificate SPIFFE extraction/validation and `not_after` renewal-window logic before marking M3 Phase 5 done

## 2026-04-17 — Codex

**What was done:**
- Completed Sprint 4 M3 Phase 5 `connector/src/agent_server.rs`
- Implemented real mTLS peer certificate extraction via Tonic request `peer_certs()`
- Parsed Shield certificate URI SAN and verified exact SPIFFE identity `spiffe://<trust_domain>/shield/<shield_id>`
- Parsed peer certificate `not_after` and set `HeartbeatResponse.re_enroll=true` when inside the renewal window
- Added `ShieldServer::serve(...)` helper with Connector cert/key, `workspace_ca.crt` trust root, and required Shield client auth for M4 wiring
- Reran `cd connector && cargo build` successfully
- Marked `M3-F1` done in `Sprint4/path.md` and Phase 5 frontmatter

**Key decisions:**
- Kept actual startup/binding out of `main.rs` beyond module exposure, because M4 owns Connector startup wiring
- Added a callable `serve(...)` helper so M4 can start the server without reimplementing mTLS setup

**What's next:**
- M4 can wire `ShieldServer::new(...).serve(...)` into `connector/src/main.rs` and pass `get_alive_shields()` data into the Connector heartbeat flow as planned

---

## 2026-04-17 — Codex (M4 Phase 5)

**What was done:**
- Implemented `shield/src/network.rs` for Shield host network bootstrap
- Added creation and reuse logic for the `zecurity0` TUN interface
- Added interface address assignment and link-up steps for the controller-assigned Shield IP
- Added base `inet zecurity` nftables rules allowing loopback and Connector traffic while dropping traffic entering on `zecurity0`
- Wired `shield/src/main.rs` to include the new `network` module
- Replaced the enrollment TODO in `shield/src/enrollment.rs` with the real best-effort `network::setup()` call
- Ran `cargo build --manifest-path shield/Cargo.toml` successfully
- Marked M4 Phase 5 done in Sprint 4 tracking docs

**Key decisions:**
- Kept network setup best-effort after enrollment so cert issuance and persisted state survive even if host capabilities or Linux tools are misconfigured
- Used the native `ip` and `nft` commands for deterministic host networking behavior while keeping idempotency and validation in Rust
- Reapplied the full nftables table declaratively on each run so restart behavior converges to one known rule set

**What's next:**
- Wait for M3 Phase 5 `connector/src/agent_server.rs` to land before starting M4 Phase 4 heartbeat/renewal
- If M3 is still in progress, M4 Phase 6 updater/systemd/install-script work is also unblocked

---

## 2026-04-17 — Codex (M4 Phase 5 Refactor)

**What was done:**
- Refactored `shield/src/network.rs` to use `rtnetlink` for interface lookup, address assignment, and link-up
- Replaced ad hoc nft rules file generation with typed `nftables` crate rule construction
- Enabled the `tokio` feature on the `nftables` crate so the async helper API compiles in the shield binary
- Updated Phase 5 and Shield service notes to reflect the final implementation accurately
- Re-ran `cargo build --manifest-path shield/Cargo.toml` successfully

**Key decisions:**
- Removed the direct `ip` binary dependency from the daemon path, since the Shield should not rely on userspace ops tooling to configure `zecurity0`
- Kept the docs honest about the current `nftables` crate: it gives typed Rust-side rule construction, but still applies rules through the system `nft` executable in this version
- Restricted documentation changes to M4/Shield implementation notes so no other member's ownership or phase dependencies changed

**What's next:**
- Continue waiting on M3 Phase 5 before starting M4 Phase 4 heartbeat/renewal
- Start M4 Phase 6 independently if you want to keep moving while M3 finishes `agent_server.rs`

---

## 2026-04-17 — Codex (M4 Phase 6)

**What was done:**
- Implemented `shield/src/updater.rs` by mirroring the connector updater for `shield-v*` releases and `/usr/local/bin/zecurity-shield`
- Wired `shield/src/main.rs` to support `--check-update` and spawn the updater loop when `AUTO_UPDATE_ENABLED=true`
- Added `shield/systemd/zecurity-shield.service`
- Added `shield/systemd/zecurity-shield-update.service`
- Added `shield/systemd/zecurity-shield-update.timer`
- Added `shield/scripts/shield-install.sh` with OS detection, kernel check, nftables installation, and active nftables-service warning
- Verified `cargo build --manifest-path shield/Cargo.toml` and `bash -n shield/scripts/shield-install.sh`
- Marked M4 Phase 6 done in Sprint 4 tracking docs

**Key decisions:**
- Aligned the Shield updater flow with the existing connector pattern and standardized on `--check-update` rather than inventing a separate `--update` flag
- Put distro-specific `nft` package installation in the install script, not the binary, so runtime assumptions stay simple for the daemon
- Recorded the operational caveat that the current `nftables` crate still applies rules via the `nft` executable, so install-time guarantees matter

**What's next:**
- Wait for M3 Phase 5 before starting M4 Phase 4 heartbeat/renewal
- M4 Phase 7 can start after that for connector main wiring and shield release CI

---

## 2026-04-18 — Claude Code (M4 Phase 4 + Phase 7)

**What was done:**
- Implemented `shield/src/heartbeat.rs` — mTLS heartbeat loop to Connector :9091 with exponential backoff, cert renewal on `re_enroll=true`, and best-effort `goodbye()` on SIGTERM
- Implemented `shield/src/renewal.rs` — RenewCert flow via Connector proxy: reads existing key, builds CSR DER, saves renewed `shield.crt` + `workspace_ca.crt`, updates `state.json`
- Added `SpiffeConnectorVerifier` to `shield/src/tls.rs` — custom rustls `ServerCertVerifier` that handles connector certs (clientAuth-only EKU, URI SANs) without requiring serverAuth EKU
- Added `build_connector_channel()` to `shield/src/tls.rs` — bypasses tonic 0.14 (no `rustls_client_config()`) via `connect_with_connector()` with a custom `SpiffeConnectorService` tower service
- Added `extract_public_key_der()` to `shield/src/crypto.rs` — mirrors connector pattern for renewal proof-of-possession
- Wired `mod heartbeat` and `mod renewal` in `shield/src/main.rs`; spawned heartbeat task; wired `goodbye()` on shutdown
- Created `.github/workflows/shield-release.yml` — mirrors connector CI; triggers on `shield-v*`; cross-builds amd64 + arm64 musl; uploads binaries, checksums, install script, systemd units
- Updated `connector/src/heartbeat.rs` — `run_heartbeat` now accepts `ShieldServer`; populates `shields` field via `get_alive_shields()` so the controller sees live shields in each heartbeat
- Updated `connector/src/main.rs` — builds controller mTLS channel, instantiates `ShieldServer::new()`, spawns Shield gRPC server on :9091, passes clone to heartbeat loop
- Checked M4-J1, M4-J2, M4-M1, M4-M2 in Sprint4/path.md
- Both `cargo build --manifest-path shield/Cargo.toml` and `cd connector && cargo build` pass clean

**Key decisions:**
- Used custom rustls `ServerCertVerifier` (`SpiffeConnectorVerifier`) instead of tonic's `ClientTlsConfig` because tonic 0.14 has no `rustls_client_config()` escape hatch and connector certs only have clientAuth EKU (not serverAuth), which WebPkiServerVerifier rejects
- Skipped the pre-flight raw-TLS step (connector heartbeat pattern) because the custom verifier is embedded directly in the tonic channel — SPIFFE verification fires on every (re)connect automatically
- Added `tower-service = "0.3"`, `hyper-util = { version = "0.1", features = ["tokio"] }`, `http = "1"` to `shield/Cargo.toml` as direct deps for the custom tower connector

**What's next:**
- M4 is fully done for Sprint 4
- Tag `shield-v0.1.0` after PR merges to trigger the CI release workflow
- Sprint 5: Resource discovery (RDE, per-resource nftables rules)

---

## 2026-04-22 — Codex (Phase E — Shield Control Stream)

**What was done:**
- Added `shield/src/control_stream.rs` with the Shield → Connector bidirectional `Control` stream, reconnect backoff, health reports, immediate resource acks, pong handling, and cert renewal reconnect flow
- Switched `shield/src/main.rs` from spawning the legacy heartbeat loop to spawning the new control stream
- Moved resource instruction apply/remove handling into `shield/src/resources.rs` so both streaming and legacy heartbeat paths share one implementation
- Added `SharedResourceState::store_ack()` and `drain_acks()` for immediate stream sends plus periodic health-loop ack flushing
- Set the default resource health check interval to 15 seconds for the streaming path
- Added `tokio-stream` to the Shield crate for tonic streaming request bodies
- Verified `cargo build --manifest-path shield/Cargo.toml` passes

**Key decisions:**
- Kept `shield/src/heartbeat.rs` compiled for rollback and for the existing best-effort `Goodbye` RPC, but it is no longer the active runtime path
- Immediate apply acks now report `protected` or `failed` based on the port probe result, matching the streaming `pending_action` guard in the controller
- Stored immediate acks in shared state after sending them so the periodic flush can converge with health-check status if the stream send/reconnect timing changes

**What's next:**
- Run the full streaming integration gate: `buf generate`, `cd controller && go build ./...`, `cd connector && cargo build`, and `cargo build --manifest-path shield/Cargo.toml`
- Exercise protect/unprotect end to end and confirm sub-2-second transitions through the Controller → Connector → Shield streams

---

## 2026-04-22 — Codex (Streaming Build Gate)

**What was done:**
- Ran `buf generate` successfully after allowing Buf remote module access
- Ran `GOCACHE=/tmp/zecurity-go-build go build ./...` in `controller/` successfully
- Ran `cargo build` in `connector/` successfully
- Ran `cargo build --manifest-path shield/Cargo.toml` successfully
- Confirmed local Postgres and Valkey containers are healthy
- Confirmed `zecurity-connector.service` and `zecurity-shield.service` are currently running, but they are installed heartbeat binaries rather than the newly built streaming binaries

**Key decisions:**
- Used `/tmp/zecurity-go-build` for the controller Go build because the sandbox cannot write to the default Go cache under `~/.cache`
- Did not replace or restart systemd services because `/usr/local/bin` and service restart require sudo, and passwordless sudo is not available

**What's next:**
- Install/restart the newly built connector and shield binaries with root privileges, then run the protect/unprotect latency test against the live services

---

## 2026-04-22 — Codex (Phase F — Streaming Cleanup)

**What was done:**
- Removed deprecated `Heartbeat` RPCs and `HeartbeatRequest` / `HeartbeatResponse` messages from `proto/connector/v1/connector.proto` and `proto/shield/v1/shield.proto`
- Regenerated proto stubs with `buf generate`
- Deleted `connector/src/heartbeat.rs` and moved its controller mTLS helper logic into `connector/src/controller_client.rs`
- Deleted `shield/src/heartbeat.rs` and moved best-effort `Goodbye` into `shield/src/control_stream.rs`
- Replaced `controller/internal/connector/heartbeat.go` with `disconnect_watcher.go`, keeping the watcher as a safety net for broken streams
- Updated connector Shield registry cleanup: buffered instructions now flush when a shield reconnects on the Control stream, and expiring shield certs receive `ReEnrollSignal`
- Bumped `connector` and `shield` crate versions to `1.1.0`

**Key decisions:**
- Kept the controller disconnect watcher because stream close handles normal disconnects, but the watcher still protects against abrupt process/network failures
- Kept DB column names like `last_heartbeat_at`; changing those names would be a larger migration with no behavioral benefit for this cutover

**Verification:**
- `buf generate`
- `GOCACHE=/tmp/zecurity-go-build go build ./...`
- `cd connector && cargo build`
- `cargo build --manifest-path shield/Cargo.toml`
- `git diff --check`

**What's next:**
- Install/restart the v1.1.0 connector and shield binaries with root privileges
- Run live protect/unprotect latency tests and then tag `connector-v1.1.0` / `shield-v1.1.0` when verified

---

## 2026-04-24 — Claude Code (Sprint 6 Planning)

**What was done:**
- Created full Sprint 6 execution plan at `.zecurity-obs/Sprint6/` — Shield Discovery + Connector Network Discovery
- `Sprint6/path.md` — master dependency map with Day 1 protocol, all phases, conflict zones, integration checklist, dependency graph
- `Sprint6/team-workflow.md` — member starter prompts + workflow guide
- `Sprint6/Member2-Go-Proto-DB/Phase1-Proto-Schema.md` — proto changes (DiscoveredService, DiscoveryReport in shield.proto; ShieldDiscoveryBatch, ScanCommand, ScanReport in connector.proto), migration 008, discovery.graphqls
- `Sprint6/Member2-Go-Proto-DB/Phase2-Discovery-Store.md` — controller/internal/discovery/ package (UpsertDiscoveredServices, ReplaceDiscoveredServices, GetDiscoveredServices, UpsertScanResults, GetScanResults, PurgeScanResults)
- `Sprint6/Member3-Go-Connector/Phase1-Discovery-Resolvers.md` — GetDiscoveredServices, PromoteDiscoveredService, TriggerScan, GetScanResults resolvers
- `Sprint6/Member3-Go-Connector/Phase2-Controller-Control-Handler.md` — ShieldDiscoveryBatch + ScanReport handlers in control.go + purge goroutine
- `Sprint6/Member3-Go-Connector/Phase3-Connector-Discovery.md` — connector/src/discovery/ module (scan.rs, tcp_ping.rs, scope.rs, service_detect.rs) + agent_server relay + control_plane scan dispatch
- `Sprint6/Member4-Rust-Shield/Phase1-Discovery-Module.md` — shield/src/discovery.rs (/proc/net/tcp parser, fingerprint, diff/full-sync logic)
- `Sprint6/Member4-Rust-Shield/Phase2-Control-Stream-Wiring.md` — heartbeat.rs wiring, full sync on connect, diffs every 60s
- `Sprint6/Member1-Frontend/Phase1-Discovery-Tab.md` — discovered services panel on Shields page + PromoteServiceModal
- `Sprint6/Member1-Frontend/Phase2-Scan-UI.md` — ScanModal with CIDR input, 3s result polling, Create Resource per row
- Updated `agent.md`, `AGENTS.md`, `CLAUDE.md` — all sprint references updated to Sprint 6
- Updated `Home.md` — Sprint 6 Active navigation block added (Sprint 4 links preserved)
- Updated `Planning/Roadmap.md` — Sprint 5 marked complete, Sprint 6 Active section added with full key decisions

**Key decisions:**
- Discovery rides existing Control streams — no new RPCs (DiscoveryReport on ShieldControlMessage field 7; ShieldDiscoveryBatch/ScanReport/ScanCommand on ConnectorControlMessage fields 8/9/10)
- Shield scans only its own host via /proc/net/tcp; Connector handles network-wide TCP scanning
- M4 can start discovery.rs scaffold on Day 1 with no proto dependency

**What's next:**
- M2 commits Day 1 proto + migration 008 + discovery.graphqls to unblock the team
- M4 can start shield/src/discovery.rs immediately (no proto needed for core logic)
- M1 can start page layout immediately (no codegen needed for structure)

## 2026-04-27 — M3 (Claude Sonnet 4.6)

**What was done:**
- Pulled M2's Day 1 updates from main (protos, migration 010, discovery.graphqls, generated stubs)
- Created `controller/internal/discovery/store.go` — all DB helpers (M2-A work that was missing)
- Created `controller/internal/discovery/config.go` — DiscoveryConfig + ScanResultTTL constant
- Implemented all 4 resolvers in `controller/graph/resolvers/discovery.resolvers.go`
- Added `toDiscoveredServiceGQL()` + `toScanResultGQL()` mappers to `helpers.go`
- Added `PushScanCommand()`, `handleShieldDiscoveryBatch()`, `handleScanReport()`, `protoToDiscoveredService()` to `controller/internal/connector/control_stream.go`
- Added hourly purge goroutine in `controller/cmd/server/main.go`
- Created `connector/src/discovery/` — `mod.rs`, `tcp_ping.rs`, `service_detect.rs`, `scope.rs`, `scan.rs`
- Modified `connector/src/agent_server.rs` — buffers DiscoveryReport per shield; `drain_discovery_batch()` produces ShieldDiscoveryBatch
- Modified `connector/src/control_stream.rs` — 5s discovery flush ticker; ScanCommand handler spawns execute_scan and sends ScanReport upstream
- Updated path.md checkboxes (M3-B1, B2, C1, C2, D1, D2, D3) and phase file statuses to done

**Key decisions:**
- Discovery store created by M3 since M2 had not implemented it — fills M2-A gap
- `pending_discovery` keyed by shield_id (latest report per shield wins before flush) — avoids double-sending if shield sends multiple reports within the 5s window
- `PushScanCommand` returns error if connector is offline — resolver surfaces this to the UI immediately rather than silently dropping

**What's next:**
- M4 to wire shield/src/heartbeat.rs discovery calls (Phase E3/E4) if not done
- M1 to complete frontend discovery tab + scan UI (Phase F)
- Final integration: buf generate clean, all five build gates green, migration 010 runs

---

## 2026-04-27 — M3 (Go+Rust / Connector)

**What was done:**
- All M3 Sprint 6 phases already complete (B1/B2/C1/C2/D1/D2/D3) — verified path.md checkboxes
- Merged main into sprint6-member3; resolved add/add conflicts in `controller/internal/discovery/config.go` and `controller/internal/discovery/store.go` (took origin/main versions — had `Config` struct, transactions, `ReachableFrom` field)
- Committed as `95ba36c` and `66f99f9`
- Pushed branch to origin

**Docs sweep — heartbeat.rs → control_stream.rs rename across all Obsidian notes:**
- Updated `Services/Shield.md`, `Services/Connector.md`, `Architecture/System Overview.excalidraw.md` — module maps, startup flows, control stream sections
- Updated `Sprint6/path.md` and `Sprint7/path.md` — conflict zones, team assignments, M4-E phase items
- Updated `Sprint7/Member4/Phase1-Shield-Tunnel-Relay.md` — file references, section header
- Added historical notes to `Sprint4/path.md`, `Sprint5/path.md` — header notes + conflict zone annotations + phase item footnotes
- Updated Sprint4/Sprint5 team-workflow files, phase spec files (`Phase2-Heartbeat-Ack.md`, `Phase2-Heartbeat-Relay.md`, `Phase4-Heartbeat-Renewal.md`, `Phase2-Core-Modules.md`, `Phase5-AgentServer-Rust.md`)
- Updated `codebase.md`, `implementation-report.md`, `improvements.md`, `studyplan.md` — module listings, section headers, call chain refs

**Key decisions:**
- Historical sprints (4/5) preserved as-is with footnotes — rewriting would lose accuracy of what was actually built at that time
- Session Log.md left unchanged — accurately records what was done in each past session
- `connector/build.rs` comment left as-is — describes proto generation, not runtime module

**What's next:**
- Sprint 6 fully complete on this branch; ready for PR to main when other members finish
- Sprint 7 planning available in `.zecurity-obs/Sprint7/path.md`

---

## 2026-04-27 — M3 (Claude Sonnet 4.6) — Shield network.rs restart crash fix

**What was done:**
- Fixed `shield/src/network.rs` `interface_index()` — was propagating `ENODEV` (os error 19) as a fatal error instead of returning `Ok(None)`, causing the shield to crash on every restart with "failed to restore network on startup / No such device"
- Root cause: `zecurity0` TUN interface is destroyed when the shield process exits; on the next startup `interface_index()` received `ENODEV` from the netlink query and treated it as a hard failure rather than "interface doesn't exist yet, create it"
- Fix: match `NetlinkError::NetlinkError` where `code.get() == -19` and return `Ok(None)`, letting `setup_tun_interface()` fall through to the creation path
- Updated `.zecurity-obs/Services/Shield.md` to document the startup-restore behaviour and the ENODEV handling
- `cargo build --manifest-path shield/Cargo.toml` passes clean

**Key decisions:**
- Fixed at the `interface_index` level — keeps the change minimal and lets the existing idempotent creation path handle the fresh-create on restart

**What's next:**
- Release a new shield binary so deployed shields stop crash-looping

---

## 2026-04-29 — Claude Code (M2, Sprint 7 Phase 1)

**What was done:**
- Sprint 7 Day 1 unblock — all M2 deliverables landed.
- New `proto/client/v1/client.proto` — `ClientService` with `GetAuthConfig`, `TokenExchange`, `EnrollDevice`.
- New `controller/migrations/011_client.sql` — `invitations` + `client_devices` tables with FK to `users` / `workspaces`.
- New `controller/graph/client.graphqls` — `Invitation`, `ClientDevice`, `myDevices`, `invitation(token)`, `createInvitation`.
- Added `graph/client.graphqls` to `controller/graph/gqlgen.yml` schema list.
- Ran `buf generate`, `gqlgen generate`, `npm run codegen` — all clean. `go build ./...` passes. Stub resolver `graph/resolvers/client.resolvers.go` ready for M3.
- Ticked all M2-D1-* and M2-A* boxes plus the buf/gqlgen/codegen TEAM lines in `Sprint7/path.md`.
- Phase file frontmatter set to `status: done`.

**Key decisions:**
- Deviated from phase doc on three points (documented in the phase file's Post-Phase Fixes section): proto `go_package` must include `proto/` segment and use `yourorg/ztna` module; GraphQL fields use `:` separator and `String` instead of nonexistent `Time` scalar; skipped the broken `models:` block — let gqlgen auto-generate into `models_gen.go` per repo convention.

**What's next:**
- M3 Phase B (`controller/internal/client/service.go` — implement the 3 RPCs) and M3 Phase C (invitation HTTP API + email + resolvers) are unblocked.
- M4 Phase F1+F2 (Rust CLI scaffold + login flow) unblocked once M3-B lands.
- M1 Phase E unblocked once M3-C lands.

---

## 2026-04-29 — Codex (M4, Sprint 7 client login fix)

**What was done:**
- Fixed `zecurity-client login` rustls CryptoProvider panic by installing the `ring` provider at process startup in `client/src/main.rs`.
- Updated client rustls/tokio-rustls dependency features to use `ring` without the direct `aws_lc_rs` default.
- Documented the fix in Sprint 7 path.md and the M4 Phase 2 phase file.

**Key decisions:**
- Matched the existing connector/shield provider choice (`ring`) rather than introducing a second runtime crypto provider policy.

**What's next:**
- Re-run `zecurity-client login` against a reachable controller to continue OAuth and enrollment validation.

---

## 2026-04-29 — Codex (M4, Sprint 7 client TLS trust fix)

**What was done:**
- Fixed `zecurity-client login` `UnknownIssuer` by fetching the controller intermediate CA from `/ca.crt` before the first gRPC call.
- Updated tonic TLS setup to use the fetched CA and controller host as the expected TLS server name.
- Added `setup --http-base` and default HTTP base derivation from `controller_address` for local dev.

**Key decisions:**
- Reused the controller's existing public CA endpoint instead of adding a second trust-bootstrap endpoint.

**What's next:**
- Run login against the local controller; it should proceed past TLS into `GetAuthConfig` / OAuth.

---

## 2026-04-29 — Codex (M4, Sprint 7 client state persistence fix)

**What was done:**
- Added `client/src/state_store.rs` for persisted `StoredWorkspaceState`.
- `zecurity-client login` now saves workspace/user/device/session state after enrollment.
- Private key PEM is encrypted at rest with AES-256-GCM (`enc1:<base64(nonce||ciphertext)>`); the AES key is stored separately in a 0600 `.key` file.
- `zecurity-client status` now loads saved state and prints logged-in user, cert expiry, device ID, and SPIFFE ID.
- `zecurity-client logout` deletes the saved state and key files.
- Documented the corrected Sprint 7 storage behavior in `Sprint7/path.md` and the M4 Phase 4 Post-Phase Fixes section.

**Key decisions:**
- Treated "in memory only" as applying to the decrypted private key, not to all client session state. Persisting certs, tokens, and metadata is required for `status`, `logout`, and Sprint 8 tunnel startup.

**What's next:**
- Re-run `zecurity-client login`, then `zecurity-client status`; status should show the saved user and certificate expiry instead of "Not connected".

---

## 2026-04-29 — Codex (Sprint 8 planning correction)

**What was done:**
- Replaced the premature Sprint 8 RDE plan with a new Sprint 8 Policy Engine plan: groups, group members, resource access rules, ACL compiler, Connector heartbeat ACL push, and Client `GetACLSnapshot`.
- Moved the existing RDE tunnel plan from `.zecurity-obs/Sprint8/` to `.zecurity-obs/Sprint9/`.
- Updated Sprint 9 RDE docs to depend on Sprint 8 ACL snapshots and removed the per-request Controller `check-access` path from the tunnel hot path.

**Key decisions:**
- Policy must come before RDE. Sprint 8 builds local default-deny ACL enforcement; Sprint 9 uses that snapshot for device tunnel routing.

**What's next:**
- Start Sprint 8 with M2 Day 1 schema/proto work: migration 012, `GetACLSnapshot`, Connector heartbeat ACL payload, and GraphQL schema/codegen.

---

## 2026-05-05 — Codex (M4, Sprint 9 client QUIC SPIFFE fix)

**What was done:**
- Diagnosed client dataplane failure from logs: QUIC handshake aborted before tunnel authorization because the client verified the connector cert against TLS name `connector`.
- Updated `client/src/tunnel_pool.rs` to accept rustls `NotValidForNameContext` as a SPIFFE-name mismatch only when the connector cert has `spiffe://<workspace-trust-domain>/connector/...`.
- Bumped client package version to `1.0.10` and documented the fix in Sprint 9 path and M4 client phase docs.
- Ran `cd client && cargo build`; build passes with pre-existing warnings.

**Key decisions:**
- Kept workspace CA chain validation intact and added explicit connector SPIFFE validation instead of disabling server certificate checks.

**What's next:**
- Cut/publish `client-v1.0.10`, reinstall the client, restart `zecurity-client.service`, run `zecurity-client up`, then retry the protected resource.

---

## 2026-05-05 — Codex (M3, Sprint 9 ACL refresh after client enrollment)

**What was done:**
- Diagnosed the post-QUIC `access denied` as stale ACL data after a new client device enrollment.
- Updated `controller/internal/client/service.go` so `EnrollDevice` notifies policy change after recording the new device SPIFFE.
- Documented the fix in Sprint 9 path.md.
- Ran `cd controller && go build ./...`; build passes.

**Key decisions:**
- Invalidating the ACL cache at enrollment keeps the existing group/rule compiler model intact while ensuring new device SPIFFE IDs reach connectors on the next snapshot push.

**What's next:**
- Deploy the updated controller, log in once, wait for connector heartbeat ACL push, then retry the protected resource.

---

## 2026-05-05 — Codex (M3, Sprint 9 connector ACL push)

**What was done:**
- Diagnosed continued `access denied` after client ACL refresh: connector Control stream never received ACL snapshots.
- Added policy store/cache/notifier dependencies to `EnrollmentHandler`.
- Updated connector health handling to send `ConnectorControlMessage_AclSnapshot` with the cached or freshly compiled ACL snapshot.
- Ran `cd controller && go build ./...`; build passes.

**Key decisions:**
- Push ACL on connector health so reconnects and policy cache invalidations converge without adding a new control RPC.

**What's next:**
- Deploy/restart controller and watch connector logs for `ACL snapshot stored`; then retry the protected resource.

---

## 2026-05-06 — Codex (M4, Sprint 9 client TUN cleanup)

**What was done:**
- Diagnosed `zecurity-client down` leaving the `zecurity0` interface behind, causing the next `up` to fail creating the TUN.
- Updated `client/src/tun.rs` so cleanup explicitly deletes the kernel link via rtnetlink and create removes stale `zecurity0` before creating a new interface.
- Bumped client package version to `1.0.11` and documented the fix.
- Ran `cd client && cargo build`; build passes with existing warnings.

**Key decisions:**
- Explicit link deletion is more reliable than relying on async task abort/drop timing for TUN cleanup.

**What's next:**
- Publish/install `client-v1.0.11`, then verify repeated `zecurity-client up/down/up` works without manual `ip link del`.

---

## 2026-05-06 — Codex (M4, Sprint 9 manual client ACL sync)

**What was done:**
- Compared the old ZTA client ACL sync model to the current client and started implementing parity one step at a time.
- Added `zecurity-client sync`, IPC request/response fields, daemon-side `sync_acl_now()`, and runtime `acl_last_sync_at` metadata.
- Documented the fix in Sprint 9 path and M4 client phase docs.
- Ran `cd client && cargo build`; build passes with existing warnings.

**Key decisions:**
- Kept this first step manual only. Auto refresh before `up`/`resources`/`status` and persisted snapshot metadata can be added in the next slice.

**What's next:**
- Test `zecurity-client sync` against a running controller/daemon, then implement automatic refresh behavior around `up`, `resources`, and login.

---

## 2026-05-06 — Codex (M4, Sprint 9 automatic client ACL refresh)

**What was done:**
- Added daemon-side ACL auto-refresh with a 60s TTL for `Resources` and `Up`.
- Changed `PostLoginState` to sync ACL before reporting login success, and fixed the login CLI to fail if daemon IPC returns `ok=false`.
- Added last successful ACL sync age to `zecurity-client status`.
- Bumped client package version to `1.0.12` and documented the fix.
- Ran `cd client && cargo build`; build passes with existing warnings.

**Key decisions:**
- Requests fail closed when no ACL snapshot exists. If a cached snapshot exists and refresh has a transient failure, the daemon logs the failure and continues with the cached snapshot.

**What's next:**
- Publish/install `client-v1.0.12`, then verify `login`, `resources`, and `up` work without running manual `sync` first.

---

## 2026-05-06 — Codex (M3/M4, Sprint 9 conditional connector ACL push)

**What was done:**
- Added `acl_version` to `ConnectorHealthReport`.
- Updated connector heartbeats to report the current local ACL snapshot version.
- Updated controller heartbeat handling to skip full ACL snapshot pushes when connector and controller versions already match.
- Documented the fix in Sprint 9 path and connector phase docs.
- Ran `cd controller && go build ./...` and `cd connector && cargo build`; both pass with existing warnings.

**Key decisions:**
- Heartbeat remains the self-healing path: connector version `0` or an older version triggers a full ACL push. Matching versions skip the payload.

**What's next:**
- Deploy controller and connector together because the heartbeat proto changed, then verify logs show `connector ACL already current` during steady-state heartbeats.

---

## 2026-05-06 — Codex (M4, Shield firewall relay semantics)

**What was done:**
- Compared the current Shield relay path against the old ZTA agent firewall model.
- Updated `shield/src/resources.rs` so per-resource nftables rules allow local relay traffic via `lo` and `127.0.0.0/8`, then drop normal LAN access.
- Removed Shield `zecurity0` from the per-resource allow path while leaving Shield interface setup untouched.
- Updated Sprint 5/Sprint 9 and Shield service docs to reflect that Shield `zecurity0` is not in the Sprint 9 protected-resource dataplane.

**Key decisions:**
- Keep the current Connector → Shield Control-stream relay model and avoid deleting Shield interface setup until routing metadata cleanup is done.

**What's next:**
- Verify `nft list ruleset` on a Shield host shows no `iifname "zecurity0"` rule in `resource_protect`, then test LAN block plus client access through Connector → Shield.

---

## 2026-06-05 — Claude (M2/M3/M4, ADR-004 Phase 2 release + Phase 3 reconciliation start)

**What was done:**
- Merged Phase 2 (desired-state snapshot resync) via PR #39; verified live: cached snapshot
  replayed on shield (re)connect, applied with matching generation, dual delivery on protect
  (instruction + live snapshot ~40ms apart), seamless recovery across shield restart.
- Released `shield-v1.0.9` + `connector-v1.0.16` (version bumps on main, tag-triggered CI;
  both workflows green, all assets incl. checksums published). Rollout via the auto-update
  timers (semver compare vs CARGO_PKG_VERSION — bump-before-tag is mandatory).
- Started Phase 3 (closed-loop reconciliation), steps 3.1–3.3 complete and building:
  - protos: `ResourceStateReport` (shield oneof 13) + `ResourceStateBatch` (connector oneof 14)
  - shield: `state_seq` bumped at all 4 active-set mutation points; `build_state_report()`
    (sorted ids + fingerprint); report emitted on every heartbeat after the ack drain
  - connector: `pending_state` latest-wins buffer + `drain_state_batch()` flushed on health tick
- Reports now arrive at the controller every ~15s and hit the default case (harmless) until 3.4.

**Key decisions:**
- Reconciler's corrective action is simply `buildSnapshotMsg` re-push (Phase 2 replace-semantics
  drops orphans + applies missing) — no new fix machinery, only new observation.
- Hysteresis is mandatory: drift must persist 2 consecutive reports before resync; a `deleting`
  tombstone must be absent 3 consecutive reports before reap. Counters in controller memory
  (restart just delays action).
- Reports describe the shield's in-memory intent state, not raw kernel nftables — manual nft
  tampering is out of scope for Phase 3 (documented limitation).

**What's next:**
- 3.4: store helpers (`GetDeletingForShield`, `ReapTombstone`), `internal/connector/reconcile.go`
  (security-scoped, hysteresis), `Recon` field on `EnrollmentHandler`, recv-loop case.
- 3.5 Gate: SQL-injected orphan → auto-resync; delete-while-down + connector restart → report-
  confirmed tombstone reap; verify no reconciler thrash during normal ops. Then commit/push/release.
- Status details in [[Decisions/ADR-004-Resource-Reconciliation]] (Phase 3 STATUS block).

---

## 2026-06-08 — Claude (M3/M4, ADR-004 Phase 3 implemented + Gate 3 verified)

**What was done:**
- Completed Phase 3 (closed-loop reconciliation): steps 3.4 (controller reconciler) + 3.5 (gate).
  - store: `GetDeletingForShield`, `ReapTombstone`.
  - new `internal/connector/reconcile.go`: per-report security scope (shield ∈ reporting
    connector+tenant); drift (orphan = reported∖desired, missing = desired∖reported) → 2-report
    hysteresis → `buildSnapshotMsg` re-push; `deleting` tombstone absent 3 reports → reap.
  - `Recon reconcileState` on `EnrollmentHandler`; `ResourceState` case in the recv loop.
- Committed all of Phase 3 (3.1–3.4) to branch `feat/resource-state-reconciliation` (90cec17),
  pushed. Hand-deployed branch binaries to connector (Archer) + shield (inkyank-01) via
  systemctl stop / install / start (update timers stopped first); controller via local `go run`.
- Gate 3 verified live: no-thrash (silent ~90s); organic orphan auto-resync with correct 2-report
  hysteresis; Test 3 tombstone reap purely by reconciler in ~76s; `nft list` on the host confirmed
  the resource_protect chain empty (rule dropped).

**Key decisions / findings:**
- `RecordAck` periodic re-verification heals DB corruption back to the shield's actual state — so
  you cannot fake a stable orphan on a legitimately-enforced resource; only `deleting` (guarded by
  `status != 'deleting'`) is immune, which is exactly why the reap test uses it.
- OPEN: should `failed` (port-not-listening, rule WAS applied) be in `GetDesiredForShield`?
  Currently excluded → reconciler strips the rule. Fail-closed alternative pending product call.
- OPEN: dev `go run` controller returns `RenewCert not implemented` → shield cert will expire
  (mTLS lockout risk, as seen 2026-06-03). Fix controller build before long deployments.

**What's next:**
- Decide `failed`-in-desired; if fail-closed, one-line change in `GetDesiredForShield` + retest.
- Merge `feat/resource-state-reconciliation` PR; bump versions; tag → release shield+connector;
  re-enable update timers on the deployed hosts.
- Phase 4 (break-glass forceDelete, vestigial `deleted_at` cleanup, drift metrics) when desired.
- Status detail in [[Decisions/ADR-004-Resource-Reconciliation]] (Phase 3 STATUS block).

---

## 2026-06-09 — Claude (M3/M4, ADR-004 fail-closed + Phase 3 release)

**What was done:**
- Merged fail-closed fix (PR #41): `GetDesiredForShield` now includes `failed` so a
  port-not-listening resource (shield HAS the drop rule) keeps enforcement instead of being
  stripped by the snapshot/reconciler. Doc + commit note the host-mismatch edge (benign re-push
  thrash; common case stays in active, no thrash).
- Bumped shield 1.0.9→1.0.10, connector 1.0.16→1.0.17 (PR #42); tagged shield-v1.0.10 +
  connector-v1.0.17; both Build & Release workflows green; all assets (binaries, checksums,
  install scripts, systemd units) published. First releases containing ADR-004 Phase 3.
- ADR-004 complete: Phase 1 (tombstone delete) + Phase 2 (snapshot resync) on 1.0.9/1.0.16;
  Phase 3 (closed-loop reconciliation) + fail-closed on 1.0.10/1.0.17. All phases live-verified.

**Key decisions:**
- Fail-closed for `failed` resources: admin intent is "protected"; don't strip a rule from a
  temporarily-down service. Controller-only change (ships with controller deploy, not the
  shield/connector release).

**What's next:**
- Roll deployed hosts onto 1.0.10/1.0.17: re-enable update timers (stopped during testing) or
  `--check-update`. Ensure the controller is running latest main (carries the fail-closed change).
- FIX `RenewCert not implemented` on the controller before any long-lived deploy — shield cert is
  7-day, renewal starts ~48h before expiry; failing renewal = mTLS lockout (seen 2026-06-03).
- Phase 4 (break-glass forceDelete, vestigial deleted_at cleanup, drift metrics) when desired.

---

## 2026-06-09 (later) — Claude (M3, shield cert renewal handler)

**What was done:**
- Root-caused the repeated `RenewCert not implemented` errors: the controller's ShieldService
  never implemented `RenewCert`. Shield requests renewal correctly and the connector proxies it,
  but the controller returned Unimplemented → shield cert (7-day) never renews → mTLS lockout
  ~7 days after enrollment (same symptom as the 2026-06-03 incident, different cause).
- Implemented `RenewCert` on the controller ShieldService (`internal/shield/renewal.go`):
  proxied-identity trust model (caller is the connector via its own mTLS; verify the shield is
  owned by that connector + trust domain + not revoked), then `pki.RenewShieldCert` (the
  `public_key_der` field actually carries a CSR — proven by the working connector path), update
  shields cert_serial/cert_not_after, return cert + CA chain.
- Extracted SPIFFE context keys/accessors into a neutral `internal/spiffe` package to break the
  connector↔shield import cycle; connector accessors now delegate (call sites unchanged); both
  unary + streaming interceptors inject via `spiffe.WithIdentity`. Updated spiffe_test.
- Controller builds + vets clean; connector package tests pass.

**Key decisions:**
- Controller-only fix — deployed shield/connector already request/proxy correctly; ships with a
  controller deploy, no shield/connector release.
- Trust chain for proxied renewal: controller trusts connector (mTLS, interceptor-verified) →
  connector verified shield mTLS → controller confirms shield∈connector. A connector already
  controls its shields' traffic, so this stays within the existing boundary.

**What's next:**
- Verify with a short CertTTL in a dev controller to watch a full renewal cycle (hard to trigger
  otherwise — only fires within 48h of the 7-day expiry).
- Merge + deploy the controller. Then Phase 4 (break-glass, deleted_at cleanup, drift metrics).

---

## 2026-06-10 — Claude (M2/M1, ADR-004 Phase 4.1 — break-glass forceDeleteResource)

**What was done:**
- Implemented the break-glass `forceDeleteResource(id)` mutation — the escape hatch for a resource
  permanently stuck mid-operation (`protecting`/`deleting`) because its shield is gone and will
  never ack removal. Hard-deletes the row in ANY state, deliberately bypassing the
  confirmation-gated tombstone path.
- New durable audit trail: migration `016_audit_logs.sql` (append-only `audit_logs` table:
  tenant, actor user/email, dotted action, target type/id, JSONB details snapshot, created_at;
  indexed by tenant+time and tenant+target). New `internal/audit` package with `Record()`
  (write-and-log; a failed audit write is logged loudly but never fails the already-completed
  action). Break-glass MUST leave a record since it skips the safety model.
- Store `ForceDeleteRow` (tenant-scoped `DELETE` regardless of status). Resolver flow: snapshot the
  row for audit → force-delete → audit-log `resource.force_delete` → best-effort
  `PushSnapshotForShield` so a still-connected shield drops the now-removed rule (replace semantics).
- Schema field gated `@hasRole(roles: [ADMIN])`; `make gqlgen` + `npm run codegen` regenerated.
- Frontend: `ForceDeleteResource` mutation + a guarded "Force delete" button on `ResourceDetail`
  that only appears when the resource is `transitional` (exactly where normal Delete is disabled),
  behind a stern break-glass confirm explaining the shield-offline rule-residue caveat.
- Controller `go build ./...` + `go vet` clean; frontend `tsc --noEmit` clean.

**Key decisions:**
- "Audit-logged" for a security break-glass means a durable, queryable DB record — not a log line.
  Built a general `audit_logs` table (first consumer: force-delete; future mutations can adopt it),
  matching the gap flagged in improvements.md 4.6.
- Best-effort snapshot re-push on force-delete: if the shield is actually alive, the orphan rule is
  dropped; if it's gone (the expected case), the push is a harmless no-op. Honest UI confirm states
  a rule held by an offline shield persists until reinstall.

**What's next:**
- Phase 4.2: vestigial `deleted_at` / `'deleted'` status cleanup (Finding 8).
- Phase 4.3: drift/reconcile metrics (`drift_detected`, `orphans_removed`, `tombstones_reaped`).
- Phase 4.4: finalize `deleting` list/detail UX.
- Migration 016 needs to run on existing DBs (manual psql or `down -v`) — it's additive (new table).

---

## 2026-06-10 (later) — Claude (M2/M1, ADR-004 Phase 4.2 — drop vestigial soft-delete, Finding 8)

**What was done:**
- Option A (drop the dead scaffolding) for the `resources` table's soft-delete leftovers. Resources
  are hard-deleted and the real tombstone is `deleting` + ack-gated reap, so `deleted_at` was always
  NULL, the `'deleted'` status unreachable, and seven `deleted_at IS NULL` filters were no-ops.
- migration `017_resources_drop_soft_delete.sql`: drop `idx_resources_shield` + `idx_resources_pending`
  → `DROP COLUMN deleted_at` → recreate both indexes without the `deleted_at IS NULL` predicate →
  swap `resources_status_check` to drop `'deleted'` (enum now
  pending|protecting|protected|unprotected|failed|deleting). Ordered so the column drop isn't blocked
  by a dependent index.
- `internal/resource/store.go`: removed all seven `deleted_at IS NULL` clauses (GetByID,
  GetByRemoteNetwork, GetAll, GetPendingForShield, GetDesiredForShield, Update, RecordAck).
- frontend: `resourceTone` in `Resources.tsx` + `ResourceDetail.tsx` aligned to the real enum —
  dropped dead `'deleted'`/`'managing'`/`'removing'`; also FIXED a latent bug where `Resources.tsx`
  had no tone for the real `deleting` state (fell through to `info`). Transitional arrays in
  `ResourceDetail.tsx` trimmed to `['protecting','deleting']`.
- Controller `go build ./...` + `go vet` clean; frontend `tsc --noEmit` clean.

**Key decisions:**
- Option A over B (repurpose `deleted_at` as a tombstone timestamp): `updated_at` already records
  when `MarkDeleting` fired, and `deleted_at` on a `deleting` (not deleted) row is misleading.
  Tombstone-age observability belongs in Phase 4.3 metrics, not a resurrected column.
- SCOPE: only the `resources` table. The `'deleted'` status on workspaces/users/connectors/shields/
  remote_networks is real in-use soft-delete — explicitly left untouched (a blind grep would break
  shield/connector lifecycle).

**What's next:**
- Phase 4.3: drift/reconcile metrics (`drift_detected`, `orphans_removed`, `tombstones_reaped`).
- Phase 4.4: finalize `deleting` list/detail UX.
- Migrations 016 + 017 won't auto-apply on an existing DB (manual psql or `down -v`). 017 is
  destructive-by-design (`DROP COLUMN`) but lossless — the column was always NULL.

---

## 2026-06-10 (later) — Claude (M2, ADR-004 Phase 4.3 — reconciler Prometheus metrics)

**Pre-work — repo state check:** PR #43 (`fix/shield-cert-renewal`) merged to main; merged PR #44
(`feat/resource-force-delete`, 4.1+4.2) → main is `3a96db0`. Branched `feat/reconcile-metrics` off
fresh main (no stacking).

**What was done:**
- Made the closed-loop reconciler observable. Backend = `github.com/prometheus/client_golang`
  (private registry); served on a SEPARATE internal listener (`METRICS_ADDR`, default
  `127.0.0.1:9102`) — deliberately NOT the public mux (metrics leak operational data). Metrics-server
  failure is logged, not fatal.
- New `internal/metrics` package: collectors + typed helpers (`ReconcileReport`, `DriftDetected(kind)`,
  `Resync`, `TombstoneReaped`, `SetReconcileGauges`) + `Handler()`. Known drift labels pre-created at 0
  so series exist from startup (a CounterVec emits nothing until a label set is observed).
- Wired into `internal/connector/reconcile.go` at the five event sites + gauge update at end of
  `reconcileShield` (under the existing `Recon.mu`): reports, drift{orphan|missing}, resyncs,
  tombstones_reaped, and current-state gauges (shields_drifting = drift entries >0; tombstones_pending
  = len(absent)).
- `main.go`: second goroutine listener serving `/metrics` on `METRICS_ADDR`.
- Metrics unit test (httptest scrape asserts families + counter/gauge values). `go build`, `go vet ./...`,
  `go test ./internal/metrics/...` + `./internal/connector/...` all green.

**Key decisions:**
- Renamed the wishlist `orphans_removed`/`missing_reapplied` → honest `drift_detected{kind}` +
  `resyncs_total`. Removal happens on the shield via snapshot replace-semantics; the controller never
  gets a per-orphan removal confirmation, only the orphan's absence in the next report. Don't ship a
  metric that claims more precision than the controller has.
- CARDINALITY: no shield_id/tenant_id/resource_id labels (unbounded → series explosion). Only `kind`.
- `reconcile_tombstones_pending` sustained >0 = a gone shield = break-glass (4.1) candidate — directly
  ties the metric back to the escape hatch.

**What's next:**
- Phase 4.4: finalize `deleting` list/detail UX (last Phase 4 item).
- Deploy note: set `METRICS_ADDR` per env; point Prometheus at it. No DB migration in 4.3.

---

## 2026-06-10 (later) — Claude (M1, ADR-004 Phase 4.4 — `deleting` UX, completes Phase 4)

**Pre-work — repo state check:** PR #45 (`feat/reconcile-metrics`, 4.3) merged → main is `6a98368`.
Branched `feat/deleting-ux` off fresh main.

**What was done (frontend only, no API change):**
- `Resources.tsx`: fixed a real bug — `transitionalStates` was `["managing","protecting","removing"]`
  (legacy states renamed away in mig 009, and MISSING the real `deleting`). A `deleting` row therefore
  polled at the slow 30s interval, so after the reconciler reaped it the row lingered up to 30s and read
  as a hung delete. Now `["protecting","deleting"]` → 3s polling, prompt disappearance.
- `ResourceDetail.tsx`: during `deleting`, `isProtected` is false, which previously showed the
  misleading "No shield is enforcing / Install a shield" hero AND a "Protect this resource" button.
  Added (a) a dedicated amber deletion hero, (b) a "Removing" row in the Protection panel, and
  suppressed the unprotected CTA in both. Copy explains the row persists until the shield confirms
  removal (explicitly "not a hung delete") and points to Force delete (4.1) if the shield is gone.
  Transitional spinner banner scoped to `protecting` so it doesn't duplicate the deletion hero.
- `tsc --noEmit` + eslint clean.

**Key decisions:**
- The two stale-status spots (`Resources.tsx` poll set here; `resourceTone` in 4.2) are the same class
  of bug from the mig-009 rename + the later `deleting` addition — worth a grep sweep for any other
  `'managing'`/`'removing'` references in future work.

**Phase 4 COMPLETE** — 4.1 break-glass + audit (PR #44), 4.2 drop soft-delete (PR #44), 4.3 reconciler
metrics (PR #45), 4.4 deleting UX (this branch). ADR-004 fully delivered across Phases 1–4.

**What's next (post-ADR-004 backlog, not Phase 4):**
- Apply migrations 016 + 017 on existing DBs; re-enable shield/connector update timers (1.0.10/1.0.17).
- Finding 7 (deferred): unprotect-against-dead-shield can stick in `protecting/remove` forever.
- Verify RenewCert end-to-end with a short `CertTTL`.

---

## 2026-06-10 (later) — Claude (live verification of Phase 4 on the distributed stack + reconciler fix)

**Setup:** controller + admin on dev box; connector on friend device 1; shield + resource on friend
device 2 (`192.168.1.164`). Applied migrations 016 + 017 to the dev DB (audit_logs created; deleted_at
dropped lossless — verified 0 non-null; 'deleted' removed from status check). Confirmed metrics endpoint
serves on `127.0.0.1:9102`.

**Verification — all 5 scenarios PASS:**
1. Baseline protect — full pipeline works; reconciler stayed quiet (0 drift/resyncs) under 300+ healthy
   reports (no-thrash confirmed under real traffic).
2. Deleting UX + tombstone delete (shield online) — sub-second ack-driven reap (RecordAck, not the
   reconciler); no audit row (correct — normal delete doesn't audit); list cleared promptly.
3. Break-glass force-delete (shield offline) — resource stuck in `deleting`; UI showed the deletion hero
   + Force delete button; force-delete removed the row AND wrote an `audit_logs` row
   (action=`resource.force_delete`, real actor email, details snapshot incl. status=deleting). 016 works e2e.
4. Drift metrics under orphan — SQL raw-deleted a protected row; reconciler detected orphan
   (`shields_drifting`→1, no resync on report 1), fired resync on report 2 (`resyncs_total` 0→1,
   `drift_detected{orphan}` →2), shield dropped the rule, drift cleared. 2-report hysteresis confirmed live.
5. Reconciler reap + gauges — SQL-injected a synthetic `deleting` tombstone; `tombstones_pending`→1,
   reaped after exactly 3 absent reports (`tombstones_reaped_total` 0→1, pending→0). 3-report hysteresis
   confirmed live, via the reconciler path (not RecordAck).

**Two findings the live run surfaced (both fixed on branch `fix/reconcile-tombstone-orphan`):**
- **`tombstones_pending` help text was wrong.** A fully-disconnected shield sends no reports → reconciler
  never runs → gauge stays 0 even with a stuck tombstone (proven in scenario 3). The "sustained >0 = gone
  shield" framing was backwards. Corrected: real break-glass signal = row stuck in `deleting` + shield
  `disconnected`, not this gauge.
- **`drift_detected{orphan}` over-counted.** The drift pass classified a `deleting` tombstone still
  enforced by the shield as an orphan (conflating normal deletes-in-progress with true zombies). Fixed in
  `reconcile.go`: orphan classification now excludes known tombstones (`GetDeletingForShield`, fetched
  before the drift pass); a still-enforced tombstone still sets `drift=true` (resync re-pushes the
  removal — backstop for a lost remove instruction) but is not counted as an orphan. `orphan` now means a
  TRUE zombie. `go build`/`vet`/`test` green.

**What's next:**
- Deploy the fix (controller restart) and optionally re-verify the orphan-vs-tombstone classification live
  (flip a protected row to `deleting` via SQL while the shield keeps reporting it → old build counts orphan,
  new build does not).
- Still open: Prometheus scrape + corrected alert (`deleting`-age + shield disconnected); production deploy
  of controller + 016/017; re-enable update timers; Finding 7; RenewCert short-TTL check.

---

## 2026-06-13 — Codex (Sprint 10.1 Relay provisioning RPC contract)

**What was done:**
- Added `proto/relay/v1/relay.proto` with the initial server-authenticated TLS
  `Provision` RPC contract.
- Wired Relay Rust protobuf client generation and generated Controller Go stubs.
- Removed the generated Relay CSR from version control and ignored Relay keys,
  CSRs, certificates, and Cargo build output.
- Updated Sprint 10.1 Phase B planning to record the completed protocol contract
  and pending Controller provisioning/heartbeat work.

**Verification:**
- Relay `cargo test`: 12 passed.
- Relay `cargo build`: passed with existing dead-code warnings.
- Controller `go build ./...`: passed.
- `buf generate`: passed.
- `buf lint`: still fails on pre-existing repository-wide proto root and control
  stream naming rules affecting Connector, Shield, Client, and Relay packages.

**What's next:**
- Implement the authenticated Controller `Provision` handler and PKI signing method.
- Add requested DNS/IP SAN fields and enforce the SAN allowlist during provisioning.
- Define and implement the mTLS `Heartbeat` RPC and Relay health persistence.

---

## 2026-06-13 — Codex (Relay Provision RPC)

**What was done:**
- Added and registered `controller/internal/relay.Service.Provision`.
- Kept `provisioning_token` reserved and ignored per the current proto contract;
  authenticated/single-use provisioning remains future work.
- Added canonical Relay UUID, DNS SAN, and IP SAN validation before PKI signing.
- Hardened PKI Relay CSR validation and aligned ECDSA leaf key usage.
- Completed Relay CSR output conversion from PEM to DER for
  `ProvisionRequest.csr_der`.

**Verification:**
- `go test ./internal/relay ./internal/pki/...`: passed.
- `go build ./...`: passed.
- Relay `cargo test`: 12 passed.
- Connector package tests require Docker and could not run in the sandbox.

**What's next:**
- Add the Relay-side TLS gRPC client call to `RelayService.Provision`.
- Add authenticated provisioning when the reserved token field is activated.

---

## 2026-06-13 — Codex (Relay Provision client)

**What was done:**
- Added Relay environment configuration for controller addresses, Relay ID,
  pinned CA fingerprint, state directory, and optional DNS/IP SANs.
- Added the Relay-side TLS `Provision` request using a locally generated P-384
  DER CSR and an Intermediate CA fetched from `/ca.crt` only after fingerprint
  verification.
- Validated the response metadata, leaf SPIFFE URI, and returned Intermediate
  CA fingerprint before storing `relay.key`, `relay.crt`, and
  `intermediate-ca.crt`.
- Wired provisioning into Relay startup. The mTLS QUIC listener and heartbeat
  remain pending because their runtime/protobuf contracts are not implemented.

**Verification:**
- Relay `cargo test`: 16 passed.
- Relay `cargo build`: passed with existing dead-code warnings.
- Controller `go test ./internal/relay ./internal/pki/...`: passed.
- Controller `go build ./...`: passed.

**Post-pull scope clarification:**
- Relay registration/JWT/Valkey support remains future work and is not required
  by the current Relay provisioning client or `Provision` RPC.
- The current target is a platform-level Relay listener that trusts the
  Platform Intermediate CA and accepts valid Connector chains from any
  workspace.
