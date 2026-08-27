// Package client implements the ClientService gRPC handlers used by the
// Rust end-user CLI (`zecurity-client`). The service runs on the same
// gRPC listener as ConnectorService/ShieldService but is exempt from the
// SPIFFE interceptor — clients have no workspace certificate until they
// complete EnrollDevice, and auth is carried as a JWT field inside the
// request.
package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/transport"
)

const (
	googleAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint = "https://oauth2.googleapis.com/token"

	clientCertTTL = 7 * 24 * time.Hour
)

// Service implements clientv1.ClientServiceServer.
type Service struct {
	clientv1.UnimplementedClientServiceServer

	pool                     *pgxpool.Pool
	authSvc                  auth.Service
	pkiSvc                   pki.Service
	idpStore                 *idp.Store
	clientGoogleClientID     string
	clientGoogleClientSecret string
	controllerHost           string
	controllerHTTPURL        string // e.g. "http://localhost:8080" — base URL for /api/clients/callback
	policyStore              *policy.Store
	policyCache              *policy.SnapshotCache
	policyNotifier           *policy.Notifier
	transportCompiler        *transport.Compiler
	postureStore             *posture.Store
	postureEvaluator         *posture.Evaluator
}

// NewService wires the ClientService with the dependencies it needs.
func NewService(
	pool *pgxpool.Pool,
	authSvc auth.Service,
	pkiSvc pki.Service,
	idpStore *idp.Store,
	clientGoogleClientID, clientGoogleClientSecret,
	controllerHost, controllerHTTPURL string,
	policyStore *policy.Store,
	policyCache *policy.SnapshotCache,
	policyNotifier *policy.Notifier,
	transportCompiler *transport.Compiler,
	postureStore *posture.Store,
	postureEvaluator *posture.Evaluator,
) *Service {
	return &Service{
		pool:                     pool,
		authSvc:                  authSvc,
		pkiSvc:                   pkiSvc,
		idpStore:                 idpStore,
		clientGoogleClientID:     clientGoogleClientID,
		clientGoogleClientSecret: clientGoogleClientSecret,
		controllerHost:           controllerHost,
		controllerHTTPURL:        strings.TrimRight(controllerHTTPURL, "/"),
		policyStore:              policyStore,
		policyCache:              policyCache,
		policyNotifier:           policyNotifier,
		transportCompiler:        transportCompiler,
		postureStore:             postureStore,
		postureEvaluator:         postureEvaluator,
	}
}

// sha256b64url returns BASE64URL(SHA256(b)) without padding — used for PKCE.
func sha256b64url(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GetAuthConfig returns the OAuth configuration the CLI needs for informational
// purposes (e.g. showing the google_client_id). The actual auth URL is built
// and returned by InitiateAuth.
func (s *Service) GetAuthConfig(ctx context.Context, req *clientv1.GetAuthConfigRequest) (*clientv1.GetAuthConfigResponse, error) {
	if req.GetWorkspaceSlug() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_slug is required")
	}

	if _, err := lookupWorkspaceBySlug(ctx, s.pool, req.GetWorkspaceSlug()); err != nil {
		if errors.Is(err, errWorkspaceNotFound) {
			return nil, status.Error(codes.NotFound, "workspace not found")
		}
		return nil, status.Errorf(codes.Internal, "lookup workspace: %v", err)
	}

	return &clientv1.GetAuthConfigResponse{
		GoogleClientId: s.clientGoogleClientID,
		AuthEndpoint:   googleAuthEndpoint,
		TokenEndpoint:  googleTokenEndpoint,
		ControllerHost: s.controllerHost,
	}, nil
}

// InitiateAuth registers a PKCE auth session and returns the Google OAuth URL
// for the CLI to open in the browser. The controller's fixed callback URL is
// embedded in the returned auth_url — the CLI never constructs the Google URL.
func (s *Service) InitiateAuth(ctx context.Context, req *clientv1.InitiateAuthRequest) (*clientv1.InitiateAuthResponse, error) {
	if req.GetWorkspaceSlug() == "" || req.GetCodeChallenge() == "" || req.GetLocalRedirectUri() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_slug, code_challenge, local_redirect_uri are required")
	}

	// Validate that local_redirect_uri is a loopback address — security check.
	u, err := url.Parse(req.GetLocalRedirectUri())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid local_redirect_uri")
	}
	h := u.Hostname()
	if h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return nil, status.Error(codes.InvalidArgument, "local_redirect_uri must be a loopback address (127.0.0.1 or localhost)")
	}

	ws, err := lookupWorkspaceBySlug(ctx, s.pool, req.GetWorkspaceSlug())
	if err != nil {
		if errors.Is(err, errWorkspaceNotFound) {
			return nil, status.Error(codes.NotFound, "workspace not found")
		}
		return nil, status.Errorf(codes.Internal, "lookup workspace: %v", err)
	}

	// Resolve the workspace's effective identity connection and its adapter.
	// The Rust client sends no connection selector, so the server picks a single
	// effective IdP (1 enterprise → it, 0 → bootstrap, >1 → explicit error).
	conns, err := s.idpStore.ListForWorkspace(ctx, ws.ID)
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

	// Controller↔IdP PKCE pair (leg a) — never leaves the server. This is
	// distinct from the CLI↔controller PKCE (leg b, req.CodeChallenge).
	rawVerifier := make([]byte, 32)
	if _, err := rand.Read(rawVerifier); err != nil {
		return nil, status.Errorf(codes.Internal, "generate idp pkce verifier: %v", err)
	}
	idpVerifier := base64.RawURLEncoding.EncodeToString(rawVerifier)
	idpChallenge := sha256b64url([]byte(idpVerifier))

	nonce, err := newNonce()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate nonce: %v", err)
	}

	sessionID, err := newSessionID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate session id: %v", err)
	}

	callbackURL := s.controllerHTTPURL + "/api/clients/callback"
	authURL, err := adapter.AuthURL(ctx, providers.AuthURLParams{
		State:         sessionID,
		Nonce:         nonce,
		CodeChallenge: idpChallenge,
		RedirectURI:   callbackURL,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build auth url: %v", err)
	}

	putSession(sessionID, &authSession{
		WorkspaceID:      ws.ID,
		WorkspaceSlug:    ws.Slug,
		ConnectionID:     conn.ID,
		CliCodeChallenge: req.GetCodeChallenge(),
		LocalRedirectURI: req.GetLocalRedirectUri(),
		IdpCodeVerifier:  idpVerifier,
		Nonce:            nonce,
		ExpiresAt:        time.Now().Add(10 * time.Minute),
	})

	return &clientv1.InitiateAuthResponse{
		AuthUrl:   authURL,
		SessionId: sessionID,
	}, nil
}

// selectEffectiveConnection picks the single connection the CLI should use, given
// all active connections resolvable for the workspace. The Rust proto has no
// connection selector yet (M4 follow-up), so: exactly one active Enterprise IdP
// → use it; none → the Bootstrap (platform) IdP; more than one Enterprise IdP →
// explicit error (the CLI can't disambiguate).
func selectEffectiveConnection(conns []idp.Connection) (*idp.Connection, error) {
	var enterprise []*idp.Connection
	var bootstrap *idp.Connection
	for i := range conns {
		c := &conns[i]
		if c.Status != "active" {
			continue
		}
		if c.TenantID == nil {
			if bootstrap == nil {
				bootstrap = c
			}
		} else {
			enterprise = append(enterprise, c)
		}
	}
	switch {
	case len(enterprise) == 1:
		return enterprise[0], nil
	case len(enterprise) == 0:
		if bootstrap != nil {
			return bootstrap, nil
		}
		return nil, fmt.Errorf("no active identity provider configured for this workspace")
	default:
		return nil, fmt.Errorf("workspace has multiple identity providers; CLI selection is not yet supported — sign in via the web console")
	}
}

// newNonce returns a 256-bit base64url OIDC nonce.
func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthCallbackHandler handles GET /api/clients/callback — the fixed redirect
// URI registered in Google Console. Google sends the auth code here; the
// controller exchanges it server-side, then redirects the browser to the
// CLI's local loopback server with a short-lived ctrl_code.
func (s *Service) AuthCallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleCode := r.URL.Query().Get("code")
		sessionID := r.URL.Query().Get("state")

		if googleCode == "" || sessionID == "" {
			http.Error(w, "missing code or state", http.StatusBadRequest)
			return
		}

		sess, ok := getSession(sessionID)
		if !ok {
			http.Error(w, "auth session not found or expired", http.StatusBadRequest)
			return
		}

		callbackURL := s.controllerHTTPURL + "/api/clients/callback"

		// Re-resolve the connection from the session's connection_id (never from
		// the request) and authenticate via the adapter. Fail closed if the
		// connection was deleted or disabled during the redirect window.
		conn, err := s.idpStore.GetByID(r.Context(), sess.ConnectionID)
		if err != nil || conn.Status != "active" {
			http.Error(w, "identity connection unavailable", http.StatusUnauthorized)
			return
		}
		adapter, err := auth.ProviderFor(conn, auth.GoogleCreds{
			ClientID:     s.clientGoogleClientID,
			ClientSecret: s.clientGoogleClientSecret,
		})
		if err != nil {
			http.Error(w, "identity provider unavailable", http.StatusUnauthorized)
			return
		}

		authCtx, err := adapter.Authenticate(r.Context(), googleCode, sess.IdpCodeVerifier, callbackURL, sess.Nonce)
		if err != nil || authCtx.Subject == "" || authCtx.Email == "" {
			http.Error(w, "identity verification failed", http.StatusUnauthorized)
			return
		}

		ctrlCode, err := newCtrlCode()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !updateSessionCtrlCode(sessionID, authCtx.Email, authCtx.Provider, authCtx.Subject, ctrlCode, time.Now().Add(60*time.Second)) {
			http.Error(w, "auth session expired during callback", http.StatusBadRequest)
			return
		}

		http.Redirect(w, r,
			sess.LocalRedirectURI+"?code="+url.QueryEscape(ctrlCode),
			http.StatusFound)
	})
}

// TokenExchange validates the ctrl_code and CLI-Controller PKCE, then issues
// a Zecurity JWT + refresh token. The session is consumed (single-use).
func (s *Service) TokenExchange(ctx context.Context, req *clientv1.TokenExchangeRequest) (*clientv1.TokenExchangeResponse, error) {
	if req.GetSessionId() == "" || req.GetCtrlCode() == "" || req.GetCodeVerifier() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id, ctrl_code, code_verifier are required")
	}

	sess, ok := consumeSession(req.GetSessionId())
	if !ok {
		return nil, status.Error(codes.NotFound, "auth session not found or expired")
	}

	if sess.CtrlCode == "" || time.Now().After(sess.CtrlCodeExpiresAt) {
		return nil, status.Error(codes.FailedPrecondition, "callback not completed or ctrl_code expired")
	}
	if req.GetCtrlCode() != sess.CtrlCode {
		return nil, status.Error(codes.Unauthenticated, "invalid ctrl_code")
	}

	// Verify CLI-Controller PKCE: SHA256(code_verifier) must equal the
	// code_challenge that was registered in InitiateAuth.
	if sha256b64url([]byte(req.GetCodeVerifier())) != sess.CliCodeChallenge {
		return nil, status.Error(codes.Unauthenticated, "pkce verification failed")
	}

	// Validate invite token if provided.
	var inviteRow *invitation
	if req.GetInviteToken() != "" {
		inv, err := getInvitationByToken(ctx, s.pool, req.GetInviteToken())
		if err != nil {
			if errors.Is(err, errInvitationNotFound) {
				return nil, status.Error(codes.NotFound, "invitation not found")
			}
			return nil, status.Errorf(codes.Internal, "lookup invitation: %v", err)
		}
		if inv.WorkspaceID != sess.WorkspaceID {
			return nil, status.Error(codes.PermissionDenied, "invitation does not belong to this workspace")
		}
		if inv.Status != "pending" {
			return nil, status.Errorf(codes.FailedPrecondition, "invitation already %s", inv.Status)
		}
		if time.Now().After(inv.ExpiresAt) {
			return nil, status.Error(codes.FailedPrecondition, "invitation expired")
		}
		inviteRow = inv
	}

	// Re-resolve the connection from the session's stored id to obtain the issuer
	// for the identity link, and to fail closed if the connection was deleted or
	// disabled during the login window.
	conn, err := s.idpStore.GetByID(ctx, sess.ConnectionID)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "identity connection unavailable: %v", err)
	}
	if conn.Status != "active" {
		return nil, status.Error(codes.Unauthenticated, "identity connection is not active")
	}

	user, gen, created, err := upsertUser(ctx, s.pool, sess.WorkspaceID, sess.Email, sess.Provider, sess.Subject, conn.ID, conn.Issuer, inviteRow != nil)
	if err != nil {
		if errors.Is(err, errUserNotInvited) {
			return nil, status.Error(codes.PermissionDenied, "no membership in workspace; ask an admin for an invitation")
		}
		if errors.Is(err, identity.ErrUserNotActive) {
			return nil, status.Error(codes.PermissionDenied, "account is not active")
		}
		return nil, status.Errorf(codes.Internal, "upsert user: %v", err)
	}

	if inviteRow != nil {
		if err := markInvitationAccepted(ctx, s.pool, inviteRow.ID); err != nil {
			return nil, status.Errorf(codes.Internal, "accept invitation: %v", err)
		}
	}
	_ = created

	accessToken, expiresIn, err := s.authSvc.IssueAccessToken(user.ID, sess.WorkspaceID, user.Role, sess.Email, gen)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue access token: %v", err)
	}
	refreshToken, err := s.authSvc.IssueRefreshToken(ctx, user.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue refresh token: %v", err)
	}

	return &clientv1.TokenExchangeResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		Email:        sess.Email,
	}, nil
}

// EnrollDevice issues an mTLS leaf certificate for the calling user's device.
func (s *Service) EnrollDevice(ctx context.Context, req *clientv1.EnrollDeviceRequest) (*clientv1.EnrollDeviceResponse, error) {
	if req.GetAccessToken() == "" || req.GetCsrPem() == "" || req.GetDeviceName() == "" || req.GetOs() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token, csr_pem, device_name, os are required")
	}

	tokenClaims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}

	block, _ := pem.Decode([]byte(req.GetCsrPem()))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, status.Error(codes.InvalidArgument, "csr_pem is not a valid CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR signature invalid: %v", err)
	}

	slug, err := lookupWorkspaceSlug(ctx, s.pool, tokenClaims.TenantID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lookup workspace slug: %v", err)
	}
	trustDomain := appmeta.WorkspaceTrustDomain(slug)

	deviceID, err := insertClientDevice(ctx, s.pool, tokenClaims.UserID, tokenClaims.TenantID, req.GetDeviceName(), req.GetOs())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "insert client device: %v", err)
	}

	certResult, err := s.pkiSvc.SignClientCert(ctx, tokenClaims.TenantID, deviceID, trustDomain, csr, clientCertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign client cert: %v", err)
	}

	spiffeID := appmeta.ClientSPIFFEID(trustDomain, deviceID)
	if err := updateClientDeviceCert(ctx, s.pool, deviceID, certResult.Serial, certResult.NotAfter, spiffeID); err != nil {
		return nil, status.Errorf(codes.Internal, "record device cert: %v", err)
	}
	if err := s.policyNotifier.NotifyPolicyChange(ctx, tokenClaims.TenantID); err != nil {
		return nil, status.Errorf(codes.Internal, "refresh policy after device enrollment: %v", err)
	}

	return &clientv1.EnrollDeviceResponse{
		CertificatePem:    certResult.CertificatePEM,
		WorkspaceCaPem:    certResult.WorkspaceCAPEM,
		IntermediateCaPem: certResult.IntermediateCAPEM,
		SpiffeId:          spiffeID,
		DeviceId:          deviceID,
	}, nil
}

// deviceGate confirms a device belongs to claims' user + workspace and derives
// the DeviceDirective to report to the client (Sprint 19 Track 2 / PENDING-13,
// see Track2-Device-Trust-Directive.md D-C). Priority: re_enroll_required (the
// status column) beats revoked (derived from revoked_at — never duplicated
// into status, so there is exactly one writer of "revoked-ness") beats
// renew_pending (status column; Track 3 populates this), else none.
//
// Used by BOTH GetACLSnapshot and GetTransportSnapshot so the two RPCs can
// never disagree about a device's trust state. A non-nil error means the
// device wasn't found or belongs to someone else; callers translate that into
// PermissionDenied exactly as before — it carries no directive information.
// On a successful lookup, last_seen_at is stamped (throttled to once per 5
// minutes, D-E) regardless of directive — the server heard from the device
// either way. The stamp is best-effort and must never fail the RPC.
func deviceGate(ctx context.Context, db *pgxpool.Pool, deviceID string, claims *auth.AccessTokenClaims) (directive clientv1.DeviceDirective, reason string, err error) {
	var deviceWorkspaceID, deviceStatus string
	var revokedAt *time.Time
	err = db.QueryRow(ctx,
		`SELECT workspace_id, status, revoked_at FROM client_devices
		 WHERE id = $1 AND user_id = $2`,
		deviceID, claims.UserID,
	).Scan(&deviceWorkspaceID, &deviceStatus, &revokedAt)
	if err != nil {
		return clientv1.DeviceDirective_DIRECTIVE_NONE, "", fmt.Errorf("device not found: %w", err)
	}
	if deviceWorkspaceID != claims.TenantID {
		return clientv1.DeviceDirective_DIRECTIVE_NONE, "", fmt.Errorf("device does not belong to this user")
	}

	switch {
	case deviceStatus == "re_enroll_required":
		directive = clientv1.DeviceDirective_DIRECTIVE_RE_ENROLL_REQUIRED
		reason = "sign in again to re-register this device"
	case revokedAt != nil:
		directive = clientv1.DeviceDirective_DIRECTIVE_REVOKED
		reason = "device access revoked — contact your admin"
	case deviceStatus == "renew_pending":
		directive = clientv1.DeviceDirective_DIRECTIVE_RENEW_SOON
		reason = "certificate renewal due soon"
	default:
		directive = clientv1.DeviceDirective_DIRECTIVE_NONE
	}

	if _, stampErr := db.Exec(ctx,
		`UPDATE client_devices SET last_seen_at = NOW()
		  WHERE id = $1 AND (last_seen_at IS NULL OR last_seen_at < NOW() - INTERVAL '5 minutes')`,
		deviceID,
	); stampErr != nil {
		log.Printf("deviceGate: stamp last_seen_at for device %s: %v", deviceID, stampErr)
	}

	return directive, reason, nil
}

// GetACLSnapshot returns the current workspace ACL snapshot for the calling device.
// Validates the access token and confirms the device belongs to the token's user/workspace.
// Default-deny: returns an empty snapshot on any validation or compile failure.
func (s *Service) GetACLSnapshot(ctx context.Context, req *clientv1.GetACLSnapshotRequest) (*clientv1.GetACLSnapshotResponse, error) {
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token and device_id are required")
	}

	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}

	directive, reason, err := deviceGate(ctx, s.pool, req.GetDeviceId(), claims)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if directive != clientv1.DeviceDirective_DIRECTIVE_NONE {
		// Directive-in-response, not error (D-B): gRPC OK, no ACL payload
		// regardless of directive — fail-closed even for a directive-ignoring
		// client, since up_to_date also stays false so it drops its cached ACL.
		return &clientv1.GetACLSnapshotResponse{DeviceDirective: directive, DirectiveReason: reason}, nil
	}

	workspaceID := claims.TenantID

	// Serve from cache, or compile under the epoch CAS so a compile raced by a
	// policy change is not cached as stale (ADR-013).
	snap, err := s.policyCache.GetOrCompile(workspaceID, func() (*policy.CompiledACL, error) {
		return policy.CompileACLSnapshot(ctx, s.policyStore,s.postureStore, s.policyNotifier, workspaceID)
	})
	if err != nil {
		// Default-deny: do not serve a partial or stale snapshot.
		return nil, status.Errorf(codes.Internal, "compile acl snapshot: %v", err)
	}

	// Skip the payload when the client already has the current version — mirrors
	// GetTransportSnapshot. Compared against the compiled snapshot's version (not
	// the notifier directly) so a compile raced by a policy change can never report
	// up_to_date for a version the client isn't actually holding.
	if req.GetKnownVersion() != 0 && req.GetKnownVersion() == snap.Version {
		return &clientv1.GetACLSnapshotResponse{UpToDate: true}, nil
	}
	return &clientv1.GetACLSnapshotResponse{Snapshot: snap}, nil
}

// GetTransportSnapshot returns the workspace transport (connectivity) snapshot
// for the calling device — per-connector relay coordinates keyed by
// remote_network_id (ADR-015 Track B). Independent of the ACL: relay changes
// bump only transport_version, so this can return a new snapshot without any
// ACL recompile. Same device ownership + revocation gate as GetACLSnapshot.
// When the client's known_version matches the current version, the snapshot is
// omitted and up_to_date is set.
//
// DISCLOSURE (finding #7): the snapshot is workspace-wide — every authenticated
// device in the workspace receives the full connector/relay topology, and the
// client filters to its authorized resources locally. This is intentional and
// consistent with GetACLSnapshot, which already returns the whole workspace ACL
// (the same connector/relay metadata) to every device. It is connectivity
// metadata only — it grants no access; authorization is always the ACL's
// allowed_spiffe_ids. Per-device least-disclosure filtering, if ever required,
// must be applied to BOTH planes together (ACL + transport), not transport alone.
func (s *Service) GetTransportSnapshot(ctx context.Context, req *clientv1.GetTransportSnapshotRequest) (*clientv1.GetTransportSnapshotResponse, error) {
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token and device_id are required")
	}

	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}

	// Same gate as GetACLSnapshot (deviceGate) so the two RPCs never disagree
	// about a device's trust state. The ACL poll is the authoritative reaction
	// path (Track2-Device-Trust-Directive.md D-A) — this just returns a clean
	// directive instead of spamming PermissionDenied post-revoke.
	directive, reason, err := deviceGate(ctx, s.pool, req.GetDeviceId(), claims)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if directive != clientv1.DeviceDirective_DIRECTIVE_NONE {
		return &clientv1.GetTransportSnapshotResponse{DeviceDirective: directive, DirectiveReason: reason}, nil
	}

	workspaceID := claims.TenantID

	snap, err := s.transportCompiler.GetOrCompile(ctx, workspaceID)
	if err != nil {
		// Default-deny: do not serve a partial or stale snapshot.
		return nil, status.Errorf(codes.Internal, "compile transport snapshot: %v", err)
	}

	// Skip the payload when the client already has the current version.
	if req.GetKnownVersion() != 0 && req.GetKnownVersion() == snap.Version {
		return &clientv1.GetTransportSnapshotResponse{UpToDate: true}, nil
	}
	return &clientv1.GetTransportSnapshotResponse{Snapshot: snap}, nil
}

// RevokeDevice marks a client device as revoked. The device's SPIFFE will be
// removed from subsequent ACL compiles and the workspace policy version is
// bumped so connectors and clients see the change. This is the server side of
// the CLI's logout flow, and is best-effort from the CLI's perspective — but
// on the server side it is fully authoritative: on success, the row is
// revoked and cannot be un-revoked without a fresh enrollment.
func (s *Service) RevokeDevice(ctx context.Context, req *clientv1.RevokeDeviceRequest) (*clientv1.RevokeDeviceResponse, error) {
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "access_token and device_id are required")
	}

	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}

	// Confirm the device belongs to this user and workspace — same shape as
	// GetACLSnapshot. Prevents alice from revoking bob's device.
	var deviceWorkspaceID string
	err = s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM client_devices
		 WHERE id = $1 AND user_id = $2`,
		req.GetDeviceId(), claims.UserID,
	).Scan(&deviceWorkspaceID)
	if err != nil || deviceWorkspaceID != claims.TenantID {
		return nil, status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}

	// Soft revoke: set revoked_at = NOW(). Idempotent — repeated calls on an
	// already-revoked device are a no-op success.
	if err := revokeClientDevice(ctx, s.pool, req.GetDeviceId(), claims.UserID, claims.TenantID); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke device: %v", err)
	}

	// Bump the workspace policy version so the SPIFFE is dropped from
	// AllowedSpiffeIds on the next ACL compile, and connectors receive the
	// updated snapshot via the push hook.
	if err := s.policyNotifier.NotifyPolicyChange(ctx, claims.TenantID); err != nil {
		return nil, status.Errorf(codes.Internal, "refresh policy after device revocation: %v", err)
	}

	return &clientv1.RevokeDeviceResponse{}, nil
}

// Compile-time interface check.
var _ clientv1.ClientServiceServer = (*Service)(nil)
