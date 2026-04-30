package invitation

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Handler exposes the invitation REST endpoints.
// Routes (registered in main.go):
//
//	POST /api/invitations              — create (JWT required; admin-only enforced in Phase 3)
//	GET  /api/invitations/{token}      — get by token (public)
//	POST /api/invitations/{token}/accept — accept (JWT required)
type Handler struct {
	store   *Store
	emailer *Emailer
}

func NewHandler(store *Store, emailer *Emailer) *Handler {
	return &Handler{store: store, emailer: emailer}
}

// invitationResponse is the JSON shape returned for all invitation endpoints.
type invitationResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Status        string `json:"status"`
	WorkspaceName string `json:"workspace_name"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     string `json:"created_at"`
}

func toResponse(inv *Invitation) invitationResponse {
	return invitationResponse{
		ID:            inv.ID,
		Email:         inv.Email,
		Status:        inv.Status,
		WorkspaceName: inv.WorkspaceName,
		ExpiresAt:     inv.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt:     inv.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// Create handles POST /api/invitations.
// Requires JWT (admin-only enforced in Phase 3 via RequireRole middleware).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenant.Get(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		writeJSONError(w, http.StatusBadRequest, "email is required")
		return
	}

	inv, err := h.store.CreateInvitation(r.Context(), body.Email, tc.TenantID, tc.UserID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create invitation")
		return
	}

	// Fetch workspace name for the email — best-effort; dev-mode emailer logs anyway.
	workspaceName := inv.WorkspaceName
	if workspaceName == "" {
		workspaceName = "your workspace"
	}
	go h.emailer.SendInvitation(inv, workspaceName) //nolint:errcheck

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toResponse(inv)) //nolint:errcheck
}

// Get handles GET /api/invitations/{token} — public, no auth.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}

	inv, err := h.store.GetByToken(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "invitation not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	if inv.Status != "pending" || time.Now().After(inv.ExpiresAt) {
		writeJSONError(w, http.StatusNotFound, "invitation not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toResponse(inv)) //nolint:errcheck
}

// Accept handles POST /api/invitations/{token}/accept.
// The caller must be authenticated with a JWT issued for the invited workspace.
// This endpoint is called by the frontend after the web OAuth callback completes
// and the user has a session in the invited workspace.
func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenant.Get(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	token := r.PathValue("token")
	if token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}

	if err := h.store.AcceptInvitation(r.Context(), token, tc.TenantID, tc.UserID, tc.Email); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "invitation not found, already used, or expired")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "accept failed")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
