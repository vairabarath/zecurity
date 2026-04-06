package tenant

import "context"

// contextKey is an unexported named type.
// Prevents key collisions with any other package storing values in context.
// Never use a raw string as a context key.
type contextKey string

const key contextKey = "tenantContext"

// TenantContext holds the verified identity for one request.
// Extracted from the JWT by AuthMiddleware.
// Every resolver and DB call reads from this — never from raw JWT claims.
// All three fields are populated together or not at all.
type TenantContext struct {
	TenantID string // workspace UUID
	UserID   string // user UUID
	Role     string // "admin" | "member" | "viewer"
}

// Set stores a TenantContext into ctx.
// Called only by AuthMiddleware after JWT verification succeeds.
func Set(ctx context.Context, tc TenantContext) context.Context {
	return context.WithValue(ctx, key, tc)
}

// Get retrieves the TenantContext from ctx.
// Returns (zero, false) if not present.
// Use this when absence is a valid case (e.g. public route handlers).
func Get(ctx context.Context) (TenantContext, bool) {
	tc, ok := ctx.Value(key).(TenantContext)
	return tc, ok
}

// MustGet retrieves the TenantContext from ctx.
// Panics if not present.
//
// Use this in all resolvers and repository functions.
// A missing TenantContext at this point means middleware was bypassed —
// that is always a programming error, never a user error.
// It must panic loudly so it gets caught and fixed immediately.
// A silent error return would let it go unnoticed until production.
func MustGet(ctx context.Context) TenantContext {
	tc, ok := Get(ctx)
	if !ok {
		panic(
			"tenant.MustGet: TenantContext not in context. " +
				"AuthMiddleware was bypassed. This is a code bug.",
		)
	}
	return tc
}
