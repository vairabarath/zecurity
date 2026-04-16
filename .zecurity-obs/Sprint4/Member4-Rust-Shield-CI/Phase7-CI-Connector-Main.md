---
type: task
status: pending
sprint: 4
member: M4
phase: 7
depends_on:
  - Phase6-Updater-Systemd (install script + units exist)
  - "M3 Phase 5: connector/src/agent_server.rs committed (ShieldServer struct public)"
unlocks:
  - Sprint 4 fully complete (Shield binary releases + Connector serves :9091)
tags:
  - ci
  - github-actions
  - rust
  - connector
---

# M4 · Phase 7 — CI Workflow + Connector main.rs

**Final M4 phase. Depends on all previous phases done + M3's agent_server.rs.**

---

## Goal

Create the GitHub Actions workflow for Shield binary releases. Modify `connector/src/main.rs` to start the Shield-facing gRPC server on :9091.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `.github/workflows/shield-release.yml` | CREATE |
| `connector/src/main.rs` | MODIFY (start ShieldServer on :9091) |

---

## Checklist

### shield-release.yml

- [ ] Trigger: `push: tags: ['shield-v*']`
- [ ] Job: `ubuntu-latest`
- [ ] Steps:
  1. `actions/checkout@v4`
  2. `dtolnay/rust-toolchain@stable`
  3. `cargo install cross --git https://github.com/cross-rs/cross`
  4. Build amd64: `cross build --manifest-path shield/Cargo.toml --release --target x86_64-unknown-linux-musl`
  5. Build arm64: `cross build --manifest-path shield/Cargo.toml --release --target aarch64-unknown-linux-musl`
  6. Rename binaries: `shield-linux-amd64`, `shield-linux-arm64`
  7. Checksums: `sha256sum shield-linux-amd64 shield-linux-arm64 > checksums.txt`
  8. Release with `softprops/action-gh-release@v1`:
     - `shield-linux-amd64`
     - `shield-linux-arm64`
     - `checksums.txt`
     - `shield/scripts/shield-install.sh`
     - `shield/systemd/zecurity-shield.service`
     - `shield/systemd/zecurity-shield-update.service`
     - `shield/systemd/zecurity-shield-update.timer`

> Pattern: identical to `.github/workflows/connector-release.yml`. Use `--manifest-path shield/Cargo.toml` instead of `connector/Cargo.toml`.

### connector/src/main.rs (MODIFY)

> **Coordination with M3:** M3 owns `agent_server.rs`. Agree on `ShieldServer::new()` signature before modifying main.rs.

- [ ] Import `agent_server::ShieldServer` (after M3 has committed the file)
- [ ] After loading `ConnectorState` (which has `trust_domain` and `connector_id`):
  ```rust
  let shield_server = ShieldServer::new(
      controller_channel.clone(),   // existing mTLS channel to Controller
      state.trust_domain.clone(),
      state.connector_id.clone(),
  );
  let shield_server_ref = Arc::new(shield_server);
  ```
- [ ] Spawn Shield gRPC server on :9091:
  ```rust
  let shield_ref = shield_server_ref.clone();
  tokio::spawn(async move {
      // Build mTLS TLS config (Connector's cert, workspace_ca as trust root)
      let tls = ServerTlsConfig::new()
          .identity(Identity::from_pem(&connector_crt, &connector_key))
          .client_ca_root(Certificate::from_pem(&workspace_ca))
          .client_auth_optional(false);  // require Shield client cert
      
      Server::builder()
          .tls_config(tls).expect("Shield server TLS config failed")
          .add_service(ShieldServiceServer::new(shield_ref))
          .serve("0.0.0.0:9091".parse().unwrap())
          .await
          .expect("Shield gRPC server failed");
  });
  ```
- [ ] Pass `shield_server_ref` to `heartbeat::run_heartbeat()` so it can include `get_alive_shields()` in HeartbeatRequest
- [ ] Log `info!("Shield gRPC server starting on :9091")`

---

## Release Steps

```bash
# After all phases complete and build is clean:
git tag shield-v0.1.0
git push origin shield-v0.1.0
# GitHub Actions builds and uploads release assets
```

---

## Build Check

```bash
# Connector with agent_server:
cd connector && cargo build
# Shield-facing server starts and binds :9091

# CI simulation:
cross build --manifest-path shield/Cargo.toml --release --target x86_64-unknown-linux-musl
```

---

## Notes

- The `cross build --manifest-path` approach (used for connector) mounts the full repo context, so `../proto/shield/v1/shield.proto` is accessible inside the Docker container.
- The Shield gRPC server on :9091 uses the **Connector's** cert for the server-side of mTLS (not the Shield's cert). Shields authenticate themselves as mTLS clients.
- If `controller_channel` is not yet built at startup (unlikely but possible on first boot), the RenewCert proxy will fail gracefully — log error and return gRPC UNAVAILABLE.

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase5-AgentServer-Rust]] — M3's agent_server.rs being started here
- [[Sprint4/Member4-Rust-Shield-CI/Phase4-Heartbeat-Renewal]] — shield_server_ref.get_alive_shields() used in heartbeat
