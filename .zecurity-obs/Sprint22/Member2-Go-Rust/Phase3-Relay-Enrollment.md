---
type: phase
member: M2
person: Barath
sprint: 22
phase: 3
execution: E
title: Relay Enrolment from the Console
status: implemented   # 2026-10-27 on sprint22/phase-e-relay-enrollment, not committed; live AT-E1…E3 PASS on an isolated local stack, lab run (AT-E4/E5) open
depends_on: [2]   # A merged
schema_change: false
tags:
  - go
  - react
  - provider
  - relay
  - provider-console
---

# Phase 3 (E) — Relay Enrolment from the Console

> **Decision Record:** **D-30** (console may create relays), **D-31** (relay-ops may create), **D-20** (expired token → replace the relay; no re-issue), **D-14** (audit).

## Problem (verified 2026-10-26)

- `POST /provider/relays` returns only `{relay_id, provisioning_token, expires_at}`.
- A relay also needs `CONTROLLER_ADDR`, `CONTROLLER_HTTP_ADDR` and `RELAY_CA_FINGERPRINT` (`relay/src/config.rs`). Operators compute the fingerprint by hand today (`docs/sprint20-live-acceptance-runbook.md` §6.3: `curl /ca.crt | openssl x509 -outform DER | sha256sum`).
- The console has no way to create a relay.

## Goal

From the Relays page, a super-admin or relay-ops operator adds a relay and gets, once, a provisioning token plus a complete install command to paste on the relay host.

## Contract change — `POST /provider/relays` → 201

```json
{
  "relay_id": "…",
  "provisioning_token": "…",
  "expires_at": "…",
  "ca_fingerprint": "<sha256 of the intermediate CA DER, lowercase hex>",
  "controller_grpc_addr": "<PROVIDER_RELAY_CONTROLLER_ADDR or empty>",
  "controller_http_addr": "<PROVIDER_RELAY_CONTROLLER_HTTP_ADDR or empty>"
}
```

- `Cache-Control: no-store`.
- The fingerprint comes from a helper shared with `shield/token.go` `loadCAFingerprint` (extract it; don't duplicate the SQL).
- The two settings are **optional**; `.env.example` documents them.

## Console

- **Add relay** button on Relays (both roles; hidden for unknown roles).
- **Form:** name (required), DNS names and IP addresses as chip inputs, validated with the same rules as the server; server errors mapped to field messages.
- **Result dialog**, shown once:
  - the token with **Copy**, its expiry ("valid for 24 hours");
  - an install block with every variable filled in (placeholders where a controller address setting is empty):
    ```bash
    RELAY_ID=… CONTROLLER_ADDR=… CONTROLLER_HTTP_ADDR=… \
    RELAY_CA_FINGERPRINT=… RELAY_PROVISIONING_TOKEN=… \
    RELAY_DNS_SANS=… RELAY_IP_SANS=… zecurity-relay
    ```
  - closing the dialog discards the token; the list refreshes and shows the new **pending** relay.
- **Pending relays** (list and detail): "Waiting for enrolment"; hint per D-20: "If the token has expired, delete this relay and add a new one."

## Tests (write first)

**Go:** response fields; fingerprint equals the SHA-256 of `/ca.crt` DER; settings empty vs set; `no-store` header; audit unchanged (`relay.create`, no token in details).

**Console:**
- Form validation mirrors the server; server errors map to fields.
- Token dialog: shown once; Copy works; token never in `sessionStorage`, the URL, or the DOM after close.
- Install block contains every variable; placeholders when settings are empty.
- relay-ops sees **Add relay**; API-surface pin includes `POST /provider/relays`; clipboard static guard allows exactly the token dialog and the temporary-password dialog.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-E1 … AT-E5** (incl. the live enrolment on the lab).

## Test gate

Go gate and console gate from [[Sprint22/path]].

## Implementation record (2026-10-27)

**Controller:**
- `internal/pki/fingerprint.go` (new): `IntermediateFingerprint(ctx, q)` reads the row `/ca.crt` serves and returns the SHA-256 of its DER; `FingerprintPEM`. The shield token (`loadCAFingerprint`) and the connector token handler now use it, so the SQL lives in one place.
- `internal/relay/admin_handler.go`:
  - `AdminHandler` gains `CAFingerprint`, `ControllerGRPCAddr`, `ControllerHTTPAddr`;
  - Create reads the fingerprint **before** the first write, so a failure leaves no relay row, token or audit row (500 `server_error`);
  - the 201 body adds `ca_fingerprint`, `controller_grpc_addr`, `controller_http_addr` (always present; empty string when the setting is unset);
  - `ValidateInstallAddr`: host:port, no scheme or path.
- `cmd/server/main.go`: wires the fingerprint and the optional `PROVIDER_RELAY_CONTROLLER_ADDR` / `PROVIDER_RELAY_CONTROLLER_HTTP_ADDR` (`optionalInstallAddr`: a malformed value stops startup). `.env.example` documents both.

**Console:**
- `src/api/relays.ts` `createRelay`; `CreateRelayResponse` and the create error codes in `types.ts`; `src/lib/relay-errors.ts`.
- `src/lib/relay-install.ts`: SAN checks mirroring `validateDNSSANs` / `validateIPSANs` (incl. Go's canonical IPv6 form and duplicates); `buildInstallCommands` (POSIX single-quoted values, placeholders when an address is unset).
- `src/components/AddRelayDialog.tsx`, `src/components/ProvisioningTokenDialog.tsx`; Relays page: **Add relay** (super-admin, relay-ops), "Waiting for enrolment" under pending rows; Relay detail: pending notice with the D-20 hint.
- `api-surface.test.ts` pins `POST /provider/relays`; the clipboard static guard allows exactly the two one-time secret dialogs.
- Login page copy no longer says "Read-only fleet view".

**Deviations from the plan above (deliberate):**
- DNS names and IP addresses are plain text fields (comma- or space-separated) with per-entry validation, not chip inputs: same rules, less UI.
- The dialog shows two commands: the systemd installer (`scripts/relay-local-install.sh`, the main path) and running `zecurity-relay` directly. Both carry every variable.
- `RELAY_DNS_SANS` / `RELAY_IP_SANS` are filled with the full allowlists (the controller signs the stored allowlists anyway).
- Server errors show in the dialog's error banner (with the server's `detail` for SAN codes), not under a field: the server names the bad value but not the field position.

**Tests (written first, confirmed failing first):**
- Go: `internal/pki/fingerprint_test.go`; `TestValidateInstallAddr`; DB-backed `TestAdminDB_CreateReturnsInstallParameters` (fingerprint = SHA-256 of the stored Intermediate DER, addresses echoed), `…UnconfiguredAddressesAreEmptyStrings`, `…CreateWithoutIntermediateLeavesNoState`; `TestAdminDB_CreateByRelayOps` now seeds an Intermediate.
- Console: `relay-install.test.ts` (25), Relays (+14, and the read-only test now expects exactly one mutating control: both roles, validation, duplicates, server codes, placeholders, copy-on-click only, outside-click keeps / Escape discards, token gone from DOM/storage/URL after close, reload on close, empty form on reopen, pending hint), RelayDetail (+2).

**Gates (2026-10-27):** `go test ./...` 1420 pass / 0 fail / 2 skip (the two unconditional `t.Skip` stubs, `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed` and `TestEnroll_CSRSignatureInvalid`; every DB- and Valkey-backed test ran). Console: 436 tests pass, lint clean, build OK.

**Live (2026-10-27): AT-E1 … AT-E3 PASS** on an isolated local stack; see [[Sprint22/Acceptance-Test-Plan]] → "Live run — Phase E".

## Post-Phase Fixes

### Fix: the relay installer stops on another relay's leftover state (2026-10-31, branch `fix/relay-installer-stale-state`)
**Issue:** in V4 case 3 (2026-10-30) the console's install block ran on a host that still held the earlier AT-E4 relay's certificate in `/var/lib/zecurity-relay/pki`. The service had only been disabled, not uninstalled. The new relay then refused to start: "Relay certificate ID … does not match configured Relay ID …". The installer gave no warning, and the new token went unused until the state was cleared by hand.

**Root Cause:** `scripts/relay-local-install.sh` created the state dir but never looked at what was already in it. The relay itself is right to refuse a mismatched identity.

**Fix Applied (`scripts/relay-local-install.sh`, new `check_existing_state`, run before anything is installed):**
- **Reads the existing certificate's relay ID** from its SPIFFE URI SAN (`spiffe://…/relay/<id>`) with openssl.
- **Same ID:** a reinstall; the state is kept.
- **Another relay's certificate, an unreadable one, or no openssl:** the installer stops, naming both IDs and how to proceed.
- **With `RELAY_RESET_STATE=1`:** a running relay is stopped and the old files are moved to `<state dir>.replaced-<timestamp>` (mode 0700). A key is never deleted.
- `scripts/relay-uninstall.sh` (which removes the state unless `--keep-state`) is still the clean way to retire a relay.
- The legacy `relay-local-install-omitid.sh` (local tests, self-generated ID) is unchanged.

**Tests:** `scripts/test-relay-installer-state.sh`, which runs without root: real openssl certificates, a fake systemctl, 18 checks.
- **Cases:** no state, same relay, another relay (refuse, files untouched), reset (stop, move, private backup), `RELAY_RESET_STATE=yes` still refuses, unreadable certificate, no openssl.
- **Mutation checks:** 5 mutants killed (never refuse, any certificate accepted, delete instead of move, world-readable backup, service not stopped).


### Fix: Add relay reopened with the previous relay's values
**Issue:** after a successful create, opening **Add relay** again showed the last name and SANs (found in the live run).

**Root Cause:** `AddRelayDialog` keeps its own form state and only reset it on Cancel.

**Fix Applied (`src/pages/Relays.tsx`):** the page bumps a `key` on every opening, so the dialog remounts with an empty form. Test: "after a successful create, Add relay opens with an empty form".

*Note:* `AddOperatorDialog` (`OperatorDialogs.tsx`) resets the same way (Cancel only), so it likely has the same behaviour after a successful add. Not changed here (outside Phase E).
