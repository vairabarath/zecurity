package provider

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// TOTP endpoints of the Provider Identity Service (Sprint 22 Phase T; D-28,
// D-33; OQ-1…OQ-3). Each sits behind RequireProvider with AllowMFAToken for
// its stage, so only the confined token issued after a correct password
// reaches it:
//
//	POST /provider/auth/totp/verify          mfa:"verify"  {code} → full session
//	POST /provider/auth/totp/enroll          mfa:"enroll"         → {secret, otpauth_uri} (once)
//	POST /provider/auth/totp/enroll/confirm  mfa:"enroll"  {code} → full session
//
// Full sessions are minted only by IdentityService.IssueSession, with amr
// ["pwd","otp"]. Secrets and codes are never logged, audited or stored in
// plaintext; the secret is sealed with the PKI master secret (SecretSealer).

const (
	AuditTOTPEnroll    = "provider_user.totp_enroll"
	AuditTOTPReset     = "provider_user.totp_reset"
	AuditRecoverAdmin  = "provider_user.recover_admin"
	AuditMFAVerify     = "provider_session.mfa_verify"
	AuditTOTPRateLimit = "provider_auth.totp_rate_limit"

	// SystemCLIActor is the provider_email of rows written by controller CLI
	// commands (recover-admin), which have no provider user.
	SystemCLIActor = "system:cli"

	maxResetReasonRunes = 500
)

// SecretSealer encrypts a secret at rest. *pki.serviceImpl implements it with
// AES-256-GCM + HKDF under PKI_MASTER_SECRET (pki.EncryptSecret).
type SecretSealer interface {
	EncryptSecret(plaintext []byte, context string) (ciphertextB64, nonceB64 string, err error)
	DecryptSecret(ciphertextB64, nonceB64, context string) ([]byte, error)
}

// totpContext domain-separates each operator's sealed TOTP secret.
func totpContext(userID string) string { return "provider-totp:" + userID }

// totpStore is what the TOTP handlers need from the provider store.
type totpStore interface {
	GetByID(ctx context.Context, id string) (*ProviderUser, error)
	GetTOTPState(ctx context.Context, id string) (*TOTPState, error)
	SetPendingTOTP(ctx context.Context, id, ciphertext, nonce string) error
	ConfirmTOTP(ctx context.Context, id string, step int64, ip string) (*ProviderUser, error)
	AcceptTOTPStep(ctx context.Context, id string, step int64) (bool, error)
	InsertAudit(ctx context.Context, e AuditEntry) error
}

// TOTPHandlers serves the TOTP verify and enrolment routes.
type TOTPHandlers struct {
	ids     *IdentityService
	store   totpStore
	sealer  SecretSealer
	limiter LoginLimiter // per-operator code failures (NewValkeyTOTPLimiter)
	now     func() time.Time
}

func NewTOTPHandlers(ids *IdentityService, store totpStore, sealer SecretSealer, limiter LoginLimiter) *TOTPHandlers {
	return &TOTPHandlers{ids: ids, store: store, sealer: sealer, limiter: limiter, now: time.Now}
}

type totpCodeRequest struct {
	Code string `json:"code"`
}

type totpEnrollResponse struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauth_uri"`
}

// Verify: POST /provider/auth/totp/verify {code}.
func (h *TOTPHandlers) Verify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, code, ip, ok := h.begin(w, r)
	if !ok {
		return
	}
	st, err := h.store.GetTOTPState(ctx, u.ID)
	if err != nil {
		log.Printf("provider totp verify: load state for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if st.EnabledAt == nil || st.Ciphertext == "" {
		writeAuthError(w, http.StatusConflict, "totp_not_enrolled")
		return
	}
	secret, err := h.sealer.DecryptSecret(st.Ciphertext, st.Nonce, totpContext(u.ID))
	if err != nil {
		log.Printf("provider totp verify: open secret for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	step, matched := matchTOTP(secret, code, h.now(), st.LastStep)
	if matched {
		// The UPDATE only succeeds for a step newer than the stored one, so two
		// concurrent requests can't both spend the same code.
		if matched, err = h.store.AcceptTOTPStep(ctx, u.ID, step); err != nil {
			log.Printf("provider totp verify: accept step for %s: %v", u.ID, err)
			writeAuthError(w, http.StatusInternalServerError, "server_error")
			return
		}
	}
	if !matched {
		h.recordFailure(w, ctx, u, ip)
		return
	}
	h.resetLimiter(ctx, u)
	token, expiresIn, err := h.ids.IssueSession(u, []string{AMRPassword, AMROTP}, false)
	if err != nil {
		log.Printf("provider totp verify: issue session: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	uid := u.ID
	if err := h.store.InsertAudit(ctx, AuditEntry{
		ProviderUserID: &uid,
		ProviderEmail:  u.Email,
		Action:         AuditMFAVerify,
		TargetType:     "provider_user",
		TargetID:       u.ID,
		Details:        map[string]any{"amr": []string{AMRPassword, AMROTP}},
		IPAddress:      ip,
	}); err != nil {
		log.Printf("provider totp verify: audit: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeAuthJSON(w, http.StatusOK, sessionResponse{Token: token, ExpiresIn: expiresIn})
}

// Enroll: POST /provider/auth/totp/enroll. Issues a new secret (replacing any
// pending one) and returns it exactly once.
func (h *TOTPHandlers) Enroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor, ok := ActorFromContext(ctx)
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "no provider actor in context")
		return
	}
	u, err := h.store.GetByID(ctx, actor.UserID)
	if err != nil {
		log.Printf("provider totp enroll: load %s: %v", actor.UserID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if u.TOTPEnabledAt != nil {
		writeAuthError(w, http.StatusConflict, "totp_already_enabled")
		return
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		log.Printf("provider totp enroll: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	ct, nonce, err := h.sealer.EncryptSecret(secret, totpContext(u.ID))
	if err != nil {
		log.Printf("provider totp enroll: seal secret for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := h.store.SetPendingTOTP(ctx, u.ID, ct, nonce); err != nil {
		if errors.Is(err, ErrTOTPAlreadyEnabled) {
			writeAuthError(w, http.StatusConflict, "totp_already_enabled")
			return
		}
		log.Printf("provider totp enroll: store pending secret for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeAuthJSON(w, http.StatusOK, totpEnrollResponse{
		Secret:     EncodeTOTPSecret(secret),
		OTPAuthURI: TOTPProvisioningURI(u.Email, secret),
	})
}

// ConfirmEnroll: POST /provider/auth/totp/enroll/confirm {code}. Enables the
// pending secret, ends every earlier session (generation bump) and returns a
// full session.
func (h *TOTPHandlers) ConfirmEnroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, code, ip, ok := h.begin(w, r)
	if !ok {
		return
	}
	st, err := h.store.GetTOTPState(ctx, u.ID)
	if err != nil {
		log.Printf("provider totp confirm: load state for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if st.EnabledAt != nil {
		writeAuthError(w, http.StatusConflict, "totp_already_enabled")
		return
	}
	if st.Ciphertext == "" {
		writeAuthError(w, http.StatusConflict, "totp_not_started")
		return
	}
	secret, err := h.sealer.DecryptSecret(st.Ciphertext, st.Nonce, totpContext(u.ID))
	if err != nil {
		log.Printf("provider totp confirm: open secret for %s: %v", u.ID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	step, matched := matchTOTP(secret, code, h.now(), nil)
	if !matched {
		h.recordFailure(w, ctx, u, ip)
		return
	}
	updated, err := h.store.ConfirmTOTP(ctx, u.ID, step, ip)
	if errors.Is(err, ErrTOTPNotStarted) {
		writeAuthError(w, http.StatusConflict, "totp_not_started")
		return
	}
	if err != nil {
		log.Printf("provider totp confirm: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	h.resetLimiter(ctx, u)
	token, expiresIn, err := h.ids.IssueSession(updated, []string{AMRPassword, AMROTP}, false)
	if err != nil {
		log.Printf("provider totp confirm: issue session: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return
	}
	writeAuthJSON(w, http.StatusOK, sessionResponse{Token: token, ExpiresIn: expiresIn})
}

// begin reads the actor and the code, checks the limiter and loads the user.
func (h *TOTPHandlers) begin(w http.ResponseWriter, r *http.Request) (*ProviderUser, string, string, bool) {
	ctx := r.Context()
	actor, ok := ActorFromContext(ctx)
	if !ok {
		writeAuthError(w, http.StatusInternalServerError, "no provider actor in context")
		return nil, "", "", false
	}
	var req totpCodeRequest
	if !decodeAuthBody(w, r, &req) || req.Code == "" {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return nil, "", "", false
	}
	ip := clientIP(r)
	if !checkLimiter(w, ctx, h.limiter, actor.Email, ip) {
		return nil, "", "", false
	}
	u, err := h.store.GetByID(ctx, actor.UserID)
	if err != nil {
		log.Printf("provider totp: load %s: %v", actor.UserID, err)
		writeAuthError(w, http.StatusInternalServerError, "server_error")
		return nil, "", "", false
	}
	return u, req.Code, ip, true
}

// recordFailure counts a wrong or replayed code, audits a lockout start once
// per tripped scope and answers 401 invalid_code — or 503 if it can't count
// (fail closed), like the login limiter.
func (h *TOTPHandlers) recordFailure(w http.ResponseWriter, ctx context.Context, u *ProviderUser, ip string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginLimiterTimeout)
	defer cancel()
	trips, err := h.limiter.RecordFailure(ctx, u.Email, ip)
	if err != nil {
		log.Printf("provider totp: record failure, failing closed: %v", err)
		writeAuthError(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	log.Printf("provider totp: invalid code for %s ip=%s", u.Email, ip)
	uid := u.ID
	for _, trip := range trips {
		if err := h.store.InsertAudit(ctx, AuditEntry{
			ProviderUserID: &uid,
			ProviderEmail:  u.Email,
			Action:         AuditTOTPRateLimit,
			TargetType:     "provider_user",
			TargetID:       u.ID,
			Details: map[string]any{
				"scope":               trip.Scope,
				"failures":            trip.Failures,
				"window_seconds":      int64(loginFailWindow / time.Second),
				"retry_after_seconds": int64(math.Ceil(trip.RetryAfter.Seconds())),
			},
			IPAddress: ip,
		}); err != nil {
			log.Printf("provider totp: audit rate limit: %v", err)
		}
	}
	writeAuthError(w, http.StatusUnauthorized, "invalid_code")
}

func (h *TOTPHandlers) resetLimiter(ctx context.Context, u *ProviderUser) {
	if err := h.limiter.Reset(ctx, u.Email); err != nil {
		log.Printf("provider totp: reset limiter for %s: %v", u.Email, err) // counter just expires
	}
}

type resetTOTPRequest struct {
	Reason string `json:"reason"`
}

// ResetTOTP: POST /provider/users/{id}/reset-totp {reason} (OQ-1). Super-admin
// only (provider_user.manage); the acting super-admin's session is TOTP-backed
// because RequireProvider demands it of every super-admin request. Refused for
// self and for a disabled operator; the reason is required and audited.
func (h *OperatorHandlers) ResetTOTP(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	var req resetTOTPRequest
	if !decodeAuthBody(w, r, &req) {
		writeAuthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeAuthError(w, http.StatusBadRequest, "reason_required")
		return
	}
	if utf8.RuneCountInString(reason) > maxResetReasonRunes {
		writeAuthError(w, http.StatusBadRequest, "reason_too_long")
		return
	}
	if id == actor.UserID {
		writeOperatorError(w, "reset totp", ErrCannotModifySelf)
		return
	}
	if err := h.store.ResetOperatorTOTP(r.Context(), actor, id, reason); err != nil {
		writeOperatorError(w, "reset totp", err)
		return
	}
	log.Printf("provider operator: totp reset for %s by %s", id, actor.Email)
	w.WriteHeader(http.StatusNoContent)
}

// recoverStore is what RecoverAdmin needs from the provider store.
type recoverStore interface {
	RecoverAdmin(ctx context.Context, email, passwordHash string) (bool, error)
}

// RecoverAdmin is the break-glass path behind `zecurity-controller provider
// recover-admin --email` (ADR-029): it gives an active super-admin a new
// temporary password (returned once, for the CLI to print) and clears their
// TOTP, so they change the password and enrol again at next sign-in.
func RecoverAdmin(ctx context.Context, store recoverStore, email string) (string, error) {
	password, hash, err := newTemporaryCredential(ctx)
	if err != nil {
		return "", err
	}
	if _, err := store.RecoverAdmin(ctx, email, hash); err != nil {
		return "", err
	}
	return password, nil
}
