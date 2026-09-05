-- PENDING-16 Phase 1
-- Device Resource Policy database foundation.
--
-- This phase only introduces the new database model.
-- Existing resource_profile_bindings remain untouched.
--
-- Numbered 037: this file was first authored as 034 on a branch that predated
-- 034_device_status, 034_scim_directory_sync, 035 and 036. Migrations apply in
-- filename order with no tracking table, so it is renumbered to the next free
-- slot rather than becoming a third 034.

CREATE TABLE device_resource_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    name TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (workspace_id, name),

    -- Redundant against the primary key alone, but required as the target of
    -- the composite foreign keys below: a tenant-safe foreign key can only
    -- reference a uniquely-constrained column pair. This is what turns "a
    -- resource's policy always belongs to that resource's workspace" into a
    -- database guarantee instead of an application convention.
    UNIQUE (id, workspace_id)
);

CREATE INDEX idx_device_resource_policies_workspace
    ON device_resource_policies(workspace_id);


-- Same purpose as the pair above, for the profile side of the
-- policy -> profile relationship. Additive only: migration 030 owns
-- device_profiles and is left untouched.
ALTER TABLE device_profiles
    ADD CONSTRAINT device_profiles_id_workspace_key
        UNIQUE (id, workspace_id);


-- A resource carries at most one policy, so this is a single nullable column
-- rather than a join table -- that is what makes "one resource has exactly one
-- resource policy" structurally impossible to violate.
--
-- The foreign key is composite: (policy, tenant) -> (policy, workspace). The
-- resources table calls its tenant column tenant_id while the posture subsystem
-- calls it workspace_id (see the note in migration 030); pairing them in the
-- foreign key is what rejects assigning a policy owned by another workspace.
-- Default MATCH SIMPLE semantics skip the check entirely while
-- device_resource_policy_id IS NULL, so "no policy assigned" remains valid.
ALTER TABLE resources
    ADD COLUMN device_resource_policy_id UUID NULL,
    ADD CONSTRAINT resources_device_resource_policy_fkey
        FOREIGN KEY (device_resource_policy_id, tenant_id)
        REFERENCES device_resource_policies (id, workspace_id)
        ON DELETE NO ACTION;

CREATE INDEX idx_resources_device_resource_policy_id
    ON resources(device_resource_policy_id);


-- Zero rows for a given policy is a valid, intentional state: the policy then
-- imposes no device requirement ("Any Device"). Duplicate (policy, profile)
-- pairs are rejected so one profile cannot attach to a policy twice.
CREATE TABLE resource_policy_profile_bindings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    device_resource_policy_id UUID NOT NULL,

    profile_id UUID NOT NULL,

    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,

    -- Both sides are tenant-paired, so a binding can never join a policy and a
    -- profile that live in different workspaces.
    FOREIGN KEY (device_resource_policy_id, workspace_id)
        REFERENCES device_resource_policies (id, workspace_id)
        ON DELETE CASCADE,

    FOREIGN KEY (profile_id, workspace_id)
        REFERENCES device_profiles (id, workspace_id)
        ON DELETE CASCADE,

    UNIQUE (device_resource_policy_id, profile_id)
);

CREATE INDEX idx_resource_policy_profile_bindings_workspace
    ON resource_policy_profile_bindings(workspace_id);

CREATE INDEX idx_resource_policy_profile_bindings_policy
    ON resource_policy_profile_bindings(device_resource_policy_id);

CREATE INDEX idx_resource_policy_profile_bindings_profile
    ON resource_policy_profile_bindings(profile_id);
