---
type: index
status: pending
created: 2026-07-03
tags:
  - pending
  - adr
  - roadmap
---

# Pending ADRs — For Team Discussion

These are **proposed, not-yet-decided** architecture decisions captured from the
2026-07-03 architecture/roadmap review. Each is a starting point for a team
discussion — the "Options" and "Open Questions" are the point; the
"Recommendation" is non-binding.

**Workflow:** discuss → decide → on adoption, promote the file to the next free
`ADR-0NN` number and move it into `../Decisions/`. Mark rejected ones
`status: rejected` and keep them here for the record.

> Current-state notes below are grounded in the code as of `integration/relay-merge`
> (2026-07-03). The relay/security items (01–03) were verified in depth; the
> product/ops items (04–14) are from a presence/absence scan and should be
> confirmed before committing scope.

| # | Title | Domain | Priority | Related |
|---|-------|--------|----------|---------|
| [PENDING-01](PENDING-01-Authenticated-Relay-Provisioning.md) | Authenticated Relay Provisioning | security/relay | **P0** | ADR-014, Sprint 10.3 |
| [PENDING-02](PENDING-02-Certificate-Revocation-Enforcement.md) | Certificate Revocation (CRL/OCSP) Enforcement | security | **P0** | ADR-014 |
| [PENDING-03](PENDING-03-Decouple-Transport-From-ACL.md) | Decouple Transport from ACL (finish Track B) | relay | P1 | ADR-015/016/017/018 |
| [PENDING-04](PENDING-04-Multiple-IdPs-Enterprise-SSO.md) | Multiple IdPs & Enterprise SSO | identity | P1 | ADR-005/006 |
| [PENDING-05](PENDING-05-Directory-Sync-SCIM.md) | Directory Sync (SCIM) | identity | P2 | — |
| [PENDING-06](PENDING-06-MFA-Step-Up-Auth.md) | MFA & Step-Up Authentication | identity | P2 | PENDING-04 |
| [PENDING-07](PENDING-07-Provider-Dashboard-Vision.md) | **Provider Dashboard — Vision** (functionality + phased roadmap) | operator | P1 | 07a, 07b |
| [PENDING-07a](PENDING-07a-Provider-Identity-and-Authorization.md) | ↳ Provider Identity & Authorization tier *(the load-bearing decision)* | operator | P1 | 01, 04 |
| [PENDING-07b](PENDING-07b-Provider-Console-Packaging.md) | ↳ Provider Console packaging (separate app vs shared; alpha = CLI) | operator | P1 | 07a |
| [PENDING-08](PENDING-08-Device-Posture-Health.md) | Device Posture & Health Checks | zero-trust | P2 | — |
| [PENDING-09](PENDING-09-Continuous-Authorization.md) | Continuous / Re-evaluated Authorization | zero-trust | P2 | ADR-001 |
| [PENDING-10](PENDING-10-Observability.md) | Observability: Metrics, Tracing, Health | operations | P1 | — |
| [PENDING-11](PENDING-11-Audit-Logging-SIEM.md) | Audit Logging & SIEM Export | operations | P2 | — |
| [PENDING-12](PENDING-12-Controller-HA-Multi-Region.md) | Controller HA & Multi-Region | operations | P2 | ADR-013 |
| [PENDING-13](PENDING-13-Client-Device-Lifecycle.md) | Client Device Lifecycle & Cert Renewal | identity/ops | P2 | ADR-002 |
| [PENDING-14](PENDING-14-FQDN-Resource-Access.md) | DNS / FQDN-Based Resource Access | data-plane | P2 | — |

**Priority key:** P0 = security hole / finish-what's-started · P1 = needed to sell/operate ·
P2 = differentiation / maturity.
