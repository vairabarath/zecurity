package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/url"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// GenerateWorkspaceCA creates a tenant-scoped Workspace CA signed by the
// Intermediate CA currently loaded in memory.
func (s *serviceImpl) GenerateWorkspaceCA(ctx context.Context, tenantID string) (*WorkspaceCAResult, error) {
	if s.intermediateKey == nil {
		return nil, fmt.Errorf("intermediate CA not initialized")
	}

	privKey, err := generateECKeyPair()
	if err != nil {
		return nil, err
	}
	defer privKey.D.SetInt64(0)

	serial, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	notBefore, notAfter := certValidity(2)

	tenantURI := &url.URL{
		Scheme: "tenant",
		Opaque: tenantID,
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "workspace-" + tenantID,
			Organization: []string{appmeta.PKIWorkspaceOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		URIs:                  []*url.URL{tenantURI},
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		s.intermediateKey.cert,
		&privKey.PublicKey,
		s.intermediateKey.privKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create workspace CA cert: %w", err)
	}

	certPEM := encodeCertToPEM(certDER)

	encKey, nonce, err := encryptPrivateKey(privKey, s.masterSecret, tenantID)
	if err != nil {
		return nil, err
	}

	return &WorkspaceCAResult{
		EncryptedPrivateKey: encKey,
		Nonce:               nonce,
		CertificatePEM:      certPEM,
		KeyAlgorithm:        "EC-P384",
		NotBefore:           notBefore,
		NotAfter:            notAfter,
	}, nil
}
