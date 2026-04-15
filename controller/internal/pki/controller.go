package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// GenerateControllerServerTLS creates a short-lived controller server
// certificate signed by the intermediate CA. The certificate is generated in
// memory at startup and is not persisted.
func (s *serviceImpl) GenerateControllerServerTLS(ctx context.Context, hosts []string, certTTL time.Duration) (*ControllerServerTLSResult, error) {
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

	spiffeURI, err := url.Parse(appmeta.SPIFFEControllerID)
	if err != nil {
		return nil, fmt.Errorf("parse controller SPIFFE URI: %w", err)
	}

	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour)
	notAfter := now.Add(certTTL)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.ProductName + " Controller",
			Organization: []string{appmeta.PKIPlatformOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{spiffeURI},
	}

	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		s.intermediateKey.cert,
		&privKey.PublicKey,
		s.intermediateKey.privKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create controller server cert: %w", err)
	}

	keyPEM, err := encodeECPrivateKeyToPEM(privKey)
	if err != nil {
		return nil, err
	}

	return &ControllerServerTLSResult{
		CertificatePEM: encodeCertToPEM(certDER),
		PrivateKeyPEM:  keyPEM,
		NotBefore:      notBefore,
		NotAfter:       notAfter,
	}, nil
}
