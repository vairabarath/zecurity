package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// initRootCA ensures a single Root CA exists in the database.
// On first startup it creates and stores one. On later startups it simply
// confirms the row already exists and leaves it encrypted at rest.
func (s *serviceImpl) initRootCA(ctx context.Context) error {
	exists, err := s.rootCAExists(ctx)
	if err != nil {
		return fmt.Errorf("check root CA: %w", err)
	}

	if exists {
		return nil
	}

	return s.generateAndStoreRootCA(ctx)
}

// rootCAExists returns whether the singleton Root CA row already exists.
func (s *serviceImpl) rootCAExists(ctx context.Context) (bool, error) {
	var count int

	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM ca_root").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query ca_root: %w", err)
	}

	return count > 0, nil
}

// generateAndStoreRootCA creates the Root CA keypair, self-signs its
// certificate, encrypts the private key, and stores the result in ca_root.
func (s *serviceImpl) generateAndStoreRootCA(ctx context.Context) error {
	privKey, err := generateECKeyPair()
	if err != nil {
		return err
	}
	defer func() {
		// Best-effort wipe of the private scalar after the key has been
		// encrypted and persisted.
		privKey.D.SetInt64(0)
	}()

	serial, err := newSerialNumber()
	if err != nil {
		return err
	}

	notBefore, notAfter := certValidity(10)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.PKIRootCACommonName,
			Organization: []string{appmeta.PKIPlatformOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLen:            2,
		MaxPathLenZero:        false,
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&privKey.PublicKey,
		privKey,
	)
	if err != nil {
		return fmt.Errorf("create root CA cert: %w", err)
	}

	certPEM := encodeCertToPEM(certDER)

	encKey, nonce, err := encryptPrivateKey(privKey, s.masterSecret, "root-ca")
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(
		ctx,
		`INSERT INTO ca_root
		 (encrypted_key, nonce, certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5)`,
		encKey,
		nonce,
		certPEM,
		notBefore,
		notAfter,
	)
	if err != nil {
		return fmt.Errorf("store root CA: %w", err)
	}

	return nil
}

// loadRootCA decrypts the stored Root CA so it can sign the Intermediate CA.
// This should only be used during PKI initialization, not on normal requests.
func (s *serviceImpl) loadRootCA(ctx context.Context) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	var encKey string
	var nonce string
	var certPEM string

	err := s.pool.QueryRow(
		ctx,
		`SELECT encrypted_key, nonce, certificate_pem
		 FROM ca_root
		 LIMIT 1`,
	).Scan(&encKey, &nonce, &certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("load root CA from DB: %w", err)
	}

	cert, err := parseCertFromPEM(certPEM)
	if err != nil {
		return nil, nil, err
	}

	privKey, err := decryptPrivateKey(encKey, nonce, s.masterSecret, "root-ca")
	if err != nil {
		return nil, nil, err
	}

	return cert, privKey, nil
}
