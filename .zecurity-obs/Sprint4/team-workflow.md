---
type: guide
status: active
sprint: 4
tags:
  - onboarding
  - workflow
  - team
---

# Sprint 4 — Team Workflow Guide

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
I'm Member 4. Start my session.
```

Claude Code already loaded `CLAUDE.md` on startup, so it knows exactly what to do.

---

### If using Codex, OpenCode, Kilo, Gemini CLI, or any other tool

Paste this starter prompt (replace `4` with your number):

```
Read the file AGENTS.md in the current directory first, then read agent.md.
I am Member 4 on Sprint 4 of this project.
Find my first unchecked phase in .zecurity-obs/Sprint4/path.md where all
depends_on items are already checked. Read that phase file and brief me:
what am I building today, which files do I touch, and what is the build check?
```

**Copy-paste versions for each member:**

**Member 1 (Frontend):**
```
Read AGENTS.md then agent.md. I am Member 1 (Frontend — React/GraphQL).
Find my first unchecked M1 phase in .zecurity-obs/Sprint4/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 2 (Go — Proto + Shield + PKI):**
```
Read AGENTS.md then agent.md. I am Member 2 (Go — proto, appmeta, internal/shield/, PKI).
Find my first unchecked M2 phase in .zecurity-obs/Sprint4/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 3 (Go — DB + GraphQL + Connector):**
```
Read AGENTS.md then agent.md. I am Member 3 (Go+Rust — DB migrations, GraphQL resolvers, connector improvements, agent_server.rs).
Find my first unchecked M3 phase in .zecurity-obs/Sprint4/path.md where
all depends_on are checked. Read the phase file and brief me.
```

**Member 4 (Rust — Shield binary + CI):**
```
Read AGENTS.md then agent.md. I am Member 4 (Rust — shield/ crate, network.rs, CI, connector/src/main.rs).
Find my first unchecked M4 phase in .zecurity-obs/Sprint4/path.md where
all depends_on are checked. Read the phase file and brief me.
```

---

## Step 2 — During Your Session

The AI will brief you on what to build. Just work. A few things to keep in mind:

**Before touching any file:**
```
Is this file in the conflict zone table in Sprint4/path.md?
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
The AI will find independent work (e.g. M4 can always do network.rs, M1 can always do layout).

---

## Step 3 — End Your Session

Tell the AI:

```
Session done. I completed [phase name(s)]. Update path.md checkboxes,
update phase file status to done, and write a session log entry.
```

The AI will:
1. Check the boxes in `Sprint4/path.md`
2. Set `status: done` in the phase file frontmatter
3. Append an entry to `.zecurity-obs/Planning/Session Log.md`

---

## Day 1 Protocol (Critical — Read This First)

Day 1 has two commits that **everyone else is waiting for**.

**M2 must commit first:**
- `proto/shield/v1/shield.proto` (NEW)
- `proto/connector/v1/connector.proto` (modified)
- `controller/internal/appmeta/identity.go` (modified)

**M3 must commit in parallel:**
- `controller/migrations/003_shield_schema.sql` (NEW)
- `controller/graph/shield.graphqls` (NEW)
- `controller/graph/connector.graphqls` (modified)

**After both commits land, anyone runs:**
```bash
buf generate                           # from repo root
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

**Until M2's proto is committed:**
- M4: work on Phase 5 (network.rs — fully independent)
- M1: work on Phase 1 (layout/routing — no backend needed)

---

## Conflict Zones (Memorize These)

| File                                 | Owner       | Rule                                                            |
| ------------------------------------ | ----------- | --------------------------------------------------------------- |
| `proto/connector/v1/connector.proto` | M2 writes   | Everyone else waits for buf generate                            |
| `connector/src/agent_server.rs`      | M3 writes   | M4 starts server in main.rs only after M3 is done               |
| `connector/src/main.rs`              | M4 modifies | M4 only, coordinate ShieldServer::new() signature with M3 first |
| `connector/src/heartbeat.rs` | M3 modifies | M3 only (historical — now control_stream.rs) |
| `cmd/server/main.go`                 | M2 modifies | M2 only                                                         |
| `graph/connector.graphqls`           | M3 modifies | M3 first, M1 consumes via codegen                               |

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
What phases are done, what's in progress, and what's still blocked in Sprint4/path.md?
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
