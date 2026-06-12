-- 016_audit_logs.sql
--
-- Durable audit trail for privileged / break-glass actions. A break-glass
-- operation deliberately bypasses a safety invariant, so it must leave an
-- immutable, queryable record of WHO did WHAT to WHICH target and WHEN.
--
-- First consumer: forceDeleteResource (ADR-004 Phase 4) — the escape hatch that
-- hard-deletes a resource stuck because its shield is permanently gone and will
-- never ack removal, bypassing the confirmation-gated tombstone path.
--
-- The table is append-only by convention (no UPDATE/DELETE in app code).

CREATE TABLE audit_logs (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id UUID,                       -- nullable: system/automated actions have no user
    actor_email   TEXT        NOT NULL,
    action        TEXT        NOT NULL,        -- dotted verb, e.g. 'resource.force_delete'
    target_type   TEXT        NOT NULL,        -- e.g. 'resource'
    target_id     TEXT        NOT NULL,
    details       JSONB,                       -- arbitrary context snapshot at action time
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Most reads are "show me this tenant's recent actions".
CREATE INDEX idx_audit_logs_tenant_created ON audit_logs (tenant_id, created_at DESC);
-- "show me everything that happened to this specific target".
CREATE INDEX idx_audit_logs_target ON audit_logs (tenant_id, target_type, target_id);
