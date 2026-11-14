package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/geoip"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/risk"
)

func TestStartReauth_SuccessPath(t *testing.T) {
	f := newStepUpFixture(t)
	f.svc.idpStore = idp.NewStore(f.pool, f.pki)
	f.svc.controllerHTTPURL = "http://localhost:8080"

	// 1. Fake OIDC discovery server
	fakeOIDC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 "http://" + r.Host,
				"authorization_endpoint": "http://" + r.Host + "/authorize",
				"token_endpoint":         "http://" + r.Host + "/token",
				"jwks_uri":               "http://" + r.Host + "/jwks",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeOIDC.Close()

	// 2. Give device a session_id
	const sid = "session-abc-123"
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE client_devices SET session_id = $1 WHERE id = $2`, sid, f.deviceID); err != nil {
		t.Fatal(err)
	}

	// 3. Set token claims matching that session
	claims := &auth.AccessTokenClaims{
		UserID:    f.userID,
		TenantID:  f.wsID,
		Email:     "history@example.test",
		SessionID: sid,
	}
	f.svc.authSvc = &postureTestAuth{claims: claims}

	// 4. Seed an active IdP connection pointing to fakeOIDC
	var connID string
	err := f.pool.QueryRow(f.ctx,
		`INSERT INTO identity_connections (tenant_id, protocol, provider, display_name, issuer, client_id, status)
		 VALUES ($1, 'oidc', 'okta', 'Okta Corp', $2, 'client-123', 'active')
		 RETURNING id`, f.wsID, fakeOIDC.URL,
	).Scan(&connID)
	if err != nil {
		t.Fatalf("insert idp connection: %v", err)
	}

	// 5. StartReauth succeeds
	resp, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{
		AccessToken: "valid-tok",
		DeviceId:    f.deviceID,
	})
	if err != nil {
		t.Fatalf("StartReauth: %v", err)
	}
	if resp.GetAuthUrl() == "" {
		t.Fatal("empty AuthUrl")
	}

	u, err := url.Parse(resp.GetAuthUrl())
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("state parameter missing in AuthUrl")
	}

	// 6. Verify the authSession in controller memory
	sess, ok := getSession(state)
	if !ok {
		t.Fatalf("session %q not found in memory", state)
	}
	if sess.WorkspaceID != f.wsID {
		t.Errorf("WorkspaceID = %s, want %s", sess.WorkspaceID, f.wsID)
	}
	if sess.Reauth == nil || sess.Reauth.DeviceID != f.deviceID || sess.Reauth.UserID != f.userID {
		t.Fatalf("reauth info mismatch: %+v", sess.Reauth)
	}
}

func TestGetACLSnapshot_AuthRequiredCount(t *testing.T) {
	f := newStepUpFixture(t)
	f.svc.policyStore = policy.NewStore(f.pool)
	f.svc.postureStore = posture.NewStore(f.pool)
	f.svc.policyCache = policy.NewSnapshotCache()
	f.svc.policyNotifier = policy.NewNotifier(f.svc.policyCache)

	claims := &auth.AccessTokenClaims{
		UserID:    f.userID,
		TenantID:  f.wsID,
		Email:     "history@example.test",
		SessionID: "sid-1",
	}
	f.svc.authSvc = &postureTestAuth{claims: claims}

	wsUUID := uuid.MustParse(f.wsID)
	userUUID := uuid.MustParse(f.userID)
	devUUID := uuid.MustParse(f.deviceID)

	// Create policy with auth frequency
	pol, err := f.svc.postureStore.CreateResourcePolicy(f.ctx, wsUUID, "Auth Req Policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	mins := 60
	if _, err := f.svc.postureStore.SetResourcePolicyAuthentication(f.ctx, wsUUID, pol.ID, &mins, false); err != nil {
		t.Fatal(err)
	}

	// Create remote network, resource, group, rule, member
	rnID := uuid.New()
	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO remote_networks (id, tenant_id, name, location, status)
		 VALUES ($1, $2, 'net1', 'office', 'active')`, rnID, wsUUID)
	if err != nil {
		t.Fatal(err)
	}

	resID := uuid.New()
	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO resources (id, tenant_id, remote_network_id, policy_id, name, host, protocol, port_from, port_to, status)
		 VALUES ($1, $2, $3, $4, 'res1', '10.0.0.10', 'tcp', 80, 80, 'protected')`,
		resID, wsUUID, rnID, pol.ID)
	if err != nil {
		t.Fatal(err)
	}

	grpID := uuid.New()
	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO groups (id, workspace_id, name) VALUES ($1, $2, 'grp1')`, grpID, wsUUID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO group_members (group_id, user_id, workspace_id) VALUES ($1, $2, $3)`,
		grpID, userUUID, wsUUID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO access_rules (workspace_id, resource_id, group_id, enabled) VALUES ($1, $2, $3, true)`,
		wsUUID, resID, grpID)
	if err != nil {
		t.Fatal(err)
	}

	// 1. With expired session_auth_at -> authRequiredCount = 1
	_, err = f.pool.Exec(f.ctx,
		`UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, devUUID)
	if err != nil {
		t.Fatal(err)
	}

	snapResp, err := f.svc.GetACLSnapshot(f.ctx, &clientv1.GetACLSnapshotRequest{
		AccessToken: "tok",
		DeviceId:    f.deviceID,
	})
	if err != nil {
		t.Fatalf("GetACLSnapshot: %v", err)
	}
	if snapResp.GetAuthRequiredCount() != 1 {
		t.Errorf("AuthRequiredCount = %d, want 1", snapResp.GetAuthRequiredCount())
	}

	// 2. With fresh session_auth_at -> authRequiredCount = 0
	_, err = f.pool.Exec(f.ctx,
		`UPDATE client_devices SET session_auth_at = NOW() WHERE id = $1`, devUUID)
	if err != nil {
		t.Fatal(err)
	}

	snapResp, err = f.svc.GetACLSnapshot(f.ctx, &clientv1.GetACLSnapshotRequest{
		AccessToken: "tok",
		DeviceId:    f.deviceID,
	})
	if err != nil {
		t.Fatalf("GetACLSnapshot: %v", err)
	}
	if snapResp.GetAuthRequiredCount() != 0 {
		t.Errorf("AuthRequiredCount = %d, want 0 after auth", snapResp.GetAuthRequiredCount())
	}
}

func TestCountryMove_RiskHook_EndToEnd(t *testing.T) {
	f := newStepUpFixture(t)
	f.svc.postureStore = posture.NewStore(f.pool)
	riskSvc := risk.NewService(f.pool, nil)
	f.svc.riskSvc = riskSvc

	wsUUID := uuid.MustParse(f.wsID)
	devUUID := uuid.MustParse(f.deviceID)

	now := time.Now()

	// 1. First sighting in US -> not a move
	_, moved, err := f.svc.postureStore.RecordNetworkObservationMove(f.ctx, wsUUID, devUUID, geoip.Lookup{Country: "US"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("first sighting must not be treated as a move")
	}

	// 2. Second sighting in DE -> move!
	_, moved, err = f.svc.postureStore.RecordNetworkObservationMove(f.ctx, wsUUID, devUUID, geoip.Lookup{Country: "DE"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("expected move from US to DE to be true")
	}

	// Trigger the risk hook as the controller does when moved == true
	if moved {
		demanded, rerr := f.svc.riskSvc.Add(f.ctx, f.wsID, f.deviceID, risk.CountryChanged)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !demanded {
			t.Errorf("expected CountryChanged (+50) to demand reauth")
		}
	}

	// Verify device risk score and demand
	d := f.device()
	if d.score != 50 {
		t.Errorf("risk score = %d, want 50", d.score)
	}
	if d.reauthAt == nil {
		t.Errorf("reauth_required_at not set")
	}
	found := false
	for _, r := range d.reasons {
		if r == risk.CountryChanged.Reason {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("risk_reasons %+v missing %s", d.reasons, risk.CountryChanged.Reason)
	}
}

func TestPostureRegression_RiskHook_EndToEnd(t *testing.T) {
	f := newStepUpFixture(t)
	riskSvc := risk.NewService(f.pool, nil)

	wsUUID := uuid.MustParse(f.wsID)
	devUUID := uuid.MustParse(f.deviceID)

	// Wire up regression hook like cmd/server/main.go
	hookFired := false
	hook := func(ctx context.Context, w, d uuid.UUID) {
		hookFired = true
		_, _ = riskSvc.Add(ctx, w.String(), d.String(), risk.PostureRegressed)
	}

	// Fire regression hook
	hook(f.ctx, wsUUID, devUUID)

	if !hookFired {
		t.Fatal("hook did not fire")
	}

	d := f.device()
	if d.score != 30 {
		t.Errorf("risk score = %d, want 30", d.score)
	}
	found := false
	for _, r := range d.reasons {
		if r == risk.PostureRegressed.Reason {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("risk_reasons %+v missing %s", d.reasons, risk.PostureRegressed.Reason)
	}
}
