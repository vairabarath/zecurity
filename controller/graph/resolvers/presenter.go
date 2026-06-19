package resolvers

import (
	"context"
	"errors"
	"log"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/yourorg/ztna/controller/internal/apperr"
)

// ErrorPresenter is gqlgen's error hook. It is fail-closed: an error reaches the
// client verbatim only if it is
//
//	1. an *apperr.UserError — an intentional, user-safe message; or
//	2. a *gqlerror.Error — gqlgen's own parse/validation errors, which carry only
//	   query/schema info (no internal detail) and whose masking would wreck dev UX.
//
// Every other error — raw resolver/DB/infra failures wrapped with fmt.Errorf — is
// logged server-side and replaced with a generic message, so internals (Postgres
// constraint names, wrapped driver errors, file paths) never leak to clients.
func ErrorPresenter(ctx context.Context, err error) *gqlerror.Error {
	var ue *apperr.UserError
	if errors.As(err, &ue) {
		gerr := graphql.DefaultErrorPresenter(ctx, err)
		gerr.Message = ue.Error()
		return gerr
	}

	// Already-structured GraphQL errors (parse/validation) pass through unmasked.
	var ge *gqlerror.Error
	if errors.As(err, &ge) {
		return graphql.DefaultErrorPresenter(ctx, err)
	}

	// Raw resolver/infra error — log the real detail, return a generic message.
	log.Printf("graphql internal error: path=%v: %v", graphql.GetPath(ctx), err)
	gerr := graphql.DefaultErrorPresenter(ctx, err)
	gerr.Message = "an unexpected error occurred"
	if gerr.Extensions == nil {
		gerr.Extensions = map[string]any{}
	}
	gerr.Extensions["code"] = "INTERNAL"
	return gerr
}
