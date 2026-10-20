package shield

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// publicKeyFingerprint is the shield key pin (certificate audit bug 10):
// SHA-256 over the parsed key re-encoded as PKIX (SubjectPublicKeyInfo) DER,
// lowercase hex. The same canonical form as the client
// (client_devices.public_key_fingerprint) and relay pins, so equal keys
// always give equal fingerprints whatever encoding the caller sent.
func publicKeyFingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

var keyFingerprintFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validKeyFingerprint reports whether a stored pin has the expected form.
func validKeyFingerprint(fp string) bool {
	return keyFingerprintFormat.MatchString(fp)
}

// renewalKeyFingerprint parses a renewal CSR, checks its self-signature
// (proof of key possession) and returns its key's fingerprint.
func renewalKeyFingerprint(csrDER []byte) (string, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "parse renewal CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "renewal CSR signature invalid: %v", err)
	}
	fp, err := publicKeyFingerprint(csr.PublicKey)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "renewal CSR public key: %v", err)
	}
	return fp, nil
}
