-- 069_alerts.sql — Sprint 24 A1/A2 (F26 alert rules, Decision Record
-- D-50, D-52, D-53).
--
-- Two alert scopes, separate in the schema from the start (D-50):
--   platform — the provider's infrastructure (relays, pools, control plane,
--              platform certificates); never carries a tenant and is never
--              shown to a tenant;
--   tenant   — something one tenant can act on; ALWAYS carries tenant_id.
-- The CHECKs below make a tenant row without tenant_id (or a platform row
-- with one) impossible, whatever the code does. Each rule kind belongs to
-- exactly one scope (alert_rules_kind_scope).
--
-- Tenant rows reference workspaces ON DELETE CASCADE: the S4 purge deletes
-- the workspaces row, so a purged tenant's alert states, events and
-- deliveries go with it (D-46).
--
-- A1 tables: alert_rules (built-in kinds with parameters; no query
-- language, D-53), alert_states (one row per rule and target while pending
-- or firing; removed when the condition clears), alert_events (append-only
-- history: fired, renotified, resolved — what F27 shows later).
-- A2 tables (D-52): alert_channels (webhook or email; the webhook secret is
-- sealed with pki.EncryptSecret and never returned) and alert_deliveries
-- (one row per delivery attempt).
--
-- Seeded: one rule per kind. Platform rules are enabled; tenant rules are
-- disabled until a super-admin turns them on.

CREATE TABLE alert_rules (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             TEXT        NOT NULL,
    scope            TEXT        NOT NULL CHECK (scope IN ('platform', 'tenant')),
    name             TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
    severity         TEXT        NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    params           JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(params) = 'object'),
    -- The condition must hold this long before the alert fires (hysteresis).
    for_seconds      INT         NOT NULL DEFAULT 0 CHECK (for_seconds BETWEEN 0 AND 86400),
    -- While firing, notify again after this long; 0 = never.
    renotify_seconds INT         NOT NULL DEFAULT 0
                                 CHECK (renotify_seconds = 0 OR renotify_seconds BETWEEN 300 AND 604800),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT alert_rules_name_key UNIQUE (name),
    CONSTRAINT alert_rules_kind_scope CHECK (
        (scope = 'platform' AND kind IN ('relay.offline', 'relay.over_capacity', 'pool.no_eligible_relay',
                                         'controlplane.degraded', 'certificate.expiring'))
     OR (scope = 'tenant'   AND kind IN ('connector.offline', 'shield.offline', 'tenant.no_relay_path',
                                         'tenant.over_quota'))
    )
);

CREATE TABLE alert_states (
    rule_id          UUID        NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    target_type      TEXT        NOT NULL,
    target_id        TEXT        NOT NULL,
    scope            TEXT        NOT NULL CHECK (scope IN ('platform', 'tenant')),
    tenant_id        UUID        NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    target_name      TEXT        NOT NULL,
    status           TEXT        NOT NULL CHECK (status IN ('pending', 'firing')),
    severity         TEXT        NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    summary          TEXT        NOT NULL,
    pending_since    TIMESTAMPTZ NOT NULL,
    firing_since     TIMESTAMPTZ NULL,
    last_notified_at TIMESTAMPTZ NULL,
    PRIMARY KEY (rule_id, target_type, target_id),
    CONSTRAINT alert_states_scope_tenant CHECK ((scope = 'tenant') = (tenant_id IS NOT NULL)),
    CONSTRAINT alert_states_firing CHECK ((status = 'firing') = (firing_since IS NOT NULL))
);
CREATE INDEX alert_states_tenant_idx ON alert_states (tenant_id) WHERE tenant_id IS NOT NULL;

CREATE TABLE alert_events (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id     UUID        NOT NULL,  -- no FK: history outlives a deleted rule
    kind        TEXT        NOT NULL,
    scope       TEXT        NOT NULL CHECK (scope IN ('platform', 'tenant')),
    tenant_id   UUID        NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    target_type TEXT        NOT NULL,
    target_id   TEXT        NOT NULL,
    target_name TEXT        NOT NULL,
    transition  TEXT        NOT NULL CHECK (transition IN ('fired', 'renotified', 'resolved')),
    status      TEXT        NOT NULL CHECK (status IN ('firing', 'resolved')),
    severity    TEXT        NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    summary     TEXT        NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT alert_events_scope_tenant CHECK ((scope = 'tenant') = (tenant_id IS NOT NULL)),
    CONSTRAINT alert_events_status CHECK ((transition = 'resolved') = (status = 'resolved'))
);
CREATE INDEX alert_events_occurred_idx ON alert_events (occurred_at DESC, id DESC);
CREATE INDEX alert_events_tenant_idx ON alert_events (tenant_id, occurred_at DESC) WHERE tenant_id IS NOT NULL;

CREATE TABLE alert_channels (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    type             TEXT        NOT NULL CHECK (type IN ('webhook', 'email')),
    -- Which alerts the channel receives (D-50): all, platform only, tenant only.
    scope_filter     TEXT        NOT NULL DEFAULT 'all' CHECK (scope_filter IN ('all', 'platform', 'tenant')),
    min_severity     TEXT        NOT NULL DEFAULT 'warning' CHECK (min_severity IN ('critical', 'warning', 'info')),
    enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
    url              TEXT        NULL,
    encrypted_secret TEXT        NULL,
    secret_nonce     TEXT        NULL,
    -- Email: explicit recipients; NULL = every active super-admin.
    recipients       TEXT[]      NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT alert_channels_name_key UNIQUE (name),
    CONSTRAINT alert_channels_webhook CHECK (
        (type = 'webhook') = (url IS NOT NULL AND encrypted_secret IS NOT NULL AND secret_nonce IS NOT NULL)),
    CONSTRAINT alert_channels_email CHECK (
        type = 'email' OR recipients IS NULL),
    CONSTRAINT alert_channels_recipients CHECK (
        recipients IS NULL OR cardinality(recipients) BETWEEN 1 AND 50)
);

CREATE TABLE alert_deliveries (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    UUID        NOT NULL REFERENCES alert_events(id) ON DELETE CASCADE,
    channel_id  UUID        NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    attempt     INT         NOT NULL CHECK (attempt >= 1),
    status      TEXT        NOT NULL CHECK (status IN ('delivered', 'failed', 'retrying')),
    http_status INT         NULL,
    error       TEXT        NULL,
    at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX alert_deliveries_event_idx ON alert_deliveries (event_id);

-- Seeded rules: fixed ids so docs and tests can name them.
INSERT INTO alert_rules (id, kind, scope, name, enabled, severity, params, for_seconds, renotify_seconds) VALUES
  ('00000000-0000-0000-0002-000000000001', 'relay.offline',          'platform', 'Relay offline',                TRUE,  'critical', '{}',                         300, 0),
  ('00000000-0000-0000-0002-000000000002', 'relay.over_capacity',    'platform', 'Relay near or over capacity',  TRUE,  'warning',  '{"threshold_percent": 90}', 600, 0),
  ('00000000-0000-0000-0002-000000000003', 'pool.no_eligible_relay', 'platform', 'Relay pool without a relay',   TRUE,  'critical', '{}',                          60, 0),
  ('00000000-0000-0000-0002-000000000004', 'controlplane.degraded',  'platform', 'Control plane degraded',       TRUE,  'critical', '{}',                         120, 0),
  ('00000000-0000-0000-0002-000000000005', 'certificate.expiring',   'platform', 'Platform certificate expiring', TRUE, 'warning',  '{"days": 30}',                0, 86400),
  ('00000000-0000-0000-0002-000000000006', 'connector.offline',      'tenant',   'Connector offline',            FALSE, 'warning',  '{}',                         600, 0),
  ('00000000-0000-0000-0002-000000000007', 'shield.offline',         'tenant',   'Shield offline',               FALSE, 'warning',  '{}',                         600, 0),
  ('00000000-0000-0000-0002-000000000008', 'tenant.no_relay_path',   'tenant',   'Tenant without a relay path',  FALSE, 'critical', '{}',                         300, 0),
  ('00000000-0000-0000-0002-000000000009', 'tenant.over_quota',      'tenant',   'Tenant over quota',            FALSE, 'info',     '{}',                           0, 0);

-- Runtime role: rules, states and channels are managed; events and
-- deliveries are append-only (no UPDATE or DELETE; the purge cascade runs as
-- the table owner).
GRANT SELECT, INSERT, UPDATE, DELETE ON alert_rules, alert_states, alert_channels TO ztna_app;
GRANT SELECT, INSERT ON alert_events, alert_deliveries TO ztna_app;
