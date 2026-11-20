-- 070_alert_delivery_queue.sql — Sprint 24 A2 (F26 delivery, Decision
-- Record D-52, refined 2026-11-20).
--
-- D-52 said retries go "through the outbox", but outbox_events.workspace_id
-- is NOT NULL (FK to workspaces), so platform alerts (no tenant) can't be
-- queued there. Decided 2026-11-20 (Sathiya, recorded as a D-52
-- refinement): alerts get their own durable queue, reusing the outbox's
-- retry/backoff policy; the outbox itself is untouched.
--
-- One row per (event, channel) still to deliver. It is written in the same
-- transaction as the alert event (alerting.insertEvent), so a committed
-- event is never lost to a crash before delivery. The delivery worker
-- leases due rows, sends, appends an attempt to alert_deliveries (the
-- append-only history, 069) and then deletes the row (delivered or finally
-- failed) or schedules the next attempt.
--
-- Rows cascade with their event (and so with a purged tenant, D-46) and
-- with a deleted channel.

CREATE TABLE alert_delivery_queue (
    event_id        UUID        NOT NULL REFERENCES alert_events(id) ON DELETE CASCADE,
    channel_id      UUID        NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    attempts        INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_until     TIMESTAMPTZ NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_id, channel_id)
);
CREATE INDEX alert_delivery_queue_due_idx ON alert_delivery_queue (next_attempt_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON alert_delivery_queue TO ztna_app;
