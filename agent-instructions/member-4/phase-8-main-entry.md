# Phase 8 — Rust Connector: Main Entry Point

Wire together enrollment, heartbeat, and updater into the connector daemon's main entry point.

---

## File to Modify

```
connector/src/main.rs
```

---

## Startup Flow

1. Init tracing with `LOG_LEVEL`
2. Load config via figment
3. Check `state.json`:
   - **Not exists** → run enrollment flow (Phase 5)
   - **Exists** → load state, go to heartbeat loop
4. `tokio::spawn(heartbeat_loop(...))`
5. If `AUTO_UPDATE_ENABLED`: `tokio::spawn(update_loop(...))`
6. Wait for SIGTERM / ctrl_c → graceful shutdown

---

## Important Rules

1. **Needs enrollment + heartbeat done** (Phases 5 + 6).
2. **Graceful shutdown** must close gRPC connections cleanly.

---

## Phase 8 Checklist

```
✓ main.rs implements full startup flow
✓ Tracing initialized
✓ Config loaded via figment
✓ state.json checked for enrollment state
✓ Enrollment flow runs if not yet enrolled
✓ Heartbeat loop spawned via tokio::spawn
✓ Update loop spawned if AUTO_UPDATE_ENABLED
✓ SIGTERM / ctrl_c handled for graceful shutdown
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 9 (deployment infrastructure).
