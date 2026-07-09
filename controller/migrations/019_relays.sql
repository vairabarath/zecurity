-- Sprint 10.1 M2 Phase 2 — Relay registration & provisioning token store.
--


-- A relay is platform-level (not workspace-scoped). The operator pre-registers
-- a relay via POST /api/relays, which records the dns/ip allowlist and stores
-- the JTI of the issued provisioning token. The Provision gRPC RPC burns the
-- JTI on success and flips status pending → active.

CREATE TABLE relays (
    id                    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT         NOT NULL,
    status                TEXT         NOT NULL DEFAULT 'pending'
                                         CHECK (status IN ('pending', 'active', 'deleted')),
    dns_allowlist         TEXT[]       NOT NULL DEFAULT '{}',
    ip_allowlist          TEXT[]       NOT NULL DEFAULT '{}',
    enrollment_token_jti  TEXT,                       -- JWT jti; burned to NULL after Provision
    cert_serial           TEXT,                       -- last issued leaf serial (audit)
    cert_not_after        TIMESTAMPTZ,
    version               TEXT,                       -- last reported via Heartbeat
    hostname              TEXT,
    last_heartbeat_at     TIMESTAMPTZ,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_relays_token_jti  ON relays (enrollment_token_jti);
CREATE INDEX idx_relays_status     ON relays (status);
