package connector

import (
	"context"
	"net/http"
	"testing"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/shield"
)

// stubShieldSvc implements shield.Service; only UpdateShieldHealth is exercised.
type stubShieldSvc struct {
	connectorChanged bool
	lanIPChanged     bool
}

var _ shield.Service = (*stubShieldSvc)(nil)

func (f *stubShieldSvc) UpdateShieldHealth(_ context.Context, _, _, _, _, _ string, _ int64) (bool, bool, error) {
	return f.connectorChanged, f.lanIPChanged, nil
}

func (f *stubShieldSvc) GenerateShieldToken(_ context.Context, _, _, _, _, _ string) (string, string, error) {
	return "", "", nil
}
func (f *stubShieldSvc) RunDisconnectWatcher(_ context.Context) {}
func (f *stubShieldSvc) TokenHandler() http.Handler             { return nil }

// recordingNotifier implements PolicyChangeNotifier and records calls.
type recordingNotifier struct {
	calls    int
	lastWsID string
}

func (n *recordingNotifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	n.calls++
	n.lastWsID = workspaceID
	return nil
}

// TestHandleShieldStatusNotifiesOnChange verifies handleShieldStatus fires
// NotifyPolicyChange when a shield's connector OR its LAN IP changed (the
// LAN-IP case is the shield-sync wiring), and stays quiet when nothing changed.
func TestHandleShieldStatusNotifiesOnChange(t *testing.T) {
	cases := []struct {
		name             string
		connectorChanged bool
		lanIPChanged     bool
		wantCalls        int
	}{
		{"lan ip changed fires notify", false, true, 1},
		{"connector changed fires notify", true, false, 1},
		{"both changed fires once", true, true, 1},
		{"nothing changed stays quiet", false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier := &recordingNotifier{}
			h := &EnrollmentHandler{
				ShieldSvc:      &stubShieldSvc{connectorChanged: tc.connectorChanged, lanIPChanged: tc.lanIPChanged},
				PolicyNotifier: notifier,
			}
			client := &connectorStreamClient{connectorID: "conn-1", tenantID: "ws-1"}

			h.handleShieldStatus(context.Background(), client, &pb.ShieldStatusBatch{
				Shields: []*pb.ShieldStatusUpdate{
					{ShieldId: "sh-1", Status: "active", Version: "v1", LanIp: "10.0.0.9"},
				},
			})

			if notifier.calls != tc.wantCalls {
				t.Fatalf("NotifyPolicyChange calls = %d, want %d", notifier.calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && notifier.lastWsID != "ws-1" {
				t.Fatalf("NotifyPolicyChange workspaceID = %q, want ws-1", notifier.lastWsID)
			}
		})
	}
}
