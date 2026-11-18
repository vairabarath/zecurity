-- 067_tenant_first_admin_invite.sql — Sprint 23 S2: provider tenant creation
-- (F7; Decision Record D-44).
--
-- A super-admin creates a workspace from the provider console and invites its
-- first administrator. That workspace has no users yet, so the invitation has
-- no tenant sender, and it must grant the admin role.
--
-- 1. invitations.invited_by becomes optional. NULL means "issued by the
--    provider" (the operator is recorded in provider_audit_logs, never here:
--    provider users are not tenant users). The same-workspace foreign key
--    (046_) still applies whenever it is set.
-- 2. invitations.role: the role the invitation grants ('member' unless set).
--    Only a provider-issued invitation (invited_by IS NULL) may grant 'admin';
--    an invitation sent by a tenant user can never grant admin.
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

ALTER TABLE invitations ALTER COLUMN invited_by DROP NOT NULL;

ALTER TABLE invitations
    ADD COLUMN role TEXT NOT NULL DEFAULT 'member'
        CHECK (role IN ('member', 'admin'));

ALTER TABLE invitations
    ADD CONSTRAINT invitations_admin_only_from_provider
        CHECK (role = 'member' OR invited_by IS NULL);

-- A provider invitation always grants admin: a NULL sender with the member
-- role would be an unattributed tenant invitation.
ALTER TABLE invitations
    ADD CONSTRAINT invitations_sender_required_for_member
        CHECK (invited_by IS NOT NULL OR role = 'admin');
