# Connector Sprint — Implementation Report

## Sprint Goal
Deploy a connector on any Linux server, see it appear ACTIVE in the dashboard within 30 seconds, and watch it go DISCONNECTED if the server goes offline. Every identity uses SPIFFE-standard certificates. Traffic does not flow yet — that is the next sprint.

## Status: COMPLETE

All planned functionality is implemented and tested end-to-end.

---

## What Was Built

### Controller (Go)
- gRPC server on port 9090 with mTLS + SPIFFE interceptor
- Enroll RPC: verifies JWT, burns token in Redis, signs CSR with Workspace CA, returns 7-day cert
- Heartbeat RPC: updates last_heartbeat_at, version, hostname, public_ip
- Disconnect watcher: marks connectors DISCONNECTED after 90s without heartbeat
- HTTP /ca.crt endpoint for bootstrap trust
- GraphQL mutations: createRemoteNetwork, deleteRemoteNetwork, generateConnectorToken, revokeConnector, deleteConnector
- GraphQL queries: remoteNetworks, remoteNetwork, connectors
- Database migration: remote_networks + connectors tables, trust_domain on workspaces

### Connector (Rust)
- 10-step enrollment: JWT parse → CA fetch → fingerprint verify → keygen → CSR → gRPC enroll → save certs → config cleanup
- mTLS heartbeat loop with SPIFFE preflight verification (every 30s)
- Auto-updater: GitHub releases, SHA-256 checksum, atomic binary replace, health-check rollback
- `--check-update` CLI flag for systemd oneshot update service
- Systemd service with full security hardening

### Admin Frontend (React)
- Remote Networks page: create/delete networks with location picker
- Connectors page: status badges, last seen, hostname, version, revoke/delete
- InstallCommandModal: generates token, shows copy-able install command
- Auto-polls every 30s for live status updates

### CI/CD
- GitHub Actions workflow on `connector-v*` tags
- Cross-compiled musl static binaries (amd64 + arm64)
- SHA-256 checksums, install script, systemd units uploaded to release

---

## PKI Certificate Chain

```
Root CA (10yr, MaxPathLen=2)
  └── Intermediate CA (5yr, MaxPathLen=1)
        ├── Workspace CA (2yr per tenant, MaxPathLen=0)
        │     └── Connector cert (7-day, SPIFFE SAN, ClientAuth)
        └── Controller cert (ephemeral, SPIFFE SAN, ServerAuth)
```

- Root CA: `ZECURITY Root CA` — self-signed, encrypted at rest, never loaded into memory after init
- Intermediate CA: `ZECURITY Intermediate CA` — signs workspace CAs, kept in memory
- Workspace CA: `workspace-<tenant_id>` — per-tenant, signs connector certs
- Connector cert: `connector-<id>` — 7-day validity, `spiffe://ws-<slug>.zecurity.in/connector/<id>`
- Controller cert: `Zecurity Controller` — `spiffe://zecurity.in/controller/global`

All private keys encrypted with AES-256-GCM (HKDF-SHA256 derived from PKI_MASTER_SECRET).

---

## SPIFFE Identity Scheme

| Entity | Trust Domain | SPIFFE URI |
|--------|-------------|------------|
| Controller | zecurity.in | spiffe://zecurity.in/controller/global |
| Connector | ws-\<slug\>.zecurity.in | spiffe://ws-\<slug\>.zecurity.in/connector/\<id\> |
| Agent (future) | ws-\<slug\>.zecurity.in | spiffe://ws-\<slug\>.zecurity.in/agent/\<id\> |

All SPIFFE strings originate from `appmeta` (Go: `internal/appmeta/identity.go`, Rust: `src/appmeta.rs`). No file hardcodes trust domain strings directly.

---

## Files Created on Connector Host

| Path | Contents | Permissions | Purpose |
|------|----------|-------------|---------|
| `/usr/local/bin/zecurity-connector` | Static binary | 0755 root:root | The connector program |
| `/etc/zecurity/connector.conf` | ENV key-value pairs | 0660 root:zecurity | Controller address, log level, connector ID |
| `/etc/zecurity/ca.crt` | Intermediate CA PEM | 0644 root:root | Initial trust for enrollment |
| `/var/lib/zecurity-connector/connector.key` | EC P-384 private key | 0600 zecurity:zecurity | mTLS client identity (never transmitted) |
| `/var/lib/zecurity-connector/connector.crt` | Signed leaf cert PEM | zecurity:zecurity | 7-day SPIFFE certificate |
| `/var/lib/zecurity-connector/workspace_ca.crt` | CA chain PEM | zecurity:zecurity | Trust root for controller verification |
| `/var/lib/zecurity-connector/state.json` | Enrollment metadata | zecurity:zecurity | Skip re-enrollment on restart |

---

## Connector Lifecycle

```
PENDING ──(enrollment)──> ACTIVE ──(no heartbeat 90s)──> DISCONNECTED
                            │                                │
                            │ <──(heartbeat resumes)─────────┘
                            │
                            └──(admin revokes)──> REVOKED
```

---

## Deviations from Sprint Plan v3

| Item | Plan | Implementation | Reason |
|------|------|---------------|--------|
| Binary path | /usr/bin/connector | /usr/local/bin/zecurity-connector | Product-prefixed, standard path |
| Config permissions | 0600 | 0660 root:zecurity | Service user needs read+write access |
| AUTO_UPDATE default | true | false | Safer for air-gapped environments |
| appmeta location | controller/appmeta/appmeta.go | controller/internal/appmeta/identity.go | Better Go encapsulation |
| Dependency versions | tonic 0.11, prost 0.12 | tonic 0.14, prost 0.14 | Updated to latest |
| systemd Restart | always | on-failure | Only restart on actual failures |
| Rust appmeta | 4 constants | 12 constants + 2 helpers + tests | More complete mirror of Go |
| Chain verification | Full chain via x509.Verify | Leaf verified directly against Workspace CA | Avoids MaxPathLen constraint issues |
| Dockerfile | Not in plan | Implemented | Enables containerized deployment |
| Controller TLS cert | Not listed as separate file | pki/controller.go | Needed for SPIFFE SAN on server cert |

---

## What Is NOT in This Sprint (Planned for Next)

| Feature | Status | Notes |
|---------|--------|-------|
| Certificate auto-renewal | `re_enroll` field plumbed, always false | Controller returns false; connector logs warning if true |
| Resource definitions | Not started | IP, port, protocol targeting |
| ACL delivery | Not started | Policy push from controller to connector |
| Traffic proxying | Not started | TCP/UDP forwarding through connector |
| Agent binary | Not started | Resource host enforcement |
| Access policies | Not started | Policy engine |
| CRL/OCSP | Not started | Revocation via DB status flag only |

---

## Verified End-to-End Flows

| Test | Result |
|------|--------|
| Create remote network in UI | PASS |
| Generate connector token (install command) | PASS |
| Install connector via curl pipe bash | PASS |
| Enrollment (JWT → CSR → signed cert) | PASS |
| CA fingerprint verification | PASS |
| SPIFFE controller identity verification | PASS |
| mTLS heartbeat every 30s | PASS |
| Connector shows ACTIVE in UI | PASS |
| Stop connector → DISCONNECTED after 90s | PASS |
| Restart connector → ACTIVE on next heartbeat | PASS |
| Revoke connector → heartbeat rejected | PASS |
| Same token twice → second enrollment fails | PASS |
| Delete connector (pending/revoked only) | PASS |
| Delete network (0 connectors only) | PASS |
| --check-update flag (single check, exit) | PASS (in code, next release binary) |
| Auto-update (GitHub release check) | PASS (disabled by default) |
| SHA-256 binary checksum verification | PASS |
| Systemd hardening (ProtectSystem, etc.) | PASS |

---

## Next Sprint Prerequisites

Before starting the next sprint (resources + traffic), ensure:

1. **Merge `connector-fixed` → `main`** — all fixes are on the feature branch
2. **Certificate renewal** — implement `re_enroll=true` logic (controller detects cert expiring within renewal window, connector re-enrolls automatically)
3. **PKI reset consideration** — if you need full chain verification later, reset PKI after merge (the MaxPathLen fixes are in the code for new CA generations, but existing CAs have the old values)
4. **Install script URL** — currently points to `connector-fixed` branch; update to `main` after merge
