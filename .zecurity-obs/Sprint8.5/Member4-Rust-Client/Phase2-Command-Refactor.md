---
type: phase
status: done
sprint: 8.5
member: M4
phase: Phase2-Command-Refactor
depends_on:
  - M4-Phase1-Daemon-Scaffold-IPC
tags:
  - rust
  - client
  - daemon
  - ipc
  - commands
---

# M4 Phase 2 — Command Refactor

---

## What You're Building

Refactor the four CLI commands (`login`, `status`, `logout`, `invite`) to talk to the daemon via IPC instead of reading the encrypted state file directly.

After this phase every command that needs active session state goes through the daemon. The state file (`state_store.rs`) is only written by the daemon and only read by the daemon on startup. CLI commands never call `load_workspace_state` or `save_workspace_state` directly.

---

## Prerequisite

Phase 1 (daemon + IPC) must be complete and `cargo build` must pass before starting here.

---

## Shared IPC Client Helper

Add a client-side helper in `ipc.rs` (or a separate `ipc_client.rs` — your choice):

```rust
/// Try IPC once. Returns Err if socket is missing or connection fails.
pub async fn send_ipc(msg: &IpcRequest) -> Result<IpcResponse>

/// Ensure daemon is running, then send IPC. Starts daemon via systemctl if needed.
/// Retries once after start. Fails closed on second failure.
pub async fn ensure_daemon_and_send(msg: &IpcRequest) -> Result<IpcResponse>
```

`ensure_daemon_and_send` flow:

```text
try send_ipc(msg)
  -> Ok: return response
  -> Err: run `systemctl start zecurity-client`
          sleep ~500ms
          try send_ipc(msg) again
          -> Ok: return response
          -> Err: return Err("daemon could not be started — run systemctl status zecurity-client")
```

---

## M4-B1 — `cmd/login.rs`

**Before:** Calls `login::run()`, saves to `state_store::save_workspace_state()`, prints result.

**After:**
1. Call `login::run(&conf, None)` — unchanged, CLI still owns the OAuth/PKCE/browser flow.
2. Build a `PostLoginState` IPC request from the `LoginResult`.
3. Send it to the daemon via `ensure_daemon_and_send`.
4. Daemon handles saving to `state_store` and holding runtime state.
5. CLI prints confirmation from daemon response.

```rust
// PostLoginState IPC payload — matches the IpcRequest variant
PostLoginState {
    workspace_slug: String,  // from conf.workspace
    access_token:   String,
    refresh_token:  String,
    expires_at:     i64,
    device_id:      String,
    spiffe_id:      String,
    certificate_pem:  String,
    private_key_pem:  String,  // plaintext — daemon encrypts before storing
    ca_cert_pem:      String,
    cert_expires_at:  i64,
    hostname:         String,
    os:               String,
    workspace_name:   String,
    workspace_id:     String,
    trust_domain:     String,
    user_email:       String,
}
```

Do **not** call `save_workspace_state` from the CLI after this refactor. The daemon owns that call.

---

## M4-B2 — `cmd/status.rs`

**Before:** Calls `state_store::load_workspace_state()` directly, prints cert expiry and device info.

**After:**
1. Send `{"type":"Status"}` IPC to daemon via `ensure_daemon_and_send`.
2. Print the `state` and session info from the daemon response.
3. If daemon is not running and cannot be started, print "Not connected" — do not fall back to reading state_store.

Expected daemon `Status` response fields:

```json
{
  "ok": true,
  "type": "Status",
  "state": "running",
  "email": "user@example.com",
  "device_id": "uuid",
  "spiffe_id": "spiffe://...",
  "cert_expires_at": 1234567890,
  "workspace": "my-workspace",
  "acl_snapshot_version": 3
}
```

If `acl_snapshot_version` is `0` or absent, print "ACL snapshot: not yet loaded".

---

## M4-B3 — `cmd/logout.rs`

**Before:** Calls `state_store::clear_workspace_state()` directly.

**After:**
1. Send `{"type":"Shutdown"}` IPC to daemon (best-effort — ignore if daemon is not running).
2. Call `state_store::clear_workspace_state(&conf.workspace)` to remove the encrypted files.
3. Print "Logged out of {workspace}."

Rationale: Shutdown tells the daemon to drop runtime state from memory. The CLI then removes durable state from disk. Order matters — clear runtime first, then disk.

---

## M4-B4 — `cmd/invite.rs`

**Before:** Calls `login::run()` every time to get a fresh access token, then posts the invite.

**After:**
1. Try `GetToken` IPC via `send_ipc` (no daemon start — if daemon is not running, skip to step 2).
2. If `GetToken` returns a valid token, use it for the invite API call.
3. If daemon is not running or token is missing/expired, run `login::run()` to get a fresh token (no direct state_store read).
4. Post the invite using whichever token was obtained.

This removes the forced re-auth on every `invite` call when the daemon is already running with a valid session.

---

## What Does NOT Change

- `cmd/setup.rs` — writes config only, no session state, no change needed.
- `state_store.rs` — stays as-is; only the daemon calls it after this phase.
- `login.rs` — the OAuth/PKCE/browser flow is unchanged; only the caller changes.
- `config.rs` — unchanged.

---

## Build Check

```bash
cd client && cargo build
```

After the build passes, verify manually:

```bash
zecurity-client login     # browser opens, completes, daemon receives PostLoginState
zecurity-client status    # shows daemon runtime state
zecurity-client logout    # daemon shuts down, state files removed
```

---

## Files Touched

### Modified
| File | What |
|------|------|
| `client/src/cmd/login.rs` | Removed `save_workspace_state` call; sends `IpcRequest::PostLoginState` via `ensure_daemon_and_send`; prints from `LoginResult` fields directly |
| `client/src/cmd/status.rs` | Removed `load_workspace_state` direct access; queries daemon `IpcRequest::Status`; prints email, cert expiry, device ID, SPIFFE ID, ACL version |
| `client/src/cmd/logout.rs` | Added best-effort `send_ipc(Shutdown)` before `clear_workspace_state`; daemon drops runtime state first, then disk state is cleared |
| `client/src/cmd/invite.rs` | Tries `GetToken` IPC first; only runs full OAuth flow if daemon has no active token |
| `client/src/daemon.rs` | `PostLoginState` handler now reconstructs `LoginResult` and calls `StoredWorkspaceState::from_login` so JWT claims correctly populate `workspace_id`, `user_id`, and `role`; removed now-unused `StoredDevice/Session/User/Workspace` imports |

---

## Post-Phase Fixes

### Fix: `PostLoginState` handler bypassed JWT claim decoding

**Issue:** The original Phase 1 `PostLoginState` handler built `StoredWorkspaceState` directly from IPC fields. Since `LoginResult.workspace.id` is always `String::new()` in `login.rs`, `workspace_id` was stored as an empty string.

**Root Cause:** `workspace_id`, `user_id`, and `role` are decoded from JWT claims inside `StoredWorkspaceState::from_login` — they are not part of `LoginResult` directly.

**Fix Applied (`client/src/daemon.rs`):**
```rust
// BEFORE: built StoredWorkspaceState directly with workspace_id: workspace_id.clone()

// AFTER: reconstruct LoginResult, call from_login to decode JWT claims
let login_result = LoginResult {
    workspace: WorkspaceInfo { id: String::new(), name: workspace_name, ... },
    session: SessionInfo { access_token, ... },
    ...
};
let stored = StoredWorkspaceState::from_login(login_result);
```
