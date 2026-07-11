package provider

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Handlers serves the provider-plane REST endpoints that sit behind
// RequireProvider. Every method assumes RequireProvider already put an Actor in
// the context — a missing Actor is a wiring bug (a handler registered without the
// middleware), NOT an anonymous request, so it fails 500, never open.
type Handlers struct {
	store *Store
	authz *Authz
}

func NewHandlers(store *Store, authz *Authz) *Handlers {
	return &Handlers{store: store, authz: authz}
}

// Me returns the calling provider's own identity. No authz beyond
// RequireProvider: any authenticated provider user may see who they are.
//
// GET /provider/me
func (h *Handlers) Me(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "no provider actor in context"})
		return
	}
	writeHandlerJSON(w, http.StatusOK, map[string]string{
		"user_id": actor.UserID,
		"email":   actor.Email,
		"role":    actor.Role,
	})
}

// ListUsers returns the provider user roster. Guarded by CanManageProviderUser,
// so relay-ops receives 403 and only super-admin sees the list. This is the
// canonical "authz chokepoint in a handler" pattern M2 mirrors for relay routes.
//
// GET /provider/users
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "no provider actor in context"})
		return
	}
	if err := h.authz.CanManageProviderUser(actor, Target{Type: "provider_user"}); err != nil {
		if errors.Is(err, ErrForbidden) {
			writeHandlerJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "authorization check failed"})
		return
	}
	users, err := h.store.List(r.Context())
	if err != nil {
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "list provider users failed"})
		return
	}
	writeHandlerJSON(w, http.StatusOK, users)
}

func writeHandlerJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
