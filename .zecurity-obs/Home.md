---
type: index
tags:
  - home
  - index
---

# Zecurity Knowledge Base

> Shared brain for all agents working on this project.
> Read [[agent.md]] (project root) before starting any session.

> [!warning] This index stopped being maintained after Sprint 6
> The navigation and sprint table below were last updated during Sprint 6 and
> are **not** a current status source. **Sprint 19 is the active sprint.**
> For current status use, in order:
> - `.zecurity-obs/pending/README.md` — the accurate status index (which PENDING items are built)
> - [[Sprint19/path]] — the active sprint's dependency map and progress tracker
> - [[Planning/Session Log]] — what was actually done, session by session

---

## Navigation

### Architecture
- [[Architecture/System Overview]] — full system: controller, connector, shield, databases, admin UI
- [[Architecture/Connector Lifecycle]] — enrollment → heartbeat → cert renewal flow

### Services
- [[Services/Controller]] — Go backend (HTTP :8080 + gRPC :9090)
- [[Services/Connector]] — Rust agent (enrollment, heartbeat, cert renewal, auto-update, Shield aggregation :9091)
- [[Services/Shield]] — Rust resource host agent (enrollment, Control stream via Connector, zecurity0 + nftables + resource protection)
- [[Services/PKI]] — 3-tier CA hierarchy (Root → Intermediate → Workspace CA)
- [[Services/Auth]] — Google OAuth + JWT session management

### Planning
- [[Planning/Roadmap]] — sprint status, current priorities, what's next
- [[Planning/Session Log]] — running log of all work sessions (read this first)

### Sprint 4 (Complete)
- [[Sprint4/team-workflow.md]] — How to start a session (AI tool onboarding guide for team members)
- [[Sprint4/path.md]] — Dependency map + progress checkboxes
- [[Sprint4/Member1-Frontend/Phase1-Layout-Routing]] — M1 phases (React + GraphQL)
- [[Sprint4/Member2-Go-Proto-Shield/Phase1-Proto-appmeta]] — M2 phases (Proto + Shield + PKI)
- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]] — M3 phases (DB + GraphQL + Connector)
- [[Sprint4/Member4-Rust-Shield-CI/Phase1-Crate-Scaffold]] — M4 phases (Shield binary + CI)

### Sprint 19 (Active)
- [[Sprint19/path]] — **Dependency map + progress tracker** for both Sprint 19 tracks
- `Sprint19/Member02/` — Track A, PENDING-16 (Resource Policy → Device Profile binding)
- `Sprint19/Member2-Go/` — Track B, PENDING-13 (Client device lifecycle)

### Sprint 6 (Complete)
- [[Sprint6/team-workflow]] — **How to start a session** (AI tool onboarding guide for team members)
- [[Sprint6/path]] — **Dependency map + progress checkboxes** (check before any code)
- [[Sprint6/Member1-Frontend/Phase1-Discovery-Tab]] — M1 phases (discovery tab + scan UI)
- [[Sprint6/Member2-Go-Proto-DB/Phase1-Proto-Schema]] — M2 phases (proto + migration 008 + GraphQL schema)
- [[Sprint6/Member3-Go-Connector/Phase1-Discovery-Resolvers]] — M3 phases (resolvers + control handler + connector scanner)
- [[Sprint6/Member4-Rust-Shield/Phase1-Discovery-Module]] — M4 phases (discovery.rs + control stream wiring)

---

## System at a Glance

```
Admin UI (React)
    │  HTTPS GraphQL
    ▼
Controller (Go)  ←──────────────────── Connector (Rust)
  HTTP :8080                              mTLS Control stream :9090
  gRPC :9090                              SPIFFE identity
    │                                      │
    │                                      │ :9091 Shield-facing gRPC
    │                                      ▼
    │                                   Shield (Rust)
    │                                   mTLS Control stream to Connector
    │                                   zecurity0 + nftables
    │                                   resource protection + discovery
    ├── PostgreSQL  (workspaces, connectors, shields, resources, CA keys)
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

| Sprint   | Status     | What Was Built |
| -------- | ---------- | -------------- |
| Sprint 1 | ✅ Done    | Auth (Google OAuth + JWT), workspace management, admin UI |
| Sprint 2 | ✅ Done    | PKI (3-tier CA), connector enrollment, heartbeat, mTLS, SPIFFE |
| Sprint 3 | ✅ Done    | Automatic cert renewal (RenewCert RPC, channel rebuild) |
| Sprint 4 | ✅ Done    | Shield deployment (resource host agent, zecurity0, nftables base table) |
| Sprint 5 | ✅ Done    | Resource protection (nftables per-resource, `pending → protected` lifecycle) |
| Sprint 6 | ✅ Done    | Discovery (Shield local service scan + Connector network scan) |
| Sprints 7–18 | ✅ Done | Not itemised here — see `.zecurity-obs/pending/README.md` and [[Planning/Session Log]] |
| Sprint 19 | 🚧 Active | Track A: Resource Policy → Device Profile binding (PENDING-16). Track B: client device lifecycle (PENDING-13) |

---

## Coordination

All agents: read `agent.md` at project root before any session.
All changes to architecture: update the canvas files in `Architecture/`.
All sessions: append an entry to [[Planning/Session Log]].
Sprint 19 work: check [[Sprint19/path]] before touching any file.
