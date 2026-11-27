-- 077_provider_alert_kinds.sql — Sprint 27 G1 (F20; Decision Record D-75,
-- D-76 Q6, extends D-50; amendment 2026-11-23, provider access).
--
-- Two new built-in PLATFORM alert kinds, in one migration (path.md: G1 owns
-- this file; K1 adds no second kinds migration):
--   * provider.break_glass_used (G1, D-75): a super-admin was recovered with
--     `zecurity-controller provider recover-admin` on the controller host.
--     Fires once per break-glass event, critical, and stays firing until a
--     super-admin acknowledges the event (with a note) in the console.
--   * api_key.expiring (K1, D-76 Q6): an active provider API key expires
--     within days_before (14) days. The signal itself lands with K1.
-- 069_alerts / 072_usage_alert_kind are never edited: the kinds CHECK is
-- replaced here, as 072 did.
--
-- It also holds the break-glass event table (G1 has no other migration):
-- one row per recover-admin run, written in the same transaction as the
-- recovery. It never holds the temporary password or any secret.

ALTER TABLE alert_rules DROP CONSTRAINT alert_rules_kind_scope;
ALTER TABLE alert_rules ADD CONSTRAINT alert_rules_kind_scope CHECK (
    (scope = 'platform' AND kind IN ('relay.offline', 'relay.over_capacity', 'pool.no_eligible_relay',
                                     'controlplane.degraded', 'certificate.expiring',
                                     'provider.break_glass_used', 'api_key.expiring'))
 OR (scope = 'tenant'   AND kind IN ('connector.offline', 'shield.offline', 'tenant.no_relay_path',
                                     'tenant.over_quota', 'tenant.usage_over'))
);

-- Seeded enabled, like the other platform rules. Fixed ids so docs and tests
-- can name them.
INSERT INTO alert_rules (id, kind, scope, name, enabled, severity, params, for_seconds, renotify_seconds) VALUES
  ('00000000-0000-0000-0002-000000000011', 'provider.break_glass_used', 'platform', 'Break-glass recovery used', TRUE, 'critical', '{}', 0, 0),
  ('00000000-0000-0000-0002-000000000012', 'api_key.expiring', 'platform', 'Provider API key expiring', TRUE, 'warning', '{"days_before": 14}', 0, 0);

CREATE TABLE provider_break_glass_events (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_user_id UUID         NOT NULL REFERENCES provider_users(id),
    email            TEXT         NOT NULL,              -- the recovered super-admin, as at recovery
    reason           TEXT         NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500),
    host             TEXT         NOT NULL CHECK (char_length(host) BETWEEN 1 AND 255),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Email to every active super-admin (sent by the controller, retried until
    -- at least one recipient took it); the count, never the addresses.
    notified_at      TIMESTAMPTZ,
    notified_count   INT          CHECK (notified_count >= 0),
    -- Console banner until a super-admin acknowledges it, with a note.
    acknowledged_at  TIMESTAMPTZ,
    acknowledged_by  UUID         REFERENCES provider_users(id),
    ack_note         TEXT,
    CONSTRAINT provider_break_glass_notified CHECK ((notified_at IS NULL) = (notified_count IS NULL)),
    CONSTRAINT provider_break_glass_ack CHECK (
        (acknowledged_at IS NULL AND acknowledged_by IS NULL AND ack_note IS NULL)
        OR (acknowledged_at IS NOT NULL AND acknowledged_by IS NOT NULL
            AND char_length(btrim(ack_note)) BETWEEN 1 AND 500))
);

CREATE INDEX provider_break_glass_unacked ON provider_break_glass_events (created_at) WHERE acknowledged_at IS NULL;

-- An event is a record: what happened never changes; notification and
-- acknowledgement are each set once.
CREATE FUNCTION provider_break_glass_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.provider_user_id <> OLD.provider_user_id OR NEW.email <> OLD.email
       OR NEW.reason <> OLD.reason OR NEW.host <> OLD.host OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'break-glass event % is immutable', OLD.id USING ERRCODE = '42501';
    END IF;
    IF OLD.notified_at IS NOT NULL AND (NEW.notified_at IS DISTINCT FROM OLD.notified_at
                                        OR NEW.notified_count IS DISTINCT FROM OLD.notified_count) THEN
        RAISE EXCEPTION 'break-glass event % was already notified', OLD.id USING ERRCODE = '42501';
    END IF;
    IF OLD.acknowledged_at IS NOT NULL AND (NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at
                                            OR NEW.acknowledged_by IS DISTINCT FROM OLD.acknowledged_by
                                            OR NEW.ack_note IS DISTINCT FROM OLD.ack_note) THEN
        RAISE EXCEPTION 'break-glass event % was already acknowledged', OLD.id USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_break_glass_guard BEFORE UPDATE ON provider_break_glass_events
    FOR EACH ROW EXECUTE FUNCTION provider_break_glass_guard();

GRANT SELECT, INSERT, UPDATE ON provider_break_glass_events TO ztna_app;
