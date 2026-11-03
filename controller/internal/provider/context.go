package provider

import "context"

// actorKey is the unexported context key under which RequireProvider stores the
// verified provider identity. Unexported so no other package can set it —
// the only way an Actor lands in a request context is through RequireProvider.
type actorKey struct{}

// WithActor returns a context carrying the verified provider identity. Called
// only by RequireProvider, after it has verified the provider JWT and confirmed
// the user is an active entry in provider_users.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFromContext returns the provider Actor injected by RequireProvider, and
// false if none is present (which means the handler was not gated by
// RequireProvider — treat that as a 500/forbidden, never as an anonymous pass).
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}
