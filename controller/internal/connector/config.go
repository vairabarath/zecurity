package connector

import (
	"errors"
	"fmt"
	"time"
)

// Config holds all tunable settings for the connector subsystem.
// Populated in main.go from environment variables in a later phase.
type Config struct {
	CertTTL             time.Duration
	EnrollmentTokenTTL  time.Duration
	HeartbeatInterval   time.Duration
	DisconnectThreshold time.Duration
	GRPCPort            string
	JWTSecret           string
	RenewalWindow       time.Duration
}

// ErrRenewalWindowTooWide reports a renewal window that is not shorter than the
// certificate lifetime.
var ErrRenewalWindowTooWide = errors.New("renewal window must be shorter than the certificate TTL")

// Validate rejects a renewal window that is not strictly shorter than CertTTL.
//
// WHY THIS IS FATAL RATHER THAN COSMETIC. A freshly issued certificate always has
// very nearly CertTTL remaining. maybeRequestRenewal prompts whenever the
// PRESENTED certificate is inside RenewalWindow, and a connector reconnects with
// its new certificate immediately after renewing. So if RenewalWindow >= CertTTL,
// the brand-new certificate is *still* inside the window and the connector is
// prompted again the moment it comes back — for ever.
//
// This is not hypothetical. Observed 2026-09-01 with CERT_TTL=168h and
// RENEWAL_WINDOW=200h: 244 certificate signings in one minute, roughly one per
// second, and the connector was driven to 'disconnected' by the constant
// reconnects. Each iteration is a CSR plus a CA signing operation.
//
// Refusing to start is deliberate. The alternative — clamping with a warning —
// leaves a controller running on a configuration whose author expected something
// else, and the failure it prevents is a self-sustaining load on the CA. A boot
// failure is loud, immediate, and fixed in one edit; the storm is neither.
func (c Config) Validate() error {
	if c.CertTTL <= 0 {
		return fmt.Errorf("%w: CONNECTOR_CERT_TTL is %v", ErrRenewalWindowTooWide, c.CertTTL)
	}
	if c.RenewalWindow >= c.CertTTL {
		return fmt.Errorf("%w: CONNECTOR_RENEWAL_WINDOW=%v, CONNECTOR_CERT_TTL=%v — a renewed "+
			"certificate would still be inside the window, so renewal would repeat without end",
			ErrRenewalWindowTooWide, c.RenewalWindow, c.CertTTL)
	}
	return nil
}
