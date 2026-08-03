package auth

import (
	"context"
	"fmt"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/idp"
)

// providerForFn is the adapter-selection seam. Production points it at
// ProviderFor; tests override it to inject a fake adapter without a network.
var providerForFn = ProviderFor

// googleCreds returns the Bootstrap-tier Google client credentials this service
// was configured with (env-sourced).
func (s *serviceImpl) googleCreds() GoogleCreds {
	return GoogleCreds{ClientID: s.cfg.GoogleClientID, ClientSecret: s.cfg.GoogleClientSecret}
}

// resolveConnection is the single connection-resolution entry point. If a
// connectionID is supplied it is loaded directly (Enterprise IdP, Chunk F);
// otherwise the platform (Bootstrap) connection for the provider is resolved.
func (s *serviceImpl) resolveConnection(ctx context.Context, provider, connectionID string) (*idp.Connection, error) {
	if connectionID != "" {
		return s.idpStore.GetByID(ctx, connectionID)
	}
	return s.idpStore.GetPlatformByProvider(ctx, provider)
}

// GoogleCreds are the Bootstrap-tier Google OAuth client credentials, sourced
// from platform env — distinct for the web (GOOGLE_CLIENT_ID) and the CLI
// (CLIENT_GOOGLE_CLIENT_ID) flows, so the caller supplies its own.
type GoogleCreds struct {
	ClientID     string
	ClientSecret string
}

// ProviderFor selects the protocol adapter for a resolved connection
// (PENDING-04 / ADR-024). Managed (Bootstrap-tier) connections resolve to a
// platform adapter using env creds; workspace (Enterprise-tier) OIDC connections
// use their own stored, decrypted creds. This is the single place "which adapter"
// is decided — shared by the web (via providerForFn) and the CLI (client pkg),
// so both login surfaces honor the same switch point (auth invariant #2).
func ProviderFor(conn *idp.Connection, google GoogleCreds) (providers.IdentityProvider, error) {
	if conn == nil {
		return nil, fmt.Errorf("nil connection")
	}
	if conn.Managed {
		switch conn.Provider {
		case "google":
			return NewGoogleProvider(google.ClientID, google.ClientSecret), nil
		default:
			return nil, fmt.Errorf("unsupported managed provider: %q", conn.Provider)
		}
	}
	switch conn.Protocol {
	case "oidc", "":
		return providers.NewOIDCProvider(conn.Provider, conn.Issuer, conn.ClientID, conn.ClientSecret, conn.DiscoveryURL, conn.Scopes), nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %q", conn.Protocol)
	}
}
