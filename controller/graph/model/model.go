package model

// AuthInitPayload is returned by the initiateAuth mutation.
// Defined here (not in generated code) to break the import cycle
// between graph (which imports internal/auth) and internal/auth
// (which needs AuthInitPayload for the Service interface).
type AuthInitPayload struct {
	RedirectURL string `json:"redirectUrl"`
	State       string `json:"state"`
}
