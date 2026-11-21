package alerting

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Incident timeline (Sprint 24 T1, F27):
//
//	GET /provider/timeline?relay_id=|pool_id=|tenant_id=&source=&since=&until=&cursor=&limit=
//	  → Page[providerquery.TimelineItem], newest first
//
// Read-only; it merges alert_events and provider audit rows (see
// providerquery/timeline.go) and stores nothing. Visibility:
//   - alert.read is required (super-admin, relay-ops);
//   - alert rows follow the A1 alert reads: relay-ops see platform relay and
//     pool kinds only, and a tenant_id request gets no rows;
//   - audit rows: super-admins (audit.view) get every incident-relevant row;
//     relay-ops (timeline.relay_audit, D-55) get relay and pool operations
//     only (provider.RelayOpsTimelineAuditActions), with actor and reason but
//     no raw details, and never a tenant or operator-account row. The full
//     provider audit log (GET /provider/audit) stays super-admin only.
//
// At most one of relay_id, pool_id, tenant_id. pool_id also matches the rows
// of the relays now in the pool (membership is current state, not history).
// source is alert | audit | "" (both); since/until are RFC 3339, half-open.

// TimelineHandler serves GET /provider/timeline.
type TimelineHandler struct {
	DB    *pgxpool.Pool
	Authz *provider.Authz
}

func parseTimelineTime(v string) (*time.Time, bool) {
	if v == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// List: GET /provider/timeline.
func (h *TimelineHandler) List(w http.ResponseWriter, r *http.Request) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if h.Authz.CanReadAlerts(actor) != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	q := r.URL.Query()
	f := providerquery.TimelineFilter{
		RelayID: q.Get("relay_id"), PoolID: q.Get("pool_id"), TenantID: q.Get("tenant_id"),
		Cursor: q.Get("cursor"),
	}
	switch q.Get("source") {
	case "":
		f.IncludeAlerts, f.IncludeAudit = true, true
	case providerquery.TimelineSourceAlert:
		f.IncludeAlerts = true
	case providerquery.TimelineSourceAudit:
		f.IncludeAudit = true
	default:
		provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
			return
		}
		f.Limit = n
	}
	var okT bool
	if f.Since, okT = parseTimelineTime(q.Get("since")); !okT {
		provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
		return
	}
	if f.Until, okT = parseTimelineTime(q.Get("until")); !okT {
		provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
		return
	}

	// Audit rows: the whole incident set with audit.view (super-admin);
	// otherwise relay and pool operations only (D-55), without details.
	if h.Authz.CanViewProviderAudit(actor) != nil {
		if h.Authz.CanReadTimelineRelayAudit(actor) != nil {
			f.IncludeAudit = false
		}
		f.AuditActions = provider.RelayOpsTimelineAuditActions
		f.OmitAuditDetails = true
	}
	if actor.Role != provider.RoleSuperAdmin {
		// relay-ops: platform relay and pool alerts only, as A1's reads; a
		// tenant filter gets nothing at all.
		if f.TenantID != "" {
			f.IncludeAlerts, f.IncludeAudit = false, false
		}
		f.AlertScope = ScopePlatform
		f.AlertKinds = RelayOpsKinds()
	}
	page, err := providerquery.QueryTimeline(r.Context(), h.DB, f)
	if err != nil {
		writeReadError(w, "timeline", err)
		return
	}
	if page.Items == nil {
		page.Items = []providerquery.TimelineItem{}
	}
	provider.WriteJSON(w, http.StatusOK, page)
}
