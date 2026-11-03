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

// Provider read APIs (Sprint 21 Phase R, D-04/D-15/D-23). Every route sits
// behind RequireProvider (which supplies the database-backed Actor) and is
// authorized here through Authz; queries live in internal/providerquery.
//
//	GET /provider/relays          relay.read  (super-admin, relay-ops)
//	GET /provider/relays/{id}     relay.read
//	GET /provider/tenants         tenant.read (super-admin)
//	GET /provider/tenants/{id}    tenant.read, audited, fail-closed (OQ-1)
//	GET /provider/audit           audit.view  (super-admin)
//	GET /provider/certificates    cert.read   (super-admin)
//
// Only a successful tenant detail read is audited. Lists, relay, audit and
// certificate reads, and 400/403/404 responses write nothing.

// ReadDB is what the read handlers need from the database: reads, plus
// transactions for the audited tenant detail. *pgxpool.Pool satisfies it.
type ReadDB interface {
	providerquery.Querier
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// ReadHandlers serves the provider read APIs.
type ReadHandlers struct {
	db         ReadDB
	authz      *Authz
	store      *Store
	heartbeats providerquery.HeartbeatSource      // may be nil: DB heartbeat only
	ctlCert    providerquery.ControllerCertSource // may be nil: no controller_grpc row
}

// NewReadHandlers wires the read handlers. heartbeats is the relay service
// (Valkey liveness); ctlCert is the controller's certificate rotator.
func NewReadHandlers(db ReadDB, authz *Authz, store *Store, heartbeats providerquery.HeartbeatSource, ctlCert providerquery.ControllerCertSource) *ReadHandlers {
	return &ReadHandlers{db: db, authz: authz, store: store, heartbeats: heartbeats, ctlCert: ctlCert}
}

// ── relays ───────────────────────────────────────────────────────────────────

// ListRelays: GET /provider/relays?status=&limit=&cursor=
func (h *ReadHandlers) ListRelays(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadRelays(actor, Target{Type: "relay"})) {
		return
	}
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	status := q.Get("status")
	if status != "" && !contains(providerquery.RelayStatuses, status) {
		writeReadError(w, http.StatusBadRequest, "invalid_status")
		return
	}
	page, err := providerquery.ListRelays(r.Context(), h.db, h.heartbeats, providerquery.RelayFilter{
		Status: status, Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, page)
}

// GetRelay: GET /provider/relays/{id}
func (h *ReadHandlers) GetRelay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadRelays(actor, Target{Type: "relay", ID: id})) {
		return
	}
	if !providerquery.IsUUID(id) {
		writeReadError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	d, err := providerquery.GetRelay(r.Context(), h.db, h.heartbeats, id)
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, d)
}

// ── tenants ──────────────────────────────────────────────────────────────────

// ListTenants: GET /provider/tenants?status=&limit=&cursor=  (not audited)
func (h *ReadHandlers) ListTenants(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadTenants(actor, Target{Type: "tenant"})) {
		return
	}
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	status := q.Get("status")
	if status != "" && !contains(providerquery.TenantStatuses, status) {
		writeReadError(w, http.StatusBadRequest, "invalid_status")
		return
	}
	page, err := providerquery.ListTenants(r.Context(), h.db, providerquery.TenantFilter{
		Status: status, Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, page)
}

// tenantReadAuditDetails is the exact details payload of a tenant.read row.
var tenantReadAuditDetails = map[string]any{"view": "detail"}

// GetTenant: GET /provider/tenants/{id}. Audited and fail-closed (OQ-1): the
// detail is read, serialized and audited inside one REPEATABLE READ
// transaction, and the first response byte is written only after COMMIT
// succeeds. If the audit insert or the commit fails, the client gets a 500
// and no tenant data. A client that disconnects after the commit leaves an
// audit row for a response it never received: errors over-record, never
// under-record.
func (h *ReadHandlers) GetTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadTenants(actor, Target{Type: "tenant", ID: id})) {
		return // 403: nothing read, nothing audited
	}
	if !providerquery.IsUUID(id) {
		writeReadError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	ctx := r.Context()

	tx, err := h.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		log.Printf("provider tenant detail: begin: %v", err)
		writeReadError(w, http.StatusInternalServerError, "server_error")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // safety net; no-op after Commit
	// fail rolls back first, then answers: nothing from the transaction is
	// ever written to the client on an error path.
	fail := func(status int, code, msg string, err error) {
		if err != nil {
			log.Printf("provider tenant detail: %s: %v", msg, err)
		}
		_ = tx.Rollback(context.Background())
		writeReadError(w, status, code)
	}

	detail, err := providerquery.GetTenant(ctx, tx, id)
	if errors.Is(err, providerquery.ErrNotFound) {
		fail(http.StatusNotFound, "not_found", "", nil) // no audit
		return
	}
	if err != nil {
		fail(http.StatusInternalServerError, "server_error", "read", err)
		return
	}

	body, err := json.Marshal(detail) // serialize before the audit so nothing can fail after COMMIT
	if err != nil {
		fail(http.StatusInternalServerError, "server_error", "marshal", err)
		return
	}

	userID := actor.UserID
	if err := h.store.InsertAuditTx(ctx, tx, AuditEntry{
		ProviderUserID: &userID,
		ProviderEmail:  actor.Email,
		Action:         ActionTenantRead,
		TargetType:     "tenant",
		TargetID:       id,
		Details:        tenantReadAuditDetails,
		IPAddress:      clientIP(r),
	}); err != nil {
		fail(http.StatusInternalServerError, "server_error", "audit insert", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(http.StatusInternalServerError, "server_error", "commit", err)
		return
	}

	writeReadBody(w, http.StatusOK, body) // first response byte: only after the audit row is committed
}

// ── provider audit ───────────────────────────────────────────────────────────

// QueryAudit: GET /provider/audit?action=&target_type=&target_id=
// &provider_email=&since=&until=&limit=&cursor=  (not audited)
func (h *ReadHandlers) QueryAudit(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanViewProviderAudit(actor)) {
		return
	}
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	since, ok := parseTime(w, q.Get("since"), "invalid_since")
	if !ok {
		return
	}
	until, ok := parseTime(w, q.Get("until"), "invalid_until")
	if !ok {
		return
	}
	if since != nil && until != nil && since.After(*until) {
		writeReadError(w, http.StatusBadRequest, "invalid_range")
		return
	}
	page, err := providerquery.QueryProviderAudit(r.Context(), h.db, providerquery.AuditFilter{
		Action: q.Get("action"), TargetType: q.Get("target_type"), TargetID: q.Get("target_id"),
		ProviderEmail: q.Get("provider_email"), Since: since, Until: until,
		Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, page)
}

// ── certificates ─────────────────────────────────────────────────────────────

// Certificates: GET /provider/certificates?within=&kind=&tenant_id=&limit=&cursor=
// (not audited)
func (h *ReadHandlers) Certificates(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.allowed(w, h.authz.CanReadCertificates(actor)) {
		return
	}
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	var within time.Duration
	if raw := q.Get("within"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > providerquery.MaxCertWithin {
			writeReadError(w, http.StatusBadRequest, "invalid_within")
			return
		}
		within = d
	}
	kind := q.Get("kind")
	if kind != "" && !contains(providerquery.CertificateKinds, kind) {
		writeReadError(w, http.StatusBadRequest, "invalid_kind")
		return
	}
	tenantID := q.Get("tenant_id")
	if tenantID != "" && !providerquery.IsUUID(tenantID) {
		writeReadError(w, http.StatusBadRequest, "invalid_tenant_id")
		return
	}
	res, err := providerquery.CertificateExpiry(r.Context(), h.db, h.ctlCert, providerquery.CertificateFilter{
		Within: within, Kind: kind, TenantID: tenantID, Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.queryError(w, err)
		return
	}
	writeReadJSON(w, res)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *ReadHandlers) actor(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	a, ok := ActorFromContext(r.Context())
	if !ok {
		writeReadError(w, http.StatusInternalServerError, "server_error")
	}
	return a, ok
}

func (h *ReadHandlers) allowed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrForbidden):
		writeReadError(w, http.StatusForbidden, "forbidden")
	default:
		writeReadError(w, http.StatusInternalServerError, "server_error")
	}
	return false
}

// queryError maps providerquery errors that survive handler validation.
func (h *ReadHandlers) queryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providerquery.ErrNotFound):
		writeReadError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, providerquery.ErrInvalidCursor):
		writeReadError(w, http.StatusBadRequest, "invalid_cursor")
	case errors.Is(err, providerquery.ErrInvalidID):
		writeReadError(w, http.StatusBadRequest, "invalid_id")
	case errors.Is(err, providerquery.ErrInvalidFilter):
		writeReadError(w, http.StatusBadRequest, "invalid_filter")
	default:
		log.Printf("provider read: %v", err)
		writeReadError(w, http.StatusInternalServerError, "server_error")
	}
}

// parseLimit accepts an empty value (default) or a positive integer; values
// above the maximum are clamped by providerquery.
func parseLimit(w http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeReadError(w, http.StatusBadRequest, "invalid_limit")
		return 0, false
	}
	return n, true
}

func parseTime(w http.ResponseWriter, raw, code string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeReadError(w, http.StatusBadRequest, code)
		return nil, false
	}
	return &t, true
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func writeReadJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("provider read: marshal: %v", err)
		writeReadError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeReadBody(w, http.StatusOK, body)
}

func writeReadError(w http.ResponseWriter, status int, code string) {
	body, _ := json.Marshal(map[string]string{"error": code})
	writeReadBody(w, status, body)
}

func writeReadBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
