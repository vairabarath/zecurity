// Package spiffe holds the verified SPIFFE identity that the gRPC interceptor
// injects into the request context, plus the accessors handlers use to read it.
//
// It lives in its own leaf package (no internal imports) so that any handler
// package — connector, shield, … — can read the identity without creating an
// import cycle through the package that owns the interceptor.
package spiffe

import "context"

type (
	idKey          struct{}
	roleKey        struct{}
	entityIDKey    struct{}
	trustDomainKey struct{}
)

// WithIdentity returns a context carrying the verified SPIFFE identity.
// Called only by the gRPC SPIFFE interceptor, after it has parsed and verified
// the peer certificate.
func WithIdentity(ctx context.Context, spiffeID, role, entityID, trustDomain string) context.Context {
	ctx = context.WithValue(ctx, idKey{}, spiffeID)
	ctx = context.WithValue(ctx, roleKey{}, role)
	ctx = context.WithValue(ctx, entityIDKey{}, entityID)
	ctx = context.WithValue(ctx, trustDomainKey{}, trustDomain)
	return ctx
}

// ID returns the full SPIFFE URI (e.g. spiffe://<td>/<role>/<id>), or "".
func ID(ctx context.Context) string { v, _ := ctx.Value(idKey{}).(string); return v }

// Role returns the SPIFFE role ("connector", "shield", "controller"), or "".
func Role(ctx context.Context) string { v, _ := ctx.Value(roleKey{}).(string); return v }

// EntityID returns the entity-specific id (e.g. connector UUID), or "".
func EntityID(ctx context.Context) string { v, _ := ctx.Value(entityIDKey{}).(string); return v }

// TrustDomain returns the trust domain from the SPIFFE URI, or "".
func TrustDomain(ctx context.Context) string { v, _ := ctx.Value(trustDomainKey{}).(string); return v }
