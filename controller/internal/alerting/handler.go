package alerting

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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Alert API (Sprint 24 A1, D-50, D-53). Errors are {"error":"<code>"}.
//
//	GET    /provider/alerts?scope=&tenant_id=&status=            → {"items":[ActiveAlert], "truncated": bool}
//	GET    /provider/alert-events?scope=&tenant_id=&cursor=&limit= → Page[Event]
//	GET    /provider/alert-rules                                 → {"items":[Rule]}
//	POST   /provider/alert-rules      {kind, name, enabled?, severity, params?, for_seconds?, renotify_seconds?, reason} → 201 Rule
//	PATCH  /provider/alert-rules/{id} {name?, enabled?, severity?, params?, for_seconds?, renotify_seconds?, reason} → 200 Rule
//	DELETE /provider/alert-rules/{id} {reason}                   → 204
//
// Alert reads: super-admins see everything; relay-ops see relay and pool
// alerts only (no tenant alerts, no tenant names; D-40 extended), and a
// relay-ops request filtering by tenant or the tenant scope gets nothing.
// Rule reads and writes: super-admin only (D-53). Every write needs a reason
// and commits with its provider audit row (before/after, F17).

const maxReasonRunes = 500

// Handler serves the alert API.
type Handler struct {
	Store         *Store
	Authz         *provider.Authz
	ProviderStore *provider.Store
	Notifier      Notifier // events of a rule deletion (resolved alerts); may be nil
	Now           func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (provider.Actor, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("alerting: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
	return actor, ok
}

func (h *Handler) forbidden(w http.ResponseWriter, err error) bool {
	if err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return true
	}
	return false
}

// readFilter builds the filter for an alert read, restricted by the role's
// alert view (relay-ops: platform relay and pool alerts; support: tenant
// alerts without usage alerts). false: the request can match nothing.
func readFilter(r *http.Request, actor provider.Actor) (ReadFilter, bool) {
	q := r.URL.Query()
	f := ReadFilter{Scope: q.Get("scope"), TenantID: q.Get("tenant_id"), Status: q.Get("status")}
	switch viewFor(actor.Role) {
	case viewAll:
	case viewTenant:
		if f.Scope == ScopePlatform {
			return f, false
		}
		f.Scope = ScopeTenant
		f.Kinds = SupportKinds()
	default:
		if f.Scope == ScopeTenant || f.TenantID != "" {
			return f, false
		}
		f.Scope = ScopePlatform
		f.Kinds = RelayOpsKinds()
	}
	return f, true
}

func writeReadError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, providerquery.ErrInvalidFilter), errors.Is(err, providerquery.ErrInvalidCursor):
		provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
	default:
		log.Printf("alerting: %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

// ListActive: GET /provider/alerts.
func (h *Handler) ListActive(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || h.forbidden(w, h.Authz.CanReadAlerts(actor)) {
		return
	}
	f, visible := readFilter(r, actor)
	items := []ActiveAlert{}
	truncated := false
	if visible {
		var err error
		if items, truncated, err = h.Store.ListActive(r.Context(), f); err != nil {
			writeReadError(w, "list active", err)
			return
		}
		if items == nil {
			items = []ActiveAlert{}
		}
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": truncated})
}

// ListEvents: GET /provider/alert-events.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || h.forbidden(w, h.Authz.CanReadAlerts(actor)) {
		return
	}
	f, visible := readFilter(r, actor)
	if !visible {
		provider.WriteJSON(w, http.StatusOK, providerquery.Page[Event]{Items: []Event{}})
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			provider.WriteError(w, http.StatusBadRequest, "invalid_filter")
			return
		}
		limit = n
	}
	page, err := h.Store.ListEvents(r.Context(), f, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeReadError(w, "list events", err)
		return
	}
	if page.Items == nil {
		page.Items = []Event{}
	}
	provider.WriteJSON(w, http.StatusOK, page)
}

// ListRules: GET /provider/alert-rules (super-admin only).
func (h *Handler) ListRules(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || h.forbidden(w, h.Authz.CanManageAlertRules(actor, provider.ActionAlertRuleRead, provider.Target{Type: "alert_rule"})) {
		return
	}
	rules, err := h.Store.ListRules(r.Context())
	if err != nil {
		writeReadError(w, "list rules", err)
		return
	}
	if rules == nil {
		rules = []Rule{}
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"items": rules})
}

func requireReason(w http.ResponseWriter, raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		provider.WriteError(w, http.StatusBadRequest, "reason_required")
		return "", false
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return "", false
	}
	return reason, true
}

func writeWriteError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrNameTaken):
		provider.WriteError(w, http.StatusConflict, "rule_name_taken")
	case errors.Is(err, ErrInvalidParams):
		provider.WriteError(w, http.StatusBadRequest, "invalid_params")
	case errors.Is(err, ErrInvalidRule):
		provider.WriteError(w, http.StatusBadRequest, "invalid_rule")
	default:
		log.Printf("alerting: %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

func (h *Handler) audit(r *http.Request, actor provider.Actor, action, reason string) AuditFunc {
	return func(ctx context.Context, tx pgx.Tx, before, after *Rule) error {
		var b, a map[string]any
		id := ""
		if before != nil {
			b, id = before.AuditView(), before.ID
		}
		if after != nil {
			a, id = after.AuditView(), after.ID
		}
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         action,
			TargetType:     "alert_rule",
			TargetID:       id,
			Details:        provider.WithChange(map[string]any{"reason": reason}, b, a),
			IPAddress:      provider.ClientIP(r),
		})
	}
}

func pathUUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return "", false
	}
	return id, true
}

type createRequest struct {
	Kind            string          `json:"kind"`
	Name            string          `json:"name"`
	Enabled         *bool           `json:"enabled"`
	Severity        string          `json:"severity"`
	Params          json.RawMessage `json:"params"`
	ForSeconds      int             `json:"for_seconds"`
	RenotifySeconds int             `json:"renotify_seconds"`
	Reason          string          `json:"reason"`
}

// CreateRule: POST /provider/alert-rules.
func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || h.forbidden(w, h.Authz.CanManageAlertRules(actor, provider.ActionAlertRuleCreate, provider.Target{Type: "alert_rule"})) {
		return
	}
	var req createRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	if ScopeOf(req.Kind) == "" {
		provider.WriteError(w, http.StatusBadRequest, "invalid_rule")
		return
	}
	params, err := ParseParams(req.Kind, req.Params)
	if err != nil {
		writeWriteError(w, "create", err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rule, err := h.Store.CreateRule(ctx, Rule{
		Kind: req.Kind, Name: req.Name, Enabled: enabled, Severity: req.Severity, Params: params,
		ForSeconds: req.ForSeconds, RenotifySeconds: req.RenotifySeconds,
	}, h.audit(r, actor, provider.ActionAlertRuleCreate, reason))
	if err != nil {
		writeWriteError(w, "create", err)
		return
	}
	provider.WriteJSON(w, http.StatusCreated, rule)
}

type updateRequest struct {
	Name            *string         `json:"name"`
	Enabled         *bool           `json:"enabled"`
	Severity        *string         `json:"severity"`
	Params          json.RawMessage `json:"params"`
	ForSeconds      *int            `json:"for_seconds"`
	RenotifySeconds *int            `json:"renotify_seconds"`
	Reason          string          `json:"reason"`
}

// UpdateRule: PATCH /provider/alert-rules/{id}. Disabling a rule resolves
// its firing alerts on the next evaluator tick.
func (h *Handler) UpdateRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r)
	if !ok || h.forbidden(w, h.Authz.CanManageAlertRules(actor, provider.ActionAlertRuleUpdate, provider.Target{Type: "alert_rule", ID: id})) {
		return
	}
	var req updateRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rule, err := h.Store.UpdateRule(ctx, id, RuleChange{
		Name: req.Name, Enabled: req.Enabled, Severity: req.Severity, Params: req.Params,
		ForSeconds: req.ForSeconds, RenotifySeconds: req.RenotifySeconds,
	}, h.audit(r, actor, provider.ActionAlertRuleUpdate, reason))
	if err != nil {
		writeWriteError(w, "update", err)
		return
	}
	provider.WriteJSON(w, http.StatusOK, rule)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// DeleteRule: DELETE /provider/alert-rules/{id}. Its firing alerts are
// resolved in the same transaction.
func (h *Handler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r)
	if !ok || h.forbidden(w, h.Authz.CanManageAlertRules(actor, provider.ActionAlertRuleDelete, provider.Target{Type: "alert_rule", ID: id})) {
		return
	}
	var req reasonRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	events, err := h.Store.DeleteRule(ctx, id, h.audit(r, actor, provider.ActionAlertRuleDelete, reason), h.now())
	if err != nil {
		writeWriteError(w, "delete", err)
		return
	}
	if len(events) > 0 && h.Notifier != nil {
		h.Notifier.Notify(context.WithoutCancel(r.Context()), events)
	}
	w.WriteHeader(http.StatusNoContent)
}
