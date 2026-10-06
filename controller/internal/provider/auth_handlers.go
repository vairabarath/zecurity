package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Auth endpoints of the Provider Identity Service (Sprint 21 Phase H):
//
//	POST /provider/auth/login     — public, rate-limited
//	POST /provider/auth/password  — RequireProvider, password-change-only tokens allowed
//	POST /provider/auth/logout    — RequireProvider
//
// Passwords are never logged, returned or audited.

const maxAuthBodyBytes = 4 << 10

// authStore is what the auth handlers need from the provider store.
type authStore interface {
	GetByID(ctx context.Context, id string) (*ProviderUser, error)
	SetPassword(ctx context.Context, id, passwordHash string, mustChange bool) (*ProviderUser, error)
	BumpSessionGeneration(ctx context.Context, id string) error
	RecordLogin(ctx context.Context, id string) error
	InsertAudit(ctx context.Context, e AuditEntry) error
}

// AuthHandlers serves the provider login, password-change and logout routes.
type AuthHandlers struct {
	ids     *IdentityService
	authn   Authenticator
	store   authStore
	limiter LoginLimiter
}

func NewAuthHandlers(ids *IdentityService, authn Authenticator, store authStore, limiter LoginLimiter) *AuthHandlers {
	return &AuthHandlers{ids: ids, authn: authn, store: store, limiter: limiter}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type sessionResponse struct {
	Token                  string `json:"token"`
	ExpiresIn              int64  `json:"expires_in"`
	PasswordChangeRequired bool   `json:"password_change_required"`
}

// Login: POST /provider/auth/login {email, password}.
func (h *AuthHandlers) Login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req loginRequest
	if !decodeAuthBody(w, r, &req) || req.Email == "" || req.Password == "" {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ip := clientIP(r)

	if !h.checkLimiter(w, ctx, req.Email, ip) {
		return
	}

	u, err := h.authn.Authenticate(ctx, Credentials{Email: req.Email, Password: req.Password})
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		h.recordFailure(w, ctx, req.Email, ip, "invalid credentials")
		return
	case errors.Is(err, ErrHashBusy), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	case err != nil:
		log.Printf("provider login: authenticate: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}

	if err := h.limiter.Reset(ctx, req.Email); err != nil {
		log.Printf("provider login: reset limiter for %s: %v", u.Email, err) // counter just expires
	}
	if err := h.store.RecordLogin(ctx, u.ID); err != nil {
		log.Printf("provider login: record last login for %s: %v", u.ID, err)
	}

	token, expiresIn, err := h.ids.IssueSession(u, []string{h.authn.Method()}, u.MustChangePassword)
	if err != nil {
		log.Printf("provider login: issue session: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	uid := u.ID
	if err := h.store.InsertAudit(ctx, AuditEntry{
		ProviderUserID: &uid,
		ProviderEmail:  u.Email,
		Action:         AuditSessionLogin,
		TargetType:     "provider_user",
		TargetID:       u.ID,
		Details:        map[string]any{"amr": []string{h.authn.Method()}, "password_change_required": u.MustChangePassword},
		IPAddress:      ip,
	}); err != nil {
		log.Printf("provider login: audit: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeAuthJSON(w, http.StatusOK, sessionResponse{Token: token, ExpiresIn: expiresIn, PasswordChangeRequired: u.MustChangePassword})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword: POST /provider/auth/password {current_password, new_password}.
// Accepts full and password-change-only tokens. The new hash is stored with a
// session_generation bump, so every earlier token — including the caller's —
// stops working; the response carries a fresh full token.
func (h *AuthHandlers) ChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := ActorFromContext(ctx)
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "no provider actor in context")
		return
	}
	var req changePasswordRequest
	if !decodeAuthBody(w, r, &req) || req.CurrentPassword == "" || req.NewPassword == "" {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ip := clientIP(r)

	// The current password is a credential too: guessing it here counts
	// toward the same lockout as the login endpoint.
	if !h.checkLimiter(w, ctx, actor.Email, ip) {
		return
	}

	u, err := h.store.GetByID(ctx, actor.UserID)
	if err != nil {
		log.Printf("provider password change: load %s: %v", actor.UserID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	match := false
	if u.PasswordHash != "" {
		match, err = VerifyPassword(ctx, req.CurrentPassword, u.PasswordHash)
		if errors.Is(err, ErrHashBusy) {
			writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
			return
		}
		if err != nil && !errors.Is(err, errMalformedHash) {
			log.Printf("provider password change: verify: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "server_error")
			return
		}
	}
	if !match {
		h.recordFailure(w, ctx, actor.Email, ip, "wrong current password")
		return
	}
	if err := ValidateNewPassword(req.NewPassword, u.Email, req.CurrentPassword); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "password_policy", "detail": err.Error()})
		return
	}
	hash, err := HashPassword(ctx, req.NewPassword)
	if errors.Is(err, ErrHashBusy) {
		writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	if err != nil {
		log.Printf("provider password change: hash: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	updated, err := h.store.SetPassword(ctx, u.ID, hash, false)
	if err != nil {
		log.Printf("provider password change: store: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	uid := u.ID
	if err := h.store.InsertAudit(ctx, AuditEntry{
		ProviderUserID: &uid,
		ProviderEmail:  u.Email,
		Action:         AuditPasswordChange,
		TargetType:     "provider_user",
		TargetID:       u.ID,
		Details:        map[string]any{"was_required": u.MustChangePassword},
		IPAddress:      ip,
	}); err != nil {
		log.Printf("provider password change: audit: %v", err)
	}
	token, expiresIn, err := h.ids.IssueSession(updated, []string{AMRPassword}, false)
	if err != nil {
		log.Printf("provider password change: issue session: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeAuthJSON(w, http.StatusOK, sessionResponse{Token: token, ExpiresIn: expiresIn})
}

// Logout: POST /provider/auth/logout. Bumps session_generation, so every
// token the operator holds (all browsers/devices) is rejected next request.
func (h *AuthHandlers) Logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := ActorFromContext(ctx)
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "no provider actor in context")
		return
	}
	if err := h.store.BumpSessionGeneration(ctx, actor.UserID); err != nil {
		log.Printf("provider logout: bump generation for %s: %v", actor.UserID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	uid := actor.UserID
	if err := h.store.InsertAudit(ctx, AuditEntry{
		ProviderUserID: &uid,
		ProviderEmail:  actor.Email,
		Action:         AuditSessionLogout,
		TargetType:     "provider_user",
		TargetID:       actor.UserID,
		IPAddress:      clientIP(r),
	}); err != nil {
		log.Printf("provider logout: audit: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkLimiter writes 429 (locked out) or 503 (limiter unavailable — fail
// closed) and returns false; true means the attempt may proceed.
func (h *AuthHandlers) checkLimiter(w http.ResponseWriter, ctx context.Context, email, ip string) bool {
	blocked, retry, err := h.limiter.Blocked(ctx, email, ip)
	if err != nil {
		log.Printf("provider auth: login limiter unavailable, failing closed: %v", err)
		writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
		return false
	}
	if blocked {
		log.Printf("provider auth: rejected locked-out attempt email=%s ip=%s", normalizeEmail(email), ip)
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		writeAuthError(w, http.StatusTooManyRequests, "too_many_attempts")
		return false
	}
	return true
}

// recordFailure counts a failed credential check, audits a lockout start once
// per tripped scope, and answers 401 — or 503 if the failure could not be
// counted (fail closed).
func (h *AuthHandlers) recordFailure(w http.ResponseWriter, ctx context.Context, email, ip, reason string) {
	trips, err := h.limiter.RecordFailure(ctx, email, ip)
	if err != nil {
		log.Printf("provider auth: record failure, failing closed: %v", err)
		writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	log.Printf("provider auth: %s email=%s ip=%s", reason, normalizeEmail(email), ip)
	for _, trip := range trips {
		if err := h.store.InsertAudit(ctx, AuditEntry{
			ProviderEmail: normalizeEmail(email),
			Action:        AuditRateLimit,
			TargetType:    "provider_login",
			TargetID:      trip.Scope + ":" + trip.Value,
			Details: map[string]any{
				"scope":               trip.Scope,
				"failures":            trip.Failures,
				"window_seconds":      int64(loginFailWindow / time.Second),
				"retry_after_seconds": int64(math.Ceil(trip.RetryAfter.Seconds())),
			},
			IPAddress: ip,
		}); err != nil {
			log.Printf("provider auth: audit rate limit: %v", err)
		}
	}
	writeAuthError(w, http.StatusUnauthorized, "invalid_credentials")
}

func decodeAuthBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst) == nil
}

// clientIP is the connection's remote host. X-Forwarded-For is deliberately
// NOT trusted (no trusted-proxy configuration exists yet).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAuthError(w http.ResponseWriter, status int, code string) {
	writeAuthJSON(w, status, map[string]string{"error": code})
}
