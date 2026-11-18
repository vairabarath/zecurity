-- 064_tenant_suspension.sql — provider tenant suspension (Sprint 23 S1,
-- Decision Record D-45, amends D-07; D-08 and D-34 preserved).
--
-- A suspended tenant is a workspace with suspended_at set. workspaces.status
-- stays 'active' on purpose (D-34): the protected status='active' predicates
-- (SPIFFE validator, disconnect watchers, enrollment, WorkspaceGuard) are not
-- touched, so a suspended tenant's connectors and shields keep their
-- certificates and connections (D-08) and get a deny-all ACL instead.
--
-- The three columns are set and cleared together: suspended_at is the
-- suspension instant, suspended_by the provider operator's email (the
-- provider audit row holds the operator id), suspension_reason the mandatory
-- reason (D-17 extended). The only reader that decides "may this tenant act"
-- is internal/tenantstate.
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

ALTER TABLE workspaces
    ADD COLUMN suspended_at      TIMESTAMPTZ NULL,
    ADD COLUMN suspended_by      TEXT        NULL,
    ADD COLUMN suspension_reason TEXT        NULL,
    ADD CONSTRAINT workspaces_suspension_fields CHECK (
        (suspended_at IS NULL AND suspended_by IS NULL AND suspension_reason IS NULL)
        OR (suspended_at IS NOT NULL AND suspended_by IS NOT NULL AND suspension_reason IS NOT NULL)
    );
