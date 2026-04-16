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
