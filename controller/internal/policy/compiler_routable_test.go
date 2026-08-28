package policy

import "testing"

// routeTypeForResource had ZERO coverage, and an error from it used to abort the
// whole ACL compile — so these cases pin both which statuses are routable and the
// blast radius of the ones that are not.

func TestRouteTypeForResource(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		shieldID string
		want     string
		wantErr  bool
	}{
		{"pending is connector-delivered", "pending", "", "connector", false},
		{"unprotected is connector-delivered", "unprotected", "", "connector", false},
		// A shield_id on a connector-delivered resource is not an error: the
		// resource is simply not being delivered by it yet.
		{"unprotected with a shield is still connector", "unprotected", "sh-1", "connector", false},
		{"protecting with a shield", "protecting", "sh-1", "shield", false},
		{"protected with a shield", "protected", "sh-1", "shield", false},
		{"failed with a shield is still shield-delivered", "failed", "sh-1", "shield", false},

		// The two error paths. Each used to take an entire workspace offline.
		{"protected without a shield", "protected", "", "", true},
		{"protecting without a shield", "protecting", "", "", true},
		{"failed without a shield", "failed", "", "", true},
		{"unknown status", "quiesced", "sh-1", "", true},
		{"empty status", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := routeTypeForResource(tc.status, tc.shieldID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status=%q shield=%q: want an error, got %q", tc.status, tc.shieldID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("status=%q shield=%q: unexpected error: %v", tc.status, tc.shieldID, err)
			}
			if got != tc.want {
				t.Fatalf("status=%q shield=%q: got %q, want %q", tc.status, tc.shieldID, got, tc.want)
			}
		})
	}
}

func row(resourceID, name, status, shieldID, groupID string) *CompilerResourceRow {
	return &CompilerResourceRow{
		ResourceID: resourceID,
		Name:       name,
		Status:     status,
		ShieldID:   shieldID,
		GroupID:    groupID,
		Address:    "10.0.0.1",
		Port:       443,
		Protocol:   "tcp",
	}
}

func TestPartitionRoutable(t *testing.T) {
	t.Run("all routable: nothing skipped, order preserved", func(t *testing.T) {
		in := []*CompilerResourceRow{
			row("r1", "a", "unprotected", "", "g1"),
			row("r2", "b", "protected", "sh-1", "g1"),
		}
		kept, skipped := partitionRoutable(in)
		if len(skipped) != 0 {
			t.Fatalf("want nothing skipped, got %v", skipped)
		}
		if len(kept) != 2 || kept[0].ResourceID != "r1" || kept[1].ResourceID != "r2" {
			t.Fatalf("kept should be unchanged and in order, got %+v", kept)
		}
	})

	// THE REGRESSION. This exact shape took every resource in the workspace offline:
	// revoking a connector cascade-deleted its shield and left the resource at
	// status='protected' with no shield_id.
	t.Run("one unroutable resource does not remove the others", func(t *testing.T) {
		in := []*CompilerResourceRow{
			row("good-1", "fqdn-test", "unprotected", "", "g1"),
			row("orphan", "prot-test", "protected", "", "g1"),
			row("good-2", "ip-control", "unprotected", "", "g1"),
		}
		kept, skipped := partitionRoutable(in)

		if len(kept) != 2 {
			t.Fatalf("the two healthy resources must survive, got %d: %+v", len(kept), kept)
		}
		for _, k := range kept {
			if k.ResourceID == "orphan" {
				t.Fatal("the unroutable resource must NOT be kept")
			}
		}
		if len(skipped) != 1 {
			t.Fatalf("want exactly one skipped resource, got %v", skipped)
		}
		reason, ok := skipped["orphan"]
		if !ok {
			t.Fatalf("want 'orphan' skipped, got %v", skipped)
		}
		if reason == "" {
			t.Fatal("a skip must carry a reason an operator can act on")
		}
	})

	// A resource appears once per group. Keeping some of its rules would leave
	// keyGroups holding a key with no matching entry.
	t.Run("every rule for a skipped resource is dropped", func(t *testing.T) {
		in := []*CompilerResourceRow{
			row("orphan", "prot-test", "protected", "", "g1"),
			row("orphan", "prot-test", "protected", "", "g2"),
			row("orphan", "prot-test", "protected", "", "g3"),
			row("good", "fqdn-test", "unprotected", "", "g1"),
		}
		kept, skipped := partitionRoutable(in)
		if len(kept) != 1 || kept[0].ResourceID != "good" {
			t.Fatalf("only the good resource should remain, got %+v", kept)
		}
		if len(skipped) != 1 {
			t.Fatalf("the reason should be recorded ONCE per resource, got %v", skipped)
		}
	})

	t.Run("all unroutable yields an empty snapshot, not an error", func(t *testing.T) {
		in := []*CompilerResourceRow{
			row("o1", "a", "protected", "", "g1"),
			row("o2", "b", "quiesced", "", "g1"),
		}
		kept, skipped := partitionRoutable(in)
		if len(kept) != 0 {
			t.Fatalf("want nothing kept, got %+v", kept)
		}
		if len(skipped) != 2 {
			t.Fatalf("want both recorded, got %v", skipped)
		}
	})

	t.Run("empty input is safe", func(t *testing.T) {
		kept, skipped := partitionRoutable(nil)
		if len(kept) != 0 || len(skipped) != 0 {
			t.Fatalf("got kept=%v skipped=%v", kept, skipped)
		}
	})

	// Distinct reasons must not be collapsed — they send an operator to different
	// places (a missing shield vs a status the controller does not know).
	t.Run("reasons are distinguishable", func(t *testing.T) {
		_, skipped := partitionRoutable([]*CompilerResourceRow{
			row("no-shield", "a", "protected", "", "g1"),
			row("bad-status", "b", "quiesced", "sh-1", "g1"),
		})
		if skipped["no-shield"] == skipped["bad-status"] {
			t.Fatalf("distinct causes share a reason: %q", skipped["no-shield"])
		}
	})
}
