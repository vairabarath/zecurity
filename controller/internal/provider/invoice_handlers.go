package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/billing"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Invoice API (Sprint 26 P3, F34; D-66, D-68, D-70). Billing is tenant data:
// invoice.read (super-admin, auditor, billing) / invoice.write (super-admin,
// billing) since D-73; relay-ops and support 403. Every
// response is Cache-Control: no-store.
//
//	GET  /provider/invoices?month=YYYY-MM&status=&tenant_id=&cursor=&limit=   (audit invoice.fleet.read, one row)
//	GET  /provider/invoices/{id}                                             (audit invoice.read)
//	POST /provider/invoices/{id}/issue      {reason?}                       → 204
//	POST /provider/invoices/{id}/mark-paid  {paid_at, reference, reason}    → 204
//	POST /provider/invoices/{id}/void       {reason}                        → 204
//	POST /provider/invoices/regenerate      {month, tenant_id?, reason}     → {regenerated}
//	POST /provider/invoices/{id}/resend     {reason}                        → 204
//
// The writes call internal/billing's lifecycle, the only implementation,
// and commit with their audit row (reason, before/after). Read audits are
// committed before the first response byte (as tenant detail, OQ-1).

// InvoiceHandler serves the invoice API.
type InvoiceHandler struct {
	DB           *pgxpool.Pool
	Authz        *Authz
	Store        *Store
	Generator    *billing.Generator
	ProviderName string           // BILLING_PROVIDER_NAME, shown on the invoice and in the email
	Now          func() time.Time // nil = time.Now
}

func (h *InvoiceHandler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// parseMonth accepts YYYY-MM or YYYY-MM-DD (the first of the month).
func parseMonth(s string) (time.Time, bool) {
	if t, err := time.Parse("2006-01", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil && t.Day() == 1 {
		return t, true
	}
	return time.Time{}, false
}

func (h *InvoiceHandler) actorOr500(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	a, ok := ActorFromContext(r.Context())
	if !ok {
		WriteError(w, http.StatusInternalServerError, "server_error")
	}
	return a, ok
}

func (h *InvoiceHandler) audit(ctx context.Context, tx pgx.Tx, r *http.Request, actor Actor, action, targetID string, details map[string]any) error {
	userID := actor.UserID
	return h.Store.InsertAuditTx(ctx, tx, AuditEntry{
		ProviderUserID: &userID,
		ProviderEmail:  actor.Email,
		Action:         action,
		TargetType:     "invoice",
		TargetID:       targetID,
		Details:        details,
		IPAddress:      clientIP(r),
	})
}

// readAudited runs read in a transaction, marshals the result, writes the
// audit row and commits; only then is the body written.
func (h *InvoiceHandler) readAudited(w http.ResponseWriter, r *http.Request, actor Actor, action, target string, details map[string]any,
	read func(ctx context.Context, tx pgx.Tx) (any, error)) {
	ctx := r.Context()
	tx, err := h.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite})
	if err != nil {
		log.Printf("invoices: begin: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	v, err := read(ctx, tx)
	switch {
	case errors.Is(err, providerquery.ErrNotFound):
		WriteError(w, http.StatusNotFound, "invoice_not_found")
		return
	case errors.Is(err, providerquery.ErrInvalidID), errors.Is(err, providerquery.ErrInvalidFilter):
		WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	case errors.Is(err, providerquery.ErrInvalidCursor):
		WriteError(w, http.StatusBadRequest, "invalid_cursor")
		return
	case err != nil:
		log.Printf("invoices: read: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("invoices: marshal: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := h.audit(ctx, tx, r, actor, action, target, details); err != nil {
		log.Printf("invoices: read audit: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("invoices: commit: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// List: GET /provider/invoices.
func (h *InvoiceHandler) List(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOr500(w, r)
	if !ok {
		return
	}
	if err := h.Authz.CanReadInvoices(actor); err != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	q := r.URL.Query()
	f := providerquery.InvoiceFilter{Status: q.Get("status"), TenantID: q.Get("tenant_id"), Cursor: q.Get("cursor")}
	if m := q.Get("month"); m != "" {
		t, ok := parseMonth(m)
		if !ok {
			WriteError(w, http.StatusBadRequest, "invalid_month")
			return
		}
		f.Month = t
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			WriteError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		f.Limit = n
	}
	details := map[string]any{}
	if !f.Month.IsZero() {
		details["month"] = f.Month.Format("2006-01")
	}
	if f.Status != "" {
		details["status"] = f.Status
	}
	if f.TenantID != "" {
		details["tenant_id"] = f.TenantID
	}
	h.readAudited(w, r, actor, ActionInvoiceFleetRead, "invoices", details, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return providerquery.ListInvoices(ctx, tx, f)
	})
}

// Get: GET /provider/invoices/{id}.
func (h *InvoiceHandler) Get(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorOr500(w, r)
	if !ok {
		return
	}
	if err := h.Authz.CanReadInvoices(actor); err != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	id := r.PathValue("id")
	if !providerquery.IsUUID(id) {
		WriteError(w, http.StatusNotFound, "invoice_not_found")
		return
	}
	h.readAudited(w, r, actor, ActionInvoiceRead, id, nil, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return providerquery.GetInvoice(ctx, tx, id, h.ProviderName)
	})
}

// ── writes ───────────────────────────────────────────────────────────────────

type invoiceWriteRequest struct {
	Reason    string `json:"reason"`
	PaidAt    string `json:"paid_at"`
	Reference string `json:"reference"`
	Month     string `json:"month"`
	TenantID  string `json:"tenant_id"`
}

func invoiceReason(w http.ResponseWriter, raw string, required bool) (string, bool) {
	reason := strings.TrimSpace(raw)
	switch {
	case reason == "" && required:
		WriteError(w, http.StatusBadRequest, "reason_required")
		return "", false
	case utf8.RuneCountInString(reason) > 500:
		WriteError(w, http.StatusBadRequest, "reason_too_long")
		return "", false
	}
	return reason, true
}

func writeInvoiceError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, billing.ErrInvoiceNotFound):
		WriteError(w, http.StatusNotFound, "invoice_not_found")
	case errors.Is(err, billing.ErrNotDraft):
		WriteError(w, http.StatusConflict, "invoice_not_draft")
	case errors.Is(err, billing.ErrNotIssued), errors.Is(err, billing.ErrNotSent):
		WriteError(w, http.StatusConflict, "invoice_not_issued")
	case errors.Is(err, billing.ErrMonthOpen):
		WriteError(w, http.StatusConflict, "month_open")
	case errors.Is(err, billing.ErrReasonRequired):
		WriteError(w, http.StatusBadRequest, "reason_required")
	case errors.Is(err, billing.ErrReasonTooLong):
		WriteError(w, http.StatusBadRequest, "reason_too_long")
	case errors.Is(err, billing.ErrReferenceTooLong):
		WriteError(w, http.StatusBadRequest, "reference_too_long")
	case errors.Is(err, billing.ErrInvalidPaidAt):
		WriteError(w, http.StatusBadRequest, "invalid_paid_at")
	default:
		log.Printf("invoices: %s: %v", what, err)
		WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

// begin decodes the body and checks invoice.write; id is "" for regenerate.
func (h *InvoiceHandler) begin(w http.ResponseWriter, r *http.Request, withID bool) (Actor, string, invoiceWriteRequest, bool) {
	var req invoiceWriteRequest
	actor, ok := h.actorOr500(w, r)
	if !ok {
		return actor, "", req, false
	}
	id := ""
	if withID {
		id = r.PathValue("id")
	}
	if err := h.Authz.CanWriteInvoices(actor, Target{Type: "invoice", ID: id}); err != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return actor, "", req, false
	}
	if withID && !providerquery.IsUUID(id) {
		WriteError(w, http.StatusNotFound, "invoice_not_found")
		return actor, "", req, false
	}
	if !DecodeJSON(w, r, &req) {
		WriteError(w, http.StatusBadRequest, "invalid_request")
		return actor, "", req, false
	}
	return actor, id, req, true
}

// transition runs one lifecycle call and its audit row in one transaction.
func (h *InvoiceHandler) transition(w http.ResponseWriter, r *http.Request, actor Actor, id, action, reason string, extra map[string]any,
	fn func(ctx context.Context, tx pgx.Tx) (before, after billing.Invoice, err error)) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err := pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		before, after, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		details := map[string]any{}
		if reason != "" {
			details["reason"] = reason
		}
		for k, v := range extra {
			details[k] = v
		}
		return h.audit(ctx, tx, r, actor, action, id, WithChange(details, before.AuditView(), after.AuditView()))
	})
	if err != nil {
		writeInvoiceError(w, action, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// Issue: POST /provider/invoices/{id}/issue.
func (h *InvoiceHandler) Issue(w http.ResponseWriter, r *http.Request) {
	actor, id, req, ok := h.begin(w, r, true)
	if !ok {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, false)
	if !ok {
		return
	}
	h.transition(w, r, actor, id, ActionInvoiceIssue, reason, nil, func(ctx context.Context, tx pgx.Tx) (billing.Invoice, billing.Invoice, error) {
		return billing.Issue(ctx, tx, id, actor.Email, h.now())
	})
}

// MarkPaid: POST /provider/invoices/{id}/mark-paid.
func (h *InvoiceHandler) MarkPaid(w http.ResponseWriter, r *http.Request) {
	actor, id, req, ok := h.begin(w, r, true)
	if !ok {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	paidAt, err := time.Parse("2006-01-02", req.PaidAt)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_paid_at")
		return
	}
	if utf8.RuneCountInString(strings.TrimSpace(req.Reference)) > 200 {
		WriteError(w, http.StatusBadRequest, "reference_too_long")
		return
	}
	h.transition(w, r, actor, id, ActionInvoiceMarkPaid, reason, nil, func(ctx context.Context, tx pgx.Tx) (billing.Invoice, billing.Invoice, error) {
		return billing.MarkPaid(ctx, tx, id, paidAt, req.Reference, actor.Email, h.now())
	})
}

// Void: POST /provider/invoices/{id}/void.
func (h *InvoiceHandler) Void(w http.ResponseWriter, r *http.Request) {
	actor, id, req, ok := h.begin(w, r, true)
	if !ok {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	h.transition(w, r, actor, id, ActionInvoiceVoid, reason, nil, func(ctx context.Context, tx pgx.Tx) (billing.Invoice, billing.Invoice, error) {
		return billing.Void(ctx, tx, id, reason, actor.Email, h.now())
	})
}

// Resend: POST /provider/invoices/{id}/resend. The audit records the
// delivery queued; recipients are resolved at send time and never audited.
func (h *InvoiceHandler) Resend(w http.ResponseWriter, r *http.Request) {
	actor, id, req, ok := h.begin(w, r, true)
	if !ok {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	h.transition(w, r, actor, id, ActionInvoiceResend, reason, nil, func(ctx context.Context, tx pgx.Tx) (billing.Invoice, billing.Invoice, error) {
		inv, err := billing.Resend(ctx, tx, id)
		return inv, inv, err
	})
}

// Regenerate: POST /provider/invoices/regenerate. Drafts are rewritten one
// tenant per transaction (as the month close); one audit row then lists
// every draft written, before and after.
func (h *InvoiceHandler) Regenerate(w http.ResponseWriter, r *http.Request) {
	actor, _, req, ok := h.begin(w, r, false)
	if !ok {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	month, ok := parseMonth(req.Month)
	if !ok {
		WriteError(w, http.StatusBadRequest, "invalid_month")
		return
	}
	if req.TenantID != "" && !providerquery.IsUUID(req.TenantID) {
		WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := h.Generator.Regenerate(ctx, month, req.TenantID)
	if err != nil && !errors.Is(err, billing.ErrMonthOpen) && len(res.Changed) == 0 {
		writeInvoiceError(w, "regenerate", err)
		return
	}
	if errors.Is(err, billing.ErrMonthOpen) {
		writeInvoiceError(w, "regenerate", err)
		return
	}
	partial := err != nil
	changes := make([]map[string]any, 0, len(res.Changed))
	for _, c := range res.Changed {
		ch := map[string]any{"invoice_id": c.After.ID, "after": c.After.AuditView()}
		if c.Before != nil {
			ch["before"] = c.Before.AuditView()
		}
		changes = append(changes, ch)
	}
	details := map[string]any{"reason": reason, "month": month.Format("2006-01"), "created": res.Created,
		"regenerated": res.Regenerated, "drafts": changes}
	if req.TenantID != "" {
		details["tenant_id"] = req.TenantID
	}
	if partial {
		details["partial"] = true // a tenant failed (logged); the rest were written
	}
	if err := pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		return h.audit(ctx, tx, r, actor, ActionInvoiceRegenerate, "invoices", details)
	}); err != nil {
		log.Printf("invoices: regenerate audit: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if partial {
		log.Printf("invoices: regenerate %s: some tenants failed: %v", month.Format("2006-01"), err)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"regenerated": res.Created + res.Regenerated})
}
