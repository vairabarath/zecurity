-- Sprint 21 Phase H — Provider Identity Foundation (Decision Record amendment
-- 2026-09-26, D-24…D-26; ADR-029).
--
-- Provider operators authenticate with local accounts (email + password,
-- Argon2id) through the controller's internal Provider Identity Service. The
-- provider JWT carries session_generation; bumping it revokes every token the
-- operator holds (logout, password change/reset, disable, role change).
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

ALTER TABLE provider_users
    -- Argon2id PHC string. NULL = no local password: reserved for operators who
    -- will sign in only through an optional external IdP later (D-29). Login
    -- rejects a NULL hash exactly like a wrong password.
    ADD COLUMN password_hash        TEXT,
    -- Set for the bootstrap account and for operator-created / reset temporary
    -- passwords; a login then yields a password-change-only token.
    ADD COLUMN must_change_password BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN password_changed_at  TIMESTAMPTZ,
    -- Every provider JWT embeds the value current at issue time; a mismatch
    -- rejects the token on the next request.
    ADD COLUMN session_generation   BIGINT      NOT NULL DEFAULT 1,
    ADD COLUMN last_login_at        TIMESTAMPTZ;
