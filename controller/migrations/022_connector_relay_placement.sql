-- Connector → Relay placement mapping.
--
-- Controller-side source of truth for "which relay is each connector
-- currently attached to". The Connector is the authoritative reporter:
-- it sends a ConnectorRelayState lifecycle message on every attach/detach
-- transition and re-advertises the current relay_id in every periodic
-- ConnectorHealthReport on the existing control stream. The Relay is not
-- involved in writing this table.
-- Read by the ACL compiler to emit the correct relay address per
-- connector in each workspace's snapshot.
--
-- v1 invariant: a connector is attached to exactly one relay at a time —
-- hence connector_id is the primary key. Reattach to a different relay
-- is an UPSERT that overwrites the old row.

CREATE TABLE connector_relay_placement (
    connector_id   UUID        NOT NULL,
    relay_id       UUID        NOT NULL,
    attached_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_confirmed TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connector_id),
    FOREIGN KEY (connector_id) REFERENCES connectors(id) ON DELETE CASCADE,
    FOREIGN KEY (relay_id)     REFERENCES relays(id)     ON DELETE CASCADE
);

-- Lookups by relay (e.g. "list all connectors on relay X" for reconciliation
-- and admin UI) need their own index since connector_id is the PK.
CREATE INDEX idx_crp_relay ON connector_relay_placement (relay_id);
