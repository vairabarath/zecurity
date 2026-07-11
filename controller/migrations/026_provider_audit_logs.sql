-- Sprint 12 M1 Phase 1 — Provider action audit trail (PENDING-07a).
--
-- Provider actions (create relay, issue token, manage provider users, revoke)
-- are platform-level and have NO tenant, so they cannot live in the tenant
-- audit_logs table, whose tenant_id is NOT NULL REFERENCES workspaces(id).
-- This is a separate, provider-scoped, append-only trail: who did what, to
-- which target, when — with a JSONB context snapshot.
--
-- Append-only by convention: app code only INSERTs. No UPDATE/DELETE.

CREATE TABLE provider_audit_logs (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_user_id  UUID         REFERENCES provider_users(id),  -- nullable: system/automated actions
    provider_email    TEXT         NOT NULL,      -- denormalized: readable even if the user is renamed/disabled
    action            TEXT         NOT NULL,      -- dotted verb, e.g. 'relay.create', 'provider_user.create'
    target_type       TEXT         NOT NULL,      -- e.g. 'relay', 'provider_user'
    target_id         TEXT         NOT NULL,      -- id of the acted-on entity (TEXT: not all targets are UUIDs)
    details           JSONB,                      -- context snapshot: name/TTL/SANs, granted role, etc.
    ip_address        TEXT,                       -- request source IP for forensics
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Most reads are "show recent provider actions" and "everything to this target".
CREATE INDEX idx_provider_audit_created ON provider_audit_logs (created_at DESC);
CREATE INDEX idx_provider_audit_target  ON provider_audit_logs (target_type, target_id);