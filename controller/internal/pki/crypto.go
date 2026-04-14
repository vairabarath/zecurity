package pki

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// generateECKeyPair generates an ECDSA P-384 private key.
//
// Why this exists:
// All three CA layers in our PKI hierarchy need asymmetric keypairs:
// Root CA, Intermediate CA, and each Workspace CA.
//
// Why P-384:
// The project plan calls for EC-P384 because it gives a stronger
// security margin than P-256 while still being broadly supported.
//
// What it returns:
// A Go *ecdsa.PrivateKey in memory only. We do not write it anywhere
// yet. Later code is responsible for encrypting it before storage.
func generateECKeyPair() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate EC P-384 key: %w", err)
	}

	return key, nil
}

// encryptPrivateKey encrypts an ECDSA private key for database storage.
//
// Why this exists:
// CA private keys must never be stored in plaintext in Postgres.
// This helper converts the key into bytes, derives a symmetric key,
// and encrypts the private key before anything is persisted.
//
// How it works:
// 1. Marshal the ECDSA private key into DER bytes.
// 2. Derive a 32-byte AES key from the master secret using HKDF-SHA256.
// 3. Use the provided context to make the derived key unique.
// 4. Encrypt the DER bytes using AES-256-GCM.
// 5. Return both ciphertext and nonce as base64 strings.
//
// Why the context matters:
// We do not want one derived key reused for every CA object.
// Examples:
// - Root CA uses "root-ca"
// - Intermediate CA uses "intermediate-ca"
// - Workspace CAs use the tenant ID
//
// This creates key separation. Even if one encrypted object is exposed,
// it does not imply the others share the same encryption key.
func encryptPrivateKey(
	key *ecdsa.PrivateKey,
	masterSecret, context string,
) (string, string, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	defer zeroBytes(keyDER)

	encKey, err := hkdf.Key(sha256.New, []byte(masterSecret), nil, context, 32)
	if err != nil {
		return "", "", fmt.Errorf("derive encryption key: %w", err)
	}
	defer zeroBytes(encKey)

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, keyDER, nil)

	return base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(nonce),
		nil
}

// decryptPrivateKey reverses encryptPrivateKey.
//
// Why this exists:
// At runtime, the PKI service sometimes needs the plaintext private key
// again, for example to sign an Intermediate CA or a Workspace CA.
//
// Important rule:
// The same master secret and the same context string used at encryption
// time must be used again here, otherwise decryption will fail.
//
// Flow:
// 1. Decode the base64 ciphertext and nonce.
// 2. Re-derive the exact same AES key using HKDF-SHA256.
// 3. Decrypt the DER bytes using AES-256-GCM.
// 4. Parse the DER bytes back into an ECDSA private key.
func decryptPrivateKey(
	ciphertextB64, nonceB64, masterSecret, context string,
) (*ecdsa.PrivateKey, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}

	encKey, err := hkdf.Key(sha256.New, []byte(masterSecret), nil, context, 32)
	if err != nil {
		return nil, fmt.Errorf("derive decryption key: %w", err)
	}
	defer zeroBytes(encKey)

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	keyDER, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt private key: %w", err)
	}
	defer zeroBytes(keyDER)

	privKey, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	return privKey, nil
}

// encodeCertToPEM converts a DER certificate into PEM text.
//
// Why this exists:
// x509.CreateCertificate gives us raw DER bytes, but PEM is the standard
// text form that is easier to store, inspect, and pass between systems.
// The database schema stores certificates as PEM text.
func encodeCertToPEM(certDER []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	}))
}

// encodeECPrivateKeyToPEM converts an ECDSA private key to PEM text.
func encodeECPrivateKeyToPEM(key *ecdsa.PrivateKey) (string, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal EC private key: %w", err)
	}
	defer zeroBytes(keyDER)

	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})), nil
}

// parseCertFromPEM parses a PEM certificate string into an x509 object.
//
// Why this exists:
// When we load Root CA or Intermediate CA certificates back from the DB,
// they are stored as PEM text. Before Go can use them for signing or
// chain validation, we need the parsed *x509.Certificate form again.
func parseCertFromPEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}

	return cert, nil
}

// newSerialNumber creates a random 128-bit certificate serial number.
//
// Why this exists:
// Every X.509 certificate needs a serial number, and serial numbers
// should be unique for certificates issued by a CA. A random 128-bit
// value gives an extremely low collision probability for our use case.
func newSerialNumber() (*big.Int, error) {
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)

	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	return serial, nil
}

// zeroBytes best-effort clears sensitive bytes from memory.
//
// Why this exists:
// Go does not guarantee when memory will be reclaimed or overwritten.
// For sensitive temporary values like DER-encoded private keys or
// derived AES keys, we overwrite the byte slice after use.
//
// Important limitation:
// This is best-effort only. It helps reduce exposure time in memory,
// but it is not a perfect memory-sanitization guarantee.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// certValidity returns the validity window for a certificate.
//
// Why this exists:
// The Root CA, Intermediate CA, and Workspace CA all have different
// lifetimes, but they all follow the same pattern for time handling.
//
// Why notBefore is backdated:
// We subtract one hour to tolerate clock skew between systems.
// Without that, a freshly issued certificate could look "not yet valid"
// to a machine whose clock is slightly behind.
func certValidity(years int) (time.Time, time.Time) {
	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour)
	notAfter := now.AddDate(years, 0, 0)

	return notBefore, notAfter
}
