---
type: phase
status: planned
sprint: 7
member: M3
phase: Phase2-Invitation-API
depends_on:
  - M2-Phase1 (011_client.sql migrated, client.graphqls + codegen done)
tags:
  - controller
  - http
  - invitation
  - email
  - graphql
---

# M3 Phase 2 — Invitation HTTP API + Email + GraphQL Resolvers

---

## What You're Building

Three HTTP endpoints for the invitation system, plus GraphQL resolvers for `createInvitation`, `invitation`, and `myDevices`.

---

## HTTP Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/api/invitations` | JWT (admin) | Create invitation, send email |
| `GET` | `/api/invitations/{token}` | None (public) | Get invitation details for the web page |
| `POST` | `/api/invitations/{token}/accept` | JWT (any user) | Mark invitation accepted after OAuth |

> Role enforcement (admin-only for POST create) is wired in Phase 3. For now, validate JWT presence only.

---

## Files to Create

### `controller/internal/invitation/handler.go`

```go
package invitation

import (
    "encoding/json"
    "net/http"

    "github.com/go-chi/chi/v5"
)

type Handler struct {
    store   *Store
    emailer *Emailer
    authSvc interface{ ValidateJWT(string) (*UserClaims, error) }
}

// POST /api/invitations
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
    // 1. Extract JWT from Authorization header → get caller user + workspace
    // 2. Decode body: { "email": "..." }
    // 3. Call store.CreateInvitation(ctx, email, workspaceID, callerUserID)
    // 4. Call emailer.SendInvitation(inv)
    // 5. Return 201 with invitation JSON
}

// GET /api/invitations/{token}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
    token := chi.URLParam(r, "token")
    // 1. store.GetByToken(ctx, token)
    // 2. If not found or expired → 404
    // 3. Return invitation JSON (id, email, workspace name, status, expires_at)
}

// POST /api/invitations/{token}/accept
func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
    token := chi.URLParam(r, "token")
    // 1. Extract JWT → get caller user ID
    // 2. store.AcceptInvitation(ctx, token, callerUserID)
    //    → marks accepted, adds user to workspace as MEMBER
    // 3. Return 200 OK
}
```

**Response shape for Create/Get:**
```json
{
  "id": "uuid",
  "email": "user@example.com",
  "status": "pending",
  "expires_at": "2026-05-05T00:00:00Z",
  "created_at": "2026-04-28T00:00:00Z"
}
```

---

### `controller/internal/invitation/store.go`

```go
package invitation

import (
    "context"
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
)

type Invitation struct {
    ID          string
    Email       string
    WorkspaceID string
    InvitedBy   string
    Token       string
    Status      string
    ExpiresAt   time.Time
    CreatedAt   time.Time
}

type Store struct {
    db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) CreateInvitation(ctx context.Context, email, workspaceID, invitedBy string) (*Invitation, error) {
    // Generate 32-byte random token
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        return nil, fmt.Errorf("generate token: %w", err)
    }
    token := hex.EncodeToString(b)

    var inv Invitation
    err := s.db.QueryRow(ctx,
        `INSERT INTO invitations (email, workspace_id, invited_by, token)
         VALUES ($1, $2, $3, $4)
         RETURNING id, email, workspace_id, invited_by, token, status, expires_at, created_at`,
        email, workspaceID, invitedBy, token,
    ).Scan(&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.InvitedBy,
        &inv.Token, &inv.Status, &inv.ExpiresAt, &inv.CreatedAt)
    return &inv, err
}

func (s *Store) GetByToken(ctx context.Context, token string) (*Invitation, error) {
    var inv Invitation
    err := s.db.QueryRow(ctx,
        `SELECT id, email, workspace_id, invited_by, token, status, expires_at, created_at
         FROM invitations WHERE token=$1`,
        token,
    ).Scan(&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.InvitedBy,
        &inv.Token, &inv.Status, &inv.ExpiresAt, &inv.CreatedAt)
    return &inv, err
}

func (s *Store) AcceptInvitation(ctx context.Context, token, userID string) error {
    tag, err := s.db.Exec(ctx,
        `UPDATE invitations SET status='accepted'
         WHERE token=$1 AND status='pending' AND expires_at > NOW()`,
        token,
    )
    if err != nil {
        return err
    }
    if tag.RowsAffected() == 0 {
        return fmt.Errorf("invitation not found or expired")
    }
    // Get workspace_id from the invitation and add user as MEMBER
    var workspaceID string
    s.db.QueryRow(ctx, `SELECT workspace_id FROM invitations WHERE token=$1`, token).Scan(&workspaceID)
    _, err = s.db.Exec(ctx,
        `INSERT INTO workspace_users (workspace_id, user_id, role)
         VALUES ($1, $2, 'member')
         ON CONFLICT (workspace_id, user_id) DO NOTHING`,
        workspaceID, userID,
    )
    return err
}
```

---

### `controller/internal/invitation/email.go`

```go
package invitation

import (
    "fmt"
    "log"
    "net/smtp"
    "os"
    "strings"
)

type Emailer struct {
    host     string
    port     string
    from     string
    password string
    baseURL  string  // e.g. "https://app.zecurity.example.com"
}

func NewEmailer(baseURL string) *Emailer {
    return &Emailer{
        host:     os.Getenv("SMTP_HOST"),
        port:     os.Getenv("SMTP_PORT"),
        from:     os.Getenv("SMTP_FROM"),
        password: os.Getenv("SMTP_PASSWORD"),
        baseURL:  baseURL,
    }
}

func (e *Emailer) SendInvitation(inv *Invitation, workspaceName string) error {
    link := fmt.Sprintf("%s/invite/%s", e.baseURL, inv.Token)

    body := fmt.Sprintf(`You've been invited to join %s on Zecurity.

Click here to accept: %s

This invitation expires in 7 days.`, workspaceName, link)

    if e.host == "" {
        // Dev mode: log to stdout
        log.Printf("[INVITE] To: %s | Link: %s", inv.Email, link)
        return nil
    }

    msg := strings.Join([]string{
        "From: " + e.from,
        "To: " + inv.Email,
        "Subject: You've been invited to Zecurity",
        "",
        body,
    }, "\r\n")

    auth := smtp.PlainAuth("", e.from, e.password, e.host)
    return smtp.SendMail(e.host+":"+e.port, auth, e.from, []string{inv.Email}, []byte(msg))
}
```

---

## GraphQL Resolvers

After `go generate ./graph/...` runs, gqlgen generates resolver stubs. Fill them in:

**`controller/graph/resolvers/client.resolvers.go`** (new file, generated stub location may vary):

```go
// Query.invitation
func (r *queryResolver) Invitation(ctx context.Context, token string) (*model.Invitation, error) {
    inv, err := r.invitationStore.GetByToken(ctx, token)
    if err != nil { return nil, nil } // return nil for not found
    if inv.Status != "pending" || inv.ExpiresAt.Before(time.Now()) { return nil, nil }
    return &model.Invitation{
        ID: inv.ID, Email: inv.Email, Status: inv.Status,
        ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt,
    }, nil
}

// Query.myDevices
func (r *queryResolver) MyDevices(ctx context.Context) ([]*model.ClientDevice, error) {
    user := auth.UserFromContext(ctx)
    if user == nil { return nil, ErrUnauthenticated }
    // SELECT * FROM client_devices WHERE user_id=$1
    return r.clientDeviceStore.ListByUser(ctx, user.ID)
}

// Mutation.createInvitation
func (r *mutationResolver) CreateInvitation(ctx context.Context, email string) (*model.Invitation, error) {
    user := auth.UserFromContext(ctx)
    if user == nil { return nil, ErrUnauthenticated }
    if user.Role != "admin" { return nil, ErrForbidden }
    inv, err := r.invitationStore.CreateInvitation(ctx, email, user.WorkspaceID, user.ID)
    if err != nil { return nil, err }
    go r.emailer.SendInvitation(inv, user.WorkspaceName) // send async
    return &model.Invitation{...}, nil
}
```

---

## Wire in `cmd/server/main.go`

```go
import invitesvc "github.com/zecurity/controller/internal/invitation"

// In main():
inviteStore  := invitesvc.NewStore(db.Pool)
inviteEmailer := invitesvc.NewEmailer(mustEnv("APP_BASE_URL"))
inviteHandler := invitesvc.NewHandler(inviteStore, inviteEmailer, authSvc)

r.Post("/api/invitations",          inviteHandler.Create)
r.Get("/api/invitations/{token}",   inviteHandler.Get)
r.Post("/api/invitations/{token}/accept", inviteHandler.Accept)
```

New env vars needed:
- `APP_BASE_URL` — e.g. `https://app.zecurity.example.com` (used for invite email link)
- `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM`, `SMTP_PASSWORD` (optional — dev mode logs if absent)

Add all to `.env.example`.

---

## Build Check

```bash
cd controller && go build ./...
```

---

## Post-Phase Fixes

_None yet._
