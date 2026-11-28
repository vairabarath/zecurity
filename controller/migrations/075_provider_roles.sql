-- 075_provider_roles.sql — Sprint 27 O1 (F19; Decision Record D-72, D-73,
-- D-74, amendment 2026-11-23, provider access).
--
-- Three new provider roles, one per operator as before:
--   * auditor: every read, no write;
--   * billing: plan cards, billing settings, invoices, usage, the tenant
--     list and tenant plan assignment;
--   * support: tenant reads, the tenants' alerts and timeline, health.
-- What each role may do is the role matrix in
-- controller/internal/provider/authz.go (decide()); this file only widens
-- the role CHECK of 025_provider_users.sql (never edit 025_). TOTP is
-- mandatory for all three (RequiresTOTP, D-74).

ALTER TABLE provider_users DROP CONSTRAINT provider_users_role_check;
ALTER TABLE provider_users ADD CONSTRAINT provider_users_role_check
    CHECK (role IN ('super-admin', 'relay-ops', 'auditor', 'billing', 'support'));
