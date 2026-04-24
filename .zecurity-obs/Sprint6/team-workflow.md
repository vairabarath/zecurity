---
type: guide
status: active
sprint: 6
tags:
  - onboarding
  - workflow
  - team
---

# Sprint 6 — Team Workflow Guide

> How to start a session with any terminal AI (Claude Code, Codex, OpenCode, Kilo, Gemini CLI, etc.)
> Hand this to your team. One page. That's it.

---

## The Two-Sentence System

1. **Navigate to the repo.** Open your AI tool.
2. **Say your member number.** The AI does the rest.

---

## Step 1 — Start Your Session

### If using Claude Code (auto-loads CLAUDE.md)

```
claude
```

Then just say:

```
I'm Member 3. Start my session.
```

Claude Code already loaded `CLAUDE.md` on startup, so it knows exactly what to do.

---

### If using Codex, OpenCode, Kilo, Gemini CLI, or any other tool

Paste this starter prompt (replace `3` with your number):

```
Read the file AGENTS.md in the current directory first, then read agent.md.
I am Member 3 on Sprint 6 of this project.
Find my first unchecked phase in .zecurity-obs-6/Sprint6/path.md where all
depends_on items are already checked. Read that phase file and brief me:
what am I building today, which files do I touch, and what is the build check?
```

**Copy-paste versions for each member:**

**Member 1 (Frontend):**
```
Read AGENTS.md then agent.md. I am Member 1 (Frontend — React/GraphQL).
Find my first unchecked M1 phase in .zecurity-obs-6/Sprint6/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 2 (Go — Proto + DB + Schema):**
```
Read AGENTS.md then agent.md. I am Member 2 (Go — proto changes, migration 008, graph/discovery.graphqls, discovery store).
Find my first unchecked M2 phase in .zecurity-obs-6/Sprint6/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 3 (Go+Rust — Controller + Connector):**
```
Read AGENTS.md then agent.md. I am Member 3 (Go+Rust — discovery resolvers,
controller control handler, connector discovery modules, RDE device tunnel,
QUIC listener, CRL manager, systemd watchdog, check_access endpoint).
Find my first unchecked M3 phase in .zecurity-obs/Sprint6/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 4 (Rust — Shield):**
```
Read AGENTS.md then agent.md. I am Member 4 (Rust — shield/src/discovery.rs,
/proc/net/tcp scanner, Control stream wiring, tunnel relay for RDE).
Find my first unchecked M4 phase in .zecurity-obs/Sprint6/path.md where
all depends_on are checked. Read the phase file and brief me.
```

---

## Step 2 — During Your Session

The AI will brief you on what to build. Just work. A few things to keep in mind:

**Before touching any file:**
```
Is this file in the conflict zone table in Sprint6/path.md?
If yes — check with the owning member first.
```

**After each build check passes:**
```
Tell the AI: "Build check passed. Mark [phase name] done."
```
The AI will check the box in `path.md` and update the phase file status.

**If you're blocked because a dependency isn't ready yet:**
```
Tell the AI: "My phase X depends on M2's Phase Y which isn't done. What can I work on independently?"
```
The AI will find independent work — M4 can always scaffold discovery.rs structs, M1 can always build page layout.

---

## Step 3 — End Your Session

Tell the AI:

```
Session done. I completed [phase name(s)]. Update path.md checkboxes,
update phase file status to done, and write a session log entry.
```

The AI will:
1. Check the boxes in `Sprint6/path.md`
2. Set `status: done` in the phase file frontmatter
3. Append an entry to `.zecurity-obs/Planning/Session Log.md`

---

## Day 1 Protocol (Critical — Read This First)

Day 1 work is **M2 only** and everyone else is blocked until it lands.

**M2 must commit first:**
- `proto/shield/v1/shield.proto` — DiscoveredService + DiscoveryReport (field 7) + TunnelOpen/Opened/Data/Close (fields 8-11) in ShieldControlMessage
- `proto/connector/v1/connector.proto` — ShieldDiscoveryBatch (8) + ScanReport (9) + ScanCommand (10) in ConnectorControlMessage
- `controller/migrations/008_discovery.sql` — shield_discovered_services + connector_scan_results tables
- `controller/graph/discovery.graphqls` — GraphQL schema

**After M2's commit lands, anyone runs:**
```bash
buf generate                           # from repo root
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

**Until M2's DAY 1 is committed:**
- M4: scaffold `shield/src/discovery.rs` structs + `discover_sync()` + `service_from_port()` (no proto types needed)
- M1: build page layout, routing additions (no codegen needed yet)

---

## Conflict Zones (Memorize These)

| File | Owner | Rule |
|------|-------|------|
| `proto/shield/v1/shield.proto` | M2 owns | Already done after Day 1 — do not modify |
| `proto/connector/v1/connector.proto` | M2 owns | Already done after Day 1 — do not modify |
| `controller/internal/connector/control.go` | M3 modifies | M3 only |
| `connector/src/agent_server.rs` | M3 modifies | M3 only |
| `connector/src/control_plane.rs` | M3 modifies | M3 only |
| `connector/src/device_tunnel.rs` | M3 creates | M3 only |
| `connector/src/quic_listener.rs` | M3 creates | M3 only |
| `connector/src/agent_tunnel.rs` | M3 modifies | M3 only |
| `connector/src/main.rs` | M3 modifies (listener wiring) | M3 only |
| `shield/src/heartbeat.rs` | M4 modifies | M4 only |
| `shield/src/tunnel.rs` | M4 creates | M4 only |
| `controller/internal/device/check_access.go` | M3 creates | M3 only |

---

## Build Gates (Non-Negotiable)

Every phase ends with a build check. **You must pass it before proceeding.**

```bash
# Go controller
cd controller && go build ./...

# Rust connector
cd connector && cargo build

# Rust shield
cargo build --manifest-path shield/Cargo.toml

# Frontend
cd admin && npm run build
```

Warnings are OK. Errors are not. Never commit broken code.

---

## Quick Status Check

At any point, ask the AI:

```
What phases are done, what's in progress, and what's still blocked in Sprint6/path.md?
```

The AI reads the checkboxes and gives you a live status summary.

---

## TL;DR

```
1. Open AI tool in repo root
2. Paste your member starter prompt (see above)
3. AI reads agent.md + path.md + your phase file → briefs you
4. Build. Pass build gate. Tell AI to check the box.
5. End session: tell AI to write session log entry
```

That's the entire workflow.
