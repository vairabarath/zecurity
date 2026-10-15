# Certificate Lifecycle Audit — 2026-10-15

Audit of every component's certificate lifecycle (enroll, issue, install, verify, renew, revoke, expire): client, connector, shield, relay, controller, CAs and revocation lists. Baseline: `38e38c6`.

Each bug is fixed in the same way:
1. A test that demonstrates the bug.
2. The fix.
3. Mutation checks: break each guard on purpose and confirm a test fails.

Suggested order: **2 → 1 → 3 → 4 + 5 → 7 → 6 → 8 → 9 → 10.**

## Certificate hierarchy (for reference)

```
Platform Root CA (10y, self-signed, ca_root) ── signs only the intermediate
└── Platform Intermediate (5y, served at /ca.crt)
    ├── Controller gRPC server cert
    ├── Relay certs
    └── Workspace CA (2y, one per workspace, SAN tenant:<id>)
        ├── Connector certs
        ├── Shield certs
        └── Client (device) certs
```

## Confirmed bugs

- [x] **Bug 2 — High — relay: the S1 fix could be bypassed.** The relay chose which workspace's revocation list (CRL) to check from a forged copy of the issuer CA, and the CRL cache ignored which CA was presented.
  - Fixed in PR #118 (merged): `b7bf4a6` demonstrates the bug, `d80dedb` fixes it.
- [ ] **Bug 1 — High — connector, shield, relay, client: first-time trust can be hijacked.**
  - Agents pinned only the first certificate in `/ca.crt` but trusted all of them. Enrollment replies were saved without validation, the private key was written before validation, and writes were not atomic.
  - The client fetches `/ca.crt` with no pin at all.
  - Progress:
    - [x] Attack-reproduction tests: `5387d43`.
    - [x] Connector: exact pin, full reply validation, atomic install with cleanup (`84195e4`).
    - [x] Shield: same, plus renewal validated against the saved anchors; renewal never rewrites `workspace_ca.crt` (`564561a`).
    - [x] Relay: same; the existing returned-intermediate pin check is kept and regression-tested (`30cd9cd`).
    - [ ] Client: **waiting on the design decision.** Proposal: embed the platform root certificate in the signed client package and verify `/ca.crt` against it. Then apply the agents' pinning and reply validation, and save the validated chain so logout and renewal reuse it. Next steps: attack tests first, then the fix.
- [ ] **Bug 3 — High — shield: revoking a shield doesn't cut it off.**
  - Revoke only updates the database (`shield.resolvers.go:78`). The connector is never told, and policy still routes resources through revoked shields (`policy/store.go:314`).
  - Decision needed: put shields on a CRL, or have the controller notify the connector? Should open connections be closed?
- [ ] **Bug 4 — High — client: a user being deprovisioned can still enroll a new device.**
  - Enrollment checks only that the login token is genuine and unexpired, which turns a 15-minute token into a 7-day certificate.
  - Policy checks only device revocation, never the user's status (`policy/store.go:346`).
- [ ] **Bug 5 — Med-High — client: disabling an IdP connection suspends its users but doesn't revoke their devices.**
  - Their devices keep access for up to 7 days.
  - Decision needed for bugs 4 and 5: should policy exclude devices of non-active users, should every suspend path revoke devices, or both?
- [ ] **Bug 6 — Medium — connector, client: the relay CRL and workspace CRL can be swapped for each other.** Each is checked against a bundle containing both signing CAs (`connector/src/main.rs:195`, `crl.rs:149-155`).
- [ ] **Bug 7 — Medium — controller: a revoked connector can still renew its shields' certificates.** `shield/renewal.go` never checks the connector's status.
- [ ] **Bug 8 — Medium — connector, shield: re-issuing an enrollment token doesn't cancel the old one.**
  - The old token stays valid for up to 24 hours.
  - Two tokens used at once can produce a certificate the database doesn't track.
- [ ] **Bug 9 — Medium — all: CRLs cover only the newest certificate.**
  - Each renewal overwrites the stored serial, so an earlier, still-valid certificate isn't revoked.
  - Needs a new table: a schema change and a DB reset.
- [ ] **Bug 10 — Low-Med — controller: connector and shield renewal accept a different key.** Stolen credentials can be moved to an attacker's key.

## Risks (need an extra condition, or latent today)

- [ ] The controller can be impersonated by a relay certificate whose allowed names match the controller's. Relays and shields never check the controller's identity.
- [ ] The client fetches its trust anchor over plain HTTP with no pin. This is the client part of bug 1.
- [ ] One signing secret is shared by login tokens and all enrollment tokens; only relay tokens carry an audience.
- [ ] Revocation doesn't cut existing relay sessions of revoked connectors and clients.
- [ ] Clients never check whether a connector is revoked.
- [ ] The connector's device port trusts the platform intermediate, so leaves from other workspaces pass TLS. Only the policy's exact identity match stops them.
- [x] The shield trusted whatever its connector returned on renewal, including a changed CA. Fixed with bug 1 (`564561a`).
- [ ] A compromised connector can claim shields in its network, or change their status.
- [ ] The controller's identity check lets unknown roles through unverified. No handler is exposed today.
- [ ] Old CRLs can be replayed within their validity window. The CRL endpoints sign a new list on every request with no limit (an easy denial of service).
- [ ] The PKI auto-reset is silent: on a fresh install it can wipe and regenerate the root and intermediate without logging.
- [ ] CA expiry (known): workspace CAs last 2 years, the platform intermediate 5.
- [ ] Relay crash between the provisioning file renames leaves a partial state that is rejected at startup, so the relay needs re-provisioning with a new token. It fails closed but needs manual action. Found during bug 1.

## Already verified as holding up

- Single-use enrollment tokens.
- The controller sets every identity.
- Workspace isolation in the controller.
- Expired certificates refused at every new connection.
- Revoked status is terminal for connectors and relays.
- Relay provisioning checks its allowlist before consuming the token.
- Login tokens can't be forged by switching the signing algorithm.
- CA private keys are encrypted per workspace.
- Earlier fixes B1, B2, B4, K1 and K2 still work.
