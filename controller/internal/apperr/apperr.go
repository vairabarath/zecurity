// Package apperr provides a marker for errors whose message is safe to expose to
// API clients. The GraphQL ErrorPresenter (graph/resolvers) is fail-closed: it
// masks every error EXCEPT those marked here (and gqlgen's own parse/validation
// errors), so internal/DB error detail never reaches a client. Keep this package
// free of GraphQL/gqlgen imports so internal services (e.g. resource/store.go)
// can return user-safe errors without depending on the graph layer.
package apperr

import "fmt"

// UserError marks a deterministic, actionable message as safe to show to API
// clients. Use it ONLY for validation/business errors with no sensitive detail —
// never wrap a raw infra/DB error in it.
type UserError struct {
	msg string
}

func (e *UserError) Error() string { return e.msg }

// UserErrorf builds a user-safe error with a formatted message.
func UserErrorf(format string, a ...any) error {
	return &UserError{msg: fmt.Sprintf(format, a...)}
}
