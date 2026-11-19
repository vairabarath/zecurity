// Package tenantexport builds and delivers a tenant's own logs (Sprint 23 S4,
// D-46, export design of 2026-11-18):
//
//   - the archive: the admin audit log (audit_logs), the access log
//     (connector_logs) and certificate and revocation history, as JSON-lines
//     files in one gzip'd tar with a manifest (row counts, SHA-256 per file);
//   - download tokens for the 30-day grace (an emailed link, export-only
//     sign-in): only a token's SHA-256 is stored, and a token is never
//     logged, audited or returned by a provider API;
//   - the tenant's download endpoint.
//
// The archive is the tenant's own data. The provider console never reads it:
// it shows deletion status only.
package tenantexport

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier reads tenant rows (a pool or a transaction).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// part is one file of the archive: the tenant's rows of one table.
type part struct {
	File  string
	Query string // one row_to_json text column; $1 = workspace id
}

// parts are the archive's contents (D-46: audit log and security records;
// the access log too, by the 2026-11-18 decision).
var parts = []part{
	{"audit_log.jsonl", `SELECT row_to_json(t)::text FROM audit_logs t WHERE tenant_id = $1 ORDER BY seq NULLS FIRST, created_at, id`},
	{"access_log.jsonl", `SELECT row_to_json(t)::text FROM connector_logs t WHERE workspace_id = $1 ORDER BY seq NULLS FIRST, created_at, id`},
	{"connector_certificates.jsonl", `SELECT row_to_json(t)::text FROM connector_certificates t WHERE tenant_id = $1 ORDER BY issued_at, id`},
	{"client_device_certificates.jsonl", `SELECT row_to_json(t)::text FROM client_device_certificates t WHERE tenant_id = $1 ORDER BY issued_at, id`},
	{"connector_revocations.jsonl", `SELECT row_to_json(t)::text FROM connector_revocations t WHERE tenant_id = $1 ORDER BY revoked_at, connector_id`},
	{"shield_revocations.jsonl", `SELECT row_to_json(t)::text FROM shield_revocations t WHERE tenant_id = $1 ORDER BY revoked_at, shield_id`},
}

// ManifestFile is one file in the manifest.
type ManifestFile struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

// Manifest describes an archive (manifest.json, the last tar entry).
type Manifest struct {
	Format      string         `json:"format"`
	WorkspaceID string         `json:"workspace_id"`
	Slug        string         `json:"slug"`
	CreatedAt   time.Time      `json:"created_at"`
	Files       []ManifestFile `json:"files"`
}

// Write streams the tenant's archive to w and returns its manifest. q should
// read one consistent snapshot (a REPEATABLE READ transaction).
func Write(ctx context.Context, q Querier, workspaceID, slug string, w io.Writer) (Manifest, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()
	m := Manifest{Format: "zecurity-tenant-export/1", WorkspaceID: workspaceID, Slug: slug, CreatedAt: now}

	for _, p := range parts {
		// Each file is staged in a temp file: tar needs its size up front.
		tmp, err := os.CreateTemp("", "zecurity-tenant-part-*")
		if err != nil {
			return Manifest{}, fmt.Errorf("tenantexport: temp file: %w", err)
		}
		mf, err := writePart(ctx, q, p, workspaceID, tmp)
		if err == nil {
			err = addFile(tw, p.File, tmp, now)
		}
		tmp.Close()
		os.Remove(tmp.Name())
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, mf)
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb)), ModTime: now}); err != nil {
		return Manifest{}, err
	}
	if _, err := tw.Write(mb); err != nil {
		return Manifest{}, err
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, err
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func writePart(ctx context.Context, q Querier, p part, workspaceID string, f *os.File) (ManifestFile, error) {
	rows, err := q.Query(ctx, p.Query, workspaceID)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("tenantexport: read %s: %w", p.File, err)
	}
	defer rows.Close()
	h := sha256.New()
	out := io.MultiWriter(f, h)
	var n int64
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return ManifestFile{}, fmt.Errorf("tenantexport: read %s: %w", p.File, err)
		}
		if _, err := io.WriteString(out, line+"\n"); err != nil {
			return ManifestFile{}, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return ManifestFile{}, fmt.Errorf("tenantexport: read %s: %w", p.File, err)
	}
	return ManifestFile{Name: p.File, Rows: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func addFile(tw *tar.Writer, name string, f *os.File, mod time.Time) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: mod}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// WriteFile writes the archive to a new temp file and returns its path, the
// archive's SHA-256 and the manifest. The caller removes the file.
func WriteFile(ctx context.Context, q Querier, workspaceID, slug string) (path, sha string, m Manifest, err error) {
	f, err := os.CreateTemp("", "zecurity-tenant-export-*.tar.gz")
	if err != nil {
		return "", "", Manifest{}, err
	}
	h := sha256.New()
	m, err = Write(ctx, q, workspaceID, slug, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", Manifest{}, err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), m, nil
}

// FileSHA256 returns the SHA-256 of a file's bytes as stored.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
