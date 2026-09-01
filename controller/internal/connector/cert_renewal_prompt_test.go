package connector

import (
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// Connector certs have a 7-day TTL and the controller never once asked a connector
// to renew: ReEnrollSignal was constructed nowhere outside generated protobuf, and
// Cfg.RenewalWindow — parsed from CONNECTOR_RENEWAL_WINDOW in main.go — was read by
// nothing. Every connector expired after 7 days into a state only manual
// re-enrolment could clear, because RenewCert needs the channel expiry removes.
//
// A threshold-only test would not have caught that: the arithmetic was never wrong,
// the send never happened. So these assert on the OUTBOUND MAILBOX — the message a
// connector would actually receive.
//
// maybeRequestRenewal touches only the mailbox and Cfg, so unlike the rest of the
// control stream these need no database.

const testWindow = 48 * time.Hour

func promptFixture(notAfter time.Time, window time.Duration) (*EnrollmentHandler, *connectorStreamClient) {
	h := &EnrollmentHandler{Cfg: Config{RenewalWindow: window}}
	c := &connectorStreamClient{
		outbound:     make(chan *pb.ConnectorControlMessage, connectorSendQueueSize),
		connectorID:  "c-test",
		certNotAfter: notAfter,
	}
	return h, c
}

// drainReEnroll reports how many messages are queued and whether the first one is a
// ReEnroll. Checking the body type matters: "something was sent" would pass even if
// the wrong message went out.
func drainReEnroll(t *testing.T, c *connectorStreamClient) (count int, firstIsReEnroll bool) {
	t.Helper()
	for {
		select {
		case msg := <-c.outbound:
			if count == 0 {
				_, firstIsReEnroll = msg.Body.(*pb.ConnectorControlMessage_ReEnroll)
			}
			count++
		default:
			return count, firstIsReEnroll
		}
	}
}

func TestMaybeRequestRenewal(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		notAfter time.Time
		window   time.Duration
		wantSend bool
		why      string
	}{
		{
			name:     "inside the window prompts",
			notAfter: now.Add(24 * time.Hour),
			window:   testWindow,
			wantSend: true,
			why:      "24h left against a 48h window is the whole point of the feature",
		},
		{
			name:     "outside the window stays quiet",
			notAfter: now.Add(5 * 24 * time.Hour),
			window:   testWindow,
			wantSend: false,
			why:      "a fresh 7-day cert must not be renewed on every heartbeat",
		},
		{
			name:     "exactly at the boundary prompts",
			notAfter: now.Add(testWindow),
			window:   testWindow,
			wantSend: true,
			why:      "the comparison is > window, so the boundary is inside it",
		},
		{
			name:     "an already-expired cert still prompts",
			notAfter: now.Add(-1 * time.Hour),
			window:   testWindow,
			wantSend: true,
			why: "if the stream somehow came up on an expired cert, prompting is the " +
				"only chance to recover without manual re-enrolment",
		},
		{
			// The trap this guard exists for: a zero time is inside EVERY window, so
			// treating "expiry unknown" as "expiring now" would prompt on every single
			// health report forever.
			name:     "unknown expiry never prompts",
			notAfter: time.Time{},
			window:   testWindow,
			wantSend: false,
			why:      "zero is inside every window; prompting on it would be an infinite loop",
		},
		{
			name:     "a zero window disables prompting",
			notAfter: now.Add(1 * time.Hour),
			window:   0,
			wantSend: false,
			why:      "an unset CONNECTOR_RENEWAL_WINDOW must not mean 'renew always'",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c := promptFixture(tc.notAfter, tc.window)
			got := h.maybeRequestRenewal(c, now)
			if got != tc.wantSend {
				t.Fatalf("maybeRequestRenewal = %v, want %v: %s", got, tc.wantSend, tc.why)
			}
			count, isReEnroll := drainReEnroll(t, c)
			if tc.wantSend {
				if count != 1 {
					t.Fatalf("queued %d messages, want exactly 1: %s", count, tc.why)
				}
				if !isReEnroll {
					t.Fatal("a message was queued but it was not a ReEnroll signal")
				}
			} else if count != 0 {
				t.Fatalf("queued %d messages, want none: %s", count, tc.why)
			}
		})
	}
}

// Prompting repeats until the connector reconnects with a fresh cert — that is what
// makes a dropped or failed renewal self-healing, with no retry path of its own.
func TestRenewalPromptRepeatsUntilTheCertChanges(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	h, c := promptFixture(now.Add(6*time.Hour), testWindow)

	for i := 0; i < 3; i++ {
		if !h.maybeRequestRenewal(c, now) {
			t.Fatalf("health report %d did not prompt; renewal would never be retried", i+1)
		}
	}
	if count, _ := drainReEnroll(t, c); count != 3 {
		t.Fatalf("queued %d prompts across 3 health reports, want 3", count)
	}
}

// A wedged connector fills its mailbox. Prompting must report failure rather than
// block the control stream's receive loop or panic.
func TestRenewalPromptFailsSoftOnAFullMailbox(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	h, c := promptFixture(now.Add(1*time.Hour), testWindow)
	for i := 0; i < connectorSendQueueSize; i++ {
		c.outbound <- &pb.ConnectorControlMessage{}
	}
	if h.maybeRequestRenewal(c, now) {
		t.Fatal("reported a successful send into a full mailbox")
	}
}

// The storm this suite originally missed.
//
// TestRenewalPromptRepeatsUntilTheCertChanges asserts that prompting REPEATS —
// correct when renewal keeps failing. What no test asked was what happens when
// renewal SUCCEEDS: the connector reconnects presenting a certificate with a full
// TTL remaining, and if that is still inside the window it is prompted again at
// once. Observed live 2026-09-01 with CERT_TTL=168h / RENEWAL_WINDOW=200h — 244
// signings in a minute, one per second, until the connector fell over.
//
// Two assertions, because the guard has two halves: the prompt must be quiet for a
// fresh cert under a sane window, and the configuration that breaks that must be
// refused at startup rather than discovered in production.
func TestAFreshlyRenewedCertIsNotPromptedAgain(t *testing.T) {
	const certTTL = 168 * time.Hour
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Exactly what a connector presents one second after renewing.
	h, c := promptFixture(now.Add(certTTL), testWindow)
	if h.maybeRequestRenewal(c, now) {
		t.Fatal("prompted a certificate that was just issued — the connector would renew, " +
			"reconnect, be prompted again, and loop for ever")
	}
	if count, _ := drainReEnroll(t, c); count != 0 {
		t.Fatalf("queued %d prompts for a fresh certificate, want none", count)
	}
}

func TestConfigValidateRejectsAWindowThatCannotTerminate(t *testing.T) {
	cases := []struct {
		name    string
		ttl     time.Duration
		window  time.Duration
		wantErr bool
		why     string
	}{
		{
			name: "the shipped defaults are valid", ttl: 168 * time.Hour, window: 48 * time.Hour,
			wantErr: false, why: "48h < 168h, so a renewed cert leaves the window",
		},
		{
			name: "window wider than the TTL is refused", ttl: 168 * time.Hour, window: 200 * time.Hour,
			wantErr: true, why: "this is the exact live configuration that caused 244 signings a minute",
		},
		{
			name: "window equal to the TTL is refused", ttl: 168 * time.Hour, window: 168 * time.Hour,
			wantErr: true, why: "a fresh cert sits exactly ON the boundary, which prompts",
		},
		{
			name: "a zero TTL is refused", ttl: 0, window: 48 * time.Hour,
			wantErr: true, why: "no positive TTL means every cert is always inside any window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Config{CertTTL: tc.ttl, RenewalWindow: tc.window}.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("accepted TTL=%v window=%v: %s", tc.ttl, tc.window, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejected a valid config (TTL=%v window=%v): %v — %s", tc.ttl, tc.window, err, tc.why)
			}
		})
	}
}
