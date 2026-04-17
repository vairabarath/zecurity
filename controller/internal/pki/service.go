package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
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

	// RenewConnectorCert issues a fresh certificate for an existing connector's public key.
	// Called by: renewal.go
	RenewConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, publicKeyDER []byte, certTTL time.Duration) (*ConnectorCertResult, error)

	// SignShieldCert signs a shield CSR with the workspace CA, producing a
	// short-lived client certificate with the shield SPIFFE ID as URI SAN.
	SignShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*ShieldCertResult, error)

	// RenewShieldCert issues a fresh shield certificate from a renewal CSR.
	RenewShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csrDER []byte, certTTL time.Duration) (*ShieldCertResult, error)

	// GenerateControllerServerTLS creates an in-memory server certificate/keypair
	// for the controller gRPC endpoint. The certificate is signed by the
	// intermediate CA, carries the controller SPIFFE ID, and includes DNS/IP SANs
	// for the supplied hosts.
	GenerateControllerServerTLS(ctx context.Context, hosts []string, certTTL time.Duration) (*ControllerServerTLSResult, error)
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
	CertificatePEM    string // leaf certificate
	WorkspaceCAPEM    string // WorkspaceCA certificate
	IntermediateCAPEM string // Intermediate CA certificate
	Serial            string
	NotBefore         time.Time
	NotAfter          time.Time
}

type ShieldCertResult struct {
	CertificatePEM    string
	WorkspaceCAPEM    string
	IntermediateCAPEM string
	Serial            string
	NotBefore         time.Time
	NotAfter          time.Time
}

// ControllerServerTLSResult holds the PEM-encoded server certificate and key
// used by the controller's gRPC listener.
type ControllerServerTLSResult struct {
	CertificatePEM string
	PrivateKeyPEM  string
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
		if recovered, recoveryErr := svc.tryRecoverFromSecretMismatch(ctx, err); recoveryErr != nil {
			return nil, recoveryErr
		} else if !recovered {
			return nil, fmt.Errorf("init root CA: %w", err)
		}
	}

	if err := svc.initIntermediateCA(ctx); err != nil {
		if recovered, recoveryErr := svc.tryRecoverFromSecretMismatch(ctx, err); recoveryErr != nil {
			return nil, recoveryErr
		} else if !recovered {
			return nil, fmt.Errorf("init intermediate CA: %w", err)
		}
	}

	return svc, nil
}

func (s *serviceImpl) tryRecoverFromSecretMismatch(ctx context.Context, initErr error) (bool, error) {
	if !isSecretMismatchError(initErr) {
		return false, nil
	}

	canReset, err := s.canResetPKI(ctx)
	if err != nil {
		return false, fmt.Errorf("check PKI recovery safety after secret mismatch: %w", err)
	}

	if !canReset {
		return false, fmt.Errorf(
			"pki secret mismatch: stored CA key material was encrypted with a different PKI_MASTER_SECRET, and controller PKI cannot be regenerated because workspaces already exist: %w",
			initErr,
		)
	}

	if err := s.resetPKI(ctx); err != nil {
		return false, fmt.Errorf("reset PKI after secret mismatch: %w", err)
	}

	if err := s.initRootCA(ctx); err != nil {
		return false, fmt.Errorf("re-init root CA after secret mismatch reset: %w", err)
	}

	if err := s.initIntermediateCA(ctx); err != nil {
		return false, fmt.Errorf("re-init intermediate CA after secret mismatch reset: %w", err)
	}

	return true, nil
}

func isSecretMismatchError(err error) bool {
	return strings.Contains(err.Error(), "cipher: message authentication failed")
}

func (s *serviceImpl) canResetPKI(ctx context.Context) (bool, error) {
	var workspaceCount int
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspaces").Scan(&workspaceCount)
	if err != nil {
		return false, fmt.Errorf("count workspaces: %w", err)
	}

	return workspaceCount == 0, nil
}

func (s *serviceImpl) resetPKI(ctx context.Context) error {
	s.intermediateKey = nil

	_, err := s.pool.Exec(ctx, "TRUNCATE TABLE ca_intermediate, ca_root")
	if err != nil {
		return fmt.Errorf("truncate controller CA tables: %w", err)
	}

	return nil
}
