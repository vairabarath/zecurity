package resolvers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/yourorg/ztna/controller/internal/apperr"
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
