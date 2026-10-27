---
title: Sprint 20 Fix 17 — a revoked relay stops its heartbeat (F32)
date: 2026-10-27
type: fix
status: FIXED in code (2026-10-27), commit d89c0310 (local). Unit tests + 5/5 mutants killed. Live check pending.
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

## Live check (pending)

1. Revoke a running lab relay (`POST /provider/relays/{id}/revoke` with a reason and `confirm_name`; super-admin with TOTP since Sprint 22 T/A).
2. Expect:
   - one relay log line `controller reports this Relay revoked; heartbeat stopped`;
   - no further `relay heartbeat: received` lines for it on the controller over 2 min;
   - the process still running.
3. Control: a live relay whose renewal superseded a serial keeps heartbeating.
