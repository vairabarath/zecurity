-- PENDING-05 / ADR-025 — Phase 9 (Connection Lifecycle + Identity Health +
-- Sync Instances). Migration 034 introduced scim_sync_instances and added
-- sync_instance_id to users + external_identities, but omitted groups. This
-- migration closes that gap so scim-origin groups record the sync instance that
-- created/last-touched them (provenance + disable→re-enable reconcile, ADR-025
-- §12). Idempotent so it is safe to re-apply.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS sync_instance_id
        UUID REFERENCES scim_sync_instances(id) ON DELETE SET NULL;
