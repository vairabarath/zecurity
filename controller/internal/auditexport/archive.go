package auditexport

// Tenant deletion export to the tenant's own S3 sink (Sprint 23 S4, D-46,
// export design of 2026-11-18): when a tenant is purged, the archive of its
// own logs goes to its configured S3 audit-export sink, if it has one. These
// helpers reuse the sink's sealed credentials and the SigV4 signer; they
// write one object and read it back for verification.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoS3Sink means the workspace has no enabled S3 sink.
var ErrNoS3Sink = errors.New("auditexport: no enabled S3 sink")

// S3Archive is where a tenant archive goes in the tenant's bucket.
type S3Archive struct {
	dest   Destination
	cfg    S3Config
	Bucket string
}

// TenantS3Archive loads the workspace's oldest enabled S3 sink (sealed
// credentials opened) for an archive upload, or ErrNoS3Sink.
func (s *Store) TenantS3Archive(ctx context.Context, workspaceID string) (*S3Archive, error) {
	var sinkID string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM audit_export_sinks
		  WHERE workspace_id = $1 AND kind = 's3' AND enabled
		  ORDER BY created_at, id LIMIT 1`, workspaceID).Scan(&sinkID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoS3Sink
		}
		return nil, fmt.Errorf("auditexport: find S3 sink: %w", err)
	}
	t, err := s.loadTarget(ctx, sinkID)
	if err != nil {
		return nil, err
	}
	cfg, err := S3{}.parse(t.Config)
	if err != nil {
		return nil, fmt.Errorf("auditexport: S3 sink config: %w", err)
	}
	if t.AuthHeaderName != S3SecretSlot || t.AuthHeaderValue == "" {
		return nil, errors.New("auditexport: S3 sink has no secret access key")
	}
	return &S3Archive{dest: t.Destination, cfg: cfg, Bucket: cfg.Bucket}, nil
}

// BucketName is the bucket the archive goes to.
func (a *S3Archive) BucketName() string { return a.Bucket }

// Key is the object key of a tenant's deletion archive:
// <prefix>/zecurity/v1/<workspace_id>/tenant-deletion/<name>.
func (a *S3Archive) Key(workspaceID, name string) string {
	k := "zecurity/v1/" + workspaceID + "/tenant-deletion/" + name
	if a.cfg.Prefix != "" {
		k = a.cfg.Prefix + "/" + k
	}
	return k
}

func (a *S3Archive) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(a.dest.URL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + key
	u.RawPath = uriEncode(u.Path, false)
	return u, nil
}

// Put uploads the file at path (whose SHA-256 is sha256hex) to key. Writing
// the same key again overwrites it (a resumed purge).
func (a *S3Archive) Put(ctx context.Context, c *http.Client, key, path, sha256hex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	u, err := a.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/gzip")
	signV4(req, sigV4Credentials{AccessKeyID: a.cfg.AccessKeyID, SecretAccessKey: a.dest.AuthHeaderValue},
		a.cfg.Region, "s3", sha256hex, time.Now())
	req.Header.Set("User-Agent", "zecurity-tenant-export/1")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("auditexport: upload archive: %s", describeTransportError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("auditexport: upload archive: HTTP %d", resp.StatusCode)
	}
	return nil
}

// SHA256 downloads key and returns the SHA-256 of what is stored.
func (a *S3Archive) SHA256(ctx context.Context, c *http.Client, key string) (string, error) {
	u, err := a.objectURL(key)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	signV4(req, sigV4Credentials{AccessKeyID: a.cfg.AccessKeyID, SecretAccessKey: a.dest.AuthHeaderValue},
		a.cfg.Region, "s3", sha256Hex(nil), time.Now())
	req.Header.Set("User-Agent", "zecurity-tenant-export/1")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("auditexport: read back archive: %s", describeTransportError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("auditexport: read back archive: HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", fmt.Errorf("auditexport: read back archive: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
