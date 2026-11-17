package client

// Step-up authentication for an enrolled device (PENDING-06 M-5).
//
//	zecurity-client auth
//	  → StartReauth (gRPC)            → IdP authorization URL
//	  → browser → IdP → GET /api/clients/callback (the sign-in callback; the
//	    stored auth session says this round trip is a step-up)
//	  → same user?                    → no: refused, audited
//	  → MFA needed?                   → no: stamp the device's session, done
//	  → TOTP page (enrol on first use) → POST /api/clients/reauth/mfa
//	  → stamp the device's session, recompile the ACL
//
// The device is NOT re-enrolled: only its session's authentication times
// (client_devices.session_auth_at / session_mfa_at) move, and its risk score
// is cleared. The ACL compiler reads those columns (policy.DeviceAuthenticated).
//
// The step-up reuses the sign-in callback URL on purpose: the IdP only
// redirects to URIs registered with it, and a second URI would have to be
// added to every identity connection.
//
// State is process-local (single controller instance, D-02), like the
// sign-in auth sessions in auth_session.go.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/mfa"
	"github.com/yourorg/ztna/controller/internal/risk"
)

// Audit actions of the step-up flow.
const (
	ActionStepUp       = "auth.step_up"
	ActionStepUpFailed = "auth.step_up_failed"
	ActionMFAEnrolled  = "mfa.enrolled"
	ActionMFALocked    = "mfa.locked"
)

const (
	reauthSessionTTL = 10 * time.Minute
	mfaTicketTTL     = 10 * time.Minute
)

// reauthInfo marks an auth session as a step-up for one enrolled device.
type reauthInfo struct {
	UserID   string
	DeviceID string
	SID      string // the device's login session
	Email    string
	// StartedAt is when StartReauth ran. The IdP's MFA counts only when its
	// auth_time is not older than this (see idpMFAFresh).
	StartedAt time.Time

	// The client that started the step-up (finding: step-up phishing). The
	// browser result goes to its loopback address as a one-time code, and
	// nothing is stamped until it redeems the code with its PKCE verifier.
	SessionID        string
	CliCodeChallenge string
	LocalRedirectURI string
}

// loopbackOrigin validates a client loopback redirect URI and returns its
// origin (for the step-up pages' form-action).
func loopbackOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Port() == "" {
		return "", errors.New("local_redirect_uri must be http://127.0.0.1:<port>/…")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return "", errors.New("local_redirect_uri must be a loopback address (127.0.0.1 or localhost)")
	}
	return u.Scheme + "://" + u.Host, nil
}

// idpAuthTimeSkew allows for clock skew between the IdP and the controller.
const idpAuthTimeSkew = time.Minute

// idpMFAFresh reports whether the IdP's MFA proof belongs to this step-up.
// A silent round trip on a live IdP session can carry an amr from a sign-in
// done long ago; that is not a second factor for this step-up. A missing
// auth_time (0) is never fresh, so the user is sent to the TOTP page.
func idpMFAFresh(amrMFA bool, authTime int64, startedAt time.Time) bool {
	if !amrMFA || authTime <= 0 || startedAt.IsZero() {
		return false
	}
	return !time.Unix(authTime, 0).Before(startedAt.Add(-idpAuthTimeSkew))
}

// EnableStepUp turns on step-up authentication. Without it StartReauth
// answers Unimplemented and no risk signals are recorded.
func (s *Service) EnableStepUp(mfaSvc *mfa.Service, riskSvc *risk.Service) {
	s.mfaSvc = mfaSvc
	s.riskSvc = riskSvc
}

// StartReauth begins a step-up for the calling device and returns the IdP
// URL to open.
func (s *Service) StartReauth(ctx context.Context, req *clientv1.StartReauthRequest) (*clientv1.StartReauthResponse, error) {
	if s.mfaSvc == nil {
		return nil, status.Error(codes.Unimplemented, "step-up authentication is not enabled")
	}
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" || req.GetCodeChallenge() == "" || req.GetLocalRedirectUri() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token, device_id, code_challenge and local_redirect_uri are required")
	}
	if _, err := loopbackOrigin(req.GetLocalRedirectUri()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}
	directive, reason, err := deviceGate(ctx, s.pool, req.GetDeviceId(), claims)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if directive == clientv1.DeviceDirective_DIRECTIVE_REVOKED || directive == clientv1.DeviceDirective_DIRECTIVE_RE_ENROLL_REQUIRED {
		return nil, status.Errorf(codes.FailedPrecondition, "device cannot authenticate: %s", reason)
	}
	// The step-up stamps the session the device was enrolled under; the
	// token must belong to that same session.
	var deviceSID *string
	if err := s.pool.QueryRow(ctx,
		`SELECT session_id FROM client_devices WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		req.GetDeviceId(), claims.UserID).Scan(&deviceSID); err != nil {
		return nil, status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if deviceSID == nil || claims.SessionID == "" || *deviceSID != claims.SessionID {
		return nil, status.Error(codes.FailedPrecondition, "this device's session cannot be stepped up; sign in again")
	}

	conns, err := s.idpStore.ListForWorkspace(ctx, claims.TenantID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list identity connections: %v", err)
	}
	conn, err := selectEffectiveConnection(conns)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	adapter, err := auth.ProviderFor(conn, auth.GoogleCreds{
		ClientID:     s.clientGoogleClientID,
		ClientSecret: s.clientGoogleClientSecret,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "select identity provider: %v", err)
	}

	rawVerifier := make([]byte, 32)
	if _, err := rand.Read(rawVerifier); err != nil {
		return nil, status.Errorf(codes.Internal, "generate idp pkce verifier: %v", err)
	}
	idpVerifier := base64.RawURLEncoding.EncodeToString(rawVerifier)
	nonce, err := newNonce()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate nonce: %v", err)
	}
	sessionID, err := newSessionID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate session id: %v", err)
	}
	authURL, err := adapter.AuthURL(ctx, providers.AuthURLParams{
		State:         sessionID,
		Nonce:         nonce,
		CodeChallenge: sha256b64url([]byte(idpVerifier)),
		RedirectURI:   s.controllerHTTPURL + "/api/clients/callback",
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build auth url: %v", err)
	}
	putSession(sessionID, &authSession{
		WorkspaceID:     claims.TenantID,
		ConnectionID:    conn.ID,
		IdpCodeVerifier: idpVerifier,
		Nonce:           nonce,
		ExpiresAt:       time.Now().Add(reauthSessionTTL),
		Reauth: &reauthInfo{
			UserID:           claims.UserID,
			DeviceID:         req.GetDeviceId(),
			SID:              claims.SessionID,
			Email:            claims.Email,
			StartedAt:        time.Now(),
			SessionID:        sessionID,
			CliCodeChallenge: req.GetCodeChallenge(),
			LocalRedirectURI: req.GetLocalRedirectUri(),
		},
	})
	return &clientv1.StartReauthResponse{AuthUrl: authURL, SessionId: sessionID}, nil
}

// finishReauthIdP runs after the IdP proved an identity for a step-up
// session. The session has already been consumed by the caller.
func (s *Service) finishReauthIdP(w http.ResponseWriter, r *http.Request, sess *authSession, connID string, authCtx *providers.AuthenticationContext) {
	ctx := r.Context()
	ra := sess.Reauth
	stepUpHeaders(w, ra.LocalRedirectURI)

	fail := func(statusCode int, reason, msg string) {
		s.recordAudit(ctx, audit.Entry{
			TenantID:    sess.WorkspaceID,
			ActorUserID: ra.UserID,
			ActorEmail:  ra.Email,
			Action:      ActionStepUpFailed,
			TargetType:  "client_device",
			TargetID:    ra.DeviceID,
			Details:     map[string]any{"reason": reason},
		})
		renderStepUp(w, statusCode, stepUpPage{Title: "Authentication failed", Message: msg})
	}

	// The IdP must have signed in the SAME user the device belongs to.
	core, found, err := identity.NewResolver(s.pool).Resolve(ctx, connID, authCtx.Subject, sess.WorkspaceID)
	if err != nil {
		log.Printf("client reauth: resolve identity: %v", err)
		renderStepUp(w, http.StatusInternalServerError, stepUpPage{Title: "Something went wrong", Message: "Try again."})
		return
	}
	if !found || core.UserID != ra.UserID {
		fail(http.StatusForbidden, "identity_mismatch",
			"You signed in as a different user than the one this device belongs to. Sign out of the identity provider and try again.")
		return
	}
	if err := identity.CheckLifecycle(core.Status); err != nil {
		fail(http.StatusForbidden, "user_not_active", "Your account is not active.")
		return
	}

	st, err := s.policyStore.GetDeviceAuthState(ctx, sess.WorkspaceID, ra.DeviceID)
	if err != nil {
		fail(http.StatusForbidden, "device_unavailable", "This device is no longer enrolled. Run zecurity-client login.")
		return
	}
	// M-3, tightened: the IdP's MFA counts only when the IdP authenticated
	// the user during this step-up (auth_time), not on an older sign-in.
	idpMFA := idpMFAFresh(auth.MFAFromAMR(authCtx.AMR), authCtx.AuthTime, ra.StartedAt)
	riskDemanded := st.Device.ReauthRequiredAt != nil
	enrolled, err := s.mfaSvc.Enrolled(ctx, ra.UserID)
	if err != nil {
		log.Printf("client reauth: mfa status: %v", err)
		renderStepUp(w, http.StatusInternalServerError, stepUpPage{Title: "Something went wrong", Message: "Try again."})
		return
	}

	// M-3: the IdP's own MFA proof is enough. Otherwise TOTP is needed when a
	// reachable policy requires MFA, or when the risk score demanded this
	// authentication and the user has an authenticator (M-9).
	needMFA := !idpMFA && (st.RequiresMFA() || (riskDemanded && enrolled))
	if !needMFA {
		s.grantStepUp(w, r, sess.WorkspaceID, ra, idpMFA, mfaMethodLabel(idpMFA, false))
		return
	}

	t := &mfaTicket{
		WorkspaceID: sess.WorkspaceID,
		Reauth:      *ra,
		// Mandatory when the risk score demanded it and the user can do it.
		Skippable: !(riskDemanded && enrolled),
		ExpiresAt: time.Now().Add(mfaTicketTTL),
	}
	if !enrolled {
		enr, err := s.mfaSvc.StartEnrollment(ctx, ra.UserID, sess.WorkspaceID, ra.Email)
		if err != nil {
			log.Printf("client reauth: start mfa enrollment: %v", err)
			renderStepUp(w, http.StatusInternalServerError, stepUpPage{Title: "Something went wrong", Message: "Try again."})
			return
		}
		t.Enrollment = enr
	}
	id, err := newCtrlCode()
	if err != nil {
		renderStepUp(w, http.StatusInternalServerError, stepUpPage{Title: "Something went wrong", Message: "Try again."})
		return
	}
	putMFATicket(id, t)
	renderStepUp(w, http.StatusOK, mfaPage(id, t, ""))
}

// ReauthMFAHandler handles the TOTP page's form (POST /api/clients/reauth/mfa).
func (s *Service) ReauthMFAHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stepUpHeaders(w, "")
		if s.mfaSvc == nil {
			http.Error(w, "not enabled", http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			renderStepUp(w, http.StatusBadRequest, stepUpPage{Title: "Authentication failed", Message: "Bad request."})
			return
		}
		id := r.PostForm.Get("ticket")
		t, ok := getMFATicket(id)
		if !ok {
			renderStepUp(w, http.StatusBadRequest, stepUpPage{Title: "This page has expired",
				Message: "Run zecurity-client auth again."})
			return
		}
		ctx := r.Context()
		ra := &t.Reauth
		stepUpHeaders(w, ra.LocalRedirectURI)

		if r.PostForm.Get("action") == "skip" {
			if !t.Skippable {
				renderStepUp(w, http.StatusOK, mfaPage(id, t, "A code is required to continue."))
				return
			}
			deleteMFATicket(id)
			s.grantStepUp(w, r, t.WorkspaceID, ra, false, "none")
			return
		}

		res, failure, err := s.mfaSvc.Verify(ctx, ra.UserID, r.PostForm.Get("code"))
		switch {
		case err == nil:
		case errors.Is(err, mfa.ErrInvalidCode):
			if failure.Tripped {
				s.recordAudit(ctx, audit.Entry{
					TenantID: t.WorkspaceID, ActorUserID: ra.UserID, ActorEmail: ra.Email,
					Action: ActionMFALocked, TargetType: "user", TargetID: ra.UserID,
					Details: map[string]any{"device_id": ra.DeviceID},
				})
				if s.riskSvc != nil {
					if _, rerr := s.riskSvc.Add(ctx, t.WorkspaceID, ra.DeviceID, risk.MFAFailures); rerr != nil {
						log.Printf("client reauth: %v", rerr)
					}
				}
			}
			renderStepUp(w, http.StatusOK, mfaPage(id, t, "That code is not valid. Enter the current code from your authenticator app."))
			return
		case errors.Is(err, mfa.ErrRateLimited):
			renderStepUp(w, http.StatusTooManyRequests, mfaPage(id, t, "Too many wrong codes. Wait a few minutes and try again."))
			return
		case errors.Is(err, mfa.ErrNotEnrolled):
			deleteMFATicket(id)
			renderStepUp(w, http.StatusBadRequest, stepUpPage{Title: "This page has expired",
				Message: "Run zecurity-client auth again."})
			return
		default:
			// Fail closed: nothing was accepted.
			renderStepUp(w, http.StatusServiceUnavailable, mfaPage(id, t, "The code could not be checked right now. Try again."))
			return
		}

		deleteMFATicket(id)
		if res.Enrolled {
			s.recordAudit(ctx, audit.Entry{
				TenantID: t.WorkspaceID, ActorUserID: ra.UserID, ActorEmail: ra.Email,
				Action: ActionMFAEnrolled, TargetType: "user", TargetID: ra.UserID,
				Details: map[string]any{"method": "totp", "client": true},
			})
		}
		s.grantStepUp(w, r, t.WorkspaceID, ra, true, "totp")
	})
}

// grantStepUp ends the browser part of a step-up. Nothing is stamped here:
// the result is parked under a one-time code that goes to the loopback
// address of the client that called StartReauth. Only that client can
// redeem it (CompleteReauth, with its PKCE verifier), so a step-up link
// completed in someone else's browser opens nothing.
func (s *Service) grantStepUp(w http.ResponseWriter, r *http.Request, workspaceID string, ra *reauthInfo, withMFA bool, method string) {
	code, err := newCtrlCode()
	if err != nil {
		renderStepUp(w, http.StatusInternalServerError, stepUpPage{Title: "Something went wrong", Message: "Try again."})
		return
	}
	putStepUpGrant(ra.SessionID, &stepUpGrant{
		WorkspaceID: workspaceID,
		Reauth:      *ra,
		WithMFA:     withMFA,
		Method:      method,
		Code:        code,
		ExpiresAt:   time.Now().Add(stepUpGrantTTL),
	})
	http.Redirect(w, r, ra.LocalRedirectURI+"?code="+url.QueryEscape(code), http.StatusSeeOther)
}

// CompleteReauth redeems a step-up's one-time code. The code, the PKCE
// verifier, the access token's user and session, and the device must all
// match the StartReauth call; only then is the device's session stamped.
func (s *Service) CompleteReauth(ctx context.Context, req *clientv1.CompleteReauthRequest) (*clientv1.CompleteReauthResponse, error) {
	if s.mfaSvc == nil {
		return nil, status.Error(codes.Unimplemented, "step-up authentication is not enabled")
	}
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" || req.GetSessionId() == "" ||
		req.GetCtrlCode() == "" || req.GetCodeVerifier() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token, device_id, session_id, ctrl_code and code_verifier are required")
	}
	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}
	// Only the client that owns the grant (same workspace, user, session and
	// device) may touch it. Anyone else gets NotFound, and the grant is left
	// alone, so a token from elsewhere cannot burn it or write audit rows into
	// the owner's workspace. For the owner, any attempt consumes it.
	g, ok := takeStepUpGrant(req.GetSessionId(), func(g *stepUpGrant) bool {
		return claims.TenantID == g.WorkspaceID && claims.UserID == g.Reauth.UserID &&
			claims.SessionID == g.Reauth.SID && req.GetDeviceId() == g.Reauth.DeviceID
	})
	if !ok {
		return nil, status.Error(codes.NotFound, "step-up not found or expired; run zecurity-client auth again")
	}
	ra := &g.Reauth
	refuse := func(c codes.Code, reason, msg string) error {
		s.recordAudit(ctx, audit.Entry{
			TenantID: g.WorkspaceID, ActorUserID: ra.UserID, ActorEmail: ra.Email,
			Action: ActionStepUpFailed, TargetType: "client_device", TargetID: ra.DeviceID,
			Details: map[string]any{"reason": reason},
		})
		return status.Error(c, msg)
	}
	if subtle.ConstantTimeCompare([]byte(req.GetCtrlCode()), []byte(g.Code)) != 1 {
		return nil, refuse(codes.Unauthenticated, "invalid_code", "invalid ctrl_code")
	}
	if subtle.ConstantTimeCompare([]byte(sha256b64url([]byte(req.GetCodeVerifier()))), []byte(ra.CliCodeChallenge)) != 1 {
		return nil, refuse(codes.Unauthenticated, "pkce_failed", "pkce verification failed")
	}
	if !s.completeStepUp(ctx, g.WorkspaceID, ra, g.WithMFA, g.Method) {
		return nil, status.Error(codes.FailedPrecondition, "this device's session has ended; run zecurity-client login")
	}
	return &clientv1.CompleteReauthResponse{
		Mfa:               g.WithMFA,
		AuthRequiredCount: s.authRequiredCount(ctx, g.WorkspaceID, ra.DeviceID),
	}, nil
}

// completeStepUp stamps the device's session (and clears its risk score),
// audits, and recompiles the workspace ACL. It reports false when the device
// or its session is gone.
func (s *Service) completeStepUp(ctx context.Context, workspaceID string, ra *reauthInfo, withMFA bool, method string) bool {
	tag, err := s.pool.Exec(ctx,
		`UPDATE client_devices
		    SET session_auth_at    = NOW(),
		        -- A step-up that answers a risk demand without a second factor
		        -- must not leave an older MFA mark standing: the demand is
		        -- cleared below, and the old mark would count again.
		        session_mfa_at     = CASE WHEN $5 THEN NOW()
		                                  WHEN reauth_required_at IS NOT NULL THEN NULL
		                                  ELSE session_mfa_at END,
		        risk_score         = 0,
		        risk_reasons       = '{}',
		        reauth_required_at = NULL
		  WHERE id = $1 AND user_id = $2 AND workspace_id = $3
		    AND session_id = $4 AND revoked_at IS NULL`,
		ra.DeviceID, ra.UserID, workspaceID, ra.SID, withMFA)
	if err != nil {
		log.Printf("client reauth: stamp device %s: %v", ra.DeviceID, err)
		return false
	}
	if tag.RowsAffected() == 0 {
		return false
	}
	s.recordAudit(ctx, audit.Entry{
		TenantID:    workspaceID,
		ActorUserID: ra.UserID,
		ActorEmail:  ra.Email,
		Action:      ActionStepUp,
		TargetType:  "client_device",
		TargetID:    ra.DeviceID,
		Details:     map[string]any{"mfa": method},
	})
	if s.policyNotifier != nil {
		if err := s.policyNotifier.NotifyPolicyChange(context.WithoutCancel(ctx), workspaceID); err != nil {
			log.Printf("client reauth: notify policy change: %v", err)
		}
	}
	return true
}

func mfaMethodLabel(idpMFA, totp bool) string {
	switch {
	case totp:
		return "totp"
	case idpMFA:
		return "idp"
	default:
		return "none"
	}
}

// authRequiredCount is how many resources the device is waiting to
// authenticate for (GetACLSnapshotResponse.auth_required_count). Errors
// read as zero: the count is advisory, the ACL is the authority.
func (s *Service) authRequiredCount(ctx context.Context, workspaceID, deviceID string) uint32 {
	if s.policyStore == nil {
		return 0
	}
	if _, err := uuid.Parse(deviceID); err != nil {
		return 0
	}
	st, err := s.policyStore.GetDeviceAuthState(ctx, workspaceID, deviceID)
	if err != nil {
		return 0
	}
	return uint32(st.Pending(time.Now()))
}

// ── MFA tickets (process-local) ──────────────────────────────────────────────

// mfaTicket carries a step-up from the IdP callback to the TOTP form. It is
// the only place a pending enrolment's secret lives outside the sealed row,
// and only for the ticket's lifetime.
type mfaTicket struct {
	WorkspaceID string
	Reauth      reauthInfo
	Enrollment  *mfa.Enrollment // non-nil while enrolling
	Skippable   bool
	ExpiresAt   time.Time
}

var (
	mfaTicketMu sync.Mutex
	mfaTickets  = map[string]*mfaTicket{}
)

func putMFATicket(id string, t *mfaTicket) {
	mfaTicketMu.Lock()
	defer mfaTicketMu.Unlock()
	now := time.Now()
	for k, v := range mfaTickets {
		if now.After(v.ExpiresAt) {
			delete(mfaTickets, k)
		}
	}
	mfaTickets[id] = t
}

func getMFATicket(id string) (*mfaTicket, bool) {
	mfaTicketMu.Lock()
	defer mfaTicketMu.Unlock()
	t, ok := mfaTickets[id]
	if !ok || time.Now().After(t.ExpiresAt) {
		delete(mfaTickets, id)
		return nil, false
	}
	return t, true
}

func deleteMFATicket(id string) {
	mfaTicketMu.Lock()
	defer mfaTicketMu.Unlock()
	delete(mfaTickets, id)
}

// ── Step-up grants (process-local, D-02) ─────────────────────────────────────

const stepUpGrantTTL = 60 * time.Second

// stepUpGrant is a finished browser step-up waiting for its client.
type stepUpGrant struct {
	WorkspaceID string
	Reauth      reauthInfo
	WithMFA     bool
	Method      string
	Code        string
	ExpiresAt   time.Time
}

var (
	stepUpGrantMu sync.Mutex
	stepUpGrants  = map[string]*stepUpGrant{}
)

func putStepUpGrant(id string, g *stepUpGrant) {
	stepUpGrantMu.Lock()
	defer stepUpGrantMu.Unlock()
	now := time.Now()
	for k, v := range stepUpGrants {
		if now.After(v.ExpiresAt) {
			delete(stepUpGrants, k)
		}
	}
	stepUpGrants[id] = g
}

// takeStepUpGrant removes and returns a live grant, but only for its owner:
// when owns reports false the grant stays in place for the real client.
func takeStepUpGrant(id string, owns func(*stepUpGrant) bool) (*stepUpGrant, bool) {
	stepUpGrantMu.Lock()
	defer stepUpGrantMu.Unlock()
	g, ok := stepUpGrants[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(g.ExpiresAt) {
		delete(stepUpGrants, id)
		return nil, false
	}
	if !owns(g) {
		return nil, false
	}
	delete(stepUpGrants, id)
	return g, true
}

// ── Pages ────────────────────────────────────────────────────────────────────

type stepUpPage struct {
	Title   string
	Message string
	Error   string

	// The TOTP form (Ticket != "").
	Ticket    string
	Skippable bool
	// Enrolment only.
	Secret string
	URI    template.URL
	QR     template.URL // data: URI of the otpauth QR code
}

func mfaPage(id string, t *mfaTicket, errMsg string) stepUpPage {
	p := stepUpPage{
		Title:     "Enter your authentication code",
		Message:   "Open your authenticator app and enter the 6-digit code for Zecurity.",
		Error:     errMsg,
		Ticket:    id,
		Skippable: t.Skippable,
	}
	if t.Enrollment != nil {
		p.Title = "Set up two-factor authentication"
		p.Message = "Scan this QR code with an authenticator app (Google Authenticator, Authy, 1Password, …), then enter the 6-digit code it shows."
		p.Secret = t.Enrollment.Secret
		p.URI = template.URL(t.Enrollment.URI)
		if png, err := qrcode.Encode(t.Enrollment.URI, qrcode.Medium, 220); err == nil {
			p.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
		}
	}
	return p
}

// stepUpHeaders: the pages can show a TOTP secret, so they are never cached,
// framed or allowed to load anything. The TOTP form's answer redirects to the
// client's loopback address, so form-action also allows that one origin.
func stepUpHeaders(w http.ResponseWriter, loopbackURI string) {
	formAction := "'self'"
	if o, err := loopbackOrigin(loopbackURI); err == nil {
		formAction += " " + o
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
}

func renderStepUp(w http.ResponseWriter, statusCode int, p stepUpPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := stepUpTmpl.Execute(w, p); err != nil {
		log.Printf("client reauth: render page: %v", err)
	}
}

var stepUpTmpl = template.Must(template.New("stepup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Zecurity</title>
<style>
:root{color-scheme:light dark}
body{margin:0;font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;background:#f4f5f7;color:#16181d;display:flex;min-height:100vh;align-items:center;justify-content:center}
main{background:#fff;border-radius:12px;padding:32px;margin:16px;max-width:420px;width:100%;box-shadow:0 2px 12px rgba(0,0,0,.08)}
h1{font-size:20px;margin:0 0 8px}
p{margin:0 0 16px;color:#4a4f5a}
.err{color:#b42318;background:#fef3f2;border-radius:8px;padding:8px 12px}
img{display:block;margin:0 auto 12px;border-radius:8px;background:#fff}
code{display:block;word-break:break-all;background:#f4f5f7;border-radius:8px;padding:8px 12px;margin:0 0 16px;font-size:14px}
input[type=text]{width:100%;box-sizing:border-box;font:inherit;font-size:22px;letter-spacing:.3em;text-align:center;padding:10px;border:1px solid #c5c9d3;border-radius:8px;margin:0 0 12px}
button{font:inherit;padding:10px 16px;border-radius:8px;border:0;cursor:pointer}
.primary{background:#1f5eff;color:#fff;width:100%}
.link{background:none;color:#4a4f5a;width:100%;margin-top:8px}
@media (prefers-color-scheme:dark){body{background:#0f1115;color:#e8eaf0}main{background:#1a1d24;box-shadow:none}p{color:#aab0bd}code{background:#0f1115}input[type=text]{background:#0f1115;color:inherit;border-color:#3a3f4b}.err{background:#3b1512;color:#fda29b}.link{color:#aab0bd}}
</style></head><body><main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
{{if .Ticket}}
{{if .Secret}}
{{if .QR}}<img src="{{.QR}}" width="220" height="220" alt="QR code for your authenticator app">{{end}}
<p>Can't scan it? Enter this key in the app:</p>
<code>{{.Secret}}</code>
{{end}}
<form method="post" action="/api/clients/reauth/mfa" autocomplete="off">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code" aria-label="6-digit code" autofocus required>
<button class="primary" type="submit" name="action" value="verify">Verify</button>
</form>
{{if .Skippable}}
<form method="post" action="/api/clients/reauth/mfa">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<button class="link" type="submit" name="action" value="skip">Skip for now</button>
</form>
{{end}}
{{end}}
</main></body></html>`))
