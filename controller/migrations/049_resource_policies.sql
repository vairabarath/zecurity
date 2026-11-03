-- 049_resource_policies.sql — Resource → Resource Policy → Device Profiles
-- (PENDING-16, decisions D-1..D-4).
--
-- 1. resource_policies: per-workspace policies. Exactly one per workspace is
--    the Default (is_default), created with the workspace by a trigger; it
--    can't be deleted or renamed (enforced by the controller).
-- 2. resource_policy_profiles: the device profiles a policy requires. A device
--    passes when it satisfies ANY of them (OR); a policy with none allows any
--    device. Deleting a profile removes it from its policies (CASCADE); the
--    controller only does that after an explicit confirmation (D-4).
-- 3. resources.policy_id: every resource has exactly one policy. A BEFORE
--    INSERT trigger fills in the workspace's Default when none is given, so
--    every insert path keeps working. NO ACTION (not RESTRICT): deleting a
--    workspace cascades to both resources and policies in one statement.
-- 4. The direct Resource ↔ Device Profile binding and the audit/enforce mode
--    are removed: a policy's profiles always gate (D-3).
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

CREATE TABLE resource_policies (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    description  TEXT,
    is_default   BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT resource_policies_workspace_name_key UNIQUE (workspace_id, name),
    CONSTRAINT resource_policies_id_workspace_key   UNIQUE (id, workspace_id)
);

-- At most one Default per workspace.
CREATE UNIQUE INDEX resource_policies_one_default
    ON resource_policies (workspace_id) WHERE is_default;

CREATE TABLE resource_policy_profiles (
    policy_id    UUID NOT NULL,
    profile_id   UUID NOT NULL,
    workspace_id UUID NOT NULL,
    PRIMARY KEY (policy_id, profile_id),
    CONSTRAINT resource_policy_profiles_policy_same_workspace_fkey
        FOREIGN KEY (policy_id, workspace_id)
        REFERENCES resource_policies (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT resource_policy_profiles_profile_same_workspace_fkey
        FOREIGN KEY (profile_id, workspace_id)
        REFERENCES device_profiles (id, workspace_id) ON DELETE CASCADE
);

CREATE INDEX resource_policy_profiles_profile_idx ON resource_policy_profiles (profile_id);

-- Every workspace gets its Default policy ("Any device", D-1) when created.
CREATE FUNCTION resource_policies_create_default() RETURNS trigger AS $$
BEGIN
    INSERT INTO resource_policies (workspace_id, name, description, is_default)
    VALUES (NEW.id, 'Default', 'Applies to resources not assigned to another policy.', TRUE);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workspaces_create_default_resource_policy
    AFTER INSERT ON workspaces
    FOR EACH ROW EXECUTE FUNCTION resource_policies_create_default();

-- Backfill: workspaces that existed before this file ran.
INSERT INTO resource_policies (workspace_id, name, description, is_default)
SELECT w.id, 'Default', 'Applies to resources not assigned to another policy.', TRUE
  FROM workspaces w
 WHERE NOT EXISTS (SELECT 1 FROM resource_policies p WHERE p.workspace_id = w.id AND p.is_default);

-- Every resource has exactly one policy.
ALTER TABLE resources ADD COLUMN policy_id UUID;

-- A resource inserted without a policy gets its workspace's Default.
CREATE FUNCTION resources_default_policy() RETURNS trigger AS $$
BEGIN
    IF NEW.policy_id IS NULL THEN
        SELECT p.id INTO NEW.policy_id
          FROM resource_policies p
         WHERE p.workspace_id = NEW.tenant_id AND p.is_default;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER resources_set_default_policy
    BEFORE INSERT ON resources
    FOR EACH ROW EXECUTE FUNCTION resources_default_policy();

-- Backfill: resources that existed before this file ran.
UPDATE resources r
   SET policy_id = p.id
  FROM resource_policies p
 WHERE p.workspace_id = r.tenant_id AND p.is_default AND r.policy_id IS NULL;

ALTER TABLE resources ALTER COLUMN policy_id SET NOT NULL;

ALTER TABLE resources
    ADD CONSTRAINT resources_policy_same_workspace_fkey
        FOREIGN KEY (policy_id, tenant_id)
        REFERENCES resource_policies (id, workspace_id) ON DELETE NO ACTION;

CREATE INDEX resources_policy_idx ON resources (policy_id);

-- The direct binding and the audit/enforce mode are replaced by policies.
DROP TABLE resource_profile_bindings;
ALTER TABLE device_profiles DROP COLUMN mode;
