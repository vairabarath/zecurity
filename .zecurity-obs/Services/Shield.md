---
type: service
status: in-progress
language: Rust
entry: shield/src/main.rs
sprint: 4
related:
  - "[[Services/Connector]]"
  - "[[Services/Controller]]"
  - "[[Services/PKI]]"
tags:
  - rust
  - grpc
  - tls
  - spiffe
  - nftables
  - shield
---

# Shield (Rust)

The resource host agent. Enrolled by admin, heartbeats through Connector, creates `zecurity0` TUN interface + base nftables table. Protects resources from unauthorized access.

> **Sprint 4 status:** In development. See [[Sprint4/path.md]] for execution plan.

---

## Role in Architecture

```
Admin Dashboard
    │  GraphQL (generate token)
    ▼
Controller :9090 ←── Shield (Enroll, plain TLS + JWT)
                         │
Connector :9091 ←────────┘  (Heartbeat, RenewCert, Goodbye — mTLS)
    │  ShieldHealth in HeartbeatRequest
    ▼
Controller :9090 (learns Shield status via Connector's heartbeat)
```

Shield never heartbeats to Controller directly. All post-enrollment communication goes through Connector.

---

## SPIFFE Identity

```
spiffe://ws-<slug>.zecurity.in/shield/<shield-id>
```

- **Trust domain:** Same workspace trust domain as Connector
- **Role path:** `shield` (cf. `connector` for Connectors)
- **CN:** `shield-<shield-id>`
- **Cert TTL:** 7 days (default), auto-renewed when < 48h remaining

---

## Module Map

```
main.rs
  ├── config.rs      figment config (env + TOML at /etc/zecurity/shield.conf)
  ├── appmeta.rs     SPIFFE + PKI constants (mirrors connector/src/appmeta.rs)
  ├── crypto.rs      EC P-384 keygen, CSR builder, PEM/DER helpers
  ├── enrollment.rs  JWT verification + CA fingerprint + Enroll RPC + state.json
  ├── heartbeat.rs   mTLS loop to Connector :9091, SIGTERM Goodbye
  ├── renewal.rs     RenewCert RPC (proof-of-possession), saves new cert
  ├── network.rs     zecurity0 TUN interface + nftables base table  ← UNIQUE
  ├── updater.rs     GitHub release binary self-update (shield-v* tags)
  ├── tls.rs         verify Connector SPIFFE ID during mTLS handshake
  └── util.rs        hostname, public IP, misc helpers
```

---

## Startup Flow

```
1. Load config (/etc/zecurity/shield.conf + env)
2. state.json exists?
   ├── No  → enrollment::enroll() → save state + certs + network setup
   └── Yes → load ShieldState
3. tokio::spawn(heartbeat::run(state, cfg))
4. tokio::spawn(updater::run(cfg))     [if auto_update_enabled]
5. Wait for SIGTERM
6. heartbeat::goodbye(&state)          [best-effort]
7. Graceful shutdown
```

---

## Enrollment Flow

```
1. Parse JWT (base64 decode) → extract shield_id, workspace_id, trust_domain,
   ca_fingerprint, connector_id, connector_addr, interface_addr
2. GET http://<CONTROLLER_HTTP_ADDR>/ca.crt → verify SHA-256 == ca_fingerprint
3. Generate EC P-384 keypair → save shield.key (mode 0600)
4. Build CSR: CN=shield-<id>, SPIFFE SAN=spiffe://<trust_domain>/shield/<id>
5. Connect to Controller :9090 (plain TLS)
6. Enroll RPC → receive cert + CA chain + interface_addr + connector_addr
7. Save shield.crt, workspace_ca.crt, state.json
8. network::setup(interface_addr, connector_addr) → zecurity0 + nftables
```

---

## Heartbeat Loop

```
mTLS to Connector :9091 every SHIELD_HEARTBEAT_INTERVAL_SECS (30s)
  Client cert: shield.crt
  Trust root:  workspace_ca.crt
  Post-handshake: verify Connector SPIFFE ID = spiffe://<td>/connector/<connector_id>

HeartbeatRequest { shield_id, version, hostname, public_ip }
HeartbeatResponse { ok, re_enroll }
  → re_enroll=true → call renewal::renew_cert()
  → error → exponential backoff (5s→60s cap)
```

---

## Network Setup (unique to Shield)

Called once after enrollment. Requires `CAP_NET_ADMIN`.

```bash
# Creates:
zecurity0    TUN interface with assigned /32 from 100.64.0.0/10
table inet zecurity {
  chain input {
    type filter hook input priority 0; policy accept;
    iif "lo" accept
    ip saddr <connector_ip> accept
    iif "zecurity0" drop         # default DROP until Sprint 5 resource rules
  }
}
```

Sprint 5 will add per-resource ACCEPT rules to this table.

**Implementation note:**
- Interface creation/address/up uses `rtnetlink` directly from the daemon.
- Firewall rules are constructed with the `nftables` Rust crate.
- The current `nftables` crate helper still invokes the system `nft` executable when applying the ruleset, so the resource host still needs `nft` installed.

---

## State Files

| File | Content | When Written |
|------|---------|-------------|
| `shield.key` | EC P-384 PEM (mode 0600) | Enrollment only |
| `shield.crt` | SPIFFE leaf cert PEM (7-day) | Enrollment + every renewal |
| `workspace_ca.crt` | CA trust chain PEM | Enrollment + every renewal |
| `state.json` | shield_id, trust_domain, connector_id, connector_addr, interface_addr, enrolled_at, cert_not_after | Enrollment + every renewal |

**State directory:** `/var/lib/zecurity-shield/`

---

## Config Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CONTROLLER_ADDR` | required | `host:9090` — enrollment target |
| `CONTROLLER_HTTP_ADDR` | required | `host:8080` — CA cert bootstrap |
| `ENROLLMENT_TOKEN` | required (first run) | JWT for enrollment |
| `SHIELD_HEARTBEAT_INTERVAL_SECS` | `30` | Heartbeat frequency |
| `AUTO_UPDATE_ENABLED` | `false` | Binary self-update |
| `LOG_LEVEL` | `info` | tracing log level |

---

## Release

Built with `cross` (musl static linking) via GitHub Actions (`.github/workflows/shield-release.yml`).

Triggered by tags matching `shield-v*`.

**Assets per release:**
- `shield-linux-amd64` — x86_64 musl
- `shield-linux-arm64` — aarch64 musl
- `shield-install.sh` — one-line install + enrollment script
- `zecurity-shield.service` + update units

## Install Script Behavior

`shield-install.sh` is responsible for preparing the host so the Shield binary can assume the required runtime pieces exist.

- Detects distro from `/etc/os-release`
- Checks kernel version is at least `3.13` for nftables support
- Installs the `nftables` package if `nft` is missing on supported distros
- Warns if the system `nftables` service is active, since host-level firewall boot logic can conflict with the Shield-managed `inet zecurity` table
- Leaves `network.rs` free to assume `nft` exists, while still making startup idempotent so rules are recreated after reboot or firewall flush

---

## Dependencies

- `[[Services/Connector]]` — Shield heartbeats and renews through Connector :9091
- `[[Services/Controller]]` — Enrollment only
- `[[Services/PKI]]` — Issues Shield certs (same CA hierarchy as Connector)
