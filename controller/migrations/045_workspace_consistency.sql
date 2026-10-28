-- 045_workspace_consistency.sql (Sprint 22 FU-2, B-1 defense in depth)
--
-- The database refuses rows that tie together objects from different
-- workspaces. The application and the ACL compiler already enforce this
-- (B-1, the resource-network fix); these constraints catch a path that misses
-- it:
--   - a resource's remote network is in the resource's workspace;
--   - an access rule's resource and group are in the rule's workspace;
--   - a group member is a user of the group's workspace. group_members gets a
--     workspace_id column, always copied from the group by a trigger (so the
--     writers don't change); the composite key to users is the real check;
--   - a scan result's key includes the reporting connector, so one
--     connector's row can never suppress or stand in for another's (FU-3
--     residual).
-- Every replaced foreign key keeps its ON DELETE CASCADE. 043_ stays reserved
-- for Sprint 22 Phase D (043_relay_drain.sql).
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

-- Targets for the composite foreign keys.
ALTER TABLE remote_networks ADD CONSTRAINT remote_networks_id_tenant_key UNIQUE (id, tenant_id);
ALTER TABLE resources       ADD CONSTRAINT resources_id_tenant_key       UNIQUE (id, tenant_id);
ALTER TABLE groups          ADD CONSTRAINT groups_id_workspace_key       UNIQUE (id, workspace_id);
ALTER TABLE users           ADD CONSTRAINT users_id_tenant_key           UNIQUE (id, tenant_id);

ALTER TABLE resources
    DROP CONSTRAINT resources_remote_network_id_fkey,
    ADD CONSTRAINT resources_remote_network_same_workspace_fkey
        FOREIGN KEY (remote_network_id, tenant_id)
        REFERENCES remote_networks (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE access_rules
    DROP CONSTRAINT access_rules_resource_id_fkey,
    DROP CONSTRAINT access_rules_group_id_fkey,
    ADD CONSTRAINT access_rules_resource_same_workspace_fkey
        FOREIGN KEY (resource_id, workspace_id)
        REFERENCES resources (id, tenant_id) ON DELETE CASCADE,
    ADD CONSTRAINT access_rules_group_same_workspace_fkey
        FOREIGN KEY (group_id, workspace_id)
        REFERENCES groups (id, workspace_id) ON DELETE CASCADE;

ALTER TABLE group_members
    ADD COLUMN workspace_id UUID NOT NULL,
    DROP CONSTRAINT group_members_group_id_fkey,
    DROP CONSTRAINT group_members_user_id_fkey,
    ADD CONSTRAINT group_members_group_same_workspace_fkey
        FOREIGN KEY (group_id, workspace_id)
        REFERENCES groups (id, workspace_id) ON DELETE CASCADE,
    ADD CONSTRAINT group_members_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id)
        REFERENCES users (id, tenant_id) ON DELETE CASCADE;

-- The workspace always comes from the group, whatever the writer sent.
CREATE FUNCTION group_members_set_workspace() RETURNS trigger AS $$
BEGIN
    SELECT workspace_id INTO NEW.workspace_id FROM groups WHERE id = NEW.group_id;
    IF NEW.workspace_id IS NULL THEN
        RAISE EXCEPTION 'group % does not exist', NEW.group_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER group_members_set_workspace
    BEFORE INSERT OR UPDATE OF group_id, workspace_id ON group_members
    FOR EACH ROW EXECUTE FUNCTION group_members_set_workspace();

ALTER TABLE connector_scan_results
    DROP CONSTRAINT connector_scan_results_pkey,
    ADD PRIMARY KEY (request_id, connector_id, ip, port, protocol);
