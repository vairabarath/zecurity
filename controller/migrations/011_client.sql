-- Sprint 7: Client application tables

-- Invitations sent by admins to new users
CREATE TABLE invitations (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT        NOT NULL,
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    invited_by   UUID        NOT NULL REFERENCES users(id),
    token        TEXT        NOT NULL UNIQUE,
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','accepted','expired')),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 days',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON invitations(token);
CREATE INDEX ON invitations(email, workspace_id);

-- Client devices enrolled by end users via CLI
CREATE TABLE client_devices (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,
    os             TEXT        NOT NULL,
    cert_serial    TEXT,
    cert_not_after TIMESTAMPTZ,
    spiffe_id      TEXT,
    last_seen_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON client_devices(user_id, workspace_id);
