---
type: service
status: active
language: Rust
entry: connector/src/main.rs
related:
  - "[[Services/Controller]]"
  - "[[Services/PKI]]"
tags:
  - rust
  - tls
  - spiffe
  - grpc
  - mtls
---

# Connector (Rust)

The Linux agent. Enrolls with the controller, maintains mTLS heartbeat, auto-renews its certificate, and self-updates its binary.

---

## Module Map

```
main.rs
  ├── config.rs      figment config (env + TOML file)
  ├── appmeta.rs     SPIFFE constants (mirrors Go appmeta.go)
  ├── crypto.rs      EC P-384 keygen, CSR builder, PEM/DER helpers
  ├── enrollment.rs  JWT + CSR → receive cert, save state
  ├── heartbeat.rs   mTLS loop, rebuild channel after renewal
  ├── renewal.rs     RenewCert RPC, save new cert + CA chain
  ├── updater.rs     GitHub release binary self-update
  ├── tls.rs         SPIFFE preflight verification
  └── util.rs        hostname, misc helpers
```

---

## Startup Flow

```
1. Load config (CONTROLLER_ADDR, ENROLLMENT_TOKEN, STATE_DIR, ...)
2. Check state.json exists?
   ├── No  → enrollment::enroll() → save state → proceed
   └── Yes → load saved state
3. Spawn heartbeat::run_heartbeat()  [tokio task, runs forever]
4. Spawn updater::run_update_loop()  [if auto_update_enabled]
5. Wait for Ctrl+C → graceful shutdown (abort tasks)
```

---

## Enrollment (`enrollment.rs`)

1. `crypto::generate_keypair()` — EC P-384 via rcgen
2. `crypto::save_private_key()` — write `connector.key` (mode 0600)
3. `crypto::build_csr()` — PKCS#10 with SPIFFE URI SAN
4. Plain TLS connection to controller (no client cert yet)
5. `Enroll` RPC: enrollment JWT + CSR DER
6. Save `connector.crt`, `workspace_ca.crt`, `state.json`

---

## Heartbeat Loop (`heartbeat.rs`)

1. SPIFFE preflight: raw TLS → verify controller SPIFFE identity
2. Build tonic mTLS channel (`connector.crt` + `workspace_ca.crt`)
3. Loop every `heartbeat_interval_secs`:
   - Send `HeartbeatRequest` (connector_id, version, hostname, public_ip)
   - On `re_enroll=true` → call `renewal::renew_cert()` → rebuild channel
   - On error → exponential backoff (5s → 60s cap)

---

## Cert Renewal (`renewal.rs`)

Triggered when controller sends `re_enroll=true` in heartbeat response.

1. Read `connector.key` from disk
2. `crypto::extract_public_key_der()` — build PKCS#10 CSR (proof of possession)
3. Build separate mTLS channel (existing cert still valid)
4. `RenewCert` RPC: connector_id + CSR DER
5. Save new `connector.crt`
6. Save updated `workspace_ca.crt` (CA chain)
7. `crypto::parse_cert_not_after()` — parse new expiry from PEM
8. Update `state.json` with new `cert_not_after`
9. Return to heartbeat loop → rebuild main mTLS channel with new cert

---

## State Files

| File | Content | When Written |
|------|---------|-------------|
| `connector.key` | EC P-384 PEM (mode 0600) | Enrollment only |
| `connector.crt` | SPIFFE leaf cert PEM | Enrollment + every renewal |
| `workspace_ca.crt` | CA trust chain PEM | Enrollment + every renewal |
| `state.json` | connector_id, trust_domain, workspace_id, enrolled_at, cert_not_after | Enrollment + every renewal |

---

## Key Config Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CONTROLLER_ADDR` | required | `host:9090` |
| `ENROLLMENT_TOKEN` | required (first run) | JWT for enrollment |
| `STATE_DIR` | `/var/lib/zecurity` | Where state files live |
| `HEARTBEAT_INTERVAL_SECS` | `30` | Heartbeat frequency |
| `LOG_LEVEL` | `info` | tracing log level |
| `AUTO_UPDATE_ENABLED` | `true` | Binary self-update |

---

## Release

Built with `cross` (musl static linking) via GitHub Actions workflow (`.github/workflows/connector-release.yml`).

Triggered by tags matching `connector-v*`.

**Assets per release:**
- `connector-linux-amd64` — x86_64 musl
- `connector-linux-arm64` — aarch64 musl
- `connector-install.sh` — one-line install + enrollment script
- Systemd service + timer units
