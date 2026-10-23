---
title: Connector Revocation — Phase C Implementation Record (client enforcement)
date: 2026-10-22
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§8, D19)
acceptance: Connector-Revocation-PhaseC-Acceptance-2026-10-22.md
branch: feat/sprint20-m1-phase2
commits: 777dade (implementation + tests), 7c444f2 (security-review fixes)
status: DONE — unit + mutation + live AC-L PASS. Committed locally, NOT pushed.
---

# Phase C — the client refuses and cuts revoked connectors

## What changed

| Area | Change |
|---|---|
| `proto/client/v1/client.proto` | `ACLSnapshot.revoked_connectors = 11` → `RevokedConnector{connector_id = 1, connector_spiffe = 2}` (additive). Go regenerated (tracked `client.pb.go`); client and connector compile it via build.rs |
| controller `policy/store.go` `GetRevokedConnectors` | **D19 realisation:** a connector is listed while any of its certificates in `connector_certificates` is still valid (`not_after > now()`) **and** its row is revoked or gone (revoked then deleted). Trust domain from `connectors`, falling back to `workspaces`. This is more exact than "revoked_at + TTL" and survives deletion |
| controller `policy/compiler.go` | fills `RevokedConnectors`; reaches clients and connectors through the shared cached snapshot (`RevokeConnector` already calls `NotifyPolicyChange`) |
| client `daemon.rs` | `resolve_entry_coords` drops revoked connectors from both the transport and ACL planes. The shared `blocked_connectors` set is replaced on every stored ACL. After an ACL change, `CloseConnectorFlows` is sent with the set (idempotent). **Staleness (D19):** monotonic `acl_last_ok`; after **1 h** without a successful sync the ACL is replaced by a placeholder (no entries, no networks, version 0, **revoked list kept**), so every resource is withdrawn (flows reset) until the next successful sync fetches a full snapshot |
| client `net_stack.rs` | `BlockedConnectors`; `RelayCtl{blocked, selected}`. The relay task skips a revoked connector, **re-checks after the tunnel opens** (race), and records the connector carrying the flow. `ResourceCmd::CloseConnectorFlows` resets only flows on the listed connectors (listeners kept). The unguarded `relay_tcp_to_quic` is test-only |
| client `tunnel_pool.rs` / `transport.rs` | `TunnelPool::new_for_connector`: **exact connector SPIFFE pinning** on the direct path; an empty or foreign id fails closed. The trust-domain prefix pool is test-only. `ClientTransport` carries its connector id |

## Tests

- Client **252 → 265**.
- Connector 237; the cross-language test passes.
- Controller full `go test ./...` green, plus `TestCompileACLSnapshot_RevokedConnectors` (DB).

| Item | Test |
|---|---|
| AC-1 | `TestCompileACLSnapshot_RevokedConnectors`: revoked and revoked-then-deleted listed; active, all-certs-expired and other-workspace not |
| AC-2 | `resolve_drops_revoked_connectors_from_both_planes`, `effective_config_excludes_revoked_and_fails_closed_when_none_left` |
| AC-3 | `close_connector_flows_resets_only_flows_on_revoked_connectors` (exact flows; others, selecting flows and the listener kept; idempotent) |
| AC-4 | `revoked_connector_is_never_tried_and_selection_is_recorded`, `connector_revoked_while_opening_is_dropped` |
| AC-5 | `acl_staleness_bound_is_one_hour`, `stale_placeholder_withdraws_everything`, `acl_sync_updates_the_blocked_set_and_staleness_clock`, `close_revoked_connector_flows_sends_the_blocked_set` |
| AC-6 | `exact_pool_accepts_only_its_own_connector`, `exact_pool_refuses_empty_or_foreign_ids`, `exact_pool_accepts_a_renewed_connector_certificate`, `transport_build_requires_an_exact_connector_identity` |

**Mutation checks: 13/13 killed.** C1–C2 retention query; C3 resolve filter; C4 skip; C5 race re-check; C6 selection record; C7 targeted close; C8 blocked-set update; C9 1 h gate; C10 close helper; C11 exact verifier; C12 production uses the exact pool; C13 the stale placeholder keeps the revoked list. C10 first hung because its test had no time limit; the test now waits at most 2 s, and on re-run C10 was killed.

**Security review (automated) during the run:**
- Two findings were read off temporary mutants (C3, C11) and don't apply to the committed code.
- Two valid points were fixed in `7c444f2`:
  - the stale placeholder now keeps the revoked list;
  - the unguarded relay and the prefix pool are now test-only.

## Live check AC-L (2026-10-22): PASS

**Setup:**
- controller `3c058aea6533` (HEAD build);
- client `2485de8f1da3` (dev build with platform root, installed with the user's sudo; backup `/usr/local/bin/zecurity-client.pre-phasec`);
- **hostile stand-in connector** `pc-hostile` = the Phase A measurement build `aac7cbea37a5`, which ignores revocation, run as the normal user on udaya (192.168.1.41) in network `pc-hostile-net`, serving resource `pc-hostile-web` (udaya:8090) to `g9-users`.

**Run:**
1. A held HTTP connection through the tunnel (udaya saw source `.41`, i.e. via the connector).
2. `revokeConnector` at 12:45:42.33 UTC.

| Time (UTC) | Event |
|---|---|
| 12:45:42.34 | Connector logs `ConnectorRevoked` / revoked marker. As a Phase A build it **keeps serving**: process alive, :9092 open, reconnect retries. The held flow kept working |
| 12:45:58.73 | Client ACL sync v4 with the revoked list (the 60 s poll; 16 s after the revoke) |
| 12:45:58.75 | Client: **`revoked connector: flows on it reset closed=1`** — the client cut the flow itself |
| 12:45:59 onward | Every new attempt: `connector offline — failing closed for managed resource` (the only connector is revoked) |

**Bound:** client enforcement takes effect within one ACL poll (≤ 60 s); an honest connector closes at once (B2).

**Cleanup:**
- hostile connector stopped;
- `pc-hostile-net` / `pc-hostile` (revoked) / `pc-hostile-web` remain in the DB as test objects;
- the client stays on the Phase C build.
