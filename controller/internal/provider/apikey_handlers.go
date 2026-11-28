package provider

// Provider API key routes (Sprint 27 K1, F35; D-76, D-30 as amended).
// Super-admin only (api_key.* has no role row), session only (RequireProvider
// without AcceptAPIKeys refuses a key with 403). Every response is
// Cache-Control: no-store; the full key appears only in the create response.
//
//	GET  /provider/api-keys                    → {items: [APIKey]}   (no secret, no hash)
//	POST /provider/api-keys                    {name, role, expires_at, allowed_cidrs?, reason} → 201 {api_key, key}
//	POST /provider/api-keys/{id}/revoke        {reason, confirm_name} → 200 {api_key}

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// APIKeyHandlers serves the API key routes.
type APIKeyHandlers struct {
	Store *Store
	Authz *Authz
	Now   func() time.Time
}

func (h *APIKeyHandlers) begin(w http.ResponseWriter, r *http.Request, action string) (Actor, bool) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		WriteError(w, http.StatusInternalServerError, "server_error")
		return Actor{}, false
	}
	// Session only (D-76): a key never manages keys, even if a route were
	// mounted with AcceptAPIKeys by mistake.
	if actor.IsAPIKey() || h.Authz.CanManageAPIKeys(actor, action) != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return Actor{}, false
	}
	return actor, true
}

// List: GET /provider/api-keys.
func (h *APIKeyHandlers) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, ActionAPIKeyRead); !ok {
		return
	}
	keys, err := h.Store.ListAPIKeys(r.Context())
	if err != nil {
		log.Printf("api keys: list: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": keys})
}

type createAPIKeyRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	ExpiresAt    string   `json:"expires_at"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	Reason       string   `json:"reason"`
}

// Create: POST /provider/api-keys. The key is in this response only.
func (h *APIKeyHandlers) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.begin(w, r, ActionAPIKeyCreate)
	if !ok {
		return
	}
	var req createAPIKeyRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_expires_at")
		return
	}
	in, err := ValidateNewAPIKey(req.Name, req.Role, exp, h.now(), req.AllowedCIDRs)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	in.Reason = reason
	key, full, err := h.Store.CreateAPIKey(r.Context(), AuditActor{UserID: actor.UserID, Email: actor.Email, IP: clientIP(r)}, in)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"api_key": key, "key": full})
}

type revokeAPIKeyRequest struct {
	Reason      string `json:"reason"`
	ConfirmName string `json:"confirm_name"`
}

// Revoke: POST /provider/api-keys/{id}/revoke.
func (h *APIKeyHandlers) Revoke(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.begin(w, r, ActionAPIKeyRevoke)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !providerquery.IsUUID(id) {
		WriteError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	var req revokeAPIKeyRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	reason, ok := invoiceReason(w, req.Reason, true)
	if !ok {
		return
	}
	key, err := h.Store.RevokeAPIKey(r.Context(), AuditActor{UserID: actor.UserID, Email: actor.Email, IP: clientIP(r)}, id, reason, req.ConfirmName)
	if err != nil {
		writeAPIKeyError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"api_key": key})
}

func (h *APIKeyHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func writeAPIKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAPIKeyInvalidName):
		WriteError(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, ErrAPIKeyInvalidRole):
		WriteError(w, http.StatusBadRequest, "invalid_role")
	case errors.Is(err, ErrAPIKeyInvalidExpiry):
		WriteError(w, http.StatusBadRequest, "invalid_expires_at")
	case errors.Is(err, ErrAPIKeyInvalidCIDR):
		WriteError(w, http.StatusBadRequest, "invalid_cidr")
	case errors.Is(err, ErrAPIKeyNameMismatch):
		WriteError(w, http.StatusBadRequest, "confirmation_mismatch")
	case errors.Is(err, ErrAPIKeyNameTaken):
		WriteError(w, http.StatusConflict, "name_taken")
	case errors.Is(err, ErrAPIKeyRevoked):
		WriteError(w, http.StatusConflict, "already_revoked")
	case errors.Is(err, ErrAPIKeyNotFound):
		WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrAPIKeyCreatorNotActive):
		WriteError(w, http.StatusForbidden, "forbidden")
	default:
		log.Printf("api keys: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
	}
}
