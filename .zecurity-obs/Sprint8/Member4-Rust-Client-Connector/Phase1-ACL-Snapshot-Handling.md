---
type: phase
status: done
sprint: 8
member: M4
phase: Phase1-ACL-Snapshot-Handling
depends_on:
  - M2-Phase1-Policy-Schema
  - M3 Compiler Output Contract documented
tags:
  - rust
  - client
  - connector
  - policy-engine
  - acl
---

# M4 Phase 1 — Connector ACL Snapshot Handling

---

## What You're Building

Wire ACL snapshots into the Rust connector. This sprint does not build RDE tunneling; it builds the local policy state RDE will depend on later.

Client active runtime state is daemon-required and moves to Sprint 8.5. See [[Decisions/ADR-002-Client-Daemon-Required]].

---

## Connector Work

Receive ACL snapshot from Connector heartbeat response and keep it locally.

Read the Compiler Output Contract in [[Sprint8/Member3-Go-Controller/Phase1-Policy-Compiler]] before implementing `is_allowed()` or resource resolution helpers.

Create a small policy cache module with helpers like:

```rust
pub fn is_allowed(&self, resource_id: &str, client_spiffe_id: &str) -> bool;
pub fn resolve_resource(&self, address: &str, port: u16, protocol: &str) -> Option<ResourceAcl>;
```

Rules:

- Missing snapshot = deny.
- Missing resource = deny.
- Missing SPIFFE ID = deny.
- Empty `allowed_spiffe_ids` = deny.
- M4 depends on M2's generated proto types and the documented compiler contract, not on M3's compiler implementation being complete.

Files likely touched:

- `connector/src/policy/`
- `connector/src/heartbeat.rs` or equivalent heartbeat client module
- `connector/src/main.rs`

---

## Build Check

```bash
cd connector && cargo build
```

---

## Post-Phase Fixes — Red Team Security Findings (2026-06-23)

> **Status: NOT IMPLEMENTED** — findings documented only; no fixes applied yet.



Security audit of the client daemon after split-tunneling implementation. All findings are against `relay-preparation` branch, commit `e28d7aa` + working tree changes.

### Finding 1 (CRITICAL) `[ NOT IMPLEMENTED ]` — `GetToken` IPC leaks access token to any same-user process

**File:** `client/src/daemon.rs` ~line 269, `client/src/ipc.rs` ~line 20

**Issue:** `IpcRequest::GetToken` returns the raw bearer token over the Unix socket. Only gate is `check_same_user` (same UID). Any process running as that user — malicious extension, tricked binary — can steal the token and impersonate the device.

**Fix:** Remove from IPC surface or require caller to sign a daemon-issued nonce with the device private key to prove it is the legitimate CLI binary.

---

### Finding 2 (CRITICAL) `[ NOT IMPLEMENTED ]` — `PostLoginState` accepts full private key injection from any same-user process

**File:** `client/src/daemon.rs` ~line 291

**Issue:** Any same-user process can POST arbitrary `private_key_pem`, `access_token`, `ca_cert_pem` — replacing the daemon's identity with attacker-controlled credentials, then MITM all QUIC tunnels.

**Fix:** Bind behind a one-time nonce issued only during an active interactive login flow.

---

### Finding 3 (HIGH) `[ NOT IMPLEMENTED ]` — `LoadState` hot-swaps daemon identity from disk at any time

**File:** `client/src/daemon.rs` ~line 250

**Issue:** Any same-user process sends `{"type":"LoadState"}` to reload whatever is on disk. Combined with a writable home directory, allows credential swap mid-session.

**Fix:** Remove hot-reload IPC; require explicit re-authentication.

---

### Finding 4 (HIGH) `[ NOT IMPLEMENTED ]` — nftables table and route table 105 have no watchdog

**File:** `client/src/tun.rs` lines 9-13

**Issue:** Any process with `CAP_NET_ADMIN` runs `nft delete table inet zecurity_client` or `ip route flush table 105` or inserts `ip rule add fwmark 0x5a lookup main priority 48`. ZTNA enforcement silently drops — protected resources become directly accessible. Daemon never detects this.

**Fix:** Verify nft table + ip rule exist every net_stack event loop iteration. Missing → tear down TUN, fail closed.

---

### Finding 5 (HIGH) `[ NOT IMPLEMENTED ]` — `Sync` IPC has no rate limit → controller DoS

**File:** `client/src/daemon.rs` ~line 660

**Issue:** Any same-user process floods `{"type":"Sync"}` → floods the controller gRPC endpoint. Also: revoked devices retain access for up to `ACL_REFRESH_TTL_SECS` (60s) after revocation.

**Fix:** Rate-limit `Sync` to once per N seconds. Controller should push revocation signals rather than relying on client polling.

---

### Finding 6 (HIGH) `[ NOT IMPLEMENTED ]` — `check_same_user` fails open on non-Linux

**File:** `client/src/ipc.rs` ~line 124

**Issue:**
```rust
#[cfg(not(target_os = "linux"))]
pub fn check_same_user(_stream: &UnixStream) -> bool { true }
```
On macOS/BSD any process on the machine has full IPC access including `GetToken`, `PostLoginState`, `Shutdown`.

**Fix:** Implement `SCM_CREDS`/`SO_LOCAL_PEERCRED` on macOS or refuse to bind if peer credential checking is unavailable.

---

### Finding 7 (MEDIUM) `[ NOT IMPLEMENTED ]` — `Shutdown` IPC leaves nftables rules orphaned

**File:** `client/src/daemon.rs` ~line 154, `client/src/tun.rs` `Drop` impl line 169

**Issue:** Any same-user process kills the daemon via `Shutdown`. `Drop` only drops the TUN device — does not call `cleanup_policy_routes()`. nft mark rules + ip rule for table 105 remain. Kernel marks flows `0x5a` but has no device to deliver to — connections to protected resources silently drop.

**Fix:** SIGTERM handler calls `cleanup_policy_routes()`. `Drop` impl also attempts synchronous cleanup.

---

### Finding 8 (MEDIUM) `[ NOT IMPLEMENTED ]` — Encryption key stored adjacent to ciphertext

**File:** `client/src/state_store.rs` ~line 231

**Issue:** `.{slug}.key` and `{slug}.json` live in the same `~/.local/share/zecurity-client/` directory, both `0o600`. Any backup agent or home directory traversal reads both — AES-256-GCM is equivalent to plaintext for offline attack.

**Fix:** Seal the key in the Linux kernel keyring (`keyctl`) or TPM rather than the filesystem.

---

### Finding 9 (MEDIUM) `[ NOT IMPLEMENTED ]` — JWT claims decoded without signature verification

**File:** `client/src/state_store.rs` ~line 539

**Issue:** `decode_claims()` base64-decodes the JWT payload with no signature validation. Combined with Finding 2, attacker crafts arbitrary `sub`/`tenant_id`/`role` claims.

**Fix:** Validate JWT signature against the CA public key before trusting claims.

---

### Finding 10 (LOW) `[ NOT IMPLEMENTED ]` — `ZECURITY_DAEMON_SOCKET` env var allows socket path override

**File:** `client/src/ipc.rs` ~line 92

**Issue:** If attacker controls the daemon's environment (systemd unit override, `/etc/environment`), they redirect IPC to a fake daemon returning crafted responses.

**Fix:** Hardcode socket path in production; do not inherit from environment.

---

## Deferred To Sprint 8.5

Client work moves to the daemon foundation:

- daemon-required IPC
- systemd user unit
- command refactor
- `GetACLSnapshot` fetch into daemon runtime state
- no optional direct-state fallback
