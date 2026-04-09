package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// x509 and time are used in the Service interface method signatures.

// Service is the public PKI contract used by the rest of the controller.
// The application should depend on this interface rather than concrete CA
// implementation details.
type Service interface {
	// GenerateWorkspaceCA creates a tenant-scoped Workspace CA.
	// It returns encrypted key material and the signed certificate, but does
	// not persist them. The bootstrap transaction stores the result in the DB.
	GenerateWorkspaceCA(ctx context.Context, tenantID string) (*WorkspaceCAResult, error)

	// SignConnectorCert signs a connector's CSR with the workspace CA, producing
	// a short-lived client certificate with the connector's SPIFFE ID as URI SAN.
	// Called by: enrollment.go (Phase 3, Step 9)
	SignConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*ConnectorCertResult, error)
}

// WorkspaceCAResult is the bootstrap-ready output of GenerateWorkspaceCA.
// These fields map directly to the workspace_ca_keys table.
type WorkspaceCAResult struct {
	EncryptedPrivateKey string
	Nonce               string
	CertificatePEM      string
	KeyAlgorithm        string
	NotBefore           time.Time
	NotAfter            time.Time
}

// ConnectorCertResult holds the output of SignConnectorCert.
// Called by: enrollment.go (Phase 3) to build the EnrollResponse.
type ConnectorCertResult struct {
	CertificatePEM string
	Serial         string
	NotBefore      time.Time
	NotAfter       time.Time
}

// serviceImpl is the concrete PKI service used inside the controller.
// It keeps the intermediate CA loaded in memory so workspace CAs can be
// signed without repeated DB reads and decrypt operations.
type serviceImpl struct {
	masterSecret    string
	pool            *pgxpool.Pool
	intermediateKey *intermediateCAState
}

// intermediateCAState is the in-memory signing material loaded during Init.
type intermediateCAState struct {
	cert    *x509.Certificate
	privKey *ecdsa.PrivateKey
}

// Init prepares the PKI service before the HTTP server starts.
// It requires a master secret, ensures the Root CA exists, and ensures the
// Intermediate CA exists and is loaded for workspace signing.
func Init(ctx context.Context, pool *pgxpool.Pool) (Service, error) {
	masterSecret := os.Getenv("PKI_MASTER_SECRET")
	if masterSecret == "" {
		return nil, fmt.Errorf("PKI_MASTER_SECRET not set")
	}

	svc := &serviceImpl{
		masterSecret: masterSecret,
		pool:         pool,
	}

	if err := svc.initRootCA(ctx); err != nil {
		return nil, fmt.Errorf("init root CA: %w", err)
	}

	if err := svc.initIntermediateCA(ctx); err != nil {
		return nil, fmt.Errorf("init intermediate CA: %w", err)
	}

	return svc, nil
}
