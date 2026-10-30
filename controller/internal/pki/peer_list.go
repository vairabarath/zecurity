package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
)

// ShieldPeerListContext domain-separates a signed shield peer list (D21-R a2)
// from everything else the Workspace CA key signs. Certificates and CRLs are
// DER SEQUENCEs (first byte 0x30); this prefix starts with 'z', so a peer-list
// signature can never be taken for a certificate or CRL signature, or the
// other way round. The shield verifies over the same bytes.
const ShieldPeerListContext = "zecurity/shield-peer-list/v1\x00"

// ShieldPeerListDigest is the SHA-384 digest a peer-list signature covers:
// ShieldPeerListContext || payload.
func ShieldPeerListDigest(payload []byte) []byte {
	h := sha512.New384()
	h.Write([]byte(ShieldPeerListContext))
	h.Write(payload)
	return h.Sum(nil)
}

// SignShieldPeerList signs a serialized PeerListPayload with tenantID's
// Workspace CA key (ECDSA P-384 over ShieldPeerListDigest, ASN.1 DER). The key
// is loaded and decrypted per call, as GenerateClientCRL does.
func (s *serviceImpl) SignShieldPeerList(ctx context.Context, tenantID string, payload []byte) ([]byte, error) {
	var encryptedKey, nonce string
	if err := s.pool.QueryRow(ctx,
		`SELECT encrypted_private_key, nonce
		   FROM workspace_ca_keys
		  WHERE tenant_id = $1`,
		tenantID,
	).Scan(&encryptedKey, &nonce); err != nil {
		return nil, fmt.Errorf("load workspace CA key: %w", err)
	}

	caPrivKey, err := decryptPrivateKey(encryptedKey, nonce, s.masterSecret, tenantID)
	if err != nil {
		return nil, fmt.Errorf("decrypt workspace CA key: %w", err)
	}
	defer caPrivKey.D.SetInt64(0)

	sig, err := ecdsa.SignASN1(rand.Reader, caPrivKey, ShieldPeerListDigest(payload))
	if err != nil {
		return nil, fmt.Errorf("sign shield peer list: %w", err)
	}
	return sig, nil
}
