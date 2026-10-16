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
- [x] **Bug 1 — High — connector, shield, relay, client: first-time trust can be hijacked.**
  - Agents pinned only the first certificate in `/ca.crt` but trusted all of them. Enrollment replies were saved without validation, the private key was written before validation, and writes were not atomic.
  - The client fetches `/ca.crt` with no pin at all.
  - Progress:
    - [x] Attack-reproduction tests: `5387d43`.
    - [x] Connector: exact pin, full reply validation, atomic install with cleanup (`84195e4`).
    - [x] Shield: same, plus renewal validated against the saved anchors; renewal never rewrites `workspace_ca.crt` (`564561a`).
    - [x] Relay: same; the existing returned-intermediate pin check is kept and regression-tested (`30cd9cd`).
    - [x] Client (decided 2026-10-16: embed the platform root; official releases carry the production root, dev and LAN controllers build with their own via `client/scripts/dev-build.sh`):
      - `/ca.crt` must be exactly one intermediate issued by the embedded root; it is the only certificate trusted for login.
      - The enroll reply is validated before anything is saved.
      - Renewal must match the saved anchors, which it never replaces.
      - Logout reuses the saved anchors.
      - Daemon startup refuses saved state not anchored in the root.
      - Release builds without a root fail.
- [x] **Bug 3 — High — shield: revoking a shield didn't cut it off.**
  - Revoke only updated the database. The connector was never told, and policy kept routing resources through the revoked shield, so user traffic kept flowing through it.
  - Fixed (decided 2026-10-16):
    - **Durable revocation:** the revocation, a tombstone (`shield_revocations`, which outlives the shields row) and the propagation work (`shield_revocation_outbox`) commit in one transaction. A trigger stops a revoked identity from ever being reused.
    - **Reliable delivery:** a dispatcher invalidates the policy and pushes the workspace's complete revoked list to every connected connector, retrying until delivered. A connector also receives the full list whenever its stream opens.
    - **Unreachable resources:** resources behind a revoked or missing shield get no ACL entry until an admin unprotects or reassigns them. There is no fallback to direct routing.
    - **Connector cut-off:** it accepts no shield until it has applied the list. On revocation it closes the shield's stream, unregisters it, discards its snapshot, ends every session through it (including ones being set up at that moment) and refuses its identity from then on.
  - A connector admits shields only on a list received over its **current** controller stream. Every (re)connect or renewal switch makes it stop admitting new shields until that stream delivers its list.
  - **Deploy order:** the controller must be deployed before the connector. A connector from this change accepts no shields until it receives the list. Migration `038_shield_revocations.sql` requires a local DB reset.
  - Follow-ups:
    - [ ] Shield CRL as an additional layer (deferred).
    - [ ] **Disconnected-connector window (accepted limitation).** While a connector cannot reach the controller, nothing can tell it about a new revocation:
      - a revoked shield that is already connected to it keeps its control stream, and its user sessions keep flowing;
      - a revoked shield may also connect to it if it was not revoked in the connector's last list;
      - this lasts until the connector's controller stream comes back, at which point the list is applied immediately: the stream is closed, sessions are torn down and the identity is refused.

      Refusing all new shield connections for the whole outage would close the second gap, but would lock out a shield that merely restarts during a controller outage. That trade-off needs a product decision.
    - [ ] **Live end-to-end acceptance pending:** revoke a real enrolled shield mid-session and confirm the stream is closed, sessions end and reconnects are refused, on the lab machines.

## Found during the bug 3 verification (2026-10-16), pre-existing and unrelated

- [ ] **34 controller tests needed a pre-seeded shield.** `RESOURCE_TEST_SHIELD_ID` (resource-ACL coherence, posture resolvers, snapshot generation, ack/reap) skipped everywhere because the URL is used as the database itself. They now run with `scripts/resource-test-fixture.sh`: 30 pass, and the same 4 fail on `fixed-pendings`:
  - `TestUpdateResource_InvalidatesWhenACLFieldChanges` and `TestForceDeleteResource_Invalidates`: the resolvers call `NotifyPolicyChange` twice, an unconditional call plus the later conditional one.
  - `TestUpdateResource_NoInvalidateForIrrelevantField`: the unconditional call fires even for ACL-irrelevant updates.
  - `TestProtectResource_DoesNotInvalidate`: the test is outdated. Since migration 021, protecting changes the route from connector to shield, so a notification is correct.

  The effect is extra ACL recompiles and pushes, never fewer. These tests should be moved to the throwaway-database pattern.
- [ ] **Leaked test databases.** A `graph/resolvers` SCIM-config test creates `resolvers_updsc_*` databases and never drops them; hundreds accumulate on a developer's Postgres.
- [x] **Bug 4 — High — client: an inactive user could still enroll or renew a device certificate.** `EnrollDevice` and `RenewCert` trusted the access token alone, so a just-suspended user's token (valid up to 15 minutes) could mint or renew a 7-day certificate.
  - Fixed (decided 2026-10-16): both now read the user's current status, session generation, workspace and membership from the database, as `/auth/refresh` does, and fail closed.
  - The final conditional write re-checks status and membership, so a suspension during issuance returns no certificate, and a refused enrollment leaves no device row.
- [x] **Bug 5 — Med-High — client: devices of inactive users kept access.** The policy authorized every unrevoked device of a group member, whatever the user's status.
  - Fixed: a device authorizes only while its user is `active` and, if the user has a `workspace_members` row, that membership is `active`. A missing row does not deny on its own, because SCIM and first-login users have none.
  - Suspension is reversible; revocation is not.
  - IdP-connection disable and delete, and SCIM reactivation, now record durable policy invalidation (outbox event `policy.invalidation.requested`, or the re-enrollment consumer) atomically with the change. Previously disable was not atomic and had no invalidation; delete and reactivation notified best-effort after commit.
  - Follow-ups:
    - [ ] **No admin lifecycle API exists yet:** there is no admin mutation to suspend, lock, delete or reactivate a user, and nothing sets `users.status = 'locked'`. When these mutations are built, each must commit its policy invalidation (`policy.NewInvalidationEvent` via the outbox) in the same transaction as the status change.
    - [ ] **Membership model gap:** SCIM provisioning and first-login (JIT) create users without `workspace_members` rows, and nothing ever sets a membership to `suspended`. Making membership authoritative would need provisioning changes and a backfill.
    - [ ] IdP-disable session revocation (generation bump) stays best-effort after commit, as before. Access is cut off by the user suspension and policy invalidation, which are atomic.
    - [ ] Tokens without a generation claim (`gen` = 0, pre-Phase-5) are accepted by `CheckGeneration` by convention, as at `/auth/refresh`.
- [ ] **Bug 6 — Medium — connector, client: the relay CRL and workspace CRL can be swapped for each other.** Each is checked against a bundle containing both signing CAs (`connector/src/main.rs:195`, `crl.rs:149-155`).
- [x] **Bug 7 — Medium — controller: a revoked connector can still renew its shields' certificates.** `shield/renewal.go` never checked the connector's status or the shield's tombstone, and its final write was unconditional. The unary SPIFFE interceptor verified a connector's certificate chain but never its revocation.
  - Why it matters: revocation does not shorten a certificate's lifetime. A revoked connector with an unexpired certificate could keep minting 7-day certificates for its shields, extending its foothold past revocation. A revocation racing a renewal could also still produce an accepted certificate.
  - Fixed (2026-10-16, controller only, no schema change):
    - **Interceptor:** every connector unary RPC (connector `RenewCert` and `Goodbye`, and the shield `RenewCert` and `Goodbye` it proxies) now reads the connector's standing from the database, scoped to the certificate's trust domain. A revoked connector (`status = 'revoked'` **or** `revoked_at` set) or an unknown one is refused with `PermissionDenied`; unreadable state fails closed with `Unavailable`. This follows the relay revocation pattern; relays and bootstrap RPCs are unchanged.
    - **Shield renewal:** before signing, the handler requires the shield to have no tombstone (even when its row is gone), to be managed by the calling connector, not revoked or deleted, in the caller's trust domain, and its connector to be unrevoked in that trust domain. The check costs no extra query: one read covers the tombstone, shield and connector. Unreadable state returns `Internal`, and nothing is signed.
    - **Race-safe final write:** the certificate is recorded only if a single conditional `UPDATE` re-confirms all of the above. It locks the connector row `FOR SHARE`, so a concurrent connector revocation waits or is seen, and a concurrent shield revocation conflicts on the shield row. If no row is updated, or the write fails, no certificate is returned.
    - **Goodbye** also requires `revoked_at IS NULL`, so it can never alter a revoked connector's row (defence in depth behind the interceptor).
  - Verified with throwaway-DB tests (direct and in-flight races, write failure) and 31 mutations, all caught.
  - Limitations:
    - [ ] The stream interceptor (`Control`) does not consult standing; the `Control` handler already refuses revoked connectors itself.
    - [ ] Shield certificates renewed before a connector was revoked stay valid until they expire (7 days). Revoking a connector does not revoke its shields.
    - [ ] Not validated live end to end (a real connector proxying a renewal after revocation).
- [ ] **Bug 8 — Medium — connector, shield: re-issuing an enrollment token doesn't cancel the old one.**
  - The old token stays valid for up to 24 hours.
  - Two tokens used at once can produce a certificate the database doesn't track.
- [ ] **Bug 9 — Medium — all: CRLs cover only the newest certificate.**
  - Each renewal overwrites the stored serial, so an earlier, still-valid certificate isn't revoked.
  - Needs a new table: a schema change and a DB reset.
- [ ] **Bug 10 — Low-Med — controller: connector and shield renewal accept a different key.** Stolen credentials can be moved to an attacker's key.

## Found during the bug 1 work (2026-10-16)

- [x] **High — client: access and refresh tokens were sent over plain HTTP.**
  - `client/src/auth.rs` POSTed to `{http_base}/auth/refresh` and `/auth/logout`, plain HTTP by default, with the refresh token in an `X-Refresh-Token` header. A passive eavesdropper could take over the session.
  - Fixed (decided 2026-10-16: option A): refresh and logout now use the new `ClientService.RefreshSession` and `Logout` RPCs over the gRPC channel pinned to the saved anchors. The controller shares the session rules with the browser's cookie path.
  - `/auth/refresh` no longer accepts the header, so clients that haven't updated must log in again.
- [ ] **Medium — client releases are not signed.** `client-release.yml` publishes binaries plus a `checksums.txt` file in the same release. That file isn't a signature: whoever can replace the binary can replace it too. The embedded root is only as trustworthy as the download channel (HTTPS from GitHub). Next step: sign the releases (minisign or cosign) and verify the signature in `client-install.sh`.
- [ ] **Planned: root rollover support.** The client embeds exactly one root, so rotating the root (10 years, or after a compromise) requires every client to update first. To allow a smooth switch:
  - the client should accept a set of roots;
  - a release would ship with both old and new roots before the controller rotates;
  - a later release drops the old one.

  The controller also has no root-rotation mechanism yet. The intermediate can rotate without any client change.
- [ ] **Production PKI must never reset.** With the root embedded in clients, a regenerated production root locks out every installed client. Lock down `pki/service.go` `canResetPKI` for production (see the "silent PKI auto-reset" risk below).

## Risks (need an extra condition, or latent today)

- [ ] The controller can be impersonated by a relay certificate whose allowed names match the controller's. Relays and shields never check the controller's identity.
- [x] The client fetched its trust anchor over plain HTTP with no pin. Fixed as the client part of bug 1.
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
