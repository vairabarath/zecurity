---
type: index
tags:
  - home
  - index
---

# Zecurity Knowledge Base

> Shared brain for all agents working on this project.
> Read [[agent.md]] (project root) before starting any session.

---

## Navigation

### Architecture
- [[Architecture/System Overview]] — full system: controller, connector, databases, admin UI
- [[Architecture/Connector Lifecycle]] — enrollment → heartbeat → cert renewal flow

### Services
- [[Services/Controller]] — Go backend (HTTP :8080 + gRPC :9090)
- [[Services/Connector]] — Rust agent (enrollment, heartbeat, cert renewal, auto-update)
- [[Services/PKI]] — 3-tier CA hierarchy (Root → Intermediate → Workspace CA)
- [[Services/Auth]] — Google OAuth + JWT session management

### Planning
- [[Planning/Roadmap]] — sprint status, current priorities, what's next
- [[Planning/Session Log]] — running log of all work sessions (read this first)

---

## System at a Glance

```
Admin UI (React)
    │  HTTPS GraphQL
    ▼
Controller (Go)  ←────────────────────  Connector (Rust)
  HTTP :8080                              mTLS gRPC :9090
  gRPC :9090                              SPIFFE identity
    │
    ├── PostgreSQL  (workspaces, connectors, CA keys)
    └── Redis       (enrollment JTIs, auth sessions)
```

### Certificate Hierarchy

```
Root CA (10yr, MaxPathLen=2)
  └── Intermediate CA (5yr, MaxPathLen=1)
        └── Workspace CA (2yr, per-tenant, AES-GCM encrypted key)
              └── Connector Cert (7-day, SPIFFE SAN, auto-renewed)
                   └── Controller TLS (ephemeral, SPIFFE SAN)
```

### SPIFFE Identity Format

```
Connector:   spiffe://<trust_domain>/connector/<connector_id>
Controller:  spiffe://<trust_domain>/controller
```

---

## Completed Sprints

| Sprint   | Status | What Was Built                                                 |
| -------- | ------ | -------------------------------------------------------------- |
| Sprint 1 | ✅ Done | Auth (Google OAuth + JWT), workspace management, admin UI      |
| Sprint 2 | ✅ Done | PKI (3-tier CA), connector enrollment, heartbeat, mTLS, SPIFFE |
| Sprint 3 | ✅ Done | Automatic cert renewal (RenewCert RPC, channel rebuild)        |

---

## Coordination

All agents: read `agent.md` at project root before any session.
All changes to architecture: update the canvas files in `Architecture/`.
All sessions: append an entry to [[Planning/Session Log]].
