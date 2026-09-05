package resolvers

import (
	"context"
	"errors"
	"log"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/scim"
)

// ErrorPresenter is gqlgen's error hook. It is fail-closed: an error reaches the
// client verbatim only if it is
//
//  1. an *apperr.UserError — an intentional, user-safe message; or
//  2. a *gqlerror.Error — gqlgen's own parse/validation errors, which carry only
//     query/schema info (no internal detail) and whose masking would wreck dev UX.
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

	// SCIM engine errors carry an HTTP status and an RFC 7644 scimType. Surface
	// the CLIENT-actionable ones so the admin UI can tell a break-glass denial
	// from a missing conflict from a DB outage — today every one of them masks
	// to the same generic string, which makes the queue unbuildable.
	//
	// Two deliberate constraints:
	//
	//   - We surface serr.Detail, never err.Error(). Detail is a field whose
	//     safety is auditable per construction site; err.Error() is whatever
	//     every intermediate wrapper concatenated, so one
	//     fmt.Errorf("...: %w: %v", serr, dbErr) would leak through here.
	//     The client still learns which field failed from graphql.GetPath.
	//   - The status set is a WHITELIST, not a 4xx range check. Status is a
	//     bare int, so a range would also surface a zero value from
	//     &SCIMError{}; 401 is excluded because its message is deliberately
	//     generic to prevent token enumeration. Everything else — 5xx
	//     included, and those are the ones whose Detail embeds raw DB text —
	//     falls through to the generic mask below.
	var serr *scim.SCIMError
	if errors.As(err, &serr) && scimStatusIsClientSafe(serr.Status) {
		log.Printf("graphql scim error: path=%v: status=%d: %v", graphql.GetPath(ctx), serr.Status, err)
		gerr := graphql.DefaultErrorPresenter(ctx, err)
		gerr.Message = serr.Detail
		if gerr.Extensions == nil {
			gerr.Extensions = map[string]any{}
		}
		gerr.Extensions["code"] = scimStatusCode(serr.Status)
		gerr.Extensions["status"] = serr.Status
		if serr.ScimType != "" {
			gerr.Extensions["scimType"] = serr.ScimType
		}
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

// scimStatusIsClientSafe reports whether a SCIMError's Detail may be shown to a
// client verbatim. Explicit whitelist — see the rationale in ErrorPresenter.
// Every Detail behind these statuses is a deterministic literal built in
// internal/scim; the error-derived Details are all 5xx and stay masked.
func scimStatusIsClientSafe(status int) bool {
	switch status {
	case 400, 403, 404, 409:
		return true
	default:
		return false
	}
}

// scimStatusCode maps a SCIM HTTP status to a stable GraphQL extensions.code
// the frontend can branch on without string-matching a message.
func scimStatusCode(status int) string {
	switch status {
	case 400:
		return "BAD_REQUEST"
	case 403:
		return "FORBIDDEN"
	case 404:
		return "NOT_FOUND"
	case 409:
		return "CONFLICT"
	default:
		return "INTERNAL"
	}
}
