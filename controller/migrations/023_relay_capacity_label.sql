-- Relay capacity tracking + tier label state machine (ADR-016 / Sprint 11 M2-C1).
--
-- Sprint 11 introduces tiered relay selection: the controller labels each
-- active relay by capacity tier (high / medium / low) computed from the
-- relay's reported connection_count and max_connections. Connectors receive
-- the labelled list via control stream and pick within the eligible pool.
--
-- The label is published with hysteresis: a candidate label observed from
-- recent heartbeats must remain stable for RELAY_LABEL_HOLDDOWN_SECS (60s)
-- before it is promoted to capacity_label and pushed to connectors. The
-- pending_* columns track the in-flight candidate; capacity_label is the
-- value currently published in LabelledRelayList. See ADR-016 §"Capacity
-- Tiers" and §"Architectural Review — Gap 1".
--
-- Defaults: existing relays start as 'high' with zero counts. The next
-- heartbeat will overwrite connection_count and max_connections; the
-- hysteresis logic in heartbeat.go will then drive capacity_label.

ALTER TABLE relays
    ADD COLUMN connection_count       INT         NOT NULL DEFAULT 0,
    ADD COLUMN max_connections        INT         NOT NULL DEFAULT 0,
    ADD COLUMN capacity_label         TEXT        NOT NULL DEFAULT 'high'
        CHECK (capacity_label IN ('high', 'medium', 'low')),
    ADD COLUMN pending_capacity_label TEXT
        CHECK (pending_capacity_label IS NULL
               OR pending_capacity_label IN ('high', 'medium', 'low')),
    ADD COLUMN pending_label_since    TIMESTAMPTZ,
    ADD COLUMN last_label_changed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW();
