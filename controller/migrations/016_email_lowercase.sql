-- 016_email_lowercase.sql
--
-- Backfill historical mixed-case emails to lowercase so they match
-- canonicalized OAuth-supplied emails on lookup. Goes hand-in-hand with
-- write-time normalization added in the controller (CreateInvitation,
-- AcceptInvitation, Bootstrap, upsertUser, LookupWorkspacesByEmail).
--
-- See: ADR-005 Email Normalization.
--
-- Safe: emails are case-insensitive per RFC 5321; consolidating to lowercase
-- is the canonical form. The DELETE below covers the rare case where two rows
-- differ only by case in the same workspace_id — we keep the oldest and drop
-- duplicates. Without that, the UPDATE could violate
-- UNIQUE(workspace_id, email) on workspace_members.

BEGIN;

-- workspace_members: collapse case-duplicate rows before lowercasing.
-- Keep the row with the longest history (earliest invited_at), drop the rest.
-- NOTE: workspace_members has no created_at column (migration 013) — its
-- creation timestamp is invited_at; the tiebreak on id keeps it deterministic.
DELETE FROM workspace_members wm_outer
 WHERE EXISTS (
     SELECT 1 FROM workspace_members wm_inner
      WHERE wm_inner.workspace_id = wm_outer.workspace_id
        AND LOWER(wm_inner.email) = LOWER(wm_outer.email)
        AND (wm_inner.invited_at < wm_outer.invited_at
             OR (wm_inner.invited_at = wm_outer.invited_at
                 AND wm_inner.id < wm_outer.id))
 );

UPDATE workspace_members
   SET email = LOWER(email)
 WHERE email <> LOWER(email);

-- users: UNIQUE(tenant_id, provider_sub) already prevents email-only
-- duplicates, so a straight lowercase is safe.
UPDATE users
   SET email = LOWER(email)
 WHERE email <> LOWER(email);

-- invitations: no UNIQUE on email; straight lowercase.
UPDATE invitations
   SET email = LOWER(email)
 WHERE email <> LOWER(email);

COMMIT;
