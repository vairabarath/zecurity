package mfa

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Console MFA endpoints (PENDING-06 M-6, M-7). They sit behind the normal
// workspace AuthMiddleware but NOT behind the console-MFA requirement: they
// are how a session gets its MFA mark.
//
//	GET  /auth/mfa/status  → {enrolled, sessionMfaAt, consoleRequireMfa}
//	POST /auth/mfa/enroll  → {secret, uri}            (once; Cache-Control: no-store)
//	POST /auth/mfa/verify  → {accessToken, expiresIn, enrolled}
//
// verify confirms a pending enrolment on its first correct code, stamps the
// caller's session and returns a fresh access token that carries the mark.

// SessionStamper is the part of auth.Service the handlers need.
type SessionStamper interface {
	VerifyAccessToken(token string) (*auth.AccessTokenClaims, error)
	StampSessionMFA(ctx context.Context, claims *auth.AccessTokenClaims, at time.Time) (string, int64, error)
}

// AuditFunc records one audit entry (best effort).
type AuditFunc func(ctx context.Context, e audit.Entry)

// Audit actions.
const (
	ActionEnrolled = "mfa.enrolled"
	ActionVerified = "mfa.verified"
	ActionLocked   = "mfa.locked"
	ActionReset    = "mfa.reset"
)

type Handlers struct {
	svc      *Service
	sessions SessionStamper
	console  *ConsolePolicy
	audit    AuditFunc
}

func NewHandlers(svc *Service, sessions SessionStamper, console *ConsolePolicy, auditFn AuditFunc) *Handlers {
	if auditFn == nil {
		auditFn = func(context.Context, audit.Entry) {}
	}
	return &Handlers{svc: svc, sessions: sessions, console: console, audit: auditFn}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenant.Get(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	enrolled, err := h.svc.Enrolled(r.Context(), tc.UserID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}
	required, err := h.console.Required(r.Context(), tc.TenantID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}
	out := map[string]any{"enrolled": enrolled, "consoleRequireMfa": required, "sessionMfaAt": nil}
	if !tc.MFAAt.IsZero() {
		out["sessionMfaAt"] = tc.MFAAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) Enroll(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenant.Get(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	enr, err := h.svc.StartEnrollment(r.Context(), tc.UserID, tc.TenantID, tc.Email)
	if errors.Is(err, ErrAlreadyEnrolled) {
		writeErr(w, http.StatusConflict, "ALREADY_ENROLLED", "an authenticator is already set up; ask an admin to reset it")
		return
	}
	if err != nil {
		log.Printf("mfa: start enrollment for user %s: %v", tc.UserID, err)
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}
	// The only time the secret leaves the server.
	writeJSON(w, http.StatusOK, map[string]string{"secret": enr.Secret, "uri": enr.URI})
}

func (h *Handlers) Verify(w http.ResponseWriter, r *http.Request) {
	tc, ok := tenant.Get(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	// The session to stamp comes from the verified bearer token, not the body.
	claims, err := h.sessions.VerifyAccessToken(bearer(r))
	if err != nil || claims.UserID != tc.UserID {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	res, failure, err := h.svc.Verify(r.Context(), tc.UserID, body.Code)
	switch {
	case err == nil:
	case errors.Is(err, ErrInvalidCode):
		if failure.Tripped {
			h.audit(r.Context(), audit.Entry{
				TenantID: tc.TenantID, ActorUserID: tc.UserID, ActorEmail: tc.Email,
				Action: ActionLocked, TargetType: "user", TargetID: tc.UserID,
				Details: map[string]any{"client": false},
			})
		}
		writeErr(w, http.StatusUnauthorized, "INVALID_CODE", "that code is not valid")
		return
	case errors.Is(err, ErrRateLimited):
		if failure.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(failure.RetryAfter.Seconds())+1))
		}
		writeErr(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many wrong codes; try again later")
		return
	case errors.Is(err, ErrNotEnrolled):
		writeErr(w, http.StatusConflict, "NOT_ENROLLED", "set up an authenticator first")
		return
	default:
		// Fail closed.
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}

	token, expiresIn, err := h.sessions.StampSessionMFA(r.Context(), claims, time.Now())
	if errors.Is(err, auth.ErrSessionNotFound) {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "session has ended; sign in again")
		return
	}
	if err != nil {
		log.Printf("mfa: stamp session for user %s: %v", tc.UserID, err)
		writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}
	action := ActionVerified
	if res.Enrolled {
		action = ActionEnrolled
	}
	h.audit(r.Context(), audit.Entry{
		TenantID: tc.TenantID, ActorUserID: tc.UserID, ActorEmail: tc.Email,
		Action: action, TargetType: "user", TargetID: tc.UserID,
		Details: map[string]any{"method": "totp", "client": false},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": token, "expiresIn": expiresIn, "enrolled": res.Enrolled,
	})
}

func bearer(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}
