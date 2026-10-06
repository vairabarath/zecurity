package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
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
// last_login_at (Sprint 21 C-a) is the timestamp of the CURRENT successful
// sign-in — the login handler records it when the session is issued — so the
// provider console shows it as "Signed in at", not as a previous login.
//
// GET /provider/me
func (h *Handlers) Me(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "no provider actor in context"})
		return
	}
	// The Actor carries only id/email/role; read the timestamp from the row.
	u, err := h.store.GetByID(r.Context(), actor.UserID)
	if err != nil {
		writeHandlerJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider lookup failed"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeHandlerJSON(w, http.StatusOK, meResponse{
		UserID:      actor.UserID,
		Email:       actor.Email,
		Role:        actor.Role,
		LastLoginAt: u.LastLoginAt,
	})
}

// meResponse is the GET /provider/me body.
type meResponse struct {
	UserID      string     `json:"user_id"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	LastLoginAt *time.Time `json:"last_login_at"`
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
	// Explicit API shape (Phase U): snake_case, no password hash or session
	// generation — never the raw ProviderUser struct.
	views := make([]OperatorView, 0, len(users))
	for i := range users {
		views = append(views, operatorViewOf(&users[i]))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeHandlerJSON(w, http.StatusOK, views)
}

func writeHandlerJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
