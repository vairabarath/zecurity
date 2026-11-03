CREATE TABLE provider_users (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT         NOT NULL UNIQUE,        -- corp Google email; join key. Stored lowercase (mirror ADR-005).
    role         TEXT         NOT NULL DEFAULT 'relay-ops'
                                CHECK (role IN ('super-admin', 'relay-ops')),
    disabled_at  TIMESTAMPTZ,                         -- soft-disable; keeps provider_audit_logs FK stable
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);