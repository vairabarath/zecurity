package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// initIntermediateCA ensures an Intermediate CA exists and is loaded in memory
// for Workspace CA signing during runtime.
func (s *serviceImpl) initIntermediateCA(ctx context.Context) error {
	exists, err := s.intermediateCAExists(ctx)
	if err != nil {
		return fmt.Errorf("check intermediate CA: %w", err)
	}

	if !exists {
		if err := s.generateAndStoreIntermediateCA(ctx); err != nil {
			return err
		}
	}

	return s.loadIntermediateCAIntoMemory(ctx)
}

// intermediateCAExists returns whether the singleton Intermediate CA row is
// already present in the database.
func (s *serviceImpl) intermediateCAExists(ctx context.Context) (bool, error) {
	var count int

	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM ca_intermediate").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query ca_intermediate: %w", err)
	}

	return count > 0, nil
}

// generateAndStoreIntermediateCA creates the Intermediate CA, signs it with the
// Root CA, encrypts the Intermediate private key, and stores it in Postgres.
func (s *serviceImpl) generateAndStoreIntermediateCA(ctx context.Context) error {
	rootCert, rootKey, err := s.loadRootCA(ctx)
	if err != nil {
		return fmt.Errorf("load root CA for intermediate signing: %w", err)
	}
	defer rootKey.D.SetInt64(0)

	privKey, err := generateECKeyPair()
	if err != nil {
		return err
	}
	defer privKey.D.SetInt64(0)

	serial, err := newSerialNumber()
	if err != nil {
		return err
	}

	notBefore, notAfter := certValidity(5)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.PKIIntermediateCommonName,
			Organization: []string{appmeta.PKIPlatformOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		MaxPathLenZero:        false,
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		rootCert,
		&privKey.PublicKey,
		rootKey,
	)
	if err != nil {
		return fmt.Errorf("create intermediate CA cert: %w", err)
	}

	certPEM := encodeCertToPEM(certDER)

	encKey, nonce, err := encryptPrivateKey(privKey, s.masterSecret, "intermediate-ca")
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(
		ctx,
		`INSERT INTO ca_intermediate
		 (encrypted_key, nonce, certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5)`,
		encKey,
		nonce,
		certPEM,
		notBefore,
		notAfter,
	)
	if err != nil {
		return fmt.Errorf("store intermediate CA: %w", err)
	}

	return nil
}

// loadIntermediateCAIntoMemory decrypts the Intermediate CA private key and
// stores the signing material in service memory for subsequent workspace CA
// generation.
func (s *serviceImpl) loadIntermediateCAIntoMemory(ctx context.Context) error {
	var encKey string
	var nonce string
	var certPEM string

	err := s.pool.QueryRow(
		ctx,
		`SELECT encrypted_key, nonce, certificate_pem
		 FROM ca_intermediate
		 LIMIT 1`,
	).Scan(&encKey, &nonce, &certPEM)
	if err != nil {
		return fmt.Errorf("load intermediate CA from DB: %w", err)
	}

	cert, err := parseCertFromPEM(certPEM)
	if err != nil {
		return err
	}

	privKey, err := decryptPrivateKey(encKey, nonce, s.masterSecret, "intermediate-ca")
	if err != nil {
		return fmt.Errorf("decrypt intermediate CA key: %w", err)
	}

	s.intermediateKey = &intermediateCAState{
		cert:    cert,
		privKey: privKey,
	}

	return nil
}
