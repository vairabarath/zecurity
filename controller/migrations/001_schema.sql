-- 001_schema.sql

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Root and Intermediate CAs stored once at controller startup
CREATE TABLE ca_root (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE ca_intermediate (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Workspaces: root of tenant hierarchy
CREATE TABLE workspaces (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'provisioning'
                            CHECK (status IN
                              ('provisioning','active','suspended','deleted')),
    ca_cert_pem TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Users: one row per (provider_sub, workspace)
-- Same Google account in two workspaces = two rows with different tenant_id
CREATE TABLE users (
    id              UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID    NOT NULL REFERENCES workspaces(id)
                            ON DELETE CASCADE,
    email           TEXT    NOT NULL,
    provider        TEXT    NOT NULL,
    provider_sub    TEXT    NOT NULL,
    role            TEXT    NOT NULL DEFAULT 'member'
                            CHECK (role IN ('admin','member','viewer')),
    status          TEXT    NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active','suspended','deleted')),
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, provider_sub)
);

-- WorkspaceCA private keys: encrypted at rest, never returned via API
CREATE TABLE workspace_ca_keys (
    id                    UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID    NOT NULL REFERENCES workspaces(id)
                                  ON DELETE CASCADE UNIQUE,
    encrypted_private_key TEXT    NOT NULL,
    nonce                 TEXT    NOT NULL,
    key_algorithm         TEXT    NOT NULL DEFAULT 'EC-P384',
    certificate_pem       TEXT    NOT NULL,
    not_before            TIMESTAMPTZ NOT NULL,
    not_after             TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes
-- provider_sub lookup happens before tenant_id is known (auth flow step 1)
CREATE INDEX idx_users_provider_sub  ON users (provider_sub, provider);
-- All other queries are scoped to tenant_id
CREATE INDEX idx_users_tenant_email  ON users (tenant_id, email);
CREATE INDEX idx_users_tenant_role   ON users (tenant_id, role);
-- workspace status check happens on every authenticated request
CREATE INDEX idx_workspaces_active  ON workspaces (id)
    WHERE status = 'active';
