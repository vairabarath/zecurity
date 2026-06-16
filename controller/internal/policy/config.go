package policy

import "fmt"

// ValidateRelayConfig enforces the "both or neither" contract on the operator's
// relay discovery configuration:
//
//   - both empty   -> relay discovery disabled (valid)
//   - both set     -> relay discovery enabled (valid)
//   - exactly one  -> misconfiguration; the controller refuses to start
//
// Returns nil for the two valid states and a descriptive error for the third.
// Called by main.go at startup; safe to call from tests with arbitrary inputs.
func ValidateRelayConfig(relayAddr, relaySPIFFEID string) error {
	if (relayAddr == "") != (relaySPIFFEID == "") {
		return fmt.Errorf(
			"relay configuration is incomplete: RELAY_ADDR=%q RELAY_SPIFFE_ID=%q; both must be set together or both left empty",
			relayAddr, relaySPIFFEID,
		)
	}
	return nil
}
