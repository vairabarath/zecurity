// Package client implements the ClientService gRPC handlers used by the
// Rust end-user CLI (`zecurity-client`). The service runs on the same
// gRPC listener as ConnectorService/ShieldService but is exempt from the
// SPIFFE interceptor — clients have no workspace certificate until they
// complete EnrollDevice, and auth is carried as a JWT field inside the
// request.
package client

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

	pool           *pgxpool.Pool
	authSvc        auth.Service
	pkiSvc         pki.Service
	googleClientID string
	controllerHost string
}

// NewService wires the ClientService with the dependencies it needs.
// googleClientID is returned to the CLI verbatim so it can build its
// authorize URL; controllerHost is the public hostname of this controller
// (without scheme/port) used by the CLI to label its config.
func NewService(
	pool *pgxpool.Pool,
	authSvc auth.Service,
	pkiSvc pki.Service,
	googleClientID, controllerHost string,
) *Service {
	return &Service{
		pool:           pool,
		authSvc:        authSvc,
		pkiSvc:         pkiSvc,
		googleClientID: googleClientID,
		controllerHost: controllerHost,
	}
}

// GetAuthConfig returns the OAuth configuration the CLI needs to drive its
// own PKCE flow. No authentication is required — the workspace_slug is
// validated only to surface a clear "workspace not found" error to the user
// before they hit the browser.
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
		GoogleClientId: s.googleClientID,
		AuthEndpoint:   googleAuthEndpoint,
		TokenEndpoint:  googleTokenEndpoint,
		ControllerHost: s.controllerHost,
	}, nil
}

// TokenExchange swaps a Google OAuth authorization code for a Zecurity
// access JWT + refresh token. If invite_token is set, the caller is
// joining the workspace via that invitation; otherwise the user must
// already exist in the workspace (admins seed themselves through the web
// flow's bootstrap path).
func (s *Service) TokenExchange(ctx context.Context, req *clientv1.TokenExchangeRequest) (*clientv1.TokenExchangeResponse, error) {
	if req.GetWorkspaceSlug() == "" || req.GetCode() == "" || req.GetCodeVerifier() == "" || req.GetRedirectUri() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_slug, code, code_verifier, redirect_uri are required")
	}

	ws, err := lookupWorkspaceBySlug(ctx, s.pool, req.GetWorkspaceSlug())
	if err != nil {
		if errors.Is(err, errWorkspaceNotFound) {
			return nil, status.Error(codes.NotFound, "workspace not found")
		}
		return nil, status.Errorf(codes.Internal, "lookup workspace: %v", err)
	}

	tokens, err := s.authSvc.ExchangeCode(ctx, req.GetCode(), req.GetCodeVerifier(), req.GetRedirectUri())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "google token exchange failed: %v", err)
	}

	claims, err := s.authSvc.VerifyIDToken(ctx, tokens.IDToken)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "id token verification failed: %v", err)
	}
	if claims.Sub == "" || claims.Email == "" {
		return nil, status.Error(codes.Unauthenticated, "id token missing required claims")
	}

	// If an invite token was supplied, validate it belongs to this workspace,
	// is still pending, and hasn't expired. We mark it accepted only after
	// the user row is upserted so a failed insert doesn't burn the token.
	var inviteRow *invitation
	if req.GetInviteToken() != "" {
		inv, err := getInvitationByToken(ctx, s.pool, req.GetInviteToken())
		if err != nil {
			if errors.Is(err, errInvitationNotFound) {
				return nil, status.Error(codes.NotFound, "invitation not found")
			}
			return nil, status.Errorf(codes.Internal, "lookup invitation: %v", err)
		}
		if inv.WorkspaceID != ws.ID {
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

	// Upsert the user. With an invite, missing users are created as 'member'.
	// Without an invite, an unknown user is rejected — the admin must invite
	// them first or they must complete the web bootstrap flow.
	user, created, err := upsertUser(ctx, s.pool, ws.ID, claims.Email, "google", claims.Sub, inviteRow != nil)
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

	accessToken, expiresIn, err := s.authSvc.IssueAccessToken(user.ID, ws.ID, user.Role)
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
		Email:        claims.Email,
	}, nil
}

// EnrollDevice issues an mTLS leaf certificate for the calling user's
// device. The access token is read from the request body (not gRPC
// metadata) because the SPIFFE interceptor is bypassed for ClientService
// and we want the auth surface to be explicit in the proto.
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
	}, nil
}

// Compile-time check that the ClientService implementation matches the
// generated server interface. If a proto RPC is added/renamed without a
// handler, this fails to build.
var _ clientv1.ClientServiceServer = (*Service)(nil)
