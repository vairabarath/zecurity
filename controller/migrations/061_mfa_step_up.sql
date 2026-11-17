-- 061_mfa_step_up.sql — MFA and step-up authentication for workspace users
-- (PENDING-06, decisions M-1 … M-10).
--
-- 1. user_totp: a workspace user's TOTP authenticator (M-1). The secret is
--    sealed with pki.EncryptSecret and is never returned after enrolment.
--    enabled_at is NULL until the user confirms the first code; last_step is
--    the last accepted RFC 6238 step (a code is never accepted twice).
-- 2. resource_policies: the policy's authentication requirement (M-2).
--    auth_frequency_minutes NULL = device-only (no authentication
--    requirement; the default, so nothing changes until an admin opts in).
--    Otherwise a device gets the policy's resources only while its session's
--    last authentication is younger than the frequency (1 hour … 31 days);
--    require_mfa adds the same rule for the last MFA.
-- 3. client_devices: when the device's login session last authenticated and
--    last passed MFA (M-4, M-5), and its risk state (M-9). Kept here, in
--    Postgres, like the 056 session deadlines: the ACL compiler reads them
--    and a Valkey flush must not change who may connect.
--      session_auth_at     last authentication (sign-in, or a step-up)
--      session_mfa_at      last MFA (IdP amr proof, or a TOTP code); NULL = none
--      risk_score          sum of the risk signals since the last authentication
--      risk_reasons        the signals that raised it
--      reauth_required_at  set when the score reached the threshold; the
--                          device must authenticate after this instant
-- 4. workspaces.console_require_mfa: Admin Console Security (M-6).
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

CREATE TABLE user_totp (
    user_id           UUID        PRIMARY KEY,
    workspace_id      UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    secret_ciphertext TEXT        NOT NULL,
    secret_nonce      TEXT        NOT NULL,
    enabled_at        TIMESTAMPTZ,
    last_step         BIGINT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- FU-5: a key between workspace tables carries the workspace.
    CONSTRAINT user_totp_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX user_totp_workspace_idx ON user_totp (workspace_id);

-- 053 granted the runtime role only the tables that existed then.
GRANT SELECT, INSERT, UPDATE, DELETE ON user_totp TO ztna_app;

ALTER TABLE resource_policies
    ADD COLUMN auth_frequency_minutes INT
        CHECK (auth_frequency_minutes BETWEEN 60 AND 44640),
    ADD COLUMN require_mfa BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT resource_policies_mfa_needs_frequency
        CHECK (NOT require_mfa OR auth_frequency_minutes IS NOT NULL);

ALTER TABLE client_devices
    ADD COLUMN session_auth_at    TIMESTAMPTZ,
    ADD COLUMN session_mfa_at     TIMESTAMPTZ,
    ADD COLUMN risk_score         INT    NOT NULL DEFAULT 0 CHECK (risk_score >= 0),
    ADD COLUMN risk_reasons       TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN reauth_required_at TIMESTAMPTZ;

ALTER TABLE workspaces
    ADD COLUMN console_require_mfa BOOLEAN NOT NULL DEFAULT FALSE;
