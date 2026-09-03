package resolvers

// Shared fixtures for the credential-verification step that createIdpConnection,
// updateIdpConnection and testIdpConnection now run before trusting a
// connection (graph/resolvers/idp_helpers.go: verifyOIDCCredentials).
//
// Two things every such test needs:
//
//  1. a token endpoint that answers the probe. The probe POSTs an invalid
//     authorization code; an IdP that authenticated the client replies
//     `invalid_grant`, which is the positive signal. A fixture serving only a
//     discovery document leaves the probe INCONCLUSIVE, and under the
//     block-unless-proven policy that is a failure.
//  2. a reversible secret encryptor. The store only populates
//     Connection.ClientSecret when encrypted_client_secret AND secret_nonce are
//     both non-NULL, so a connection seeded without them has no credential to
//     verify.

import (
	"encoding/base64"
	"net/http"

	"github.com/yourorg/ztna/controller/internal/pki"
)

// testSecretEnc is a stand-in for pki.Service implementing only the two secret
// methods the idp store uses, reversibly so round-trips work. It embeds
// pki.Service to satisfy the full interface; real crypto is covered in
// internal/pki. Mirrors internal/idp's own base64Enc.
type testSecretEnc struct{ pki.Service }

func (testSecretEnc) EncryptSecret(plaintext []byte, _ string) (string, string, error) {
	return base64.StdEncoding.EncodeToString(plaintext), "test-nonce", nil
}
func (testSecretEnc) DecryptSecret(ciphertextB64, _, _ string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(ciphertextB64)
}

// seededSecretCiphertext / seededSecretNonce are what testSecretEnc produces for
// a connection seeded through raw SQL, so scanConnection can decrypt it.
const (
	seededSecretPlaintext  = "probe-secret"
	seededSecretCiphertext = "cHJvYmUtc2VjcmV0" // base64("probe-secret")
	seededSecretNonce      = "test-nonce"
)

// registerCredentialProbeToken makes a discovery fixture answer the credential
// probe POSITIVELY. `invalid_grant` is what a real IdP returns once the client
// has authenticated and only the (deliberately invalid) code is rejected —
// client authentication is evaluated before the grant, which is what makes the
// probe sound. See providers.VerifyClientCredentials.
func registerCredentialProbeToken(mux *http.ServeMux) {
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code is invalid"}`))
	})
}

// registerRejectingCredentialProbeToken makes a fixture REJECT the probe, as an
// IdP does for a wrong (or swapped) client ID / client secret pair.
func registerRejectingCredentialProbeToken(mux *http.ServeMux) {
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Okta's envelope: HTTP 400 + errorCode, not the RFC's 401 + error.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorCode":"invalid_client","errorSummary":"Invalid value for 'client_id' parameter."}`))
	})
}
