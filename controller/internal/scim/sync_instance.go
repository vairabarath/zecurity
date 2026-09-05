package scim

import (
	"context"
	"fmt"
)

// Sync-instance primitives (ADR-025 §12 / PENDING-05 P9).
//
// Each SCIM "connect" opens a Sync Instance — a scim_sync_instances row with a
// fresh UUID — and every provisioned user/external_identity/group records the
// sync_instance_id that created/last-touched it. A disable→re-enable reconnect
// opens a NEW instance; objects whose sync_instance_id no longer matches the
// current instance are "stale" and can be reconciled (audited/touched) without
// guessing directory intent. The instance also carries last_sync_at, which the
// connection-health derivation reads.

// OpenSyncInstance inserts a fresh scim_sync_instances row for a connection and
// returns its id. displayName/externalID are optional provenance hints (the
// IdP's directory name / tenant id, when known).
func (s *DirectoryService) OpenSyncInstance(ctx context.Context, sc *scope, externalID, displayName string) (string, error) {
	var id string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO scim_sync_instances
		   (workspace_id, connection_id, external_id, display_name, created_at, last_sync_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 RETURNING id::text`,
		sc.workspaceID, sc.connectionID, nullIfText(externalID), nullIfText(displayName),
	).Scan(&id); err != nil {
		return "", fmt.Errorf("open sync instance: %w", err)
	}
	return id, nil
}

// CurrentSyncInstance returns the most recent sync-instance id for the
// connection, or "" if none has been opened.
func (s *DirectoryService) CurrentSyncInstance(ctx context.Context, sc *scope) string {
	var id string
	_ = s.pool.QueryRow(ctx,
		`SELECT id::text FROM scim_sync_instances
		  WHERE workspace_id = $1 AND connection_id = $2
		  ORDER BY created_at DESC LIMIT 1`,
		sc.workspaceID, sc.connectionID,
	).Scan(&id)
	return id
}

// EnsureSyncInstance returns the connection's current sync instance, opening a
// new one if none exists yet. Called by every SCIM write path so provisioned
// objects always carry a sync_instance_id (replacing the Phase 5 NULL default).
func (s *DirectoryService) EnsureSyncInstance(ctx context.Context, sc *scope) (string, error) {
	if cur := s.CurrentSyncInstance(ctx, sc); cur != "" {
		return cur, nil
	}
	return s.OpenSyncInstance(ctx, sc, "", "")
}

// ReconcileStaleUsers returns the ids of active/scim-owned users whose
// sync_instance_id differs from the given current instance — i.e. users that
// were last touched by a prior (pre-reconnect) sync and have not yet been seen
// by the current instance. The caller decides what "reconcile" means (audit,
// re-touch, or leave); this helper only identifies the set, scoped to the
// connection so it can never cross a workspace boundary.
func (s *DirectoryService) ReconcileStaleUsers(ctx context.Context, sc *scope, currentInst string) ([]string, error) {
	if currentInst == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT u.id::text
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.tenant_id = $1 AND ei.connection_id = $2
		    AND u.provisioning_owner = 'scim'
		    AND COALESCE(u.sync_instance_id::text, '') <> $3
		    AND u.status <> 'deleted'`,
		sc.workspaceID, sc.connectionID, currentInst,
	)
	if err != nil {
		return nil, fmt.Errorf("reconcile stale users: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// ReconcileStaleGroups returns the ids of scim-origin groups whose
// sync_instance_id differs from the current instance.
func (s *DirectoryService) ReconcileStaleGroups(ctx context.Context, sc *scope, currentInst string) ([]string, error) {
	if currentInst == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM groups
		  WHERE tenant_id = $1 AND connection_id = $2 AND origin = 'scim'
		    AND COALESCE(sync_instance_id::text, '') <> $3`,
		sc.workspaceID, sc.connectionID, currentInst,
	)
	if err != nil {
		return nil, fmt.Errorf("reconcile stale groups: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}
