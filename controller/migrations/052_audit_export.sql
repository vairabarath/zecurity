-- 052_audit_export.sql — PENDING-11 S1b: SIEM export sinks and cursors (P-5, P-6, P-8).
--
-- A sink is one destination (webhook or Splunk HEC) for one workspace's
-- hash-chained audit streams (051). Each sink keeps one cursor per stream: the
-- last seq the destination acknowledged. Delivery is at-least-once; the
-- exporter advances the cursor only after a 2xx.
--
-- Secrets (the HMAC signing key and the optional extra auth header value) are
-- sealed with pki.EncryptSecret under "audit-export-sink:<sink id>" and are
-- never returned after create/rotate.
--
-- Privileges: 051's ALTER DEFAULT PRIVILEGES gives ztna_app normal DML here.

CREATE TABLE audit_export_sinks (
    id                UUID        PRIMARY KEY,
    workspace_id      UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name              TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    kind              TEXT        NOT NULL CHECK (kind IN ('webhook', 'splunk_hec')),
    url               TEXT        NOT NULL CHECK (url LIKE 'https://%'),
    streams           TEXT[]      NOT NULL CHECK (cardinality(streams) BETWEEN 1 AND 2
                                                  AND streams <@ ARRAY['admin', 'access']::TEXT[]),
    enabled           BOOLEAN     NOT NULL DEFAULT true,
    -- Non-secret adapter settings (for example the Splunk index).
    config            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    signing_key_ct    TEXT        NOT NULL,
    signing_key_nonce TEXT        NOT NULL,
    auth_header_name  TEXT,
    auth_header_ct    TEXT,
    auth_header_nonce TEXT,
    created_by        UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name),
    CHECK ((auth_header_name IS NULL) = (auth_header_ct IS NULL)
       AND (auth_header_ct IS NULL) = (auth_header_nonce IS NULL))
);

CREATE INDEX audit_export_sinks_workspace ON audit_export_sinks (workspace_id);

CREATE TABLE audit_export_cursors (
    sink_id         UUID        NOT NULL REFERENCES audit_export_sinks(id) ON DELETE CASCADE,
    stream          TEXT        NOT NULL CHECK (stream IN ('admin', 'access')),
    -- Last seq the destination acknowledged. Set on create to the retention
    -- checkpoint ("all retained history") or the head ("now").
    last_seq        BIGINT      NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_success_at TIMESTAMPTZ,
    last_error      TEXT,
    failing_since   TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sink_id, stream)
);
