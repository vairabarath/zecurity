package plans

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/billing"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Plan admin API (Sprint 23 B1, D-43). Every write is super-admin only
// (D-06, through decide()) and commits with its audit row (before/after,
// F17). Writes that change what tenants may create (update, delete, set
// default, assign) need a reason. Errors are {"error":"<code>"}.
//
//	POST   /provider/plans                {name, description?, limits, price?, reason?} → 201 plan
//	PATCH  /provider/plans/{id}           {name?, description?, limits?, price?, reason} → 204
//	DELETE /provider/plans/{id}           {reason}                              → 204
//	POST   /provider/plans/{id}/default   {reason}                              → 204
//	PUT    /provider/tenants/{id}/plan    {plan_id, reason}                     → 204
//
// Reads (GET /provider/plans[/{id}], /provider/plans/{id}/revisions) are in
// internal/provider/read_handlers.go over internal/providerquery (D-04).
//
// Sprint 26 P1b (D-63, D-64): price is the card's price part (null = not
// billable; on PATCH an absent price is unchanged and null removes it). It
// must be in the billing currency. Every create and edit writes one card
// revision and one audit row in one transaction.

const maxReasonRunes = 500

// Handler serves the plan admin API.
type Handler struct {
	Store         *Store
	Authz         *provider.Authz
	ProviderStore *provider.Store
}

// PlanJSON is a plan in API responses.
type PlanJSON struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Limits      Limits         `json:"limits"`
	IsDefault   bool           `json:"is_default"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Price       *billing.Price `json:"price"`
	Revision    int            `json:"revision"`
	Tenants     int64          `json:"tenants"`
}

func planJSON(p Plan) PlanJSON {
	return PlanJSON{ID: p.ID, Name: p.Name, Description: p.Description, Limits: p.Limits,
		IsDefault: p.IsDefault, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Price: p.Price, Revision: p.Revision}
}

// decodePriceField reads an optional price field: absent → unchanged
// (Set=false), null → remove, an object → that price.
func decodePriceField(raw json.RawMessage) (billing.PriceChange, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return billing.PriceChange{}, nil
	}
	if bytes.Equal(t, []byte("null")) {
		return billing.PriceChange{Set: true}, nil
	}
	p, err := billing.DecodePrice(t)
	if err != nil {
		return billing.PriceChange{}, err
	}
	return billing.PriceChange{Set: true, Price: p}, nil
}

// optionalReason trims an optional reason; too long is refused.
func optionalReason(w http.ResponseWriter, raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return "", false
	}
	return reason, true
}

func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (provider.Actor, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("plans: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
	return actor, ok
}

func (h *Handler) authorize(w http.ResponseWriter, actor provider.Actor, action string, target provider.Target) bool {
	if err := h.Authz.CanManagePlans(actor, action, target); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func pathUUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return "", false
	}
	return id, true
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

// decodeLimits parses a limits object. It must define all six keys, each a
// non-negative integer or null; anything else is invalid_limits.
func decodeLimits(raw json.RawMessage) (Limits, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrInvalidLimits
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var l Limits
	if err := dec.Decode(&l); err != nil {
		return nil, ErrInvalidLimits
	}
	if err := l.Validate(); err != nil {
		return nil, ErrInvalidLimits
	}
	return l, nil
}

func writeError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrWorkspaceNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrNameTaken):
		provider.WriteError(w, http.StatusConflict, "plan_name_taken")
	case errors.Is(err, ErrInUse):
		provider.WriteError(w, http.StatusConflict, "plan_in_use")
	case errors.Is(err, ErrIsDefault):
		provider.WriteError(w, http.StatusConflict, "plan_is_default")
	case errors.Is(err, ErrInvalidName):
		provider.WriteError(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, ErrInvalidDesc):
		provider.WriteError(w, http.StatusBadRequest, "invalid_description")
	case errors.Is(err, ErrInvalidLimits):
		provider.WriteError(w, http.StatusBadRequest, "invalid_limits")
	case errors.Is(err, billing.ErrInvalidPrice):
		provider.WriteError(w, http.StatusBadRequest, "invalid_price")
	case errors.Is(err, billing.ErrCurrencyMismatch):
		provider.WriteError(w, http.StatusBadRequest, "currency_mismatch")
	case errors.Is(err, billing.ErrPriceOverflow):
		provider.WriteError(w, http.StatusBadRequest, "price_overflow")
	default:
		log.Printf("plans: %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

func (h *Handler) audit(r *http.Request, actor provider.Actor, action, targetType, targetID string, details map[string]any) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         action,
			TargetType:     targetType,
			TargetID:       targetID,
			Details:        details,
			IPAddress:      provider.ClientIP(r),
		})
	}
}

type createRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Limits      json.RawMessage `json:"limits"`
	Price       json.RawMessage `json:"price"`
	Reason      string          `json:"reason"`
}

// Create: POST /provider/plans. A new plan is never the default.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok || !h.authorize(w, actor, provider.ActionPlanCreate, provider.Target{Type: "plan"}) {
		return
	}
	var req createRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	limits, err := decodeLimits(req.Limits)
	if err != nil {
		writeError(w, "create", err)
		return
	}
	price, err := decodePriceField(req.Price)
	if err != nil {
		writeError(w, "create", err)
		return
	}
	reason, ok := optionalReason(w, req.Reason)
	if !ok {
		return
	}
	var details map[string]any
	if reason != "" {
		details = map[string]any{"reason": reason}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	p, err := h.Store.Create(ctx, req.Name, req.Description, limits, price.Price, CardMeta{Actor: actor.Email, Reason: reason},
		func(ctx context.Context, tx pgx.Tx, p Plan) error {
			return h.audit(r, actor, provider.ActionPlanCreate, "plan", p.ID,
				provider.WithChange(details, nil, p.AuditView()))(ctx, tx)
		})
	if err != nil {
		writeError(w, "create", err)
		return
	}
	provider.WriteJSON(w, http.StatusCreated, planJSON(p))
}

type updateRequest struct {
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Limits      json.RawMessage `json:"limits"`
	Price       json.RawMessage `json:"price"`
	Reason      string          `json:"reason"`
}

// Update: PATCH /provider/plans/{id}. Changed limits take effect at once for
// every tenant on the plan (D-43).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r)
	if !ok || !h.authorize(w, actor, provider.ActionPlanUpdate, provider.Target{Type: "plan", ID: id}) {
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
	change := Change{Name: req.Name, Description: req.Description}
	if req.Limits != nil {
		l, err := decodeLimits(req.Limits)
		if err != nil {
			writeError(w, "update", err)
			return
		}
		change.Limits = l
	}
	price, err := decodePriceField(req.Price)
	if err != nil {
		writeError(w, "update", err)
		return
	}
	change.Price = price
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err = h.Store.Update(ctx, id, change, CardMeta{Actor: actor.Email, Reason: reason}, func(ctx context.Context, tx pgx.Tx, before, after Plan) error {
		return h.audit(r, actor, provider.ActionPlanUpdate, "plan", id,
			provider.WithChange(map[string]any{"reason": reason}, before.AuditView(), after.AuditView()))(ctx, tx)
	})
	if err != nil {
		writeError(w, "update", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// Delete: DELETE /provider/plans/{id}. Refused while the plan is the default
// or assigned to any tenant (D-43).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r)
	if !ok || !h.authorize(w, actor, provider.ActionPlanDelete, provider.Target{Type: "plan", ID: id}) {
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
	err := h.Store.Delete(ctx, id, func(ctx context.Context, tx pgx.Tx, before Plan) error {
		return h.audit(r, actor, provider.ActionPlanDelete, "plan", id,
			provider.WithChange(map[string]any{"reason": reason}, before.AuditView(), nil))(ctx, tx)
	})
	if err != nil {
		writeError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetDefault: POST /provider/plans/{id}/default. New workspaces get the
// default from then on; existing workspaces keep their plan.
func (h *Handler) SetDefault(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r)
	if !ok || !h.authorize(w, actor, provider.ActionPlanSetDefault, provider.Target{Type: "plan", ID: id}) {
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
	_, err := h.Store.SetDefault(ctx, id, func(ctx context.Context, tx pgx.Tx, previous, next Plan) error {
		details := map[string]any{"reason": reason, "plan_name": next.Name}
		var before map[string]any
		if previous.ID != "" {
			before = map[string]any{"default_plan_id": previous.ID, "default_plan_name": previous.Name}
		}
		return h.audit(r, actor, provider.ActionPlanSetDefault, "plan", id,
			provider.WithChange(details, before, map[string]any{"default_plan_id": next.ID, "default_plan_name": next.Name}))(ctx, tx)
	})
	if err != nil {
		writeError(w, "set default", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type assignRequest struct {
	PlanID string `json:"plan_id"`
	Reason string `json:"reason"`
}

// Assign: PUT /provider/tenants/{id}/plan. The new limits apply at once;
// existing resources are never deleted (D-43).
func (h *Handler) Assign(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	workspaceID, ok := pathUUID(w, r)
	if !ok || !h.authorize(w, actor, provider.ActionTenantPlanAssign, provider.Target{Type: "tenant", ID: workspaceID}) {
		return
	}
	var req assignRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if _, err := uuid.Parse(req.PlanID); err != nil {
		provider.WriteError(w, http.StatusBadRequest, "invalid_plan_id")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.Store.Assign(ctx, workspaceID, req.PlanID, func(ctx context.Context, tx pgx.Tx, before, after Plan) error {
		return h.audit(r, actor, provider.ActionTenantPlanAssign, "tenant", workspaceID,
			provider.WithChange(map[string]any{"reason": reason},
				map[string]any{"plan_id": before.ID, "plan_name": before.Name},
				map[string]any{"plan_id": after.ID, "plan_name": after.Name}))(ctx, tx)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			provider.WriteError(w, http.StatusBadRequest, "plan_not_found")
			return
		}
		writeError(w, "assign", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
