package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Usage reads (Sprint 25 U2, F33; D-57, D-59, D-60):
//
//	GET /provider/usage?tenant_id=&from=&to=&granularity=hour|day|month
//	  one tenant's usage per bucket with completeness; audited tenant.usage.read
//	GET /provider/usage/tenants?month=YYYY-MM&sort=<meter>&cursor=&limit=
//	  every tenant's usage for a month, largest first; audited usage.fleet.read
//
// tenant.usage.read: super-admin, auditor, billing (D-73; relay-ops and
// support 403). Both reads are
// audited and fail closed like tenant detail (OQ-1): read, serialize and
// audit in one REPEATABLE READ transaction; the first response byte is
// written only after COMMIT. A purged tenant's usage stays readable, named
// by its snapshot (D-58).

// GetTenantUsage: GET /provider/usage. from/to are RFC 3339 (default: the
// current UTC month so far, by day); they are widened to whole buckets.
func (h *ReadHandlers) GetTenantUsage(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadUsage(actor)) {
		return // 403: nothing read, nothing audited
	}
	q := r.URL.Query()
	tenantID := q.Get("tenant_id")
	if !providerquery.IsUUID(tenantID) {
		writeReadError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now
	for _, p := range []struct {
		key string
		dst *time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := q.Get(p.key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeReadError(w, http.StatusBadRequest, "invalid_range")
				return
			}
			*p.dst = t
		}
	}
	win, err := providerquery.NewUsageWindow(q.Get("granularity"), from, to)
	if !h.usageQueryOK(w, err) {
		return
	}
	h.auditedUsageRead(w, r, actor, AuditEntry{
		Action: ActionTenantUsageRead, TargetType: "tenant", TargetID: tenantID,
		Details: map[string]any{"view": "usage", "granularity": win.Granularity, "from": win.From, "to": win.To},
	}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		t, err := providerquery.ReadUsageTenant(ctx, tx, tenantID)
		if err != nil {
			return nil, err
		}
		return providerquery.ReadTenantUsage(ctx, tx, t, win)
	})
}

// ListFleetUsage: GET /provider/usage/tenants. month is YYYY-MM (default:
// the current UTC month); sort is a meter (default relay_bytes_to_clients).
func (h *ReadHandlers) ListFleetUsage(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadUsage(actor)) {
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if v := q.Get("month"); v != "" {
		m, err := time.Parse("2006-01", v)
		if err != nil {
			writeReadError(w, http.StatusBadRequest, "invalid_month")
			return
		}
		month = m.UTC()
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeReadError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = n
	}
	sortBy, cursor := q.Get("sort"), q.Get("cursor")
	// Validate sort and cursor before anything is read or audited.
	if _, err := providerquery.PageFleetUsage(providerquery.MonthUsage{}, sortBy, cursor, limit); !h.usageQueryOK(w, err) {
		return
	}
	h.auditedUsageRead(w, r, actor, AuditEntry{
		Action: ActionUsageFleetRead, TargetType: "usage", TargetID: month.Format("2006-01"),
		Details: map[string]any{"view": "fleet", "month": month.Format("2006-01"), "sort": sortBy},
	}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		m, err := providerquery.ReadMonthUsage(ctx, tx, month)
		if err != nil {
			return nil, err
		}
		return providerquery.PageFleetUsage(m, sortBy, cursor, limit)
	})
}

func (h *ReadHandlers) usageQueryOK(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	var qe *providerquery.UsageQueryError
	if errors.As(err, &qe) {
		writeReadError(w, http.StatusBadRequest, qe.Code)
		return false
	}
	h.queryError(w, err)
	return false
}

// auditedUsageRead reads, serializes and audits in one REPEATABLE READ
// transaction and writes the body only after COMMIT (fail closed, OQ-1). A
// not-found read is not audited.
func (h *ReadHandlers) auditedUsageRead(w http.ResponseWriter, r *http.Request, actor Actor, entry AuditEntry, read func(context.Context, pgx.Tx) (any, error)) {
	ctx := r.Context()
	tx, err := h.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		log.Printf("provider usage read: begin: %v", err)
		writeReadError(w, http.StatusInternalServerError, "server_error")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	fail := func(status int, code, msg string, err error) {
		if err != nil {
			log.Printf("provider usage read: %s: %v", msg, err)
		}
		_ = tx.Rollback(context.Background())
		writeReadError(w, status, code)
	}
	data, err := read(ctx, tx)
	if errors.Is(err, providerquery.ErrNotFound) {
		fail(http.StatusNotFound, "not_found", "", nil) // no audit
		return
	}
	var qe *providerquery.UsageQueryError
	if errors.As(err, &qe) {
		fail(http.StatusBadRequest, qe.Code, "", nil)
		return
	}
	if err != nil {
		fail(http.StatusInternalServerError, "server_error", "read", err)
		return
	}
	body, err := json.Marshal(data)
	if err != nil {
		fail(http.StatusInternalServerError, "server_error", "marshal", err)
		return
	}
	entry.ProviderUserID = actor.AuditUserID()
	entry.ProviderEmail = actor.Email
	entry.Details = actor.AuditDetails(entry.Details)
	entry.IPAddress = clientIP(r)
	if err := h.store.InsertAuditTx(ctx, tx, entry); err != nil {
		fail(http.StatusInternalServerError, "server_error", "audit insert", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(http.StatusInternalServerError, "server_error", "commit", err)
		return
	}
	writeReadBody(w, http.StatusOK, body)
}
