package relay

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
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Relay pool admin API (Decision Record D-37, D-38, D-40, D-41):
//
//	POST   /provider/relay-pools                relay-ops (region/residency: super-admin) {name, region?, residency?} → 201 pool
//	PATCH  /provider/relay-pools/{id}           relay-ops (region/residency: super-admin) {name?, region?, residency?} → 204
//	DELETE /provider/relay-pools/{id}           relay-ops                                  → 204
//	POST   /provider/relays/{id}/pool           relay-ops   {pool_id, reason}              → 204
//	PUT    /provider/tenants/{id}/relay-pool    super-admin {pool_id, failover_pool_id?, reason, migration} → 204
//
// Every mutation commits with its audit row (before/after, F17). Moves and
// assignments publish the relay lists after commit (connectors of the affected
// pools migrate make-before-break). Errors are {"error":"<code>"}.

// DefaultPoolMigrationWindow is how long a graceful tenant move keeps the old
// pool authorized (D-40). It must stay longer than the connector drain
// timeout (120 s) so make-before-break completes.
const DefaultPoolMigrationWindow = 5 * time.Minute

// MinPoolMigrationWindow is the connector's default relay drain timeout.
const MinPoolMigrationWindow = 120 * time.Second

type poolResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Region    *string `json:"region"`
	Residency bool    `json:"residency"`
}

func poolJSON(p Pool) poolResponse {
	return poolResponse{ID: p.ID, Name: p.Name, Region: p.Region, Residency: p.Residency}
}

func poolAudit(p Pool) map[string]any {
	return map[string]any{"name": p.Name, "region": p.Region, "residency": p.Residency}
}

// actorOr500 returns the injected provider actor; its absence is a wiring bug.
func actorOr500(w http.ResponseWriter, r *http.Request, what string) (provider.Actor, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("relay pools: %s without provider actor (wiring bug)", what)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
	return actor, ok
}

// writePoolError maps store errors to the API's codes.
func writePoolError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrPoolNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrRelayNotFound), errors.Is(err, ErrWorkspaceNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, errInvalidPoolName):
		provider.WriteError(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, errInvalidRegion):
		provider.WriteError(w, http.StatusBadRequest, "invalid_region")
	case errors.Is(err, errResidencyNeedsRegion):
		provider.WriteError(w, http.StatusBadRequest, "residency_needs_region")
	case errors.Is(err, errPoolNameTaken):
		provider.WriteError(w, http.StatusConflict, "name_taken")
	case errors.Is(err, errPoolIsDefault):
		provider.WriteError(w, http.StatusConflict, "default_pool")
	case errors.Is(err, errPoolInUse):
		provider.WriteError(w, http.StatusConflict, "pool_in_use")
	case errors.Is(err, errPoolNotEmpty):
		provider.WriteError(w, http.StatusConflict, "pool_not_empty")
	case errors.Is(err, errLastEligibleRelay):
		provider.WriteError(w, http.StatusConflict, "last_eligible_relay")
	case errors.Is(err, errPoolHasNoEligibleRelay):
		provider.WriteError(w, http.StatusConflict, "pool_has_no_eligible_relay")
	case errors.Is(err, errAlreadyDeleted):
		provider.WriteError(w, http.StatusConflict, "already_deleted")
	case errors.Is(err, errAlreadyRevoked):
		provider.WriteError(w, http.StatusConflict, "already_revoked")
	case errors.Is(err, errFailoverSameAsPrimary):
		provider.WriteError(w, http.StatusBadRequest, "failover_same_as_primary")
	case errors.Is(err, errFailoverRegionMismatch):
		provider.WriteError(w, http.StatusBadRequest, "failover_region_mismatch")
	case errors.Is(err, errFailoverNeedsResidency):
		provider.WriteError(w, http.StatusBadRequest, "failover_needs_residency")
	default:
		log.Printf("relay pools: %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

// pathUUID returns the {id} path value, or writes 404 when it isn't a UUID.
func pathUUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return "", false
	}
	return id, true
}

// requireReason validates a mandatory reason (D-41, D-17 style).
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

// bodyPoolID validates a pool id given in a body.
func bodyPoolID(w http.ResponseWriter, raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	if _, err := uuid.Parse(id); err != nil {
		provider.WriteError(w, http.StatusBadRequest, "pool_not_found")
		return "", false
	}
	return id, true
}

func (h *AdminHandler) publishAfterPoolChange(r *http.Request, what string) {
	if h.OnRelayPoolChanged == nil {
		return
	}
	ctx, cancel := fanoutContext(r)
	defer cancel()
	if err := h.OnRelayPoolChanged(ctx); err != nil {
		log.Printf("relay pools: %s: publish relay lists: %v (connectors get the current list at the next sync or reconnect)", what, err)
	}
}

type createPoolRequest struct {
	Name      string  `json:"name"`
	Region    *string `json:"region"`
	Residency bool    `json:"residency"`
}

// CreatePool: POST /provider/relay-pools.
func (h *AdminHandler) CreatePool(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr500(w, r, "create")
	if !ok {
		return
	}
	var req createPoolRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	target := provider.Target{Type: "relay_pool"}
	authz := h.Authz.CanCreateRelayPool(actor, target)
	if authz == nil && (req.Region != nil || req.Residency) {
		authz = h.Authz.CanUpdateRelayPool(actor, target, true)
	}
	if authz != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	name := strings.TrimSpace(req.Name)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	p, err := h.Store.CreatePool(ctx, name, req.Region, req.Residency, func(ctx context.Context, tx pgx.Tx, p Pool) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayPoolCreate,
			TargetType:     "relay_pool",
			TargetID:       p.ID,
			Details:        provider.WithChange(nil, nil, poolAudit(p)),
			IPAddress:      provider.ClientIP(r),
		})
	})
	if err != nil {
		writePoolError(w, "create pool", err)
		return
	}
	provider.WriteJSON(w, http.StatusCreated, poolJSON(p))
}

// UpdatePool: PATCH /provider/relay-pools/{id} {name?, region?, residency?}.
// "region": null clears the region. Region and residency changes are
// super-admin only and refused while a workspace uses the pool (D-37).
func (h *AdminHandler) UpdatePool(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr500(w, r, "update")
	if !ok {
		return
	}
	poolID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	change, ok := decodePoolChange(w, r)
	if !ok {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.Authz.CanUpdateRelayPool(actor, provider.Target{Type: "relay_pool", ID: poolID}, change.TouchesPlacement()); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	action := provider.ActionRelayPoolUpdate
	if change.TouchesPlacement() {
		action = provider.ActionRelayPoolSetPlacement
	}
	_, err := h.Store.UpdatePool(ctx, poolID, change, func(ctx context.Context, tx pgx.Tx, before, after Pool) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         action,
			TargetType:     "relay_pool",
			TargetID:       poolID,
			Details:        provider.WithChange(nil, poolAudit(before), poolAudit(after)),
			IPAddress:      provider.ClientIP(r),
		})
	})
	if err != nil {
		writePoolError(w, "update pool", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodePoolChange reads a PATCH body, telling an absent "region" from
// "region": null. Unknown fields are refused.
func decodePoolChange(w http.ResponseWriter, r *http.Request) (PoolChange, bool) {
	var raw map[string]json.RawMessage
	if !provider.DecodeJSON(w, r, &raw) {
		return PoolChange{}, false
	}
	var c PoolChange
	for k, v := range raw {
		switch k {
		case "name":
			var name string
			if err := json.Unmarshal(v, &name); err != nil {
				return PoolChange{}, false
			}
			name = strings.TrimSpace(name)
			c.Name = &name
		case "region":
			c.SetRegion = true
			if !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				var region string
				if err := json.Unmarshal(v, &region); err != nil {
					return PoolChange{}, false
				}
				c.Region = &region
			}
		case "residency":
			var res bool
			if err := json.Unmarshal(v, &res); err != nil {
				return PoolChange{}, false
			}
			c.Residency = &res
		default:
			return PoolChange{}, false
		}
	}
	return c, true
}

// DeletePool: DELETE /provider/relay-pools/{id}. Only an empty pool.
func (h *AdminHandler) DeletePool(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr500(w, r, "delete")
	if !ok {
		return
	}
	poolID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if err := h.Authz.CanDeleteRelayPool(actor, provider.Target{Type: "relay_pool", ID: poolID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := h.Store.DeletePool(ctx, poolID, func(ctx context.Context, tx pgx.Tx, p Pool) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayPoolDelete,
			TargetType:     "relay_pool",
			TargetID:       poolID,
			Details:        provider.WithChange(nil, poolAudit(p), nil),
			IPAddress:      provider.ClientIP(r),
		})
	})
	if err != nil {
		writePoolError(w, "delete pool", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type moveRelayRequest struct {
	PoolID string `json:"pool_id"`
	Reason string `json:"reason"`
}

// MoveRelay: POST /provider/relays/{id}/pool {pool_id, reason}.
func (h *AdminHandler) MoveRelay(w http.ResponseWriter, r *http.Request) {
	actor, relayID, ok := h.authorizeRelayAction(w, r, provider.ActionRelayMove, h.Authz.CanMoveRelay)
	if !ok {
		return
	}
	var req moveRelayRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	toPool, ok := bodyPoolID(w, req.PoolID)
	if !ok {
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	changed, err := h.Store.MoveRelay(ctx, relayID, toPool, func(ctx context.Context, tx pgx.Tx, from, to Pool) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayMove,
			TargetType:     "relay",
			TargetID:       relayID,
			Details: provider.WithChange(map[string]any{"reason": reason},
				map[string]any{"pool_id": from.ID, "pool_name": from.Name},
				map[string]any{"pool_id": to.ID, "pool_name": to.Name}),
			IPAddress: provider.ClientIP(r),
		})
	})
	if errors.Is(err, ErrPoolNotFound) {
		// The relay exists (its pool was locked); the named pool doesn't.
		provider.WriteError(w, http.StatusBadRequest, "pool_not_found")
		return
	}
	if err != nil {
		writePoolError(w, "move relay "+relayID, err)
		return
	}
	if changed {
		h.publishAfterPoolChange(r, "move relay "+relayID)
	}
	w.WriteHeader(http.StatusNoContent)
}

type assignPoolRequest struct {
	PoolID string `json:"pool_id"`
	// FailoverPoolID is the failover pool (D-39); absent or null means none.
	// The request is the whole assignment: omitting it removes a failover pool.
	FailoverPoolID *string `json:"failover_pool_id"`
	Reason         string  `json:"reason"`
	Migration      string  `json:"migration"`
}

// AssignTenantPool: PUT /provider/tenants/{id}/relay-pool {pool_id,
// failover_pool_id?, reason, migration: "graceful" (default) | "immediate"}.
// Super-admin only. The migration mode applies when the primary pool changes.
func (h *AdminHandler) AssignTenantPool(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorOr500(w, r, "assign")
	if !ok {
		return
	}
	workspaceID := r.PathValue("id")
	if err := h.Authz.CanAssignTenantRelayPool(actor, provider.Target{Type: "tenant", ID: workspaceID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	if _, err := uuid.Parse(workspaceID); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	var req assignPoolRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	toPool, ok := bodyPoolID(w, req.PoolID)
	if !ok {
		return
	}
	var failoverPool *string
	if req.FailoverPoolID != nil {
		id, ok := bodyPoolID(w, *req.FailoverPoolID)
		if !ok {
			return
		}
		failoverPool = &id
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	graceful := true
	switch req.Migration {
	case "", "graceful":
	case "immediate":
		graceful = false
	default:
		provider.WriteError(w, http.StatusBadRequest, "invalid_migration")
		return
	}
	window := h.PoolMigrationWindow
	if window <= 0 {
		window = DefaultPoolMigrationWindow
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	changed, err := h.Store.AssignWorkspacePool(ctx, workspaceID, toPool, failoverPool, graceful, window,
		func(ctx context.Context, tx pgx.Tx, before, after PoolAssignment) error {
			mode := "immediate"
			if graceful {
				mode = "graceful"
			}
			return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderUserID: &actor.UserID,
				ProviderEmail:  actor.Email,
				Action:         provider.ActionTenantRelayPoolAssign,
				TargetType:     "tenant",
				TargetID:       workspaceID,
				Details: provider.WithChange(map[string]any{"reason": reason, "migration": mode},
					assignmentAudit(before), assignmentAudit(after)),
				IPAddress: provider.ClientIP(r),
			})
		})
	if errors.Is(err, ErrPoolNotFound) {
		provider.WriteError(w, http.StatusBadRequest, "pool_not_found")
		return
	}
	if err != nil {
		writePoolError(w, "assign tenant pool "+workspaceID, err)
		return
	}
	if changed {
		h.publishAfterPoolChange(r, "assign tenant pool "+workspaceID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func assignmentAudit(a PoolAssignment) map[string]any {
	out := map[string]any{"relay_pool_id": a.PoolID, "failover_pool_id": a.FailoverPoolID}
	if a.PreviousPoolID != nil {
		out["previous_pool_id"] = *a.PreviousPoolID
		out["previous_pool_until"] = a.PreviousPoolUntil.UTC()
	}
	return out
}
