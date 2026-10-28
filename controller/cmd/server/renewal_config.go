package main

import (
	"fmt"
	"time"
)

// connectorRenewalWarning explains a CONNECTOR_CERT_TTL that isn't longer than
// CONNECTOR_RENEWAL_WINDOW, or returns "". A certificate is issued with the full
// TTL left, so with such settings every new certificate is already inside the
// window: the controller asks again on the connector's new Control stream right
// after each renewal (the connector ignores it as a duplicate) and the connector
// renews every ReEnroll resend interval instead of near expiry. Seen while
// running Sprint 20 G4 / AT-G.1 live.
func connectorRenewalWarning(certTTL, renewalWindow time.Duration) string {
	if certTTL <= 0 || renewalWindow <= 0 || certTTL > renewalWindow {
		return ""
	}
	return fmt.Sprintf("WARNING: CONNECTOR_CERT_TTL (%s) is not longer than CONNECTOR_RENEWAL_WINDOW (%s): "+
		"every new connector certificate starts inside the renewal window, so connectors renew on every "+
		"ReEnroll resend instead of near expiry. Set the window below the TTL (for example 48h of 168h).",
		certTTL, renewalWindow)
}
