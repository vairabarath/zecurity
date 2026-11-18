-- 065_tenant_quotas.sql — Sprint 23 S3: per-tenant quota overrides (F12;
-- Decision Record D-47, on top of the plans of D-43 / migration 062).
--
-- A tenant's limit for a quota key is its plan's value unless an override row
-- exists for (workspace, key). An override's limit_value is a non-negative
-- integer or NULL, which means explicitly unlimited; no row means "use the
-- plan value". (The column is limit_value because LIMIT is a reserved word.)
--
-- Overrides are set by provider super-admins (tenant.quota.set), always with
-- a reason, and audited with before/after in provider_audit_logs (F17). The
-- only reader that turns plan + overrides into limits is internal/quota.
-- Lowering a limit never deletes anything (D-47): the tenant reads "over"
-- and can't create more until it is under its limit again.
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

CREATE TABLE workspace_quota_overrides (
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    key          TEXT        NOT NULL
                             CHECK (key IN ('users', 'resources', 'remote_networks',
                                            'connectors', 'shields', 'client_devices')),
    limit_value  BIGINT      NULL CHECK (limit_value IS NULL OR limit_value >= 0),
    reason       TEXT        NOT NULL
                             CHECK (reason = btrim(reason) AND char_length(reason) BETWEEN 1 AND 500),
    set_by       TEXT        NOT NULL,
    set_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, key)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_quota_overrides TO ztna_app;
