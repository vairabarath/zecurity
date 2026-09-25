package connector

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// Sprint 20 Phase G-1 — controller-side connector renewal trigger.

const testRenewalWindow = 48 * time.Hour

// drainReEnrolls empties the client's mailbox and counts ReEnroll messages.
func drainReEnrolls(client *connectorStreamClient) int {
	n := 0
	for {
		select {
		case msg := <-client.outbound:
			if _, ok := msg.Body.(*pb.ConnectorControlMessage_ReEnroll); ok {
				n++
			}
		default:
			return n
		}
	}
}

func renewalHandler() *EnrollmentHandler {
	return &EnrollmentHandler{Cfg: Config{RenewalWindow: testRenewalWindow}}
}

func TestRenewalDue(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	cases := []struct {
		name     string
		notAfter *time.Time
		window   time.Duration
		want     bool
	}{
		{"inside window", at(1 * time.Hour), testRenewalWindow, true},
		{"just inside", at(testRenewalWindow - time.Second), testRenewalWindow, true},
		{"at window edge", at(testRenewalWindow), testRenewalWindow, false},
		{"outside window", at(7 * 24 * time.Hour), testRenewalWindow, false},
		{"NULL cert_not_after", nil, testRenewalWindow, false},
		{"window disabled", at(1 * time.Hour), 0, false},
	}
	for _, tc := range cases {
		if got := renewalDue(tc.notAfter, now, tc.window); got != tc.want {
			t.Errorf("%s: renewalDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Inside the window → exactly one ReEnroll.
func TestMaybeSendReEnroll_InsideWindowSendsExactlyOne(t *testing.T) {
	h := renewalHandler()
	client := testClient("c1", "ws")
	now := time.Now()
	notAfter := now.Add(time.Hour)

	if !h.maybeSendReEnroll(client, &notAfter, now) {
		t.Fatal("expected ReEnroll inside the renewal window")
	}
	if n := drainReEnrolls(client); n != 1 {
		t.Fatalf("ReEnroll messages = %d, want 1", n)
	}
}

// The per-stream throttle suppresses duplicates until the interval elapses.
func TestMaybeSendReEnroll_ThrottleSuppressesDuplicates(t *testing.T) {
	h := renewalHandler()
	client := testClient("c1", "ws")
	now := time.Now()
	notAfter := now.Add(time.Hour)

	h.maybeSendReEnroll(client, &notAfter, now)
	for _, later := range []time.Duration{15 * time.Second, time.Minute, reEnrollResendInterval - time.Second} {
		if h.maybeSendReEnroll(client, &notAfter, now.Add(later)) {
			t.Fatalf("duplicate ReEnroll %s after the first", later)
		}
	}
	if n := drainReEnrolls(client); n != 1 {
		t.Fatalf("ReEnroll messages within the throttle window = %d, want 1", n)
	}

	if !h.maybeSendReEnroll(client, &notAfter, now.Add(reEnrollResendInterval)) {
		t.Fatal("expected a re-ask once the throttle interval elapsed")
	}
	if n := drainReEnrolls(client); n != 1 {
		t.Fatalf("ReEnroll messages after the interval = %d, want 1", n)
	}
}

// The throttle is per stream: a reconnect (new stream client) is asked again.
func TestMaybeSendReEnroll_ThrottleIsPerStream(t *testing.T) {
	h := renewalHandler()
	now := time.Now()
	notAfter := now.Add(time.Hour)

	first := testClient("c1", "ws")
	h.maybeSendReEnroll(first, &notAfter, now)
	second := testClient("c1", "ws")
	if !h.maybeSendReEnroll(second, &notAfter, now.Add(time.Second)) {
		t.Fatal("a new stream for the same connector must get its own ReEnroll")
	}
}

// Outside the window, NULL cert_not_after, or a disabled window → nothing.
func TestMaybeSendReEnroll_OutsideWindowOrNullSendsNothing(t *testing.T) {
	now := time.Now()
	far := now.Add(7 * 24 * time.Hour)

	h := renewalHandler()
	client := testClient("c1", "ws")
	if h.maybeSendReEnroll(client, &far, now) || h.maybeSendReEnroll(client, nil, now) {
		t.Fatal("ReEnroll sent outside the window or for NULL cert_not_after")
	}
	disabled := &EnrollmentHandler{Cfg: Config{RenewalWindow: 0}}
	soon := now.Add(time.Hour)
	if disabled.maybeSendReEnroll(client, &soon, now) {
		t.Fatal("ReEnroll sent with the renewal window disabled")
	}
	if n := drainReEnrolls(client); n != 0 {
		t.Fatalf("ReEnroll messages = %d, want 0", n)
	}
}

// A full mailbox does not record the throttle, so the next health report retries.
func TestMaybeSendReEnroll_FullMailboxRetriesOnNextReport(t *testing.T) {
	h := renewalHandler()
	client := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 1), connectorID: "c1", tenantID: "ws"}
	client.outbound <- &pb.ConnectorControlMessage{} // mailbox full
	now := time.Now()
	notAfter := now.Add(time.Hour)

	if h.maybeSendReEnroll(client, &notAfter, now) {
		t.Fatal("enqueue into a full mailbox reported success")
	}
	<-client.outbound // connector drained its mailbox
	if !h.maybeSendReEnroll(client, &notAfter, now.Add(15*time.Second)) {
		t.Fatal("next health report must retry after a failed enqueue")
	}
}

// --- through handleConnectorHealth (DB-backed) --------------------------------

func setConnectorCertNotAfter(t *testing.T, h *EnrollmentHandler, connID string, notAfter *time.Time) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `UPDATE connectors SET cert_not_after = $2 WHERE id = $1`, connID, notAfter); err != nil {
		t.Fatalf("set cert_not_after: %v", err)
	}
}

func healthReport() *pb.ConnectorHealthReport {
	return &pb.ConnectorHealthReport{Version: "test", Hostname: "h"}
}

// Inside the window: the first health report triggers exactly one ReEnroll; the
// next report on the same stream is throttled.
func TestHandleConnectorHealth_SendsReEnrollInsideWindow(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, fmt.Sprintf("g1-in-%d", time.Now().UnixNano()))
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)
	h := &EnrollmentHandler{Pool: pool, Cfg: Config{RenewalWindow: testRenewalWindow}}
	soon := time.Now().Add(time.Hour)
	setConnectorCertNotAfter(t, h, connID, &soon)
	client := testClient(connID, wsID)

	if !h.handleConnectorHealth(ctx, client, healthReport()) {
		t.Fatal("active connector's health report closed the stream")
	}
	if n := drainReEnrolls(client); n != 1 {
		t.Fatalf("ReEnroll after first report = %d, want 1", n)
	}
	if !h.handleConnectorHealth(ctx, client, healthReport()) {
		t.Fatal("second health report closed the stream")
	}
	if n := drainReEnrolls(client); n != 0 {
		t.Fatalf("ReEnroll after second report (throttled) = %d, want 0", n)
	}
}

// Outside the window, or cert_not_after NULL: health reports send nothing.
func TestHandleConnectorHealth_NoReEnrollOutsideWindowOrNull(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, fmt.Sprintf("g1-out-%d", time.Now().UnixNano()))
	h := &EnrollmentHandler{Pool: pool, Cfg: Config{RenewalWindow: testRenewalWindow}}

	far := time.Now().Add(7 * 24 * time.Hour)
	for name, notAfter := range map[string]*time.Time{"outside window": &far, "NULL cert_not_after": nil} {
		t.Run(name, func(t *testing.T) {
			connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)
			setConnectorCertNotAfter(t, h, connID, notAfter)
			client := testClient(connID, wsID)

			if !h.handleConnectorHealth(ctx, client, healthReport()) {
				t.Fatal("active connector's health report closed the stream")
			}
			if n := drainReEnrolls(client); n != 0 {
				t.Fatalf("ReEnroll messages = %d, want 0", n)
			}
		})
	}
}

// A revoked connector — even one whose cert is inside the window — never gets
// ReEnroll: the revocation-guarded UPDATE closes its stream first (Phase B).
func TestHandleConnectorHealth_RevokedNeverGetsReEnroll(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, fmt.Sprintf("g1-rev-%d", time.Now().UnixNano()))
	h := &EnrollmentHandler{Pool: pool, Cfg: Config{RenewalWindow: testRenewalWindow}}
	soon := time.Now().Add(time.Hour)
	revokedAt := time.Now()

	for name, seed := range map[string]func() string{
		"status revoked": func() string {
			return seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "revoked", &revokedAt)
		},
		"revoked_at set, legacy status": func() string {
			return seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "disconnected", &revokedAt)
		},
	} {
		t.Run(name, func(t *testing.T) {
			connID := seed()
			setConnectorCertNotAfter(t, h, connID, &soon)
			client := testClient(connID, wsID)

			if h.handleConnectorHealth(ctx, client, healthReport()) {
				t.Fatal("revoked connector's health report did not close the stream")
			}
			if n := drainReEnrolls(client); n != 0 {
				t.Fatalf("revoked connector received %d ReEnroll message(s), want 0", n)
			}
		})
	}
}
