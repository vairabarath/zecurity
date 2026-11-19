package tenantlifecycle

// The tenant purge job (Sprint 23 S4, D-46; D-09, D-13; export design of
// 2026-11-18). Single controller (D-02): one loop, one tenant at a time.
//
// A tenant is due when it is suspended, its grace (delete_after) has passed
// and it is not on legal hold. Its purge is one tenant_purges row that moves
// pending → exported → revoked → destroyed → done; every step is idempotent
// and records its completion, so a purge interrupted at any point (crash,
// restart, error) resumes at the next pass from the last completed step:
//
//  1. exported: the tenant's own logs (tenantexport archive) are written to
//     the tenant's S3 audit-export sink, or to ExportDir when it has none,
//     read back and their SHA-256 verified. The object name is fixed per
//     scheduled deletion, so a re-run overwrites it (no double export).
//  2. revoked: every remaining connector, shield and client device and their
//     certificates are revoked (tombstones written), every session ended,
//     download links dropped, relay pool policies bumped.
//  3. destroyed: tenant_purge(ws), the privileged path (D-13), deletes the
//     workspace and everything tenant-scoped (cascade), including its
//     audit_logs, after re-checking the verified export, the grace and the
//     legal hold. provider_audit_logs is never touched (D-12).
//  4. done: the tombstone and a system-actor provider audit row.
//
// A legal hold set during a purge pauses it before the next step (and
// tenant_purge refuses while held); clearing it lets the purge continue.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/auditexport"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay"
	"github.com/yourorg/ztna/controller/internal/tenantexport"
)

// SystemPurgeActor is the provider audit actor of a completed purge.
const SystemPurgeActor = "system:tenant-purge"

// DefaultPurgeInterval is how often the job looks for due tenants.
const DefaultPurgeInterval = 5 * time.Minute

// ErrNoExportDestination: the tenant has no S3 sink and no ExportDir is set;
// nothing is destroyed until one is available.
var ErrNoExportDestination = errors.New("tenant purge: no export destination (no tenant S3 sink and TENANT_PURGE_EXPORT_DIR unset)")

// ArchiveTarget is a tenant's S3 bucket for the archive
// (*auditexport.S3Archive).
type ArchiveTarget interface {
	Key(workspaceID, name string) string
	Put(ctx context.Context, c *http.Client, key, path, sha256hex string) error
	SHA256(ctx context.Context, c *http.Client, key string) (string, error)
	BucketName() string
}

// S3Archives opens a tenant's S3 sink for the archive; it returns
// auditexport.ErrNoS3Sink when the tenant has none.
type S3Archives interface {
	TenantS3Archive(ctx context.Context, workspaceID string) (ArchiveTarget, error)
}

// AuditExportArchives adapts *auditexport.Store to S3Archives.
type AuditExportArchives struct{ Store *auditexport.Store }

// TenantS3Archive implements S3Archives.
func (a AuditExportArchives) TenantS3Archive(ctx context.Context, workspaceID string) (ArchiveTarget, error) {
	t, err := a.Store.TenantS3Archive(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// copyFile writes the local export (a variable so a test can corrupt it).
var copyFile = copyFileSync

// Purger runs tenant purges.
type Purger struct {
	Pool          *pgxpool.Pool
	ProviderStore *provider.Store
	S3            S3Archives   // optional: tenant S3 sinks
	HTTP          *http.Client // for S3
	ExportDir     string       // local export directory (fallback)
	Sessions      SessionInvalidator
	Policy        PolicyNotifier
	// PublishRelayLists rebuilds and broadcasts the relay lists. Optional.
	PublishRelayLists func(ctx context.Context) error
	Interval          time.Duration

	// hook, if set (tests), runs after each committed step; an error stops
	// the purge there, as a crash would.
	hook func(workspaceID, step string) error
}

// Run purges due tenants every Interval until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	iv := p.Interval
	if iv <= 0 {
		iv = DefaultPurgeInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := p.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("tenant purge: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce purges every due tenant and resumes every unfinished purge. One
// tenant's failure doesn't stop the others; the first error is returned.
func (p *Purger) RunOnce(ctx context.Context) error {
	rows, err := p.Pool.Query(ctx, `
		SELECT id::text FROM workspaces
		 WHERE suspended_at IS NOT NULL AND delete_after IS NOT NULL AND delete_after <= NOW() AND NOT legal_hold
		UNION
		SELECT workspace_id::text FROM tenant_purges WHERE step <> 'done'`)
	if err != nil {
		return fmt.Errorf("list due tenants: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list due tenants: %w", err)
	}
	var first error
	for _, id := range ids {
		if err := p.Purge(ctx, id); err != nil {
			log.Printf("tenant purge %s: %v", id, err)
			p.recordError(ctx, id, err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (p *Purger) recordError(ctx context.Context, workspaceID string, err error) {
	if _, e := p.Pool.Exec(context.WithoutCancel(ctx), `SELECT tenant_purge_record_error($1, $2)`,
		workspaceID, err.Error()); e != nil {
		log.Printf("tenant purge %s: record error: %v", workspaceID, e)
	}
}

type purgeRow struct {
	slug, step, by, reason           string
	scheduledAt                      time.Time
	exportKind, exportLoc, exportSHA *string
}

// Purge advances one tenant's purge as far as it can.
func (p *Purger) Purge(ctx context.Context, workspaceID string) error {
	// Start (or find) the purge row; only a due tenant gets one. The
	// function locks the workspace row, so it serialises with CancelDelete
	// (which refuses once a purge row exists) and SetLegalHold.
	var started bool
	if err := p.Pool.QueryRow(ctx, `SELECT tenant_purge_start($1)`, workspaceID).Scan(&started); err != nil {
		return fmt.Errorf("start purge: %w", err)
	}
	if !started {
		return nil
	}
	for {
		var r purgeRow
		err := p.Pool.QueryRow(ctx, `SELECT slug, step, delete_by, delete_reason, scheduled_at,
			       export_kind, export_location, export_sha256
			  FROM tenant_purges WHERE workspace_id = $1`, workspaceID).
			Scan(&r.slug, &r.step, &r.by, &r.reason, &r.scheduledAt, &r.exportKind, &r.exportLoc, &r.exportSHA)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not due
		}
		if err != nil {
			return fmt.Errorf("read purge: %w", err)
		}
		if r.step == "done" {
			return nil
		}
		if r.step != "destroyed" {
			held, err := p.onHold(ctx, workspaceID)
			if err != nil {
				return err
			}
			if held {
				return nil // paused until the hold is cleared
			}
		}
		var next string
		switch r.step {
		case "pending":
			err, next = p.export(ctx, workspaceID, r), "exported"
		case "exported":
			err, next = p.revoke(ctx, workspaceID), "revoked"
		case "revoked":
			err, next = p.destroy(ctx, workspaceID), "destroyed"
		case "destroyed":
			err, next = p.finish(ctx, workspaceID, r), "done"
		default:
			return fmt.Errorf("unknown purge step %q", r.step)
		}
		if err != nil {
			return fmt.Errorf("step %s: %w", next, err)
		}
		if p.hook != nil {
			if err := p.hook(workspaceID, next); err != nil {
				return err
			}
		}
	}
}

func (p *Purger) onHold(ctx context.Context, workspaceID string) (bool, error) {
	var held bool
	err := p.Pool.QueryRow(ctx, `SELECT legal_hold FROM workspaces WHERE id = $1`, workspaceID).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return held, err
}

// export writes and verifies the tenant's archive, then records step
// 'exported'. The destination name is fixed per scheduled deletion.
func (p *Purger) export(ctx context.Context, workspaceID string, r purgeRow) error {
	var path, sha string
	err := pgx.BeginTxFunc(ctx, p.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		path, sha, _, err = tenantexport.WriteFile(ctx, tx, workspaceID, r.slug)
		return err
	})
	if err != nil {
		return fmt.Errorf("build archive: %w", err)
	}
	defer os.Remove(path)
	name := r.scheduledAt.UTC().Format("20060102T150405Z") + ".tar.gz"

	kind, loc, err := p.deliver(ctx, workspaceID, name, path, sha)
	if err != nil {
		return err
	}
	if _, err := p.Pool.Exec(ctx, `SELECT tenant_purge_record_export($1, $2, $3, $4)`, workspaceID, kind, loc, sha); err != nil {
		return fmt.Errorf("record export: %w", err)
	}
	return nil
}

// deliver writes the archive to the tenant's S3 sink, else to ExportDir, and
// verifies what is stored. It returns the kind and location.
func (p *Purger) deliver(ctx context.Context, workspaceID, name, path, sha string) (string, string, error) {
	if p.S3 != nil {
		a, err := p.S3.TenantS3Archive(ctx, workspaceID)
		switch {
		case err == nil:
			key := a.Key(workspaceID, name)
			if err := a.Put(ctx, p.HTTP, key, path, sha); err != nil {
				return "", "", err
			}
			got, err := a.SHA256(ctx, p.HTTP, key)
			if err != nil {
				return "", "", err
			}
			if got != sha {
				return "", "", fmt.Errorf("verify S3 export: stored SHA-256 %s, want %s", got, sha)
			}
			return "s3", "s3://" + a.BucketName() + "/" + key, nil
		case !errors.Is(err, auditexport.ErrNoS3Sink):
			return "", "", fmt.Errorf("open tenant S3 sink: %w", err)
		}
	}
	if p.ExportDir == "" {
		return "", "", ErrNoExportDestination
	}
	dir := filepath.Join(p.ExportDir, workspaceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("local export: %w", err)
	}
	dst := filepath.Join(dir, name)
	if err := copyFile(path, dst); err != nil {
		return "", "", fmt.Errorf("local export: %w", err)
	}
	got, err := tenantexport.FileSHA256(dst)
	if err != nil {
		return "", "", fmt.Errorf("verify local export: %w", err)
	}
	if got != sha {
		return "", "", fmt.Errorf("verify local export: stored SHA-256 %s, want %s", got, sha)
	}
	return "local", dst, nil
}

// copyFileSync copies src to dst atomically (temp file, fsync, rename).
func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// revoke revokes everything that can authenticate as the tenant, then
// records step 'revoked'. Sessions are ended after commit.
func (p *Purger) revoke(ctx context.Context, workspaceID string) error {
	var userIDs []string
	err := pgx.BeginTxFunc(ctx, p.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Workspace row first: the same lock order as CancelDelete and
		// SetLegalHold; tenant_purge_mark_revoked re-checks "due" under it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID); err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		stmts := []string{
			// connectors_record_revocation (041) writes the tombstones.
			`UPDATE connectors SET status = 'revoked', revoked_at = COALESCE(revoked_at, NOW()), updated_at = NOW()
			  WHERE tenant_id = $1 AND (status <> 'revoked' OR revoked_at IS NULL)`,
			`INSERT INTO shield_revocations (shield_id, tenant_id)
			 SELECT id, tenant_id FROM shields WHERE tenant_id = $1
			 ON CONFLICT (shield_id) DO NOTHING`,
			`INSERT INTO shield_revocation_outbox (tenant_id, shield_id)
			 SELECT tenant_id, id FROM shields WHERE tenant_id = $1 AND status <> 'revoked'`,
			`UPDATE shields SET status = 'revoked', updated_at = NOW() WHERE tenant_id = $1 AND status <> 'revoked'`,
			`UPDATE client_devices SET revoked_at = COALESCE(revoked_at, NOW()) WHERE workspace_id = $1 AND revoked_at IS NULL`,
			`UPDATE connector_certificates SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`,
			`UPDATE client_device_certificates SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`,
			`DELETE FROM tenant_export_tokens WHERE workspace_id = $1`,
		}
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q, workspaceID); err != nil {
				return fmt.Errorf("revoke: %w", err)
			}
		}
		rows, err := tx.Query(ctx, `UPDATE users SET identity_generation = identity_generation + 1, updated_at = NOW()
			 WHERE tenant_id = $1 RETURNING id::text`, workspaceID)
		if err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if userIDs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if err := relay.BumpWorkspacePoolPoliciesTx(ctx, tx, workspaceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT tenant_purge_mark_revoked($1)`, workspaceID); err != nil {
			return fmt.Errorf("record revoke: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fanoutTimeout)
	defer cancel()
	if p.Sessions != nil {
		for _, id := range userIDs {
			if err := p.Sessions.InvalidateUserSessions(fctx, id); err != nil {
				log.Printf("tenant purge %s: end sessions of user %s: %v", workspaceID, id, err)
			}
		}
	}
	if p.Policy != nil {
		if err := p.Policy.NotifyPolicyChange(fctx, workspaceID); err != nil {
			log.Printf("tenant purge %s: push ACL: %v", workspaceID, err)
		}
	}
	return nil
}

// destroy runs the privileged purge (D-13) and records step 'destroyed'.
func (p *Purger) destroy(ctx context.Context, workspaceID string) error {
	// tenant_purge (D-13) deletes the workspace and records 'destroyed'.
	if _, err := p.Pool.Exec(ctx, `SELECT tenant_purge($1)`, workspaceID); err != nil {
		return fmt.Errorf("tenant_purge: %w", err)
	}
	if p.PublishRelayLists != nil {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fanoutTimeout)
		defer cancel()
		if err := p.PublishRelayLists(fctx); err != nil {
			log.Printf("tenant purge %s: publish relay lists: %v", workspaceID, err)
		}
	}
	return nil
}

// finish writes the tombstone and the system provider audit row, then
// records step 'done'.
func (p *Purger) finish(ctx context.Context, workspaceID string, r purgeRow) error {
	if r.exportKind == nil || r.exportLoc == nil || r.exportSHA == nil {
		return errors.New("finish: purge row has no export")
	}
	return pgx.BeginTxFunc(ctx, p.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var written bool
		if err := tx.QueryRow(ctx, `SELECT tenant_purge_finish($1)`, workspaceID).Scan(&written); err != nil {
			return fmt.Errorf("tombstone: %w", err)
		}
		if written {
			if err := p.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderEmail: SystemPurgeActor,
				Action:        provider.ActionTenantPurged,
				TargetType:    "tenant",
				TargetID:      workspaceID,
				Details: map[string]any{
					"slug": r.slug, "scheduled_by": r.by, "reason": r.reason,
					"scheduled_at": r.scheduledAt.UTC(),
					"export":       map[string]any{"kind": *r.exportKind, "location": *r.exportLoc, "sha256": *r.exportSHA},
				},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
