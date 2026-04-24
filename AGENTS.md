# Zecurity — Agent Context (Codex / OpenCode / Kilo / Gemini CLI)

> Load this file at session start. It is the entry point for all AI agents.

---

## Project Summary

**Zecurity** — Zero Trust Network Access platform.

| Component | Lang | Location | Port |
|-----------|------|----------|------|
| Controller | Go | `controller/` | HTTP :8080, gRPC :9090 |
| Connector | Rust | `connector/` | heartbeat to :9090, Shield server :9091 |
| Shield | Rust | `shield/` | heartbeats to Connector :9091 |
| Admin UI | React | `admin/` | dev :5173 |

**Sprint 6 is active:** Building Shield Discovery (local `/proc/net/tcp` scan → Control stream) and Connector Network Discovery (admin-triggered TCP scan across CIDR scope).

---

## First Action Every Session

The human will tell you their member number. Do this immediately:

```
Step 1: Read agent.md             → full project conventions
Step 2: Read .zecurity-obs/Sprint6/path.md  → dependency map + checkboxes
Step 3: Find first unchecked phase for this member where all depends_on are ✅
Step 4: Read that phase file      → exact spec, files, build check
Step 5: Brief the human: "Here's what you're building today..."
```

---

## Authoritative Files

- **`agent.md`** — conventions, code style, env vars, release process
- **`.zecurity-obs/Sprint6/path.md`** — ordered execution with checkboxes (source of truth for what's done)
- **`.zecurity-obs/Sprint6/Member{1-4}-*/Phase*.md`** — per-phase implementation specs
- **`.zecurity-obs/Services/*.md`** — service documentation (read before touching a subsystem)

---

## Proto Convention

Two proto files exist (both at repo root under `proto/`):
- `proto/connector/v1/connector.proto` — Connector ↔ Controller
- `proto/shield/v1/shield.proto` — Shield ↔ Connector + Shield ↔ Controller

**Run from repo root:** `buf generate` → Go stubs land in `controller/gen/go/proto/`

Rust stubs are generated automatically via `build.rs` in each crate.

---

## End of Session

Before ending, always:
1. Mark completed phase checkboxes in `.zecurity-obs/Sprint6/path.md`
2. Update the phase file frontmatter `status: done`
3. Append entry to `.zecurity-obs/Planning/Session Log.md`
