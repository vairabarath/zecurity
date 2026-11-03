-- 032_platform_login_toggle.sql
--
-- PENDING-04 Phase 7 — platform login toggle (ADR-024 §5).
--
-- Per-workspace switch controlling whether the shared platform IdP (e.g. Google)
-- is offered as a login path for that workspace. Default TRUE preserves today's
-- behavior (platform fallback always available). When a workspace flips this to
-- FALSE, its members must authenticate through the workspace's own Enterprise
-- IdP(s): the discovery endpoint stops advertising the Bootstrap tier, and the
-- no-lockout guard (Phase 6) treats the platform fallback as unavailable — so
-- disabling platform login while no active Enterprise connection exists is
-- refused unless a break-glass admin is configured.

ALTER TABLE workspaces
    ADD COLUMN platform_login_enabled BOOLEAN NOT NULL DEFAULT TRUE;
