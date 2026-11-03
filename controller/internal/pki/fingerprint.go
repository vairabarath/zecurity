package pki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RowQuerier is the part of pgxpool.Pool / pgx.Tx needed to read one row.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IntermediateFingerprint returns the lowercase hex SHA-256 of the Platform
// Intermediate CA's DER: the certificate GET /ca.crt serves, and the value
// bootstrapping agents pin (connector and shield tokens, RELAY_CA_FINGERPRINT).
func IntermediateFingerprint(ctx context.Context, q RowQuerier) (string, error) {
	var certPEM string
	if err := q.QueryRow(ctx, `SELECT certificate_pem FROM ca_intermediate LIMIT 1`).Scan(&certPEM); err != nil {
		return "", fmt.Errorf("load intermediate ca: %w", err)
	}
	return FingerprintPEM(certPEM)
}

// FingerprintPEM returns the lowercase hex SHA-256 of the DER of the first
// PEM block in certPEM.
func FingerprintPEM(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || len(block.Bytes) == 0 {
		return "", errors.New("decode intermediate ca pem")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}
