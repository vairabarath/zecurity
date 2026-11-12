package providerquery

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

// PKI hierarchy and CRL status (quick-win F15/F16, cert.read, super-admin).
//
// The tree is Root → Intermediate → workspace CAs. Each node's parent is the
// CA whose SubjectKeyId equals the node's AuthorityKeyId (the issuer's
// subject DN when a certificate carries no key identifiers), so the links
// are the real signing relationships, not assumed ones. A certificate that
// does not parse is still listed, with no subject, issuer, serial or parent.
//
// Each node carries the CRL that CA signs, as the CRL endpoints produce it:
//   - the Intermediate signs /relay.crl: revoked, unexpired relay
//     certificates (relay_certificates);
//   - each workspace CA signs /ca.crl?workspace_id=…: revoked, unexpired
//     client device and connector certificates (039 history tables);
//   - the Root signs none.
//
// CRLs are signed on request and never stored, so "entries" is what a CRL
// fetched now would list, counted with the endpoints' own predicates; nothing
// is signed here. The relay CRL is attached only when exactly one
// intermediate exists: the PKI service loads one with LIMIT 1, and with
// several rows the signer cannot be named from the data.
//
// No key material is read: encrypted keys and nonces are never selected, and
// the public certificate_pem is parsed here and never returned.

// PKIValidity is how long a freshly signed CRL is valid. The caller passes
// pki.RelayCRLValidity and pki.WorkspaceCRLValidity (this package must not
// depend on pki, which pulls in net/http).
type PKIValidity struct {
	Relay     time.Duration
	Workspace time.Duration
}

// PKIFilter pages the workspace CAs. Roots and intermediates are always
// complete.
type PKIFilter struct {
	Validity PKIValidity
	Limit    int
	Cursor   string
}

// CRLStatus is the CRL a CA signs.
type CRLStatus struct {
	URL             string   `json:"url"`
	Covers          []string `json:"covers"`
	Entries         int64    `json:"entries"`
	ValidForSeconds int64    `json:"valid_for_seconds"`
}

// CANode is one CA in the hierarchy.
type CANode struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"` // root | intermediate | workspace_ca
	Name      string     `json:"name"`
	TenantID  *string    `json:"tenant_id"`
	Subject   *string    `json:"subject"`
	Issuer    *string    `json:"issuer"`
	Serial    *string    `json:"serial"`
	NotBefore *time.Time `json:"not_before"`
	NotAfter  time.Time  `json:"not_after"`
	ParentID  *string    `json:"parent_id"`
	CRL       *CRLStatus `json:"crl"`
}

// PKIHierarchy is GET /provider/pki. WorkspaceCAs is one page (newest
// first); WorkspaceCAsTotal and NextCursor make a partial page explicit.
type PKIHierarchy struct {
	Roots             []CANode `json:"roots"`
	Intermediates     []CANode `json:"intermediates"`
	WorkspaceCAs      []CANode `json:"workspace_cas"`
	WorkspaceCAsTotal int64    `json:"workspace_cas_total"`
	NextCursor        *string  `json:"next_cursor"`
}

// parsedCA holds what the tree needs from a certificate.
type parsedCA struct {
	subject, issuer, serial string
	ski, aki                []byte
	notBefore               time.Time
}

func parseCAPEM(s string) (parsedCA, bool) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return parsedCA{}, false
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return parsedCA{}, false
	}
	return parsedCA{
		subject: c.Subject.String(), issuer: c.Issuer.String(), serial: c.SerialNumber.Text(16),
		ski: c.SubjectKeyId, aki: c.AuthorityKeyId, notBefore: c.NotBefore.UTC(),
	}, true
}

func nodeFrom(id, kind, name string, tenantID *string, notAfter time.Time, p parsedCA, ok bool) CANode {
	n := CANode{ID: id, Kind: kind, Name: name, TenantID: tenantID, NotAfter: notAfter.UTC()}
	if ok {
		subject, issuer, serial, nb := p.subject, p.issuer, p.serial, p.notBefore
		n.Subject, n.Issuer, n.Serial, n.NotBefore = &subject, &issuer, &serial, &nb
	}
	return n
}

// issuerOf returns the id of the CA that signed p, among candidates: by key
// identifier when p has one, else by subject DN. Never p itself.
func issuerOf(selfID string, p parsedCA, candidates []caKey) *string {
	for _, c := range candidates {
		if c.id == selfID {
			continue
		}
		if len(p.aki) > 0 && len(c.ski) > 0 {
			if bytes.Equal(p.aki, c.ski) {
				id := c.id
				return &id
			}
			continue
		}
		if p.issuer == c.subject {
			id := c.id
			return &id
		}
	}
	return nil
}

type caKey struct {
	id      string
	subject string
	ski     []byte
}

// PKI returns the CA hierarchy with CRL status. Five statements whatever the
// page size: the platform CAs, the relay CRL count, the workspace CA total,
// the page, and the page's workspace CRL counts.
func PKI(ctx context.Context, q Querier, f PKIFilter) (PKIHierarchy, error) {
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return PKIHierarchy{}, err
	}
	limit := ClampLimit(f.Limit)
	res := PKIHierarchy{Roots: []CANode{}, Intermediates: []CANode{}, WorkspaceCAs: []CANode{}}

	// 1. Platform CAs.
	rows, err := q.Query(ctx, `
SELECT 'root', id::text, certificate_pem, not_after FROM ca_root
UNION ALL
SELECT 'intermediate', id::text, certificate_pem, not_after FROM ca_intermediate
ORDER BY 1 DESC, 2`)
	if err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki platform CAs: %w", err)
	}
	var rootKeys, interKeys []caKey
	var rootParsed, interParsed []parsedCA
	for rows.Next() {
		var kind, id, certPEM string
		var notAfter time.Time
		if err := rows.Scan(&kind, &id, &certPEM, &notAfter); err != nil {
			rows.Close()
			return PKIHierarchy{}, fmt.Errorf("scan platform CA: %w", err)
		}
		p, ok := parseCAPEM(certPEM)
		if kind == "root" {
			res.Roots = append(res.Roots, nodeFrom(id, kind, "Root CA", nil, notAfter, p, ok))
			rootParsed = append(rootParsed, p)
			if ok {
				rootKeys = append(rootKeys, caKey{id: id, subject: p.subject, ski: p.ski})
			}
		} else {
			res.Intermediates = append(res.Intermediates, nodeFrom(id, kind, "Intermediate CA", nil, notAfter, p, ok))
			interParsed = append(interParsed, p)
			if ok {
				interKeys = append(interKeys, caKey{id: id, subject: p.subject, ski: p.ski})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki platform CAs: %w", err)
	}
	for i := range res.Roots {
		if res.Roots[i].Subject != nil {
			res.Roots[i].ParentID = issuerOf(res.Roots[i].ID, rootParsed[i], rootKeys)
		}
	}
	for i := range res.Intermediates {
		if res.Intermediates[i].Subject != nil {
			res.Intermediates[i].ParentID = issuerOf(res.Intermediates[i].ID, interParsed[i], rootKeys)
		}
	}

	// 2. The relay CRL, signed by the (single) intermediate.
	var relayEntries int64
	if err := q.QueryRow(ctx, `
SELECT count(*) FROM relay_certificates WHERE revoked_at IS NOT NULL AND not_after > now()`).Scan(&relayEntries); err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki relay CRL: %w", err)
	}
	if len(res.Intermediates) == 1 {
		res.Intermediates[0].CRL = &CRLStatus{
			URL: "/relay.crl", Covers: []string{"relays"}, Entries: relayEntries,
			ValidForSeconds: int64(f.Validity.Relay / time.Second),
		}
	}

	// 3. Workspace CAs: total, then one page (deleted workspaces excluded,
	// as in the certificate inventory).
	if err := q.QueryRow(ctx, `
SELECT count(*) FROM workspace_ca_keys k JOIN workspaces w ON w.id = k.tenant_id WHERE w.status <> 'deleted'`).Scan(&res.WorkspaceCAsTotal); err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki workspace CA total: %w", err)
	}
	ct, cid := keysetArgs(cur)
	rows, err = q.Query(ctx, `
SELECT k.id::text, w.id::text, w.name, k.certificate_pem, k.not_after, k.created_at
  FROM workspace_ca_keys k JOIN workspaces w ON w.id = k.tenant_id
 WHERE w.status <> 'deleted'
   AND ($1::timestamptz IS NULL OR (k.created_at, k.id) < ($1, $2::uuid))
 ORDER BY k.created_at DESC, k.id DESC
 LIMIT $3`, ct, cid, limit+1)
	if err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki workspace CAs: %w", err)
	}
	var createdAt []time.Time
	tenants := []string{}
	for rows.Next() {
		var id, tenant, name, certPEM string
		var notAfter, created time.Time
		if err := rows.Scan(&id, &tenant, &name, &certPEM, &notAfter, &created); err != nil {
			rows.Close()
			return PKIHierarchy{}, fmt.Errorf("scan workspace CA: %w", err)
		}
		p, ok := parseCAPEM(certPEM)
		tid := tenant
		n := nodeFrom(id, "workspace_ca", name, &tid, notAfter, p, ok)
		if ok {
			n.ParentID = issuerOf(id, p, interKeys)
		}
		res.WorkspaceCAs = append(res.WorkspaceCAs, n)
		createdAt = append(createdAt, created.UTC())
		tenants = append(tenants, tenant)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki workspace CAs: %w", err)
	}
	if len(res.WorkspaceCAs) > limit {
		res.WorkspaceCAs, tenants = res.WorkspaceCAs[:limit], tenants[:limit]
		last := res.WorkspaceCAs[limit-1]
		next := EncodeCursor(createdAt[limit-1], last.ID)
		res.NextCursor = &next
	}

	// 4. The page's workspace CRL entry counts, in one statement.
	counts := map[string]int64{}
	rows, err = q.Query(ctx, `
SELECT tenant_id::text, count(*) FROM (
    SELECT tenant_id FROM client_device_certificates
     WHERE tenant_id = ANY($1::uuid[]) AND revoked_at IS NOT NULL AND not_after > now()
    UNION ALL
    SELECT tenant_id FROM connector_certificates
     WHERE tenant_id = ANY($1::uuid[]) AND revoked_at IS NOT NULL AND not_after > now()
) c GROUP BY tenant_id`, tenants)
	if err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki workspace CRLs: %w", err)
	}
	for rows.Next() {
		var tenant string
		var n int64
		if err := rows.Scan(&tenant, &n); err != nil {
			rows.Close()
			return PKIHierarchy{}, fmt.Errorf("scan workspace CRL count: %w", err)
		}
		counts[tenant] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PKIHierarchy{}, fmt.Errorf("pki workspace CRLs: %w", err)
	}
	for i := range res.WorkspaceCAs {
		tenant := *res.WorkspaceCAs[i].TenantID
		res.WorkspaceCAs[i].CRL = &CRLStatus{
			URL: "/ca.crl?workspace_id=" + tenant, Covers: []string{"client_devices", "connectors"},
			Entries: counts[tenant], ValidForSeconds: int64(f.Validity.Workspace / time.Second),
		}
	}
	return res, nil
}
