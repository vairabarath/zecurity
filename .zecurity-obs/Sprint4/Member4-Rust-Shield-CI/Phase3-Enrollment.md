---
type: task
status: done
sprint: 4
member: M4
phase: 3
depends_on:
  - Phase2-Core-Modules (crypto.rs, config.rs, util.rs)
  - "M2 Phase 2: Enroll handler live in dev controller"
unlocks:
  - Phase4-Heartbeat-Renewal (enrollment must succeed first)
  - Phase5-Network (network::setup() called from enrollment.rs)
tags:
  - rust
  - shield
  - enrollment
  - pki
---

# M4 · Phase 3 — Enrollment Flow

**Depends on: M2's Enroll handler live in dev. Can scaffold enrollment.rs before M2 is done, but integration test requires M2.**

---

## Goal

Implement the Shield enrollment flow: parse JWT → fetch + verify CA fingerprint → generate EC keypair → build CSR → call Controller Enroll RPC → save certs + state.json → call `network::setup()`.

---

## File to Create

`shield/src/enrollment.rs`

---

## Checklist

### enroll() function

- [ ] `pub async fn enroll(cfg: &ShieldConfig) -> anyhow::Result<ShieldState>`

### Step-by-step flow

- [ ] **Step 1**: Parse JWT payload (base64url decode middle segment, parse JSON — no signature verification in Rust)
  - Extract: `shield_id`, `workspace_id`, `trust_domain`, `ca_fingerprint`, `connector_id`, `connector_addr`, `interface_addr`
  
- [ ] **Step 2**: Bootstrap CA verification
  - `GET http://<CONTROLLER_HTTP_ADDR>/ca.crt` (plain HTTP, not HTTPS)
  - Compute SHA-256 of downloaded CA cert DER bytes
  - Compare hex against `ca_fingerprint` from JWT
  - On mismatch: `error!("CA fingerprint mismatch"); std::process::exit(1)` (do not proceed)

- [ ] **Step 3**: Generate EC P-384 keypair
  - Call `crypto::generate_keypair()`
  - Save `shield.key` to `{state_dir}/shield.key` with mode 0600

- [ ] **Step 4**: Build PKCS#10 CSR
  - CN: `format!("{}{}", appmeta::PKI_SHIELD_CN_PREFIX, shield_id)`
  - SPIFFE URI SAN: `format!("spiffe://{}/{}/{}", trust_domain, appmeta::SPIFFE_ROLE_SHIELD, shield_id)`
  - Call `crypto::build_csr(key, cn, spiffe_uri)`

- [ ] **Step 5**: Connect to Controller via plain TLS (no client cert yet)
  - Use `CONTROLLER_ADDR` from config
  - Trust root: the downloaded CA cert

- [ ] **Step 6**: Call `ShieldService.Enroll` RPC
  - `EnrollRequest { enrollment_token: cfg.enrollment_token, csr_der: csr, version: env!("CARGO_PKG_VERSION"), hostname: util::read_hostname() }`

- [ ] **Step 7**: Process `EnrollResponse`
  - Save `shield.crt` to `{state_dir}/shield.crt`
  - Save `workspace_ca.crt` to `{state_dir}/workspace_ca.crt` (workspace_ca_pem + intermediate_ca_pem concatenated)
  - Parse `cert_not_after` from `certificate_pem`

- [ ] **Step 8**: Write `state.json`
  - `ShieldState { shield_id, trust_domain, connector_id, connector_addr, interface_addr, enrolled_at: now(), cert_not_after }`

- [ ] **Step 9**: Update config file — remove `ENROLLMENT_TOKEN`, write `SHIELD_ID=<id>` to `/etc/zecurity/shield.conf`

- [ ] **Step 10**: Call `network::setup(interface_addr, connector_addr).await`

- [ ] **Step 11**: Log `info!("enrollment complete shield_id={}", shield_id)`

- [ ] **Step 12**: Return `ShieldState`

---

## Integration Test

```bash
# With dev controller running (M2's Enroll handler must be live):
CONTROLLER_ADDR=localhost:9090 \
CONTROLLER_HTTP_ADDR=localhost:8080 \
ENROLLMENT_TOKEN=<generated-from-dashboard> \
cargo run --manifest-path shield/Cargo.toml

# Expected:
# - Shield appears in DB with status='active'
# - state.json written to /var/lib/zecurity-shield/
# - shield.crt, shield.key, workspace_ca.crt written
# - zecurity0 interface visible (ip link show zecurity0)
```

---

## Error Handling

| Error | Action |
|-------|--------|
| CA fingerprint mismatch | `error!` + `exit(1)` — STOP, do not enroll |
| JWT parse failure | `error!` + `exit(1)` |
| Controller returns PERMISSION_DENIED | `error!` + `exit(1)` — token used or expired |
| Network setup fails | `warn!` + continue (network is best-effort for now) |
| Any I/O error | Return `Err(e)` — main will log and exit |

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase5-Network]] — network::setup() called from here
- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — Enroll handler being called
