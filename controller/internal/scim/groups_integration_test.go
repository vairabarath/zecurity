package scim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestGroups_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_phase7_test_%d", os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := seedWorkspace(ctx, t, pool, "alpha")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	conn2 := seedSCIMConnection(ctx, t, pool, ws, "entra", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), notifier, nil, nil)
	sc := scopeFor(t, ctx, ds, ws, conn)
	sc2 := scopeFor(t, ctx, ds, ws, conn2)

	provision := func(ext, email string) string {
		res, serr := ds.Provision(ctx, sc, map[string]any{
			"userName":   email,
			"externalId": ext,
		}, "")
		if serr != nil {
			t.Fatalf("provision %s: %v", ext, serr)
		}
		return res.user.ID
	}
	provision2 := func(ext, email string) string {
		res, serr := ds.Provision(ctx, sc2, map[string]any{
			"userName":   email,
			"externalId": ext,
		}, "")
		if serr != nil {
			t.Fatalf("provision2 %s: %v", ext, serr)
		}
		return res.user.ID
	}

	t.Run("create/list/get/update/delete scim group", func(t *testing.T) {
		g, serr := ds.CreateGroup(ctx, sc, "grp-1", "Engineering")
		if serr != nil {
			t.Fatalf("create group: %v", serr)
		}
		if g.ExternalID != "grp-1" || g.Name != "Engineering" {
			t.Fatalf("unexpected group: %+v", g)
		}

		all, err := ds.ListGroups(ctx, sc)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		if len(all) != 1 {
			t.Fatalf("expected 1 group, got %d", len(all))
		}

		got, serr := ds.GetGroup(ctx, sc, "grp-1")
		if serr != nil {
			t.Fatalf("get group: %v", serr)
		}
		if got.ID != g.ID {
			t.Fatalf("group id mismatch")
		}

		upd, serr := ds.UpdateGroup(ctx, sc, "grp-1", "Engineering SCIM")
		if serr != nil {
			t.Fatalf("update group: %v", serr)
		}
		if upd.Name != "Engineering SCIM" {
			t.Fatalf("expected updated name, got %q", upd.Name)
		}

		if serr := ds.DeleteGroup(ctx, sc, "grp-1"); serr != nil {
			t.Fatalf("delete group: %v", serr)
		}
		all, _ = ds.ListGroups(ctx, sc)
		if len(all) != 0 {
			t.Fatalf("expected empty groups after delete")
		}
	})

	t.Run("patch add preserves identity (not just count)", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-add", "Add")
		u1 := provision("add-1", "add1@alpha.example.com")
		u2 := provision("add-2", "add2@alpha.example.com")

		patch := &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"add-1", "add-2"}},
		}}
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, patch); serr != nil {
			t.Fatalf("patch add: %v", serr)
		}
		members, err := ds.ListGroupMembers(ctx, sc, g.ID)
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		if len(members) != 2 {
			t.Fatalf("expected 2 members, got %v", members)
		}
		if !containsAll(members, u1, u2) {
			t.Fatalf("expected exactly {add-1,add-2}, got %v (u1=%s u2=%s)", members, u1, u2)
		}
	})

	t.Run("patch remove preserves identity (not just count)", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-rm", "Remove")
		provision("rm-1", "rm1@alpha.example.com")
		u2 := provision("rm-2", "rm2@alpha.example.com")

		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"rm-1", "rm-2"}},
		}}); serr != nil {
			t.Fatalf("seed members: %v", serr)
		}
		// Remove user-1; user-2 MUST remain.
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "remove", Values: []string{"rm-1"}},
		}}); serr != nil {
			t.Fatalf("patch remove: %v", serr)
		}
		members, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(members) != 1 {
			t.Fatalf("expected 1 member, got %v", members)
		}
		if members[0] != u2 {
			t.Fatalf("remove kept the WRONG member: got %s want %s", members[0], u2)
		}
	})

	t.Run("patch replace is exact set (not union/add)", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-rep", "Replace")
		provision("rep-1", "rep1@alpha.example.com")
		u2 := provision("rep-2", "rep2@alpha.example.com")
		provision("rep-3", "rep3@alpha.example.com")

		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"rep-1", "rep-2", "rep-3"}},
		}}); serr != nil {
			t.Fatalf("seed members: %v", serr)
		}
		// Replace with only rep-2.
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "replace", Values: []string{"rep-2"}},
		}}); serr != nil {
			t.Fatalf("patch replace: %v", serr)
		}
		members, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(members) != 1 || members[0] != u2 {
			t.Fatalf("replace must set exact membership {rep-2}, got %v", members)
		}
	})

	t.Run("patch replace with MULTIPLE values sets the exact set (Bug #2)", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-rep-multi", "RepMulti")
		provision("rmm-x", "rmm-x@alpha.example.com")
		provision("rmm-y", "rmm-y@alpha.example.com")
		uA := provision("rmm-a", "rmm-a@alpha.example.com")
		uB := provision("rmm-b", "rmm-b@alpha.example.com")
		uC := provision("rmm-c", "rmm-c@alpha.example.com")

		// Seed initial membership [X,Y].
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"rmm-x", "rmm-y"}},
		}}); serr != nil {
			t.Fatalf("seed members: %v", serr)
		}
		seed, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(seed) != 2 {
			t.Fatalf("expected 2 seeded members, got %v", seed)
		}

		// A SINGLE replace op carrying [A,B,C] must yield exactly {A,B,C},
		// not just {C} (the pre-fix bug clobbered A and B).
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "replace", Values: []string{"rmm-a", "rmm-b", "rmm-c"}},
		}}); serr != nil {
			t.Fatalf("patch replace multi: %v", serr)
		}
		members, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(members) != 3 || !containsAll(members, uA, uB, uC) {
			t.Fatalf("multi-value replace must set exactly {A,B,C}; got %v (a=%s b=%s c=%s)", members, uA, uB, uC)
		}
	})

	t.Run("mixed replace/add/remove in request order (Bug #2)", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-mix2", "Mix2")
		provision("mx-x", "mx-x@alpha.example.com")
		provision("mx-y", "mx-y@alpha.example.com")
		_ = provision("mx-a", "mx-a@alpha.example.com") // A is removed at the end; not expected in final set
		uB := provision("mx-b", "mx-b@alpha.example.com")
		uC := provision("mx-c", "mx-c@alpha.example.com")

		// Seed initial membership [X,Y].
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"mx-x", "mx-y"}},
		}}); serr != nil {
			t.Fatalf("seed members: %v", serr)
		}

		// replace [A,B] -> {A,B}; add [C] -> {A,B,C}; remove [A] -> {B,C}.
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "replace", Values: []string{"mx-a", "mx-b"}},
			{Op: "add", Values: []string{"mx-c"}},
			{Op: "remove", Values: []string{"mx-a"}},
		}}); serr != nil {
			t.Fatalf("mixed patch: %v", serr)
		}
		members, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(members) != 2 || !containsAll(members, uB, uC) {
			t.Fatalf("mixed patch must yield exactly {B,C}; got %v (b=%s c=%s)", members, uB, uC)
		}
	})

	t.Run("mixed valid+invalid member -> 404 and ZERO mutation", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-mix", "Mix")
		u1 := provision("mix-1", "mix1@alpha.example.com")
		// Seed one known member first.
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"mix-1"}},
		}}); serr != nil {
			t.Fatalf("seed member: %v", serr)
		}

		// Attempt to add a known AND an unknown member together.
		_, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"mix-1", "mix-unknown"}},
		}})
		if serr == nil || serr.Status != 404 {
			t.Fatalf("expected 404 for unknown member, got %+v", serr)
		}
		members, err := ds.ListGroupMembers(ctx, sc, g.ID)
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		if len(members) != 1 || members[0] != u1 {
			t.Fatalf("mutation must be zero: expected exactly {mix-1}, got %v", members)
		}
	})

	t.Run("cross-connection group isolation", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-iso", "Iso")
		// conn2 must not see conn1's group.
		all2, err := ds.ListGroups(ctx, sc2)
		if err != nil {
			t.Fatalf("list groups conn2: %v", err)
		}
		if len(all2) != 0 {
			t.Fatalf("conn2 must not see conn1 groups, got %d", len(all2))
		}
		// conn2 must not be able to GET conn1's group by id.
		if _, serr := ds.GetGroup(ctx, sc2, g.ID); serr == nil || serr.Status != 404 {
			t.Fatalf("conn2 GET of conn1 group must 404, got %+v", serr)
		}
		// conn2 must not be able to add members to conn1's group.
		provision("iso-user", "iso@alpha.example.com")
		if _, serr := ds.PatchGroup(ctx, sc2, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{"iso-user"}},
		}}); serr == nil || serr.Status != 404 {
			t.Fatalf("conn2 PATCH of conn1 group must 404, got %+v", serr)
		}
	})

	t.Run("cross-connection UUID member isolation", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-uuid", "UUID")
		// User belongs to conn2 only.
		u2ID := provision2("uuid-2", "uuid2@alpha.example.com")

		// conn1's group tries to add conn2's user by UUID -> must 404, no mutation.
		_, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
			{Op: "add", Values: []string{u2ID}},
		}})
		if serr == nil || serr.Status != 404 {
			t.Fatalf("cross-conn UUID add must 404, got %+v", serr)
		}
		members, _ := ds.ListGroupMembers(ctx, sc, g.ID)
		if len(members) != 0 {
			t.Fatalf("cross-conn UUID must not mutate membership, got %v", members)
		}
	})

	t.Run("membership change notifies policy change", func(t *testing.T) {
		g, _ := ds.CreateGroup(ctx, sc, "grp-notify", "Notify")
		provision("notify-1", "notify@alpha.example.com")

		before := notifier.Version(ws)
		patch := &groupPatch{Ops: []patchOp{{Op: "add", Values: []string{"notify-1"}}}}
		if _, serr := ds.PatchGroup(ctx, sc, g.ID, patch); serr != nil {
			t.Fatalf("patch: %v", serr)
		}
		after := notifier.Version(ws)
		if after != before+1 {
			t.Fatalf("expected policy version bump by 1, got before=%d after=%d", before, after)
		}
	})
}

// TestGroups_HTTP exercises the SCIM HTTP surface: RFC 7644 array-form PATCH
// values, targeted member removal, multi-value replace, and the 204-on-DELETE
// contract.
func TestGroups_HTTP(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_phase7_http_%d", os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := seedWorkspace(ctx, t, pool, "http")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	idpStore := idp.NewStore(pool, nil)
	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), notifier, nil, nil)
	store, err := NewStore(pool, []byte("test-scim-hash-key"), 0)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	router := store.Router(ds)

	tok, err := store.Mint(ctx, ws, conn, nil, nil, nil)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	auth := tok.Plaintext

	res1, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
		"userName":   "h1@http.example.com",
		"externalId": "h-1",
	}, "")
	if serr != nil {
		t.Fatalf("provision: %v", serr)
	}
	res2, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
		"userName":   "h2@http.example.com",
		"externalId": "h-2",
	}, "")
	if serr != nil {
		t.Fatalf("provision2: %v", serr)
	}
	res3, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
		"userName":   "h3@http.example.com",
		"externalId": "h-3",
	}, "")
	if serr != nil {
		t.Fatalf("provision3: %v", serr)
	}
	u1ID, u2ID, u3ID := res1.user.ID, res2.user.ID, res3.user.ID

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/scim+json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+auth)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	// Create a group via HTTP.
	rec := do("POST", "/scim/v2/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"externalId":"http-grp","displayName":"HTTP"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created group: %v", err)
	}

	// RFC 7644 array-form PATCH add.
	rec = do("PATCH", "/scim/v2/Groups/"+created.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":[{"op":"add","path":"members","value":[{"value":"h-1"},{"value":"h-2"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("array PATCH add: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var g scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode patched group: %v", err)
	}
	if len(g.Members) != 2 {
		t.Fatalf("expected 2 members after array add, got %d (%v)", len(g.Members), g.Members)
	}
	mvals := memberValues(g)
	if !(containsStr(mvals, u1ID) && containsStr(mvals, u2ID)) {
		t.Fatalf("array add must contain h-1 and h-2 uuids, got %v", mvals)
	}

	// Targeted removal of h-1 via members[value eq "h-1"].
	rec = do("PATCH", "/scim/v2/Groups/"+created.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":[{"op":"remove","path":"members[value eq \"h-1\"]"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("targeted remove: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode after remove: %v", err)
	}
	if len(g.Members) != 1 || g.Members[0].Value != u2ID {
		t.Fatalf("targeted remove must leave only h-2 (uuid %s), got %v", u2ID, g.Members)
	}

	// Unknown member via HTTP -> 404 error envelope, no mutation.
	rec = do("PATCH", "/scim/v2/Groups/"+created.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":[{"op":"add","path":"members","value":[{"value":"h-2"},{"value":"h-unknown"}]}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown member: expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do("GET", "/scim/v2/Groups/"+created.ID, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Members) != 1 || g.Members[0].Value != u2ID {
		t.Fatalf("mutation must be zero on failed PATCH, got %v", g.Members)
	}

	// Single RFC 7644 replace op with MULTIPLE members (Bug #2) via HTTP.
	// A single replace carrying [h-1, h-2, h-3] must yield exactly those three.
	rec = do("PATCH", "/scim/v2/Groups/"+created.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"members","value":[{"value":"h-1"},{"value":"h-2"},{"value":"h-3"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("multi-replace PATCH: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Verify immediately via GET that the set is exactly A,B,C.
	rec = do("GET", "/scim/v2/Groups/"+created.ID, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode after multi-replace: %v", err)
	}
	if len(g.Members) != 3 {
		t.Fatalf("multi-value replace must return 3 members, got %d (%v)", len(g.Members), g.Members)
	}
	gmvals := memberValues(g)
	if !(containsStr(gmvals, u1ID) && containsStr(gmvals, u2ID) && containsStr(gmvals, u3ID)) {
		t.Fatalf("multi-replace must contain exactly h-1,h-2,h-3 uuids, got %v", gmvals)
	}

	// Successful DELETE -> 204 No Content, empty body.
	rec = do("DELETE", "/scim/v2/Groups/"+created.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("DELETE: expected empty body, got %q", rec.Body.String())
	}
	rec = do("GET", "/scim/v2/Groups/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: expected 404, got %d", rec.Code)
	}

	// ── Okta "Push Groups" (push type: By name) ──────────────────────────────
	// Okta sends displayName ONLY, no externalId. Before the fix this was
	// rejected at the handler with 400 "externalId is required" before
	// CreateGroup was ever reached, so Okta's group push failed with
	// "Errors reported by remote server: externalId is required".
	// The handler now derives a fallback key via DeriveGroupExternalID.
	rec = do("POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"hermes"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Okta push-by-name (displayName only): expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var pushed scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &pushed); err != nil {
		t.Fatalf("decode pushed group: %v", err)
	}
	if pushed.DisplayName != "hermes" {
		t.Fatalf("pushed group displayName = %q, want %q", pushed.DisplayName, "hermes")
	}
	if pushed.ExternalID != "hermes" {
		t.Fatalf("pushed group externalId should be derived as %q, got %q", "hermes", pushed.ExternalID)
	}

	// Multi-word name derives a slug.
	rec = do("POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Marketing Team"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("push-by-name multi-word: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var pushed2 scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &pushed2); err != nil {
		t.Fatalf("decode pushed group 2: %v", err)
	}
	if pushed2.ExternalID != "marketing-team" {
		t.Fatalf("derived externalId = %q, want %q", pushed2.ExternalID, "marketing-team")
	}

	// Still fail closed when neither externalId nor a derivable displayName
	// is supplied — the fallback must not become an "accept anything" path.
	rec = do("POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"!!!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("undrivable displayName: expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do("POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no externalId and no displayName: expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	// ── Okta "Push Groups" first-push PATCH shape ─────────────────────────
	// Okta sends the initial membership set as a bare array of member objects
	// with NO "path" and NO wrapping {"members":...} object:
	//   {"op":"add","value":[{"value":"<user-scim-id>"}, ...]}
	// Before the fix this hit groupMemberValues' "group PATCH requires a
	// members path or a members value object" 400 (groups.go) and Okta logged
	// "Error while creating user group <name>: Bad Request. ... group PATCH
	// requires a members path or a members value object". This is the exact
	// payload Okta emitted during the 08-29-2026 hermes group push.
	rec = do("POST", "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"okta-bare-array"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create okta-bare-array group: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var bare scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &bare); err != nil {
		t.Fatalf("decode okta-bare-array group: %v", err)
	}
	rec = do("PATCH", "/scim/v2/Groups/"+bare.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":[{"op":"add","value":[{"value":"h-1"},{"value":"h-2"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Okta bare-array PATCH add: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var bg scimGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &bg); err != nil {
		t.Fatalf("decode bare-array patched group: %v", err)
	}
	if len(bg.Members) != 2 {
		t.Fatalf("Okta bare-array PATCH: expected 2 members, got %d (%v)", len(bg.Members), bg.Members)
	}
	bvals := memberValues(bg)
	if !(containsStr(bvals, u1ID) && containsStr(bvals, u2ID)) {
		t.Fatalf("Okta bare-array PATCH must contain h-1 and h-2 uuids, got %v", bvals)
	}

	// ── Okta group-metadata sync PATCH (no members key) ───────────────────
	// Okta also sends a group-replace PATCH carrying only displayName/id and
	// NO "members" key: {"op":"replace","value":{"displayName":"x","id":"<g>"}}
	// (observed live 08-31-2026). This is a metadata sync with no membership
	// change and must be a no-op 200, not the "members path or value object" 400.
	rec = do("PATCH", "/scim/v2/Groups/"+bare.ID,
		`{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":[{"op":"replace","value":{"displayName":"okta-bare-array","id":"`+bare.ID+`"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Okta metadata-replace PATCH (no members): expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

type scimGroup struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	ExternalID  string `json:"externalId"`
	Members     []struct {
		Value string `json:"value"`
	} `json:"members"`
}

func containsAll(haystack []string, needles ...string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return len(set) == len(needles)
}

func memberValues(g scimGroup) []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, m.Value)
	}
	return out
}

func containsStr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
