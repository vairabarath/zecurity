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
| PENDING-01 ✅ → [ADR-020](../Decisions/ADR-020-Authenticated-Relay-Provisioning.md) | Authenticated Relay Provisioning *(accepted, Sprint 12)* | security/relay | **P0** | ADR-014, Sprint 10.3 |
| PENDING-02 ✅ → [ADR-027](../Decisions/ADR-027-Certificate-Revocation-Enforcement.md) | Certificate Revocation (CRL/OCSP) Enforcement *(accepted 2026-08-10)* | security | **P0** | ADR-014 |
| ✅ [PENDING-03](PENDING-03-Decouple-Transport-From-ACL.md) | Decouple Transport from ACL (finish Track B) — implemented in Sprint 13 | relay | P1 | ADR-015/016/017/018 |
| [PENDING-04](PENDING-04-Multiple-IdPs-Enterprise-SSO.md) | Multiple IdPs & Enterprise SSO | identity | P1 | ADR-005/006 |
| [PENDING-05](PENDING-05-Directory-Sync-SCIM.md) | Directory Sync (SCIM) | identity | P2 | — |
| [PENDING-06](PENDING-06-MFA-Step-Up-Auth.md) | MFA & Step-Up Authentication | identity | P2 | PENDING-04 |
| [PENDING-07](PENDING-07-Provider-Dashboard-Vision.md) | **Provider Dashboard — Vision** (functionality + phased roadmap) | operator | P1 | 07a, 07b |
| PENDING-07a ✅ → [ADR-021](../Decisions/ADR-021-Provider-Identity-and-Authorization.md) | ↳ Provider Identity & Authorization tier *(accepted, Sprint 12)* | operator | P1 | 01, 04 |
| [PENDING-07b](PENDING-07b-Provider-Console-Packaging.md) | ↳ Provider Console packaging (separate app vs shared; alpha = CLI) | operator | P1 | 07a |
| ✅ [PENDING-08](PENDING-08-Device-Posture-Health.md) | Device Posture & Health Checks — implemented in Sprint 15 (manual/E2E pass outstanding) | zero-trust | P2 | — |
| [PENDING-09](PENDING-09-Continuous-Authorization.md) | Continuous / Re-evaluated Authorization — *Option B (bounded) implemented in Sprint 15; full decision (A/C) still open* | zero-trust | P2 | ADR-001 |
| [PENDING-10](PENDING-10-Observability.md) | Observability: Metrics, Tracing, Health | operations | P1 | — |
| [PENDING-11](PENDING-11-Audit-Logging-SIEM.md) | Audit Logging & SIEM Export | operations | P2 | — |
| [PENDING-12](PENDING-12-Controller-HA-Multi-Region.md) | Controller HA & Multi-Region | operations | P2 | ADR-013 |
| [PENDING-13](PENDING-13-Client-Device-Lifecycle.md) | Client Device Lifecycle & Cert Renewal | identity/ops | P2 | ADR-002 |
| [PENDING-14](PENDING-14-FQDN-Resource-Access.md) | DNS / FQDN-Based Resource Access | data-plane | P2 | — |
| ✅ [PENDING-15](PENDING-15-Durable-Outbox-Infrastructure.md) | Platform Durable Outbox Infrastructure — implemented in Sprint 18 | platform | P1 | ADR-025, PENDING-13, PENDING-02 |
| [PENDING-16](PENDING-16-Resource-Policy-Device-Profile-Binding.md) | Resource Policy → Device Profile Binding | policy | P1 | PENDING-08, PENDING-09 |

**Priority key:** P0 = security hole / finish-what's-started · P1 = needed to sell/operate ·
P2 = differentiation / maturity.

---

## Implementation Order

> **Correction to a common assumption:** the two P0s do **not** require the provider *dashboard
> (UI)*. They require the provider **identity/authorization tier** ([[ADR-021-Provider-Identity-and-Authorization]])
> — a small backend slice (`provider_users` allowlist + `RequireProvider` + a `/provider` route
> group + per-action audit), drivable by CLI. The dashboard *UI* ([[PENDING-07b-Provider-Console-Packaging]])
> is a Beta concern. So the true first buildable step is **07a**, then the P0s land correctly on
> top of it.

### Phase 0 — Provider foundation + P0 security *(Alpha: internal, CLI-driven)*
1. **07a (alpha slice)** — provider identity tier: `provider_users` allowlist via corp SSO,
   `RequireProvider`, `/provider` API route group, one authz chokepoint, per-action audit.
   *The boundary everything provider-side sits on.*
2. **PENDING-01** — authenticated relay provisioning (verify + burn the token; re-home
   `POST /api/relays` under `/provider`). *Lands correctly on 07a.*
3. **PENDING-02** — CRL/OCSP enforcement in the relay + controller heartbeat verifiers (backend,
   independent); expose the relay-revoke trigger under `/provider`.

> ⚠️ Security escape hatch: 01's token-*verify* can be hotfixed on its own to stop anonymous CA
> signing **before** 07a is ready — but re-homing token *issuance* under 07a is required to make
> it authorization-correct.

### Phase 1 — Relay correctness + signal *(P1)*
4. **PENDING-03** — decouple transport from ACL. Start with the cheap client-rebuild-on-ACL-change
   fix (review finding F3); do full `TransportSnapshot` only if automatic relay failover is
   prioritized.
5. **PENDING-10** — observability (relay capacity, migration/probe health, cert-expiry runway,
   failover convergence). *Feeds the provider fleet views in Phase 2.*

### Phase 2 — Provider dashboard (Beta) + enterprise identity *(P1)*
6. **PENDING-07b / 07-Beta** — the React provider console (relay fleet + tenant lifecycle, basic
   provider RBAC). *Now there's data + a tier worth rendering.*
7. **PENDING-04** — multiple IdPs / enterprise SSO. *Unblocks 05 and 06.*

### Phase 3 — Identity depth + zero-trust *(P2)*
8. **PENDING-05** — SCIM directory sync *(needs 04)*.
9. **PENDING-06** — MFA / step-up *(needs 04; relates to 09)*.
10. **PENDING-13** — client device lifecycle + cert renewal. *Provides 02's client-device revoke trigger.*
11. **PENDING-09** — continuous / re-evaluated authorization. *Option B (bounded) landed early,
    in Sprint 15, alongside 08; Options A/C still pending team decision.*
12. **PENDING-08** — device posture *(best alongside 09)*. ✅ **Done — Sprint 15**, ahead of this
    sequencing (landed before 04/05/06/13 rather than after).

### Phase 4 — Maturity / GA *(P2)*
13. **PENDING-11 (full)** — audit + SIEM export (beyond the provider-action audit slice from Phase 0).
14. **PENDING-12** — controller HA / multi-region.
15. **PENDING-14** — DNS / FQDN-based resource access.
16. **07-GA** — partner/reseller multi-tenancy, billing/quotas, break-glass impersonation, SoD roles.

### Dependency summary
- **07a** → unblocks *correct* 01, 02's relay-revoke trigger, 07b, and the entire provider plane.
- **04** → unblocks 05, 06.
- **13** → supplies 02's client-device revoke trigger.
- **09 ↔ 08, 06** → the zero-trust posture cluster.
- **10** → feeds the 07 dashboard fleet views and 03 failover visibility.
- **07** (vision) is a reference/north-star, not a build step; it decomposes into 07a (Phase 0)
  and 07b (Phase 2).
