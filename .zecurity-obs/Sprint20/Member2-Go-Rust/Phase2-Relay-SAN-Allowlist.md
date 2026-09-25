---
type: phase
member: M2
person: Barath
sprint: 20
phase: 2
execution: C
title: Relay SAN Allowlist Enforcement
status: implemented   # code + tests done; live acceptance PENDING
depends_on: [1]
tags:
  - go
  - relay
  - pki
  - provisioning
  - security
  - provider-dashboard
---

# Phase 2 (C) — Relay SAN Allowlist Enforcement

> Depends on Phase 1 (A) only for M2 sequencing (same package). Decision Record prerequisite #5 (Q15)
> and ADR-020's intent ("ties SANs to an operator-created row").

## Problem (verified)

- `POST /provider/relays` validates and stores `dns_allowlist` / `ip_allowlist` on the relay row (`admin_handler.go:73-94`, `store.go:49-55`).
- `Provision` (`provision.go:93-196`) validates the **request's own** `DnsSans` / `IpSans` (lines 102-109) and passes them to `SignRelayCert` as the allowed set (line 170).
- The stored allowlists are never read during provisioning. A holder of a valid provisioning token can put **any** DNS/IP SAN on a platform-signed relay certificate.
- The proto comments are stale: `provisioning_token` "currently ignored"; `dns_sans` called "Operator-confirmed" (`proto/relay/v1/relay.proto`).

## Goal

The DNS/IP SANs on a relay certificate are always a subset of the operator-registered allowlist on
that relay's row. A request that violates this is rejected **without consuming the single-use
provisioning token**.

## Files

| File | Change |
|------|--------|
| `controller/internal/relay/provision.go` | Load row before burn; allowlist check; pass stored lists to `SignRelayCert` |
| `controller/internal/relay/store.go` | `heartbeatStore` interface (or a new narrow interface) gains `LoadRelayByID` for `Provision` |
| `proto/relay/v1/relay.proto` | Comment fixes only |
| `scripts/relay-local-install.sh`, `scripts/relay-local-install-omitid.sh`, `scripts/run-relay-local.sh` | Comment/usage text: SAN env vars must match the registered allowlist |
| `controller/internal/relay/provision_test.go` | New cases |

## Steps

### C1/C3 — new `Provision` order

```text
1. canonicalRelayID(req.RelayId)                     (unchanged)
2. validateDNSSANs / validateIPSANs on request lists  (unchanged: format only)
3. parse CSR                                          (unchanged)
4. token present; auth configured                     (unchanged)
5. VerifyProvisioningToken; claims.RelayID == relayID (unchanged)
6. row := store.LoadRelayByID(relayID)                ← NEW (before burn)
      ErrRelayNotFound            → FailedPrecondition "relay not registered"
      row.Status != "pending"     → FailedPrecondition "relay is not awaiting provisioning"
7. allowlist check (C2)                               ← NEW (before burn)
8. BurnProvisioningJTI                                (unchanged, now after all rejections)
9. SignRelayCert(relayID, csr, storedDNS, storedIPs)  ← stored lists, not request lists
10. MarkProvisioned                                   (unchanged; keeps its own status guard)
```

The `MarkProvisioned` guard stays: it covers a row revoked between step 6 and step 10.

### C2 — allowlist semantics

- `allowedDNS := row.DNSAllowlist`; `allowedIPs := parse(row.IPAllowlist)`. These are the lists already validated and normalised at creation (`admin_handler.go:73-86`).
- Each entry in `req.DnsSans` / `req.IpSans` must appear in the corresponding stored list. Otherwise return `PermissionDenied "requested SAN not in relay allowlist"` and name the offending value in the log, not in the response.
- **Pre-check the CSR too:** every DNS/IP SAN in the parsed CSR must be in the stored lists, checked in step 7 so the rejection happens before the burn (I2).
- Pass the **stored** lists to `SignRelayCert`. Its existing check (`pki/relay.go:112-158`) stays as the second line of defense.
- An empty stored list forbids that SAN type. This is the semantics already documented on the proto fields ("Pass empty lists to forbid that SAN type entirely").

### C4 — proto comments (no field changes)

- `rpc Provision`: provisioning-token authentication **is enforced** (ADR-020).
- `ProvisionRequest.provisioning_token = 1`: "single-use provisioning token minted by `POST /provider/relays`; required."
- `dns_sans = 6` / `ip_sans = 7`: "SANs the relay requests; each must be in the allowlist registered on the relay row at `POST /provider/relays`. Empty = none requested."

Field numbers and types are untouched. `buf generate` must produce no Go/Rust API change other than
comments.

### C5 — local scripts

Add usage text in the three relay scripts: `RELAY_DNS_SANS` / `RELAY_IP_SANS` must be a subset of
the `dns_allowlist` / `ip_allowlist` sent to `POST /provider/relays`. Example: `run-relay-local.sh`
defaults `RELAY_IP_SANS=127.0.0.1`, so the relay must be created with `"ip_allowlist": ["127.0.0.1"]`.
No behavioural change to the scripts.

## Invariants

- **I1** Relay cert DNS/IP SANs ⊆ stored `dns_allowlist` / `ip_allowlist` of that relay.
- **I2** No rejection in steps 6–7 consumes the token. The JTI is still present in Valkey afterwards.
- **I3** The single-use token and `MarkProvisioned` status guard (ADR-020) are unchanged.
- **I4** Already-provisioned relays are unaffected. Phase F (renewal) applies the same allowlist rule on renewal.
- **I5** No proto field number or type changes.

## Tests (`provision_test.go`)

- CSR and request SANs ⊆ stored allowlist → signed; row `active`; JTI burned.
- Request `dns_sans` contains a name not in `dns_allowlist` → `PermissionDenied`; **JTI still present**; row still `pending`.
- Request lists empty but CSR carries a DNS SAN not in the stored allowlist → `PermissionDenied` from the step-7 CSR pre-check; **JTI still present** (`SignRelayCert` would also reject it, but only after the burn).
- Stored lists empty, CSR with only the SPIFFE URI SAN → signed.
- Stored lists empty, request `ip_sans=["127.0.0.1"]` → `PermissionDenied`; JTI present.
- Relay row `revoked` / `active` (already provisioned) → `FailedPrecondition`; JTI present.
- Unregistered relay → `FailedPrecondition` (existing case, now before burn).

## Acceptance Criteria

> **Status:** the first two items are live checks against a running controller + relay and are
> **PENDING / NOT YET RUN**. Their behaviour is covered by `provision_allowlist_test.go`
> (see Implementation Checklist).

- [ ] **PENDING** — A relay created with `ip_allowlist: []` that requests `RELAY_IP_SANS=10.0.0.5` fails to provision; re-running with the SAN removed succeeds **with the same token**.
- [ ] **PENDING** — A relay created with `dns_allowlist: ["relay1.example.com"]` provisions with that SAN and cannot obtain `relay2.example.com`.
- [x] `buf generate` diff is comment-only (verified: only two trailing field comments changed in `relay.pb.go`; tags identical).

## Build Check

```bash
buf generate                              # repo root
cd controller && go build ./...
cd controller && go test ./internal/relay/... ./internal/pki/...
cd relay && cargo build                   # regenerated comments only
```

## Implementation Checklist

- [x] **M2-C1** Load row before burn; require `pending` (`LoadRelayByID` added to the `heartbeatStore` interface; the `Store` method already existed, so `store.go` is unchanged)
- [x] **M2-C2** Allowlist check; stored lists passed to `SignRelayCert`
- [x] **M2-C3** All rejections before `BurnProvisioningJTI`. `precheckRelayCSR` mirrors every `SignRelayCert` check (signature, SPIFFE URI SAN, P-384, DNS/IP SANs); the signer still enforces them independently
- [x] **M2-C4** Proto comment fixes
- [x] **M2-C5** Script usage text
- [x] **M2-C6** Tests:
  - `provision_test.go`: fakes updated (`fakePKI` records allowlists, store `LoadRelayByID`, SPIFFE-URI CSR helper); unregistered relay now asserts **no** burn
  - `provision_allowlist_test.go`: signer receives stored allowlists; request/CSR DNS/IP SAN outside allowlist; empty allowlist (URI-only OK, IP rejected); relay not pending (active/inactive/revoked/deleted); wrong CSR identity before burn; retry with same token; canonicalization (mixed-case/trailing-dot request DNS accepted, non-canonical CSR DNS rejected before burn, IPv6 parsed comparison, `canonicalDNSName`, dedupe across equivalent forms)
- [x] **Build gate:** `buf generate`, `go build ./...`, `go test ./internal/relay/... ./internal/pki/...` (0 skips with `PKI_TEST_DATABASE_URL`), `go test ./...`, relay `cargo build`. Re-run after rebasing onto Phase B (`f882c3c`)
- [ ] **Live acceptance — PENDING / NOT YET RUN** (see Acceptance Criteria)

**Implementation notes** (implementation-level, within the Decision Record):
- Request DNS SANs are normalized (lowercase, one trailing dot stripped); IPs are parsed and compared with `net.IP.Equal`.
- CSR DNS SANs must already be canonical. `SignRelayCert` copies CSR names verbatim and matches them exactly, so a normalize-only pre-check would let a mixed-case name fail at the signer **after** the burn.
- Admin relay creation keeps its strict `validateDNSSANs` / `validateIPSANs`, so stored allowlists are canonical by construction.

## Post-Phase Fixes

_None yet._
