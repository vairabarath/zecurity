-- 046_workspace_consistency_all.sql (Sprint 22 FU-5, follows 045 / FU-2)
--
-- Every remaining foreign key between two workspace-scoped tables also
-- carries the workspace, so the database refuses a row that points into
-- another workspace (devices, posture, profiles, bindings, shields,
-- connectors, SCIM sync instances, users). Each key keeps its ON DELETE
-- action; SET NULL keys clear only the reference column, never the row's
-- workspace. The keys were derived from the catalogue (every single-column
-- key from a tenant_id/workspace_id table to another one); the dev database
-- had no row breaking any of them.
--
-- Not constrained, on purpose: keys to identity_connections. A connection
-- with tenant_id NULL is a shared platform connection (ADR-024) that every
-- workspace's external identities may use, which a composite key can't
-- express.
--
-- Scan requests: triggerScan records which connector a request was sent to
-- (scan_requests), and a result row must belong to that request and that
-- connector. A connector's report for a request it wasn't sent is dropped,
-- also within one workspace (FU-3 residual). Results go with their request,
-- and requests with their connector.
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

-- Targets for the composite foreign keys (045 added remote_networks,
-- resources, groups and users).
ALTER TABLE client_devices         ADD CONSTRAINT client_devices_id_workspace_key         UNIQUE (id, workspace_id);
ALTER TABLE connectors             ADD CONSTRAINT connectors_id_tenant_key                UNIQUE (id, tenant_id);
ALTER TABLE device_posture_reports ADD CONSTRAINT device_posture_reports_id_workspace_key UNIQUE (id, workspace_id);
ALTER TABLE device_profiles        ADD CONSTRAINT device_profiles_id_workspace_key        UNIQUE (id, workspace_id);
ALTER TABLE scim_sync_instances    ADD CONSTRAINT scim_sync_instances_id_workspace_key    UNIQUE (id, workspace_id);
ALTER TABLE shields                ADD CONSTRAINT shields_id_tenant_key                   UNIQUE (id, tenant_id);

ALTER TABLE device_posture_reports
    DROP CONSTRAINT device_posture_reports_device_id_fkey,
    ADD CONSTRAINT device_posture_reports_device_same_workspace_fkey
        FOREIGN KEY (device_id, workspace_id) REFERENCES client_devices (id, workspace_id);

ALTER TABLE device_profile_evaluations
    DROP CONSTRAINT device_profile_evaluations_device_id_fkey,
    ADD CONSTRAINT device_profile_evaluations_device_same_workspace_fkey
        FOREIGN KEY (device_id, workspace_id) REFERENCES client_devices (id, workspace_id);

ALTER TABLE shields
    DROP CONSTRAINT shields_connector_id_fkey,
    ADD CONSTRAINT shields_connector_same_workspace_fkey
        FOREIGN KEY (connector_id, tenant_id) REFERENCES connectors (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE device_profile_evaluations
    DROP CONSTRAINT device_profile_evaluations_report_id_fkey,
    ADD CONSTRAINT device_profile_evaluations_report_same_workspace_fkey
        FOREIGN KEY (report_id, workspace_id) REFERENCES device_posture_reports (id, workspace_id) ON DELETE SET NULL (report_id);

ALTER TABLE device_profile_evaluations
    DROP CONSTRAINT device_profile_evaluations_profile_id_fkey,
    ADD CONSTRAINT device_profile_evaluations_profile_same_workspace_fkey
        FOREIGN KEY (profile_id, workspace_id) REFERENCES device_profiles (id, workspace_id) ON DELETE CASCADE;

ALTER TABLE resource_profile_bindings
    DROP CONSTRAINT resource_profile_bindings_profile_id_fkey,
    ADD CONSTRAINT resource_profile_bindings_profile_same_workspace_fkey
        FOREIGN KEY (profile_id, workspace_id) REFERENCES device_profiles (id, workspace_id) ON DELETE CASCADE;

ALTER TABLE connectors
    DROP CONSTRAINT connectors_remote_network_id_fkey,
    ADD CONSTRAINT connectors_remote_network_same_workspace_fkey
        FOREIGN KEY (remote_network_id, tenant_id) REFERENCES remote_networks (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE shields
    DROP CONSTRAINT shields_remote_network_id_fkey,
    ADD CONSTRAINT shields_remote_network_same_workspace_fkey
        FOREIGN KEY (remote_network_id, tenant_id) REFERENCES remote_networks (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE resource_profile_bindings
    DROP CONSTRAINT resource_profile_bindings_resource_id_fkey,
    ADD CONSTRAINT resource_profile_bindings_resource_same_workspace_fkey
        FOREIGN KEY (resource_id, workspace_id) REFERENCES resources (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE external_identities
    DROP CONSTRAINT external_identities_sync_instance_id_fkey,
    ADD CONSTRAINT external_identities_sync_instance_same_workspace_fkey
        FOREIGN KEY (sync_instance_id, tenant_id) REFERENCES scim_sync_instances (id, workspace_id) ON DELETE SET NULL (sync_instance_id);

ALTER TABLE groups
    DROP CONSTRAINT groups_sync_instance_id_fkey,
    ADD CONSTRAINT groups_sync_instance_same_workspace_fkey
        FOREIGN KEY (sync_instance_id, workspace_id) REFERENCES scim_sync_instances (id, workspace_id) ON DELETE SET NULL (sync_instance_id);

ALTER TABLE users
    DROP CONSTRAINT users_sync_instance_id_fkey,
    ADD CONSTRAINT users_sync_instance_same_workspace_fkey
        FOREIGN KEY (sync_instance_id, tenant_id) REFERENCES scim_sync_instances (id, workspace_id) ON DELETE SET NULL (sync_instance_id);

ALTER TABLE resources
    DROP CONSTRAINT resources_shield_id_fkey,
    ADD CONSTRAINT resources_shield_same_workspace_fkey
        FOREIGN KEY (shield_id, tenant_id) REFERENCES shields (id, tenant_id) ON DELETE SET NULL (shield_id);

ALTER TABLE client_devices
    DROP CONSTRAINT client_devices_approved_by_fkey,
    ADD CONSTRAINT client_devices_approved_by_same_workspace_fkey
        FOREIGN KEY (approved_by, workspace_id) REFERENCES users (id, tenant_id) ON DELETE SET NULL (approved_by);

ALTER TABLE client_devices
    DROP CONSTRAINT client_devices_user_id_fkey,
    ADD CONSTRAINT client_devices_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE external_identities
    DROP CONSTRAINT external_identities_user_id_fkey,
    ADD CONSTRAINT external_identities_user_same_workspace_fkey
        FOREIGN KEY (user_id, tenant_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE invitations
    DROP CONSTRAINT invitations_invited_by_fkey,
    ADD CONSTRAINT invitations_invited_by_same_workspace_fkey
        FOREIGN KEY (invited_by, workspace_id) REFERENCES users (id, tenant_id);

ALTER TABLE outbox_events
    DROP CONSTRAINT outbox_events_user_id_fkey,
    ADD CONSTRAINT outbox_events_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE SET NULL (user_id);

ALTER TABLE scim_identity_conflicts
    DROP CONSTRAINT scim_identity_conflicts_user_id_fkey,
    ADD CONSTRAINT scim_identity_conflicts_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE workspace_members
    DROP CONSTRAINT workspace_members_invited_by_fkey,
    ADD CONSTRAINT workspace_members_invited_by_same_workspace_fkey
        FOREIGN KEY (invited_by, workspace_id) REFERENCES users (id, tenant_id);

ALTER TABLE workspace_members
    DROP CONSTRAINT workspace_members_user_id_fkey,
    ADD CONSTRAINT workspace_members_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE;

ALTER TABLE workspace_permissions
    DROP CONSTRAINT workspace_permissions_granted_by_fkey,
    ADD CONSTRAINT workspace_permissions_granted_by_same_workspace_fkey
        FOREIGN KEY (granted_by, workspace_id) REFERENCES users (id, tenant_id) ON DELETE SET NULL (granted_by);

ALTER TABLE workspace_permissions
    DROP CONSTRAINT workspace_permissions_user_id_fkey,
    ADD CONSTRAINT workspace_permissions_user_same_workspace_fkey
        FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE;

CREATE TABLE scan_requests (
    request_id   TEXT        PRIMARY KEY,
    connector_id UUID        NOT NULL,
    tenant_id    UUID        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (request_id, connector_id),
    FOREIGN KEY (connector_id, tenant_id) REFERENCES connectors (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX idx_scan_requests_created_at ON scan_requests (created_at);

ALTER TABLE connector_scan_results
    ADD CONSTRAINT connector_scan_results_request_fkey
        FOREIGN KEY (request_id, connector_id)
        REFERENCES scan_requests (request_id, connector_id) ON DELETE CASCADE;
