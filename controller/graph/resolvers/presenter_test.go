package resolvers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/scim"
)

func TestErrorPresenter_UserErrorExposed(t *testing.T) {
	err := apperr.UserErrorf("a connector named %q already exists", "prod-01")
	got := ErrorPresenter(context.Background(), err)
	if got.Message != `a connector named "prod-01" already exists` {
		t.Fatalf("UserError message should pass through, got %q", got.Message)
	}
}

func TestErrorPresenter_InfraErrorMasked(t *testing.T) {
	// A raw resolver/DB error carrying internal detail must NOT reach the client.
	secret := `duplicate key value violates unique constraint "connectors_tenant_id_name_key"`
	err := fmt.Errorf("generate connector token: insert connector: %w", fmt.Errorf("%s", secret))

	got := ErrorPresenter(context.Background(), err)

	if got.Message != "an unexpected error occurred" {
		t.Fatalf("infra error should be masked, got %q", got.Message)
	}
	if strings.Contains(got.Message, "constraint") || strings.Contains(got.Message, "duplicate") {
		t.Fatalf("masked message leaked internal detail: %q", got.Message)
	}
	if got.Extensions["code"] != "INTERNAL" {
		t.Fatalf("expected extensions.code=INTERNAL, got %v", got.Extensions["code"])
	}
}

func TestErrorPresenter_GqlErrorPassthrough(t *testing.T) {
	// gqlgen parse/validation errors are already structured — pass through.
	err := gqlerror.Errorf("Cannot query field \"bogus\" on type \"Query\".")
	got := ErrorPresenter(context.Background(), err)
	if got.Message != `Cannot query field "bogus" on type "Query".` {
		t.Fatalf("gqlerror should pass through unmasked, got %q", got.Message)
	}
}

// A client-actionable SCIM error surfaces verbatim with a branchable code, so
// the admin UI can distinguish a break-glass denial from any other failure.
func TestErrorPresenter_ScimClientErrorSurfaced(t *testing.T) {
	serr := &scim.SCIMError{
		Status:   403,
		ScimType: "",
		Detail:   `requires the "identity.mapping.break_glass" permission (ADMIN role alone is insufficient)`,
	}
	// Wrapped exactly as the resolvers wrap it.
	got := ErrorPresenter(context.Background(), fmt.Errorf("acceptScimConflict: %w", serr))

	if got.Message != serr.Detail {
		t.Fatalf("expected Detail verbatim, got %q", got.Message)
	}
	if got.Extensions["code"] != "FORBIDDEN" {
		t.Fatalf("expected extensions.code=FORBIDDEN, got %v", got.Extensions["code"])
	}
	if got.Extensions["status"] != 403 {
		t.Fatalf("expected extensions.status=403, got %v", got.Extensions["status"])
	}
}

// A 409 carries its RFC 7644 scimType so the queue can branch on the conflict
// kind rather than parsing prose.
func TestErrorPresenter_ScimConflictCarriesScimType(t *testing.T) {
	serr := &scim.SCIMError{Status: 409, ScimType: "identity_conflict", Detail: "conflict is \"rejected\""}
	got := ErrorPresenter(context.Background(), fmt.Errorf("acceptScimConflict: %w", serr))
	if got.Extensions["code"] != "CONFLICT" {
		t.Fatalf("expected CONFLICT, got %v", got.Extensions["code"])
	}
	if got.Extensions["scimType"] != "identity_conflict" {
		t.Fatalf("expected scimType=identity_conflict, got %v", got.Extensions["scimType"])
	}
}

// THE security assertion for this change. Every error-derived SCIMError Detail
// in internal/scim is a 5xx (verified by grep at the time of writing), and those
// embed raw Postgres text. Widening the whitelist to a 4xx range or to "any
// SCIMError" must fail this test.
func TestErrorPresenter_ScimInfraErrorMasked(t *testing.T) {
	secret := `lookup conflict: ERROR: duplicate key value violates unique constraint "idx_conflicts_uniq_pending"`
	serr := &scim.SCIMError{Status: 500, Detail: secret}
	got := ErrorPresenter(context.Background(), fmt.Errorf("acceptScimConflict: %w", serr))

	if got.Message != "an unexpected error occurred" {
		t.Fatalf("5xx SCIMError must be masked, got %q", got.Message)
	}
	if strings.Contains(got.Message, "constraint") || strings.Contains(got.Message, "idx_conflicts") {
		t.Fatalf("masked message leaked internal detail: %q", got.Message)
	}
	if got.Extensions["code"] != "INTERNAL" {
		t.Fatalf("expected extensions.code=INTERNAL, got %v", got.Extensions["code"])
	}
}

// A zero-valued SCIMError must not be treated as client-safe — this is why the
// whitelist is an explicit set rather than a `>= 400 && < 500` range check.
func TestErrorPresenter_ScimZeroStatusMasked(t *testing.T) {
	got := ErrorPresenter(context.Background(), fmt.Errorf("boom: %w", &scim.SCIMError{Detail: "internal detail"}))
	if got.Message != "an unexpected error occurred" {
		t.Fatalf("zero-status SCIMError must be masked, got %q", got.Message)
	}
}

// 401 is excluded on purpose: writeSCIM401's message is deliberately generic so
// a caller cannot distinguish unknown / expired / revoked tokens.
func TestErrorPresenter_Scim401Masked(t *testing.T) {
	got := ErrorPresenter(context.Background(), fmt.Errorf("x: %w", &scim.SCIMError{Status: 401, Detail: "invalid or expired token"}))
	if got.Message != "an unexpected error occurred" {
		t.Fatalf("401 must not surface through GraphQL, got %q", got.Message)
	}
}

// The updateScimConfig enable-refusal must reach the client. It is the message
// that tells an admin to use enableScimBreakGlass; masked to INTERNAL it is
// indistinguishable from a DB outage and the UI cannot offer the break-glass
// flow. Regression guard for a multi-line fmt.Errorf that a line-based apperr
// conversion missed.
func TestErrorPresenter_ScimEnableRefusalSurfaced(t *testing.T) {
	err := apperr.UserErrorf(
		"cannot enable SCIM — %s. Enabling despite an unproven mapping "+
			"requires the %q permission via enableScimBreakGlass (a mandatory reason is audited)",
		"the identity mapping is not proven", "identity.mapping.break_glass")

	got := ErrorPresenter(context.Background(), err)
	if !strings.Contains(got.Message, "enableScimBreakGlass") {
		t.Fatalf("enable refusal must surface the break-glass path, got %q", got.Message)
	}
	if got.Extensions["code"] == "INTERNAL" {
		t.Fatalf("enable refusal must not be masked as INTERNAL")
	}
}
