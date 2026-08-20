-- 034_backfill_legacy_external_identities.sql
--
-- Identity federation (031) introduced external_identities as the canonical
-- login lookup. Existing deployments may already contain users created by the
-- legacy (tenant_id, provider_sub) identity model, so link each such user to
-- its matching active, managed platform connection before the resolver relies
-- on external_identities exclusively.
--
-- This is intentionally additive and idempotent. It only maps providers for
-- which the platform owns a managed connection; workspace-owned IdPs never
-- infer legacy links from users.provider.

INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
SELECT
    u.tenant_id,
    u.id,
    c.id,
    c.issuer,
    u.provider_sub
FROM users AS u
JOIN identity_connections AS c
  ON c.tenant_id IS NULL
 AND c.managed = TRUE
 AND c.status = 'active'
 AND c.provider = u.provider
ON CONFLICT (tenant_id, connection_id, subject) DO NOTHING;
