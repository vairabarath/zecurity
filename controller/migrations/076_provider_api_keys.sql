-- 076_provider_api_keys.sql — Sprint 27 K1 (F35; Decision Record D-76,
-- amendment 2026-11-23, provider access).
--
-- Read-only provider API keys for scripts:
--   * the key is zpk_<id without dashes>_<base64url of 32 random bytes>; only
--     its SHA-256 is stored (secret_sha256), compared in constant time. The
--     full key is returned once, by the create call, and never again;
--   * one role per key, never super-admin (the CHECK below; the role matrix
--     and the read-only rule live in decide(), internal/provider/authz.go);
--   * every key expires, at most 365 days after creation;
--   * an optional source CIDR allowlist; revocation is terminal;
--   * a key never becomes a session (it never goes through IssueSession).
-- Super-admins create and revoke keys (audited, reason required, D-30 as
-- amended by D-76).

CREATE TABLE provider_api_keys (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT         NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    role           TEXT         NOT NULL
                                CHECK (role IN ('relay-ops', 'auditor', 'billing', 'support')),
    prefix         TEXT         NOT NULL CHECK (prefix LIKE 'zpk\_%' AND char_length(prefix) = 12),
    secret_sha256  BYTEA        NOT NULL CHECK (octet_length(secret_sha256) = 32),
    created_by     UUID         NOT NULL REFERENCES provider_users(id),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ  NOT NULL,
    last_used_at   TIMESTAMPTZ,
    last_used_ip   TEXT,
    allowed_cidrs  CIDR[]       NOT NULL DEFAULT '{}',
    revoked_at     TIMESTAMPTZ,
    revoked_by     UUID         REFERENCES provider_users(id),
    revoke_reason  TEXT,
    CONSTRAINT provider_api_keys_expiry CHECK (expires_at > created_at AND expires_at <= created_at + INTERVAL '365 days'),
    CONSTRAINT provider_api_keys_revoked CHECK (
        (revoked_at IS NULL AND revoked_by IS NULL AND revoke_reason IS NULL)
        OR (revoked_at IS NOT NULL AND revoked_by IS NOT NULL AND revoke_reason IS NOT NULL))
);

-- A name is unique among keys that are not revoked.
CREATE UNIQUE INDEX provider_api_keys_active_name ON provider_api_keys (lower(name)) WHERE revoked_at IS NULL;

-- Revocation is terminal: a revoked key can't be un-revoked or edited, except
-- for the last-used stamp, which never changes after revocation either.
CREATE FUNCTION provider_api_keys_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'provider api key % is revoked', OLD.id USING ERRCODE = '42501';
    END IF;
    IF NEW.id <> OLD.id OR NEW.name <> OLD.name OR NEW.role <> OLD.role OR NEW.prefix <> OLD.prefix
       OR NEW.secret_sha256 <> OLD.secret_sha256 OR NEW.created_by <> OLD.created_by
       OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at
       OR NEW.allowed_cidrs <> OLD.allowed_cidrs THEN
        RAISE EXCEPTION 'provider api key % is immutable', OLD.id USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_api_keys_guard BEFORE UPDATE ON provider_api_keys
    FOR EACH ROW EXECUTE FUNCTION provider_api_keys_guard();

GRANT SELECT, INSERT, UPDATE ON provider_api_keys TO ztna_app;
