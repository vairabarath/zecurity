---
type: adr
status: pending
id: PENDING-14
domain: data-plane
priority: P2
created: 2026-07-03
related: []
tags: [pending, adr, data-plane, dns, resources]
---

# Pending ADR 14 — DNS / FQDN-Based Resource Access

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.

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
