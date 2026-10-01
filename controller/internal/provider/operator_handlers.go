package provider

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Operator management API (Sprint 21 Phase U; D-27). All routes sit behind
// RequireProvider and are authorized with CanManageProviderUser
// (provider_user.manage → super-admin only):
//
//	POST  /provider/users                      {email, role} → 201 {user, temporary_password}
//	PATCH /provider/users/{id}                 {role}        → 200 {user}
//	POST  /provider/users/{id}/disable                       → 204
//	POST  /provider/users/{id}/enable                        → 204 (clears the credential; no password issued)
//	POST  /provider/users/{id}/reset-password                → 200 {temporary_password}
//
// A temporary password is generated and hashed HERE, returned exactly once in
// the create/reset response (Cache-Control: no-store), and never logged,
// audited or passed to the store in plaintext.

// operatorStore is what the operator handlers need from the provider store.
type operatorStore interface {
	GetByID(ctx context.Context, id string) (*ProviderUser, error)
	CreateOperator(ctx context.Context, actor AuditActor, email, role, passwordHash string) (*ProviderUser, error)
	ChangeRole(ctx context.Context, actor AuditActor, id, role string) (*ProviderUser, bool, error)
	SetDisabled(ctx context.Context, actor AuditActor, id string, disabled bool) (bool, error)
	ResetOperatorPassword(ctx context.Context, actor AuditActor, id, passwordHash string) error
}

// OperatorHandlers serves the operator management routes.
type OperatorHandlers struct {
	store operatorStore
	authz *Authz
}

func NewOperatorHandlers(store operatorStore, authz *Authz) *OperatorHandlers {
	return &OperatorHandlers{store: store, authz: authz}
}

// OperatorView is the API shape of a provider operator. It never contains the
// password hash or the session generation.
type OperatorView struct {
	ID                 string     `json:"id"`
	Email              string     `json:"email"`
	Role               string     `json:"role"`
	DisabledAt         *time.Time `json:"disabled_at"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	MustChangePassword bool       `json:"must_change_password"`
	HasPassword        bool       `json:"has_password"`
}

func operatorViewOf(u *ProviderUser) OperatorView {
	return OperatorView{
		ID:                 u.ID,
		Email:              u.Email,
		Role:               u.Role,
		DisabledAt:         u.DisabledAt,
		CreatedAt:          u.CreatedAt,
		LastLoginAt:        u.LastLoginAt,
		MustChangePassword: u.MustChangePassword,
		HasPassword:        u.PasswordHash != "",
	}
}

type createOperatorRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type createOperatorResponse struct {
	User              OperatorView `json:"user"`
	TemporaryPassword string       `json:"temporary_password"`
}

type changeRoleRequest struct {
	Role string `json:"role"`
}

type resetPasswordResponse struct {
	TemporaryPassword string `json:"temporary_password"`
}

// authorize returns the acting super-admin, or writes 403/500 and returns false.
func (h *OperatorHandlers) authorize(w http.ResponseWriter, r *http.Request) (AuditActor, bool) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "no provider actor in context")
		return AuditActor{}, false
	}
	if err := h.authz.CanManageProviderUser(actor, Target{Type: "provider_user"}); err != nil {
		if errors.Is(err, ErrForbidden) {
			writeAuthError(w, http.StatusForbidden, "forbidden")
			return AuditActor{}, false
		}
		writeAuthError(w, http.StatusInternalServerError, "authorization check failed")
		return AuditActor{}, false
	}
	return AuditActor{UserID: actor.UserID, Email: actor.Email, IP: clientIP(r)}, true
}

// targetID returns the {id} path value if it is a UUID; otherwise it writes 404
// (so a malformed id never reaches Postgres as a cast error).
func targetID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeAuthError(w, http.StatusNotFound, "not_found")
		return "", false
	}
	return id, true
}

// newTemporaryCredential generates a temporary password and its Argon2id hash.
func newTemporaryCredential(ctx context.Context) (password, hash string, err error) {
	if password, err = GenerateTemporaryPassword(); err != nil {
		return "", "", err
	}
	if hash, err = HashPassword(ctx, password); err != nil {
		return "", "", err
	}
	return password, hash, nil
}

// Create: POST /provider/users {email, role}.
func (h *OperatorHandlers) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	var req createOperatorRequest
	if !decodeAuthBody(w, r, &req) {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	email, err := NormalizeOperatorEmail(req.Email)
	if err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_email")
		return
	}
	if !ValidRole(req.Role) {
		writeAuthError(w, http.StatusBadRequest, "invalid_role")
		return
	}
	password, hash, err := newTemporaryCredential(r.Context())
	if err != nil {
		writeOperatorError(w, "create", err)
		return
	}
	u, err := h.store.CreateOperator(r.Context(), actor, email, req.Role, hash)
	if err != nil {
		writeOperatorError(w, "create", err)
		return
	}
	log.Printf("provider operator: created %s (%s) by %s", u.Email, u.Role, actor.Email)
	writeAuthJSON(w, http.StatusCreated, createOperatorResponse{User: operatorViewOf(u), TemporaryPassword: password})
}

// ChangeRole: PATCH /provider/users/{id} {role}.
func (h *OperatorHandlers) ChangeRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	var req changeRoleRequest
	if !decodeAuthBody(w, r, &req) {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !ValidRole(req.Role) {
		writeAuthError(w, http.StatusBadRequest, "invalid_role")
		return
	}
	u, changed, err := h.store.ChangeRole(r.Context(), actor, id, req.Role)
	if err != nil {
		writeOperatorError(w, "change role", err)
		return
	}
	if changed {
		log.Printf("provider operator: role of %s changed to %s by %s", u.Email, u.Role, actor.Email)
	}
	writeAuthJSON(w, http.StatusOK, map[string]OperatorView{"user": operatorViewOf(u)})
}

// Disable: POST /provider/users/{id}/disable.
func (h *OperatorHandlers) Disable(w http.ResponseWriter, r *http.Request) {
	h.setDisabled(w, r, true)
}

// Enable: POST /provider/users/{id}/enable. Clears the old credential; the
// operator cannot sign in until a super-admin performs reset-password.
func (h *OperatorHandlers) Enable(w http.ResponseWriter, r *http.Request) {
	h.setDisabled(w, r, false)
}

func (h *OperatorHandlers) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	changed, err := h.store.SetDisabled(r.Context(), actor, id, disabled)
	if err != nil {
		writeOperatorError(w, verb, err)
		return
	}
	if changed {
		log.Printf("provider operator: %sd %s by %s", verb, id, actor.Email)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// ResetPassword: POST /provider/users/{id}/reset-password. Refused for self
// (use POST /provider/auth/password) and for a disabled account — checked
// BEFORE any password is generated; the store re-checks authoritatively.
func (h *OperatorHandlers) ResetPassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	if id == actor.UserID {
		writeOperatorError(w, "reset password", ErrCannotModifySelf)
		return
	}
	target, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		writeOperatorError(w, "reset password", err)
		return
	}
	if target.DisabledAt != nil {
		writeOperatorError(w, "reset password", ErrAccountDisabled)
		return
	}
	password, hash, err := newTemporaryCredential(r.Context())
	if err != nil {
		writeOperatorError(w, "reset password", err)
		return
	}
	if err := h.store.ResetOperatorPassword(r.Context(), actor, id, hash); err != nil {
		writeOperatorError(w, "reset password", err)
		return
	}
	log.Printf("provider operator: password reset for %s by %s", target.Email, actor.Email)
	writeAuthJSON(w, http.StatusOK, resetPasswordResponse{TemporaryPassword: password})
}

// writeOperatorError maps store/credential errors to API error codes.
func writeOperatorError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrProviderUserNotFound):
		writeAuthError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrCannotModifySelf):
		writeAuthError(w, http.StatusConflict, "cannot_modify_self")
	case errors.Is(err, ErrLastSuperAdmin):
		writeAuthError(w, http.StatusConflict, "last_super_admin")
	case errors.Is(err, ErrAlreadyExists):
		writeAuthError(w, http.StatusConflict, "already_exists")
	case errors.Is(err, ErrExistsDisabled):
		writeAuthError(w, http.StatusConflict, "exists_disabled")
	case errors.Is(err, ErrAccountDisabled):
		writeAuthError(w, http.StatusConflict, "account_disabled")
	case errors.Is(err, ErrInvalidRole):
		writeAuthError(w, http.StatusBadRequest, "invalid_role")
	case errors.Is(err, ErrInvalidEmail):
		writeAuthError(w, http.StatusBadRequest, "invalid_email")
	case errors.Is(err, ErrHashBusy), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAuthError(w, http.StatusServiceUnavailable, "service_unavailable")
	default:
		log.Printf("provider operator: %s failed: %v", op, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
	}
}
