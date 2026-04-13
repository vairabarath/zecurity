package auth

import (
	"context"
	"net/http"

	"github.com/yourorg/ztna/controller/graph/model"
)

// Service is the contract between Member 4 (consumer)
// and Member 2 (implementor).
// Member 4 depends on this interface.
// Member 2 writes the concrete implementation.
// Neither touches the other's files.
type Service interface {
	// InitiateAuth builds the IdP redirect URL with PKCE.
	// Called by the initiateAuth GraphQL mutation resolver.
	// workspaceName is optional — set during signup flow, empty during normal login.
	InitiateAuth(ctx context.Context, provider string, workspaceName *string) (*model.AuthInitPayload, error)

	// CallbackHandler handles GET /auth/callback.
	// Google redirects here after user authenticates.
	// Verifies state, exchanges code, calls Bootstrap,
	// issues JWT, sets refresh cookie, redirects React.
	CallbackHandler() http.Handler

	// RefreshHandler handles POST /auth/refresh.
	// Reads httpOnly refresh cookie, issues new JWT.
	RefreshHandler() http.Handler
}
