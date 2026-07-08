---
type: phase
member: M1
sprint: 11.3
phase: 1
title: Client — Token Refresh Scheduler & Transparent ACL Retry
status: completed
commit: 5cddc9b
depends_on:
  - Sprint11.3/Member2-Go/Phase1-Auth-Handlers
---

# Phase 1 — Client: Token Refresh Scheduler & Transparent ACL Retry

## Goal

Keep the daemon's access token alive automatically. Two mechanisms:

1. **Proactive scheduler** — background task sleeps until 60s before expiry,
   rotates the token ahead of time. User never sees a 401.

2. **Reactive retry** — if the ACL fetch gets a `Unauthenticated` gRPC error
   despite the scheduler (race, restart, clock skew), transparently refresh
   once and retry. Dead session → clear local state, prompt re-login.

## Files

| File | Change |
|---|---|
| `client/src/auth.rs` (new) | `refresh_access_token`, `logout`, `RefreshError`, `extract_exp` |
| `client/src/daemon.rs` | `run_refresh_scheduler`, `fetch_acl_snapshot_with_refresh` |
| `client/src/state_store.rs` | `save_rotated_tokens` |
| `client/src/cmd/logout.rs` | Call `auth::logout` before clearing local state |
| `client/src/main.rs` | Wire `auth` module |

## auth.rs

```rust
pub struct RefreshedTokens {
    pub access_token: String,
    pub refresh_token: String,
    pub expires_at: i64,      // from JWT `exp` claim
}

pub enum RefreshError {
    SessionDead,              // HTTP 401 — user must re-login
    Transient(anyhow::Error), // network/5xx — retry later
}

pub async fn refresh_access_token(
    conf: &ClientConf,
    access_token: &str,   // expired OK — server accepts for identity only
    refresh_token: &str,
) -> Result<RefreshedTokens, RefreshError>

pub async fn logout(conf: &ClientConf, access_token: &str) -> Result<()>
// best-effort; Ok even on non-2xx — server is idempotent
```

### JWT exp extraction

```rust
fn extract_exp(access_token: &str) -> Result<i64> {
    // base64url-decode payload segment (no signature verification)
    // deserialize { exp: i64 }
}
```

Signature is enforced server-side on every API call. Client only needs `exp`
to schedule proactive refresh — verifying it again here is redundant.

## daemon.rs

### Proactive refresh scheduler

```rust
async fn run_refresh_scheduler(state: Arc<Mutex<SharedState>>, conf: Arc<ClientConf>) {
    loop {
        let expires_at = { state.lock().await.session.expires_at };
        let sleep_until = expires_at - 60;  // wake 60s before expiry
        // sleep until sleep_until, then:
        // 1. read (access_token, refresh_token) from state
        // 2. call auth::refresh_access_token
        // 3. on Ok: save_rotated_tokens + update in-memory state atomically
        // 4. on SessionDead: clear session, signal re-login
        // 5. on Transient: log warn, retry on next tick
    }
}
```

### Transparent retry on 401

```rust
async fn fetch_acl_snapshot_with_refresh(...) -> Result<AclSnapshot> {
    match fetch_acl_snapshot(..., &access_token, ...).await {
        Ok(snap) => Ok(snap),
        Err(e) if is_grpc_unauthenticated(&e) => {
            // 1. refresh_access_token(conf, &access_token, &refresh_token)
            // 2. save_rotated_tokens atomically
            // 3. update in-memory session
            // 4. retry fetch_acl_snapshot once with new token
            // 5. second Unauthenticated → session dead
        }
        Err(e) => Err(e),
    }
}
```

## state_store.rs

```rust
pub fn save_rotated_tokens(
    access_token: String,
    refresh_token: String,
    expires_at: i64,
) -> Result<()>
// Atomically updates the encrypted state store with the rotated token pair.
// Must be called before retry — crash after refresh but before persist
// would otherwise lose the rotated refresh token, breaking the next cycle.
```

## Implementation Checklist

- [x] **M1-B1** `auth.rs` — `refresh_access_token`: POST `/auth/refresh`, Bearer + X-Refresh-Token header
- [x] **M1-B2** `auth.rs` — `RefreshError`: `SessionDead` vs `Transient`
- [x] **M1-B3** `auth.rs` — `logout`: best-effort POST `/auth/logout`; Ok on non-2xx
- [x] **M1-B4** `auth.rs` — `extract_exp`: base64url-decode JWT payload, read `exp` claim
- [x] **M1-B5** `daemon.rs` — `run_refresh_scheduler` background task; proactive rotation 60s before expiry
- [x] **M1-B6** `daemon.rs` — `fetch_acl_snapshot_with_refresh`: single transparent retry on 401
- [x] **M1-B7** `state_store.rs` — `save_rotated_tokens`: atomic encrypted persist of rotated pair
- [x] **M1-B8** `cmd/logout.rs` — call `auth::logout` before clearing local state
- [x] **M1-B9** `main.rs` — wire `auth` module
- [x] **Build gate:** `cd client && cargo build` passes
