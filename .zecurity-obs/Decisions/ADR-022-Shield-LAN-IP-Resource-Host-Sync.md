---
type: decision
status: accepted
date: 2026-07-27
related:
  - "[[Decisions/ADR-004-Resource-Reconciliation]]"
  - "[[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]"
  - "[[pending/PENDING-14-FQDN-Resource-Access]]"
  - "[[pending/PENDING-12-Controller-HA-Multi-Region]]"
tags:
  - adr
  - shield
  - controller
  - resource
  - reconciliation
  - reliability
---

# ADR-022 — Shield LAN-IP Change Re-Syncs Bound Resources' Host

## Context

A shield-backed resource is addressed by `resources.host` (an IP), and the resource is bound to
the shield running on that host via `resources.shield_id`. The whole shield↔resource model rests
on an **unstated invariant**:

> **`resources.host == shields.lan_ip`** for every shield-backed resource.

This invariant is assumed in three independent places:
- `AutoMatchShield` (`internal/resource/store.go`) binds a resource to a shield `WHERE lan_ip = $host`.
- `protect_resource` requires an active shield where `s.lan_ip = r.host`.
- the shield's own `validate_host` (`shield/src/resources.rs`) accepts a resource instruction only
  when `host == "127.0.0.1"` or `host == detect_lan_ip()`, and **rejects** it otherwise.

Nothing maintained the invariant when a shield's LAN IP **changed** — DHCP lease change, host
reboot, or a network move. On such a change the shield keeps heartbeating a new `lan_ip`
(`UpdateShieldHealth` already stores it), but every bound resource's `host` stays pinned to the
**old** IP. Consequences, all at once:
- the shield **rejects its own resource instructions** (`validate_host` now fails),
- `AutoMatchShield` / `protect_resource` break for that host,
- the compiled ACL routes clients and the connector to a **dead address**.

This is a silent break, not mere staleness — connectivity to the resource is lost until a human
re-points the resource by hand. Related but out of scope: **connector-routed** resources
(`shield_id` NULL — see ADR-018 / migration 021) have no shield IP to follow; dynamic addressing
for those is **PENDING-14 (FQDN)**.

## Decision

On every shield heartbeat, when the reported `lan_ip` differs from the stored one, **atomically
re-point that shield's resources whose `host` was tracking the old IP to the new IP**, then bump
the workspace ACL version so the client and connector converge on the new address.

The shield's own resource instructions need no separate invalidation: they are delivered from a
snapshot whose fingerprint already hashes `host` (ADR-004 Phase 2/3), so a `host` change advances
the snapshot generation and the shield re-applies automatically.

### Invariant this maintains
> When a shield's LAN IP changes, the `host` of every resource bound to that shield follows it, so
> `resources.host == shields.lan_ip` continues to hold and every consumer (ACL, connector match,
> shield `validate_host`, `AutoMatch`/`protect`) stays correct.

## Implementation

- **`internal/shield/heartbeat.go` — `UpdateShieldHealth`.** One atomic statement (data-modifying
  CTEs): snapshot the old `connector_id` + `lan_ip`; apply the heartbeat; and — only when the shield
  row was actually updated **and** the LAN IP changed — `UPDATE resources SET host = <new> WHERE
  shield_id = <this> AND host = <old>`. Returns a new `lanIPChanged` flag alongside the existing
  `connectorChanged`.
  - Guard `host = <old>` moves only rows that were tracking the shield's IP — **preserves
    `127.0.0.1` / custom hosts**.
  - Guard `NULLIF($6,'') IS NOT NULL` **never wipes a host to empty** — an IP-detection failure
    keeps the last-known-good host so routing continues.
  - Guard `AND (SELECT lan_ip_changed FROM updated)` ties the resource sync to the shield row
    actually changing, so a **rejected heartbeat** (inactive / tenant-network-mismatched connector →
    0 rows updated) can never desync `host` from `lan_ip` in the opposite direction.
- **`internal/shield/config.go`** — `UpdateShieldHealth` signature returns
  `(connectorChanged, lanIPChanged, err)`.
- **`internal/connector/control_stream.go` — `handleShieldStatus`.** Fire `NotifyPolicyChange` on
  `lanIPChanged` (reusing the same ACL-version-bump path already triggered by a connector move).

## Guardrails

- **Tenant/shield scoping.** A shield only re-points its own resources (`WHERE shield_id = …`) —
  same security scoping as the ADR-004 reconciler.
- **Fires only on real change.** The ACL recompile fires on an actual IP change, never on a normal
  steady-state heartbeat.
- **Shield-backed only.** Connector-routed resources (`shield_id` NULL) are untouched — PENDING-14.
- **Single-controller assumption unchanged.** The policy notifier's version counter is process-local
  and in-memory; making it multi-replica-safe is PENDING-12 (covers both the ACL and transport
  notifiers together).
- Per CLAUDE.md: state rides the existing heartbeat/Control stream — no new RPCs, no proto changes.

## Verification (2026-07-27)

`go build ./...` + `go vet ./...` clean; `shield` / `connector` / `policy` / `transport` /
`resource` suites pass.

- **`internal/shield/heartbeat_test.go`** (integration, run against a freshly, fully-migrated DB):
  - shield IP changes → tracking resource re-pointed, `lanIPChanged = true`;
  - no-op heartbeat (same IP) → nothing changes;
  - `127.0.0.1` resource preserved;
  - **inactive connector → no desync** — the `(SELECT lan_ip_changed FROM updated)` guard is
    **proven load-bearing**: removing it makes this case fail (resource wrongly re-synced);
  - empty `lan_ip` → host preserved (never wiped).
- **`internal/connector/shield_status_test.go`** (unit): `NotifyPolicyChange` fires on
  lan-IP-changed / connector-changed / both, and stays quiet when neither changed.
- **Code-verified (not driven by a new end-to-end test):** the downstream delivery — client ACL
  re-poll, connector re-match on the new host, and shield snapshot-fingerprint regeneration — is the
  existing, separately-tested machinery this change triggers. A full 4-component live run was not
  performed.

## Consequences

**Positive:** closes a silent "shield IP changed → resource unreachable" break; keeps the
`resources.host == shields.lan_ip` invariant that `AutoMatch` / `protect` / `validate_host` all
depend on; reuses ADR-004's snapshot-fingerprint path (shield) and ADR-001's ACL-version path
(client/connector) with no new proto or RPC surface.

**Negative / cost:** `UpdateShieldHealth` is now a multi-CTE statement that also writes `resources`
on the heartbeat path (one extra table touched, only on a real IP change); adds a `lanIPChanged`
return threaded through the shield service interface and its one caller. Bounded and shield-backed
only — connector-routed dynamic addressing remains PENDING-14.
