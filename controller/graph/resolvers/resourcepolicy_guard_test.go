package resolvers

// resourcepolicy_guard_test.go -- PENDING-16 Phase 3.
//
// HTTP-level tests for the parts that are about presentation rather than
// business behaviour: the @hasRole(ADMIN) guard and the user-facing error
// wording. These need no database, so they run everywhere.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// execResourcePolicyGQL runs one GraphQL document as the given role and returns
// the decoded error messages.
func execResourcePolicyGQL(t *testing.T, role, query string) []string {
	t.Helper()

	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: uuid.NewString(),
		UserID:   uuid.NewString(),
		Role:     role,
	})

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: &Resolver{PostureStore: posture.NewStore(nil)},
		Directives: graph.DirectiveRoot{
			HasRole: HasRole,
		},
	}))
	srv.SetErrorPresenter(ErrorPresenter)

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatalf("marshal query: %v", err)
	}
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, req)

	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, recorder.Body.String())
	}

	messages := make([]string, len(response.Errors))
	for i, e := range response.Errors {
		messages[i] = e.Message
	}
	return messages
}

// Every Resource Policy operation is administrative and must be refused for a
// non-admin caller.
func TestResourcePolicyOperationsRequireAdmin(t *testing.T) {
	const policyID = "00000000-0000-0000-0000-000000000001"
	const profileID = "00000000-0000-0000-0000-000000000002"
	const resourceID = "00000000-0000-0000-0000-000000000003"

	cases := map[string]string{
		"resourcePolicies": `query { resourcePolicies { id } }`,
		"resourcePolicy":   `query { resourcePolicy(id: "` + policyID + `") { id } }`,

		"createResourcePolicy": `mutation { createResourcePolicy(name: "X") { id } }`,
		"updateResourcePolicy": `mutation { updateResourcePolicy(id: "` + policyID + `", name: "X") { id } }`,
		"deleteResourcePolicy": `mutation { deleteResourcePolicy(id: "` + policyID + `") }`,

		"assignResourcePolicy": `mutation { assignResourcePolicy(resourceId: "` + resourceID +
			`", policyId: "` + policyID + `") { id } }`,
		"unassignResourcePolicy": `mutation { unassignResourcePolicy(resourceId: "` + resourceID + `") { id } }`,

		"addProfileToResourcePolicy": `mutation { addProfileToResourcePolicy(policyId: "` + policyID +
			`", profileId: "` + profileID + `") { id } }`,
		"removeProfileFromResourcePolicy": `mutation { removeProfileFromResourcePolicy(policyId: "` + policyID +
			`", profileId: "` + profileID + `") { id } }`,
	}

	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			messages := execResourcePolicyGQL(t, "member", query)
			if len(messages) != 1 || !strings.Contains(messages[0], "forbidden") {
				t.Fatalf("errors = %#v, want a single forbidden error", messages)
			}
		})
	}
}

// A malformed UUID must produce a safe user-facing message, never a raw
// internal error, and must not reach the store (PostureStore has a nil pool
// here, so any store call would panic).
func TestResourcePolicyInvalidIDsReturnUserErrors(t *testing.T) {
	cases := map[string]struct {
		query string
		want  string
	}{
		"resourcePolicy": {
			query: `query { resourcePolicy(id: "not-a-uuid") { id } }`,
			want:  "invalid resource policy id",
		},
		"updateResourcePolicy": {
			query: `mutation { updateResourcePolicy(id: "not-a-uuid", name: "X") { id } }`,
			want:  "invalid resource policy id",
		},
		"deleteResourcePolicy": {
			query: `mutation { deleteResourcePolicy(id: "not-a-uuid") }`,
			want:  "invalid resource policy id",
		},
		"assignResourcePolicy_resource": {
			query: `mutation { assignResourcePolicy(resourceId: "not-a-uuid",` +
				` policyId: "00000000-0000-0000-0000-000000000001") { id } }`,
			want: "invalid resource id",
		},
		"assignResourcePolicy_policy": {
			query: `mutation { assignResourcePolicy(resourceId: "00000000-0000-0000-0000-000000000003",` +
				` policyId: "not-a-uuid") { id } }`,
			want: "invalid resource policy id",
		},
		"unassignResourcePolicy": {
			query: `mutation { unassignResourcePolicy(resourceId: "not-a-uuid") { id } }`,
			want:  "invalid resource id",
		},
		"addProfileToResourcePolicy_policy": {
			query: `mutation { addProfileToResourcePolicy(policyId: "not-a-uuid",` +
				` profileId: "00000000-0000-0000-0000-000000000002") { id } }`,
			want: "invalid resource policy id",
		},
		"addProfileToResourcePolicy_profile": {
			query: `mutation { addProfileToResourcePolicy(policyId: "00000000-0000-0000-0000-000000000001",` +
				` profileId: "not-a-uuid") { id } }`,
			want: "invalid profile id",
		},
		"removeProfileFromResourcePolicy_profile": {
			query: `mutation { removeProfileFromResourcePolicy(policyId: "00000000-0000-0000-0000-000000000001",` +
				` profileId: "not-a-uuid") { id } }`,
			want: "invalid profile id",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			messages := execResourcePolicyGQL(t, "admin", tc.query)
			if len(messages) != 1 || messages[0] != tc.want {
				t.Fatalf("errors = %#v, want [%q]", messages, tc.want)
			}
		})
	}
}

// A blank policy name is rejected before the store is touched, with the
// mutation-name-prefixed wording the project uses.
func TestCreateResourcePolicyBlankNameReturnsUserError(t *testing.T) {
	messages := execResourcePolicyGQL(t, "admin", `mutation { createResourcePolicy(name: "   ") { id } }`)
	const want = "createResourcePolicy: name is required"
	if len(messages) != 1 || messages[0] != want {
		t.Fatalf("errors = %#v, want [%q]", messages, want)
	}
}
