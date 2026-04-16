---
type: task
status: pending
sprint: 4
member: M4
phase: 1
depends_on:
  - "M2 Phase 1: proto/shield/v1/shield.proto committed"
unlocks:
  - Phase2-Core-Modules
  - All subsequent M4 phases
tags:
  - rust
  - cargo
  - scaffold
  - shield
---

# M4 · Phase 1 — Crate Scaffold (shield/)

**Depends on: `proto/shield/v1/shield.proto` committed by M2.**

---

## Goal

Stand up the new `shield/` Rust crate with all project files so subsequent phases can add source modules without fighting infrastructure.

---

## Files to Create

| File | Purpose |
|------|---------|
| `shield/Cargo.toml` | Crate manifest + all dependencies |
| `shield/build.rs` | tonic proto compilation |
| `shield/Cross.toml` | Cross-compilation pre-build step |
| `shield/Dockerfile` | Docker image for cross-build (mirrors connector) |
| `shield/src/main.rs` | Empty stub (just `fn main() {}`) |

---

## Checklist

### Cargo.toml

- [ ] `[package]`: `name = "zecurity-shield"`, `version = "0.1.0"`, `edition = "2021"`
- [ ] `[dependencies]`:
  - `tokio = { version = "1", features = ["full"] }`
  - `tonic = { version = "0.14", features = ["tls"] }` — match connector's tonic version
  - `prost = "0.14"` — match connector's prost version
  - `rcgen = "0.13"` — EC P-384 key + CSR generation
  - `tokio-rustls = "0.26"`
  - `rustls = "0.23"`
  - `x509-parser = "0.16"` — parse cert not_after from PEM
  - `oid-registry = "0.7"`
  - `sha2 = "0.10"` — CA fingerprint verification
  - `hex = "0.4"`
  - `anyhow = "1"`
  - `tracing = "1"`
  - `tracing-subscriber = "0.3"`
  - `figment = { version = "0.10", features = ["env", "toml"] }`
  - `serde = { version = "1", features = ["derive"] }`
  - `serde_json = "1"`
  - `semver = "1"` — for updater version comparison
  - `reqwest = { version = "0.12", features = ["json", "rustls-tls"] }` — CA cert bootstrap + updater
  - `tokio-retry = "0.3"` — enrollment retry backoff
  - `rtnetlink = "0.14"` — zecurity0 TUN interface creation
  - `nftables = "0.4"` — nftables rules (check crate name on crates.io)
- [ ] `[build-dependencies]`: `tonic-build = "0.14"`

> **Version alignment:** Check `connector/Cargo.toml` for exact versions of tonic, prost, rustls, tokio. Use the same versions to avoid dependency conflicts in a future workspace.

### build.rs

- [ ] Single call:
  ```rust
  fn main() -> Result<(), Box<dyn std::error::Error>> {
      tonic_build::compile_protos("../proto/shield/v1/shield.proto")?;
      Ok(())
  }
  ```

### Cross.toml

- [ ] Mirror `connector/Cross.toml`:
  ```toml
  [build.pre-build]
  cmd = ["apt-get", "install", "-y", "protobuf-compiler"]
  ```

### Dockerfile

- [ ] Mirror `connector/Dockerfile`. Change binary name from `zecurity-connector` to `zecurity-shield`.

### src/main.rs (stub)

- [ ] Minimal stub to verify compilation:
  ```rust
  fn main() {
      println!("zecurity-shield stub");
  }
  ```

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
# Must compile (even if main is just a stub)
# Rust stubs generated from shield.proto (OUT_DIR/shield.v1.rs)
```

---

## Notes

- Check crates.io for actual version availability of `rtnetlink` and `nftables`. The versions in the plan are approximate.
- The `nftables` crate may be named differently — search crates.io for an nftables Rust binding. Alternative: use `std::process::Command` to invoke `nft` CLI if no good crate exists (document the tradeoff in this note).
- Do NOT add this crate to a workspace `Cargo.toml` yet — it's a standalone crate for now. Future sprint may unify.

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase2-Core-Modules]] — next phase
- [[Sprint4/Member2-Go-Proto-Shield/Phase1-Proto-appmeta]] — proto file this build.rs compiles
