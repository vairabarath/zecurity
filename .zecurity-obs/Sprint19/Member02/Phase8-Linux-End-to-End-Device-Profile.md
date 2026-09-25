---
type: phase
member: M02
sprint: 19
phase: 8
title: Linux Device Profile End-to-End Validation
status: complete
depends_on: [5, 6, 7]
tags: [linux, posture, device-profile, e2e, ztna, pending-16]
---

# Phase 8 — Linux Device Profile End-to-End Validation

> This phase is mandatory. Sprint 19 is not complete if Device Profiles only work at the schema/UI
> level. At least one real Linux path must work end-to-end.

## Goal

Prove the full path:

```text
Linux Device
   ↓
Client identity
   ↓
Posture collection/report
   ↓
Controller posture evaluation
   ↓
Device Profile satisfaction
   ↓
Resource Policy
   ↓
ACL compilation
   ↓
ACL snapshot propagation
   ↓
Connector authorization
   ↓
ALLOW / DENY
```

## Required validation

### Profile creation

- [x] Create a Linux Device Profile.
- [x] Configure at least the currently supported Linux checks.
- [x] Confirm requirements persist and revisions change correctly.

### Posture reporting

- [x] Real Linux client can report posture.
- [x] Controller receives and stores the report.
- [x] Report is associated with the correct device/workspace.
- [x] Supported checks produce real observations.
- [x] Unsupported checks are handled according to the existing project semantics, not silently treated as passing. *(verified by test, not by this hardware — see below)*

### Profile evaluation

- [x] Passing Linux device satisfies the profile.
- [x] Failing Linux device does not satisfy the profile.
- [x] Stale/missing posture is handled according to existing posture semantics.
- [x] Evaluation uses the current profile revision.

### Resource Policy enforcement

- [x] Attach the Linux profile to a Resource Policy.
- [x] Assign the Resource Policy to a Resource.
- [x] Passing Linux device is represented in the compiled allowed identity set.
- [x] Failing Linux device is absent/denied.
- [x] Remove the profile and verify the policy becomes Any Device.
- [x] Add two profiles and verify OR behavior.

### Live propagation

- [x] Change the Resource Policy while the Connector is running.
- [x] Verify ACL invalidation/recompile/push.
- [x] Verify the Connector receives the new ACL.
- [x] Verify access changes without requiring a manual service restart.

## Evidence

The phase must leave reproducible test/e2e evidence showing:

```text
Linux posture PASS → Resource Policy → ACL → Connector → ALLOW

Linux posture FAIL → Resource Policy → ACL → Connector → DENY
```

Do not mark this phase done from mocked posture data alone.


---

# Implementation — live run, 2026-09-23

Run on real hardware over a LAN. Two machines:

| Host | Role |
|---|---|
| `192.168.1.34` (Arch) | Controller, Postgres + Valkey (containers), Connector, protected services |
| `192.168.1.42` `fsociety` (Arch) | **The Linux device under test** — client daemon, real posture |

The client is the separate box on purpose. A Device Profile is a statement about a
*device*, so the device whose posture decides access had to be the real one; and
putting the firewall lever there keeps it away from the control plane.

## No Shield in this phase

`routeTypeForResource` (`controller/internal/policy/compiler.go:380`) routes a
resource in `pending`/`unprotected` status through the **connector**; only
`protecting`/`protected` require a shield. Phase 8's decision point is the
Connector (`connector/src/device_tunnel.rs`), which `route_type: "connector"`
exercises in full. Shield is network enforcement — a separate concern that Phase
10 lists as its own box. No Shield was installed, so no nftables changes were made
on the controller host.

## The measurement trap, and how the test was fixed

The first resource was an HTTP service on `192.168.1.34:18080` — **on the same LAN
as the client**. A successful `curl` therefore proved nothing: it could have gone
direct. This is not hypothetical; it actually produced two false positives before
it was caught.

`ip route get` does not settle it either. The client marks flows with nftables
`type route hook output priority mangle` and uses policy routing
(`client/src/tun.rs:56-62`), so the diversion is invisible to an unmarked route
lookup — an early `ip route get 192.168.1.34` showed `dev wlan0` while traffic was
in fact being tunnelled.

Worse, the failure is asymmetric. When access is revoked the client tears down
`zecurity0` and removes its marking rules, so traffic **silently falls back to the
direct LAN path** and `curl` starts succeeding again — which reads exactly like
"access restored". Two results were recorded that way before the connector log was
checked and found to contain no matching authorization.

**Fix:** the resource was moved to `172.17.0.4:80`, an nginx container on the
controller host's Docker bridge. The connector reaches it locally; `192.168.1.42`
has no route to `172.17.0.0/16` and cannot reach it at all except through the
tunnel. Verified in both directions before re-running. Every access result below is
from that tunnel-only resource and is corroborated by a connector log line.

**The connector log is the evidence, not `curl`.** Only these authorization
decisions ever reached the Connector:

```
09:42:06  access allowed   resource=phase8-http        (LAN resource, posture PASS)
10:19:57  access allowed   resource=phase8-http        (LAN resource, posture PASS)
10:24:51  access denied    dest=192.168.1.34 reason="no_acl_match"
10:29:38  access allowed   dest=172.17.0.4:80          (tunnel-only, Any Device)
10:30:09  access denied    dest=172.17.0.4:80 reason="no_acl_match"
```

## What the device actually reported

Four Linux checks exist. On this hardware, with no fixture anywhere:

```
linux.os.version           -> PASS  (Arch Linux ?)
linux.firewall.active      -> FAIL  (nftables present but not default-deny)
linux.disk_encryption.luks -> FAIL  (crypttab has no entries)
linux.secure_boot.enabled  -> FAIL  (Secure Boot disabled)
```

`linux.firewall.active` is the only check flippable on demand, so profile
**Corporate Linux** requires it strictly plus `linux.os.version` with
`allowUnsupported`. The lever is an nft table with `policy drop` (SSH, loopback,
established and ICMP explicitly accepted, so the box stays reachable and the
daemon keeps its outbound path). `collect_firewall` tries `nft` first and `nft` is
installed, so it returns on that branch and never reaches the iptables/ufw
fallbacks.

A second profile **Any Linux OS** requires only `linux.os.version`, which always
passes here — the counterpart needed to demonstrate OR.

## Posture PASS -> ALLOW

```
linux.firewall.active -> PASS (nft default-deny active)
evaluation: satisfied=true profile_revision=3 reason=all requirements satisfied

compiled ACL:
  allowed_spiffe_ids  [spiffe://ws-phase8-lab.zecurity.in/client/a10b837b-...]
  ValidUntil          report_received_at + 10m   (posture-bounded lease)

connector: session registered ... transport="quic"
           access allowed dest=... proto=tcp route="connector"
```

## Posture FAIL -> DENY, by the natural cadence

The firewall was dropped at **10:20:07** and then *nothing was touched*. The
client's own 5-minute posture timer produced the next report at **10:24:19**:

```
10:24:19.749  posture report received  -> firewall FAIL
10:24:19.781  ACL snapshot version=10 pushed to the connector   (32 ms later)
10:24:51.041  access denied  reason="no_acl_match"
```

Client daemon pid unchanged throughout; the connector was never restarted. This is
the `access changes without a manual service restart` box, proven by waiting rather
than by forcing.

`is_allowed` (`connector/src/policy/mod.rs:59`) denies on a missing snapshot, a
missing entry, **or an empty `allowed_spiffe_ids`** — default-deny at all three
points, so an empty allowed set is a denial and not an omission.

## Any Device, and a live policy change

With posture still failing, the profile was removed from the policy, leaving it
with zero profiles:

```
10:29:38  access allowed  dest=172.17.0.4:80    <- allowed DESPITE failing posture
          ValidUntil 0001-01-01                 <- no posture lease when ungated
```

Re-attaching the failing profile while the tunnel was live:

```
10:30:04  addProfileToResourcePolicy
10:30:05  ACL snapshot version=19 pushed          (~1 s)
10:30:09  access denied  reason="no_acl_match"
```

Same daemon pid, no restart anywhere. That is the
`Change the Resource Policy while the Connector is running` box together with
invalidate -> recompile -> push -> enforce.

## OR across two profiles

With both profiles attached and the device failing one of them:

```
evaluations:  Corporate Linux: satisfied=false
              Any Linux OS:    satisfied=true
compiled ACL: allowed_spiffe_ids [ ...client/a10b837b-... ]     -> ALLOWED
```

Removing the satisfied profile, leaving only the failing one:

```
compiled ACL: allowed_spiffe_ids (empty)                        -> DENIED
```

Verified at the ACL/compiler level, which is the artefact the Connector enforces.
The `curl` that accompanied the first OR check was one of the two LAN false
positives described above and is **not** cited as evidence.

## ACL propagation

Snapshot versions the connector accepted across the run, none of them requiring a
restart or reconfiguration: 1, 2, 3 (device enrol + first posture + satisfied),
then 7-14 as posture flipped, then 18, 19 on policy edits. Each followed
`NotifyPolicyChange` -> invalidate -> recompile -> push, landing between 32 ms and
~1 s after the triggering change.

## Stale posture fails closed

The device dropped off the LAN, so posture reporting stopped. With the policy
gated by two profiles and `Any Linux OS` evaluating **satisfied**, the report was
allowed to age past `MaxReportAge` (600s):

```
report age:   601s
evaluations:  Any Linux OS: satisfied=true      <- row UNCHANGED in the database
              Corporate Linux: satisfied=false
compiled ACL: allowed_spiffe_ids (empty)        <- device dropped anyway
```

This is the important nuance: the stored evaluation still says satisfied, because
nothing re-evaluated it — no report arrived to trigger that. Freshness is enforced
*again* at ACL compile time, where `applyPosture`
(`internal/policy/compiler.go:339`) recomputes
`evaluation.ReportReceivedAt + MaxReportAge` and refuses the device when it has
passed, regardless of the `Satisfied` flag. A device that stops reporting loses
access on its own, with no revocation step and nothing to run.

## Unsupported checks — verified by test, not by this hardware

`/sys/firmware/efi` exists on the test device, so Secure Boot reports
`FAIL ("Secure Boot disabled")`. **No check can return `UNSUPPORTED` on this
machine**, so the box cannot be exercised here; it needs a BIOS/non-EFI host.

The semantics are covered by `TestEvaluateProfileStatusMatrix`
(`internal/posture/evaluate_test.go:24-32`), which asserts the full matrix:

| Observation | `allowUnsupported` | Satisfied |
|---|---|---|
| `UNSUPPORTED` | false | **false** — not silently passing |
| `UNSUPPORTED` | true | true |
| `UNKNOWN` / `ERROR` | either | false |

That is exactly what the box asks: unsupported is honoured only when the
requirement opts in, and `UNKNOWN`/`ERROR` never pass. Ticked on that basis, with
the hardware limitation stated rather than hidden.

## How login was handled

`zecurity-client login` performs a Google OAuth browser round trip, and no OAuth
credentials exist for this environment. Phase 8 is about posture, policy and
authorization, not the login handshake, so the device was enrolled with an access
token minted from `JWT_SECRET` — the same token the OAuth callback issues — and the
result handed to the daemon through its own existing `PostLoginState` IPC message.

Everything downstream is unmodified production code: the same `EnrollDevice` RPC,
the same P-384 key and possession-proving CSR, the same certificate signing and
SPIFFE assignment, the same durable state, the same posture collector, the same ACL
compiler, the same connector enforcement. **No client, controller or connector
source was changed for this run.** The substitution is the browser consent step and
nothing else, recorded here so nobody later reads this phase as having exercised
OAuth.

The harness lives in `controller/cmd/phase8/` (`bootstrap`, `token`, `enroll`,
`acl`). `acl` exists because the compiled ACL snapshot has **no read surface
anywhere** — no GraphQL query, no debug endpoint — so "the passing device is in the
allowed identity set" is otherwise unobservable from outside the controller.

## Bug found: `createGroup` is broken on `pending-16`

```
createGroup: number of field descriptions must equal number of destinations, got 6 and 9
```

`GroupRow` gained three provenance columns (`origin`, `external_id`,
`connection_id`) with the SCIM work; `CreateGroup`'s `RETURNING` clause was never
updated, so it passes nine scan destinations against six columns. Creating a group
fails outright. `UpdateGroup` has the quiet form: six against six, no error, but
`origin` always returns empty.

The INSERT succeeds and only the scan fails — the group row was already in the
database after the failed call, which is how it was confirmed and worked around.

Already fixed on `fixed-pendings` by `c5eb347`. **`pending-16` has not been merged
with that branch**, so the fix is absent and Phase 8 walked straight into it.

## Environment notes for anyone re-running this

- The controller requires `GOOGLE_CLIENT_ID`/`SECRET`/`REDIRECT_URI` **and** a
  second pair `CLIENT_GOOGLE_CLIENT_ID`/`SECRET` at boot (`mustEnv`). Placeholders
  are enough to start it; only the login flow needs real ones.
- Port 8080 was occupied on the controller host, so the controller ran on 8081 with
  dedicated Postgres (5440) and Valkey (6390) containers, leaving unrelated
  projects on that machine untouched.
- `192.168.1.42` dropped off the LAN twice mid-run (ARP INCOMPLETE, no ICMP) —
  suspend or WiFi power management. The daemon and its durable state survived both
  times; only the in-flight step had to be repeated.

## Build gate

```
go build ./...                          OK
go vet ./cmd/phase8/...                 OK
go test ./internal/... ./graph/...      all packages ok
```
