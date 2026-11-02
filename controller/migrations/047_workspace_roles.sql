-- 047_workspace_roles.sql — workspace admin-console roles (PENDING-19, R1).
--
-- Widens the role CHECK on users and workspace_members from
-- ('admin','member','viewer') to the six console roles. users.role is the
-- source of truth for authorization (live role check, PENDING-19 §5);
-- workspace_members.role is kept equal to it by setMemberRole.
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('admin', 'viewer', 'devops', 'security', 'helpdesk', 'member'));

ALTER TABLE workspace_members DROP CONSTRAINT workspace_members_role_check;
ALTER TABLE workspace_members ADD CONSTRAINT workspace_members_role_check
    CHECK (role IN ('admin', 'viewer', 'devops', 'security', 'helpdesk', 'member'));

-- The last-Admin guard locks a workspace's active Admin rows on every
-- role change and SCIM disable; idx_users_tenant_role (001) already covers
-- (tenant_id, role).
