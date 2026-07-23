---
type: task
status: done
sprint: 4
member: M4
phase: 2
depends_on:
  - Phase1-Crate-Scaffold (crate compiles)
  - "M2 Phase 1: appmeta constants finalized"
unlocks:
  - Phase3-Enrollment
  - Phase4-Heartbeat-Renewal
  - Phase5-Network
tags:
  - rust
  - shield
  - core
---

# M4 · Phase 2 — Core Modules

**Depends on: crate scaffold compiles + appmeta constants from M2.**

---

## Goal

Implement the core infrastructure modules: appmeta constants, config loading, main.rs startup flow, crypto helpers, TLS verification, and utilities.

---

## Files to Create

| File | Purpose |
|------|---------|
| `shield/src/appmeta.rs` | SPIFFE + PKI constants (mirrors connector) |
| `shield/src/config.rs` | figment config loader |
| `shield/src/main.rs` | Full startup flow |
| `shield/src/crypto.rs` | EC P-384 keygen, CSR, PEM/DER helpers |
| `shield/src/tls.rs` | Connector SPIFFE verification |
| `shield/src/util.rs` | hostname, public IP helpers |

---

## Checklist

### appmeta.rs

- [x] Mirror `connector/src/appmeta.rs` exactly for shared constants
- [x] Add Shield-specific constants:
  ```rust
  pub const SPIFFE_ROLE_SHIELD: &str     = "shield";
  pub const SPIFFE_ROLE_CONNECTOR: &str  = "connector";
  pub const PKI_SHIELD_CN_PREFIX: &str   = "shield-";
  pub const SHIELD_INTERFACE_NAME: &str  = "zecurity0";
  pub const SHIELD_INTERFACE_CIDR_RANGE: &str = "100.64.0.0/10";
  pub const PRODUCT_NAME: &str           = "ZECURITY";
  pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";
  pub const SPIFFE_CONTROLLER_ID: &str   = "spiffe://zecurity.in/controller/global";
  ```
- [x] Verify constants match Go `appmeta/identity.go` exactly (same strings)

### config.rs

- [x] `ShieldConfig` struct with `#[derive(Deserialize)]`
- [x] Required fields: `controller_addr: String`, `controller_http_addr: String`, `enrollment_token: Option<String>`
- [x] Optional with defaults: `auto_update_enabled: bool` (false), `log_level: String` ("info"), `shield_heartbeat_interval_secs: u64` (30)
- [x] State dir: `state_dir: String` (default: `/var/lib/zecurity-shield`)
- [x] `pub fn load() -> anyhow::Result<ShieldConfig>` using figment env + TOML
- [x] Config file: `/etc/zecurity/shield.conf` (TOML format)

### main.rs (full startup)

- [x] Init tracing (from `LOG_LEVEL`)
- [x] Load config via `config::load()`
- [x] Check if `state.json` exists in state_dir:
  - Not exists → run `enrollment::enroll(&cfg).await`
  - Exists → load `ShieldState` from `state.json`
- [x] `tokio::spawn(heartbeat::run(state.clone(), cfg.clone()))` 
- [x] If `auto_update_enabled`: `tokio::spawn(updater::run(cfg.clone()))`
- [x] Wait for SIGTERM signal
- [x] On SIGTERM: call `heartbeat::goodbye(&state, &cfg).await` (best-effort)
- [x] Graceful shutdown

### crypto.rs

- [x] Mirror `connector/src/crypto.rs` with same functions:
  - `generate_keypair() -> anyhow::Result<rcgen::Certificate>`
  - `save_private_key(key: &str, path: &Path) -> anyhow::Result<()>` (mode 0600)
  - `build_csr(key, cn, spiffe_uri) -> anyhow::Result<Vec<u8>>` (DER-encoded PKCS#10)
  - `parse_cert_not_after(pem: &[u8]) -> anyhow::Result<DateTime<Utc>>`
  - `pem_to_der(pem: &[u8]) -> anyhow::Result<Vec<u8>>`

### tls.rs

- [x] `verify_connector_spiffe(cert_der: &[u8], expected_spiffe_id: &str) -> anyhow::Result<()>`
  - Parse cert with `x509-parser`
  - Extract URI SAN
  - Compare against `expected_spiffe_id`
  - Return error if mismatch
- [x] The expected ID is built in `heartbeat.rs` (now `control_stream.rs`):
  ```rust
  let expected = format!("spiffe://{}/{}/{}",
      state.trust_domain,
      appmeta::SPIFFE_ROLE_CONNECTOR,
      state.connector_id);
  ```

### util.rs

- [x] `read_hostname() -> String` — reads `/etc/hostname` or `hostname()` syscall
- [x] `get_public_ip() -> Option<String>` — HTTP GET to IP echo service (use `reqwest`)
- [x] `sha256_hex(data: &[u8]) -> String` — for CA fingerprint verification

### ShieldState struct (in main.rs or a types.rs)

- [x] `#[derive(Serialize, Deserialize, Clone)]`
- [x] Fields: `shield_id`, `trust_domain`, `connector_id`, `connector_addr`, `interface_addr`, `enrolled_at`, `cert_not_after`
- [x] `fn load(state_dir: &str) -> anyhow::Result<ShieldState>`
- [x] `fn save(&self, state_dir: &str) -> anyhow::Result<()>`

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
# All core modules compile cleanly
# No unused import warnings that would block future phases
```

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase3-Enrollment]] — uses crypto.rs + config.rs
- [[Sprint4/Member4-Rust-Shield-CI/Phase4-Heartbeat-Renewal]] — uses tls.rs + util.rs
