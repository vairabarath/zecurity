-- PENDING-16 Phase 1
-- Device Resource Policy database foundation.
--
-- This phase only introduces the new database model.
-- Existing resource_profile_bindings remain untouched.

CREATE TABLE device_resource_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    name TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (workspace_id, name)
);

CREATE INDEX idx_device_resource_policies_workspace
    ON device_resource_policies(workspace_id);


ALTER TABLE resources
    ADD COLUMN device_resource_policy_id UUID NULL
        REFERENCES device_resource_policies(id) ON DELETE NO ACTION;

CREATE INDEX idx_resources_device_resource_policy_id
    ON resources(device_resource_policy_id);


CREATE TABLE resource_policy_profile_bindings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    device_resource_policy_id UUID NOT NULL
        REFERENCES device_resource_policies(id) ON DELETE CASCADE,

    profile_id UUID NOT NULL
        REFERENCES device_profiles(id) ON DELETE CASCADE,

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    UNIQUE (device_resource_policy_id, profile_id)
);

CREATE INDEX idx_resource_policy_profile_bindings_workspace
    ON resource_policy_profile_bindings(workspace_id);

CREATE INDEX idx_resource_policy_profile_bindings_policy
    ON resource_policy_profile_bindings(device_resource_policy_id);

CREATE INDEX idx_resource_policy_profile_bindings_profile
    ON resource_policy_profile_bindings(profile_id);
