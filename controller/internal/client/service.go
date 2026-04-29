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
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
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
	"github.com/yourorg/ztna/controller/internal/pki"
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
	clientGoogleClientID     string
	clientGoogleClientSecret string
	controllerHost           string
	controllerHTTPURL        string // e.g. "http://localhost:8080" — base URL for /api/clients/callback
}

// NewService wires the ClientService with the dependencies it needs.
func NewService(
	pool *pgxpool.Pool,
	authSvc auth.Service,
	pkiSvc pki.Service,
	clientGoogleClientID, clientGoogleClientSecret,
	controllerHost, controllerHTTPURL string,
) *Service {
	return &Service{
		pool:                     pool,
		authSvc:                  authSvc,
		pkiSvc:                   pkiSvc,
		clientGoogleClientID:     clientGoogleClientID,
		clientGoogleClientSecret: clientGoogleClientSecret,
		controllerHost:           controllerHost,
		controllerHTTPURL:        strings.TrimRight(controllerHTTPURL, "/"),
	}
}

// sha256b64url returns BASE64URL(SHA256(b)) without padding — used for PKCE.
func sha256b64url(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// exchangeCode performs the Google OAuth code exchange using the CLI OAuth
// app's credentials. codeVerifier is the controller's own Google PKCE verifier.
func (s *Service) exchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*auth.GoogleTokenResponse, error) {
	body := url.Values{}
	body.Set("code", code)
	body.Set("code_verifier", codeVerifier)
	body.Set("client_id", s.clientGoogleClientID)
	body.Set("client_secret", s.clientGoogleClientSecret)
	body.Set("redirect_uri", redirectURI)
	body.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		googleTokenEndpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody) //nolint:errcheck
		return nil, fmt.Errorf("google token exchange failed: status=%d body=%v", resp.StatusCode, errBody)
	}

	var tokenResp auth.GoogleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tokenResp.IDToken == "" {
		return nil, fmt.Errorf("google did not return id_token")
	}
	return &tokenResp, nil
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

	// Generate the controller's own Google PKCE pair.
	// The CLI never sees this verifier — it lives only in the session store.
	rawVerifier := make([]byte, 32)
	if _, err := rand.Read(rawVerifier); err != nil {
		return nil, status.Errorf(codes.Internal, "generate google pkce verifier: %v", err)
	}
	googleVerifier := base64.RawURLEncoding.EncodeToString(rawVerifier)
	googleChallenge := sha256b64url([]byte(googleVerifier))

	sessionID, err := newSessionID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate session id: %v", err)
	}

	callbackURL := s.controllerHTTPURL + "/api/clients/callback"
	authURL := fmt.Sprintf(
		"%s?client_id=%s&redirect_uri=%s&response_type=code"+
			"&scope=openid%%20email&code_challenge=%s&code_challenge_method=S256&state=%s",
		googleAuthEndpoint,
		url.QueryEscape(s.clientGoogleClientID),
		url.QueryEscape(callbackURL),
		url.QueryEscape(googleChallenge),
		url.QueryEscape(sessionID),
	)

	putSession(sessionID, &authSession{
		WorkspaceID:        ws.ID,
		WorkspaceSlug:      ws.Slug,
		CliCodeChallenge:   req.GetCodeChallenge(),
		LocalRedirectURI:   req.GetLocalRedirectUri(),
		GoogleCodeVerifier: googleVerifier,
		ExpiresAt:          time.Now().Add(10 * time.Minute),
	})

	return &clientv1.InitiateAuthResponse{
		AuthUrl:   authURL,
		SessionId: sessionID,
	}, nil
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
		tokens, err := s.exchangeCode(r.Context(), googleCode, sess.GoogleCodeVerifier, callbackURL)
		if err != nil {
			http.Error(w, "google token exchange failed", http.StatusBadRequest)
			return
		}

		claims, err := auth.VerifyGoogleIDToken(r.Context(), tokens.IDToken, s.clientGoogleClientID)
		if err != nil || claims.Sub == "" || claims.Email == "" {
			http.Error(w, "identity verification failed", http.StatusUnauthorized)
			return
		}

		ctrlCode, err := newCtrlCode()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !updateSessionCtrlCode(sessionID, claims.Email, claims.Sub, ctrlCode, time.Now().Add(60*time.Second)) {
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

	user, created, err := upsertUser(ctx, s.pool, sess.WorkspaceID, sess.Email, "google", sess.GoogleSub, inviteRow != nil)
	if err != nil {
		if errors.Is(err, errUserNotInvited) {
			return nil, status.Error(codes.PermissionDenied, "no membership in workspace; ask an admin for an invitation")
		}
		return nil, status.Errorf(codes.Internal, "upsert user: %v", err)
	}

	if inviteRow != nil {
		if err := markInvitationAccepted(ctx, s.pool, inviteRow.ID); err != nil {
			return nil, status.Errorf(codes.Internal, "accept invitation: %v", err)
		}
	}
	_ = created

	accessToken, expiresIn, err := s.authSvc.IssueAccessToken(user.ID, sess.WorkspaceID, user.Role)
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

	return &clientv1.EnrollDeviceResponse{
		CertificatePem:    certResult.CertificatePEM,
		WorkspaceCaPem:    certResult.WorkspaceCAPEM,
		IntermediateCaPem: certResult.IntermediateCAPEM,
		SpiffeId:          spiffeID,
		DeviceId:          deviceID,
	}, nil
}

// Compile-time interface check.
var _ clientv1.ClientServiceServer = (*Service)(nil)
