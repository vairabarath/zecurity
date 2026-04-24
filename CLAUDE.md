# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Admin UI (React).

**Sprint 6 is the active sprint.** The team is building two discovery features — Shield Discovery (Shield scans `/proc/net/tcp` and reports services via Control stream) and Connector Network Discovery (Admin triggers a TCP scan across a CIDR/IP scope via Connector, results appear in the UI for resource creation).

---

## Your First Step

When a team member starts a session, they will tell you their member number (M1, M2, M3, or M4). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint6/path.md` — dependency map and progress checkboxes
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are checked
4. Brief them: what they're building, which files to touch, and the build check command

If they don't give you a member number, ask: *"Which team member are you? (M1 Frontend / M2 Go / M3 Go+Rust / M4 Rust)"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint6/path.md` | Dependency map + progress tracker (checkboxes) |
| `.zecurity-obs/Sprint6/Member{N}-*/Phase*.md` | Detailed spec per phase |
| `.zecurity-obs/Planning/Session Log.md` | Append a session entry when done |

---

## Build Commands (memorize these)

```bash
cd controller && go build ./...                              # Go controller
cd connector && cargo build                                  # Rust connector
cargo build --manifest-path shield/Cargo.toml               # Rust shield
buf generate                                                 # Proto → Go stubs (from repo root)
cd controller && go generate ./graph/...                     # GraphQL codegen
cd admin && npm run codegen                                  # Frontend TS hooks
```

---

## Rules (non-negotiable)

- Build gate passes before proceeding to next phase
- Never change proto field numbers
- Check `Sprint5/path.md` conflict zone table before editing shared files
- `appmeta` constants must be identical in Go and Rust
- Shield heartbeats to Connector `:9091` only — never directly to Controller
- Shield validates `resource.host == detect_lan_ip()` before applying nftables
- nftables `chain resource_protect` always flushed + rebuilt atomically — never appended
- Resource instructions delivered via heartbeat piggyback only — no new RPCs
