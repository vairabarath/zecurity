package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/url"
	"time"

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

// SignConnectorCert signs a connector's CSR with the workspace CA, producing
// a short-lived client certificate with the connector's SPIFFE ID as URI SAN.
// Called by: enrollment.go (Phase 3, Step 9)
//
// Parameters:
//   - tenantID: workspace ID (used to load the workspace CA key from workspace_ca_keys)
//   - connectorID: connector UUID (for CN and SPIFFE ID)
//   - trustDomain: workspace trust domain (for the SPIFFE URI SAN)
//   - csr: parsed x509.CertificateRequest from the connector
//   - certTTL: certificate lifetime (typically 7 days from cfg.CertTTL)
//
// Certificate properties:
//   - Subject.CommonName = appmeta.PKIConnectorCNPrefix + connectorID
//   - Subject.Organization = appmeta.PKIWorkspaceOrganization
//   - Single URI SAN = appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
//   - NotAfter = now + certTTL
//   - KeyUsage = DigitalSignature (client cert — no KeyEncipherment)
//   - ExtKeyUsage = ClientAuth (connector authenticates to controller via mTLS)
//   - IsCA = false (leaf certificate, cannot sign other certificates)
//   - Signed by workspace CA (loaded + decrypted from workspace_ca_keys table)
func (s *serviceImpl) SignConnectorCert(
	ctx context.Context,
	tenantID, connectorID, trustDomain string,
	csr *x509.CertificateRequest,
	certTTL time.Duration,
) (*ConnectorCertResult, error) {
	// 1. Load workspace CA key material from the database.
	// The encrypted private key + nonce + certificate are stored in workspace_ca_keys.
	var encryptedKey, nonce, caCertPEM string
	err := s.pool.QueryRow(ctx,
		`SELECT encrypted_private_key, nonce, certificate_pem
		   FROM workspace_ca_keys
		  WHERE tenant_id = $1`,
		tenantID,
	).Scan(&encryptedKey, &nonce, &caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("load workspace CA key for tenant %s: %w", tenantID, err)
	}

	// 2. Decrypt the workspace CA private key.
	// Uses the same master secret and tenant-scoped context as encryptPrivateKey.
	// Called: crypto.go → decryptPrivateKey()
	caPrivKey, err := decryptPrivateKey(encryptedKey, nonce, s.masterSecret, tenantID)
	if err != nil {
		return nil, fmt.Errorf("decrypt workspace CA key: %w", err)
	}
	defer caPrivKey.D.SetInt64(0) // zero the private scalar after use

	// 3. Parse the workspace CA certificate (needed as the issuer for signing).
	// Called: crypto.go → parseCertFromPEM()
	caCert, err := parseCertFromPEM(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse workspace CA cert: %w", err)
	}

	// 4. Generate a unique serial number for the connector certificate.
	// Called: crypto.go → newSerialNumber()
	serial, err := newSerialNumber()
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	// 5. Build the SPIFFE URI SAN for the connector.
	// Format: "spiffe://<trustDomain>/connector/<connectorID>"
	// Called: appmeta/identity.go → ConnectorSPIFFEID()
	spiffeURI, err := url.Parse(appmeta.ConnectorSPIFFEID(trustDomain, connectorID))
	if err != nil {
		return nil, fmt.Errorf("parse SPIFFE URI: %w", err)
	}

	// 6. Build validity window.
	// Backdated 1 hour for clock skew tolerance (same pattern as certValidity).
	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour)
	notAfter := now.Add(certTTL)

	// 7. Build the certificate template.
	// The public key comes from the connector's CSR — we never generate keys for them.
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.PKIConnectorCNPrefix + connectorID,
			Organization: []string{appmeta.PKIWorkspaceOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{spiffeURI},
	}

	// 8. Sign the certificate with the workspace CA.
	// The connector's public key is taken from the CSR.
	// The signing key is the workspace CA's decrypted private key.
	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		caCert,
		csr.PublicKey,
		caPrivKey,
	)
	if err != nil {
		return nil, fmt.Errorf("sign connector certificate: %w", err)
	}

	// 9. Encode the DER certificate to PEM.
	// Called: crypto.go → encodeCertToPEM()
	certPEM := encodeCertToPEM(certDER)

	return &ConnectorCertResult{
		CertificatePEM: certPEM,
		Serial:         serial.Text(16),
		NotBefore:      notBefore,
		NotAfter:       notAfter,
	}, nil
}

// RenewConnectorCert issues a fresh certificate for an existing connector's public key.
// Called by: renewal.go (Phase 3)
//
// Unlike SignConnectorCert (enrollment), there is no CSR here.
// The connector keeps its existing EC P-384 keypair — we just issue
// a new certificate with a fresh validity window.
//
// Parameters:
//   - tenantID: workspace ID (used to load the workspace CA key)
//   - connectorID: connector UUID (for CN and SPIFFE ID)
//   - trustDomain: workspace trust domain (for the SPIFFE URI SAN)
//   - publicKeyDER: connector's existing EC P-384 public key in DER format
//   - certTTL: certificate lifetime (typically 7 days)
//
// Returns the new certificate PEM + serial number + validity window.
func (s *serviceImpl) RenewConnectorCert(
	ctx context.Context,
	tenantID, connectorID, trustDomain string,
	publicKeyDER []byte,
	certTTL time.Duration,
) (*ConnectorCertResult, error) {
	// 1. Parse the connector's CSR to extract the public key.
	// The connector sends a PKCS#10 CSR (self-signed) — CheckSignature proves
	// it holds the corresponding private key, and PublicKey carries the key
	// we need to sign the renewal certificate.
	csr, err := x509.ParseCertificateRequest(publicKeyDER)
	if err != nil {
		return nil, fmt.Errorf("parse connector CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("verify connector CSR signature: %w", err)
	}
	pubKey := csr.PublicKey

	// 2. Load workspace CA key material from the database.
	var encryptedKey, nonce, caCertPEM string
	err = s.pool.QueryRow(ctx,
		`SELECT encrypted_private_key, nonce, certificate_pem
		   FROM workspace_ca_keys
		  WHERE tenant_id = $1`,
		tenantID,
	).Scan(&encryptedKey, &nonce, &caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("load workspace CA key for tenant %s: %w", tenantID, err)
	}

	// 3. Decrypt the workspace CA private key.
	caPrivKey, err := decryptPrivateKey(encryptedKey, nonce, s.masterSecret, tenantID)
	if err != nil {
		return nil, fmt.Errorf("decrypt workspace CA key: %w", err)
	}
	defer caPrivKey.D.SetInt64(0)

	// 4. Parse the workspace CA certificate.
	caCert, err := parseCertFromPEM(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse workspace CA cert: %w", err)
	}

	// 5. Generate a unique serial number.
	serial, err := newSerialNumber()
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	// 6. Build the SPIFFE URI SAN.
	spiffeURI, err := url.Parse(appmeta.ConnectorSPIFFEID(trustDomain, connectorID))
	if err != nil {
		return nil, fmt.Errorf("parse SPIFFE URI: %w", err)
	}

	// 7. Build validity window.
	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour) // clock skew tolerance
	notAfter := now.Add(certTTL)

	// 8. Build the certificate template (same as SignConnectorCert, but using provided public key).
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.PKIConnectorCNPrefix + connectorID,
			Organization: []string{appmeta.PKIWorkspaceOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{spiffeURI},
	}

	// 9. Sign the certificate with the workspace CA.
	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		caCert,
		pubKey,
		caPrivKey,
	)
	if err != nil {
		return nil, fmt.Errorf("sign connector certificate: %w", err)
	}

	// 10. Encode to PEM.
	certPEM := encodeCertToPEM(certDER)

	// 11. Load intermediate CA for the response
	var intermediateCAPEM string
	err = s.pool.QueryRow(ctx,
		`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
	).Scan(&intermediateCAPEM)
	if err != nil {
		return nil, fmt.Errorf("load intermediate CA: %w", err)
	}

	// 12. Return the CA chain for the connector
	return &ConnectorCertResult{
		CertificatePEM:    certPEM,
		WorkspaceCAPEM:    caCertPEM,
		IntermediateCAPEM: intermediateCAPEM,
		Serial:            serial.Text(16),
		NotBefore:         notBefore,
		NotAfter:          notAfter,
	}, nil
}
