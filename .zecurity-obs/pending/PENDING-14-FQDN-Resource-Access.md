---
type: adr
status: implemented
id: PENDING-14
domain: data-plane
priority: P2
created: 2026-07-03
related:
  - "[[Decisions/ADR-022-Shield-LAN-IP-Resource-Host-Sync]]"
  - "[[Decisions/ADR-023-Privileged-OS-DNS-Integration]]"
  - "[[Sprint16/path]]"
tags: [pending, adr, data-plane, dns, resources]
---

# Pending ADR 14 — DNS / FQDN-Based Resource Access

> **Status: IMPLEMENTED in Sprint 16** (Stages 1–2 + Phase 11), verified end-to-end on a two-host
> stack. Following the precedent of PENDING-02 and PENDING-03, this stays in `pending/` marked
> `implemented`; the design decisions taken along the way live in
> [[Decisions/ADR-022-Shield-LAN-IP-Resource-Host-Sync]] and
> [[Decisions/ADR-023-Privileged-OS-DNS-Integration]].
>
> **What shipped.** The acceptance criterion below is met and proven: a resource's backend IP can change
> with **no controller action, no ACL version bump and no tunnel restart**. Mechanism: `resource_id` on
> the wire (the connector authorizes by identity, not address) · connector-side resolution at dial time
> with a TTL-bounded, process-local cache that never touches controller state · client-side synthetic IPs
> from `100.64.0.0/10`, allocated locally and never seen by the controller · a client DNS responder on
> `127.0.0.1:53` answering managed names with those synthetic IPs.
>
> Gate 2 (5/5) verified the headline claim twice on independent runs — DNS moved the backend, traffic
> followed, ACL version unchanged both times (`cache_hit=false`, sub-millisecond re-resolution).
>
> **What is deferred.** Automatic OS DNS integration (Sprint 16 Phase 12). It is blocked on a *privilege*
> decision, not on DNS: per-link `systemd-resolved` configuration is polkit-gated behind `auth_admin`, and
> the client daemon runs as the enrolling user — capabilities do not help, because polkit authorizes on
> uid. Until that is decided, a managed name is reached via the responder explicitly (`dig @127.0.0.1`,
> `curl --resolve`) or a `hosts` entry. See [[Decisions/ADR-023-Privileged-OS-DNS-Integration]].
>
> **Out of scope, still.** Wildcard/pattern matching (`ACLEntry.pattern` is `reserved 14`; invariant #4
> requires wire-level validation before any wildcard is honoured) and IPv6 (`AAAA` deliberately answers
> NODATA, matching the connector's v4-only resolver).

## Context / Current State

Resources are defined and routed by **IP:port**. The client data plane keys transports on
`(Ipv4Addr, u16)` and skips any ACL entry whose address doesn't parse as an IP
(`client/src/daemon.rs` `build_transports_by_resource` → `entry.address.parse::<IpAddr>()`;
non-V4 is dropped). So a resource that is only reachable by hostname, or whose IP changes (cloud
DNS, load balancers, k8s services), can't be expressed cleanly. Most ZTNA use cases are
FQDN-oriented ("give group X access to `db.internal.acme.com`").

## Problem — Decision Needed

Do we support FQDN-based resources, and where is name resolution performed?

## Options

### Option A — Connector-side DNS resolution + FQDN ACL entries
ACL entries carry FQDNs; the client routes matching names to the connector; the **connector**
resolves the name inside the remote network (split-horizon correctness).
- **Pros:** correct for internal DNS/split-horizon; connector is already the egress. **Cons:**
  client must intercept DNS + map names→transport (TUN DNS handling); wildcard/domain matching.

### Option B — Client-side resolution to IP, keep IP routing
Client resolves FQDN then uses existing IP path.
- **Pros:** smaller data-plane change. **Cons:** wrong for internal/split-horizon DNS; breaks on
  dynamic IPs; the client can't see private DNS.

### Option C — Wildcard/domain-scoped resources
Support `*.internal.acme.com` style scopes (superset of single FQDN).
- **Pros:** powerful, matches real network segmentation. **Cons:** matching + policy semantics
  complexity.

## Recommendation (non-binding)
Option A (connector-side resolution with FQDN ACL entries) is the ZTNA-correct model; design the
ACL/proto so C (wildcards) is a natural extension. Requires client DNS interception in the TUN
data plane — a meaningful but well-scoped addition.

## Open Questions
- DNS interception design in the client TUN stack (`net_stack.rs`/`tun.rs`)?
- Proto/schema changes to `ACLEntry` to carry names/wildcards (mind field-number rules)?
- Interaction with split-tunneling (ADR-009) and IPv6?

## Rough Effort / Priority
**M–L, P2.** High user-facing value; touches proto + client data plane.
