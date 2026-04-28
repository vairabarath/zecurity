---
type: phase
status: planned
sprint: 7
member: M3
phase: Phase1-ClientService-gRPC
depends_on:
  - M2-Phase1 (proto stubs generated via buf generate)
tags:
  - grpc
  - controller
  - oauth
  - pki
---

# M3 Phase 1 — ClientService gRPC Implementation

---

## What You're Building

A new Go package `controller/internal/client/` implementing the `ClientService` gRPC server. Three RPCs:
- `GetAuthConfig` — return Google OAuth config to the CLI (no auth)
- `TokenExchange` — exchange Google OAuth code for a Zecurity JWT (reuses existing auth internals)
- `EnrollDevice` — issue mTLS cert from a CSR (reuses existing PKI)

Register the service on the existing gRPC server (port 9090, plain TLS — no mTLS required for this service).

---

## Existing Code to Reuse

| What | Where |
|------|-------|
| Google token exchange | `controller/internal/auth/exchange.go` |
| Google ID token verify | `controller/internal/auth/idtoken.go` |
| JWT issuance | `controller/internal/auth/session.go` — `IssueAccessToken()` |
| Refresh token | `controller/internal/auth/session.go` — `IssueRefreshToken()` |
| User upsert / bootstrap | `controller/internal/auth/bootstrap.go` (or similar bootstrap service) |
| Sign CSR → cert | `controller/internal/pki/service.go` — `SignCSR()` |
| Workspace CA lookup | `controller/internal/pki/service.go` |
| DB pool | passed in from `cmd/server/main.go` |

---

## Files to Create

### `controller/internal/client/service.go`

```go
package client

import (
    "context"
    "crypto/x509"
    "encoding/pem"
    "fmt"

    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"

    clientv1 "github.com/zecurity/controller/proto/client/v1"
    "github.com/zecurity/controller/internal/auth"
    "github.com/zecurity/controller/internal/pki"
)

type Service struct {
    clientv1.UnimplementedClientServiceServer
    db           *pgxpool.Pool
    authSvc      *auth.Service
    pkiSvc       *pki.Service
    googleClientID     string
    googleClientSecret string
    controllerHost     string  // e.g. "controller.example.com"
}

func NewService(db *pgxpool.Pool, authSvc *auth.Service, pkiSvc *pki.Service,
    googleClientID, googleClientSecret, controllerHost string) *Service {
    return &Service{
        db:                 db,
        authSvc:            authSvc,
        pkiSvc:             pkiSvc,
        googleClientID:     googleClientID,
        googleClientSecret: googleClientSecret,
        controllerHost:     controllerHost,
    }
}

// GetAuthConfig — no auth required.
func (s *Service) GetAuthConfig(ctx context.Context, req *clientv1.GetAuthConfigRequest) (*clientv1.GetAuthConfigResponse, error) {
    // Validate workspace exists
    var exists bool
    err := s.db.QueryRow(ctx,
        `SELECT EXISTS(SELECT 1 FROM workspaces WHERE slug=$1)`, req.WorkspaceSlug,
    ).Scan(&exists)
    if err != nil || !exists {
        return nil, status.Errorf(codes.NotFound, "workspace not found")
    }

    return &clientv1.GetAuthConfigResponse{
        GoogleClientId: s.googleClientID,
        AuthEndpoint:   "https://accounts.google.com/o/oauth2/v2/auth",
        TokenEndpoint:  "https://oauth2.googleapis.com/token",
        ControllerHost: s.controllerHost,
    }, nil
}

// TokenExchange — exchange Google OAuth code for Zecurity JWT.
func (s *Service) TokenExchange(ctx context.Context, req *clientv1.TokenExchangeRequest) (*clientv1.TokenExchangeResponse, error) {
    // 1. Exchange code with Google (reuse auth.ExchangeCode)
    googleTokens, err := s.authSvc.ExchangeCode(ctx, req.Code, req.CodeVerifier, req.RedirectUri)
    if err != nil {
        return nil, status.Errorf(codes.Unauthenticated, "google token exchange failed: %v", err)
    }

    // 2. Verify Google ID token
    claims, err := s.authSvc.VerifyIDToken(ctx, googleTokens.IDToken)
    if err != nil {
        return nil, status.Errorf(codes.Unauthenticated, "id token verification failed: %v", err)
    }

    // 3. Get workspace
    var workspaceID string
    err = s.db.QueryRow(ctx,
        `SELECT id FROM workspaces WHERE slug=$1`, req.WorkspaceSlug,
    ).Scan(&workspaceID)
    if err != nil {
        return nil, status.Errorf(codes.NotFound, "workspace not found")
    }

    // 4. Upsert user (get or create)
    user, err := s.authSvc.UpsertUser(ctx, claims.Email, claims.Sub, workspaceID)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "user upsert failed: %v", err)
    }

    // 5. If invite_token present: accept invitation, ensure user is MEMBER
    if req.InviteToken != "" {
        if err := acceptInvitation(ctx, s.db, req.InviteToken, user.ID, workspaceID); err != nil {
            return nil, status.Errorf(codes.InvalidArgument, "invitation error: %v", err)
        }
    }

    // 6. Issue JWT
    accessToken, expiresIn, err := s.authSvc.IssueAccessToken(user)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "token issuance failed: %v", err)
    }

    // 7. Issue refresh token
    refreshToken, err := s.authSvc.IssueRefreshToken(ctx, user.ID)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "refresh token failed: %v", err)
    }

    return &clientv1.TokenExchangeResponse{
        AccessToken:  accessToken,
        RefreshToken: refreshToken,
        ExpiresIn:    expiresIn,
        Email:        claims.Email,
    }, nil
}

// EnrollDevice — issue mTLS cert from CSR.
func (s *Service) EnrollDevice(ctx context.Context, req *clientv1.EnrollDeviceRequest) (*clientv1.EnrollDeviceResponse, error) {
    // 1. Validate JWT from request field (not gRPC metadata)
    user, workspaceID, err := s.authSvc.ValidateAccessToken(req.AccessToken)
    if err != nil {
        return nil, status.Errorf(codes.Unauthenticated, "invalid access token")
    }

    // 2. Parse CSR
    block, _ := pem.Decode([]byte(req.CsrPem))
    if block == nil || block.Type != "CERTIFICATE REQUEST" {
        return nil, status.Errorf(codes.InvalidArgument, "invalid CSR PEM")
    }
    csr, err := x509.ParseCertificateRequest(block.Bytes)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "CSR parse error: %v", err)
    }
    if err := csr.CheckSignature(); err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "CSR signature invalid")
    }

    // 3. Get workspace slug for SPIFFE ID
    var workspaceSlug string
    s.db.QueryRow(ctx, `SELECT slug FROM workspaces WHERE id=$1`, workspaceID).Scan(&workspaceSlug)

    // 4. Create device record first (to get device UUID for SPIFFE ID)
    deviceID, err := insertClientDevice(ctx, s.db, user.ID, workspaceID, req.DeviceName, req.Os)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "device record failed: %v", err)
    }

    // 5. Build SPIFFE ID
    spiffeID := fmt.Sprintf("spiffe://ws-%s.zecurity.in/client/%s", workspaceSlug, deviceID)

    // 6. Sign CSR via PKI service (reuse existing method)
    certPEM, workspaceCAPEM, intermediateCAPEM, serial, notAfter, err := s.pkiSvc.SignClientCSR(ctx, workspaceID, csr, spiffeID)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "cert issuance failed: %v", err)
    }

    // 7. Update device with cert info
    s.db.Exec(ctx,
        `UPDATE client_devices SET cert_serial=$1, cert_not_after=$2, spiffe_id=$3 WHERE id=$4`,
        serial, notAfter, spiffeID, deviceID,
    )

    return &clientv1.EnrollDeviceResponse{
        CertificatePem:    certPEM,
        WorkspaceCaPem:    workspaceCAPEM,
        IntermediateCaPem: intermediateCAPEM,
        SpiffeId:          spiffeID,
    }, nil
}
```

### `controller/internal/client/store.go`

```go
package client

import (
    "context"
    "fmt"
    "github.com/jackc/pgx/v5/pgxpool"
)

func insertClientDevice(ctx context.Context, db *pgxpool.Pool,
    userID, workspaceID, name, os string) (string, error) {
    var id string
    err := db.QueryRow(ctx,
        `INSERT INTO client_devices (user_id, workspace_id, name, os)
         VALUES ($1, $2, $3, $4)
         RETURNING id`,
        userID, workspaceID, name, os,
    ).Scan(&id)
    if err != nil {
        return "", fmt.Errorf("insert client_device: %w", err)
    }
    return id, nil
}

func acceptInvitation(ctx context.Context, db *pgxpool.Pool,
    token, userID, workspaceID string) error {
    tag, err := db.Exec(ctx,
        `UPDATE invitations
         SET status = 'accepted'
         WHERE token = $1
           AND workspace_id = $2
           AND status = 'pending'
           AND expires_at > NOW()`,
        token, workspaceID,
    )
    if err != nil {
        return fmt.Errorf("accept invitation: %w", err)
    }
    if tag.RowsAffected() == 0 {
        return fmt.Errorf("invitation not found, already used, or expired")
    }
    // Ensure user has MEMBER role in workspace
    _, err = db.Exec(ctx,
        `INSERT INTO workspace_users (workspace_id, user_id, role)
         VALUES ($1, $2, 'member')
         ON CONFLICT (workspace_id, user_id) DO NOTHING`,
        workspaceID, userID,
    )
    return err
}
```

---

## Files to Modify

### `cmd/server/main.go`

Add ClientService registration after existing service registrations:

```go
// Import:
clientv1 "github.com/zecurity/controller/proto/client/v1"
clientsvc "github.com/zecurity/controller/internal/client"

// In main(), after existing service inits:
clientSvc := clientsvc.NewService(
    db.Pool,
    authSvc,
    pkiSvc,
    mustEnv("GOOGLE_CLIENT_ID"),
    mustEnv("GOOGLE_CLIENT_SECRET"),
    mustEnv("CONTROLLER_HOST"),  // add this env var
)
clientv1.RegisterClientServiceServer(grpcServer, clientSvc)
```

> **Note:** `CONTROLLER_HOST` is a new env var — the hostname/domain of the controller, used by the CLI to construct the OAuth redirect URI. Add to `.env.example`.

---

## PKI Note: `SignClientCSR`

Check if `pki.Service` already has a method that signs arbitrary CSRs. If it has `SignCSR(ctx, workspaceID, csr *x509.CertificateRequest, spiffeID string)` or similar, reuse it directly.

If not, add `SignClientCSR` to `controller/internal/pki/service.go` following the same pattern as the existing connector CSR signing — same CA hierarchy, same 7-day validity, same P-384 validation, just with `/client/{id}` in the SPIFFE path.

---

## Build Check

```bash
cd controller && go build ./...
```

Must pass with no errors.

---

## Post-Phase Fixes

_None yet._
