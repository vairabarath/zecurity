# Security finding: purged slug / trust domain can be reused (2026-11-24)

**Status:** open, not in PR A (F23 findings). For Barath to decide. Found while answering the R1-1 design questions (Q6).

## Facts (verified in code, `main` `2ae68355`)
- The tenant purge hard-deletes the workspace row: `controller/migrations/066_tenant_deletion.sql:211`. It writes `workspace_tombstones`, but nothing outside `internal/tenantlifecycle/purge.go` reads that table.
- `slug` and `trust_domain` are UNIQUE only among *existing* rows. The trust domain is derived from the slug: `appmeta.WorkspaceTrustDomain`, which gives `ws-<slug>.zecurity.in`.
- Both creation paths can claim a purged slug:
  - self-signup (`bootstrap/bootstrap.go:181`, slug = slugified workspace name);
  - the super-admin `POST /provider/tenants`.

  Neither checks the tombstones; the only check is the UNIQUE constraint (`tenant_create.go:383`).

## Risks (from the review; R1 re-verified)
- **R1: shield certificates of the purged tenant are accepted by the new tenant's connector for up to 7 days.**
  - The certificate chain passes TLS:
    - `connector/src/enrollment.rs:231` writes `workspace_ca.crt` = workspace CA + platform intermediate;
    - `connector/src/tls/server_cfg.rs:14-25` adds every certificate in it as a root.

    So any leaf chaining through any workspace CA passes TLS on :9091 and :9092.
  - The identity check doesn't stop it: the shield identity check is only the prefix `spiffe://<own trust domain>/shield/` (`agent_server.rs:648`), plus the connector's in-memory revoked set.
  - Nothing revokes the old certificates: `shield_revocations` cascades away with the workspace (`038:15`).
  - **Precondition:** an old shield's private key.
  - **Effect:** a control stream to the new tenant's connector (health and discovery reports forwarded with the old shield id). No instructions reach it. Not yet checked: that `UpdateShieldHealth` and `ReplaceDiscoveredServices` are no-ops for an unknown id.
- **R2 (latent): the relay pairs client and connector by trust domain only** (`relay/src/session.rs:280-281`, `spiffe.rs:131`), not by the CA's `tenant:` id. The connector's inner mTLS (own workspace CA only, `relay_handler.rs:349-355`) still stops an old client, but one defence layer is missing.
- **R3 (fragile): the device tunnel has the same intermediate anchor.** Separation rests on exact client SPIFFE IDs in the ACL; device ids are random UUIDs, so it's safe today.
- **Metrics (R1-1 Q6):** a reused trust domain could inherit a relay counter. This is covered in PR A by the rule 4 extension (per-domain tenant id).

## Checked safe
- Controller mTLS against the *current* workspace CA: `workspace_ca_keys` cascades, and the new tenant gets a new key.
- Connector standing: trust domain + random UUID.
- Relay CRL and residency: keyed by the CA's `tenant:` id.
- ACL snapshot cache: keyed by workspace id.

## Options
1. **Reject tombstoned slugs in both creation paths, inside the creation transaction.**
   - Minimum window: the longest certificate lifetime after the purge (7 days at defaults).
   - Review recommendation: **permanent**, optionally with a super-admin override plus reason after the window.
2. **Fix the root causes too:**
   - (a) the connector's shield/device verifier trusts only the workspace CA (as `relay_handler.rs` already does);
   - (b) the relay requires the client's `tenant:` id to equal the connector's;
   - (c) optionally, a workspace CRL check for shields at the connector.
3. **Alternative (architectural, needs a Decision Record entry):** a random suffix in the trust domain (`ws-<slug>-<rand>`). It removes the class of problem, but changes the identity format.
