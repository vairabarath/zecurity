package providerquery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Provider audit reads (Phase R, audit.view, super-admin only). These are
// provider-operator records from provider_audit_logs (D-14); tenant audit
// rows live in audit_logs and are never read here. Reading the audit log is
// not itself audited.

// AuditFilterMaxLen bounds each string filter; longer values are rejected
// rather than sent to the database.
const AuditFilterMaxLen = 256

// AuditEntry is one row of GET /provider/audit: the whole allowed response
// surface, exactly the provider_audit_logs columns.
type AuditEntry struct {
	ID             string          `json:"id"`
	CreatedAt      time.Time       `json:"created_at"`
	ProviderUserID *string         `json:"provider_user_id"`
	ProviderEmail  string          `json:"provider_email"`
	Action         string          `json:"action"`
	TargetType     string          `json:"target_type"`
	TargetID       string          `json:"target_id"`
	Details        json.RawMessage `json:"details"`
	IPAddress      *string         `json:"ip_address"`
}

// AuditFilter narrows QueryProviderAudit. Empty strings and nil times mean
// "no filter". String filters are exact matches; ProviderEmail is matched
// case-insensitively (operator emails are stored lowercase). The time range
// is half-open: Since <= created_at < Until.
type AuditFilter struct {
	Action        string
	TargetType    string
	TargetID      string
	ProviderEmail string
	Since         *time.Time
	Until         *time.Time
	Limit         int
	Cursor        string
}

func (f AuditFilter) validate() error {
	for name, v := range map[string]string{
		"action": f.Action, "target_type": f.TargetType, "target_id": f.TargetID, "provider_email": f.ProviderEmail,
	} {
		if len(v) > AuditFilterMaxLen {
			return fmt.Errorf("%w: %s longer than %d", ErrInvalidFilter, name, AuditFilterMaxLen)
		}
	}
	if f.Since != nil && f.Until != nil && f.Since.After(*f.Until) {
		return fmt.Errorf("%w: since is after until", ErrInvalidFilter)
	}
	return nil
}

func optString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// QueryProviderAudit returns one page of provider audit rows, newest first by
// (created_at, id), in one statement.
func QueryProviderAudit(ctx context.Context, q Querier, f AuditFilter) (Page[AuditEntry], error) {
	if err := f.validate(); err != nil {
		return Page[AuditEntry]{}, err
	}
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return Page[AuditEntry]{}, err
	}
	limit := ClampLimit(f.Limit)
	ct, cid := keysetArgs(cur)

	rows, err := q.Query(ctx, `
SELECT id::text, created_at, provider_user_id::text, provider_email, action, target_type, target_id,
       details, ip_address
  FROM provider_audit_logs
 WHERE ($1::text IS NULL OR action = $1)
   AND ($2::text IS NULL OR target_type = $2)
   AND ($3::text IS NULL OR target_id = $3)
   AND ($4::text IS NULL OR lower(provider_email) = lower($4))
   AND ($5::timestamptz IS NULL OR created_at >= $5)
   AND ($6::timestamptz IS NULL OR created_at < $6)
   AND ($7::timestamptz IS NULL OR (created_at, id) < ($7, $8::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT $9`,
		optString(f.Action), optString(f.TargetType), optString(f.TargetID), optString(f.ProviderEmail),
		optTime(f.Since), optTime(f.Until), ct, cid, limit+1)
	if err != nil {
		return Page[AuditEntry]{}, fmt.Errorf("query provider audit: %w", err)
	}
	defer rows.Close()

	items := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var e AuditEntry
		var details []byte
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.ProviderUserID, &e.ProviderEmail, &e.Action, &e.TargetType,
			&e.TargetID, &details, &e.IPAddress); err != nil {
			return Page[AuditEntry]{}, fmt.Errorf("scan provider audit: %w", err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		if details != nil {
			e.Details = json.RawMessage(details)
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return Page[AuditEntry]{}, fmt.Errorf("query provider audit: %w", err)
	}
	page := Page[AuditEntry]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := EncodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &next
	}
	return page, nil
}
