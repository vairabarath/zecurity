---
type: planning
status: active
tags:
  - roadmap
  - planning
---

# Roadmap

---

## Current State (2026-04-16)

Three sprints complete. The connector is fully operational with automatic cert renewal. Traffic proxying is the next major feature.

---

## Sprint Status

### ✅ Sprint 1 — Foundation (Complete)
- Google OAuth + JWT auth
- Workspace management (create, lookup)
- Admin UI (React + Apollo)
- PostgreSQL schema (users, workspaces)

### ✅ Sprint 2 — Connector (Complete)
- 3-tier PKI (Root → Intermediate → Workspace CA)
- Connector enrollment (JWT + CSR + SPIFFE cert)
- mTLS heartbeat loop (SPIFFE interceptor)
- Disconnect watcher
- Binary auto-update (GitHub releases)
- One-line install script + systemd units
- Admin UI: connector provisioning + enrollment tokens

### ✅ Sprint 3 — Cert Renewal (Complete)
- `RenewCert` gRPC RPC (mTLS, proof-of-possession CSR)
- Controller heartbeat signals `re_enroll=true` within renewal window
- Connector auto-renews cert, saves new bundle, rebuilds mTLS channel
- Zero downtime, zero admin action
- `CONNECTOR_RENEWAL_WINDOW` env var (default 48h)


---

## Phase 6 — End-to-End Renewal Test (In Progress)

Testing cert renewal with short TTLs:

```env
CONNECTOR_CERT_TTL=3m
CONNECTOR_RENEWAL_WINDOW=2m
CONNECTOR_HEARTBEAT_INTERVAL=5s
```

**Expected timeline:**
```
0:00  Enroll → 3-minute cert issued
1:00  Heartbeat → 2 min left → re_enroll=true
1:00  Connector calls RenewCert → new 3-minute cert
1:00  mTLS channel rebuilt with new cert
3:00  Old cert expires → doesn't matter, already renewed
```

**Binary:** `connector-v0.2.0` (GitHub Actions build via `connector-v*` tag)

---

## Deferred Features

| Feature | Reason Deferred |
|---------|----------------|
| CRL / OCSP revocation | DB status flag is sufficient for now |
| Agent cert renewal | Same pattern as connector, next sprint |
| Client cert renewal | Same pattern, future sprint |
| Renewal failure alerting | Connector retries on next heartbeat |
| Admin notification on renewal | No UI change needed, fully automatic |
