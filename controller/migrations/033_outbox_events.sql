CREATE TABLE outbox_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type      TEXT        NOT NULL,
    workspace_id    UUID        NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    user_id         UUID        REFERENCES users(id) ON DELETE SET NULL,
    correlation_id  UUID        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    retry_count     INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ DEFAULT NOW(),
    lease_id        UUID,
    claimed_at      TIMESTAMPTZ,
    last_error      TEXT
);

CREATE INDEX idx_outbox_claim
    ON outbox_events (status, next_attempt_at, retry_count)
    WHERE status IN ('pending', 'failed');

CREATE INDEX idx_outbox_workspace_event
    ON outbox_events (workspace_id, event_type);

CREATE INDEX idx_outbox_processing
    ON outbox_events (status, claimed_at)
    WHERE status = 'processing';
