-- Sprint 8.5: Workspace membership table
--
-- Separates workspace membership + role from the users identity table.
-- A row is created at invite time (user_id NULL, status 'invited') and
-- updated when the user accepts and authenticates (user_id set, status 'active').
-- This gives admins visibility into pending invites before acceptance,
-- matching the Twingate PENDING → ACTIVE lifecycle pattern.
--
-- The first workspace admin is also inserted here during bootstrap so
-- workspace_members is the single source of truth for all members.

CREATE TABLE workspace_members (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- NULL until the invited user authenticates and accepts the invitation.
    user_id      UUID        REFERENCES users(id) ON DELETE CASCADE,
    email        TEXT        NOT NULL,
    role         TEXT        NOT NULL DEFAULT 'member'
                             CHECK (role IN ('admin', 'member', 'viewer')),
    status       TEXT        NOT NULL DEFAULT 'invited'
                             CHECK (status IN ('invited', 'active', 'suspended')),
    invited_by   UUID        REFERENCES users(id),
    invited_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Set when the user accepts the invitation and their account becomes active.
    joined_at    TIMESTAMPTZ,
    UNIQUE (workspace_id, email)
);

-- Membership lookups by workspace (member list page)
CREATE INDEX ON workspace_members(workspace_id, status);
-- Bootstrap and invite-accept look up by email to find pending invites
CREATE INDEX ON workspace_members(email, status);
-- user_id lookups for role resolution
CREATE INDEX ON workspace_members(user_id);
