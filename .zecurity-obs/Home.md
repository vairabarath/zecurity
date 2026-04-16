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
- [[Architecture/System Overview]] — full system: controller, connector, shield, databases, admin UI
- [[Architecture/Connector Lifecycle]] — enrollment → heartbeat → cert renewal flow

### Services
- [[Services/Controller]] — Go backend (HTTP :8080 + gRPC :9090)
- [[Services/Connector]] — Rust agent (enrollment, heartbeat, cert renewal, auto-update, Shield aggregation :9091)
- [[Services/Shield]] — Rust resource host agent (enrollment, heartbeat via Connector, zecurity0 + nftables) 🚧 Sprint 4
- [[Services/PKI]] — 3-tier CA hierarchy (Root → Intermediate → Workspace CA)
- [[Services/Auth]] — Google OAuth + JWT session management

### Planning
- [[Planning/Roadmap]] — sprint status, current priorities, what's next
- [[Planning/Session Log]] — running log of all work sessions (read this first)

### Sprint 4 (Active)
- [[Sprint4/team-workflow.md]] — **How to start a session** (AI tool onboarding guide for team members)
- [[Sprint4/path.md]] — **Dependency map + progress checkboxes** (check before any code)
- [[Sprint4/Member1-Frontend/Phase1-Layout-Routing]] — M1 phases (React + GraphQL)
- [[Sprint4/Member2-Go-Proto-Shield/Phase1-Proto-appmeta]] — M2 phases (Proto + Shield + PKI)
- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]] — M3 phases (DB + GraphQL + Connector)
- [[Sprint4/Member4-Rust-Shield-CI/Phase1-Crate-Scaffold]] — M4 phases (Shield binary + CI)

---

## System at a Glance

```
Admin UI (React)
    │  HTTPS GraphQL
    ▼
Controller (Go)  ←──────────────────── Connector (Rust)
  HTTP :8080                              mTLS gRPC :9090
  gRPC :9090                              SPIFFE identity
    │                                      │
    │                                      │ :9091 Shield-facing gRPC (Sprint 4)
    │                                      ▼
    │                                   Shield (Rust)
    │                                   mTLS to Connector
    │                                   zecurity0 + nftables
    ├── PostgreSQL  (workspaces, connectors, shields, CA keys)
    └── Redis       (enrollment JTIs, auth sessions)
```

### Certificate Hierarchy

```
Root CA (10yr, MaxPathLen=2)
  └── Intermediate CA (5yr, MaxPathLen=1)
        └── Workspace CA (2yr, per-tenant, AES-GCM encrypted key)
              ├── Connector Cert (7-day, SPIFFE SAN, auto-renewed)
              ├── Shield Cert    (7-day, SPIFFE SAN, auto-renewed via Connector)
              └── Controller TLS (ephemeral, SPIFFE SAN)
```

### SPIFFE Identity Format

```
Connector:   spiffe://<trust_domain>/connector/<connector_id>
Shield:      spiffe://<trust_domain>/shield/<shield_id>
Controller:  spiffe://<trust_domain>/controller
```

---

## Sprint Status

| Sprint   | Status | What Was Built |
| -------- | ------ | -------------- |
| Sprint 1 | ✅ Done | Auth (Google OAuth + JWT), workspace management, admin UI |
| Sprint 2 | ✅ Done | PKI (3-tier CA), connector enrollment, heartbeat, mTLS, SPIFFE |
| Sprint 3 | ✅ Done | Automatic cert renewal (RenewCert RPC, channel rebuild) |
| Sprint 4 | 🚧 Active | Shield deployment (resource host agent, zecurity0, nftables) |
| Sprint 5 | 📋 Planned | Resource discovery + per-resource nftables rules |

---

## Coordination

All agents: read `agent.md` at project root before any session.
All changes to architecture: update the canvas files in `Architecture/`.
All sessions: append an entry to [[Planning/Session Log]].
Sprint 4 work: check [[Sprint4/path.md]] before touching any file.
