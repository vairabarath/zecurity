---
type: decision
status: proposed
date: 2026-06-13
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[CodeStudy/04-Connector-Enrollment-Flow]]"
tags:
  - adr
  - controller
  - connector
  - enrollment
  - lifecycle
  - state-machine
  - security
  - critical
---

# ADR-011 — Connector Enrollment Lifecycle Hardening

## Context

The Stage 4+5 connector-flow audit (independent adversarial pass) surfaced
two real findings in the connector enrollment lifecycle. The first is
**critical** — it allows a persistent privileged backdoor that is invisible
to the admin UI. The second is **medium** — token regeneration does not
revoke the previously-leaked token.

Both have the same root cause: the enrollment lifecycle does not consistently
re-validate ambient state. Closes audit findings **STAGE4/5-F3** and
**STAGE4/5-F4**.

---

### Finding 1 (CRITICAL) — STAGE4/5-F3: Orphan connector via soft-deleted remote network

#### Attack sequence

1. Admin calls `generateConnectorToken(remoteNetworkID, name)` — connector
   row created in `status='pending'`, JWT issued, install command delivered.
2. Admin (or another admin) calls `deleteRemoteNetwork(remoteNetworkID)`.
   - Soft-delete sets `status='deleted'`.
   - The delete guard at
     [connector.resolvers.go DeleteRemoteNetwork](controller/graph/resolvers/connector.resolvers.go)
     blocks only on `active`/`disconnected` connectors. `pending` is
     explicitly allowed:
     ```sql
     AND NOT EXISTS (
         SELECT 1 FROM connectors
          WHERE remote_network_id = $1
            AND status NOT IN ('pending', 'revoked')
     )
     ```
3. Installer (anyone holding the JWT) runs the install command on a server.
4. `Enroll` handler ([enrollment.go:98+](controller/internal/connector/enrollment.go#L98))
   validates:
   - JWT signature ✅
   - JTI not burned ✅
   - Connector row exists and is `pending` ✅
   - Workspace `status='active'` ✅
   - **Does NOT check `remote_networks.status`** ← the gap.
5. Workspace CA signs the cert. Connector transitions to `status='active'`.

#### Why this is critical

After enrollment, the controller's downstream code paths do NOT check the
remote network's status when handling the connector. Verified by grep:

```
grep "remote_networks.status" controller/internal/connector/ -r → empty
grep "remote_network" controller/internal/policy/compiler.go → empty
```

Heartbeat handler, control stream, policy compiler, RenewCert — none of
them join to `remote_networks` to validate. They all key on
`connector_id` + SPIFFE cert. **The orphan connector is fully functional.**

It can:
- Heartbeat to controller indefinitely
- Receive workspace-wide ACL snapshots (sensitive network topology data —
  resource IPs, ports, allowed SPIFFE IDs)
- Host shields on `:9091`
- Mediate device tunnel traffic through `:9092`
- Renew its certificate every 7 days indefinitely via `RenewCert` mTLS

Worse: the admin UI's `remoteNetworks` query filters `status='active'`
([resolvers line 228](controller/graph/resolvers/connector.resolvers.go#L228)),
so connectors of deleted networks are **never returned by the admin's
own UI**. The admin has no UI affordance to discover the orphan exists.
Discovery requires raw DB access:

```sql
SELECT * FROM connectors
 WHERE remote_network_id IN (
     SELECT id FROM remote_networks WHERE status = 'deleted'
 );
```

Realistic exploit window: admin issues a token, then changes their mind
and deletes the network. Between issuance and the next `revokeConnector`
sweep (which doesn't exist), an attacker holding the JWT (via
STAGE1-F4 Apollo cache, copy/paste mishap, or insider access) installs
the connector and obtains a persistent, invisible foothold.

**Severity: 🔴 Critical — persistent privileged access, no UI visibility.**

**CWE-367 (TOCTOU) + state machine integrity violation.**

---

### Finding 2 (MEDIUM) — STAGE4/5-F4: Token regeneration does not revoke the prior JTI

#### What happens

When admin re-visits a pending connector's detail page, the SPA calls
`POST /api/connectors/{id}/token`
([token_handler.go:RegenerateTokenHandler](controller/internal/connector/token_handler.go)).
The handler:

1. Generates a new JWT + JTI.
2. Stores the new JTI in Redis via `StoreEnrollmentJTI`.
3. UPDATEs `connectors.enrollment_token_jti` to the new JTI.
4. **Does NOT delete the OLD JTI from Redis.**

Both old and new tokens remain valid for up to 24 hours
(`CONNECTOR_ENROLLMENT_TOKEN_TTL` default). Single-use enforcement is
done by `BurnEnrollmentJTI` which `GETDEL`s the Redis key — whichever
token is presented first wins; the other waits for natural TTL expiry.

#### Why this matters

Admins regenerate tokens **because the previous token was exposed** (copy
to wrong Slack channel, screen share, email forward, etc.). The whole
point of regeneration is to invalidate the leaked token. The current
implementation does not.

A leaked token remains usable for up to 24 hours after regeneration.

**Severity: 🟡 Medium — defeats the regeneration security mechanism.**

**CWE-613 — Insufficient Session Expiration.**

---

## Decision

Apply BOTH fixes. They are small (~15-20 LOC each), low risk, and close
real findings.

### Fix A (primary, for F3) — Enroll handler validates remote network status

In `controller/internal/connector/enrollment.go`, after the existing
connector-row lookup and workspace-status check, add:

```go
var rnStatus string
err = h.Pool.QueryRow(ctx,
    `SELECT rn.status
       FROM remote_networks rn
       JOIN connectors c ON c.remote_network_id = rn.id
      WHERE c.id = $1`,
    connectorID,
).Scan(&rnStatus)
if err != nil {
    return nil, status.Errorf(codes.Internal,
        "load remote network for connector %s: %v", connectorID, err)
}
if rnStatus != "active" {
    return nil, status.Errorf(codes.FailedPrecondition,
        "remote network for connector %s is %s, expected active",
        connectorID, rnStatus)
}
```

Closes the race window. Even if the JWT is held by an attacker, `Enroll`
will reject it once the remote network is no longer active.

### Fix B (defense in depth, for F3) — Cascade-revoke pending connectors AND shields on RN delete; extend guard to shields

A remote network has **two** child types: connectors and shields. The
current guard only inspects `connectors`, so an RN containing active
shields can still be soft-deleted (e.g. when all connectors are
revoked but shields are still active). That produces an orphan
shield in a `'deleted'` RN — invisible in the admin UI, still
enforcing its last-known ACL on the customer machine, until its cert
expires.

Symmetric treatment: the guard must reject the delete if *either*
table has a non-terminal child, and the cascade must clear pending
rows in *both* tables.

#### B.1 — Extend the guard to also block on non-terminal shields

In `controller/graph/resolvers/connector.resolvers.go` inside
`DeleteRemoteNetwork`, add a second `NOT EXISTS` clause:

```sql
UPDATE remote_networks
    SET status = 'deleted', updated_at = NOW()
  WHERE id = $1
    AND tenant_id = $2
    AND status = 'active'
    AND NOT EXISTS (
        SELECT 1 FROM connectors
         WHERE remote_network_id = $1
           AND tenant_id = $2
           AND status NOT IN ('pending', 'revoked')
    )
    AND NOT EXISTS (
        SELECT 1 FROM shields
         WHERE remote_network_id = $1
           AND tenant_id = $2
           AND status NOT IN ('pending', 'revoked')
    )
 RETURNING id
```

Same predicate (`status NOT IN ('pending', 'revoked')`) on both
tables — pending and revoked are safe-to-discard, anything else
blocks the delete.

#### B.2 — Cascade-revoke pending rows in BOTH tables

Before the soft-delete UPDATE, revoke pending connectors AND pending
shields:

```go
// Cascade-revoke pending connectors so their JWTs can no longer enroll.
// Active/disconnected connectors block the delete entirely (extended guard above);
// revoked connectors are already terminal, no change.
_, err := r.TenantDB.Exec(ctx,
    `UPDATE connectors
        SET status = 'revoked', updated_at = NOW()
      WHERE remote_network_id = $1
        AND tenant_id = $2
        AND status = 'pending'`,
    id, tc.TenantID,
)
if err != nil {
    return false, fmt.Errorf("delete remote network: revoke pending connectors: %w", err)
}

// Same for pending shields — their enrollment JWTs would otherwise stay
// valid for 24h and could enroll into a deleted RN.
_, err = r.TenantDB.Exec(ctx,
    `UPDATE shields
        SET status = 'revoked', updated_at = NOW()
      WHERE remote_network_id = $1
        AND tenant_id = $2
        AND status = 'pending'`,
    id, tc.TenantID,
)
if err != nil {
    return false, fmt.Errorf("delete remote network: revoke pending shields: %w", err)
}
```

Belt-and-suspenders: even if Fix A is somehow bypassed, the
connector/shield row is no longer in `'pending'` so `Enroll`
refuses.

> **Note on audit scope**: F3 as documented in this ADR is the
> connector-side symptom (orphan invisible connector). The shield-side
> symptom is structurally identical — orphan invisible shield with
> stale ACL enforcement — and the fix lives in the same SQL guard, so
> it's bundled here for implementation hygiene rather than in a
> separate ADR. A dedicated shield-flow audit may surface additional
> shield-only concerns; those will go in a separate ADR.

### Fix C (for F4) — Token regeneration deletes the previous JTI

In `controller/internal/connector/token_handler.go::RegenerateTokenHandler`,
before storing the new JTI, read the existing one from the connector row
and delete it from Redis:

```go
// Best-effort revoke of the previous JTI. If Redis is down, the old token
// will expire on its natural TTL; we don't fail regeneration on this.
var oldJTI *string
_ = pool.QueryRow(ctx,
    `SELECT enrollment_token_jti FROM connectors
      WHERE id = $1 AND tenant_id = $2`,
    connectorID, tc.TenantID,
).Scan(&oldJTI)
if oldJTI != nil && *oldJTI != "" {
    if err := rdb.Del(ctx, "enrollment:jti:"+*oldJTI).Err(); err != nil {
        log.Printf("warn: failed to revoke previous enrollment JTI %s: %v",
            *oldJTI, err)
    }
}
```

Note: the same pattern could be applied to `generateConnectorToken`
GraphQL resolver — but that resolver always creates a NEW connector row
(INSERT, not UPDATE), so there's no "old JTI" to revoke. F4 fix is
scoped to the REST regenerate handler only.

### Optional follow-up — Admin UI / API to list orphan connectors

Existing orphans (created before this fix shipped) are still invisible
to the admin UI. Provide either:
- A new GraphQL query `orphanConnectors: [Connector!]!` (admin-only) that
  returns connectors whose `remote_network_id` points to a non-`active`
  network, OR
- A one-time cleanup script run during deployment.

**Deferred**: pre-fix orphans only exist if (a) an admin already deleted
a remote network with a pending connector, AND (b) an enrollment occurred
during the gap. In a beta product, this is extremely unlikely. The cleanup
script approach is cheaper than UI work.

---

## Alternatives Considered

### Alt A — Hard-delete (DELETE) pending connectors instead of revoke (rejected)

`DeleteRemoteNetwork`'s cascade could `DELETE FROM connectors WHERE …
status = 'pending'` instead of marking them revoked.

**Rejected because**:
- Loses the audit trail of "this connector was created and then removed
  via RN delete"
- Inconsistent with the codebase's pattern of using `revoked` status as
  a terminal soft-delete (see `RevokeConnector` for the same pattern)
- A scheduled cleanup job can hard-delete revoked-and-old rows later if
  storage matters

### Alt B — Hard-delete the remote_networks row (rejected)

Use a real `DELETE FROM remote_networks WHERE id = $1` and let the FK's
`ON DELETE CASCADE` clean up connectors.

**Rejected because**:
- The codebase chose soft-delete intentionally for audit / referential
  history. Reverting that decision is out of scope for this finding.
- Other tables likely depend on the soft-delete invariant.

### Alt C — Block remote_network delete if ANY connectors exist, including pending (rejected)

Tighten the guard to `WHERE status != 'revoked'`.

**Rejected because**:
- A pending connector is by definition not-yet-installed. Forcing the
  admin to revoke it explicitly before deleting the RN is friction.
- The Fix A + Fix B combination handles the safety concern without
  blocking legitimate workflows.

### Alt D — Defer Fix C; let old JTI expire on TTL (rejected)

Skip the F4 fix entirely. After 24 hours the old JTI is gone anyway.

**Rejected because**:
- Defeats the whole purpose of regeneration (which is to revoke a
  leaked token before its natural expiry).
- The fix is ~10 LOC. Not worth deferring.

---

## Consequences

### Wins
- Closes a critical persistent-backdoor vulnerability (F3)
- Closes a token regeneration weakness (F4)
- No admin-UX changes — all fixes are server-side
- Fix A + B are independent layers (defense in depth)
- Fix C aligns regeneration semantics with admin expectations

### Costs
- ~35 LOC across 3 files (enrollment.go, connector.resolvers.go,
  token_handler.go)
- One additional DB query per `Enroll` call (the RN status check)
  — negligible at scale
- One additional Redis `DEL` per token regeneration — negligible

### Risks
- **Fix A risk**: if the JOIN query fails for a transient DB reason, the
  Enroll call fails with `codes.Internal`. Admin has to retry. Acceptable —
  fail-closed.
- **Fix B risk**: cascade-revoking pending connectors at delete time may
  surprise admins who didn't realize the pending connectors existed. Add
  a UI confirmation step that mentions "N pending connectors will be
  revoked." Optional polish, not blocking the security fix.
- **Fix C risk**: if the Redis DEL fails (Redis temporarily down), the
  old JTI lingers for its natural TTL. Same as today. Logged, not fatal.

### Pre-fix orphan cleanup

After deploying, run a one-time SQL to clean up any existing orphans:

```sql
-- Revoke connectors whose remote_network is no longer active.
UPDATE connectors c
   SET status = 'revoked', updated_at = NOW()
  FROM remote_networks rn
 WHERE c.remote_network_id = rn.id
   AND rn.status != 'active'
   AND c.status IN ('active', 'pending', 'disconnected');
```

Run as part of the deploy that includes this fix. Logs the rows affected.

---

## Plan

### Phase 1 — Implementation
1. Add Fix A to `enrollment.go`. Verify with `go vet` + integration test
   (existing test should still pass; add a test for the new RN-deleted
   case).
2. Add Fix B to `connector.resolvers.go::DeleteRemoteNetwork`. Verify
   with integration test that simulates the race.
3. Add Fix C to `token_handler.go::RegenerateTokenHandler`. Verify with
   integration test that the old JTI is unusable after regeneration.

### Phase 2 — Pre-fix orphan cleanup
4. Run the one-time SQL above as part of the deploy. Log the number of
   rows affected for ops awareness.

### Phase 3 — Audit doc update
5. Mark STAGE4/5-F3 and STAGE4/5-F4 as ✅ Fixed in
   [[CodeStudy/07-Connector-Audit]] (when implementation lands).

### Phase 4 — Optional polish (deferred)
6. Add admin UI confirmation dialog mentioning pending-connector cascade.
7. Add `orphanConnectors` GraphQL query if Phase 2 cleanup script proves
   insufficient over time.

---

## Verification

After applying:

1. Create a connector (`generateConnectorToken`) → install command
   delivered.
2. Delete the remote network (`deleteRemoteNetwork`) → expect success;
   connector should be `revoked` (verify via DB).
3. Attempt to enroll the connector using the original JWT → expect
   `FailedPrecondition: remote network for connector X is deleted` (Fix A)
   or `connector status is revoked, expected pending` (Fix B catching it
   first).
4. Generate a new token via REST `POST /api/connectors/{id}/token` →
   verify old JTI is gone from Redis (`redis-cli GET enrollment:jti:OLD_JTI`
   returns nil), new JTI is present.

---

## Notes

- Closes audit findings **STAGE4/5-F3** (Critical) and **STAGE4/5-F4**
  (Medium) in [[CodeStudy/07-Connector-Audit]].
- This is the **highest-severity finding** surfaced in the entire connector
  audit so far. Should be prioritized ahead of ADR-009 / ADR-010
  implementations.
- Fix A and Fix B together provide layered defense; they are independent
  and both should ship.
- Fix C is conceptually separate (different finding, different file) but
  can ship in the same PR for review efficiency.
- No frontend changes required.
- No schema changes required.

## Open questions for team discussion

1. **Cascade behavior on delete**: should pending connectors be revoked
   (current ADR proposal) or hard-deleted? Recommend revoke for audit
   trail; flag if team disagrees.
2. **Confirmation dialog**: when admin clicks delete on a network with
   pending connectors, do we want a UI prompt? "This network has N
   pending connectors. Deleting will revoke them. Continue?"
3. **Pre-fix orphan cleanup**: should the cleanup SQL be wrapped in a
   migration (auto-run) or a separate ops command (deliberate run)?
4. **`disconnected` connector handling**: today, `disconnected` blocks
   the RN delete (correct — could come back). Should we also handle the
   case where a disconnected connector reconnects after its RN was
   somehow soft-deleted out from under it? (Edge case, deferred.)
