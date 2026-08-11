-- 030_device_posture.sql
-- Sprint 15: device posture reports, profiles, requirements,
-- resource bindings, and latest profile evaluations.

CREATE TABLE device_posture_reports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Client-generated idempotency key. Validated as a canonical UUID by
    -- the application, but stored as text to preserve the submitted form.
    report_id TEXT NOT NULL UNIQUE,

    device_id UUID NOT NULL
        REFERENCES client_devices(id),

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    client_version TEXT NOT NULL,
    os_info JSONB NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_device_posture_reports_device_received
    ON device_posture_reports (device_id, received_at DESC);

CREATE INDEX idx_device_posture_reports_workspace
    ON device_posture_reports (workspace_id);

-- Required by the Phase 4 retention worker.
CREATE INDEX idx_device_posture_reports_received_at
    ON device_posture_reports (received_at);

CREATE TABLE device_posture_observations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    report_id UUID NOT NULL
        REFERENCES device_posture_reports(id) ON DELETE CASCADE,

    check_id TEXT NOT NULL,

    status TEXT NOT NULL
        CHECK (
            status IN (
                'PASS',
                'FAIL',
                'UNSUPPORTED',
                'UNKNOWN',
                'ERROR'
            )
        ),

    -- Normalized, bounded detail only. Never store raw command output here.
    detail TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (report_id, check_id)
);

CREATE INDEX idx_device_posture_observations_report
    ON device_posture_observations (report_id);

CREATE TABLE device_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    name TEXT NOT NULL,

    mode TEXT NOT NULL DEFAULT 'audit'
        CHECK (mode IN ('audit', 'enforce')),

    -- Incremented atomically whenever requirements change.
    revision BIGINT NOT NULL DEFAULT 1
        CHECK (revision > 0),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (workspace_id, name)
);

CREATE INDEX idx_device_profiles_workspace
    ON device_profiles (workspace_id);

CREATE TABLE device_profile_requirements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    profile_id UUID NOT NULL
        REFERENCES device_profiles(id) ON DELETE CASCADE,

    check_id TEXT NOT NULL,

    -- If true, UNSUPPORTED satisfies this requirement.
    -- FAIL, UNKNOWN, and ERROR still fail it.
    allow_unsupported BOOLEAN NOT NULL DEFAULT FALSE,

    UNIQUE (profile_id, check_id)
);
CREATE INDEX idx_device_profile_requirements_profile
    ON device_profile_requirements (profile_id);

CREATE TABLE resource_profile_bindings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    resource_id UUID NOT NULL
        REFERENCES resources(id) ON DELETE CASCADE,

    profile_id UUID NOT NULL
        REFERENCES device_profiles(id) ON DELETE CASCADE,

    -- The resources table calls this tenant_id. This table intentionally uses
    -- workspace_id for consistency with the posture subsystem. Application
    -- writes must verify resources.tenant_id = workspace_id and
    -- device_profiles.workspace_id = workspace_id.
    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    UNIQUE (resource_id, profile_id)
);

CREATE INDEX idx_resource_profile_bindings_workspace
    ON resource_profile_bindings (workspace_id);

CREATE INDEX idx_resource_profile_bindings_resource
    ON resource_profile_bindings (resource_id);

CREATE INDEX idx_resource_profile_bindings_profile
    ON resource_profile_bindings (profile_id);

    CREATE TABLE device_profile_evaluations (
    device_id UUID NOT NULL
        REFERENCES client_devices(id),

    profile_id UUID NOT NULL
        REFERENCES device_profiles(id) ON DELETE CASCADE,

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    satisfied BOOLEAN NOT NULL,

    -- The profile revision used to calculate this evaluation.
    profile_revision BIGINT NOT NULL
        CHECK (profile_revision > 0),

    reason TEXT,
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Must be nullable with ON DELETE SET NULL so raw-report retention can
    -- delete old reports without deleting the latest derived evaluation.
    report_id UUID
        REFERENCES device_posture_reports(id) ON DELETE SET NULL,

    PRIMARY KEY (device_id, profile_id)
);

CREATE INDEX idx_device_profile_evaluations_workspace
    ON device_profile_evaluations (workspace_id);

CREATE INDEX idx_device_profile_evaluations_profile
    ON device_profile_evaluations (profile_id);
