# Phase 4 — Rust Connector: Foundation

Create the `connector/` directory with all foundational source files. This is the start of the entire Rust connector binary.

---

## Files to Create

```
connector/Cargo.toml
connector/build.rs
connector/src/appmeta.rs
connector/src/config.rs
```

---

## `connector/src/appmeta.rs` — SPIFFE Constants

Mirrors `controller/internal/appmeta/identity.go` **EXACTLY**:

```rust
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";
pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global";
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";
pub const PRODUCT_NAME: &str = "ZECURITY";
pub const PKI_CONNECTOR_CN_PREFIX: &str = "connector-";
```

These values **MUST** match Member 3's `appmeta.go` constants character-for-character. If Member 3 changes a constant, you must update `appmeta.rs` to match immediately.

---

## `connector/Cargo.toml` — Dependencies

Required dependencies:

- `tokio` — async runtime
- `tonic` — gRPC client
- `rcgen` — certificate/key generation
- `tokio-rustls` — TLS support
- `x509-parser` — X.509 certificate parsing
- `sha2` — SHA-256 for CA fingerprint verification
- `figment` — configuration loading (env vars + config file)
- `semver` — version comparison for auto-updater
- `reqwest` — HTTP client for CA cert fetch + GitHub releases
- `tracing` / `tracing-subscriber` — logging
- `serde` / `serde_json` — serialization

---

## `connector/build.rs` — Proto Compilation

Uses `tonic-build` to compile Member 2's `controller/proto/connector.proto` into Rust stubs.

```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::compile_protos("../controller/proto/connector.proto")?;
    Ok(())
}
```

You consume the proto — you don't write it. If you need a proto change, ask Member 2.

---

## `connector/src/config.rs` — Configuration

Uses `figment` to read:

- **Required (first run):** `CONTROLLER_ADDR`, `ENROLLMENT_TOKEN`
- **Optional with defaults:** `AUTO_UPDATE_ENABLED`, `LOG_LEVEL`, `HEARTBEAT_INTERVAL_SECS`, `UPDATE_CHECK_INTERVAL_SECS`
- Config file: `/etc/zecurity/connector.conf`

---

## Important Rules

1. **`appmeta.rs` must mirror `appmeta.go` exactly.** Character-for-character match on all string values.
2. **You consume the proto, you don't write it.** Member 2 writes `connector.proto`.
3. **`build.rs` needs Member 2's proto committed first.**

---

## Phase 4 Checklist

```
✓ connector/ directory created
✓ Cargo.toml with all required dependencies
✓ build.rs compiles connector.proto via tonic-build
✓ appmeta.rs mirrors Member 3's Go constants exactly
✓ config.rs loads env vars + config file correctly
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 5 (enrollment flow).
