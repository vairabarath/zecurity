---
title: Sprint 20 Fix 17 — a revoked relay stops its heartbeat (F32)
date: 2026-10-27
type: fix
status: FIXED and verified live (2026-10-27), commit d89c0310 (local). Unit tests + 5/5 mutants killed; live PASS.
owner: Sathiya (M1)
findings: F32 (Sprint20/path.md → Post-Sprint Fixes; live run F5–F7, 2026-09-28)
---

# Fix 17 — a revoked relay stops its heartbeat

**Issue.** After `POST /provider/relays/{id}/revoke`, the relay process stayed up and redialled its heartbeat every 5 s forever. The controller refused each attempt with `PermissionDenied: relay certificate revoked`. Low severity: the relay is refused everywhere, but it's wasted work and log noise. It's the same problem as F38 (connector, Fix 13) and F46 (shield, Fix 16).

**Root cause.** `relay/src/heartbeat.rs` `run_with` treated every error the same way: log, sleep `RECONNECT_DELAY` (5 s), reconnect. Nothing told the relay that it was revoked, as opposed to a transient refusal.

## Why the controller doesn't just check the serial

The interceptor (`controller/internal/connector/spiffe.go`) refuses on a **revoked serial** (`RelayRevocationChecker`). A revoked serial doesn't always mean a revoked relay: `RecordRenewedCert` (`internal/relay/store.go`) revokes a superseded renewal's serial while the relay stays `active`. So the relay can't read a plain `PermissionDenied` as terminal. Error messages carry no security meaning (connector contract, `authorization.go`).

## Fix

Same convention as the connector-state marker (Fix 13 / F40): a structured gRPC trailer that is set only after an authoritative lookup.

- **Controller** (`spiffe.go`):
  - On the revoked-serial refusal, `markTerminalRelay` reads `relays.status` (new `WorkspaceStore.GetRelayStatus`, implemented in `cmd/server/main.go`).
  - If the status is `revoked` or `deleted` (both terminal, Sprint 20 invariant), it sets the trailer `zecurity-relay-state: revoked`.
  - An `active` relay, a missing row or an unreadable status gets no marker. The call is still refused.
  - The lookup runs only on the refusal path, so the common path doesn't gain a query.
- **Relay** (`heartbeat.rs`):
  - `is_terminal_refusal` walks the anyhow chain for a `tonic::Status` with `PermissionDenied` **and** that marker.
  - On it, the relay logs one error and the heartbeat loop returns.
  - Any other error keeps the 5 s retry, including `Unavailable` ("relay revocation state unavailable", fail-closed), so a controller DB blip can't silence a live relay.
  - The process does not exit: under systemd that would become a restart loop, with a provisioning run each time.
- A revoked relay is replaced, not revived (D-20), so a stopped heartbeat never needs to resume.

No proto change, no schema change.

## Tests

- `TestUnarySPIFFEInterceptor_RelayRevoked_TerminalMarker`: marker for `revoked` and `deleted`; none for `active` (superseded serial), a missing row or a lookup error. The code is `PermissionDenied` in all five cases.
- `revoked_marker_stops_the_heartbeat` (paused time, 60 s): 1 connect, then the loop ends.
- `refusals_without_the_marker_keep_retrying`: each of these keeps retrying, with at least 10 connects in 60 s:
  - `PermissionDenied` without a marker;
  - `Unavailable`;
  - a marker with another value;
  - the marker on an `Unavailable` code.
- Mutants, all killed:
  1. never terminal;
  2. `||` instead of `&&` (marker ignored);
  3. any status code accepted;
  4. Go: marker for any relay status;
  5. Go: marker never set.

  Mutant 3 first survived; I added the "marker on `Unavailable`" case to catch it.
- Suites: relay 118 pass; controller 1418 pass, 0 fail, 2 skips (DB-backed tests enabled).

## Not changed

- **Renewal.** A revoked relay's renewal is refused too (`PermissionDenied`, `renew.go`). It already backs off from 60 s up to 30 min and stops at expiry, so it isn't a 5 s loop. Left as is.
- **The relay's QUIC listener** keeps running. Connectors and clients already refuse a revoked relay through the relay CRL.

## Live check — PASS (2026-10-27)

**Setup:**
- Controller and relay `f17-relay-local` (`62383d9b`) ran on 192.168.1.164 from the fix17 branch (`351c8a66`, controller sha `28371b2a…`, relay `ccf4f291…`).
- The relay ran as a plain user process.
- Before the revoke, the controller had received 2 heartbeats, 30 s apart.
- The local dev DB got `042_provider_totp.sql` applied by hand (it predated #138).
- The super-admin was reset with `provider recover-admin`, then went through the password change and TOTP enrolment.

| Time (UTC) | Event |
|---|---|
| 05:26:25.397 | `POST /provider/relays/{id}/revoke` (reason + `confirm_name`) → 204 |
| 05:26:35.690 | relay: `ERROR … controller reports this Relay revoked; heartbeat stopped. Replace the relay to bring it back` |
| 05:26:35 → 05:35:22 | no further relay log line (1 in total since the revoke); controller `relay heartbeat: received` count stays 2; relay pid 123001 still running |

Before the fix this would have been a refused reconnect every 5 s, about 100 in that window.

**Not run live:** the superseded-serial control (a live relay with a revoked old serial keeps retrying). It's covered by the interceptor test "relay active, serial superseded" and the relay test for a `PermissionDenied` without the marker.

## F20 live check (same session) — PASS

F20 was closed in code by bug 3; this is its first live run of the original scenario.

**Setup:**
- connector `f20-conn` on inkyank .75;
- shield `f20-shield` on manoj .53;
- client on .164, rebuilt with `client/scripts/dev-build.sh`;
- `f20-prot` (manoj :8000) protected through the shield;
- `f20-direct` (nika :8001), unprotected;
- both in `cl-group`.

| Time (UTC) | Event |
|---|---|
| 05:33:43 | through the tunnel both return 200 (client: `tunnel opened` for .44:8001 and .53:8000) |
| 05:33:51.929 | `revokeShield`; connector: `shield revoked: stream closed, sessions ended`; **ACL v9 stored at 05:33:51.972** |
| 05:33:54.968 | `deleteShield` → `f20-prot` is `protected` with `shield_id` NULL, shield row gone (the F20 orphan) |
| 05:34:01 | new resource `f20-after` (nika :8002) assigned → connector ACL v10, v11 |
| 05:34:51 | client: `ACL snapshot synced version=11 entries=2`, hot-applied (removed 1, added 1); resources = `f20-direct`, `f20-after` |
| 05:34:58 | `f20-direct` via the tunnel → 200; manoj :8000 → not captured, unreachable (000) |

The controller log has 0 `compile acl` / `requires a shield` errors for the whole run. With F20, the connector would have stayed at the pre-orphan version, and `f20-after` would never have arrived.

## Post-Phase Fixes


### Fix: a revoked relay kept accepting clients (2026-10-30, live E2E run F-8)
**Issue:** after Fix 17 a revoked relay stopped heartbeating but kept its QUIC listener open; clients kept reaching it
and failing ("Relay Lookup failed") while their connectors had already moved.

**Fix Applied:** `relay/src/heartbeat.rs` `run` returns only on the controller's revoked marker (documented);
`relay/src/main.rs` turns that into a `stop` future for `listener::run_listener`, which then closes the endpoint with
application close `0x524556` / `relay revoked`, waits up to 2 s for peers to receive it, and returns; the process exits
0 (the unit's `Restart=on-failure` does not restart it — revoked stays terminal).

**Tests:** `revoked_relay_closes_connections_and_stops` (peer sees `relay revoked` within 3 s; fails without the
close); relay 119 pass. **Live:** revoked relays logged `Relay revoked: closing every connection and the listener` /
`Relay stopped after revocation` on their next heartbeat and their units went inactive.
