---
type: planning
status: active
tags:
  - session-log
  - history
---

# Session Log

Most recent first. Every agent appends an entry after their session.

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

## 2026-04-17 — Codex

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

## 2026-04-17 — Codex

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
