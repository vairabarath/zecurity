---
type: phase
status: done
sprint: 8.5
member: M4
phase: Phase3-ACL-Runtime-Fetch
depends_on:
  - M4-Phase1-Daemon-Scaffold-IPC
  - M4-Phase2-Command-Refactor
tags:
  - rust
  - client
  - daemon
  - acl
  - policy
---

# M4 Phase 3 — ACL Runtime Fetch

---

## What You're Building

After the daemon receives a `PostLoginState` IPC message (or after a token refresh in Sprint 9), it calls the Controller's `GetACLSnapshot` RPC and stores the result in daemon memory. Missing or invalid snapshot means default-deny.

This wires the client into the Sprint 8 Policy Engine. Sprint 9 RDE tunnel routing will read the ACL snapshot from daemon memory to make access decisions without contacting the Controller on the hot path.

---

## Prerequisite

Phase 1 and Phase 2 must be complete and `cargo build` must pass before starting here.

---

## Files to Modify

- `client/src/runtime.rs` — add `acl_snapshot` field to `RuntimeState`
- `client/src/daemon.rs` — fetch ACL snapshot after `PostLoginState`, store in runtime state
- `client/src/ipc.rs` — expose `acl_snapshot_version` in `Status` response

---

## M4-C1 — Fetch ACL Snapshot After `PostLoginState`

In `daemon.rs`, after processing `PostLoginState` and persisting durable state, call `GetACLSnapshot`:

```rust
async fn fetch_and_store_acl(
    runtime: &SharedState,
    conf: &ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
) {
    match fetch_acl_snapshot(conf, ca_pem, access_token, device_id).await {
        Ok(snapshot) => {
            let mut state = runtime.write().await;
            state.acl_snapshot = Some(snapshot);
            tracing::info!(
                version = state.acl_snapshot.as_ref().map(|s| s.version).unwrap_or(0),
                "ACL snapshot stored"
            );
        }
        Err(e) => {
            tracing::warn!(error = %e, "ACL snapshot fetch failed — default-deny in effect");
        }
    }
}
```

Implement `fetch_acl_snapshot` using the existing `grpc::connect_grpc` helper and the `GetACLSnapshot` RPC:

```rust
async fn fetch_acl_snapshot(
    conf: &ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
) -> Result<AclSnapshot> {
    let mut grpc = crate::grpc::connect_grpc(conf.controller(), ca_pem).await?;
    let resp = grpc
        .get_acl_snapshot(GetAclSnapshotRequest {
            access_token: access_token.to_string(),
            device_id: device_id.to_string(),
        })
        .await?
        .into_inner();
    resp.snapshot.ok_or_else(|| anyhow::anyhow!("controller returned empty ACL snapshot"))
}
```

The `AclSnapshot` and `GetAclSnapshotRequest` types come from `crate::grpc::client_v1`.

The `ca_pem` is loaded from the durable state's `ca_cert_pem` field (stored by `state_store` during `PostLoginState` handling).

Spawn the fetch as a background task so it does not block the IPC accept loop:

```rust
// Inside the PostLoginState handler, after persisting durable state:
let runtime_clone = Arc::clone(&runtime);
let conf_clone = conf.clone();
let ca_pem = stored_state.device.ca_cert_pem.clone();
let access_token = stored_state.session.access_token.clone();
let device_id = stored_state.device.id.clone();
tokio::spawn(async move {
    fetch_and_store_acl(&runtime_clone, &conf_clone, &ca_pem, &access_token, &device_id).await;
});
```

---

## M4-C2 — Store ACL Snapshot in `RuntimeState`

In `client/src/runtime.rs`, add the `acl_snapshot` field:

```rust
use crate::grpc::client_v1::AclSnapshot;

#[derive(Debug, Default, Clone)]
pub struct RuntimeState {
    pub schema_version: u32,
    pub workspace:      Option<WorkspaceInfo>,
    pub user:           Option<UserInfo>,
    pub device:         Option<DeviceInfo>,
    pub session:        Option<SessionInfo>,
    pub resources:      Vec<Resource>,
    pub last_sync_at:   Option<i64>,
    pub acl_snapshot:   Option<AclSnapshot>,  // ← new field
}
```

`AclSnapshot` does not implement `Default` from prost, but `Option<AclSnapshot>` defaults to `None`, so `#[derive(Default)]` still works.

---

## M4-C3 — Default-Deny for Missing Snapshot

The daemon never serves a fabricated or partial snapshot. The enforcement rule:

- `acl_snapshot` is `None` → deny (snapshot not yet fetched or fetch failed)
- `acl_snapshot` is `Some` with no matching entry → deny
- `acl_snapshot` is `Some` with matching entry but empty `allowed_spiffe_ids` → deny

For Sprint 8.5, the daemon does not yet route traffic — this is enforced by Sprint 9's tunnel code, which will read `acl_snapshot` from daemon memory. The daemon's responsibility here is to:
1. Only set `acl_snapshot` on a successful fetch.
2. Never replace a valid snapshot with `None` on a transient fetch failure (see note below).
3. Expose `acl_snapshot_version` in the `Status` IPC response so the CLI can show snapshot state.

**Snapshot replacement rule:** On a successful re-fetch (after token refresh in Sprint 9), replace the stored snapshot. On fetch failure, log a warning and keep the existing snapshot if one is present — do not revert to `None`. This avoids a transient network hiccup denying all access to already-active sessions.

Update the `Status` IPC response in `ipc.rs` to include:

```json
{
  "ok": true,
  "type": "Status",
  "state": "running",
  "acl_snapshot_version": 5
}
```

Return `0` if `acl_snapshot` is `None`.

---

## Build Check

```bash
cd client && cargo build
```

Manual verification after daemon is running:

```bash
zecurity-client login
zecurity-client status    # should show acl_snapshot_version > 0
```

Confirm in daemon logs that "ACL snapshot stored" appears after login.

---

## Notes for Sprint 9

- Sprint 9 Phase F (`device_tunnel.rs`) reads `acl_snapshot` from daemon memory to enforce per-connection access decisions.
- The daemon should re-fetch the ACL snapshot after token refresh (Sprint 9 Phase B).
- Do not add tunnel routing, TUN interface, or route management here. That is Sprint 9.

---

## Files Touched

### Modified
| File | What |
|------|------|
| `client/src/runtime.rs` | Added `use crate::grpc::client_v1::AclSnapshot`; added `acl_snapshot: Option<AclSnapshot>` field to `RuntimeState` |
| `client/src/daemon.rs` | Added `use crate::grpc::{self, client_v1::GetAclSnapshotRequest}`; startup block now spawns `fetch_and_store_acl` after `populate_runtime`; Status handler returns `acl_snapshot_version`; PostLoginState handler spawns `fetch_and_store_acl` after save+populate; added `fetch_acl_snapshot` and `fetch_and_store_acl` functions |

---

## Post-Phase Fixes

_None yet._
