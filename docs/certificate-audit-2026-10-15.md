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
- [x] **Bug 6 — Medium — connector, client: the relay CRL and workspace CRL can be swapped for each other.** Each was checked against a bundle containing both signing CAs (`connector/src/main.rs:195`, `crl.rs:149-155`), and the CRL's own issuer name picked which one.
  - Why it matters: the Workspace CRL (`/ca.crl`, revoked devices and connectors) is signed by the Workspace CA; the Relay CRL (`/relay.crl`, revoked relays) by the Platform Intermediate. Both are fetched over plain HTTP. Anyone on the path could serve the genuine Relay CRL as `/ca.crl`, so the connector would accept revoked devices and connectors, or the genuine Workspace CRL as `/relay.crl`, so the connector and the client would accept revoked relays. Both carry valid signatures, so signature checks alone could not tell them apart.
  - Fixed (2026-10-17, connector and client only; no controller, schema or proto change, no Issuing Distribution Point):
    - **One issuer per CRL:** `validate_crl` takes exactly one issuer certificate and refuses a bundle. The connector checks `/ca.crl` against the Workspace CA and `/relay.crl` against the Platform Intermediate; the client checks `/relay.crl` against the Platform Intermediate.
    - **Roles from the chain, not the order:** `crl_issuers` (in each crate's `crl.rs`) derives both issuers from the stored two-certificate bundle. It requires exactly two distinct, currently valid CA certificates that may sign certificates and CRLs, where the Workspace CA is issued (name and signature) by the Intermediate and not the reverse. The Intermediate must not be self-signed and must be allowed to issue a CA (no path length 0), and the two must not share a subject. Anything malformed, ambiguous, unexpected or invalid is rejected. The bundle itself is still anchored in the platform root by the existing enrollment and renewal checks (D1).
    - **Fail closed:** if the issuers cannot be derived, the CRL managers stay empty, so `check` returns `Unavailable` and every caller denies (device tunnel, relay dialing, client relay routing). A rejected CRL never replaces a verified one and never reads as an empty list. Expiry, freshness, signature, AKI and parsing checks are unchanged.
    - Startup moved into tested functions (`crl::start_crl_managers` in the connector, `crl::start_relay_crl` in the client).
  - Verified: tests first (the swap attacks succeeded against the old behaviour), startup tests against a local HTTP server serving swapped CRLs, and 41 mutation runs across both crates: all caught except 2 equivalent mutations per crate (the duplicate-certificate and self-signed-Workspace-CA checks, which other checks make unreachable).
  - Limitations:
    - [ ] `crl_issuers` is duplicated in the connector and client crates (no shared crate), like the rest of `crl.rs`.
    - [ ] The CRL issuers are fixed at startup from the bundle on disk; a Workspace CA rotation needs a restart (existing behaviour).
    - [ ] Not validated live end to end (a real connector and client fetching swapped CRLs from a controller).
- [ ] **Follow-up (found during bug 6): the shield enforces no revocation at all.** It never fetches or checks a CRL, so a revoked connector whose certificate has not expired can still reach its shields. Separate from bug 6; to be designed on its own.
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
- [x] **Bug 8 — Medium — connector, shield: re-issuing an enrollment token didn't cancel the old one.**
  - Root cause: minting (`connector/token_handler.go`, `shield/token.go`) stored the new token's ID in Valkey and in the row's `enrollment_token_jti`, but never removed the previous entry, and `Enroll` never compared the presented token with the row's current one. Every earlier token stayed usable until it expired (up to 24 hours); connector tokens are minted on each detail-page load. Enrollment burned the Valkey entry first, checked the row without a lock and activated it with an unconditional `UPDATE … WHERE id`, so two valid tokens used at once were both signed while only the last writer's serial was recorded. The other certificate worked but could not be revoked, because CRLs list recorded serials only. The shield's minting update also ignored whether a row changed. A failed activation left the token burned, so a legitimate retry was refused.
  - Fixed (2026-10-17, controller only; no schema, proto, Rust or relay change):
    - **The row is the authority on which token is current.** `Enroll` (connector and shield) claims the row with `SELECT … FOR UPDATE` requiring `status = 'pending'` and `enrollment_token_jti` = the presented token's ID. A superseded or already used token is refused before anything is signed, whatever Valkey still holds.
    - **Serialized enrollment.** One transaction: claim → existing checks → sign → (connector) CA certs → conditional activation (still pending, still this token, exactly one row) → commit. A certificate is returned only after its serial is committed. The signers only read CA keys, so a certificate signed in a transaction that then fails never leaves the controller.
    - **Valkey ordering.** The entry is checked (not consumed) before the claim and deleted after the commit. A failed activation or commit therefore leaves the token usable for a retry. An entry left behind by a failed post-commit delete cannot enroll again, because the row no longer names the token.
    - **Re-issue.** Store the new entry, then swap the row's token ID under the row lock and only while pending, requiring exactly one row. A refused swap drops the new entry (409 from both token endpoints). The predecessor's entry is deleted as best-effort cleanup; the swap, not the delete, is what supersedes it.
  - Verified: 23 regression tests (connector and shield) on throwaway databases with real PKI signing. 16 failed against the original code (superseded token, predecessor cleanup, concurrent superseded and current tokens producing an untracked certificate, re-issue racing enrollment, shield re-issue for a non-pending row, retry after a failed write or commit). The other 7 guard behaviour the old code already had or the fix keeps. Lock-wait detection instead of sleeps; stable 5× under `-race`. Full suite twice; mutation testing. The activation predicates and row-count check survive mutation alone, because the claim lock makes them unreachable; removing them together with the lock returns an untracked certificate, so they stay as the second layer.
  - Limitations:
    - [ ] Valkey and Postgres are not updated atomically. Any mismatch leaves at most an unusable leftover entry (the row decides), or a token refused until re-issued (its entry missing). It never creates a usable stale token.
    - [ ] A commit error after the server actually committed leaves the connector or shield enrolled without the client holding its certificate; it must be re-created. Safe direction: never an untracked certificate.
    - [ ] Not validated live end to end (real connector and shield installs with re-issued tokens).
  - **Relay provisioning is out of scope.** A relay's provisioning token is issued once, when the relay is created, and there is no endpoint to re-issue it, so no token can be superseded. Revisit if a re-issue path is ever added: it must then bind provisioning to `relays.enrollment_token_jti` the same way.
- [x] **Bug 9 — Medium — connector, client: the workspace CRL covered only the newest certificate.**
  - Root cause: `GenerateClientCRL` (`internal/pki/workspace.go`) listed revoked connectors and client devices by their current `cert_serial`. Every renewal overwrote it: client `updateClientDeviceCertOnRenewal` and connector `RenewCert`. Revoking a renewed connector or device therefore left its earlier, still-valid certificates (up to 7 days) off the CRL. Deleting a revoked connector (allowed for `revoked`) dropped it from the CRL entirely. The connector's device port and the relay check client and connector certificates against this CRL. For devices the policy also drops a revoked device's identity; for older connector certificates presented to a relay, the CRL was the main check.
  - Fixed (2026-10-20, controller only; no protobuf, Rust, relay or shield change):
    - **Migration `039_certificate_history.sql` `[schema: reset DB]`:** `connector_certificates` and `client_device_certificates`, one row per issued certificate (tenant, owner, unique serial, `not_after`, `revoked_at`), modelled on `relay_certificates`. No foreign key to the owning row, so history outlives a deleted connector or device; only deleting the workspace removes it. Indexes: unique serial, owner, and a partial `(tenant_id, not_after) WHERE revoked_at IS NOT NULL` for CRL generation.
    - **Atomic history through triggers** on `connectors` and `client_devices`, in the same statement (so the same transaction) as the write they record:
      - a new or changed `cert_serial` records the certificate. This covers connector `Enroll` (inside the bug 8 transaction) and `RenewCert`, and client `EnrollDevice` and `RenewCert`. A refused or rolled-back write records nothing.
      - the first transition to revoked marks every certificate of that owner revoked. For connectors that is status `revoked` or `revoked_at` set, either marker counting; for devices, `revoked_at`. This covers `revokeConnector`, `revokeDevice`, the client `RevokeDevice` RPC (`revokeClientDevice`) and SCIM offboarding (`revokeUserDevices`).
      - a certificate recorded on an already revoked owner starts out revoked.

      Triggers rather than per-path code, so no current or future issuance or revocation path can skip the history.
    - **Race safety:** renewal and revocation update the same owner row, so they serialize on its row lock. A renewal that commits first is revoked with the rest; a revocation that commits first makes the renewal's guarded write refuse it, so no certificate is returned.
    - **CRL from history:** every unexpired (`not_after > NOW()`) revoked certificate of the workspace; expired ones drop off. Superseded certificates are not treated as revoked. A device serial that cannot be encoded now fails CRL generation instead of being listed as zero. Workspace CA, signing, CRL number and validity window are unchanged.
    - **Backfill:** the serial each row currently holds, with its revocation (`revoked_at`, or `updated_at` for legacy status-only revoked connectors).
  - Historical-data limitation: serials overwritten before migration 039 cannot be recovered, because no other source records them. They stay off the CRL until they expire, at most one certificate lifetime (7 days) after the last renewal before the migration. A row without `cert_not_after` is kept with `not_after = infinity`, so if revoked it stays listed rather than dropping off for an unknown expiry.
  - Verified: 18 tests on throwaway databases with real PKI and the real issuance and revocation handlers. 12 failed against the original code for the expected reason; the backfill test failed because the schema was absent; 4 are guards that already passed; 1 (refused enrollment) was added after the fix. Full suite twice, `-race` on the affected packages, and 21 mutations of the triggers, backfill and CRL queries, all caught after closing 6 test gaps.
  - Excluded: **relays** already keep per-certificate history (`relay_certificates`, migration 027) and list every unexpired serial of a revoked relay. **Shields** are revoked by identity (bug 3 tombstones enforced by connectors), not by serial, and have no CRL.
  - Follow-ups:
    - [ ] No pruning: expired history rows are kept (filtered out of the CRL), like `relay_certificates`.
    - [ ] Not validated live end to end (a renewed then revoked connector presenting its older certificate to a relay).
    - [ ] Every developer must recreate their local database after pulling (`[schema: reset DB]`).
- [x] **Bug 10 — Low-Med — controller: connector and shield renewal accepted a different key.** Stolen credentials could be moved to an attacker's key.
  - Root cause: `RenewConnectorCert` and `RenewShieldCert` checked the renewal CSR's self-signature, then signed whatever public key it carried. Renewal is authenticated with the current certificate, but nothing tied the new key to it. Whoever briefly held a connector's key could move the identity onto a key of their own and keep renewing it. A connector could silently re-key its shields: shield renewal is proxied by the connector, and the controller never sees the shield's own certificate.
  - Fixed (2026-10-20, controller only; no protobuf, Rust, client or relay change):
    - **Connector:** `RenewCert` renews only for the key of the mTLS peer certificate the SPIFFE interceptor verified for the connection (never a certificate from the request), comparing parsed keys (`Equal`), as relay renewal does. A missing peer certificate, an unreadable presented key or an invalid CSR fail closed. Runs after the bug 7 revocation checks, before signing.
    - **Shield, migration `040_shield_public_key_fingerprint.sql` `[schema: reset DB]`:** `shields.public_key_fingerprint`, SHA-256 of the parsed key's PKIX DER, lowercase hex. It has the same canonical form as the client (036) and relay pins, and a format `CHECK`. No backfill: shield keys were never stored.
    - **Shield enrollment** pins the fingerprint of the CSR key the certificate is issued for, inside the bug 8 enrollment transaction; a failed enrollment pins nothing.
    - **Shield renewal** requires the CSR key's fingerprint to equal the pin before signing, and again in the bug 7 conditional final write, so a pin change during renewal returns no certificate. A missing or malformed pin is refused with "re-enroll the shield" (no trust on first use). Renewal never changes the pin; changing a key means re-enrolling.
  - Verified: 14 new tests on throwaway databases with real enrollment, renewal and signing. 4 failed against the original code for the expected reason (connector: different key, no presented certificate, unreadable presented key; shield: different key). 5 failed because the pin column did not exist yet; 2 are guards that already passed (same-key renewal); 3 were added after the fix (format constraint; corrupted CSR signature, connector and shield). Existing tests now renew as real connectors and shields do (same key, presented certificate). Full suite twice, `-race` on the connector and shield packages. 13 mutations: 11 caught. The 2 survivors (C5, S6) remove the handlers' own CSR signature check. They are equivalent: the signers (`RenewConnectorCert`, `RenewShieldCert`) verify the signature before creating any certificate. Tests with a corrupted signature (`40b0e14`) still refuse the renewal and record nothing with those checks removed; only the error code changes, from InvalidArgument to Internal.
  - Excluded: **clients** already pin a key fingerprint at enrollment (036) and renew only for it. **Relays** already renew only for the presented certificate's key.
  - Limitations:
    - [ ] Shields enrolled before migration 040 have no pin and must re-enroll before they can renew. **The database reset is a deployment prerequisite:** migrations run only when the Postgres volume is created, so after the reset there are no unpinned shields. Without the reset, 040 is never applied and shield enrollment and renewal fail loudly (missing column). If 040 were applied by hand to an existing database, existing shields' renewals are refused with "re-enroll the shield". The connector logs that reason (`shield cert renewal proxy failed`); the shield only sees "unavailable", and loses access when its certificate expires (≤ 7 days) unless re-enrolled.
    - [ ] There is no key-rotation operation: a connector or shield that needs a new key must re-enroll.
    - [ ] Not validated live end to end (a real connector and shield renewing, and a renewal with a different key refused).

## Certificate-system closure checklist (2026-10-20)

Sources: this tracker, `git log` on `fixed-pendings`, and the Sprint 21 test report (`.zecurity-obs/Sprint21/Test-Status-Report-2026-10-17.md`). "Verified locally" means the regression tests were written first, shown failing, and the merged fix passed them plus the Go suite and, where noted, mutation testing. **No bug has been verified in live integration since its fix.**

| Bug | Fix commits | PR | Regression evidence | Live | Remaining limitations | Status |
|---|---|---|---|---|---|---|
| 1 — first-time trust hijack (connector, shield, relay, client) | `5387d43` tests; `84195e4`, `564561a`, `30cd9cd` agents; `0bd6280`, `9f95a9f`, `7e3d544` client | #120, #121 merged | attack-reproduction tests per agent and client (Rust suites, CI) | not recorded | unsigned client releases; no root rollover; production PKI auto-reset must be locked down (see below) | verified locally |
| 2 — relay CRL picked from a forged issuer copy | `b7bf4a6` test, `d80dedb` fix | #118 merged | relay demonstration test (Rust, CI) | not recorded | — | verified locally |
| 3 — revoking a shield didn't cut it off | `47e381e` tests; `86a436d`, `b2b00b5` fixes; `77196f3`, `aa899d3` docs | #123 merged | Go and connector Rust tests; resource fixture 30 pass / 4 pre-existing fail | **pending** (F33 in the test report) | disconnected-connector window; no shield CRL | verified locally |
| 4/5 — inactive users' devices: issuance and access | `6c49e17` tests, `ca2be59` fix, `a303782` docs | #124 merged | Go tests; 28 mutations, 2 survivors (layered checks) | not recorded (session teardown gap noted in PR) | no admin lifecycle API; membership not authoritative for SCIM/JIT users | verified locally |
| 6 — Workspace/Relay CRLs swappable | `7045bb9` tests, `4a7fae8` fix, `cf4ff91` docs | #126 merged | connector and client Rust tests; 41 mutation runs, 4 equivalent survivors | not validated | `crl_issuers` duplicated in two crates; restart needed on Workspace CA rotation | verified locally |
| 7 — revoked connector renews shield certificates | `cccd1e1` tests, `473d953` fix, `f7c3e3b` docs | #125 merged | Go tests; 31 mutations, all caught | not validated | stream interceptor does not check standing; shields renewed before revocation stay valid ≤ 7 days | verified locally |
| 8 — re-issued enrollment token not cancelled | `e2eb952` tests, `a4a14c9` fix, `ee35d77` tests, `c803ea0` docs | #128 merged | 23 Go tests (16 failed pre-fix); 34 + 6 mutations, 6 unreachable under the lock | not validated | Valkey/Postgres not atomic (no usable stale token); ambiguous commit error needs re-creation | verified locally |
| 9 — workspace CRL covered only the newest certificate | `34f088f` tests, `52c16fa` fix, `2d14935` tests, `6cd032e` docs | #129 merged | 18 Go tests (12 failed pre-fix); 21 mutations, all caught | not validated | serials overwritten before migration 039 unrecoverable (≤ 7-day window); no pruning | verified locally |
| 10 — renewal accepted a different key | `cb95534` tests, `12614ae` fix, `c740fae` and `40b0e14` tests, tracker update | **not merged** | 14 Go tests (4 failed pre-fix, 5 schema-absent); 13 mutations, 2 shown equivalent | not validated | pre-040 shields must re-enroll; no key rotation | **pending review** |

**The certificate system is not closed.** Still open:
- **F34 (High, Sprint 20 live finding): a revoked connector keeps serving tunnels.** Connector-side enforcement is at Phase A (#127) and does not fail closed; Phase B1 is pending.
- **Shield revocation is not enforced by the shield** (follow-up found during bug 6).
- **F33:** live acceptance of the bug 3 shield revocation is pending.
- **F32 (Low):** a revoked relay keeps reconnecting its heartbeat.
- Live end-to-end validation of bugs 1–10.
- The open items under "Found during the bug 1 work" and "Risks" below.

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
