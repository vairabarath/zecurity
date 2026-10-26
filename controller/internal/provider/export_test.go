package provider

import "time"

// Test-only exports for the external provider_test package (end-to-end tests
// that wire the real middleware, which imports this package).
var NewTestStore = newTestStore

// SetTOTPClock lets end-to-end tests drive the TOTP time step.
func SetTOTPClock(h *TOTPHandlers, now func() time.Time) { h.now = now }
