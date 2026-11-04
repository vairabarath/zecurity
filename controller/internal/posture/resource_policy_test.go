package posture

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

// newPolicyTestDB makes a throwaway database with every migration applied.
func newPolicyTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := postureTestDBName(t)
	admin := postureTestPool(t, ctx, adminDSN)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	dsn, err := postureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := postureTestPool(t, ctx, dsn)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		admin.Close()
	})
	if err := applyPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return ctx, pool
}

func profileWithRequirement(t *testing.T, ctx context.Context, s *Store, ws uuid.UUID, name string) uuid.UUID {
	t.Helper()
	p, err := s.CreateProfile(ctx, ws, name, false)
	if err != nil {
		t.Fatalf("create profile %s: %v", name, err)
	}
	if err := s.AddRequirement(ctx, ws, p.ID, Requirement{CheckID: CheckFirewall}); err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	return p.ID
}

func policyOf(t *testing.T, ctx context.Context, s *Store, ws, resourceID uuid.UUID) *ResourcePolicy {
	t.Helper()
	p, err := s.PolicyOfResource(ctx, ws, resourceID)
	if err != nil {
		t.Fatalf("policy of resource: %v", err)
	}
	return p
}

// PENDING-16: Resource → Resource Policy → Device Profiles in the store.
func TestResourcePolicyStore(t *testing.T) {
	ctx, pool := newPolicyTestDB(t)
	s := NewStore(pool)
	wsA := insertPostureTestWorkspace(t, ctx, pool, "pol-a")
	wsB := insertPostureTestWorkspace(t, ctx, pool, "pol-b")
	resA := insertPostureTestResource(t, ctx, pool, wsA, "a")
	resB := insertPostureTestResource(t, ctx, pool, wsB, "b")

	t.Run("every workspace has one Default, and new resources are in it", func(t *testing.T) {
		list, err := s.ListResourcePolicies(ctx, wsA)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || !list[0].IsDefault || list[0].Name != "Default" || len(list[0].ProfileIDs) != 0 {
			t.Fatalf("policies = %+v, want just the Default (any device)", list)
		}
		if p := policyOf(t, ctx, s, wsA, resA); p.ID != list[0].ID || p.ResourceCount != 1 {
			t.Fatalf("resource policy = %+v, want the Default with 1 resource", p)
		}
	})

	t.Run("create, rename, describe; names unique; Default can't be renamed or deleted", func(t *testing.T) {
		p, err := s.CreateResourcePolicy(ctx, wsA, "  Engineering  ", nil)
		if err != nil || p.Name != "Engineering" || p.IsDefault {
			t.Fatalf("create = %+v, %v", p, err)
		}
		if _, err := s.CreateResourcePolicy(ctx, wsA, "Engineering", nil); !errors.Is(err, ErrPolicyNameTaken) {
			t.Fatalf("duplicate name err = %v", err)
		}
		if _, err := s.CreateResourcePolicy(ctx, wsB, "Engineering", nil); err != nil {
			t.Fatalf("same name in another workspace: %v", err)
		}
		for _, bad := range []string{"", "   ", strings.Repeat("x", 101)} {
			if _, err := s.CreateResourcePolicy(ctx, wsA, bad, nil); !errors.Is(err, ErrInvalidPolicyName) {
				t.Fatalf("name %q err = %v", bad, err)
			}
		}
		name, desc := "Engineers", "for the engineering team"
		got, err := s.UpdateResourcePolicy(ctx, wsA, p.ID, &name, &desc)
		if err != nil || got.Name != name || got.Description == nil || *got.Description != desc {
			t.Fatalf("update = %+v, %v", got, err)
		}
		def := defaultPolicy(t, ctx, s, wsA)
		newName := "Renamed"
		if _, err := s.UpdateResourcePolicy(ctx, wsA, def.ID, &newName, nil); !errors.Is(err, ErrDefaultPolicyFixed) {
			t.Fatalf("rename Default err = %v", err)
		}
		if _, err := s.UpdateResourcePolicy(ctx, wsA, def.ID, nil, &desc); err != nil {
			t.Fatalf("describe Default: %v", err)
		}
		if _, err := s.DeleteResourcePolicy(ctx, wsA, def.ID); !errors.Is(err, ErrDefaultPolicyFixed) {
			t.Fatalf("delete Default err = %v", err)
		}
	})

	t.Run("profiles: OR set, none = any device, empty and foreign profiles refused", func(t *testing.T) {
		pol, err := s.CreateResourcePolicy(ctx, wsA, "Profiles", nil)
		if err != nil {
			t.Fatal(err)
		}
		p1 := profileWithRequirement(t, ctx, s, wsA, "p1")
		p2 := profileWithRequirement(t, ctx, s, wsA, "p2")
		got, err := s.SetResourcePolicyProfiles(ctx, wsA, pol.ID, []uuid.UUID{p2, p1, p2})
		if err != nil || len(got.ProfileIDs) != 2 {
			t.Fatalf("set profiles = %+v, %v; want 2 (deduplicated)", got, err)
		}
		empty, _ := s.CreateProfile(ctx, wsA, "empty", false)
		if _, err := s.SetResourcePolicyProfiles(ctx, wsA, pol.ID, []uuid.UUID{p1, empty.ID}); !errors.Is(err, ErrEmptyProfileInPolicy) {
			t.Fatalf("empty profile err = %v", err)
		}
		foreign := profileWithRequirement(t, ctx, s, wsB, "foreign")
		if _, err := s.SetResourcePolicyProfiles(ctx, wsA, pol.ID, []uuid.UUID{foreign}); !errors.Is(err, ErrPolicyProfileMissing) {
			t.Fatalf("foreign profile err = %v", err)
		}
		if got, _ := s.GetResourcePolicy(ctx, wsA, pol.ID); len(got.ProfileIDs) != 2 {
			t.Fatalf("a refused set changed the profiles: %+v", got.ProfileIDs)
		}
		if got, err := s.SetResourcePolicyProfiles(ctx, wsA, pol.ID, nil); err != nil || len(got.ProfileIDs) != 0 {
			t.Fatalf("clear profiles = %+v, %v", got, err)
		}
		if _, err := s.SetResourcePolicyProfiles(ctx, wsB, pol.ID, nil); !errors.Is(err, ErrPolicyNotFound) {
			t.Fatalf("foreign policy err = %v", err)
		}
	})

	t.Run("resource assignment: own policy only", func(t *testing.T) {
		pol, _ := s.CreateResourcePolicy(ctx, wsA, "Assign", nil)
		prev, err := s.SetResourcePolicy(ctx, wsA, resA, pol.ID)
		if err != nil || prev != defaultPolicy(t, ctx, s, wsA).ID {
			t.Fatalf("assign = %v, %v", prev, err)
		}
		polB, _ := s.CreateResourcePolicy(ctx, wsB, "Other", nil)
		if _, err := s.SetResourcePolicy(ctx, wsA, resA, polB.ID); !errors.Is(err, ErrPolicyNotFound) {
			t.Fatalf("foreign policy err = %v", err)
		}
		if _, err := s.SetResourcePolicy(ctx, wsA, resB, pol.ID); !errors.Is(err, ErrPolicyResourceGone) {
			t.Fatalf("foreign resource err = %v", err)
		}
		if p := policyOf(t, ctx, s, wsA, resA); p.ID != pol.ID {
			t.Fatalf("refused moves changed the policy: %s", p.Name)
		}
	})

	t.Run("deleting a policy moves its resources to the Default", func(t *testing.T) {
		pol, _ := s.CreateResourcePolicy(ctx, wsA, "Doomed", nil)
		if _, err := s.SetResourcePolicy(ctx, wsA, resA, pol.ID); err != nil {
			t.Fatal(err)
		}
		moved, err := s.DeleteResourcePolicy(ctx, wsA, pol.ID)
		if err != nil || len(moved) != 1 || moved[0] != resA {
			t.Fatalf("delete = %v, %v; want [resA] moved", moved, err)
		}
		if p := policyOf(t, ctx, s, wsA, resA); !p.IsDefault {
			t.Fatalf("resource is in %q, want the Default", p.Name)
		}
		if _, err := s.GetResourcePolicy(ctx, wsA, pol.ID); !errors.Is(err, ErrPolicyNotFound) {
			t.Fatalf("deleted policy still readable: %v", err)
		}
	})

	t.Run("profile in use: last requirement kept; delete needs confirmation (D-4)", func(t *testing.T) {
		only := profileWithRequirement(t, ctx, s, wsA, "only")
		shared := profileWithRequirement(t, ctx, s, wsA, "shared")
		solo, _ := s.CreateResourcePolicy(ctx, wsA, "Solo", nil)
		pair, _ := s.CreateResourcePolicy(ctx, wsA, "Pair", nil)
		if _, err := s.SetResourcePolicyProfiles(ctx, wsA, solo.ID, []uuid.UUID{only}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetResourcePolicyProfiles(ctx, wsA, pair.ID, []uuid.UUID{only, shared}); err != nil {
			t.Fatal(err)
		}

		if err := s.RemoveRequirement(ctx, wsA, only, CheckFirewall); !errors.Is(err, ErrLastRequirement) {
			t.Fatalf("remove last requirement in use err = %v", err)
		}
		free := profileWithRequirement(t, ctx, s, wsA, "free")
		if err := s.RemoveRequirement(ctx, wsA, free, CheckFirewall); err != nil {
			t.Fatalf("remove last requirement of an unused profile: %v", err)
		}

		_, err := s.DeleteProfile(ctx, wsA, only, false)
		var inUse *ProfileInUseError
		if !errors.As(err, &inUse) || len(inUse.Policies) != 2 {
			t.Fatalf("unconfirmed delete err = %v", err)
		}
		var ue *apperr.UserError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "Pair") || !strings.Contains(err.Error(), "Solo") {
			t.Fatalf("refusal must be user-safe and name the policies: %v", err)
		}
		if got, _ := s.GetResourcePolicy(ctx, wsA, solo.ID); len(got.ProfileIDs) != 1 {
			t.Fatalf("a refused delete changed the policy")
		}

		res, err := s.DeleteProfile(ctx, wsA, only, true)
		if err != nil || len(res.RemovedFrom) != 2 || len(res.NowAnyDevice) != 1 || res.NowAnyDevice[0].ID != solo.ID {
			t.Fatalf("confirmed delete = %+v, %v; want removed from 2, Solo now any device", res, err)
		}
		if got, _ := s.GetResourcePolicy(ctx, wsA, pair.ID); len(got.ProfileIDs) != 1 || got.ProfileIDs[0] != shared {
			t.Fatalf("Pair profiles = %v, want [shared]", got.ProfileIDs)
		}
		if res, err := s.DeleteProfile(ctx, wsA, free, false); err != nil || len(res.RemovedFrom) != 0 {
			t.Fatalf("delete unused profile = %+v, %v", res, err)
		}
	})

	t.Run("the compiler's view: resources with profiles only", func(t *testing.T) {
		p := profileWithRequirement(t, ctx, s, wsA, "view")
		pol, _ := s.CreateResourcePolicy(ctx, wsA, "View", nil)
		if _, err := s.SetResourcePolicyProfiles(ctx, wsA, pol.ID, []uuid.UUID{p}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetResourcePolicy(ctx, wsA, resA, pol.ID); err != nil {
			t.Fatal(err)
		}
		m, err := s.ResourceProfilesForWorkspace(ctx, wsA)
		if err != nil || len(m) != 1 || len(m[resA]) != 1 || m[resA][0] != p {
			t.Fatalf("resource profiles = %v, %v", m, err)
		}
		if mB, _ := s.ResourceProfilesForWorkspace(ctx, wsB); len(mB) != 0 {
			t.Fatalf("workspace B sees %v", mB)
		}
	})
}

// A delete and a concurrent assignment to the same policy: the assignment
// waits on the policy lock and fails, so no resource is left pointing at a
// deleted policy. The seam pauses the delete inside the window.
func TestDeleteResourcePolicyRacesAssignment(t *testing.T) {
	ctx, pool := newPolicyTestDB(t)
	s := NewStore(pool)
	ws := insertPostureTestWorkspace(t, ctx, pool, "race")
	res := insertPostureTestResource(t, ctx, pool, ws, "r")
	pol, err := s.CreateResourcePolicy(ctx, ws, "Racy", nil)
	if err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	afterPolicyDeleteLock = func() {
		close(locked)
		time.Sleep(300 * time.Millisecond)
	}
	defer func() { afterPolicyDeleteLock = nil }()

	var wg sync.WaitGroup
	var delErr, setErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, delErr = s.DeleteResourcePolicy(ctx, ws, pol.ID)
	}()
	go func() {
		defer wg.Done()
		<-locked
		_, setErr = s.SetResourcePolicy(ctx, ws, res, pol.ID)
	}()
	wg.Wait()
	if delErr != nil {
		t.Fatalf("delete: %v", delErr)
	}
	if !errors.Is(setErr, ErrPolicyNotFound) {
		t.Fatalf("racing assignment err = %v, want ErrPolicyNotFound", setErr)
	}
	if p := policyOf(t, ctx, s, ws, res); !p.IsDefault {
		t.Fatalf("resource ended in %q, want the Default", p.Name)
	}
}

func defaultPolicy(t *testing.T, ctx context.Context, s *Store, ws uuid.UUID) ResourcePolicy {
	t.Helper()
	list, err := s.ListResourcePolicies(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.IsDefault {
			return p
		}
	}
	t.Fatal("no Default policy")
	return ResourcePolicy{}
}
