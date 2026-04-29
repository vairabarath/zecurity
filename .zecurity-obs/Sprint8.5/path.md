---
type: planning
status: planned
sprint: 8.5
tags:
  - sprint8_5
  - client
  - daemon
  - runtime-state
---

# Sprint 8.5 — Client Daemon Foundation

> M4-only bridge sprint. This does not block M1/M2/M3 Sprint 8 Policy Engine work, but Sprint 9 RDE must not start until this foundation is complete.

---

## Sprint Goal

Refactor the Rust client from direct one-shot command state access into a daemon-required model for active runtime state.

Durable state remains encrypted at rest. Active runtime state lives in the daemon.

See [[Decisions/ADR-002-Client-Daemon-Required]].

---

## Key Decisions

| Decision | Detail |
|----------|--------|
| **Daemon required** | Commands that need active session/tunnel/runtime state use daemon IPC. No optional direct-state fallback. |
| **Service manager** | Linux system-level service unit: `/etc/systemd/system/zecurity-client.service`, installed with `User=<enrolling_user>`. |
| **Startup behavior** | CLI checks daemon IPC; if missing, starts daemon with `systemctl start zecurity-client`, then retries. |
| **IPC** | Same-user local Unix socket using newline-delimited JSON: one JSON object per line, `\n` delimiter. Reject cross-user access. |
| **Login ownership** | CLI runs the existing PKCE/browser login flow, then posts the login result to daemon with `PostLoginState`. Daemon persists durable encrypted state and holds active runtime state. |
| **Durable state** | Encrypted `state_store.rs` remains for refresh token/device identity/bootstrap state. |
| **Runtime state** | Decrypted private key, active access token, ACL snapshot, route/TUN/tunnel state live in daemon memory. |

---

## Team Assignment

| Member | Role | Area |
|--------|------|------|
| **M4** | Rust Client | daemon process, IPC, system-level systemd unit, command refactor, ACL runtime fetch |

---

## Phases

### PHASE A — Daemon Scaffold + IPC

> See [[Sprint8.5/Member4-Rust-Client/Phase1-Daemon-Scaffold-IPC]].

- [ ] **M4-A1** Add daemon subcommand/internal mode.
- [ ] **M4-A2** Add same-user Unix socket IPC.
- [ ] **M4-A3** Add `status`, `shutdown`, `load_state`, `get_token`, and `post_login_state` IPC messages using newline-delimited JSON.
- [ ] **M4-A4** Add system-level systemd unit template with installer-populated `User=`.
- [ ] **M4-A5** CLI helper starts daemon when required and retries IPC.

> Build check: `cd client && cargo build` passes.

### PHASE B — Command Refactor

- [ ] **M4-B1** `login` runs the existing OAuth/enrollment flow, then sends `PostLoginState` to the daemon.
- [ ] **M4-B2** `status` queries daemon runtime status.
- [ ] **M4-B3** `logout` tells daemon to clear runtime state and removes durable state.
- [ ] **M4-B4** `invite` uses daemon token path or explicit login, without direct-state fallback.

> Build check: `cd client && cargo build` passes.

### PHASE C — ACL Runtime Fetch

- [ ] **M4-C1** Daemon calls `GetACLSnapshot` after receiving `PostLoginState` or after refresh.
- [ ] **M4-C2** Daemon stores ACL snapshot in runtime state.
- [ ] **M4-C3** Missing/invalid ACL snapshot means default-deny.

> Build check: `cd client && cargo build` passes.

---

## Final Verification Checklist

- [ ] `cd client && cargo build` passes.
- [ ] `zecurity-client status` starts daemon if needed and reports daemon state.
- [ ] `zecurity-client logout` clears daemon runtime and encrypted durable state.
- [ ] Daemon socket rejects cross-user access.
- [ ] IPC uses newline-delimited JSON and rejects malformed frames.
- [ ] Daemon can load encrypted durable state but keeps decrypted key/access token in memory only.
- [ ] Daemon fetches ACL snapshot and treats missing snapshot as default-deny.

---

## Notes

1. Do not implement RDE tunnel routing here. RDE is Sprint 9.
2. Do not add a direct-state fallback path for active runtime state.
3. macOS `launchd` is deferred.
4. The daemon does not run the browser OAuth flow in Sprint 8.5; the CLI runs the existing flow and posts the result to the daemon.
