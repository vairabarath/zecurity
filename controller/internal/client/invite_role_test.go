package client

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sprint 23 S2 (D-44): the client CLI sign-in creates members, so it must
// refuse a provider first-admin invitation instead of consuming it as a
// member join (the admin seat would be lost). Member invitations work as
// before.
func TestCheckClientInvitation_RefusesFirstAdminInvitation(t *testing.T) {
	now := time.Now()
	member := &invitation{WorkspaceID: "w1", Status: "pending", Role: "member", ExpiresAt: now.Add(time.Hour)}
	if err := checkClientInvitation(member, "w1", now); err != nil {
		t.Fatalf("pending member invitation: %v", err)
	}
	admin := *member
	admin.Role = "admin"
	err := checkClientInvitation(&admin, "w1", now)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("first-admin invitation through the client: err = %v, want FailedPrecondition", err)
	}
	other := *member
	if err := checkClientInvitation(&other, "w2", now); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other workspace: %v", err)
	}
	expired := *member
	expired.ExpiresAt = now.Add(-time.Second)
	if err := checkClientInvitation(&expired, "w1", now); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expired: %v", err)
	}
}
