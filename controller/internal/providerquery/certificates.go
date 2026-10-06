package providerquery

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// Certificate expiry (Phase R, cert.read, super-admin only).
//
// Two kinds of source, kept apart:
//   - Persisted inventory: ca_root, ca_intermediate, workspace_ca_keys,
//     relays, connectors, shields and client_devices rows. Revoked or
//     deleted entities (and everything in a deleted workspace) are excluded.
//   - Process-local: the controller's own gRPC certificate, which exists only
//     in memory (pki.ControllerCertRotator). It is read through
//     ControllerCertSource at query time and handed to SQL as plain
//     parameters so the one row sorts and paginates with the rest. Nothing
//     about it is stored.
//
// No key material is ever selected (no encrypted_key, encrypted_private_key
// or nonce). For root, intermediate and workspace CA rows the public
// certificate_pem is read only to parse its serial; the PEM never leaves
// this file. Client device names are user-chosen and may identify a person,
// so client_device rows carry no entity_name (OQ-2).

// CertificateKinds is the closed set of kinds, in the order they sort.
var CertificateKinds = []string{
	"client_device", "connector", "controller_grpc", "intermediate", "relay", "root", "shield", "workspace_ca",
}

const (
	DefaultCertWithin = 720 * time.Hour
	MaxCertWithin     = 8760 * time.Hour

	// controllerEntityID is the entity_id of the single controller_grpc row
	// (single controller, D-02).
	controllerEntityID = "controller"
)

// ControllerCertSource reports the certificate this controller process is
// serving. *pki.ControllerCertRotator satisfies it.
type ControllerCertSource interface {
	CurrentCertInfo() (serial string, notBefore, notAfter time.Time, ok bool)
}

// Expiry buckets, from the request's "now". Boundaries: expired means
// not_after <= now; lt_24h means now < not_after <= now+24h; and so on; ok is
// beyond 30 days.
const (
	BucketExpired = "expired"
	BucketLT24h   = "lt_24h"
	BucketLT7d    = "lt_7d"
	BucketLT30d   = "lt_30d"
	BucketOK      = "ok"
)

// CertificateEntry is one certificate in GET /provider/certificates.
type CertificateEntry struct {
	Kind       string    `json:"kind"`
	TenantID   *string   `json:"tenant_id"`
	EntityID   string    `json:"entity_id"`
	EntityName *string   `json:"entity_name"`
	Serial     *string   `json:"serial"`
	NotAfter   time.Time `json:"not_after"`
	Bucket     string    `json:"bucket"`
}

// CertificateSummary counts every certificate matching kind/tenant_id per
// bucket. It ignores `within`.
type CertificateSummary struct {
	Expired int64 `json:"expired"`
	LT24h   int64 `json:"lt_24h"`
	LT7d    int64 `json:"lt_7d"`
	LT30d   int64 `json:"lt_30d"`
	OK      int64 `json:"ok"`
}

// CertificateExpiryResult is GET /provider/certificates.
type CertificateExpiryResult struct {
	Summary    CertificateSummary `json:"summary"`
	Items      []CertificateEntry `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

// CertificateFilter narrows CertificateExpiry. Within 0 means
// DefaultCertWithin; Items include certificates with not_after <= Now+Within
// (already-expired ones always). Now zero means time.Now().
type CertificateFilter struct {
	Within   time.Duration
	Kind     string
	TenantID string
	Limit    int
	Cursor   string
	Now      time.Time
}

type certCursor struct {
	T    time.Time
	Kind string
	ID   string
}

type certCursorWire struct {
	T  string `json:"t"`
	K  string `json:"k"`
	ID string `json:"id"`
}

func encodeCertCursor(e CertificateEntry) string {
	b, _ := json.Marshal(certCursorWire{T: e.NotAfter.UTC().Format(time.RFC3339Nano), K: e.Kind, ID: e.EntityID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCertCursor(s string) (*certCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding", ErrInvalidCursor)
	}
	var w certCursorWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: payload", ErrInvalidCursor)
	}
	t, err := time.Parse(time.RFC3339Nano, w.T)
	if err != nil || !validCertKind(w.K) || !(IsUUID(w.ID) || w.ID == controllerEntityID) {
		return nil, fmt.Errorf("%w: fields", ErrInvalidCursor)
	}
	return &certCursor{T: t, Kind: w.K, ID: w.ID}, nil
}

func validCertKind(k string) bool {
	for _, v := range CertificateKinds {
		if k == v {
			return true
		}
	}
	return false
}

// certsCTE is the normalized certificate inventory. $1/$2 carry the
// process-local controller certificate (both NULL when unavailable); the
// remaining branches read persisted rows only.
const certsCTE = `
WITH certs AS (
    SELECT 'root'::text AS kind, NULL::text AS tenant_id, r.id::text AS entity_id,
           'Root CA'::text AS entity_name, NULL::text AS serial, r.not_after, r.certificate_pem AS pem
      FROM ca_root r
    UNION ALL
    SELECT 'intermediate', NULL, i.id::text, 'Intermediate CA', NULL, i.not_after, i.certificate_pem
      FROM ca_intermediate i
    UNION ALL
    SELECT 'workspace_ca', w.id::text, k.id::text, w.name, NULL, k.not_after, k.certificate_pem
      FROM workspace_ca_keys k JOIN workspaces w ON w.id = k.tenant_id
     WHERE w.status <> 'deleted'
    UNION ALL
    SELECT 'controller_grpc', NULL, 'controller', 'Controller gRPC', $1::text, $2::timestamptz, NULL
     WHERE $2::timestamptz IS NOT NULL
    UNION ALL
    SELECT 'relay', NULL, r.id::text, r.name, r.cert_serial, r.cert_not_after, NULL
      FROM relays r
     WHERE r.status NOT IN ('revoked', 'deleted') AND r.cert_not_after IS NOT NULL
    UNION ALL
    SELECT 'connector', c.tenant_id::text, c.id::text, c.name, c.cert_serial, c.cert_not_after, NULL
      FROM connectors c JOIN workspaces w ON w.id = c.tenant_id
     WHERE c.revoked_at IS NULL AND c.status <> 'revoked' AND c.cert_not_after IS NOT NULL AND w.status <> 'deleted'
    UNION ALL
    SELECT 'shield', s.tenant_id::text, s.id::text, s.name, s.cert_serial, s.cert_not_after, NULL
      FROM shields s JOIN workspaces w ON w.id = s.tenant_id
     WHERE s.status <> 'revoked' AND s.cert_not_after IS NOT NULL AND w.status <> 'deleted'
    UNION ALL
    SELECT 'client_device', d.workspace_id::text, d.id::text, NULL, d.cert_serial, d.cert_not_after, NULL
      FROM client_devices d JOIN workspaces w ON w.id = d.workspace_id
     WHERE d.revoked_at IS NULL AND d.cert_not_after IS NOT NULL AND w.status <> 'deleted'
)`

// certFilterSQL applies the kind ($3) and tenant ($4) filters.
const certFilterSQL = `($3::text IS NULL OR kind = $3) AND ($4::text IS NULL OR tenant_id = $4)`

// CertificateExpiry returns the expiry summary (ignores Within) and one page
// of certificates expiring within the window, soonest first by
// (not_after, kind, entity_id). Two statements.
func CertificateExpiry(ctx context.Context, q Querier, ctl ControllerCertSource, f CertificateFilter) (CertificateExpiryResult, error) {
	within := f.Within
	if within == 0 {
		within = DefaultCertWithin
	}
	if within < 0 || within > MaxCertWithin {
		return CertificateExpiryResult{}, fmt.Errorf("%w: within must be in (0, %s]", ErrInvalidFilter, MaxCertWithin)
	}
	if f.Kind != "" && !validCertKind(f.Kind) {
		return CertificateExpiryResult{}, fmt.Errorf("%w: kind %q", ErrInvalidFilter, f.Kind)
	}
	if f.TenantID != "" && !IsUUID(f.TenantID) {
		return CertificateExpiryResult{}, ErrInvalidID
	}
	cur, err := decodeCertCursor(f.Cursor)
	if err != nil {
		return CertificateExpiryResult{}, err
	}
	now := f.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	limit := ClampLimit(f.Limit)

	var ctlSerial, ctlNotAfter any
	if ctl != nil {
		if serial, _, notAfter, ok := ctl.CurrentCertInfo(); ok {
			ctlSerial, ctlNotAfter = serial, notAfter
		}
	}
	var kind, tenant any
	if f.Kind != "" {
		kind = f.Kind
	}
	if f.TenantID != "" {
		tenant = f.TenantID
	}

	var res CertificateExpiryResult
	s := &res.Summary
	if err := q.QueryRow(ctx, certsCTE+`
SELECT count(*) FILTER (WHERE not_after <= $5),
       count(*) FILTER (WHERE not_after >  $5 AND not_after <= $5 + interval '24 hours'),
       count(*) FILTER (WHERE not_after >  $5 + interval '24 hours' AND not_after <= $5 + interval '7 days'),
       count(*) FILTER (WHERE not_after >  $5 + interval '7 days'   AND not_after <= $5 + interval '30 days'),
       count(*) FILTER (WHERE not_after >  $5 + interval '30 days')
  FROM certs
 WHERE `+certFilterSQL, ctlSerial, ctlNotAfter, kind, tenant, now).Scan(&s.Expired, &s.LT24h, &s.LT7d, &s.LT30d, &s.OK); err != nil {
		return CertificateExpiryResult{}, fmt.Errorf("certificate summary: %w", err)
	}

	var ct, ck, cid any
	if cur != nil {
		ct, ck, cid = cur.T, cur.Kind, cur.ID
	}
	rows, err := q.Query(ctx, certsCTE+`
SELECT kind, tenant_id, entity_id, entity_name, serial, not_after, pem
  FROM certs
 WHERE `+certFilterSQL+`
   AND not_after <= $5::timestamptz
   AND ($6::timestamptz IS NULL OR (not_after, kind, entity_id) > ($6, $7::text, $8::text))
 ORDER BY not_after, kind, entity_id
 LIMIT $9`, ctlSerial, ctlNotAfter, kind, tenant, now.Add(within), ct, ck, cid, limit+1)
	if err != nil {
		return CertificateExpiryResult{}, fmt.Errorf("certificate list: %w", err)
	}
	defer rows.Close()
	items := make([]CertificateEntry, 0, limit)
	for rows.Next() {
		var e CertificateEntry
		var pemText *string
		if err := rows.Scan(&e.Kind, &e.TenantID, &e.EntityID, &e.EntityName, &e.Serial, &e.NotAfter, &pemText); err != nil {
			return CertificateExpiryResult{}, fmt.Errorf("scan certificate: %w", err)
		}
		e.NotAfter = e.NotAfter.UTC()
		if pemText != nil {
			e.Serial = serialFromPEM(*pemText)
		}
		e.Bucket = bucketFor(e.NotAfter, now)
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return CertificateExpiryResult{}, fmt.Errorf("certificate list: %w", err)
	}
	res.Items = items
	if len(items) > limit {
		res.Items = items[:limit]
		next := encodeCertCursor(res.Items[limit-1])
		res.NextCursor = &next
	}
	return res, nil
}

// serialFromPEM extracts the serial (lowercase hex, the SerialNumber.Text(16)
// form used for relay certificates) from a public certificate PEM. A PEM that
// doesn't parse yields no serial rather than failing the whole response.
func serialFromPEM(s string) *string {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	serial := cert.SerialNumber.Text(16)
	return &serial
}

func bucketFor(notAfter, now time.Time) string {
	switch {
	case !notAfter.After(now):
		return BucketExpired
	case !notAfter.After(now.Add(24 * time.Hour)):
		return BucketLT24h
	case !notAfter.After(now.Add(7 * 24 * time.Hour)):
		return BucketLT7d
	case !notAfter.After(now.Add(30 * 24 * time.Hour)):
		return BucketLT30d
	default:
		return BucketOK
	}
}
